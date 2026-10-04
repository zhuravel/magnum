package agents

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// testClock is a settable clock shared by the store and the manager.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// fakeHerdr is an in-memory herdr: workspaces, panes and agents, plus a
// record of every call. errs injects an error per method name (persistent
// until deleted).
type fakeHerdr struct {
	mu sync.Mutex

	workspaces []herdr.Workspace
	panes      []herdr.Pane
	agents     []herdr.AgentInfo
	nextWS     int
	nextPane   map[string]int

	errs map[string]error
	// promptStatus is the status AgentPrompt reports after submission.
	promptStatus herdr.Status
	// busy marks panes whose foreground process is not the shell.
	busy map[string]bool
	// reads maps a pane id or agent name to the text a read returns.
	reads map[string]string
	// startStatus is the status AgentStart gives the new agent (default
	// idle); startErr is returned after the agent was registered (herdr's
	// agent_not_ready keeps a blocked agent addressable).
	startStatus herdr.Status
	startErr    error
	// busyStarts makes that many AgentStart calls fail with agent_pane_busy
	// before one succeeds (the idle-shell race StartAgent retries through).
	busyStarts int
	// onKeys runs (with f.mu held) after every send-keys call.
	onKeys func(f *fakeHerdr, target string, keys []string)
	// onRun runs (with f.mu held) after every pane run.
	onRun func(f *fakeHerdr, pane, command string)
	// waitLine is the line PaneWaitOutput reports as matched ("" = the
	// pattern itself).
	waitLine string
	// onPrompt runs (with f.mu held) inside every AgentPrompt call.
	onPrompt func()

	calls    []string
	creates  []herdr.WorkspaceCreateOptions
	splits   []splitCall
	renames  map[string]string
	starts   []herdr.AgentStartOptions
	prompts  []promptCall
	runs     []paneRunCall
	waits    []herdr.WaitOutputOptions
	keys     []keysCall
	closed   []string
	agentRen []string
}

type splitCall struct {
	From string
	Opts herdr.SplitOptions
	New  string
}

type promptCall struct {
	Target, Text string
	Wait         *herdr.PromptWait
}

type paneRunCall struct{ Pane, Command string }

type keysCall struct {
	Target string
	Keys   []string
}

func newFakeHerdr() *fakeHerdr {
	return &fakeHerdr{
		nextPane:     map[string]int{},
		errs:         map[string]error{},
		busy:         map[string]bool{},
		reads:        map[string]string{},
		renames:      map[string]string{},
		promptStatus: herdr.StatusWorking,
	}
}

func (f *fakeHerdr) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeHerdr) err(method string) error { return f.errs[method] }

func (f *fakeHerdr) callsWith(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeHerdr) newPaneLocked(ws, cwd string) herdr.Pane {
	f.nextPane[ws]++
	p := herdr.Pane{ID: fmt.Sprintf("%s:p%d", ws, f.nextPane[ws]), WorkspaceID: ws, TabID: ws + ":t1", Cwd: cwd, AgentStatus: herdr.StatusUnknown}
	f.panes = append(f.panes, p)
	return p
}

// addWorkspace registers a workspace with one pane (for tests that
// pre-populate herdr state).
func (f *fakeHerdr) addWorkspace(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workspaces = append(f.workspaces, herdr.Workspace{ID: id})
}

func (f *fakeHerdr) addPane(p herdr.Pane) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panes = append(f.panes, p)
}

func (f *fakeHerdr) addAgent(a herdr.AgentInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents = append(f.agents, a)
}

func (f *fakeHerdr) setAgentStatus(name string, s herdr.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.agents {
		if f.agents[i].Name == name {
			f.agents[i].AgentStatus = s
		}
	}
}

func (f *fakeHerdr) setAgentSession(name, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.agents {
		if f.agents[i].Name == name {
			f.agents[i].AgentSession = &herdr.AgentSession{Kind: "id", Value: value}
		}
	}
}

func (f *fakeHerdr) removeAgent(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.agents[:0]
	for _, a := range f.agents {
		if a.Name != name {
			out = append(out, a)
		}
	}
	f.agents = out
}

func (f *fakeHerdr) removePane(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var panes []herdr.Pane
	for _, p := range f.panes {
		if p.ID != id {
			panes = append(panes, p)
		}
	}
	f.panes = panes
	var agents []herdr.AgentInfo
	for _, a := range f.agents {
		if a.PaneID != id {
			agents = append(agents, a)
		}
	}
	f.agents = agents
}

func (f *fakeHerdr) removeWorkspaceLocked(id string) {
	var ws []herdr.Workspace
	for _, w := range f.workspaces {
		if w.ID != id {
			ws = append(ws, w)
		}
	}
	f.workspaces = ws
	var panes []herdr.Pane
	for _, p := range f.panes {
		if p.WorkspaceID != id {
			panes = append(panes, p)
		}
	}
	f.panes = panes
	var agents []herdr.AgentInfo
	for _, a := range f.agents {
		if a.WorkspaceID != id {
			agents = append(agents, a)
		}
	}
	f.agents = agents
}

func (f *fakeHerdr) removeWorkspace(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeWorkspaceLocked(id)
}

func (f *fakeHerdr) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Snapshot")
	if err := f.err("Snapshot"); err != nil {
		return herdr.Snapshot{}, err
	}
	s := herdr.Snapshot{
		Workspaces: append([]herdr.Workspace(nil), f.workspaces...),
		Panes:      append([]herdr.Pane(nil), f.panes...),
		Agents:     append([]herdr.AgentInfo(nil), f.agents...),
	}
	// Panes mirror the agent session of the agent running in them.
	for i := range s.Panes {
		for _, a := range s.Agents {
			if a.PaneID == s.Panes[i].ID {
				s.Panes[i].Agent = a.Agent
				s.Panes[i].AgentStatus = a.AgentStatus
				s.Panes[i].AgentSession = a.AgentSession
			}
		}
	}
	return s, nil
}

func (f *fakeHerdr) WorkspaceCreate(ctx context.Context, o herdr.WorkspaceCreateOptions) (herdr.WorkspaceCreated, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("WorkspaceCreate %s %s", o.Cwd, o.Label)
	f.creates = append(f.creates, o)
	if err := f.err("WorkspaceCreate"); err != nil {
		return herdr.WorkspaceCreated{}, err
	}
	f.nextWS++
	id := fmt.Sprintf("w%d", f.nextWS)
	ws := herdr.Workspace{ID: id, Label: o.Label}
	f.workspaces = append(f.workspaces, ws)
	root := f.newPaneLocked(id, o.Cwd)
	return herdr.WorkspaceCreated{Workspace: ws, Tab: herdr.Tab{ID: id + ":t1", WorkspaceID: id}, RootPane: root}, nil
}

func (f *fakeHerdr) WorkspaceClose(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("WorkspaceClose %s", id)
	f.closed = append(f.closed, id)
	if err := f.err("WorkspaceClose"); err != nil {
		return err
	}
	f.removeWorkspaceLocked(id)
	return nil
}

func (f *fakeHerdr) PaneSplit(ctx context.Context, paneID string, o herdr.SplitOptions) (herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("PaneSplit %s %s", paneID, o.Direction)
	if err := f.err("PaneSplit"); err != nil {
		return herdr.Pane{}, err
	}
	ws := ""
	for _, p := range f.panes {
		if p.ID == paneID {
			ws = p.WorkspaceID
		}
	}
	if ws == "" {
		return herdr.Pane{}, &herdr.Error{Method: "pane.split", Code: herdr.CodePaneNotFound, Message: paneID}
	}
	p := f.newPaneLocked(ws, o.Cwd)
	f.splits = append(f.splits, splitCall{From: paneID, Opts: o, New: p.ID})
	return p, nil
}

func (f *fakeHerdr) PaneRename(ctx context.Context, paneID, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("PaneRename %s %s", paneID, label)
	f.renames[paneID] = label
	return f.err("PaneRename")
}

func (f *fakeHerdr) PaneRun(ctx context.Context, paneID, command string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("PaneRun %s", paneID)
	f.runs = append(f.runs, paneRunCall{paneID, command})
	if err := f.err("PaneRun"); err != nil {
		return err
	}
	if f.onRun != nil {
		f.onRun(f, paneID, command)
	}
	return nil
}

func (f *fakeHerdr) PaneWaitOutput(ctx context.Context, paneID string, o herdr.WaitOutputOptions) (herdr.OutputMatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("PaneWaitOutput %s", paneID)
	f.waits = append(f.waits, o)
	if err := f.err("PaneWaitOutput"); err != nil {
		return herdr.OutputMatch{}, err
	}
	line := o.Match
	if f.waitLine != "" {
		line = f.waitLine
	}
	return herdr.OutputMatch{PaneID: paneID, MatchedLine: line}, nil
}

func (f *fakeHerdr) PaneRead(ctx context.Context, paneID string, o herdr.ReadOptions) (herdr.ReadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("PaneRead %s %s %d", paneID, o.Source, o.Lines)
	if err := f.err("PaneRead"); err != nil {
		return herdr.ReadResult{}, err
	}
	return herdr.ReadResult{PaneID: paneID, Source: o.Source, Text: f.reads[paneID]}, nil
}

func (f *fakeHerdr) PaneGet(ctx context.Context, paneID string) (herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("PaneGet %s", paneID)
	if err := f.err("PaneGet"); err != nil {
		return herdr.Pane{}, err
	}
	for _, p := range f.panes {
		if p.ID == paneID {
			for _, a := range f.agents {
				if a.PaneID == paneID {
					p.Agent, p.AgentStatus, p.AgentSession = a.Agent, a.AgentStatus, a.AgentSession
				}
			}
			return p, nil
		}
	}
	return herdr.Pane{}, &herdr.Error{Method: "pane.get", Code: herdr.CodePaneNotFound, Message: paneID}
}

func (f *fakeHerdr) PaneSendKeys(ctx context.Context, paneID string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("PaneSendKeys %s %s", paneID, strings.Join(keys, ","))
	f.keys = append(f.keys, keysCall{paneID, keys})
	if err := f.err("PaneSendKeys"); err != nil {
		return err
	}
	if f.onKeys != nil {
		f.onKeys(f, paneID, keys)
	}
	return nil
}

func (f *fakeHerdr) PaneProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("PaneProcessInfo %s", paneID)
	if err := f.err("PaneProcessInfo"); err != nil {
		return herdr.ProcessInfo{}, err
	}
	if f.busy[paneID] {
		return herdr.ProcessInfo{PaneID: paneID, ShellPID: 10, ForegroundPGID: 20}, nil
	}
	return herdr.ProcessInfo{PaneID: paneID, ShellPID: 10, ForegroundPGID: 10}, nil
}

func (f *fakeHerdr) WaitIdleShell(ctx context.Context, paneID string, timeout time.Duration) (herdr.ProcessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("WaitIdleShell %s", paneID)
	if err := f.err("WaitIdleShell"); err != nil {
		return herdr.ProcessInfo{}, err
	}
	if f.busy[paneID] {
		return herdr.ProcessInfo{PaneID: paneID, ShellPID: 10, ForegroundPGID: 20},
			fmt.Errorf("herdr pane %s: shell not idle: %w", paneID, herdr.ErrTimeout)
	}
	return herdr.ProcessInfo{PaneID: paneID, ShellPID: 10, ForegroundPGID: 10}, nil
}

func (f *fakeHerdr) AgentStart(ctx context.Context, o herdr.AgentStartOptions) (herdr.AgentStarted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("AgentStart %s %s %s %s", o.Name, o.Kind, o.PaneID, strings.Join(o.Args, " "))
	f.starts = append(f.starts, o)
	if err := f.err("AgentStart"); err != nil {
		return herdr.AgentStarted{}, err
	}
	if f.busyStarts > 0 {
		f.busyStarts--
		return herdr.AgentStarted{}, &herdr.Error{Method: "agent.start", Code: "agent_pane_busy", Message: "agent target pane " + o.PaneID + " is not an available shell"}
	}
	ws, tab := "", ""
	for _, p := range f.panes {
		if p.ID == o.PaneID {
			ws, tab = p.WorkspaceID, p.TabID
		}
	}
	status := f.startStatus
	if status == "" {
		status = herdr.StatusIdle
	}
	a := herdr.AgentInfo{PaneID: o.PaneID, WorkspaceID: ws, TabID: tab, Name: o.Name, Agent: o.Kind, AgentStatus: status}
	f.agents = append(f.agents, a)
	if f.startErr != nil {
		return herdr.AgentStarted{}, f.startErr
	}
	return herdr.AgentStarted{Agent: a, Argv: append([]string{o.Kind}, o.Args...)}, nil
}

func (f *fakeHerdr) AgentPrompt(ctx context.Context, target, text string, wait *herdr.PromptWait) (herdr.AgentInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("AgentPrompt %s", target)
	f.prompts = append(f.prompts, promptCall{target, text, wait})
	if f.onPrompt != nil {
		f.onPrompt()
	}
	if err := f.err("AgentPrompt"); err != nil {
		return herdr.AgentInfo{}, err
	}
	for i := range f.agents {
		if f.agents[i].Name == target || f.agents[i].PaneID == target {
			f.agents[i].AgentStatus = f.promptStatus
			return f.agents[i], nil
		}
	}
	return herdr.AgentInfo{}, &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentNotFound, Message: target}
}

func (f *fakeHerdr) AgentRead(ctx context.Context, target string, o herdr.ReadOptions) (herdr.ReadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("AgentRead %s %s %d", target, o.Source, o.Lines)
	if err := f.err("AgentRead"); err != nil {
		return herdr.ReadResult{}, err
	}
	return herdr.ReadResult{Source: o.Source, Text: f.reads[target]}, nil
}

func (f *fakeHerdr) AgentRename(ctx context.Context, target, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("AgentRename %s %s", target, name)
	f.agentRen = append(f.agentRen, target+"="+name)
	if err := f.err("AgentRename"); err != nil {
		return err
	}
	for i := range f.agents {
		if f.agents[i].Name == target || f.agents[i].PaneID == target {
			f.agents[i].Name = name
		}
	}
	return nil
}

func (f *fakeHerdr) AgentSendKeys(ctx context.Context, target string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("AgentSendKeys %s %s", target, strings.Join(keys, ","))
	f.keys = append(f.keys, keysCall{target, keys})
	if err := f.err("AgentSendKeys"); err != nil {
		return err
	}
	if f.onKeys != nil {
		f.onKeys(f, target, keys)
	}
	return nil
}

// env bundles everything a manager test needs.
type env struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	clock *testClock
	h     *fakeHerdr
	run   *execx.Fake
	cfg   *config.Config
	m     *Manager
	repo  store.Repo
	pr    store.PR
	slept []time.Duration

	// codexConfig and claudeConfig are the temp CLI configs EnsureTrust
	// edits (claudeConfig does not exist until a test writes it).
	codexConfig, claudeConfig string
	logs                      *logSink
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state", "magnum.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &testClock{now: t0}
	st.Clock = clk.Now

	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_1", Owner: "talkable", Name: "talkable", WatchOwner: "talkable",
		DefaultBranch: "master", Mode: store.RepoModePool})
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	up, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "PR_11920", Number: 11920,
		URL: "https://github.com/talkable/talkable/pull/11920", HeadSHA: "abc1234def5678abc1234def5678abc1234def56",
		Title: store.Ptr("Ignored title"), GHState: store.GHOpen, InitialState: store.PRReviewing, Identity: "talkable-app"})
	if err != nil {
		t.Fatalf("UpsertPRFromGitHub: %v", err)
	}

	cfg := config.Defaults()
	cfg.Identities = []config.Identity{
		{Name: "talkable-app", Kind: "app", Login: "talkable[bot]", NoFindingsEvent: "COMMENT"},
		{Name: "zhuravel", Kind: "gh", Login: "zhuravel", NoFindingsEvent: "APPROVE"},
	}
	layout := paths.Layout{Home: t.TempDir()}
	cfg.Layout = layout

	h := newFakeHerdr()
	run := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"zsh", "-ic", "whence -w codex"}, Result: execx.Result{Stdout: []byte("codex: function\n")}},
		{Prefix: []string{"zsh", "-ic", "whence -w claude"}, Result: execx.Result{Stdout: []byte("claude: function\n")}},
	}}
	cliDir := t.TempDir()
	e := &env{t: t, ctx: ctx, st: st, clock: clk, h: h, run: run, cfg: cfg, repo: repo, pr: up.PR,
		codexConfig: filepath.Join(cliDir, "codex", "config.toml"), claudeConfig: filepath.Join(cliDir, "claude.json"), logs: &logSink{}}
	e.m = New(Deps{Herdr: h, Store: st, Runner: run, Config: cfg, Layout: layout, Clock: clk.Now,
		CodexConfig: e.codexConfig, ClaudeConfig: e.claudeConfig, Log: e.logs})
	e.m.sleep = func(ctx context.Context, d time.Duration) error {
		e.slept = append(e.slept, d)
		return nil
	}
	return e
}

// logSink collects Deps.Log lines.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// reloadPR refreshes e.pr from the store.
func (e *env) reloadPR() store.PR {
	e.t.Helper()
	pr, err := e.st.PRByID(e.ctx, e.pr.ID)
	if err != nil {
		e.t.Fatalf("PRByID: %v", err)
	}
	e.pr = pr
	return pr
}

func (e *env) session(role Role) store.Session {
	e.t.Helper()
	s, err := e.st.LiveSessionByPRRole(e.ctx, e.pr.ID, string(role))
	if err != nil {
		e.t.Fatalf("LiveSessionByPRRole %s: %v", role, err)
	}
	return s
}

func (e *env) sessions() []store.Session {
	e.t.Helper()
	ss, err := e.st.SessionsByPR(e.ctx, e.pr.ID)
	if err != nil {
		e.t.Fatalf("SessionsByPR: %v", err)
	}
	return ss
}

func (e *env) run1(id string) store.Run {
	e.t.Helper()
	r, err := e.st.RunByID(e.ctx, id)
	if err != nil {
		e.t.Fatalf("RunByID: %v", err)
	}
	return r
}

// spec is the configured role named r.
func (e *env) spec(r Role) config.Role {
	e.t.Helper()
	role, ok := e.cfg.RoleByNameOrAlias(nil, string(r))
	if !ok {
		e.t.Fatalf("no configured role %q", r)
	}
	return role
}

// specs are the configured roles named rs, in that order.
func (e *env) specs(rs ...Role) []config.Role {
	e.t.Helper()
	out := make([]config.Role, len(rs))
	for i, r := range rs {
		out[i] = e.spec(r)
	}
	return out
}

// coreRoles are the roles with a pane in every classic workspace:
// codex-judge, claude-review and codex-review (claude-simplify on demand).
func (e *env) coreRoles() []config.Role { return e.specs(RoleJudge, RoleClaude, RoleCodexReview) }

// addRole appends a role to the configuration (normalized).
func (e *env) addRole(r config.Role) config.Role {
	e.t.Helper()
	e.cfg.Roles = append(e.cfg.Roles, r)
	e.cfg.Normalize()
	return e.spec(Role(r.Name))
}

// setKind edits the configured kind name in place.
func (e *env) setKind(name string, edit func(k *config.Kind)) {
	e.t.Helper()
	k, ok := e.cfg.Kinds[name]
	if !ok {
		e.t.Fatalf("no configured kind %q", name)
	}
	edit(&k)
	e.cfg.Kinds[name] = k
}

// workspace creates the standard three-pane workspace.
func (e *env) workspace() Workspace {
	e.t.Helper()
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/Users/x/Projects/talkable.review1",
		map[string]string{"WT_BRANCH": "review1", "MAGNUM_PR_URL": e.pr.URL}, "talkable#11920", e.coreRoles())
	if err != nil {
		e.t.Fatalf("EnsureWorkspace: %v", err)
	}
	return ws
}

// started creates the workspace and starts the judge and claude agents.
func (e *env) started() Workspace {
	e.t.Helper()
	ws := e.workspace()
	for _, r := range []Role{RoleJudge, RoleClaude} {
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(r), ws.Panes[r], ""); err != nil {
			e.t.Fatalf("StartAgent %s: %v", r, err)
		}
	}
	return ws
}

func sortedKeys(m map[Role]string) []string {
	var out []string
	for k := range m {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

// Compile-time check that the real client satisfies the interface.
var _ Herdr = (*herdr.Client)(nil)
