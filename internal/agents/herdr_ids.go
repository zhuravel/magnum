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

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/store"
)

// EventHerdrIDsReused is recorded (info, the PR's subject) when retired
// sessions of the PR record herdr ids that live sessions of other PRs use
// now: their herdr ids are dropped and nothing of theirs is closed (data:
// sessions, the rows; panes and workspaces, the ids dropped).
const EventHerdrIDsReused = "agents.herdr_ids_reused"

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

// stale reports whether session s, retired, records a pane or a workspace
// that a starting or live session of another PR records: a live session's
// pane is its own (herdr shows its agent there, or it was laid out there),
// so the retired row's id is one herdr gave out again.
func (o herdrOwners) stale(s store.Session) bool {
	return retired(s) && (o.pane(store.Deref(s.HerdrPaneID)) || o.workspace(store.Deref(s.HerdrWorkspaceID)))
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
	return m.dropStale(ctx, pr, sessions, others), others, nil
}

// dropStale drops the herdr ids (pane, workspace and tab) of the PR's
// retired sessions that record a pane or a workspace a newer starting or
// live session of another PR records: herdr gave that id to the other PR
// after it restarted, and the row's own pane went with the restart. The
// write is compare-and-set on the ids read (a row moved since is left
// alone); the row keeps its state and its session_id, so a parked or lost
// conversation still resumes, in a new pane. One EventHerdrIDsReused names
// the rows. It returns sessions with those rows' ids dropped.
func (m *Manager) dropStale(ctx context.Context, pr store.PR, sessions []store.Session, others herdrOwners) []store.Session {
	var ids []int64
	var panes, workspaces []string
	out := slices.Clone(sessions)
	for i, s := range out {
		if !others.stale(s) {
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
			m.logErr(ctx, err, "agents: drop the herdr ids another pr uses now from session %d: %v", s.ID, err)
			continue
		}
		if err == nil {
			ids = append(ids, s.ID)
			panes = appendNew(panes, store.Deref(s.HerdrPaneID))
			workspaces = appendNew(workspaces, store.Deref(s.HerdrWorkspaceID))
		}
		out[i].HerdrPaneID, out[i].HerdrWorkspaceID, out[i].HerdrTabID = nil, nil, nil
	}
	if len(ids) > 0 {
		m.event(ctx, m.prSubject(ctx, pr.ID), "info", EventHerdrIDsReused,
			fmt.Sprintf("%d retired sessions recorded herdr ids that other PRs' live sessions use now (%s; herdr numbers them again after a restart): "+
				"their ids are dropped and nothing of theirs is closed", len(ids), strings.Join(panes, ", ")),
			map[string]any{"sessions": ids, "panes": panes, "workspaces": workspaces})
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
