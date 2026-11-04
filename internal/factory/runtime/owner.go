package runtime

// A task's owner row in the factory's agents store: the PR row magnum's
// agents.Manager hangs a task's sessions and runs on, as the retro's pane
// agent hangs its own (internal/engine/retro_pane.go). Its repository is
// factory/<repo name>, its number the task's id, its URL the task's
// directory and its head the task's current attempt: every run's target is
// the attempt it ran in. The row's head is the runtime's bookkeeping alone;
// the task's own head (Task.HeadSHA) is the engine's, from git.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

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
	up, err := r.st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: ownerNode(t.ID), Number: int(t.ID),
		URL: "file://" + r.d.Layout.FactoryTaskDir(t.ID), HeadSHA: head, Title: &title, GHState: store.GHOpen,
		InitialState: store.PRClaiming, Identity: ownerIdentity})
	if err != nil {
		return store.PR{}, fmt.Errorf("owner of %s: %w", taskKey(t), err)
	}
	return up.PR, nil
}

// ownerRow is task id's owner row as it is (store.ErrNotFound before the
// task's first launch).
func (r *Runtime) ownerRow(ctx context.Context, id int64) (store.PR, error) {
	repos, err := r.st.ListRepos(ctx)
	if err != nil {
		return store.PR{}, err
	}
	for _, repo := range repos {
		if repo.Owner != ownerRepo {
			continue
		}
		pr, err := r.st.PRByRepoNumber(ctx, repo.ID, int(id))
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

// ownerOf is the task and the current attempt of owner row prID (its
// number, its head).
func (r *Runtime) ownerOf(ctx context.Context, prID int64) (registry.Task, store.PR, error) {
	pr, err := r.st.PRByID(ctx, prID)
	if err != nil {
		return registry.Task{}, store.PR{}, err
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
