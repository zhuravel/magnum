package engine

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// curatedProposal runs a curation of the harness's notes (the curator's
// writer decides what it proposes) and returns its pending proposal.
func curatedProposal(t *testing.T, h *harness) store.NotesProposal {
	t.Helper()
	if msg, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err != nil || !strings.Contains(msg, "started") {
		t.Fatalf("requestCurate = %q, %v", msg, err)
	}
	h.settle()
	ps, err := h.st.NotesProposals(h.ctx, store.NotesProposalFilter{States: []string{store.ProposalPending}, Limit: 1})
	if err != nil || len(ps) != 1 {
		t.Fatalf("pending proposals = %+v, %v", ps, err)
	}
	return ps[0]
}

// reviewed is what a decider reviewing p now approves: the notes'
// fingerprint and the state an apply would write.
func reviewed(t *testing.T, h *harness, nr notes.Repo, p store.NotesProposal) NotesApply {
	t.Helper()
	check, err := CheckStale(h.ctx, h.st, nr, p)
	if err != nil {
		t.Fatal(err)
	}
	target, ok := ApplyTarget(check)
	if !ok {
		t.Fatal("the proposal's merge conflicts")
	}
	return NotesApply{Live: check.Live.Fingerprint(), Target: target.Fingerprint(), Now: h.clock.Now()}
}

func by(a NotesApply, decider string) NotesApply { a.By = decider; return a }

// liveMatchesApplied checks that the notes on disk hold the version the
// registry records as p's applied state.
func liveMatchesApplied(t *testing.T, h *harness, nr notes.Repo, p store.NotesProposal) {
	t.Helper()
	if p.State != store.ProposalApplied || p.AppliedVersionID == nil {
		t.Fatalf("proposal = %+v, want applied with a version", p)
	}
	c, err := h.st.NotesVersionContent(h.ctx, *p.AppliedVersionID)
	if err != nil {
		t.Fatal(err)
	}
	live, err := notes.ReadState(nr)
	if err != nil || live.Fingerprint() != StateOf(c).Fingerprint() {
		t.Fatalf("the notes on disk = %q, the registry's applied version %d = %q (%v)", live.Notes, *p.AppliedVersionID, c.Notes, err)
	}
}

func readLive(t *testing.T, nr notes.Repo) notes.State {
	t.Helper()
	s, err := notes.ReadState(nr)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func proposalNow(t *testing.T, h *harness, id int64) store.NotesProposal {
	t.Helper()
	p, err := h.st.NotesProposalByID(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var errCrash = errors.New("the process died here")

// crashAfter makes an apply stop after step as a crash would.
func crashAfter(t *testing.T, step string) {
	t.Helper()
	applyCrash = func(s string) error {
		if s == step {
			return errCrash
		}
		return nil
	}
	t.Cleanup(func() { applyCrash = nil })
}

// The operator's y and the critic's apply race: exactly one applies, the
// notes on disk hold the registry's applied version, and the other decider
// is refused, told who applied it.
func TestTheOperatorAndTheCriticRaceAndExactlyOneApplies(t *testing.T) {
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	deciders := []string{store.DecidedByOperator, store.DecidedByCritic}
	errs := make([]error, len(deciders))
	var wg sync.WaitGroup
	for i, d := range deciders {
		wg.Go(func() { _, errs[i] = ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, d)) })
	}
	wg.Wait()
	winner, loser := -1, -1
	for i, err := range errs {
		if err == nil {
			winner = i
		} else {
			loser = i
		}
	}
	if winner < 0 || loser < 0 {
		t.Fatalf("errors = %v: exactly one apply must succeed", errs)
	}
	var decided *ProposalDecidedError
	if !errors.As(errs[loser], &decided) || !strings.Contains(errs[loser].Error(), "already applied by "+DeciderName(deciders[winner])) {
		t.Fatalf("the loser's refusal = %v", errs[loser])
	}
	got := proposalNow(t, h, p.ID)
	if got.DecidedBy != deciders[winner] {
		t.Errorf("decided by %q, want %q", got.DecidedBy, deciders[winner])
	}
	liveMatchesApplied(t, h, nr, got)
	if evs := notesEvents(t, h, "notes.proposal_applied"); len(evs) != 1 {
		t.Errorf("applied events = %d, want 1", len(evs))
	}
}

// A rejection and an apply race: the claim comes before any write, so the
// one that claims first wins. A rejection during an apply that claimed is
// refused and the apply goes on; an apply after a rejection writes nothing.
func TestARejectionAndAnApplyNeverBothWin(t *testing.T) {
	storetest.Serial(t) // swaps applyCrash
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	claimed, proceed := make(chan struct{}), make(chan struct{})
	applyCrash = func(step string) error {
		if step == "claimed" {
			close(claimed)
			<-proceed
		}
		return nil
	}
	t.Cleanup(func() { applyCrash = nil })
	done := make(chan error, 1)
	go func() {
		_, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, store.DecidedByCritic))
		done <- err
	}()
	<-claimed
	_, err := h.st.DecideNotesProposal(h.ctx, p.ID, []string{store.ProposalPending}, store.ProposalRejected, store.DecidedByOperator, "no", h.clock.Now(), nil)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a rejection during the apply = %v, want a conflict", err)
	}
	close(proceed)
	if err := <-done; err != nil {
		t.Fatalf("the apply = %v", err)
	}
	liveMatchesApplied(t, h, nr, proposalNow(t, h, p.ID))

	// The other order: rejected first, the apply writes nothing.
	applyCrash = nil
	h2, _, nr2 := curationHarness(t, goodProposal)
	p2 := curatedProposal(t, h2)
	a2 := reviewed(t, h2, nr2, p2)
	before := readLive(t, nr2)
	if _, err := h2.st.DecideNotesProposal(h2.ctx, p2.ID, []string{store.ProposalPending}, store.ProposalRejected, store.DecidedByOperator, "no",
		h2.clock.Now(), nil); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyNotesProposal(h2.ctx, h2.st, nr2, p2, by(a2, store.DecidedByCritic))
	var decided *ProposalDecidedError
	if !errors.As(err, &decided) || !strings.Contains(err.Error(), "already rejected by you") {
		t.Fatalf("an apply after the rejection = %v", err)
	}
	if !readLive(t, nr2).Same(before) {
		t.Error("an apply of a rejected proposal wrote the notes")
	}
}

// A daemon stop between the claim and the write leaves the proposal
// applying with the notes untouched; the next start reverts the claim (an
// event says so), and the proposal can be applied again.
func TestAStopBetweenTheClaimAndTheWriteIsRevertedAtTheNextStart(t *testing.T) {
	storetest.Serial(t) // swaps applyCrash
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	before := readLive(t, nr)
	crashAfter(t, "claimed")
	if _, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, store.DecidedByCritic)); !errors.Is(err, errCrash) {
		t.Fatalf("apply = %v", err)
	}
	if got := proposalNow(t, h, p.ID); got.State != store.ProposalApplying || got.DecidedBy != store.DecidedByCritic || got.PriorVersionID == nil ||
		got.TargetSHA256 != a.Target {
		t.Fatalf("after the claim = %+v", got)
	}
	applyCrash = nil
	h.startup()
	got := proposalNow(t, h, p.ID)
	if got.State != store.ProposalPending || got.DecidedBy != "" || got.PriorVersionID != nil || got.TargetSHA256 != "" {
		t.Fatalf("after the start = %+v, want pending again", got)
	}
	if !readLive(t, nr).Same(before) {
		t.Error("the recovery changed the notes")
	}
	if evs := notesEvents(t, h, "notes.apply_reverted"); len(evs) != 1 || !strings.Contains(evs[0].Message, "it wrote nothing") {
		t.Fatalf("revert events = %+v", evs)
	}
	applied, err := ApplyNotesProposal(h.ctx, h.st, nr, got, by(reviewed(t, h, nr, got), store.DecidedByOperator))
	if err != nil {
		t.Fatal(err)
	}
	liveMatchesApplied(t, h, nr, applied)
}

// A daemon stop between the write and the record leaves the notes written
// and the proposal applying; the next start records it applied with the
// version the notes hold.
func TestAStopBetweenTheWriteAndTheRecordIsFinishedAtTheNextStart(t *testing.T) {
	storetest.Serial(t) // swaps applyCrash
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	crashAfter(t, "written")
	if _, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, store.DecidedByOperator)); !errors.Is(err, errCrash) {
		t.Fatalf("apply = %v", err)
	}
	if readLive(t, nr).Fingerprint() != a.Target {
		t.Fatal("the write did not happen")
	}
	applyCrash = nil
	h.startup()
	got := proposalNow(t, h, p.ID)
	liveMatchesApplied(t, h, nr, got)
	if got.DecidedBy != store.DecidedByOperator {
		t.Errorf("decided by %q", got.DecidedBy)
	}
	hist, _ := h.st.NotesHistory(h.ctx, repoID(t, h), 1)
	if len(hist) != 1 || hist[0].ID != *got.AppliedVersionID || hist[0].Source != store.NotesFromCuration {
		t.Errorf("the newest version = %+v, want the curation's applied one", hist)
	}
	if evs := notesEvents(t, h, "notes.apply_finished"); len(evs) != 1 {
		t.Fatalf("finish events = %+v", evs)
	}
}

// A write stopped between its two steps (the harness swapped, the notes
// not) is completed by the next start and recorded applied.
func TestAWriteStoppedHalfwayIsCompletedAtTheNextStart(t *testing.T) {
	storetest.Serial(t) // swaps applyCrash
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	before := readLive(t, nr)
	crashAfter(t, "claimed")
	if _, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, store.DecidedByCritic)); !errors.Is(err, errCrash) {
		t.Fatalf("apply = %v", err)
	}
	proposed, err := h.st.NotesVersionContent(h.ctx, *p.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	half := notes.State{Exists: true, Notes: before.Notes, Files: StateOf(proposed).Files}
	if err := notes.WriteState(nr, half); err != nil {
		t.Fatal(err)
	}
	applyCrash = nil
	h.startup()
	liveMatchesApplied(t, h, nr, proposalNow(t, h, p.ID))
	if readLive(t, nr).Fingerprint() != a.Target {
		t.Error("the notes are not the proposal's")
	}
}

// An apply cut short under notes someone changed after the crash (neither
// what it found nor what it meant to write): the next start keeps the notes
// as found (recorded, so the registry holds them) and the proposal waits
// again, stale against them.
func TestAnApplyCutShortUnderChangedNotesWaitsAgain(t *testing.T) {
	storetest.Serial(t) // swaps applyCrash
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	crashAfter(t, "claimed")
	if _, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, store.DecidedByCritic)); !errors.Is(err, errCrash) {
		t.Fatalf("apply = %v", err)
	}
	cur := readLive(t, nr)
	cur.Notes = append(cur.Notes, []byte("- a judge's lesson written after the crash\n")...)
	if err := notes.WriteState(nr, cur); err != nil {
		t.Fatal(err)
	}
	applyCrash = nil
	h.startup()
	if got := proposalNow(t, h, p.ID); got.State != store.ProposalPending {
		t.Fatalf("after the start = %+v, want pending", got)
	}
	latest, err := h.st.LatestNotesVersion(h.ctx, repoID(t, h))
	if err != nil || latest.SHA256 != notes.TextSHA(cur.Notes) {
		t.Fatalf("the registry's newest version = %+v, %v; want the notes as found", latest, err)
	}
	if evs := notesEvents(t, h, "notes.apply_reverted"); len(evs) != 1 || !strings.Contains(evs[0].Message, "neither") {
		t.Fatalf("revert events = %+v", evs)
	}
}

// A proposal that would lose a line of the notes (one stored before the
// preservation check, say) can be applied by neither decider: nothing is
// written and it still waits, for a rejection or a new curation.
func TestAProposalThatLosesALineCannotBeAppliedByEitherDecider(t *testing.T) {
	h := newHarness(t)
	nr := notesOf(t, h)
	writeNotes(t, nr, "# Notes for talkable/talkable\n\n## Tests\n- run bin/rspec\n- the admin needs a seeded user\n", nil)
	knownRepo(t, h)
	base, err := h.st.LatestNotesVersion(h.ctx, repoID(t, h))
	if err != nil {
		t.Fatal(err)
	}
	lossy := store.NotesContent{Notes: []byte("# Notes for talkable/talkable (updated)\n\n## Tests\n- run bin/rspec\n")}
	p, err := h.st.CreateNotesProposal(h.ctx, store.NotesProposalInput{RepoID: repoID(t, h), Kind: store.ProposalCuration, Trigger: "request",
		BaseVersionID: base.ID, Proposed: &lossy, Changes: []byte(`{"sections":[{"name":"Tests","action":"kept","reason":"r"}],"files":[]}`),
		State: store.ProposalPending})
	if err != nil {
		t.Fatal(err)
	}
	before := readLive(t, nr)
	a := reviewed(t, h, nr, p)
	for _, d := range []string{store.DecidedByOperator, store.DecidedByCritic} {
		_, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, d))
		var lost *NotPreservedError
		if !errors.As(err, &lost) || !strings.Contains(err.Error(), "the admin needs a seeded user") {
			t.Fatalf("an apply by %s = %v, want a preservation refusal quoting the line", d, err)
		}
	}
	if got := proposalNow(t, h, p.ID); got.State != store.ProposalPending || got.DecidedBy != "" {
		t.Fatalf("the proposal = %+v, want it still pending", got)
	}
	if !readLive(t, nr).Same(before) {
		t.Error("a refused apply wrote the notes")
	}
}

// An approval binds to the state it reviewed: an apply whose approved
// target is not the state it would write is refused, and so is one whose
// notes changed since the review.
func TestAnApprovalOfAnotherTargetIsRefused(t *testing.T) {
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	before := readLive(t, nr)
	other := a
	other.Target = notes.Fingerprint([]byte("another state"), nil)
	if _, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(other, store.DecidedByCritic)); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("an apply of another target = %v", err)
	}
	stale := a
	stale.Live = notes.Fingerprint([]byte("notes nobody saw"), nil)
	if _, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(stale, store.DecidedByCritic)); !errors.Is(err, ErrNotesChanged) {
		t.Fatalf("an apply over changed notes = %v", err)
	}
	if got := proposalNow(t, h, p.ID); got.State != store.ProposalPending {
		t.Fatalf("the proposal = %+v", got)
	}
	if !readLive(t, nr).Same(before) {
		t.Error("a refused apply wrote the notes")
	}
}
