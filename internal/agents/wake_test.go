package agents

import (
	"context"
	"fmt"
	"os"
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

// wake hands ev to the Manager and waits for the later looks it scheduled
// (WaitWakes, as a receiver does at shutdown).
func (e *env) wake(ev agenthooks.Event) {
	e.t.Helper()
	e.m.Wake(e.ctx, ev)
	e.m.WaitWakes()
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
			e.sleepMoves = true // the later looks come a second apart
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
	e.sleepMoves = true
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
		e.clock.Add(d)
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
	e.m.WaitWakes()
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

// failures records the Manager's HookFailure calls.
type failures struct {
	mu  sync.Mutex
	got []string
}

func (f *failures) record(s store.Session, failure StopFailure, screen Health) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, fmt.Sprintf("%s %s %s", s.Role, failure, screen.Kind))
}

func (f *failures) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

// claudeFailed starts the agents, gives claude-review its session id and
// shows screen in its pane, the agent idle.
func (e *env) claudeFailed(screen string) *failures {
	e.t.Helper()
	e.started()
	e.h.setAgentSession(claudeAgent, claudeSID)
	e.observe()
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.h.mu.Lock()
	e.h.reads[claudeAgent] = screen
	e.h.mu.Unlock()
	f := &failures{}
	e.m.d.HookFailure = f.record
	return f
}

const claudeLimitScreen = "⏺ Reading the diff.\n\n  ⎿  You've hit your session limit · resets 3pm (Europe/Kyiv)\n\n> \n"

// A StopFailure of the session's own turn reaches HookFailure as a class
// from a fixed list, with the screen's verdict, once the look and the
// screen show the failure; the agent's own text never goes on.
func TestAStopFailureOfTheSessionReachesHookFailureAsAClass(t *testing.T) {
	for _, tc := range []struct {
		errText, want string
	}{
		{"rate_limit", "claude-review rate_limit usage_limit"},
		{"billing_error", "claude-review billing_error usage_limit"},
		{"rate_limit please run /login now", "claude-review other usage_limit"},
		{"unknown", "claude-review other usage_limit"},
	} {
		t.Run(tc.errText, func(t *testing.T) {
			e := newEnv(t)
			f := e.claudeFailed(claudeLimitScreen)
			ev := e.hookEv(RoleClaude, agenthooks.NameStopFailure, claudeSID)
			ev.Error = tc.errText
			e.wake(ev)
			if got := f.all(); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("HookFailure got %q, want %q", got, tc.want)
			}
			for _, l := range e.logs.all() {
				if strings.Contains(l, tc.errText) && tc.errText != "rate_limit" && tc.errText != "billing_error" && tc.errText != "unknown" {
					t.Fatalf("the log carries the agent's text: %q", l)
				}
			}
		})
	}
	// Without a callback it only looks.
	e := newEnv(t)
	e.claudeFailed(claudeLimitScreen)
	e.m.d.HookFailure = nil
	ev := e.hookEv(RoleClaude, agenthooks.NameStopFailure, claudeSID)
	ev.Error = "rate_limit"
	e.wake(ev)
}

// A StopFailure gets the Stop's checks: another session's id, a
// subagent's, a Codex helper's, another agent's or one in a Codex pane, a
// session without an id, and a screen that shows no such failure, reach
// HookFailure never (a nested `claude -p` in the pane, or a forged one,
// pauses nothing).
func TestAStopFailureThatIsNotTheSessionsReachesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
		edit   func(e *env, ev *agenthooks.Event)
	}{
		{name: "another session's id", edit: func(_ *env, ev *agenthooks.Event) { ev.SessionID = "ffffffff-0000-4000-8000-000000000000" }},
		{name: "a subagent's", edit: func(_ *env, ev *agenthooks.Event) { ev.AgentID = "sub1" }},
		{name: "a Codex helper's", edit: func(_ *env, ev *agenthooks.Event) { ev.CodexThread = "thread-1" }},
		{name: "another agent's", edit: func(_ *env, ev *agenthooks.Event) { ev.Agent = agenthooks.AgentCodex }},
		{name: "a Codex pane's", edit: func(e *env, ev *agenthooks.Event) {
			*ev = e.hookEv(RoleJudge, agenthooks.NameStopFailure, codexSID)
			ev.Agent, ev.Error = agenthooks.AgentClaude, "rate_limit"
			e.h.setAgentSession(judgeAgent, codexSID)
			e.observe()
			e.h.mu.Lock()
			e.h.reads[judgeAgent] = claudeLimitScreen
			e.h.mu.Unlock()
		}},
		{name: "a session without an id", edit: func(e *env, ev *agenthooks.Event) {
			e.h.setAgentSession(claudeAgent, "")
			e.noID(RoleClaude)
		}},
		{name: "a screen that shows no failure", screen: "⏺ Report written.\n\n> \n"},
		{name: "a screen that shows another failure", screen: "  ⎿  API Error: 529 overloaded_error\n> \n",
			edit: func(_ *env, ev *agenthooks.Event) { ev.Error = "authentication_failed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			screen := claudeLimitScreen
			if tc.screen != "" {
				screen = tc.screen
			}
			f := e.claudeFailed(screen)
			ev := e.hookEv(RoleClaude, agenthooks.NameStopFailure, claudeSID)
			ev.Error = "rate_limit"
			if tc.edit != nil {
				tc.edit(e, &ev)
			}
			e.wake(ev)
			if got := f.all(); len(got) != 0 {
				t.Fatalf("HookFailure got %q, want nothing", got)
			}
		})
	}
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

// sessionFile writes Claude session file id's transcript in project of
// the env's Claude config dir: a record without a session id first (Claude
// Code's file-history snapshot), then the first one with one, naming
// recordID in checkout cwd.
func (e *env) sessionFile(project, id, recordID, cwd string) string {
	e.t.Helper()
	path := filepath.Join(e.m.d.ClaudeDir, "projects", project, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatal(err)
	}
	body := `{"type":"file-history-snapshot","messageId":"m1","snapshot":{}}` + "\n" +
		`{"parentUuid":null,"isSidechain":false,"type":"user","sessionId":"` + recordID + `","cwd":"` + cwd +
		`","message":{"role":"user","content":"review"},"timestamp":"2026-10-03T12:00:00.000Z"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

const slotCwd = "/Users/x/Projects/talkable.review1"

// A SessionStart gives a row without an id the agent's own once the
// session's transcript is where magnum reads that slot's sessions and its
// first record names the id in the slot. Until the file is there the event
// waits, and the pane's next event checks it again.
func TestASessionStartFillsAMissingIDOnceItsTranscriptNamesIt(t *testing.T) {
	e := newEnv(t)
	e.claudeTranscript()
	e.started()
	if s := e.noID(RoleClaude); s.SessionID != nil {
		t.Fatal("the row has an id")
	}
	ev := e.hookEv(RoleClaude, agenthooks.NameSessionStart, claudeSID)
	ev.TranscriptPath = filepath.Join(e.m.d.ClaudeDir, "projects", claudeProject, claudeSID+".jsonl")
	ev.Source = "startup"
	e.wake(ev)
	if got := store.Deref(e.session(RoleClaude).SessionID); got != "" {
		t.Fatalf("session_id = %q before its transcript exists", got)
	}
	e.sessionFile(claudeProject, claudeSID, claudeSID, slotCwd)
	e.clock.Add(time.Second)
	e.wake(e.hookEv(RoleClaude, agenthooks.NameSessionEnd, ""))
	if got := store.Deref(e.session(RoleClaude).SessionID); got != claudeSID {
		t.Fatalf("session_id = %q, want the waiting hook's once its transcript names it", got)
	}
	evs := e.recoveredIDEvents()
	if len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"source":"hook"`) {
		t.Fatalf("%s events = %+v", EventSessionID, evs)
	}
	if s := e.session(RoleClaude); s.State != store.SessionLive {
		t.Fatalf("state = %s", s.State)
	}
}

// The review's probe: a forged SessionStart names a flag as the session id
// and the slot's path for it. No id lands that is not a UUID, whatever the
// file says, and none that the transcript does not name: a forged event
// without a file, a file whose first record names another session or works
// in another checkout, a path the event names elsewhere, a subagent's,
// another agent's, an id another session holds, a second id.
func TestASessionStartRecordsNoIDItsTranscriptDoesNotProve(t *testing.T) {
	const other = "0a1b2c3d-0000-4000-8000-0000000000aa"
	for _, tc := range []struct {
		name string
		id   string
		edit func(e *env, ev *agenthooks.Event)
	}{
		{name: "a flag as the id, its file there", id: "--dangerously-skip-permissions"},
		{name: "a flag with a value", id: "--settings=x.json"},
		{name: "an id that is no UUID", id: "abc-123"},
		{name: "no transcript file", edit: func(e *env, ev *agenthooks.Event) {
			if err := os.Remove(filepath.Join(e.m.d.ClaudeDir, "projects", claudeProject, claudeSID+".jsonl")); err != nil {
				e.t.Fatal(err)
			}
		}},
		{name: "a first record of another session", edit: func(e *env, ev *agenthooks.Event) {
			e.sessionFile(claudeProject, claudeSID, other, slotCwd)
		}},
		{name: "a first record in another checkout", edit: func(e *env, ev *agenthooks.Event) {
			e.sessionFile(claudeProject, claudeSID, claudeSID, "/Users/x/Projects/talkable.review2")
		}},
		{name: "another checkout's path", edit: func(e *env, ev *agenthooks.Event) {
			ev.TranscriptPath = filepath.Join(e.m.d.ClaudeDir, "projects", "-Users-x-Projects-talkable-review2", claudeSID+".jsonl")
		}},
		{name: "a path outside the config dir", edit: func(e *env, ev *agenthooks.Event) {
			ev.TranscriptPath = filepath.Join("/tmp/projects", claudeProject, claudeSID+".jsonl")
		}},
		{name: "a subagent's", edit: func(e *env, ev *agenthooks.Event) { ev.AgentID = "a1f00d" }},
		{name: "another agent's in the pane", edit: func(e *env, ev *agenthooks.Event) { ev.Agent = agenthooks.AgentCodex }},
		{name: "an id another session holds", edit: func(e *env, ev *agenthooks.Event) {
			judge := e.session(RoleJudge)
			if err := e.st.UpdateSession(e.ctx, judge.ID, func(u *store.SessionUpdate) { u.Set("session_id", claudeSID) }); err != nil {
				e.t.Fatal(err)
			}
		}},
		{name: "a second id", edit: func(e *env, ev *agenthooks.Event) {
			s := e.session(RoleClaude)
			if err := e.st.UpdateSession(e.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("session_id", other) }); err != nil {
				e.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.claudeTranscript()
			e.started()
			e.noID(RoleClaude)
			id := claudeSID
			if tc.id != "" {
				id = tc.id
			}
			e.sessionFile(claudeProject, id, id, slotCwd)
			ev := e.hookEv(RoleClaude, agenthooks.NameSessionStart, id)
			ev.TranscriptPath = filepath.Join(e.m.d.ClaudeDir, "projects", claudeProject, id+".jsonl")
			if tc.edit != nil {
				tc.edit(e, &ev)
			}
			before := store.Deref(e.session(RoleClaude).SessionID)
			e.wake(ev)
			e.clock.Add(time.Second)
			e.wake(e.hookEv(RoleClaude, agenthooks.NameSessionEnd, "")) // a waiting event is checked again
			if got := store.Deref(e.session(RoleClaude).SessionID); got != before {
				t.Fatalf("session_id = %q, want it left at %q", got, before)
			}
			if evs := e.recoveredIDEvents(); len(evs) != 0 {
				t.Fatalf("%s events = %+v", EventSessionID, evs)
			}
		})
	}
}

// A role whose last session recorded an id a CLI would read as a flag
// starts fresh: the id never reaches the launch.
func TestAResumeOfAnIDThatCouldBeAFlagStartsFresh(t *testing.T) {
	for _, id := range []string{"--dangerously-skip-permissions", "--settings=x.json", "-r", "a b"} {
		t.Run(id, func(t *testing.T) {
			e := newEnv(t)
			ws := e.workspace()
			if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], id); err != nil {
				t.Fatal(err)
			}
			e.h.mu.Lock()
			args := slices.Clone(e.h.starts[len(e.h.starts)-1].Args)
			e.h.mu.Unlock()
			if slices.Contains(args, id) || slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--resume") }) {
				t.Fatalf("args = %q: the id reached the launch", args)
			}
			if s := e.session(RoleClaude); s.ResumedFrom != nil || s.SessionID != nil {
				t.Fatalf("row = resumed_from %v session_id %v, want a fresh start", s.ResumedFrom, s.SessionID)
			}
		})
	}
	e := newEnv(t)
	ws := e.workspace()
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], claudeSID); err != nil {
		t.Fatal(err)
	}
	if args := e.h.starts[len(e.h.starts)-1].Args; !slices.Contains(args, claudeSID) {
		t.Fatalf("args = %q, want the UUID resumed", args)
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
		e.clock.Add(time.Second)
		e.wake(e.hookEv(RoleJudge, agenthooks.NameSessionEnd, ""))
		if got := store.Deref(e.session(RoleJudge).SessionID); got != codexSID {
			t.Fatalf("session_id = %q, want the waiting hook's once its rollout is there", got)
		}
	})
	t.Run("an id that is no UUID", func(t *testing.T) {
		e, home := start(t)
		const id = "--last"
		metaRollout(t, home, id, cwd, "cli", "", e.session(RoleJudge).StartedAt)
		ev := e.hookEv(RoleJudge, agenthooks.NameSessionStart, id)
		ev.CodexThread = id
		e.wake(ev)
		if got := store.Deref(e.session(RoleJudge).SessionID); got != "" {
			t.Fatalf("session_id = %q, want none", got)
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
	e.m.d.HookFailure = func(store.Session, StopFailure, Health) { called = true }
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

// The review's M1: a look's snapshot may take a while, and a pass that
// started while it was being taken may read herdr before it. The look
// counts from when its snapshot came back, so such a pass leaves the
// session alone instead of writing back the older status.
func TestAPassThatStartedDuringALooksSnapshotLeavesTheSessionAlone(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	e.claudeTurnDone(tr)
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	stale, _ := e.h.Snapshot(e.ctx) // herdr read while the agent worked
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	start := e.clock.Now()
	slow := true
	e.h.onSnapshot = func() {
		if slow {
			slow = false
			e.clock.Add(2 * time.Second) // the look's herdr call takes two seconds
		}
	}
	e.wake(e.hookEv(RoleClaude, agenthooks.NameSessionEnd, ""))
	e.h.onSnapshot = nil
	if _, err := e.m.ObserveSnapshotAt(e.ctx, stale, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if s := e.session(RoleClaude); store.Deref(s.AgentStatus) != string(herdr.StatusIdle) {
		t.Fatalf("claude status = %s: a pass that started during the look's snapshot overwrote it", store.Deref(s.AgentStatus))
	}
}

// claudeObs are the claude-review observations of obs.
func claudeObs(obs []Observation) []Observation {
	var out []Observation
	for _, o := range obs {
		if o.Role == RoleClaude {
			out = append(out, o)
		}
	}
	return out
}

// The looks' observations wait for the next pass as each session's newest
// reading plus every run end and loss: a pane looked at again and again
// costs one entry, its reading without a kind still names when the second
// idle reading is due (SecondReadingAt, the clock's), and no end is pushed
// out.
func TestTheLooksKeepEachSessionsNewestReadingAndEveryEnd(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.claudeTurnDone(tr)
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	staleAt := e.clock.Now()
	stale, _ := e.h.Snapshot(e.ctx)
	for range 4 {
		e.clock.Add(time.Second)
		e.wake(e.hookEv(RoleClaude, agenthooks.NameSessionEnd, ""))
	}
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.clock.Add(time.Second)
	e.wake(e.hookEv(RoleClaude, agenthooks.NameSessionEnd, ""))
	obs, err := e.m.ObserveSnapshotAt(e.ctx, stale, staleAt) // older than the looks: claude's readings are theirs alone
	if err != nil {
		t.Fatal(err)
	}
	got := claudeObs(obs)
	if len(got) != 1 || got[0].Status != herdr.StatusIdle || got[0].Kind != "" || got[0].SecondReadingAt.IsZero() {
		t.Fatalf("claude observations = %+v, want the newest look's idle reading with its second reading due", got)
	}

	e.clock.Add(time.Second)
	e.wake(e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID)) // ends the run
	for range 3 {
		e.clock.Add(time.Second)
		e.wake(e.hookEv(RoleClaude, agenthooks.NameSessionEnd, ""))
	}
	obs, err = e.m.ObserveSnapshotAt(e.ctx, stale, staleAt)
	if err != nil {
		t.Fatal(err)
	}
	got = claudeObs(obs)
	if len(got) != 2 || got[0].Kind != ObsCompleted || got[0].Run == nil || got[0].Run.ID != id || got[1].Kind != "" {
		t.Fatalf("claude observations = %+v, want the run's end, then the newest reading", got)
	}
}

// The review's M3: a Stop that comes while the later looks of an earlier
// event are on their way gets later looks of its own, after those end.
func TestAStopDuringTheLaterLooksGetsLaterLooksOfItsOwn(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	e.claudeTurnDone(tr)
	e.sleepMoves = true
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	n := 0
	e.onSleep = func() {
		if n++; n == 2 { // the earlier event's last wait
			e.m.Wake(e.ctx, e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID))
		}
	}
	e.wake(e.hookEv(RoleClaude, agenthooks.NameInterrupt, claudeSID))
	if len(e.slept) != 4 {
		t.Fatalf("waits = %v, want two for each event", e.slept)
	}
}

// The later looks end with the context Wake was given, and WaitWakes
// returns once they did: a receiver waits for them at shutdown.
func TestTheLaterLooksEndWithTheirContext(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	e.claudeTurnDone(tr)
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	ctx, cancel := context.WithCancel(e.ctx)
	e.onSleep = cancel // the daemon stops during the first wait
	snaps := len(e.h.callsWith("Snapshot"))
	e.m.Wake(ctx, e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID))
	done := make(chan struct{})
	go func() { e.m.WaitWakes(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitWakes did not return after the context ended")
	}
	if got := len(e.h.callsWith("Snapshot")) - snaps; got != 1 {
		t.Fatalf("%d looks, want the first alone", got)
	}
}

// The review's M4: the looks of one session merge. A look within
// lookMerge of the last one, or while another of the session waits for the
// observe lock, is skipped: a flood of events costs no more than about four
// looks a second.
func TestTheLooksOfOneSessionMerge(t *testing.T) {
	e := newEnv(t)
	e.started()
	ev := e.hookEv(RoleClaude, agenthooks.NameSessionEnd, "")
	looks := func() int { return len(e.h.callsWith("Snapshot")) }
	n := looks()
	e.wake(ev)
	e.wake(ev)
	e.clock.Add(lookMerge - time.Millisecond)
	e.wake(ev)
	if got := looks() - n; got != 1 {
		t.Fatalf("%d looks within %s, want 1", got, lookMerge)
	}
	e.clock.Add(time.Millisecond)
	e.wake(ev)
	if got := looks() - n; got != 2 {
		t.Fatalf("%d looks, want 2 once %s passed", got, lookMerge)
	}

	// One look waits for the observe lock: another of the session is skipped.
	e.clock.Add(time.Second)
	e.m.obsMu.Lock()
	done := make(chan struct{})
	go func() { e.m.Wake(e.ctx, ev); close(done) }()
	sid := e.session(RoleClaude).ID
	for queued := false; !queued; {
		e.m.mu.Lock()
		queued = e.m.looks[sid] != nil && e.m.looks[sid].queued
		e.m.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	e.clock.Add(time.Second)
	e.m.Wake(e.ctx, ev) // returns at once
	e.m.obsMu.Unlock()
	<-done
	if got := looks() - n; got != 3 {
		t.Fatalf("%d looks, want 3: the queued look's twin is skipped", got)
	}
}

// The review's M6: herdr shows a permission prompt a second or more after
// the hook. The request looks again at +1 s and +2 s, and the prompt is
// denied at the look that sees it.
func TestAPermissionRequestLooksAgainWhileHerdrLags(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.clock.Add(3 * time.Minute)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunInitial, "review"); err != nil {
		t.Fatal(err)
	}
	e.sleepMoves = true
	e.h.onKeys = resolveOn("2")
	var once sync.Once
	e.onSleep = func() {
		once.Do(func() {
			e.h.setAgentStatus(claudeAgent, herdr.StatusBlocked)
			e.h.mu.Lock()
			e.h.reads[claudeAgent] = claudeRmPrompt
			e.h.mu.Unlock()
		})
	}
	e.wake(e.hookEv(RoleClaude, agenthooks.NamePermissionRequest, ""))
	if got := e.keysSent(); !slices.Equal(got, []string{claudeAgent + ":2"}) {
		t.Fatalf("keys = %q, want the deny at the look a second later", got)
	}
	if n := countWaits(e.slept, time.Second); n != 1 {
		t.Fatalf("waits = %v, want one later look: the deny ends them", e.slept)
	}
}

// countWaits counts the waits of length d.
func countWaits(waits []time.Duration, d time.Duration) int {
	n := 0
	for _, w := range waits {
		if w == d {
			n++
		}
	}
	return n
}

// A Stop never reads the path its event names: a file there with a newer
// turn end ends nothing while the session's own transcript shows none.
func TestAStopNeverReadsThePathItsEventNames(t *testing.T) {
	e := newEnv(t)
	e.sleepMoves = true
	tr := e.claudeTranscript()
	id := e.promptClaude()
	tr.add(turnEnded(e.clock.Now().Add(-time.Minute))) // the session's own: an earlier turn's end
	forged := &transcript{t: t, path: filepath.Join(t.TempDir(), claudeSID+".jsonl")}
	forged.add(turnEnded(e.clock.Now().Add(10 * time.Second)))
	e.clock.Add(30 * time.Second)
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	ev := e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID)
	ev.TranscriptPath = forged.path
	e.wake(ev)
	if r := e.run1(id); r.State == store.RunEnded {
		t.Fatal("the Stop ended the run by the file its event names")
	}
}

// An Interrupt looks, and looks again, but never ends a run by itself: only
// a Stop is a turn's end.
func TestAnInterruptLooksButNeverEndsTheRunByItself(t *testing.T) {
	e := newEnv(t)
	e.sleepMoves = true
	tr := e.claudeTranscript()
	id := e.claudeTurnDone(tr)
	e.wake(e.hookEv(RoleClaude, agenthooks.NameInterrupt, claudeSID))
	if r := e.run1(id); r.State == store.RunEnded {
		t.Fatal("the Interrupt ended the run")
	}
	if len(e.slept) != 2 {
		t.Fatalf("waits = %v, want the two later looks", e.slept)
	}
}

// The hook state of a session no longer starting or live is forgotten at
// the next pass: its look's time and the SessionStart that waited.
func TestTheHookStateOfASessionNoLongerLiveIsForgotten(t *testing.T) {
	e := newEnv(t)
	e.m.d.CodexHome = t.TempDir()
	judge := e.codexWithoutID(slotCwd)
	ev := e.hookEv(RoleJudge, agenthooks.NameSessionStart, codexSID)
	ev.CodexThread = codexSID
	e.wake(ev) // no rollout yet: the event waits
	e.clock.Add(time.Second)
	e.wake(e.hookEv(RoleJudge, agenthooks.NameSessionEnd, ""))
	e.m.obsMu.Lock()
	_, waiting := e.m.hookIDs[judge.ID]
	_, looked := e.m.lookedAt[judge.ID]
	e.m.obsMu.Unlock()
	if !waiting || !looked {
		t.Fatalf("hook state before = waiting %v looked %v", waiting, looked)
	}
	if err := e.st.TransitionSession(e.ctx, judge.ID, nil, store.SessionParked, nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(time.Second)
	e.observe()
	e.m.obsMu.Lock()
	defer e.m.obsMu.Unlock()
	if len(e.m.hookIDs) != 0 || len(e.m.lookedAt) != 0 {
		t.Fatalf("hook state after = %v, %v; want none", e.m.hookIDs, e.m.lookedAt)
	}
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	if e.m.looks[judge.ID] != nil {
		t.Fatal("the merge state of the parked session stays")
	}
}
