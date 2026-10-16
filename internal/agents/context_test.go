package agents

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// claudeScreenWith is a claude pane's visible screen whose last lines are
// the composer, the status line and the footer.
func claudeScreenWith(status, footer string) string {
	return "⏺ Fixed in a1b2c3d.\n\n╭──────────────╮\n│ >            │\n╰──────────────╯\n  " + status + "\n  " + footer + "\n"
}

// The claude kind's context_pattern reads both forms from the pane's last
// status lines: the operator's status line "418k/1m (41%)" as 41% used,
// Claude Code's "Context left until auto-compact: 8%" as 92% used, the
// newest match when the screen shows both, and nothing from a screen
// without a figure, from a figure above the status lines, from a kind
// without a pattern or from a pane herdr cannot read.
func TestContextUsedReadsBothForms(t *testing.T) {
	e := newEnv(t)
	e.started()
	s := e.session(RoleClaude)
	for _, tc := range []struct {
		name, screen string
		want         int
		ok           bool
	}{
		{"the status line", claudeScreenWith("Fable 5.1 | pr-23@HEAD | 418k/1m (41%) | effort: xhigh | 5h 20% @16:40", "? for shortcuts"), 41, true},
		{"claude's own warning", claudeScreenWith("Fable 5.1 | pr-23@HEAD", "⏵⏵ bypass permissions on         Context left until auto-compact: 8%"), 92, true},
		{"both: the newest", claudeScreenWith("Fable 5.1 | 940k/1m (94%)", "Context left until auto-compact: 3%"), 97, true},
		{"no figure", claudeScreenWith("Fable 5.1 | pr-23@HEAD | effort: xhigh", "? for shortcuts"), 0, false},
		{"a figure above the status lines", "⏺ Coverage (85%) is fine.\n\n" + claudeScreenWith("Fable 5.1 | pr-23@HEAD", "? for shortcuts"), 0, false},
		{"no percent above 100", claudeScreenWith("Fable 5.1 | (418%)", "? for shortcuts"), 0, false},
		{"an empty screen", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.h.mu.Lock()
			e.h.reads[claudeAgent] = tc.screen
			e.h.mu.Unlock()
			got, ok := e.m.ContextUsed(e.ctx, s)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("ContextUsed = %d, %v; want %d, %v", got, ok, tc.want, tc.ok)
			}
		})
	}

	e.h.mu.Lock()
	e.h.reads[claudeAgent] = claudeScreenWith("Fable 5.1 | 418k/1m (41%)", "? for shortcuts")
	e.h.errs["AgentRead"] = &herdr.Error{Method: "agent.read", Code: herdr.CodeAgentNotFound, Message: claudeAgent}
	e.h.errs["PaneRead"] = errors.New("herdr is down")
	e.h.mu.Unlock()
	if got, ok := e.m.ContextUsed(e.ctx, s); ok {
		t.Errorf("an unreadable pane reads %d%%", got)
	}
	e.h.mu.Lock()
	delete(e.h.errs, "AgentRead")
	delete(e.h.errs, "PaneRead")
	e.h.mu.Unlock()
	e.setKind(KindClaude, func(k *config.Kind) { k.ContextPattern = "" })
	if got, ok := e.m.ContextUsed(e.ctx, s); ok {
		t.Errorf("a kind without context_pattern reads %d%%", got)
	}
	e.setKind(KindClaude, func(k *config.Kind) { k.ContextPattern = `(?P<left>\d+)% left` })
	e.h.mu.Lock()
	e.h.reads[claudeAgent] = claudeScreenWith("Fable 5.1 | 30% left", "? for shortcuts")
	e.h.mu.Unlock()
	if got, ok := e.m.ContextUsed(e.ctx, s); got != 70 || !ok {
		t.Errorf("a pattern of the kind's own: ContextUsed = %d, %v; want 70", got, ok)
	}
}

// compactingScreen makes the claude-review agent work on /compact for polls
// sleeps after the command, then go idle with the status line showing after%,
// and observe runs at each of those sleeps.
func (e *env) compactingScreen(polls, after int, observe func()) {
	e.h.mu.Lock()
	e.h.reads[claudeAgent] = claudeScreenWith("Fable 5.1 | 820k/1m (82%)", "? for shortcuts")
	e.h.onRun = func(f *fakeHerdr, _, cmd string) {
		if cmd == CompactCommand {
			setStatusLocked(f, claudeAgent, herdr.StatusWorking)
		}
	}
	e.h.mu.Unlock()
	n := 0
	e.m.sleep = func(context.Context, time.Duration) error {
		n++
		e.clock.Add(compactPoll)
		if observe != nil {
			observe()
		}
		if n == polls {
			e.h.mu.Lock()
			setStatusLocked(e.h, claudeAgent, herdr.StatusIdle)
			e.h.reads[claudeAgent] = claudeScreenWith(fmt.Sprintf("Fable 5.1 | %dk/1m (%d%%)", after*10, after), "? for shortcuts")
			e.h.mu.Unlock()
		}
		return nil
	}
}

// Compact types /compact into the idle agent's pane, as /model is typed, and
// returns once the compaction turn ended: the agent seen working, then idle
// on two polls. The turn is magnum's: the observer never takes it for a
// human's, however long it runs, and the session's last_prompt_at says when
// the command went in.
func TestCompactTypesTheCommandAndWaitsForTheTurnToEnd(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.clock.Add(10 * time.Minute) // past the start's grace: a working agent with no run reads as a human
	s := e.session(RoleClaude)
	e.compactingScreen(90, 31, func() { // three minutes at work
		if o := e.observe()[RoleClaude]; o.Kind == ObsHumanActive {
			t.Errorf("the observer took the compaction for a human at %s", e.clock.Now())
		}
	})
	sent := e.clock.Now()
	if err := e.m.Compact(e.ctx, s); err != nil {
		t.Fatal(err)
	}
	compacts := slices.DeleteFunc(slices.Clone(e.h.runs), func(r paneRunCall) bool { return r.Command != "/compact" })
	if len(compacts) != 1 || compacts[0].Pane != store.Deref(s.HerdrPaneID) {
		t.Fatalf("pane runs = %+v, want one /compact in %s", e.h.runs, store.Deref(s.HerdrPaneID))
	}
	if got, ok := e.m.ContextUsed(e.ctx, s); got != 31 || !ok {
		t.Errorf("after the compaction: %d%%, %v", got, ok)
	}
	cur := e.session(RoleClaude)
	if cur.LastPromptAt == nil || !cur.LastPromptAt.Equal(sent) {
		t.Errorf("last_prompt_at = %v, want %v", cur.LastPromptAt, sent)
	}
	if pr := e.reloadPR(); pr.HumanActiveAt != nil {
		t.Errorf("human_active_at = %v", pr.HumanActiveAt)
	}
	if e.m.isCompacting(s.ID) {
		t.Error("the session is still marked compacting")
	}
}

// A compaction herdr never sees at work (it ended between two polls, or did
// not start) ends once the agent stayed idle a few polls; an agent at work
// is refused before anything is typed; one that never goes idle again times
// out.
func TestCompactWaitsQuietlyAndRefusesABusyAgent(t *testing.T) {
	e := newEnv(t)
	e.started()
	s := e.session(RoleClaude)
	polls := 0
	e.m.sleep = func(context.Context, time.Duration) error { polls++; return nil }
	if err := e.m.Compact(e.ctx, s); err != nil {
		t.Fatal(err)
	}
	if polls != compactQuietPolls {
		t.Errorf("an agent never seen working: %d polls, want %d", polls, compactQuietPolls)
	}

	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	e.h.mu.Lock()
	e.h.runs = nil
	e.h.mu.Unlock()
	if err := e.m.Compact(e.ctx, s); !errors.Is(err, ErrBusy) {
		t.Fatalf("a working agent: %v, want ErrBusy", err)
	}
	if len(e.h.runs) != 0 {
		t.Fatalf("typed into a working agent: %+v", e.h.runs)
	}

	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.h.mu.Lock()
	e.h.onRun = func(f *fakeHerdr, _, cmd string) { setStatusLocked(f, claudeAgent, herdr.StatusWorking) }
	e.h.mu.Unlock()
	polls = 0
	if err := e.m.Compact(e.ctx, s); !errors.Is(err, ErrTimeout) {
		t.Fatalf("a compaction that never ends: %v, want ErrTimeout", err)
	}
	if want := int(CompactTimeout / compactPoll); polls != want {
		t.Errorf("timed out after %d polls, want %d", polls, want)
	}
}
