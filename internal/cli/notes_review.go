package cli

// `magnum notes <repo> --curate | --review | --restore <version>`: a
// curation proposal (engine/notes_curate.go) or a restore of an earlier
// version waits in the registry for the operator; --review shows it and asks
// y/N. y applies it (engine.ApplyNotesProposal: under the notes lock, with
// the protocol the judges use, agents.NotesLockLine; the live notes are read
// again and must still be what the review showed, and the proposal is
// claimed before anything is written, so a decision the critic made
// meanwhile wins and is named, and a critique in flight is named before the
// question); n rejects it with --reason, which the next
// curation reads. A curation that would lose a line of the notes
// (notes.CheckPreserved) cannot be applied: the review says so, and y asks
// for a new curation instead. A stale proposal (the notes changed since it was made,
// engine.CheckStale) says so first: when its changes and the notes' merge,
// the review shows the merge and y applies it; when they conflict, or the
// operator answers c, y asks the daemon for a new curation from the notes
// now that reads the stale proposal, which is kept as superseded. Nothing a
// proposal removes is lost: the registry keeps every version and every
// proposal. A curation given the retro's misses shows what it did with each;
// the decision moves them (store.DecideNotesProposal): used when applied,
// back to new when rejected, dismissed after a second rejection.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// notesLockWait is how long an apply waits for the notes lock (tests
// shorten it).
var notesLockWait = engine.NotesLockWait

// errNotesChanged refuses an apply when the live notes changed while the
// operator read the review.
var errNotesChanged = fmt.Errorf("%w; `--review` again shows it merged with their changes", engine.ErrNotesChanged)

// notesCurateJSON is what --curate --json prints.
type notesCurateJSON struct {
	Payload engine.NotesCuratePayload `json:"payload"`
	Request *actRequestJSON           `json:"request,omitempty"`
	Error   string                    `json:"error,omitempty"`
}

// notesCurate asks the daemon for a curation now (engine.ReqNotesCurate);
// supersede names the stale proposal it follows up on (0 = none).
func notesCurate(ctx context.Context, c *Context, d *actDeps, f notesFlags, full string, supersede int64, why string) int {
	out := notesCurateJSON{Payload: engine.NotesCuratePayload{Repo: full, Supersede: supersede, Why: why}}
	fail := func(err error) int {
		if f.json {
			out.Error = err.Error()
			_ = writeJSON(c.Stdout, out)
			return 1
		}
		return cmdFail(c, "notes", err)
	}
	res, err := d.reqs().send(ctx, engine.ReqNotesCurate, out.Payload, d.quick())
	if err != nil {
		return fail(err)
	}
	rv := res.view()
	out.Request = &rv
	switch {
	case res.PID == 0 && res.Pending():
		res.printNotes(c.Stderr)
		return fail(fmt.Errorf("no daemon is running: request %d stays queued and starts the curation once one runs: %s", res.ID(), actDaemonFix))
	case res.Failed():
		res.printNotes(c.Stderr)
		return fail(errors.New(res.Result()))
	case f.json:
		_ = writeJSON(c.Stdout, out)
		return 0
	}
	res.print(c.Stdout, c.Stderr)
	return 0
}

// notesReviewData is a proposal as --review shows it (and --json prints):
// engine.NotesReviewView, which the notes critic reads too.
type notesReviewData = engine.NotesReviewView

// notesReview reviews the newest proposal waiting for the repository.
func notesReview(ctx context.Context, c *Context, d *actDeps, f notesFlags, repo store.Repo, nr notes.Repo) int {
	ps, err := d.Store.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repo.ID, States: engine.WaitingStates, Limit: 1})
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	if len(ps) > 0 && ps[0].State == store.ProposalApplying {
		if f.json {
			_ = writeJSON(c.Stdout, map[string]any{"repo": repo.FullName(), "proposal": ps[0]})
			return 0
		}
		fmt.Fprintf(c.Stderr, "proposal %d for the notes of %s is being applied by %s (an apply cut short is finished or undone at the daemon's next start)\n",
			ps[0].ID, repo.FullName(), engine.DeciderName(ps[0].DecidedBy))
		return 0
	}
	if len(ps) == 0 {
		if f.json {
			_ = writeJSON(c.Stdout, map[string]any{"repo": repo.FullName(), "proposal": nil})
			return 0
		}
		fmt.Fprintf(c.Stderr, "no proposal for the notes of %s waits for review (`magnum notes %s --curate` asks for a curation)\n",
			repo.FullName(), repo.FullName())
		return 0
	}
	return notesReviewProposal(ctx, c, d, f, repo, nr, ps[0])
}

// notesRestore proposes version f.restore of the repository's notes back,
// based on the notes as they are now (recorded first when the registry has
// no record of them), and reviews it as a curation. Nothing is written
// before the operator can answer: without a terminal it refuses, and --json
// prints the restore as it would be proposed (proposal id 0) and records
// nothing, so no run that cannot answer leaves a proposal waiting.
func notesRestore(ctx context.Context, c *Context, d *actDeps, f notesFlags, repo store.Repo, nr notes.Repo) int {
	full := repo.FullName()
	v, err := d.Store.NotesVersionByID(ctx, f.restore)
	if errors.Is(err, store.ErrNotFound) || (err == nil && v.RepoID != repo.ID) {
		return cmdFail(c, "notes", fmt.Errorf("version %d is not a version of the notes of %s (`magnum notes %s --log` lists them)", f.restore, full, full))
	}
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	live, err := notes.ReadState(nr)
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	if live.Fingerprint() == notesFingerprint(v) {
		fmt.Fprintf(c.Stderr, "the notes of %s hold version %d already\n", full, v.ID)
		return 0
	}
	switch {
	case f.json:
		return notesRestorePreview(ctx, c, d, repo, v, live)
	case !d.StdinTTY:
		return inspUsage(c, "notes", "--restore asks y/N on a terminal; --json prints the restore for scripts", notesUsage)
	}
	base, _, err := d.Store.RecordNotesVersion(ctx, store.NotesVersionInput{RepoID: repo.ID, Source: store.NotesFromImport,
		Content: engine.ContentOf(live), Dedupe: true})
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	p, err := d.Store.CreateNotesProposal(ctx, store.NotesProposalInput{RepoID: repo.ID, Kind: store.ProposalRestore, Trigger: "restore",
		BaseVersionID: base.ID, VersionID: v.ID, State: store.ProposalPending})
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	return notesReviewProposal(ctx, c, d, f, repo, nr, p)
}

// notesRestorePreview prints, as --review --json does, what a restore of
// version v would change in the notes now (live), without recording
// anything: the proposal has no id (0) nor state, and its base is the
// latest version when that holds the notes now (none when they are not
// recorded yet).
func notesRestorePreview(ctx context.Context, c *Context, d *actDeps, repo store.Repo, v store.NotesVersion, live notes.State) int {
	content, err := d.Store.NotesVersionContent(ctx, v.ID)
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	p := store.NotesProposal{RepoID: repo.ID, Kind: store.ProposalRestore, Trigger: "restore", VersionID: &v.ID, CreatedAt: notesNow(d)}
	if latest, err := d.Store.LatestNotesVersion(ctx, repo.ID); err == nil && notesFingerprint(latest) == live.Fingerprint() {
		p.BaseVersionID = &latest.ID
	}
	data, err := notesReviewFrom(ctx, d, repo.FullName(), p, engine.StaleCheck{Live: live, Base: live, Proposed: engine.StateOf(content)})
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	if err := writeJSON(c.Stdout, data); err != nil {
		return cmdFail(c, "notes", err)
	}
	return 0
}

// notesReviewProposal shows proposal p and asks y/N on a terminal: y
// applies it (a stale one merged with the notes' changes since), n rejects
// it with --reason, anything else leaves it waiting. A stale curation whose
// changes conflict with the notes', or one the operator answers c to, is
// followed up by a new curation instead: y asks the daemon for it. --json
// prints the data and asks nothing.
func notesReviewProposal(ctx context.Context, c *Context, d *actDeps, f notesFlags, repo store.Repo, nr notes.Repo, p store.NotesProposal) int {
	full := repo.FullName()
	data, err := notesReviewOf(ctx, d, full, nr, p)
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	data.Expired = notesNow(d).Sub(p.CreatedAt) >= engine.ProposalTTL
	if f.json {
		if err := writeJSON(c.Stdout, data); err != nil {
			return cmdFail(c, "notes", err)
		}
		return 0
	}
	if !d.StdinTTY {
		return inspUsage(c, "notes", "--review asks y/N on a terminal; --json prints the proposal for scripts", notesUsage)
	}
	notesPrintReview(c, d.StdoutTTY, data)
	if data.Expired {
		if _, err := d.Store.DecideNotesProposal(ctx, p.ID, []string{store.ProposalPending}, store.ProposalExpired, store.DecidedByMagnum,
			"not reviewed within "+engine.ProposalTTL.String(), notesNow(d), nil); err != nil {
			return cmdFail(c, "notes", err)
		}
		return cmdFail(c, "notes", fmt.Errorf("proposal %d is older than 7 days and expired; `magnum notes %s --curate` asks for a new one", p.ID, full))
	}
	// A recovery (magnum's own proposal) is reviewed like a restore: no
	// curator to follow it up.
	curation := p.Kind == store.ProposalCuration && p.Trigger != engine.CurateTriggerRecover
	conflict := data.Stale && !data.Merge.Clean
	lossy := len(data.NotPreserved) > 0 && !conflict
	notesSayCriticOn(ctx, c, d, p.ID, !lossy && !conflict)
	what, again := "restore", fmt.Sprintf("--restore %d` proposes the version", store.Deref(p.VersionID))
	if p.Trigger == engine.CurateTriggerRecover {
		what, again = "recovery", "--recover` proposes it"
	}
	switch {
	case lossy && curation:
		fmt.Fprintf(c.Stdout, "Proposal %d would lose lines of the notes, so it cannot be applied. y asks for a new curation from the notes now, "+
			"which reads proposal %d (kept as superseded), n rejects it, anything else leaves it for later [y/N] ", p.ID, p.ID)
	case lossy:
		fmt.Fprintf(c.Stdout, "Proposal %d would lose lines of the notes, so it cannot be applied. n rejects it, anything else leaves it for later [y/N] ", p.ID)
	case conflict && curation:
		fmt.Fprintf(c.Stdout, "Ask for a new curation of the notes of %s? y asks for one from the notes now, which reads proposal %d (kept as superseded), "+
			"n rejects it, anything else leaves it for later [y/N] ", full, p.ID)
	case conflict:
		fmt.Fprintf(c.Stdout, "Proposal %d cannot be applied. y expires it (`%s again from the notes now), "+
			"n rejects it, anything else leaves it for later [y/N] ", p.ID, again)
	case data.Stale && curation:
		fmt.Fprintf(c.Stdout, "Apply the merge of proposal %d to the notes of %s? y applies it, c asks for a new curation from the notes now instead "+
			"(proposal %d kept as superseded), n rejects it, anything else leaves it for later [y/N] ", p.ID, full, p.ID)
	case data.Stale:
		fmt.Fprintf(c.Stdout, "Apply the merge of proposal %d to the notes of %s? y applies it, n rejects it, anything else leaves it for later [y/N] ", p.ID, full)
	default:
		fmt.Fprintf(c.Stdout, "Apply proposal %d to the notes of %s? y applies it, n rejects it, anything else leaves it for later [y/N] ", p.ID, full)
	}
	answer, err := d.readLine(ctx)
	if err != nil {
		fmt.Fprintln(c.Stdout)
		if ctx.Err() != nil {
			return 130
		}
		answer = ""
	}
	switch a := strings.ToLower(strings.TrimSpace(answer)); {
	case curation && lossy && (a == "y" || a == "yes"):
		return notesCurate(ctx, c, d, f, full, p.ID, "it would lose lines of the notes; the operator asked for a new curation from the notes now")
	case lossy && (a == "y" || a == "yes"):
		fmt.Fprintf(c.Stdout, "left for later: proposal %d cannot be applied; `magnum notes %s --review` and n rejects it\n", p.ID, full)
	case curation && ((conflict && (a == "y" || a == "yes")) || (data.Stale && a == "c")):
		return notesCurate(ctx, c, d, f, full, p.ID, "")
	case (a == "y" || a == "yes") && conflict:
		if _, err := d.Store.DecideNotesProposal(ctx, p.ID, []string{store.ProposalPending}, store.ProposalExpired, store.DecidedByMagnum,
			"the notes changed since the "+what+" was proposed, and the changes conflict", notesNow(d), nil); err != nil {
			return cmdFail(c, "notes", err)
		}
		return cmdFail(c, "notes", fmt.Errorf("proposal %d conflicts with the notes' changes since and expired; `magnum notes %s %s again from the notes now",
			p.ID, full, again))
	case a == "y" || a == "yes":
		target, _ := data.Target()
		applied, err := notesApply(ctx, d.Store, nr, p, data.Check.Live.Fingerprint(), target.Fingerprint(), notesNow(d))
		if err != nil {
			return cmdFail(c, "notes", err)
		}
		what := ""
		if data.Stale {
			what = ", the proposal merged with the notes' changes since"
		}
		fmt.Fprintf(c.Stdout, "applied: the notes of %s are version %d%s%s\n", full, store.Deref(applied.AppliedVersionID), what, notesMissesMoved(ctx, d, p.ID))
	case a == "n" || a == "no":
		reason := strings.TrimSpace(f.reason)
		if _, err := d.Store.DecideNotesProposal(ctx, p.ID, []string{store.ProposalPending}, store.ProposalRejected, store.DecidedByOperator, reason, notesNow(d), nil); err != nil {
			return cmdFail(c, "notes", notesDecided(ctx, d, p.ID, err))
		}
		msg := fmt.Sprintf("rejected proposal %d", p.ID)
		if reason != "" && curation {
			msg += "; the next curation reads why"
		}
		fmt.Fprintln(c.Stdout, msg+notesMissesMoved(ctx, d, p.ID))
	default:
		fmt.Fprintf(c.Stdout, "left for later: `magnum notes %s --review`\n", full)
	}
	return 0
}

// notesSayCriticOn says, when the notes critic works on proposal id (the
// one under review), "the critic is on proposal 3 (fable, 2m).", and when y
// would apply it (applies), that it applies anyway: the claim settles who
// wins, and the critic's decision then comes too late and is dropped.
func notesSayCriticOn(ctx context.Context, c *Context, d *actDeps, id int64, applies bool) {
	m := engine.ReadCritic(ctx, d.Store)
	if m == nil || m.Proposal != id {
		return
	}
	since := actAgo(notesNow(d), m.Started)
	if m.Model != "" {
		since = notes.Printable(m.Model) + ", " + since
	}
	s := fmt.Sprintf("the critic is on proposal %d (%s).", m.Proposal, since)
	if applies {
		s += " y applies anyway, and the critic's decision then comes too late and is dropped."
	}
	fmt.Fprintln(c.Stdout, s)
}

// notesMissesMoved says where the decision of proposal id left the misses
// it was given ("" when it had none): used, back to new, dismissed.
func notesMissesMoved(ctx context.Context, d *actDeps, id int64) string {
	links, err := d.Store.ProposalMisses(ctx, id)
	if err != nil || len(links) == 0 {
		return ""
	}
	n := map[string]int{}
	for _, l := range links {
		n[l.Miss.State]++
	}
	var parts []string
	if k := n[store.MissUsed]; k > 0 {
		parts = append(parts, textx.Count(k, "miss", "misses")+" used")
	}
	if k := n[store.MissNew]; k > 0 {
		parts = append(parts, textx.Count(k, "miss", "misses")+" back to new for the next curation")
	}
	if k := n[store.MissDismissed]; k > 0 {
		parts = append(parts, fmt.Sprintf("%s dismissed after %d rejected proposals", textx.Count(k, "miss", "misses"), store.MissDismissRejections))
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, ", ")
}

// notesReviewOf gathers a proposal's review (engine.ReviewProposal).
func notesReviewOf(ctx context.Context, d *actDeps, full string, nr notes.Repo, p store.NotesProposal) (notesReviewData, error) {
	return engine.ReviewProposal(ctx, d.Store, notesMaxLine(d), full, nr, p)
}

// notesReviewFrom is notesReviewOf from p's states as check read them (a
// restore not proposed yet has no states in the registry to read).
func notesReviewFrom(ctx context.Context, d *actDeps, full string, p store.NotesProposal, check engine.StaleCheck) (notesReviewData, error) {
	return engine.ReviewFrom(ctx, d.Store, notesMaxLine(d), full, p, check)
}

// notesMaxLine is [notes] max_line, for the sizes (0 without a config).
func notesMaxLine(d *actDeps) int {
	if d.Cfg == nil {
		return 0
	}
	return d.Cfg.Notes.MaxLine
}

// notesPrintReview writes a proposal's review: what it is, the sizes, the
// notes' diff (colored on a terminal), the harness changes and the
// sections with the curator's reasons.
func notesPrintReview(c *Context, color bool, data notesReviewData) {
	p := data.Proposal
	what := "a curation"
	switch {
	case p.Kind == store.ProposalRestore:
		what = fmt.Sprintf("a restore of version %d", store.Deref(p.VersionID))
	case p.Trigger == engine.CurateTriggerRecover:
		what = "a recovery of the lines the notes history held and the notes lack (the notes stay as they are)"
	case p.Trigger != "":
		what += " (" + p.Trigger + ")"
	}
	if p.Model != "" {
		what += " by " + p.Model
	}
	w := c.Stdout
	fmt.Fprintf(w, "proposal %d for the notes of %s: %s, %s\n", p.ID, data.Repo, what, p.CreatedAt.Local().Format("2006-01-02 15:04"))
	base := store.Deref(p.BaseVersionID)
	fromName := fmt.Sprintf("%s notes, version %d (when the proposal was made)", data.Repo, base)
	from, to := data.Check.Base.Notes, data.Check.Proposed.Notes
	fromArchive, toArchive := data.Check.Base.Archive(), data.Check.Proposed.Archive()
	if m := data.Merge; data.Stale {
		fmt.Fprintf(w, "STALE: the notes changed since the proposal was made from version %d.\n", base)
		switch {
		case m.Clean:
			fmt.Fprintln(w, "Its changes and theirs merge: below is the merge, as it would change the notes now.")
			if len(m.Kept) > 0 {
				fmt.Fprintf(w, "The proposal deletes %s, which a round changed since: kept as the round left it.\n", strings.Join(m.Kept, ", "))
			}
			fromName, to = data.Repo+" notes, now", data.Check.Merge.State.Notes
			from = data.Check.Live.Notes
			fromArchive, toArchive = data.Check.Live.Archive(), data.Check.Merge.State.Archive()
		default:
			var where []string
			if len(m.NotesConflicts) > 0 {
				where = append(where, "the notes ("+strings.Join(m.NotesConflicts, ", ")+fmt.Sprintf(" of version %d)", base))
			}
			if len(m.Conflicts) > 0 {
				where = append(where, "harness "+strings.Join(m.Conflicts, ", "))
			}
			fmt.Fprintf(w, "Its changes conflict with theirs in %s, so it cannot be applied; below is what it changed in version %d.\n",
				strings.Join(where, " and "), base)
		}
	}
	if len(data.NotPreserved) > 0 {
		fmt.Fprintf(w, "CANNOT BE APPLIED: it would lose what the notes hold (curation organizes the notes and never loses a line):\n")
		for _, pr := range data.NotPreserved {
			fmt.Fprintf(w, "  - %s\n", notes.Printable(pr))
		}
	}
	b, a := data.Before, data.After
	archive := ""
	if b.ArchiveBytes > 0 || a.ArchiveBytes > 0 {
		archive = fmt.Sprintf("; archive: %d → %d bytes", b.ArchiveBytes, a.ArchiveBytes)
	}
	fmt.Fprintf(w, "notes: %d → %d bytes, %d → %d lines; harness: %d → %d files, %d → %d bytes%s\n\n",
		b.Bytes, a.Bytes, b.Lines, a.Lines, b.HarnessFiles, a.HarnessFiles, b.HarnessBytes, a.HarnessBytes, archive)
	toName := fmt.Sprintf("%s notes, proposal %d", data.Repo, p.ID)
	if data.Stale && data.Merge.Clean {
		toName += " merged"
	}
	notesPrintDiff(w, color, fromName, toName, from, to)
	if !bytes.Equal(fromArchive, toArchive) {
		fmt.Fprintln(w)
		notesPrintDiff(w, color, strings.Replace(fromName, " notes,", " archive,", 1), strings.Replace(toName, " notes,", " archive,", 1), fromArchive, toArchive)
	}
	fmt.Fprintln(w)
	var added, removed, changed []string
	reasons := map[string]notes.Change{}
	for _, h := range data.Harness {
		switch h.Change {
		case "added":
			added = append(added, h.Name)
		case "removed":
			removed = append(removed, h.Name)
		case "changed":
			changed = append(changed, h.Name)
		}
		if h.Action != "" || h.Reason != "" {
			reasons[h.Name] = notes.Change{Name: h.Name, Action: h.Action, Into: h.Into, Reason: h.Reason}
		}
	}
	notesPrintHarness(w, color, added, removed, changed, maps.Clone(reasons))
	if len(data.Sections) > 0 {
		fmt.Fprintln(w, "sections:")
		for _, s := range data.Sections {
			line := "  " + notes.Printable(s.Name) + ": " + s.Action
			if s.Into != "" {
				line += " into " + notes.Printable(s.Into)
			}
			fmt.Fprintln(w, line+": "+notes.Printable(s.Reason))
		}
	}
	notesPrintMisses(w, data.Misses)
	fmt.Fprintln(w)
}

// notesPrintMisses lists the misses a proposal was given (the retro's,
// for the repository's notes) with what it did with each: noted in a
// section, or skipped and why.
func notesPrintMisses(w io.Writer, ms []engine.NotesReviewMiss) {
	if len(ms) == 0 {
		return
	}
	noted := 0
	for _, m := range ms {
		if m.Outcome == store.MissNoted {
			noted++
		}
	}
	fmt.Fprintf(w, "misses: %d noted, %d skipped\n", noted, len(ms)-noted)
	for _, m := range ms {
		line := "  " + strconv.FormatInt(m.ID, 10)
		for _, s := range []string{m.Severity, m.Where, m.Title} {
			if s = notes.Printable(s); s != "" {
				line += " " + s
			}
		}
		switch m.Outcome {
		case store.MissNoted:
			line += " → noted in " + notes.Printable(m.Section)
		default:
			line += " → skipped: " + notes.Printable(m.Reason)
		}
		fmt.Fprintln(w, line)
	}
}

// notesApply applies proposal p as the operator decided it
// (engine.ApplyNotesProposal): the live notes must still be live (their
// fingerprint when the review read them) and the state to write the one the
// review showed (target, its fingerprint). A refusal says why: the notes
// changed (errNotesChanged), someone decided the proposal meanwhile (named),
// or it would lose a line of the notes.
func notesApply(ctx context.Context, st *store.Store, nr notes.Repo, p store.NotesProposal, live, target string, now time.Time) (store.NotesProposal, error) {
	applied, err := engine.ApplyNotesProposal(ctx, st, nr, p, engine.NotesApply{Live: live, Target: target, By: store.DecidedByOperator,
		Now: now, Wait: max(notesLockWait, time.Nanosecond)})
	if errors.Is(err, engine.ErrNotesChanged) || errors.Is(err, engine.ErrTargetChanged) {
		return applied, errNotesChanged
	}
	return applied, err
}

// notesDecided words a decision the registry refused because the proposal
// no longer waits: who decided it (engine.ProposalDecidedError).
func notesDecided(ctx context.Context, d *actDeps, id int64, err error) error {
	if !errors.Is(err, store.ErrConflict) {
		return err
	}
	if cur, rerr := d.Store.NotesProposalByID(ctx, id); rerr == nil {
		return &engine.ProposalDecidedError{Proposal: cur}
	}
	return err
}
