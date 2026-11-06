package herdr

// The herdr a slot guard sees. A guard (slots.Guard) skips the panes and
// workspaces of the live sessions its store records, and those run in one
// herdr session (the daemon's own: the factory's, or the one of magnum's
// review panes), while your agents and consoles run in yours. Each herdr
// server numbers its workspaces and panes from w1, so an id of yours can be
// one of the daemon's: GuardView gives yours a prefix, so the guard never
// takes your pane for one its store records.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// YourPrefix starts the ids of your herdr session's workspaces, tabs and
// panes as a GuardView shows them.
const YourPrefix = "yours:"

// View is what a guard reads of a herdr session (*Client).
type View interface {
	Snapshot(ctx context.Context) (Snapshot, error)
	PaneProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error)
}

// GuardView is the herdr a slot guard sees (slots.Deps Snapshot and
// ProcessInfo): Own, the session whose ids are the ones the guard's store
// records, and Yours, its ids prefixed with YourPrefix. Either may be nil
// (no such session: yours is Own's socket).
type GuardView struct{ Own, Yours View }

// Snapshot is Own's snapshot with Yours added, your workspaces', tabs' and
// panes' ids prefixed. A session that does not answer is an error: the
// guard then holds off, as on its own herdr. Your session with no server
// running (NoServer) adds nothing: it has no pane processes, so nothing of
// yours can be at work in a checkout. Own's never counts as empty: the
// panes its store records could not be checked.
func (g GuardView) Snapshot(ctx context.Context) (Snapshot, error) {
	var out Snapshot
	if g.Own != nil {
		s, err := g.Own.Snapshot(ctx)
		if err != nil {
			return Snapshot{}, fmt.Errorf("the herdr session of its panes: %w", err)
		}
		out = s
	}
	if g.Yours == nil {
		return out, nil
	}
	s, err := g.Yours.Snapshot(ctx)
	if NoServer(err) {
		return out, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("your herdr session: %w", err)
	}
	for _, w := range s.Workspaces {
		w.ID, w.ActiveTabID = yours(w.ID), yours(w.ActiveTabID)
		out.Workspaces = append(out.Workspaces, w)
	}
	for _, t := range s.Tabs {
		t.ID, t.WorkspaceID = yours(t.ID), yours(t.WorkspaceID)
		out.Tabs = append(out.Tabs, t)
	}
	for _, p := range s.Panes {
		p.ID, p.WorkspaceID, p.TabID = yours(p.ID), yours(p.WorkspaceID), yours(p.TabID)
		out.Panes = append(out.Panes, p)
	}
	for _, a := range s.Agents {
		a.PaneID, a.WorkspaceID, a.TabID = yours(a.PaneID), yours(a.WorkspaceID), yours(a.TabID)
		out.Agents = append(out.Agents, a)
	}
	return out, nil
}

// ProcessInfo asks the session a pane id of Snapshot's is of: Yours for a
// prefixed id (without the prefix), else Own.
func (g GuardView) ProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error) {
	view, id := g.Own, paneID
	if rest, ok := strings.CutPrefix(paneID, YourPrefix); ok {
		view, id = g.Yours, rest
	}
	if view == nil {
		return ProcessInfo{}, errors.New("herdr: no session has pane " + paneID)
	}
	pi, err := view.PaneProcessInfo(ctx, id)
	pi.PaneID = paneID
	return pi, err
}

// yours prefixes an id of your session; "" stays "".
func yours(id string) string {
	if id == "" {
		return ""
	}
	return YourPrefix + id
}
