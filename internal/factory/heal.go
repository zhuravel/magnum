package factory

// Self-healing (spec section 10.1): a panic stops only the tick of its task
// and the second sends the task to you; a sleep of the Mac moves every turn
// deadline by the gap and its first tick counts no failure; a failure is
// classified as the machine's (an infrastructure hold, holds.go) or the
// task's own.

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/scrub"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// maxPanics is how many panics of one task send it to you.
	maxPanics = 2
	// sleepTicks: a wait between passes longer than this many poll
	// intervals by the wall clock was a sleep of the Mac (wake), as the
	// review daemon's sleptBefore (internal/engine/poll_health.go) counts.
	sleepTicks = 3
	// sameFailureWindow: the same failure on two tasks within it is the
	// machine's, not theirs.
	sameFailureWindow = 10 * time.Minute
	// panicRunes and signatureRunes bound what a panic event and a failure
	// signature keep.
	panicRunes     = 300
	signatureRunes = 200
)

// errPanicked marks the error safely returns for a recovered panic: the
// panic event is its record, and it is no failure of the task's step.
var errPanicked = errors.New("panicked")

// safely runs step for task t and recovers a panic in it: a `panic` fail
// event with the panic's message (the stack goes to the log only), and the
// loop goes on with the other tasks. A task's second panic moves it to
// NEEDS YOU with "you: the factory failed twice on this task". The pattern
// is the review engine's safely (internal/engine/util.go), per task.
func (e *Engine) safely(ctx context.Context, t registry.Task, step func(ctx context.Context) error) (err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		err = e.panicked(context.WithoutCancel(ctx), t, r, debug.Stack())
	}()
	return step(ctx)
}

// contained runs pass, a part of the observe pass that runs outside any
// task's step (the runtime's Observe and Watchdog), and turns a panic in it
// into its error, the stack in the log: the loop goes on, and the next tick
// weighs the failed read.
func (e *Engine) contained(what string, pass func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			msg := textx.Clip(strings.Join(strings.Fields(execx.Redact(fmt.Sprint(r))), " "), panicRunes)
			e.log.Error("panic recovered in the observe pass", "pass", what, "panic", msg, "stack", execx.Redact(string(debug.Stack())))
			err = fmt.Errorf("%s: %w: %s", what, errPanicked, msg)
		}
	}()
	return pass()
}

// panicked records the panic r of task t's step and settles the task; a
// panic while settling is logged, never re-raised.
func (e *Engine) panicked(ctx context.Context, t registry.Task, r any, stack []byte) error {
	msg := textx.Clip(strings.Join(strings.Fields(scrub.Text(fmt.Sprint(r))), " "), panicRunes)
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("panic while settling a panic", "task", t.ID, "panic", fmt.Sprint(r),
				"stack", execx.Redact(string(debug.Stack())))
		}
	}()
	e.log.Error("panic recovered; the other tasks go on", "task", t.ID, "panic", msg, "stack", execx.Redact(string(stack)))
	e.event(ctx, t.ID, "panic", registry.PhaseFail, msg)
	e.panics[t.ID]++
	if e.panics[t.ID] >= maxPanics {
		delete(e.panics, t.ID)
		e.toOperator(ctx, t, "the factory failed twice on this task", "panic")
	}
	return fmt.Errorf("task %d: %w: %s", t.ID, errPanicked, msg)
}

// wake measures the wall-clock gap since the last tick or observe pass
// ended: one over sleepTicks poll intervals was a sleep of the Mac (Go's
// monotonic clock stops while macOS sleeps, so the wall clock is read). It
// moves every turn deadline by the gap (Runtime.Shift), so no turn times
// out for the hours the Mac slept, and the failures of the first tick after
// it count none: the network comes up after a wake. It returns the gap, 0
// for no sleep.
func (e *Engine) wake(now time.Time) time.Duration {
	now = now.Round(0)
	if e.lastAlive.IsZero() {
		return 0
	}
	gap := now.Sub(e.lastAlive)
	if gap <= sleepTicks*e.poll() {
		return 0
	}
	e.lastAlive = now
	e.uncounted = true
	if e.d.Runtime != nil {
		e.d.Runtime.Shift(gap)
	}
	e.log.Info("the Mac slept; every turn deadline moves by the gap", "gap", gap.Round(time.Second))
	return gap
}

// taskFailure is a task's step that failed, with the task.
type taskFailure struct {
	task registry.Task
	err  error
}

func (f taskFailure) Error() string { return fmt.Sprintf("%s: %v", TaskKey(f.task), f.err) }
func (f taskFailure) Unwrap() error { return f.err }

// mysqlFailure is a failed ping of the copies' database server.
type mysqlFailure struct{ err error }

func (f mysqlFailure) Error() string { return "MySQL ping: " + f.err.Error() }
func (f mysqlFailure) Unwrap() error { return f.err }

// infraPatterns are the SSH, DNS and TLS failures that engine.InfraCause
// does not name (it knows a refused SSH key and the network's: DNS,
// timeouts, refusals, TLS), lower case.
var infraPatterns = []struct{ match, cause string }{
	{"ssh: connect to host", "SSH connection failed"},
	{"kex_exchange_identification", "SSH connection failed"},
	{"ssh_exchange_identification", "SSH connection failed"},
	{"host key verification failed", "SSH host key refused"},
	{"no such host", "DNS lookup failed"},
	{"server misbehaving", "DNS lookup failed"},
	{"x509: certificate", "TLS failure"},
	{"tls: ", "TLS failure"},
}

// infraCause names the infrastructure failure err reports, "" for none: a
// failed MySQL ping, engine.InfraCause, then infraPatterns. herdr's own
// failures (herdr.ErrUnavailable, herdr.ErrTimeout: "connection refused" on
// its socket) are none: the herdr hold is theirs (checkHerdr), and an
// infrastructure hold beside it would hold every task two minutes past
// herdr's return and raise a second blocker for one cause.
func infraCause(err error) string {
	if err == nil || herdrDown(err) {
		return ""
	}
	if c, ok := errors.AsType[envCrash](err); ok {
		return c.holdCause()
	}
	if _, ok := errors.AsType[mysqlFailure](err); ok {
		return "MySQL does not answer"
	}
	if cause := engine.InfraCause(err); cause != "" {
		return cause
	}
	msg := strings.ToLower(err.Error())
	for _, p := range infraPatterns {
		if strings.Contains(msg, p.match) {
			return p.cause
		}
	}
	return ""
}

// herdrDown reports a herdr failure (herdr.ErrUnavailable, herdr.ErrTimeout).
// It is herdr's hold that covers one, never a failure of the task: a stage
// that meets herdr down inside its pass is a held pass (Tick), which counts
// toward no stage failure and no infrastructure hold.
func herdrDown(err error) bool {
	return errors.Is(err, herdr.ErrUnavailable) || errors.Is(err, herdr.ErrTimeout)
}

// recentFailure is the last task a failure signature was seen on, and when.
type recentFailure struct {
	taskID int64
	at     time.Time
}

// failureSignature is a task's failure with the task's own words (its
// branch, copy, key and number) taken out, so the same failure of two tasks
// compares equal: its redacted first line, bounded.
func failureSignature(t registry.Task, err error) string {
	s := textx.FirstLine(execx.Redact(err.Error()))
	for _, w := range []string{t.Branch, t.Copy, t.JiraKey, TaskKey(t)} {
		if w != "" {
			s = strings.ReplaceAll(s, w, "<task>")
		}
	}
	return textx.Clip(s, signatureRunes)
}

// sameOnTwoTasks reports whether f is a failure that another task had
// within sameFailureWindow, and remembers f.
func (e *Engine) sameOnTwoTasks(f taskFailure, now time.Time) (string, bool) {
	sig := failureSignature(f.task, f.err)
	prev, seen := e.recent[sig]
	e.recent[sig] = recentFailure{taskID: f.task.ID, at: now}
	for s, r := range e.recent { // forget what fell out of the window
		if now.Sub(r.at) > sameFailureWindow {
			delete(e.recent, s)
		}
	}
	return sig, seen && prev.taskID != f.task.ID && now.Sub(prev.at) <= sameFailureWindow
}
