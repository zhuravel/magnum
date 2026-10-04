package steps

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "magnum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func phases(t *testing.T, st *store.Store, subject string) []string {
	t.Helper()
	evs, err := st.EventsBySubject(context.Background(), subject, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		if e.Kind == store.KindStep {
			out = append(out, store.Deref(e.Step)+":"+store.Deref(e.Phase))
		}
	}
	return out
}

// failRow returns the level and message of the single fail row of subject.
func failRow(t *testing.T, st *store.Store, subject string) (string, string) {
	t.Helper()
	evs, err := st.EventsBySubject(context.Background(), subject, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []store.Event
	for _, e := range evs {
		if e.Kind == store.KindStep && store.Deref(e.Phase) == store.PhaseFail {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("fail rows for %s = %d, want 1", subject, len(found))
	}
	return found[0].Level, found[0].Message
}

func TestStepRecordsAndSkipsOnRerun(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	calls := 0
	fn := func(context.Context) error { calls++; return nil }
	if err := Step(ctx, st, "slot:review1", "checkout.fetch", fn); err != nil {
		t.Fatal(err)
	}
	if err := Step(ctx, st, "slot:review1", "checkout.fetch", fn); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fn ran %d times", calls)
	}
	got := phases(t, st, "slot:review1")
	if len(got) != 2 || got[0] != "checkout.fetch:begin" || got[1] != "checkout.fetch:ok" {
		t.Fatalf("events = %v", got)
	}
	// Same step name on another subject is independent.
	if err := Step(ctx, st, "slot:review2", "checkout.fetch", fn); err != nil || calls != 2 {
		t.Fatalf("other subject: calls=%d err=%v", calls, err)
	}
}

func TestStepFailureIsRecordedAndRetried(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	boom := errors.New("git exited 128: token ghp_abcdefghijklmnopqrstuvwxyz0123456789 rejected")
	err := Step(ctx, st, "pr:1", "fetch", func(context.Context) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	evs, _ := st.EventsBySubject(ctx, "pr:1", 0)
	last := evs[len(evs)-1]
	if store.Deref(last.Phase) != "fail" || last.Level != "error" {
		t.Fatalf("fail row: %+v", last)
	}
	if want := "<redacted>"; !strings.Contains(last.Message, want) || strings.Contains(last.Message, "ghp_") {
		t.Fatalf("secret not redacted: %q", last.Message)
	}
	ran := false
	if err := Step(ctx, st, "pr:1", "fetch", func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("retry: ran=%v err=%v", ran, err)
	}
}

// TestCrashResumeRunsEachSideEffectOnce simulates a crash before every step of a
// three-step sequence and checks that re-running the sequence after each crash
// completes every side effect exactly once.
func TestCrashResumeRunsEachSideEffectOnce(t *testing.T) {
	names := []string{"fetch", "switch", "render"}
	for crashAt := range names {
		t.Run(names[crashAt], func(t *testing.T) {
			st := newStore(t)
			counts := map[string]int{}
			sequence := func(ctx context.Context) error {
				for _, n := range names {
					if err := Step(ctx, st, "slot:review1", n, func(context.Context) error { counts[n]++; return nil }); err != nil {
						return err
					}
				}
				return nil
			}
			crash := errors.New("crash")
			ctx := WithFailpoint(context.Background(), func(subject, name string, at Point) error {
				if name == names[crashAt] && at == BeforeRun {
					return crash
				}
				return nil
			})
			if err := sequence(ctx); !errors.Is(err, crash) {
				t.Fatalf("first run err = %v", err)
			}
			if err := sequence(context.Background()); err != nil {
				t.Fatalf("resume: %v", err)
			}
			for _, n := range names {
				if counts[n] != 1 {
					t.Fatalf("%s ran %d times (counts %v)", n, counts[n], counts)
				}
			}
		})
	}
}

// A crash after fn succeeded but before its ok row is written re-runs fn on
// resume: steps are at-least-once, so side effects must be idempotent.
func TestCrashAfterRunBeforeOKRerunsStep(t *testing.T) {
	st := newStore(t)
	calls := 0
	fn := func(context.Context) error { calls++; return nil }
	crash := errors.New("crash")
	ctx := WithFailpoint(context.Background(), func(_, _ string, at Point) error {
		if at == AfterRun {
			return crash
		}
		return nil
	})
	if err := Step(ctx, st, "pr:1", "post", fn); !errors.Is(err, crash) {
		t.Fatalf("err = %v", err)
	}
	if err := Step(context.Background(), st, "pr:1", "post", fn); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (at-least-once)", calls)
	}
}

func TestResetSubjectStartsNewGeneration(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	calls := 0
	fn := func(context.Context) error { calls++; return nil }
	Step(ctx, st, "pr:1", "checkout", fn)
	Step(ctx, st, "pr:2", "checkout", fn)
	if err := ResetSubject(ctx, st, "pr:1"); err != nil {
		t.Fatal(err)
	}
	Step(ctx, st, "pr:1", "checkout", fn)
	Step(ctx, st, "pr:2", "checkout", fn)
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (pr:1 twice, pr:2 once)", calls)
	}
	done, err := Done(ctx, st, "pr:1", "checkout")
	if err != nil || !done {
		t.Fatalf("Done = %v %v", done, err)
	}
}

func TestStepHonorsCancelledContextForBookkeeping(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	err := Step(ctx, st, "pr:1", "slow", func(context.Context) error { cancel(); return context.Canceled })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	got := phases(t, st, "pr:1")
	if len(got) != 2 || got[1] != "slow:fail" {
		t.Fatalf("fail row not written after cancel: %v", got)
	}
	level, msg := failRow(t, st, "pr:1")
	if level != "warn" || !strings.Contains(msg, "interrupted") || strings.Contains(msg, "failed") {
		t.Fatalf("cancelled fail row = level %q msg %q, want warn and interrupted", level, msg)
	}
	// A fail row never counts as done, so the step runs again on resume.
	if done, err := Done(context.Background(), st, "pr:1", "slow"); err != nil || done {
		t.Fatalf("cancelled step Done = %v %v, want false", done, err)
	}
}

func TestStepRealFailureStaysErrorLevel(t *testing.T) {
	st := newStore(t)
	boom := errors.New("boom")
	err := Step(context.Background(), st, "pr:2", "flaky", func(context.Context) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	level, msg := failRow(t, st, "pr:2")
	if level != "error" || !strings.Contains(msg, "flaky failed: boom") {
		t.Fatalf("failure row = level %q msg %q, want error and failed", level, msg)
	}
	// Wrapped cancellation still counts as an interruption.
	wrapped := fmt.Errorf("fetch: %w", context.Canceled)
	_ = Step(context.Background(), st, "pr:3", "wrapped", func(context.Context) error { return wrapped })
	if level, msg := failRow(t, st, "pr:3"); level != "warn" || !strings.Contains(msg, "interrupted") {
		t.Fatalf("wrapped cancel row = level %q msg %q", level, msg)
	}
	// A deadline is a real failure, not a shutdown.
	_ = Step(context.Background(), st, "pr:4", "slowdeadline", func(context.Context) error { return context.DeadlineExceeded })
	if level, _ := failRow(t, st, "pr:4"); level != "error" {
		t.Fatalf("deadline row level = %q, want error", level)
	}
}
