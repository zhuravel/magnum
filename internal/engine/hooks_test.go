package engine

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/paths"
)

// shortLayout is a layout whose hook socket path fits a Unix socket's:
// t.TempDir's is too long.
func shortLayout(t *testing.T) paths.Layout {
	t.Helper()
	dir, err := os.MkdirTemp("", "mg") //nolint:usetesting // t.TempDir's path is too long for a Unix socket's
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l := paths.Layout{Home: dir}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return l
}

// claim is ClaimDaemon on l, released when the test ends.
func claim(t *testing.T, l paths.Layout) *Claim {
	t.Helper()
	c, err := ClaimDaemon(l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Release)
	return c
}

// serveClaim serves c's listener into a channel of the events it hands on,
// until the test ends.
func serveClaim(t *testing.T, c *Claim) <-chan agenthooks.Event {
	t.Helper()
	if c.listener == nil {
		t.Fatal("the claim has no listener")
	}
	got := make(chan agenthooks.Event, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.listener.Serve(ctx, func(ev agenthooks.Event) { got <- ev })
	}()
	t.Cleanup(func() { cancel(); <-done })
	return got
}

// hookEvent is a Stop of pane, as the hook script sends it.
func hookEvent(pane string) agenthooks.Event {
	return agenthooks.Event{Agent: agenthooks.AgentClaude, Name: agenthooks.NameStop, Pane: pane,
		SessionID: "0b5c1a52-3d2e-4f43-9b7c-4f6a3e2d1c00"}
}

// The review daemon starts its hook listener on Layout.HookSocket under its
// lock: a socket an earlier daemon left there is replaced, and a second
// daemon, which finds the lock taken, neither listens nor touches the first
// one's socket.
func TestTheDaemonListensOnItsHookSocketUnderItsLock(t *testing.T) {
	l := shortLayout(t)
	path := l.HookSocket()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	stale.Close() // the file stays, as a killed daemon's does
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the stale socket: %v", err)
	}

	first := claim(t, l)
	if first.Held() || first.HookSocket() != path || first.Off() != "" {
		t.Fatalf("the first claim: held %v, socket %q, off %q; want the lock and a listener on %s", first.Held(), first.HookSocket(), first.Off(), path)
	}
	got := serveClaim(t, first)

	second := claim(t, l)
	if !second.Held() || second.HookSocket() != "" || second.listener != nil {
		t.Fatalf("the second claim: held %v, socket %q; want the lock taken and no listener", second.Held(), second.HookSocket())
	}
	if err := agenthooks.Send(path, hookEvent("w1:p1")); err != nil {
		t.Fatalf("send to the first daemon's socket after the second claim: %v", err)
	}
	select {
	case ev := <-got:
		if ev.Pane != "w1:p1" {
			t.Fatalf("the first daemon read %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first daemon's listener got nothing")
	}

	first.Release()
	third := claim(t, l)
	if third.Held() {
		t.Fatal("the lock stays taken after the first claim's release")
	}
}

// A hook socket path too long for a Unix socket leaves the daemon on its
// polling alone: no listener, no socket file, and why.
func TestAHookSocketPathTooLongLeavesTheDaemonPolling(t *testing.T) {
	l := paths.Layout{Home: filepath.Join(t.TempDir(), strings.Repeat("x", paths.SocketPathLimit))}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	c := claim(t, l)
	if c.Held() || c.HookSocket() != "" || c.listener != nil {
		t.Fatalf("held %v, socket %q: want the lock and no listener", c.Held(), c.HookSocket())
	}
	if !strings.Contains(c.Off(), "must be under 104") {
		t.Fatalf("off = %q, want the path's length named", c.Off())
	}
	if _, err := os.Lstat(l.HookSocket()); !os.IsNotExist(err) {
		t.Fatalf("a socket file at the long path: %v", err)
	}
}

// A listener that cannot start (a file that is no socket holds its path)
// does not stop the daemon: it runs its ticks with polling alone, and one
// warning says why; the file stays as it was.
func TestAHookListenerThatFailsDoesNotStopTheDaemon(t *testing.T) {
	l := shortLayout(t)
	path := l.HookSocket()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a socket\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := claim(t, l)
	if c.Held() || c.HookSocket() != "" || !strings.Contains(c.Off(), "no socket") {
		t.Fatalf("held %v, socket %q, off %q: want the lock, no listener and why", c.Held(), c.HookSocket(), c.Off())
	}
	logs := &recordLog{}
	h := newHarness(t, func(hs *harness) { hs.d.Logger = slog.New(logs) })
	before := h.ag.count("observe")
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true, Claim: c})
	for deadline := time.Now().Add(clockGuard); h.ag.count("observe") <= before; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the daemon never ticked")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if n := logs.count(slog.LevelWarn, hooksOffMsg); n != 1 {
		t.Fatalf("%q warned %d times, want once", hooksOffMsg, n)
	}
	if why := logs.attr(slog.LevelWarn, hooksOffMsg, "why"); !strings.Contains(why, "no socket") {
		t.Fatalf("the warning's why = %q, want the listener's error", why)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "not a socket\n" {
		t.Fatalf("the file at the socket path: %q, %v", b, err)
	}
}

// At shutdown the daemon stops its listener, whose Serve returns once the
// Wake in flight returned, and only then waits for the later looks Wake
// started (WaitWakes).
func TestTheDaemonStopsItsHookListenerBeforeItWaitsForTheWakes(t *testing.T) {
	l := shortLayout(t)
	c := claim(t, l)
	h := newHarness(t)
	h.prInFlight(1, config.RoleClaudeReview, "mg-1-claude-review-abc123", "w1:p1", t.TempDir())
	gate, started := make(chan struct{}), make(chan struct{}, 1)
	h.ag.mu.Lock()
	h.ag.wakeGate, h.ag.wakeStarted = gate, started
	h.ag.mu.Unlock()
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true, Claim: c})
	for deadline := time.Now().Add(clockGuard); ; time.Sleep(5 * time.Millisecond) {
		if err := agenthooks.Send(l.HookSocket(), hookEvent("w1:p1")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon never listened")
		}
	}
	select {
	case <-started:
	case <-time.After(clockGuard):
		t.Fatal("the event never reached Wake")
	}
	cancel()
	time.Sleep(50 * time.Millisecond) // Run would wait for the wakes now, were it not waiting for Serve
	if n := h.ag.count("wait wakes"); n != 0 {
		t.Fatal("WaitWakes ran while a Wake was in flight")
	}
	close(gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(clockGuard):
		t.Fatal("Run did not stop")
	}
	calls := slices.DeleteFunc(h.ag.all(), func(s string) bool {
		return !strings.HasPrefix(s, "wake ") && !strings.HasPrefix(s, "woke ") && s != "wait wakes"
	})
	if !slices.Equal(calls, []string{"wake stop w1:p1", "woke stop w1:p1", "wait wakes"}) {
		t.Fatalf("calls = %q, want the Wake, its return, then WaitWakes", calls)
	}
	if err := agenthooks.Send(l.HookSocket(), hookEvent("w1:p1")); err == nil {
		t.Fatal("the socket still reads after the daemon stopped")
	}
}

// attr is the value of key in the first record of level and msg ("" for
// none).
func (l *recordLog) attr(level slog.Level, msg, key string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if r.Level != level || r.Message != msg {
			continue
		}
		v := ""
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				v = a.Value.String()
				return false
			}
			return true
		})
		return v
	}
	return ""
}
