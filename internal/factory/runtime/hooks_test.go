package runtime

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// paneEnvs are the environments of the panes herdr made: each workspace's
// root pane, then each split.
func (f *fakeHerdr) paneEnvs() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]string
	for _, c := range f.creates {
		out = append(out, c.Env)
	}
	for _, s := range f.splits {
		out = append(out, s.Env)
	}
	return out
}

// Once the factory daemon listens on its hook socket (UseHookSocket, before
// its first launch), every pane the runtime makes names the socket and the
// binary its agents' hooks run, beside the copy's env.
func TestEveryPaneNamesTheHookSocketOnceTheDaemonListens(t *testing.T) {
	g := newRig(t)
	socket := "/tmp/fh1/state/run/factory-hooks.sock"
	g.r.UseHookSocket(socket)
	g.launch("fx-repro", "fx-history")

	envs := g.h.paneEnvs()
	if len(envs) != 2 {
		t.Fatalf("%d panes made, want the two roles'", len(envs))
	}
	for i, env := range envs {
		if env[agenthooks.EnvSocket] != socket || env[agenthooks.EnvBin] != g.layout.Binary() {
			t.Errorf("pane %d: %s=%q %s=%q, want the socket %q and the binary %q", i, agenthooks.EnvSocket,
				env[agenthooks.EnvSocket], agenthooks.EnvBin, env[agenthooks.EnvBin], socket, g.layout.Binary())
		}
		if env["RAILS_ENV"] != "development" {
			t.Errorf("pane %d lost the copy's env: %v", i, env)
		}
	}
}

// A daemon without a listener (a path too long, a listener that failed)
// never calls UseHookSocket: no pane names a socket nobody reads.
func TestWithoutAHookSocketNoPaneNamesOne(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro", "fx-history")
	for i, env := range g.h.paneEnvs() {
		if _, ok := env[agenthooks.EnvSocket]; ok {
			t.Errorf("pane %d names a hook socket: %v", i, env)
		}
		if _, ok := env[agenthooks.EnvBin]; ok {
			t.Errorf("pane %d names a hook binary: %v", i, env)
		}
	}
}

// wakeWithin hands ev to Wake and waits for its later looks, and fails the
// test when they do not return within ten seconds: a Wake under a lock its
// callbacks take never would.
func wakeWithin(t *testing.T, g *rig, ev agenthooks.Event) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.r.Wake(g.ctx, ev)
		g.r.WaitWakes()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Wake(%s) did not return: it runs under a lock the runtime's callbacks take", ev.Name)
	}
}

// claudeFailing has the rig's Claude role fx-repro at work on a prompt,
// with herdr's session id sid recorded on its row, and its pane showing
// screen: the state a StopFailure hook of its turn finds. It returns the
// role's pane.
func (g *rig) claudeFailing(sid, screen string) string {
	g.t.Helper()
	g.launch("fx-repro")
	g.send("fx-repro", "reproduce the bug")
	g.h.setSession(g.agent("fx-repro"), sid)
	g.observe() // the row takes herdr's session id
	g.h.screen(g.agent("fx-repro"), screen)
	return store.Deref(g.session("fx-repro").HerdrPaneID)
}

// claudeSID is the Claude session id the StopFailure tests' row records.
const claudeSID = "6a1d5e0c-0000-4000-8000-000000000002"

// stopFailure is the StopFailure event Claude's hook sends for pane with
// error errText.
func stopFailure(pane, errText string) agenthooks.Event {
	return agenthooks.Event{V: agenthooks.Version, Agent: agenthooks.AgentClaude, Name: agenthooks.NameStopFailure, Pane: pane,
		SessionID: claudeSID, Error: errText}
}

// The agents manager runs the runtime's callbacks inside Wake: a look at a
// working Codex agent names it through ownerName, which takes r.mu, so
// Wake itself never takes r.mu, or that look would deadlock; and the loop's
// pass and a hook's look run on two goroutines at once without either
// waiting for the other for good (the manager's observe lock, then r.mu,
// never the other way round).
func TestWakeNeverRunsUnderTheRuntimesLock(t *testing.T) {
	t.Run("a look that names the agent", func(t *testing.T) {
		g := newRig(t)
		g.launch("fx-cause")
		g.send("fx-cause", "find the cause")
		g.restart() // a new manager, whose namer has no title yet: the look names the agent (ownerName)
		pane := store.Deref(g.session("fx-cause").HerdrPaneID)
		before := len(g.h.runCalls())

		wakeWithin(t, g, agenthooks.Event{V: agenthooks.Version, Agent: agenthooks.AgentCodex, Name: agenthooks.NameStop, Pane: pane})

		if !slices.ContainsFunc(g.h.runCalls()[before:], func(c paneRunCall) bool { return strings.HasPrefix(c.Command, "/rename ") }) {
			t.Fatalf("the look named no agent (pane runs %+v): the test reached no callback that takes r.mu", g.h.runCalls()[before:])
		}
	})
	t.Run("a pass and a look on two goroutines", func(t *testing.T) {
		g := newRig(t)
		g.launch("fx-cause", "fx-repro")
		g.send("fx-cause", "find the cause")
		g.send("fx-repro", "reproduce the bug")
		g.restart() // the namer has no titles: the looks and the passes both name the agents (ownerName)
		cause, repro := store.Deref(g.session("fx-cause").HerdrPaneID), store.Deref(g.session("fx-repro").HerdrPaneID)
		done := make(chan struct{})
		go func() {
			defer close(done)
			var wg sync.WaitGroup
			wg.Go(func() {
				for range 40 {
					if _, err := g.r.Observe(g.ctx); err != nil {
						t.Errorf("Observe: %v", err)
						return
					}
				}
			})
			wg.Go(func() {
				for i := range 40 {
					ev := agenthooks.Event{V: agenthooks.Version, Agent: agenthooks.AgentCodex, Name: agenthooks.NameStop, Pane: cause}
					if i%2 == 1 {
						ev.Agent, ev.Pane = agenthooks.AgentClaude, repro
					}
					g.r.Wake(g.ctx, ev)
				}
			})
			wg.Wait()
			g.r.WaitWakes()
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the passes and the looks did not end: a lock order between them deadlocks")
		}
	})
}

// A StopFailure decides no pause on its own: it asks the loop for an
// observe pass now (ObserveSoon), without blocking, and the pass applies its
// own rule to the screen after the run's prompt. Whatever the class and the
// screen, the hook pauses no kind; a second request while one is pending is
// absorbed, and the hook still returns at once.
func TestAStopFailureAsksTheLoopForAPassAndDecidesNoPause(t *testing.T) {
	limit := "Reading spec/models/user_spec.rb\nYou've hit your limit · resets 4pm (UTC)\n"
	for _, tc := range []struct{ name, errText, screen string }{
		{"rate_limit on a usage limit", string(agents.StopFailureRateLimit), limit},
		{"authentication_failed on a login", string(agents.StopFailureAuthentication), "API Error: 401 · Please run /login\n"},
		{"overloaded on an overload", string(agents.StopFailureOverloaded),
			`  ⎿  API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			pane := g.claudeFailing(claudeSID, tc.screen)

			wakeWithin(t, g, stopFailure(pane, tc.errText))

			if w := g.r.paused(agents.KindClaude); w != nil {
				t.Fatalf("the StopFailure hook paused claude (%v): the pass decides", w)
			}
			select {
			case <-g.r.ObserveSoon():
			default:
				t.Fatal("the StopFailure asked the loop for no observe pass")
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				s := g.session("fx-repro")
				g.r.hookFailure(s, agents.StopFailureRateLimit, agents.Health{Kind: agents.HealthUsageLimit})
				g.r.hookFailure(s, agents.StopFailureRateLimit, agents.Health{Kind: agents.HealthUsageLimit}) // one pending: absorbed
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("a StopFailure waited on the loop")
			}
			if log := g.logs.String(); strings.Contains(log, "spec/models") || strings.Contains(log, "API Error") {
				t.Fatalf("the log carries the screen's text:\n%s", log)
			}
		})
	}
}

// The pass that ends the run a StopFailure ended pauses the kind as it does
// without hooks, from the screen after the run's prompt.
func TestTheRunAStopFailureEndedStillEndsOnThePass(t *testing.T) {
	g := newRig(t)
	pane := g.claudeFailing(claudeSID, "Reading spec/models/user_spec.rb\nYou've hit your limit · resets 4pm (UTC)\n")
	wakeWithin(t, g, stopFailure(pane, string(agents.StopFailureRateLimit)))

	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	paused := only(g.observeN(2), EventPaused)
	if len(paused) != 1 || paused[0].Reason != "usage: claude limit until 16:00" {
		t.Fatalf("the pass after the hook gave %+v, want the run's pause", paused)
	}
	if rr := g.roleRun("fx-repro"); rr.Outcome != "paused" {
		t.Fatalf("the role run's outcome is %q, want paused", rr.Outcome)
	}
}

// The review's probe: a usage limit an earlier turn hit is still on screen
// above the run's prompt (the stage told the session to go on once it
// lifted), and the new turn fails on an error no pattern knows. The screen
// read whole shows a limit, but the pass reads it after the prompt and sees
// none: the run ends, no kind pauses, and the role's next prompt goes out.
func TestAStopFailureOnAStaleLimitLineEndsTheRunWithNoPause(t *testing.T) {
	g := newRig(t)
	screen := "You've hit your limit · resets 4pm (UTC)\n> reproduce the bug\n  ⎿  API Error: 400 " +
		`{"type":"error","error":{"type":"invalid_request_error","message":"messages.3: unexpected tool_use_id"}}` + "\n"
	pane := g.claudeFailing(claudeSID, screen)

	wakeWithin(t, g, stopFailure(pane, "invalid_request"))
	if w := g.r.paused(agents.KindClaude); w != nil {
		t.Fatalf("after the hook claude is paused: %v", w)
	}

	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	evs := g.observeN(2)
	if p := only(evs, EventPaused); len(p) != 0 {
		t.Fatalf("the pass paused on the stale line: %+v", p)
	}
	if e := only(evs, EventEnded); len(e) != 1 {
		t.Fatalf("the pass gave %+v, want the run's end", evs)
	}
	if w := g.r.paused(agents.KindClaude); w != nil {
		t.Fatalf("after the pass claude is paused: %v", w)
	}
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "go on"); err != nil {
		t.Fatalf("the role's next prompt = %v, want it sent", err)
	}
}

// Ended is the agents manager's: it closes when a pass (or a hook's look)
// ends a run, so the loop can take a turn's end that a look after the
// event's own found at once.
func TestEndedClosesWhenARunEnds(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.send("fx-repro", "reproduce the bug")
	g.observe()
	ended := g.r.Ended()
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	if e := only(g.observeN(2), EventEnded); len(e) != 1 {
		t.Fatalf("no run ended: %+v", e)
	}
	select {
	case <-ended:
	default:
		t.Fatal("Ended did not close when the run ended")
	}
}
