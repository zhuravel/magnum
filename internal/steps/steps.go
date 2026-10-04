// Package steps makes multi-step side effects resumable after a crash.
//
// Step writes an events row (kind "step", phase begin) before running a side
// effect and an ok or fail row after it. When the daemon restarts and replays
// the same sequence, steps that already have an ok row in the subject's
// current generation are skipped, so the sequence picks up where it stopped.
// ResetSubject starts a new generation (for example, a new review round on a
// new head) so every step runs again; old rows stay as audit history.
//
// Guarantee: a step whose ok row was written never runs again in that
// generation. A crash after fn returned but before the ok row was written
// re-runs fn, so every fn must be idempotent or prechecked (at-least-once).
//
// Subjects are free-form strings such as "pr:123" or "slot:review3"; they are
// also the events.subject that `magnum logs` filters on.
package steps

import (
	"context"
	"errors"
	"fmt"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// Point identifies where in a step a failpoint fires.
type Point string

const (
	// BeforeRun fires after the begin row is written, before fn runs.
	BeforeRun Point = "before"
	// AfterRun fires after fn succeeded, before the ok row is written.
	AfterRun Point = "after"
)

// Failpoint lets tests simulate a crash: a non-nil error aborts Step at that
// point with the error and without writing an ok or fail row.
type Failpoint func(subject, name string, at Point) error

type failpointKey struct{}

// WithFailpoint returns a context whose Step calls consult fp.
func WithFailpoint(ctx context.Context, fp Failpoint) context.Context {
	return context.WithValue(ctx, failpointKey{}, fp)
}

func failpoint(ctx context.Context, subject, name string, at Point) error {
	if fp, ok := ctx.Value(failpointKey{}).(Failpoint); ok && fp != nil {
		return fp(subject, name, at)
	}
	return nil
}

// Step runs fn once per subject generation. If (subject, name) already has
// an ok row in the current generation, fn is skipped and Step returns nil.
// Otherwise it writes a begin row, runs fn and writes an ok row, or a fail
// row (message redacted with execx.Redact) and returns fn's error. Rows are
// written even when ctx is cancelled during fn. A fail row caused by context
// cancellation (daemon shutdown) is logged at level "warn" as "interrupted"
// instead of level "error"; it is still a fail row, so the step is not done and
// runs again on resume.
func Step(ctx context.Context, st *store.Store, subject, name string, fn func(ctx context.Context) error) error {
	done, err := st.StepDone(ctx, subject, name)
	if err != nil {
		return fmt.Errorf("step %s %s: %w", subject, name, err)
	}
	if done {
		return nil
	}
	if err := record(ctx, st, subject, name, store.PhaseBegin, "info", "begin "+name); err != nil {
		return err
	}
	if err := failpoint(ctx, subject, name, BeforeRun); err != nil {
		return err
	}
	runErr := fn(ctx)
	bookkeeping := context.WithoutCancel(ctx)
	if runErr != nil {
		level, verb := "error", "failed"
		if errors.Is(runErr, context.Canceled) {
			level, verb = "warn", "interrupted"
		}
		msg := execx.Redact(fmt.Sprintf("%s %s: %v", name, verb, runErr))
		if err := record(bookkeeping, st, subject, name, store.PhaseFail, level, msg); err != nil {
			return fmt.Errorf("%w (and %v)", runErr, err)
		}
		return runErr
	}
	if err := failpoint(ctx, subject, name, AfterRun); err != nil {
		return err
	}
	return record(bookkeeping, st, subject, name, store.PhaseOK, "info", "ok "+name)
}

// Done reports whether (subject, name) completed in the current generation.
func Done(ctx context.Context, st *store.Store, subject, name string) (bool, error) {
	return st.StepDone(ctx, subject, name)
}

// ResetSubject starts a new step generation for subject: earlier ok rows are
// kept for history but no longer cause steps to be skipped.
func ResetSubject(ctx context.Context, st *store.Store, subject string) error {
	_, err := st.AppendEvent(ctx, store.Event{
		Level: "info", Subject: &subject, Kind: store.KindStepReset, Message: "new step generation",
	})
	if err != nil {
		return fmt.Errorf("reset steps %s: %w", subject, err)
	}
	return nil
}

func record(ctx context.Context, st *store.Store, subject, name, phase, level, msg string) error {
	_, err := st.AppendEvent(ctx, store.Event{
		Level: level, Subject: &subject, Kind: store.KindStep, Step: &name, Phase: &phase, Message: msg,
	})
	if err != nil {
		return fmt.Errorf("step %s %s %s: %w", subject, name, phase, err)
	}
	return nil
}
