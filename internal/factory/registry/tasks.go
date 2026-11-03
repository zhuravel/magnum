package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// Task states. A task is queued until the daemon has room for it, active while
// agents or the daemon work on it, needs_you while only the operator can move
// it, waiting while someone or something else must (a reviewer, CI, a person
// the operator asked), and done, stopped or failed once it has ended.
const (
	TaskQueued   = "queued"
	TaskActive   = "active"
	TaskNeedsYou = "needs_you"
	TaskWaiting  = "waiting"
	TaskDone     = "done"
	TaskStopped  = "stopped"
	TaskFailed   = "failed"
)

var taskStates = map[string]bool{
	TaskQueued: true, TaskActive: true, TaskNeedsYou: true, TaskWaiting: true,
	TaskDone: true, TaskStopped: true, TaskFailed: true,
}

// Task is one piece of work: a Jira key or a free prompt, taken to a draft PR.
type Task struct {
	ID int64
	// JiraKey is the ticket, "" for a task that started from a free prompt. Of
	// the tasks that are not done or stopped, one key has at most one.
	JiraKey string
	// Prompt is the operator's free text; Guidance the words after a Jira key.
	Prompt, Guidance string
	// Kind is a configured kind of task (bug, feature, chore, ...), Repo the
	// configured repository's name.
	Kind, Repo string
	Title      string
	State      string
	// Stage is the configured stage the task is in, Branch the local branch,
	// Copy the repository copy it holds and HeadSHA the branch's head.
	Stage, Branch, Copy, HeadSHA string
	// WaitsWho and WaitsWhat say who the task waits on and for what, in the
	// words the screen shows.
	WaitsWho, WaitsWhat string
	// Attempt counts the attempts of the current stage; PRNumber is the draft
	// PR's number, 0 before it exists.
	Attempt, PRNumber int
	Created, Updated  time.Time
}

// ErrActiveKey is returned (as a value; use errors.As) by CreateTask when a
// task for the same Jira key is not done or stopped. It also matches
// ErrConflict.
type ErrActiveKey struct {
	Key    string
	TaskID int64 // the task that holds the key
}

// Error implements error.
func (e ErrActiveKey) Error() string {
	return fmt.Sprintf("factory registry: %s already has an active task (%d)", e.Key, e.TaskID)
}

// Unwrap makes errors.Is(err, ErrConflict) true.
func (e ErrActiveKey) Unwrap() error { return ErrConflict }

// TaskUpdate collects the column assignments of TransitionTask. Settable
// columns: kind, repo, title, guidance, stage, branch, copy, head_sha,
// waits_who, waits_what, attempt and pr_number. The state moves through
// TransitionTask's `to` and updated_at is the registry's.
type TaskUpdate struct{ update }

var taskSettable = map[string]bool{
	"kind": true, "repo": true, "title": true, "guidance": true, "stage": true, "branch": true,
	"copy": true, "head_sha": true, "waits_who": true, "waits_what": true, "attempt": true, "pr_number": true,
}

const taskColumns = "id, jira_key, prompt, guidance, kind, repo, title, state, stage, branch, copy, head_sha, waits_who, waits_what, attempt, pr_number, created_at, updated_at"

func scanTask(sc scanner) (Task, error) {
	var t Task
	err := sc.Scan(&t.ID, &t.JiraKey, &t.Prompt, &t.Guidance, &t.Kind, &t.Repo, &t.Title, &t.State, &t.Stage,
		&t.Branch, &t.Copy, &t.HeadSHA, &t.WaitsWho, &t.WaitsWhat, &t.Attempt, &t.PRNumber,
		timeCol{&t.Created}, timeCol{&t.Updated})
	return t, err
}

// CreateTask inserts t and returns the stored row. A task needs a Jira key or
// a prompt; an empty State is queued; Created and Updated are the clock's. It
// returns an ErrActiveKey naming the holder when a task for the same Jira key
// is not done or stopped.
func (r *Registry) CreateTask(ctx context.Context, t Task) (Task, error) {
	if t.JiraKey == "" && t.Prompt == "" {
		return Task{}, errors.New("create task: a Jira key or a prompt is required")
	}
	if t.State == "" {
		t.State = TaskQueued
	}
	if !taskStates[t.State] {
		return Task{}, fmt.Errorf("create task: unknown state %q", t.State)
	}
	var out Task
	err := r.tx(ctx, func(tx *sql.Tx) error {
		if t.JiraKey != "" {
			var holder int64
			err := tx.QueryRowContext(ctx, "SELECT id FROM tasks WHERE jira_key = ? AND state NOT IN ('done','stopped') ORDER BY id LIMIT 1", t.JiraKey).Scan(&holder)
			if err == nil {
				return ErrActiveKey{Key: t.JiraKey, TaskID: holder}
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("create task: %w", err)
			}
		}
		now := store.FormatTime(r.now())
		res, err := tx.ExecContext(ctx, `INSERT INTO tasks (jira_key, prompt, guidance, kind, repo, title, state, stage, branch, copy, head_sha, waits_who, waits_what, attempt, pr_number, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.JiraKey, t.Prompt, t.Guidance, t.Kind, t.Repo, t.Title, t.State, t.Stage, t.Branch, t.Copy, t.HeadSHA,
			t.WaitsWho, t.WaitsWhat, t.Attempt, t.PRNumber, now, now)
		if err != nil {
			return fmt.Errorf("create task: %w", mapErr(err))
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("create task: %w", err)
		}
		out, err = scanTask(tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = ?", id))
		if err != nil {
			return fmt.Errorf("create task: %w", err)
		}
		return nil
	})
	if err != nil {
		return Task{}, err
	}
	return out, nil
}

// TransitionTask moves task id to state `to` only if its current state is one
// of from (nil from = any state; empty to = keep the state), applying set's
// assignments and the new updated_at in the same UPDATE. It returns
// ErrConflict when the state did not match (or when the move would give a Jira
// key a second unfinished task) and ErrNotFound when the task does not exist.
func (r *Registry) TransitionTask(ctx context.Context, id int64, from []string, to string, set func(*TaskUpdate)) error {
	u := &TaskUpdate{update{settable: taskSettable}}
	if set != nil {
		set(u)
	}
	u.raw("updated_at = ?", store.FormatTime(r.now()))
	return r.transition(ctx, "tasks", "id", "task", id, taskStates, from, to, &u.update)
}

// TaskByID looks up a task; a missing one is ErrNotFound.
func (r *Registry) TaskByID(ctx context.Context, id int64) (Task, error) {
	t, err := scanTask(r.db.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = ?", id))
	if err != nil {
		return Task{}, notFound(err, "task", id)
	}
	return t, nil
}

// Tasks returns every task, oldest first.
func (r *Registry) Tasks(ctx context.Context) ([]Task, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+taskColumns+" FROM tasks ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("tasks: %w", err)
	}
	ts, err := collect(rows, scanTask)
	if err != nil {
		return nil, fmt.Errorf("tasks: %w", err)
	}
	return ts, nil
}

// Attempt states. An attempt is launching from the moment the daemon records
// the intent to start a stage's agents (before any workspace exists), running
// while they work, ended when they stopped, then verified or failed by the
// stage's end fact; abandoned is an attempt a restart found and gave up.
const (
	AttemptLaunching = "launching"
	AttemptRunning   = "running"
	AttemptEnded     = "ended"
	AttemptVerified  = "verified"
	AttemptFailed    = "failed"
	AttemptAbandoned = "abandoned"
)

var attemptStates = map[string]bool{
	AttemptLaunching: true, AttemptRunning: true, AttemptEnded: true,
	AttemptVerified: true, AttemptFailed: true, AttemptAbandoned: true,
}

// Attempt is one launch of a stage of a task.
type Attempt struct {
	ID     string // t<task>-<stage>-<n>
	TaskID int64
	Stage  string
	N      int // 1 for the first attempt of the stage, then 2, 3, ...
	State  string
	// WorkspaceLabel is the herdr workspace label that carries the attempt id.
	WorkspaceLabel string
	Error          string
	Started, Ended time.Time // Ended is the zero time until the attempt has ended
}

// AttemptUpdate collects the column assignments of TransitionAttempt.
// Settable columns: workspace_label and error. The state moves through
// TransitionAttempt's `to`, and ended_at is stamped when the attempt first
// reaches ended, verified, failed or abandoned.
type AttemptUpdate struct{ update }

var attemptSettable = map[string]bool{"workspace_label": true, "error": true}

const attemptColumns = "id, task_id, stage, n, state, workspace_label, error, started_at, ended_at"

func scanAttempt(sc scanner) (Attempt, error) {
	var a Attempt
	err := sc.Scan(&a.ID, &a.TaskID, &a.Stage, &a.N, &a.State, &a.WorkspaceLabel, &a.Error,
		timeCol{&a.Started}, timeCol{&a.Ended})
	return a, err
}

// BeginAttempt records a launching attempt for the next number of the task's
// stage and returns it. The task must exist (ErrNotFound).
func (r *Registry) BeginAttempt(ctx context.Context, taskID int64, stage string) (Attempt, error) {
	if stage == "" {
		return Attempt{}, errors.New("begin attempt: empty stage")
	}
	var out Attempt
	err := r.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(n), 0) + 1 FROM attempts WHERE task_id = ? AND stage = ?", taskID, stage).Scan(&n); err != nil {
			return fmt.Errorf("begin attempt: %w", err)
		}
		id := fmt.Sprintf("t%d-%s-%d", taskID, stage, n)
		if _, err := tx.ExecContext(ctx, "INSERT INTO attempts (id, task_id, stage, n, state, started_at) VALUES (?, ?, ?, ?, ?, ?)",
			id, taskID, stage, n, AttemptLaunching, store.FormatTime(r.now())); err != nil {
			return fmt.Errorf("begin attempt %s: %w", id, mapErr(err))
		}
		var err error
		out, err = scanAttempt(tx.QueryRowContext(ctx, "SELECT "+attemptColumns+" FROM attempts WHERE id = ?", id))
		return err
	})
	if err != nil {
		return Attempt{}, err
	}
	return out, nil
}

// TransitionAttempt is TransitionTask for attempts.
func (r *Registry) TransitionAttempt(ctx context.Context, id string, from []string, to string, set func(*AttemptUpdate)) error {
	u := &AttemptUpdate{update{settable: attemptSettable}}
	if set != nil {
		set(u)
	}
	switch to {
	case AttemptEnded, AttemptVerified, AttemptFailed, AttemptAbandoned:
		u.raw("ended_at = COALESCE(ended_at, ?)", store.FormatTime(r.now()))
	}
	return r.transition(ctx, "attempts", "id", "attempt", id, attemptStates, from, to, &u.update)
}

// AttemptByID looks up an attempt; a missing one is ErrNotFound.
func (r *Registry) AttemptByID(ctx context.Context, id string) (Attempt, error) {
	a, err := scanAttempt(r.db.QueryRowContext(ctx, "SELECT "+attemptColumns+" FROM attempts WHERE id = ?", id))
	if err != nil {
		return Attempt{}, notFound(err, "attempt", id)
	}
	return a, nil
}

// Attempts returns a task's attempts in the order they began.
func (r *Registry) Attempts(ctx context.Context, taskID int64) ([]Attempt, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+attemptColumns+" FROM attempts WHERE task_id = ? ORDER BY rowid", taskID)
	if err != nil {
		return nil, fmt.Errorf("attempts of task %d: %w", taskID, err)
	}
	as, err := collect(rows, scanAttempt)
	if err != nil {
		return nil, fmt.Errorf("attempts of task %d: %w", taskID, err)
	}
	return as, nil
}
