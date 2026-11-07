package factory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// heldRig is the harness with a log the test reads (INFO and up) and a task
// at stage whose stage returns wait, as a stage does: it records the wait
// (waitOn) and returns a held error of who while wait is not "", a pass
// that is not done otherwise.
type heldRig struct {
	*harness
	log  *syncBuffer
	task registry.Task
	who  string
	wait string
}

func newHeldRig(t *testing.T, stage ...string) *heldRig {
	t.Helper()
	log := &syncBuffer{}
	h := newHarness(t, func(d *Deps) { d.Log = slog.New(slog.NewTextHandler(log, nil)) })
	at := "investigate"
	if len(stage) > 0 {
		at = stage[0]
	}
	r := &heldRig{harness: h, log: log, task: h.task("PR-12400", registry.TaskActive, at), who: waitAgent}
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		if r.wait == "" {
			return false, nil
		}
		h.e.waitOn(ctx, t, r.who, r.wait)
		return false, heldError{who: r.who, what: r.wait}
	})
	return r
}

// tickAfter moves the clock by d and runs a tick, which must return no
// error: a held wait is no failure of the tick.
func (r *heldRig) tickAfter(d time.Duration) {
	r.t.Helper()
	r.clock.Add(d)
	if err := r.e.Tick(r.ctx); err != nil {
		r.t.Fatalf("tick error %v, want none for a held wait", err)
	}
}

// lines are the log's lines at level whose message is msg.
func (r *heldRig) lines(level, msg string) []string {
	var out []string
	for line := range strings.SplitSeq(r.log.String(), "\n") {
		if strings.Contains(line, "level="+level+" ") && strings.Contains(line, fmt.Sprintf("msg=%q", msg)) {
			out = append(out, line)
		}
	}
	return out
}

// A held wait logs at INFO once, when it begins, naming the task, its stage
// and the wait; the ticks while it holds the same (a number that moves is
// the same wait) log nothing more and return no error, so the daemon's
// "factory tick" WARN never names it. Past heldWarnAfter it logs at WARN,
// with how long it has held, and again after twice as long each time
// (30 minutes, then 1 hour, then 2 hours later).
func TestAHeldWaitLogsAtInfoOnceAndAtWarnPastTheThreshold(t *testing.T) {
	r := newHeldRig(t)
	r.wait = "the fx-critic agent works in the copy (turn 1m)"
	r.tickAfter(0)
	for i := range 10 {
		r.wait = fmt.Sprintf("the fx-critic agent works in the copy (turn %dm)", i+2)
		r.tickAfter(45 * time.Second)
	}
	begun := r.lines("INFO", "a factory task waits")
	if len(begun) != 1 || !strings.Contains(begun[0], fmt.Sprintf("task=%d", r.task.ID)) || !strings.Contains(begun[0], "stage=investigate") ||
		!strings.Contains(begun[0], `wait="agent: the fx-critic agent works in the copy (turn 1m)"`) {
		t.Fatalf("INFO lines %q, want one that names the task, its stage and the wait:\n%s", begun, r.log.String())
	}
	if n := strings.Count(r.log.String(), "level=WARN"); n != 0 {
		t.Fatalf("%d WARN lines within the threshold, want none:\n%s", n, r.log.String())
	}

	r.tickAfter(heldWarnAfter - 10*45*time.Second) // heldWarnAfter since the wait began
	r.tickAfter(45 * time.Second)
	warned := r.lines("WARN", "a factory task still waits")
	if len(warned) != 1 || !strings.Contains(warned[0], fmt.Sprintf("task=%d", r.task.ID)) || !strings.Contains(warned[0], "for=30m0s") ||
		!strings.Contains(warned[0], "turn 11m") {
		t.Fatalf("WARN lines %q, want one past the threshold with how long the wait held:\n%s", warned, r.log.String())
	}
	r.tickAfter(heldWarnAfter)
	if warned = r.lines("WARN", "a factory task still waits"); len(warned) != 1 {
		t.Fatalf("WARN lines %q, want no second one a threshold later", warned)
	}
	r.tickAfter(heldWarnAfter)
	if warned = r.lines("WARN", "a factory task still waits"); len(warned) != 2 || !strings.Contains(warned[1], "for=1h30m45s") {
		t.Fatalf("WARN lines %q, want a second one twice the threshold after the first", warned)
	}
	r.tickAfter(heldWarnAfter + time.Hour)
	if warned = r.lines("WARN", "a factory task still waits"); len(warned) != 2 {
		t.Fatalf("WARN lines %q, want no third one 1h30m after the second", warned)
	}
	r.tickAfter(heldWarnAfter)
	if warned = r.lines("WARN", "a factory task still waits"); len(warned) != 3 || !strings.Contains(warned[2], "for=3h30m45s") {
		t.Fatalf("WARN lines %q, want a third one 2 hours after the second", warned)
	}
	if begun = r.lines("INFO", "a factory task waits"); len(begun) != 1 {
		t.Fatalf("INFO lines %q, want still the one", begun)
	}
}

// A wait's end logs at INFO with how long it held: the stage no longer
// holds, the wait says other words (which begins another wait), or the
// task leaves the live states.
func TestAHeldWaitsEndLogsHowLongItHeld(t *testing.T) {
	r := newHeldRig(t)
	r.wait = "you're in the fx-critic pane; the factory changes the copy once you leave it"
	r.tickAfter(0)
	r.wait = "magnum: a review round of magnum's runs; the local review yields to it"
	r.tickAfter(5 * time.Minute)
	ended := r.lines("INFO", "a factory task's wait ended")
	if len(ended) != 1 || !strings.Contains(ended[0], "fx-critic pane") || !strings.Contains(ended[0], "after=5m0s") {
		t.Fatalf("ended lines %q, want the first wait's after 5m:\n%s", ended, r.log.String())
	}
	if begun := r.lines("INFO", "a factory task waits"); len(begun) != 2 || !strings.Contains(begun[1], "review round") {
		t.Fatalf("begun lines %q, want the second wait's too", begun)
	}

	r.wait = ""
	r.tickAfter(2 * time.Minute)
	if ended = r.lines("INFO", "a factory task's wait ended"); len(ended) != 2 || !strings.Contains(ended[1], "after=2m0s") {
		t.Fatalf("ended lines %q, want the second wait's after 2m once the stage no longer holds", ended)
	}

	r.wait = "you're in the fx-critic pane; the factory changes the copy once you leave it"
	r.tickAfter(time.Minute)
	if err := r.reg.TransitionTask(r.ctx, r.task.ID, nil, registry.TaskNeedsYou, func(u *registry.TaskUpdate) { u.Because("test") }); err != nil {
		t.Fatal(err)
	}
	r.tickAfter(3 * time.Minute)
	if ended = r.lines("INFO", "a factory task's wait ended"); len(ended) != 3 || !strings.Contains(ended[2], "after=3m0s") {
		t.Fatalf("ended lines %q, want the third wait's once its task left the live states", ended)
	}
}

// A wait that held before a restart keeps its start: the daemon's memory of
// its waits is gone, but the task's wait span in the registry says when it
// began, so the wait logs at INFO once with that start and at WARN at once
// when it has held past heldWarnAfter.
func TestAHeldWaitKeepsItsStartAcrossARestart(t *testing.T) {
	r := newHeldRig(t)
	r.wait = "another heavy stage runs"
	r.tickAfter(0)
	r.e.helds = map[int64]*heldLog{} // a restart: the new daemon knows no wait
	r.tickAfter(40 * time.Minute)
	if begun := r.lines("INFO", "a factory task waits"); len(begun) != 2 || !strings.Contains(begun[1], "since=") {
		t.Fatalf("INFO lines %q, want the wait again after the restart, with its start", begun)
	}
	if warned := r.lines("WARN", "a factory task still waits"); len(warned) != 1 || !strings.Contains(warned[0], "for=40m0s") {
		t.Fatalf("WARN lines %q, want one at once: the wait began 40 minutes ago", warned)
	}
}

// The memory hold's wait is one wait while it holds, whatever the names of
// processes it gives (they change as the table is read again): no end and
// no new beginning in the log, and no new wait span in the registry.
func TestAMemoryHoldsWaitStaysOneWaitWhileItsNamesChange(t *testing.T) {
	r := newHeldRig(t)
	r.who = HoldMemory
	r.wait = "swap 97% used, launches wait below 80%; largest: codex 2.5 GB (pid 90)"
	r.tickAfter(0)
	spans := func() int {
		ss, err := r.reg.Spans(r.ctx, r.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return len(ss)
	}
	before := spans()
	r.wait = "swap 98% used, launches wait below 80%; largest: Slack 3.1 GB (pid 77); Codex app-server broker: pid 455 (3d)"
	r.tickAfter(10 * time.Minute)
	if ended := r.lines("INFO", "a factory task's wait ended"); len(ended) != 0 {
		t.Errorf("ended lines %q, want none: the memory hold still holds", ended)
	}
	if begun := r.lines("INFO", "a factory task waits"); len(begun) != 1 {
		t.Errorf("INFO lines %q, want the one", begun)
	}
	if n := spans(); n != before {
		t.Errorf("%d spans after the names changed, want %d", n, before)
	}
	if got := r.reload(r.task.ID).WaitsWhat; !strings.Contains(got, "Slack") {
		t.Errorf("waits_what %q, want the new names", got)
	}
}

// A new task's first wait, whose stage advance records in the same pass,
// is the same wait on the next tick: no end and no new beginning.
func TestANewTasksFirstWaitIsOneWait(t *testing.T) {
	r := newHeldRig(t, "")
	r.wait = "the fx-repro agent cannot take its prompt yet"
	r.tickAfter(0)
	r.tickAfter(45 * time.Second)
	if ended := r.lines("INFO", "a factory task's wait ended"); len(ended) != 0 {
		t.Errorf("ended lines %q, want none", ended)
	}
	if begun := r.lines("INFO", "a factory task waits"); len(begun) != 1 {
		t.Errorf("INFO lines %q, want one", begun)
	}
}
