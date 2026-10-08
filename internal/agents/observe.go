package agents

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// ObservationKind is what one tick noticed about a session.
type ObservationKind string

// Observation kinds; "" means nothing to act on.
const (
	// ObsCompleted: a submitted/working run's agent was idle|done on
	// CompletionIdleTicks consecutive ticks. The run is now ended; verify it
	// (reports on disk, the judge's review on GitHub).
	ObsCompleted ObservationKind = "completed"
	// ObsHumanActive: the agent works with no magnum run in flight; prs.human_active_at
	// was set to now, so Submit refuses for daemon.human_cooldown.
	ObsHumanActive ObservationKind = "human_active"
	// ObsBlocked: the agent waits on a dialog magnum leaves to the human (a
	// kind with on_permission_prompt "wait", a dialog that is not a
	// recognised permission prompt, a run past MaxPromptDenies): tell the
	// human.
	ObsBlocked ObservationKind = "blocked"
	// ObsPromptDenied: the agent was blocked on a permission prompt during
	// one of magnum's runs and magnum answered No (this tick, or the tick
	// before while the screen catches up; see answerPermission); the human
	// is not needed.
	ObsPromptDenied ObservationKind = "prompt_denied"
	// ObsDenyContinued: the agent went idle after such a deny, during the
	// same run, and was sent its kind's after_deny_prompt (see
	// continueAfterDeny); the run goes on.
	ObsDenyContinued ObservationKind = "deny_continued"
	// ObsLost: the pane or the agent is gone; the session is now lost (its
	// session_id kept for ResumeID). Run carries the run that was in flight.
	ObsLost ObservationKind = "lost"
)

// EventHumanActive is recorded on the PR when an ObsHumanActive begins its
// cooldown (Observation.CooldownUntil; data: role, until).
const EventHumanActive = "agent.human_active"

// Observation is the result of one tick for one starting/live session.
type Observation struct {
	Kind    ObservationKind
	Role    Role
	PRID    int64
	Status  herdr.Status  // agent status this tick ("" for shell panes and missing agents)
	Session store.Session // row after this tick's update
	Run     *store.Run    // newest run in flight for the session (completed run for ObsCompleted)
	// Background is the work an idle claude agent started in the background
	// during Run and left running, as its transcript shows (see
	// backgroundWait); while there is any, the run does not end.
	Background int
	// CooldownUntil is, for an ObsHumanActive that began the PR's cooldown
	// (its human_active_at was unset, or daemon.human_cooldown had passed
	// since), when that cooldown ends; zero when the tick only extended a
	// cooldown already running, and when human_cooldown is 0 (none).
	CooldownUntil time.Time
}

// LostGrace keeps an observe tick from judging a session lost from a
// snapshot that may predate its agent: a session whose started_at,
// agent_status_at or live mark (StartAgent, a rebind in Submit) is after, or
// within LostGrace before, the snapshot's capture time is left for the next
// tick. A round once failed on a judge marked lost in the second it started:
// the tick's snapshot was taken before StartAgent made the session live.
const LostGrace = 15 * time.Second

// Observe takes one herdr snapshot and runs ObserveSnapshotAt with the time
// it was taken. The daemon calls ObserveSnapshotAt itself, once per tick,
// with the snapshot that tick also counts working agents from (polling is
// the completion signal).
func (m *Manager) Observe(ctx context.Context) ([]Observation, error) {
	at := m.now()
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("agents: observe: %w", err)
	}
	return m.ObserveSnapshotAt(ctx, snap, at)
}

// ObserveSnapshot is ObserveSnapshotAt with the capture time now (a
// snapshot of unknown age).
func (m *Manager) ObserveSnapshot(ctx context.Context, snap herdr.Snapshot) ([]Observation, error) {
	return m.ObserveSnapshotAt(ctx, snap, m.now())
}

// ObserveSnapshotAt updates every starting/live session from snap, taken at
// capturedAt (read the clock before herdr's snapshot call), and returns one
// Observation per session:
//
//   - agent found (by name, else by pane): status/status_at stored; idle|done
//     increments idle_ticks, working|blocked resets it; a newly reported
//     agent_session.value becomes session_id; a starting session becomes live.
//     working + a submitted run -> run working (working_seen_at).
//     idle_ticks >= CompletionIdleTicks + a submitted/working run -> run ended,
//     ObsCompleted. working with no pending/submitted/working run (outside a
//     short grace after start/prompt) -> ObsHumanActive (CooldownUntil set
//     when it begins the PR's cooldown), unless the agent is a claude agent
//     whose transcript shows no prompt typed into the pane after magnum's
//     last prompt to it (magnum's turn going on after esc cut it short, a
//     turn a task notification began; see humanTurn), or a judge still
//     finishing the turn of a run the round settled on its result file (see
//     turnTail). blocked -> ObsBlocked,
//     or ObsPromptDenied when a pending/submitted/working run is in flight,
//     the kind's on_permission_prompt is "deny" and the screen shows a
//     permission prompt, which is answered No (see answerPermission; never
//     without a run in flight, where the human may be driving the agent, so
//     an agent that resumes working after a deny is never human_active).
//     idle after such a deny, with that run still in flight and the agent
//     not seen working since -> the kind's after_deny_prompt is sent (see
//     continueAfterDeny), ObsDenyContinued, and the run does not end.
//     A claude agent idle with a submitted/working run whose transcript
//     shows background work started during the run still running, or a
//     task notification not answered yet, counts as working for completion
//     (idle_ticks 0, Background set; see backgroundWait), until the
//     pipeline tells it (Tell) to stop waiting for that work.
//     An agent of a kind named by a rename command (codex), working on a
//     submitted/working run, whose terminal title lacks Title gets that
//     command (`/rename <Title>`) typed into its pane (at most once per call,
//     TitleAttempts per session row; see nameAgent).
//   - live agent session without its agent, or any session whose pane is gone
//     -> session lost, ObsLost. A starting session (agent not started yet)
//     with its pane still present is left alone, and so is a session that
//     started or became live too close to capturedAt (LostGrace).
func (m *Manager) ObserveSnapshotAt(ctx context.Context, snap herdr.Snapshot, capturedAt time.Time) ([]Observation, error) {
	sessions, err := m.d.Store.LiveSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("agents: observe: %w", err)
	}
	m.nextObserveTick(sessions)
	active, err := m.d.Store.ActiveRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("agents: observe: %w", err)
	}
	inflight := map[int64][]store.Run{} // session id -> pending/submitted/working runs, oldest first
	for _, r := range active {
		if r.SessionID != nil && r.State != store.RunEnded {
			inflight[*r.SessionID] = append(inflight[*r.SessionID], r)
		}
	}
	panes := paneIndex(snap)
	now := m.now()

	var out []Observation
	var errs []error
	for _, s := range sessions {
		o, err := m.observeOne(ctx, s, snap, panes, inflight[s.ID], now, capturedAt)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, o)
	}
	return out, errors.Join(errs...)
}

func (m *Manager) observeOne(ctx context.Context, s store.Session, snap herdr.Snapshot, panes map[string]herdr.Pane,
	runs []store.Run, now, capturedAt time.Time) (Observation, error) {
	role := Role(s.Role)
	o := Observation{Role: role, PRID: s.PRID, Session: s}
	if len(runs) > 0 {
		r := runs[len(runs)-1]
		o.Run = &r
	}
	paneID := store.Deref(s.HerdrPaneID)
	_, paneOK := panes[paneID]

	lost := func() (Observation, error) {
		if m.liveSince(s).After(capturedAt.Add(-LostGrace)) {
			return o, nil // the snapshot may predate the agent: judge it next tick
		}
		if err := m.markLost(ctx, s); err != nil {
			return o, err
		}
		o.Kind = ObsLost
		o.Session.State = store.SessionLost
		return o, nil
	}

	if !m.isAgentSession(s) {
		if paneID != "" && !paneOK {
			return lost()
		}
		return o, nil
	}
	a, found := findAgent(snap, s)
	if !found {
		if s.State == store.SessionLive || (paneID != "" && !paneOK) {
			return lost()
		}
		return o, nil
	}

	o.Status = a.AgentStatus
	idle := a.AgentStatus == herdr.StatusIdle || a.AgentStatus == herdr.StatusDone
	busy := a.AgentStatus == herdr.StatusWorking || a.AgentStatus == herdr.StatusBlocked
	sid := store.Deref(s.SessionID) // the agent's own session id (a claude agent's transcript)
	if a.AgentSession != nil && a.AgentSession.Value != "" {
		sid = a.AgentSession.Value
	}
	held := false // idle, but background work keeps the turn going
	if idle && o.Run != nil && (o.Run.State == store.RunSubmitted || o.Run.State == store.RunWorking) {
		o.Background, held = m.backgroundWait(ctx, s, sid, *o.Run)
	}
	err := m.d.Store.TransitionSession(ctx, s.ID, liveStates, store.SessionLive, func(u *store.SessionUpdate) {
		if a.AgentStatus != "" && string(a.AgentStatus) != store.Deref(s.AgentStatus) {
			u.Set("agent_status", string(a.AgentStatus))
			u.Set("agent_status_at", now)
		}
		switch {
		case busy || held:
			u.Set("idle_ticks", 0)
		case idle:
			u.Inc("idle_ticks", 1)
		}
		if a.PaneID != "" && a.PaneID != paneID {
			u.Set("herdr_pane_id", a.PaneID)
		}
		if a.AgentSession != nil && a.AgentSession.Value != "" && a.AgentSession.Value != store.Deref(s.SessionID) {
			u.Set("session_id", a.AgentSession.Value)
		}
	})
	if errors.Is(err, store.ErrConflict) {
		return o, nil // parked/closed meanwhile
	}
	if err != nil {
		return o, fmt.Errorf("agents: observe session %d: %w", s.ID, err)
	}
	if fresh, err := m.d.Store.SessionByID(ctx, s.ID); err == nil {
		s, o.Session = fresh, fresh
	}

	ref := paneRef{name: store.Deref(s.AgentName), pane: a.PaneID}
	if ref.pane == "" {
		ref.pane = paneID
	}
	switch {
	case a.AgentStatus == herdr.StatusWorking:
		m.denyResumed(s.ID)
		turn := false // magnum's prompt is what the agent works on
		for _, r := range runs {
			if r.State == store.RunSubmitted {
				_ = m.d.Store.TransitionRun(ctx, r.ID, []string{store.RunSubmitted}, store.RunWorking, func(u *store.RunUpdate) {
					u.Set("working_seen_at", now)
				})
			}
			turn = turn || r.State == store.RunSubmitted || r.State == store.RunWorking
		}
		if turn {
			pane := a.PaneID
			if pane == "" {
				pane = paneID
			}
			m.nameAgent(ctx, s, pane, terminalTitle(a, panes[pane]))
		}
		if len(runs) == 0 && !recent(s.StartedAt, now) && (s.LastPromptAt == nil || !recent(*s.LastPromptAt, now)) &&
			m.humanTurn(s, sid) && !m.turnTail(ctx, s) {
			until, err := m.humanActive(ctx, s.PRID, now)
			if err != nil {
				return o, fmt.Errorf("agents: observe pr %d human activity: %w", s.PRID, err)
			}
			o.Kind, o.CooldownUntil = ObsHumanActive, until
		}
	case a.AgentStatus == herdr.StatusBlocked && m.isSwitching(s.ID):
		// magnum's own model-switch confirmation (SwitchModel answers it)
	case a.AgentStatus == herdr.StatusBlocked:
		o.Kind = ObsBlocked
		if o.Run != nil {
			o.Kind = m.answerPermission(ctx, s, *o.Run, ref)
		}
	case idle && o.Run != nil && m.continueAfterDeny(ctx, s, *o.Run, ref):
		o.Kind = ObsDenyContinued
		if fresh, err := m.d.Store.SessionByID(ctx, s.ID); err == nil {
			o.Session = fresh
		}
	case idle && s.IdleTicks >= CompletionIdleTicks:
		var done *store.Run
		for _, r := range runs {
			if r.State != store.RunSubmitted && r.State != store.RunWorking {
				continue
			}
			err := m.d.Store.TransitionRun(ctx, r.ID, []string{store.RunSubmitted, store.RunWorking}, store.RunEnded, func(u *store.RunUpdate) {
				u.Set("ended_at", now)
			})
			if err == nil {
				if fresh, err := m.d.Store.RunByID(ctx, r.ID); err == nil {
					done = &fresh
				}
			}
		}
		if done != nil {
			o.Kind, o.Run = ObsCompleted, done
		}
	}
	return o, nil
}

func recent(t, now time.Time) bool { return now.Sub(t) < humanGrace }

// humanActive sets PR prID's human_active_at to now, which holds prompts to
// the PR for daemon.human_cooldown (Submit, RunShell), and returns when the
// cooldown this begins ends: zero when the cooldown of the PR's last
// human_active_at still ran (now only extends it) or human_cooldown is 0.
func (m *Manager) humanActive(ctx context.Context, prID int64, now time.Time) (time.Time, error) {
	pr, err := m.d.Store.PRByID(ctx, prID)
	if err != nil {
		return time.Time{}, err
	}
	if err := m.d.Store.UpdatePR(ctx, prID, func(u *store.PRUpdate) { u.Set("human_active_at", now) }); err != nil {
		return time.Time{}, err
	}
	cfg := m.d.Config.Daemon
	if now.Before(cfg.CooldownUntil(pr.HumanActiveAt)) {
		return time.Time{}, nil
	}
	return cfg.CooldownUntil(&now), nil
}

// turnTail reports whether judge session s (its row after this tick's
// update) shows its agent still finishing the turn of its last run: the run
// ended while the agent was working (the round went on once the judge's
// result file held a final status, so the agent may still print its last
// message) and the agent has not been seen anything but working since, as
// agent_status_at, which moves only when the status changes, says. That
// work is the run's: it is never a human's, and Submit waits for it to end
// (awaitTail). A reviewer role keeps the plain rule.
func (m *Manager) turnTail(ctx context.Context, s store.Session) bool {
	if herdr.Status(store.Deref(s.AgentStatus)) != herdr.StatusWorking || s.AgentStatusAt == nil {
		return false
	}
	if spec, ok := m.roleSpec(Role(s.Role)); !ok || !spec.Judge {
		return false
	}
	r, err := m.d.Store.LastEndedRun(ctx, s.PRID, s.ID)
	return err == nil && r.EndedAt != nil && !s.AgentStatusAt.After(*r.EndedAt)
}

// liveSince is the latest sign that session s is (newly) live: started_at,
// agent_status_at, or when this Manager marked it live.
func (m *Manager) liveSince(s store.Session) time.Time {
	t := s.StartedAt
	if s.AgentStatusAt != nil && s.AgentStatusAt.After(t) {
		t = *s.AgentStatusAt
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if at, ok := m.liveAt[s.ID]; ok && at.After(t) {
		t = at
	}
	return t
}

// ReadRecent returns the last lines of a session's pane output (unwrapped
// text): agent.read by agent name, falling back to pane.read (shell panes,
// agents that exited).
func (m *Manager) ReadRecent(ctx context.Context, s store.Session, lines int) (string, error) {
	opts := herdr.ReadOptions{Source: herdr.SourceRecentUnwrapped, Lines: lines}
	pane := store.Deref(s.HerdrPaneID)
	var agentErr error
	if name := store.Deref(s.AgentName); name != "" && m.isAgentSession(s) {
		r, err := m.d.Herdr.AgentRead(ctx, name, opts)
		if err == nil {
			return r.Text, nil
		}
		agentErr = err
	}
	if pane == "" {
		return "", fmt.Errorf("agents: read session %d: no pane: %w", s.ID, agentErr)
	}
	r, err := m.d.Herdr.PaneRead(ctx, pane, opts)
	if err != nil {
		return "", fmt.Errorf("agents: read pane %s: %w", pane, err)
	}
	return r.Text, nil
}

// HealthLines is how much pane output CheckHealth classifies.
const HealthLines = 80

// CheckHealth classifies the session's recent pane output with the health
// patterns of its kind ([kinds.<kind>] health_patterns; a shell role's Tool,
// the defaults when it has none) and the manager's clock.
func (m *Manager) CheckHealth(ctx context.Context, s store.Session) (Health, error) {
	text, err := m.ReadRecent(ctx, s, HealthLines)
	if err != nil {
		return Health{}, err
	}
	return classify(m.healthRules(m.healthKind(s)), text, m.now()), nil
}
