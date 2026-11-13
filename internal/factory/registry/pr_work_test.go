package registry_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fixwork"
)

// The pr_work table comes with migration 9, on a file an older binary left
// (version 1, its tasks kept) as on the tests' template: no PR has a row
// until the factory works it.
func TestThePRWorkMigrationAppliesOnAnOlderFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "factory.db")
	writeVersion1(t, path)
	old, err := registry.Open(path, registry.Options{})
	if err != nil {
		t.Fatalf("Open of a version 1 file: %v", err)
	}
	defer old.Close()
	if v, err := old.SchemaVersion(t.Context()); err != nil || v != registry.LatestSchemaVersion() || v < 9 {
		t.Fatalf("SchemaVersion = %d, %v; want the latest (%d), 9 or later", v, err, registry.LatestSchemaVersion())
	}
	if busy, err := old.BusyPRWork(t.Context()); err != nil || len(busy) != 0 {
		t.Errorf("BusyPRWork of an older file = %v, %v; want none", busy, err)
	}
	if _, err := old.PRWorkOf(t.Context(), "example/app", 7); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("PRWorkOf before any work: %v, want ErrNotFound", err)
	}
}

// One row per PR, its repository told case aside: EnsurePRWork makes it
// idle with no task once and finds it after, and the row keeps every
// record in fixwork's types, so what magnum imports reads back as magnum
// wrote it.
func TestAPRWorkRowIsOnePerPRAndKeepsTheFixRecords(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	w, err := r.EnsurePRWork(ctx, "Example/App", 7)
	if err != nil {
		t.Fatal(err)
	}
	if w.Phase != registry.PRWorkIdle || w.TaskID != 0 || w.FixOff != nil || w.Cycle != 0 || w.LeaseHolder != "" || !w.Created.Equal(t0) {
		t.Fatalf("a new row = %+v, want idle, no task, no opt-out, no lease, made at t0", w)
	}
	again, err := r.EnsurePRWork(ctx, "example/app", 7)
	if err != nil || again.ID != w.ID {
		t.Fatalf("EnsurePRWork again = %+v, %v; want the same row %d", again, err, w.ID)
	}
	if _, err := r.EnsurePRWork(ctx, "app", 7); err == nil {
		t.Error("EnsurePRWork of a repository without its owner made a row")
	}

	task := createTask(t, r, "PR-12390")
	clk.Add(time.Minute)
	off := &fixwork.FixOff{Since: t0, By: "magnum fix --off"}
	pushed := []fixwork.CIPush{{Head: "a1a1a1a", At: t0}}
	ciFix := fixwork.CIFixRecord{Starts: []fixwork.CIFixStart{{Head: "b2b2b2b", Entries: []string{"Completion"}, At: t0, By: "autofix",
		Outcome: fixwork.CIOutcomePushed, Pushed: "c3c3c3c"}}}
	dispatch := fixwork.CIStart{Head: "c3c3c3c", Workflow: "ci.yml", At: t0, Run: &fixwork.CIRun{ID: 42, Status: "completed", Conclusion: "success"}}
	rerun := fixwork.CIRerun{Head: "c3c3c3c", ReadAt: t0, Refused: "no run started no job"}
	ledger := fixwork.Ledger{Reviews: []string{fixwork.ReviewKey("r-1")}, FixerHeads: []string{"c3c3c3c"}, Since: t0,
		Handed: map[string]time.Time{"thread:1@2": t0}}
	err = r.UpdatePRWork(ctx, w.ID, []string{registry.PRWorkIdle}, registry.PRWorkFixing, func(u *registry.PRWorkUpdate) {
		u.Task(task.ID)
		u.FixOff(off)
		u.Pushed(pushed)
		u.CIFix(ciFix)
		u.CIDispatch(dispatch)
		u.CIRerun(rerun)
		u.Ledger(ledger)
		u.Set("head", "c3c3c3c")
		u.Set("cycle", 1)
		u.Set("cap", 3)
		u.Set("flipped_at", t0)
		u.Set("imported_at", t0)
		u.Worked()
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.PRWorkOf(ctx, "EXAMPLE/app", 7)
	if err != nil {
		t.Fatal(err)
	}
	now := t0.Add(time.Minute)
	if got.TaskID != task.ID || !reflect.DeepEqual(got.FixOff, off) || !reflect.DeepEqual(got.Pushed, pushed) ||
		!reflect.DeepEqual(got.CIFix, ciFix) || !reflect.DeepEqual(got.CIDispatch, dispatch) || !reflect.DeepEqual(got.CIRerun, rerun) ||
		!reflect.DeepEqual(got.Ledger, ledger) {
		t.Errorf("the records read back as %+v", got)
	}
	if got.Phase != registry.PRWorkFixing || !got.PhaseAt.Equal(now) || got.Head != "c3c3c3c" || got.Cycle != 1 || got.Cap != 3 ||
		!got.FlippedAt.Equal(t0) || !got.ImportedAt.Equal(t0) || !got.WorkedAt.Equal(now) || !got.Updated.Equal(now) {
		t.Errorf("the loop reads back as %+v", got)
	}

	// worked_at keeps the first work; the phase's time moves only with it.
	clk.Add(time.Minute)
	if err := r.UpdatePRWork(ctx, w.ID, nil, registry.PRWorkFixing, func(u *registry.PRWorkUpdate) { u.Worked(); u.FixOff(nil) }); err != nil {
		t.Fatal(err)
	}
	got, _ = r.PRWorkByID(ctx, w.ID)
	if !got.WorkedAt.Equal(now) || !got.PhaseAt.Equal(now) || got.FixOff != nil {
		t.Errorf("after a second work in the same phase: worked %s, phase at %s, fix_off %v; want both at %s and no opt-out",
			got.WorkedAt, got.PhaseAt, got.FixOff, now)
	}
}

// The phase moves by compare-and-set: a move from a phase the row is not
// in is ErrConflict and changes nothing, a missing row is ErrNotFound, an
// unknown phase or a loop without its cycle is refused, and the busy rows
// are the ones whose phase is not idle.
func TestThePRWorkPhaseMovesByCompareAndSet(t *testing.T) {
	t.Parallel()
	r := registrytest0(t)
	ctx := t.Context()
	a, _ := r.EnsurePRWork(ctx, "example/app", 1)
	b, _ := r.EnsurePRWork(ctx, "example/app", 2)
	cycle := func(u *registry.PRWorkUpdate) { u.Set("cycle", 1); u.Set("cap", 3) }
	if err := r.UpdatePRWork(ctx, a.ID, []string{registry.PRWorkIdle}, registry.PRWorkFixing, cycle); err != nil {
		t.Fatal(err)
	}
	err := r.UpdatePRWork(ctx, a.ID, []string{registry.PRWorkIdle}, registry.PRWorkReviewing, nil)
	if !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("a move from idle of a fixing row: %v, want ErrConflict", err)
	}
	if got, _ := r.PRWorkByID(ctx, a.ID); got.Phase != registry.PRWorkFixing {
		t.Fatalf("after the refused move the row is %s", got.Phase)
	}
	if err := r.UpdatePRWork(ctx, 999, nil, registry.PRWorkIdle, nil); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("a move of a missing row: %v, want ErrNotFound", err)
	}
	if err := r.UpdatePRWork(ctx, b.ID, nil, "queued", nil); err == nil {
		t.Error("an unknown phase was taken")
	}
	if err := r.UpdatePRWork(ctx, b.ID, nil, registry.PRWorkFixing, nil); err == nil {
		t.Error("a loop without its cycle was taken")
	}
	if err := r.UpdatePRWork(ctx, b.ID, nil, "", func(u *registry.PRWorkUpdate) { u.Set("phase", registry.PRWorkFixing) }); err == nil {
		t.Error("the phase was set as a column")
	}
	busy, err := r.BusyPRWork(ctx)
	if err != nil || len(busy) != 1 || busy[0].ID != a.ID {
		t.Fatalf("BusyPRWork = %+v, %v; want the fixing row alone", busy, err)
	}
}

// The lease is the factory's hold on a PR while it works: a holder takes a
// free lease, renews its own, takes one that ran out (a daemon that
// stopped), and is refused another's that runs; a release ends only the
// holder's own.
func TestThePRWorkLeaseIsTakenOnlyWhenFreeOrRunOut(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	w, _ := r.EnsurePRWork(ctx, "example/app", 3)
	until := t0.Add(10 * time.Minute)
	if err := r.TakePRWorkLease(ctx, w.ID, "factory 100", until); err != nil {
		t.Fatal(err)
	}
	got, _ := r.PRWorkByID(ctx, w.ID)
	if got.LeaseHolder != "factory 100" || !got.LeaseUntil.Equal(until) || !got.Leased(t0) || got.Leased(until) {
		t.Fatalf("the lease reads %q until %s (leased now %v)", got.LeaseHolder, got.LeaseUntil, got.Leased(t0))
	}
	if err := r.TakePRWorkLease(ctx, w.ID, "factory 200", t0.Add(20*time.Minute)); !errors.Is(err, registry.ErrLeaseHeld) ||
		!errors.Is(err, registry.ErrConflict) {
		t.Fatalf("another holder's take of a running lease: %v, want ErrLeaseHeld", err)
	}
	if err := r.TakePRWorkLease(ctx, w.ID, "factory 100", t0.Add(15*time.Minute)); err != nil {
		t.Fatalf("the holder's renewal: %v", err)
	}
	if err := r.ReleasePRWorkLease(ctx, w.ID, "factory 200"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.PRWorkByID(ctx, w.ID); got.LeaseHolder != "factory 100" {
		t.Fatalf("another holder's release ended the lease: %q", got.LeaseHolder)
	}
	clk.Add(15 * time.Minute)
	if err := r.TakePRWorkLease(ctx, w.ID, "factory 200", t0.Add(30*time.Minute)); err != nil {
		t.Fatalf("a take of a lease that ran out: %v", err)
	}
	if err := r.ReleasePRWorkLease(ctx, w.ID, "factory 200"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.PRWorkByID(ctx, w.ID); got.LeaseHolder != "" || !got.LeaseUntil.IsZero() {
		t.Fatalf("after the release the lease reads %q until %s", got.LeaseHolder, got.LeaseUntil)
	}
	if err := r.TakePRWorkLease(ctx, 999, "factory 100", until); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("a lease of a missing row: %v, want ErrNotFound", err)
	}
}

// registrytest0 is a registry at t0 whose clock stays there.
func registrytest0(t *testing.T) *registry.Registry {
	t.Helper()
	r, _ := openAt(t)
	return r
}
