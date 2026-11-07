package factory

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
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
	value := Drain{Since: h.clock.Now(), PID: pid}.Value()
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
// record while it runs, and until a tick took its result.
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
	r.tick()
	if got := whats(r.workRecord()); len(got) != 0 {
		t.Fatalf("the record holds %q after a tick took the check's result, want nothing", got)
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
	h.e.drainerAlive = func(Drain) bool { return true }
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
	h.e.drainerAlive = func(d Drain) bool { return d.PID != 4243 }
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
	h.e.drainerAlive = func(Drain) bool { return true }
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

// A check the drain waited for is the drain's until a tick took its result:
// the result lives only in the daemon's memory, so a restart right after
// the check ended would run the proof and the precheck again. The end of a
// check wakes the loop, and the tick that takes the result ends its item.
func TestADrainWaitsForTheCheckUntilATickTakesItsResult(t *testing.T) {
	r := newCodingRig(t)
	r.e.drainerAlive = func(Drain) bool { return true }
	task := r.codingTask("PR-12398", "bug")
	r.startStage(task)
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.clock.Add(time.Minute)
	r.fix(task)
	block := make(chan struct{})
	r.blocks["bin/lint"] = block
	r.ended(task, "fx-writer")
	r.tick()
	<-r.entered
	value := r.installDrain(4245)
	r.tick()
	want := []string{"check: proof and precheck of PR-12398"}
	if rec := r.workRecord(); rec.Drain != value || !slices.Equal(whats(rec), want) {
		t.Fatalf("the record says drain %q, work %q; want the drain held and the check", rec.Drain, whats(rec))
	}
	select {
	case <-r.e.kick:
	default:
	}

	close(block)
	r.e.waitBackground()

	if got := whats(r.workRecord()); !slices.Equal(got, want) {
		t.Fatalf("the record holds %q once the check ended, before any tick took its result; want %q", got, want)
	}
	select {
	case <-r.e.kick:
	default:
		t.Fatal("the end of the check did not wake the loop")
	}
	r.tick()
	if got := whats(r.workRecord()); len(got) != 0 {
		t.Fatalf("the record holds %q after the tick took the check's result, want nothing", got)
	}
	if got := r.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("task at %s after the tick, want local_review: the result was taken", got.Stage)
	}
}

// The install drain is a hold of the machine while it holds: holds.json,
// which the screen draws, names it, whose it is and since when, and it
// leaves holds.json once the drain is lifted.
func TestAnInstallDrainIsAHoldTheScreenDraws(t *testing.T) {
	h := newHarness(t)
	h.e.drainerAlive = func(Drain) bool { return true }
	value := h.installDrain(4242)

	h.tick()

	hs, err := ReadHolds(h.layout)
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 1 || hs[0].Kind != HoldDrain || !strings.Contains(hs[0].Reason, "pid 4242") ||
		!strings.Contains(hs[0].Reason, "nothing new starts") || hs[0].Since.IsZero() {
		t.Fatalf("holds %+v, want the drain's, whose it is and since when", hs)
	}
	if err := LiftDrain(h.layout, value); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if hs, err := ReadHolds(h.layout); err != nil || len(hs) != 0 {
		t.Fatalf("holds %+v (%v) after the lift, want none", hs, err)
	}
}

// A drain.json the daemon cannot read is no drain: the work goes on, and
// the daemon says so once, not on every tick.
func TestAnUnreadableDrainIsIgnoredWithOneWarning(t *testing.T) {
	log := &syncBuffer{}
	h := newHarness(t, func(d *Deps) { d.Log = slog.New(slog.NewTextHandler(log, nil)) })
	h.task("PR-12399", registry.TaskActive, "investigate")
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		_, err := h.e.claimCopy(ctx, t)
		return false, err
	})
	if err := WriteDrain(h.layout, "not a drain"); err != nil {
		t.Fatal(err)
	}

	h.tickAndWait()
	h.tickAndWait()

	if n := h.copies.claimCount(); n != 1 {
		t.Fatalf("%d claims, want the claim run: an unreadable drain holds nothing", n)
	}
	if rec := h.workRecord(); rec.Drain != "" {
		t.Fatalf("the record says drain %q, want none", rec.Drain)
	}
	if n := strings.Count(log.String(), "drain.json"); n != 1 {
		t.Fatalf("the log names drain.json %d times, want one warning:\n%s", n, log.String())
	}
}

// A drain names its command by pid and start: a pid that another process
// took since (the factory's agents run magnum too) is not the command, so
// the drain is lifted; the command itself still holds it.
func TestADrainWhosePidWasReusedIsLifted(t *testing.T) {
	h := newHarness(t)
	h.e.procStart = func(int) (string, error) { return "Sat Nov  7 10:00:00 2026", nil }
	held := Drain{Since: h.clock.Now(), PID: 4246, Start: "Sat Nov  7 10:00:00 2026"}.Value()
	if err := WriteDrain(h.layout, held); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if rec := h.workRecord(); rec.Drain != held {
		t.Fatalf("the record says drain %q, want the command's own held", rec.Drain)
	}

	reused := Drain{Since: h.clock.Now(), PID: 4246, Start: "Sat Nov  7 09:00:00 2026"}.Value()
	if err := WriteDrain(h.layout, reused); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if v, err := ReadDrain(h.layout); err != nil || v != "" {
		t.Fatalf("drain.json holds %q (%v), want it lifted: pid 4246 is another process now", v, err)
	}
	if rec := h.workRecord(); rec.Drain != "" {
		t.Fatalf("the record says drain %q, want none", rec.Drain)
	}
}

// A record the daemon cannot write is removed, not left stale: install then
// cannot tell what runs and fails closed. The next write that goes through
// writes it again.
func TestAFailedWriteOfTheRecordRemovesIt(t *testing.T) {
	h := newHarness(t)
	h.copies.block = make(chan struct{})
	h.task("PR-12400", registry.TaskActive, "investigate")
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		_, err := h.e.claimCopy(ctx, t)
		return false, err
	})
	h.tick()
	if got := whats(h.workRecord()); len(got) != 1 {
		t.Fatalf("the record holds %q, want the claim", got)
	}

	h.e.workWrite = func(string, []byte) error { return errors.New("no space left on device") }
	close(h.copies.block)
	h.e.waitBackground()
	if _, ok, err := ReadWork(h.layout); ok || err != nil {
		t.Fatalf("a record is still there after its write failed (%v): install would trust it", err)
	}

	h.e.workWrite = nil
	h.tick()
	if _, ok, err := ReadWork(h.layout); !ok || err != nil {
		t.Fatalf("no record after a write went through again (%v)", err)
	}
}
