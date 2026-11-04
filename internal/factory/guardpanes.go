package factory

// The herdr the copies' guard sees. magnum's slot guard (slots.Guard) skips
// the panes and workspaces of the live sessions its store records; the
// copies' store is the factory's, whose sessions run in the factory's herdr
// session, while your agents and consoles run in yours. Each herdr server
// numbers its workspaces and panes from w1, so an id of yours can be one of
// the factory's: GuardPanes gives yours a prefix, so the guard never takes
// your pane for the factory's.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/herdr"
)

// YourPanePrefix starts the ids of your herdr session's workspaces, tabs
// and panes as the copies' guard sees them (GuardPanes).
const YourPanePrefix = "yours:"

// HerdrView is what the copies' guard reads of a herdr session
// (*herdr.Client).
type HerdrView interface {
	Snapshot(ctx context.Context) (herdr.Snapshot, error)
	PaneProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
}

// GuardPanes is the herdr the copies' guard sees (slots.Deps Snapshot and
// ProcessInfo): the factory's session, whose ids are the ones the agents
// store records, and yours, its ids prefixed with YourPanePrefix. Either may
// be nil (no such session).
type GuardPanes struct{ Factory, Yours HerdrView }

// Snapshot is the factory's session's snapshot with yours added, your
// workspaces', tabs' and panes' ids prefixed. A session that does not
// answer is an error: the guard then holds off, as on its own herdr.
func (g GuardPanes) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	var out herdr.Snapshot
	if g.Factory != nil {
		s, err := g.Factory.Snapshot(ctx)
		if err != nil {
			return herdr.Snapshot{}, fmt.Errorf("the factory's herdr session: %w", err)
		}
		out = s
	}
	if g.Yours == nil {
		return out, nil
	}
	s, err := g.Yours.Snapshot(ctx)
	if err != nil {
		return herdr.Snapshot{}, fmt.Errorf("your herdr session: %w", err)
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

// ProcessInfo asks the session a pane id of Snapshot's is of: yours for a
// prefixed id (without the prefix), else the factory's.
func (g GuardPanes) ProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error) {
	view, id := g.Factory, paneID
	if rest, ok := strings.CutPrefix(paneID, YourPanePrefix); ok {
		view, id = g.Yours, rest
	}
	if view == nil {
		return herdr.ProcessInfo{}, errors.New("herdr: no session has pane " + paneID)
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
	return YourPanePrefix + id
}
