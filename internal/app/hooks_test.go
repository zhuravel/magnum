package app

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
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

// The App's agents manager hands the StopFailures its agents' hooks prove to
// the engine's queue (HookFailures, through agents.Deps.HookFailure): a
// StopFailure of a review session whose screen shows a usage limit, given
// to Wake, comes out of the queue as its class and the screen's verdict.
func TestTheAppsAgentsHandTheirHookFailuresToTheEnginesQueue(t *testing.T) {
	srv := newHerdrServer(t, herdr.Snapshot{})
	// The look's snapshot fails, so the look leaves the session to the
	// observe pass and only the StopFailure's read of the screen counts.
	srv.answerWith("⏺ Reading the diff.\n\n  ⎿  You've hit your session limit · resets 3pm (Europe/Kyiv)\n\n> \n", "session.snapshot")
	a, _ := testAppWith(t, herdrConfig(srv.socket, ""), Options{HookSocket: "/state/run/hooks.sock"})
	ctx := t.Context()
	repo, err := a.Store.UpsertRepo(ctx, store.Repo{NodeID: "R_1", Owner: "talkable", Name: "talkable", WatchOwner: "talkable", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	up, err := a.Store.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "PR_1", Number: 1, URL: "https://github.com/talkable/talkable/pull/1",
		HeadSHA: "a1", GHState: store.GHOpen, InitialState: store.PRReviewing, Identity: "talkable-app"})
	if err != nil {
		t.Fatal(err)
	}
	const sid = "0b5c1a52-3d2e-4f43-9b7c-4f6a3e2d1c00"
	s, err := a.Store.CreateSession(ctx, store.Session{PRID: up.PR.ID, Role: config.RoleClaudeReview, AgentName: new("mg-1-claude-review-abc123"),
		AgentKind: new(agents.KindClaude), SessionID: new(sid), HerdrWorkspaceID: new("w1"), HerdrPaneID: new("w1:p1"),
		State: store.SessionLive, StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	a.Agents.Wake(ctx, agenthooks.Event{Agent: agenthooks.AgentClaude, Name: agenthooks.NameStopFailure, Pane: "w1:p1", SessionID: sid,
		Error: string(agents.StopFailureRateLimit)})
	a.Agents.WaitWakes()
	select {
	case f := <-a.HookFailures:
		if f.Session.ID != s.ID || f.Failure != agents.StopFailureRateLimit || f.Screen.Kind != agents.HealthUsageLimit {
			t.Fatalf("queued %+v, want session %d's rate limit with the screen's usage limit", f, s.ID)
		}
	default:
		t.Fatal("the review session's StopFailure never reached the engine's queue")
	}
}
