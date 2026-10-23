package engine

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// stalledSlotFailingRelease is PR #2's slot review1 stalled in releasing,
// whose every resumed release fails: with an error left on the row, as a
// failed step leaves it (slots.Manager.setLastError), which moves its
// updated_at; with touches false the guard refuses and the row stays as it
// was.
func stalledSlotFailingRelease(t *testing.T, touches ...bool) *harness {
	t.Helper()
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	releaseStalledSlot(h, "review1")
	h.sl.mu.Lock()
	h.sl.releaseErr, h.sl.releaseTouches = errors.New("fetch_base: could not reach origin"), !slices.Contains(touches, false)
	h.sl.mu.Unlock()
	return h
}

// resumesFailing runs n reconciles, each half an hour and a minute after
// the last.
func resumesFailing(h *harness, n int) {
	h.t.Helper()
	for range n {
		h.advance(31 * time.Minute)
		reconcileNow(h)
	}
}

// A release whose resumption fails again and again (a fetch that cannot
// reach GitHub, a guard that keeps refusing, a dependency step that breaks)
// was tried every half hour for good, each try a fetch or a schema reload on
// a machine under memory pressure (spec 07 of the 2026-10-23 run, the cap).
// After 3 resumed releases of the same slot in a row that end without the
// slot free the reconcile stops resuming it: one slot.release_given_up
// warning and one toast say which slot and what to run, and nothing is tried
// again for as long as the slot is as the last try left it.
func TestAStalledReleaseThatFailsThreeTimesInARowIsGivenUp(t *testing.T) {
	t.Run("a failure that leaves its error on the row", func(t *testing.T) { failsThreeTimesAndIsGivenUp(t, stalledSlotFailingRelease(t)) })
	t.Run("a refusal that leaves the row alone", func(t *testing.T) {
		h := stalledSlotFailingRelease(t, false)
		h.advance(31 * time.Minute)
		reconcileNow(h)
		for range 3 { // tried at most every half hour, not at every reconcile
			h.advance(9 * time.Minute)
			reconcileNow(h)
		}
		if got := releasesOf(h); len(got) != 1 {
			t.Fatalf("releases = %v: tried again within half an hour of a refusal that left the row alone", got)
		}
		h.advance(5 * time.Minute)
		reconcileNow(h)
		h.advance(31 * time.Minute)
		reconcileNow(h)
		h.advance(31 * time.Minute)
		reconcileNow(h)
		if got := releasesOf(h); len(got) != 3 || len(h.events("slot.release_given_up")) != 1 {
			t.Fatalf("releases %v, give-ups %d: want three tries and one give-up", got, len(h.events("slot.release_given_up")))
		}
	})
}

func failsThreeTimesAndIsGivenUp(t *testing.T, h *harness) {
	t.Helper()
	resumesFailing(h, 2)
	if got := releasesOf(h); len(got) != 2 || len(h.events("slot.release_given_up")) != 0 {
		t.Fatalf("after two failures: releases %v, give-up events %d: want two tries and no give-up", got, len(h.events("slot.release_given_up")))
	}
	resumesFailing(h, 1)
	evs := h.events("slot.release_given_up")
	if got := releasesOf(h); len(got) != 3 || len(evs) != 1 || evs[0].Level != "warn" {
		t.Fatalf("after the third: releases %v, events %+v: want three tries and one warning", got, evs)
	}
	h.flush()
	toasts := toastsWith(h, "review1")
	if len(toasts) != 1 || !strings.Contains(toasts[0], "`magnum release review1`") || !strings.Contains(toasts[0], "`magnum slots repair review1`") {
		t.Fatalf("toasts = %q, want one that names the slot and the commands that hand it back and repair it", toasts)
	}
	resumesFailing(h, 4)
	h.flush()
	if got := releasesOf(h); len(got) != 3 {
		t.Fatalf("releases = %v: a fourth try after the give-up", got)
	}
	if n := len(h.events("slot.release_given_up")); n != 1 {
		t.Fatalf("slot.release_given_up events = %d, want 1", n)
	}
	if toasts := toastsWith(h, "review1"); len(toasts) != 1 {
		t.Fatalf("toasts = %q, want the one", toasts)
	}
	rec := releaseRecordOf(t, h, "review1")
	if !rec.GivenUp || rec.Failed != 3 {
		t.Fatalf("record = %+v, want given up after 3 failures", rec)
	}
}

// releaseRecordOf is slot name's record of resumed releases in the registry.
func releaseRecordOf(t *testing.T, h *harness, name string) releaseResumes {
	t.Helper()
	v, ok := h.kvOf(kvSlotReleaseResumes(h.slot(name).ID))
	if !ok {
		return releaseResumes{}
	}
	var rec releaseResumes
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		t.Fatalf("record %q: %v", v, err)
	}
	return rec
}

// What gives a slot up gives it up for the next daemon too: a restart (the
// daemon restarts every few hours, and its start reconciles at once) neither
// tries it again nor says so again.
func TestAGivenUpReleaseStaysGivenUpAcrossARestart(t *testing.T) {
	h := stalledSlotFailingRelease(t)
	resumesFailing(h, 3)
	h.e = New(h.d)
	h.startup()
	resumesFailing(h, 2)
	h.flush()
	if got := releasesOf(h); len(got) != 3 {
		t.Fatalf("releases = %v: the restarted daemon tried the slot again", got)
	}
	if n, toasts := len(h.events("slot.release_given_up")), toastsWith(h, "review1"); n != 1 || len(toasts) != 1 {
		t.Fatalf("give-up events %d, toasts %q: want one each", n, toasts)
	}
}

// A slot changed by another path (the operator's repair or release, a PR
// waiting for a slot that resumed it) is resumed again once it has been
// stalled for half an hour in its new state, and gives up again after three
// more failures: the state and updated_at the last try left are the mark.
func TestAGivenUpReleaseIsResumedAgainWhenTheSlotChangedByAnotherPath(t *testing.T) {
	for name, c := range map[string]struct {
		after  time.Duration // how long after the last try the other path comes
		change func(h *harness, sl store.Slot) error
	}{
		// At the very instant of the last try: only the state differs.
		"its state": {0, func(h *harness, sl store.Slot) error {
			return h.st.TransitionSlot(h.ctx, sl.ID, []string{store.SlotReleasing}, store.SlotDirtySchema, nil)
		}},
		"its updated_at": {time.Minute, func(h *harness, sl store.Slot) error {
			return h.st.UpdateSlotFields(h.ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("last_error", "someone looked at it") })
		}},
	} {
		t.Run(name, func(t *testing.T) {
			h := stalledSlotFailingRelease(t)
			resumesFailing(h, 3)
			h.advance(c.after)
			if err := c.change(h, h.slot("review1")); err != nil {
				t.Fatal(err)
			}
			h.advance(29 * time.Minute)
			reconcileNow(h)
			if got := releasesOf(h); len(got) != 3 {
				t.Fatalf("releases = %v: resumed before the slot stalled half an hour in its new state", got)
			}
			h.advance(2 * time.Minute)
			reconcileNow(h)
			if got := releasesOf(h); len(got) != 4 {
				t.Fatalf("releases = %v: not resumed after the slot changed", got)
			}
			if n := len(h.events("slot.release_given_up")); n != 1 {
				t.Fatalf("slot.release_given_up events = %d after one more failure, want the first only", n)
			}
			if rec := releaseRecordOf(t, h, "review1"); rec.GivenUp || rec.Failed != 1 {
				t.Fatalf("record = %+v, want the count started over", rec)
			}
			resumesFailing(h, 2)
			resumesFailing(h, 2)
			h.flush()
			if got := releasesOf(h); len(got) != 6 {
				t.Fatalf("releases = %v: want three more tries, then none", got)
			}
			if n, toasts := len(h.events("slot.release_given_up")), toastsWith(h, "review1"); n != 2 || len(toasts) != 2 {
				t.Fatalf("give-up events %d, toasts %q: want two of each", n, toasts)
			}
		})
	}
}

// Only a resumed release that ran and failed counts: one the PR's
// reservation turned into a no-op (a round or other slot work holds the PR)
// ran no step, so any number of them give nothing up; the release then goes
// through three failures as usual.
func TestAResumeThatRanNoReleaseIsNotCounted(t *testing.T) {
	h := stalledSlotFailingRelease(t)
	prID := h.pr(2).ID
	if !h.e.reserveEviction(prID) {
		t.Fatal("the PR is not free to reserve")
	}
	resumesFailing(h, 4)
	if got := releasesOf(h); len(got) != 0 || len(h.events("slot.release_given_up")) != 0 {
		t.Fatalf("releases %v, give-ups %d while the PR was reserved", got, len(h.events("slot.release_given_up")))
	}
	h.e.endEviction(prID)
	resumesFailing(h, 2)
	if len(h.events("slot.release_given_up")) != 0 {
		t.Fatal("given up after two failures")
	}
	resumesFailing(h, 1)
	if got := releasesOf(h); len(got) != 3 || len(h.events("slot.release_given_up")) != 1 {
		t.Fatalf("releases %v, give-ups %d: want three tries and one give-up", got, len(h.events("slot.release_given_up")))
	}
}

// A release that ends with the slot free starts the count over, and its
// record goes.
func TestAReleaseThatEndsFreeStartsTheCountOver(t *testing.T) {
	h := stalledSlotFailingRelease(t)
	resumesFailing(h, 2)
	if rec := releaseRecordOf(t, h, "review1"); rec.Failed != 2 {
		t.Fatalf("record = %+v, want two failures", rec)
	}
	h.sl.mu.Lock()
	h.sl.releaseErr = nil
	h.sl.mu.Unlock()
	resumesFailing(h, 1)
	if sl := h.slot("review1"); sl.State != store.SlotFree {
		t.Fatalf("slot = %+v, want the third try to free it", sl)
	}
	if v, ok := h.kvOf(kvSlotReleaseResumes(h.slot("review1").ID)); ok {
		t.Fatalf("the freed slot keeps its record: %q", v)
	}
}
