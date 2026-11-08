package factory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// A pass whose stage passed while the task came to wait on you (a writer
// beside the loop, such as a CLI command, set it meanwhile) chains nothing:
// the next stage waits for your answer, not for the tick.
func TestAPassThatSendsTheTaskToYouChainsNothing(t *testing.T) {
	h := newHarness(t)
	var ran []string
	h.instantKind(&ran)
	h.e.runners["instant"] = func(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
		ran = append(ran, st.Name)
		return true, h.reg.TransitionTask(ctx, t.ID, nil, "", func(u *registry.TaskUpdate) {
			u.Set("waits_who", waitsYou)
			u.Set("waits_what", "a question about the plan")
		})
	}
	task, err := h.reg.CreateTask(h.ctx, registry.Task{JiraKey: "PR-12508", Kind: "instant", Repo: "example/shop", Title: "t",
		State: registry.TaskActive, Stage: "one"})
	if err != nil {
		t.Fatal(err)
	}

	h.tick()

	if got := h.reload(task.ID); got.Stage != "two" || got.WaitsWho != waitsYou || !slices.Equal(ran, []string{"one"}) {
		t.Fatalf("the task is at %q waiting on %q, stages run %v; want one run, the task at two waiting on you", got.Stage,
			got.WaitsWho, ran)
	}
}

// A task's own failure is tried again no sooner than a poll after it,
// however often the ticks come: the ticks other tasks' turn ends kick would
// spend its stageFailLimit tries in under a minute (before the kick, five
// tries took four polls, three minutes) and send you a fault the next
// minutes heal. A tick exactly a poll after the failure tries it. A new
// task's first stage, which its first pass records, is paced the same, and
// its failures still need you after exactly five tries at the poll's pace.
func TestKickedTicksTryAFailingStageNoMoreOftenThanThePoll(t *testing.T) {
	for _, stage := range []string{"investigate", ""} {
		t.Run(cmp.Or(stage, "a new task's first stage"), func(t *testing.T) {
			h := newHarness(t)
			failing := h.task("PR-12509", registry.TaskActive, stage)
			busy := h.task("PR-12510", registry.TaskActive, "investigate")
			var tries []time.Duration
			h.stage(config.FactoryStageAgents, func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
				if t.ID != failing.ID {
					return false, nil
				}
				tries = append(tries, h.clock.Now().Sub(t0))
				return false, fmt.Errorf("gh pr create: HTTP 403: Resource not accessible by token")
			})
			// A minute of ticks, each kicked by the other task's turn end;
			// the sixth lands exactly a poll after the first failure.
			for i, secs := range []int{0, 10, 20, 30, 40, 45, 55, 60} {
				h.clock.Set(t0.Add(time.Duration(secs) * time.Second))
				h.observeEvents(runtime.Event{TaskID: busy.ID, Role: "fx-repro", RunID: fmt.Sprintf("r%d", i), Kind: runtime.EventEnded})
				if !h.kicked() {
					t.Fatal("the turn's end kicked no tick")
				}
				h.tick()
			}
			if got := h.reload(failing.ID); got.State != registry.TaskActive || !slices.Equal(tries, []time.Duration{0, 45 * time.Second}) {
				t.Fatalf("after a minute of kicked ticks the failing task is %s waiting %q, tried at %v; want at work, tried at "+
					"0s and 45s, a poll after", got.State, got.WaitsWhat, tries)
			}
			for range 6 {
				if h.reload(failing.ID).State == registry.TaskNeedsYou {
					break
				}
				h.clock.Add(45 * time.Second)
				h.tick()
			}
			if got := h.reload(failing.ID); got.State != registry.TaskNeedsYou || len(tries) != stageFailLimit ||
				!strings.Contains(got.WaitsWhat, "the investigate stage failed 5 times in a row") {
				t.Fatalf("the failing task is %s waiting %q after %d tries; want you after %d, the stage named", got.State,
					got.WaitsWhat, len(tries), stageFailLimit)
			}
		})
	}
}

// A task whose stage changed since its last failure (a send back to
// coding, a request that moved it) runs its new stage at once: the wait
// after a failure is the failed stage's, not the task's.
func TestAStageThatChangedSinceAFailureRunsAtOnce(t *testing.T) {
	h := newHarness(t)
	var ran []string
	h.instantKind(&ran)
	h.e.runners["instant"] = func(_ context.Context, _ registry.Task, st config.FactoryStage) (bool, error) {
		ran = append(ran, st.Name)
		if st.Name == "two" {
			return false, fmt.Errorf("gh pr create: HTTP 403: Resource not accessible by token")
		}
		return false, nil
	}
	task, err := h.reg.CreateTask(h.ctx, registry.Task{JiraKey: "PR-12511", Kind: "instant", Repo: "example/shop", Title: "t",
		State: registry.TaskActive, Stage: "two"})
	if err != nil {
		t.Fatal(err)
	}
	h.tick() // two fails
	if err := h.reg.TransitionTask(h.ctx, task.ID, nil, "", func(u *registry.TaskUpdate) {
		u.Set("stage", "one")
		u.Set("attempt", 0)
		u.Because("back to one, as a send back moves a task")
	}); err != nil {
		t.Fatal(err)
	}
	h.clock.Add(10 * time.Second)
	h.tick()

	if !slices.Equal(ran, []string{"two", "one"}) {
		t.Fatalf("stages run %v; want two, then one at once on the next tick", ran)
	}
}

// failedNow records a failure of task id's stage now, as the loop records
// its pass's own failure (taskFailed): the task's next try waits a poll.
func (h *harness) failedNow(id int64) {
	h.t.Helper()
	h.e.taskFailed(h.ctx, taskFailure{task: h.reload(id), err: errors.New("gh pr create: HTTP 403: Resource not accessible by token")})
	if !h.e.retryLater(h.reload(id), h.clock.Now()) {
		h.t.Fatal("the failure set no wait")
	}
}

// Your word moves a task at once, whatever its stage's last failure: your
// answers, a let-go and your decision on the ready card each clear the wait
// a poll after a failure (as a retry does), so the tick the request kicks
// runs the task's stage. Only the factory's own retries keep the poll's
// spacing.
func TestYourWordRunsAStageThatFailedAMomentAgoAtOnce(t *testing.T) {
	t.Run("an answer", func(t *testing.T) {
		h := newHarness(t)
		task := h.gateTask("PR-12512", "feature", whenQuestion)
		h.tick()
		if got := h.reload(task.ID); got.State != registry.TaskNeedsYou || got.Stage != "questions" {
			t.Fatalf("task %s at %s, want NEEDS YOU at its questions", got.State, got.Stage)
		}
		h.failedNow(task.ID)
		h.clock.Add(10 * time.Second)
		h.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: whenQuestion.Options[0].Label}})
		if got := h.reload(task.ID); got.Stage != "coding" {
			t.Fatalf("after your answer the task is %s at %s; want the gate run in the answer's tick, at coding", got.State,
				got.Stage)
		}
	})

	t.Run("a let-go", func(t *testing.T) {
		h := newHarness(t)
		passes := 0
		h.stage(config.FactoryStageAgents, func(context.Context, registry.Task, config.FactoryStage) (bool, error) {
			passes++
			return false, nil
		})
		task := h.task("PR-12513", registry.TaskActive, "investigate")
		a, err := h.reg.BeginAttempt(h.ctx, task.ID, "investigate")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.reg.AppendLedger(h.ctx, registry.LedgerEntry{TaskID: task.ID, AttemptID: a.ID, Role: "fx-repro", RunID: "run-1",
			Hash: runtime.PromptHash("reproduce the bug"), Text: "reproduce the bug"}); err != nil { // its delivery unknown
			t.Fatal(err)
		}
		h.clock.Add(runtime.UnknownAfter)
		h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-repro", Kind: runtime.EventDeliveryUnknown,
			Reason: runtime.UnknownReason(task.ID, "fx-repro")})
		if got := h.reload(task.ID); got.State != registry.TaskNeedsYou {
			t.Fatalf("task %s waits %q, want NEEDS YOU on the pane", got.State, got.WaitsWhat)
		}
		h.failedNow(task.ID)
		h.clock.Add(10 * time.Second)
		if _, err := h.reg.EnqueueRequest(h.ctx, ReqLetGo, LetGoPayload{TaskID: task.ID, Role: "fx-repro", Surface: "screen"}); err != nil {
			t.Fatal(err)
		}
		h.tick()
		if got := h.reload(task.ID); got.State != registry.TaskActive || passes != 1 {
			t.Fatalf("after your let-go the task is %s and its stage ran %d times; want active, run once in the let-go's tick",
				got.State, passes)
		}
	})

	t.Run("a decision", func(t *testing.T) {
		r := newReadyRig(t)
		task, _ := r.toCard("PR-12514")
		r.failedNow(task.ID)
		r.clock.Add(10 * time.Second)
		if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
			t.Fatalf("push request %s: %q", q.State, q.Result)
		}
		if got := r.reload(task.ID); got.Stage != "publish" {
			t.Fatalf("after your push the task is %s at %s; want the ready stage run in the push's tick, at publish", got.State,
				got.Stage)
		}
	})
}

// daemonStops is a stop of the daemon in the middle of a pass, as the
// beforeStop seam stands in for it: a stop costs the task nothing (a held
// pass, as a drain's), never a failure of its stage, so the same engine's
// next pass stands in for the next daemon's, which has no failure to wait
// on either.
var daemonStops = heldError{who: holdRestart, what: drainWhat}

// A QA stop for you (QA could not run, a state it could not reach, the
// writer's decline) moves the task to you at its qa stage and passes
// nothing, so nothing chains past it: the PR body's stage never starts, in
// that tick or after, until you act.
func TestAQAStopForYouChainsNothing(t *testing.T) {
	for _, stop := range []string{"unavailable", "blocked", "declined"} {
		t.Run(stop, func(t *testing.T) {
			r := newQARig(t)
			task, head := r.qaTask("PR-12515")
			judged := len(r.rt.promptsTo("fx-judge"))
			want := ""
			switch stop {
			case "unavailable":
				r.setProbe(func(bool, int) (int, error) { return 0, errRefused })
				want = "QA could not run"
			case "blocked":
				r.toWalker(task)
				v := oneSecurityPage()
				v.Blocked = []QABlocked{{What: "the passkey autofill list", Why: "headless Chrome draws no autofill", Check: "A1"}}
				r.walk(task, v, map[string]string{"P1": "clean-tooltip.json"})
				want = "QA could not reach 1 state"
			case "declined":
				r.toWalker(task)
				r.walk(task, escapedWalk("/settings/security"), escapedProbes)
				r.toWriter(task, 1)
				r.writerFixesQA(task, 1, head, nil, map[string]string{"Q1": "the key is shared with the mailer"})
				want = "the writer declined"
			}
			r.toYou(task)
			r.run(3, nil) // and the ticks after it
			got := r.reload(task.ID)
			if got.State != registry.TaskNeedsYou || got.Stage != "qa" || !strings.Contains(got.WaitsWhat, want) {
				t.Fatalf("task %s at %s waits %q; want NEEDS YOU at qa, %q", got.State, got.Stage, got.WaitsWhat, want)
			}
			if slices.ContainsFunc(r.attempts(task.ID), func(a registry.Attempt) bool { return a.Stage == "pr_body" }) ||
				len(r.rt.promptsTo("fx-judge")) != judged {
				t.Fatalf("attempts %+v, %d prompts to the judge since QA began; want no PR body stage after the stop",
					r.attempts(task.ID), len(r.rt.promptsTo("fx-judge"))-judged)
			}
		})
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
