package agents

// herdr numbers its workspaces and panes again from the start after it
// restarts (DECISIONS "A PR never closes, reuses or retires what another
// PR's live session records"): at 08:37:24Z on 2026-10-23 it began again at
// wMW, so new workspaces took the ids that parked, closed and lost sessions
// of the day before still recorded, and the three places that read only the
// PR's own sessions (the retired-pane close, Park's workspace close and the
// workspace reuse) closed and took the panes and workspaces of other PRs'
// starting and live sessions: 22 of the next 23 agent.lost events, all 12
// pr.panes_closed events and two parks' workspace closes, and at 13:26:13Z
// the old rows of two PRs closed the new workspaces of three others. Each of
// the three now reads what other PRs' starting and live sessions record
// (othersLive) and never closes, reuses or retires it, and a retired session
// of the PR whose ids such a session records is stale: its herdr ids are
// dropped (dropStale), so nothing reads them again.
//
// What no row of the registry records is out of othersLive's sight
// (DECISIONS "magnum closes or reuses a pane or a workspace by a recorded
// id only while herdr shows it is still the session's"): at 15:10:53Z on
// 2026-10-23 a retired row of talkable#12069 closed pane 145, which an eval
// replay's codex-review held in the replay's own workspace (a replay keeps
// its sessions in a scratch registry), and the operator's own panes are as
// unseen. So a pane or a workspace a session recorded is closed or reused,
// and a pane gets a key by that id (beforeKeys), only while herdr's
// snapshot shows it is still the session's (herdrView): the agent in the
// pane carries the row's agent name, or, with no agent, the pane works in
// the session's checkout, and a workspace is the PR's while every pane in
// it is. A retired row whose pane or workspace herdr shows otherwise is
// stale too.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// EventHerdrIDsReused is recorded (info, the PR's subject) when retired
// sessions of the PR record herdr ids that are no longer theirs: their
// herdr ids are dropped and nothing of theirs is closed. One event per
// reason (data: reason; sessions, the rows; panes and workspaces, the ids
// dropped): "other_pr", live sessions of other PRs use them now;
// "another_agent", herdr shows another agent in the pane (or in the
// workspace) they record; "outside_checkout", herdr shows a pane without
// their agent that works outside their checkout there. It names ids, never
// a pane's text or directory.
const EventHerdrIDsReused = "agents.herdr_ids_reused"

// The reasons of EventHerdrIDsReused: why a pane or a workspace a session
// recorded is no longer its own.
const (
	// idsOtherPR: a starting or live session of another PR records it.
	idsOtherPR = "other_pr"
	// idsAnotherAgent: herdr shows an agent there that does not carry the
	// session's agent name.
	idsAnotherAgent = "another_agent"
	// idsOutsideCheckout: herdr shows a pane without the session's agent
	// that works outside the session's checkout, or does not say where.
	idsOutsideCheckout = "outside_checkout"
)

// herdrOwners are the herdr panes and workspaces that starting or live
// sessions of other PRs record.
type herdrOwners struct {
	panes, workspaces map[string]bool
}

// othersLive reads which herdr panes and workspaces starting or live
// sessions of PRs other than pr record (one registry query). An id a live
// session of pr records too belongs to the newer of the two rows (the
// higher id): the older is one herdr restarted under, which the observer
// marks lost.
func (m *Manager) othersLive(ctx context.Context, pr store.PR, own []store.Session) (herdrOwners, error) {
	live, err := m.d.Store.LiveSessions(ctx)
	if err != nil {
		return herdrOwners{}, fmt.Errorf("agents: the live sessions of other prs: %w", err)
	}
	ownPane, ownWS := map[string]int64{}, map[string]int64{} // id → the PR's newest live row recording it
	for _, s := range own {
		if isLive(s) {
			ownPane[store.Deref(s.HerdrPaneID)] = max(ownPane[store.Deref(s.HerdrPaneID)], s.ID)
			ownWS[store.Deref(s.HerdrWorkspaceID)] = max(ownWS[store.Deref(s.HerdrWorkspaceID)], s.ID)
		}
	}
	o := herdrOwners{panes: map[string]bool{}, workspaces: map[string]bool{}}
	for _, s := range live {
		if s.PRID == pr.ID {
			continue
		}
		if p := store.Deref(s.HerdrPaneID); p != "" && ownPane[p] < s.ID {
			o.panes[p] = true
		}
		if ws := store.Deref(s.HerdrWorkspaceID); ws != "" && ownWS[ws] < s.ID {
			o.workspaces[ws] = true
		}
	}
	return o, nil
}

// pane reports whether another PR's starting or live session records pane p.
func (o herdrOwners) pane(p string) bool { return o.panes[p] }

// workspace reports whether another PR's starting or live session records
// workspace ws.
func (o herdrOwners) workspace(ws string) bool { return o.workspaces[ws] }

// stale is idsOtherPR when session s, retired, records a pane or a
// workspace that a starting or live session of another PR records ("" when
// not): a live session's pane is its own (herdr shows its agent there, or
// it was laid out there), so the retired row's id is one herdr gave out
// again.
func (o herdrOwners) stale(s store.Session) string {
	if retired(s) && (o.pane(store.Deref(s.HerdrPaneID)) || o.workspace(store.Deref(s.HerdrWorkspaceID))) {
		return idsOtherPR
	}
	return ""
}

// sessionsBesideOthers reads the PR's sessions and what other PRs' starting
// and live sessions record (othersLive), with the PR's stale rows' herdr ids
// dropped (dropStale) both in the registry and in the sessions it returns.
func (m *Manager) sessionsBesideOthers(ctx context.Context, pr store.PR) ([]store.Session, herdrOwners, error) {
	sessions, err := m.d.Store.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return nil, herdrOwners{}, err
	}
	others, err := m.othersLive(ctx, pr, sessions)
	if err != nil {
		return nil, herdrOwners{}, err
	}
	return m.dropStale(ctx, pr, sessions, others.stale), others, nil
}

// inHerdr is the PR's sessions as herdr's snapshot snap shows them: the
// herdr ids of the PR's retired sessions whose pane or workspace snap shows
// as no longer theirs (herdrView.stale) are dropped (dropStale), and the
// view over snap of the sessions so is returned with them. Call it before
// anything closes or reuses a pane or a workspace a session recorded.
func (m *Manager) inHerdr(ctx context.Context, pr store.PR, snap herdr.Snapshot, sessions []store.Session) ([]store.Session, herdrView) {
	v := m.viewOf(ctx, pr.ID, snap, sessions)
	v.sessions = m.dropStale(ctx, pr, sessions, v.stale)
	return v.sessions, v
}

// herdrView is what a herdr snapshot shows of a PR's sessions: whether a
// pane or a workspace they record is still theirs.
type herdrView struct {
	snap     herdr.Snapshot
	panes    map[string]herdr.Pane
	agents   map[string]herdr.AgentInfo // the agent herdr lists in each pane
	sessions []store.Session
	// slot is the path of the PR's slot: the checkout of a row that
	// records no cwd ("" when none).
	slot string
}

// viewOf is the herdrView over snap of sessions, the sessions of the PR
// prID. The PR's slot is read only when one of them records no cwd.
func (m *Manager) viewOf(ctx context.Context, prID int64, snap herdr.Snapshot, sessions []store.Session) herdrView {
	v := herdrView{snap: snap, panes: paneIndex(snap), agents: make(map[string]herdr.AgentInfo, len(snap.Agents)), sessions: sessions}
	for _, a := range snap.Agents {
		v.agents[a.PaneID] = a
	}
	if slices.ContainsFunc(sessions, func(s store.Session) bool { return store.Deref(s.Cwd) == "" }) {
		switch sl, err := m.d.Store.SlotByPR(ctx, prID); {
		case err == nil:
			v.slot = sl.Path
		case !errors.Is(err, store.ErrNotFound):
			m.logErr(ctx, err, "agents: the slot of pr id %d, the checkout of its sessions that record none: %v", prID, err)
		}
	}
	return v
}

// checkout is the checkout session s ran in: its cwd, else the PR's slot.
func (v herdrView) checkout(s store.Session) string { return cmp.Or(store.Deref(s.Cwd), v.slot) }

// agentIn is the name of the agent herdr lists in pane p ("" when herdr
// names none: a CLI it detected but did not start); ok is false when it
// lists none there.
func (v herdrView) agentIn(p herdr.Pane) (name string, ok bool) {
	a, ok := v.agents[p.ID]
	return a.Name, ok
}

// paneReason says why herdr's pane p is not session s's ("" when it is):
// the agent herdr shows there must carry s's agent name (idsAnotherAgent);
// a pane with no agent, a parked session's shell, must work in s's checkout
// (idsOutsideCheckout, also when herdr or the row does not say where). An
// agent herdr names none of in the pane of a session with no agent name (a
// shell role whose command runs a CLI herdr detects) is the second case.
func (v herdrView) paneReason(p herdr.Pane, s store.Session) string {
	own := store.Deref(s.AgentName)
	if name, ok := v.agentIn(p); ok && (name != "" || own != "") {
		if name == own {
			return ""
		}
		return idsAnotherAgent
	}
	if inCheckout(p, v.checkout(s)) {
		return ""
	}
	return idsOutsideCheckout
}

// inCheckout reports whether pane p works in checkout or below it (its
// cwd, else its foreground process's); an unknown one does not.
func inCheckout(p herdr.Pane, checkout string) bool {
	dir := cmp.Or(p.Cwd, p.ForegroundCwd)
	return dir != "" && checkout != "" && sameTree(dir, checkout)
}

// recordedReason is paneReason of pane p for the PR's sessions that record
// it: "" when p is one's, else the first one's reason; recorded is false
// when none records it.
func (v herdrView) recordedReason(p herdr.Pane) (why string, recorded bool) {
	for _, s := range v.sessions {
		if store.Deref(s.HerdrPaneID) != p.ID {
			continue
		}
		r := v.paneReason(p, s)
		if r == "" {
			return "", true
		}
		why, recorded = cmp.Or(why, r), true
	}
	return why, recorded
}

// workspaceReason says why workspace ws is not the PR's ("" when every pane
// herdr shows in it is): a pane the PR's sessions record must be one of
// theirs (recordedReason); one none records, an agent under one of the
// PR's agent names, or a pane without one (or with one herdr names none
// of) that works in the checkout of a session that records ws (a shell the
// operator split there, or one herdr split a moment before its row).
func (v herdrView) workspaceReason(ws string) string {
	var names, checkouts []string
	for _, s := range v.sessions {
		names = appendNew(names, store.Deref(s.AgentName))
		if store.Deref(s.HerdrWorkspaceID) == ws {
			checkouts = appendNew(checkouts, v.checkout(s))
		}
	}
	for _, p := range v.snap.Panes {
		if p.WorkspaceID != ws {
			continue
		}
		why, recorded := v.recordedReason(p)
		if !recorded {
			name, _ := v.agentIn(p)
			switch {
			case name != "" && !slices.Contains(names, name):
				why = idsAnotherAgent
			case name == "" && !slices.ContainsFunc(checkouts, func(c string) bool { return inCheckout(p, c) }):
				why = idsOutsideCheckout
			}
		}
		if why != "" {
			return why
		}
	}
	return ""
}

// stale says why session s, retired, records a pane or a workspace that
// herdr shows as no longer its own ("" when it does not): its pane, when
// herdr shows it (paneReason), else its workspace, when herdr shows that
// (workspaceReason).
func (v herdrView) stale(s store.Session) string {
	if !retired(s) {
		return ""
	}
	if p, ok := v.panes[store.Deref(s.HerdrPaneID)]; ok {
		return v.paneReason(p, s)
	}
	if ws := store.Deref(s.HerdrWorkspaceID); ws != "" && hasWorkspace(v.snap, ws) {
		return v.workspaceReason(ws)
	}
	return ""
}

// beforeKeys looks at pane, which session s records, in a fresh herdr
// snapshot before magnum sends it a key by that id (a quit's or a park's
// ctrl+c, the hard park's exit keys before its close): herdr may have given
// the id to another agent or to a shell elsewhere after a restart. It
// answers send (false, nil) only while the pane is s's (paneReason).
// Otherwise nothing may be sent: gone when herdr no longer has the pane, or
// lists s's agent in no other pane either (its agent is gone, which counts
// as quit); an error when herdr cannot say, or, wrapping ErrBusy, when it
// shows s's agent in another pane (the observer moves the row there at its
// next tick, so a later quit reaches it). A pane that is not s's is logged
// (warn), naming ids, never its text or directory.
func (m *Manager) beforeKeys(ctx context.Context, s store.Session, pane string) (gone bool, err error) {
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return false, fmt.Errorf("read herdr before a key to pane %s: %w", pane, err)
	}
	p, ok := paneIndex(snap)[pane]
	if !ok {
		return true, nil
	}
	why := m.viewOf(ctx, s.PRID, snap, []store.Session{s}).paneReason(p, s)
	if why == "" {
		return false, nil
	}
	name := store.Deref(s.AgentName)
	if a, ok := snap.AgentByName(name); ok && name != "" && a.PaneID != "" && a.PaneID != pane {
		m.logAt(slog.LevelWarn, "agents: %s session %d: its pane %s is not its own any more (%s): no key sent; herdr shows its agent in pane %s, where the observer moves it",
			s.Role, s.ID, pane, notTheirs(why), a.PaneID)
		return false, fmt.Errorf("pane %s is not its own any more (%s) and herdr shows its agent in pane %s: no key sent: %w", pane, notTheirs(why), a.PaneID, ErrBusy)
	}
	m.logAt(slog.LevelWarn, "agents: %s session %d: its pane %s is not its own any more (%s): no key sent; herdr lists its agent nowhere, so it counts as quit",
		s.Role, s.ID, pane, notTheirs(why))
	return true, nil
}

// notTheirs says in words what a reason other than idsOtherPR found in a
// pane a session records, never its text or its directory.
func notTheirs(why string) string {
	if why == idsAnotherAgent {
		return "herdr shows another agent there"
	}
	return "herdr shows a pane there that works outside the session's checkout"
}

// dropStale drops the herdr ids (pane, workspace and tab) of the PR's
// sessions that why gives a reason for (herdrOwners.stale: a newer starting
// or live session of another PR records their pane or workspace;
// herdrView.stale: herdr shows their pane or workspace as no longer theirs):
// herdr gave that id to something else after it restarted, and the row's
// own pane went with the restart. The write is compare-and-set on the ids
// read (a row moved since is left alone); the row keeps its state and its
// session_id, so a parked or lost conversation still resumes, in a new pane.
// One EventHerdrIDsReused per reason names the rows. It returns sessions
// with those rows' ids dropped.
func (m *Manager) dropStale(ctx context.Context, pr store.PR, sessions []store.Session, why func(store.Session) string) []store.Session {
	type dropped struct {
		ids               []int64
		panes, workspaces []string
	}
	var reasons []string
	byReason := map[string]*dropped{}
	out := slices.Clone(sessions)
	for i, s := range out {
		reason := why(s)
		if reason == "" {
			continue
		}
		err := m.d.Store.TransitionSession(ctx, s.ID, []string{s.State}, "", func(u *store.SessionUpdate) {
			u.Where("herdr_pane_id", s.HerdrPaneID)
			u.Where("herdr_workspace_id", s.HerdrWorkspaceID)
			u.Set("herdr_pane_id", nil)
			u.Set("herdr_workspace_id", nil)
			u.Set("herdr_tab_id", nil)
		})
		if err != nil && !errors.Is(err, store.ErrConflict) {
			m.logErr(ctx, err, "agents: drop the herdr ids that are no longer its own (%s) from session %d: %v", reason, s.ID, err)
			continue
		}
		if err == nil {
			d := byReason[reason]
			if d == nil {
				d = &dropped{}
				byReason[reason] = d
				reasons = append(reasons, reason)
			}
			d.ids = append(d.ids, s.ID)
			d.panes = appendNew(d.panes, store.Deref(s.HerdrPaneID))
			d.workspaces = appendNew(d.workspaces, store.Deref(s.HerdrWorkspaceID))
		}
		out[i].HerdrPaneID, out[i].HerdrWorkspaceID, out[i].HerdrTabID = nil, nil, nil
	}
	if len(reasons) == 0 {
		return out
	}
	subject := m.prSubject(ctx, pr.ID)
	for _, reason := range reasons {
		d := byReason[reason]
		where := "that other PRs' live sessions use now"
		switch reason {
		case idsAnotherAgent:
			where = "where herdr shows another agent now"
		case idsOutsideCheckout:
			where = "where herdr shows a pane outside their checkout now"
		}
		ids := d.panes
		if len(ids) == 0 { // rows whose pane was gone, dropped for their workspace
			ids = d.workspaces
		}
		m.event(ctx, subject, "info", EventHerdrIDsReused,
			fmt.Sprintf("%d retired sessions recorded herdr ids %s (%s; herdr numbers them again after a restart): "+
				"their ids are dropped and nothing of theirs is closed", len(d.ids), where, strings.Join(ids, ", ")),
			map[string]any{"reason": reason, "sessions": d.ids, "panes": d.panes, "workspaces": d.workspaces})
	}
	return out
}

// appendNew appends v to list unless it is empty or already there.
func appendNew(list []string, v string) []string {
	if v == "" || slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}
