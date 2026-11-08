package runtime

import (
	"slices"
	"strings"
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
// working Codex agent names it through ownerName, and a StopFailure its
// screen shows pauses the kind through hookFailure, both under r.mu. Wake
// itself never takes r.mu, or each of these would deadlock.
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
	t.Run("a StopFailure that pauses", func(t *testing.T) {
		g := newRig(t)
		pane := g.claudeFailing(claudeSID, "Reading spec/models/user_spec.rb\nYou've hit your limit · resets 4pm (UTC)\n")

		wakeWithin(t, g, stopFailure(pane, string(agents.StopFailureRateLimit)))

		if g.r.paused(agents.KindClaude) == nil {
			t.Fatal("the StopFailure paused nothing: the test reached no callback that takes r.mu")
		}
	})
}

// A StopFailure of a Claude turn whose screen, read again, shows it pauses
// the kind at once, as the pass that ends the turn would on that screen: a
// usage limit until its reset, a lost login until a login, an overload for
// its backoff, whatever the class the hook named (the screen decides, as on
// the pass). A model's own limit is the pass's (it moves the session to a
// fallback model), and a screen that shows no such failure pauses nothing.
// Each Wake returns at once: the listener never waits on the loop.
func TestAStopFailureHookPausesTheKindAsItsScreenSays(t *testing.T) {
	limit := "Reading spec/models/user_spec.rb\nYou've hit your limit · resets 4pm (UTC)\n"
	for _, tc := range []struct {
		name, errText, screen, want string
	}{
		{"rate_limit on a usage limit", string(agents.StopFailureRateLimit), limit, "usage: claude limit until 16:00"},
		{"billing_error on a usage limit", string(agents.StopFailureBilling), limit, "usage: claude limit until 16:00"},
		{"another class on a usage limit", "server_error", limit, "usage: claude limit until 16:00"},
		{"authentication_failed on a login", string(agents.StopFailureAuthentication), "API Error: 401 · Please run /login\n",
			"login: claude is logged out"},
		{"overloaded on an overload", string(agents.StopFailureOverloaded),
			`  ⎿  API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n",
			"overload: claude is overloaded"},
		{"rate_limit on a model's own limit", string(agents.StopFailureRateLimit), "You've hit your Opus limit · resets 3:45pm\n", ""},
		{"a screen that shows nothing", string(agents.StopFailureOverloaded), "Reading spec/models/user_spec.rb\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			pane := g.claudeFailing(claudeSID, tc.screen)

			wakeWithin(t, g, stopFailure(pane, tc.errText))

			w := g.r.paused(agents.KindClaude)
			switch {
			case tc.want == "" && w != nil:
				t.Fatalf("the StopFailure paused claude (%v); the pass decides on this screen", w)
			case tc.want != "" && (w == nil || !strings.HasPrefix(w.Error(), tc.want)):
				t.Fatalf("after the StopFailure claude's pause is %v, want %q", w, tc.want)
			}
			if tc.want == "" {
				return
			}
			if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "go on"); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("a prompt to claude after the StopFailure = %v, want the wait %q", err, tc.want)
			}
			if log := g.logs.String(); strings.Contains(log, "spec/models") || strings.Contains(log, "API Error") {
				t.Fatalf("the log carries the screen's text:\n%s", log)
			}
		})
	}
}

// The pause the hook set is the pass's own: the run the StopFailure ended
// still ends on the pass, which pauses it as before (EventPaused, the run
// abandoned as paused), and the hook's pause does not keep the session from
// being observed.
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

// A liaison talks with you about a task and is no role of its attempts: as
// on the pass, what its screen shows pauses no kind.
func TestALiaisonsStopFailurePausesNothing(t *testing.T) {
	g := newRig(t)
	reset := t0.Add(7 * time.Hour)
	g.r.hookFailure(store.Session{ID: 1, Role: LiaisonRole, AgentKind: new(agents.KindClaude)}, agents.StopFailureRateLimit,
		agents.Health{Kind: agents.HealthUsageLimit, ResetAt: &reset})
	if w := g.r.paused(agents.KindClaude); w != nil {
		t.Fatalf("a liaison's StopFailure paused claude: %v", w)
	}
}
