package factory

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/store"
)

// TurnsInFlight is the runtime's, from the runs Send made that are still
// pending or working: the drain for a reboot waits for them.
func (f *fakeRuntime) TurnsInFlight(context.Context) ([]runtime.TurnInFlight, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtime.TurnInFlight
	for _, p := range f.sent {
		switch f.runState[p.RunID].state {
		case store.RunPending, store.RunSubmitted, store.RunWorking:
			out = append(out, runtime.TurnInFlight{TaskID: p.TaskID, Role: p.Role, Liaison: p.Role == runtime.LiaisonRole})
		}
	}
	return out, nil
}

// endRuns ends every run of role, as the agent's turn ending does.
func (f *fakeRuntime) endRuns(role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.sent {
		if p.Role == role {
			f.setRunLocked(p.RunID, store.RunEnded, "")
		}
	}
}

// parkList are the tasks Park was called for so far.
func (f *fakeRuntime) parkList() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.parks)
}

// sendTo sends a prompt to role of task t in a new attempt of stage, as a
// stage does: its run works until endRuns.
func (h *harness) sendTo(t registry.Task, stage, role string) {
	h.t.Helper()
	a, err := h.reg.BeginAttempt(h.ctx, t.ID, stage)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.rt.Send(h.ctx, t, a, config.Role{Name: role}, "go on"); err != nil {
		h.t.Fatal(err)
	}
}

// pause writes the pause `magnum pause` asks of the factory, from now until
// until (zero: until resume).
func (h *harness) pause(until time.Time, reason string) Pause {
	h.t.Helper()
	p := Pause{Generation: 1, Since: h.clock.Now(), Until: until, Reason: reason}
	if err := WritePause(h.layout, p.Value()); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// rebootDrain writes the drain for a reboot `magnum drain` asks of the
// factory, of generation gen, begun now on boot.
func (h *harness) rebootDrain(gen int64, until string, boot time.Time) engine.RebootDrain {
	h.t.Helper()
	d := engine.RebootDrain{Generation: gen, Since: h.clock.Now(), Until: until, Boot: boot}
	if err := WriteRebootDrain(h.layout, d.Value()); err != nil {
		h.t.Fatal(err)
	}
	return d
}

// holdKinds are the kinds of the holds holds.json shows.
func (h *harness) holdKinds() []string {
	h.t.Helper()
	hs, err := ReadHolds(h.layout)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, x := range hs {
		out = append(out, x.Kind)
	}
	return out
}

// `magnum pause` holds the factory's new automatic work: a launch (its task
// waits on the pause, saying it), a claim of a copy, a new local review
// round, a check and a QA run (startsHeld); your push (an effect) and your
// talk still run. holds.json shows the pause.
func TestAFactoryPauseHoldsNewWorkAndLetsYourPushAndTalkRun(t *testing.T) {
	h := newHarness(t)
	worker := h.task("PR-12400", registry.TaskActive, "investigate")
	talker := h.task("PR-12401", registry.TaskNeedsYou, "report")
	var claimErr, launchErr, effectErr error
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		_, claimErr = h.e.claimCopy(ctx, t)
		launchErr = h.e.launch(ctx, t, registry.Attempt{}, Copy{}, nil)
		_, _, effectErr = h.e.beginEffect(ctx, "publish:9:abc", "publish", t.ID)
		return false, nil
	})
	talk := h.askTalk(talker.ID)
	h.pause(time.Time{}, "lunch")

	h.tickAndWait()

	if !errors.Is(claimErr, ErrHeld) || !errors.Is(launchErr, ErrHeld) {
		t.Fatalf("claim %v, launch %v: want both held by the pause", claimErr, launchErr)
	}
	if effectErr != nil {
		t.Fatalf("effect %v: want your push to go out under a pause", effectErr)
	}
	if !h.e.startsHeld() || h.e.drainsHeld() {
		t.Fatal("the pause must hold the rounds, the checks and the QA runs (startsHeld), and no effect or talk (drainsHeld)")
	}
	if q := h.request(talk); q.State != registry.RequestDone {
		t.Fatalf("the talk is %s under the pause, want done", q.State)
	}
	if got := h.reload(worker.ID); got.WaitsWho != HoldPause || !strings.Contains(got.WaitsWhat, "paused by you") ||
		!strings.Contains(got.WaitsWhat, "(lunch)") {
		t.Fatalf("the task waits on %q: %q, want the pause", got.WaitsWho, got.WaitsWhat)
	}
	if got := h.holdKinds(); !slices.Contains(got, HoldPause) {
		t.Fatalf("holds.json shows %q, want the pause", got)
	}
	if who, _, ok := h.e.MayLaunch(); !ok {
		t.Fatalf("the runtime's starts outside a launch are held by %q: a pause holds nothing you ask for", who)
	}
}

// The pause lives in its file, so a restart keeps it; the first tick after
// its end lifts it, and the launches go again.
func TestAFactoryPauseSurvivesARestartAndEndsAtItsUntil(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12402", registry.TaskActive, "investigate")
	var launchErr error
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		launchErr = h.e.launch(ctx, t, registry.Attempt{}, Copy{}, nil)
		return false, nil
	})
	h.pause(h.clock.Now().Add(time.Hour), "")
	h.tickAndWait()
	if !errors.Is(launchErr, ErrHeld) {
		t.Fatalf("launch %v, want held", launchErr)
	}

	h.e = New(h.deps) // the daemon restarts
	h.stage(config.FactoryStageAgents, func(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		launchErr = h.e.launch(ctx, t, registry.Attempt{}, Copy{}, nil)
		return false, nil
	})
	h.clock.Add(30 * time.Minute)
	h.tickAndWait()
	if !errors.Is(launchErr, ErrHeld) {
		t.Fatalf("launch %v after the restart, want still held", launchErr)
	}

	h.clock.Add(31 * time.Minute)
	h.tickAndWait()
	if launchErr != nil {
		t.Fatalf("launch %v after the pause's end, want it to go", launchErr)
	}
	if v, err := ReadPause(h.layout); err != nil || v != "" {
		t.Fatalf("pause.json holds %q (%v) after its end, want it removed", v, err)
	}
	if got := h.reload(task.ID); got.WaitsWho == HoldPause {
		t.Fatalf("the task still waits on the pause: %q", got.WaitsWhat)
	}
	var texts []string
	for _, ev := range h.events(0, "pause") {
		texts = append(texts, ev.Text)
	}
	if len(texts) != 3 || !strings.Contains(texts[2], "ended at its time") {
		t.Fatalf("pause events %q, want held (twice: once per daemon) then ended", texts)
	}
}

// The factory's drain for a reboot stays pending while a writer works, though
// work.json names nothing: a reboot would cut the turn. Once the turn ended
// it is quiet, every task's idle agents are parked once, and the
// acknowledgement says so.
func TestTheFactoryDrainForARebootWaitsForAWritersTurnThenParks(t *testing.T) {
	h := newHarness(t)
	writer := h.task("PR-12403", registry.TaskActive, "coding")
	waiting := h.task("PR-12404", registry.TaskNeedsYou, "report")
	h.stage(config.FactoryStageAgents, idleStage)
	h.sendTo(writer, "coding", "fx-writer")
	d := h.rebootDrain(21, engine.DrainUntilBoot, time.Unix(1762930000, 0))

	h.tickAndWait()

	rec := h.workRecord()
	if len(rec.Work) != 0 || rec.Controls != engine.ControlsVersion {
		t.Fatalf("work.json holds %q (controls %d), want no work and this build's controls", whats(rec), rec.Controls)
	}
	ack := rec.RebootDrain
	if ack == nil || ack.Generation != d.Generation || ack.PID != os.Getpid() || ack.Quiet ||
		!slices.Equal(ack.Work, []string{"fx-writer turn of PR-12403"}) {
		t.Fatalf("acknowledgement %+v, want generation 21, not quiet, naming the writer's turn", ack)
	}
	if n := len(h.rt.parkList()); n != 0 {
		t.Fatalf("%d parks while the writer works", n)
	}
	if _, _, ok := h.e.MayPrompt(); ok {
		t.Fatal("a new prompt may go out under the drain")
	}

	h.rt.endRuns("fx-writer")
	h.tickAndWait()
	h.tickAndWait()

	if ack := h.workRecord().RebootDrain; ack == nil || !ack.Done(d.Generation, os.Getpid()) {
		t.Fatalf("acknowledgement %+v, want quiet and parked", ack)
	}
	parks := h.rt.parkList()
	if !slices.Contains(parks, writer.ID) || !slices.Contains(parks, waiting.ID) || len(slices.Compact(slices.Sorted(slices.Values(parks)))) != len(parks) {
		t.Fatalf("parked %v, want %d and %d, each once", parks, writer.ID, waiting.ID)
	}
	if got := h.holdKinds(); !slices.Contains(got, HoldReboot) {
		t.Fatalf("holds.json shows %q, want the drain", got)
	}
}

// The liaison's turn counts too: a talk in flight holds the drain.
func TestTheFactoryDrainForARebootWaitsForTheLiaisonsTurn(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12406", registry.TaskNeedsYou, "report")
	h.stage(config.FactoryStageAgents, idleStage)
	h.sendTo(task, "report", runtime.LiaisonRole)
	h.rebootDrain(22, engine.DrainUntilBoot, time.Time{})
	h.tickAndWait()
	if ack := h.workRecord().RebootDrain; ack == nil || ack.Quiet || !slices.Equal(ack.Work, []string{"liaison turn of PR-12406"}) {
		t.Fatalf("acknowledgement %+v, want the liaison's turn named", ack)
	}
}

// A drain for a reboot outlives the daemon's restarts on the same boot and
// ends at the first start on another boot; a drain until "resume" stays.
// `magnum resume` (the file gone) drops the acknowledgement and the work
// starts again.
func TestTheFactoryDrainForARebootEndsAtTheFirstStartOnANewBoot(t *testing.T) {
	boot := time.Unix(1762930000, 0)
	h := newHarness(t, func(d *Deps) { d.BootTime = func(context.Context) (time.Time, error) { return boot, nil } })
	h.rebootDrain(23, engine.DrainUntilBoot, boot)
	h.e.endRebootDrainAtStart(h.ctx) // a crash restart on the same boot
	if v, _ := ReadRebootDrain(h.layout); v == "" {
		t.Fatal("a start on the same boot ended the drain")
	}

	boot = boot.Add(10 * time.Hour) // the Mac rebooted
	h.e = New(h.deps)
	h.e.endRebootDrainAtStart(h.ctx)
	if v, _ := ReadRebootDrain(h.layout); v != "" {
		t.Fatalf("the first start after the reboot kept the drain %q", v)
	}
	var texts []string
	for _, ev := range h.events(0, "drain") {
		texts = append(texts, ev.Text)
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "rebooted") {
		t.Fatalf("drain events %q", texts)
	}

	h.rebootDrain(24, engine.DrainUntilResume, boot.Add(-time.Hour))
	h.e.endRebootDrainAtStart(h.ctx)
	if v, _ := ReadRebootDrain(h.layout); v == "" {
		t.Fatal("a drain until resume ended at a start on another boot")
	}
	h.stage(config.FactoryStageAgents, idleStage)
	h.tickAndWait()
	if h.workRecord().RebootDrain == nil {
		t.Fatal("no acknowledgement")
	}
	if err := LiftRebootDrain(h.layout, ""); err != nil { // magnum resume
		t.Fatal(err)
	}
	h.tickAndWait()
	if ack := h.workRecord().RebootDrain; ack != nil {
		t.Fatalf("acknowledgement %+v after the resume, want none", ack)
	}
	if h.e.drainsHeld() {
		t.Fatal("the daemon still holds new work after the resume")
	}
}

// A restart's drain and a drain for a reboot are two files: the start ends
// the restart's alone, and the lift of either leaves the other.
func TestTheFactorysRestartDrainAndRebootDrainDoNotLiftEachOther(t *testing.T) {
	boot := time.Unix(1762930000, 0)
	h := newHarness(t, func(d *Deps) { d.BootTime = func(context.Context) (time.Time, error) { return boot, nil } })
	h.e.drainerAlive = func(Drain) bool { return true }
	h.stage(config.FactoryStageAgents, idleStage)
	h.rebootDrain(25, engine.DrainUntilBoot, boot)
	h.installDrain(4250)
	h.e.endDrainAtStart(h.ctx)
	h.e.endRebootDrainAtStart(h.ctx)
	if v, _ := ReadDrain(h.layout); v != "" {
		t.Fatal("the start kept the restart's drain")
	}
	if v, _ := ReadRebootDrain(h.layout); v == "" {
		t.Fatal("the start ended the drain for a reboot with the restart's")
	}
	restart := h.installDrain(4251)
	if err := LiftRebootDrain(h.layout, ""); err != nil {
		t.Fatal(err)
	}
	h.tickAndWait()
	if rec := h.workRecord(); rec.Drain != restart {
		t.Fatalf("work.json holds drain %q after the reboot's was lifted, want the restart's", rec.Drain)
	}
}

// An hour quiet under the drain with no reboot is a NEEDS YOU blocker, with
// its one alert.
func TestAQuietFactoryDrainForARebootSaysOnceAfterAnHour(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	h.rebootDrain(26, engine.DrainUntilBoot, time.Time{})
	h.tickAndWait()
	h.clock.Add(59 * time.Minute)
	h.tickAndWait()
	if len(h.events(0, "alert")) != 0 {
		t.Fatal("an alert before an hour")
	}
	h.clock.Add(2 * time.Minute)
	h.tickAndWait()
	h.tickAndWait()
	alerts := h.events(0, "alert")
	if len(alerts) != 1 || !strings.Contains(alerts[0].Text, "drain for a reboot still holds") {
		t.Fatalf("alerts %+v, want one", alerts)
	}
}
