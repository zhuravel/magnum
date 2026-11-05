package runtime

import (
	"errors"
	"fmt"
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

	// Step 1: the agent is nudged (StopShell stops nothing for an agent's
	// run, only a shell role's command).
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
	if last := prompts[len(prompts)-1]; !strings.HasPrefix(last, first) || !strings.Contains(last, "nudge "+strings.Repeat("!", 2*MaxStagePrompts+1)) {
		t.Fatalf("the fresh session got %q, want the first prompt from the files and the caller's text", last)
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

// lostThenResumed launches role with a conversation, sends it a prompt,
// loses its pane and lets the runtime resume it.
func (g *rig) lostThenResumed(role, sid string) {
	g.t.Helper()
	g.launch(role)
	g.h.setSession(g.agent(role), sid)
	g.send(role, "work on the task")
	g.observeN(2)
	g.h.losePane(g.agent(role))
	g.observe()
	if s := g.h.startCalls(); len(s) != 2 || !strings.Contains(strings.Join(s[1].Args, " "), sid) {
		g.t.Fatalf("starts %+v, want the conversation resumed", s)
	}
	g.observe()
}

// quietHour runs the watchdog and the observe pass every 10 minutes for an
// hour and returns their events.
func (g *rig) quietHour() []Event {
	g.t.Helper()
	var evs []Event
	for range 6 {
		g.clock.Add(NoProgressAfter)
		evs = append(evs, g.watchdog()...)
		evs = append(evs, g.observe()...)
	}
	return evs
}

// A recovered Codex role that is refused is never started again: no
// recovery, no retry, no lost pane at the end.
func TestARecoveredRoleThatIsRefusedIsNeverStartedAgain(t *testing.T) {
	g := newRig(t)
	g.lostThenResumed("fx-cause", "11111111-1111-4111-8111-111111111111")
	starts, prompts := len(g.h.startCalls()), len(g.h.promptTexts())

	g.h.screen(g.agent("fx-cause"), "■ This content was flagged for possible cybersecurity risk.\n")
	g.h.setStatus(g.agent("fx-cause"), herdr.StatusIdle)
	evs := g.observeN(2)
	if len(only(evs, EventRefused)) != 1 {
		t.Fatalf("events %+v, want the refusal", evs)
	}

	evs = g.quietHour()
	if n := len(g.h.startCalls()); n != starts {
		t.Fatalf("%d starts after the refusal: the refused conversation was started again", n-starts)
	}
	if got := g.h.promptTexts()[prompts:]; len(got) != 0 {
		t.Fatalf("prompts after the refusal %q", got)
	}
	if lost := only(evs, EventLost); len(lost) != 0 {
		t.Fatalf("the refusal ended as %+v", lost)
	}
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-cause"), "go on"); err == nil {
		t.Fatal("a prompt went to fx-cause after its refusal")
	}
	// Before the engine marks the task never for Codex, its Codex roles run
	// as their Claude fallback already.
	if runs, err := g.r.RunsAs(g.task, g.role("fx-cause")); err != nil || runs.Name != "fx-cause-claude" {
		t.Fatalf("after the refusal fx-cause runs as %q (%v), want fx-cause-claude", runs.Name, err)
	}
}

// A recovered role that pauses on its CLI's usage limit waits for the
// pause: no resume, no fresh session, no lost pane.
func TestARecoveredRoleThatPausesWaitsForThePause(t *testing.T) {
	g := newRig(t)
	g.lostThenResumed("fx-repro", "sid-1")
	starts, prompts := len(g.h.startCalls()), len(g.h.promptTexts())

	g.h.screen(g.agent("fx-repro"), "You've hit your limit · resets 4pm (UTC)\n")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	if paused := only(g.observeN(2), EventPaused); len(paused) != 1 {
		t.Fatalf("events %+v, want the pause", paused)
	}

	evs := g.quietHour()
	if n := len(g.h.startCalls()); n != starts {
		t.Fatalf("%d starts during the pause", n-starts)
	}
	if got := g.h.promptTexts()[prompts:]; len(got) != 0 {
		t.Fatalf("prompts during the pause %q", got)
	}
	if lost := only(evs, EventLost); len(lost) != 0 {
		t.Fatalf("the pause ended as %+v", lost)
	}
}

// ---- fix 3

// The CLIs show the time a turn has run while it works; a ticking timer
// is no progress, and the watchdog still takes its step.
func TestTheWatchdogSeesThroughACLIsTickingTimer(t *testing.T) {
	for _, tc := range []struct {
		role  string
		timer func(m int) string
	}{
		{"fx-repro", func(m int) string {
			return fmt.Sprintf("Bash(bin/rspec)\n✻ Cogitating… (%dm %ds · ↓ %d.%dk tokens · esc to interrupt)\n", m, (m*7)%60, m, m%10)
		}},
		{"fx-cause", func(m int) string {
			return fmt.Sprintf("• Running bin/rspec\n\n• Working (%dm %02ds • esc to interrupt)\n", m, m%60)
		}},
	} {
		t.Run(tc.role, func(t *testing.T) {
			g := newRig(t)
			g.launch(tc.role)
			g.send(tc.role, "find out why it fails")
			timeout := g.role(tc.role).Timeout.Duration
			g.clock.Add(timeout - NoProgressAfter)
			for m := range 11 {
				g.h.screen(g.agent(tc.role), tc.timer(int(timeout/time.Minute)-10+m))
				g.watchdog()
				g.clock.Add(time.Minute)
			}
			prompts := g.h.promptTexts()
			if len(prompts) != 2 || !strings.Contains(prompts[1], "stuck") {
				t.Fatalf("prompts %q: a turn whose screen shows only its timer ticking was never nudged", prompts)
			}
		})
	}
}

// ---- fix 4

// sendLoop sends prompt i of a loop to role (the first is the attempt's
// first prompt), the agent idle in between.
func (g *rig) sendLoop(role string, i int) (string, error) {
	g.h.setStatus(g.agent(role), herdr.StatusIdle)
	text := "reproduce the bug"
	if i > 1 {
		text = "nudge " + strings.Repeat("!", i)
	}
	return g.r.Send(g.ctx, g.task, g.attempt, g.role(role), text)
}

func TestTheLoopLadderNeverQuitsAPaneTheOperatorIsIn(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	for i := 1; i <= 2*MaxStagePrompts; i++ {
		if _, err := g.sendLoop("fx-repro", i); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
	}
	starts, prompts, keys := len(g.h.startCalls()), len(g.h.promptTexts()), len(g.h.keyCalls())
	g.h.focus(g.agent("fx-repro")) // the operator looks at the pane

	_, err := g.sendLoop("fx-repro", 2*MaxStagePrompts+1)

	if !errors.Is(err, ErrOperatorInPane) {
		t.Fatalf("the fresh step with you in the pane = %v, want ErrOperatorInPane", err)
	}
	if len(g.h.startCalls()) != starts || len(g.h.promptTexts()) != prompts || len(g.h.keyCalls()) != keys {
		t.Fatal("the pane the operator is in was quit, restarted or prompted")
	}

	g.h.focus("") // gone
	if _, err := g.sendLoop("fx-repro", 2*MaxStagePrompts+1); err != nil {
		t.Fatal(err)
	}
	if len(g.h.startCalls()) != starts+1 {
		t.Fatal("the fresh step did not run once the operator left")
	}
}

func TestTheLoopLadderFreshStepSendsTheCallersText(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	for i := 1; i <= 2*MaxStagePrompts; i++ {
		if _, err := g.sendLoop("fx-repro", i); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
	}
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)

	id, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "write findings.md now")
	if err != nil {
		t.Fatal(err)
	}

	prompts := g.h.promptTexts()
	last := prompts[len(prompts)-1]
	if !strings.Contains(last, "reproduce the bug") || !strings.Contains(last, "write findings.md now") {
		t.Fatalf("the fresh session got %q, want the first prompt and the caller's text", last)
	}
	if r := g.storeRun(id); r.PromptText != last {
		t.Fatalf("Send returned run %s with prompt %q, not the fresh session's", id, r.PromptText)
	}
}

func TestAStillScreenDropsOnlyTheCLIsTimerLines(t *testing.T) {
	screen := "• Ran bin/rspec (12s)\n✻ Cogitating… (3m 12s · ↓ 2.1k tokens)\n• Working (12m 03s • esc to interrupt)\n› "
	if got := stillScreen(screen); got != "• Ran bin/rspec (12s)\n› " {
		t.Fatalf("stillScreen = %q, want the tool line and the composer only", got)
	}
	// Codex's wait on a command it runs in the background ticks every
	// second: a hung command must read as still, so the watchdog steps in.
	for _, line := range []string{"Waiting for background terminal (3m 2s)", "◦ Waiting for background terminal (12m 40s • esc to interrupt)",
		"  waiting for background terminal (45s)"} {
		if got := stillScreen("• Ran bin/rspec (12s)\n" + line + "\n› "); got != "• Ran bin/rspec (12s)\n› " {
			t.Errorf("stillScreen kept %q: %q", line, got)
		}
	}
	if got := stillScreen("• Waited for background terminal output (2s)\n› "); got == "› " {
		t.Errorf("stillScreen dropped a tool line that only carries its duration: %q", got)
	}
}
