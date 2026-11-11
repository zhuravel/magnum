package agents

// The agents' own hooks give each session a state of their own, in shadow
// beside herdr's status (spec 09, the first step of a pane backend beside
// herdr): ready at a SessionStart, working at a UserPromptSubmit, blocked
// at a PermissionRequest, idle at a Stop, a StopFailure or a Codex
// Interrupt, or when the session's own transcript (Claude) or rollout
// (Codex) shows its turn ended after the hooks said working. Each observe
// pass compares it with herdr's status, which stays the truth: no
// decision, wait or retry reads the hooks' state. A disagreement that
// outlasts the observe interval writes one agent.hook_state_differs event
// per session and pair of states, and every reading of a pass counts, per
// agent kind and hour, as agreed or not (store.KVHookAgreement), which
// `magnum status` sums over the last 24 hours (ReadHookAgreement): the
// measure that decides whether the hooks can stand in for herdr's status.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// HookState is a session's state as its agent's own hooks tell it.
type HookState string

// The hooks' states ("" while the hooks said nothing, or after the
// session's SessionEnd).
const (
	HookReady   HookState = "ready"   // a SessionStart: the agent came up and waits
	HookWorking HookState = "working" // a UserPromptSubmit: a turn began
	HookBlocked HookState = "blocked" // a PermissionRequest: the agent waits on an approval
	HookIdle    HookState = "idle"    // the turn ended: a Stop, a StopFailure, an Interrupt, the transcript or the rollout
)

// EventHookStateDiffers is recorded (info, on the PR) once per session and
// pair of states, when herdr's status and the hooks' state agree again
// after they disagreed for longer than the observe interval
// (hookObserveEvery), or the pass can no longer compare them: data role,
// session, kind, hook, herdr, by (what last moved the hooks' state: an
// event's name, "transcript" or "rollout") and seconds, from the later of
// the two changes to the last pass that saw them differ. No PR text.
const EventHookStateDiffers = "agent.hook_state_differs"

// The tally's bounds: the passes' counts reach the registry at most once
// per hookTallyEvery, which keeps each hour's for hookTallyKeep.
const (
	hookTallyEvery = time.Minute
	hookTallyKeep  = 25 * time.Hour
)

// hookPair is a pair of states that disagree: the hooks' and herdr's.
type hookPair struct {
	hook  HookState
	herdr herdr.Status
}

// hookDiff is a disagreement the passes see: since the later of the two
// changes that made it, last seen by the pass at seen; by is what had moved
// the hooks' state to it (hookShadow.by).
type hookDiff struct {
	pair        hookPair
	since, seen time.Time
	by          string
}

// hookShadow is what the hooks said of one session (under mu): its kind,
// its state, when and by what the state was last moved, the disagreement
// with herdr under way, and the pairs an event was written for.
type hookShadow struct {
	kind  string
	state HookState
	at    time.Time
	by    string
	diff  *hookDiff
	told  map[hookPair]bool
}

// hookTallyKey is one kind's hour (its start, UTC) in the tally.
type hookTallyKey struct {
	kind string
	hour time.Time
}

// HookAgreement is how often the hooks' state of a kind's sessions agreed
// with herdr's status at the observe passes (ReadHookAgreement).
type HookAgreement struct {
	Kind     string `json:"kind"`
	Agreed   int    `json:"agreed"`
	Differed int    `json:"differed"`
}

// hookTallyRow is one kind's hour in store.KVHookAgreement.
type hookTallyRow struct {
	Hour time.Time `json:"hour"`
	HookAgreement
}

// hookStateAfter is the state event ev moves a session's hooks' state cur
// to, false when it moves nothing: a SessionStart is ready (but a
// compaction's, which leaves a known state as it was), a UserPromptSubmit
// working, a PermissionRequest blocked (a subagent's too: the pane shows
// it), a Stop, StopFailure or Interrupt idle, and a SessionEnd no state.
// A subagent's (agent_id) or a Codex helper's (CODEX_THREAD_ID not its
// session's) prompt or turn end is not the session's and moves nothing.
func hookStateAfter(cur HookState, ev agenthooks.Event) (HookState, bool) {
	own := ev.AgentID == "" && (ev.CodexThread == "" || ev.CodexThread == ev.SessionID)
	switch ev.Name {
	case agenthooks.NameSessionStart:
		if ev.Source == "compact" && cur != "" {
			return cur, false
		}
		return HookReady, true
	case agenthooks.NameUserPromptSubmit:
		return HookWorking, own
	case agenthooks.NamePermissionRequest:
		return HookBlocked, true
	case agenthooks.NameStop, agenthooks.NameStopFailure, agenthooks.NameInterrupt:
		return HookIdle, own
	case agenthooks.NameSessionEnd:
		return "", true
	}
	return cur, false
}

// noteHookEvent moves session s's hooks' state by its hook event ev (Wake).
func (m *Manager) noteHookEvent(s store.Session, ev agenthooks.Event) {
	kind, at := m.sessionKind(s), m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.hookStates[s.ID]
	if h == nil {
		h = &hookShadow{told: map[hookPair]bool{}}
		m.hookStates[s.ID] = h
	}
	h.kind = kind
	if state, ok := hookStateAfter(h.state, ev); ok {
		h.state, h.at, h.by = state, at, ev.Name
	}
}

// hookStateOf is session id's hooks' state ("" when they said nothing).
func (m *Manager) hookStateOf(id int64) HookState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h := m.hookStates[id]; h != nil {
		return h.state
	}
	return ""
}

// herdrHookState is herdr's status as the hooks' states compare with it:
// idle and done are idle, unknown and none are nothing to compare.
func herdrHookState(st herdr.Status) HookState {
	switch st {
	case herdr.StatusIdle, herdr.StatusDone:
		return HookIdle
	case herdr.StatusWorking:
		return HookWorking
	case herdr.StatusBlocked:
		return HookBlocked
	}
	return ""
}

// agreesWith reports whether the hooks' state s says what herdr's state
// (herdrHookState) does: ready is idle.
func (s HookState) agreesWith(h HookState) bool {
	if s == HookReady {
		s = HookIdle
	}
	return s == h
}

// shadowHookState compares, for a pass's observation o read from a
// snapshot captured at at, the hooks' state of o's session with herdr's
// status (o.Status), under the observe lock. While the hooks say working
// and herdr idle, the session's transcript or rollout may settle it
// (hookTurnEnded). A reading of both counts in the tally; a disagreement
// that ends writes its event (hookDiffEnded).
func (m *Manager) shadowHookState(ctx context.Context, o Observation, at time.Time) {
	s := o.Session
	m.mu.Lock()
	h := m.hookStates[s.ID]
	var state HookState
	var since time.Time
	if h != nil {
		state, since = h.state, h.at
	}
	m.mu.Unlock()
	if h == nil {
		return
	}
	herdrState := herdrHookState(o.Status)
	if state == HookWorking && herdrState == HookIdle {
		if by := m.hookTurnEnded(s, since); by != "" {
			m.mu.Lock()
			if h.state == HookWorking && h.at.Equal(since) { // no event moved it meanwhile
				h.state, h.at, h.by = HookIdle, at, by
			}
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	ended, kind := m.compareHookState(h, o, herdrState, at), h.kind
	if h.state != "" && herdrState != "" {
		agreed := h.state.agreesWith(herdrState)
		key := hookTallyKey{kind: kind, hour: at.UTC().Truncate(time.Hour)}
		c := m.hookTallies[key]
		if agreed {
			c.Agreed++
		} else {
			c.Differed++
		}
		c.Kind = kind
		m.hookTallies[key] = c
	}
	m.mu.Unlock()
	if ended != nil {
		m.hookDiffEnded(ctx, s, kind, *ended)
	}
}

// compareHookState moves h's disagreement by a reading (under mu): herdr's
// status o.Status (herdrState as the hooks name it) at at. A reading that
// agrees, differs in another pair or cannot be compared ends the one under
// way, which it returns when it outlasted the observe interval and its pair
// of the session was not written yet; a differing reading starts one or
// marks it seen.
func (m *Manager) compareHookState(h *hookShadow, o Observation, herdrState HookState, at time.Time) *hookDiff {
	var ended *hookDiff
	end := func() {
		if d := h.diff; d != nil && d.seen.Sub(d.since) > m.hookObserveEvery() && !h.told[d.pair] {
			h.told[d.pair] = true
			ended = d
		}
		h.diff = nil
	}
	if h.state == "" || herdrState == "" || h.state.agreesWith(herdrState) {
		end()
		return ended
	}
	pair := hookPair{hook: h.state, herdr: o.Status}
	if h.diff != nil && h.diff.pair != pair {
		end()
	}
	if h.diff == nil {
		since := h.at
		if t := o.Session.AgentStatusAt; t != nil && t.After(since) {
			since = *t // herdr's status changed last
		}
		if since.After(at) {
			since = at // a change after the snapshot was taken
		}
		h.diff = &hookDiff{pair: pair, since: since, by: h.by}
	}
	h.diff.seen = at
	return ended
}

// hookDiffEnded writes the agent.hook_state_differs event of session s
// (of kind) for disagreement d.
func (m *Manager) hookDiffEnded(ctx context.Context, s store.Session, kind string, d hookDiff) {
	took := d.seen.Sub(d.since).Round(time.Second)
	msg := fmt.Sprintf("%s (session %d): its hooks said %s while herdr said %s, for %s (herdr's status stays the truth)",
		s.Role, s.ID, d.pair.hook, d.pair.herdr, took)
	m.event(ctx, m.prSubject(ctx, s.PRID), "info", EventHookStateDiffers, msg, map[string]any{"role": s.Role, "session": s.ID,
		"kind": kind, "hook": string(d.pair.hook), "herdr": string(d.pair.herdr), "by": d.by, "seconds": int(took.Seconds())})
}

// hookTurnEnded says what shows that session s's turn ended after since:
// "transcript" for a Claude turn_duration of the main thread stamped after
// since, "rollout" for a Codex task_complete or turn_aborted stamped at or
// after it; "" when neither does, the session has no id yet, or the file
// cannot be found or read.
func (m *Manager) hookTurnEnded(s store.Session, since time.Time) string {
	sid := store.Deref(s.SessionID)
	if sid == "" {
		return ""
	}
	switch m.sessionKind(s) {
	case KindClaude:
		if path, err := m.transcriptPath(s, sid); err == nil {
			if ended, err := claudeTurnEndedAfter(path, since); err == nil && ended {
				return "transcript"
			}
		}
	case KindCodex:
		if path, err := m.rolloutPath(s, sid); err == nil {
			if ended, err := turnEndedSince(path, since); err == nil && ended {
				return "rollout"
			}
		}
	}
	return ""
}

// hookObserveEvery is the observe interval a disagreement must outlast to
// be written: [daemon] observe_interval, else poll_interval (with the
// observe clock off the tick observes alone), else 5 s.
func (m *Manager) hookObserveEvery() time.Duration {
	if c := m.d.Config; c != nil {
		if d := c.Daemon.ObserveInterval.Duration; d > 0 {
			return d
		}
		if d := c.Daemon.PollInterval.Duration; d > 0 {
			return d
		}
	}
	return 5 * time.Second
}

// forgetHookStates forgets the hooks' state of every session no longer
// live, writing a disagreement still under way that outlasted the observe
// interval (its last pass's reading counts).
func (m *Manager) forgetHookStates(ctx context.Context, live []store.Session) {
	keep := make(map[int64]bool, len(live))
	for _, s := range live {
		keep[s.ID] = true
	}
	type gone struct {
		s    store.Session
		kind string
		d    hookDiff
	}
	var ended []gone
	m.mu.Lock()
	for id, h := range m.hookStates {
		if keep[id] {
			continue
		}
		if d := h.diff; d != nil && d.seen.Sub(d.since) > m.hookObserveEvery() && !h.told[d.pair] {
			ended = append(ended, gone{store.Session{ID: id}, h.kind, *d})
		}
		delete(m.hookStates, id)
	}
	m.mu.Unlock()
	for _, g := range ended {
		s, err := m.d.Store.SessionByID(ctx, g.s.ID)
		if err != nil {
			continue
		}
		m.hookDiffEnded(ctx, s, g.kind, g.d)
	}
}

// flushHookTally adds the passes' tally to the registry's
// (store.KVHookAgreement) at most once per hookTallyEvery, under the
// observe lock; counts a failed write kept are added at the next flush.
func (m *Manager) flushHookTally(ctx context.Context, now time.Time) {
	m.mu.Lock()
	if len(m.hookTallies) == 0 || !m.hookFlushed.IsZero() && now.Sub(m.hookFlushed) < hookTallyEvery {
		m.mu.Unlock()
		return
	}
	pending := m.hookTallies
	m.hookTallies, m.hookFlushed = map[hookTallyKey]HookAgreement{}, now
	m.mu.Unlock()
	if err := addHookTally(ctx, m.d.Store, pending, now); err != nil {
		m.logErr(ctx, err, "agents: the hooks' state tally: %v", err)
		m.mu.Lock()
		for k, c := range pending {
			n := m.hookTallies[k]
			n.Kind, n.Agreed, n.Differed = c.Kind, n.Agreed+c.Agreed, n.Differed+c.Differed
			m.hookTallies[k] = n
		}
		m.mu.Unlock()
	}
}

// addHookTally adds pending to the tally store.KVHookAgreement holds,
// dropping the hours older than hookTallyKeep, by compare and set: another
// writer between the read and the write has it read again.
func addHookTally(ctx context.Context, st *store.Store, pending map[hookTallyKey]HookAgreement, now time.Time) error {
	for range 3 {
		old, ok, err := st.GetKV(ctx, store.KVHookAgreement)
		if err != nil {
			return err
		}
		sums := map[hookTallyKey]HookAgreement{}
		for _, r := range readHookTally(old) {
			sums[hookTallyKey{kind: r.Kind, hour: r.Hour}] = r.HookAgreement
		}
		for k, c := range pending {
			s := sums[k]
			s.Kind, s.Agreed, s.Differed = k.kind, s.Agreed+c.Agreed, s.Differed+c.Differed
			sums[k] = s
		}
		var rows []hookTallyRow
		for _, k := range slices.SortedFunc(maps.Keys(sums), func(a, b hookTallyKey) int {
			return cmp.Or(a.hour.Compare(b.hour), cmp.Compare(a.kind, b.kind))
		}) {
			if now.Sub(k.hour) <= hookTallyKeep {
				rows = append(rows, hookTallyRow{Hour: k.hour, HookAgreement: sums[k]})
			}
		}
		b, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		if !ok {
			return st.SetKV(ctx, store.KVHookAgreement, string(b))
		}
		if done, err := st.SetKVIf(ctx, store.KVHookAgreement, old, string(b)); err != nil || done {
			return err
		}
	}
	return errors.New("the tally changed under three writes in a row")
}

// readHookTally reads store.KVHookAgreement's rows; a value that cannot be
// read holds none.
func readHookTally(v string) []hookTallyRow {
	var rows []hookTallyRow
	if v == "" || json.Unmarshal([]byte(v), &rows) != nil {
		return nil
	}
	return rows
}

// ReadHookAgreement is how often, per agent kind (by name), the hooks'
// state of a session agreed with herdr's status at the observe passes of
// the current hour and the 23 before it, as now reads them; kinds without
// a reading are left out.
func ReadHookAgreement(ctx context.Context, st *store.Store, now time.Time) ([]HookAgreement, error) {
	v, _, err := st.GetKV(ctx, store.KVHookAgreement)
	if err != nil {
		return nil, err
	}
	from := now.UTC().Truncate(time.Hour).Add(-23 * time.Hour)
	sums := map[string]HookAgreement{}
	for _, r := range readHookTally(v) {
		if r.Hour.Before(from) || r.Agreed+r.Differed <= 0 {
			continue
		}
		s := sums[r.Kind]
		s.Kind, s.Agreed, s.Differed = r.Kind, s.Agreed+r.Agreed, s.Differed+r.Differed
		sums[r.Kind] = s
	}
	out := make([]HookAgreement, 0, len(sums))
	for _, k := range slices.Sorted(maps.Keys(sums)) {
		out = append(out, sums[k])
	}
	return out, nil
}
