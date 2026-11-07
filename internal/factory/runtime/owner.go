package runtime

// A task's owner row in the factory's agents store: the PR row magnum's
// agents.Manager hangs a task's sessions and runs on, as the retro's pane
// agent hangs its own (internal/engine/retro_pane.go). Its repository is
// factory/<repo name>, its number the task's id, its URL the task's
// directory and its head the task's current attempt: every run's target is
// the attempt it ran in. The row's head is the runtime's bookkeeping alone;
// the task's own head (Task.HeadSHA) is the engine's, from git. A task's
// liaison hangs on a row of its own (liaisonRow), never the task's.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
)

// ownerRepo is the owner part of every owner row's repository.
const ownerRepo = "factory"

// ownerIdentity is the identity of the owner rows: no posting identity of
// magnum's, so a run names no reviewer.
const ownerIdentity = "factory"

// taskKey names a task as the factory does: its Jira key, else "task <id>".
func taskKey(t registry.Task) string {
	if t.JiraKey != "" {
		return t.JiraKey
	}
	return "task " + strconv.FormatInt(t.ID, 10)
}

// repoName is the name part of a task's owner/name repository ("task" when
// the task has none yet).
func repoName(full string) string {
	if _, name, ok := strings.Cut(full, "/"); ok && name != "" {
		return name
	}
	if full != "" {
		return full
	}
	return "task"
}

// ownerNodePrefix starts the node id of a task's owner row.
const ownerNodePrefix = "factory:task:"

func ownerNode(id int64) string { return ownerNodePrefix + strconv.FormatInt(id, 10) }

// OwnerTask is the task an owner row's node id names (Owner), false for a
// row that is no task's.
func OwnerTask(nodeID string) (int64, bool) {
	rest, ok := strings.CutPrefix(nodeID, ownerNodePrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil && id > 0
}

// Owner upserts task t's owner row: repository factory/<repo name>, number
// the task's id, URL file://<task dir>, head the task's newest attempt
// (the task's id while it has none).
func (r *Runtime) Owner(ctx context.Context, t registry.Task) (store.PR, error) {
	head := ""
	as, err := r.reg.Attempts(ctx, t.ID)
	if err != nil {
		return store.PR{}, fmt.Errorf("owner of %s: %w", taskKey(t), err)
	}
	if len(as) > 0 {
		head = as[len(as)-1].ID
	}
	return r.owner(ctx, t, head)
}

// owner is Owner with the head the caller names (an attempt's id).
func (r *Runtime) owner(ctx context.Context, t registry.Task, head string) (store.PR, error) {
	r.mu.Lock()
	r.keys[t.ID] = taskKey(t)
	r.mu.Unlock()
	if head == "" {
		head = "t" + strconv.FormatInt(t.ID, 10)
	}
	// One row per repository name: the store holds a name once per owner,
	// and the task's id, the row's number, tells the tasks apart.
	name := repoName(t.Repo)
	repo, err := r.st.UpsertRepo(ctx, store.Repo{NodeID: ownerRepo + ":" + name, Owner: ownerRepo, Name: name,
		WatchOwner: ownerRepo, Mode: store.RepoModePerPR})
	if err != nil {
		return store.PR{}, fmt.Errorf("owner of %s: %w", taskKey(t), err)
	}
	title := taskKey(t)
	if t.Title != "" {
		title += ": " + t.Title
	}
	// The task's base: an agent's start compares the copy's project config
	// with origin/<base>, and a repository whose base is main has no
	// origin/master (the row's repository has the store's default).
	fr, _ := r.cfg.FactoryRepo(t.Repo)
	base := cmp.Or(fr.Base, config.DefaultFactoryBase)
	up, err := r.st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: ownerNode(t.ID), Number: int(t.ID),
		URL: "file://" + r.d.Layout.FactoryTaskDir(t.ID), HeadSHA: head, Title: &title, BaseRef: &base, GHState: store.GHOpen,
		InitialState: store.PRClaiming, Identity: ownerIdentity})
	if err != nil {
		return store.PR{}, fmt.Errorf("owner of %s: %w", taskKey(t), err)
	}
	return up.PR, nil
}

// liaisonRepo is the name part of the liaisons' rows' repository,
// factory-liaison/liaison: a repository of their own, so a liaison's row
// takes its task's id as its number beside the task's row.
const liaisonRepo = "liaison"

// liaisonRepoNode is the node id and the owner of the liaisons' rows'
// repository. The store keeps a repository once per node id and once per
// owner and name, and a task's repository is factory/<repo name> (node id
// factory:<repo name>): with an owner of its own, the liaisons' repository
// can be no task's, one whose repository is named liaison included.
const liaisonRepoNode = "factory-liaison"

// liaisonNodePrefix starts the node id of a task's liaison row.
const liaisonNodePrefix = "factory:liaison:"

func liaisonNode(id int64) string { return liaisonNodePrefix + strconv.FormatInt(id, 10) }

// LiaisonTask is the task a liaison row's node id names, false for a row
// that is no liaison's.
func LiaisonTask(nodeID string) (int64, bool) {
	rest, ok := strings.CutPrefix(nodeID, liaisonNodePrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil && id > 0
}

// liaisonRow upserts task t's liaison row: the owner row of the agent you
// talk with about the task, apart from the task's own row, so your chat
// with it is never input into the task's panes (the task row's
// human_active_at) and its sessions are no role of the task's attempts
// (ownerOf). Its repository is factory-liaison/liaison, its number the task's id,
// its URL the task's folder (where the liaison works: never the copy), and
// its head t<id>, no attempt's.
func (r *Runtime) liaisonRow(ctx context.Context, t registry.Task) (store.PR, error) {
	r.mu.Lock()
	r.keys[t.ID] = taskKey(t)
	r.mu.Unlock()
	repo, err := r.st.UpsertRepo(ctx, store.Repo{NodeID: liaisonRepoNode, Owner: liaisonRepoNode, Name: liaisonRepo,
		WatchOwner: ownerRepo, Mode: store.RepoModePerPR})
	if err != nil {
		return store.PR{}, fmt.Errorf("the liaison of %s: %w", taskKey(t), err)
	}
	title := t.Title
	up, err := r.st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: liaisonNode(t.ID), Number: int(t.ID),
		URL: "file://" + r.d.Layout.FactoryTaskDir(t.ID), HeadSHA: "t" + strconv.FormatInt(t.ID, 10), Title: &title,
		GHState: store.GHOpen, InitialState: store.PRClaiming, Identity: ownerIdentity})
	if err != nil {
		return store.PR{}, fmt.Errorf("the liaison of %s: %w", taskKey(t), err)
	}
	return up.PR, nil
}

// liaisonRowIn is task id's liaison row in st as it is (store.ErrNotFound
// before the task's first Talk): the reads that must not make one (Park,
// the typed turns, Recover).
func liaisonRowIn(ctx context.Context, st *store.Store, id int64) (store.PR, error) {
	repo, err := st.RepoByFullName(ctx, liaisonRepoNode+"/"+liaisonRepo)
	if err != nil {
		return store.PR{}, fmt.Errorf("the liaison of task %d: %w", id, err)
	}
	pr, err := st.PRByRepoNumber(ctx, repo.ID, int(id))
	if err != nil {
		return store.PR{}, fmt.Errorf("the liaison of task %d: %w", id, err)
	}
	if pr.NodeID != liaisonNode(id) {
		return store.PR{}, fmt.Errorf("the liaison of task %d: %w", id, store.ErrNotFound)
	}
	return pr, nil
}

// LiaisonSession is the live session of task id's liaison in st, the agent
// you step into after a talk: store.ErrNotFound before the task's first talk
// and once its liaison is parked.
func LiaisonSession(ctx context.Context, st *store.Store, id int64) (store.Session, error) {
	pr, err := liaisonRowIn(ctx, st, id)
	if err != nil {
		return store.Session{}, err
	}
	return st.LiveSessionByPRRole(ctx, pr.ID, LiaisonRole)
}

// ownerRow is task id's owner row as it is (store.ErrNotFound before the
// task's first launch).
func (r *Runtime) ownerRow(ctx context.Context, id int64) (store.PR, error) {
	return ownerRowIn(ctx, r.st, id)
}

// TaskSessions are the sessions of task id in the factory's agents store
// st, oldest first: every generation of every role the runtime launched
// for it. A task never launched has none.
func TaskSessions(ctx context.Context, st *store.Store, id int64) ([]store.Session, error) {
	pr, err := ownerRowIn(ctx, st, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return st.SessionsByPR(ctx, pr.ID)
}

// ownerRowIn is task id's owner row in st (ownerRow).
func ownerRowIn(ctx context.Context, st *store.Store, id int64) (store.PR, error) {
	repos, err := st.ListRepos(ctx)
	if err != nil {
		return store.PR{}, err
	}
	for _, repo := range repos {
		if repo.Owner != ownerRepo {
			continue
		}
		pr, err := st.PRByRepoNumber(ctx, repo.ID, int(id))
		if err == nil && pr.NodeID == ownerNode(id) {
			return pr, nil
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return store.PR{}, err
		}
	}
	return store.PR{}, fmt.Errorf("owner of task %d: %w", id, store.ErrNotFound)
}

// ownerName names an owner row in its agents' titles and pane labels: its
// task's key ("PR-12345", "task 12"), from the last Owner call (the row's
// number otherwise).
func (r *Runtime) ownerName(pr store.PR) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if k, ok := r.keys[int64(pr.Number)]; ok {
		return k
	}
	return "task " + strconv.Itoa(pr.Number)
}

// errLiaisonRow is ownerOf on a liaison's row (liaisonRow): its number is
// its task's id, but its sessions are no role of the task's attempts, so
// what they show (a dialog, a turn that ended, a lost pane) is no event of
// the task's roles. The callers drop it as they drop a row of no task.
var errLiaisonRow = errors.New("the liaison's row: no role of the task's")

// ownerOf is the task and the current attempt of owner row prID (its
// number, its head); errLiaisonRow for a liaison's row.
func (r *Runtime) ownerOf(ctx context.Context, prID int64) (registry.Task, store.PR, error) {
	pr, err := r.st.PRByID(ctx, prID)
	if err != nil {
		return registry.Task{}, store.PR{}, err
	}
	if _, ok := LiaisonTask(pr.NodeID); ok {
		return registry.Task{}, pr, errLiaisonRow
	}
	t, err := r.reg.TaskByID(ctx, int64(pr.Number))
	if err != nil {
		return registry.Task{}, pr, err
	}
	r.mu.Lock()
	r.keys[t.ID] = taskKey(t)
	r.mu.Unlock()
	return t, pr, nil
}
