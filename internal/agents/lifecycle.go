package agents

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Quit stops a session's agent the way a human would: ctrl+c, 1 s, ctrl+c,
// then waits up to QuitIdleTimeout for the pane to be an idle shell again
// (ErrBusy otherwise, session unchanged). The session id is captured first
// (RecordSessionID); the session becomes parked when it is resumable (an
// agent session with a session id whose kind reports ids), else closed. A
// pane or agent that is already gone counts as quit. For a shell role's pane
// this interrupts the running command.
func (m *Manager) Quit(ctx context.Context, s store.Session) error {
	if sid, err := m.RecordSessionID(ctx, s); err == nil && sid != "" {
		s.SessionID = &sid
	}
	pane := store.Deref(s.HerdrPaneID)
	send := func() error {
		if pane != "" {
			return m.d.Herdr.PaneSendKeys(ctx, pane, "ctrl+c")
		}
		return m.d.Herdr.AgentSendKeys(ctx, target(s), "ctrl+c")
	}
	gone := false
	for i := 0; i < 2 && !gone; i++ {
		if i > 0 {
			if err := m.sleep(ctx, time.Second); err != nil {
				return fmt.Errorf("agents: quit session %d: %w", s.ID, err)
			}
		}
		if err := send(); err != nil {
			if !isGone(err) {
				return fmt.Errorf("agents: quit session %d: %w", s.ID, err)
			}
			gone = true
		}
	}
	if !gone && pane != "" {
		if _, err := m.d.Herdr.WaitIdleShell(ctx, pane, QuitIdleTimeout); err != nil && !isGone(err) {
			if herdr.IsTimeout(err) {
				return fmt.Errorf("agents: quit session %d: %w: %w", s.ID, ErrBusy, err)
			}
			return fmt.Errorf("agents: quit session %d: %w", s.ID, err)
		}
	}
	return m.retire(ctx, s)
}

// retire parks a resumable agent session (see resumable) and closes
// everything else.
func (m *Manager) retire(ctx context.Context, s store.Session) error {
	to := store.SessionClosed
	if m.resumable(s) {
		to = store.SessionParked
	}
	now := m.now()
	err := m.d.Store.TransitionSession(ctx, s.ID, []string{store.SessionStarting, store.SessionLive, store.SessionLost}, to,
		func(u *store.SessionUpdate) {
			u.Set("idle_ticks", 0)
			if to == store.SessionClosed {
				u.Set("closed_at", now)
			}
		})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("agents: %s session %d: %w", to, s.ID, err)
	}
	return nil
}

// Park releases a PR's panes while keeping its conversations resumable.
// It refuses with ErrBusy while a run of the PR is pending/submitted/working,
// an agent is working or blocked, a shell role's pane runs a command, or any
// pane of a workspace it would close runs something other than an idle
// shell or an idle agent (a lost session's pane where the user started a
// build, say). Session ids visible in herdr are recorded first. A workspace
// of the PR's live or lost sessions that holds only the PR's own panes is
// closed; one with foreign panes (the user split it) stays, and only the
// PR's live agents are quit. Live sessions with a session_id become parked,
// the rest closed (see Quit); lost sessions stay lost. No live or lost
// sessions: no-op.
func (m *Manager) Park(ctx context.Context, pr store.PR) error {
	sessions, err := m.d.Store.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return fmt.Errorf("agents: park pr %d: %w", pr.Number, err)
	}
	var live, lost []store.Session
	ours := map[string]bool{}
	for _, s := range sessions {
		if p := store.Deref(s.HerdrPaneID); p != "" {
			ours[p] = true
		}
		switch {
		case isLive(s):
			live = append(live, s)
		case s.State == store.SessionLost:
			lost = append(lost, s)
		}
	}
	if len(live) == 0 && len(lost) == 0 {
		return nil
	}
	runs, err := m.d.Store.ActiveRuns(ctx)
	if err != nil {
		return fmt.Errorf("agents: park pr %d: %w", pr.Number, err)
	}
	for _, r := range runs {
		if r.PRID == pr.ID && r.State != store.RunEnded {
			return fmt.Errorf("agents: park pr %d: run %s is %s: %w", pr.Number, r.ID, r.State, ErrBusy)
		}
	}
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("agents: park pr %d: %w", pr.Number, err)
	}
	if err := m.parkCheckLive(ctx, pr, snap, live); err != nil {
		return err
	}

	// Workspaces of live and lost sessions: one holding only the PR's panes
	// is closed, once none of its panes is busy.
	byWS := map[string][]store.Session{}
	for _, s := range append(append([]store.Session(nil), live...), lost...) {
		if ws := store.Deref(s.HerdrWorkspaceID); ws != "" && hasWorkspace(snap, ws) {
			byWS[ws] = append(byWS[ws], s)
		}
	}
	wsIDs := slices.Sorted(maps.Keys(byWS))
	closing := map[string]bool{}
	for _, ws := range wsIDs {
		closing[ws] = !slices.ContainsFunc(snap.Panes, func(p herdr.Pane) bool { return p.WorkspaceID == ws && !ours[p.ID] })
	}
	for _, p := range snap.Panes {
		if closing[p.WorkspaceID] {
			if err := m.parkCheckPane(ctx, pr, snap, p); err != nil {
				return err
			}
		}
	}
	for _, ws := range wsIDs {
		if closing[ws] {
			if err := m.d.Herdr.WorkspaceClose(ctx, ws); err != nil && !isGone(err) {
				return fmt.Errorf("agents: park pr %d: close workspace %s: %w", pr.Number, ws, err)
			}
			continue
		}
		for _, s := range byWS[ws] {
			if _, ok := findAgent(snap, s); ok && isLive(s) && m.isAgentSession(s) {
				if err := m.Quit(ctx, s); err != nil {
					return fmt.Errorf("agents: park pr %d: %w", pr.Number, err)
				}
			}
		}
	}
	for _, s := range live {
		if err := m.retire(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// parkCheckLive refuses (ErrBusy) while a live session's agent works or is
// blocked, or a live shell role's pane runs a command, and records the
// session ids herdr shows (live[i].SessionID too).
func (m *Manager) parkCheckLive(ctx context.Context, pr store.PR, snap herdr.Snapshot, live []store.Session) error {
	panes := paneIndex(snap)
	for i, s := range live {
		pane := store.Deref(s.HerdrPaneID)
		if m.isAgentSession(s) {
			a, ok := findAgent(snap, s)
			if !ok {
				continue // its surviving pane, if any, is checked with the workspace
			}
			if a.AgentStatus == herdr.StatusWorking || a.AgentStatus == herdr.StatusBlocked {
				return fmt.Errorf("agents: park pr %d: %s is %s: %w", pr.Number, Role(s.Role).Label(), a.AgentStatus, ErrBusy)
			}
			if a.AgentSession != nil && a.AgentSession.Value != "" {
				if err := m.setSessionID(ctx, s, a.AgentSession.Value); err != nil {
					return err
				}
				v := a.AgentSession.Value
				live[i].SessionID = &v
			}
			continue
		}
		if p, ok := panes[pane]; ok {
			if err := m.parkCheckPane(ctx, pr, snap, p); err != nil {
				return fmt.Errorf("%w (%s pane)", err, Role(s.Role).Label())
			}
		}
	}
	return nil
}

// parkCheckPane refuses (ErrBusy) a pane about to be closed or interrupted
// unless it holds an idle agent or an idle shell.
func (m *Manager) parkCheckPane(ctx context.Context, pr store.PR, snap herdr.Snapshot, p herdr.Pane) error {
	for _, a := range snap.Agents {
		if a.PaneID != p.ID {
			continue
		}
		if a.AgentStatus == herdr.StatusWorking || a.AgentStatus == herdr.StatusBlocked {
			return fmt.Errorf("agents: park pr %d: agent %s in pane %s is %s: %w", pr.Number, a.Name, p.ID, a.AgentStatus, ErrBusy)
		}
		return nil
	}
	pi, err := m.d.Herdr.PaneProcessInfo(ctx, p.ID)
	switch {
	case isGone(err):
		return nil
	case err != nil:
		return fmt.Errorf("agents: park pr %d: %w", pr.Number, err)
	case !herdr.IdleShell(pi):
		return fmt.Errorf("agents: park pr %d: pane %s runs a command: %w", pr.Number, p.ID, ErrBusy)
	}
	return nil
}

// ResumeID is the session id to resume for a PR role: the newest parked or
// lost session of that role that recorded one and ran the role's current
// agent kind ("" when none, and always "" for a shell role or a kind with
// session_source "none"; a conversation of the role's previous kind is
// never resumed by another CLI).
func (m *Manager) ResumeID(ctx context.Context, prID int64, role Role) (string, error) {
	sessions, err := m.d.Store.SessionsByPR(ctx, prID)
	if err != nil {
		return "", fmt.Errorf("agents: resume id pr %d %s: %w", prID, role, err)
	}
	if spec, ok := m.roleSpec(role); ok {
		if spec.IsShell() {
			return "", nil
		}
		if k, ok := m.kindSpec(spec.AgentKind()); ok && k.SessionSource == config.SessionNone {
			return "", nil
		}
	}
	s, ok := latest(sessions, role, m.resumeCandidate(role))
	if !ok {
		return "", nil
	}
	return store.Deref(s.SessionID), nil
}

// resumeCandidate selects the parked or lost resumable sessions of role
// that ran its configured agent kind (any kind when role is not
// configured).
func (m *Manager) resumeCandidate(role Role) func(store.Session) bool {
	kind := ""
	if spec, ok := m.roleSpec(role); ok && !spec.IsShell() {
		kind = spec.AgentKind()
	}
	return func(s store.Session) bool {
		return (s.State == store.SessionParked || s.State == store.SessionLost) && m.resumable(s) &&
			(kind == "" || m.sessionKind(s) == kind)
	}
}

// RecoverAction says what Recover did with a session.
type RecoverAction string

// Recover actions.
const (
	RecoverOK       RecoverAction = "ok"       // pane and agent are where the row says
	RecoverRebound  RecoverAction = "rebound"  // live row moved to the pane now holding its session id
	RecoverRestored RecoverAction = "restored" // parked/lost row is live again: herdr restored its conversation
	RecoverLost     RecoverAction = "lost"     // pane/agent gone and not found elsewhere
)

// Recovered is one Recover outcome.
type Recovered struct {
	Role    Role
	Action  RecoverAction
	Session store.Session // row before the change
}

// Recover reconciles a PR's sessions with herdr after a daemon or herdr
// restart, before anything is started (herdr may restore Codex itself):
// live rows whose agent is gone are rebound to the pane whose
// agent_session.value equals their session_id, else marked lost; for agent
// roles without a live row (every role with an agent session row of the PR,
// in order of first appearance), the newest resumable parked/lost session
// of the role's current kind (see ResumeID) whose id herdr shows in a pane
// becomes live there. Rebound agents are renamed to the role's agent name. Starting rows whose pane still exists are left alone. Runs are not
// touched: submitted runs stay observed, never re-sent.
func (m *Manager) Recover(ctx context.Context, pr store.PR) ([]Recovered, error) {
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("agents: recover pr %d: %w", pr.Number, err)
	}
	sessions, err := m.d.Store.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return nil, fmt.Errorf("agents: recover pr %d: %w", pr.Number, err)
	}
	panes := paneIndex(snap)
	var out []Recovered
	covered := map[Role]bool{}
	for _, s := range sessions {
		if !isLive(s) {
			continue
		}
		role := Role(s.Role)
		_, paneOK := panes[store.Deref(s.HerdrPaneID)]
		if m.isAgentSession(s) {
			if a, ok := findAgent(snap, s); ok {
				if a.PaneID != "" && a.PaneID != store.Deref(s.HerdrPaneID) {
					if p, ok := panes[a.PaneID]; ok {
						if err := m.rebind(ctx, pr, s, p, snap, liveStates); err != nil {
							return out, err
						}
						out = append(out, Recovered{role, RecoverRebound, s})
						covered[role] = true
						continue
					}
				}
				out = append(out, Recovered{role, RecoverOK, s})
				covered[role] = true
				continue
			}
			if sid := store.Deref(s.SessionID); sid != "" {
				if p, ok := snap.PaneBySession(sid); ok {
					if err := m.rebind(ctx, pr, s, p, snap, liveStates); err != nil {
						return out, err
					}
					out = append(out, Recovered{role, RecoverRebound, s})
					covered[role] = true
					continue
				}
			}
			if s.State == store.SessionStarting && paneOK {
				out = append(out, Recovered{role, RecoverOK, s})
				covered[role] = true
				continue
			}
		} else if paneOK {
			out = append(out, Recovered{role, RecoverOK, s})
			covered[role] = true
			continue
		}
		if err := m.markLost(ctx, s); err != nil {
			return out, err
		}
		out = append(out, Recovered{role, RecoverLost, s})
	}
	var agentRoles []Role
	for _, s := range sessions {
		if r := Role(s.Role); m.isAgentSession(s) && !slices.Contains(agentRoles, r) {
			agentRoles = append(agentRoles, r)
		}
	}
	for _, role := range agentRoles {
		if covered[role] {
			continue
		}
		s, ok := latest(sessions, role, m.resumeCandidate(role))
		if !ok {
			continue
		}
		p, ok := snap.PaneBySession(store.Deref(s.SessionID))
		if !ok {
			continue
		}
		// A lost row may have been marked lost a moment ago in the loop above.
		if err := m.rebind(ctx, pr, s, p, snap, []string{store.SessionParked, store.SessionLost}); err != nil {
			return out, err
		}
		out = append(out, Recovered{role, RecoverRestored, s})
	}
	return out, nil
}

// rebind points session s (state in from) at pane p and makes it live,
// renaming the pane's agent to the role's agent name when needed.
func (m *Manager) rebind(ctx context.Context, pr store.PR, s store.Session, p herdr.Pane, snap herdr.Snapshot, from []string) error {
	name := store.Deref(s.AgentName)
	if name == "" {
		n, err := m.agentName(ctx, pr, Role(s.Role))
		if err != nil {
			return err
		}
		name = n
	}
	status := p.AgentStatus
	current := ""
	for _, a := range snap.Agents {
		if a.PaneID == p.ID {
			current, status = a.Name, a.AgentStatus
		}
	}
	if current != name {
		if err := m.d.Herdr.AgentRename(ctx, p.ID, name); err != nil {
			name = "" // address it by pane id
		}
	}
	now := m.now()
	err := m.d.Store.TransitionSession(ctx, s.ID, from, store.SessionLive, func(u *store.SessionUpdate) {
		u.Set("herdr_pane_id", p.ID)
		u.Set("herdr_workspace_id", nonEmpty(p.WorkspaceID))
		u.Set("herdr_tab_id", nonEmpty(p.TabID))
		u.Set("agent_name", nonEmpty(name))
		u.Set("idle_ticks", 0)
		if status != "" {
			u.Set("agent_status", string(status))
			u.Set("agent_status_at", now)
		}
	})
	if err != nil {
		return fmt.Errorf("agents: rebind session %d to %s: %w", s.ID, p.ID, err)
	}
	return nil
}
