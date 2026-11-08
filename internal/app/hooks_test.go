package app

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// The daemon's agents manager gives its panes the hook socket and the
// binary only when the daemon command passes the socket of a listener it
// started (Options.HookSocket); without one, no pane gets either, so its
// agents' hooks send nothing and the daemon polls.
func TestTheAppsPanesGetTheHookEnvOnlyWithItsListenersSocket(t *testing.T) {
	const socket = "/state/run/hooks.sock"
	for _, tc := range []struct {
		name, socket string
	}{{"listening", socket}, {"no listener", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			a, layout := testApp(t, Options{HookSocket: tc.socket})
			if a.HookSocket != tc.socket {
				t.Fatalf("App.HookSocket = %q, want %q", a.HookSocket, tc.socket)
			}
			env, err := a.Agents.PaneEnv(nil, a.Config.Roles[0])
			if err != nil {
				t.Fatal(err)
			}
			got, has := env[agenthooks.EnvSocket]
			bin, hasBin := env[agenthooks.EnvBin]
			switch {
			case tc.socket != "" && (got != tc.socket || bin != layout.Binary()):
				t.Fatalf("pane env socket %q, bin %q; want %q and %q", got, bin, tc.socket, layout.Binary())
			case tc.socket == "" && (has || hasBin):
				t.Fatalf("pane env without a listener: %v", env)
			}
		})
	}
}

// A StopFailure the agents' hooks proved goes to the engine through a
// bounded queue (HookFailures) and never waits for it: with the engine's
// consumer blocked (nobody reads), the sender, which runs on the hook
// listener's goroutine or on Wake's own, returns at once, and the failures
// past the queue's room are dropped (the run's own end reads its screen
// again). Without a listener there is no queue, and a send does nothing.
func TestAHookFailureNeverWaitsForABlockedConsumer(t *testing.T) {
	a, _ := testApp(t, Options{HookSocket: "/state/run/hooks.sock"})
	if cap(a.HookFailures) != hookFailureQueue {
		t.Fatalf("the queue holds %d, want %d", cap(a.HookFailures), hookFailureQueue)
	}
	s := store.Session{ID: 7, Role: "claude-review", AgentKind: new(agents.KindClaude)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range hookFailureQueue + 3 {
			a.SendHookFailure(s, agents.StopFailureRateLimit, agents.Health{Kind: agents.HealthUsageLimit})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SendHookFailure waited for a consumer that reads nothing")
	}
	if n := len(a.HookFailures); n != hookFailureQueue {
		t.Fatalf("%d failures queued, want the queue's %d", n, hookFailureQueue)
	}
	f := <-a.HookFailures
	if f.Session.ID != 7 || f.Failure != agents.StopFailureRateLimit || f.Screen.Kind != agents.HealthUsageLimit {
		t.Fatalf("queued %+v", f)
	}

	none, _ := testApp(t, Options{})
	if none.HookFailures != nil {
		t.Fatal("a queue without a listener")
	}
	none.SendHookFailure(s, agents.StopFailureRateLimit, agents.Health{Kind: agents.HealthUsageLimit})
}
