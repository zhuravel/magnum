package cli

// `magnum notes <repo> --curate | --review | --restore <version>`: a
// curation proposal (engine/notes_curate.go) or a restore of an earlier
// version waits in the registry for the operator; --review shows it and asks
// y/N. y applies it under the notes lock, with the protocol the judges use
// (agents.NotesLockLine): the live notes are read again and must still be
// what the review showed; n rejects it with --reason, which the next
// curation reads. A stale proposal (the notes changed since it was made,
// engine.CheckStale) says so first: when its changes and the notes' merge,
// the review shows the merge and y applies it; when they conflict, or the
// operator answers c, y asks the daemon for a new curation from the notes
// now that reads the stale proposal, which is kept as superseded. Nothing a
// proposal removes is lost: the registry keeps every version and every
// proposal. A curation given the retro's misses shows what it did with each;
// the decision moves them (store.DecideNotesProposal): used when applied,
// back to new when rejected, dismissed after a second rejection.

import (
	"context"
	"encoding/json"
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
var errNotesChanged = errors.New("the notes changed while the proposal was shown; `--review` again shows it merged with their changes")

// notesCurateJSON is what --curate --json prints.
type notesCurateJSON struct {
	Payload engine.NotesCuratePayload `json:"payload"`
	Request *actRequestJSON           `json:"request,omitempty"`
	Error   string                    `json:"error,omitempty"`
}

// notesCurate asks the daemon for a curation now (engine.ReqNotesCurate);
// supersede names the stale proposal it follows up on (0 = none).
func notesCurate(ctx context.Context, c *Context, d *actDeps, f notesFlags, full string, supersede int64) int {
	out := notesCurateJSON{Payload: engine.NotesCuratePayload{Repo: full, Supersede: supersede}}
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

// notesHarnessChange is one harness file of a proposal: how it differs from
// the base, and what the curator said of it.
type notesHarnessChange struct {
	Name   string `json:"name"`
	Change string `json:"change"` // added | removed | changed | kept
	Action string `json:"action,omitempty"`
	Into   string `json:"into,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// notesReviewMiss is a miss a proposal was given, and what it did with it.
type notesReviewMiss struct {
	ID       int64  `json:"id"`
	Severity string `json:"severity,omitempty"`
	Where    string `json:"where,omitempty"`
	Title    string `json:"title,omitempty"`
	Outcome  string `json:"outcome"` // store.MissNoted | store.MissSkipped
	Section  string `json:"section,omitempty"`
	Reason   string `json:"reason,omitempty"`
	State    string `json:"state"` // the miss's state now
}

// notesReviewData is a proposal as --review shows it (and --json prints).
type notesReviewData struct {
	Repo     string               `json:"repo"`
	Proposal store.NotesProposal  `json:"proposal"`
	Diff     string               `json:"notes_diff"`
	Harness  []notesHarnessChange `json:"harness"`
	Sections []notes.Change       `json:"sections"`
	Misses   []notesReviewMiss    `json:"misses"`
	Before   notes.Size           `json:"size_before"`
	After    notes.Size           `json:"size_after"`
	Expired  bool                 `json:"expired,omitempty"` // older than engine.ProposalTTL: it can no longer be applied
	// Stale: the notes changed since the proposal was made; Merge is its
	// three-way merge with their changes, and when it is clean the diff,
	// the harness changes and the sizes are those of the merge applied to
	// the notes now.
	Stale bool            `json:"stale"`
	Merge *notesMergeView `json:"merge,omitempty"`

	base, proposed, live notes.State
	merged               notes.State
	changes              notes.Changes
}

// notesMergeView is a stale proposal's merge with the notes' changes since.
type notesMergeView struct {
	Clean          bool     `json:"clean"`
	NotesConflicts []string `json:"notes_conflicts,omitempty"`   // the base version's lines both changed differently
	Conflicts      []string `json:"harness_conflicts,omitempty"` // harness files both changed in ways that do not merge
	Kept           []string `json:"harness_kept,omitempty"`      // files the proposal deletes that a round changed since: kept
	Merged         []string `json:"harness_merged,omitempty"`    // files both changed whose texts merged
}

// notesReview reviews the newest proposal waiting for the repository.
func notesReview(ctx context.Context, c *Context, d *actDeps, f notesFlags, repo store.Repo, nr notes.Repo) int {
	ps, err := d.Store.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repo.ID, States: []string{store.ProposalPending}, Limit: 1})
	if err != nil {
		return cmdFail(c, "notes", err)
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
// no record of them), and reviews it as a curation.
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
	base, _, err := d.Store.RecordNotesVersion(ctx, store.NotesVersionInput{RepoID: repo.ID, Source: store.NotesFromImport,
		Content: engine.ContentOf(live), Dedupe: true})
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	if notesFingerprint(base) == notesFingerprint(v) {
		fmt.Fprintf(c.Stderr, "the notes of %s hold version %d already\n", full, v.ID)
		return 0
	}
	p, err := d.Store.CreateNotesProposal(ctx, store.NotesProposalInput{RepoID: repo.ID, Kind: store.ProposalRestore, Trigger: "restore",
		BaseVersionID: base.ID, VersionID: v.ID, State: store.ProposalPending})
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	return notesReviewProposal(ctx, c, d, f, repo, nr, p)
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
		if _, err := d.Store.DecideNotesProposal(ctx, p.ID, []string{store.ProposalPending}, store.ProposalExpired,
			"not reviewed within "+engine.ProposalTTL.String(), notesNow(d), nil); err != nil {
			return cmdFail(c, "notes", err)
		}
		return cmdFail(c, "notes", fmt.Errorf("proposal %d is older than 7 days and expired; `magnum notes %s --curate` asks for a new one", p.ID, full))
	}
	curation := p.Kind == store.ProposalCuration
	conflict := data.Stale && !data.Merge.Clean
	switch {
	case conflict && curation:
		fmt.Fprintf(c.Stdout, "Ask for a new curation of the notes of %s? y asks for one from the notes now, which reads proposal %d (kept as superseded), "+
			"n rejects it, anything else leaves it for later [y/N] ", full, p.ID)
	case conflict:
		fmt.Fprintf(c.Stdout, "Proposal %d cannot be applied. y expires it (`--restore %d` proposes the version again from the notes now), "+
			"n rejects it, anything else leaves it for later [y/N] ", p.ID, store.Deref(p.VersionID))
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
	case curation && ((conflict && (a == "y" || a == "yes")) || (data.Stale && a == "c")):
		return notesCurate(ctx, c, d, f, full, p.ID)
	case (a == "y" || a == "yes") && conflict:
		if _, err := d.Store.DecideNotesProposal(ctx, p.ID, []string{store.ProposalPending}, store.ProposalExpired,
			"the notes changed since the restore was proposed, and the changes conflict", notesNow(d), nil); err != nil {
			return cmdFail(c, "notes", err)
		}
		return cmdFail(c, "notes", fmt.Errorf("proposal %d conflicts with the notes' changes since and expired; `magnum notes %s --restore %d` "+
			"proposes the version again from the notes now", p.ID, full, store.Deref(p.VersionID)))
	case a == "y" || a == "yes":
		target := data.proposed
		if data.Stale {
			target = data.merged
		}
		applied, err := notesApply(ctx, d.Store, nr, p, data.live.Fingerprint(), engine.ContentOf(target), data.Stale, notesNow(d))
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
		if _, err := d.Store.DecideNotesProposal(ctx, p.ID, []string{store.ProposalPending}, store.ProposalRejected, reason, notesNow(d), nil); err != nil {
			return cmdFail(c, "notes", err)
		}
		msg := fmt.Sprintf("rejected proposal %d", p.ID)
		if reason != "" && p.Kind == store.ProposalCuration {
			msg += "; the next curation reads why"
		}
		fmt.Fprintln(c.Stdout, msg+notesMissesMoved(ctx, d, p.ID))
	default:
		fmt.Fprintf(c.Stdout, "left for later: `magnum notes %s --review`\n", full)
	}
	return 0
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

// notesReviewOf gathers a proposal's review: its base and proposed states,
// the notes now and, when they are no longer its base (stale), its merge
// with their changes since; the notes' diff, the harness changes with the
// curator's reasons, the sections, the misses it was given with what it did
// with them and the sizes before and after: of the proposal against its
// base, or of a clean merge against the notes now.
func notesReviewOf(ctx context.Context, d *actDeps, full string, nr notes.Repo, p store.NotesProposal) (notesReviewData, error) {
	data := notesReviewData{Repo: full, Proposal: p, Harness: []notesHarnessChange{}, Sections: []notes.Change{}, Misses: []notesReviewMiss{}}
	check, err := engine.CheckStale(ctx, d.Store, nr, p)
	if err != nil {
		return data, err
	}
	data.base, data.proposed, data.live, data.Stale = check.Base, check.Proposed, check.Live, check.Stale
	if len(p.Changes) > 0 {
		if err := json.Unmarshal(p.Changes, &data.changes); err != nil {
			return data, fmt.Errorf("proposal %d: changes: %w", p.ID, err)
		}
		data.Sections = append(data.Sections, data.changes.Sections...)
	}
	from, to := data.base, data.proposed
	fromName := "now"
	if m := check.Merge; data.Stale {
		data.Merge = &notesMergeView{Clean: m.Clean, Conflicts: m.Conflicts, Kept: m.Kept, Merged: m.Merged}
		for _, c := range m.NotesConflicts {
			data.Merge.NotesConflicts = append(data.Merge.NotesConflicts, notesLinesText(c))
		}
		if m.Clean {
			data.merged, from, to = m.State, data.live, m.State
		} else {
			fromName = fmt.Sprintf("version %d", store.Deref(p.BaseVersionID))
		}
	}
	maxLine := 0
	if d.Cfg != nil {
		maxLine = d.Cfg.Notes.MaxLine
	}
	data.Before, data.After = from.Size(maxLine), to.Size(maxLine)
	data.Diff = notes.Unified(fromName, fmt.Sprintf("proposal %d", p.ID), string(from.Notes), string(to.Notes), 3)
	reasons := map[string]notes.Change{}
	for _, ch := range data.changes.Files {
		reasons[ch.Name] = ch
	}
	kept := map[string]bool{}
	if data.Merge != nil && data.Merge.Clean {
		for _, n := range data.Merge.Kept {
			kept[n] = true
		}
	}
	added, removed, changed := notes.HarnessDelta(from.Listing(), to.Listing())
	seen := map[string]bool{}
	add := func(name, change string) {
		seen[name] = true
		ch := reasons[name]
		if kept[name] {
			ch = notes.Change{Action: notes.ActionKept, Reason: "the proposal deletes it, but a round changed it since: kept as the round left it"}
		}
		data.Harness = append(data.Harness, notesHarnessChange{Name: name, Change: change, Action: ch.Action, Into: ch.Into, Reason: ch.Reason})
	}
	for _, n := range removed {
		add(n, "removed")
	}
	for _, n := range added {
		add(n, "added")
	}
	for _, n := range changed {
		add(n, "changed")
	}
	for _, b := range to.Files {
		if !seen[b.Path] {
			add(b.Path, "kept")
		}
	}
	links, err := d.Store.ProposalMisses(ctx, p.ID)
	if err != nil {
		return data, err
	}
	for _, l := range links {
		m := *l.Miss
		data.Misses = append(data.Misses, notesReviewMiss{ID: m.ID, Severity: m.Severity, Where: missesPlace(m), Title: m.Title,
			Outcome: l.Outcome, Section: l.Section, Reason: l.Reason, State: m.State})
	}
	return data, nil
}

// notesLinesText names a conflict's lines of the base version, 1-based:
// "lines 3-5", "line 3", or "at line 3" for lines both sides inserted there.
func notesLinesText(c notes.MergeConflict) string {
	switch {
	case c.End > c.Start+1:
		return fmt.Sprintf("lines %d-%d", c.Start+1, c.End)
	case c.End > c.Start:
		return fmt.Sprintf("line %d", c.Start+1)
	}
	return fmt.Sprintf("at line %d", c.Start+1)
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
	from, to := data.base.Notes, data.proposed.Notes
	if m := data.Merge; data.Stale {
		fmt.Fprintf(w, "STALE: the notes changed since the proposal was made from version %d.\n", base)
		switch {
		case m.Clean:
			fmt.Fprintln(w, "Its changes and theirs merge: below is the merge, as it would change the notes now.")
			if len(m.Kept) > 0 {
				fmt.Fprintf(w, "The proposal deletes %s, which a round changed since: kept as the round left it.\n", strings.Join(m.Kept, ", "))
			}
			fromName, to = data.Repo+" notes, now", data.merged.Notes
			from = data.live.Notes
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
	b, a := data.Before, data.After
	fmt.Fprintf(w, "notes: %d → %d bytes, %d → %d lines; harness: %d → %d files, %d → %d bytes\n\n",
		b.Bytes, a.Bytes, b.Lines, a.Lines, b.HarnessFiles, a.HarnessFiles, b.HarnessBytes, a.HarnessBytes)
	toName := fmt.Sprintf("%s notes, proposal %d", data.Repo, p.ID)
	if data.Stale && data.Merge.Clean {
		toName += " merged"
	}
	notesPrintDiff(w, color, fromName, toName, from, to)
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
func notesPrintMisses(w io.Writer, ms []notesReviewMiss) {
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

// notesApply applies proposal p under the notes lock: the live notes are
// read again and must still be what the review showed (live, their
// fingerprint), else nothing is written and errNotesChanged asks for another
// review; then the notes and the harness become target (p's state, or for a
// stale p its merge with the notes' changes since, whose live state is
// recorded first, so the history keeps it) and the registry records target
// as a version (a curation's, or the operator's for a restore) linked to p.
func notesApply(ctx context.Context, st *store.Store, nr notes.Repo, p store.NotesProposal, live string, target store.NotesContent, stale bool, now time.Time) (store.NotesProposal, error) {
	unlock, err := notes.Lock(ctx, nr.Lock(), notesLockWait)
	if err != nil {
		return p, fmt.Errorf("the notes lock: %w", err)
	}
	defer unlock()
	state, err := notes.ReadState(nr)
	if err != nil {
		return p, err
	}
	if state.Fingerprint() != live {
		return p, errNotesChanged
	}
	if stale {
		if _, _, err := st.RecordNotesVersion(ctx, store.NotesVersionInput{RepoID: p.RepoID, Source: store.NotesFromImport,
			Content: engine.ContentOf(state), Dedupe: true}); err != nil {
			return p, err
		}
	}
	if err := notes.WriteState(nr, engine.StateOf(target)); err != nil {
		return p, err
	}
	source := store.NotesFromCuration
	if p.Kind == store.ProposalRestore {
		source = store.NotesFromHuman
	}
	// Dedupe: a curation that only skips the misses it was given leaves the
	// notes as they are, and the version they hold is not recorded again.
	return st.DecideNotesProposal(ctx, p.ID, []string{store.ProposalPending}, store.ProposalApplied, "", now,
		&store.NotesVersionInput{Source: source, Content: target, Dedupe: true})
}
