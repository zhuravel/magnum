package factory

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/factory/stats"
	"github.com/zhuravel/magnum/internal/herdr"
)

// typedEvent is what an observe pass hands over of role's pane: the turns
// its transcript holds that the factory did not send.
func typedEvent(task registry.Task, role string, turns ...runtime.Turn) runtime.Event {
	return runtime.Event{TaskID: task.ID, Role: role, Kind: runtime.EventTyped, Turns: turns}
}

// paneInputs are the task's pane_typed inputs, in the order recorded.
func (h *harness) paneInputs(id int64) []registry.Input {
	h.t.Helper()
	ins, err := h.reg.Inputs(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []registry.Input
	for _, in := range ins {
		if in.Kind == registry.InputPaneTyped {
			out = append(out, in)
		}
	}
	return out
}

// A turn you typed while herdr showed you in the pane becomes one pane_typed
// input: where (the pane, the stage, the role), the proof of its origin, the
// agent's state on its arrival and its two turns before, the time you typed
// it and its class, stored with it. The pass after hands the same turn again,
// and a second session of the role may hold it too: it is recorded once. No
// stage waits for it.
func TestATurnTheOperatorTypedIsRecordedOnce(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12391", registry.TaskActive, "coding")
	at := h.clock.Now().Add(-time.Minute)
	ev := typedEvent(task, "fx-writer", runtime.Turn{At: at, Text: "open PR", Before: "Shall I open the PR?", Stage: "coding",
		AgentState: string(herdr.StatusIdle), Origin: registry.OriginHerdr})

	h.observeEvents(ev, ev)
	h.observeEvents(ev)

	ins := h.paneInputs(task.ID)
	if len(ins) != 1 {
		t.Fatalf("pane inputs %+v, want one", ins)
	}
	in := ins[0]
	if in.Surface != "pane" || in.Stage != "coding" || in.Role != "fx-writer" || in.Text != "open PR" || in.Origin != registry.OriginHerdr ||
		in.Before != "Shall I open the PR?" || in.AgentState != "idle" || !in.At.Equal(at) || in.DroppedReason != "" {
		t.Fatalf("input %+v, want the turn as typed in fx-writer's pane", in)
	}
	if in.Class != registry.ClassStepStart {
		t.Fatalf("class %q, want step_start stored with the turn", in.Class)
	}
	if evs := h.e.takeEvents(task.ID); len(evs) != 0 {
		t.Fatalf("the typed turns went to the task's stage: %+v", evs)
	}
}

// A turn nothing proves you typed is kept with the origin unknown, for
// evaluation only: the stats count it apart, never as a typed message or a
// step start.
func TestATurnWithoutProofIsKeptAsUnknown(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12392", registry.TaskActive, "coding")
	at := h.clock.Now().Add(-time.Minute)
	h.observeEvents(typedEvent(task, "fx-writer",
		runtime.Turn{At: at, Text: "open PR", Stage: "coding", AgentState: "idle", Origin: registry.OriginUnknown},
		runtime.Turn{At: at.Add(time.Second), Text: "push it", Stage: "coding", AgentState: "idle", Origin: registry.OriginUnknown},
		runtime.Turn{At: at.Add(2 * time.Second), Text: "merge master", Stage: "coding", AgentState: "idle", Origin: registry.OriginHerdr},
	))

	ins := h.paneInputs(task.ID)
	if len(ins) != 3 || ins[0].Origin != registry.OriginUnknown || ins[0].Text != "open PR" || ins[2].Origin != registry.OriginHerdr {
		t.Fatalf("pane inputs %+v, want the three turns, two of unknown origin", ins)
	}
	rep, err := stats.ComputeAt(h.ctx, h.reg, time.Time{}, h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Tasks) != 1 {
		t.Fatalf("stats %+v, want one task", rep.Tasks)
	}
	if got := rep.Tasks[0]; got.Typed != 1 || got.StepStarts != 1 || got.UnknownOrigin != 2 {
		t.Fatalf("stats typed %d, step starts %d, unknown origin %d; want 1, 1 and 2", got.Typed, got.StepStarts, got.UnknownOrigin)
	}
}

// What you typed, and what the agent said before it, is scrubbed before it
// is kept (a token cut by the 1 KiB the agent's turns keep is scrubbed
// whole first); a long pasted block keeps only why it was dropped.
func TestATypedTurnIsScrubbedAndALongPasteDropped(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12393", registry.TaskActive, "coding")
	at := h.clock.Now().Add(-time.Minute)
	token := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	// The agent's last 1 KiB begins 20 bytes into the token.
	before := "Run it with " + token + " " + strings.Repeat("x", 984) + "and then the specs "
	paste := strings.Repeat("app/models/point.rb:12: undefined method\n", 41)
	h.observeEvents(typedEvent(task, "fx-writer",
		runtime.Turn{At: at, Text: "the key is " + token + ", password=hunter2, ask alice@example.com", Before: before, Stage: "coding",
			Origin: registry.OriginHerdr},
		runtime.Turn{At: at.Add(time.Second), Text: paste, Stage: "coding", Origin: registry.OriginHerdr},
	))

	ins := h.paneInputs(task.ID)
	if len(ins) != 2 {
		t.Fatalf("pane inputs %+v, want two", ins)
	}
	typed, pasted := ins[0], ins[1]
	for _, leak := range []string{"ghp_", "hunter2", "alice@example.com"} {
		if strings.Contains(typed.Text, leak) {
			t.Errorf("text %q keeps %q", typed.Text, leak)
		}
	}
	if !strings.Contains(typed.Text, "the key is") || typed.Class == "" {
		t.Errorf("input %+v, want the scrubbed words, classified", typed)
	}
	if len(typed.Before) > 1024 || strings.Contains(typed.Before, "0123456789") || !strings.HasSuffix(typed.Before, "and then the specs ") {
		t.Errorf("before %q (%d bytes), want the agent's last 1 KiB, the token scrubbed before the cut", typed.Before, len(typed.Before))
	}
	if pasted.Text != "" || pasted.DroppedReason != "a long paste: more than 40 lines or 4000 bytes" || pasted.Class != "" {
		t.Errorf("long paste %+v, want dropped with its reason and no class", pasted)
	}
}

// A turn within minutes of an access card (the task's wait on you for an
// access step, as its spans record it) is dropped: it may hold a code or a
// password. Further from the card it is kept.
func TestATurnNearAnAccessCardIsDropped(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12394", registry.TaskActive, "investigate")
	h.clock.Add(time.Hour)
	shown := h.clock.Now()
	wait := func(state, who, what string) {
		t.Helper()
		if err := h.reg.TransitionTask(h.ctx, task.ID, nil, state, func(u *registry.TaskUpdate) {
			u.Set("waits_who", who)
			u.Set("waits_what", what)
		}); err != nil {
			t.Fatal(err)
		}
	}
	wait(registry.TaskNeedsYou, waitsYou, accessWait+": the AWS MFA session ran out")
	h.clock.Add(8 * time.Minute)
	wait(registry.TaskActive, "", "")
	h.clock.Add(30 * time.Minute)
	reopened := h.clock.Now()
	wait(registry.TaskNeedsYou, waitsYou, accessWait+": the VPN is down") // still open
	h.clock.Add(10 * time.Minute)

	turn := func(at time.Time, text string) runtime.Turn {
		return runtime.Turn{At: at, Text: text, Before: "Paste the code here.", Stage: "investigate", Origin: registry.OriginHerdr}
	}
	h.observeEvents(typedEvent(task, "fx-repro",
		turn(shown.Add(-10*time.Minute), "kept: long before the card"),
		turn(shown.Add(-2*time.Minute), "000000"),
		turn(shown.Add(3*time.Minute), "my code is 000000"),
		turn(shown.Add(12*time.Minute), "done, I logged in"),
		turn(shown.Add(20*time.Minute), "kept: long after the card"),
		turn(reopened.Add(9*time.Minute), "the password is in the vault"),
	))

	ins := h.paneInputs(task.ID)
	if len(ins) != 6 {
		t.Fatalf("pane inputs %+v, want six", ins)
	}
	for _, in := range ins {
		kept := strings.HasPrefix(in.Text, "kept:")
		switch {
		case kept && (in.DroppedReason != "" || in.Before == ""):
			t.Errorf("a turn far from the cards was dropped: %+v", in)
		case !kept && (in.Text != "" || in.Before != "" || in.Class != "" || !strings.Contains(in.DroppedReason, "access card")):
			t.Errorf("a turn near an access card was kept, or the agent's turns before it: %+v", in)
		}
	}
}

// A restart forgets nothing the registry holds: the runtime's first read
// after it hands every turn again, those recorded before (a dropped paste
// too) are not recorded twice, and one typed while the daemon was down is
// recorded.
func TestATypedTurnSurvivesARestartWithoutADuplicate(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12395", registry.TaskActive, "coding")
	at := h.clock.Now().Add(-time.Minute)
	kept := runtime.Turn{At: at, Text: "only the refund path", Stage: "coding", Origin: registry.OriginHerdr}
	pasted := runtime.Turn{At: at.Add(time.Second), Text: strings.Repeat("a line of a log\n", 50), Stage: "coding", Origin: registry.OriginHerdr}
	h.observeEvents(typedEvent(task, "fx-writer", kept, pasted))
	if n := len(h.paneInputs(task.ID)); n != 2 {
		t.Fatalf("%d pane inputs before the restart, want 2", n)
	}

	h.restart()
	h.clock.Add(time.Hour)
	down := runtime.Turn{At: h.clock.Now().Add(-30 * time.Minute), Text: "and skip archived points", Stage: "coding",
		Origin: registry.OriginHerdr}
	h.observeEvents(typedEvent(task, "fx-writer", kept, pasted, down))

	ins := h.paneInputs(task.ID)
	if len(ins) != 3 || ins[0].Text != "only the refund path" || ins[1].DroppedReason == "" || ins[2].Text != "and skip archived points" {
		t.Fatalf("pane inputs after the restart %+v, want the two from before once and the one typed while down", ins)
	}
}
