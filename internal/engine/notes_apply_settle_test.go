package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// failWrite makes the apply's write of the notes and the harness run write
// instead of notes.WriteStateReport (a test that calls it is serial).
func failWrite(t *testing.T, write func(nr notes.Repo, target notes.State) (*notes.Leftover, error)) {
	t.Helper()
	writeNotesState = write
	t.Cleanup(func() { writeNotesState = notes.WriteStateReport })
}

// stoppedHalfway is a write that swapped the harness for the target's and
// stopped before the notes: the notes of before with the harness of target.
func stoppedHalfway(t *testing.T, before notes.State) func(notes.Repo, notes.State) (*notes.Leftover, error) {
	t.Helper()
	return func(nr notes.Repo, target notes.State) (*notes.Leftover, error) {
		half := notes.State{Exists: before.Exists, Notes: before.Notes, Files: target.Files}
		if _, err := notes.WriteStateReport(nr, half); err != nil {
			t.Errorf("the half write: %v", err)
		}
		return nil, errors.New("disk full")
	}
}

func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root removes a directory whatever its mode")
	}
}

// The swap and the notes write are the apply's write. An old script
// directory that cannot be removed afterwards (mode 0500, as in the run
// that found it) is a leftover: the proposal is applied and its version
// recorded, one leftover is told, and no notes.apply_failed says the apply
// failed.
func TestAnApplyWhoseOldHarnessCannotBeRemovedIsApplied(t *testing.T) {
	skipAsRoot(t)
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	if err := os.Chmod(nr.Harness(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		left, _ := filepath.Glob(nr.Harness() + ".old-*")
		for _, d := range left {
			_ = os.Chmod(d, 0o700)
		}
	})
	applied, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, store.DecidedByCritic))
	if err != nil {
		t.Fatalf("apply = %v, want it applied", err)
	}
	liveMatchesApplied(t, h, nr, applied)
	if got := proposalNow(t, h, p.ID); got.State != store.ProposalApplied || got.DecidedBy != store.DecidedByCritic || got.AppliedVersionID == nil {
		t.Fatalf("the proposal = %+v, want applied by the critic with its version", got)
	}
	if evs := notesEvents(t, h, "notes.apply_failed"); len(evs) != 0 {
		t.Errorf("apply_failed events = %+v, want none", evs)
	}
	evs := notesEvents(t, h, "notes.write_leftover")
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "proposal "+itoa(p.ID)) {
		t.Fatalf("leftover events = %+v, want one warning", evs)
	}
	if left, _ := filepath.Glob(nr.Harness() + ".old-*"); len(left) != 1 {
		t.Errorf("old harnesses next to the notes: %v, want the one that stays", left)
	}
}

// An apply whose write fails before it changed anything releases the claim:
// the proposal waits again with no decider, the notes are as they were, one
// notes.apply_failed says so, and the proposal can be applied again.
func TestAnApplyThatFailsBeforeAnyWriteReleasesTheClaim(t *testing.T) {
	storetest.Serial(t) // swaps writeNotesState
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	before := readLive(t, nr)
	failWrite(t, func(notes.Repo, notes.State) (*notes.Leftover, error) { return nil, errors.New("disk full") })
	if _, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, store.DecidedByCritic)); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("apply = %v, want the write's error", err)
	}
	got := proposalNow(t, h, p.ID)
	if got.State != store.ProposalPending || got.DecidedBy != "" || got.PriorVersionID != nil || got.TargetSHA256 != "" {
		t.Fatalf("the proposal = %+v, want pending again with no claim", got)
	}
	if !readLive(t, nr).Same(before) {
		t.Error("the notes changed")
	}
	if evs := notesEvents(t, h, "notes.apply_failed"); len(evs) != 1 {
		t.Errorf("apply_failed events = %+v, want one", evs)
	}
	if evs := notesEvents(t, h, "notes.apply_unsettled"); len(evs) != 0 {
		t.Errorf("unsettled events = %+v, want none", evs)
	}
	writeNotesState = notes.WriteStateReport
	applied, err := ApplyNotesProposal(h.ctx, h.st, nr, got, by(reviewed(t, h, nr, got), store.DecidedByOperator))
	if err != nil {
		t.Fatalf("the second apply = %v", err)
	}
	liveMatchesApplied(t, h, nr, applied)
}

// An apply whose write fails between its two steps (the harness swapped, the
// notes not) does not know it wrote nothing: the proposal stays applying,
// claim and all, and the recovery completes the write and records it.
func TestAnApplyThatFailsBetweenItsTwoWritesStaysApplyingAndTheRecoverySettlesIt(t *testing.T) {
	storetest.Serial(t) // swaps writeNotesState
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	failWrite(t, stoppedHalfway(t, readLive(t, nr)))
	if _, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, store.DecidedByCritic)); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("apply = %v, want the write's error", err)
	}
	got := proposalNow(t, h, p.ID)
	if got.State != store.ProposalApplying || got.DecidedBy != store.DecidedByCritic || got.PriorVersionID == nil || got.TargetSHA256 != a.Target {
		t.Fatalf("the proposal = %+v, want it applying with its claim", got)
	}
	if evs := notesEvents(t, h, "notes.apply_failed"); len(evs) != 0 {
		t.Errorf("apply_failed events = %+v, want none: the claim is not released", evs)
	}
	if evs := notesEvents(t, h, "notes.apply_unsettled"); len(evs) != 1 || evs[0].Level != "warn" {
		t.Errorf("unsettled events = %+v, want one warning", evs)
	}
	writeNotesState = notes.WriteStateReport
	h.startup()
	done := proposalNow(t, h, p.ID)
	liveMatchesApplied(t, h, nr, done)
	if done.DecidedBy != store.DecidedByCritic {
		t.Errorf("decided by %q", done.DecidedBy)
	}
	if evs := notesEvents(t, h, "notes.apply_finished"); len(evs) != 1 {
		t.Errorf("finish events = %+v, want one", evs)
	}
}

// A write that reports an error after it put the state in place is not a
// failed apply: the notes hold the target, so the proposal is recorded
// applied and a warning names the error.
func TestAnApplyWhoseWriteErroredAfterTheTargetWasInPlaceIsApplied(t *testing.T) {
	storetest.Serial(t) // swaps writeNotesState
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	failWrite(t, func(nr notes.Repo, target notes.State) (*notes.Leftover, error) {
		if _, err := notes.WriteStateReport(nr, target); err != nil {
			t.Errorf("the write: %v", err)
		}
		return nil, errors.New("sync failed")
	})
	applied, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, store.DecidedByOperator))
	if err != nil {
		t.Fatalf("apply = %v, want it applied", err)
	}
	liveMatchesApplied(t, h, nr, applied)
	if evs := notesEvents(t, h, "notes.apply_failed"); len(evs) != 0 {
		t.Errorf("apply_failed events = %+v, want none", evs)
	}
	if evs := notesEvents(t, h, "notes.write_error"); len(evs) != 1 || !strings.Contains(evs[0].Message, "sync failed") {
		t.Errorf("write_error events = %+v, want one naming the error", evs)
	}
}

// A write that failed with notes someone else changed meanwhile (neither
// what the apply found nor what it meant to write) leaves the proposal
// applying too: the recovery keeps those notes as found and releases it.
func TestAnApplyThatFailsUnderNotesSomeoneChangedStaysApplyingForTheRecovery(t *testing.T) {
	storetest.Serial(t) // swaps writeNotesState
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	changed := readLive(t, nr)
	changed.Notes = append(changed.Notes, []byte("- a judge's lesson written during the apply\n")...)
	failWrite(t, func(nr notes.Repo, _ notes.State) (*notes.Leftover, error) {
		if err := notes.WriteState(nr, changed); err != nil {
			t.Errorf("the other writer: %v", err)
		}
		return nil, errors.New("disk full")
	})
	if _, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, store.DecidedByCritic)); err == nil {
		t.Fatal("apply succeeded")
	}
	if got := proposalNow(t, h, p.ID); got.State != store.ProposalApplying {
		t.Fatalf("the proposal = %+v, want applying", got)
	}
	writeNotesState = notes.WriteStateReport
	h.startup()
	if got := proposalNow(t, h, p.ID); got.State != store.ProposalPending || got.DecidedBy != "" {
		t.Fatalf("after the recovery = %+v, want pending", got)
	}
	if latest, err := h.st.LatestNotesVersion(h.ctx, repoID(t, h)); err != nil || latest.SHA256 != notes.TextSHA(changed.Notes) {
		t.Fatalf("the newest version = %+v, %v; want the notes as found", latest, err)
	}
}

// One rule says what the notes hold against a claim, for the apply that
// failed and for the recovery: the target first (a claim that wrote
// nothing new is finished), then the prior, then half of each, then
// neither.
func TestWhatTheNotesHoldIsOneRuleForTheApplyAndTheRecovery(t *testing.T) {
	prior := notes.State{Exists: true, Notes: []byte("old\n"), Files: []notes.Blob{{Path: "a.sh", Body: []byte("a")}}}
	target := notes.State{Exists: true, Notes: []byte("new\n"), Files: []notes.Blob{{Path: "b.sh", Body: []byte("b")}}}
	other := notes.State{Exists: true, Notes: []byte("else\n"), Files: []notes.Blob{{Path: "c.sh", Body: []byte("c")}}}
	for _, c := range []struct {
		name          string
		live          notes.State
		prior, target notes.State
		want          claimHolds
	}{
		{"the target", target, prior, target, holdsTarget},
		{"the prior", prior, prior, target, holdsPrior},
		{"the target's harness under the prior's notes", notes.State{Exists: true, Notes: prior.Notes, Files: target.Files}, prior, target, holdsHalf},
		{"the target's notes over the prior's harness", notes.State{Exists: true, Notes: target.Notes, Files: prior.Files}, prior, target, holdsHalf},
		{"neither", other, prior, target, holdsOther},
		{"no harness at all", notes.State{Exists: true, Notes: prior.Notes}, prior, target, holdsOther},
		{"a claim whose target is the notes as found", prior, prior, prior, holdsTarget},
	} {
		if got := whatTheNotesHold(c.live, c.prior, c.target); got != c.want {
			t.Errorf("%s: whatTheNotesHold = %d, want %d", c.name, got, c.want)
		}
	}
}

// The reconcile imports what the notes hold as a version of the registry
// (source import) only when no apply is in flight: a state a write left
// halfway is not the notes' state.
func TestAReconcileDuringAHalfFinishedApplyRecordsNoImportVersion(t *testing.T) {
	storetest.Serial(t) // swaps writeNotesState
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	failWrite(t, stoppedHalfway(t, readLive(t, nr)))
	if _, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, store.DecidedByCritic)); err == nil {
		t.Fatal("apply succeeded")
	}
	versions := func() int {
		hist, err := h.st.NotesHistory(h.ctx, repoID(t, h), 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(hist)
	}
	before := versions()
	h.e.syncNotes(h.ctx, false)
	h.e.syncNotes(h.ctx, true)
	if after := versions(); after != before {
		t.Fatalf("versions = %d, want %d: a reconcile recorded the half-written notes", after, before)
	}
	writeNotesState = notes.WriteStateReport
	h.startup()
	liveMatchesApplied(t, h, nr, proposalNow(t, h, p.ID))
}

// A reconcile that reads the notes between the write and the record of an
// apply must not record them as an import: the applied version would be
// that import, not the curation's.
func TestAReconcileBetweenAnApplysWriteAndRecordLeavesTheCurationsVersion(t *testing.T) {
	storetest.Serial(t) // swaps applyCrash
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	a := reviewed(t, h, nr, p)
	applyCrash = func(step string) error {
		if step == "written" {
			h.e.syncNotes(h.ctx, false)
		}
		return nil
	}
	t.Cleanup(func() { applyCrash = nil })
	applied, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(a, store.DecidedByCritic))
	if err != nil {
		t.Fatal(err)
	}
	liveMatchesApplied(t, h, nr, applied)
	hist, err := h.st.NotesHistory(h.ctx, repoID(t, h), 1)
	if err != nil || len(hist) != 1 || hist[0].ID != *applied.AppliedVersionID || hist[0].Source != store.NotesFromCuration {
		t.Fatalf("the newest version = %+v, %v; want the curation's applied version %d", hist, err, store.Deref(applied.AppliedVersionID))
	}
}

// the reconcile removes what a write left next to a harness once it is an
// hour old, and only then: not a fresh leftover, not one beside a held
// notes lock, not one of a repository whose apply is unsettled.

func leftoverAt(t *testing.T, h *harness, nr notes.Repo, suffix string, age time.Duration) string {
	t.Helper()
	dir := nr.Harness() + suffix
	if err := os.MkdirAll(filepath.Join(dir, "qa"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qa", "x.sh"), []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	at := h.clock.Now().Add(-age)
	if err := os.Chtimes(dir, at, at); err != nil {
		t.Fatal(err)
	}
	return dir
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestTheReconcileSweepsTheDirectoriesAWriteLeftOnceTheyAreAnHourOld(t *testing.T) {
	h, _, nr := curationHarness(t, goodProposal)
	oldDir := leftoverAt(t, h, nr, ".old-ABCDEFGH", 2*time.Hour)
	staged := leftoverAt(t, h, nr, ".new-ABCDEFGH", 90*time.Minute)
	fresh := leftoverAt(t, h, nr, ".old-ZYXWVUTS", 10*time.Minute)
	before := readLive(t, nr)

	h.e.syncNotes(h.ctx, false)
	if exists(oldDir) || exists(staged) {
		t.Errorf("an hour-old leftover stays: old %v, staged %v", exists(oldDir), exists(staged))
	}
	if !exists(fresh) {
		t.Error("a fresh leftover was removed: a write may still need it")
	}
	if !readLive(t, nr).Same(before) {
		t.Error("the sweep changed the notes")
	}
	h.advance(2 * time.Hour)
	h.e.syncNotes(h.ctx, false)
	if exists(fresh) {
		t.Error("the leftover stays once it is two hours old")
	}
}

func TestTheSweepWaitsForTheNotesLockAndForAnUnsettledApply(t *testing.T) {
	storetest.Serial(t) // swaps applyCrash
	h, _, nr := curationHarness(t, goodProposal)
	p := curatedProposal(t, h)
	old := leftoverAt(t, h, nr, ".old-ABCDEFGH", 2*time.Hour)

	unlock, err := notes.Lock(h.ctx, nr.Lock(), 0)
	if err != nil {
		t.Fatal(err)
	}
	h.e.syncNotes(h.ctx, false)
	if !exists(old) {
		t.Error("the sweep removed a leftover under a held notes lock: a write in flight may need it")
	}
	unlock()

	crashAfter(t, "claimed") // an apply cut short: the proposal stays applying
	if _, err := ApplyNotesProposal(h.ctx, h.st, nr, p, by(reviewed(t, h, nr, p), store.DecidedByCritic)); !errors.Is(err, errCrash) {
		t.Fatalf("apply = %v", err)
	}
	applyCrash = nil
	h.e.syncNotes(h.ctx, false)
	if !exists(old) {
		t.Error("the sweep removed a leftover of a repository whose apply is unsettled")
	}
	h.startup() // the recovery releases the claim; the start's reconcile sweeps
	if exists(old) {
		t.Error("the leftover stays after the apply was settled")
	}
}
