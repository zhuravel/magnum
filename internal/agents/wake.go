package agents

// An agent's own hooks tell the daemon at once what it would otherwise see
// on its next observe pass (R50 slice 2): the pane env carries the daemon's
// hook socket (Deps.HookSocket), the hook script sends a datagram there
// (internal/agenthooks), and the daemon's listener hands each event to
// Wake. An event is a hint, never the source of truth: Wake reads herdr, the
// transcript or the rollout again and acts only through the observe pass's
// own paths (observeOne, answerPermission), and polling stays the floor. An
// agent in the pane has the env and can send any datagram, so a forged or
// late event costs an early look and nothing else.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// SessionIDFromHook is EventSessionID's source of an id an agent's
// SessionStart hook gave (Wake).
const SessionIDFromHook = "hook"

// relookAfter are the waits between the looks Wake takes again after a
// Stop or an Interrupt that did not end the run (herdr's status and the
// transcript's turn end may lag the hook by a second or two): +1 s, then
// +2 s; after that the observe clock decides.
var relookAfter = []time.Duration{time.Second, time.Second}

// maxWoken bounds the observations of Wake's looks kept for the next pass.
const maxWoken = 256

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
// a pass whose snapshot is older than a look's leaves the session alone,
// and the next pass returns the look's observations, so the daemon handles
// them as its own (the factory's turn end, the engine's lost pane). A look
// never counts as an observe tick (nextObserveTick), so the deny's pace
// (answerPermission) and the namer's stay the passes'.
//
//   - stop and interrupt: a look now. A Stop counts as the second idle
//     reading when herdr shows the agent idle and all of these hold: its
//     session id is the session's, it comes from no subagent (agent_id
//     empty) or Codex helper (CODEX_THREAD_ID its session id), its agent is
//     the session's kind, it reports no background task, nothing holds the
//     turn (background work, a Codex rollout still writing, a deny awaiting
//     its continuation), and the transcript's turn_duration (Claude) or the
//     rollout's task_complete (Codex) is newer than the run's prompt
//     (stopEndsTurn). Then the run ends and Ended's channel closes, as on a
//     pass. Otherwise, while a run is in flight, Wake looks again at +1 s
//     and +2 s on a goroutine of its own (relookAfter; never the caller's),
//     then leaves it to the clock. A late Stop of an earlier turn, a
//     subagent's or a forged one only causes an early look.
//   - permission_request: a look now, so answerPermission denies the
//     prompt now. The hook itself decides nothing.
//   - stop_failure: a look now, and the error to Deps.HookFailure, which
//     feeds the engine's usage, login and overload handling.
//   - session_start: a row without a session id gets the event's, when it
//     passes the checks recoverSessionID's rule makes (adoptHookID). A
//     Codex session whose rollout is not written yet keeps the event and
//     is checked again at the pane's next event.
//   - session_end: a look now.
//
// ctx bounds the later looks too: pass the daemon's, not one that ends
// when Wake returns.
func (m *Manager) Wake(ctx context.Context, ev agenthooks.Event) {
	s, ok := m.hookSession(ctx, ev)
	if !ok {
		return
	}
	switch ev.Name {
	case agenthooks.NameStop, agenthooks.NameInterrupt:
		var hint *agenthooks.Event
		if ev.Name == agenthooks.NameStop && creditable(ev) {
			hint = &ev
		}
		if o, ok := m.look(ctx, s.ID, hint); ok && o.Kind != ObsCompleted && o.Kind != ObsLost && o.Run != nil && turnInFlight(*o.Run) {
			m.lookAgain(ctx, s.ID, hint)
		}
	case agenthooks.NamePermissionRequest, agenthooks.NameSessionEnd:
		m.look(ctx, s.ID, nil)
	case agenthooks.NameStopFailure:
		if o, ok := m.look(ctx, s.ID, nil); ok {
			s = o.Session
		}
		if m.d.HookFailure != nil && ev.Error != "" {
			m.d.HookFailure(s, ev.Error)
		}
	case agenthooks.NameSessionStart:
		m.hookSessionStart(ctx, s.ID, ev)
	}
}

// hookSession is the starting or live agent session whose pane sent ev
// (the newest, should two rows record the pane); false, logged at debug
// level with the event's name and pane alone, when there is none.
func (m *Manager) hookSession(ctx context.Context, ev agenthooks.Event) (store.Session, bool) {
	live, err := m.d.Store.LiveSessions(ctx)
	if err != nil {
		m.logErr(ctx, err, "agents: hook %s from pane %s: %v", ev.Name, ev.Pane, err)
		return store.Session{}, false
	}
	var found *store.Session
	for i, s := range live {
		if ev.Pane != "" && store.Deref(s.HerdrPaneID) == ev.Pane && (found == nil || s.ID > found.ID) {
			found = &live[i]
		}
	}
	if found == nil || !m.isAgentSession(*found) {
		m.logAt(slog.LevelDebug, "agents: hook %s from pane %s: no agent session of magnum's has the pane", ev.Name, ev.Pane)
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

// look observes session id from a fresh herdr snapshot under the observe
// lock (see Wake), with Stop hint (nil: none) able to count as its second
// idle reading. ok is false when the session is no longer starting or live,
// herdr does not answer or the observation failed (logged).
func (m *Manager) look(ctx context.Context, id int64, hint *agenthooks.Event) (Observation, bool) {
	m.obsMu.Lock()
	defer m.obsMu.Unlock()
	s, err := m.d.Store.SessionByID(ctx, id)
	if err != nil || !isLive(s) {
		return Observation{}, false
	}
	if m.adoptPending(ctx, s) {
		if fresh, err := m.d.Store.SessionByID(ctx, id); err == nil {
			s = fresh
		}
	}
	active, err := m.d.Store.ActiveRuns(ctx)
	if err != nil {
		m.logErr(ctx, err, "agents: hook look at %s: %v", s.Role, err)
		return Observation{}, false
	}
	capturedAt := m.now()
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		m.logAt(slog.LevelDebug, "agents: hook look at %s (session %d): no herdr snapshot; the observe pass decides: %v", s.Role, s.ID, err)
		return Observation{}, false
	}
	o, err := m.observeOne(ctx, s, snap, paneIndex(snap), inflightRuns(active)[s.ID], m.now(), capturedAt, hint)
	if err != nil {
		m.logErr(ctx, err, "agents: hook look at %s: %v", s.Role, err)
		return o, false
	}
	m.lookedAt[s.ID] = capturedAt
	if o.Kind == ObsCompleted || o.Kind == ObsLost {
		m.wakeEnded()
	}
	if o.Kind != "" {
		if len(m.woken) >= maxWoken {
			m.woken = m.woken[1:]
		}
		m.woken = append(m.woken, o)
	}
	return o, true
}

// lookAgain has Wake look at session id again after each of relookAfter,
// on a goroutine of its own, until the run ends, no run is in flight or the
// waits are over; a session with such looks scheduled gets no second
// goroutine, but a later Stop that may end its turn (hint) is the one its
// next look uses.
func (m *Manager) lookAgain(ctx context.Context, id int64, hint *agenthooks.Event) {
	m.mu.Lock()
	cur, scheduled := m.relooks[id]
	if !scheduled || hint != nil || cur == nil {
		m.relooks[id] = hint
	}
	m.mu.Unlock()
	if scheduled {
		return
	}
	m.wakes.Go(func() {
		defer func() {
			m.mu.Lock()
			delete(m.relooks, id)
			m.mu.Unlock()
		}()
		for _, wait := range relookAfter {
			if m.sleep(ctx, wait) != nil {
				return
			}
			m.mu.Lock()
			hint := m.relooks[id]
			m.mu.Unlock()
			o, ok := m.look(ctx, id, hint)
			if !ok || o.Kind == ObsCompleted || o.Kind == ObsLost || o.Run == nil || !turnInFlight(*o.Run) {
				return
			}
		}
	})
}

// takeWoken returns the observations of Wake's looks since the last pass
// and forgets them (under obsMu).
func (m *Manager) takeWoken() []Observation {
	out := m.woken
	m.woken = nil
	return out
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
// its Codex rollout (hookIDs) while s still has no id, under obsMu, and
// reports whether it recorded the id.
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

// adoptHookID records the session id SessionStart ev gave session s (its
// row, under obsMu), and returns why it did not ("" when it did):
//
//   - nested sessions are skipped: a subagent's (agent_id), a Codex
//     helper's (CODEX_THREAD_ID not the session id), another agent's than
//     the session's kind (a `codex exec` a Claude agent runs), and a
//     second, different id for a session that has one, or that waits for
//     the check of another;
//   - a Claude session's transcript, as the event names it, must be
//     <config dir>/projects/<the slot's cwd as Claude names it>/<id>.jsonl
//     (or exist there when the event names none): its cwd is the slot;
//   - a Codex session's rollout of the id, under its Codex home, must be
//     the interactive TUI's (session_meta source "cli", no parent thread)
//     with the slot's cwd (recoverSessionID's checks). A rollout not
//     written yet keeps the event in hookIDs for the next look (Codex
//     writes it at the session's first prompt);
//   - no other session of the registry holds the id.
//
// The id is recorded compare-and-set on a row still without one, keeping
// its state, with an EventSessionID (source SessionIDFromHook); herdr's id,
// when it reports another, wins on the next pass, as it does over a
// rollout's. The event's transcript path is checked, never stored: the
// readers find the file from the id and the slot (transcriptPath,
// rolloutPath).
func (m *Manager) adoptHookID(ctx context.Context, s store.Session, ev agenthooks.Event) string {
	id, kind := ev.SessionID, m.sessionKind(s)
	switch cur := store.Deref(s.SessionID); {
	case !rolloutID(id):
		return "no session id"
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
	switch kind {
	case KindClaude:
		if !m.claudeTranscriptInSlot(s, id, ev.TranscriptPath) {
			return "its transcript is not the slot's"
		}
	case KindCodex:
		if why, wait := m.codexRolloutInSlot(s, id); why != "" {
			if wait {
				m.hookIDs[s.ID] = ev
			}
			return why
		}
	default:
		return "a kind without hooks"
	}
	if held, err := m.d.Store.SessionIDHeld(ctx, id, s.ID); err != nil || held {
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

// claudeTranscriptInSlot reports whether the transcript of Claude session
// id of s lies where Claude Code keeps a session of s's checkout: path (the
// event's), else the file when the event names none, is
// <claudeDir>/projects/<cwd with every character but [A-Za-z0-9] turned
// into "-">/<id>.jsonl, the cwd as recorded or with its symlinks resolved.
func (m *Manager) claudeTranscriptInSlot(s store.Session, id, path string) bool {
	dir, cwd := m.claudeDir(s), store.Deref(s.Cwd)
	if dir == "" || cwd == "" {
		return false
	}
	for _, c := range []string{cwd, resolvedDir(cwd)} {
		want := filepath.Join(dir, "projects", projectDirName.ReplaceAllString(c, "-"), id+".jsonl")
		if path != "" && filepath.Clean(path) == want {
			return true
		}
		if path == "" && fsx.Exists(want) {
			return true
		}
	}
	return false
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

// staleFor reports whether a pass's snapshot, taken at capturedAt, is
// older than the one a Wake look applied to session id (under obsMu): the
// pass leaves the session alone. A look as old or older is forgotten.
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
}
