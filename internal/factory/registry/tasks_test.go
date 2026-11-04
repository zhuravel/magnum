package registry_test

import (
	"errors"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// openAt opens a registry whose clock the test moves.
func openAt(t *testing.T) (*registry.Registry, *storetest.Clock) {
	t.Helper()
	clk := storetest.NewClock(t0)
	return registrytest.OpenWith(t, registry.Options{Clock: clk.Now}), clk
}

func createTask(t *testing.T, r *registry.Registry, key string) registry.Task {
	t.Helper()
	got, err := r.CreateTask(t.Context(), registry.Task{JiraKey: key, Kind: "bug", Repo: "example/app", Title: "A title"})
	if err != nil {
		t.Fatalf("CreateTask(%q): %v", key, err)
	}
	return got
}

func TestCreateTaskRefusesASecondActiveTaskForOneKey(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	first := createTask(t, r, "PR-12345")

	_, err := r.CreateTask(t.Context(), registry.Task{JiraKey: "PR-12345", Kind: "bug"})
	var active registry.ErrActiveKey
	if !errors.As(err, &active) {
		t.Fatalf("second CreateTask = %v, want an ErrActiveKey", err)
	}
	if active.TaskID != first.ID || active.Key != "PR-12345" {
		t.Errorf("ErrActiveKey = %+v, want task %d for PR-12345", active, first.ID)
	}
	if !errors.Is(err, registry.ErrConflict) {
		t.Errorf("ErrActiveKey does not match ErrConflict: %v", err)
	}
	tasks, err := r.Tasks(t.Context())
	if err != nil || len(tasks) != 1 {
		t.Errorf("Tasks = %d rows, err %v; want the first task only", len(tasks), err)
	}
}

// A task of a free prompt is linked to a Jira key under the rule of one
// unfinished task per key: a key another such task holds is refused with
// that task's id, a key the task holds already is no change, and a task
// keeps the key it holds.
func TestLinkKeyHoldsOneUnfinishedTaskPerKey(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	prompt, err := r.CreateTask(ctx, registry.Task{Prompt: "remind advocates before their credit expires", Kind: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	holder := createTask(t, r, "PR-12345")
	err = r.LinkKey(ctx, prompt.ID, "PR-12345")
	var active registry.ErrActiveKey
	if !errors.As(err, &active) || active.TaskID != holder.ID {
		t.Fatalf("LinkKey to a key task %d holds = %v, want an ErrActiveKey naming it", holder.ID, err)
	}
	if got, _ := r.TaskByID(ctx, prompt.ID); got.JiraKey != "" {
		t.Fatalf("a refused link left key %q", got.JiraKey)
	}

	if err := r.TransitionTask(ctx, holder.ID, nil, registry.TaskDone, nil); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second is no change
		if err := r.LinkKey(ctx, prompt.ID, "PR-12345"); err != nil {
			t.Fatalf("LinkKey once the holder is done = %v", err)
		}
	}
	if got, err := r.TaskByKey(ctx, "PR-12345"); err != nil || got.ID != prompt.ID {
		t.Fatalf("TaskByKey = %+v, %v; want the linked task %d", got, err, prompt.ID)
	}
	if err := r.LinkKey(ctx, prompt.ID, "PR-12346"); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("LinkKey to a second key = %v, want ErrConflict", err)
	}
	if _, err := r.CreateTask(ctx, registry.Task{JiraKey: "PR-12345", Kind: "bug"}); !errors.As(err, &active) || active.TaskID != prompt.ID {
		t.Fatalf("CreateTask for the linked key = %v, want an ErrActiveKey naming task %d", err, prompt.ID)
	}
	if err := r.LinkKey(ctx, 999, "PR-12347"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("LinkKey of a missing task = %v, want ErrNotFound", err)
	}
}

func TestCreateTaskAllowsAKeyAgainAfterDone(t *testing.T) {
	t.Parallel()
	for _, end := range []string{registry.TaskDone, registry.TaskStopped} {
		t.Run(end, func(t *testing.T) {
			t.Parallel()
			r := registrytest.Open(t)
			first := createTask(t, r, "PR-12345")
			if err := r.TransitionTask(t.Context(), first.ID, nil, end, nil); err != nil {
				t.Fatal(err)
			}
			second, err := r.CreateTask(t.Context(), registry.Task{JiraKey: "PR-12345", Kind: "bug"})
			if err != nil {
				t.Fatalf("CreateTask after %s: %v", end, err)
			}
			if second.ID == first.ID {
				t.Errorf("the second task reuses id %d", first.ID)
			}
		})
	}
}

func TestCreateTaskKeepsTheKeyHeldWhileTheTaskHasFailed(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	first := createTask(t, r, "PR-12345")
	if err := r.TransitionTask(t.Context(), first.ID, nil, registry.TaskFailed, nil); err != nil {
		t.Fatal(err)
	}
	_, err := r.CreateTask(t.Context(), registry.Task{JiraKey: "PR-12345"})
	var active registry.ErrActiveKey
	if !errors.As(err, &active) || active.TaskID != first.ID {
		t.Errorf("CreateTask over a failed task = %v, want ErrActiveKey for task %d", err, first.ID)
	}
}

func TestCreateTaskFromPromptsHoldsNoKey(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	for range 2 {
		if _, err := r.CreateTask(t.Context(), registry.Task{Prompt: "rename the helper", Kind: "chore"}); err != nil {
			t.Fatalf("CreateTask from a prompt: %v", err)
		}
	}
	if _, err := r.CreateTask(t.Context(), registry.Task{Kind: "chore"}); err == nil {
		t.Error("CreateTask without a Jira key or a prompt succeeded")
	}
}

func TestCreateTaskStartsQueuedAtTheClock(t *testing.T) {
	t.Parallel()
	r, _ := openAt(t)
	got := createTask(t, r, "PR-12301")
	if got.State != registry.TaskQueued || got.ID == 0 {
		t.Errorf("created task = %+v, want a queued task with an id", got)
	}
	if !got.Created.Equal(t0) || !got.Updated.Equal(t0) {
		t.Errorf("Created %v, Updated %v; want %v", got.Created, got.Updated, t0)
	}
	again, err := r.TaskByID(t.Context(), got.ID)
	if err != nil || again != got {
		t.Errorf("TaskByID = %+v, %v; want %+v", again, err, got)
	}
	if _, err := r.TaskByID(t.Context(), got.ID+100); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("TaskByID of a missing id = %v, want ErrNotFound", err)
	}
}

func TestTransitionTaskIsCompareAndSet(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12345")
	if err := r.TransitionTask(ctx, task.ID, []string{registry.TaskQueued}, registry.TaskActive, nil); err != nil {
		t.Fatal(err)
	}
	err := r.TransitionTask(ctx, task.ID, []string{registry.TaskActive}, registry.TaskNeedsYou, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "you")
		u.Set("waits_what", "3 questions before coding")
	})
	if err != nil {
		t.Fatal(err)
	}

	err = r.TransitionTask(ctx, task.ID, []string{registry.TaskActive}, registry.TaskWaiting, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "ci")
	})
	if !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("transition from active while needs_you = %v, want ErrConflict", err)
	}
	got, _ := r.TaskByID(ctx, task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWho != "you" {
		t.Errorf("a refused transition changed the row: %+v", got)
	}

	err = r.TransitionTask(ctx, task.ID+100, nil, registry.TaskActive, nil)
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("transition of a missing task = %v, want ErrNotFound", err)
	}
}

func TestTransitionTaskAppliesItsAssignmentsInTheSameUpdate(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12345")
	clk.Add(time.Minute)
	err := r.TransitionTask(ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", "coding")
		u.Set("branch", "zhuravel-PR-12345-fix")
		u.Set("copy", "factory1")
		u.Set("head_sha", "abc123")
		u.Set("pr_number", 77)
		u.Inc("attempt", 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.TaskByID(ctx, task.ID)
	want := registry.Task{ID: task.ID, JiraKey: "PR-12345", Kind: "bug", Repo: "example/app", Title: "A title",
		State: registry.TaskActive, Stage: "coding", Branch: "zhuravel-PR-12345-fix", Copy: "factory1",
		HeadSHA: "abc123", Attempt: 1, PRNumber: 77, Created: t0, Updated: t0.Add(time.Minute)}
	if got != want {
		t.Errorf("task after the transition\n got %+v\nwant %+v", got, want)
	}

	// An empty target state keeps the state and still stamps Updated.
	clk.Add(time.Minute)
	if err := r.TransitionTask(ctx, task.ID, []string{registry.TaskActive}, "", func(u *registry.TaskUpdate) { u.Set("stage", "tests") }); err != nil {
		t.Fatal(err)
	}
	got, _ = r.TaskByID(ctx, task.ID)
	if got.State != registry.TaskActive || got.Stage != "tests" || !got.Updated.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("task after a state-keeping update = %+v", got)
	}
}

func TestTransitionTaskRefusesWhatItCannotSet(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12345")
	for _, col := range []string{"state", "id", "jira_key", "created_at", "no_such_column"} {
		err := r.TransitionTask(ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) { u.Set(col, "x") })
		if err == nil || errors.Is(err, registry.ErrConflict) || errors.Is(err, registry.ErrNotFound) {
			t.Errorf("Set(%q) = %v, want a plain refusal", col, err)
		}
	}
	err := r.TransitionTask(ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) { u.Set("title", []string{"x"}) })
	if err == nil {
		t.Error("Set of an unsupported value type succeeded")
	}
	if err := r.TransitionTask(ctx, task.ID, nil, "paused", nil); err == nil {
		t.Error("a transition to an unknown state succeeded")
	}
	got, _ := r.TaskByID(ctx, task.ID)
	if got.State != registry.TaskQueued || got.Title != "A title" {
		t.Errorf("a refused update changed the row: %+v", got)
	}
}

func TestTransitionTaskRefusesToReviveATaskWhileAnotherHoldsTheKey(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	old := createTask(t, r, "PR-12345")
	if err := r.TransitionTask(ctx, old.ID, nil, registry.TaskStopped, nil); err != nil {
		t.Fatal(err)
	}
	createTask(t, r, "PR-12345")
	err := r.TransitionTask(ctx, old.ID, []string{registry.TaskStopped}, registry.TaskActive, nil)
	if !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("reviving the stopped task = %v, want ErrConflict", err)
	}
	if got, _ := r.TaskByID(ctx, old.ID); got.State != registry.TaskStopped {
		t.Errorf("the stopped task is %s after a refused revival", got.State)
	}
}

func TestBeginAttemptNumbersPerTaskAndStage(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	one := createTask(t, r, "PR-12301")
	two := createTask(t, r, "PR-12302")

	first, err := r.BeginAttempt(ctx, one.ID, "investigate")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "t1-investigate-1" || first.N != 1 || first.State != registry.AttemptLaunching ||
		first.TaskID != one.ID || first.Stage != "investigate" || !first.Started.Equal(t0) || !first.Ended.IsZero() {
		t.Errorf("first attempt = %+v", first)
	}
	clk.Add(time.Minute)
	second, _ := r.BeginAttempt(ctx, one.ID, "investigate")
	coding, _ := r.BeginAttempt(ctx, one.ID, "coding")
	other, _ := r.BeginAttempt(ctx, two.ID, "investigate")
	for _, c := range []struct{ got, want string }{
		{second.ID, "t1-investigate-2"}, {coding.ID, "t1-coding-1"}, {other.ID, "t2-investigate-1"},
	} {
		if c.got != c.want {
			t.Errorf("attempt id %q, want %q", c.got, c.want)
		}
	}

	list, err := r.Attempts(ctx, one.ID)
	if err != nil || len(list) != 3 || list[0].ID != first.ID || list[2].ID != coding.ID {
		t.Errorf("Attempts(task 1) = %+v, %v; want the three attempts in order", list, err)
	}
	if _, err := r.BeginAttempt(ctx, 999, "coding"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("BeginAttempt of a missing task = %v, want ErrNotFound", err)
	}
	if _, err := r.BeginAttempt(ctx, one.ID, ""); err == nil {
		t.Error("BeginAttempt without a stage succeeded")
	}
}

func TestTransitionAttemptIsCompareAndSetAndStampsTheEnd(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	a, err := r.BeginAttempt(ctx, task.ID, "investigate")
	if err != nil {
		t.Fatal(err)
	}
	err = r.TransitionAttempt(ctx, a.ID, []string{registry.AttemptLaunching}, registry.AttemptRunning, func(u *registry.AttemptUpdate) {
		u.Set("workspace_label", "factory PR-12301 "+a.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionAttempt(ctx, a.ID, []string{registry.AttemptLaunching}, registry.AttemptRunning, nil); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("second launching->running = %v, want ErrConflict", err)
	}
	if err := r.TransitionAttempt(ctx, "t9-x-1", nil, registry.AttemptRunning, nil); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("transition of a missing attempt = %v, want ErrNotFound", err)
	}
	if got, _ := r.AttemptByID(ctx, a.ID); !got.Ended.IsZero() || got.WorkspaceLabel != "factory PR-12301 "+a.ID {
		t.Errorf("running attempt = %+v", got)
	}

	clk.Add(time.Minute)
	if err := r.TransitionAttempt(ctx, a.ID, []string{registry.AttemptRunning}, registry.AttemptEnded, nil); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	err = r.TransitionAttempt(ctx, a.ID, []string{registry.AttemptEnded}, registry.AttemptFailed, func(u *registry.AttemptUpdate) {
		u.Set("error", "the end fact failed")
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.AttemptByID(ctx, a.ID)
	if got.State != registry.AttemptFailed || got.Error != "the end fact failed" || !got.Ended.Equal(t0.Add(time.Minute)) {
		t.Errorf("failed attempt = %+v, want Ended kept at the first end (%v)", got, t0.Add(time.Minute))
	}
}
