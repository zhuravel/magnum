package registry_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/store"
)

func TestOpenPinsTheConnectionPragmas(t *testing.T) {
	t.Parallel()
	first, second := registrytest.OpenPair(t)
	ctx := t.Context()
	for pragma, want := range map[string]int{"busy_timeout": 5000, "synchronous": 1, "foreign_keys": 1} {
		var got int
		if err := first.DBForTest().QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
			t.Errorf("PRAGMA %s = %d, %v; want %d", pragma, got, err, want)
		}
	}

	// _txlock=immediate: a write transaction takes the write lock when it
	// begins, so a second registry's write waits for the first one's
	// transaction instead of failing with a busy snapshot after it has read.
	hold, err := first.DBForTest().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback()
	ts := store.FormatTime(t0)
	if _, err := hold.ExecContext(ctx, "INSERT INTO tasks (jira_key, state, created_at, updated_at) VALUES ('PR-12302', 'queued', ?, ?)", ts, ts); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := second.CreateTask(ctx, registry.Task{JiraKey: "PR-12301", Kind: "bug"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a write of the second registry returned (%v) while the first one's transaction held the write lock", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := hold.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the second registry's write after the first one committed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second registry's write never finished")
	}
	if tasks, err := second.Tasks(ctx); err != nil || len(tasks) != 2 {
		t.Errorf("Tasks = %d rows, %v; want both writes", len(tasks), err)
	}
}

func TestTwoRegistriesRaceForOneKey(t *testing.T) {
	t.Parallel()
	one, two := registrytest.OpenPair(t)
	ctx := t.Context()
	regs := []*registry.Registry{one, two}

	var wg sync.WaitGroup
	start := make(chan struct{})
	created := make([]registry.Task, len(regs))
	errs := make([]error, len(regs))
	for i, r := range regs {
		wg.Go(func() {
			<-start
			created[i], errs[i] = r.CreateTask(ctx, registry.Task{JiraKey: "PR-12301", Kind: "bug"})
		})
	}
	close(start)
	wg.Wait()
	var winner registry.Task
	wins, losses := 0, 0
	for i, err := range errs {
		var active registry.ErrActiveKey
		switch {
		case err == nil:
			wins++
			winner = created[i]
		case errors.As(err, &active):
			losses++
		default:
			t.Errorf("registry %d: CreateTask = %v, want success or ErrActiveKey", i, err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("%d creations won and %d lost the key, want one each", wins, losses)
	}
	if tasks, err := one.Tasks(ctx); err != nil || len(tasks) != 1 {
		t.Fatalf("Tasks = %d rows, %v; want exactly one task for the key", len(tasks), err)
	}

	// Two moves out of queued: one wins, the other sees the state moved on.
	start = make(chan struct{})
	moves := make([]error, len(regs))
	for i, r := range regs {
		wg.Go(func() {
			<-start
			moves[i] = r.TransitionTask(ctx, winner.ID, []string{registry.TaskQueued}, registry.TaskActive, nil)
		})
	}
	close(start)
	wg.Wait()
	ok, conflicts := 0, 0
	for i, err := range moves {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, registry.ErrConflict):
			conflicts++
		default:
			t.Errorf("registry %d: TransitionTask = %v, want success or ErrConflict", i, err)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Errorf("%d moves succeeded and %d conflicted, want one each", ok, conflicts)
	}
	if spans, err := one.Spans(ctx, winner.ID); err != nil || len(spans) != 2 {
		t.Errorf("Spans = %+v, %v; want the creation and the one move that won", spans, err)
	}
}
