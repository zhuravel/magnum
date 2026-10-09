package mysqlx

import (
	"context"
	"errors"
	"testing"
	"time"
)

// shortDeadlines shortens the deadlines calls get when their caller's
// context has none, for one test.
func shortDeadlines(t *testing.T, call, drop time.Duration) {
	t.Helper()
	oldCall, oldDrop := callTimeout, dropTimeout
	callTimeout, dropTimeout = call, drop
	t.Cleanup(func() { callTimeout, dropTimeout = oldCall, oldDrop })
}

// within runs fn and fails the test unless it returns a deadline error
// within limit (the call's own deadline and some slack).
func within(t *testing.T, name string, limit time.Duration, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s = %v, want its own deadline", name, err)
		}
		if took := time.Since(start); took > limit {
			t.Errorf("%s took %s, want at most %s", name, took, limit)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s still blocked after 10s: it has no deadline of its own", name)
	}
}

// A stalled server, or a DROP waiting on a metadata lock, must not block
// the caller (the engine's single heavy worker) for good: when the caller's
// context has no deadline, every call brings its own.
func TestCallsOnAStalledServerEndAtTheirOwnDeadline(t *testing.T) {
	shortDeadlines(t, 50*time.Millisecond, 150*time.Millisecond)
	srv := &fakeServer{stall: true}
	c := newFakeClient(t, srv)
	ctx := context.Background()
	within(t, "Ping", 2*time.Second, func() error { return c.Ping(ctx) })
	within(t, "ListPrefixed", 2*time.Second, func() error {
		_, err := c.ListPrefixed(ctx, []string{"talkable_development__"})
		return err
	})
	within(t, "ListSuffixed", 2*time.Second, func() error {
		_, err := c.ListSuffixed(ctx)
		return err
	})
	within(t, "SchemaMigrationsMax", 2*time.Second, func() error {
		_, err := c.SchemaMigrationsMax(ctx, "talkable_development__review1")
		return err
	})
	start := time.Now()
	within(t, "Drop", 2*time.Second, func() error { return c.Drop(ctx, "talkable_development__review3", reviewGuard) })
	if took := time.Since(start); took < 150*time.Millisecond {
		t.Errorf("Drop gave up after %s, before its own (longer) deadline", took)
	}
}

// A caller's own deadline wins over the call's: a shorter one is kept, and a
// longer one is not cut short.
func TestACallersDeadlineWins(t *testing.T) {
	shortDeadlines(t, 20*time.Millisecond, 20*time.Millisecond)
	srv := &fakeServer{stall: true}
	c := newFakeClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Drop(ctx, "talkable_development__review3", reviewGuard); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drop = %v", err)
	}
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Fatalf("Drop gave up after %s, before the caller's deadline", took)
	}
}
