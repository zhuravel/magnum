package runtime

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
)

func TestAHungTurnClimbsTheWatchdogSteps(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	first := "reproduce the bug"
	g.send("fx-repro", first)
	g.observe()
	hung := "running rspec spec/models/user_spec.rb"
	g.h.screen(g.agent("fx-repro"), "Bash(bin/rspec spec/models/user_spec.rb)\n"+hung+"\n")
	timeout := g.role("fx-repro").Timeout.Duration

	g.clock.Add(timeout - NoProgressAfter)
	if evs := g.watchdog(); len(evs) != 0 {
		t.Fatalf("before the timeout the watchdog gave %+v", evs)
	}
	starts := len(g.h.startCalls())

	// Step 1: the shell is stopped and the agent nudged.
	g.clock.Add(NoProgressAfter)
	g.watchdog()
	prompts := g.h.promptTexts()
	if len(prompts) != 2 || !strings.Contains(prompts[1], "stuck") {
		t.Fatalf("prompts after step 1 %q, want a nudge", prompts)
	}
	if len(g.h.startCalls()) != starts {
		t.Fatal("step 1 restarted the agent")
	}
	g.clock.Add(NoProgressAfter / 2)
	g.watchdog()
	if len(g.h.startCalls()) != starts || len(g.h.promptTexts()) != 2 {
		t.Fatal("step 2 came 5 minutes after step 1, not 10")
	}

	// Step 2, 10 still minutes after step 1: the session is quit hard and
	// resumed.
	g.clock.Add(NoProgressAfter / 2)
	g.watchdog()
	s := g.h.startCalls()
	if len(s) != starts+1 || !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume sid-1") {
		t.Fatalf("starts after step 2 %+v, want the conversation resumed", s)
	}
	if !slices.ContainsFunc(g.h.keyCalls(), func(k keysCall) bool { return slices.Contains(k.Keys, "ctrl+c") }) {
		t.Fatal("step 2 resumed an agent it never stopped")
	}
	g.h.screen(g.agent("fx-repro"), "resumed\n"+hung+"\n") // it went on a little, then hung again
	g.watchdog()

	// Step 3: a fresh session from the files.
	g.clock.Add(NoProgressAfter)
	g.watchdog()
	s = g.h.startCalls()
	if len(s) != starts+2 || strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume") {
		t.Fatalf("starts after step 3 %+v, want a fresh session", s)
	}
	prompts = g.h.promptTexts()
	if prompts[len(prompts)-1] != first {
		t.Fatalf("the fresh session got %q, want the first prompt again", prompts[len(prompts)-1])
	}
	g.h.screen(g.agent("fx-repro"), "Bash(bin/rspec)\n"+hung+"\n\n")
	g.watchdog()

	// After the 3 steps: stuck, with the pane's last line.
	g.clock.Add(NoProgressAfter)
	evs := g.watchdog()
	if len(evs) != 1 || evs[0].Kind != EventStuck || evs[0].Reason != hung || evs[0].TaskID != g.task.ID || evs[0].Role != "fx-repro" {
		t.Fatalf("after the steps the watchdog gave %+v, want stuck with %q", evs, hung)
	}
	g.clock.Add(NoProgressAfter)
	if evs := g.watchdog(); len(evs) != 0 {
		t.Fatalf("a stuck run was reported again: %+v", evs)
	}
	if n := len(g.h.startCalls()); n != starts+2 {
		t.Fatalf("%d starts: a step ran after the 3 steps", n-starts)
	}
}

func TestAWatchdogStepWaitsWhileTheScreenChanges(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.send("fx-repro", "reproduce the bug")
	timeout := g.role("fx-repro").Timeout.Duration
	for i := range 6 {
		g.clock.Add(timeout/6 + time.Minute)
		g.h.screen(g.agent("fx-repro"), strings.Repeat("progress\n", i+1))
		g.watchdog()
	}
	if prompts := g.h.promptTexts(); len(prompts) != 1 {
		t.Fatalf("prompts %q: a turn past its timeout whose screen still changes was nudged", prompts)
	}
}

func TestShiftMovesTheTurnDeadlines(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.send("fx-repro", "reproduce the bug")
	timeout := g.role("fx-repro").Timeout.Duration
	g.clock.Add(timeout - NoProgressAfter)
	g.watchdog()

	g.r.Shift(2 * time.Hour) // the Mac slept

	g.clock.Add(2 * NoProgressAfter)
	g.watchdog()
	if prompts := g.h.promptTexts(); len(prompts) != 1 {
		t.Fatalf("prompts %q: the watchdog nudged a turn whose deadline the sleep moved", prompts)
	}
}

func TestALostPaneResumesTwiceThenStartsFresh(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	first := "reproduce the bug"
	g.send("fx-repro", first)
	g.observeN(2)

	lose := func() []Event {
		t.Helper()
		g.h.losePane(g.agent("fx-repro"))
		evs := g.observe()
		g.observeN(2) // past the lost grace of the session that took its place
		return evs
	}
	for i := 1; i <= MaxResumes; i++ {
		if evs := lose(); len(evs) != 0 {
			t.Fatalf("loss %d gave %+v, want a resume", i, evs)
		}
		s := g.h.startCalls()
		if len(s) != 1+i || !strings.Contains(strings.Join(s[i].Args, " "), "--resume sid-1") {
			t.Fatalf("start %d %+v, want resume %d of sid-1", i+1, s[len(s)-1], i)
		}
	}

	if evs := lose(); len(evs) != 0 {
		t.Fatalf("the third loss gave %+v, want a fresh session", evs)
	}
	s := g.h.startCalls()
	if len(s) != 4 || strings.Contains(strings.Join(s[3].Args, " "), "--resume") {
		t.Fatalf("starts %+v, want a fresh fourth", s)
	}
	if prompts := g.h.promptTexts(); prompts[len(prompts)-1] != first {
		t.Fatalf("the fresh session got %q, want the first prompt", prompts[len(prompts)-1])
	}

	evs := lose()
	if len(evs) != 1 || evs[0].Kind != EventLost || evs[0].Reason != "no progress after 3 recoveries" {
		t.Fatalf("the fourth loss gave %+v, want lost after 3 recoveries", evs)
	}
	if len(g.h.startCalls()) != 4 {
		t.Fatal("a recovery ran after the recoveries ran out")
	}
}

func TestARecoveryWithoutProgressMovesOnAfter10Minutes(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observeN(2)
	g.h.losePane(g.agent("fx-repro"))
	g.observe() // resume 1
	starts := len(g.h.startCalls())

	g.clock.Add(NoProgressAfter - time.Minute)
	g.watchdog()
	if len(g.h.startCalls()) != starts {
		t.Fatal("the recovery moved on before its 10 minutes")
	}
	var evs []Event
	for range 4 {
		g.clock.Add(NoProgressAfter)
		evs = append(evs, g.watchdog()...)
	}
	if got := len(g.h.startCalls()); got != starts+2 {
		t.Fatalf("%d starts after the first resume, want the second resume and the fresh session", got-starts)
	}
	if lost := only(evs, EventLost); len(lost) != 1 || lost[0].Reason != "no progress after 3 recoveries" {
		t.Fatalf("events %+v, want lost once after 3 recoveries", evs)
	}
}

func TestALoopWithoutProgressCompactsThenStartsFresh(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	first := "reproduce the bug"
	send := func(i int) (string, error) {
		g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
		text := first
		if i > 1 {
			text = "nudge " + strings.Repeat("!", i)
		}
		return g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), text)
	}
	for i := 1; i <= MaxStagePrompts; i++ {
		if _, err := send(i); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
	}
	if slices.ContainsFunc(g.h.runCalls(), func(r paneRunCall) bool { return r.Command == agents.CompactCommand }) {
		t.Fatal("compacted within the stage's prompt budget")
	}

	// The seventh prompt of the stage: compact first.
	if _, err := send(MaxStagePrompts + 1); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(g.h.runCalls(), func(r paneRunCall) bool { return r.Command == agents.CompactCommand }) {
		t.Fatalf("pane runs %+v, want /compact before the seventh prompt", g.h.runCalls())
	}
	if n := len(g.h.promptTexts()); n != MaxStagePrompts+1 {
		t.Fatalf("%d prompts, want the seventh sent after the compaction", n)
	}

	// Another stage's worth: one fresh session from the files.
	for i := MaxStagePrompts + 2; i <= 2*MaxStagePrompts; i++ {
		if _, err := send(i); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
	}
	starts := len(g.h.startCalls())
	if _, err := send(2*MaxStagePrompts + 1); err != nil {
		t.Fatal(err)
	}
	s := g.h.startCalls()
	if len(s) != starts+1 || strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume") {
		t.Fatalf("starts %+v, want one fresh session", s[starts:])
	}
	prompts := g.h.promptTexts()
	if prompts[len(prompts)-1] != first {
		t.Fatalf("the fresh session got %q, want the first prompt from the files", prompts[len(prompts)-1])
	}

	// And then the operator.
	for i := 2*MaxStagePrompts + 2; i <= 3*MaxStagePrompts; i++ {
		if _, err := send(i); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
	}
	before := len(g.h.promptTexts())
	if _, err := send(3*MaxStagePrompts + 1); !errors.Is(err, ErrNoProgress) {
		t.Fatalf("Send after the fresh session's budget = %v, want ErrNoProgress", err)
	}
	if len(g.h.promptTexts()) != before {
		t.Fatal("a prompt went out after the loop's steps ran out")
	}
	evs := only(g.observe(), EventNoProgress)
	if len(evs) != 1 || evs[0].Role != "fx-repro" || evs[0].TaskID != g.task.ID {
		t.Fatalf("events %+v, want no progress for fx-repro", evs)
	}
	if _, err := send(3*MaxStagePrompts + 2); !errors.Is(err, ErrNoProgress) {
		t.Fatalf("a later Send = %v, want ErrNoProgress until the operator acts", err)
	}
	if evs := only(g.observe(), EventNoProgress); len(evs) != 0 {
		t.Fatalf("the loop was reported again: %+v", evs)
	}

	// The operator's retry is a new attempt of the stage: the ladder climbs again.
	g.attempt = g.begin(g.task, "investigate")
	g.launch("fx-repro")
	if _, err := send(1); err != nil {
		t.Fatalf("the retry's first prompt: %v", err)
	}
}

func TestAContextOver85PercentCompactsTheSession(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.send("fx-repro", "reproduce the bug")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	g.h.screen(g.agent("fx-repro"), "done\n❯ \nOpus 5.5 | copy | 870k/1m (87%)\n")

	g.send("fx-repro", "write findings.md")

	if !slices.ContainsFunc(g.h.runCalls(), func(r paneRunCall) bool { return r.Command == agents.CompactCommand }) {
		t.Fatalf("pane runs %+v, want /compact at 87%% of the context", g.h.runCalls())
	}
}
