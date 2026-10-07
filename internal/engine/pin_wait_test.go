package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// pinnedThenPushed restores #2 with `magnum open` (the PR and review1
// pinned), pushes b2 past the re-review interval and lets the push quiet
// period end, so its re-review waits on the pin; it returns when the wait
// was recorded.
func (h *harness) pinnedThenPushed() store.PR {
	h.t.Helper()
	h.openedPR()
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(6 * time.Minute)
	h.tick()
	return h.wantState(2, store.PRRereviewPending)
}

// A slot `magnum open` pinned holds the PR's round as a wait, not an error:
// no last_error (the board's title shows no error mark), the wait names the
// pin and who set it, its cell reads "re-review · pinned → u", and its
// sentence says how to hand the slot back.
func TestAPinnedSlotIsAWaitNotAnError(t *testing.T) {
	h := newHarness(t)
	pr := h.pinnedThenPushed()
	opened, err := h.st.EventsOfKindsSince(h.ctx, time.Time{}, evPROpened)
	if err != nil || len(opened) != 1 {
		t.Fatalf("pr.opened events: %v %v", opened, err)
	}
	openedAt := opened[0].At
	if le := deref(pr.LastError); le != "" {
		t.Fatalf("the pin was recorded as an error: %q", le)
	}
	now := h.clock.Now()
	w := waitOf(t, h, 2)
	if w.Reason != WaitPinned || w.Subject != "magnum open" {
		t.Fatalf("wait = %+v, want the pin by magnum open", w)
	}
	if got := w.Short(now); got != "re-review · pinned → u" {
		t.Errorf("short = %q", got)
	}
	want := "re-review waits: slot review1 is pinned by magnum open since " + openedAt.Local().Format("15:04") +
		"; `magnum unpin talkable#2` lets it run"
	if got := w.Sentence("talkable#2", now); !strings.HasPrefix(got, want) {
		t.Errorf("sentence = %q, want %q…", got, want)
	}
	if row := h.boardRow(2); row.LastError != "" {
		t.Errorf("board row carries an error: %q", row.LastError)
	}
}

// A PR whose wait an older daemon recorded as an error (last_error "slot
// review1 is pinned (magnum unpin)") loses it on the next dispatch.
func TestAPinRecordedAsAnErrorIsClearedOnTheNextDispatch(t *testing.T) {
	h := newHarness(t)
	pr := h.pinnedThenPushed()
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("last_error", "slot review1 is pinned (magnum unpin)") }); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	h.tick()
	if le := deref(h.pr(2).LastError); le != "" {
		t.Fatalf("last_error = %q, want the old pin error cleared", le)
	}
}

// A round that has waited 30 minutes on a pin toasts once, per PR and pin;
// the pin never lapses by itself, and the unpin lets the round run.
func TestARoundWaitingOnAPinToastsOnceAfter30Minutes(t *testing.T) {
	h := newHarness(t, noEveryReview)
	h.pinnedThenPushed()
	before := len(h.rd.all())
	h.advance(27 * time.Minute)
	h.tick()
	h.flush() // 29 minutes
	if got := toastsWith(h, "waits for your pin"); len(got) != 0 {
		t.Fatalf("toasted before 30 minutes: %q", got)
	}
	h.advance(time.Minute)
	h.tick()
	h.flush()
	got := toastsWith(h, "waits for your pin")
	if len(got) != 1 || !strings.HasPrefix(got[0], "talkable#2 waits for your pin (magnum unpin talkable/talkable#2) | ") ||
		!strings.Contains(got[0], "slot review1") || !strings.Contains(got[0], "magnum open") {
		t.Fatalf("toasts = %q, want one pin toast", h.nh.shown())
	}
	for range 6 {
		h.advance(time.Hour)
		h.tick()
	}
	h.flush()
	if n := len(toastsWith(h, "waits for your pin")); n != 1 {
		t.Fatalf("the pin toasted %d times", n)
	}
	if n := len(h.rd.all()); n != before {
		t.Fatalf("a round ran (%d) on a pinned slot", n-before)
	}
	if sl := h.slot("review1"); !sl.Pinned {
		t.Fatal("the pin lapsed by itself")
	}
	h.enqueue(ReqUnpin, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	h.wantState(2, store.PRReviewed)
}

// A retry says which attempt comes next and what failed, in a word read
// from the error the failed attempt left; the narrow form drops the word.
func TestRetryWaitSaysTheAttemptAndTheCause(t *testing.T) {
	for _, tc := range []struct {
		lastError, cause string
	}{
		{"fetch pr 2: git fetch origin pull/2/head exited 128: fatal: unable to access", "setup"},
		{"deps bundle: bundle install exited 1: Could not find gem", "setup"},
		{"error: pipeline: judge session lost: pane p-judge is gone", "judge lost"},
		{"error: pipeline: codex-review session lost: agent gone", "codex-review lost"},
		{"error: pipeline: could not read the result", "error"},
		{"timeout: judge_timeout passed without a result", "timeout"},
		{"round stopped: shutdown", "stopped"},
		{"human active in agent pane", "human active"},
		{"agents: start judge: agent busy", "agent busy"},
		{"infrastructure (network): fetch failed", "infra"},
		{"the judge found the PR closed", "found closed"},
	} {
		t.Run(tc.cause, func(t *testing.T) {
			h := newHarness(t)
			pr := pushedAfter(t, h, 40*time.Minute)
			at := time.Date(2026, 10, 5, 22, 57, 0, 0, time.Local)
			if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) {
				u.Set("next_attempt_at", at)
				u.Set("attempts", 1)
				u.Set("last_error", tc.lastError)
			}); err != nil {
				t.Fatal(err)
			}
			h.tick()
			now := h.clock.Now()
			w := waitOf(t, h, 2)
			if w.Reason != WaitRetry || w.Count != 2 || w.Max != maxAttempts || w.Subject != tc.cause {
				t.Fatalf("wait = %+v", w)
			}
			if got, want := w.Short(now), "re-review · retry 2/3 "+tc.cause+" → 22:57"; got != want {
				t.Errorf("short = %q, want %q", got, want)
			}
			if got, want := w.Narrow(now), "re-review · retry 2/3 → 22:57"; got != want {
				t.Errorf("narrow = %q, want %q", got, want)
			}
		})
	}
}

// Every wait but a retry has the same narrow form as its short one.
func TestNarrowWaitIsTheShortOneButForARetry(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	for _, w := range []Wait{
		{Reason: WaitQuiet, Rereview: true, Until: now.Add(5 * time.Minute)},
		{Reason: WaitPinned, Rereview: true, Subject: "magnum open"},
		{Reason: WaitCap, Count: 6, Max: 6, Until: now.Add(12 * time.Hour)},
		{Reason: WaitRetry, Count: 2, Max: 3, Until: now.Add(time.Minute)}, // an older wait without a cause
	} {
		if w.Narrow(now) != w.Short(now) {
			t.Errorf("%+v: narrow %q, short %q", w, w.Narrow(now), w.Short(now))
		}
	}
}
