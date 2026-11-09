package registry_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

func TestAppendEventKeepsEachTasksStepsInOrder(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	one := createTask(t, r, "PR-12301")
	two := createTask(t, r, "PR-12302")

	steps := []struct {
		task        int64
		kind, phase string
		text        string
	}{
		{one.ID, "push", registry.PhaseBegin, "pushing abc123"},
		{two.ID, "launch", registry.PhaseInfo, "workspace opened"},
		{one.ID, "push", registry.PhaseOK, "pushed abc123"},
		{0, "daemon", registry.PhaseInfo, "tick"},
	}
	for _, s := range steps {
		clk.Add(time.Second)
		if err := r.AppendEvent(ctx, s.task, s.kind, s.phase, s.text); err != nil {
			t.Fatal(err)
		}
	}

	got, err := r.Events(ctx, one.ID, 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("Events(task 1) = %+v, %v", got, err)
	}
	if got[0].Phase != registry.PhaseBegin || got[1].Phase != registry.PhaseOK || got[0].Text != "pushing abc123" ||
		got[0].Kind != "push" || got[0].TaskID != one.ID || !got[0].At.Equal(t0.Add(time.Second)) || !got[0].At.Before(got[1].At) {
		t.Errorf("events of task 1 = %+v", got)
	}
	if newest, _ := r.Events(ctx, one.ID, 1); len(newest) != 1 || newest[0].Phase != registry.PhaseOK {
		t.Errorf("Events(task 1, limit 1) = %+v, want the newest only", newest)
	}
	if daemon, _ := r.Events(ctx, 0, 0); len(daemon) != 1 || daemon[0].TaskID != 0 || daemon[0].Kind != "daemon" {
		t.Errorf("Events(0) = %+v, want the daemon-level event", daemon)
	}
}

func TestAppendEventRefusesWhatTheLogCannotHold(t *testing.T) {
	t.Parallel()
	r, _ := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	if err := r.AppendEvent(ctx, task.ID, "push", "started", "x"); err == nil {
		t.Error("AppendEvent with an unknown phase succeeded")
	}
	if err := r.AppendEvent(ctx, task.ID, "", registry.PhaseInfo, "x"); err == nil {
		t.Error("AppendEvent without a kind succeeded")
	}
	if err := r.AppendEvent(ctx, 999, "push", registry.PhaseInfo, "x"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("AppendEvent for a missing task = %v, want ErrNotFound", err)
	}
	if got, _ := r.Events(ctx, task.ID, 0); len(got) != 0 {
		t.Errorf("refused events left %d rows", len(got))
	}
}

// EventsAfter reads the events of its kinds written after an event, of
// every task and of the daemon, oldest first and at most limit of them,
// through the index of the events by kind (the alerts' delivery reads it
// every tick).
func TestEventsAfterReadsTheNewerEventsOfItsKindsOldestFirst(t *testing.T) {
	t.Parallel()
	r, _ := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	for _, e := range []struct {
		task       int64
		kind, text string
	}{{task.ID, "alert", "a1"}, {0, "stage", "not one"}, {0, "alert", "a2"}, {task.ID, "alert", "a3"}, {task.ID, "alert_held", "h1"}} {
		if err := r.AppendEvent(ctx, e.task, e.kind, registry.PhaseInfo, e.text); err != nil {
			t.Fatal(err)
		}
	}
	all, err := r.EventsAfter(ctx, 0, 0, "alert")
	if err != nil || len(all) != 3 || all[0].Text != "a1" || all[1].Text != "a2" || all[1].TaskID != 0 || all[2].Text != "a3" {
		t.Fatalf("EventsAfter(0) = %+v, %v; want a1, a2, a3", all, err)
	}
	if got, _ := r.EventsAfter(ctx, all[0].ID, 1, "alert"); len(got) != 1 || got[0].Text != "a2" {
		t.Errorf("EventsAfter(a1, limit 1) = %+v, want a2", got)
	}
	if got, _ := r.EventsAfter(ctx, all[2].ID, 0, "alert"); len(got) != 0 {
		t.Errorf("EventsAfter(a3) = %+v, want none", got)
	}
	if got, _ := r.EventsAfter(ctx, all[1].ID, 0, "alert", "alert_held"); len(got) != 2 || got[1].Kind != "alert_held" {
		t.Errorf("EventsAfter(a2, two kinds) = %+v, want a3 and h1", got)
	}
	if got, err := r.EventsAfter(ctx, 0, 0); err != nil || len(got) != 0 {
		t.Errorf("EventsAfter of no kind = %+v, %v; want none", got, err)
	}
	plan := queryPlan(t, r.DBForTest(), "EXPLAIN QUERY PLAN SELECT id FROM events WHERE kind IN ('alert') AND id > 1 ORDER BY id LIMIT 100")
	if !strings.Contains(strings.Join(plan, "; "), "events_kind") {
		t.Errorf("plan %q, want the index events_kind", plan)
	}
}

// TaskEvents reads one task's events of one kind, oldest first, at most the
// newest limit of them: the screen's per-card read and the engine's checks
// of a task's own stage events stay bounded however long the task's log
// grows, and go by the task's index.
func TestTaskEventsReadsOneTasksKindUpToItsNewestLimit(t *testing.T) {
	t.Parallel()
	r, _ := openAt(t)
	ctx := t.Context()
	task, other := createTask(t, r, "PR-12301"), createTask(t, r, "PR-12302")
	for _, e := range []struct {
		task       int64
		kind, text string
	}{{task.ID, "stage", "s1"}, {task.ID, "push", "p1"}, {other.ID, "stage", "o1"}, {task.ID, "stage", "s2"}, {task.ID, "stage", "s3"}} {
		if err := r.AppendEvent(ctx, e.task, e.kind, registry.PhaseInfo, e.text); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.TaskEvents(ctx, task.ID, "stage", 0)
	if err != nil || len(got) != 3 || got[0].Text != "s1" || got[2].Text != "s3" {
		t.Fatalf("TaskEvents(stage) = %+v, %v; want s1, s2, s3", got, err)
	}
	if got, _ := r.TaskEvents(ctx, task.ID, "stage", 2); len(got) != 2 || got[0].Text != "s2" || got[1].Text != "s3" {
		t.Errorf("TaskEvents(stage, limit 2) = %+v, want the newest two, s2 and s3", got)
	}
	if got, _ := r.TaskEvents(ctx, task.ID, "nothing", 0); len(got) != 0 {
		t.Errorf("TaskEvents of a kind it has none of = %+v", got)
	}
	plan := queryPlan(t, r.DBForTest(), "EXPLAIN QUERY PLAN SELECT id FROM events WHERE task_id = 1 AND kind = 'stage' ORDER BY id DESC LIMIT 100")
	if joined := strings.Join(plan, "; "); !strings.Contains(joined, "events_task") && !strings.Contains(joined, "events_kind") {
		t.Errorf("the query plan %q uses no index of events", joined)
	}
}
