package runtime

// The watchdog and the recoveries (spec section 10.1). A turn past its
// role's timeout whose transcript did not grow, whose screen did not change
// and whose teammates' transcripts did not grow for NoProgressAfter climbs
// three steps, each NoProgressAfter of stillness after the last: the shell
// stopped and a nudge, the session quit hard and resumed, a fresh session
// from the attempt's first prompt; then EventStuck with the pane's last
// line. A lost pane gets MaxRecoveries recoveries an attempt, each after
// its wait (RecoveryWaits: 1, 5 and 15 minutes): resumed from its
// conversation before the last, which starts fresh from the files; a role
// whose pane is lost before its first prompt (its CLI exited at its start)
// is relaunched, its stage sending that prompt next; a recovery that shows
// no progress within NoProgressAfter takes the next step, and EventLost
// ends them (recoveries.go counts them across restarts). A loop without progress (Send) is compacted, then started fresh
// once, then left to the operator. A role a heal of the factory's herdr
// server holds (volume.go) takes none of these steps.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
)

// The texts the runtime sends on its own.
const (
	nudgeText = "You seem stuck: nothing on your screen or in your transcript changed for 10 minutes and your turn is past " +
		"its time. If a command hangs, stop it and go on another way; if you are done, write your output files and stop."
	resumeText = "magnum restarted this session (its pane was lost or its turn hung). Continue the task where you stopped; " +
		"its inputs are the task's files."

	// backgroundNudgeLead opens the nudge of a turn that only background
	// work holds open (backgroundNudge), where nudgeText says the turn seems
	// stuck: the agent ended its turn and sits idle at its prompt, and the
	// agents manager counts work it started in the background as running.
	// Telling the agent lets that work go (agents.Manager.Tell), so the
	// text says that ending the turn ends magnum's wait for it.
	backgroundNudgeLead = "Your turn is past its time and nothing on your screen or in your transcript changed for 10 minutes, " +
		"but magnum still counts "
	backgroundNudgeOne = "1 background task you started (a command, an agent, a teammate or a monitor) as running. " +
		"If you still need it, wait for it and make sure that it makes progress before you end this turn: " +
		"once this turn ends, magnum no longer waits for that work. If you do not need it, stop it with TaskStop. "
	backgroundNudgeMany = "%d background tasks you started (commands, agents, teammates or monitors) as running. " +
		"If you still need one, wait for it and make sure that it makes progress before you end this turn: " +
		"once this turn ends, magnum no longer waits for that work. Stop the ones you do not need with TaskStop. "
	// backgroundNudgeEnd keeps the hung command's clause: a count above 0
	// does not prove that the agent is idle.
	backgroundNudgeEnd = "If a command you run hangs, stop it and go on another way. " +
		"If you are done, write your output files and end your turn."
)

// backgroundNudge is the nudge of an idle agent whose turn n background
// tasks (n > 0) hold open: it names them, where nudgeText would tell an agent
// that is done that it seems stuck.
func backgroundNudge(n int) string {
	if n == 1 {
		return backgroundNudgeLead + backgroundNudgeOne + backgroundNudgeEnd
	}
	return backgroundNudgeLead + fmt.Sprintf(backgroundNudgeMany, n) + backgroundNudgeEnd
}

// errRecoveriesSpent is a recovery asked for once the recoveries of a
// role's attempt ran out.
type errRecoveriesSpent struct{ n int }

func (e errRecoveriesSpent) Error() string {
	return fmt.Sprintf("no progress after %d recoveries", e.n)
}

// Watchdog checks the runs in flight at now (the engine's observe pass):
// the hung-turn steps, and the recoveries that show no progress. It
// returns EventStuck and EventLost when their steps ran out. A panic is
// contained: it comes back as the error.
func (r *Runtime) Watchdog(ctx context.Context, now time.Time) (evs []Event, err error) {
	defer contain("watchdog", r.log, &err)
	runs, err := r.st.ActiveRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("watchdog: %w", err)
	}
	seen := map[roleKey]bool{}
	for _, run := range runs {
		if run.State != store.RunSubmitted && run.State != store.RunWorking {
			continue
		}
		k := roleKey{run.TargetSHA, run.Role}
		if seen[k] {
			continue
		}
		st, err := r.stateFor(ctx, k.attempt, k.role)
		if err != nil {
			continue // no factory attempt's
		}
		seen[k] = true
		if ev, ok := r.watch(ctx, st, run, now); ok {
			evs = append(evs, ev)
		}
	}
	r.mu.Lock()
	var idle []*roleState
	for k, st := range r.roles {
		if !seen[k] && (st.recovering || st.pending) && !st.lost && !st.stuck {
			idle = append(idle, st)
		}
	}
	r.mu.Unlock()
	for _, st := range idle {
		if ev, ok := r.watch(ctx, st, store.Run{}, now); ok {
			evs = append(evs, ev)
		}
	}
	return evs, nil
}

// watch takes the step a role's run (none: a recovery waiting for one, or
// a step that quit the role's session and failed before a new run took its
// place, pending) is due at now. A pending role that was on no ladder yet
// (a fresh start of a Codex session that took no prompt, a loop's fresh
// session) takes the recoveries' steps. Under a drain for a reboot
// (mayPrompt) a role with no turn in flight takes no step: each would start
// an agent or send a prompt, so it stays due until the drain is lifted.
func (r *Runtime) watch(ctx context.Context, st *roleState, run store.Run, now time.Time) (Event, bool) {
	if r.healing(ctx, st.taskID, st.key.attempt, st.role.Name) != nil {
		return Event{}, false // the heal relaunches it, or told you
	}
	r.mu.Lock()
	climbing, recovering, done := st.step > 0, st.recovering, st.stuck || st.lost
	shift, due := r.shifts[run.ID], st.dueAt
	r.mu.Unlock()
	if done {
		return Event{}, false
	}
	if !due.IsZero() { // a recovery that waits: it runs once its wait is over, whatever the screen shows
		if now.Before(due) {
			return Event{}, false
		}
		return r.recoverDue(ctx, st)
	}
	deadline := time.Time{}
	if run.ID != "" {
		start := run.CreatedAt
		if run.SubmittedAt != nil {
			start = *run.SubmittedAt
		}
		deadline = start.Add(st.role.Timeout.Duration + shift)
		if !climbing && !recovering && now.Before(deadline.Add(-NoProgressAfter)) {
			return Event{}, false // too early to watch
		}
	}
	sig := r.signature(ctx, st, run)
	r.mu.Lock()
	if sig != st.sig || st.since.IsZero() {
		st.sig, st.since = sig, now
		r.mu.Unlock()
		return Event{}, false
	}
	quiet := now.Sub(st.since)
	r.mu.Unlock()
	if quiet < NoProgressAfter || (!climbing && !recovering && now.Before(deadline)) {
		return Event{}, false
	}
	t, err := r.reg.TaskByID(ctx, st.taskID)
	if err != nil {
		return Event{}, false
	}
	a, err := r.reg.AttemptByID(ctx, st.key.attempt)
	if err != nil {
		return Event{}, false
	}
	if (t.State != registry.TaskActive && t.State != registry.TaskWaiting) ||
		(a.State != registry.AttemptLaunching && a.State != registry.AttemptRunning) {
		return Event{}, false // the task stopped, or its stage moved on: nothing is restarted for it
	}
	if err := r.mayRestart(ctx, t, st); err != nil {
		if errors.Is(err, errRefusedRole) {
			r.calm(st.key.attempt, st.key.role, t.ID, st.role.AgentKind(), true)
		}
		return Event{}, false // a pause, a refusal or you in the pane: no step now
	}
	if recovering || (run.ID == "" && !climbing) {
		err := r.recoverRole(ctx, t, a, st.role, "no progress for "+NoProgressAfter.String(), run.ID != "")
		if spent, ok := errors.AsType[errRecoveriesSpent](err); ok {
			return r.lostEvent(t, a, st.role, spent), true
		}
		if _, wait := errors.AsType[*WaitError](err); wait {
			r.waitToRecover(a.ID, st.role.Name) // a drain for a reboot: the role stays due
		} else if err != nil {
			r.log.Warn("the next recovery of a role", "attempt", a.ID, "role", st.role.Name, "err", err)
		}
		r.baseline(ctx, st, store.Run{})
		return Event{}, false
	}
	if run.ID == "" { // drain.go: no turn in flight, so the climb's next step starts one (a fresh agent, a prompt)
		if err := r.mayPrompt(); err != nil {
			r.baseline(ctx, st, store.Run{}) // the role stays pending, and climbs once the drain is lifted
			return Event{}, false
		}
	}
	return r.climb(ctx, t, a, st, run)
}

// recoverDue takes the recovery of role st whose wait is over (dueAt), as
// watch takes a recovery that shows no progress: none for a task that
// stopped or a stage that moved on, none while it may not restart (a
// pause, you in its pane), and EventLost once the recoveries ran out.
func (r *Runtime) recoverDue(ctx context.Context, st *roleState) (Event, bool) {
	t, err := r.reg.TaskByID(ctx, st.taskID)
	if err != nil {
		return Event{}, false
	}
	a, err := r.reg.AttemptByID(ctx, st.key.attempt)
	if err != nil {
		return Event{}, false
	}
	if (t.State != registry.TaskActive && t.State != registry.TaskWaiting) ||
		(a.State != registry.AttemptLaunching && a.State != registry.AttemptRunning) {
		r.mu.Lock()
		st.dueAt, st.recovering = time.Time{}, false
		r.mu.Unlock()
		return Event{}, false
	}
	err = r.recoverRole(ctx, t, a, st.role, "its pane was lost", false)
	if spent, ok := errors.AsType[errRecoveriesSpent](err); ok {
		return r.lostEvent(t, a, st.role, spent), true
	}
	if err != nil && !errors.Is(err, errRefusedRole) {
		r.log.Warn("a role's recovery after its wait", "attempt", a.ID, "role", st.role.Name, "err", err)
	}
	return Event{}, false
}

// signature is what tells the watchdog the role moved: its transcript's
// size, its screen and the progress of the teammates it waits on (their
// transcripts' sizes, agents.Manager.BackgroundProgress: a lead idle on
// teammates that work is not stuck).
func (r *Runtime) signature(ctx context.Context, st *roleState, run store.Run) string {
	s, ok := r.liveSession(ctx, st)
	if !ok && run.SessionID != nil {
		if x, err := r.st.SessionByID(ctx, *run.SessionID); err == nil {
			s, ok = x, true
		}
	}
	if !ok {
		return "no session"
	}
	size := int64(-1)
	if path, err := r.mgr.TranscriptPath(ctx, s); err == nil {
		if fi, err := os.Stat(path); err == nil {
			size = fi.Size()
		}
	}
	screen, _ := r.mgr.ReadRecent(ctx, s, agents.HealthLines)
	return strconv.FormatInt(size, 10) + "\x00" + shortHash(stillScreen(screen)) + "\x00" + r.mgr.BackgroundProgress(ctx, run)
}

// statusLine is a line a CLI redraws while its turn runs, a timer in it:
// Claude Code's spinner ("✻ Cogitating… (3m 12s · ↓ 2.1k tokens · esc to
// interrupt)"), Codex's ("• Working (12m 03s • esc to interrupt)", and its
// wait on a command it runs in the background, "Waiting for background
// terminal (3m 2s)", whose wording Task 21 checks against a live Codex). A
// tool line that only carries its duration ("• Ran bin/rspec (12s)") stays:
// a new one is progress.
var statusLine = regexp.MustCompile(`(?i)esc to interrupt|\bworking\s*\(\s*\d|\bwaiting for background terminal\b|^\s*\S\s+\S[^(]*…\s*\(\s*\d+\s*[hms]\b`)

// stillScreen is a screen as the watchdog compares it: without the status
// lines whose timer ticks while a turn runs, so a turn that only waits still
// reads as still.
func stillScreen(text string) string {
	var keep []string
	for line := range strings.SplitSeq(strings.ReplaceAll(text, "\r", ""), "\n") {
		if !statusLine.MatchString(line) {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

// baseline takes the role's reading now: the stillness a next step waits
// for counts from here. run is the run the next pass will read the role
// through (the nudge's, which stays in flight; none when a step starts a new
// session), so the reading carries the same teammates' part as that pass's.
func (r *Runtime) baseline(ctx context.Context, st *roleState, run store.Run) {
	sig := r.signature(ctx, st, run)
	r.mu.Lock()
	st.sig, st.since = sig, r.now()
	r.mu.Unlock()
}

// climb takes the next step of a hung turn: StopShell and a nudge, the
// session quit hard and resumed, a fresh session from the files, then
// EventStuck with the pane's last line.
func (r *Runtime) climb(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, run store.Run) (Event, bool) {
	r.mu.Lock()
	step := st.step
	st.step++
	r.mu.Unlock()
	r.log.Warn("a turn is hung: the watchdog takes a step", "task", t.ID, "role", st.role.Name, "step", step+1)
	after := store.Run{} // the run the next pass reads the role through: only the nudge keeps the hung one
	switch step {
	case 0:
		if err := r.mgr.StopShell(ctx, run); err != nil {
			r.log.Warn("stop a hung turn's shell", "run", run.ID, "err", err)
		}
		r.nudge(ctx, t, a, st, run)
		after = run
	case 1:
		if err := r.restart(ctx, t, a, st, true, "its turn hung"); err != nil {
			r.log.Warn("resume a hung session", "role", st.role.Name, "err", err)
		}
	case 2:
		if _, err := r.fromFiles(ctx, t, a, st, "its turn hung", ""); err != nil {
			r.log.Warn("start a hung role fresh", "role", st.role.Name, "err", err)
		}
	default:
		r.mu.Lock()
		st.stuck, st.ranOut = true, r.now()
		r.mu.Unlock()
		line := ""
		if s, ok := r.liveSession(ctx, st); ok {
			screen, _ := r.mgr.ReadRecent(ctx, s, agents.HealthLines)
			line = lastLine(screen)
		}
		return Event{TaskID: t.ID, Role: st.role.Name, RunID: run.ID, Kind: EventStuck, Reason: line}, true
	}
	r.baseline(ctx, st, after)
	return Event{}, false
}

// nudge tells a hung run's agent, within the run, that it seems stuck, or,
// when the agents manager counts background work of the agent as running
// (the work may hold the run open after the agent ended its turn), what it
// counts and what ending the turn means: a ledger entry of the text, then
// agents.Manager.Tell.
func (r *Runtime) nudge(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, run store.Run) {
	text := nudgeText
	if n, ok := r.mgr.BackgroundTasks(ctx, run); ok && n > 0 {
		text = backgroundNudge(n)
	}
	e, err := r.reg.AppendLedger(ctx, registry.LedgerEntry{TaskID: t.ID, AttemptID: a.ID, Role: st.role.Name, RunID: run.ID,
		Hash: PromptHash(text), Text: text})
	if err != nil {
		r.log.Warn("the ledger did not take a nudge", "role", st.role.Name, "err", err)
		return
	}
	err = r.mgr.Tell(ctx, run, text)
	verdict := registry.DeliveredYes
	if err != nil {
		r.log.Warn("nudge a hung turn", "role", st.role.Name, "err", err)
		verdict = registry.DeliveredUnknown                      // herdr may have typed it
		if neverSent(err) || errors.Is(err, agents.ErrBlocked) { // Tell's dialog check, or herdr's refusal
			verdict = registry.DeliveredNo
		}
	}
	r.settle(ctx, e, verdict)
	r.mu.Lock()
	st.nudges++
	r.mu.Unlock()
}

// RecoverLost recovers role (as it runs) of attempt a of task t whose pane
// was lost, or whose last turn a recovery gave up with no turn in its place
// (RecoveryPrefix, which a stage rebuilt after a restart reads): its next
// recovery of MaxRecoveries, after its wait (recoverRole); then the
// recoveries ran out (EventLost, "no progress after <n> recoveries", at the
// next Observe). Each recovery that shows no progress within
// NoProgressAfter takes the next step (Watchdog).
// A role this runtime already recovers is left to the watchdog. One whose
// steps ran out (stuck, lost) is left so while a turn of it is at work in a
// live pane (its end, or you in the pane, moves it); with nothing at work,
// its ladders start over: the stage asks only once your retry rebuilt it.
// One whose recovery cannot run now (a pause, you in its pane, a failure)
// is due it, and the watchdog takes it once it may. One a heal of the
// factory's herdr server is to relaunch is left to the heal.
func (r *Runtime) RecoverLost(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role) error {
	role, err := r.RunsAs(t, role)
	if err != nil {
		return err
	}
	if r.healDue(t.ID, a.ID, role.Name) {
		return nil
	}
	st, err := r.stateFor(ctx, a.ID, role.Name)
	if err != nil {
		return err
	}
	r.mu.Lock()
	ranOut, owned := st.stuck || st.lost, st.recovering || st.pending
	r.mu.Unlock()
	switch {
	case ranOut && r.turnAtWork(ctx, t, role.Name):
		return nil
	case ranOut: // your retry: the ladders start over
		r.mu.Lock()
		st.stuck, st.lost, st.pending, st.recovering = false, false, false, false
		st.step, st.resumes, st.fresh = 0, 0, false
		st.recoveries, st.counted, st.dueAt = 0, true, time.Time{}
		r.mu.Unlock()
		r.noteStartOver(ctx, t, a, role.Name, "your retry") // a restart counts from here too
	case owned:
		return nil
	}
	err = r.recoverRole(ctx, t, a, role, "its pane was lost or its recovery left no turn", false)
	switch spent, ok := errors.AsType[errRecoveriesSpent](err); {
	case ok:
		r.emit("", r.lostEvent(t, a, role, spent))
	case err != nil && !errors.Is(err, errRefusedRole):
		r.waitToRecover(a.ID, role.Name)
	}
	return err
}

// turnAtWork reports whether a run of role of task t is in flight in a live
// session (one that starts or runs), or whether the store cannot tell.
func (r *Runtime) turnAtWork(ctx context.Context, t registry.Task, role string) bool {
	pr, err := r.ownerRow(ctx, t.ID)
	if err != nil {
		return true
	}
	runs, err := r.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		return true
	}
	for _, run := range runs {
		if run.Role != role || !inFlight(run) || run.SessionID == nil {
			continue
		}
		if s, err := r.st.SessionByID(ctx, *run.SessionID); err != nil || s.State == store.SessionStarting || s.State == store.SessionLive {
			return true
		}
	}
	return false
}

// lostEvent is EventLost for a role whose recoveries ran out, once.
func (r *Runtime) lostEvent(t registry.Task, a registry.Attempt, role config.Role, spent errRecoveriesSpent) Event {
	r.mu.Lock()
	if st := r.roles[roleKey{a.ID, role.Name}]; st != nil {
		st.lost, st.recovering, st.pending, st.ranOut = true, false, false, r.now()
	}
	r.mu.Unlock()
	return Event{TaskID: t.ID, Role: role.Name, Kind: EventLost, Reason: spent.Error()}
}

// recoverRole is RecoverLost's next step: recovery n of the role's
// MaxRecoveries in the attempt (recoveriesUsed, which a restart reads from
// the task's recover events), after its wait (RecoveryWaits[n-1], counted
// from the first call that found it due: until then the role is due it, and
// the watchdog takes it once the wait is over). Its step is recoveryStep's:
// a role that took no prompt yet in the attempt has no first prompt to
// start afresh from, so it is relaunched, its stage sending that prompt
// next; one with a conversation is resumed before the last recovery, which
// starts a fresh session from the files. Each recovery is a recover event
// and an alert. Once MaxRecoveries ran, it is errRecoveriesSpent. Under a
// drain for a reboot (mayPrompt) a role with no turn in flight starts
// nothing and answers the drain's *WaitError: the role stays due, and the
// watchdog recovers it once the drain is lifted. One whose recovery turn
// hangs in flight (inFlight) takes its next step all the same, as the climb
// of a turn in flight does: the drain waits for that turn, and only this
// step cuts it.
func (r *Runtime) recoverRole(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, why string, inFlight bool) error {
	st, err := r.stateFor(ctx, a.ID, role.Name)
	if err != nil {
		return err
	}
	if err := r.mayRestart(ctx, t, st); err != nil {
		return err // no step is spent
	}
	if !inFlight {
		if err := r.mayPrompt(); err != nil {
			return err // drain.go: no step is spent either
		}
	}
	pr, err := r.owner(ctx, t, a.ID)
	if err != nil {
		return err
	}
	resume, err := r.mgr.ResumeID(ctx, pr.ID, agents.Role(role.Name))
	if err != nil {
		return err
	}
	unprompted := !r.hadPrompt(ctx, t.ID, a.ID, role.Name)
	used := r.recoveriesUsed(ctx, st)
	now := r.now()
	r.mu.Lock()
	if st.lost || used >= MaxRecoveries {
		st.recovering, st.dueAt = false, time.Time{}
		r.mu.Unlock()
		return errRecoveriesSpent{n: used}
	}
	n := used + 1
	if st.dueAt.IsZero() {
		st.dueAt = now.Add(RecoveryWaits[n-1])
	}
	if now.Before(st.dueAt) {
		st.recovering = true // the watchdog takes it once its wait is over
		due := st.dueAt
		r.mu.Unlock()
		r.log.Info("a role's recovery waits", "task", t.ID, "role", role.Name, "recovery", n, "due", due)
		return nil
	}
	step := recoveryStep(n, resume, unprompted)
	st.dueAt, st.recoveries, st.recovering = time.Time{}, n, true
	r.mu.Unlock()
	r.log.Warn("a role is recovered", "task", t.ID, "role", role.Name, "why", why, "step", step, "recovery", n)
	r.noteRecovery(ctx, t, a, role.Name, n, step, why)
	switch step {
	case "resume":
		err = r.restart(ctx, t, a, st, true, why)
	case "relaunch":
		err = r.relaunchRole(ctx, t, a, st, false)
	default:
		_, err = r.fromFiles(ctx, t, a, st, why, "")
	}
	r.baseline(ctx, st, store.Run{})
	return err
}

// relaunchRole starts role st afresh in attempt a of task t with no prompt
// (Stagger after the last start when stagger): a role that took none yet,
// whose stage sends its first prompt next. A starting or live session of it
// is quit hard first and its conversation, which took no prompt of the
// task, closed.
func (r *Runtime) relaunchRole(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, stagger bool) error {
	pr, err := r.owner(ctx, t, a.ID)
	if err != nil {
		return err
	}
	if s, ok := r.liveSession(ctx, st); ok {
		if err := r.mgr.QuitHard(ctx, s); err != nil {
			return fmt.Errorf("quit %s: %w", st.role.Name, err)
		}
		r.closeConversation(ctx, s)
	}
	return r.startIn(ctx, t, a, st, pr, "", stagger)
}

// restart starts role st again in attempt a, resumed from its stored
// conversation when resume (and it has one), and tells it to go on: a live
// session is quit hard first, and a lost one gets a pane again. Its runs in
// flight stay in flight while their session is live, so a restart that
// fails before the quit leaves the watchdog watching the role through them;
// once the session is quit (or found gone), they are abandoned for the
// recovery and the role is due its next step until a new run of it is sent
// (handOver), so a failure after the quit leaves nothing in flight on a
// retired session to hold the task's park.
func (r *Runtime) restart(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, resume bool, why string) error {
	if err := r.mayRestart(ctx, t, st); err != nil {
		return err
	}
	pr, err := r.owner(ctx, t, a.ID)
	if err != nil {
		return err
	}
	old := r.runsInFlight(ctx, pr, st.role.Name)
	r.letGoOf(ctx, t, old)
	if s, ok := r.liveSession(ctx, st); ok {
		if err := r.mgr.QuitHard(ctx, s); err != nil {
			return fmt.Errorf("quit %s: %w", st.role.Name, err)
		}
		if r.emptyConversation(ctx, t, pr, s.ID) {
			r.closeConversation(ctx, s) // the resume takes the role's own conversation, not one that never saw the task
		}
	}
	r.handOver(ctx, st, old, why)
	id, err := r.mgr.ResumeID(ctx, pr.ID, agents.Role(st.role.Name)) // after the quit: its own conversation is the newest
	if err != nil {
		return err
	}
	if !resume || id == "" {
		_, err := r.fromFiles(ctx, t, a, st, why, "")
		return err
	}
	if err := r.startIn(ctx, t, a, st, pr, id, false); err != nil {
		return err
	}
	_, err = r.send(ctx, t, a, st.role, pr, resumeText, store.RunRecovery)
	return err
}

// fromFiles starts role st afresh in attempt a and sends it the attempt's
// first prompt again, the task's files being its inputs, with then after
// it (a caller's prompt the fresh session is to act on; "" for none); the
// run's id comes back. A role that may not restart (mayRestart) is left as
// it is, with that error. Its runs in flight are handed over once its
// session is quit, as restart's are.
func (r *Runtime) fromFiles(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, why, then string) (string, error) {
	if err := r.mayRestart(ctx, t, st); err != nil {
		return "", err
	}
	first, err := r.firstPrompt(ctx, st)
	if err != nil {
		return "", err
	}
	pr, err := r.owner(ctx, t, a.ID)
	if err != nil {
		return "", err
	}
	old := r.runsInFlight(ctx, pr, st.role.Name)
	r.letGoOf(ctx, t, old)
	if s, ok := r.liveSession(ctx, st); ok {
		if err := r.mgr.QuitHard(ctx, s); err != nil {
			return "", fmt.Errorf("quit %s: %w", st.role.Name, err)
		}
		r.closeConversation(ctx, s)
	}
	r.handOver(ctx, st, old, "a fresh session: "+why)
	// The old conversation is gone: whether its prompts arrived no longer
	// matters, and none of them is sent again (the first prompt is the
	// fresh session's own).
	if _, err := r.letGoWhere(ctx, t.ID, func(e registry.LedgerEntry) bool {
		return e.AttemptID == a.ID && e.Role == st.role.Name
	}); err != nil {
		r.log.Warn("let go of the prompts a fresh session replaces", "role", st.role.Name, "err", err)
	}
	if err := r.startIn(ctx, t, a, st, pr, "", false); err != nil {
		return "", err
	}
	return r.send(ctx, t, a, st.role, pr, freshText(first, then), store.RunRecovery)
}

// letGoOf lets go of the prompts of unknown delivery of runs old, whose
// session a recovery is about to restart (the watchdog's nudge, say): their
// fate no longer matters, and a quit that fails leaves them watched by the
// steps, not sent to you in the middle of them (EventDeliveryUnknown).
func (r *Runtime) letGoOf(ctx context.Context, t registry.Task, old []string) {
	if len(old) == 0 {
		return
	}
	if _, err := r.letGoWhere(ctx, t.ID, func(e registry.LedgerEntry) bool { return slices.Contains(old, e.RunID) }); err != nil {
		r.log.Warn("let go of the prompts of the runs a recovery replaces", "err", err)
	}
}

// handOver abandons runs old, whose session the recovery quit or found
// gone, with RecoveryPrefix and why, and keeps role st due its next step
// until a new run of it is sent (pending, which sent clears): the watchdog
// takes that step, and a stage rebuilt after a restart reads the reason
// and calls RecoverLost.
func (r *Runtime) handOver(ctx context.Context, st *roleState, old []string, why string) {
	for _, id := range old {
		r.abandon(ctx, id, RecoveryPrefix+why)
	}
	r.mu.Lock()
	st.pending = true
	r.mu.Unlock()
}

// freshLead joins the attempt's first prompt and what the caller meant to
// say in the prompt a fresh session starts with (freshText).
const freshLead = "\n\n---\n\nmagnum started this session afresh: the one before made no progress. Read the task's files as " +
	"the prompt above says, then:\n\n"

// freshText is the prompt a fresh session starts with: the attempt's first
// prompt, then what the caller meant to say when it is something else.
func freshText(first, then string) string {
	if strings.TrimSpace(then) == "" || normalPrompt(then) == normalPrompt(first) {
		return first
	}
	return first + freshLead + then
}

// TurnText is the prompt of the turn a ledger entry's text (scrubbed)
// starts: what the caller meant to say, which the prompt of a fresh session
// (freshText) carries after the attempt's first prompt, else text itself.
// The factory tells its fix turns by it.
func TurnText(text string) string {
	if _, then, ok := strings.Cut(text, freshLead); ok {
		return then
	}
	return text
}

// emptyConversation reports whether the conversation of session id (its
// row as the quit left it) never took a prompt of task t: no run of any row
// of owner row pr in that conversation has a ledger entry that may have
// arrived (a fresh session whose first prompt herdr refused). A
// conversation it cannot tell of, or a row with none, is not.
func (r *Runtime) emptyConversation(ctx context.Context, t registry.Task, pr store.PR, id int64) bool {
	s, err := r.st.SessionByID(ctx, id)
	if err != nil || store.Deref(s.SessionID) == "" {
		return false
	}
	sessions, err := r.st.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return false
	}
	rows := map[int64]bool{}
	for _, x := range sessions {
		if store.Deref(x.SessionID) == store.Deref(s.SessionID) {
			rows[x.ID] = true
		}
	}
	runs, err := r.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		return false
	}
	ofIt := map[string]bool{}
	for _, run := range runs {
		if run.SessionID != nil && rows[*run.SessionID] {
			ofIt[run.ID] = true
		}
	}
	entries, err := r.reg.LedgerOf(ctx, t.ID)
	if err != nil {
		return false
	}
	return !slices.ContainsFunc(entries, func(e registry.LedgerEntry) bool {
		return ofIt[e.RunID] && e.Delivered != registry.DeliveredNo
	})
}

// closeConversation closes a quit session that is parked, so no later
// resume takes the conversation a fresh session replaced.
func (r *Runtime) closeConversation(ctx context.Context, s store.Session) {
	err := r.st.TransitionSession(ctx, s.ID, []string{store.SessionParked}, store.SessionClosed, func(u *store.SessionUpdate) {
		u.Set("closed_at", r.now())
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		r.log.Warn("close a replaced conversation", "session", s.ID, "err", err)
	}
}

// startIn starts role st's agent (resumed from resume when it is set) in a
// pane of the task's workspace: the pane its quit session left as a shell,
// else one EnsureWorkspace lays out; Stagger after the last start when
// stagger (the memory at warn, as a launch's).
func (r *Runtime) startIn(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, pr store.PR, resume string,
	stagger bool) error {
	pane := ""
	if s, ok := r.newestSession(ctx, pr.ID, st.role.Name); ok && (s.State == store.SessionParked || s.State == store.SessionClosed) {
		if snap, err := r.d.Herdr.Snapshot(ctx); err == nil {
			for _, p := range snap.Panes {
				if p.ID == store.Deref(s.HerdrPaneID) && p.Agent == "" {
					pane = p.ID
				}
			}
		}
	}
	if pane == "" {
		label := a.WorkspaceLabel
		if label == "" {
			label = WorkspaceLabel(t, a)
		}
		path, env := r.copyOf(st)
		ws, err := r.mgr.EnsureWorkspace(ctx, pr, path, env, label, []config.Role{st.role})
		if err != nil {
			return fmt.Errorf("a pane for %s: %w", st.role.Name, err)
		}
		pane = ws.Panes[agents.Role(st.role.Name)]
	}
	return r.start(ctx, pr, a, st.role, pane, resume, stagger, false)
}

// relaunch quits idle session s and resumes its conversation in a new
// process, in the pane the quit left; nothing is sent.
func (r *Runtime) relaunch(ctx context.Context, s store.Session) error {
	t, pr, err := r.ownerOf(ctx, s.PRID)
	if err != nil {
		return err
	}
	a, err := r.reg.AttemptByID(ctx, pr.HeadSHA)
	if err != nil {
		return err
	}
	st, err := r.stateFor(ctx, a.ID, s.Role)
	if err != nil {
		return err
	}
	if err := r.mayRestart(ctx, t, st); err != nil {
		return err
	}
	if err := r.mgr.Quit(ctx, s); err != nil {
		return err
	}
	return r.startIn(ctx, t, a, st, pr, store.Deref(s.SessionID), false)
}

// liveSession is role st's starting or live session.
func (r *Runtime) liveSession(ctx context.Context, st *roleState) (store.Session, bool) {
	pr, err := r.ownerRow(ctx, st.taskID)
	if err != nil {
		return store.Session{}, false
	}
	s, err := r.st.LiveSessionByPRRole(ctx, pr.ID, st.role.Name)
	return s, err == nil
}

// newestSession is the newest session of role on owner row prID.
func (r *Runtime) newestSession(ctx context.Context, prID int64, role string) (store.Session, bool) {
	ss, err := r.st.SessionsByPR(ctx, prID)
	if err != nil {
		return store.Session{}, false
	}
	for _, s := range slices.Backward(ss) {
		if s.Role == role {
			return s, true
		}
	}
	return store.Session{}, false
}

// abandonRuns abandons role's runs of owner row pr that are in flight.
func (r *Runtime) abandonRuns(ctx context.Context, pr store.PR, role, why string) {
	for _, id := range r.runsInFlight(ctx, pr, role) {
		r.abandon(ctx, id, why)
	}
}

// runsInFlight are the ids of role's runs of owner row pr that are in
// flight (none when the store cannot say).
func (r *Runtime) runsInFlight(ctx context.Context, pr store.PR, role string) []string {
	runs, err := r.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		return nil
	}
	var ids []string
	for _, run := range runs {
		if run.Role == role && inFlight(run) {
			ids = append(ids, run.ID)
		}
	}
	return ids
}

// The steps of a loop without progress (loopStep).
const (
	loopNone = iota
	loopCompact
	loopFresh
)

// loopStep is the step a prompt to role in attempt a takes against a loop
// without progress: MaxStagePrompts prompts in the stage since the last
// step, the same failing output SameFailures times in a row, or a context
// over MaxContext. The first time the session is compacted (and the prompt
// goes out), the second a fresh session starts from the files (in place of
// the prompt), the third is ErrNoProgress with EventNoProgress. A new
// attempt of the stage climbs again from a ladder that ran out; so does
// your retry, which goes on in the same attempt (retryStartsOver): the
// ladder starts over at step 0 from the stage's prompts now, its failures
// forgotten, and a loop after it is told again.
func (r *Runtime) loopStep(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, pr store.PR,
	entries []registry.LedgerEntry) (int, error) {
	n, err := r.stagePrompts(ctx, t.ID, a.Stage, role.Name, entries)
	if err != nil {
		return loopNone, err
	}
	k := loopKey{t.ID, a.Stage, role.Name}
	r.mu.Lock()
	l := r.loops[k]
	if l == nil {
		l = &loopState{}
		r.loops[k] = l
	}
	if l.reset {
		l.base, l.reset = n, false
	}
	if l.attempt != a.ID { // a new attempt of the stage: a loop left to you climbs again
		l.attempt = a.ID
		if l.step > loopFresh {
			l.step, l.base = 0, n
		}
	}
	stepped := l.step > loopNone
	r.mu.Unlock()
	over := r.retryStartsOver(ctx, t, a, role.Name, stepped, entries)
	r.mu.Lock()
	if over {
		l.step, l.base, l.fails = loopNone, n, nil
		delete(r.once, noProgressKey(k))
	}
	if l.step > loopFresh {
		r.mu.Unlock()
		return loopNone, fmt.Errorf("send to %s: %w (left to you in this attempt)", role.Name, ErrNoProgress)
	}
	why := ""
	switch {
	case n-l.base >= MaxStagePrompts:
		why = fmt.Sprintf("%d prompts in the %s stage", n, a.Stage)
	case len(l.fails) >= SameFailures && sameAll(l.fails):
		why = fmt.Sprintf("the same failure %d times: %s", len(l.fails), l.fails[0])
	}
	r.mu.Unlock()
	s, serr := r.st.LiveSessionByPRRole(ctx, pr.ID, role.Name)
	if why == "" && serr == nil {
		if used, ok := r.mgr.ContextUsed(ctx, s); ok && used > MaxContext {
			why = fmt.Sprintf("its context at %d%%", used)
		}
	}
	if why == "" {
		return loopNone, nil
	}
	if st, err := r.stateFor(ctx, a.ID, role.Name); err == nil && r.operatorIn(ctx, st) {
		return loopNone, fmt.Errorf("send to %s: %s, but the session is not compacted or restarted while you are in it: %w",
			role.Name, why, ErrOperatorInPane)
	}
	r.mu.Lock()
	l.step++
	l.base, l.fails = n, nil
	step := l.step
	r.mu.Unlock()
	r.log.Warn("a loop without progress", "task", t.ID, "role", role.Name, "why", why, "step", step)
	switch step {
	case 1:
		if serr == nil {
			if err := r.mgr.Compact(ctx, s); err != nil {
				r.log.Warn("compact a session in a loop", "role", role.Name, "err", err)
			}
		}
		return loopCompact, nil
	case 2:
		return loopFresh, nil
	}
	r.emit(noProgressKey(k), Event{TaskID: t.ID, Role: role.Name, Kind: EventNoProgress, Reason: "no progress: " + why})
	return loopNone, fmt.Errorf("send to %s: %s: %w", role.Name, why, ErrNoProgress)
}

// retryStartsOver reports whether your retry of task t starts the ladder
// of role's loop over before its next prompt in attempt a: a retry since
// the role's newest prompt in the attempt (entries; the attempt's start
// before any), which the task went to you after (its last turn ended, or the
// ladder ran out before the prompt left), when the ladder took a step since
// it last started (stepped) or the role has the factory's cap of prompts in
// the attempt (MaxStagePrompts). The prompt it lets out is newer than it,
// so a retry starts a ladder over once, and a restart in between changes
// nothing. A retry while neither held (the task waited on you for another
// cause) leaves the ladder as it was. A registry that cannot be read starts
// nothing over.
func (r *Runtime) retryStartsOver(ctx context.Context, t registry.Task, a registry.Attempt, role string, stepped bool,
	entries []registry.LedgerEntry) bool {
	newest, prompts := a.Started, 0
	for _, e := range entries {
		if e.Role == role && e.AttemptID == a.ID && e.Delivered != registry.DeliveredNo {
			prompts++
			if e.Sent.After(newest) {
				newest = e.Sent
			}
		}
	}
	if !stepped && prompts < MaxStagePrompts {
		return false
	}
	return r.retriedSince(ctx, t.ID, newest)
}

// noProgressKey keys the one EventNoProgress of a loop.
func noProgressKey(k loopKey) string {
	return fmt.Sprintf("noprogress:%d:%s:%s", k.task, k.stage, k.role)
}

func sameAll(xs []string) bool {
	for _, x := range xs[1:] {
		if x != xs[0] {
			return false
		}
	}
	return true
}
