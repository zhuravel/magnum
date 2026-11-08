package agents

// An agent's own hooks tell the daemon at once what it would otherwise see
// on its next observe pass (R50 slice 2): the pane env carries the daemon's
// hook socket (Deps.HookSocket), the hook script sends a datagram there
// (internal/agenthooks), and the daemon's listener hands each event to
// Wake. An event is a hint, never the source of truth: Wake reads herdr, the
// screen, the transcript or the rollout again and acts only through the
// observe pass's own paths (observeOne, answerPermission), and polling stays
// the floor. Anyone with the pane's env can send any datagram, so a forged
// or late event costs an early look and nothing else.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/store"
)

// SessionIDFromHook is EventSessionID's source of an id an agent's
// SessionStart hook gave (Wake).
const SessionIDFromHook = "hook"

// relookAfter are the waits between the looks Wake takes again after a
// Stop, an Interrupt or a PermissionRequest whose look found nothing to act
// on yet (herdr's status, the screen and the transcript may lag the hook by
// a second or two): +1 s, then +2 s; after that the observe clock decides.
var relookAfter = []time.Duration{time.Second, time.Second}

// lookMerge is how long after a look of a session another is skipped
// (Wake): a flood of events costs about four looks a second at most.
const lookMerge = 250 * time.Millisecond

// StopFailure is the class of API error a turn ended on, as an agent's
// StopFailure hook names it (Wake, Deps.HookFailure): one of a fixed list,
// never the agent's own text.
type StopFailure string

// The classes of StopFailure: Claude Code's error names, and
// StopFailureOther for any other (invalid_request, server_error,
// max_output_tokens, unknown, anything an agent made up).
const (
	StopFailureRateLimit      StopFailure = "rate_limit"
	StopFailureOverloaded     StopFailure = "overloaded"
	StopFailureAuthentication StopFailure = "authentication_failed"
	StopFailureBilling        StopFailure = "billing_error"
	StopFailureOther          StopFailure = "other"
)

// classifyStopFailure is the StopFailure the hook's error names.
func classifyStopFailure(errText string) StopFailure {
	switch f := StopFailure(errText); f {
	case StopFailureRateLimit, StopFailureOverloaded, StopFailureAuthentication, StopFailureBilling:
		return f
	}
	return StopFailureOther
}

// shownBy reports whether a screen the health classifier reads as kind
// shows failure f: a rate limit a usage, model or rate limit; an overload
// an overload; a failed authentication a login to do; a billing error a
// usage limit or a login; any other a failure of any kind.
func (f StopFailure) shownBy(kind HealthKind) bool {
	switch f {
	case StopFailureRateLimit:
		return kind == HealthUsageLimit || kind == HealthModelLimit || kind == HealthOverloaded
	case StopFailureOverloaded:
		return kind == HealthOverloaded
	case StopFailureAuthentication:
		return kind == HealthLoginRequired
	case StopFailureBilling:
		return kind == HealthUsageLimit || kind == HealthLoginRequired
	}
	return kind != HealthOK && kind != HealthBlocked && kind != HealthTrustDialog
}

// hookEnv sets env's hook variables (agenthooks.EnvSocket, EnvBin) from
// Deps.HookSocket and the binary (hookBin), or removes them when there is
// no socket or no binary; env itself is never changed.
func (m *Manager) hookEnv(env map[string]string) map[string]string {
	socket, bin := m.d.HookSocket, m.hookBin()
	_, hasSocket := env[agenthooks.EnvSocket]
	_, hasBin := env[agenthooks.EnvBin]
	if (socket == "" || bin == "") && !hasSocket && !hasBin {
		return env
	}
	out := make(map[string]string, len(env)+2)
	maps.Copy(out, env)
	if socket == "" || bin == "" {
		delete(out, agenthooks.EnvSocket)
		delete(out, agenthooks.EnvBin)
		return out
	}
	out[agenthooks.EnvSocket], out[agenthooks.EnvBin] = socket, bin
	return out
}

// hookBin is the binary a pane's hook runs: Deps.HookBin, else the
// layout's (Layout.Binary).
func (m *Manager) hookBin() string {
	if m.d.HookBin != "" {
		return m.d.HookBin
	}
	return m.d.Layout.Binary()
}

// Wake takes an event of an agent's hook (agenthooks.Listener's) as a hint
// to look at its pane now, outside the observe passes. The pane is mapped
// to its starting or live session by herdr_pane_id; an event of a pane no
// such session (of an agent) records is dropped. Every look takes the
// observe lock the passes take (obsMu), one herdr snapshot and the
// session's in-flight runs, and goes through observeOne, as a pass would;
// a pass whose snapshot started before a look's came back leaves the
// session alone, and the next pass returns the looks' observations (each
// session's newest, and every run's end and session's loss), so the daemon
// handles them as its own (the factory's turn end, the engine's lost pane,
// the clock's SecondReadingAt). A look never counts as an observe tick
// (nextObserveTick), so the deny's pace (answerPermission) and the namer's
// stay the passes'. The looks of one session merge: one within lookMerge
// of the last, or while another waits for the observe lock, is skipped.
//
//   - stop, interrupt and permission_request: a look now. A Stop counts as
//     the second idle reading when herdr shows the agent idle and all of
//     these hold: its session id is the session's, it comes from no
//     subagent (agent_id empty) or Codex helper (CODEX_THREAD_ID its
//     session id), its agent is the session's kind, it reports no
//     background task, nothing holds the turn (background work, a Codex
//     rollout still writing, a deny awaiting its continuation), and the
//     transcript's turn_duration (Claude) or the rollout's task_complete
//     (Codex), found where magnum reads the session's (never the path the
//     event names), is newer than the run's prompt (stopEndsTurn). Then the
//     run ends and Ended's channel closes, as on a pass. A permission
//     request has answerPermission deny the prompt the look sees; the hook
//     itself decides nothing. When the look found nothing to act on while
//     a run is in flight (or was merged into another), Wake looks again at
//     +1 s and +2 s on a goroutine of its own (relookAfter; never the
//     caller's), then leaves it to the clock. A late Stop of an earlier
//     turn, a subagent's or a forged one only causes an early look.
//   - stop_failure: a look now, then, when the event is the session's own
//     (a Claude session, its session id, no subagent's) and the screen read
//     again shows such a failure (CheckHealth, StopFailure.shownBy; read once
//     more a second later when it does not yet), the failure's class and the
//     screen's verdict to Deps.HookFailure, once per failure, which feeds the
//     engine's usage, login and overload handling (hookFailure). The
//     StopFailures of one session merge as its looks do. The agent's error
//     text never goes on, nor into a log.
//   - session_start: a row without a session id gets the event's, a UUID,
//     once the agent's own file proves it (adoptHookID): the Claude
//     transcript where magnum reads the slot's sessions, whose first record
//     names the id in the slot, or the Codex rollout of the TUI in the slot.
//     Until the file is there the event waits, and the pane's next event
//     checks it again.
//   - session_end: a look now.
//
// The rules a receiver keeps: ctx bounds the later looks too, so pass the
// daemon's, not one that ends when Wake returns, and call WaitWakes once
// the listener stopped. Wake runs the Deps callbacks (HookFailure on the
// caller's goroutine; OwnerName, Toast, IdentityEnv and the rest under the
// observe lock), so never call it under a lock a callback takes: the
// factory's OwnerName (ownerName) takes Runtime.mu, so the factory calls
// Wake, and its passes, without Runtime.mu held.
func (m *Manager) Wake(ctx context.Context, ev agenthooks.Event) {
	s, ok := m.hookSession(ctx, ev)
	if !ok {
		return
	}
	switch ev.Name {
	case agenthooks.NameStop, agenthooks.NameInterrupt, agenthooks.NamePermissionRequest:
		var hint *agenthooks.Event
		if ev.Name == agenthooks.NameStop && creditable(ev) {
			hint = &ev
		}
		if o, res := m.look(ctx, s.ID, hint); res == lookMerged || res == lookDone && lookAgainAfter(o) {
			m.lookAgain(ctx, s.ID, hint)
		}
	case agenthooks.NameSessionEnd:
		m.look(ctx, s.ID, nil)
	case agenthooks.NameStopFailure:
		m.look(ctx, s.ID, nil)
		m.hookFailure(ctx, s.ID, ev)
	case agenthooks.NameSessionStart:
		m.hookSessionStart(ctx, s.ID, ev)
	}
}

// WaitWakes waits for the later looks Wake started (lookAgain): they end
// with the context Wake was given. A receiver calls it after its listener
// stopped and the last Wake returned, before it closes the registry.
func (m *Manager) WaitWakes() { m.wakes.Wait() }

// lookAgainAfter reports whether a look's observation o leaves something
// for a later look: it found nothing to act on while a run is in flight.
func lookAgainAfter(o Observation) bool {
	return o.Kind == "" && o.Run != nil && turnInFlight(*o.Run)
}

// hookSession is the starting or live agent session whose pane sent ev
// (the newest, should two rows record the pane); false, logged at debug
// level with the event's name alone, when there is none.
func (m *Manager) hookSession(ctx context.Context, ev agenthooks.Event) (store.Session, bool) {
	live, err := m.d.Store.LiveSessions(ctx)
	if err != nil {
		m.logErr(ctx, err, "agents: hook %s: %v", ev.Name, err)
		return store.Session{}, false
	}
	var found *store.Session
	for i, s := range live {
		if ev.Pane != "" && store.Deref(s.HerdrPaneID) == ev.Pane && (found == nil || s.ID > found.ID) {
			found = &live[i]
		}
	}
	if found == nil || !m.isAgentSession(*found) {
		m.logAt(slog.LevelDebug, "agents: hook %s: no agent session of magnum's has its pane", ev.Name)
		return store.Session{}, false
	}
	return *found, true
}

// creditable reports whether Stop ev may count as a turn's end by what it
// says itself: it is the session's own (no subagent's agent_id, no Codex
// helper's thread) and reports no background task. The rest is
// stopEndsTurn's, which reads the row, the transcript and the rollout.
func creditable(ev agenthooks.Event) bool {
	return ev.Name == agenthooks.NameStop && ev.SessionID != "" && ev.AgentID == "" && ev.Background == 0 &&
		(ev.CodexThread == "" || ev.CodexThread == ev.SessionID)
}

// lookResult is what became of a look.
type lookResult int

const (
	lookDone   lookResult = iota // the session was observed
	lookMerged                   // skipped: another look of the session was just taken or waits (lookMerge)
	lookFailed                   // the session is no longer live, herdr did not answer, or the observation failed
)

// lookMark is the merge state of one session's looks (under mu): when its
// last look ended, and whether one waits for the observe lock.
type lookMark struct {
	at     time.Time
	queued bool
}

// markDue marks a look (marks: looks) or a StopFailure's read (failing) of
// session id as under way, unless one is under way already or the last
// ended within lookMerge.
func (m *Manager) markDue(marks map[int64]*lookMark, id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	mark := marks[id]
	if mark == nil {
		mark = &lookMark{}
		marks[id] = mark
	}
	if mark.queued || !mark.at.IsZero() && m.now().Sub(mark.at) < lookMerge {
		return false
	}
	mark.queued = true
	return true
}

// markEnded records that the work markDue let through for session id
// ended.
func (m *Manager) markEnded(marks map[int64]*lookMark, id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mark := marks[id]; mark != nil {
		mark.queued, mark.at = false, m.now()
	}
}

// look observes session id from a fresh herdr snapshot under the observe
// lock (see Wake), with Stop hint (nil: none) able to count as its second
// idle reading.
func (m *Manager) look(ctx context.Context, id int64, hint *agenthooks.Event) (Observation, lookResult) {
	if !m.markDue(m.looks, id) {
		return Observation{}, lookMerged
	}
	defer m.markEnded(m.looks, id)
	m.obsMu.Lock()
	defer m.obsMu.Unlock()
	s, err := m.d.Store.SessionByID(ctx, id)
	if err != nil || !isLive(s) {
		return Observation{}, lookFailed
	}
	if m.adoptPending(ctx, s) {
		if fresh, err := m.d.Store.SessionByID(ctx, id); err == nil {
			s = fresh
		}
	}
	active, err := m.d.Store.ActiveRuns(ctx)
	if err != nil {
		m.logErr(ctx, err, "agents: hook look at %s: %v", s.Role, err)
		return Observation{}, lookFailed
	}
	capturedAt := m.now()
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		m.logAt(slog.LevelDebug, "agents: hook look at %s (session %d): no herdr snapshot; the observe pass decides: %v", s.Role, s.ID, err)
		return Observation{}, lookFailed
	}
	// A pass that started before the snapshot came back may hold an older
	// reading: it leaves the session alone (staleFor).
	m.lookedAt[s.ID] = m.now()
	o, err := m.observeOne(ctx, s, snap, paneIndex(snap), inflightRuns(active)[s.ID], m.now(), capturedAt, hint)
	if err != nil {
		m.logErr(ctx, err, "agents: hook look at %s: %v", s.Role, err)
		return o, lookFailed
	}
	if o.Kind == ObsCompleted || o.Kind == ObsLost {
		m.wakeEnded()
	}
	m.keepWoken(o)
	return o, lookDone
}

// wokenKinds bounds the observations with a kind besides the run ends and
// losses that the looks keep for one session until the next pass.
const wokenKinds = 4

// woken is what the looks of one session saw since the last pass: every
// observation with a kind, in order (kinds), and the newest reading.
type woken struct {
	kinds []Observation
	last  Observation
}

// final reports whether kind ends a run or a session: such an observation
// is never merged nor dropped.
func final(kind ObservationKind) bool { return kind == ObsCompleted || kind == ObsLost }

// keepWoken keeps a look's observation for the next pass (under obsMu),
// per session: every observation with a kind, which may happen once (a
// run's end, a session's loss, the start of a typing cooldown, a dialog),
// the same kind in a row once (the newest, with the first's
// CooldownUntil), at most wokenKinds of them besides the ends and losses,
// the oldest dropped first; and the newest reading, for its
// SecondReadingAt when it has no kind.
func (m *Manager) keepWoken(o Observation) {
	w := m.woken[o.Session.ID]
	if w == nil {
		w = &woken{}
		m.woken[o.Session.ID] = w
	}
	w.last = o
	if o.Kind == "" {
		return
	}
	if n := len(w.kinds); n > 0 && !final(o.Kind) && w.kinds[n-1].Kind == o.Kind {
		if first := w.kinds[n-1].CooldownUntil; !first.IsZero() {
			o.CooldownUntil = first
		}
		w.kinds[n-1] = o
		return
	}
	w.kinds = append(w.kinds, o)
	if final(o.Kind) {
		return
	}
	kept := 0
	for _, k := range w.kinds {
		if !final(k.Kind) {
			kept++
		}
	}
	if kept > wokenKinds { // one over: the oldest that may go goes
		i := slices.IndexFunc(w.kinds, func(k Observation) bool { return !final(k.Kind) })
		w.kinds = slices.Delete(w.kinds, i, i+1)
	}
}

// takeWoken returns the observations of Wake's looks since the last pass,
// session by session (each one's kinds, then its newest reading when it
// has no kind), and forgets them (under obsMu).
func (m *Manager) takeWoken() []Observation {
	var out []Observation
	for _, id := range slices.Sorted(maps.Keys(m.woken)) {
		w := m.woken[id]
		out = append(out, w.kinds...)
		if w.last.Kind == "" {
			out = append(out, w.last)
		}
	}
	clear(m.woken)
	return out
}

// relook is the state of one session's later looks (under mu): the Stop
// they may count (nil: none), and whether another event asked for later
// looks of its own while they ran.
type relook struct {
	hint  *agenthooks.Event
	again bool
}

// lookAgain has Wake look at session id again after each of relookAfter,
// on a goroutine of its own (WaitWakes waits for it), until a look acts on
// something, no run is in flight or the waits are over. A session with
// such looks under way gets no second goroutine: the event's Stop, if it
// may end the turn, is the one the next look uses, and once the looks are
// over they run once more for the event.
func (m *Manager) lookAgain(ctx context.Context, id int64, hint *agenthooks.Event) {
	m.mu.Lock()
	if st := m.relooks[id]; st != nil {
		st.again = true
		if hint != nil {
			st.hint = hint
		}
		m.mu.Unlock()
		return
	}
	st := &relook{hint: hint}
	m.relooks[id] = st
	m.mu.Unlock()
	m.wakes.Go(func() {
		for {
			m.laterLooks(ctx, id, st)
			m.mu.Lock()
			if !st.again || ctx.Err() != nil {
				delete(m.relooks, id)
				m.mu.Unlock()
				return
			}
			st.again = false
			m.mu.Unlock()
		}
	})
}

// laterLooks is one round of lookAgain's looks.
func (m *Manager) laterLooks(ctx context.Context, id int64, st *relook) {
	for _, wait := range relookAfter {
		if m.sleep(ctx, wait) != nil {
			return
		}
		m.mu.Lock()
		hint := st.hint
		m.mu.Unlock()
		if o, res := m.look(ctx, id, hint); res == lookFailed || res == lookDone && !lookAgainAfter(o) {
			return
		}
	}
}

// failRetry is how long after a StopFailure whose screen does not show the
// failure yet hookFailure reads the screen again: Claude may draw the error
// after its hook ran.
const failRetry = time.Second

// failKey is one failure HookFailure was given for a session: its class
// and the session's prompt it ended (last_prompt_at, else started_at).
type failKey struct {
	failure StopFailure
	prompt  time.Time
}

// hookFailure hands a StopFailure of session id to Deps.HookFailure (see
// Wake): only the session's own, the class from the fixed list, only when
// the screen, read again, shows such a failure, and once per failure (a
// class per prompt of the session). StopFailures of one session merge as
// its looks do (lookMerge, or another's read under way); a screen that
// does not show the failure yet is read again failRetry later, on a
// goroutine of its own (WaitWakes waits for it). Its logs never carry the
// agent's error text.
func (m *Manager) hookFailure(ctx context.Context, id int64, ev agenthooks.Event) {
	if m.d.HookFailure == nil {
		return
	}
	if !m.markDue(m.failing, id) {
		return
	}
	if m.checkFailure(ctx, id, ev, false) != failUnseen {
		m.markEnded(m.failing, id)
		return
	}
	m.wakes.Go(func() {
		defer m.markEnded(m.failing, id)
		if m.sleep(ctx, failRetry) == nil {
			m.checkFailure(ctx, id, ev, true)
		}
	})
}

// failCheck is what checkFailure made of a StopFailure.
type failCheck int

const (
	failDone   failCheck = iota // passed to HookFailure, or never will be (not the session's, passed already)
	failUnseen                  // the session's own, but the screen does not show it (yet)
)

// checkFailure reads session id's row and screen for StopFailure ev and
// passes it to HookFailure when they prove it (see hookFailure); last says
// whether no read follows, which logs a screen that does not show it.
func (m *Manager) checkFailure(ctx context.Context, id int64, ev agenthooks.Event, last bool) failCheck {
	s, err := m.d.Store.SessionByID(ctx, id)
	if err != nil || !isLive(s) {
		return failDone
	}
	failure, sid := classifyStopFailure(ev.Error), store.Deref(s.SessionID)
	key := failKey{failure: failure, prompt: s.StartedAt}
	if s.LastPromptAt != nil {
		key.prompt = *s.LastPromptAt
	}
	why := ""
	switch {
	case m.sessionKind(s) != KindClaude || ev.Agent != agenthooks.AgentClaude:
		why = "not a Claude session's"
	case ev.AgentID != "" || ev.CodexThread != "":
		why = "a subagent's"
	case sid == "" || ev.SessionID != sid:
		why = "not of the session's id"
	case m.failedAlready(s.ID, key):
		return failDone
	}
	if why != "" {
		m.logAt(slog.LevelDebug, "agents: %s (session %d): a StopFailure hook (%s) goes nowhere: %s", s.Role, s.ID, failure, why)
		return failDone
	}
	h, err := m.CheckHealth(ctx, s)
	switch {
	case err == nil && failure.shownBy(h.Kind):
		m.mu.Lock()
		m.failed[s.ID] = key
		m.mu.Unlock()
		m.d.HookFailure(s, failure, h)
		return failDone
	case !last:
		return failUnseen
	case err != nil:
		why = "its screen cannot be read: " + err.Error()
	default:
		why = fmt.Sprintf("its screen shows %s", h.Kind)
	}
	m.logAt(slog.LevelDebug, "agents: %s (session %d): a StopFailure hook (%s) goes nowhere: %s", s.Role, s.ID, failure, why)
	return failDone
}

// failedAlready reports whether HookFailure was given failure key of
// session id.
func (m *Manager) failedAlready(id int64, key failKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.failed[id]
	return ok && k.failure == key.failure && k.prompt.Equal(key.prompt)
}

// stopEndsTurn reports whether Stop ev counts as the second idle reading of
// run on session s, whose agent herdr shows idle with nothing holding its
// turn (see Wake): the Stop is creditable, of the session's kind and of its
// session id (sid, this reading's), no deny of the run awaits its
// continuation (endGap), and the session's transcript (Claude) or rollout
// (Codex), as magnum finds it (never the path the event names), ends a turn
// after the run's prompt (promptAt). Another kind, and a file it cannot
// find or read, say no: the passes end the turn.
func (m *Manager) stopEndsTurn(s store.Session, sid string, run store.Run, ev agenthooks.Event) bool {
	kind := m.sessionKind(s)
	if !creditable(ev) || ev.SessionID != sid || ev.Agent != kind || m.endGap(s.ID, &run) != CompletionGap {
		return false
	}
	at := promptAt(s, run)
	var ended bool
	var err error
	switch kind {
	case KindClaude:
		var path string
		if path, err = m.transcriptPath(s, sid); err == nil {
			ended, err = claudeTurnEndedAfter(path, at)
		}
	case KindCodex:
		var path string
		if path, err = m.rolloutPath(s, sid); err == nil {
			ended, err = taskCompleteAfter(path, at)
		}
	default:
		return false
	}
	if err != nil {
		m.logAt(slog.LevelDebug, "agents: %s (session %d): the Stop hook's turn end is not checked, the observe pass decides: %v", s.Role, s.ID, err)
		return false
	}
	return ended
}

// promptAt is when magnum last prompted session s within run: the latest of
// the run's creation, its submitted_at and the session's last_prompt_at (a
// Tell, a continuation after a deny).
func promptAt(s store.Session, run store.Run) time.Time {
	at := run.CreatedAt
	if run.SubmittedAt != nil && run.SubmittedAt.After(at) {
		at = *run.SubmittedAt
	}
	if s.LastPromptAt != nil && s.LastPromptAt.After(at) {
		at = *s.LastPromptAt
	}
	return at
}

// claudeTurnEndedAfter reports whether the newest turn_duration entry of
// the main thread (not a subagent's sidechain) in the end of the Claude
// transcript at path is stamped after at.
func claudeTurnEndedAfter(path string, at time.Time) (ended bool, err error) {
	err = scanRolloutTail(path, func(line []byte) bool {
		if !bytes.Contains(line, []byte(`"turn_duration"`)) {
			return false // most lines: no JSON decoding
		}
		var e struct {
			Type        string    `json:"type"`
			Subtype     string    `json:"subtype"`
			Timestamp   time.Time `json:"timestamp"`
			IsSidechain bool      `json:"isSidechain"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "system" || e.Subtype != "turn_duration" || e.IsSidechain {
			return false
		}
		ended = e.Timestamp.After(at)
		return true
	})
	return ended, err
}

// taskCompleteAfter reports whether the newest task_complete event in the
// end of the Codex rollout at path is stamped after at.
func taskCompleteAfter(path string, at time.Time) (ended bool, err error) {
	err = scanRolloutTail(path, func(line []byte) bool {
		if !bytes.Contains(line, []byte(`"task_complete"`)) {
			return false
		}
		var ev rolloutEvent
		if json.Unmarshal(line, &ev) != nil || ev.Payload.Type != "task_complete" {
			return false
		}
		ended = ev.Timestamp.After(at)
		return true
	})
	return ended, err
}

// hookSessionStart records the session id a SessionStart hook of session
// id's pane gave (adoptHookID), under the observe lock: the passes write
// session_id too.
func (m *Manager) hookSessionStart(ctx context.Context, id int64, ev agenthooks.Event) {
	m.obsMu.Lock()
	defer m.obsMu.Unlock()
	s, err := m.d.Store.SessionByID(ctx, id)
	if err != nil || !isLive(s) {
		return
	}
	if why := m.adoptHookID(ctx, s, ev); why != "" {
		m.logAt(slog.LevelDebug, "agents: %s (session %d): its SessionStart hook's session id is not recorded: %s", s.Role, s.ID, why)
	}
}

// adoptPending checks again the SessionStart of session s that waited for
// its transcript or rollout (hookIDs) while s still has no id, under obsMu,
// and reports whether it recorded the id.
func (m *Manager) adoptPending(ctx context.Context, s store.Session) bool {
	ev, ok := m.hookIDs[s.ID]
	if !ok {
		return false
	}
	if store.Deref(s.SessionID) != "" {
		delete(m.hookIDs, s.ID)
		return false
	}
	return m.adoptHookID(ctx, s, ev) == ""
}

// agentID is the shape of Claude's and Codex's session ids: a UUID.
var agentID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// adoptHookID records the session id SessionStart ev gave session s (its
// row, under obsMu), and returns why it did not ("" when it did):
//
//   - an id that is no UUID (Claude's and Codex's shape) is refused: it
//     could reach a resume's args as a flag;
//   - nested sessions are skipped: a subagent's (agent_id), a Codex
//     helper's (CODEX_THREAD_ID not the session id), another agent's than
//     the session's kind (a `codex exec` a Claude agent runs), and a
//     second, different id for a session that has one, or that waits for
//     the check of another;
//   - the agent's own file must prove it. A Claude session's transcript
//     where magnum reads the slot's sessions (<config dir>/projects/<the
//     slot's cwd, as recorded or with its symlinks resolved, as Claude names
//     it>/<id>.jsonl, the path the event names when it names one) must be a
//     file whose first record with a sessionId names the id, in the slot
//     when it names a cwd (claudeTranscriptNames). A Codex session's
//     rollout of the id, under its Codex home, must be the interactive
//     TUI's (session_meta source "cli", no parent thread) with the slot's
//     cwd (recoverSessionID's checks). A file not written yet keeps the
//     event in hookIDs for the pane's next look: Claude writes its
//     transcript, and Codex its rollout, at the session's first prompt;
//   - no other session of the registry holds the id.
//
// The id is recorded compare-and-set on a row still without one, keeping
// its state, with an EventSessionID (source SessionIDFromHook); herdr's id,
// when it reports another, wins on the next pass, as it does over a
// rollout's. The event's transcript path is never read nor stored: the
// readers find the file from the id and the slot (transcriptPath,
// rolloutPath).
func (m *Manager) adoptHookID(ctx context.Context, s store.Session, ev agenthooks.Event) string {
	id, kind := ev.SessionID, m.sessionKind(s)
	switch cur := store.Deref(s.SessionID); {
	case !agentID.MatchString(id):
		return "no session id of Claude's or Codex's shape"
	case ev.AgentID != "":
		return "a subagent's"
	case ev.CodexThread != "" && ev.CodexThread != id:
		return "a Codex helper thread's"
	case ev.Agent != kind:
		return fmt.Sprintf("a %s session's in a %s pane", ev.Agent, kind)
	case cur == id:
		delete(m.hookIDs, s.ID)
		return ""
	case cur != "":
		return "the session has another id"
	}
	if p, ok := m.hookIDs[s.ID]; ok && p.SessionID != id {
		return "the session waits for the check of another id"
	}
	var why string
	var wait bool
	switch kind {
	case KindClaude:
		why, wait = m.claudeTranscriptNames(s, id, ev.TranscriptPath)
	case KindCodex:
		why, wait = m.codexRolloutInSlot(s, id)
	default:
		why = "a kind without hooks"
	}
	if wait {
		m.hookIDs[s.ID] = ev
	} else if why != "" {
		delete(m.hookIDs, s.ID)
	}
	if why != "" {
		return why
	}
	if held, err := m.d.Store.SessionIDHeld(ctx, id, s.ID); err != nil || held {
		delete(m.hookIDs, s.ID)
		if err != nil {
			return err.Error()
		}
		return "another session holds it"
	}
	err := m.d.Store.TransitionSession(ctx, s.ID, liveStates, "", func(u *store.SessionUpdate) {
		u.Where("session_id", nil)
		u.Set("session_id", id)
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return "the row changed meanwhile"
		}
		m.logErr(ctx, err, "agents: %s (session %d): record the session id of its SessionStart hook: %v", s.Role, s.ID, err)
		return err.Error()
	}
	delete(m.hookIDs, s.ID)
	m.event(ctx, m.prSubject(ctx, s.PRID), "info", EventSessionID,
		fmt.Sprintf("%s: session id %s from its SessionStart hook", s.Role, id),
		map[string]any{"role": s.Role, "session": s.ID, "session_id": id, "source": SessionIDFromHook})
	return ""
}

// claudeTranscriptNames reports why the Claude transcript of session id is
// not s's ("" when it is; see adoptHookID): the file where magnum reads the
// sessions of s's slot, whose first record with a sessionId names id, in
// the slot when it names a cwd. A path the event names (eventPath) must be
// one of those. wait is true while no such file, or no such record, is
// there yet.
func (m *Manager) claudeTranscriptNames(s store.Session, id, eventPath string) (why string, wait bool) {
	dir, cwd := m.claudeDir(s), store.Deref(s.Cwd)
	if dir == "" || cwd == "" {
		return "no Claude config dir or checkout to check it in", false
	}
	var paths []string
	for _, c := range []string{cwd, resolvedDir(cwd)} {
		if p := filepath.Join(dir, "projects", projectDirName.ReplaceAllString(c, "-"), id+".jsonl"); !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	if eventPath != "" && !slices.Contains(paths, filepath.Clean(eventPath)) {
		return "the transcript it names is not where the slot's sessions are", false
	}
	for _, p := range paths {
		if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
			continue
		}
		rec, recCwd, err := firstSessionRecord(p)
		switch {
		case err != nil:
			return "its transcript cannot be read yet: " + err.Error(), true
		case rec == "":
			return "its transcript names no session yet", true
		case rec != id:
			return "its transcript is another session's", false
		case recCwd != "" && !sameTree(recCwd, cwd):
			return "its transcript works outside the slot", false
		}
		return "", false
	}
	return "its transcript is not written yet", true
}

// sessionRecordHead bounds how much of a transcript firstSessionRecord
// reads.
const sessionRecordHead = 256 << 10

// firstSessionRecord is the sessionId and cwd of the first complete record
// of the Claude transcript at path that carries a sessionId (Claude Code's
// file-history snapshots carry none); "" when none of the first
// sessionRecordHead bytes does.
func firstSessionRecord(path string) (sessionID, cwd string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	r := bufio.NewReaderSize(io.LimitReader(f, sessionRecordHead), 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil { // the end, or a line still being written
			return "", "", nil
		}
		var rec struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.SessionID != "" {
			return rec.SessionID, rec.Cwd, nil
		}
	}
}

// codexRolloutInSlot reports why Codex session id is not s's ("" when it
// is): its rollout under s's Codex home must be the interactive TUI's with
// s's cwd. wait is true when the rollout is not there yet.
func (m *Manager) codexRolloutInSlot(s store.Session, id string) (why string, wait bool) {
	home, cwd := m.codexHome(s), store.Deref(s.Cwd)
	if home == "" || cwd == "" {
		return "no Codex home or checkout to check it in", false
	}
	path, err := rolloutIn(home, id)
	if err != nil {
		return "its rollout is not written yet", true
	}
	h, ok, err := readRolloutHead(path)
	switch {
	case err != nil || !ok:
		return "its rollout has no session_meta of the id", false
	case h.id != id || h.source != codexSourceCLI || h.parent != "":
		return "its rollout is no interactive session's", false
	case resolvedDir(h.cwd) != resolvedDir(cwd):
		return "its rollout works outside the slot", false
	}
	return "", false
}

// inflightRuns maps each session to its pending, submitted and working
// runs, oldest first, of the active runs.
func inflightRuns(active []store.Run) map[int64][]store.Run {
	out := map[int64][]store.Run{}
	for _, r := range active {
		if r.SessionID != nil && r.State != store.RunEnded {
			out[*r.SessionID] = append(out[*r.SessionID], r)
		}
	}
	return out
}

// staleFor reports whether a pass's snapshot, taken at capturedAt, started
// before the snapshot of a Wake look of session id came back (under
// obsMu): the pass leaves the session alone. A look as old or older is
// forgotten.
func (m *Manager) staleFor(id int64, capturedAt time.Time) bool {
	at, ok := m.lookedAt[id]
	if !ok {
		return false
	}
	if capturedAt.Before(at) {
		return true
	}
	delete(m.lookedAt, id)
	return false
}

// forgetHooks drops the hook state of the sessions no longer starting or
// live (under obsMu).
func (m *Manager) forgetHooks(live []store.Session) {
	keep := make(map[int64]bool, len(live))
	for _, s := range live {
		keep[s.ID] = true
	}
	for id := range m.lookedAt {
		if !keep[id] {
			delete(m.lookedAt, id)
		}
	}
	for id := range m.hookIDs {
		if !keep[id] {
			delete(m.hookIDs, id)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, marks := range []map[int64]*lookMark{m.looks, m.failing} {
		for id, mark := range marks {
			if !keep[id] && !mark.queued {
				delete(marks, id)
			}
		}
	}
	for id := range m.failed {
		if !keep[id] {
			delete(m.failed, id)
		}
	}
}
