package factory

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
)

// kicked reports whether a kick of the loop is pending, and takes it.
func (h *harness) kicked() bool {
	select {
	case <-h.e.kick:
		return true
	default:
		return false
	}
}

// An observe pass that keeps an event an agents stage acts on when it takes
// it (onAgentEvent: a run's end, a pause, a Codex refusal, a dialog, the
// hooks review, a lost pane, a stuck turn, a loop without progress) asks
// for a tick at once, so the stage takes it now rather than at the next
// poll, up to 45 s later.
func TestAnEndedRolesEventKicksTheLoop(t *testing.T) {
	for _, kind := range []runtime.EventKind{runtime.EventEnded, runtime.EventPaused, runtime.EventRefused, runtime.EventBlocked,
		runtime.EventHooksTrust, runtime.EventLost, runtime.EventStuck, runtime.EventNoProgress} {
		t.Run(string(kind), func(t *testing.T) {
			h := newHarness(t)
			task := h.task("PR-12500", registry.TaskActive, "investigate")
			if h.kicked() {
				t.Fatal("a kick pending before the observe pass")
			}
			h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", RunID: "r1", Kind: kind, Reason: "usage: claude limit until 16:00"})
			if !h.kicked() {
				t.Fatalf("an observe pass that kept a %s event asked for no tick: its stage takes it at the next poll", kind)
			}
		})
	}
}

// A typed turn is a record, and the delivery events are settled by the
// observe pass itself (deliveryUnknown, delivered, undelivered), none of
// them anything a stage does when it takes them: none asks for a tick.
func TestATypedEventDoesNotKickTheLoop(t *testing.T) {
	for _, kind := range []runtime.EventKind{runtime.EventTyped, runtime.EventDeliveryUnknown, runtime.EventUndelivered,
		runtime.EventDelivered} {
		t.Run(string(kind), func(t *testing.T) {
			h := newHarness(t)
			task := h.task("PR-12501", registry.TaskActive, "investigate")
			h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", RunID: "r1", Kind: kind})
			if h.kicked() {
				t.Fatalf("an observe pass that kept a %s event asked for a tick", kind)
			}
		})
	}
}

// In the daemon's loop the kick is a tick right after the observe pass that
// saw the end, not a poll later: with a poll of an hour, the stage still
// takes the end within the second.
func TestTheLoopTicksRightAfterAnObservePassSawAnEnd(t *testing.T) {
	h := newHarness(t)
	h.cfg.Factory.Poll = config.Duration{Duration: time.Hour}
	h.cfg.Factory.Observe = config.Duration{Duration: 20 * time.Millisecond}
	task := h.task("PR-12502", registry.TaskActive, "investigate")
	passes := make(chan struct{}, 1)
	h.stage(config.FactoryStageAgents, func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		if t.ID == task.ID {
			select {
			case passes <- struct{}{}:
			default:
			}
		}
		return false, nil
	})
	h.captureSignals()
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan error, 1)
	go func() { done <- h.e.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case <-passes: // the tick at the start
	case <-time.After(10 * time.Second):
		t.Fatal("Run never ticked")
	}

	h.rt.setEvents([]runtime.Event{{TaskID: task.ID, Role: "fx-repro", RunID: "r1", Kind: runtime.EventEnded}})
	select {
	case <-passes:
	case <-time.After(10 * time.Second):
		t.Fatal("no tick followed the observe pass that saw the end: its stage waits for the poll")
	}
}

// passInvestigation starts the bug task's investigation, writes the
// investigators' findings and has the observe pass see their runs end.
func (h *harness) passInvestigation(task registry.Task) {
	h.t.Helper()
	h.startStage(task)
	for _, role := range []string{"fx-repro", "fx-cause", "fx-history"} {
		h.taskFile(task.ID, "findings-"+role+".md", role+" found it in app/models/campaign.rb:88\n")
	}
	h.ended(task, "fx-repro", "fx-cause", "fx-history")
}

// A stage that passes in a tick starts the next stage in the same tick: the
// investigation's end launches the judge and sends its prompt at once, not
// a poll (45 s) later.
func TestAStageThatPassesLaunchesTheNextStagesRolesInTheSameTick(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12503", "bug", "investigate")
	h.passInvestigation(task)

	h.tick()

	got := h.reload(task.ID)
	as := h.attempts(task.ID)
	if got.Stage != "judge" || got.State != registry.TaskActive || len(as) != 2 || as[0].State != registry.AttemptVerified ||
		as[1].Stage != "judge" || as[1].State != registry.AttemptRunning || got.Attempt != as[1].N {
		t.Fatalf("task %s at %s attempt %d, attempts %+v; want active in the judge's running attempt", got.State, got.Stage,
			got.Attempt, as)
	}
	if l := h.rt.launchedRoles(); len(l) != 2 || !slices.Equal(l[1], []string{"fx-judge"}) {
		t.Fatalf("launches %v, want the judge launched in the tick the investigation passed", l)
	}
	if ps := h.rt.promptsTo("fx-judge"); len(ps) != 1 || ps[0].Attempt != as[1].ID {
		t.Fatalf("prompts to the judge %+v, want its first, in its attempt", ps)
	}
}

// instantKind is a kind of three stages of a type that passes at once; ran
// records the stages its runner ran, in order.
func (h *harness) instantKind(ran *[]string) {
	h.cfg.Factory.Kinds["instant"] = config.FactoryKind{Stages: []config.FactoryStage{
		{Name: "one", Type: "instant"}, {Name: "two", Type: "instant"}, {Name: "three", Type: "instant"}}}
	h.stage("instant", func(_ context.Context, _ registry.Task, st config.FactoryStage) (bool, error) {
		*ran = append(*ran, st.Name)
		return true, nil
	})
}

// The chain is one extra pass, not a loop: a next stage that also passes at
// once moves the task on and waits for the next tick, so a tick's work per
// task stays bounded.
func TestTheChainRunsAtMostOncePerTaskPerTick(t *testing.T) {
	h := newHarness(t)
	var ran []string
	h.instantKind(&ran)
	task, err := h.reg.CreateTask(h.ctx, registry.Task{JiraKey: "PR-12504", Kind: "instant", Repo: "example/shop", Title: "t",
		State: registry.TaskActive, Stage: "one"})
	if err != nil {
		t.Fatal(err)
	}

	h.tick()
	if got := h.reload(task.ID).Stage; got != "three" || !slices.Equal(ran, []string{"one", "two"}) {
		t.Fatalf("after one tick the task is at %q, stages run %v; want one and two run, at three", got, ran)
	}
	h.tick()
	if !slices.Equal(ran, []string{"one", "two", "three"}) {
		t.Fatalf("stages run %v, want three on the next tick", ran)
	}
}

// No chain while the daemon drains: a stop that comes during the pass ends
// the tick with the task's move, and the next daemon starts the next stage.
func TestNoChainWhileTheDaemonDrains(t *testing.T) {
	h := newHarness(t)
	var ran []string
	h.instantKind(&ran)
	h.e.runners["instant"] = func(_ context.Context, _ registry.Task, st config.FactoryStage) (bool, error) {
		ran = append(ran, st.Name)
		h.e.draining.Store(true) // SIGTERM during the pass
		return true, nil
	}
	task, err := h.reg.CreateTask(h.ctx, registry.Task{JiraKey: "PR-12505", Kind: "instant", Repo: "example/shop", Title: "t",
		State: registry.TaskActive, Stage: "one"})
	if err != nil {
		t.Fatal(err)
	}

	h.tick()

	if got := h.reload(task.ID).Stage; got != "two" || !slices.Equal(ran, []string{"one"}) {
		t.Fatalf("the task is at %q, stages run %v; want one run and the task moved to two, nothing more", got, ran)
	}
}

// Under a hold of the machine's (the memory) or install's drain the chained
// pass launches nothing, as any pass: the next stage starts on a later tick,
// once the hold is lifted, as it did before the chain.
func TestNoChainedLaunchUnderAHold(t *testing.T) {
	for _, hold := range []string{"memory", "install's drain"} {
		t.Run(hold, func(t *testing.T) {
			var swap atomic.Int64
			swap.Store(10)
			h := newHarness(t, func(d *Deps) {
				d.Memory = func(context.Context) engine.MemorySample {
					return engine.MemorySample{Level: engine.MemoryNormal, Swap: &engine.MemorySwap{UsedBytes: swap.Load() * gib,
						TotalBytes: 100 * gib}}
				}
			})
			h.e.drainerAlive = func(Drain) bool { return true }
			task := h.kindTask("PR-12506", "bug", "investigate")
			h.passInvestigation(task)
			lift := func() { swap.Store(10) }
			if hold == "memory" {
				swap.Store(97)
			} else {
				value := h.installDrain(4245)
				lift = func() {
					if err := LiftDrain(h.layout, value); err != nil {
						t.Fatal(err)
					}
				}
			}

			h.tick()
			if got := h.reload(task.ID); got.Stage != "judge" || len(h.rt.launchedRoles()) != 1 || len(h.rt.promptsTo("fx-judge")) != 0 {
				t.Fatalf("under the hold: task at %q, launches %v, prompts to the judge %d; want the task at judge, nothing launched",
					got.Stage, h.rt.launchedRoles(), len(h.rt.promptsTo("fx-judge")))
			}
			lift()
			h.tick()
			if l := h.rt.launchedRoles(); len(l) != 2 || !slices.Equal(l[1], []string{"fx-judge"}) || len(h.rt.promptsTo("fx-judge")) != 1 {
				t.Fatalf("once the hold lifted: launches %v, want the judge launched and prompted", l)
			}
		})
	}
}

// A move to the next stage that fails chains nothing: the next stage's pass
// never runs on a task that is not there.
func TestAFailedTransitionChainsNothing(t *testing.T) {
	h := newHarness(t)
	var ran []string
	h.instantKind(&ran)
	h.e.runners["instant"] = func(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
		ran = append(ran, st.Name)
		// The task moved meanwhile, so the stage's compare-and-set move
		// from active fails.
		return true, h.reg.TransitionTask(ctx, t.ID, []string{registry.TaskActive}, registry.TaskWaiting, func(u *registry.TaskUpdate) {
			u.Because("a test moves it")
		})
	}
	task, err := h.reg.CreateTask(h.ctx, registry.Task{JiraKey: "PR-12507", Kind: "instant", Repo: "example/shop", Title: "t",
		State: registry.TaskActive, Stage: "one"})
	if err != nil {
		t.Fatal(err)
	}

	h.tick()

	if got := h.reload(task.ID); got.Stage != "one" || !slices.Equal(ran, []string{"one"}) {
		t.Fatalf("the task is at %q, stages run %v; want one run once and the task left at one", got.Stage, ran)
	}
}

// attemptPrompts are the prompts of ps sent in attempt id.
func attemptPrompts(ps []sentPrompt, id string) []sentPrompt {
	return slices.DeleteFunc(slices.Clone(ps), func(p sentPrompt) bool { return p.Attempt != id })
}

// pushHeld pushes task id's ready card while install's drain holds the
// effects: the tick that takes the push passes the ready stage and chains
// the publish's first pass, which the drain stops before anything leaves
// the Mac, so a test can change what the publish reads before it runs. lift
// ends the drain.
func (r *readyRig) pushHeld(id int64) (lift func()) {
	r.t.Helper()
	r.e.drainerAlive = func(Drain) bool { return true }
	value := r.installDrain(4247)
	if q := r.decideOn(id, r.card(id), ChoicePush, "", ""); q.State != registry.RequestDone {
		r.t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	if got := r.reload(id); got.Stage != "publish" || r.remoteHead(got.Branch) != "" {
		r.t.Fatalf("task at %s, remote %q after your push under the drain; want it at publish, nothing pushed", got.Stage,
			r.remoteHead(got.Branch))
	}
	return func() {
		r.t.Helper()
		if err := LiftDrain(r.layout, value); err != nil {
			r.t.Fatal(err)
		}
	}
}
