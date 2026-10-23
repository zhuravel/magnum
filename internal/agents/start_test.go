package agents

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

func TestStartAgentFreshWrapperPassesOnlyExtras(t *testing.T) {
	e := newEnv(t)
	e.setKind(KindCodex, func(k *config.Kind) { k.Args = []string{"--should-not-appear"} })
	ws := e.started()

	if len(e.h.starts) != 2 {
		t.Fatalf("starts = %+v", e.h.starts)
	}
	j := e.h.starts[0]
	// The judge role's effort (xhigh) goes through the codex kind's effort
	// args, its max_subagents (2) through the subagents args.
	if j.Name != "mg-11920-codex-judge-5d01cf" || j.Kind != "codex" || j.PaneID != ws.Panes[RoleJudge] || j.Timeout != 120*time.Second ||
		!slices.Equal(j.Args, []string{"-c", "model_reasoning_effort=xhigh", "-c", "agents.max_concurrent_threads_per_session=2", "-c", "tool_output_token_limit=65536", "-c", "features.apps=false", "-c", "skills.include_instructions=false"}) {
		t.Fatalf("judge start = %+v", j)
	}
	c := e.h.starts[1]
	// Claude gets its pane title as one --name arg (herdr shell-quotes it)
	// and the role's effort as --effort, so the session never runs at the
	// operator's own Claude Code effortLevel; --strict-mcp-config keeps the
	// operator's MCP servers out of it, a wrapper's session too.
	if c.Name != "mg-11920-claude-review-5d01cf" || c.Kind != "claude" || c.PaneID != ws.Panes[RoleClaude] ||
		!slices.Equal(c.Args, []string{"--name", "PR #11920 claude-review - talkable", "--effort", "high", "--strict-mcp-config"}) {
		t.Fatalf("claude start = %+v", c)
	}
	if got := e.h.callsWith("WaitIdleShell"); len(got) != 2 {
		t.Fatalf("WaitIdleShell calls = %v", got)
	}
	for _, r := range []Role{RoleJudge, RoleClaude} {
		s := e.session(r)
		if s.State != store.SessionLive {
			t.Fatalf("%s state = %s, want live", r, s.State)
		}
	}
	// Probe each kind once, then cached.
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatal(err)
	}
	if got := store.Deref(e.session(RoleJudge).AgentKind); got != KindCodex {
		t.Fatalf("judge agent_kind = %q", got)
	}
	if n := len(e.run.CallsWithPrefix("zsh", "-ic", "whence -w codex")); n != 1 {
		t.Fatalf("codex wrapper probes = %d, want 1", n)
	}
	if n := len(e.run.CallsWithPrefix("zsh", "-ic", "whence -w claude")); n != 1 {
		t.Fatalf("claude wrapper probes = %d, want 1", n)
	}
	for _, c := range e.run.Calls {
		if c.Mutates {
			t.Fatalf("wrapper probe must not be Mutates: %s", c.String())
		}
	}
}

func TestStartAgentIdempotentWhenAgentRuns(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	n := len(e.h.starts)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatal(err)
	}
	if len(e.h.starts) != n {
		t.Fatalf("AgentStart called again for a running agent")
	}
}

func TestStartAgentResumeArgs(t *testing.T) {
	cases := []struct {
		name        string
		mode        string
		role        Role
		cfgArgs     []string
		want        string
		probeOutput string
	}{
		{"codex wrapper", "auto", RoleJudge, []string{"--x"}, "resume 01a0-uuid -c tui.resume_cwd=current -c model_reasoning_effort=xhigh -c agents.max_concurrent_threads_per_session=2 -c tool_output_token_limit=65536 -c features.apps=false -c skills.include_instructions=false", "codex: function"},
		{"claude wrapper", "true", RoleClaude, []string{"--x"}, "--resume c1d2-id --name PR #11920 claude-review - talkable --effort high --strict-mcp-config", ""},
		{"codex plain", "false", RoleJudge, []string{"--dangerously-bypass-approvals-and-sandbox", "--search"}, "resume 01a0-uuid -c tui.resume_cwd=current -c model_reasoning_effort=xhigh -c agents.max_concurrent_threads_per_session=2 -c tool_output_token_limit=65536 -c features.apps=false -c skills.include_instructions=false --dangerously-bypass-approvals-and-sandbox --search", ""},
		{"claude plain", "false", RoleClaude, []string{"--dangerously-skip-permissions"}, "--resume c1d2-id --name PR #11920 claude-review - talkable --effort high --strict-mcp-config --dangerously-skip-permissions", ""},
		{"codex no function", "auto", RoleJudge, []string{"--search"}, "resume 01a0-uuid -c tui.resume_cwd=current -c model_reasoning_effort=xhigh -c agents.max_concurrent_threads_per_session=2 -c tool_output_token_limit=65536 -c features.apps=false -c skills.include_instructions=false --search", "codex: command"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			for _, kind := range []string{KindCodex, KindClaude} {
				e.setKind(kind, func(k *config.Kind) { k.Wrapper, k.Args = tc.mode, tc.cfgArgs })
			}
			if tc.probeOutput != "" {
				e.run.Rules = append([]execx.Rule{{Prefix: []string{"zsh", "-ic", "whence -w codex"},
					Result: execx.Result{Stdout: []byte(tc.probeOutput + "\n")}}}, e.run.Rules...)
			}
			ws := e.workspace()
			resume := "01a0-uuid"
			if tc.role == RoleClaude {
				resume = "c1d2-id"
			}
			if err := e.m.StartAgent(e.ctx, e.pr, e.spec(tc.role), ws.Panes[tc.role], resume); err != nil {
				t.Fatal(err)
			}
			got := strings.Join(e.h.starts[0].Args, " ")
			if got != tc.want {
				t.Fatalf("args = %q, want %q", got, tc.want)
			}
			s := e.session(tc.role)
			if store.Deref(s.SessionID) != resume || store.Deref(s.ResumedFrom) != resume || s.State != store.SessionLive {
				t.Fatalf("session = %+v", s)
			}
			if tc.mode != "auto" && len(e.run.CallsWithPrefix("zsh")) != 0 {
				t.Fatalf("explicit wrapper must not probe: %v", e.run.Calls)
			}
		})
	}
}

func TestStartAgentAdoptsPaneHerdrRestored(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()
	// herdr restored the old judge (no name) in another pane of the checkout.
	e.h.addWorkspace("w9")
	e.h.addPane(herdr.Pane{ID: "w9:p1", WorkspaceID: "w9", TabID: "w9:t1", Cwd: "/Users/x/Projects/talkable.review1"})
	e.h.addAgent(herdr.AgentInfo{PaneID: "w9:p1", WorkspaceID: "w9", TabID: "w9:t1", Agent: "codex",
		AgentStatus: herdr.StatusIdle, AgentSession: &herdr.AgentSession{Kind: "id", Value: "01a0-uuid"}})

	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], "01a0-uuid"); err != nil {
		t.Fatal(err)
	}
	if len(e.h.starts) != 0 {
		t.Fatalf("AgentStart must not run when adopting: %+v", e.h.starts)
	}
	if len(e.h.agentRen) != 1 || e.h.agentRen[0] != "w9:p1=mg-11920-codex-judge-5d01cf" {
		t.Fatalf("agent renames = %v", e.h.agentRen)
	}
	s := e.session(RoleJudge)
	if store.Deref(s.HerdrPaneID) != "w9:p1" || store.Deref(s.HerdrWorkspaceID) != "w9" || s.State != store.SessionLive ||
		store.Deref(s.SessionID) != "01a0-uuid" {
		t.Fatalf("adopted session = %+v", s)
	}
	// The next round keeps the adopted pane instead of losing the session.
	again := e.workspace()
	if again.Created || again.Panes[RoleJudge] != "w9:p1" || e.session(RoleJudge).ID != s.ID {
		t.Fatalf("workspace after adoption = %+v", again)
	}
}

// A restored conversation is adopted only when its pane runs the role's kind
// in the PR's checkout; otherwise it is resumed in the role's own pane.
func TestStartAgentSkipsRestoredPaneElsewhere(t *testing.T) {
	for name, pane := range map[string]herdr.Pane{
		"other checkout": {ID: "w9:p1", WorkspaceID: "w9", Cwd: "/Users/x/Projects/talkable.review2"},
		"unknown cwd":    {ID: "w9:p1", WorkspaceID: "w9"},
		"other kind":     {ID: "w9:p1", WorkspaceID: "w9", Cwd: "/Users/x/Projects/talkable.review1", Agent: "claude"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ws := e.workspace()
			e.h.addWorkspace("w9")
			e.h.addPane(pane)
			kind := pane.Agent
			if kind == "" {
				kind = "codex"
			}
			e.h.addAgent(herdr.AgentInfo{PaneID: "w9:p1", WorkspaceID: "w9", Agent: kind, AgentStatus: herdr.StatusIdle,
				AgentSession: &herdr.AgentSession{Kind: "id", Value: "01a0-uuid"}})
			if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], "01a0-uuid"); err != nil {
				t.Fatal(err)
			}
			if len(e.h.agentRen) != 0 || len(e.h.starts) != 1 || e.h.starts[0].PaneID != ws.Panes[RoleJudge] {
				t.Fatalf("renames %v, starts %+v: want a resume in the judge's own pane", e.h.agentRen, e.h.starts)
			}
			if s := e.session(RoleJudge); store.Deref(s.HerdrPaneID) != ws.Panes[RoleJudge] {
				t.Fatalf("session = %+v", s)
			}
		})
	}
}

// An agent that already carries the role's name is adopted only when it runs
// the role's kind in the PR's checkout; otherwise the name is taken.
func TestStartAgentRefusesForeignNamedAgent(t *testing.T) {
	name := AgentName("talkable/talkable", 11920, RoleJudge)
	for label, tc := range map[string]struct {
		cwd, kind string
	}{
		"other checkout": {"/Users/x/Projects/other.review1", "codex"},
		"other kind":     {"/Users/x/Projects/talkable.review1", "claude"},
	} {
		t.Run(label, func(t *testing.T) {
			e := newEnv(t)
			ws := e.workspace()
			e.h.addWorkspace("w9")
			e.h.addPane(herdr.Pane{ID: "w9:p1", WorkspaceID: "w9", Cwd: tc.cwd})
			e.h.addAgent(herdr.AgentInfo{Name: name, PaneID: "w9:p1", WorkspaceID: "w9", Agent: tc.kind, AgentStatus: herdr.StatusIdle})
			err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], "")
			if err == nil || !strings.Contains(err.Error(), "name is taken") {
				t.Fatalf("err = %v, want the name taken", err)
			}
			if len(e.h.starts) != 0 {
				t.Fatalf("starts = %+v", e.h.starts)
			}
			if s := e.session(RoleJudge); s.State != store.SessionStarting {
				t.Fatalf("session = %+v, want still starting", s)
			}
		})
	}
	// Its own agent in a subdirectory of the checkout is adopted.
	e := newEnv(t)
	ws := e.workspace()
	e.h.addPane(herdr.Pane{ID: "w1:p9", WorkspaceID: "w1", Cwd: "/Users/x/Projects/talkable.review1/app"})
	e.h.addAgent(herdr.AgentInfo{Name: name, PaneID: "w1:p9", WorkspaceID: "w1", Agent: "codex", AgentStatus: herdr.StatusIdle})
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil || len(e.h.starts) != 0 {
		t.Fatalf("own agent: err %v, starts %+v", err, e.h.starts)
	}
}

// herdr may still list an agent Quit just stopped under the role's name: a
// fresh start (no conversation to resume) that adopted it lost the cold
// judge 0.5 s later. An agent whose conversation is one magnum parked for
// the PR and role is never adopted by a fresh start (ErrBusy: the round
// retries); one in another conversation still is.
func TestAFreshStartNeverAdoptsTheConversationMagnumParked(t *testing.T) {
	name := AgentName("talkable/talkable", 11920, RoleJudge)
	for label, tc := range map[string]struct {
		conversation string
		adopted      bool
	}{
		"the parked conversation": {"01a0-uuid", false},
		"another conversation":    {"02b1-uuid", true},
	} {
		t.Run(label, func(t *testing.T) {
			e := newEnv(t)
			if _, err := e.st.CreateSession(e.ctx, store.Session{PRID: e.pr.ID, Role: string(RoleJudge), AgentName: &name,
				AgentKind: new(KindCodex), SessionID: new("01a0-uuid"), State: store.SessionParked}); err != nil {
				t.Fatal(err)
			}
			ws := e.workspace()
			e.h.addPane(herdr.Pane{ID: "w1:p9", WorkspaceID: "w1", Cwd: "/Users/x/Projects/talkable.review1"})
			e.h.addAgent(herdr.AgentInfo{Name: name, PaneID: "w1:p9", WorkspaceID: "w1", Agent: "codex", AgentStatus: herdr.StatusIdle,
				AgentSession: &herdr.AgentSession{Kind: "id", Value: tc.conversation}})
			err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], "")
			if tc.adopted {
				if err != nil || len(e.h.starts) != 0 || store.Deref(e.session(RoleJudge).HerdrPaneID) != "w1:p9" {
					t.Fatalf("err %v, starts %+v: want the agent adopted", err, e.h.starts)
				}
				return
			}
			if !errors.Is(err, ErrBusy) || len(e.h.starts) != 0 {
				t.Fatalf("err %v, starts %+v: want ErrBusy and no start", err, e.h.starts)
			}
			if s := e.session(RoleJudge); s.State == store.SessionLive {
				t.Fatalf("session = %+v: the parked conversation's agent was adopted", s)
			}
		})
	}
}

func TestStartAgentErrors(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleCodexReview), ws.Panes[RoleCodexReview], ""); !errors.Is(err, ErrNotAgent) {
		t.Fatalf("codex-review start: err = %v, want ErrNotAgent", err)
	}
	e.h.busy[ws.Panes[RoleJudge]] = true
	err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], "")
	if !errors.Is(err, herdr.ErrTimeout) {
		t.Fatalf("busy pane: err = %v, want herdr.ErrTimeout", err)
	}
	if len(e.h.starts) != 0 || e.session(RoleJudge).State != store.SessionStarting {
		t.Fatal("busy pane must not start an agent")
	}
	e.h.busy[ws.Panes[RoleJudge]] = false
	e.h.errs["AgentStart"] = &herdr.Error{Method: "agent.start", Code: herdr.CodeTimeout, Message: "not ready"}
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); !errors.Is(err, ErrTimeout) {
		t.Fatalf("start timeout: err = %v, want ErrTimeout", err)
	}
	e.setKind(KindCodex, func(k *config.Kind) { k.Wrapper = "sometimes" })
	delete(e.h.errs, "AgentStart")
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err == nil {
		t.Fatal("invalid wrapper: want error")
	}
	unknown := e.spec(RoleClaude)
	unknown.Kind = "gemini"
	if err := e.m.StartAgent(e.ctx, e.pr, unknown, ws.Panes[RoleClaude], ""); err == nil || !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("undeclared kind: err = %v", err)
	}
}

func TestWrapperProbeFailureDefaultsToWrapperAndRetries(t *testing.T) {
	e := newEnv(t)
	e.run.Rules = []execx.Rule{{Prefix: []string{"zsh"}, Err: errors.New("zsh: boom")}}
	w, err := e.m.Wrapper(e.ctx, KindCodex)
	if err != nil || !w {
		t.Fatalf("Wrapper = %v, %v; want true (safe default) with no error", w, err)
	}
	e.m.Wrapper(e.ctx, KindCodex)
	if n := len(e.run.Calls); n != 2 {
		t.Fatalf("probe calls = %d, want 2 (failures are not cached)", n)
	}
}

// The wrapper probe runs zsh detached from any terminal (NoTTY), as the
// daemon does under launchd, so a CLI run in a terminal gets the same answer
// instead of an interactive zsh stopped on that terminal.
func TestWrapperProbeHasNoTerminal(t *testing.T) {
	e := newEnv(t)
	if _, err := e.m.Wrapper(e.ctx, KindCodex); err != nil {
		t.Fatal(err)
	}
	probes := e.run.CallsWithPrefix("zsh", "-ic")
	if len(probes) != 1 || !probes[0].NoTTY {
		t.Fatalf("probes = %+v", probes)
	}
}

func TestRecordSessionID(t *testing.T) {
	e := newEnv(t)
	e.started()
	s := e.session(RoleJudge)
	id, err := e.m.RecordSessionID(e.ctx, s)
	if err != nil || id != "" {
		t.Fatalf("before first prompt: %q, %v", id, err)
	}
	e.h.setAgentSession("mg-11920-codex-judge-5d01cf", "01a0fe66-c0de")
	id, err = e.m.RecordSessionID(e.ctx, s)
	if err != nil || id != "01a0fe66-c0de" {
		t.Fatalf("RecordSessionID = %q, %v", id, err)
	}
	if got := store.Deref(e.session(RoleJudge).SessionID); got != "01a0fe66-c0de" {
		t.Fatalf("stored session_id = %q", got)
	}
}

func TestStartAgentSimplifyGetsName(t *testing.T) {
	e := newEnv(t)
	ws, err := e.m.EnsurePane(e.ctx, e.pr, e.workspace(), "/Users/x/Projects/talkable.review1", nil, e.spec(RoleSimplify))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleSimplify), ws.Panes[RoleSimplify], ""); err != nil {
		t.Fatal(err)
	}
	if len(e.h.starts) != 1 || !slices.Equal(e.h.starts[0].Args, []string{"--name", "PR #11920 claude-simplify - talkable", "--strict-mcp-config"}) {
		t.Fatalf("simplify start = %+v", e.h.starts)
	}
}

// A Claude review session starts with none of the operator's MCP servers
// (the claude kind's mcp_off and mcp_strict, --strict-mcp-config with no
// --mcp-config), launched or resumed: they cost tokens, and they stayed in
// the pane's foreground after the agent was asked to exit, so the quit
// before a checkout failed. mcp_off = false starts it with them as before,
// and a server file added to mcp_strict is passed with it.
func TestClaudeStartsWithoutTheOperatorsMCPServers(t *testing.T) {
	for _, resume := range []string{"", "c1d2-id"} {
		e := newEnv(t)
		ws := e.workspace()
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], resume); err != nil {
			t.Fatal(err)
		}
		if got := e.h.starts[0].Args; !slices.Contains(got, "--strict-mcp-config") || slices.Contains(got, "--mcp-config") {
			t.Fatalf("resume %q: args = %q, want --strict-mcp-config alone", resume, got)
		}
	}

	e := newEnv(t)
	e.setKind(KindClaude, func(k *config.Kind) { k.MCPOff = false })
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), e.workspace().Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; slices.Contains(got, "--strict-mcp-config") {
		t.Fatalf("mcp_off = false: args = %q", got)
	}

	e = newEnv(t)
	file := "/Users/x/.config/magnum/claude-mcp.json"
	e.setKind(KindClaude, func(k *config.Kind) { k.MCPStrict = append(k.MCPStrict, "--mcp-config", file) })
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), e.workspace().Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; !slices.Equal(got[len(got)-3:], []string{"--strict-mcp-config", "--mcp-config", file}) {
		t.Fatalf("a server file: args = %q", got)
	}
}

// A pane herdr still reports busy right after the idle-shell check is retried
// with the same resume id instead of being handed back as a failure (which
// would make the engine start a fresh conversation).
func TestStartAgentRetriesPaneBusyKeepingResume(t *testing.T) {
	e := newEnv(t)
	e.h.busyStarts = 2
	ws := e.workspace()
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], "01a0-uuid"); err != nil {
		t.Fatal(err)
	}
	if len(e.h.starts) != 3 {
		t.Fatalf("starts = %d, want 3 (two busy, one ok)", len(e.h.starts))
	}
	for i, o := range e.h.starts {
		if !strings.Contains(strings.Join(o.Args, " "), "resume 01a0-uuid") {
			t.Fatalf("start %d lost the resume id: %v", i, o.Args)
		}
	}
	if s := e.session(RoleJudge); store.Deref(s.SessionID) != "01a0-uuid" || s.State != store.SessionLive {
		t.Fatalf("session = %+v", s)
	}
	e.h.busyStarts = startBusyRetries + 1
	err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], "")
	if err == nil || !herdr.IsCode(err, codeAgentPaneBusy) {
		t.Fatalf("err = %v, want agent_pane_busy after %d retries", err, startBusyRetries)
	}
}

// herdr refuses to type a launch line longer than the terminal's canonical
// line while the shell has not drawn its prompt yet (agent_start_input_failed:
// "wait for the shell prompt and retry", one setup failed on it on
// 2026-10-23): the same shell-startup race as agent_pane_busy, retried the
// same way, resume id kept, logged as such.
func TestStartAgentRetriesAStartInputTheShellWasNotReadyFor(t *testing.T) {
	e := newEnv(t)
	e.h.busyStarts = 2
	e.h.busyErr = &herdr.Error{Method: "agent.start", Code: "agent_start_input_failed",
		Message: "input contains a 1560-byte line but the terminal is in canonical mode with a 1024-byte limit; wait for the shell prompt and retry"}
	ws := e.workspace()
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], "01a0-uuid"); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if len(e.h.starts) != 3 || !strings.Contains(strings.Join(e.h.starts[2].Args, " "), "resume 01a0-uuid") {
		t.Fatalf("starts = %+v, want two refused and one with the resume id", e.h.starts)
	}
	if s := e.session(RoleJudge); s.State != store.SessionLive {
		t.Fatalf("session = %+v, want live", s)
	}
	if recs := e.logs.records("did not take its launch line (attempt 1/"); len(recs) != 1 {
		t.Fatalf("logs %q, want the first retry said", e.logs.all())
	}
	e.h.busyStarts = startBusyRetries + 1
	err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], "")
	if err == nil || !herdr.IsCode(err, codeAgentStartInputFailed) {
		t.Fatalf("err = %v, want agent_start_input_failed after %d retries", err, startBusyRetries)
	}
}

// A fresh start that cannot read the PR's sessions cannot tell whether the
// agent herdr lists carries a conversation magnum parked: it refuses
// (ErrBusy: the round retries) rather than adopt it.
func TestAFreshStartThatCannotReadTheSessionsAdoptsNothing(t *testing.T) {
	name := AgentName("talkable/talkable", 11920, RoleJudge)
	e := newEnv(t)
	parked, err := e.st.CreateSession(e.ctx, store.Session{PRID: e.pr.ID, Role: string(RoleJudge), AgentName: &name,
		AgentKind: new(KindCodex), SessionID: new("01a0-uuid"), State: store.SessionParked})
	if err != nil {
		t.Fatal(err)
	}
	ws := e.workspace()
	if _, err := e.st.DB().ExecContext(e.ctx, "UPDATE sessions SET started_at = 'garbage' WHERE id = ?", parked.ID); err != nil {
		t.Fatal(err)
	}
	e.h.addPane(herdr.Pane{ID: "w1:p9", WorkspaceID: "w1", Cwd: "/Users/x/Projects/talkable.review1"})
	e.h.addAgent(herdr.AgentInfo{Name: name, PaneID: "w1:p9", WorkspaceID: "w1", Agent: "codex", AgentStatus: herdr.StatusIdle,
		AgentSession: &herdr.AgentSession{Kind: "id", Value: "01a0-uuid"}})
	err = e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], "")
	if !errors.Is(err, ErrBusy) || len(e.h.starts) != 0 {
		t.Fatalf("err %v, starts %+v: want ErrBusy and no start", err, e.h.starts)
	}
	if s, serr := e.st.LiveSessionByPRRole(e.ctx, e.pr.ID, string(RoleJudge)); serr == nil && store.Deref(s.HerdrPaneID) == "w1:p9" && s.State == store.SessionLive {
		t.Fatalf("session = %+v: the agent was adopted", s)
	}
}

// A kind with wrapper = "bypass" gets the shell function or alias of its
// name removed from the pane's shell before the launch, so the binary runs
// with magnum's args alone, the kind's args included, and no probe runs;
// a kind in auto mode keeps its pane's shell as it is.
func TestBypassRemovesTheShellFunctionBeforeTheLaunch(t *testing.T) {
	e := newEnv(t)
	e.setKind(KindCodex, func(k *config.Kind) {
		k.Wrapper, k.Args = config.WrapperBypass, []string{"--dangerously-bypass-approvals-and-sandbox"}
	})
	ws := e.started()
	pane := ws.Panes[RoleJudge]
	const unset = `unset -f codex 2>/dev/null; unalias codex 2>/dev/null; echo bypass:""magnum-bypass-codex-`
	var lines []string
	for _, r := range e.h.runs {
		if r.Pane == pane && strings.HasPrefix(r.Command, "unset") {
			lines = append(lines, r.Command)
		}
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], unset) {
		t.Fatalf("judge pane removals = %q, want one starting %q", lines, unset)
	}
	// The line is cleared first, and the removal counts once the shell
	// echoed the marker (the typed line, with its split marker, never
	// matches).
	if !slices.ContainsFunc(e.h.keys, func(k keysCall) bool { return k.Target == pane && slices.Equal(k.Keys, []string{"ctrl+u"}) }) {
		t.Fatalf("keys = %+v, want ctrl+u on %s", e.h.keys, pane)
	}
	marker := strings.TrimPrefix(lines[0], `unset -f codex 2>/dev/null; unalias codex 2>/dev/null; echo bypass:""`)
	waits := slices.DeleteFunc(slices.Clone(e.h.waits), func(w herdr.WaitOutputOptions) bool { return !strings.Contains(w.Match, "bypass") })
	if len(waits) != 1 || !waits[0].Regex || waits[0].Timeout != bypassWait || waits[0].Source != herdr.SourceRecentUnwrapped ||
		waits[0].Match != `(?m)^bypass:`+regexp.QuoteMeta(marker)+`[ \t]*$` {
		t.Fatalf("waits = %+v", waits)
	}
	if re := regexp.MustCompile(waits[0].Match); re.MatchString("$ "+lines[0]) || !re.MatchString("bypass:"+marker+"\n") {
		t.Fatalf("the pattern %q must match the echo alone", waits[0].Match)
	}
	calls := e.h.callsWith("")
	run := slices.Index(calls, "PaneRun "+pane)
	start := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "AgentStart") })
	if run < 0 || start < 0 || run > start {
		t.Fatalf("the removal must precede the launch: calls = %q", calls)
	}
	j := e.h.starts[0]
	want := []string{"-c", "model_reasoning_effort=xhigh", "-c", "agents.max_concurrent_threads_per_session=2", "-c", "tool_output_token_limit=65536",
		"-c", "features.apps=false", "-c", "skills.include_instructions=false", "--dangerously-bypass-approvals-and-sandbox"}
	if j.Kind != "codex" || !slices.Equal(j.Args, want) {
		t.Fatalf("judge start = %+v, want args %q", j, want)
	}
	if n := len(e.run.CallsWithPrefix("zsh", "-ic", "whence -w codex")); n != 0 {
		t.Fatalf("codex wrapper probes = %d, want none", n)
	}
	for _, r := range e.h.runs {
		if r.Pane == ws.Panes[RoleClaude] && strings.HasPrefix(r.Command, "unset -f") {
			t.Fatalf("claude (wrapper auto) pane run = %q", r.Command)
		}
	}
}

// A shell that never echoes the removal back (the line typed during its
// startup is lost) gets it typed again, and after the last attempt the
// start fails without a launch.
func TestBypassFailsTheStartWhenTheShellNeverEchoes(t *testing.T) {
	e := newEnv(t)
	e.setKind(KindCodex, func(k *config.Kind) { k.Wrapper = config.WrapperBypass })
	e.h.errs["PaneWaitOutput"] = &herdr.Error{Method: "pane.wait_output", Code: herdr.CodeTimeout, Message: "timeout"}
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, slot1, nil, "talkable#11920", []config.Role{e.spec(RoleJudge)})
	if err != nil {
		t.Fatal(err)
	}
	err = e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], "")
	if err == nil || !strings.Contains(err.Error(), "bypass the codex wrapper in pane "+ws.Panes[RoleJudge]) || !strings.Contains(err.Error(), "attempt 3/3") {
		t.Fatalf("err = %v", err)
	}
	n := 0
	for _, r := range e.h.runs {
		if r.Pane == ws.Panes[RoleJudge] && strings.HasPrefix(r.Command, "unset -f codex") {
			n++
		}
	}
	if n != bypassAttempts || len(e.h.starts) != 0 {
		t.Fatalf("removal lines = %d, starts = %d", n, len(e.h.starts))
	}
}

// The whiteboard role's session gets the Whiteboard MCP server alone: its
// args carry the server as one inline JSON string, after the claude kind's
// --strict-mcp-config, which keeps every other server (the operator's own
// whiteboard plugin included) out. herdr shell-quotes each argument, so the
// string reaches the launch as one element, intact.
func TestTheWhiteboardRolesMCPServerReachesItsLaunchIntact(t *testing.T) {
	e := newEnv(t)
	role := e.spec(Role(config.RoleClaudeWhiteboard))
	if err := e.m.StartAgent(e.ctx, e.pr, role, "p-whiteboard", ""); err != nil {
		t.Fatal(err)
	}
	got := e.h.starts[0].Args
	const server = `{"mcpServers":{"whiteboard":{"command":"whiteboard","args":["mcp"]}}}`
	strict := slices.Index(got, "--strict-mcp-config")
	if strict < 0 || len(got) < 2 || !slices.Equal(got[len(got)-2:], []string{"--mcp-config", server}) || slices.Index(got, "--mcp-config") < strict {
		t.Fatalf("whiteboard args = %q, want --strict-mcp-config, then --mcp-config %s last", got, server)
	}
}
