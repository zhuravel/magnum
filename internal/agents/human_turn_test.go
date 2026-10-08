package agents

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// A claude agent at work with no run in flight is someone typing only when
// its transcript shows a prompt typed into the pane after magnum's last
// prompt to it: magnum's own turn, cut short by esc while its subagents work
// on, is nobody's.

const (
	reviewPrompt   = "/code-review https://github.com/talkable/talkable/pull/11920 medium"
	reviewPromptID = "c0ffee00-0000-4000-8000-000000000001"
)

// interruptedReview prompts claude-review, which launches two subagents in
// the background and waits on them; then a restart cuts its run short: esc
// ends the turn (Claude Code's interrupt line), the pipeline counts the
// work the agent left running and, with stop, tells the agent within the
// run to stop it (Tell), and the run is abandoned, as the pipeline's cut does, while
// herdr still shows the agent working on its subagents (stopping them did
// not work). It returns the run id.
func (e *env) interruptedReview(tr *transcript, stop bool) string {
	e.t.Helper()
	id := e.promptClaude()
	now := e.clock.Now()
	p := humanPrompt(now, reviewPrompt)
	p["promptId"] = reviewPromptID
	tr.add(p)
	tr.addAll(agentAsync(now.Add(2*time.Minute), "toolu_agent1", "a1f00d"), agentAsync(now.Add(2*time.Minute+10*time.Second), "toolu_agent2", "a2f00d"))
	tr.add(toolUse(now.Add(3*time.Minute), "toolu_wait1", "Bash", map[string]any{"command": "sleep 240", "description": "Wait for the subagents"}))
	e.clock.Add(6 * time.Minute)
	if o := e.observe()[RoleClaude]; o.Kind != "" || o.Run == nil || o.Run.ID != id {
		e.t.Fatalf("working on its run = %+v", o)
	}
	tr.add(interruption(e.clock.Now(), reviewPromptID))
	e.clock.Add(time.Minute) // InterruptWait: the agent still shows working
	if n, ok := e.m.BackgroundTasks(e.ctx, e.run1(id)); !ok || n != 2 {
		e.t.Fatalf("BackgroundTasks after the interrupt = %d, %v; want the two subagents", n, ok)
	}
	if stop {
		const text = "The PR head moved, so stop here. Stop every background task you started with TaskStop and do nothing else."
		if err := e.m.Tell(e.ctx, e.run1(id), text); err != nil {
			e.t.Fatal(err)
		}
		at := e.clock.Now()
		tr.add(humanPrompt(at, text),
			toolUse(at.Add(2*time.Second), "toolu_stop1", "TaskStop", map[string]any{"task_id": "a1f00d"}),
			toolResult(at.Add(3*time.Second), "toolu_stop1", "Error: no such task", nil, true),
			assistantSays(at.Add(4*time.Second), "Could not stop the subagents."), turnEnded(at.Add(4*time.Second)))
		e.clock.Add(2 * time.Minute) // StopGrace
	}
	e.abandon(id)
	return id
}

// abandon ends run id as the pipeline's cut does.
func (e *env) abandon(id string) {
	e.t.Helper()
	if err := e.st.TransitionRun(e.ctx, id, []string{store.RunSubmitted, store.RunWorking}, store.RunAbandoned, func(u *store.RunUpdate) {
		u.Set("ended_at", e.clock.Now())
	}); err != nil {
		e.t.Fatal(err)
	}
}

// restartDaemon replaces the manager with a fresh one on the same registry,
// as a daemon restart does: no transcript watch survives.
func (e *env) restartDaemon() {
	e.t.Helper()
	sleep := e.m.sleep
	e.m = New(e.m.d)
	e.m.sleep = sleep
}

// workingTicks runs n ticks 30 s apart with the claude agent working and
// fails on any observation.
func (e *env) workingTicks(n int, what string) {
	e.t.Helper()
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	for tick := 1; tick <= n; tick++ {
		e.clock.Add(30 * time.Second)
		if o := e.observe()[RoleClaude]; o.Kind != "" {
			e.t.Fatalf("tick %d %s = %q, want nothing", tick, what, o.Kind)
		}
	}
	if pr := e.reloadPR(); pr.HumanActiveAt != nil {
		e.t.Fatalf("human_active_at = %v after %s, want none", pr.HumanActiveAt, what)
	}
}

// The live case (round 3 of a PR on 2026-10-08): the restart's esc ended
// the turn magnum's prompt began, and Claude Code wrote its interrupt line,
// which carries that prompt's promptId and no origin; the two subagents
// worked on for minutes with herdr showing the agent working and no run in
// flight. The interrupt line begins no turn: the turn is magnum's, nobody
// typed, and no cooldown holds the restarted round's prompts. The message
// that now tells a cut reviewer to stop its background work begins a turn
// that is magnum's too.
func TestAnInterruptedTurnWhoseSubagentsWorkOnIsNoHuman(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop bool
	}{{"esc alone", false}, {"esc and the message to stop the background work", true}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			e.interruptedReview(tr, tc.stop)
			e.workingTicks(20, "while the subagents work")
		})
	}
}

// A person who types into the pane after magnum's last prompt is a human at
// once, while the subagents of the cut turn still work.
func TestAPromptTypedAfterAnInterruptedTurnIsAHuman(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	e.interruptedReview(tr, false)
	e.workingTicks(4, "while the subagents work")
	tr.add(humanPrompt(e.clock.Now(), "What are the subagents doing?"))
	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleClaude]; o.Kind != ObsHumanActive {
		t.Fatalf("after a typed prompt = %q, want %q", o.Kind, ObsHumanActive)
	}
	if pr := e.reloadPR(); pr.HumanActiveAt == nil || !pr.HumanActiveAt.Equal(e.clock.Now()) {
		t.Fatalf("human_active_at = %v", pr.HumanActiveAt)
	}
}

// After a daemon restart no transcript watch is left: the run-less watch
// scans back for the last turn start, passes over the interrupt line and
// finds magnum's prompt, so the agent's work is still nobody's.
func TestAfterARestartTheInterruptLineIsNoTurnStart(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	e.interruptedReview(tr, false)
	e.restartDaemon()
	e.workingTicks(6, "after the restart")
}

// Magnum's prompt to an agent at work waits in Claude Code's queue: its
// enqueue is stamped when magnum typed it, before last_prompt_at, and its
// user entry when the agent took it, minutes later. The turn is magnum's
// whatever the entry's stamp, also for a watch that starts after a restart.
func TestAPromptMagnumQueuedIsNoHumanWhenTheAgentTakesItLater(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "the watch reads on"
		if restart {
			name = "after a daemon restart"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			e.started()
			e.h.setAgentSession(claudeAgent, claudeSID)
			e.clock.Add(10 * time.Minute)
			tr.addAll(notification(e.clock.Now(), "toolu_bash1", "bg1", "completed")) // a turn nobody typed
			e.workingTicks(1, "on a notification's turn")

			e.clock.Add(time.Minute)
			tr.add(enqueued(e.clock.Now().Add(-time.Second), reviewPrompt))
			id, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunInitial, reviewPrompt)
			if err != nil {
				t.Fatal(err)
			}
			tr.add(toolUse(e.clock.Now().Add(time.Minute), "toolu_cat1", "Bash", map[string]any{"command": "cat /tmp/claude/tasks/bg1.output"}))
			e.clock.Add(4 * time.Minute)
			tr.addAll(taken(e.clock.Now(), reviewPrompt))
			tr.add(toolUse(e.clock.Now().Add(time.Second), "toolu_diff1", "Bash", map[string]any{"command": "git diff"}))
			if o := e.observe()[RoleClaude]; o.Kind != "" || o.Run == nil || o.Run.ID != id {
				t.Fatalf("working on its run = %+v", o)
			}
			e.clock.Add(time.Minute)
			e.abandon(id) // cut short; the agent works on
			if restart {
				e.restartDaemon()
			}
			e.workingTicks(6, "on magnum's queued prompt")
		})
	}
}

// A person who types into the pane while the agent works queues the prompt;
// the enqueue is stamped after magnum's last prompt, so once the agent takes
// it, its turn is a human's.
func TestAPromptAPersonQueuedIsAHuman(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	e.started()
	e.h.setAgentSession(claudeAgent, claudeSID)
	e.clock.Add(10 * time.Minute)
	tr.addAll(notification(e.clock.Now(), "toolu_bash1", "bg1", "completed"))
	e.workingTicks(1, "on a notification's turn")

	e.clock.Add(time.Minute)
	const typed = "Stop the spec run and summarize."
	tr.add(enqueued(e.clock.Now(), typed))
	e.workingTicks(2, "with the prompt still queued")
	tr.addAll(taken(e.clock.Now(), typed))
	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleClaude]; o.Kind != ObsHumanActive {
		t.Fatalf("after the agent took the queued prompt = %q, want %q", o.Kind, ObsHumanActive)
	}
}

// Claude Code mostly takes a prompt typed into a busy pane into the turn it
// is in (a queued command stamped when it was typed) instead of starting a
// turn with it. A person's prompt taken into a turn nobody typed (a task
// notification's) makes that turn a human's; magnum's own, typed with its
// last prompt, does not.
func TestAPromptTakenIntoTheTurnCountsByWhenItWasTyped(t *testing.T) {
	t.Run("a person's", func(t *testing.T) {
		e := newEnv(t)
		tr := e.claudeTranscript()
		e.started()
		e.h.setAgentSession(claudeAgent, claudeSID)
		e.clock.Add(10 * time.Minute)
		tr.addAll(notification(e.clock.Now(), "toolu_bash1", "bg1", "completed"))
		e.workingTicks(1, "on a notification's turn")
		const typed = "Also check the migrations."
		at := e.clock.Now()
		tr.add(enqueued(at, typed))
		tr.addAll(absorbed(at, at.Add(20*time.Second), typed))
		e.clock.Add(30 * time.Second)
		if o := e.observe()[RoleClaude]; o.Kind != ObsHumanActive {
			t.Fatalf("after the agent took a typed prompt into its turn = %q, want %q", o.Kind, ObsHumanActive)
		}
	})
	t.Run("magnum's", func(t *testing.T) {
		e := newEnv(t)
		tr := e.claudeTranscript()
		id := e.promptClaude()
		now := e.clock.Now()
		tr.add(humanPrompt(now, reviewPrompt))
		tr.add(toolUse(now.Add(time.Minute), "toolu_specs1", "Bash", map[string]any{"command": "bin/rspec spec/models"}))
		e.clock.Add(40 * time.Minute)
		const stop = "Time is up: write the report now."
		if err := e.m.Tell(e.ctx, e.run1(id), stop); err != nil {
			t.Fatal(err)
		}
		at := e.clock.Now()
		tr.add(enqueued(at, stop))
		tr.addAll(absorbed(at, at.Add(30*time.Second), stop))
		e.clock.Add(5 * time.Minute)
		e.abandon(id)
		e.workingTicks(6, "on magnum's turn")
	})
}

// The tick that sets human_active_at on a PR with no cooldown running says
// when that cooldown ends; a tick that only extends it says nothing, and the
// first after it ran out starts a new one.
func TestObserveSaysWhenAHumanCooldownStarts(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.clock.Add(10 * time.Minute)
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking) // no transcript: the old rule, a human
	cd := e.cfg.Daemon.HumanCooldown.Duration
	o := e.observe()[RoleClaude]
	if o.Kind != ObsHumanActive || !o.CooldownUntil.Equal(e.clock.Now().Add(cd)) {
		t.Fatalf("first tick = kind %q cooldown until %v, want %v", o.Kind, o.CooldownUntil, e.clock.Now().Add(cd))
	}
	for tick := 1; tick <= 3; tick++ {
		e.clock.Add(30 * time.Second)
		if o := e.observe()[RoleClaude]; o.Kind != ObsHumanActive || !o.CooldownUntil.IsZero() {
			t.Fatalf("tick %d within the cooldown = kind %q cooldown until %v, want it only extended", tick, o.Kind, o.CooldownUntil)
		}
	}
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.clock.Add(cd)
	e.observe()
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleClaude]; o.Kind != ObsHumanActive || !o.CooldownUntil.Equal(e.clock.Now().Add(cd)) {
		t.Fatalf("after the cooldown ran out = kind %q cooldown until %v, want a new one", o.Kind, o.CooldownUntil)
	}
}
