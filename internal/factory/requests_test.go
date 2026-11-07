package factory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// askTalk enqueues a talk request for task id as the CLI sends it.
func (h *harness) askTalk(id int64) int64 {
	h.t.Helper()
	rid, err := h.reg.EnqueueRequest(h.ctx, ReqTalk, TalkPayload{TaskID: id, Surface: "cli"})
	if err != nil {
		h.t.Fatal(err)
	}
	return rid
}

// request is request id as the registry holds it.
func (h *harness) request(id int64) registry.Request {
	h.t.Helper()
	q, err := h.reg.RequestByID(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return q
}

// tickAndWait runs a tick and waits for the work it started off the loop.
func (h *harness) tickAndWait() {
	h.t.Helper()
	h.tick()
	h.e.waitBackground()
}

// tickEnds runs a tick and fails the test when it has not ended within
// limit: a tick that waits for work that blocks is not a tick of a loop.
func (h *harness) tickEnds(limit time.Duration) {
	h.t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.tick()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		h.t.Fatalf("the tick did not end within %s: it waits for the talk", limit)
	}
}

// blockedTalk makes the runtime's Talk wait until the returned release is
// called (the test's cleanup calls it as well); started waits until a Talk
// began, and fails the test when none does.
func (h *harness) blockedTalk() (started func(), release func()) {
	h.t.Helper()
	begun, gate := make(chan struct{}, 8), make(chan struct{})
	var once sync.Once
	rel := func() { once.Do(func() { close(gate) }) }
	h.t.Cleanup(rel)
	h.rt.mu.Lock()
	h.rt.talk = func(ctx context.Context, _ registry.Task, _ string) (string, error) {
		begun <- struct{}{}
		select {
		case <-gate:
			return "p_7", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	h.rt.mu.Unlock()
	started = func() {
		h.t.Helper()
		select {
		case <-begun:
		case <-time.After(20 * time.Second):
			h.t.Fatal("no Talk began")
		}
	}
	return started, rel
}

// A talk request starts the task's liaison: the daemon renders the
// liaison's prompt with the task's data (the daemon's own magnum, the
// task's folder, its key and the task's number in the commands it runs) and
// hands it to the runtime once, and the request completes with the pane the
// liaison talks in.
func TestTalkRequestStartsTheLiaison(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12301", registry.TaskNeedsYou, "report")
	h.rt.talk = func(context.Context, registry.Task, string) (string, error) { return "p_7", nil }

	id := h.askTalk(task.ID)
	h.tickAndWait()

	q := h.request(id)
	want := "the liaison of PR-12301 talks in pane p_7 of the factory's herdr session"
	if q.State != registry.RequestDone || q.Result != want {
		t.Fatalf("the talk request is %s with %q, want done with %q", q.State, q.Result, want)
	}
	calls := h.rt.talkCalls()
	if len(calls) != 1 || calls[0].task != task.ID {
		t.Fatalf("Talk was called %+v, want once for task %d", calls, task.ID)
	}
	prompt := calls[0].prompt
	magnum := execx.ShellQuote(h.layout.Binary())
	for _, need := range []string{
		"PR-12301",
		h.layout.FactoryTaskDir(task.ID),
		magnum + " factory answer " + strconv.FormatInt(task.ID, 10),
		magnum + " factory retry " + strconv.FormatInt(task.ID, 10),
	} {
		if !strings.Contains(prompt, need) {
			t.Errorf("the liaison's prompt lacks %q:\n%s", need, prompt)
		}
	}
	if strings.Contains(prompt, "{{") || strings.Contains(prompt, "<no value>") {
		t.Errorf("the liaison's prompt has a hole in it:\n%s", prompt)
	}
}

// A task that ended has no liaison to talk with: the request fails with a
// line that names the state, and Talk is not called. A task in any other
// state, a published one included, is talked about.
func TestTalkRefusesAnEndedTask(t *testing.T) {
	for _, state := range []string{registry.TaskDone, registry.TaskStopped, registry.TaskFailed} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t)
			task := h.task("PR-12302", state, "report")

			id := h.askTalk(task.ID)
			h.tickAndWait()

			q := h.request(id)
			if q.State != registry.RequestFailed || !strings.Contains(q.Result, "PR-12302 is "+state) {
				t.Fatalf("the talk request is %s with %q, want failed with a line naming %q", q.State, q.Result, state)
			}
			if calls := h.rt.talkCalls(); len(calls) != 0 {
				t.Fatalf("Talk was called %+v for a task that is %s", calls, state)
			}
		})
	}
	for _, state := range []string{registry.TaskQueued, registry.TaskActive, registry.TaskNeedsYou, registry.TaskWaiting, registry.TaskPublished} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t)
			task := h.task("PR-12303", state, "report")

			id := h.askTalk(task.ID)
			h.tickAndWait()

			if q := h.request(id); q.State != registry.RequestDone {
				t.Fatalf("a talk about a task that is %s is %s (%s), want done", state, q.State, q.Result)
			}
		})
	}
}

// A liaison that is live but whose first prompt waited (you typed into its
// pane, or herdr refused it) is no failed request: the request completes
// with its pane and says the prompt waits, so you can step in. A talk that
// got no pane fails.
func TestTalkNamesThePaneWhenTheFirstPromptWaits(t *testing.T) {
	cases := []struct {
		name    string
		pane    string
		err     error
		state   string
		contain []string
	}{
		{"you typed in its pane", "p_7", fmt.Errorf("the liaison of PR-12304: its first prompt: %w", agents.ErrHumanActive),
			registry.RequestDone, []string{"pane p_7", "factory's herdr session",
				"you typed into its pane within human_cooldown; the next talk sends it"}},
		{"herdr refused the prompt", "p_8", errors.New("the liaison of PR-12304: its first prompt: herdr: the agent is not ready"),
			registry.RequestDone, []string{"pane p_8", "first prompt waits", "the agent is not ready", "the next talk sends it"}},
		{"no pane", "", errors.New("the liaison of PR-12304: no free workspace"),
			registry.RequestFailed, []string{"no free workspace"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			task := h.task("PR-12304", registry.TaskActive, "report")
			h.rt.talk = func(context.Context, registry.Task, string) (string, error) { return c.pane, c.err }

			id := h.askTalk(task.ID)
			h.tickAndWait()

			q := h.request(id)
			if q.State != c.state {
				t.Fatalf("the talk request is %s (%s), want %s", q.State, q.Result, c.state)
			}
			for _, want := range c.contain {
				if !strings.Contains(q.Result, want) {
					t.Errorf("the result %q lacks %q", q.Result, want)
				}
			}
		})
	}
}

// Talk can take a minute (it starts an agent and prompts it), so it runs
// off the loop: the tick that begins it ends at once, another request is
// answered in the same tick, a second tick starts no second Talk, and the
// request completes when the Talk does.
func TestTalkRunsOffTheLoop(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12305", registry.TaskNeedsYou, "report")
	other := h.task("PR-12306", registry.TaskActive, "investigate")
	h.stage(config.FactoryStageAgents, idleStage)
	started, release := h.blockedTalk()

	talk := h.askTalk(task.ID)
	stop, err := h.reg.EnqueueRequest(h.ctx, ReqStop, StopPayload{TaskID: other.ID, Surface: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	h.tickEnds(20 * time.Second)

	started()
	if q := h.request(stop); q.State != registry.RequestDone {
		t.Fatalf("a stop asked after the talk is %s (%s), want done in the same tick", q.State, q.Result)
	}
	if q := h.request(talk); q.State != registry.RequestPending {
		t.Fatalf("the talk request is %s (%s) while Talk runs, want pending", q.State, q.Result)
	}
	h.tickEnds(20 * time.Second)
	if calls := h.rt.talkCalls(); len(calls) != 1 {
		t.Fatalf("Talk was called %d times by two ticks, want once", len(calls))
	}

	release()
	h.e.waitBackground()
	if q := h.request(talk); q.State != registry.RequestDone || !strings.Contains(q.Result, "pane p_7") {
		t.Fatalf("the talk request is %s (%s), want done in pane p_7", q.State, q.Result)
	}
}

// Two talks about one task take turns: the second waits, pending, until the
// first ended, then finds the liaison the first started.
func TestTwoTalksOfATaskTakeTurns(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12307", registry.TaskNeedsYou, "report")
	started, release := h.blockedTalk()

	first, second := h.askTalk(task.ID), h.askTalk(task.ID)
	h.tickEnds(20 * time.Second)
	started()
	if calls := h.rt.talkCalls(); len(calls) != 1 {
		t.Fatalf("Talk was called %d times for two talks of one task, want one running", len(calls))
	}
	if q := h.request(second); q.State != registry.RequestPending {
		t.Fatalf("the second talk is %s while the first runs, want pending", q.State)
	}

	release()
	h.e.waitBackground()
	if q := h.request(first); q.State != registry.RequestDone {
		t.Fatalf("the first talk is %s (%s), want done", q.State, q.Result)
	}
	h.tickAndWait()
	if q := h.request(second); q.State != registry.RequestDone {
		t.Fatalf("the second talk is %s (%s), want done after its turn", q.State, q.Result)
	}
	if calls := h.rt.talkCalls(); len(calls) != 2 {
		t.Fatalf("Talk was called %d times by two talks, want 2", len(calls))
	}
}

// A task that ends while its liaison starts (you stopped it a moment after
// asking to talk) has its liaison parked once the Talk ended: the stop's own
// park found the Talk at work and left the liaison, and nothing parks it
// later. The talk request fails with a line that says so.
func TestAStopDuringATalkParksTheLiaisonAfterIt(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12308", registry.TaskNeedsYou, "report")
	started, release := h.blockedTalk()

	talk := h.askTalk(task.ID)
	h.tickEnds(20 * time.Second)
	started()
	if _, err := h.reg.EnqueueRequest(h.ctx, ReqStop, StopPayload{TaskID: task.ID, Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	h.tickEnds(20 * time.Second)
	if got := h.reload(task.ID).State; got != registry.TaskStopped {
		t.Fatalf("the task is %s after its stop, want stopped", got)
	}
	if parked := h.rt.parked(); !slices.Equal(parked, []int64{task.ID}) {
		t.Fatalf("parks %v while Talk runs, want the stop's own", parked)
	}

	release()
	h.e.waitBackground()

	if parked := h.rt.parked(); !slices.Equal(parked, []int64{task.ID, task.ID}) {
		t.Fatalf("parks %v after Talk ended, want the stop's and one for the liaison Talk started", parked)
	}
	if q := h.request(talk); q.State != registry.RequestFailed || !strings.Contains(q.Result, "PR-12308 is stopped") {
		t.Fatalf("the talk request is %s (%s), want failed naming the stop", q.State, q.Result)
	}
}

// A task stopped while its Talk ran, and a drain then cut the Talk: the park
// after it runs all the same, and its failure is in the task's event log,
// though the drain cancelled the talk's context.
func TestAFailedParkAfterADrainedTalkIsLogged(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12311", registry.TaskNeedsYou, "report")
	started, _ := h.blockedTalk()
	h.askTalk(task.ID)
	h.tickEnds(20 * time.Second)
	started()
	if _, err := h.reg.EnqueueRequest(h.ctx, ReqStop, StopPayload{TaskID: task.ID, Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	h.tickEnds(20 * time.Second)
	h.rt.mu.Lock()
	h.rt.parkErr = errors.New("herdr: the liaison is at work")
	h.rt.mu.Unlock()

	h.e.stopBackground()

	if parked := h.rt.parked(); !slices.Equal(parked, []int64{task.ID, task.ID}) {
		t.Fatalf("parks %v, want the stop's and the one after the drained Talk", parked)
	}
	want := "the liaison of a task that ended while it started: herdr: the liaison is at work"
	if got := h.eventTexts(task.ID, "park"); !slices.Contains(got, want) {
		t.Fatalf("the task's park events %q, want %q", got, want)
	}
}

// A drain ends a Talk that is running (its context is the engine's), and
// leaves its request pending: the next daemon talks again, and Talk finds the
// liaison the cancelled one started, or starts it.
func TestADrainLeavesARunningTalkPending(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12309", registry.TaskNeedsYou, "report")
	started, _ := h.blockedTalk()

	talk := h.askTalk(task.ID)
	h.tickEnds(20 * time.Second)
	started()

	h.e.stopBackground()

	if q := h.request(talk); q.State != registry.RequestPending {
		t.Fatalf("the talk request is %s (%s) after the drain cancelled Talk, want pending for the next daemon", q.State, q.Result)
	}
}

// A daemon that drains starts no liaison: the request waits for the next one.
func TestADrainingDaemonStartsNoTalk(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12310", registry.TaskNeedsYou, "report")
	h.e.draining.Store(true)

	talk := h.askTalk(task.ID)
	h.tickAndWait()

	if q := h.request(talk); q.State != registry.RequestPending {
		t.Fatalf("the talk request is %s (%s) in a draining daemon, want pending", q.State, q.Result)
	}
	if calls := h.rt.talkCalls(); len(calls) != 0 {
		t.Fatalf("a draining daemon called Talk %+v", calls)
	}
}

// A request of an unknown task fails with the registry's words, not a panic.
func TestTalkOfAnUnknownTaskFails(t *testing.T) {
	h := newHarness(t)

	id := h.askTalk(9999)
	h.tickAndWait()

	if q := h.request(id); q.State != registry.RequestFailed {
		t.Fatalf("the talk request is %s (%s), want failed", q.State, q.Result)
	}
}

// A task keeps its copy across a daemon restart (its row names it) while the
// engine's own memory of the copies it holds starts empty and only a claim
// fills it: the liaison's first prompt still tells it where the copy is, by
// the repository's copies pattern, read only. A copy the engine holds comes
// from the claim, and a task that holds none, or one the repository's
// pattern does not render, gets no copy line.
func TestTalkPromptCarriesTheTasksCopyAfterARestart(t *testing.T) {
	h := newHarness(t)
	root := t.TempDir()
	h.cfg.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop", Copies: filepath.Join(root, "shop.factory{n}"),
		CopyName: "factory{n}", MaxCopies: 3}}
	taskWith := func(key, copyName, branch string) registry.Task {
		t.Helper()
		task, err := h.reg.CreateTask(h.ctx, registry.Task{JiraKey: key, Kind: "bug", Repo: "example/shop", Title: key + " title",
			State: registry.TaskNeedsYou, Stage: "report", Copy: copyName, Branch: branch})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	promptOf := func(task registry.Task) string {
		t.Helper()
		id := h.askTalk(task.ID)
		h.tickAndWait()
		if q := h.request(id); q.State != registry.RequestDone {
			t.Fatalf("the talk request is %s (%s), want done", q.State, q.Result)
		}
		for _, call := range slices.Backward(h.rt.talkCalls()) {
			if call.task == task.ID {
				return call.prompt
			}
		}
		t.Fatalf("Talk was not called for task %d", task.ID)
		return ""
	}

	// After a restart: the task's row names factory2, the engine holds nothing.
	restarted := taskWith("PR-12311", "factory2", "zhuravel-PR-12311-credit")
	prompt := promptOf(restarted)
	line := "The task's copy of the repository is " + filepath.Join(root, "shop.factory2") + ", on `zhuravel-PR-12311-credit`."
	if !strings.Contains(prompt, line) {
		t.Fatalf("the liaison's prompt lacks %q:\n%s", line, prompt)
	}

	// A copy the engine holds is the claim's own.
	held := taskWith("PR-12312", "factory3", "zhuravel-PR-12312-credit")
	h.e.claimMu.Lock()
	h.e.held[held.ID] = Copy{Name: "factory3", Path: filepath.Join(root, "claimed.factory3"), Number: 3}
	h.e.claimMu.Unlock()
	if prompt := promptOf(held); !strings.Contains(prompt, "The task's copy of the repository is "+filepath.Join(root, "claimed.factory3")+",") {
		t.Fatalf("the prompt of a task whose claim the engine holds lacks the claim's copy:\n%s", prompt)
	}

	// No copy, or a name the repository's pattern does not render: no copy line.
	for _, task := range []registry.Task{taskWith("PR-12313", "", ""), taskWith("PR-12314", "elsewhere7", "zhuravel-PR-12314-credit")} {
		if prompt := promptOf(task); strings.Contains(prompt, "copy of the repository") {
			t.Errorf("the prompt of %s (copy %q) names a copy:\n%s", task.JiraKey, task.Copy, prompt)
		}
	}
}
