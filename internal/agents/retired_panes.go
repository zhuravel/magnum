package agents

// A retired session's pane is closed (DECISIONS "A start clears a dead
// launch that holds its agent name, and a fresh start closes the panes it
// replaces" and "A parked session's pane is closed at every park and at the
// restart"): a park, a hard park or a lost agent leaves the session's pane
// as an idle shell ("Resume this session with: …") while the workspace's
// other panes (a shell the operator opened) keep it open, and the next
// workspace splits a new pane for every role. talkable#11932's workspace
// held 17 panes after a day of fresh starts, then 21 for 4 live sessions
// once only the fresh start closed them.

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// retiredPanes are the panes of the PR's parked, closed and lost sessions
// in snap that no starting or live session records, of the PR or of another
// (others), in session order; the rows whose pane herdr shows as no longer
// theirs have their ids dropped already (inHerdr). A pane counts only while
// it sits in the workspace its session recorded (when it recorded one): an
// id herdr shows in another workspace is no longer that session's pane; nor
// does one in a workspace another PR's starting or live session records
// (herdr may have split it for that session a moment before its row). roles names the role
// of the newest such session of each pane, and lost the panes whose newest
// such session is lost (its agent went away, as against one magnum parked
// or closed).
func retiredPanes(snap herdr.Snapshot, sessions []store.Session, others herdrOwners) (panes []string, roles map[string]string, lost map[string]bool) {
	inUse := map[string]bool{}
	for _, s := range sessions {
		if isLive(s) {
			inUse[store.Deref(s.HerdrPaneID)] = true
		}
	}
	have := paneIndex(snap)
	roles = map[string]string{}
	lost = map[string]bool{}
	for _, s := range sessions {
		p := store.Deref(s.HerdrPaneID)
		if p == "" || inUse[p] || others.pane(p) || !retired(s) {
			continue
		}
		pane, ok := have[p]
		if !ok || others.workspace(pane.WorkspaceID) {
			continue
		}
		if ws := store.Deref(s.HerdrWorkspaceID); ws != "" && pane.WorkspaceID != ws {
			continue
		}
		if _, seen := roles[p]; !seen {
			panes = append(panes, p)
		}
		roles[p] = s.Role
		lost[p] = s.State == store.SessionLost
	}
	return panes, roles, lost
}

// retired reports whether session s no longer has an agent of its own in
// its pane: parked, closed or lost.
func retired(s store.Session) bool {
	switch s.State {
	case store.SessionParked, store.SessionClosed, store.SessionLost:
		return true
	}
	return false
}

// EventPanesClosed is recorded (info) when CloseRetiredPanes closed panes:
// how many, and which roles they served. Of a lost session's pane the close
// reads the CLI's own last error line first (paneError), which the
// agent.lost event does not carry and the pane takes with it: the event's
// last_lines maps each such role to its line, and the message repeats them.
const EventPanesClosed = "pr.panes_closed"

// CloseRetiredPanes closes the panes of the PR's parked, closed and lost
// sessions that herdr still has (a park, a hard park or an agent that went
// away leaves its session's pane as an idle shell, and the next workspace
// splits a new pane for every role, so a PR's workspace grew by a pane per
// role and start: 21 for 4 live sessions on talkable#11932). A pane is
// closed only when its foreground process is the shell and no agent herdr
// lists there works or is blocked, and never when a starting or live
// session of the PR or of another PR records it or its workspace; a pane no
// session records (a shell the operator opened) is never one. A retired
// session whose pane or workspace a starting or live session of another
// PR records (herdr numbers them again after a restart), or whose pane (its
// workspace, when the pane is gone) herdr shows as no longer its own (an
// agent there that does not carry the row's agent name, or a pane without
// it that works outside the session's checkout: an eval replay's or the
// operator's, which no row records), has its herdr ids dropped, with one
// EventHerdrIDsReused per reason, and nothing of it closes. A
// workspace the closes leave without a pane is closed too. A parked or lost session keeps its conversation: the
// role's next start resumes it in a new pane. One EventPanesClosed names
// the count and the roles, and, of a lost session's pane that showed one,
// the CLI's last error line by role (last_lines; when two closed panes of a
// role show one, the newer session's); a pane herdr refuses to close is
// logged, and the first such failure returned after the others were tried.
func (m *Manager) CloseRetiredPanes(ctx context.Context, pr store.PR) error {
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("agents: close the retired panes of pr %d: %w", pr.Number, err)
	}
	return m.CloseRetiredPanesIn(ctx, pr, snap)
}

// CloseRetiredPanesIn is CloseRetiredPanes over snap, a herdr snapshot the
// caller took for several PRs (the engine's reconcile takes one for every
// PR with a workspace). The PR's sessions are read now, and each pane's
// foreground process is asked of herdr before it is closed, so a pane
// snap is stale about is gone or left alone.
func (m *Manager) CloseRetiredPanesIn(ctx context.Context, pr store.PR, snap herdr.Snapshot) error {
	sessions, others, err := m.sessionsBesideOthers(ctx, pr)
	if err != nil {
		return fmt.Errorf("agents: close the retired panes of pr %d: %w", pr.Number, err)
	}
	sessions, _ = m.inHerdr(ctx, pr, snap, sessions) // what herdr shows as no longer theirs drops its ids: none of it closes
	panes, roleOf, lostPane := retiredPanes(snap, sessions, others)
	var closed, roles []string
	lastLines := map[string]string{}
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
		var line string
		if lostPane[p] {
			line = m.paneError(ctx, p) // before the close: why the agent went away, if the CLI said
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
		if line != "" {
			lastLines[roleOf[p]] = line
		}
	}
	if len(closed) == 0 {
		return first
	}
	emptied := m.closeEmptied(ctx, snap, closed)
	slices.Sort(roles)
	data := map[string]any{"count": len(closed), "roles": roles, "panes": closed}
	if len(emptied) > 0 {
		data["workspaces"] = emptied
	}
	msg := fmt.Sprintf("closed %d panes of retired sessions (%s); the next start lays out one pane per role", len(closed), joinList(roles, "and"))
	if len(lastLines) > 0 {
		data["last_lines"] = lastLines
		for _, role := range slices.Sorted(maps.Keys(lastLines)) {
			msg += fmt.Sprintf(" (%s: %s)", role, lastLines[role])
		}
	}
	m.event(ctx, m.prSubject(ctx, pr.ID), "info", EventPanesClosed, msg, data)
	return first
}

// closeEmptied closes the workspaces whose every pane in snap was just
// closed (closed) and that herdr, asked again, still lists without a pane:
// herdr may keep an empty workspace, which the next start would reuse
// (its session lost there) with no pane to split from. It returns the
// workspaces it closed; a failure is logged.
func (m *Manager) closeEmptied(ctx context.Context, snap herdr.Snapshot, closed []string) []string {
	var candidates []string
	for _, p := range snap.Panes {
		if slices.Contains(closed, p.ID) && !slices.Contains(candidates, p.WorkspaceID) && p.WorkspaceID != "" &&
			!slices.ContainsFunc(snap.Panes, func(o herdr.Pane) bool { return o.WorkspaceID == p.WorkspaceID && !slices.Contains(closed, o.ID) }) {
			candidates = append(candidates, p.WorkspaceID)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	now, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		m.logErr(ctx, err, "agents: check the emptied workspaces %v: %v", candidates, err)
		return nil
	}
	var out []string
	for _, ws := range candidates {
		if !hasWorkspace(now, ws) || slices.ContainsFunc(now.Panes, func(p herdr.Pane) bool { return p.WorkspaceID == ws }) {
			continue // herdr dropped it with its last pane, or someone split a pane there since
		}
		if err := m.d.Herdr.WorkspaceClose(ctx, ws); err != nil && !isGone(err) {
			m.logErr(ctx, err, "agents: close the emptied workspace %s: %v", ws, err)
			continue
		}
		out = append(out, ws)
	}
	return out
}

// ParkClosingPanes is Park, then, once the park worked, CloseRetiredPanes:
// the park of a release (cleanup's Park: `magnum release`, a closed PR's
// cleanup, a slot's removal), which the engine's own parks do in two steps.
// A close that fails is logged; the park held.
func (m *Manager) ParkClosingPanes(ctx context.Context, pr store.PR) error {
	if err := m.Park(ctx, pr); err != nil {
		return err
	}
	if err := m.CloseRetiredPanes(ctx, pr); err != nil {
		m.logErr(ctx, err, "agents: park pr %d: %v", pr.Number, err)
	}
	return nil
}
