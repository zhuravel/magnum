package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// evalHerdr is an in-memory herdr whole enough for a real agents.Manager to
// lay out a replay's workspace and start its agents there, and for the
// replay's close step: workspaces, panes and named agents (in the embedded
// fakeHerdr's lists, under its lock), keys and closes. An agent's status is
// the test's to set; ctrl+c or esc leaves a working agent idle unless it is
// deaf. Several replays share one evalHerdr, as they share the user's herdr.
type evalHerdr struct {
	*fakeHerdr

	nextWS   int
	nextPane map[string]int
	// starts are the agent.start calls, keys the keys sent ("<target>:<keys>"),
	// prompts the prompt targets.
	starts  []herdr.AgentStartOptions
	keys    []string
	prompts []string
	// deaf agents keep working through any key; closeErr fails every
	// WorkspaceClose (nothing closes then).
	deaf     map[string]bool
	closeErr error
}

var _ agents.Herdr = (*evalHerdr)(nil)

func newEvalHerdr() *evalHerdr {
	return &evalHerdr{fakeHerdr: &fakeHerdr{}, nextPane: map[string]int{}, deaf: map[string]bool{}}
}

// addWorkspace adds a workspace labelled label whose panes work in cwds.
func (h *evalHerdr) addWorkspace(label string, cwds ...string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextWS++
	id := fmt.Sprintf("w%d", h.nextWS)
	h.workspaces = append(h.workspaces, herdr.Workspace{ID: id, Label: label})
	for _, cwd := range cwds {
		h.newPaneLocked(id, cwd)
	}
	return id
}

// addAgent runs an agent named name of kind in pane, with status.
func (h *evalHerdr) addAgent(pane, name, kind string, status herdr.Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.panes {
		if p.ID == pane {
			h.agents = append(h.agents, herdr.AgentInfo{PaneID: p.ID, WorkspaceID: p.WorkspaceID, TabID: p.TabID, Name: name,
				Agent: kind, AgentStatus: status, AgentSession: &herdr.AgentSession{Kind: "id", Value: "conv-" + name}})
		}
	}
}

func (h *evalHerdr) newPaneLocked(ws, cwd string) herdr.Pane {
	h.nextPane[ws]++
	p := herdr.Pane{ID: fmt.Sprintf("%s:p%d", ws, h.nextPane[ws]), WorkspaceID: ws, TabID: ws + ":t1", Cwd: cwd}
	h.panes = append(h.panes, p)
	return p
}

// setStatus sets the status of the agents whose name holds the role.
func (h *evalHerdr) setStatus(role string, s herdr.Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.agents {
		if strings.Contains(h.agents[i].Name, "-"+role+"-") {
			h.agents[i].AgentStatus = s
		}
	}
}

// startNames are the names of the agents started so far.
func (h *evalHerdr) startNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, s := range h.starts {
		out = append(out, s.Name)
	}
	return out
}

func (h *evalHerdr) sent() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.keys)
}

func (h *evalHerdr) closedIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.closed)
}

func (h *evalHerdr) labelOf(ws string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, w := range h.workspaces {
		if w.ID == ws {
			return w.Label
		}
	}
	return ""
}

func (h *evalHerdr) has(ws string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.ContainsFunc(h.workspaces, func(w herdr.Workspace) bool { return w.ID == ws })
}

func (h *evalHerdr) WorkspaceClose(ctx context.Context, id string) error {
	h.mu.Lock()
	err := h.closeErr
	h.mu.Unlock()
	if err != nil {
		return err
	}
	return h.fakeHerdr.WorkspaceClose(ctx, id)
}

func (h *evalHerdr) WorkspaceCreate(_ context.Context, o herdr.WorkspaceCreateOptions) (herdr.WorkspaceCreated, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextWS++
	ws := herdr.Workspace{ID: fmt.Sprintf("w%d", h.nextWS), Label: o.Label}
	h.workspaces = append(h.workspaces, ws)
	root := h.newPaneLocked(ws.ID, o.Cwd)
	return herdr.WorkspaceCreated{Workspace: ws, Tab: herdr.Tab{ID: root.TabID, WorkspaceID: ws.ID}, RootPane: root}, nil
}

func (h *evalHerdr) paneLocked(id string) (herdr.Pane, bool) {
	for _, p := range h.panes {
		if p.ID == id {
			return p, true
		}
	}
	return herdr.Pane{}, false
}

func (h *evalHerdr) PaneSplit(_ context.Context, paneID string, o herdr.SplitOptions) (herdr.Pane, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	from, ok := h.paneLocked(paneID)
	if !ok {
		return herdr.Pane{}, &herdr.Error{Method: "pane.split", Code: herdr.CodePaneNotFound, Message: paneID}
	}
	return h.newPaneLocked(from.WorkspaceID, o.Cwd), nil
}

func (h *evalHerdr) PaneRename(_ context.Context, paneID, label string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.panes {
		if h.panes[i].ID == paneID {
			h.panes[i].Label = label
		}
	}
	return nil
}

func (h *evalHerdr) PaneRun(context.Context, string, string) error { return nil }

func (h *evalHerdr) PaneWaitOutput(_ context.Context, paneID string, o herdr.WaitOutputOptions) (herdr.OutputMatch, error) {
	return herdr.OutputMatch{PaneID: paneID, MatchedLine: o.Match}, nil
}

func (h *evalHerdr) PaneRead(_ context.Context, paneID string, o herdr.ReadOptions) (herdr.ReadResult, error) {
	return herdr.ReadResult{PaneID: paneID, Source: o.Source}, nil
}

func (h *evalHerdr) PaneGet(_ context.Context, paneID string) (herdr.Pane, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p, ok := h.paneLocked(paneID); ok {
		return p, nil
	}
	return herdr.Pane{}, &herdr.Error{Method: "pane.get", Code: herdr.CodePaneNotFound, Message: paneID}
}

func (h *evalHerdr) PaneSendKeys(_ context.Context, paneID string, keys ...string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keys = append(h.keys, paneID+":"+strings.Join(keys, "+"))
	return nil
}

func (h *evalHerdr) PaneProcessInfo(_ context.Context, paneID string) (herdr.ProcessInfo, error) {
	return herdr.ProcessInfo{PaneID: paneID, ShellPID: 10, ForegroundPGID: 10}, nil
}

func (h *evalHerdr) WaitIdleShell(_ context.Context, paneID string, _ time.Duration) (herdr.ProcessInfo, error) {
	return herdr.ProcessInfo{PaneID: paneID, ShellPID: 10, ForegroundPGID: 10}, nil
}

func (h *evalHerdr) AgentStart(_ context.Context, o herdr.AgentStartOptions) (herdr.AgentStarted, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.starts = append(h.starts, o)
	p, ok := h.paneLocked(o.PaneID)
	if !ok {
		return herdr.AgentStarted{}, &herdr.Error{Method: "agent.start", Code: herdr.CodePaneNotFound, Message: o.PaneID}
	}
	a := herdr.AgentInfo{PaneID: p.ID, WorkspaceID: p.WorkspaceID, TabID: p.TabID, Name: o.Name, Agent: o.Kind,
		AgentStatus: herdr.StatusIdle, AgentSession: &herdr.AgentSession{Kind: "id", Value: "conv-" + o.Name}}
	h.agents = append(h.agents, a)
	return herdr.AgentStarted{Agent: a}, nil
}

func (h *evalHerdr) AgentPrompt(_ context.Context, target, _ string, _ *herdr.PromptWait) (herdr.AgentInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prompts = append(h.prompts, target)
	return herdr.AgentInfo{}, &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentNotFound, Message: target}
}

func (h *evalHerdr) AgentRead(_ context.Context, _ string, o herdr.ReadOptions) (herdr.ReadResult, error) {
	return herdr.ReadResult{Source: o.Source}, nil
}

func (h *evalHerdr) AgentRename(_ context.Context, target, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.agents {
		if h.agents[i].Name == target || h.agents[i].PaneID == target {
			h.agents[i].Name = name
		}
	}
	return nil
}

func (h *evalHerdr) AgentSendKeys(_ context.Context, target string, keys ...string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keys = append(h.keys, target+":"+strings.Join(keys, "+"))
	for i := range h.agents {
		a := &h.agents[i]
		if (a.Name == target || a.PaneID == target) && !h.deaf[a.Name] && a.AgentStatus == herdr.StatusWorking {
			a.AgentStatus = herdr.StatusIdle
		}
	}
	return nil
}

// newEvalReplay is an engine for one replay of an eval run: a scratch
// registry of its own and a real agents.Manager under tag (the run's agent
// tag), over herdr h, which the replays of a test share. Its waits sleep a
// millisecond at most.
func newEvalReplay(t *testing.T, h *evalHerdr, tag string) *harness {
	t.Helper()
	return newHarness(t, func(hs *harness) {
		hs.layout = paths.Layout{Home: hs.layout.Home, Scratch: t.TempDir()}
		hs.d.Layout = hs.layout
		hs.d.Herdr = h
		run := &execx.Fake{Rules: []execx.Rule{
			{Prefix: []string{"codex", "login", "status"}, Result: execx.Result{Stdout: []byte("Logged in using ChatGPT\n")}},
			{Prefix: []string{"claude", "auth", "status"}, Result: execx.Result{Stdout: []byte(`{"loggedIn": true}`)}},
			{Prefix: []string{"zsh", "-ic", "whence -w codex"}, Result: execx.Result{Stdout: []byte("codex: function\n")}},
			{Prefix: []string{"zsh", "-ic", "whence -w claude"}, Result: execx.Result{Stdout: []byte("claude: function\n")}},
		}}
		cli := t.TempDir()
		hs.d.Agents = agents.New(agents.Deps{Herdr: h, Store: hs.st, Runner: run, Config: hs.cfg, Layout: hs.layout,
			Clock: hs.clock.Now, Tag: tag, CodexConfig: filepath.Join(cli, "codex.toml"), ClaudeConfig: filepath.Join(cli, "claude.json"),
			ClaudeSettings: filepath.Join(cli, "settings.json"), ClaudeDir: cli, CodexHome: cli,
			Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }})
		hs.d.Sleep = func(ctx context.Context, d time.Duration) error {
			tm := time.NewTimer(min(d, time.Millisecond))
			defer tm.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tm.C:
				return nil
			}
		}
	})
}

// liveSessions are the PR's live and starting sessions in h's registry.
func (hs *harness) liveSessions(pr store.PR) []store.Session {
	hs.t.Helper()
	ss, err := hs.st.SessionsByPR(hs.ctx, pr.ID)
	if err != nil {
		hs.t.Fatal(err)
	}
	return slices.DeleteFunc(ss, func(s store.Session) bool {
		return s.State != store.SessionLive && s.State != store.SessionStarting
	})
}
