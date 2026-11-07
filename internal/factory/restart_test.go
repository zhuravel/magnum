package factory

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// work reads the record of the work a restart would cut as `magnum factory
// install` reads it, and fails the test when there is none.
func (h *harness) workRecord() WorkRecord {
	h.t.Helper()
	rec, ok, err := ReadWork(h.layout)
	if err != nil || !ok {
		h.t.Fatalf("the work record: ok %v, %v", ok, err)
	}
	return rec
}

// whats are the record's items as kind: what.
func whats(rec WorkRecord) []string {
	var out []string
	for _, w := range rec.Work {
		out = append(out, w.Kind+": "+w.What)
	}
	return out
}

// installDrain writes the drain `magnum factory install --drain` asks for,
// started now by pid, and returns its value.
func (h *harness) installDrain(pid int) string {
	h.t.Helper()
	value := engine.Drain{Since: h.clock.Now(), PID: pid}.Value()
	if err := WriteDrain(h.layout, value); err != nil {
		h.t.Fatal(err)
	}
	return value
}

// drainTexts are the texts of the daemon's drain events.
func (h *harness) drainTexts() []string {
	h.t.Helper()
	var out []string
	for _, ev := range h.events(0, "drain") {
		out = append(out, ev.Text)
	}
	return out
}

// The daemon keeps a record of the work a restart would cut, for `magnum
// factory install` to read: a claim of a copy and a talk with a liaison that
// run off the loop are in it, under the daemon's pid, while they run, and
// leave it once they ended.
func TestTheWorkRecordNamesTheClaimAndTheTalkThatRun(t *testing.T) {
	h := newHarness(t)
	h.copies.block = make(chan struct{})
	claimer := h.task("PR-12390", registry.TaskActive, "investigate")
	talker := h.task("PR-12391", registry.TaskNeedsYou, "report")
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		_, err := h.e.claimCopy(ctx, t)
		return false, err
	})
	started, release := h.blockedTalk()
	h.askTalk(talker.ID)

	h.tickEnds(20 * time.Second)
	started()

	rec := h.workRecord()
	if rec.PID != os.Getpid() {
		t.Fatalf("the record names pid %d, want the daemon's %d", rec.PID, os.Getpid())
	}
	want := []string{"talk: talk with the liaison of PR-12391", "claim: claim of a copy for PR-12390"} // the requests come first
	if got := whats(rec); !slices.Equal(got, want) {
		t.Fatalf("the record holds %q, want %q", got, want)
	}
	for _, w := range rec.Work {
		if w.Since.IsZero() || (w.Kind == WorkClaim && w.Task != claimer.ID) || (w.Kind == WorkTalk && w.Task != talker.ID) {
			t.Fatalf("item %+v: want its task and its start", w)
		}
	}

	close(h.copies.block)
	release()
	h.e.waitBackground()
	if got := whats(h.workRecord()); len(got) != 0 {
		t.Fatalf("the record holds %q after the claim and the talk ended, want nothing", got)
	}
}

// A check of a task's head (the factory's proof and the precheck) is in the
// record while it runs.
func TestTheWorkRecordNamesTheCheckThatRuns(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12392", "bug")
	r.startStage(task)
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.clock.Add(time.Minute)
	r.fix(task)
	block := make(chan struct{})
	r.blocks["bin/lint"] = block
	r.ended(task, "fx-writer")
	r.tick()
	<-r.entered

	if got := whats(r.workRecord()); !slices.Equal(got, []string{"check: proof and precheck of PR-12392"}) {
		t.Fatalf("the record holds %q, want the check that runs", got)
	}
	close(block)
	r.e.waitBackground()
	if got := whats(r.workRecord()); len(got) != 0 {
		t.Fatalf("the record holds %q after the check ended, want nothing", got)
	}
}

// A round of the local review is in the record while it runs: a restart
// loses its agents' turns and runs it again from its start.
func TestTheWorkRecordNamesTheLocalRoundThatRuns(t *testing.T) {
	r := newReviewRig(t, result())
	r.rv.block = make(chan struct{})
	task, _ := r.reviewTask("PR-12393")
	r.startBlockedRound()

	if got := whats(r.workRecord()); !slices.Equal(got, []string{"round: local review round 1 of PR-12393"}) {
		t.Fatalf("the record holds %q, want the round that runs", got)
	}
	close(r.rv.block)
	r.e.waitBackground()
	if got := whats(r.workRecord()); len(got) != 0 {
		t.Fatalf("the record holds %q after the round of %s ended, want nothing", got, TaskKey(task))
	}
}

// The drain `magnum factory install --drain` asks for holds what a restart
// would cut and the launches and effects, as a stop's drain does, while
// the daemon goes on: the tick that takes it says so in the record (install
// restarts only once it does), a claim, a talk, a launch and an effect wait,
// and once the command lifts it they start again.
func TestAnInstallDrainHoldsNewWorkUntilItIsLifted(t *testing.T) {
	h := newHarness(t)
	h.e.drainerAlive = func(int) bool { return true }
	claimer := h.task("PR-12394", registry.TaskActive, "investigate")
	talker := h.task("PR-12395", registry.TaskNeedsYou, "report")
	var claimErr, launchErr, effectErr error
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		_, claimErr = h.e.claimCopy(ctx, t)
		launchErr = h.e.launch(ctx, t, registry.Attempt{}, Copy{}, nil)
		_, _, effectErr = h.e.beginEffect(ctx, "publish:9:abc", "publish", t.ID)
		return false, nil
	})
	talk := h.askTalk(talker.ID)
	value := h.installDrain(4242)

	h.tickAndWait()

	if rec := h.workRecord(); rec.Drain != value || len(rec.Work) != 0 {
		t.Fatalf("the record says drain %q and work %q, want the drain held and nothing running", rec.Drain, whats(rec))
	}
	if !errors.Is(claimErr, ErrHeld) || !errors.Is(launchErr, ErrHeld) || !errors.Is(effectErr, ErrHeld) {
		t.Fatalf("claim %v, launch %v, effect %v: want all held by the drain", claimErr, launchErr, effectErr)
	}
	if n := h.copies.claimCount(); n != 0 {
		t.Fatalf("%d claims ran during the drain", n)
	}
	if q := h.request(talk); q.State != registry.RequestPending || len(h.rt.talkCalls()) != 0 {
		t.Fatalf("the talk request is %s (%d Talk calls) during the drain, want pending and no Talk", q.State, len(h.rt.talkCalls()))
	}
	if got := h.reload(claimer.ID); !isLive(got) {
		t.Fatalf("the drain moved %s to %s: %q", TaskKey(got), got.State, got.WaitsWhat)
	}
	if got := h.drainTexts(); len(got) != 1 || !strings.Contains(got[0], "pid 4242") || !strings.Contains(got[0], "nothing new starts") {
		t.Fatalf("drain events %q, want one that says the drain holds and whose it is", got)
	}

	if err := LiftDrain(h.layout, value); err != nil {
		t.Fatal(err)
	}
	h.tickAndWait()

	if rec := h.workRecord(); rec.Drain != "" {
		t.Fatalf("the record still says drain %q after the command lifted it", rec.Drain)
	}
	if n := h.copies.claimCount(); n != 1 {
		t.Fatalf("%d claims after the drain was lifted, want the held one run", n)
	}
	if q := h.request(talk); q.State != registry.RequestDone {
		t.Fatalf("the talk request is %s (%s) after the drain was lifted, want done", q.State, q.Result)
	}
	if got := h.drainTexts(); len(got) != 2 || !strings.Contains(got[1], "lifted") {
		t.Fatalf("drain events %q, want the lift recorded", got)
	}
}

// A drain whose command is gone (its terminal closed) is lifted at the next
// tick: no restart follows it, so the work it held starts again.
func TestADrainWhoseCommandIsGoneIsLifted(t *testing.T) {
	h := newHarness(t)
	h.e.drainerAlive = func(pid int) bool { return pid != 4243 }
	task := h.task("PR-12396", registry.TaskActive, "investigate")
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		_, err := h.e.claimCopy(ctx, t)
		return false, err
	})
	h.installDrain(4243)

	h.tickAndWait()

	if v, err := ReadDrain(h.layout); err != nil || v != "" {
		t.Fatalf("drain.json holds %q (%v) after its command was gone, want it removed", v, err)
	}
	if rec := h.workRecord(); rec.Drain != "" {
		t.Fatalf("the record says drain %q, want none", rec.Drain)
	}
	if n := h.copies.claimCount(); n != 1 {
		t.Fatalf("%d claims for %s, want it to run: the drain is gone", n, TaskKey(task))
	}
	if got := h.drainTexts(); len(got) != 1 || !strings.Contains(got[0], "pid 4243") || !strings.Contains(got[0], "is gone") {
		t.Fatalf("drain events %q, want the lift and why", got)
	}
}

// The daemon that starts after `magnum factory install --drain` is the one
// it waited for: it ends the drain, starts its work, keeps its record from
// its start and removes the record as it stops.
func TestTheRestartEndsTheInstallDrain(t *testing.T) {
	h := newHarness(t)
	h.e.drainerAlive = func(int) bool { return true }
	h.task("PR-12397", registry.TaskActive, "investigate")
	h.installDrain(4244)
	ticked := make(chan struct{}, 1)
	h.stage(config.FactoryStageAgents, func(context.Context, registry.Task, config.FactoryStage) (bool, error) {
		select {
		case ticked <- struct{}{}:
		default:
		}
		return false, nil
	})
	sigs := h.captureSignals()
	done := make(chan error, 1)
	go func() { done <- h.e.Run(h.ctx) }()
	sig := <-sigs
	select {
	case <-ticked:
	case <-time.After(10 * time.Second):
		t.Fatal("the restarted daemon advanced no task: the drain still holds it")
	}
	if v, err := ReadDrain(h.layout); err != nil || v != "" {
		t.Fatalf("drain.json holds %q (%v) after the restart, want it ended", v, err)
	}
	if rec := h.workRecord(); rec.PID != os.Getpid() || rec.Drain != "" {
		t.Fatalf("the record %+v, want this daemon's, holding no drain", rec)
	}
	if got := h.drainTexts(); len(got) != 1 || !strings.Contains(got[0], "restarted") {
		t.Fatalf("drain events %q, want the drain ended by the restart", got)
	}
	sig <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
	if _, ok, err := ReadWork(h.layout); ok || err != nil {
		t.Fatalf("the record is still there after the daemon stopped (%v)", err)
	}
}
