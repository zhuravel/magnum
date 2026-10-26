package evalrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/dblock"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// warnings records what a replay's Deps.Warn says.
type warnings struct {
	mu    sync.Mutex
	lines []string
}

func (w *warnings) warn(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, fmt.Sprintf(format, args...))
}

func (w *warnings) said() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.lines...)
}

// waitFor fails the test unless cond holds within a few seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lockOptions are the options of a replay in run id under l's runs root.
func lockOptions(l paths.Layout, id string) Options {
	return Options{RunDir: filepath.Join(RunsRoot(l), id), AgentTag: eval.AgentTag(id)}
}

// Two replays of one case share its herdr workspace label and, under one runs root, its worktree,
// and destroyed each other on 2026-10-20. RunCase takes the case's lock for the whole replay, its
// cleanup included: a second replay of the case starts only once the first ended, saying once that it
// waits and for which run, while a replay of another case runs at once. (No watch covers the
// widgets repository, so each replay ends right after it took its lock; the first is held there by
// its clock, which it reads once it holds the lock.)
func TestTwoReplaysOfOneCaseNeverOverlap(t *testing.T) {
	storetest.Serial(t) // lockPoll
	defer func(p time.Duration) { lockPoll = p }(lockPoll)
	lockPoll = 10 * time.Millisecond
	l := paths.Layout{Home: t.TempDir()}
	cfg := testConfig(t)
	ctx := context.Background()

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	first := Deps{Layout: l, Config: cfg, Now: func() time.Time {
		once.Do(func() { close(entered); <-release })
		return time.Now()
	}}
	firstDone := make(chan eval.CaseRun, 1)
	go func() { firstDone <- RunCase(ctx, first, widgetsCase(), lockOptions(l, "20261020-100000")) }()
	<-entered

	var w warnings
	second := Deps{Layout: l, Config: cfg, Warn: w.warn}
	secondDone := make(chan eval.CaseRun, 1)
	go func() { secondDone <- RunCase(ctx, second, widgetsCase(), lockOptions(l, "20261020-100001")) }()

	other := widgetsCase()
	other.Name = "y"
	if cr := RunCase(ctx, second, other, lockOptions(l, "20261020-100002")); cr.Outcome != OutcomeError || !strings.Contains(cr.Error, "[[watch]]") {
		t.Fatalf("a replay of another case: %+v, want it to run (and fail on the watch) while case x is replayed", cr)
	}
	waitFor(t, "the second replay to say it waits", func() bool { return len(w.said()) > 0 })
	select {
	case cr := <-secondDone:
		t.Fatalf("the second replay of case x ran beside the first: %+v", cr)
	case <-time.After(100 * time.Millisecond):
	}
	ended := time.Now()
	close(release)
	if cr := <-firstDone; cr.Outcome != OutcomeError || !strings.Contains(cr.Error, "[[watch]]") {
		t.Fatalf("the first replay: %+v", cr)
	}
	select {
	case cr := <-secondDone:
		if cr.Outcome != OutcomeError || !strings.Contains(cr.Error, "[[watch]]") || cr.Started.Before(ended) {
			t.Fatalf("the second replay: %+v, want it started after the first ended (%s)", cr, ended)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second replay never started after the first ended")
	}
	said := w.said()
	if len(said) != 1 || !strings.Contains(said[0], "case x") || !strings.Contains(said[0], "20261020-100000") {
		t.Fatalf("the waiting replay said %q, want once that case x waits for run 20261020-100000", said)
	}
	if _, ok := dblock.ReadHolder(caseLock(l, "x")); ok {
		t.Fatal("the lock file still names a holder after both replays ended")
	}
}

// A replay waiting for another replay of its case honors its context: cancelled, it ends at once
// with an error that says it was waiting, not at its next try of the lock, and starts nothing.
func TestAReplayWaitingForItsCaseStopsWhenCancelled(t *testing.T) {
	l := paths.Layout{Home: t.TempDir()}
	held, err := dblock.Acquire(context.Background(), caseLock(l, "x"), dblock.Holder{Command: "run 20261020-100000", PID: os.Getpid()}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	var w warnings
	d := Deps{Layout: l, Config: testConfig(t), Warn: w.warn}
	o := lockOptions(l, "20261020-100001")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan eval.CaseRun, 1)
	go func() { done <- RunCase(ctx, d, widgetsCase(), o) }()
	waitFor(t, "the replay to say it waits", func() bool { return len(w.said()) > 0 })
	cancel()
	select {
	case cr := <-done:
		if cr.Outcome != OutcomeError || !strings.Contains(cr.Error, "waiting for") || !strings.Contains(cr.Error, context.Canceled.Error()) {
			t.Fatalf("case run %+v, want an error saying the wait was cancelled", cr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled replay kept waiting for its case's lock")
	}
	if _, err := os.Stat(filepath.Join(o.RunDir, "cases")); !os.IsNotExist(err) {
		t.Fatalf("a scratch tree was made: %v", err)
	}
}
