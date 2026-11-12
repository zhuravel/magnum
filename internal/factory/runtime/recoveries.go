package runtime

// The recoveries of a lost pane, counted where a restart finds them. A role
// whose pane is lost gets MaxRecoveries recoveries in one attempt, the
// first at once, the next after their waits (RecoveryWaits: 1 and 5
// minutes): its conversation
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
// recovery of a lost pane: the first runs at once, as a lost pane's always
// did, and the next give a cause of the moment (a herdr restart, the Mac's
// memory) time to pass.
var RecoveryWaits = [MaxRecoveries]time.Duration{0, time.Minute, 5 * time.Minute}

// recoverKind is the kind of the events that count a role's recoveries.
const recoverKind = "recover"

// The words of a recover event after its lead (recoverLead): a recovery,
// the count starting over, and the recoveries running out (after which
// your retry starts the count over).
const (
	recoveryWord  = "recovery "
	startOverWord = "starts over"
	ranOutWord    = "ran out"
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
// the count last started over, counted from the task's recover events and
// your retries each time (countRecoveries), so the daemon in memory and
// after a restart count alike.
func (r *Runtime) recoveriesUsed(ctx context.Context, st *roleState) int {
	return r.countRecoveries(ctx, st.taskID, st.key.attempt, st.role.Name)
}

// countRecoveries counts the recoveries of role in attempt of task id that
// the task's recover events record since the count last started over: a
// "starts over" event of the role (your answer's fresh start, your retry of
// a role whose recoveries ran out), or your retry of the task after its
// recoveries ran out (a "ran out" event). A retry while recoveries are left
// starts nothing over. A registry that cannot be read counts none: the
// recoveries then take the steps they took before.
func (r *Runtime) countRecoveries(ctx context.Context, id int64, attempt, role string) int {
	evs, err := r.reg.TaskEvents(ctx, id, recoverKind, 0)
	if err != nil {
		r.log.Warn("read a role's recoveries", "task", id, "role", role, "err", err)
		return 0
	}
	var retries []time.Time
	if ins, err := r.reg.Inputs(ctx, id); err == nil {
		for _, in := range ins {
			if in.Kind == registry.InputRetry && in.Role == "" {
				retries = append(retries, in.At)
			}
		}
	}
	lead := recoverLead(role, attempt)
	n, ranOut, next := 0, false, 0
	for _, ev := range evs {
		rest, ok := strings.CutPrefix(ev.Text, lead)
		if !ok {
			continue
		}
		for ; next < len(retries) && retries[next].Before(ev.At); next++ {
			if ranOut { // your retry after they ran out: they start over
				n, ranOut = 0, false
			}
		}
		switch {
		case strings.HasPrefix(rest, startOverWord):
			n, ranOut = 0, false
		case strings.HasPrefix(rest, ranOutWord):
			ranOut = true
		case strings.HasPrefix(rest, recoveryWord):
			n++
		}
	}
	if ranOut && next < len(retries) {
		n = 0
	}
	return n
}

// recoveryAlertLead and recoveryAlertWords frame the alert of a recovery
// (noteRecovery) after the task's key and before and after the role.
const (
	recoveryAlertLead  = "the factory recovers "
	recoveryAlertWords = "'s lost pane by itself: "
)

// RecoveryAlert reports whether what, an alert's text after its task's key,
// is the alert of a lost pane's recovery (noteRecovery): news of a step the
// factory took by itself, which waits on nobody.
func RecoveryAlert(what string) bool {
	return strings.HasPrefix(what, recoveryAlertLead) && strings.Contains(what, recoveryAlertWords)
}

// noteRecovery records recovery n of role in attempt a of task t, the step
// it takes and why, as a recover event (the count a restart reads) and as
// an alert, so the screen shows what the factory did by itself.
func (r *Runtime) noteRecovery(ctx context.Context, t registry.Task, a registry.Attempt, role string, n int, step, why string) {
	text := fmt.Sprintf("%s%s%d of %d (%s): %s", recoverLead(role, a.ID), recoveryWord, n, MaxRecoveries, step, why)
	r.appendEvent(ctx, t.ID, recoverKind, text)
	r.appendEvent(ctx, t.ID, "alert", fmt.Sprintf("%s: %s%s%srecovery %d of %d (%s)", taskKey(t), recoveryAlertLead, role,
		recoveryAlertWords, n, MaxRecoveries, step))
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
func (r *Runtime) SendFresh(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, then string,
	recoveriesOver bool) (string, error) {
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
	st.stuck, st.pending, st.step, st.dueAt = false, false, 0, time.Time{}
	if recoveriesOver {
		st.lost, st.recovering = false, false
	}
	if l := r.loops[loopKey{t.ID, a.Stage, role.Name}]; l != nil { // a fresh session: its loop's ladder starts over too
		l.step, l.fails, l.reset = loopNone, nil, true
	}
	r.mu.Unlock()
	if recoveriesOver {
		r.noteStartOver(ctx, t, a, role.Name, "a fresh session your answer asked for")
	}
	return r.fromFiles(ctx, t, a, st, "the factory's stage asked for a fresh session", then)
}

// MadeProgress starts the loop ladder of role in attempt a of task t over,
// as a verified run's progress or your retry does: the writer's go-on at its
// fix budget (the factory's stage found its turns making progress), whose
// turns the ladder then counts from its next prompt, never as a loop.
func (r *Runtime) MadeProgress(t registry.Task, a registry.Attempt, role string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := loopKey{t.ID, a.Stage, role}
	l := r.loops[k]
	if l == nil { // after a restart: the ladder counts from the next prompt, not from the stage's first
		l = &loopState{}
		r.loops[k] = l
	}
	l.step, l.fails, l.reset = loopNone, nil, true
	delete(r.once, noProgressKey(k))
}
