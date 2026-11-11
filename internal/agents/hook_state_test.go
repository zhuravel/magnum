package agents

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Every agent pane a daemon that listens for hooks makes names its agent in
// MAGNUM_AGENT, the key its hook events carry: the session row's agent
// name, which needs no herdr. A shell pane has no agent and no key, and the
// rows record the env the panes were made with.
func TestAnAgentsPaneNamesItsAgentAsTheHookKey(t *testing.T) {
	e := newEnv(t)
	e.m.d.HookSocket, e.m.d.HookBin = hookSocket, hookBin
	ws := e.workspace()
	if _, err := e.m.EnsurePane(e.ctx, e.pr, ws, "/Users/x/Projects/talkable.review1", map[string]string{"MAGNUM_PR_URL": e.pr.URL},
		e.spec(RoleSimplify)); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range e.sessions() {
		name, key := store.Deref(s.AgentName), s.Env[agenthooks.EnvAgent]
		switch {
		case store.Deref(s.AgentKind) == KindShell && key != "":
			t.Errorf("%s, a shell pane, got the key %q", s.Role, key)
		case store.Deref(s.AgentKind) != KindShell && (name == "" || key != name):
			t.Errorf("%s row: agent %q, its pane's key %q", s.Role, name, key)
		}
		if name != "" {
			names = append(names, name)
		}
	}
	e.h.mu.Lock()
	keys := []string{e.h.creates[0].Env[agenthooks.EnvAgent]}
	for _, s := range e.h.splits {
		keys = append(keys, s.Opts.Env[agenthooks.EnvAgent])
	}
	e.h.mu.Unlock()
	keys = slices.DeleteFunc(keys, func(k string) bool { return k == "" })
	slices.Sort(names)
	slices.Sort(keys)
	if len(names) != 3 || !slices.Equal(keys, names) {
		t.Fatalf("the panes' keys %q, want the agents' names %q", keys, names)
	}

	shell := e.addRole(config.Role{Name: "lint", Kind: config.KindShell, Command: "bin/lint {{.BaseRef}}"})
	for _, from := range []map[string]string{{agenthooks.EnvSocket: hookSocket}, {agenthooks.EnvSocket: hookSocket, agenthooks.EnvAgent: "mg-1-old-abcdef"}} {
		if env, err := e.m.withAgentKey(e.ctx, e.pr, shell, from); err != nil || env[agenthooks.EnvAgent] != "" {
			t.Fatalf("a shell pane's env from %v = %v, %v; want no key", from, env, err)
		}
	}
}

// Without a socket no pane names a key either, also when the env it is made
// from carries one (a recorded session's).
func TestNoPaneGetsTheHookKeyWithoutASocket(t *testing.T) {
	e := newEnv(t)
	e.m.d.HookBin = hookBin
	stale := map[string]string{"WT_BRANCH": "review1", agenthooks.EnvSocket: "/old.sock", agenthooks.EnvAgent: "mg-1-old-abcdef"}
	if _, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/Users/x/Projects/talkable.review1", stale, "talkable#11920", e.coreRoles()); err != nil {
		t.Fatal(err)
	}
	for _, s := range e.sessions() {
		if k, ok := s.Env[agenthooks.EnvAgent]; ok {
			t.Errorf("%s's pane got the key %q without a socket", s.Role, k)
		}
	}
	if stale[agenthooks.EnvAgent] != "mg-1-old-abcdef" {
		t.Fatal("the caller's env was changed")
	}
}

// keyedEv is ev with role's agent name as its pane key and no herdr pane:
// the event of a pane outside herdr.
func (e *env) keyedEv(role Role, ev agenthooks.Event) agenthooks.Event {
	e.t.Helper()
	ev.Pane, ev.PaneKey = "", store.Deref(e.session(role).AgentName)
	if ev.PaneKey == "" {
		e.t.Fatalf("%s has no agent name", role)
	}
	return ev
}

// An event that names its pane key reaches the session whose agent has that
// name, with no herdr pane or with another pane's: the run's own Stop ends
// the run as one by the pane does.
func TestAHookEventReachesItsSessionByItsPaneKey(t *testing.T) {
	for _, pane := range []string{"", "judge's"} {
		e := newEnv(t)
		tr := e.claudeTranscript()
		id := e.claudeTurnDone(tr)
		ev := e.keyedEv(RoleClaude, e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID))
		if pane != "" {
			ev.Pane = store.Deref(e.session(RoleJudge).HerdrPaneID)
		}
		e.wake(ev)
		if r := e.run1(id); r.State != store.RunEnded {
			t.Fatalf("pane %q: run = %s, want ended by the keyed Stop", pane, r.State)
		}
	}
}

// An event without the key (a pane made before it, an older hook-event)
// still reaches its session by its herdr pane, and so does one whose key no
// live session's agent has (a row whose name a failed rename cleared).
func TestAnEventWithOnlyItsHerdrPaneStillReachesItsSession(t *testing.T) {
	e := newEnv(t)
	e.started()
	s := e.session(RoleClaude)
	e.wake(e.hookEv(RoleClaude, agenthooks.NameUserPromptSubmit, claudeSID))
	if got := e.m.hookStateOf(s.ID); got != HookWorking {
		t.Fatalf("hook state by the pane = %q, want working", got)
	}
	ev := e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID)
	ev.PaneKey = "mg-1-gone-abcdef"
	e.wake(ev)
	if got := e.m.hookStateOf(s.ID); got != HookIdle {
		t.Fatalf("hook state by the pane, an unknown key = %q, want idle", got)
	}
	e.wake(agenthooks.Event{V: agenthooks.Version, Agent: agenthooks.AgentClaude, Name: agenthooks.NameUserPromptSubmit, PaneKey: "mg-1-gone-abcdef"})
	if got := e.m.hookStateOf(s.ID); got != HookIdle {
		t.Fatalf("an event of no session's key or pane changed the state to %q", got)
	}
}

// The hooks give each session a state of their own: ready at a
// SessionStart, working at a UserPromptSubmit, blocked at a
// PermissionRequest, idle at a Stop (or a StopFailure, or Codex's
// Interrupt). A subagent's or a Codex helper's Stop is not the session's
// turn end, and a SessionStart of a compaction leaves the turn as it was.
func TestTheHooksGiveEachSessionAStateOfItsOwn(t *testing.T) {
	e := newEnv(t)
	e.started()
	s := e.session(RoleClaude)
	if got := e.m.hookStateOf(s.ID); got != "" {
		t.Fatalf("before any event: %q", got)
	}
	step := func(name string, edit func(*agenthooks.Event), want HookState) {
		t.Helper()
		ev := e.hookEv(RoleClaude, name, claudeSID)
		if edit != nil {
			edit(&ev)
		}
		e.wake(ev)
		if got := e.m.hookStateOf(s.ID); got != want {
			t.Fatalf("after %s: %q, want %q", name, got, want)
		}
	}
	step(agenthooks.NameSessionStart, func(ev *agenthooks.Event) { ev.Source = "startup" }, HookReady)
	step(agenthooks.NameUserPromptSubmit, nil, HookWorking)
	step(agenthooks.NameStop, func(ev *agenthooks.Event) { ev.AgentID = "sub-1" }, HookWorking)
	step(agenthooks.NameStop, func(ev *agenthooks.Event) { ev.CodexThread = "helper-2" }, HookWorking)
	step(agenthooks.NameSessionStart, func(ev *agenthooks.Event) { ev.Source = "compact" }, HookWorking)
	step(agenthooks.NamePermissionRequest, func(ev *agenthooks.Event) { ev.AgentID = "sub-1" }, HookBlocked)
	step(agenthooks.NameStop, nil, HookIdle)
	step(agenthooks.NameUserPromptSubmit, nil, HookWorking)
	step(agenthooks.NameStopFailure, nil, HookIdle)
	step(agenthooks.NameUserPromptSubmit, nil, HookWorking)
	step(agenthooks.NameInterrupt, nil, HookIdle)
	step(agenthooks.NameSessionEnd, nil, "")
}

// differs are the hook_state_differs events of e's PR.
func (e *env) differs() []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for _, ev := range e.eventsOf(EventHookStateDiffers) {
		if ev.Level != "info" {
			e.t.Errorf("event level %q, want info", ev.Level)
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			e.t.Fatal(err)
		}
		data["message"] = ev.Message
		out = append(out, data)
	}
	return out
}

// A herdr status that disagrees with the hooks' state for longer than the
// observe interval writes one agent.hook_state_differs event when they
// agree again, with both states and how long they differed; the same pair
// of the same session writes no second one. An agreeing pass, and a
// disagreement shorter than the interval, write none, and nothing waits on
// or acts on the hooks' state: herdr's status stays the truth.
func TestAHerdrStatusThatDisagreesLongerThanTheIntervalWritesOneEvent(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.wake(e.hookEv(RoleClaude, agenthooks.NameSessionStart, claudeSID))
	e.clock.Add(time.Second)
	e.observe() // ready and idle agree
	if got := e.differs(); len(got) != 0 {
		t.Fatalf("an agreeing pass wrote %v", got)
	}

	// A disagreement shorter than the interval (5 s): none.
	e.wake(e.hookEv(RoleClaude, agenthooks.NameUserPromptSubmit, claudeSID))
	e.clock.Add(time.Second)
	e.observe()
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	e.clock.Add(2 * time.Second)
	e.observe()
	if got := e.differs(); len(got) != 0 {
		t.Fatalf("a short disagreement wrote %v", got)
	}

	// herdr shows the agent working 30 s after its Stop.
	e.wake(e.hookEv(RoleClaude, agenthooks.NameStop, claudeSID))
	for range 3 {
		e.clock.Add(10 * time.Second)
		e.observe()
	}
	if got := e.differs(); len(got) != 0 {
		t.Fatalf("an ongoing disagreement wrote %v", got)
	}
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.clock.Add(10 * time.Second)
	e.observe()
	got := e.differs()
	if len(got) != 1 {
		t.Fatalf("events = %v, want one", got)
	}
	if d := got[0]; d["hook"] != "idle" || d["herdr"] != "working" || d["by"] != agenthooks.NameStop || d["role"] != string(RoleClaude) || d["kind"] != KindClaude ||
		d["seconds"] != float64(30) || !strings.Contains(d["message"].(string), "for 30s") {
		t.Fatalf("event = %v", d)
	}

	// The same pair again: no second event.
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	for range 3 {
		e.clock.Add(10 * time.Second)
		e.observe()
	}
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.clock.Add(10 * time.Second)
	e.observe()
	if got := e.differs(); len(got) != 1 {
		t.Fatalf("events = %v, want still one", got)
	}
}

// A disagreement still under way when its session is lost is written then,
// from the later of the two changes (here the hooks') to the last pass that
// saw it.
func TestADisagreementOfASessionThatEndsIsWrittenAtItsEnd(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
	e.clock.Add(time.Second)
	e.observe()
	e.clock.Add(time.Second)
	e.wake(e.hookEv(RoleClaude, agenthooks.NamePermissionRequest, claudeSID))
	for range 3 {
		e.clock.Add(10 * time.Second)
		e.observe()
	}
	e.h.removeAgent(claudeAgent)
	e.clock.Add(time.Minute)
	e.observe() // the session is lost
	e.clock.Add(time.Minute)
	e.observe() // and no longer live
	got := e.differs()
	if len(got) != 1 || got[0]["hook"] != "blocked" || got[0]["herdr"] != "working" || got[0]["seconds"] != float64(30) {
		t.Fatalf("events = %v, want blocked against working for 30 s", got)
	}
}

// While the hooks say working and herdr idle, the session's own transcript
// (Claude) or rollout (Codex) settles it: a turn end after the hooks said
// working makes the hooks' state idle, so a lost Stop is no disagreement.
func TestATurnEndInTheTranscriptOrRolloutMakesTheHookStateIdle(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	e.promptClaude()
	s := e.session(RoleClaude)
	e.wake(e.hookEv(RoleClaude, agenthooks.NameUserPromptSubmit, claudeSID))
	said := e.clock.Now()
	tr.add(turnEnded(said.Add(-time.Second))) // an earlier turn's end settles nothing
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.clock.Add(10 * time.Second)
	e.observe()
	if got := e.m.hookStateOf(s.ID); got != HookWorking {
		t.Fatalf("after an earlier turn's end: %q, want working", got)
	}
	tr.add(turnEnded(said.Add(20 * time.Second)))
	e.clock.Add(20 * time.Second)
	e.observe()
	if got := e.m.hookStateOf(s.ID); got != HookIdle {
		t.Fatalf("after the turn's end in the transcript: %q, want idle", got)
	}

	c := newEnv(t)
	home, _ := c.codexTurn(true)
	j := c.session(RoleJudge)
	ev := c.hookEv(RoleJudge, agenthooks.NameUserPromptSubmit, codexSID)
	c.wake(ev)
	c.clock.Add(20 * time.Second)
	c.rolloutAt(home, c.clock.Now(), taskComplete(c.clock.Now().Add(-time.Second), "", ""))
	c.h.setAgentStatus(turnJudge, herdr.StatusIdle)
	c.observe()
	if got := c.m.hookStateOf(j.ID); got != HookIdle {
		t.Fatalf("after the turn's end in the rollout: %q, want idle", got)
	}
}

// Each pass's readings count, per kind, as agreed or not; the counts reach
// the registry at most once a minute, by the hour, and ReadHookAgreement
// sums the last 24 hours for `magnum status`. A session the hooks said
// nothing of, and a herdr status that is unknown, count for nothing.
func TestThePassesTallyTheHookStatesAgreementPerKind(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.observe() // no hook state yet: nothing counts
	e.wake(e.hookEv(RoleClaude, agenthooks.NameSessionStart, claudeSID))
	e.wake(e.hookEv(RoleJudge, agenthooks.NameUserPromptSubmit, codexSID)) // herdr shows the judge idle
	for range 3 {
		e.clock.Add(10 * time.Second)
		e.observe()
	}
	e.h.setAgentStatus(claudeAgent, herdr.StatusUnknown)
	e.clock.Add(10 * time.Second)
	e.observe()
	e.clock.Add(time.Minute)
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.observe()
	got, err := ReadHookAgreement(e.ctx, e.st, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := []HookAgreement{{Kind: KindClaude, Agreed: 4}, {Kind: KindCodex, Differed: 5}}
	if !slices.Equal(got, want) {
		t.Fatalf("agreement = %+v, want %+v", got, want)
	}
	if got, _ := ReadHookAgreement(e.ctx, e.st, e.clock.Now().Add(25*time.Hour)); len(got) != 0 {
		t.Fatalf("a day later: %+v, want nothing", got)
	}
}
