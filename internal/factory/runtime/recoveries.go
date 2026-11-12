package runtime

// The recoveries of a lost pane, counted where a restart finds them. A role
// whose pane is lost gets MaxRecoveries recoveries in one attempt, each
// after its wait (RecoveryWaits: 1, 5 and 15 minutes): its conversation
// resumed while it has one, and the last one a fresh session from the
// attempt's first prompt and the task's files (its context); a role that
// took no prompt yet is relaunched each time, its stage sending its first
// prompt next. Then the recoveries ran out (EventLost), and the stage asks
// you a question (start the role again, give it a direction, or drop the
// task), never "retry". Each recovery is a "recover" event of the task, so
// a restart counts the ones taken (recoveriesUsed): it neither takes one
// twice nor loses one. Your retry starts the count over, and so does a
// fresh start your answer asks for (SendFresh, a "starts over" event);
// neither spends a recovery.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/scrub"
)

// MaxRecoveries is how many recoveries a lost pane gets in one attempt
// before the stage asks you; RecoveryWaits is the wait before each of them,
// counted from the loss (or from the pass that found the recovery before it
// made no progress).
const MaxRecoveries = 3

// RecoveryWaits are the waits before the first, the second and the third
// recovery of a lost pane.
var RecoveryWaits = [MaxRecoveries]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

// recoverKind is the kind of the events that count a role's recoveries.
const recoverKind = "recover"

// The words of a recover event after its lead (recoverLead): a recovery,
// or the count starting over.
const (
	recoveryWord  = "recovery "
	startOverWord = "starts over"
)

// recoverLead starts the recover events of role in attempt.
func recoverLead(role, attempt string) string { return role + " in " + attempt + ": " }

// recoveryStep is the step recovery n (from 1) of a role takes: a resume of
// its conversation (resume, its id; one you typed in, for a role that took
// no prompt yet) before the last recovery, else a relaunch for a role that
// took no prompt yet (its stage sends its first prompt next), else a fresh
// session from the attempt's first prompt and the task's files.
func recoveryStep(n int, resume string, unprompted bool) string {
	switch {
	case n < MaxRecoveries && resume != "":
		return "resume"
	case unprompted:
		return "relaunch"
	}
	return "fresh"
}

// recoveriesUsed is how many recoveries role st took in its attempt since
// the count last started over: read once from the task's recover events
// (and your retries), then kept in st.
func (r *Runtime) recoveriesUsed(ctx context.Context, st *roleState) int {
	r.mu.Lock()
	counted, n := st.counted, st.recoveries
	r.mu.Unlock()
	if counted {
		return n
	}
	n = r.countRecoveries(ctx, st.taskID, st.key.attempt, st.role.Name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !st.counted {
		st.recoveries, st.counted = n, true
	}
	return st.recoveries
}

// countRecoveries counts the recoveries of role in attempt of task id that
// the task's recover events record since the count last started over: the
// newest "starts over" event of the role, or your newest retry of the task.
// A registry that cannot be read counts none: the recoveries then take the
// steps they took before.
func (r *Runtime) countRecoveries(ctx context.Context, id int64, attempt, role string) int {
	evs, err := r.reg.TaskEvents(ctx, id, recoverKind, 0)
	if err != nil {
		r.log.Warn("read a role's recoveries", "task", id, "role", role, "err", err)
		return 0
	}
	var since time.Time
	if ins, err := r.reg.Inputs(ctx, id); err == nil {
		for _, in := range ins {
			if in.Kind == registry.InputRetry && in.Role == "" && in.At.After(since) {
				since = in.At
			}
		}
	}
	lead := recoverLead(role, attempt)
	n := 0
	for _, ev := range evs {
		rest, ok := strings.CutPrefix(ev.Text, lead)
		if !ok || ev.At.Before(since) {
			continue
		}
		switch {
		case strings.HasPrefix(rest, startOverWord):
			n = 0
		case strings.HasPrefix(rest, recoveryWord):
			n++
		}
	}
	return n
}

// noteRecovery records recovery n of role in attempt a of task t, the step
// it takes and why, as a recover event (the count a restart reads) and as
// an alert, so the screen shows what the factory did by itself.
func (r *Runtime) noteRecovery(ctx context.Context, t registry.Task, a registry.Attempt, role string, n int, step, why string) {
	text := fmt.Sprintf("%s%s%d of %d (%s): %s", recoverLead(role, a.ID), recoveryWord, n, MaxRecoveries, step, why)
	r.appendEvent(ctx, t.ID, recoverKind, text)
	r.appendEvent(ctx, t.ID, "alert", fmt.Sprintf("%s: the factory recovers %s's lost pane by itself: recovery %d of %d (%s)",
		taskKey(t), role, n, MaxRecoveries, step))
}

// noteStartOver records that the recoveries of role in attempt a of task t
// start over (why), so a restart counts from here.
func (r *Runtime) noteStartOver(ctx context.Context, t registry.Task, a registry.Attempt, role, why string) {
	r.appendEvent(ctx, t.ID, recoverKind, recoverLead(role, a.ID)+startOverWord+": "+why)
}

// appendEvent writes an event of kind for task id, scrubbed; a failure is
// logged.
func (r *Runtime) appendEvent(ctx context.Context, id int64, kind, text string) {
	if err := r.reg.AppendEvent(context.WithoutCancel(ctx), id, kind, registry.PhaseInfo, scrub.Text(text)); err != nil {
		r.log.Warn("factory event", "task", id, "kind", kind, "err", err)
	}
}

// SendFresh starts role (as it runs) of attempt a of task t afresh, on your
// answer to a question its stage asked (the role's pane lost after its
// recoveries, the writer's fix turns spent) or for the writer's one fresh
// session at its fix budget: its session quit, its runs handed over, and a
// new session that starts from the attempt's first prompt with then after
// it (fromFiles). Its ladders start over (the watchdog's, the recoveries',
// counted from here: a "starts over" event, and its loop's in the stage,
// counted from its next prompt), so it spends no recovery. The
// new run's id comes back; a role that may not restart now (a pause, you in
// its pane) is that error.
func (r *Runtime) SendFresh(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, then string) (string, error) {
	if a.TaskID != t.ID {
		return "", fmt.Errorf("start %s afresh: attempt %s is not %s's", role.Name, a.ID, taskKey(t))
	}
	role, err := r.RunsAs(t, role)
	if err != nil {
		return "", err
	}
	st, err := r.stateFor(ctx, a.ID, role.Name)
	if err != nil {
		return "", fmt.Errorf("start %s afresh: %w", role.Name, err)
	}
	if err := r.mayRestart(ctx, t, st); err != nil {
		return "", err
	}
	r.mu.Lock()
	st.stuck, st.lost, st.pending, st.recovering = false, false, false, false
	st.step, st.recoveries, st.counted, st.dueAt = 0, 0, true, time.Time{}
	if l := r.loops[loopKey{t.ID, a.Stage, role.Name}]; l != nil { // a fresh session: its loop's ladder starts over too
		l.step, l.fails, l.reset = loopNone, nil, true
	}
	r.mu.Unlock()
	r.noteStartOver(ctx, t, a, role.Name, "a fresh session the factory's stage asked for")
	return r.fromFiles(ctx, t, a, st, "the factory's stage asked for a fresh session", then)
}
