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

// EventParkForced is recorded when a hard park (ParkHard) closed the pane
// of an agent that was still its pane's foreground process after ctrl+c
// and its kind's exit keys; it names the role, the pane and the cause,
// never the pane's text.
const EventParkForced = "pr.park_forced"

// parkForcedCause is the cause EventParkForced records.
const parkForcedCause = "no idle shell after ctrl+c and the exit keys; pane closed"

// Quit stops a session's agent the way a human would: ctrl+c, 1 s, ctrl+c,
// then waits up to QuitIdleTimeout for the pane to be an idle shell again
// (ErrBusy otherwise, session unchanged, herdr's timeout in the chain). An
// agent of a kind with exit keys that herdr lists for a session with a pane
// is quit as the early park quits it (earlyQuit: one ctrl+c, then its exit
// keys, each after the look at a bare composer, ErrBusy with no key sent
// otherwise): two ctrl+c did not quit Claude 2.1.303 nor 2.1.311 at its
// idle prompt, and the quit before a checkout that changed AGENTS.md failed
// every attempt. The session id is captured first (RecordSessionID); the
// session becomes parked when it is resumable (an agent session with a
// session id whose kind reports ids), else closed. A pane or agent that is
// already gone counts as quit. For a shell role's pane this interrupts the
// running command.
func (m *Manager) Quit(ctx context.Context, s store.Session) error {
	a, ok, err := m.exitKeysAgent(ctx, s)
	switch {
	case err != nil:
		return err
	case !ok:
		return m.quit(ctx, s, false)
	}
	_, waited, err := m.earlyQuit(ctx, &s, a)
	switch {
	case err != nil:
		return err
	case waited != nil:
		return quitBusy(s, waited)
	}
	return m.retire(ctx, s)
}

// exitKeysAgent is the agent Quit quits with its kind's exit keys
// (earlyQuit), from a fresh snapshot: ok only for a session with a pane
// whose kind has exit keys and whose agent herdr lists with a conversation
// it or the row names (earlyQuit's look sends nothing without one).
func (m *Manager) exitKeysAgent(ctx context.Context, s store.Session) (a herdr.AgentInfo, ok bool, err error) {
	if k, has := m.kindSpec(m.sessionKind(s)); !has || len(k.ExitKeys) == 0 || store.Deref(s.HerdrPaneID) == "" {
		return a, false, nil
	}
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return a, false, fmt.Errorf("agents: quit session %d: %w", s.ID, err)
	}
	a, ok = findAgent(snap, s)
	named := store.Deref(s.SessionID) != "" || (a.AgentSession != nil && a.AgentSession.Value != "")
	return a, ok && named, nil
}

// quit is Quit; hard goes on where Quit waits (forceQuit).
func (m *Manager) quit(ctx context.Context, s store.Session, hard bool) error {
	waited, err := m.interrupt(ctx, &s)
	switch {
	case err != nil:
		return err
	case waited != nil && hard:
		return m.forceQuit(ctx, s, store.Deref(s.HerdrPaneID), waited)
	case waited != nil:
		return quitBusy(s, waited)
	}
	return m.retire(ctx, s)
}

// interrupt is a quit up to its retire: the session id is recorded first
// (s.SessionID too), then ctrl+c, 1 s, ctrl+c and a wait of up to
// QuitIdleTimeout for the pane to be an idle shell again. waited is herdr's
// timeout when the pane was still no idle shell then (quitBusy says so,
// forceQuit goes on from there); a pane or agent already gone counts as
// quit.
func (m *Manager) interrupt(ctx context.Context, s *store.Session) (waited, err error) {
	if gone, err := m.ctrlC(ctx, s, 2); err != nil || gone {
		return nil, err
	}
	if pane := store.Deref(s.HerdrPaneID); pane != "" {
		if _, err := m.d.Herdr.WaitIdleShell(ctx, pane, QuitIdleTimeout); err != nil && !isGone(err) {
			if !herdr.IsTimeout(err) {
				return nil, fmt.Errorf("agents: quit session %d: %w", s.ID, err)
			}
			return err, nil
		}
	}
	return nil, nil
}

// ctrlC records the session id of s first (s.SessionID too), then sends n
// ctrl+c 1 s apart to its pane, or to its agent when it records no pane
// (interrupt, the early park's earlyQuit). gone: herdr no longer has the
// pane or the agent, which counts as quit. The pane is the one the row
// records, so before both a fresh snapshot must show it is still the
// session's (beforeKeys): otherwise nothing is read from it or sent to it,
// and the quit counts as done when herdr lists the session's agent nowhere,
// or fails with ErrBusy when it shows the agent in another pane.
func (m *Manager) ctrlC(ctx context.Context, s *store.Session, n int) (gone bool, err error) {
	pane := store.Deref(s.HerdrPaneID)
	if pane != "" {
		if gone, err := m.beforeKeys(ctx, *s, pane); err != nil || gone {
			if err != nil {
				return false, fmt.Errorf("agents: quit session %d: %w", s.ID, err)
			}
			return true, nil
		}
	}
	if sid, err := m.RecordSessionID(ctx, *s); err == nil && sid != "" {
		s.SessionID = &sid
	}
	for i := range n {
		if i > 0 {
			if err := m.sleep(ctx, time.Second); err != nil {
				return false, fmt.Errorf("agents: quit session %d: %w", s.ID, err)
			}
		}
		var err error
		if pane != "" {
			err = m.d.Herdr.PaneSendKeys(ctx, pane, "ctrl+c")
		} else {
			err = m.d.Herdr.AgentSendKeys(ctx, target(*s), "ctrl+c")
		}
		switch {
		case isGone(err):
			return true, nil
		case err != nil:
			return false, fmt.Errorf("agents: quit session %d: %w", s.ID, err)
		}
	}
	return false, nil
}

// quitBusy is the failure of a quit whose pane was still no idle shell
// (waited: herdr's timeout): ErrBusy, with the timeout in the chain.
func quitBusy(s store.Session, waited error) error {
	return fmt.Errorf("agents: quit session %d: %w: %w", s.ID, ErrBusy, waited)
}

// forceQuit goes on with the quit of session s, whose pane was no idle shell
// QuitIdleTimeout after two ctrl+c (waited: that wait's timeout): it sends
// the kind's exit keys (Kind.ExitKeys; none: skipped) and waits
// QuitIdleTimeout again, and an agent they quit is retired as by Quit. A
// pane still not an idle shell is closed alone (Herdr.PaneClose), never its
// workspace, whose other panes may be anyone's (on talkable#11932 a shell
// no session owns shared the PR's), and the session becomes closed, never
// parked, with one EventParkForced. A pane herdr refuses to close fails
// with ErrBusy and herdr's timeout, the session unchanged, as Quit's wait
// does. The keys and the close go to the pane the row records, so first a
// fresh snapshot must show it is still the session's (beforeKeys: herdr
// may have restarted during the wait and given the id to another agent):
// otherwise nothing is sent or closed; a session whose agent herdr lists
// nowhere is retired as by Quit, and one it shows in another pane, or a
// snapshot herdr cannot give, fails the quit with ErrBusy, the session
// unchanged.
func (m *Manager) forceQuit(ctx context.Context, s store.Session, pane string, waited error) error {
	switch gone, err := m.beforeKeys(ctx, s, pane); {
	case err != nil:
		return fmt.Errorf("agents: quit session %d: %w: %w; %w", s.ID, ErrBusy, waited, err)
	case gone:
		return m.retire(ctx, s)
	}
	if k, ok := m.kindSpec(m.sessionKind(s)); ok && len(k.ExitKeys) > 0 {
		sent, timedOut, err := m.sendExitKeys(ctx, pane, k.ExitKeys)
		switch {
		case err == nil && timedOut == nil:
			return m.retire(ctx, s)
		case err != nil && (sent || ctx.Err() != nil):
			return fmt.Errorf("agents: quit session %d: %w", s.ID, err)
		case err != nil: // keys herdr refused (invalid_key, say): the close is all that is left
			m.logErr(ctx, err, "agents: exit keys to %s pane %s of session %d: %v", s.Role, pane, s.ID, err)
		default:
			waited = timedOut
		}
	}
	if err := m.d.Herdr.PaneClose(ctx, pane); err != nil && !isGone(err) {
		return fmt.Errorf("agents: quit session %d: %w: %w; close pane %s: %w", s.ID, ErrBusy, waited, pane, err)
	}
	if err := m.retireAs(ctx, s, store.SessionClosed); err != nil {
		return err
	}
	m.event(ctx, m.prSubject(ctx, s.PRID), "info", EventParkForced,
		fmt.Sprintf("%s did not quit: %s (pane %s)", s.Role, parkForcedCause, pane),
		map[string]any{"role": s.Role, "pane": pane, "cause": parkForcedCause})
	return nil
}

// sendExitKeys sends keys, the exit keys of a session's kind, to pane and
// waits up to QuitIdleTimeout for it to be an idle shell again (forceQuit,
// the early park's earlyQuit): waited is herdr's timeout when it is still
// none, and a pane gone counts as quit. err is herdr's refusal of the keys
// (sent false: the wait never ran) or a wait that failed otherwise (sent).
func (m *Manager) sendExitKeys(ctx context.Context, pane string, keys []string) (sent bool, waited, err error) {
	if err := m.d.Herdr.PaneSendKeys(ctx, pane, keys...); err != nil {
		if isGone(err) {
			return true, nil, nil
		}
		return false, nil, err
	}
	_, err = m.d.Herdr.WaitIdleShell(ctx, pane, QuitIdleTimeout)
	switch {
	case err == nil, isGone(err):
		return true, nil, nil
	case herdr.IsTimeout(err):
		return true, err, nil
	}
	return true, nil, err
}

// retire parks a resumable agent session (see resumable) and closes
// everything else.
func (m *Manager) retire(ctx context.Context, s store.Session) error {
	to := store.SessionClosed
	if m.resumable(s) {
		to = store.SessionParked
	}
	return m.retireAs(ctx, s, to)
}

// retireAs moves a starting, live or lost session to state to (parked or
// closed, with closed_at); a session already elsewhere stays.
func (m *Manager) retireAs(ctx context.Context, s store.Session, to string) error {
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

// ParkHard is Park for an agent that will not quit: the live agents it
// quits go on where Quit waits (forceQuit), so one still its pane's
// foreground process after ctrl+c and its kind's exit keys has that pane
// closed and its session closed. Every refusal of Park (a run in flight, an
// agent working or blocked, a busy pane of a workspace it closes) stands.
func (m *Manager) ParkHard(ctx context.Context, pr store.PR) error {
	return m.park(ctx, pr, true)
}

// Park releases a PR's panes while keeping its conversations resumable.
// It refuses with ErrBusy while a run of the PR is pending/submitted/working,
// an agent is working or blocked, a shell role's pane runs a command, or any
// pane of a workspace it would close runs something other than an idle
// shell or an idle agent (a lost session's pane where the user started a
// build, say). Session ids visible in herdr are recorded first. A workspace
// of the PR's live or lost sessions that holds only the PR's own panes is
// closed; one with foreign panes (the user split it) stays, and only the
// PR's live agents are quit. A pane or a workspace another PR's starting or
// live session records is never the PR's (herdr numbers them again after a
// restart): the PR's retired sessions that record one drop their herdr ids
// first (EventHerdrIDsReused), and no such workspace is closed. Nor is one
// with a pane herdr shows as no longer its session's (another agent than
// the row's agent name, or a pane without it outside the session's
// checkout: an eval replay's or the operator's, which no row records);
// the PR's retired sessions whose pane, or workspace when the pane is
// gone, herdr shows so drop their herdr ids too. Live
// sessions with a session_id become parked, the rest closed (see Quit);
// lost sessions stay lost. A quit that fails
// does not stop the others: the PR's other live agents are still quit and
// the failures come back joined (the stuck agent's session stays live, so
// ErrBusy and herdr's timeout are in the chain as for one failure). No
// live or lost sessions: no-op.
func (m *Manager) Park(ctx context.Context, pr store.PR) error {
	return m.park(ctx, pr, false)
}

// park is Park; hard quits as ParkHard does.
func (m *Manager) park(ctx context.Context, pr store.PR, hard bool) error {
	sessions, others, err := m.sessionsBesideOthers(ctx, pr)
	if err != nil {
		return fmt.Errorf("agents: park pr %d: %w", pr.Number, err)
	}
	if !slices.ContainsFunc(sessions, func(s store.Session) bool { return isLive(s) || s.State == store.SessionLost }) {
		return nil
	}
	if err := m.parkCheckRuns(ctx, pr); err != nil {
		return err
	}
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("agents: park pr %d: %w", pr.Number, err)
	}
	sessions, view := m.inHerdr(ctx, pr, snap, sessions)
	var live, lost []store.Session
	ours := map[string]bool{} // never a pane another PR's starting or live session records
	for _, s := range sessions {
		if p := store.Deref(s.HerdrPaneID); p != "" && !others.pane(p) {
			ours[p] = true
		}
		switch {
		case isLive(s):
			live = append(live, s)
		case s.State == store.SessionLost:
			lost = append(lost, s)
		}
	}
	if err := m.parkCheckLive(ctx, pr, snap, live); err != nil {
		return err
	}

	// Workspaces of live and lost sessions: one holding only the PR's panes,
	// each of which herdr shows as still its session's, is closed, once none
	// of its panes is busy; never one another PR's starting or live session
	// records.
	byWS := map[string][]store.Session{}
	for _, s := range append(append([]store.Session(nil), live...), lost...) {
		if ws := store.Deref(s.HerdrWorkspaceID); ws != "" && hasWorkspace(snap, ws) {
			byWS[ws] = append(byWS[ws], s)
		}
	}
	wsIDs := slices.Sorted(maps.Keys(byWS))
	closing := map[string]bool{}
	for _, ws := range wsIDs {
		closing[ws] = !others.workspace(ws) && view.workspaceReason(ws) == "" &&
			!slices.ContainsFunc(snap.Panes, func(p herdr.Pane) bool { return p.WorkspaceID == ws && !ours[p.ID] })
	}
	for _, p := range snap.Panes {
		if closing[p.WorkspaceID] {
			if err := m.parkCheckPane(ctx, pr, snap, p); err != nil {
				return err
			}
		}
	}
	var quitErrs []error
	for _, ws := range wsIDs {
		if closing[ws] {
			if err := m.d.Herdr.WorkspaceClose(ctx, ws); err != nil && !isGone(err) {
				return fmt.Errorf("agents: park pr %d: close workspace %s: %w", pr.Number, ws, err)
			}
			continue
		}
		for _, s := range byWS[ws] {
			if _, ok := findAgent(snap, s); ok && isLive(s) && m.isAgentSession(s) {
				if err := m.quit(ctx, s, hard); err != nil {
					// One agent that will not quit leaves the others' quits to go on:
					// the failures come back joined, the sessions that quit parked.
					quitErrs = append(quitErrs, err)
					if ctx.Err() != nil {
						return fmt.Errorf("agents: park pr %d: %w", pr.Number, errors.Join(quitErrs...))
					}
				}
			}
		}
	}
	if len(quitErrs) > 0 {
		return fmt.Errorf("agents: park pr %d: %w", pr.Number, errors.Join(quitErrs...))
	}
	for _, s := range live {
		if err := m.retire(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// parkCheckRuns refuses (ErrBusy) while a run of the PR is pending,
// submitted or working.
func (m *Manager) parkCheckRuns(ctx context.Context, pr store.PR) error {
	runs, err := m.d.Store.ActiveRuns(ctx)
	if err != nil {
		return fmt.Errorf("agents: park pr %d: %w", pr.Number, err)
	}
	for _, r := range runs {
		if r.PRID == pr.ID && r.State != store.RunEnded {
			return fmt.Errorf("agents: park pr %d: run %s is %s: %w", pr.Number, r.ID, r.State, ErrBusy)
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
