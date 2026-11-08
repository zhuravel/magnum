package agents

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	hookSocket = "/Users/x/.local/state/magnum/run/hooks.sock"
	hookBin    = "/Users/x/Projects/magnum/bin/magnum"
)

// hookEnvOf is the hook part of a pane env.
func hookEnvOf(env map[string]string) [2]string {
	return [2]string{env[agenthooks.EnvSocket], env[agenthooks.EnvBin]}
}

// A daemon that listens for hooks names its socket and binary in the env of
// every pane it makes, so the agents' hooks there reach it; the rows record
// the env the panes were made with.
func TestEveryPaneGetsTheHookSocketAndBinaryWhileTheDaemonListens(t *testing.T) {
	e := newEnv(t)
	e.m.d.HookSocket, e.m.d.HookBin = hookSocket, hookBin
	ws := e.workspace()
	if _, err := e.m.EnsurePane(e.ctx, e.pr, ws, "/Users/x/Projects/talkable.review1", map[string]string{"MAGNUM_PR_URL": e.pr.URL},
		e.spec(RoleSimplify)); err != nil {
		t.Fatal(err)
	}
	want := [2]string{hookSocket, hookBin}
	e.h.mu.Lock()
	envs := []map[string]string{e.h.creates[0].Env}
	for _, s := range e.h.splits {
		envs = append(envs, s.Opts.Env)
	}
	e.h.mu.Unlock()
	if len(envs) != 4 {
		t.Fatalf("%d panes made, want 4", len(envs))
	}
	for i, env := range envs {
		if got := hookEnvOf(env); got != want {
			t.Errorf("pane %d's hook env = %q, want %q", i, got, want)
		}
		if env["WT_BRANCH"] != "review1" && i < 3 {
			t.Errorf("pane %d lost its own env: %v", i, env)
		}
	}
	for _, s := range e.sessions() {
		if got := hookEnvOf(s.Env); got != want {
			t.Errorf("%s row's hook env = %q, want %q", s.Role, got, want)
		}
	}

	// Without a binary of its own the layout's runs.
	e2 := newEnv(t)
	e2.m.d.HookSocket = hookSocket
	e2.workspace()
	if got, want := hookEnvOf(e2.h.creates[0].Env), [2]string{hookSocket, e2.m.d.Layout.Binary()}; got != want || want[1] == "" {
		t.Fatalf("hook env = %q, want %q", got, want)
	}
}

// Without a socket (no listener, a path too long, a listener that failed)
// no pane gets the hook env, also when the env it is made from carries one
// (a recorded session's), and the caller's env is never changed.
func TestNoPaneGetsTheHookEnvWithoutASocket(t *testing.T) {
	e := newEnv(t)
	e.m.d.HookBin = hookBin
	stale := map[string]string{"WT_BRANCH": "review1", agenthooks.EnvSocket: "/old.sock", agenthooks.EnvBin: "/old/magnum"}
	if _, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/Users/x/Projects/talkable.review1", stale, "talkable#11920", e.coreRoles()); err != nil {
		t.Fatal(err)
	}
	e.h.mu.Lock()
	envs := []map[string]string{e.h.creates[0].Env}
	for _, s := range e.h.splits {
		envs = append(envs, s.Opts.Env)
	}
	e.h.mu.Unlock()
	for i, env := range envs {
		if _, ok := env[agenthooks.EnvSocket]; ok {
			t.Errorf("pane %d got %s: %v", i, agenthooks.EnvSocket, env)
		}
		if _, ok := env[agenthooks.EnvBin]; ok {
			t.Errorf("pane %d got %s: %v", i, agenthooks.EnvBin, env)
		}
		if env["WT_BRANCH"] != "review1" {
			t.Errorf("pane %d lost its own env: %v", i, env)
		}
	}
	if stale[agenthooks.EnvSocket] != "/old.sock" {
		t.Fatal("the caller's env was changed")
	}
	if env, err := e.m.PaneEnv(map[string]string{"A": "1"}, e.spec(RoleJudge)); err != nil || hookEnvOf(env) != [2]string{} {
		t.Fatalf("PaneEnv = %v, %v; want no hook env", env, err)
	}
}

// hookEv is a hook event of role's pane in e.
func (e *env) hookEv(role Role, name, sid string) agenthooks.Event {
	e.t.Helper()
	s := e.session(role)
	agent := agenthooks.AgentClaude
	if m := e.m.sessionKind(s); m == KindCodex {
		agent = agenthooks.AgentCodex
	}
	return agenthooks.Event{V: agenthooks.Version, Agent: agent, Name: name, SessionID: sid, Pane: store.Deref(s.HerdrPaneID)}
}

// wake hands ev to the Manager and waits for the later looks it scheduled.
func (e *env) wake(ev agenthooks.Event) {
	e.t.Helper()
	e.m.Wake(e.ctx, ev)
	e.m.wakes.Wait()
}

// claudeTurnDone prompts claude-review, has its transcript end the turn
// after the prompt, and shows the agent idle. It returns the run's id.
func (e *env) claudeTurnDone(tr *transcript) string {
	e.t.Helper()
	id := e.promptClaude()
	prompted := e.clock.Now()
	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleClaude]; o.Status != herdr.StatusWorking {
		e.t.Fatalf("working reading = %+v", o)
	}
	tr.add(userPrompt(prompted, "/code-review"), assistantSays(prompted.Add(20*time.Second), "Report written."),
		turnEnded(prompted.Add(20*time.Second)))
	e.clock.Add(5 * time.Second)
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	return id
}

// The agent's Stop for the run's own turn is the second idle reading: herdr
// shows the agent idle, the Stop names the run's session, no subagent and
// no background task, and the transcript ended a turn after the prompt.
// The run ends at the hook, not two readings later, and the waiters wake.
func TestAStopOfTheRunsTurnEndsTheRunAtOnce(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.claudeTurnDone(tr)
	ended := e.m.Ended()

	e.wake(e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID))
	if r := e.run1(id); r.State != store.RunEnded || r.EndedAt == nil {
		t.Fatalf("run = %s, want ended by the Stop", r.State)
	}
	if !closed(ended) {
		t.Fatal("the run ended and Ended's channel is still open")
	}
	if s := e.session(RoleClaude); s.IdleTicks != 1 {
		t.Fatalf("idle_ticks = %d: the Stop's look is one reading", s.IdleTicks)
	}
}

// A Codex Stop counts the same way, by the rollout's task_complete.
func TestACodexStopEndsTheRunByItsRolloutsTaskComplete(t *testing.T) {
	e := newEnv(t)
	home, run := e.codexTurn(true)
	e.clock.Add(20 * time.Second)
	e.rolloutAt(home, e.clock.Now(), agentMessage(e.clock.Now().Add(-5*time.Second)), taskComplete(e.clock.Now().Add(-time.Second), "", ""))
	e.h.setAgentStatus(turnJudge, herdr.StatusIdle)
	ev := e.hookEv(RoleJudge, agenthooks.NameStop, codexSID)
	ev.CodexThread, ev.TurnID = codexSID, "t1"

	e.wake(ev)
	if r := e.run1(run.ID); r.State != store.RunEnded {
		t.Fatalf("run = %s, want ended by the Stop", r.State)
	}
}

// A Stop that does not prove the run's turn ended only causes a look: a
// late Stop of an earlier turn, a subagent's, one with background tasks
// running, another session's, a Codex helper's, another agent's in the
// pane. Its looks are readings like a pass's, so the gap rule still holds.
func TestAStopThatDoesNotProveTheTurnEndOnlyObserves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(ev *agenthooks.Event)
		older bool // the transcript's last turn end is before the prompt
	}{
		{name: "a Stop of an earlier turn", older: true},
		{name: "a subagent's", edit: func(ev *agenthooks.Event) { ev.AgentID = "a1f00d" }},
		{name: "with background tasks", edit: func(ev *agenthooks.Event) { ev.Background = 1 }},
		{name: "another session's id", edit: func(ev *agenthooks.Event) { ev.SessionID = "ffffffff-0000-4000-8000-000000000000" }},
		{name: "a Codex helper thread's", edit: func(ev *agenthooks.Event) { ev.CodexThread = "helper-1" }},
		{name: "another agent's in the pane", edit: func(ev *agenthooks.Event) { ev.Agent = agenthooks.AgentCodex }},
		{name: "a forged one without a session", edit: func(ev *agenthooks.Event) { ev.SessionID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			var id string
			if tc.older {
				id = e.promptClaude()
				tr.add(turnEnded(e.clock.Now().Add(-time.Minute)))
				e.clock.Add(30 * time.Second)
				e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
			} else {
				id = e.claudeTurnDone(tr)
			}
			ended := e.m.Ended()
			ev := e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID)
			if tc.edit != nil {
				tc.edit(&ev)
			}
			e.wake(ev)
			if r := e.run1(id); r.State == store.RunEnded {
				t.Fatal("the Stop ended the run")
			}
			if closed(ended) {
				t.Fatal("Ended's channel closed")
			}
			if s := e.session(RoleClaude); s.IdleTicks < 1 || store.Deref(s.AgentStatus) != string(herdr.StatusIdle) {
				t.Fatalf("session = idle_ticks %d status %s: the Stop's look was no reading", s.IdleTicks, store.Deref(s.AgentStatus))
			}
			// The pass the gap later still ends the run, as it would have.
			e.clock.Add(CompletionGap)
			if o := e.observe()[RoleClaude]; o.Kind != ObsCompleted {
				t.Fatalf("reading after the gap = %+v, want completed", o)
			}
		})
	}
}

// herdr's status may lag the hook: a Stop seen while herdr still shows the
// agent working looks again a second later, on a goroutine of its own, and
// ends the run there once herdr shows it idle.
func TestAStopLooksAgainWhileHerdrStillShowsTheAgentWorking(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.claudeTurnDone(tr)
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	var once sync.Once
	e.onSleep = func() { once.Do(func() { e.h.setAgentStatus(claudeAgent, herdr.StatusIdle) }) }

	e.wake(e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID))
	if r := e.run1(id); r.State != store.RunEnded {
		t.Fatalf("run = %s, want ended by the look a second later", r.State)
	}
	if !slices.Equal(e.slept, []time.Duration{time.Second}) {
		t.Fatalf("waits = %v, want one second (the run ended at the first look again)", e.slept)
	}
}

// Wake never holds its caller for the later looks: it returns after its
// first, and looks again at +1 s and +2 s, then stops.
func TestWakeLooksAgainOnItsOwnAndStopsAfterTwoSeconds(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.claudeTurnDone(tr)
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	release := make(chan struct{})
	var mu sync.Mutex
	sleeps := 0
	e.m.sleep = func(ctx context.Context, d time.Duration) error {
		<-release
		mu.Lock()
		sleeps++
		mu.Unlock()
		return ctx.Err()
	}
	done := make(chan struct{})
	go func() { e.m.Wake(e.ctx, e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID)); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wake waited for its later looks")
	}
	close(release)
	e.m.wakes.Wait()
	if sleeps != 2 {
		t.Fatalf("%d later looks, want 2", sleeps)
	}
	if r := e.run1(id); r.State == store.RunEnded {
		t.Fatal("the run ended while herdr showed the agent working")
	}
}

// A permission request has the observer deny the prompt now, as a pass
// would, and the hook's look counts as no observe tick: the deny's pace
// stays the passes'.
func TestAPermissionRequestHasThePromptDeniedNow(t *testing.T) {
	e := newEnv(t)
	e.started()
	run := e.blockedOn(RoleClaude, claudeRmPrompt)
	e.h.onKeys = resolveOn("2")
	tick := e.m.tick

	e.wake(e.hookEv(RoleClaude, agenthooks.NamePermissionRequest, ""))
	if got := e.keysSent(); !slices.Equal(got, []string{claudeAgent + ":2"}) {
		t.Fatalf("keys = %q, want the deny", got)
	}
	evs := e.promptEvents(EventPromptDenied)
	if len(evs) != 1 || !strings.Contains(string(evs[0].Data), run) {
		t.Fatalf("%s events = %+v", EventPromptDenied, evs)
	}
	if e.m.tick != tick {
		t.Fatalf("observe tick %d after the look, want %d", e.m.tick, tick)
	}
}

// A StopFailure looks now and hands its error to HookFailure, with the
// session's row; without a callback it only looks.
func TestAStopFailureHandsItsErrorToHookFailure(t *testing.T) {
	e := newEnv(t)
	e.started()
	var got []string
	e.m.d.HookFailure = func(s store.Session, errText string) { got = append(got, s.Role+" "+errText) }
	ev := e.hookEv(RoleClaude, agenthooks.NameStopFailure, claudeSID)
	ev.Error = "rate_limit"
	e.wake(ev)
	if !slices.Equal(got, []string{"claude-review rate_limit"}) {
		t.Fatalf("HookFailure got %q", got)
	}
	e.m.d.HookFailure = nil
	e.wake(ev) // nil-safe
}

// noID clears the session id herdr's report gave role's row.
func (e *env) noID(role Role) store.Session {
	e.t.Helper()
	s := e.session(role)
	if err := e.st.UpdateSession(e.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("session_id", nil) }); err != nil {
		e.t.Fatal(err)
	}
	return e.session(role)
}

// A SessionStart gives a row without an id the agent's own, when the
// transcript it names is the slot's; nested ids never land: a subagent's, a
// transcript of another checkout, another agent's in the pane, an id
// another session holds, and a second id for a row that has one.
func TestASessionStartFillsAMissingIDAndSkipsNestedOnes(t *testing.T) {
	transcriptOf := func(e *env, project string) string {
		return filepath.Join(e.m.d.ClaudeDir, "projects", project, claudeSID+".jsonl")
	}
	t.Run("the session's own", func(t *testing.T) {
		e := newEnv(t)
		e.claudeTranscript()
		e.started()
		if s := e.noID(RoleClaude); s.SessionID != nil {
			t.Fatal("the row has an id")
		}
		ev := e.hookEv(RoleClaude, agenthooks.NameSessionStart, claudeSID)
		ev.TranscriptPath, ev.Source = transcriptOf(e, claudeProject), "startup"
		e.wake(ev)
		if got := store.Deref(e.session(RoleClaude).SessionID); got != claudeSID {
			t.Fatalf("session_id = %q, want the hook's", got)
		}
		evs := e.recoveredIDEvents()
		if len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"source":"hook"`) {
			t.Fatalf("%s events = %+v", EventSessionID, evs)
		}
		if s := e.session(RoleClaude); s.State != store.SessionLive {
			t.Fatalf("state = %s", s.State)
		}
	})
	for _, tc := range []struct {
		name string
		edit func(e *env, ev *agenthooks.Event)
	}{
		{"a subagent's", func(e *env, ev *agenthooks.Event) { ev.AgentID = "a1f00d" }},
		{"another checkout's transcript", func(e *env, ev *agenthooks.Event) {
			ev.TranscriptPath = transcriptOf(e, "-Users-x-Projects-talkable-review2")
		}},
		{"a transcript outside the config dir", func(e *env, ev *agenthooks.Event) {
			ev.TranscriptPath = filepath.Join("/tmp/projects", claudeProject, claudeSID+".jsonl")
		}},
		{"another agent's in the pane", func(e *env, ev *agenthooks.Event) { ev.Agent = agenthooks.AgentCodex }},
		{"an id another session holds", func(e *env, ev *agenthooks.Event) {
			judge := e.session(RoleJudge)
			if err := e.st.UpdateSession(e.ctx, judge.ID, func(u *store.SessionUpdate) { u.Set("session_id", claudeSID) }); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"a second id", func(e *env, ev *agenthooks.Event) {
			s := e.session(RoleClaude)
			if err := e.st.UpdateSession(e.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("session_id", "0a1b2c3d-0000-4000-8000-0000000000aa") }); err != nil {
				e.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.claudeTranscript()
			e.started()
			e.noID(RoleClaude)
			ev := e.hookEv(RoleClaude, agenthooks.NameSessionStart, claudeSID)
			ev.TranscriptPath = transcriptOf(e, claudeProject)
			tc.edit(e, &ev)
			before := store.Deref(e.session(RoleClaude).SessionID)
			e.wake(ev)
			if got := store.Deref(e.session(RoleClaude).SessionID); got != before {
				t.Fatalf("session_id = %q, want it left at %q", got, before)
			}
			if evs := e.recoveredIDEvents(); len(evs) != 0 {
				t.Fatalf("%s events = %+v", EventSessionID, evs)
			}
		})
	}
}

// A Codex SessionStart passes the rollout's checks: the interactive TUI's
// rollout of the id, in the slot. Not a helper thread's, not `codex exec`'s,
// not one of another checkout. A rollout not written yet is checked again
// at the pane's next event.
func TestACodexSessionStartFillsTheIDItsRolloutProves(t *testing.T) {
	const cwd = "/Users/x/Projects/talkable.review1"
	start := func(t *testing.T) (*env, string) {
		t.Helper()
		e := newEnv(t)
		home := t.TempDir()
		e.m.d.CodexHome = home
		e.codexWithoutID(cwd)
		return e, home
	}
	codexStart := func(e *env) agenthooks.Event {
		ev := e.hookEv(RoleJudge, agenthooks.NameSessionStart, codexSID)
		ev.CodexThread, ev.Source = codexSID, "startup"
		return ev
	}
	t.Run("the TUI's rollout in the slot", func(t *testing.T) {
		e, home := start(t)
		metaRollout(t, home, codexSID, cwd, "cli", "", e.session(RoleJudge).StartedAt)
		e.wake(codexStart(e))
		if got := store.Deref(e.session(RoleJudge).SessionID); got != codexSID {
			t.Fatalf("session_id = %q, want the hook's", got)
		}
	})
	t.Run("a rollout written after the hook", func(t *testing.T) {
		e, home := start(t)
		e.wake(codexStart(e))
		if got := store.Deref(e.session(RoleJudge).SessionID); got != "" {
			t.Fatalf("session_id = %q before its rollout exists", got)
		}
		metaRollout(t, home, codexSID, cwd, "cli", "", e.session(RoleJudge).StartedAt)
		e.wake(e.hookEv(RoleJudge, agenthooks.NameSessionEnd, ""))
		if got := store.Deref(e.session(RoleJudge).SessionID); got != codexSID {
			t.Fatalf("session_id = %q, want the waiting hook's once its rollout is there", got)
		}
	})
	for _, tc := range []struct {
		name                 string
		source, parent, wcwd string
		edit                 func(ev *agenthooks.Event)
	}{
		{name: "a helper thread's", source: "cli", wcwd: cwd, edit: func(ev *agenthooks.Event) { ev.CodexThread = "01a0fbf8-0c6b-7841-aee0-000000000001" }},
		{name: "codex exec's", source: "exec", wcwd: cwd},
		{name: "a helper's rollout", source: "cli", parent: "01a0fbf8-0c6b-7841-aee0-000000000002", wcwd: cwd},
		{name: "another checkout's", source: "cli", wcwd: "/Users/x/Projects/talkable.review2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, home := start(t)
			metaRollout(t, home, codexSID, tc.wcwd, tc.source, tc.parent, e.session(RoleJudge).StartedAt)
			ev := codexStart(e)
			if tc.edit != nil {
				tc.edit(&ev)
			}
			e.wake(ev)
			if got := store.Deref(e.session(RoleJudge).SessionID); got != "" {
				t.Fatalf("session_id = %q, want none", got)
			}
		})
	}
}

// An event of a pane no session of magnum's has is dropped: no look, no
// callback, no id.
func TestAnEventOfAnUnknownPaneIsDropped(t *testing.T) {
	e := newEnv(t)
	e.claudeTranscript()
	ws := e.started()
	e.noID(RoleClaude)
	// The newest row is an agent's, which a lookup by anything but the pane
	// would find.
	if _, err := e.m.EnsurePane(e.ctx, e.pr, ws, "/Users/x/Projects/talkable.review1", nil, e.spec(RoleSimplify)); err != nil {
		t.Fatal(err)
	}
	called := false
	e.m.d.HookFailure = func(store.Session, string) { called = true }
	snaps := len(e.h.callsWith("Snapshot"))
	for _, name := range []string{agenthooks.NameStop, agenthooks.NameStopFailure, agenthooks.NamePermissionRequest, agenthooks.NameSessionStart,
		agenthooks.NameSessionEnd, agenthooks.NameInterrupt} {
		ev := agenthooks.Event{V: 1, Agent: agenthooks.AgentClaude, Name: name, SessionID: claudeSID, Error: "rate_limit", Pane: "w9:p9"}
		e.wake(ev)
	}
	if called {
		t.Fatal("HookFailure called for an unknown pane")
	}
	if n := len(e.h.callsWith("Snapshot")); n != snaps {
		t.Fatalf("%d snapshots for an unknown pane", n-snaps)
	}
	if s := e.session(RoleClaude); s.SessionID != nil {
		t.Fatalf("session_id = %q from an unknown pane", *s.SessionID)
	}
}

// The look's observations reach the daemon through its next observe pass,
// once, as the pass's own would: the factory hears of the turn's end there.
func TestAHookLooksObservationsComeWithTheNextPass(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.claudeTurnDone(tr)
	e.wake(e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID))

	snap, _ := e.h.Snapshot(e.ctx)
	obs, err := e.m.ObserveRunsAt(e.ctx, snap, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Kind != ObsCompleted || obs[0].Run == nil || obs[0].Run.ID != id || obs[0].Role != RoleClaude {
		t.Fatalf("pass after the hook = %+v, want the look's completed run", obs)
	}
	obs, err = e.m.ObserveSnapshotAt(e.ctx, snap, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.Kind == ObsCompleted {
			t.Fatalf("the next pass returned the completion again: %+v", o)
		}
	}
}

// One snapshot is applied at a time, in the order taken: a pass whose
// snapshot is older than a hook's look leaves that session alone, so it
// never writes back the status the look replaced; the other sessions it
// reads as usual.
func TestAPassOlderThanAHooksLookLeavesTheSessionAlone(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	e.claudeTurnDone(tr)
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	staleAt := e.clock.Now()
	stale, _ := e.h.Snapshot(e.ctx)
	e.clock.Add(time.Second)
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.wake(e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID))

	judgeTicks := e.session(RoleJudge).IdleTicks
	if _, err := e.m.ObserveSnapshotAt(e.ctx, stale, staleAt); err != nil {
		t.Fatal(err)
	}
	if s := e.session(RoleClaude); store.Deref(s.AgentStatus) != string(herdr.StatusIdle) {
		t.Fatalf("claude status = %s: the older snapshot overwrote the look's", store.Deref(s.AgentStatus))
	}
	if got := e.session(RoleJudge).IdleTicks; got != judgeTicks+1 {
		t.Fatalf("judge idle_ticks = %d, want %d: the pass skipped another session", got, judgeTicks+1)
	}
	// A pass as new as the look reads the session again.
	if _, err := e.m.ObserveSnapshotAt(e.ctx, stale, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if s := e.session(RoleClaude); store.Deref(s.AgentStatus) != string(herdr.StatusWorking) {
		t.Fatalf("claude status = %s after a newer pass", store.Deref(s.AgentStatus))
	}
}

// A look waits for the pass in flight: Wake takes the observe lock the
// passes hold.
func TestAHookLookWaitsForTheObservePassInFlight(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.m.obsMu.Lock()
	done := make(chan struct{})
	go func() { e.m.Wake(e.ctx, e.hookEv(RoleClaude, agenthooks.NameSessionEnd, "")); close(done) }()
	select {
	case <-done:
		e.m.obsMu.Unlock()
		t.Fatal("the look ran while a pass held the observe lock")
	case <-time.After(100 * time.Millisecond):
	}
	e.m.obsMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the look never ran")
	}
}
