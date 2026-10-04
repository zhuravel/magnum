package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/notify"
)

// TestUrgentToastDoesNotBlockAndShutdownCancelsIt: an urgent toast that
// herdr keeps rate-limiting runs in its own goroutine (the caller returns at
// once); shutdown waits a bounded time, then cancels it, which releases its
// dedupe key; later urgent toasts are only logged.
func TestUrgentToastDoesNotBlockAndShutdownCancelsIt(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.nh.decline = "rate_limited"
		h.d.Notifier.Sleep = func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }
	})
	h.e.toastWait = 20 * time.Millisecond

	returned := make(chan struct{})
	go func() {
		h.e.urgent("attention:test", "magnum: x needs attention", "body", time.Hour)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("urgent blocked its caller")
	}

	stopped := make(chan struct{})
	go func() { h.e.stopToasts(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stopToasts did not cancel the retrying toast")
	}
	h.e.toastWG.Wait()
	if ok, err := h.st.ShouldSend(h.ctx, "attention:test", time.Hour); err != nil || !ok {
		t.Fatalf("the cancelled toast kept its dedupe key: %v %v", ok, err)
	}

	h.nh.decline = ""
	h.e.urgent("late", "magnum: late", "body", time.Hour)
	h.e.toastWG.Wait()
	if got := h.nh.titles("late"); len(got) != 0 {
		t.Fatalf("a toast after shutdown was shown: %v", got)
	}
}

// TestNeedsAttentionToastIsUrgent: herdr answering rate_limited once does
// not lose the attention toast (it is retried).
func TestNeedsAttentionToastIsUrgent(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.d.Notifier.Sleep = func(context.Context, time.Duration) error {
			h.nh.mu.Lock()
			h.nh.decline = "" // the retry is shown
			h.nh.mu.Unlock()
			return nil
		}
	})
	h.nh.decline = "rate_limited"
	h.e.urgent("attention:1:failed", "magnum: talkable#2 needs attention", "failed: boom", attentionWindow)
	h.settle()
	if got := h.nh.titles("needs attention"); len(got) != 1 {
		t.Fatalf("urgent toast through a rate limit = %v", got)
	}
}

// TestNewRepositoriesAreAnnouncedInOneSummary: repositories that appear
// together are one toast ("magnum: 3 new repositories"), sent once the
// batch window passed, not one toast each.
func TestNewRepositoriesAreAnnouncedInOneSummary(t *testing.T) {
	h := newHarness(t)
	h.gh.set("zhuravel/first")
	h.startup()
	h.tick() // first sync of the owner: no announcement
	for _, r := range []string{"zhuravel/a", "zhuravel/b", "zhuravel/c"} {
		h.gh.set(r)
	}
	h.tick()
	if got := h.nh.titles("new repo"); len(got) != 0 {
		t.Fatalf("announced before the batch window: %v", got)
	}
	h.advance(notify.DefaultBatchWindow + time.Second)
	h.tick()
	got := h.nh.titles("new repo")
	if len(got) != 1 || got[0] != "magnum: 3 new repositories" {
		t.Fatalf("new repository toasts = %v", got)
	}
	h.advance(notify.DefaultBatchWindow + time.Second)
	h.tick()
	if got := h.nh.titles("new repo"); len(got) != 1 {
		t.Fatalf("announced again: %v", got)
	}
}

// TestTabBarFileCarriesItsTimestamp: the tab-bar file starts with when it
// was written and how long it stays fresh (3 poll intervals), so the herdr
// command can show "magnum down" for a daemon that stopped ticking.
func TestTabBarFileCarriesItsTimestamp(t *testing.T) {
	h := newHarness(t)
	h.startup()
	h.tick()
	at, maxAge, line, ok := readTabBarFile(h.layout.TabBar())
	if !ok || !at.Equal(h.clock.Now().Truncate(time.Second)) || maxAge != 90*time.Second || line != "magnum · idle" {
		t.Fatalf("tab bar: at %v (now %v) max %v line %q ok %v", at, h.clock.Now(), maxAge, line, ok)
	}
	// The next tick's heartbeat refreshes the timestamp before anything else.
	h.advance(time.Minute)
	h.e.heartbeat()
	if at2, _, line2, _ := readTabBarFile(h.layout.TabBar()); !at2.Equal(h.clock.Now().Truncate(time.Second)) || line2 != line {
		t.Fatalf("heartbeat: at %v line %q", at2, line2)
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("multi-line status: %q", line)
	}
}
