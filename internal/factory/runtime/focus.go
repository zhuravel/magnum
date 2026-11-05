package runtime

// You in the factory's panes, as the observe pass sees you anyway (the
// alerts' toasts hold while you are in one, spec 9.F), and where a task's
// sidebar token goes.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// PaneFocus is what the observe passes saw of herdr's focus in the
// factory's session.
type PaneFocus struct {
	// Pane is herdr's focused pane when it is in a task's workspace (its
	// label is the factory's, WorkspaceLabel), "" when the focus is
	// elsewhere or herdr did not answer the last pass.
	Pane string
	// Since is when the focus moved onto Pane, or herdr last showed you at
	// work in a factory pane after that (agents.ObsHumanActive); Left is
	// when a pass first saw the focus off the factory's panes after it was
	// on one (zero when none did).
	Since, Left time.Time
}

// PaneFocus is what the observe passes saw of you in the factory's panes.
func (r *Runtime) PaneFocus() PaneFocus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.focus
}

// noteFocus takes herdr's focus from the snapshot an observe pass read at
// at (the zero snapshot when herdr did not answer: you are in none of its
// panes).
func (r *Runtime) noteFocus(snap herdr.Snapshot, at time.Time) {
	pane := factoryPane(snap)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch pane {
	case r.focus.Pane:
	case "":
		r.focus = PaneFocus{Left: at}
	default:
		r.focus = PaneFocus{Pane: pane, Since: at, Left: r.focus.Left}
	}
}

// renewFocus notes herdr showing you at work in a factory pane at at: you
// are in the pane you focus.
func (r *Runtime) renewFocus(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.focus.Pane != "" {
		r.focus.Since = at
	}
}

// factoryPane is snap's focused pane when it is in one of the factory's
// workspaces, "" otherwise.
func factoryPane(snap herdr.Snapshot) string {
	id := snap.FocusedPaneID
	if id == "" {
		if i := slices.IndexFunc(snap.Panes, func(p herdr.Pane) bool { return p.Focused }); i >= 0 {
			id = snap.Panes[i].ID
		}
	}
	i := slices.IndexFunc(snap.Panes, func(p herdr.Pane) bool { return p.ID == id })
	if id == "" || i < 0 {
		return ""
	}
	if slices.ContainsFunc(snap.Workspaces, func(w herdr.Workspace) bool {
		return w.ID == snap.Panes[i].WorkspaceID && strings.HasPrefix(w.Label, Tag+" ")
	}) {
		return id
	}
	return ""
}

// Workspaces are the herdr workspaces of task t's live sessions, each
// once: where its sidebar token goes. None for a task that never launched.
func (r *Runtime) Workspaces(ctx context.Context, t registry.Task) ([]string, error) {
	pr, err := r.ownerRow(ctx, t.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ss, err := r.st.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range ss {
		ws := store.Deref(s.HerdrWorkspaceID)
		if (s.State == store.SessionLive || s.State == store.SessionStarting) && ws != "" && !slices.Contains(out, ws) {
			out = append(out, ws)
		}
	}
	return out, nil
}
