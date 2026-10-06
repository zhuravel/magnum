package cli

// `magnum notes <repo> --curate | --review | --restore <version>`: a
// curation proposal (engine/notes_curate.go) or a restore of an earlier
// version waits in the registry for the operator; --review shows it and asks
// y/N. y applies it under the notes lock, with the protocol the judges use
// (agents.NotesLockLine): the live notes are read again and must still be
// what the proposal started from; n rejects it with --reason, which the next
// curation reads. Nothing a proposal removes is lost: the registry keeps
// every version and every proposal. A curation given the retro's misses
// shows what it did with each; the decision moves them (store.
// DecideNotesProposal): used when applied, back to new when rejected,
// dismissed after a second rejection.

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

// errNotesChanged refuses an apply when the live notes are no longer the
// proposal's base.
var errNotesChanged = errors.New("notes changed since the proposal; run --curate again")

// notesCurateJSON is what --curate --json prints.
type notesCurateJSON struct {
	Payload engine.NotesCuratePayload `json:"payload"`
	Request *actRequestJSON           `json:"request,omitempty"`
	Error   string                    `json:"error,omitempty"`
}

// notesCurate asks the daemon for a curation now (engine.ReqNotesCurate).
func notesCurate(ctx context.Context, c *Context, d *actDeps, f notesFlags, full string) int {
	out := notesCurateJSON{Payload: engine.NotesCuratePayload{Repo: full}}
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

	base, proposed notes.State
	changes        notes.Changes
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
// applies it, n rejects it with --reason, anything else leaves it waiting.
// --json prints the data and asks nothing.
func notesReviewProposal(ctx context.Context, c *Context, d *actDeps, f notesFlags, repo store.Repo, nr notes.Repo, p store.NotesProposal) int {
	full := repo.FullName()
	data, err := notesReviewOf(ctx, d, full, p)
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
	fmt.Fprintf(c.Stdout, "Apply proposal %d to the notes of %s? y applies it, n rejects it, anything else leaves it for later [y/N] ", p.ID, full)
	answer, err := d.readLine(ctx)
	if err != nil {
		fmt.Fprintln(c.Stdout)
		if ctx.Err() != nil {
			return 130
		}
		answer = ""
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		applied, err := notesApply(ctx, d.Store, nr, p, notesNow(d))
		if err != nil {
			return cmdFail(c, "notes", err)
		}
		fmt.Fprintf(c.Stdout, "applied: the notes of %s are version %d%s\n", full, store.Deref(applied.AppliedVersionID), notesMissesMoved(ctx, d, p.ID))
	case "n", "no":
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
// the notes' diff, the harness changes with the curator's reasons, the
// sections, the misses it was given with what it did with them and the
// sizes before and after.
func notesReviewOf(ctx context.Context, d *actDeps, full string, p store.NotesProposal) (notesReviewData, error) {
	data := notesReviewData{Repo: full, Proposal: p, Harness: []notesHarnessChange{}, Sections: []notes.Change{}, Misses: []notesReviewMiss{}}
	if p.VersionID == nil {
		return data, fmt.Errorf("proposal %d has no proposed state", p.ID)
	}
	if p.BaseVersionID != nil {
		c, err := d.Store.NotesVersionContent(ctx, *p.BaseVersionID)
		if err != nil {
			return data, err
		}
		data.base = engine.StateOf(c)
	}
	c, err := d.Store.NotesVersionContent(ctx, *p.VersionID)
	if err != nil {
		return data, err
	}
	data.proposed = engine.StateOf(c)
	if len(p.Changes) > 0 {
		if err := json.Unmarshal(p.Changes, &data.changes); err != nil {
			return data, fmt.Errorf("proposal %d: changes: %w", p.ID, err)
		}
		data.Sections = append(data.Sections, data.changes.Sections...)
	}
	maxLine := 0
	if d.Cfg != nil {
		maxLine = d.Cfg.Notes.MaxLine
	}
	data.Before, data.After = data.base.Size(maxLine), data.proposed.Size(maxLine)
	data.Diff = notes.Unified("now", fmt.Sprintf("proposal %d", p.ID), string(data.base.Notes), string(data.proposed.Notes), 3)
	reasons := map[string]notes.Change{}
	for _, ch := range data.changes.Files {
		reasons[ch.Name] = ch
	}
	added, removed, changed := notes.HarnessDelta(data.base.Listing(), data.proposed.Listing())
	seen := map[string]bool{}
	add := func(name, change string) {
		seen[name] = true
		ch := reasons[name]
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
	for _, b := range data.proposed.Files {
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
	b, a := data.Before, data.After
	fmt.Fprintf(w, "notes: %d → %d bytes, %d → %d lines; harness: %d → %d files, %d → %d bytes\n\n",
		b.Bytes, a.Bytes, b.Lines, a.Lines, b.HarnessFiles, a.HarnessFiles, b.HarnessBytes, a.HarnessBytes)
	notesPrintDiff(w, color, fmt.Sprintf("%s notes, version %d (when the proposal was made)", data.Repo, store.Deref(p.BaseVersionID)),
		fmt.Sprintf("%s notes, proposal %d", data.Repo, p.ID), data.base.Notes, data.proposed.Notes)
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
// read again and must still be what p started from (else p expires and
// errNotesChanged says so); then the notes and the harness become p's state
// and the registry records it as a version (a curation's, or the
// operator's for a restore) linked to p.
func notesApply(ctx context.Context, st *store.Store, nr notes.Repo, p store.NotesProposal, now time.Time) (store.NotesProposal, error) {
	if p.VersionID == nil {
		return p, fmt.Errorf("proposal %d has no proposed state", p.ID)
	}
	unlock, err := notes.Lock(ctx, nr.Lock(), notesLockWait)
	if err != nil {
		return p, fmt.Errorf("the notes lock: %w", err)
	}
	defer unlock()
	live, err := notes.ReadState(nr)
	if err != nil {
		return p, err
	}
	var base notes.State
	if p.BaseVersionID != nil {
		c, err := st.NotesVersionContent(ctx, *p.BaseVersionID)
		if err != nil {
			return p, err
		}
		base = engine.StateOf(c)
	}
	if live.Fingerprint() != base.Fingerprint() {
		if _, err := st.DecideNotesProposal(ctx, p.ID, []string{store.ProposalPending}, store.ProposalExpired,
			"the notes changed since the proposal", now, nil); err != nil {
			return p, errors.Join(errNotesChanged, err)
		}
		return p, errNotesChanged
	}
	content, err := st.NotesVersionContent(ctx, *p.VersionID)
	if err != nil {
		return p, err
	}
	if err := notes.WriteState(nr, engine.StateOf(content)); err != nil {
		return p, err
	}
	source := store.NotesFromCuration
	if p.Kind == store.ProposalRestore {
		source = store.NotesFromHuman
	}
	// Dedupe: a curation that only skips the misses it was given leaves the
	// notes as they are, and the version they hold is not recorded again.
	return st.DecideNotesProposal(ctx, p.ID, []string{store.ProposalPending}, store.ProposalApplied, "", now,
		&store.NotesVersionInput{Source: source, Content: content, Dedupe: true})
}
