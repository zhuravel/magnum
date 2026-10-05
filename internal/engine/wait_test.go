package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// waitOf is the wait the last tick recorded for PR n.
func waitOf(t *testing.T, h *harness, n int) Wait {
	t.Helper()
	v, ok := kvValue(h, KVPRWait(h.pr(n).ID))
	w, parsed := ParseWait(v)
	if !ok || !parsed {
		t.Fatalf("no wait recorded for #%d (%q)", n, v)
	}
	return w
}

// pushedAfter reviews #2 at b1, lets d pass and pushes b2 (one poll, which
// also dispatches and records the waits).
func pushedAfter(t *testing.T, h *harness, d time.Duration) store.PR {
	t.Helper()
	h.reviewedPR(2, "b1")
	pollPR(h, d, 2, "b2")
	return h.wantState(2, store.PRRereviewPending)
}

func TestWaitTimingReasons(t *testing.T) {
	t.Run("quiet period", func(t *testing.T) {
		h := newHarness(t)
		pushedAfter(t, h, 40*time.Minute)
		w := waitOf(t, h, 2)
		if w.Reason != WaitQuiet || !w.Rereview || !w.Until.Equal(h.clock.Now().Add(5*time.Minute)) {
			t.Fatalf("wait = %+v", w)
		}
		if got, want := w.Short(h.clock.Now()), "re-review · quiet → "+h.clock.Now().Add(5*time.Minute).Format("15:04"); got != want {
			t.Errorf("short = %q, want %q", got, want)
		}
		if got := w.Sentence("talkable#2", h.clock.Now()); !strings.HasPrefix(got, "re-review waits for the push quiet period (5m) until ") ||
			!strings.HasSuffix(got, "; `magnum review talkable#2` runs it now") {
			t.Errorf("sentence = %q", got)
		}
	})
	t.Run("interval", func(t *testing.T) {
		h := newHarness(t)
		pr := pushedAfter(t, h, time.Minute)
		w := waitOf(t, h, 2)
		if w.Reason != WaitInterval || !w.Until.Equal(pr.LastRoundStartedAt.Add(30*time.Minute)) {
			t.Fatalf("wait = %+v", w)
		}
		if !strings.Contains(w.Sentence("talkable#2", h.clock.Now()), "the minimum re-review interval (30m since the last round)") {
			t.Errorf("sentence = %q", w.Sentence("talkable#2", h.clock.Now()))
		}
	})
	t.Run("draft interval", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		h.advance(40 * time.Minute)
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2", draft: true})
		h.tick()
		if w := waitOf(t, h, 2); w.Reason != WaitDraftInterval || w.Short(h.clock.Now()) == "" {
			t.Fatalf("wait = %+v", w)
		}
	})
	t.Run("burst", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		pollPR(h, 40*time.Minute, 2, "b2")
		pollPR(h, time.Minute, 2, "b3")
		pollPR(h, time.Minute, 2, "b4")
		w := waitOf(t, h, 2)
		if w.Reason != WaitBurst || !w.Until.Equal(h.clock.Now().Add(15*time.Minute)) || !strings.Contains(w.Detail, "15m after 3 pushes within 30m") {
			t.Fatalf("wait = %+v", w)
		}
	})
	t.Run("daily cap", func(t *testing.T) {
		h := newHarness(t, func(h *harness) { h.cfg.Daemon.MaxRoundsPerPRPerDay = 1 })
		pushedAfter(t, h, 40*time.Minute)
		h.advance(10 * time.Minute)
		h.tick()
		w := waitOf(t, h, 2)
		midnight := time.Date(2026, 10, 6, 0, 0, 0, 0, time.Local)
		if w.Reason != WaitCap || w.Count != 1 || w.Max != 1 || !w.Until.Equal(midnight) {
			t.Fatalf("wait = %+v", w)
		}
		if got := w.Short(h.clock.Now()); got != "re-review · cap 1/1 → 00:00" {
			t.Errorf("short = %q", got)
		}
		if got := w.Sentence("talkable#2", h.clock.Now()); got != "re-review waits for the daily round cap (1 of 1 automatic rounds today) until 00:00; `magnum review talkable#2` runs it now" {
			t.Errorf("sentence = %q", got)
		}
	})
}

func TestWaitRetryMuteAndQuietHours(t *testing.T) {
	t.Run("retry", func(t *testing.T) {
		h := newHarness(t)
		pr := pushedAfter(t, h, 40*time.Minute)
		at := h.clock.Now().Add(20 * time.Minute)
		if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) {
			u.Set("next_attempt_at", at)
			u.Set("attempts", 1)
			u.Set("last_error", "timeout: judge\ntimed out")
		}); err != nil {
			t.Fatal(err)
		}
		h.tick()
		if w := waitOf(t, h, 2); w.Reason != WaitRetry || !w.Until.Equal(at) || w.Detail != "a retry (attempt 2) after: timeout: judge timed out" {
			t.Fatalf("wait = %+v", w)
		}
	})
	t.Run("muted", func(t *testing.T) {
		h := newHarness(t)
		pr := pushedAfter(t, h, 40*time.Minute)
		if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("muted", true) }); err != nil {
			t.Fatal(err)
		}
		h.advance(10 * time.Minute)
		h.tick()
		w := waitOf(t, h, 2)
		if w.Reason != WaitMuted || !strings.HasSuffix(w.Sentence("talkable#2", h.clock.Now()), "`magnum unmute talkable#2` brings it back") {
			t.Fatalf("wait = %+v (%q)", w, w.Sentence("talkable#2", h.clock.Now()))
		}
	})
	t.Run("quiet hours", func(t *testing.T) {
		h := newHarness(t)
		pushedAfter(t, h, 40*time.Minute)
		h.cfg.Daemon.QuietHours = "10:00-23:00" // after the first review
		h.advance(10 * time.Minute)
		h.tick()
		w := waitOf(t, h, 2)
		if w.Reason != WaitQuietHours || w.Until.Hour() != 23 || w.Short(h.clock.Now()) != "re-review · quiet hours → 23:00" {
			t.Fatalf("wait = %+v", w)
		}
	})
}

func TestWaitGlobalReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(h *harness)
		reason string
		hint   string
	}{
		{"daemon paused", func(h *harness) { h.e.setKV(h.ctx, KVDaemonPaused, "1") }, WaitPaused, "`magnum resume` lifts the pause"},
		{"draining", func(h *harness) { h.e.setKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now())) }, WaitDraining, "the end of the rounds in flight"},
		{"infra", func(h *harness) {
			h.e.setKV(h.ctx, KVInfraPausedUntil, store.FormatTime(h.clock.Now().Add(4*time.Minute)))
			h.e.setKV(h.ctx, KVInfraPausedReason, "ssh")
		}, WaitInfra, "`magnum resume` lifts the pause"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			pushedAfter(t, h, 40*time.Minute)
			h.advance(10 * time.Minute)
			tc.set(h)
			h.tick()
			w := waitOf(t, h, 2)
			if w.Reason != tc.reason || !strings.HasSuffix(w.Sentence("talkable#2", h.clock.Now()), tc.hint) {
				t.Fatalf("wait = %+v (%q)", w, w.Sentence("talkable#2", h.clock.Now()))
			}
		})
	}
	t.Run("herdr down", func(t *testing.T) {
		h := newHarness(t)
		pushedAfter(t, h, 40*time.Minute)
		h.advance(10 * time.Minute)
		h.e.noteWaits(h.ctx, tickState{herdrUp: false})
		if w := waitOf(t, h, 2); w.Reason != WaitHerdr {
			t.Fatalf("wait = %+v", w)
		}
	})
}

func TestWaitDispatchGates(t *testing.T) {
	t.Run("kind paused", func(t *testing.T) {
		h := newHarness(t)
		pushedAfter(t, h, 40*time.Minute)
		h.advance(10 * time.Minute)
		until := h.clock.Now().Add(time.Hour)
		h.e.setKV(h.ctx, KVToolPausedUntil("codex"), store.FormatTime(until))
		h.e.setKV(h.ctx, KVToolPausedReason("codex"), "usage_limit")
		h.tick()
		w := waitOf(t, h, 2)
		if w.Reason != WaitKind || w.Subject != "codex" || !w.Until.Equal(until) || w.Short(h.clock.Now()) != "re-review · codex paused → "+until.Format("15:04") {
			t.Fatalf("wait = %+v", w)
		}
		if got := w.Sentence("talkable#2", h.clock.Now()); got != "re-review waits: codex paused (usage_limit) until "+until.Format("15:04")+"; `magnum resume` lifts the pause" {
			t.Errorf("sentence = %q", got)
		}
	})
	t.Run("identity", func(t *testing.T) {
		h := newHarness(t)
		pushedAfter(t, h, 40*time.Minute)
		h.advance(10 * time.Minute)
		h.e.setKV(h.ctx, KVIdentityCheck("talkable-app"), "fail")
		h.e.setKV(h.ctx, KVIdentityError("talkable-app"), "installation token refused")
		h.tick()
		if w := waitOf(t, h, 2); w.Reason != WaitIdentity || !strings.Contains(w.Detail, "installation token refused") {
			t.Fatalf("wait = %+v", w)
		}
	})
	t.Run("capacity", func(t *testing.T) {
		h := newHarness(t, func(h *harness) { h.cfg.Daemon.MaxConcurrentReviews = 1 })
		h.open(prSpec{n: 1, head: "base1"})
		h.startup()
		h.tick()
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
		h.tick()
		h.advance(5 * time.Minute)
		h.rd.gate = make(chan struct{})
		defer func() { close(h.rd.gate); h.settle() }()
		if err := h.e.Tick(h.ctx); err != nil {
			t.Fatal(err)
		}
		w := waitOf(t, h, 3)
		if w.Reason != WaitCapacity || w.Rereview || w.Short(h.clock.Now()) != "review · capacity" ||
			w.Sentence("talkable#3", h.clock.Now()) != "review waits: review capacity: 1 rounds running (max_concurrent_reviews 1)" {
			t.Fatalf("wait = %+v (%q)", w, w.Sentence("talkable#3", h.clock.Now()))
		}
	})
	t.Run("next", func(t *testing.T) {
		h := newHarness(t)
		pr := pushedAfter(t, h, 40*time.Minute)
		h.advance(10 * time.Minute)
		w := h.e.waitFor(h.ctx, h.pr(2), nil, h.clock.Now())
		if w.Reason != WaitNext || w.Short(h.clock.Now()) != "re-review · next tick" || w.Sentence("talkable#2", h.clock.Now()) != "re-review starts at the next dispatch" {
			t.Fatalf("wait = %+v for %+v", w, pr)
		}
	})
}

// A started round clears the PR's wait.
func TestWaitClearedWhenTheRoundStarts(t *testing.T) {
	h := newHarness(t)
	pushedAfter(t, h, 40*time.Minute)
	waitOf(t, h, 2)
	h.advance(10 * time.Minute)
	h.tick()
	if v, ok := kvValue(h, KVPRWait(h.pr(2).ID)); ok {
		t.Fatalf("wait %q kept after the round started", v)
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Minute: "5m", 2 * time.Hour: "2h", 90 * time.Minute: "1h30m", 45 * time.Second: "45s"} {
		if got := humanDuration(d); got != want {
			t.Errorf("%s: %q, want %q", d, got, want)
		}
	}
}

func TestWaitClock(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.Local)
	for at, want := range map[time.Time]string{
		now.Add(9 * time.Minute):                        "14:09",
		time.Date(2026, 10, 6, 0, 0, 0, 0, time.Local):  "00:00",
		time.Date(2026, 10, 7, 9, 30, 0, 0, time.Local): "Wed 09:30",
		time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local): "Oct 20 09:00",
		now.Add(-2 * 24 * time.Hour):                    "Oct 3 14:00",
	} {
		if got := waitClock(at, now); got != want {
			t.Errorf("waitClock(%s) = %q, want %q", at, got, want)
		}
	}
}

// TestWaitCellNeverShowsAMinuteThatHasPassed: a wait ends at a second, the
// cell shows minutes, so the end is rounded up to the minute it falls in
// (quiet until 16:28:51 shows "→ 16:29", not "→ 16:28" for most of a
// minute), and a wait the next tick has not yet replaced says "→ now".
func TestWaitCellNeverShowsAMinuteThatHasPassed(t *testing.T) {
	at := func(h, m, s int) time.Time { return time.Date(2026, 10, 5, h, m, s, 0, time.Local) }
	w := Wait{Reason: WaitQuiet, Rereview: true, Until: at(16, 28, 51)}
	if got, want := w.Short(at(16, 28, 30)), "re-review · quiet → 16:29"; got != want {
		t.Errorf("short before the end = %q, want %q", got, want)
	}
	if got, want := w.Short(at(16, 29, 5)), "re-review · quiet → now"; got != want {
		t.Errorf("short after the end = %q, want %q", got, want)
	}
	if got, want := (Wait{Reason: WaitQuiet, Until: at(16, 40, 0)}).Short(at(16, 30, 0)), "review · quiet → 16:40"; got != want {
		t.Errorf("short on a whole minute = %q, want %q", got, want)
	}
	if got := w.Sentence("talkable#2", at(16, 28, 30)); !strings.Contains(got, "until 16:29") {
		t.Errorf("sentence = %q", got)
	}
}

// TestDrainWaitIsNotCalledAPause: a drain (magnum daemon-restart --drain)
// holds every PR, a requested one too, but it is not `magnum pause`: its
// cell says "draining" and its sentence does not offer `magnum resume`,
// which would not lift it.
func TestDrainWaitIsNotCalledAPause(t *testing.T) {
	h := newHarness(t)
	pushedAfter(t, h, 40*time.Minute)
	h.advance(10 * time.Minute)
	h.e.setKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now()))
	h.tick()
	h.wantState(2, store.PRRereviewPending)
	w := waitOf(t, h, 2)
	if w.Reason != WaitDraining || w.Short(h.clock.Now()) != "re-review · draining" {
		t.Fatalf("wait = %+v (%q)", w, w.Short(h.clock.Now()))
	}
	if got := w.Sentence("talkable#2", h.clock.Now()); strings.Contains(got, "magnum resume") || !strings.Contains(got, "restart") {
		t.Errorf("sentence = %q", got)
	}
}
