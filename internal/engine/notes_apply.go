package engine

// Applying a notes proposal (DECISIONS "A notes proposal is applied by a
// claim, a write and a record"), for every decider: the operator's y in
// `magnum notes <repo> --review` and the notes critic's apply. Under the
// notes lock the apply reads the notes again and refuses when they are not
// what the decider saw or the state to write is not the one it approved;
// runs the preservation check of curation (notes.CheckPreserved) on what it
// would write; claims the proposal with a compare-and-set (pending becomes
// applying, store.ClaimNotesProposal); writes the notes and the harness;
// and records the proposal applied with its version. A decider that lost a
// race is refused before it writes anything, naming who decided. A crash
// between the claim and the record leaves the proposal applying: the next
// daemon start (and every curation scan after it) finishes the apply when
// the files hold the state it wrote, and reverts the claim when they hold
// the state it found (RecoverNotesApply).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// Errors of ApplyNotesProposal.
var (
	// ErrNotesChanged: the notes are not what the decider saw.
	ErrNotesChanged = errors.New("the notes changed since the proposal was reviewed")
	// ErrTargetChanged: the state the apply would write is not the one the
	// decider approved (its fingerprint differs).
	ErrTargetChanged = errors.New("the state to write is not the one that was approved")
	// ErrStaleConflict: the notes changed since the proposal was made and
	// its changes conflict with theirs; it cannot be applied.
	ErrStaleConflict = errors.New("the notes changed since the proposal was made, and the changes conflict")
)

// ProposalDecidedError refuses an apply of a proposal that no longer waits:
// another decider applied or rejected it, or claimed it first.
type ProposalDecidedError struct{ Proposal store.NotesProposal }

func (e *ProposalDecidedError) Error() string {
	p := e.Proposal
	what := p.State
	switch p.State {
	case store.ProposalApplying:
		what = "being applied"
	case store.ProposalApplied, store.ProposalRejected, store.ProposalExpired, store.ProposalSuperseded:
		what = "already " + p.State
	}
	by := ""
	if p.DecidedBy != "" {
		by = " by " + DeciderName(p.DecidedBy)
	}
	at := ""
	if p.DecidedAt != nil {
		at = " at " + p.DecidedAt.Local().Format("2006-01-02 15:04")
	}
	return fmt.Sprintf("proposal %d is %s%s%s", p.ID, what, by, at)
}

// DeciderName says who decided a proposal (store.DecidedBy*), for a
// sentence: "you", "the critic", "magnum".
func DeciderName(by string) string {
	switch by {
	case store.DecidedByOperator:
		return "you"
	case store.DecidedByCritic:
		return "the critic"
	}
	return by
}

// NotPreservedError refuses an apply that would lose lines of the notes
// (notes.CheckPreserved): such a proposal can only be rejected or curated
// again.
type NotPreservedError struct {
	ID       int64
	Problems []string
}

func (e *NotPreservedError) Error() string {
	return fmt.Sprintf("proposal %d would lose what the notes hold, so it cannot be applied (reject it, or curate again): %s",
		e.ID, strings.Join(e.Problems, "; "))
}

// NotesApply is what a decider approved: the fingerprints of the notes it
// saw and of the state to write, and who it is.
type NotesApply struct {
	Live   string // the notes' fingerprint when the decider read them
	Target string // the fingerprint of the state it approved to write
	By     string // store.DecidedBy*
	// Reason is the decider's one-line reason, kept with the proposal (the
	// critic's; "" = none).
	Reason string
	Now    time.Time
	// Wait bounds the wait for the notes lock (0 = NotesLockWait).
	Wait time.Duration
}

// WaitingStates are the states of a proposal that waits for a decision or
// is being applied: a repository with one gets no new curation or recovery.
var WaitingStates = []string{store.ProposalPending, store.ProposalApplying}

// waiting reports whether a proposal in state waits (WaitingStates).
func waiting(state string) bool {
	return state == store.ProposalPending || state == store.ProposalApplying
}

// applyCrash, when set (tests), is called after each step of an apply
// ("claimed", "written"); an error it returns ends the apply there, as a
// crash would: nothing after it runs, not even the claim's release.
var applyCrash func(step string) error

// ApplyTarget is what applying p writes to the notes now (check): its
// proposed state, or when it is stale its merge with the notes' changes
// since; false when that merge conflicts.
func ApplyTarget(check StaleCheck) (notes.State, bool) {
	if !check.Stale {
		return check.Proposed, true
	}
	return check.Merge.State, check.Merge.Clean
}

// PreservationProblems is notes.CheckPreserved of applying p over live
// (target, ApplyTarget's): nil for a restore, which brings back a recorded
// version the operator chose (the registry keeps the state it replaces).
func PreservationProblems(p store.NotesProposal, live, target notes.State) ([]string, error) {
	if p.Kind != store.ProposalCuration {
		return nil, nil
	}
	var ch notes.Changes
	if len(p.Changes) > 0 {
		if err := json.Unmarshal(p.Changes, &ch); err != nil {
			return nil, fmt.Errorf("proposal %d: changes: %w", p.ID, err)
		}
	}
	return notes.CheckPreserved(live, target, ch), nil
}

// ApplyNotesProposal applies proposal id (p, as the decider reviewed it)
// to nr's notes, decided by a.By, under the notes lock: the proposal must
// still be pending (else ProposalDecidedError), the notes must be what the
// decider saw (ErrNotesChanged), the state to write the one it approved
// (ErrTargetChanged) and a clean merge when the proposal is stale
// (ErrStaleConflict), and a curation must lose no line (NotPreservedError).
// Then the claim, the write and the record: the notes as found are recorded
// first (an import, when the registry did not hold them), and the state
// written is recorded as a version (a curation's, or the operator's for a
// restore) linked to the proposal. A write that fails is settled by what
// the notes hold afterwards (settleFailedWrite): as found, the claim is
// released and the proposal waits again; as meant to be written, the apply
// goes on; any other state, the proposal stays applying for the recovery.
func ApplyNotesProposal(ctx context.Context, st *store.Store, nr notes.Repo, p store.NotesProposal, a NotesApply) (store.NotesProposal, error) {
	wait := a.Wait
	if wait <= 0 {
		wait = NotesLockWait
	}
	if a.Now.IsZero() {
		a.Now = time.Now()
	}
	unlock, err := notes.Lock(ctx, nr.Lock(), wait)
	if err != nil {
		return p, fmt.Errorf("the notes lock: %w", err)
	}
	defer unlock()
	cur, err := st.NotesProposalByID(ctx, p.ID)
	if err != nil {
		return p, err
	}
	if cur.State != store.ProposalPending {
		return cur, &ProposalDecidedError{Proposal: cur}
	}
	live, err := notes.ReadState(nr)
	if err != nil {
		return cur, err
	}
	if live.Fingerprint() != a.Live {
		return cur, ErrNotesChanged
	}
	check, err := checkStaleWith(ctx, st, cur, live)
	if err != nil {
		return cur, err
	}
	target, ok := ApplyTarget(check)
	switch {
	case !ok:
		return cur, ErrStaleConflict
	case target.Fingerprint() != a.Target:
		return cur, ErrTargetChanged
	}
	problems, err := PreservationProblems(cur, live, target)
	if err != nil {
		return cur, err
	}
	if len(problems) > 0 {
		return cur, &NotPreservedError{ID: cur.ID, Problems: problems}
	}
	subject := notesSubject(nr)
	claimed, err := st.ClaimNotesProposal(ctx, cur.ID, store.NotesClaim{By: a.By, Prior: ContentOf(live), Target: target.Fingerprint(), At: a.Now})
	if errors.Is(err, store.ErrConflict) {
		if again, rerr := st.NotesProposalByID(context.WithoutCancel(ctx), cur.ID); rerr == nil {
			return again, &ProposalDecidedError{Proposal: again}
		}
	}
	if err != nil {
		return cur, err
	}
	if err := crashAt("claimed"); err != nil {
		return claimed, err
	}
	if err := writeTarget(ctx, st, nr, claimed, target); err != nil {
		if err = settleFailedWrite(ctx, st, nr, claimed, live, target, err); err != nil {
			return claimed, err
		}
	}
	if err := crashAt("written"); err != nil {
		return claimed, err
	}
	applied, err := finishApply(ctx, st, claimed, target, a.Reason, a.Now)
	if err != nil {
		return claimed, fmt.Errorf("record the apply (the next daemon start does): %w", err)
	}
	notesEvent(ctx, st, "info", subject, "notes.proposal_applied", fmt.Sprintf("proposal %d applied to the notes of %s by %s: version %d",
		cur.ID, nr.FullName(), DeciderName(a.By), store.Deref(applied.AppliedVersionID)),
		map[string]any{"proposal": cur.ID, "by": a.By, "version": store.Deref(applied.AppliedVersionID), "stale": check.Stale})
	return applied, nil
}

// writeNotesState writes the notes and the harness of an apply; tests
// replace it to fail a write at the step they choose.
var writeNotesState = notes.WriteStateReport

// writeTarget writes target for claimed proposal p (an apply's, or a
// recovery's completion of one) and tells the old harness it could not
// remove: a write that stands, with a directory next to the harness that
// the reconcile removes once it is an hour old.
func writeTarget(ctx context.Context, st *store.Store, nr notes.Repo, p store.NotesProposal, target notes.State) error {
	left, err := writeNotesState(nr, target)
	if left != nil {
		notesEvent(ctx, st, "warn", notesSubject(nr), "notes.write_leftover", fmt.Sprintf("the write for proposal %d of %s is done, but its old harness stays at %s "+
			"(%s); the reconcile removes it once it is an hour old", p.ID, nr.FullName(), left.Path, oneLine(left.Err.Error(), retroWhyRunes)),
			map[string]any{"proposal": p.ID, "by": p.DecidedBy})
	}
	return err
}

// claimHolds is what the notes on disk hold against a claim: the one fact
// an apply whose write failed and the recovery of a cut-short apply both
// decide by (whatTheNotesHold).
type claimHolds int

const (
	// holdsTarget: the state the claim meant to write; the write is done.
	holdsTarget claimHolds = iota + 1
	// holdsPrior: the state the claim found; nothing was written.
	holdsPrior
	// holdsHalf: the notes of one and the harness of the other; the write
	// stopped between its two steps (WriteState swaps the harness first).
	holdsHalf
	// holdsOther: neither; someone wrote after the claim, or the write
	// left a state that is none of these.
	holdsOther
)

// whatTheNotesHold compares live with the state a claim found (prior) and
// meant to write (target). The target comes first: a claim that had
// nothing to change is finished, whatever else it found. Every decision on
// a claim's notes goes through here, so the apply and the recovery cannot
// disagree.
func whatTheNotesHold(live, prior, target notes.State) claimHolds {
	lfp := live.Fingerprint()
	switch {
	case lfp == target.Fingerprint():
		return holdsTarget
	case lfp == prior.Fingerprint():
		return holdsPrior
	case mixedState(live, prior, target):
		return holdsHalf
	}
	return holdsOther
}

// settleFailedWrite is what an apply does when its write failed with err:
// it reads the notes again and decides as the recovery does
// (whatTheNotesHold, against the notes it found, prior, and the state it
// meant to write). The notes as found: the claim is released and the
// proposal waits again (notes.apply_failed). The state it meant to write,
// whatever the error says: nil, the apply goes on to its record
// (notes.write_error). Anything else, a write cut short, or notes that
// cannot be read: the proposal stays applying with its claim and the
// recovery settles it (notes.apply_unsettled); releasing it would hide a
// half-written state from the recovery.
func settleFailedWrite(ctx context.Context, st *store.Store, nr notes.Repo, claimed store.NotesProposal, prior, target notes.State, err error) error {
	bctx := context.WithoutCancel(ctx)
	subject, full := notesSubject(nr), nr.FullName()
	data := map[string]any{"proposal": claimed.ID, "by": claimed.DecidedBy}
	var held claimHolds // 0: the notes cannot be read
	live, rerr := notes.ReadState(nr)
	if rerr == nil {
		held = whatTheNotesHold(live, prior, target)
	}
	switch held {
	case holdsTarget:
		notesEvent(bctx, st, "warn", subject, "notes.write_error", fmt.Sprintf("the write for proposal %d of %s reported an error, but the notes hold the state "+
			"it meant to write, so it is recorded applied: %s", claimed.ID, full, oneLine(err.Error(), retroWhyRunes)), data)
		return nil
	case holdsPrior:
		if _, rel := st.ReleaseNotesClaim(bctx, claimed.ID); rel != nil {
			err = errors.Join(err, fmt.Errorf("release the claim (the next daemon start does): %w", rel))
		}
		notesEvent(bctx, st, "warn", subject, "notes.apply_failed", fmt.Sprintf("proposal %d for the notes of %s was not applied: %s",
			claimed.ID, full, oneLine(err.Error(), retroWhyRunes)), data)
		return err
	}
	why := "a write cut short"
	if rerr != nil {
		why = "notes that cannot be read again (" + oneLine(rerr.Error(), retroWhyRunes) + ")"
	}
	notesEvent(bctx, st, "warn", subject, "notes.apply_unsettled", fmt.Sprintf("the write for proposal %d of %s failed and left %s: %s; "+
		"the proposal stays applying and the next scan settles it", claimed.ID, full, why, oneLine(err.Error(), retroWhyRunes)), data)
	return fmt.Errorf("the write failed and left the notes for the recovery to settle (the proposal stays applying): %w", err)
}

// crashAt runs the test hook after an apply's step.
func crashAt(step string) error {
	if applyCrash == nil {
		return nil
	}
	return applyCrash(step)
}

// finishApply records claimed (applying) as applied, with the decider's
// reason and target recorded as a version linked to it: a curation's, or
// the operator's for a restore. Dedupe: a curation that only skips the misses it was given
// leaves the notes as they are, and the version they hold is not recorded
// again.
func finishApply(ctx context.Context, st *store.Store, claimed store.NotesProposal, target notes.State, reason string, now time.Time) (store.NotesProposal, error) {
	source := store.NotesFromCuration
	if claimed.Kind == store.ProposalRestore {
		source = store.NotesFromHuman
	}
	return st.DecideNotesProposal(context.WithoutCancel(ctx), claimed.ID, []string{store.ProposalApplying}, store.ProposalApplied,
		claimed.DecidedBy, reason, now, &store.NotesVersionInput{Source: source, Content: ContentOf(target), Dedupe: true})
}

// notesEvent appends an audit event (best effort): the apply runs in the
// CLI as well as in the daemon.
func notesEvent(ctx context.Context, st *store.Store, level, subject, kind, msg string, data map[string]any) {
	ev := store.Event{Level: level, Kind: kind, Message: execx.Redact(msg)}
	if subject != "" {
		ev.Subject = &subject
	}
	if b, err := json.Marshal(data); err == nil && len(data) > 0 {
		ev.Data = json.RawMessage(execx.Redact(string(b)))
	}
	_, _ = st.AppendEvent(context.WithoutCancel(ctx), ev)
}

// Outcomes of RecoverNotesApply.
const (
	ApplyFinished = "finished" // the files held what the apply wrote (or part of it, completed): recorded applied
	ApplyReverted = "reverted" // the files held what the apply found, or something else: the proposal waits again
)

// RecoverNotesApply finishes or reverts proposal p, which an apply left
// applying (a crash between its claim and its record), under the notes lock
// (waiting at most wait): when the notes hold the state the claim meant to
// write (its target, computed again from the base, the prior and the
// proposed versions and checked against the target the claim recorded), the
// proposal is recorded applied; when they hold the state the claim found
// (its prior version), nothing was written and the claim is released; when
// they hold the notes of one and the harness of the other (the write cut
// short between its two steps), the target is written whole and recorded.
// Anything else (a writer after the crash) is recorded as an import, so the
// registry holds it, and the claim is released: the proposal waits again,
// stale against those notes. It reports what it did ("" when p no longer
// waits for a recovery) and writes an event.
func RecoverNotesApply(ctx context.Context, st *store.Store, nr notes.Repo, p store.NotesProposal, now time.Time, wait time.Duration) (string, error) {
	unlock, err := notes.Lock(ctx, nr.Lock(), wait)
	if err != nil {
		return "", fmt.Errorf("the notes lock: %w", err)
	}
	defer unlock()
	cur, err := st.NotesProposalByID(ctx, p.ID)
	if err != nil || cur.State != store.ProposalApplying {
		return "", err
	}
	live, err := notes.ReadState(nr)
	if err != nil {
		return "", err
	}
	prior, target, err := claimStates(ctx, st, cur)
	subject, full := notesSubject(nr), nr.FullName()
	revert := func(why string) (string, error) {
		if _, _, err := st.RecordNotesVersion(ctx, store.NotesVersionInput{RepoID: cur.RepoID, Source: store.NotesFromImport,
			Content: ContentOf(live), Dedupe: true}); err != nil {
			return "", err
		}
		if _, err := st.ReleaseNotesClaim(ctx, cur.ID); err != nil {
			return "", err
		}
		notesEvent(ctx, st, "warn", subject, "notes.apply_reverted", fmt.Sprintf("an apply of proposal %d to the notes of %s was cut short: %s; "+
			"the proposal waits for a decision again", cur.ID, full, why), map[string]any{"proposal": cur.ID, "by": cur.DecidedBy})
		return ApplyReverted, nil
	}
	if err != nil {
		return revert("its states cannot be read again (" + oneLine(err.Error(), retroWhyRunes) + ")")
	}
	finish := func(why string) (string, error) {
		applied, err := finishApply(ctx, st, cur, target, "", now)
		if err != nil {
			return "", err
		}
		notesEvent(ctx, st, "warn", subject, "notes.apply_finished", fmt.Sprintf("an apply of proposal %d to the notes of %s was cut short: %s; "+
			"recorded applied, version %d", cur.ID, full, why, store.Deref(applied.AppliedVersionID)),
			map[string]any{"proposal": cur.ID, "by": cur.DecidedBy, "version": store.Deref(applied.AppliedVersionID)})
		return ApplyFinished, nil
	}
	switch whatTheNotesHold(live, prior, target) {
	case holdsTarget:
		return finish("the notes hold what it wrote")
	case holdsPrior:
		return revert("it wrote nothing")
	case holdsHalf:
		if err := writeTarget(ctx, st, nr, cur, target); err != nil {
			return "", err
		}
		return finish("its write stopped halfway, and was completed")
	}
	return revert("the notes hold neither what it found nor what it meant to write (kept as found)")
}

// claimStates reads what claimed proposal p found (its prior version) and
// meant to write: its proposed state, or its merge with the prior notes
// when they were no longer its base, checked against the target the claim
// recorded.
func claimStates(ctx context.Context, st *store.Store, p store.NotesProposal) (prior, target notes.State, err error) {
	if p.PriorVersionID == nil {
		return prior, target, errors.New("the claim recorded no prior version")
	}
	content, err := st.NotesVersionContent(ctx, *p.PriorVersionID)
	if err != nil {
		return prior, target, err
	}
	prior = StateOf(content)
	check, err := checkStaleWith(ctx, st, p, prior)
	if err != nil {
		return prior, target, err
	}
	target, ok := ApplyTarget(check)
	switch {
	case !ok:
		return prior, target, errors.New("its merge conflicts")
	case target.Fingerprint() != p.TargetSHA256:
		return prior, target, errors.New("its target is not the one the claim recorded")
	}
	return prior, target, nil
}

// mixedState reports whether live holds the notes of one of a and b and
// the harness of the other: WriteState swaps the harness first, then the
// notes.
func mixedState(live, a, b notes.State) bool {
	harness := func(s notes.State) string { return notes.Fingerprint(nil, s.Listing()) }
	sameNotes := func(x, y notes.State) bool { return x.Exists == y.Exists && bytes.Equal(x.Notes, y.Notes) }
	return (sameNotes(live, a) && harness(live) == harness(b)) || (sameNotes(live, b) && harness(live) == harness(a))
}

// applyRecoverWait bounds a recovery's wait for the notes lock: a crashed
// holder's lock is taken over once stale, and a later scan tries again.
const applyRecoverWait = 5 * time.Second

// recoverNotesApplies finishes or reverts every proposal an apply left
// applying (RecoverNotesApply): at the daemon's start, before the notes are
// imported, and at every curation scan after it (a lock a crashed holder
// left is taken over once stale). A dry run leaves them.
func (e *Engine) recoverNotesApplies(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	ps, err := e.st.NotesProposals(ctx, store.NotesProposalFilter{States: []string{store.ProposalApplying}})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "notes proposals: list the applies cut short")
		return
	}
	for _, p := range ps {
		repo, err := e.st.RepoByID(ctx, p.RepoID)
		if err != nil {
			continue
		}
		nr, ok := notesRepo(e.d.Layout, repo.Owner, repo.Name)
		if !ok {
			continue
		}
		if _, err := RecoverNotesApply(ctx, e.st, nr, p, e.now(), applyRecoverWait); err != nil {
			e.warnUnlessStopped(ctx, err, "notes proposals: recover an apply cut short", "proposal", p.ID)
		}
	}
}

// notesApplying reports whether a proposal of repoID is applying: an apply
// is writing its notes, or was cut short and waits for its recovery. A
// registry error counts as applying: the caller leaves the notes alone.
func (e *Engine) notesApplying(ctx context.Context, repoID int64) bool {
	ps, err := e.st.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repoID, States: []string{store.ProposalApplying}, Limit: 1})
	return err != nil || len(ps) > 0
}

// notesLeftoverAge is how old a directory a notes write left next to a
// harness (notes.Leftovers) must be before the reconcile removes it.
const notesLeftoverAge = time.Hour

// sweepNotesLeftovers removes the old and the staged directories that a
// write of nr's notes left (an old harness that could not be removed, a
// stage a crash cut off) once they are an hour old. It takes the notes lock
// without waiting, as a write holds it for as long as it needs its old
// harness: a lock held by a judge or an apply leaves the sweep for the next
// reconcile. A repository with a proposal applying is left alone, so the
// directory a recovery may still need stays.
func (e *Engine) sweepNotesLeftovers(ctx context.Context, repo store.Repo, nr notes.Repo) {
	cutoff := e.now().Add(-notesLeftoverAge)
	if left, err := notes.Leftovers(nr, cutoff); err != nil || len(left) == 0 || e.notesApplying(ctx, repo.ID) {
		return
	}
	unlock, err := notes.Lock(ctx, nr.Lock(), 0)
	if err != nil {
		return
	}
	defer unlock()
	removed, err := notes.SweepLeftovers(nr, cutoff)
	if len(removed) > 0 {
		e.log.Info("repository notes: removed the directories a write left", "repo", nr.FullName(), "dirs", len(removed))
	}
	if err != nil {
		e.log.Warn("repository notes: remove a directory a write left", "repo", nr.FullName(), "err", err)
	}
}
