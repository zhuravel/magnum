package registry_test

import (
	"errors"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
)

func appendPrompt(t *testing.T, r *registry.Registry, taskID int64, attemptID, hash string) registry.LedgerEntry {
	t.Helper()
	e, err := r.AppendLedger(t.Context(), registry.LedgerEntry{TaskID: taskID, AttemptID: attemptID, Role: "claude-reproducer",
		RunID: "run-" + hash, Hash: hash, Text: "Read ticket.md and reproduce the bug."})
	if err != nil {
		t.Fatalf("AppendLedger(%s): %v", hash, err)
	}
	return e
}

func TestUndeliveredListsOnlyUnknownEntries(t *testing.T) {
	t.Parallel()
	r, _ := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	a, err := r.BeginAttempt(ctx, task.ID, "investigate")
	if err != nil {
		t.Fatal(err)
	}
	arrived := appendPrompt(t, r, task.ID, a.ID, "h1")
	lost := appendPrompt(t, r, task.ID, a.ID, "h2")
	inFlight := appendPrompt(t, r, task.ID, a.ID, "h3")
	if arrived.Delivered != registry.DeliveredUnknown || arrived.ID == 0 || !arrived.Sent.Equal(t0) {
		t.Errorf("appended entry = %+v, want unknown delivery sent at the clock", arrived)
	}

	if err := r.MarkDelivered(ctx, arrived.ID, registry.DeliveredYes); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkDelivered(ctx, lost.ID, registry.DeliveredNo); err != nil {
		t.Fatal(err)
	}
	got, err := r.Undelivered(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != inFlight {
		t.Errorf("Undelivered = %+v, want only %+v", got, inFlight)
	}
}

func TestMarkDeliveredDecidesOnce(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	a, _ := r.BeginAttempt(ctx, task.ID, "investigate")
	e := appendPrompt(t, r, task.ID, a.ID, "h1")

	if err := r.MarkDelivered(ctx, e.ID, registry.DeliveredUnknown); err == nil {
		t.Error("MarkDelivered(unknown) succeeded")
	}
	if err := r.MarkDelivered(ctx, e.ID, registry.DeliveredYes); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkDelivered(ctx, e.ID, registry.DeliveredYes); err != nil {
		t.Errorf("repeating the same verdict = %v, want nil", err)
	}
	if err := r.MarkDelivered(ctx, e.ID, registry.DeliveredNo); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("reversing a verdict = %v, want ErrConflict", err)
	}
	if err := r.MarkDelivered(ctx, e.ID+100, registry.DeliveredYes); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("MarkDelivered of a missing entry = %v, want ErrNotFound", err)
	}
}

func TestAppendLedgerNeedsItsAttemptAndItsHash(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	a, _ := r.BeginAttempt(ctx, task.ID, "investigate")
	if _, err := r.AppendLedger(ctx, registry.LedgerEntry{TaskID: task.ID, AttemptID: "t1-nope-1", Role: "r", Hash: "h", Text: "x"}); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("AppendLedger for a missing attempt = %v, want ErrNotFound", err)
	}
	for name, e := range map[string]registry.LedgerEntry{
		"no hash": {TaskID: task.ID, AttemptID: a.ID, Role: "r", Text: "x"},
		"no text": {TaskID: task.ID, AttemptID: a.ID, Role: "r", Hash: "h"},
		"no role": {TaskID: task.ID, AttemptID: a.ID, Hash: "h", Text: "x"},
		"decided": {TaskID: task.ID, AttemptID: a.ID, Role: "r", Hash: "h", Text: "x", Delivered: registry.DeliveredYes},
	} {
		if _, err := r.AppendLedger(ctx, e); err == nil {
			t.Errorf("%s: AppendLedger succeeded", name)
		}
	}
	if got, _ := r.Undelivered(ctx); len(got) != 0 {
		t.Errorf("refused entries left %d rows", len(got))
	}
}

func TestALedgerEntryBelongsToTheTaskOfItsAttempt(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	one := createTask(t, r, "PR-12301")
	two := createTask(t, r, "PR-12302")
	a, err := r.BeginAttempt(ctx, one.ID, "investigate")
	if err != nil {
		t.Fatal(err)
	}
	for name, taskID := range map[string]int64{"another task": two.ID, "no task": 0, "a task that does not exist": 999} {
		_, err := r.AppendLedger(ctx, registry.LedgerEntry{TaskID: taskID, AttemptID: a.ID, Role: "r", Hash: "h", Text: "x"})
		if err == nil || errors.Is(err, registry.ErrNotFound) {
			t.Errorf("%s: AppendLedger = %v, want a refusal that names the mismatch", name, err)
		}
	}
	if got, _ := r.Undelivered(ctx); len(got) != 0 {
		t.Errorf("refused entries left %d rows", len(got))
	}
	if _, err := r.AppendLedger(ctx, registry.LedgerEntry{TaskID: one.ID, AttemptID: a.ID, Role: "r", Hash: "h", Text: "x"}); err != nil {
		t.Errorf("AppendLedger for the attempt's own task: %v", err)
	}
}
