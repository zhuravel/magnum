package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// A pool slot whose release stalled (a restart cut its fetch_base, a
// GitHub outage failed it) stayed releasing until every held slot of the
// pool was in use, which on 2026-10-23 left 3 of 18 slots out of the pool
// for a day while held slots existed (spec 07 of the 2026-10-23 run). Each
// reconcile now resumes the oldest one that has been releasing for over half
// an hour with no step of its release running, through the same job and
// guards as an eviction (evictJob), once the heavy worker has nothing else
// to do.

// releaseStalledSlot moves PR #2's slot (review1) from held to releasing, as
// an eviction that failed half way leaves it.
func releaseStalledSlot(h *harness, name string) store.Slot {
	h.t.Helper()
	sl := h.slot(name)
	if err := h.st.TransitionSlot(h.ctx, sl.ID, []string{store.SlotHeld}, store.SlotReleasing, nil); err != nil {
		h.t.Fatal(err)
	}
	return h.slot(name)
}

// releasesOf lists the releases the fake slot manager ran.
func releasesOf(h *harness) []string {
	var out []string
	for _, c := range h.sl.all() {
		if len(c) > 8 && c[:8] == "release:" {
			out = append(out, c)
		}
	}
	return out
}

// reconcileNow runs the reconcile on the test's goroutine and then the heavy
// jobs it queued, as the heavy worker does.
func reconcileNow(h *harness) {
	h.t.Helper()
	if err := h.e.reconcile(h.ctx); err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	h.settle()
}

// A release that has stalled for 31 minutes resumes on the next reconcile:
// the slot is free again, with an event that says why. One that stalled 29
// minutes ago does not yet.
func TestAReleaseStalledForOverHalfAnHourResumesOnTheNextReconcile(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	releaseStalledSlot(h, "review1")
	h.advance(29 * time.Minute)
	reconcileNow(h)
	if got := releasesOf(h); len(got) != 0 {
		t.Fatalf("a release stalled for 29 minutes was resumed: %v", got)
	}
	if sl := h.slot("review1"); sl.State != store.SlotReleasing {
		t.Fatalf("slot = %+v", sl)
	}
	h.advance(2 * time.Minute)
	reconcileNow(h)
	if got := releasesOf(h); !slices.Equal(got, []string{"release:review1:evicted"}) {
		t.Fatalf("releases = %v, want the stalled one resumed", got)
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Fatalf("slot after the resumed release: %+v", sl)
	}
	evs := h.events("slot.release_resumed")
	if len(evs) != 1 || evs[0].Level != "info" {
		t.Fatalf("slot.release_resumed events = %+v", evs)
	}
}

// The reconcile the daemon's tick queues does it, on the heavy worker.
func TestTheTicksReconcileResumesAStalledRelease(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	releaseStalledSlot(h, "review1")
	h.advance(31 * time.Minute)
	h.tick() // the reconcile it queues runs on the heavy worker, which then runs the release
	if got := releasesOf(h); !slices.Equal(got, []string{"release:review1:evicted"}) {
		t.Fatalf("releases = %v, want the stalled one resumed", got)
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree {
		t.Fatalf("slot = %+v", sl)
	}
}

// A release a step of which still runs is not resumed, however long the slot
// has been releasing: the newest step event of "slot:<name>:release" is a
// begin that nothing ended, this daemon wrote it and it is not two hours
// old. A step that ended (ok or fail), one begun before this daemon started
// (a dead daemon's), and one older than two hours do not hold it.
func TestAReleaseWithAStepRunningIsNotResumed(t *testing.T) {
	step := func(h *harness, phase string, at time.Time) {
		h.t.Helper()
		subject, name := "slot:review1:release", "fetch_base"
		if _, err := h.st.AppendEvent(h.ctx, store.Event{At: at, Subject: &subject, Kind: store.KindStep, Step: &name, Phase: &phase,
			Message: phase + " " + name}); err != nil {
			h.t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		events func(h *harness, started time.Time)
		resume bool
	}{
		{"a step begun a minute ago", func(h *harness, _ time.Time) { step(h, store.PhaseBegin, h.clock.Now().Add(-time.Minute)) }, false},
		{"a step that ended", func(h *harness, _ time.Time) {
			step(h, store.PhaseBegin, h.clock.Now().Add(-5*time.Minute))
			step(h, store.PhaseFail, h.clock.Now().Add(-4*time.Minute))
		}, true},
		{"a step a dead daemon begun", func(h *harness, _ time.Time) { // the daemon restarted 10 minutes ago
			if err := h.st.SetKV(h.ctx, store.KVDaemonStartedAt, store.FormatTime(h.clock.Now().Add(-10*time.Minute))); err != nil {
				h.t.Fatal(err)
			}
			step(h, store.PhaseBegin, h.clock.Now().Add(-30*time.Minute))
		}, true},
		{"a step begun over two hours ago", func(h *harness, started time.Time) { step(h, store.PhaseBegin, started.Add(time.Minute)) }, true},
		{"no step", func(*harness, time.Time) {}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reviewedPR(2, "b1")
			started := h.clock.Now()
			releaseStalledSlot(h, "review1")
			h.advance(4 * time.Hour)
			tc.events(h, started)
			reconcileNow(h)
			if got := releasesOf(h); (len(got) == 1) != tc.resume {
				t.Fatalf("releases = %v, want it resumed: %v", got, tc.resume)
			}
		})
	}
}

// One stalled release is resumed per reconcile, the oldest first; the heavy
// worker is left alone when it has other jobs.
func TestAReconcileResumesOneStalledReleaseTheOldestFirst(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Pools[0].Max = 2 })
	h.reviewedPR(2, "b1")
	pool := h.cfg.Pools[0]
	older, err := h.st.CreateSlot(h.ctx, store.Slot{Name: pool.Slot(2), RepoFullName: pool.Repo, Kind: store.SlotKindPool, Path: pool.Path(2),
		MainClone: pool.MainClone, PlaceholderBranch: new("review2"), DBSlug: new("review2"), State: store.SlotReleasing})
	if err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	releaseStalledSlot(h, "review1")
	h.advance(time.Hour)
	// The worker has another job queued: not idle, nothing is resumed now.
	if !h.e.enqueueHeavy("provision:other/repo", func(context.Context) error { return nil }) {
		t.Fatal("not queued")
	}
	if err := h.e.reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := releasesOf(h); len(got) != 0 {
		t.Fatalf("releases = %v: resumed while the heavy worker had a job", got)
	}
	h.settle()
	reconcileNow(h)
	if got := releasesOf(h); !slices.Equal(got, []string{"release:review2:evicted"}) {
		t.Fatalf("releases after the first reconcile = %v, want the older review2 only", got)
	}
	if sl := h.slot(older.Name); sl.State != store.SlotFree {
		t.Fatalf("review2 = %+v", sl)
	}
	reconcileNow(h)
	if got := releasesOf(h); !slices.Equal(got, []string{"release:review2:evicted", "release:review1:evicted"}) {
		t.Fatalf("releases after the second reconcile = %v", got)
	}
}

// A stalled release that fails again (a guard refuses it, GitHub is
// unreachable) is tried no more often than every half hour, not at every
// reconcile.
func TestAStalledReleaseThatFailsAgainIsTriedEveryHalfHourAtMost(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	releaseStalledSlot(h, "review1")
	h.sl.mu.Lock()
	h.sl.releaseErr = errors.New("slot review1: tracked changes a human made")
	h.sl.mu.Unlock()
	h.advance(31 * time.Minute)
	reconcileNow(h)
	for range 2 {
		h.advance(10 * time.Minute)
		reconcileNow(h)
	}
	if got := releasesOf(h); len(got) != 1 {
		t.Fatalf("releases = %v: tried again within half an hour", got)
	}
	h.advance(11 * time.Minute)
	reconcileNow(h)
	if got := releasesOf(h); len(got) != 2 {
		t.Fatalf("releases = %v: not tried again after half an hour", got)
	}
}

// A release stalled on a slot whose PR is closing is cleanup's to resume,
// not the reconcile's here.
func TestAStalledReleaseOfAClosingPRIsLeftToCleanup(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	releaseStalledSlot(h, "review1")
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRReviewed}, store.PRClosed, nil); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Hour)
	h.e.resumeStalledReleases(h.ctx) // the reconcile's own cleanup would free it first
	h.settle()
	if evs := h.events("slot.release_resumed"); len(evs) != 0 || slices.Contains(releasesOf(h), "release:review1:evicted") {
		t.Fatalf("resumed the release of a closing PR's slot: %+v, %v", evs, releasesOf(h))
	}
}

// A release this process runs is never resumed, in the moment between two
// of its steps either: the reconcile asks, while the release of a slot that
// has been releasing for an hour is under way (here, inside the slot
// manager), and queues nothing. Every release the engine starts goes
// through releaseSlot, the ones a request's goroutine runs included.
func TestAReleaseThisProcessRunsIsNotResumedBetweenItsSteps(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	sl := releaseStalledSlot(h, "review1")
	h.advance(time.Hour)
	var during int
	h.sl.mu.Lock()
	h.sl.onRelease = func(store.Slot) {
		h.e.resumeStalledReleases(h.ctx)
		during = len(h.e.heavy)
	}
	h.sl.mu.Unlock()
	if err := h.e.releaseSlot(h.ctx, sl, h.cfg.Pools[0], "stopped"); err != nil {
		t.Fatal(err)
	}
	if during != 0 || len(h.events("slot.release_resumed")) != 0 {
		t.Fatalf("the reconcile queued %d jobs for a release under way", during)
	}
	if h.e.releasingNow(sl.ID) {
		t.Fatal("a finished release is still marked as running")
	}
}

// No engine code starts a release but through releaseSlot: the slot
// manager's Release is called once, there.
func TestEveryReleaseTheEngineStartsGoesThroughReleaseSlot(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "d.Slots.Release(") && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				calls = append(calls, fmt.Sprintf("%s:%d", f, i+1))
			}
		}
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "pool.go:") {
		t.Fatalf("the slot manager's Release is called at %v: want once, in releaseSlot (pool.go)", calls)
	}
}
