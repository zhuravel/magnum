package herdr

// The herdr a slot guard sees. A guard (slots.Guard) skips the panes and
// workspaces of the live sessions its store records, and those run in one
// herdr session (the daemon's own: the factory's, or the one of magnum's
// review panes), while your agents and consoles run in yours, and the
// factory's local review in magnum's review session. Each herdr server
// numbers its workspaces and panes from w1, so an id of another session can
// be one of the daemon's: GuardView gives the others a prefix, so the guard
// never takes such a pane for one its store records.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// YourPrefix starts the ids of your herdr session's workspaces, tabs and
// panes as a GuardView shows them, and ReviewPrefix those of magnum's
// review session.
const (
	YourPrefix   = "yours:"
	ReviewPrefix = "review:"
)

// View is what a guard reads of a herdr session (*Client).
type View interface {
	Snapshot(ctx context.Context) (Snapshot, error)
	PaneProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error)
}

// GuardView is the herdr a slot guard sees (slots.Deps Snapshot and
// ProcessInfo): Own, the session whose ids are the ones the guard's store
// records; Yours, its ids prefixed with YourPrefix; and Review, magnum's
// review session where it is neither (the factory's copies, in which the
// local review's rounds work), its ids prefixed with ReviewPrefix. Any may
// be nil (no such session apart from Own's).
type GuardView struct{ Own, Yours, Review View }

// Snapshot is Own's snapshot with Yours and Review added, their
// workspaces', tabs' and panes' ids prefixed. A session that does not
// answer is an error: the guard then holds off, as on its own herdr. Yours
// or Review with no server running (NoServer) has no pane processes, but
// herdr restores its panes when the server starts and resumes their agents
// in their conversations: it adds the agents herdr saved, idle in their
// directories (Saver: a client's Saved), so one in a checkout still holds
// it, and a saved state that cannot be read is an error. Own's never counts
// as empty: the panes its store records could not be checked.
func (g GuardView) Snapshot(ctx context.Context) (Snapshot, error) {
	var out Snapshot
	if g.Own != nil {
		s, err := g.Own.Snapshot(ctx)
		if err != nil {
			return Snapshot{}, fmt.Errorf("the herdr session of its panes: %w", err)
		}
		out = s
	}
	for _, side := range []struct {
		view         View
		prefix, name string
	}{{g.Yours, YourPrefix, "your herdr session"}, {g.Review, ReviewPrefix, "magnum's review session"}} {
		if side.view == nil {
			continue
		}
		s, err := side.view.Snapshot(ctx)
		if NoServer(err) {
			sv, ok := side.view.(Saver)
			if !ok {
				continue
			}
			if s, err = sv.Saved(); err != nil {
				return Snapshot{}, fmt.Errorf("%s, which runs no server: %w", side.name, err)
			}
		}
		if err != nil {
			return Snapshot{}, fmt.Errorf("%s: %w", side.name, err)
		}
		addPrefixed(&out, s, side.prefix)
	}
	return out, nil
}

// addPrefixed adds s's workspaces, tabs, panes and agents to out, their ids
// prefixed.
func addPrefixed(out *Snapshot, s Snapshot, prefix string) {
	at := func(id string) string {
		if id == "" {
			return ""
		}
		return prefix + id
	}
	for _, w := range s.Workspaces {
		w.ID, w.ActiveTabID = at(w.ID), at(w.ActiveTabID)
		out.Workspaces = append(out.Workspaces, w)
	}
	for _, t := range s.Tabs {
		t.ID, t.WorkspaceID = at(t.ID), at(t.WorkspaceID)
		out.Tabs = append(out.Tabs, t)
	}
	for _, p := range s.Panes {
		p.ID, p.WorkspaceID, p.TabID = at(p.ID), at(p.WorkspaceID), at(p.TabID)
		out.Panes = append(out.Panes, p)
	}
	for _, a := range s.Agents {
		a.PaneID, a.WorkspaceID, a.TabID = at(a.PaneID), at(a.WorkspaceID), at(a.TabID)
		out.Agents = append(out.Agents, a)
	}
}

// ProcessInfo asks the session a pane id of Snapshot's is of: Yours or
// Review for a prefixed id (without the prefix), else Own.
func (g GuardView) ProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error) {
	view, id := g.Own, paneID
	if rest, ok := strings.CutPrefix(paneID, YourPrefix); ok {
		view, id = g.Yours, rest
	} else if rest, ok := strings.CutPrefix(paneID, ReviewPrefix); ok {
		view, id = g.Review, rest
	}
	if view == nil {
		return ProcessInfo{}, errors.New("herdr: no session has pane " + paneID)
	}
	pi, err := view.PaneProcessInfo(ctx, id)
	pi.PaneID = paneID
	return pi, err
}
