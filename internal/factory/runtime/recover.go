package runtime

// A restart's recovery of the factory's agents (spec section 10): the
// sessions bound again to the panes herdr shows (Recover), and the
// workspaces a crash left between their creation and their first row,
// which no row names (magnum creates a workspace before it records its
// pane): one whose attempt still launches is adopted by that attempt's
// next launch (adopt), any other is closed (SettleWorkspaces), and so is a
// task's liaison's (liaisonLabel). Labels are unique per task and attempt
// (WorkspaceLabel), or per task for a liaison, so the label says whose a
// workspace is.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Recover reconciles task t's sessions with herdr after a restart, before
// anything is launched or sent (agents.Manager.Recover on its owner row): a
// live row whose agent herdr shows elsewhere is bound to that pane again, a
// conversation herdr restored is live again, and a row whose agent is gone
// is lost (the watchdog recovers it). Runs are not touched: a submitted
// prompt is observed, never sent again. The task's liaison's row (Talk) is
// reconciled the same way, so a liaison herdr shows elsewhere is bound
// there again and its next Talk finds it live. It returns what changed
// ("fx-repro rebound"); a task without an owner row or a liaison has
// nothing to recover.
func (r *Runtime) Recover(ctx context.Context, t registry.Task) ([]string, error) {
	r.mu.Lock()
	r.keys[t.ID] = taskKey(t)
	r.mu.Unlock()
	var changed []string
	for _, row := range []func(context.Context, *store.Store, int64) (store.PR, error){ownerRowIn, liaisonRowIn} {
		pr, err := row(ctx, r.st, t.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return changed, fmt.Errorf("recover %s: %w", taskKey(t), err)
		}
		rec, err := r.mgr.Recover(ctx, pr)
		for _, x := range rec {
			if x.Action != agents.RecoverOK {
				changed = append(changed, string(x.Role)+" "+string(x.Action))
			}
		}
		if err != nil {
			return changed, fmt.Errorf("recover %s: %w", taskKey(t), err)
		}
	}
	return changed, nil
}

// Orphan is a workspace of the factory's herdr session that no session row
// names: its id and label, the attempt the label names (its last word; ""
// for a liaison's workspace, liaisonLabel) and that attempt's task, or the
// task the liaison's label names by its key (0 when the registry has no
// such attempt or task), whether it is kept for the attempt's next launch
// to adopt (Adopted) or was closed, and why.
type Orphan struct {
	Workspace, Label, Attempt string
	TaskID                    int64
	Adopted                   bool
	Why                       string
}

// SettleWorkspaces settles, after a restart, the factory's workspaces (a
// label of WorkspaceLabel's shape, labelAttempt, or a liaison's,
// labelLiaison) that no session row names and that hold no agent (a
// workspace with an agent is Recover's to bind), a crash's leftovers
// between a workspace's creation and its first row: one whose attempt still
// launches or runs is kept, and the attempt's next launch adopts it
// (Launch); any other is closed, its attempt ended or unknown to the
// registry, and so is every liaison's (the next Talk makes the liaison a
// workspace of its own). What it settled comes back in herdr's order; a
// workspace it could not settle is in the error, and is settled at the
// next restart.
func (r *Runtime) SettleWorkspaces(ctx context.Context) ([]Orphan, error) {
	snap, err := r.d.Herdr.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("settle the factory's workspaces: %w", err)
	}
	var out []Orphan
	var errs []error
	var keys map[string]int64 // task key -> id, read for the first liaison's leftover
	for _, w := range snap.Workspaces {
		attempt, ok := labelAttempt(w.Label)
		key, liaison := labelLiaison(w.Label)
		if !ok && !liaison {
			continue
		}
		if ok, err := r.unnamed(ctx, snap, w.ID); err != nil {
			errs = append(errs, fmt.Errorf("workspace %s (%q): %w", w.ID, w.Label, err))
			continue
		} else if !ok {
			continue
		}
		o := Orphan{Workspace: w.ID, Label: w.Label, Attempt: attempt}
		if liaison {
			if keys == nil {
				var kerr error
				if keys, kerr = r.taskKeys(ctx); kerr != nil {
					errs = append(errs, fmt.Errorf("workspace %s (%q): %w", w.ID, w.Label, kerr))
					continue
				}
			}
			o.TaskID, o.Why = keys[key], "a liaison's workspace that holds no liaison"
		} else {
			a, err := r.reg.AttemptByID(ctx, o.Attempt)
			switch {
			case errors.Is(err, registry.ErrNotFound):
				o.Why = "the registry has no attempt " + o.Attempt
			case err != nil:
				errs = append(errs, fmt.Errorf("workspace %s (%q): %w", w.ID, w.Label, err))
				continue
			case a.State == registry.AttemptLaunching || a.State == registry.AttemptRunning:
				o.TaskID, o.Adopted, o.Why = a.TaskID, true, "its attempt's launch takes it over"
				out = append(out, o)
				continue
			default:
				o.TaskID, o.Why = a.TaskID, "its attempt "+a.ID+" is "+a.State
			}
		}
		if err := r.d.Herdr.WorkspaceClose(ctx, w.ID); err != nil && !gone(err) {
			errs = append(errs, fmt.Errorf("close workspace %s (%q): %w", w.ID, w.Label, err))
			continue
		}
		r.log.Info("closed a factory workspace no row names", "workspace", w.ID, "label", w.Label, "why", o.Why)
		out = append(out, o)
	}
	return out, errors.Join(errs...)
}

// taskKeys are the registry's tasks by their key (taskKey): the task a
// liaison's workspace label names.
func (r *Runtime) taskKeys(ctx context.Context) (map[string]int64, error) {
	tasks, err := r.reg.Tasks(ctx)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]int64, len(tasks))
	for _, t := range tasks {
		keys[taskKey(t)] = t.ID
	}
	return keys, nil
}

// attemptID is the shape of an attempt's id (registry.BeginAttempt:
// t<task>-<stage>-<n>).
var attemptID = regexp.MustCompile(`^t[0-9]+-[a-z0-9_]+-[0-9]+$`)

// labelAttempt is the attempt a workspace label of WorkspaceLabel's shape
// names ("factory <key> <attempt id>"); false for any other label.
func labelAttempt(label string) (string, bool) {
	fields := strings.Fields(label)
	if len(fields) < 3 || fields[0] != Tag || !attemptID.MatchString(fields[len(fields)-1]) {
		return "", false
	}
	return fields[len(fields)-1], true
}

// unnamed reports whether workspace ws of snap is a crash's leftover: no
// agent in it, and no session row names one of its panes.
func (r *Runtime) unnamed(ctx context.Context, snap herdr.Snapshot, ws string) (bool, error) {
	var panes []string
	for _, p := range snap.Panes {
		if p.WorkspaceID != ws {
			continue
		}
		if p.Agent != "" {
			return false, nil
		}
		panes = append(panes, p.ID)
	}
	if slices.ContainsFunc(snap.Agents, func(a herdr.AgentInfo) bool { return a.WorkspaceID == ws || slices.Contains(panes, a.PaneID) }) {
		return false, nil
	}
	named, err := r.st.PRsWithPanes(ctx, panes)
	return len(named) == 0, err
}

// gone reports whether herdr says the thing a call named no longer exists.
func gone(err error) bool {
	return herdr.IsCode(err, herdr.CodeWorkspaceNotFound) || herdr.IsCode(err, herdr.CodePaneNotFound)
}

// adopt takes over, for attempt a's launch, the workspace a crash left
// before its first row: the one labelled for the attempt (label, or the
// label it recorded before a key was linked) that holds no agent and whose
// panes no row names. When the task has a session that starts or lives, or
// a lost one whose workspace herdr still has (EnsureWorkspace takes that
// workspace, or marks the rest lost and makes a new one), the leftover is
// closed, holding nothing; else its root
// pane (in the copy at path) is recorded for the root role of roles (the
// judge, else the first: EnsureWorkspace's layout), the row EnsureWorkspace
// would have written, so EnsureWorkspace takes the workspace over instead
// of making a second one.
func (r *Runtime) adopt(ctx context.Context, pr store.PR, a registry.Attempt, label string, roles []config.Role, path string,
	env map[string]string) error {
	snap, err := r.d.Herdr.Snapshot(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(snap.Workspaces, func(w herdr.Workspace) bool {
		return w.Label == label || (a.WorkspaceLabel != "" && w.Label == a.WorkspaceLabel)
	})
	if i < 0 {
		return nil
	}
	ws := snap.Workspaces[i]
	if ok, err := r.unnamed(ctx, snap, ws.ID); err != nil || !ok {
		return err
	}
	sessions, err := r.st.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(sessions, func(s store.Session) bool {
		id := store.Deref(s.HerdrWorkspaceID)
		return s.State == store.SessionStarting || s.State == store.SessionLive ||
			s.State == store.SessionLost && slices.ContainsFunc(snap.Workspaces, func(w herdr.Workspace) bool { return w.ID == id })
	}) {
		if err := r.d.Herdr.WorkspaceClose(ctx, ws.ID); err != nil && !gone(err) {
			return fmt.Errorf("close workspace %s (%q), which the task's own makes needless: %w", ws.ID, ws.Label, err)
		}
		return nil
	}
	root := roles[0]
	if j := slices.IndexFunc(roles, func(r config.Role) bool { return r.Judge }); j >= 0 {
		root = roles[j]
	}
	var pane herdr.Pane
	for _, p := range snap.Panes {
		if p.WorkspaceID == ws.ID && (pane.ID == "" || fsx.Canon(p.Cwd) == fsx.Canon(path) && fsx.Canon(pane.Cwd) != fsx.Canon(path)) {
			pane = p
		}
	}
	if pane.ID == "" {
		return nil // a workspace without a pane: EnsureWorkspace makes one
	}
	repo, err := r.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return err
	}
	name := agents.TaggedAgentName(Tag, repo.Owner+"/"+repo.Name, pr.Number, agents.Role(root.Name))
	kind := root.AgentKind()
	penv := maps.Clone(env)
	if penv == nil {
		penv = map[string]string{}
	}
	maps.Copy(penv, r.cfg.RoleEnv(root))
	tab := pane.TabID
	_, err = r.st.CreateSession(ctx, store.Session{PRID: pr.ID, Role: root.Name, AgentName: &name, AgentKind: &kind,
		HerdrWorkspaceID: &ws.ID, HerdrTabID: nonEmpty(tab), HerdrPaneID: &pane.ID, Cwd: &path, Env: penv, State: store.SessionStarting})
	if err != nil {
		return fmt.Errorf("adopt workspace %s (%q): %w", ws.ID, ws.Label, err)
	}
	r.log.Info("adopted the workspace a crash left before its first row", "workspace", ws.ID, "label", ws.Label, "role", root.Name)
	return nil
}

// nonEmpty is s's address, nil for "".
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
