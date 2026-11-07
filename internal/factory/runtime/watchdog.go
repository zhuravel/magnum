package runtime

// The watchdog and the recoveries (spec section 10.1). A turn past its
// role's timeout whose transcript did not grow and whose screen did not
// change for NoProgressAfter climbs three steps, each NoProgressAfter of
// stillness after the last: the shell stopped and a nudge, the session quit
// hard and resumed, a fresh session from the attempt's first prompt; then
// EventStuck with the pane's last line. A lost pane is resumed from its
// conversation, at most MaxResumes times an attempt, then starts fresh from
// the files; a recovery that shows no progress within NoProgressAfter takes
// the next step, and EventLost ends them. A loop without progress (Send) is
// compacted, then started fresh once, then left to the operator.

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
)

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
		if !seen[k] && st.recovering && !st.lost {
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

// watch takes the step a role's run (none: a recovery waiting for one)
// is due at now.
func (r *Runtime) watch(ctx context.Context, st *roleState, run store.Run, now time.Time) (Event, bool) {
	r.mu.Lock()
	climbing, recovering, done := st.step > 0, st.recovering, st.stuck || st.lost
	shift := r.shifts[run.ID]
	r.mu.Unlock()
	if done {
		return Event{}, false
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
	if recovering {
		err := r.recoverRole(ctx, t, a, st.role, "no progress for "+NoProgressAfter.String())
		if spent, ok := errors.AsType[errRecoveriesSpent](err); ok {
			return r.lostEvent(t, a, st.role, spent), true
		}
		if err != nil {
			r.log.Warn("the next recovery of a role", "attempt", a.ID, "role", st.role.Name, "err", err)
		}
		r.baseline(ctx, st)
		return Event{}, false
	}
	return r.climb(ctx, t, a, st, run)
}

// signature is what tells the watchdog the role moved: its transcript's
// size and its screen.
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
	return strconv.FormatInt(size, 10) + "\x00" + shortHash(stillScreen(screen))
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
// for counts from here.
func (r *Runtime) baseline(ctx context.Context, st *roleState) {
	sig := r.signature(ctx, st, store.Run{})
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
	switch step {
	case 0:
		if err := r.mgr.StopShell(ctx, run); err != nil {
			r.log.Warn("stop a hung turn's shell", "run", run.ID, "err", err)
		}
		r.nudge(ctx, t, a, st, run)
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
		st.stuck = true
		r.mu.Unlock()
		line := ""
		if s, ok := r.liveSession(ctx, st); ok {
			screen, _ := r.mgr.ReadRecent(ctx, s, agents.HealthLines)
			line = lastLine(screen)
		}
		return Event{TaskID: t.ID, Role: st.role.Name, RunID: run.ID, Kind: EventStuck, Reason: line}, true
	}
	r.baseline(ctx, st)
	return Event{}, false
}

// nudge tells a hung run's agent, within the run, that it seems stuck: a
// ledger entry, then agents.Manager.Tell.
func (r *Runtime) nudge(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, run store.Run) {
	e, err := r.reg.AppendLedger(ctx, registry.LedgerEntry{TaskID: t.ID, AttemptID: a.ID, Role: st.role.Name, RunID: run.ID,
		Hash: PromptHash(nudgeText), Text: nudgeText})
	if err != nil {
		r.log.Warn("the ledger did not take a nudge", "role", st.role.Name, "err", err)
		return
	}
	err = r.mgr.Tell(ctx, run, nudgeText)
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
// was lost: a resume of its conversation (at most MaxResumes in the
// attempt), then a fresh session from the files; then the recoveries ran
// out (EventLost, "no progress after <n> recoveries", at the next Observe).
// Each recovery that shows no progress within NoProgressAfter takes the
// next step (Watchdog).
func (r *Runtime) RecoverLost(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role) error {
	role, err := r.RunsAs(t, role)
	if err != nil {
		return err
	}
	err = r.recoverRole(ctx, t, a, role, "its pane was lost")
	if spent, ok := errors.AsType[errRecoveriesSpent](err); ok {
		r.emit("", r.lostEvent(t, a, role, spent))
	}
	return err
}

// lostEvent is EventLost for a role whose recoveries ran out, once.
func (r *Runtime) lostEvent(t registry.Task, a registry.Attempt, role config.Role, spent errRecoveriesSpent) Event {
	r.mu.Lock()
	if st := r.roles[roleKey{a.ID, role.Name}]; st != nil {
		st.lost, st.recovering = true, false
	}
	r.mu.Unlock()
	return Event{TaskID: t.ID, Role: role.Name, Kind: EventLost, Reason: spent.Error()}
}

// recoverRole is RecoverLost's next step.
func (r *Runtime) recoverRole(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, why string) error {
	st, err := r.stateFor(ctx, a.ID, role.Name)
	if err != nil {
		return err
	}
	if err := r.mayRestart(ctx, t, st); err != nil {
		return err // no step is spent
	}
	pr, err := r.owner(ctx, t, a.ID)
	if err != nil {
		return err
	}
	resume, err := r.mgr.ResumeID(ctx, pr.ID, agents.Role(role.Name))
	if err != nil {
		return err
	}
	r.mu.Lock()
	if st.lost {
		n := st.resumes + 1
		r.mu.Unlock()
		return errRecoveriesSpent{n: n}
	}
	step := "fresh"
	switch {
	case st.resumes < MaxResumes && resume != "":
		st.resumes++
		step = "resume"
	case !st.fresh:
		st.fresh = true
	default:
		n := st.resumes + 1
		st.recovering = false
		r.mu.Unlock()
		return errRecoveriesSpent{n: n}
	}
	st.recovering = true
	r.mu.Unlock()
	r.log.Warn("a role is recovered", "task", t.ID, "role", role.Name, "why", why, "step", step)
	if step == "resume" {
		err = r.restart(ctx, t, a, st, true, why)
	} else {
		_, err = r.fromFiles(ctx, t, a, st, why, "")
	}
	r.baseline(ctx, st)
	return err
}

// restart starts role st again in attempt a, resumed from its stored
// conversation when resume (and it has one), and tells it to go on: a live
// session is quit hard first, and a lost one gets a pane again. Its runs in
// flight are abandoned only once the prompt of the run that replaces them
// may have left (replaced): a restart that fails before (its quit, its
// resume id, its start, a prompt herdr refused) leaves them in flight, so
// the watchdog goes on watching the role through them, after a restart of
// the daemon too, and its next step comes NoProgressAfter later.
func (r *Runtime) restart(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, resume bool, why string) error {
	if err := r.mayRestart(ctx, t, st); err != nil {
		return err
	}
	pr, err := r.owner(ctx, t, a.ID)
	if err != nil {
		return err
	}
	old := r.runsInFlight(ctx, pr, st.role.Name)
	if s, ok := r.liveSession(ctx, st); ok {
		if err := r.mgr.QuitHard(ctx, s); err != nil {
			return fmt.Errorf("quit %s: %w", st.role.Name, err)
		}
	}
	id, err := r.mgr.ResumeID(ctx, pr.ID, agents.Role(st.role.Name)) // after the quit: its own conversation is the newest
	if err != nil {
		return err
	}
	if !resume || id == "" {
		_, err := r.fromFiles(ctx, t, a, st, why, "")
		return err
	}
	if err := r.startIn(ctx, t, a, st, pr, id); err != nil {
		return err
	}
	run, err := r.send(ctx, t, a, st.role, pr, resumeText, store.RunRecovery)
	r.replaced(ctx, old, run, "recovery: "+why)
	return err
}

// fromFiles starts role st afresh in attempt a and sends it the attempt's
// first prompt again, the task's files being its inputs, with then after
// it (a caller's prompt the fresh session is to act on; "" for none); the
// run's id comes back. A role that may not restart (mayRestart) is left as
// it is, with that error. Its runs in flight are abandoned once that prompt
// may have left (replaced), as restart's are.
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
	if s, ok := r.liveSession(ctx, st); ok {
		if err := r.mgr.QuitHard(ctx, s); err != nil {
			return "", fmt.Errorf("quit %s: %w", st.role.Name, err)
		}
		r.closeConversation(ctx, s)
	}
	// The old conversation is gone: whether its prompts arrived no longer
	// matters, and none of them is sent again (the first prompt is the
	// fresh session's own).
	if _, err := r.letGoWhere(ctx, t.ID, func(e registry.LedgerEntry) bool {
		return e.AttemptID == a.ID && e.Role == st.role.Name
	}); err != nil {
		r.log.Warn("let go of the prompts a fresh session replaces", "role", st.role.Name, "err", err)
	}
	if err := r.startIn(ctx, t, a, st, pr, ""); err != nil {
		return "", err
	}
	run, err := r.send(ctx, t, a, st.role, pr, freshText(first, then), store.RunRecovery)
	r.replaced(ctx, old, run, "a fresh session: "+why)
	return run, err
}

// freshText is the prompt a fresh session starts with: the attempt's first
// prompt, then what the caller meant to say when it is something else.
func freshText(first, then string) string {
	if strings.TrimSpace(then) == "" || normalPrompt(then) == normalPrompt(first) {
		return first
	}
	return first + "\n\n---\n\nmagnum started this session afresh: the one before made no progress. Read the task's files as " +
		"the prompt above says, then:\n\n" + then
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
// else one EnsureWorkspace lays out.
func (r *Runtime) startIn(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, pr store.PR, resume string) error {
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
	return r.start(ctx, pr, a, st.role, pane, resume, false)
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
	return r.startIn(ctx, t, a, st, pr, store.Deref(s.SessionID))
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

// replaced abandons the runs old once run, the recovery's run that replaces
// them, may have taken its prompt (its id came back from send): from then
// on that run, or its ledger entry, is what the role is watched by. A
// recovery whose prompt never left (no run) abandons nothing.
func (r *Runtime) replaced(ctx context.Context, old []string, run, why string) {
	if run == "" {
		return
	}
	for _, id := range old {
		if id != run {
			r.abandon(ctx, id, why)
		}
	}
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
// the prompt), the third is ErrNoProgress with EventNoProgress.
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
	if l.attempt != a.ID { // a new attempt of the stage (the operator's retry): a loop left to them climbs again
		l.attempt = a.ID
		if l.step > loopFresh {
			l.step, l.base = 0, n
		}
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
