package agents

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Workspace is a PR's herdr workspace and the pane serving each role.
type Workspace struct {
	WorkspaceID string
	TabID       string
	// Panes maps a role name to its herdr pane id, for agent and shell roles
	// alike (a shell role's pane runs its command; see RunShell).
	Panes map[Role]string
	// Roles is the layout order: the root pane's role (the judge) first, then
	// the other roles in the order they were laid out (EnsurePane appends).
	Roles []Role
	// Created is true when this call created the workspace (agents must be
	// started; resume ids come from ResumeID).
	Created bool
	// MovedFrom is the previous checkout when the PR's old workspace pointed
	// at another slot and was parked; pass it to the judge as moved_from.
	MovedFrom string
}

// layoutOrder puts the root role first: the judge, else the first role.
func layoutOrder(roles []config.Role) []config.Role {
	root := 0
	for i, r := range roles {
		if r.Judge {
			root = i
			break
		}
	}
	out := make([]config.Role, 0, len(roles))
	out = append(out, roles[root])
	for i, r := range roles {
		if i != root {
			out = append(out, r)
		}
	}
	return out
}

func roleNames(roles []config.Role) []Role {
	out := make([]Role, len(roles))
	for i, r := range roles {
		out[i] = Role(r.Name)
	}
	return out
}

// EnsureWorkspace returns the PR's workspace with a pane per role in roles
// (the watch's roles that need a pane now, e.g. Config.RolesFor minus the
// on-demand ones), creating what is missing:
//
//   - The workspace of the PR's newest starting/live/lost session is reused
//     while herdr still has it and its checkout equals slotPath. Live panes
//     are kept wherever they are; a missing role pane is re-split (a lost
//     session's surviving pane is reused).
//   - If that workspace belongs to another checkout, the PR is parked first
//     (ErrBusy while an agent works) and MovedFrom is set.
//   - Otherwise a new workspace is created (cwd slotPath, label, no focus):
//     its root pane serves the judge (the first role when none is a judge)
//     and every other role gets a split, in the given order, laid out in two
//     columns: each pane joins the column with fewer panes, the right one on
//     a tie (the judge heads the left column). The first other role splits
//     right of the judge, the second down from the first (the classic
//     codex-judge | claude-review over codex-review), the third below the
//     judge, the fourth down the right column again, and so on.
//
// Every pane gets env with the role's env (Config.RoleEnv) laid over it.
// Every newly assigned pane gets a sessions row (state starting, agent
// name/kind or agent_kind "shell", pane/workspace/tab ids, cwd, env_json) and
// is renamed "PR #N <role>" (TaggedPaneLabel); a new workspace whose root pane
// cannot be recorded is closed again (nothing else would find it). Live
// rows whose pane is gone are marked lost first, keeping their session_id
// for ResumeID. Pane envs are
// stored in sessions.env_json, so they must not carry secrets
// (identity.Source.Env never does).
func (m *Manager) EnsureWorkspace(ctx context.Context, pr store.PR, slotPath string, env map[string]string, label string, roles []config.Role) (Workspace, error) {
	if len(roles) == 0 {
		return Workspace{}, fmt.Errorf("agents: workspace pr %d: no roles", pr.Number)
	}
	roles = layoutOrder(roles)
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return Workspace{}, fmt.Errorf("agents: workspace pr %d: %w", pr.Number, err)
	}
	sessions, err := m.d.Store.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return Workspace{}, fmt.Errorf("agents: workspace pr %d: %w", pr.Number, err)
	}

	ws := Workspace{Panes: map[Role]string{}, Roles: roleNames(roles)}
	wsID, tabID, cwd := reusableWorkspace(snap, sessions)
	if wsID != "" && cwd != "" && !samePath(cwd, slotPath) {
		if err := m.Park(ctx, pr); err != nil {
			return Workspace{}, fmt.Errorf("agents: pr %d moved from %s to %s: %w", pr.Number, cwd, slotPath, err)
		}
		ws.MovedFrom = cwd
		wsID = ""
		if snap, err = m.d.Herdr.Snapshot(ctx); err != nil {
			return Workspace{}, fmt.Errorf("agents: workspace pr %d: %w", pr.Number, err)
		}
		if sessions, err = m.d.Store.SessionsByPR(ctx, pr.ID); err != nil {
			return Workspace{}, fmt.Errorf("agents: workspace pr %d: %w", pr.Number, err)
		}
	}
	panes := paneIndex(snap)

	// Pass 1: keep the live panes that still exist; mark the rest lost.
	if wsID != "" {
		ws.WorkspaceID, ws.TabID = wsID, tabID
		for _, role := range ws.Roles {
			live, ok := latest(sessions, role, isLive)
			if !ok {
				continue
			}
			// The pane may sit in another workspace when StartAgent adopted a
			// conversation herdr restored elsewhere; it still serves the role.
			if p, ok := panes[store.Deref(live.HerdrPaneID)]; ok {
				ws.Panes[role] = p.ID
				continue
			}
			if err := m.markLost(ctx, live); err != nil {
				return Workspace{}, err
			}
		}
	} else {
		for _, s := range sessions {
			if isLive(s) {
				if err := m.markLost(ctx, s); err != nil {
					return Workspace{}, err
				}
			}
		}
		root := roles[0]
		created, err := m.d.Herdr.WorkspaceCreate(ctx, herdr.WorkspaceCreateOptions{Cwd: slotPath, Label: label,
			Env: m.paneEnv(env, root), Focus: false})
		if err != nil {
			return Workspace{}, fmt.Errorf("agents: create workspace pr %d: %w", pr.Number, err)
		}
		ws.WorkspaceID, ws.TabID, ws.Created = created.Workspace.ID, created.Tab.ID, true
		if ws.TabID == "" {
			ws.TabID = created.RootPane.TabID
		}
		if err := m.recordPane(ctx, pr, ws, root, created.RootPane.ID, slotPath, m.paneEnv(env, root)); err != nil {
			// No session row points at the new workspace, so nothing would
			// ever find (and close) it.
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceCloseTimeout)
			cerr := m.d.Herdr.WorkspaceClose(cctx, ws.WorkspaceID)
			cancel()
			if cerr != nil && !isGone(cerr) {
				err = errors.Join(err, fmt.Errorf("agents: close unrecorded workspace %s: %w", ws.WorkspaceID, cerr))
			}
			return Workspace{}, err
		}
		if err := m.namePane(ctx, pr, &ws, root, created.RootPane.ID); err != nil {
			return Workspace{}, err
		}
	}

	// Pass 2: fill the missing roles, reusing a lost session's pane if herdr
	// still has it, else splitting from the role's anchor.
	for _, role := range roles {
		r := Role(role.Name)
		if ws.Panes[r] != "" {
			continue
		}
		paneID := ""
		if !ws.Created {
			if lost, ok := latest(sessions, r, func(s store.Session) bool { return s.State == store.SessionLost }); ok {
				if p, ok := panes[store.Deref(lost.HerdrPaneID)]; ok && p.WorkspaceID == ws.WorkspaceID && !usedPane(ws, p.ID) {
					paneID = p.ID
				}
			}
		}
		penv := m.paneEnv(env, role)
		if paneID == "" {
			paneID, err = m.split(ctx, snap, ws, r, slotPath, penv)
			if err != nil {
				return Workspace{}, fmt.Errorf("agents: pane for pr %d %s: %w", pr.Number, r, err)
			}
		}
		if err := m.assignPane(ctx, pr, &ws, role, paneID, slotPath, penv); err != nil {
			return Workspace{}, err
		}
	}
	return ws, nil
}

// EnsurePane adds a pane for role to ws unless a live session of the role
// already has a pane in that workspace (e.g. claude-simplify, which runs
// on demand and is not in EnsureWorkspace's roles). A new pane is laid out
// as the next pane of ws.Roles (see EnsureWorkspace), gets env with the
// role's env laid over it, a starting sessions row and the "PR #N <role>"
// name; start an agent role with StartAgent. The returned Workspace is a
// copy of ws with the pane (ws itself is not modified).
func (m *Manager) EnsurePane(ctx context.Context, pr store.PR, ws Workspace, slotPath string, env map[string]string, role config.Role) (Workspace, error) {
	r := Role(role.Name)
	out := ws
	out.Panes = make(map[Role]string, len(ws.Panes)+1)
	maps.Copy(out.Panes, ws.Panes)
	out.Roles = slices.Clone(ws.Roles)
	if !slices.Contains(out.Roles, r) {
		out.Roles = append(out.Roles, r)
	}
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return ws, fmt.Errorf("agents: %s pane pr %d: %w", r, pr.Number, err)
	}
	live, err := m.d.Store.LiveSessionByPRRole(ctx, pr.ID, role.Name)
	switch {
	case err == nil:
		if p, ok := paneIndex(snap)[store.Deref(live.HerdrPaneID)]; ok && p.WorkspaceID == ws.WorkspaceID {
			out.Panes[r] = p.ID
			return out, nil
		}
		if err := m.markLost(ctx, live); err != nil {
			return ws, err
		}
	case !errors.Is(err, store.ErrNotFound):
		return ws, fmt.Errorf("agents: %s pane pr %d: %w", r, pr.Number, err)
	}
	delete(out.Panes, r)
	penv := m.paneEnv(env, role)
	paneID, err := m.split(ctx, snap, out, r, slotPath, penv)
	if err != nil {
		return ws, fmt.Errorf("agents: %s pane pr %d: %w", r, pr.Number, err)
	}
	if err := m.assignPane(ctx, pr, &out, role, paneID, slotPath, penv); err != nil {
		return ws, err
	}
	return out, nil
}

// paneEnv is env with the role's env (Config.RoleEnv) laid over it; nil
// when both are empty.
func (m *Manager) paneEnv(env map[string]string, role config.Role) map[string]string {
	var extra map[string]string
	if m.d.Config != nil {
		extra = m.d.Config.RoleEnv(role)
	}
	if len(extra) == 0 {
		return env
	}
	out := make(map[string]string, len(env)+len(extra))
	maps.Copy(out, env)
	maps.Copy(out, extra)
	return out
}

// Layout columns.
const (
	colLeft  = 0
	colRight = 1
)

// columns assigns the non-root roles (in layout order) a column: each joins
// the column with fewer panes, the right one on a tie; the root pane heads
// the left column.
func columns(n int) []int {
	out := make([]int, n)
	count := [2]int{1, 0}
	for i := range n {
		c := colRight
		if count[colLeft] < count[colRight] {
			c = colLeft
		}
		out[i] = c
		count[c]++
	}
	return out
}

// anchor is a pane to split and the split direction.
type anchor struct {
	role Role
	dir  string
}

// anchors lists where role's pane may be split from, best first, for the
// layout order roles (the root first):
//
//   - the root: down from any other pane, in order;
//   - another role: down from the nearest earlier role of its column, else
//     from the root (right for the right column, down for the left), else
//     down from a later role of its column.
//
// For codex-judge, claude-review, codex-review and claude-simplify this is
// the classic geometry: claude-review right of codex-judge (repair: above
// codex-review), codex-review below claude-review (repair: right of
// codex-judge), claude-simplify below codex-judge.
func anchors(roles []Role, role Role) []anchor {
	if len(roles) == 0 {
		return nil
	}
	root, others := roles[0], roles[1:]
	if role == root {
		out := make([]anchor, 0, len(others))
		for _, o := range others {
			out = append(out, anchor{o, herdr.SplitDown})
		}
		return out
	}
	i := slices.Index(others, role)
	if i < 0 {
		return []anchor{{root, herdr.SplitDown}}
	}
	cols := columns(len(others))
	var out []anchor
	for j := i - 1; j >= 0; j-- {
		if cols[j] == cols[i] {
			out = append(out, anchor{others[j], herdr.SplitDown})
		}
	}
	if cols[i] == colRight {
		out = append(out, anchor{root, herdr.SplitRight})
	} else {
		out = append(out, anchor{root, herdr.SplitDown})
	}
	for j := i + 1; j < len(others); j++ {
		if cols[j] == cols[i] {
			out = append(out, anchor{others[j], herdr.SplitDown})
		}
	}
	return out
}

// split creates role's pane next to its first anchor (see anchors) that has
// a pane, else down from any pane of the workspace.
func (m *Manager) split(ctx context.Context, snap herdr.Snapshot, ws Workspace, role Role, cwd string, env map[string]string) (string, error) {
	from, dir := "", herdr.SplitDown
	for _, a := range anchors(ws.Roles, role) {
		if p := ws.Panes[a.role]; p != "" {
			from, dir = p, a.dir
			break
		}
	}
	if from == "" {
		for _, p := range snap.Panes {
			if p.WorkspaceID == ws.WorkspaceID {
				from = p.ID
				break
			}
		}
	}
	if from == "" {
		return "", fmt.Errorf("workspace %s has no pane to split", ws.WorkspaceID)
	}
	p, err := m.d.Herdr.PaneSplit(ctx, from, herdr.SplitOptions{Direction: dir, Cwd: cwd, Env: env, Focus: false})
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

// workspaceCloseTimeout bounds closing a workspace EnsureWorkspace created
// but could not record.
const workspaceCloseTimeout = 15 * time.Second

// assignPane records role's pane: a starting sessions row and the pane name.
func (m *Manager) assignPane(ctx context.Context, pr store.PR, ws *Workspace, role config.Role, paneID, cwd string, env map[string]string) error {
	if err := m.recordPane(ctx, pr, *ws, role, paneID, cwd, env); err != nil {
		return err
	}
	return m.namePane(ctx, pr, ws, role, paneID)
}

// recordPane creates the starting sessions row of role's pane in ws.
func (m *Manager) recordPane(ctx context.Context, pr store.PR, ws Workspace, role config.Role, paneID, cwd string, env map[string]string) error {
	r := Role(role.Name)
	x := store.Session{
		PRID:             pr.ID,
		Role:             role.Name,
		HerdrWorkspaceID: nonEmpty(ws.WorkspaceID),
		HerdrTabID:       nonEmpty(ws.TabID),
		HerdrPaneID:      nonEmpty(paneID),
		Cwd:              nonEmpty(cwd),
		Env:              env,
		State:            store.SessionStarting,
	}
	if role.IsShell() {
		x.AgentKind = new(KindShell)
	} else {
		name, err := m.agentName(ctx, pr, r)
		if err != nil {
			return err
		}
		x.AgentName, x.AgentKind = &name, new(role.AgentKind())
	}
	if _, err := m.d.Store.CreateSession(ctx, x); err != nil {
		return fmt.Errorf("agents: session pr %d %s: %w", pr.Number, r, err)
	}
	return nil
}

// namePane puts role's recorded pane into ws and names it "PR #N <role>".
func (m *Manager) namePane(ctx context.Context, pr store.PR, ws *Workspace, role config.Role, paneID string) error {
	r := Role(role.Name)
	ws.Panes[r] = paneID
	if err := m.d.Herdr.PaneRename(ctx, paneID, TaggedPaneLabel(m.d.Tag, pr.Number, r)); err != nil {
		return fmt.Errorf("agents: rename pane %s: %w", paneID, err)
	}
	return nil
}

// markLost moves a starting/live session to lost (its pane or agent is gone),
// keeping session_id for a later resume. A lost race is not an error.
func (m *Manager) markLost(ctx context.Context, s store.Session) error {
	err := m.d.Store.TransitionSession(ctx, s.ID, liveStates, store.SessionLost, nil)
	if err != nil && !errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("agents: mark session %d lost: %w", s.ID, err)
	}
	return nil
}

// reusableWorkspace picks the workspace of the newest starting/live/lost
// session that herdr still has.
func reusableWorkspace(snap herdr.Snapshot, sessions []store.Session) (wsID, tabID, cwd string) {
	for _, s := range slices.Backward(sessions) {

		if !isLive(s) && s.State != store.SessionLost {
			continue
		}
		if id := store.Deref(s.HerdrWorkspaceID); id != "" && hasWorkspace(snap, id) {
			return id, store.Deref(s.HerdrTabID), store.Deref(s.Cwd)
		}
	}
	return "", "", ""
}

// latest returns the newest session of role matching keep.
func latest(sessions []store.Session, role Role, keep func(store.Session) bool) (store.Session, bool) {
	for _, session := range slices.Backward(sessions) {
		if session.Role == string(role) && keep(session) {
			return session, true
		}
	}
	return store.Session{}, false
}

func usedPane(ws Workspace, paneID string) bool {
	for _, p := range ws.Panes {
		if p == paneID {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
