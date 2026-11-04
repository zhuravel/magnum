package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
	"github.com/zhuravel/magnum/internal/usage"
)

var t0 = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

// fakeHerdr is an in-memory herdr session: workspaces, panes and agents,
// the screen text of each pane, and a record of every call. Two ctrl+c to
// a pane end its agent, as they end a CLI.
type fakeHerdr struct {
	mu sync.Mutex

	workspaces []herdr.Workspace
	panes      []herdr.Pane
	agents     []herdr.AgentInfo
	nextWS     int
	nextPane   map[string]int
	ctrlC      map[string]int
	typed      map[string]string
	// screens is the text a read of a pane (by its id) returns.
	screens map[string]string
	// promptStatus is the status an agent shows once prompted.
	promptStatus herdr.Status

	// onCreate runs (without the lock) when a workspace is created;
	// onPrompt (with the lock held) inside every AgentPrompt; onRun (with
	// the lock held) after every pane run.
	onCreate func(o herdr.WorkspaceCreateOptions)
	onPrompt func(f *fakeHerdr, target, text string)
	onRun    func(f *fakeHerdr, pane, command string)
	snapErr  error
	// promptErr, when set, is what AgentPrompt answers for text, after it
	// recorded the prompt (herdr may have typed it); focused is the pane
	// the operator's terminal shows focused.
	promptErr func(text string) error
	focused   string

	creates []herdr.WorkspaceCreateOptions
	starts  []herdr.AgentStartOptions
	prompts []promptCall
	keys    []keysCall
	runs    []paneRunCall
}

type promptCall struct{ Target, Text string }

type keysCall struct {
	Target string
	Keys   []string
}

type paneRunCall struct{ Pane, Command string }

func newFakeHerdr() *fakeHerdr {
	return &fakeHerdr{nextPane: map[string]int{}, ctrlC: map[string]int{}, typed: map[string]string{}, screens: map[string]string{},
		promptStatus: herdr.StatusWorking}
}

func (f *fakeHerdr) newPaneLocked(ws, cwd string) herdr.Pane {
	f.nextPane[ws]++
	p := herdr.Pane{ID: fmt.Sprintf("%s:p%d", ws, f.nextPane[ws]), WorkspaceID: ws, TabID: ws + ":t1", Cwd: cwd}
	f.panes = append(f.panes, p)
	return p
}

func (f *fakeHerdr) agentLocked(target string) (int, bool) {
	for i, a := range f.agents {
		if a.Name == target || a.PaneID == target {
			return i, true
		}
	}
	return -1, false
}

func (f *fakeHerdr) Snapshot(context.Context) (herdr.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapErr != nil {
		return herdr.Snapshot{}, f.snapErr
	}
	s := herdr.Snapshot{Workspaces: slices.Clone(f.workspaces), Panes: slices.Clone(f.panes), Agents: slices.Clone(f.agents),
		FocusedPaneID: f.focused}
	for i := range s.Panes {
		s.Panes[i].Focused = s.Panes[i].ID == f.focused
		for _, a := range s.Agents {
			if a.PaneID == s.Panes[i].ID {
				s.Panes[i].Agent, s.Panes[i].AgentStatus, s.Panes[i].AgentSession = a.Agent, a.AgentStatus, a.AgentSession
			}
		}
	}
	return s, nil
}

func (f *fakeHerdr) WorkspaceCreate(_ context.Context, o herdr.WorkspaceCreateOptions) (herdr.WorkspaceCreated, error) {
	if f.onCreate != nil {
		f.onCreate(o)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, o)
	f.nextWS++
	id := fmt.Sprintf("w%d", f.nextWS)
	ws := herdr.Workspace{ID: id, Label: o.Label}
	f.workspaces = append(f.workspaces, ws)
	root := f.newPaneLocked(id, o.Cwd)
	return herdr.WorkspaceCreated{Workspace: ws, Tab: herdr.Tab{ID: id + ":t1", WorkspaceID: id}, RootPane: root}, nil
}

func (f *fakeHerdr) WorkspaceClose(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workspaces = slices.DeleteFunc(f.workspaces, func(w herdr.Workspace) bool { return w.ID == id })
	f.panes = slices.DeleteFunc(f.panes, func(p herdr.Pane) bool { return p.WorkspaceID == id })
	f.agents = slices.DeleteFunc(f.agents, func(a herdr.AgentInfo) bool { return a.WorkspaceID == id })
	return nil
}

func (f *fakeHerdr) PaneSplit(_ context.Context, paneID string, o herdr.SplitOptions) (herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.panes {
		if p.ID == paneID {
			return f.newPaneLocked(p.WorkspaceID, o.Cwd), nil
		}
	}
	return herdr.Pane{}, &herdr.Error{Method: "pane.split", Code: herdr.CodePaneNotFound, Message: paneID}
}

func (f *fakeHerdr) PaneRename(context.Context, string, string) error { return nil }

func (f *fakeHerdr) PaneRun(_ context.Context, paneID, command string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, paneRunCall{paneID, command})
	if f.onRun != nil {
		f.onRun(f, paneID, command)
	}
	return nil
}

func (f *fakeHerdr) PaneWaitOutput(_ context.Context, paneID string, o herdr.WaitOutputOptions) (herdr.OutputMatch, error) {
	return herdr.OutputMatch{PaneID: paneID, MatchedLine: o.Match}, nil
}

func (f *fakeHerdr) read(target string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, ok := f.agentLocked(target); ok {
		target = f.agents[i].PaneID
	}
	return f.screens[target]
}

func (f *fakeHerdr) PaneRead(_ context.Context, paneID string, o herdr.ReadOptions) (herdr.ReadResult, error) {
	return herdr.ReadResult{PaneID: paneID, Source: o.Source, Text: f.read(paneID)}, nil
}

func (f *fakeHerdr) AgentRead(_ context.Context, target string, o herdr.ReadOptions) (herdr.ReadResult, error) {
	return herdr.ReadResult{Source: o.Source, Text: f.read(target)}, nil
}

func (f *fakeHerdr) PaneGet(_ context.Context, paneID string) (herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.panes {
		if p.ID == paneID {
			if i, ok := f.agentLocked(paneID); ok {
				a := f.agents[i]
				p.Agent, p.AgentStatus, p.AgentSession = a.Agent, a.AgentStatus, a.AgentSession
			}
			return p, nil
		}
	}
	return herdr.Pane{}, &herdr.Error{Method: "pane.get", Code: herdr.CodePaneNotFound, Message: paneID}
}

// keysLocked records keys to target; two ctrl+c in a row, or /exit and
// enter, end its agent.
func (f *fakeHerdr) keysLocked(target string, keys []string) {
	f.keys = append(f.keys, keysCall{target, keys})
	pane := target
	if i, ok := f.agentLocked(target); ok {
		pane = f.agents[i].PaneID
	}
	end := func() {
		f.agents = slices.DeleteFunc(f.agents, func(a herdr.AgentInfo) bool { return a.PaneID == pane })
		f.ctrlC[pane] = 0
		f.typed[pane] = ""
	}
	for _, k := range keys {
		switch {
		case k == "ctrl+c":
			if f.ctrlC[pane]++; f.ctrlC[pane] >= 2 {
				end()
			}
			continue
		case k == "enter":
			if f.typed[pane] == "/exit" {
				end()
			}
			f.typed[pane] = ""
		case len(k) == 1:
			f.typed[pane] += k
		}
		f.ctrlC[pane] = 0
	}
}

func (f *fakeHerdr) PaneSendKeys(_ context.Context, paneID string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keysLocked(paneID, keys)
	return nil
}

func (f *fakeHerdr) AgentSendKeys(_ context.Context, target string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keysLocked(target, keys)
	return nil
}

func (f *fakeHerdr) PaneClose(_ context.Context, paneID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closePaneLocked(paneID)
	return nil
}

// closePaneLocked removes a pane with its agent, and its workspace with its
// last pane, as herdr does.
func (f *fakeHerdr) closePaneLocked(paneID string) {
	ws := ""
	for _, p := range f.panes {
		if p.ID == paneID {
			ws = p.WorkspaceID
		}
	}
	f.panes = slices.DeleteFunc(f.panes, func(p herdr.Pane) bool { return p.ID == paneID })
	f.agents = slices.DeleteFunc(f.agents, func(a herdr.AgentInfo) bool { return a.PaneID == paneID })
	if ws != "" && !slices.ContainsFunc(f.panes, func(p herdr.Pane) bool { return p.WorkspaceID == ws }) {
		f.workspaces = slices.DeleteFunc(f.workspaces, func(w herdr.Workspace) bool { return w.ID == ws })
	}
}

func (f *fakeHerdr) PaneProcessInfo(_ context.Context, paneID string) (herdr.ProcessInfo, error) {
	return herdr.ProcessInfo{PaneID: paneID, ShellPID: 10, ForegroundPGID: 10}, nil
}

func (f *fakeHerdr) WaitIdleShell(_ context.Context, paneID string, _ time.Duration) (herdr.ProcessInfo, error) {
	return herdr.ProcessInfo{PaneID: paneID, ShellPID: 10, ForegroundPGID: 10}, nil
}

func (f *fakeHerdr) AgentStart(_ context.Context, o herdr.AgentStartOptions) (herdr.AgentStarted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, o)
	ws, tab := "", ""
	for _, p := range f.panes {
		if p.ID == o.PaneID {
			ws, tab = p.WorkspaceID, p.TabID
		}
	}
	a := herdr.AgentInfo{PaneID: o.PaneID, WorkspaceID: ws, TabID: tab, Name: o.Name, Agent: o.Kind, AgentStatus: herdr.StatusIdle}
	if i := slices.Index(o.Args, "--resume"); i >= 0 && i+1 < len(o.Args) {
		a.AgentSession = &herdr.AgentSession{Kind: "id", Value: o.Args[i+1]}
	}
	if i := slices.Index(o.Args, "resume"); i >= 0 && i+1 < len(o.Args) {
		a.AgentSession = &herdr.AgentSession{Kind: "id", Value: o.Args[i+1]}
	}
	f.agents = slices.DeleteFunc(f.agents, func(x herdr.AgentInfo) bool { return x.PaneID == o.PaneID })
	f.agents = append(f.agents, a)
	if o.Kind == agents.KindCodex {
		f.screens[o.PaneID] = "OpenAI Codex\n\n› \n" // its composer, free: a first prompt goes out at once
	}
	return herdr.AgentStarted{Agent: a, Argv: append([]string{o.Kind}, o.Args...)}, nil
}

func (f *fakeHerdr) AgentPrompt(_ context.Context, target, text string, _ *herdr.PromptWait) (herdr.AgentInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, promptCall{target, text})
	if f.onPrompt != nil {
		f.onPrompt(f, target, text)
	}
	if f.promptErr != nil {
		if err := f.promptErr(text); err != nil {
			return herdr.AgentInfo{}, err
		}
	}
	if i, ok := f.agentLocked(target); ok {
		f.agents[i].AgentStatus = f.promptStatus
		return f.agents[i], nil
	}
	return herdr.AgentInfo{}, &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentNotFound, Message: target}
}

func (f *fakeHerdr) AgentRename(_ context.Context, target, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, ok := f.agentLocked(target); ok {
		f.agents[i].Name = name
	}
	return nil
}

// setStatus sets the status of the agent named name.
func (f *fakeHerdr) setStatus(name string, s herdr.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, ok := f.agentLocked(name); ok {
		f.agents[i].AgentStatus = s
	}
}

// setSession gives the agent named name a conversation id.
func (f *fakeHerdr) setSession(name, sid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, ok := f.agentLocked(name); ok {
		f.agents[i].AgentSession = &herdr.AgentSession{Kind: "id", Value: sid}
	}
}

// screen sets the text the pane of the agent named name shows.
func (f *fakeHerdr) screen(name, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, ok := f.agentLocked(name); ok {
		f.screens[f.agents[i].PaneID] = text
	}
}

// focus shows the pane of the agent named name focused in the operator's
// terminal ("" for none).
func (f *fakeHerdr) focus(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.focused = ""
	if i, ok := f.agentLocked(name); ok {
		f.focused = f.agents[i].PaneID
	}
}

// losePane closes the pane of the agent named name, as a crash of its
// terminal would.
func (f *fakeHerdr) losePane(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.agentLocked(name)
	if !ok {
		return
	}
	f.closePaneLocked(f.agents[i].PaneID)
}

func (f *fakeHerdr) promptTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.prompts {
		out = append(out, p.Text)
	}
	return out
}

func (f *fakeHerdr) startCalls() []herdr.AgentStartOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.starts)
}

func (f *fakeHerdr) keyCalls() []keysCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.keys)
}

func (f *fakeHerdr) runCalls() []paneRunCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.runs)
}

// rig is a runtime on fakes: a herdr session, the factory's agents store
// and registry, a fake clock every sleep moves, and scripted readings of
// memory, Codex's usage and Codex's login.
type rig struct {
	t     *testing.T
	ctx   context.Context
	clock *storetest.Clock
	h     *fakeHerdr
	st    *store.Store
	reg   *registry.Registry
	cfg   *config.Config
	run   *execx.Fake
	r     *Runtime

	mu         sync.Mutex
	memLevel   int
	memReads   int
	snap       usage.Snapshot
	usageReads int
	loginFP    string
	loggedOut  map[string]bool // kind -> its preflight says logged out
	slept      []time.Duration
	signals    []int

	layout               paths.Layout
	claudeDir, codexHome string
	copy                 string
	// copyEnv is the env of the copy the rig's launches claim (claimed).
	copyEnv map[string]string
	task    registry.Task
	attempt registry.Attempt
}

// newRig builds a runtime over fakes, with a bug task in its investigate
// stage and the stage's first attempt, and runs t in parallel.
func newRig(t *testing.T) *rig {
	t.Helper()
	storetest.Parallel(t)
	g := &rig{t: t, ctx: context.Background(), clock: storetest.NewClock(t0), h: newFakeHerdr(),
		memLevel: engine.MemoryNormal, loginFP: "acct-1", loggedOut: map[string]bool{}}
	g.st = storetest.Open(t, filepath.Join(t.TempDir(), "agents.db"))
	g.st.Clock = g.clock.Now
	g.reg = registrytest.OpenWith(t, registry.Options{Clock: g.clock.Now})
	g.layout = paths.Layout{Home: t.TempDir()}
	g.claudeDir, g.codexHome, g.copy = t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(g.codexHome, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	g.cfg = config.Defaults()
	g.cfg.Layout = g.layout
	g.cfg.Usage.CodexSoft, g.cfg.Usage.CodexHard = 90, 98
	// The repository's env is a template the copies render (Copies.Claim):
	// a pane gets the copy's, never this.
	g.cfg.Factory.Repos = []config.FactoryRepo{{Repo: "talkable/talkable", Env: map[string]string{"REDIS_DB_OFFSET": "{n*16}"}}}
	g.copyEnv = map[string]string{"RAILS_ENV": "development"}
	g.run = &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"zsh", "-ic"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			kind := strings.TrimPrefix(c.Args[1], "whence -w ")
			return execx.Result{Stdout: []byte(kind + ": command\n")}, nil
		}},
		{Prefix: []string{"claude", "auth", "status"}, Fn: func(execx.Cmd) (execx.Result, error) {
			return execx.Result{Stdout: fmt.Appendf(nil, `{"loggedIn": %v}`, !g.isLoggedOut(agents.KindClaude))}, nil
		}},
		{Prefix: []string{"codex", "login", "status"}, Fn: func(execx.Cmd) (execx.Result, error) {
			if g.isLoggedOut(agents.KindCodex) {
				return execx.Result{Stdout: []byte("Not logged in\n")}, nil
			}
			return execx.Result{Stdout: []byte("Logged in using ChatGPT\n")}, nil
		}},
		{Prefix: []string{"claude", "--version"}, Result: execx.Result{Stdout: []byte("2.1.400 (Claude Code)\n")}},
		{Prefix: []string{"codex", "--version"}, Result: execx.Result{Stdout: []byte("codex-cli 0.162.1\n")}},
	}}
	g.task = g.newTask("PR-12345")
	g.attempt = g.begin(g.task, "investigate")
	g.restart()
	return g
}

// restart builds the rig's runtime afresh over the same registry, agents
// store and herdr, as a daemon restart does: nothing it kept in memory
// survives.
func (g *rig) restart() {
	t := g.t
	t.Helper()
	r, err := newWith(Deps{Config: g.cfg, Layout: g.layout, Herdr: g.h, Store: g.st, Registry: g.reg, Runner: g.run,
		Memory: g.memory, Clock: g.clock.Now}, seams{
		sleep: func(ctx context.Context, d time.Duration) error {
			g.mu.Lock()
			g.slept = append(g.slept, d)
			g.mu.Unlock()
			g.clock.Add(d)
			return ctx.Err()
		},
		usage: func(context.Context, time.Time) (usage.Snapshot, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.usageReads++
			return g.snap, nil
		},
		login: func() (usage.Login, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			return usage.Login{Fingerprint: g.loginFP}, nil
		},
		signal: func(pid int, _ syscall.Signal) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.signals = append(g.signals, pid)
			return nil
		},
		claudeDir: g.claudeDir, codexHome: g.codexHome,
	})
	if err != nil {
		t.Fatalf("newWith: %v", err)
	}
	g.r = r
}

func (g *rig) isLoggedOut(kind string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.loggedOut[kind]
}

func (g *rig) memory(context.Context) engine.MemorySample {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.memReads++
	return engine.MemorySample{Level: g.memLevel, Pressure: engine.PressureName(g.memLevel)}
}

func (g *rig) setMemory(level int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.memLevel = level
}

func (g *rig) setUsage(s usage.Snapshot) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.snap = s
}

func (g *rig) setLogin(fp string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loginFP = fp
}

func (g *rig) reads() (mem, use int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.memReads, g.usageReads
}

func (g *rig) sleeps() []time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.slept)
}

// newTask creates an active bug task of talkable/talkable.
func (g *rig) newTask(key string) registry.Task {
	g.t.Helper()
	t, err := g.reg.CreateTask(g.ctx, registry.Task{JiraKey: key, Kind: "bug", Repo: "talkable/talkable",
		State: registry.TaskActive, Stage: "investigate"})
	if err != nil {
		g.t.Fatalf("CreateTask: %v", err)
	}
	return t
}

// begin records a launching attempt of task's stage.
func (g *rig) begin(t registry.Task, stage string) registry.Attempt {
	g.t.Helper()
	a, err := g.reg.BeginAttempt(g.ctx, t.ID, stage)
	if err != nil {
		g.t.Fatalf("BeginAttempt: %v", err)
	}
	return a
}

// role is the factory role named name.
func (g *rig) role(name string) config.Role {
	g.t.Helper()
	r, ok := g.cfg.FactoryRole(name)
	if !ok {
		g.t.Fatalf("no factory role %q", name)
	}
	return r
}

// roles are the factory roles named names.
func (g *rig) roles(names ...string) []config.Role {
	out := make([]config.Role, 0, len(names))
	for _, n := range names {
		out = append(out, g.role(n))
	}
	return out
}

// launch launches the roles named names for the rig's task and attempt.
func (g *rig) launch(names ...string) {
	g.t.Helper()
	if err := g.r.Launch(g.ctx, g.task, g.attempt, g.claimed(), g.roles(names...)); err != nil {
		g.t.Fatalf("Launch %v: %v", names, err)
	}
}

// claimed is the copy the rig's task claimed: copy 1 at g.copy, with
// g.copyEnv.
func (g *rig) claimed() Copy {
	return Copy{Name: "factory1", Path: g.copy, Number: 1, Env: g.copyEnv}
}

// send sends text to the role named name of the rig's task.
func (g *rig) send(name, text string) string {
	g.t.Helper()
	id, err := g.r.Send(g.ctx, g.task, g.attempt, g.role(name), text)
	if err != nil {
		g.t.Fatalf("Send %s: %v", name, err)
	}
	return id
}

// agent is the herdr name of the rig's task's agent of the role named name.
func (g *rig) agent(name string) string {
	return agents.TaggedAgentName(Tag, "factory/talkable", int(g.task.ID), agents.Role(name))
}

// observe runs one observe pass 10 seconds after the last.
func (g *rig) observe() []Event {
	g.t.Helper()
	g.clock.Add(10 * time.Second)
	evs, err := g.r.Observe(g.ctx)
	if err != nil {
		g.t.Fatalf("Observe: %v", err)
	}
	return evs
}

// observeN runs n observe passes and returns their events.
func (g *rig) observeN(n int) []Event {
	g.t.Helper()
	var out []Event
	for range n {
		out = append(out, g.observe()...)
	}
	return out
}

// watchdog runs the watchdog at the clock's time.
func (g *rig) watchdog() []Event {
	g.t.Helper()
	evs, err := g.r.Watchdog(g.ctx, g.clock.Now())
	if err != nil {
		g.t.Fatalf("Watchdog: %v", err)
	}
	return evs
}

// owner is the task's owner row.
func (g *rig) owner() store.PR {
	g.t.Helper()
	pr, err := g.r.Owner(g.ctx, g.task)
	if err != nil {
		g.t.Fatalf("Owner: %v", err)
	}
	return pr
}

// session is the live session of the role named name.
func (g *rig) session(name string) store.Session {
	g.t.Helper()
	s, err := g.st.LiveSessionByPRRole(g.ctx, g.owner().ID, name)
	if err != nil {
		g.t.Fatalf("LiveSessionByPRRole %s: %v", name, err)
	}
	return s
}

// storeRun is the agents store's run id.
func (g *rig) storeRun(id string) store.Run {
	g.t.Helper()
	r, err := g.st.RunByID(g.ctx, id)
	if err != nil {
		g.t.Fatalf("RunByID %s: %v", id, err)
	}
	return r
}

// ledger is the task's prompt ledger.
func (g *rig) ledger() []registry.LedgerEntry {
	g.t.Helper()
	es, err := g.reg.LedgerOf(g.ctx, g.task.ID)
	if err != nil {
		g.t.Fatalf("LedgerOf: %v", err)
	}
	return es
}

// roleRun is the recorded run of the role named name in the rig's attempt.
func (g *rig) roleRun(name string) registry.RoleRun {
	g.t.Helper()
	runs, err := g.reg.RoleRuns(g.ctx, g.attempt.ID)
	if err != nil {
		g.t.Fatalf("RoleRuns: %v", err)
	}
	for _, rr := range runs {
		if rr.Role == name {
			return rr
		}
	}
	g.t.Fatalf("no role run of %s in %s: %+v", name, g.attempt.ID, runs)
	return registry.RoleRun{}
}

// transcriptLine is one line of a Claude Code transcript.
type transcriptLine struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Operation string    `json:"operation,omitempty"`
	Content   any       `json:"content,omitempty"`
	Message   *struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"message,omitempty"`
	TurnOrigin string `json:"turnOrigin,omitempty"`
}

// userLine is a transcript line of a prompt typed at at.
func userLine(at time.Time, text string) transcriptLine {
	l := transcriptLine{Type: "user", Timestamp: at, TurnOrigin: "human"}
	l.Message = &struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}{"user", text}
	return l
}

// assistantLine is a transcript line of the agent's reply at at.
func assistantLine(at time.Time, text string) transcriptLine {
	l := transcriptLine{Type: "assistant", Timestamp: at}
	l.Message = &struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}{"assistant", []map[string]any{{"type": "text", "text": text}}}
	return l
}

// writeTranscript writes the Claude transcript of conversation sid.
func (g *rig) writeTranscript(sid string, lines ...transcriptLine) {
	g.t.Helper()
	dir := filepath.Join(g.claudeDir, "projects", "copy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		g.t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		data, err := json.Marshal(l)
		if err != nil {
			g.t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(b.String()), 0o600); err != nil {
		g.t.Fatal(err)
	}
}

// metaRollout writes the rollout of Codex session id, started at started in
// the rig's copy, as Codex files it: by its start in local time, its first
// line the session_meta.
func (g *rig) metaRollout(id string, started time.Time) {
	g.t.Helper()
	local := started.Local()
	dir := filepath.Join(g.codexHome, "sessions", local.Format("2006"), local.Format("01"), local.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		g.t.Fatal(err)
	}
	stamp := started.UTC().Format("2006-01-02T15:04:05.000Z")
	line, err := json.Marshal(map[string]any{"timestamp": stamp, "type": "session_meta", "payload": map[string]any{
		"id": id, "session_id": id, "timestamp": stamp, "cwd": g.copy, "originator": "codex_cli_rs", "source": "cli"}})
	if err != nil {
		g.t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+local.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		g.t.Fatal(err)
	}
}

// only returns the events of kind k.
func only(evs []Event, k EventKind) []Event {
	var out []Event
	for _, e := range evs {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}
