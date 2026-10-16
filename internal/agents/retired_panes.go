package agents

// A fresh start closes the panes it replaces (DECISIONS "A start clears a
// dead launch that holds its agent name, and a fresh start closes the panes
// it replaces"): a fresh start parks the PR's sessions, which leaves their
// panes as idle shells while the workspace's other panes (a shell the
// operator opened) keep it open, and the next workspace splits a new pane
// for every role; talkable#11932's workspace held 17 panes after a day of
// retries.

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// retiredPanes are the panes of the PR's parked and closed sessions in
// snap that no starting, live or lost session of the PR records (a lost
// session's pane is the one the next workspace reuses), in session order.
func retiredPanes(snap herdr.Snapshot, sessions []store.Session) (panes []string, roles map[string]string) {
	inUse := map[string]bool{}
	for _, s := range sessions {
		if isLive(s) || s.State == store.SessionLost {
			inUse[store.Deref(s.HerdrPaneID)] = true
		}
	}
	have := paneIndex(snap)
	roles = map[string]string{}
	for _, s := range sessions {
		p := store.Deref(s.HerdrPaneID)
		if p == "" || inUse[p] || (s.State != store.SessionParked && s.State != store.SessionClosed) {
			continue
		}
		if _, ok := have[p]; !ok {
			continue
		}
		if _, seen := roles[p]; !seen {
			panes = append(panes, p)
		}
		roles[p] = s.Role
	}
	return panes, roles
}

// EventPanesClosed is recorded (info) when CloseRetiredPanes closed panes:
// how many, and which roles they served.
const EventPanesClosed = "pr.panes_closed"

// CloseRetiredPanes closes the panes of the PR's parked and closed sessions
// that herdr still has (a fresh start's park, and a hard park, retire
// sessions and leave their panes as idle shells, and the next workspace
// splits a new pane for every role, so a PR's workspace grew by a pane per
// role and fresh start: 17 on talkable#11932). A pane is closed only when
// its foreground process is the shell and no agent herdr lists there works
// or is blocked, and never when a starting, live or lost session of the PR
// records it; a pane no session records (a shell the operator opened) is
// never one. A parked session keeps its conversation: the role's next start
// resumes it in a new pane. One EventPanesClosed names the count and the
// roles; a pane herdr refuses to close is logged, and the first such
// failure returned after the others were tried.
func (m *Manager) CloseRetiredPanes(ctx context.Context, pr store.PR) error {
	sessions, err := m.d.Store.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return fmt.Errorf("agents: close the retired panes of pr %d: %w", pr.Number, err)
	}
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("agents: close the retired panes of pr %d: %w", pr.Number, err)
	}
	panes, roleOf := retiredPanes(snap, sessions)
	var closed, roles []string
	var first error
	for _, p := range panes {
		if slices.ContainsFunc(snap.Agents, func(a herdr.AgentInfo) bool {
			return a.PaneID == p && (a.AgentStatus == herdr.StatusWorking || a.AgentStatus == herdr.StatusBlocked)
		}) {
			continue
		}
		pi, err := m.d.Herdr.PaneProcessInfo(ctx, p)
		if err != nil || !herdr.IdleShell(pi) {
			continue // gone, unknown or running something: left alone
		}
		if err := m.d.Herdr.PaneClose(ctx, p); err != nil && !isGone(err) {
			m.logErr(ctx, err, "agents: close pane %s of a retired %s session: %v", p, roleOf[p], err)
			first = cmp.Or(first, fmt.Errorf("agents: close pane %s of pr %d: %w", p, pr.Number, err))
			continue
		}
		closed = append(closed, p)
		if !slices.Contains(roles, roleOf[p]) {
			roles = append(roles, roleOf[p])
		}
	}
	if len(closed) > 0 {
		slices.Sort(roles)
		m.event(ctx, m.prSubject(ctx, pr.ID), "info", EventPanesClosed,
			fmt.Sprintf("closed %d panes of retired sessions (%s); the next start lays out one pane per role", len(closed), joinList(roles, "and")),
			map[string]any{"count": len(closed), "roles": roles, "panes": closed})
	}
	return first
}
