package registry_test

import (
	"errors"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

func TestAppendEventKeepsEachTasksStepsInOrder(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	one := createTask(t, r, "PR-1")
	two := createTask(t, r, "PR-2")

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
	task := createTask(t, r, "PR-1")
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
