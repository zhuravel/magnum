package runtime

// The observe pass: herdr's snapshot through agents.Manager.ObserveSnapshotAt
// (a run ends after two idle ticks, a lost pane is marked lost), turned into
// the factory's events, and the faults it heals itself (spec section 10.1).
// A run that ends on a usage limit, a logout or an overload is a pause of
// its CLI, not a failure: the kind's launches and prompts wait until the
// limit's reset, a login, or a backoff. A model's own limit moves the
// session to the role's next fallback model and the turn goes on. A Codex
// cybersecurity warning is a refusal, never retried. A dialog the factory
// does not answer is blocked (no key is sent, unlike the retro's escape);
// Codex's hooks review holds the task with its reason. A changed Codex
// login lifts its pauses and restarts the idle Codex sessions in new
// processes; a Codex session that never took its first prompt starts
// fresh.

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// The kinds of a pause (pause.who), as a wait names them.
const (
	pauseUsage    = "usage"
	pauseLogin    = "login"
	pauseOverload = "overload"
)

// pause is a hold on an agent kind: who and what as the wait says them,
// until when (a login pause holds until a login lifts it).
type pause struct {
	who, what string
	until     time.Time
}

func (p *pause) wait() *WaitError { return &WaitError{Who: p.who, What: p.what, Until: p.until} }

// pause holds agent kind for who until until (a usage limit's reset, else
// usageFallback; an overload's backoff) and returns the wait.
func (r *Runtime) pause(_ context.Context, kind, who string, until time.Time) *WaitError {
	now := r.now()
	p := &pause{who: who, until: until}
	switch who {
	case pauseUsage:
		if !p.until.After(now) {
			p.until = now.Add(usageFallback)
		}
		p.what = kind + " limit until " + r.clock(p.until)
	case pauseLogin:
		p.until, p.what = time.Time{}, kind+" is logged out"
	case pauseOverload:
		p.until = now.Add(overloadWait)
		p.what = kind + " is overloaded, tries again at " + r.clock(p.until)
	}
	r.mu.Lock()
	r.pauses[kind] = p
	r.mu.Unlock()
	r.log.Warn("an agent kind pauses: its launches and prompts wait", "kind", kind, "pause", p.who+": "+p.what)
	return p.wait()
}

// paused is the wait of agent kind's pause in force (nil for none; a usage
// or overload pause past its end lifts).
func (r *Runtime) paused(kind string) *WaitError {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pauses[kind]
	if p == nil {
		return nil
	}
	if p.who != pauseLogin && !r.now().Before(p.until) {
		delete(r.pauses, kind)
		return nil
	}
	return p.wait()
}

// lift ends agent kind's pauses of the kinds in whos.
func (r *Runtime) lift(kind, why string, whos ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.pauses[kind]; p != nil && slices.Contains(whos, p.who) {
		delete(r.pauses, kind)
		r.log.Info("an agent kind's pause is lifted", "kind", kind, "pause", p.who+": "+p.what, "because", why)
	}
}

// Observe runs one observe pass over herdr's snapshot and returns its
// events (and those Send and the recoveries found since the last pass): a
// run that ended (EventEnded), one that paused on its CLI's limit, login or
// overload (EventPaused, its reason and end), a refusal (EventRefused), a
// dialog left to the operator (EventBlocked, EventHooksTrust), a lost pane
// whose recoveries ran out (EventLost) and a loop without progress
// (EventNoProgress), and the turns typed into its panes (EventTyped, typed).
// A pass reads the user turns of an agent's transcript once, for the turns
// typed and the delivery of a prompt alike. A panic is contained: it comes
// back as the error.
func (r *Runtime) Observe(ctx context.Context) (evs []Event, err error) {
	defer contain("observe", r.log, &err)
	at := r.now()
	snap, err := r.d.Herdr.Snapshot(ctx)
	if err != nil {
		r.noteFocus(herdr.Snapshot{}, at) // a herdr that does not answer shows you in none of its panes
		return r.drain(), fmt.Errorf("observe: %w", err)
	}
	r.noteFocus(snap, at)
	obs, oerr := r.mgr.ObserveSnapshotAt(ctx, snap, at)
	for _, o := range obs {
		evs = append(evs, r.onObservation(ctx, o)...)
	}
	r.checkLogin(ctx)
	r.freshStarts(ctx)
	reads := turnReads{}
	r.reconcileUnknown(ctx, reads)
	evs = append(evs, r.typed(ctx, reads)...)
	if oerr != nil {
		oerr = fmt.Errorf("observe: %w", oerr)
	}
	return append(r.drain(), evs...), oerr
}

// contain turns a panic of a pass the engine runs outside a task's step
// into its error, with the stack in the log, both redacted as the engine's
// containment redacts them: a panic's value can carry a pane's text.
func contain(what string, log interface {
	Error(msg string, args ...any)
}, err *error) {
	if p := recover(); p != nil {
		msg := execx.Redact(fmt.Sprint(p))
		log.Error("factory runtime: a pass panicked", "pass", what, "panic", msg, "stack", execx.Redact(string(debug.Stack())))
		*err = fmt.Errorf("%s: panic: %s", what, msg)
	}
}

// emit queues an event for the next Observe, once per key ("" = always).
func (r *Runtime) emit(key string, ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if key != "" {
		if r.once[key] {
			return
		}
		r.once[key] = true
	}
	r.pending = append(r.pending, ev)
}

// drain returns the queued events and forgets them.
func (r *Runtime) drain() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	evs := r.pending
	r.pending = nil
	return evs
}

// first reports whether key is new (and marks it).
func (r *Runtime) first(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.once[key] {
		return false
	}
	r.once[key] = true
	return true
}

// forget unmarks key.
func (r *Runtime) forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.once, key)
}

// onObservation turns one session's observation into events; one of you
// in the pane (ObsHumanActive) is the proof of what you typed there
// (sawYou).
func (r *Runtime) onObservation(ctx context.Context, o agents.Observation) []Event {
	r.noteSession(ctx, o)
	if o.Kind == agents.ObsHumanActive {
		r.sawYou(o.Session.ID)
		r.renewFocus(r.now())
	}
	if o.Status != herdr.StatusBlocked {
		r.forget(fmt.Sprintf("blocked:%d", o.Session.ID))
	}
	switch o.Kind {
	case agents.ObsCompleted:
		if o.Run != nil {
			return r.completed(ctx, o.Session, *o.Run)
		}
	case agents.ObsBlocked:
		return r.blocked(ctx, o)
	case agents.ObsLost:
		return r.lostPane(ctx, o)
	}
	return nil
}

// noteSession records on the RoleRun of the session's current attempt what
// the session tells first: its conversation id and transcript, and the
// time it first worked.
func (r *Runtime) noteSession(ctx context.Context, o agents.Observation) {
	pr, err := r.st.PRByID(ctx, o.PRID)
	if err != nil {
		return
	}
	attempt, role := pr.HeadSHA, o.Session.Role
	if o.Run != nil {
		attempt = o.Run.TargetSHA
	}
	if sid := store.Deref(o.Session.SessionID); sid != "" && r.first("sid:"+attempt+":"+role+":"+sid) {
		rr := registry.RoleRun{AttemptID: attempt, Role: role, SessionID: sid}
		rr.Transcript, _ = r.mgr.TranscriptPath(ctx, o.Session)
		if rr.Transcript == "" {
			r.forget("sid:" + attempt + ":" + role + ":" + sid) // read again once the file is there
		}
		r.record(ctx, rr)
	}
	if o.Status == herdr.StatusWorking && o.Run != nil && r.first("out:"+attempt+":"+role) {
		r.record(ctx, registry.RoleRun{AttemptID: attempt, Role: role, FirstOutput: r.now()})
	}
}

// record writes rr, logging a failure (an attempt the registry does not
// know is no factory run's).
func (r *Runtime) record(ctx context.Context, rr registry.RoleRun) {
	if err := r.reg.RecordRoleRun(ctx, rr); err != nil && !errors.Is(err, registry.ErrNotFound) {
		r.log.Warn("record a role's run", "attempt", rr.AttemptID, "role", rr.Role, "err", err)
	}
}

// settleLiaisonRun settles run, a turn of a task's liaison that ended:
// verified at once, as no stage checks a liaison's turn for an end fact
// (its end is no event of the task's: ownerOf), so it never stays among the
// agents store's active runs.
func (r *Runtime) settleLiaisonRun(ctx context.Context, run store.Run) {
	now := r.now()
	err := r.st.TransitionRun(ctx, run.ID, []string{store.RunEnded}, store.RunVerified, func(u *store.RunUpdate) {
		u.Set("verified_at", now)
		u.Set("outcome", "ended")
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		r.log.Warn("settle a turn of a task's liaison", "run", run.ID, "err", err)
	}
}

// completed is a run that ended (two idle ticks): its screen after its
// prompt tells an end from a limit, a logout, an overload, a model's own
// limit and a refusal. A liaison's turn is settled at once (settleLiaisonRun).
func (r *Runtime) completed(ctx context.Context, s store.Session, run store.Run) []Event {
	t, pr, err := r.ownerOf(ctx, run.PRID)
	if errors.Is(err, errLiaisonRow) {
		r.settleLiaisonRun(ctx, run)
		return nil
	}
	if err != nil {
		return nil
	}
	ev := Event{TaskID: t.ID, Role: run.Role, RunID: run.ID}
	kind := store.Deref(s.AgentKind)
	h := r.health(ctx, s, run)
	switch h.Kind {
	case agents.HealthUsageLimit, agents.HealthLoginRequired, agents.HealthOverloaded:
		who := map[agents.HealthKind]string{agents.HealthUsageLimit: pauseUsage, agents.HealthLoginRequired: pauseLogin,
			agents.HealthOverloaded: pauseOverload}[h.Kind]
		var until time.Time
		if h.ResetAt != nil {
			until = *h.ResetAt
		}
		w := r.pause(ctx, kind, who, until)
		r.pausedRun(ctx, run, w)
		r.calm(run.TargetSHA, run.Role, t.ID, kind, false)
		ev.Kind, ev.Reason = EventPaused, w.Error()
		return []Event{ev}
	case agents.HealthModelLimit:
		if r.modelFallback(ctx, t, pr, s, run, h) {
			return nil
		}
		var until time.Time
		if h.ResetAt != nil {
			until = *h.ResetAt
		}
		w := r.pause(ctx, kind, pauseUsage, until)
		r.pausedRun(ctx, run, w)
		r.calm(run.TargetSHA, run.Role, t.ID, kind, false)
		ev.Kind, ev.Reason = EventPaused, w.Error()
		return []Event{ev}
	case agents.HealthRefused:
		r.calm(run.TargetSHA, run.Role, t.ID, kind, true)
		r.abandon(ctx, run.ID, "refused: "+h.Detail)
		r.record(ctx, registry.RoleRun{AttemptID: run.TargetSHA, Role: run.Role, Ended: r.now(), Outcome: "failed:refused",
			EndWhy: textx.Clip(h.Detail, 300)})
		if err := r.mgr.Quit(ctx, s); err != nil { // never prompted again: nothing is retried
			r.log.Warn("quit a refused session", "role", run.Role, "err", err)
		}
		ev.Kind, ev.Reason = EventRefused, kind+": "+cmpText(h.Detail, "a safety warning")
		return []Event{ev}
	}
	r.ended(ctx, run)
	ev.Kind = EventEnded
	return []Event{ev}
}

// ended notes a run that ended well: its RoleRun's end, and the watchdog's
// and the recoveries' ladders of its role start over.
func (r *Runtime) ended(ctx context.Context, run store.Run) {
	rr := registry.RoleRun{AttemptID: run.TargetSHA, Role: run.Role}
	if run.EndedAt != nil {
		rr.Ended = *run.EndedAt
	}
	r.record(ctx, rr)
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.roles[roleKey{run.TargetSHA, run.Role}]; st != nil {
		st.step, st.stuck, st.recovering, st.sig, st.since = 0, false, false, "", time.Time{}
	}
}

// pausedRun settles a run that ended on its CLI's pause: abandoned in the
// agents store (no failure), paused on its RoleRun.
func (r *Runtime) pausedRun(ctx context.Context, run store.Run, w *WaitError) {
	r.abandon(ctx, run.ID, PausedPrefix+w.Error())
	r.record(ctx, registry.RoleRun{AttemptID: run.TargetSHA, Role: run.Role, Ended: r.now(), Outcome: "paused", EndWhy: w.Error()})
}

// health classifies the screen of session s after run's prompt, with the
// patterns of its kind.
func (r *Runtime) health(ctx context.Context, s store.Session, run store.Run) agents.Health {
	text, err := r.mgr.ReadRecent(ctx, s, agents.HealthLines)
	if err != nil {
		return agents.Health{Kind: agents.HealthOK}
	}
	if anchor := promptAnchor(run.PromptText); anchor != "" {
		if i := strings.LastIndex(text, anchor); i >= 0 {
			text = text[i+len(anchor):]
		}
	}
	return r.classify(store.Deref(s.AgentKind), text)
}

// classify is ClassifyWith over kind's health patterns (the defaults when
// they do not compile).
func (r *Runtime) classify(kind, text string) agents.Health {
	if k, ok := r.cfg.KindSpec(kind); ok {
		if rx, err := k.HealthPatterns.Compile(); err == nil {
			return agents.ClassifyWith(rx, text, r.now())
		}
	}
	return agents.ClassifyAt(text, r.now())
}

// promptAnchor is the line of a prompt a screen is cut after: its first
// line, when it is long enough to tell.
func promptAnchor(prompt string) string {
	line := strings.TrimSpace(textx.FirstLine(prompt))
	if len(line) < 12 {
		return ""
	}
	return textx.Clip(line, 60)
}

func cmpText(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// blocked is a session herdr shows blocked on a dialog the manager left
// alone: Codex's hooks review holds the task (EventHooksTrust), a usage
// limit, a login or an overload on screen pauses as on an end (Review Focus
// 5), anything else is EventBlocked with the dialog's line. Once per
// blocked spell; no key is ever sent.
func (r *Runtime) blocked(ctx context.Context, o agents.Observation) []Event {
	if !r.first(fmt.Sprintf("blocked:%d", o.Session.ID)) {
		return nil
	}
	t, _, err := r.ownerOf(ctx, o.PRID)
	if err != nil {
		return nil
	}
	ev := Event{TaskID: t.ID, Role: o.Session.Role}
	if o.Run != nil {
		ev.RunID = o.Run.ID
	}
	kind := store.Deref(o.Session.AgentKind)
	screen, _ := r.mgr.ReadRecent(ctx, o.Session, agents.HealthLines)
	if agents.HooksReviewShown(screen) {
		ev.Kind, ev.Reason = EventHooksTrust, kind+": hooks need review (Trust all, in the pane or by running "+kind+" once)"
		return []Event{ev}
	}
	switch h := r.classify(kind, screen); h.Kind {
	case agents.HealthUsageLimit, agents.HealthLoginRequired, agents.HealthOverloaded: // a pause, as on a completed run
		who := map[agents.HealthKind]string{agents.HealthUsageLimit: pauseUsage, agents.HealthLoginRequired: pauseLogin,
			agents.HealthOverloaded: pauseOverload}[h.Kind]
		var until time.Time
		if h.ResetAt != nil {
			until = *h.ResetAt
		}
		ev.Kind, ev.Reason = EventPaused, r.pause(ctx, kind, who, until).Error()
	case agents.HealthBlocked, agents.HealthTrustDialog:
		ev.Kind, ev.Reason = EventBlocked, "dialog: "+cmpText(h.Detail, lastLine(screen))
	default:
		ev.Kind, ev.Reason = EventBlocked, "dialog: "+cmpText(execx.Redact(lastLine(screen)), "a dialog the factory does not answer")
	}
	return []Event{ev}
}

// lastLine is text's last line that is not blank, trimmed and redacted.
func lastLine(text string) string {
	for _, l := range slices.Backward(strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")) {
		if l = strings.TrimSpace(l); l != "" {
			return textx.Clip(execx.Redact(l), 200)
		}
	}
	return ""
}

// lostPane recovers the role of a session whose pane is gone, while its
// attempt runs (RecoverLost); EventLost once the recoveries ran out.
func (r *Runtime) lostPane(ctx context.Context, o agents.Observation) []Event {
	t, pr, err := r.ownerOf(ctx, o.PRID)
	if err != nil || (t.State != registry.TaskActive && t.State != registry.TaskWaiting) {
		return nil
	}
	a, err := r.reg.AttemptByID(ctx, pr.HeadSHA)
	if err != nil || (a.State != registry.AttemptLaunching && a.State != registry.AttemptRunning) {
		return nil
	}
	role, ok := r.d.Config.FactoryRole(o.Session.Role)
	if !ok {
		return nil
	}
	err = r.recoverRole(ctx, t, a, role, "its pane was lost")
	if spent, ok := errors.AsType[errRecoveriesSpent](err); ok {
		return []Event{r.lostEvent(t, a, role, spent)}
	}
	if _, wait := errors.AsType[*WaitError](err); wait || errors.Is(err, ErrOperatorInPane) {
		r.waitToRecover(a.ID, role.Name) // the watchdog recovers it once the pause ends or you leave
		return nil
	}
	if err != nil && !errors.Is(err, errRefusedRole) {
		r.log.Warn("recover a lost pane", "task", t.ID, "role", role.Name, "err", err)
	}
	return nil
}

// waitToRecover marks role of attempt as due a recovery that waits (on a
// pause, on the operator): the watchdog takes it when it may.
func (r *Runtime) waitToRecover(attempt, role string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.roles[roleKey{attempt, role}]; st != nil {
		st.recovering = true
	}
}

// modelFallback continues run's turn, which ended on its model's own limit
// h, on the role's next fallback model: the limit recorded, the session
// switched (agents.Manager.SwitchModel), the run abandoned and the
// model-fallback prompt sent as its continuation. false when the kind
// cannot switch, no fallback is free, or the switch failed: the kind
// pauses as for a usage limit.
func (r *Runtime) modelFallback(ctx context.Context, t registry.Task, pr store.PR, s store.Session, run store.Run, h agents.Health) bool {
	limit, err := r.mgr.NoteModelLimit(ctx, s, h)
	if err != nil {
		r.log.Warn("record a model's limit", "role", run.Role, "err", err)
	}
	model, ok := r.mgr.FallbackModel(ctx, s, nil)
	if !ok {
		return false
	}
	if err := r.mgr.SwitchModel(ctx, s, model, agents.SwitchLimitHit); err != nil {
		r.log.Warn("switch a limited session to its fallback model", "role", run.Role, "model", model, "err", err)
		return false
	}
	a, err := r.reg.AttemptByID(ctx, run.TargetSHA)
	if err != nil {
		return false
	}
	role, ok := r.d.Config.FactoryRole(run.Role)
	if !ok {
		return false
	}
	head := t.HeadSHA
	if head == "" {
		head = a.ID
	}
	text, err := r.mgr.FallbackPrompt(agents.FallbackData{Model: model, Previous: limit.Model, Role: role.Name, URL: pr.URL, HeadSHA: head})
	if err != nil {
		r.log.Warn("the model-fallback prompt", "err", err)
		return false
	}
	r.abandon(ctx, run.ID, "its model's limit: continued on "+model)
	if _, err := r.send(ctx, t, a, role, pr, text, store.RunContinue); err != nil {
		r.log.Warn("continue on the fallback model", "role", role.Name, "model", model, "err", err)
	}
	return true
}

// checkLogin, at most once every loginEvery: a login pause whose CLI's own
// check passes again lifts; a Codex login whose account changed lifts
// Codex's login and usage pauses (they were the old account's) and
// restarts the idle Codex sessions in new processes (a process keeps the
// login it started with).
func (r *Runtime) checkLogin(ctx context.Context) {
	now := r.now()
	r.mu.Lock()
	if !r.loginRead.IsZero() && now.Sub(r.loginRead) < loginEvery {
		r.mu.Unlock()
		return
	}
	r.loginRead = now
	var kinds []string
	for kind, p := range r.pauses {
		if p.who == pauseLogin {
			kinds = append(kinds, kind)
		}
	}
	r.mu.Unlock()
	for _, kind := range kinds {
		if err := r.mgr.Preflight(ctx, kind); err == nil {
			r.lift(kind, "its login check passes", pauseLogin)
		}
	}
	login, err := r.s.login()
	if err != nil {
		return
	}
	r.mu.Lock()
	prev := r.loginFP
	r.loginFP = login.Fingerprint
	r.mu.Unlock()
	if prev == "" || prev == login.Fingerprint {
		return
	}
	r.lift(agents.KindCodex, "the Codex login changed", pauseLogin, pauseUsage)
	r.restartIdleCodex(ctx)
}

// restartIdleCodex restarts every live Codex session of the factory with
// no run in flight and its agent idle: quit, then resumed in a new
// process, which reads the new login.
func (r *Runtime) restartIdleCodex(ctx context.Context) {
	live, err := r.st.LiveSessions(ctx)
	if err != nil {
		r.log.Warn("restart the Codex sessions after a login change", "err", err)
		return
	}
	active, err := r.st.ActiveRuns(ctx)
	if err != nil {
		return
	}
	for _, s := range live {
		if store.Deref(s.AgentKind) != agents.KindCodex || s.State != store.SessionLive || store.Deref(s.SessionID) == "" {
			continue // a session without a conversation keeps its process: a fresh one would lose its context
		}
		if st := herdr.Status(store.Deref(s.AgentStatus)); st == herdr.StatusWorking || st == herdr.StatusBlocked {
			continue
		}
		if slices.ContainsFunc(active, func(run store.Run) bool {
			return run.SessionID != nil && *run.SessionID == s.ID && inFlight(run)
		}) {
			continue
		}
		if err := r.relaunch(ctx, s); err != nil && !errors.Is(err, errRefusedRole) && !errors.Is(err, errLiaisonRow) {
			r.log.Warn("restart an idle Codex session after a login change", "role", s.Role, "err", err)
		}
	}
}

// freshStarts starts afresh every Codex session that never took its first
// prompt (agents.Manager.TookNoPrompt), once per session, and sends it
// that prompt again.
func (r *Runtime) freshStarts(ctx context.Context) {
	live, err := r.st.LiveSessions(ctx)
	if err != nil {
		return
	}
	now := r.now()
	for _, s := range live {
		if store.Deref(s.AgentKind) != agents.KindCodex || store.Deref(s.SessionID) != "" || !r.dueCheck(s.ID, now) ||
			!r.mgr.TookNoPrompt(ctx, s) || !r.first(fmt.Sprintf("noprompt:%d", s.ID)) {
			continue
		}
		t, pr, err := r.ownerOf(ctx, s.PRID)
		if err != nil {
			continue
		}
		a, err := r.reg.AttemptByID(ctx, pr.HeadSHA)
		if err != nil || (a.State != registry.AttemptLaunching && a.State != registry.AttemptRunning) {
			continue
		}
		st, err := r.stateFor(ctx, a.ID, s.Role)
		if err != nil {
			continue
		}
		if err := r.mayRestart(ctx, t, st); err != nil {
			r.forget(fmt.Sprintf("noprompt:%d", s.ID)) // look again once it may
			continue
		}
		r.mu.Lock()
		st.noPrompt++
		n := st.noPrompt
		r.mu.Unlock()
		if n > 1 { // the fresh session took none either: the operator looks
			r.emit("", Event{TaskID: t.ID, Role: s.Role, Kind: EventBlocked,
				Reason: "codex: the session took no prompt, and neither did a fresh one"})
			continue
		}
		r.log.Warn("a Codex session never took its first prompt; it starts fresh", "task", t.ID, "role", s.Role)
		if _, err := r.fromFiles(ctx, t, a, st, "it never took its first prompt", ""); err != nil {
			r.log.Warn("start a session that took no prompt fresh", "role", s.Role, "err", err)
		}
	}
}

// dueCheck reports whether Codex session id is due a TookNoPrompt read (at
// most one a minute: it lists the Codex home's rollouts).
func (r *Runtime) dueCheck(id int64, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.checked[id]; ok && now.Sub(last) < loginEvery {
		return false
	}
	r.checked[id] = now
	return true
}

// Proving reports whether the proof that ledger entry id never arrived is
// under way: a read (Reconcile) found no such turn in a transcript written
// past the prompt less than provingBound ago, and the read after
// RedeliverAfter decides. A proof whose later reads failed (the registry or
// the agents store refused) is under way no longer once its bound passed,
// so its task is told to you rather than held for good; a read that settles
// it or finds no proof ends it. It is the runtime's own: a fresh runtime
// after a restart starts every proof again.
func (r *Runtime) Proving(id int64) bool {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	first, ok := r.misses[id]
	return ok && now.Sub(first) < provingBound
}

// provingBound is how long a proof may stay under way: its second read
// comes RedeliverAfter after its first, on an observe pass at most
// loginEvery apart (reconcileUnknown), with one more RedeliverAfter for a
// read that failed.
const provingBound = 2*RedeliverAfter + loginEvery

// reconcileUnknown settles, at most once every loginEvery, the prompts of
// unknown delivery a stalled or cut-short Submit left (Reconcile). One
// proven never to have arrived is EventUndelivered, for its stage to send
// it once more; one its transcript shows arrived is EventDelivered (its run
// is watched as submitted, nothing is sent again); one nobody can tell
// past UnknownAfter is EventDeliveryUnknown, once, unless its proof is
// under way (a first read found no such turn: a fresh runtime after a
// restart starts the proof again, and its first read is no reason to call
// you). The transcripts it reads are reads' (Observe's pass).
func (r *Runtime) reconcileUnknown(ctx context.Context, reads turnReads) {
	now := r.now()
	r.mu.Lock()
	due := r.reconciled.IsZero() || now.Sub(r.reconciled) >= loginEvery
	if due {
		r.reconciled = now
	}
	r.mu.Unlock()
	if !due {
		return
	}
	es, err := r.reg.Undelivered(ctx)
	if err != nil {
		return
	}
	for _, e := range es {
		got, err := r.reconcile(ctx, e, reads)
		if err != nil {
			r.log.Warn("reconcile a prompt's delivery", "entry", e.ID, "err", err)
		}
		switch got {
		case registry.DeliveredNo:
			r.emit("", Event{TaskID: e.TaskID, Role: e.Role, RunID: e.RunID, Kind: EventUndelivered,
				Reason: "the prompt to " + e.Role + " never arrived (its transcript, read twice, has no such turn): it goes once more"})
			continue
		case registry.DeliveredYes:
			r.emit("", Event{TaskID: e.TaskID, Role: e.Role, RunID: e.RunID, Kind: EventDelivered,
				Reason: "the prompt to " + e.Role + " arrived (its transcript holds it): its run is watched, never sent again"})
			continue
		}
		if got == registry.DeliveredUnknown && now.Sub(e.Sent) >= UnknownAfter && !r.Proving(e.ID) {
			// No transcript tells, and the role gets no prompt meanwhile:
			// the operator looks at the pane and lets it go (LetGo).
			r.emit(unknownKey(e.ID), Event{TaskID: e.TaskID, Role: e.Role, RunID: e.RunID, Kind: EventDeliveryUnknown,
				Reason: UnknownReason(e.TaskID, e.Role)})
		}
	}
}
