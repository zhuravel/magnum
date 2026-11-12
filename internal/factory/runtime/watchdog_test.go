package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// TurnText reads, in a ledger entry's text as the ledger keeps it (scrubbed),
// the prompt its turn is for: a fresh session's first prompt carries the
// caller's after the attempt's first, and the factory tells a fix turn by
// it; any other entry is its own text.
func TestTurnTextIsWhatAFreshSessionCarriesAfterTheFirstPrompt(t *testing.T) {
	const first = "Read the task's files and make the change."
	const fix = "The factory checked your change after your turn: precheck: lint: 1 offense on the changed lines."
	for _, tc := range []struct{ text, want string }{
		{ledgerText(freshText(first, fix)), fix},
		{ledgerText(freshText(first, "")), first},
		{ledgerText(fix), fix},
		{ledgerText(nudgeText), nudgeText},
	} {
		if got := TurnText(tc.text); got != tc.want {
			t.Errorf("TurnText(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

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

// A lost pane is recovered MaxRecoveries times: the first at once, the
// second 1 minute and the third 5 minutes after its loss; its conversation
// resumed twice, then a fresh session from the attempt's first prompt; a
// loss after that is EventLost, the stage's question for you. Each recovery
// is a recover event and an alert.
func TestALostPaneIsRecoveredThreeTimesAfterItsWaitsTheLastInAFreshSession(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	first := "reproduce the bug"
	g.send("fx-repro", first)
	g.observeN(2)

	waits := []time.Duration{0, time.Minute, 5 * time.Minute}
	lose := func(n int) []Event {
		t.Helper()
		starts := len(g.h.startCalls())
		g.h.losePane(g.agent("fx-repro"))
		evs := g.observe()
		if wait := waits[n-1]; wait > 0 {
			g.clock.Add(wait - 20*time.Second)
			evs = append(evs, g.watchdog()...)
			if len(g.h.startCalls()) != starts {
				t.Fatalf("recovery %d ran before its wait of %s", n, wait)
			}
			g.clock.Add(20 * time.Second) // past the wait
			evs = append(evs, g.watchdog()...)
		}
		if len(g.h.startCalls()) != starts+1 {
			t.Fatalf("recovery %d did not run after its wait of %s", n, waits[n-1])
		}
		g.observeN(2) // past the lost grace of the session that took its place
		return evs
	}
	for n := 1; n < MaxRecoveries; n++ {
		if evs := lose(n); len(evs) != 0 {
			t.Fatalf("loss %d gave %+v, want a resume", n, evs)
		}
		s := g.h.startCalls()
		if len(s) != 1+n || !strings.Contains(strings.Join(s[n].Args, " "), "--resume sid-1") {
			t.Fatalf("start %d %+v, want resume %d of sid-1", n+1, s[len(s)-1], n)
		}
	}
	if evs := lose(MaxRecoveries); len(evs) != 0 {
		t.Fatalf("the third loss gave %+v, want a fresh session", evs)
	}
	s := g.h.startCalls()
	if len(s) != 1+MaxRecoveries || strings.Contains(strings.Join(s[MaxRecoveries].Args, " "), "--resume") {
		t.Fatalf("starts %+v, want a fresh fourth", s)
	}
	if prompts := g.h.promptTexts(); prompts[len(prompts)-1] != first {
		t.Fatalf("the fresh session got %q, want the first prompt", prompts[len(prompts)-1])
	}

	g.h.losePane(g.agent("fx-repro"))
	evs := g.observe()
	if len(evs) != 1 || evs[0].Kind != EventLost || evs[0].Reason != "no progress after 3 recoveries" {
		t.Fatalf("the fourth loss gave %+v, want lost after 3 recoveries", evs)
	}
	if len(g.h.startCalls()) != 1+MaxRecoveries {
		t.Fatal("a recovery ran after the recoveries ran out")
	}
	recs, err := g.reg.TaskEvents(g.ctx, g.task.ID, recoverKind, 0)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, ev := range recs {
		if strings.Contains(ev.Text, ": "+ranOutWord) {
			steps = append(steps, ranOutWord)
			continue
		}
		steps = append(steps, ev.Text[strings.Index(ev.Text, "(")+1:strings.Index(ev.Text, ")")])
	}
	if !slices.Equal(steps, []string{"resume", "resume", "fresh", ranOutWord}) {
		t.Fatalf("recover events %+v, want resume, resume, fresh, then ran out", recs)
	}
	alerts, err := g.reg.TaskEvents(g.ctx, g.task.ID, "alert", 0)
	if err != nil || len(alerts) != MaxRecoveries || !strings.Contains(alerts[0].Text, "recovers fx-repro's lost pane by itself") {
		t.Fatalf("alerts %+v (%v), want one for each recovery", alerts, err)
	}
	for _, a := range alerts { // news the factory's delivery reads as such
		if _, what, _ := strings.Cut(a.Text, ": "); !RecoveryAlert(what) {
			t.Errorf("alert %q is no recovery alert as RecoveryAlert reads it", a.Text)
		}
	}
}

// The count of a role's recoveries lives in the task's recover events: a
// restart after each recovery takes the next one, after its wait, never one
// twice, and the role is lost after MaxRecoveries, as with no restart.
func TestARestartBetweenTheRecoveriesNeitherDoublesNorLosesOne(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observeN(2)
	for n := 1; n <= MaxRecoveries; n++ {
		starts := len(g.h.startCalls())
		g.h.losePane(g.agent("fx-repro"))
		g.observe()
		g.restart() // the recovery's wait in memory is gone: the rebuilt stage asks for it again
		if err := g.r.RecoverLost(g.ctx, g.task, g.attempt, g.role("fx-repro")); err != nil {
			t.Fatalf("RecoverLost after restart %d: %v", n, err)
		}
		g.afterWait(n)
		if got := len(g.h.startCalls()); got != starts+1 {
			t.Fatalf("recovery %d after a restart: %d starts, want one", n, got-starts)
		}
		g.observeN(2)
	}
	g.h.losePane(g.agent("fx-repro"))
	g.restart()
	evs := g.observe()
	if lost := only(evs, EventLost); len(lost) != 1 || lost[0].Reason != "no progress after 3 recoveries" {
		t.Fatalf("events %+v after the restarts, want lost after 3 recoveries", evs)
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
	for range 8 {
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

	// A new attempt of the stage (a send back, a merge turn) climbs again. Your
	// retry goes on in the same attempt: TestARetryInTheSameAttemptStartsARunOutLadderOver.
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

// Codex-never fails closed (the operator's account-risk rule): with a stale
// copy of the task and a registry that cannot be read, a Codex role runs as
// its Claude fallback, since the mark may be there; a Claude role runs as
// itself, whatever the registry says.
func TestCodexNeverFailsClosedWhenTheRegistryCannotBeRead(t *testing.T) {
	g := newRig(t)
	if err := g.reg.Close(); err != nil {
		t.Fatal(err)
	}
	if runs, err := g.r.RunsAs(g.task, g.role("fx-cause")); err != nil || runs.Name != "fx-cause-claude" {
		t.Fatalf("with the registry unreadable fx-cause runs as %q (%v), want its Claude fallback", runs.Name, err)
	}
	if runs, err := g.r.RunsAs(g.task, g.role("fx-repro")); err != nil || runs.Name != "fx-repro" {
		t.Fatalf("with the registry unreadable fx-repro runs as %q (%v), want itself", runs.Name, err)
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
	typed := func(at time.Time) {
		t.Helper()
		if err := g.st.UpdatePR(g.ctx, g.owner().ID, func(u *store.PRUpdate) { u.Set("human_active_at", at) }); err != nil {
			t.Fatal(err)
		}
	}
	typed(g.clock.Now()) // the operator types in the pane

	_, err := g.sendLoop("fx-repro", 2*MaxStagePrompts+1)

	if !errors.Is(err, ErrOperatorInPane) {
		t.Fatalf("the fresh step with you in the pane = %v, want ErrOperatorInPane", err)
	}
	if len(g.h.startCalls()) != starts || len(g.h.promptTexts()) != prompts || len(g.h.keyCalls()) != keys {
		t.Fatal("the pane the operator is in was quit, restarted or prompted")
	}

	typed(g.clock.Now().Add(-time.Hour)) // gone
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

// A pass the runtime contains logs its panic and its stack redacted, as the
// engine's own containment does: a panic's value can carry a pane's text.
func TestAContainedPanicIsRedacted(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	const token = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	err := func() (err error) {
		defer contain("observe", log, &err)
		panic("the pane shows " + token)
	}()
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(logs.String(), token) {
		t.Fatalf("err %v, log %q: want the panic contained and the token redacted", err, logs.String())
	}
}

// ---- a failed step stays watched (R13)

// hungTurn launches fx-repro on conversation sid-1, sends it first, and lets
// its turn hang until the watchdog took its first step (the nudge): the
// next pass NoProgressAfter later is due step 2, the resume. It returns the
// hung run.
func (g *rig) hungTurn(first string) string {
	g.t.Helper()
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-1")
	id := g.send("fx-repro", first)
	g.observe()
	g.h.screen(g.agent("fx-repro"), "Bash(bin/rspec spec/models/user_spec.rb)\nrunning rspec\n")
	g.clock.Add(g.role("fx-repro").Timeout.Duration - NoProgressAfter)
	g.watchdog()
	g.clock.Add(NoProgressAfter)
	g.watchdog()
	if p := g.h.promptTexts(); len(p) != 2 || !strings.Contains(p[1], "stuck") {
		g.t.Fatalf("prompts %q, want the first and the watchdog's nudge", p)
	}
	return id
}

// hungRunLeft fails t unless the hung run id is as a failed step leaves it:
// still in flight on its live session when the step failed at the quit
// (the watchdog watches the role through it), else abandoned for the
// recovery that quit its session (RecoveryPrefix), so nothing holds the
// task's park and the store says the role is due a recovery.
func (g *rig) hungRunLeft(id string, quitFailed bool, when string) {
	g.t.Helper()
	r := g.storeRun(id)
	switch {
	case quitFailed && r.State != store.RunSubmitted && r.State != store.RunWorking:
		g.t.Fatalf("%s the hung run is %s, want it still in flight on its live session, so the watchdog watches its role", when, r.State)
	case !quitFailed && (r.State != store.RunAbandoned || !strings.HasPrefix(store.Deref(r.Error), RecoveryPrefix)):
		g.t.Fatalf("%s the hung run is %s (%q), want it abandoned once its session was quit, with %q", when, r.State,
			store.Deref(r.Error), RecoveryPrefix)
	}
}

// The faults a watchdog step can meet partway, after it decided to restart
// the role: the quit of the hung session, the start of its replacement, and
// the replacement's prompt, which herdr refuses. quits says whether the
// hung session was quit before the fault.
var stepFaults = []struct {
	name   string
	quits  bool
	inject func(g *rig)
}{
	{"the quit fails", false, func(g *rig) {
		g.h.keysErr = func(string) error { return errors.New("herdr: connection reset by peer") }
	}},
	{"the start fails", true, func(g *rig) {
		g.h.startErr = func(herdr.AgentStartOptions) error {
			return &herdr.Error{Method: "agent.start", Code: "agent_start_failed", Message: "the agent did not come up"}
		}
	}},
	{"the prompt never leaves", true, func(g *rig) {
		g.h.promptErr = func(string) error {
			return &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentNotFound, Message: "no such agent"}
		}
	}},
}

// A watchdog step that fails partway (its quit, its start, its prompt)
// leaves the role watched: the hung run stays in flight while its session
// does, and once the step quit that session the run is abandoned and the
// role is still due its next step, which comes NoProgressAfter later and
// starts a fresh session.
func TestAWatchdogStepThatFailsPartwayKeepsTheRoleWatched(t *testing.T) {
	for _, fault := range stepFaults {
		t.Run(fault.name, func(t *testing.T) {
			g := newRig(t)
			first := "reproduce the bug"
			hung := g.hungTurn(first)
			starts := len(g.h.startCalls())
			fault.inject(g)

			g.clock.Add(NoProgressAfter)
			if evs := g.watchdog(); len(evs) != 0 {
				t.Fatalf("the failed step gave %+v, want nothing yet", evs)
			}
			g.hungRunLeft(hung, !fault.quits, "after the failed resume")

			g.h.keysErr, g.h.startErr, g.h.promptErr = nil, nil, nil
			g.clock.Add(NoProgressAfter / 2)
			g.watchdog()
			if n := len(g.h.startCalls()); n > starts+1 {
				t.Fatalf("%d starts 5 minutes after the failed step, want the next step to wait its 10 minutes", n-starts)
			}
			g.clock.Add(NoProgressAfter / 2)
			g.watchdog()
			s := g.h.startCalls()
			if len(s) == starts || strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume") {
				t.Fatalf("starts %+v after the failed step, want the next step's fresh session", s[starts:])
			}
			if p := g.h.promptTexts(); p[len(p)-1] != first {
				t.Fatalf("the fresh session got %q, want the first prompt", p[len(p)-1])
			}
			if r := g.storeRun(hung); r.State != store.RunAbandoned {
				t.Fatalf("the hung run is %s once a fresh session replaced it, want abandoned", r.State)
			}
			if role, _, err := g.r.AtWork(g.ctx, g.task); err != nil || role != "fx-repro" {
				t.Fatalf("AtWork = %q, %v; want the fresh session's turn at work", role, err)
			}
		})
	}
}

// Steps that all fail end where steps that do not help end: EventStuck, so
// the task needs you, never a hung role nobody watches. Once the steps quit
// its session, nothing of the role stays in flight: the task parks, and
// nobody is at work in its copy.
func TestAWatchdogWhoseStepsAllFailEndsStuck(t *testing.T) {
	for _, fault := range stepFaults {
		t.Run(fault.name, func(t *testing.T) {
			g := newRig(t)
			hung := g.hungTurn("reproduce the bug")
			fault.inject(g)
			var evs []Event
			for range 3 {
				g.clock.Add(NoProgressAfter)
				evs = append(evs, g.watchdog()...)
			}
			if stuck := only(evs, EventStuck); len(stuck) != 1 || stuck[0].Role != "fx-repro" || stuck[0].TaskID != g.task.ID {
				t.Fatalf("events %+v after every step failed, want fx-repro stuck once", evs)
			}
			g.hungRunLeft(hung, !fault.quits, "once it is stuck")
			if !fault.quits {
				return // its hung turn is still at work in its live session
			}
			g.h.keysErr, g.h.startErr, g.h.promptErr = nil, nil, nil
			if role, you, err := g.r.AtWork(g.ctx, g.task); err != nil || role != "" || you {
				t.Fatalf("AtWork = %q, %v, %v once it is stuck; want nobody at work in the copy", role, you, err)
			}
			g.endTask(registry.TaskStopped)
			g.restart()
			if err := g.r.Park(g.ctx, g.task); err != nil {
				t.Fatalf("Park of the stopped task = %v, want its agents parked", err)
			}
		})
	}
}

// A failed step survives a restart of the daemon. One that failed at the
// quit left the hung run in flight in the agents store, and the fresh
// runtime's watchdog takes its ladder over from it; one that failed after
// the quit left the run abandoned for a recovery, and RecoverLost, which the
// stage calls on that reason once it is rebuilt, recovers the role.
func TestAFailedWatchdogStepStaysWatchedAcrossARestart(t *testing.T) {
	g := newRig(t)
	g.hungTurn("reproduce the bug")
	stepFaults[0].inject(g)
	g.clock.Add(NoProgressAfter)
	g.watchdog()
	g.h.keysErr = nil
	before := len(g.h.promptTexts())

	g.restart()
	for range 2 {
		g.clock.Add(NoProgressAfter)
		g.watchdog()
	}
	if p := g.h.promptTexts(); len(p) != before+1 || !strings.Contains(p[len(p)-1], "stuck") {
		t.Fatalf("prompts after the restart %q, want the fresh runtime's nudge on the hung turn", p[before:])
	}
}

// After a step that failed once it quit the session, the role is the
// watchdog's: RecoverLost, which the stage calls when it is rebuilt from a
// run a recovery abandoned, starts nothing more in the same runtime, and
// the climb's next step comes on its pace. After a restart RecoverLost
// recovers the role itself.
func TestRecoverLostLeavesARoleTheWatchdogHasAloneAndRecoversItAfterARestart(t *testing.T) {
	g := newRig(t)
	g.hungTurn("reproduce the bug")
	stepFaults[1].inject(g) // the start fails
	g.clock.Add(NoProgressAfter)
	g.watchdog()
	g.h.startErr = nil
	starts := len(g.h.startCalls())

	if err := g.r.RecoverLost(g.ctx, g.task, g.attempt, g.role("fx-repro")); err != nil {
		t.Fatal(err)
	}
	if n := len(g.h.startCalls()); n != starts {
		t.Fatalf("%d starts from RecoverLost while the watchdog had the role, want none", n-starts)
	}
	g.clock.Add(NoProgressAfter)
	g.watchdog()
	if n := len(g.h.startCalls()); n != starts+1 {
		t.Fatalf("%d starts NoProgressAfter later, want the climb's next step", n-starts)
	}

	h := newRig(t)
	hung := h.hungTurn("reproduce the bug")
	stepFaults[1].inject(h)
	h.clock.Add(NoProgressAfter)
	h.watchdog()
	h.h.startErr = nil
	if _, why, err := h.r.RunState(h.ctx, hung); err != nil || !strings.HasPrefix(why, RecoveryPrefix) {
		t.Fatalf("RunState of the hung run = %q, %v; want the recovery's reason, which a rebuilt stage reads", why, err)
	}
	h.restart()
	starts = len(h.h.startCalls())
	if err := h.r.RecoverLost(h.ctx, h.task, h.attempt, h.role("fx-repro")); err != nil {
		t.Fatal(err)
	}
	if s := h.h.startCalls(); len(s) != starts+1 || !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume sid-1") {
		t.Fatalf("starts %+v after the restart, want the role's conversation resumed", s[starts:])
	}
}

// A role whose steps ran out with nothing of it at work (every step failed
// once the recovery quit its session, or its lost pane's recoveries ran
// out) starts over when RecoverLost asks for it, which the stage does once
// your retry rebuilt it: its ladders begin again in the same daemon. A role
// stuck on a turn still at work in its live pane is left as it is: its end,
// or you in its pane, moves it.
func TestRecoverLostStartsOverARoleWhoseStepsRanOutWithNothingAtWork(t *testing.T) {
	g := newRig(t)
	g.hungTurn("reproduce the bug")
	stepFaults[1].inject(g) // the start fails, after the quit
	var evs []Event
	for range 3 {
		g.clock.Add(NoProgressAfter)
		evs = append(evs, g.watchdog()...)
	}
	if stuck := only(evs, EventStuck); len(stuck) != 1 {
		t.Fatalf("events %+v, want fx-repro stuck", evs)
	}
	g.h.startErr = nil // you fixed the CLI
	starts, prompts := len(g.h.startCalls()), len(g.h.promptTexts())

	if err := g.r.RecoverLost(g.ctx, g.task, g.attempt, g.role("fx-repro")); err != nil {
		t.Fatal(err)
	}
	if s := g.h.startCalls(); len(s) != starts+1 || !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume sid-1") {
		t.Fatalf("starts %+v after your retry, want the role's conversation resumed", s[starts:])
	}
	if p := g.h.promptTexts(); len(p) != prompts+1 || p[len(p)-1] != resumeText {
		t.Fatalf("prompts %q after your retry, want the resume prompt", p[prompts:])
	}

	h := newRig(t)
	h.hungTurn("reproduce the bug")
	for range 3 {
		h.clock.Add(NoProgressAfter)
		h.watchdog()
	}
	starts = len(h.h.startCalls())
	if err := h.r.RecoverLost(h.ctx, h.task, h.attempt, h.role("fx-repro")); err != nil {
		t.Fatal(err)
	}
	if n := len(h.h.startCalls()); n != starts {
		t.Fatalf("%d starts: RecoverLost restarted a role whose stuck turn is at work in its live pane", n-starts)
	}

	l := newRig(t)
	l.launch("fx-repro")
	l.h.setSession(l.agent("fx-repro"), "sid-1")
	l.send("fx-repro", "reproduce the bug")
	l.observeN(2)
	var lost []Event
	for n := 1; n <= MaxRecoveries+1; n++ {
		l.h.losePane(l.agent("fx-repro"))
		lost = append(lost, l.observe()...)
		if n <= MaxRecoveries {
			lost = append(lost, l.afterWait(n)...)
		}
		l.observeN(2)
	}
	if len(only(lost, EventLost)) != 1 {
		t.Fatalf("events %+v, want the lost pane's recoveries run out", lost)
	}
	starts = len(l.h.startCalls())
	if err := l.r.RecoverLost(l.ctx, l.task, l.attempt, l.role("fx-repro")); err != nil {
		t.Fatal(err)
	}
	if s := l.h.startCalls(); len(s) != starts+1 {
		t.Fatalf("starts %+v after your retry of a lost role, want its recoveries started over", s[starts:])
	}
}

// A recovery that quits a session whose conversation never took a prompt
// (a fresh session whose first prompt herdr refused) closes that
// conversation, so the resume after it takes the role's own conversation,
// the one that holds the task, and never tells an empty one to continue
// where it stopped.
func TestARecoveryNeverResumesAConversationThatTookNoPrompt(t *testing.T) {
	g := newRig(t)
	g.hungTurn("reproduce the bug")
	stepFaults[2].inject(g) // every prompt is refused
	g.clock.Add(NoProgressAfter)
	g.watchdog() // step 2: sid-1 resumed, its prompt refused
	g.clock.Add(NoProgressAfter)
	g.watchdog() // step 3: a fresh session, its first prompt refused
	g.h.setSession(g.agent("fx-repro"), "sid-fresh")
	g.observe()
	var evs []Event
	for range 2 {
		g.clock.Add(NoProgressAfter)
		evs = append(evs, g.watchdog()...)
	}
	if len(only(evs, EventStuck)) != 1 {
		t.Fatalf("events %+v, want fx-repro stuck", evs)
	}
	g.h.promptErr = nil
	starts, prompts := len(g.h.startCalls()), len(g.h.promptTexts())

	if err := g.r.RecoverLost(g.ctx, g.task, g.attempt, g.role("fx-repro")); err != nil {
		t.Fatal(err)
	}
	s := g.h.startCalls()
	if len(s) != starts+1 || !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume sid-1") {
		t.Fatalf("starts %+v after your retry, want the role's own conversation, sid-1, resumed", s[starts:])
	}
	if p := g.h.promptTexts(); len(p) != prompts+1 || p[len(p)-1] != resumeText {
		t.Fatalf("prompts %q after your retry, want the resume prompt", p[prompts:])
	}
}

// A recovery's prompt that herdr may have typed (its delivery unknown), and
// that two reads of the transcript then prove never arrived, leaves no role
// alone, across a restart too: the runs it replaced were abandoned for the
// recovery (RecoveryPrefix), so the stage, rebuilt on EventUndelivered or
// after the restart, calls RecoverLost, which recovers the role again.
func TestARecoveryPromptThatNeverArrivedIsRecoveredAgain(t *testing.T) {
	g := newRig(t)
	hung := g.hungTurn("reproduce the bug")
	g.h.promptErr = func(text string) error {
		if text == resumeText {
			return fmt.Errorf("herdr agent.prompt: connection closed without a response: %w", io.EOF)
		}
		return nil
	}
	g.clock.Add(NoProgressAfter)
	g.watchdog() // step 2: resumed, and its prompt's fate unknown
	g.h.promptErr = nil
	es := g.ledger()
	e := es[len(es)-1]
	if e.Text != resumeText || e.Delivered != registry.DeliveredUnknown {
		t.Fatalf("the last ledger entry %q is %s, want the resume prompt, unknown", e.Text, e.Delivered)
	}
	g.writeTranscript("sid-1", assistantLine(e.Sent.Add(30*time.Second), "the agent wrote after the prompt left"))
	var evs []Event
	for range 4 {
		g.clock.Add(time.Minute)
		evs = append(evs, g.observe()...)
	}
	if no := only(evs, EventUndelivered); len(no) != 1 || no[0].Role != "fx-repro" {
		t.Fatalf("events %+v, want the resume prompt proven undelivered", evs)
	}
	if _, why, err := g.r.RunState(g.ctx, hung); err != nil || !strings.HasPrefix(why, RecoveryPrefix) {
		t.Fatalf("RunState of the replaced run = %q, %v; want the recovery's reason, which a rebuilt stage reads", why, err)
	}

	g.restart() // before anything acted on the proof
	starts := len(g.h.startCalls())
	if err := g.r.RecoverLost(g.ctx, g.task, g.attempt, g.role("fx-repro")); err != nil {
		t.Fatal(err)
	}
	if s := g.h.startCalls(); len(s) != starts+1 || !strings.Contains(strings.Join(s[len(s)-1].Args, " "), "--resume sid-1") {
		t.Fatalf("starts %+v after the resume prompt never arrived, want the role recovered again", s[starts:])
	}
	if p := g.h.promptTexts(); p[len(p)-1] != resumeText {
		t.Fatalf("the recovered role got %q, want the resume prompt", p[len(p)-1])
	}
}

// A step that cannot quit the hung session lets go of the hung run's
// prompts of unknown delivery (the watchdog's nudge, say): the session is
// to be restarted, so their fate no longer matters, and none of them sends
// the task to you in the middle of the steps.
func TestAStepThatCannotQuitLetsGoOfTheHungRunsUnknownPrompts(t *testing.T) {
	g := newRig(t)
	g.h.promptErr = func(text string) error {
		if text == nudgeText {
			return fmt.Errorf("herdr agent.prompt: connection closed without a response: %w", io.EOF)
		}
		return nil
	}
	g.hungTurn("reproduce the bug")
	g.h.promptErr = nil
	nudge := g.ledger()[1]
	if nudge.Text != nudgeText || nudge.Delivered != registry.DeliveredUnknown {
		t.Fatalf("the nudge's entry %q is %s, want unknown", nudge.Text, nudge.Delivered)
	}
	stepFaults[0].inject(g) // the quit fails
	g.clock.Add(NoProgressAfter)
	g.watchdog()

	if got := g.ledger()[1].Delivered; got != registry.DeliveredAbandoned {
		t.Fatalf("the hung run's nudge is %s after the step, want let go", got)
	}
	g.h.keysErr = nil
	g.clock.Add(UnknownAfter)
	if evs := only(g.observeN(2), EventDeliveryUnknown); len(evs) != 0 {
		t.Fatalf("events %+v: the nudge's fate went to you in the middle of the steps", evs)
	}
}
