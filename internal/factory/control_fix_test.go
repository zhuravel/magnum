package factory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// liveRuntime is the fake runtime that says which tasks have live agents.
type liveRuntime struct {
	*fakeRuntime
	live map[int64]bool
}

func (r *liveRuntime) LiveAgents(_ context.Context, t registry.Task) (bool, error) {
	return r.live[t.ID], nil
}

// The drain parks the agents of every task that has a live one, whatever
// its state: a queued task can have a liaison (a talk), and one that ended
// keeps the liaison its end left live while you were in its pane, into which
// you can type a turn. A task with no live agent is not parked.
func TestTheFactoryDrainParksEveryTaskWithALiveAgent(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	byState := map[string]registry.Task{}
	for i, state := range []string{registry.TaskActive, registry.TaskWaiting, registry.TaskNeedsYou, registry.TaskPublished,
		registry.TaskDone, registry.TaskStopped, registry.TaskFailed, registry.TaskQueued} {
		byState[state] = h.task("PR-1250"+string(rune('0'+i)), state, "report")
	}
	idle := h.task("PR-12599", registry.TaskNeedsYou, "report")
	live := map[int64]bool{}
	for _, task := range byState {
		live[task.ID] = true
	}
	h.e.d.Runtime = &liveRuntime{fakeRuntime: h.rt, live: live}
	h.rebootDrain(80, engine.DrainUntilBoot, factoryBoot)
	h.tickAndWait()

	parks := h.rt.parkList()
	for state, task := range byState {
		if !slices.Contains(parks, task.ID) {
			t.Errorf("a %s task with a live agent was not parked", state)
		}
	}
	if slices.Contains(parks, idle.ID) {
		t.Error("a task with no live agent was parked")
	}
	if ack := h.workRecord().RebootDrain; ack == nil || !ack.Parked {
		t.Fatalf("acknowledgement %+v, want parked", ack)
	}
}

// A park that keeps failing does not keep the drain from saying parked for
// good: after its tries the task is left, said once with what the reboot
// does to it, and the acknowledgement names it.
func TestAFactoryParkThatKeepsFailingIsLeftNamed(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	task := h.task("PR-12600", registry.TaskNeedsYou, "report")
	h.rt.mu.Lock()
	h.rt.parkErr = errors.New("herdr: pane p_9 does not answer")
	h.rt.mu.Unlock()
	d := h.rebootDrain(81, engine.DrainUntilBoot, factoryBoot)
	for range parkTries - 1 {
		h.tickAndWait()
		if ack := h.workRecord().RebootDrain; ack == nil || ack.Parked {
			t.Fatalf("acknowledgement %+v before the tries ran out, want not parked", ack)
		}
	}
	h.tickAndWait()
	h.tickAndWait()
	ack := h.workRecord().RebootDrain
	if ack == nil || !ack.Done(d.Generation, os.Getpid()) || len(ack.Left) != 1 || !strings.Contains(ack.Left[0], TaskKey(task)) ||
		!strings.Contains(ack.Left[0], "does not answer") {
		t.Fatalf("acknowledgement %+v, want parked with the task left, named with why", ack)
	}
	var left []string
	for _, ev := range h.events(task.ID, "park") {
		if strings.Contains(ev.Text, "left") {
			left = append(left, ev.Text)
		}
	}
	if len(left) != 1 {
		t.Fatalf("the left park was said %d times (%q), want once", len(left), left)
	}
}

// A write of work.json that failed leaves no acknowledgement in memory
// only: the next tick writes it again.
func TestTheFactorysAcknowledgementIsWrittenAgainAfterAFailedWrite(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	var mu sync.Mutex
	fail := true
	h.e.workWrite = func(path string, b []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if fail && strings.Contains(string(b), "reboot_drain") {
			fail = false
			return syscall.ENOSPC
		}
		return os.WriteFile(path, b, 0o600)
	}
	d := h.rebootDrain(82, engine.DrainUntilBoot, factoryBoot)
	h.tickAndWait()
	h.tickAndWait()
	if ack := h.workRecord().RebootDrain; ack == nil || ack.Generation != d.Generation {
		t.Fatalf("acknowledgement %+v after a failed write, want it written again", ack)
	}
}

// Under the factory's pause your retry still lets the task go on, and its
// answer says that its next launch, claim or check waits for the pause.
func TestARetryUnderTheFactorysPauseSaysItWaitsForThePause(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12601", "bug", "coding")
	if err := h.reg.TransitionTask(h.ctx, task.ID, []string{registry.TaskActive}, registry.TaskNeedsYou, func(u *registry.TaskUpdate) {
		u.Set("waits_who", waitsYou)
		u.Set("waits_what", "the writer stopped")
	}); err != nil {
		t.Fatal(err)
	}
	h.pause(time.Time{}, "lunch")
	h.e.takeControls(h.ctx)
	qid, err := h.reg.EnqueueRequest(h.ctx, ReqRetry, RetryPayload{TaskID: task.ID, Surface: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	h.e.handleRequests(h.ctx)
	q, err := h.reg.RequestByID(h.ctx, qid)
	if err != nil || q.State != registry.RequestDone || !strings.Contains(q.Result, "goes on") || !strings.Contains(q.Result, "magnum resume") ||
		!strings.Contains(q.Result, "paused") {
		t.Fatalf("your retry under the pause: %+v (%v), want it to go on, saying it waits for the pause", q, err)
	}
}

// probeCopies records, at the startup's reconcile of the copies, whether the
// engine held new work then.
type probeCopies struct {
	Copies
	e                   **Engine
	mu                  sync.Mutex
	drains, starts, did bool
	reconciled          chan struct{}
}

func (p *probeCopies) Reconcile(ctx context.Context) error {
	p.mu.Lock()
	first := !p.did
	p.drains, p.starts, p.did = (*p.e).drainsHeld(), (*p.e).startsHeld(), true
	p.mu.Unlock()
	if first {
		close(p.reconciled)
	}
	return p.Copies.Reconcile(ctx)
}

// A drain for a reboot and a pause written before the daemon starts hold
// from its first second: the startup's recovery already runs under them,
// before the first tick reads them.
func TestTheFactoryHoldsADrainAndAPauseFromItsStart(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Boot = func(context.Context) (engine.BootID, error) { return factoryBoot, nil } })
	probe := &probeCopies{Copies: h.copies, e: &h.e, reconciled: make(chan struct{})}
	h.e.d.Copies = probe
	h.rebootDrain(83, engine.DrainUntilBoot, factoryBoot)
	h.pause(time.Time{}, "")
	sigs := h.captureSignals()
	done := make(chan error, 1)
	go func() { done <- h.e.Run(h.ctx) }()
	sig := <-sigs
	select {
	case <-probe.reconciled: // the stop comes after the startup's recovery, so its own drain is not what holds
	case <-time.After(10 * time.Second):
		t.Fatal("the startup never reconciled the copies")
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
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if !probe.did || !probe.drains || !probe.starts {
		t.Fatalf("at the startup's recovery: ran %v, drains held %v, starts held %v; want both held", probe.did, probe.drains, probe.starts)
	}
}

// A pause.json the daemon cannot read is said: a warn event, once, naming
// what to do, and no pause holds.
func TestAnUnreadablePauseIsSaidOnce(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	if err := WritePause(h.layout, "{not a pause"); err != nil {
		t.Fatal(err)
	}
	h.tickAndWait()
	h.tickAndWait()
	var said []string
	for _, ev := range h.events(0, "pause") {
		if ev.Phase == registry.PhaseFail {
			said = append(said, ev.Text)
		}
	}
	if len(said) != 1 || !strings.Contains(said[0], "pause.json") || !strings.Contains(said[0], "magnum resume") {
		t.Fatalf("pause events %q, want one warning naming the file and what to do", said)
	}
	if h.e.startsHeld() {
		t.Fatal("an unreadable pause holds")
	}
}

// A reboot-drain.json the daemon cannot read (here a directory: EISDIR)
// holds a drain from its first tick, as one it cannot parse does: nothing
// new starts, no acknowledgement certifies it, and a warn event says the
// drain holds until `magnum resume --factory` removes it.
func TestAnUnreadableRebootDrainHolds(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	if err := os.MkdirAll(h.layout.FactoryRebootDrain(), 0o700); err != nil {
		t.Fatal(err)
	}
	h.tickAndWait()
	if !h.e.drainsHeld() {
		t.Fatal("an unreadable reboot-drain.json holds no drain: new work starts")
	}
	if who, _, ok := h.e.MayPrompt(); ok || who == "" {
		t.Fatal("an unreadable reboot-drain.json holds no prompt")
	}
	if ack := h.workRecord().RebootDrain; ack != nil {
		t.Fatalf("acknowledgement %+v of a drain the daemon cannot read, want none", ack)
	}
	var said []string
	for _, ev := range h.events(0, "drain") {
		if ev.Phase == registry.PhaseFail {
			said = append(said, ev.Text)
		}
	}
	if len(said) != 1 || !strings.Contains(said[0], "the drain holds until `magnum resume --factory` removes it") {
		t.Fatalf("drain events %q, want one warning that the drain holds", said)
	}
}

// A pause the daemon held stays held when pause.json turns unreadable, and
// its warning says so, never that no pause holds.
func TestAPauseThatTurnsUnreadableHoldsAndSaysSo(t *testing.T) {
	h := newHarness(t)
	h.stage(config.FactoryStageAgents, idleStage)
	h.pause(time.Time{}, "freeze")
	h.tickAndWait()
	if err := os.Remove(h.layout.FactoryPause()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.layout.FactoryPause(), 0o700); err != nil {
		t.Fatal(err)
	}
	h.tickAndWait()
	if !h.e.startsHeld() {
		t.Fatal("the pause went when its file turned unreadable")
	}
	var said []string
	for _, ev := range h.events(0, "pause") {
		if ev.Phase == registry.PhaseFail {
			said = append(said, ev.Text)
		}
	}
	if len(said) != 1 || !strings.Contains(said[0], "the pause holds as it was") || strings.Contains(said[0], "no pause holds") {
		t.Fatalf("pause events %q, want one warning that the pause holds", said)
	}
}

// The lift of a control file is a compare-and-delete: a file that holds
// another value than the one to lift (a pause `magnum pause` wrote between
// the read and the remove) stays, and one that holds it goes.
func TestTheLiftOfAControlRemovesOnlyTheValueItRead(t *testing.T) {
	h := newHarness(t)
	path := h.layout.FactoryPause()
	if err := WritePause(h.layout, "new"); err != nil {
		t.Fatal(err)
	}
	if err := removeControlIf(path, "old"); err != nil {
		t.Fatal(err)
	}
	if v, err := ReadPause(h.layout); err != nil || v != "new" {
		t.Fatalf("pause.json holds %q (%v) after a lift of another value, want the new pause kept", v, err)
	}
	if err := removeControlIf(path, "new"); err != nil {
		t.Fatal(err)
	}
	if v, err := ReadPause(h.layout); err != nil || v != "" {
		t.Fatalf("pause.json holds %q (%v) after the lift of its value, want it gone", v, err)
	}
	if left, _ := filepath.Glob(path + ".lift-*"); len(left) != 0 {
		t.Fatalf("the lift left %q aside", left)
	}
}

// The undo of a command that overwrote a control file puts back what it
// overwrote only while the file still holds what the command wrote: a pause
// written meanwhile stays, and a file lifted meanwhile is not made again.
func TestThePutBackOfAControlReplacesOnlyTheValueItWrote(t *testing.T) {
	h := newHarness(t)
	if err := WritePause(h.layout, "another"); err != nil {
		t.Fatal(err)
	}
	if err := ReplacePause(h.layout, "mine", "before"); err != nil {
		t.Fatal(err)
	}
	if v, _ := ReadPause(h.layout); v != "another" {
		t.Fatalf("pause.json holds %q after a put-back over another pause, want it kept", v)
	}
	if err := WritePause(h.layout, "mine"); err != nil {
		t.Fatal(err)
	}
	if err := ReplacePause(h.layout, "mine", "before"); err != nil {
		t.Fatal(err)
	}
	if v, _ := ReadPause(h.layout); v != "before" {
		t.Fatalf("pause.json holds %q after the put-back, want the pause it overwrote", v)
	}
	if err := LiftPause(h.layout, ""); err != nil {
		t.Fatal(err)
	}
	if err := ReplacePause(h.layout, "mine", "before"); err != nil {
		t.Fatal(err)
	}
	if v, _ := ReadPause(h.layout); v != "" {
		t.Fatalf("pause.json holds %q after a put-back of a lifted pause, want none", v)
	}
	if left, _ := filepath.Glob(h.layout.FactoryPause() + ".swap-*"); len(left) != 0 {
		t.Fatalf("the put-back left %q aside", left)
	}
}
