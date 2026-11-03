package registry_test

import (
	"errors"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
)

func TestBeginEffectIsIdempotentByKey(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-1")
	const key = "t1:pr:draft"

	e, run, err := r.BeginEffect(ctx, key, "draft_pr", task.ID)
	if err != nil || !run {
		t.Fatalf("first BeginEffect = %+v, run %v, %v; want run", e, run, err)
	}
	if e.State != registry.EffectBegin || e.Key != key || e.Kind != "draft_pr" || e.TaskID != task.ID || !e.Begun.Equal(t0) {
		t.Errorf("begun effect = %+v", e)
	}

	clk.Add(time.Minute)
	if err := r.EndEffect(ctx, key, registry.EffectOK, "https://github.example/example/app/pull/77"); err != nil {
		t.Fatal(err)
	}
	again, run, err := r.BeginEffect(ctx, key, "draft_pr", task.ID)
	if err != nil || run {
		t.Fatalf("BeginEffect after ok = %+v, run %v, %v; want no run", again, run, err)
	}
	if again.State != registry.EffectOK || again.Result != "https://github.example/example/app/pull/77" || !again.Ended.Equal(t0.Add(time.Minute)) {
		t.Errorf("the finished effect = %+v, want its result and end time", again)
	}
}

func TestBeginEffectRunsAgainAfterAFailureOrACrash(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-1")
	const key = "t1:push:abc123"

	if _, run, err := r.BeginEffect(ctx, key, "push", task.ID); err != nil || !run {
		t.Fatalf("first BeginEffect: run %v, %v", run, err)
	}
	// The daemon died before EndEffect: the effect is still begun, and the next
	// start runs its guard again.
	if e, run, err := r.BeginEffect(ctx, key, "push", task.ID); err != nil || !run || e.State != registry.EffectBegin {
		t.Errorf("BeginEffect over a begun effect = %+v, run %v, %v; want run", e, run, err)
	}
	if err := r.EndEffect(ctx, key, registry.EffectFail, "the remote head moved"); err != nil {
		t.Fatal(err)
	}
	e, run, err := r.BeginEffect(ctx, key, "push", task.ID)
	if err != nil || !run || e.State != registry.EffectBegin || e.Result != "" || !e.Ended.IsZero() {
		t.Errorf("BeginEffect over a failed effect = %+v, run %v, %v; want a clean retry", e, run, err)
	}
}

func TestBeginEffectRefusesAKeyThatBelongsToAnotherEffect(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	one := createTask(t, r, "PR-1")
	two := createTask(t, r, "PR-2")
	if _, _, err := r.BeginEffect(ctx, "k", "push", one.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.BeginEffect(ctx, "k", "draft_pr", one.ID); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("same key, other kind = %v, want ErrConflict", err)
	}
	if _, _, err := r.BeginEffect(ctx, "k", "push", two.ID); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("same key, other task = %v, want ErrConflict", err)
	}
	if _, _, err := r.BeginEffect(ctx, "", "push", one.ID); err == nil {
		t.Error("BeginEffect with an empty key succeeded")
	}
	if _, _, err := r.BeginEffect(ctx, "k2", "push", 999); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("BeginEffect for a missing task = %v, want ErrNotFound", err)
	}
}

func TestEndEffectEndsABegunEffectOnce(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-1")
	if err := r.EndEffect(ctx, "nope", registry.EffectOK, ""); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("EndEffect of an unknown key = %v, want ErrNotFound", err)
	}
	if _, _, err := r.BeginEffect(ctx, "k", "push", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.EndEffect(ctx, "k", registry.EffectBegin, ""); err == nil {
		t.Error("EndEffect with the begin state succeeded")
	}
	if err := r.EndEffect(ctx, "k", registry.EffectOK, "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := r.EndEffect(ctx, "k", registry.EffectFail, "late"); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("EndEffect twice = %v, want ErrConflict", err)
	}
	if got, _ := r.EffectByKey(ctx, "k"); got.State != registry.EffectOK || got.Result != "abc123" {
		t.Errorf("effect after a refused second end = %+v", got)
	}
	if _, err := r.EffectByKey(ctx, "nope"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("EffectByKey of an unknown key = %v, want ErrNotFound", err)
	}
}

func TestOpenEffectsListsTheBegunOnesForRecovery(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-1")
	for _, k := range []string{"a", "b", "c"} {
		if _, _, err := r.BeginEffect(ctx, k, "push", task.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.EndEffect(ctx, "b", registry.EffectOK, "x"); err != nil {
		t.Fatal(err)
	}
	got, err := r.OpenEffects(ctx)
	if err != nil || len(got) != 2 || got[0].Key != "a" || got[1].Key != "c" {
		t.Errorf("OpenEffects = %+v, %v; want a and c", got, err)
	}
}
