package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// stagingPaneDeps are pane agent deps over a fakePaneAgents (made new for
// each manager the agent makes) on h's herdr and clock.
func stagingPaneDeps(t *testing.T, h *harness, ag **fakePaneAgents) paneDeps {
	t.Helper()
	return paneDeps{Config: h.cfg, Layout: h.layout, Logger: h.d.Logger, Herdr: h.hd, Store: h.st, Now: h.clock.Now, Hooks: h.e.agentHooks, OpenScratch: openScratch,
		Sleep:        func(ctx context.Context, _ time.Duration) error { return sleepCtx(ctx, time.Millisecond) },
		ObserveEvery: time.Millisecond, Poll: time.Millisecond, CloseWorkspace: h.hd.WorkspaceClose,
		Keys: func(context.Context, string, ...string) error { return nil },
		Agents: func(st *store.Store, cfg *config.Config, _ paths.Layout) paneAgents {
			if len(cfg.Roles) != 1 || cfg.Roles[0].Identity != "" || cfg.Roles[0].Fix {
				t.Errorf("the manager's roles = %v, want the resolver's role alone, with no identity", cfg.Roles)
			}
			*ag = &fakePaneAgents{fakeAgents: &fakeAgents{st: st}}
			return *ag
		}}
}

// The resolver's pane env marks the pane as an agent's, so the commands that
// act for the operator refuse to run there (the CLI's refuseInAgentPane),
// and leaves git no way to push or fetch as the operator whatever the
// pane's shell sets again from its rc files (an SSH agent's socket, an ssh
// command): git's ssh transport is /usr/bin/false, no credential helper
// answers, not even the checkout's own, and the push URL of a GitHub
// remote, ssh or https, is a path that does not exist.
func TestTheStagingResolversPaneEnvMarksItAndLeavesGitNoWayToPush(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env := resolvePaneEnv(filepath.Join(dir, "job"), map[string]string{"WT_BRANCH": "review1"})
	if env[resolvePaneMarker] == "" {
		t.Errorf("the pane env has no %s: the operator's commands would run there", resolvePaneMarker)
	}
	ran := filepath.Join(dir, "ssh-ran")
	sshCmd := filepath.Join(dir, "ssh.sh")
	if err := os.WriteFile(sshCmd, []byte("#!/bin/sh\ntouch "+ran+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The shell's rc files set their own ssh command; the pane's env wins.
	run := &execx.Real{BaseEnv: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_SSH_COMMAND=" + sshCmd, "LC_ALL=C"}}
	git := func(stdin string, args ...string) (string, error) {
		res, err := run.Run(t.Context(), execx.Cmd{Name: "git", Args: append([]string{"-C", filepath.Join(dir, "repo")}, args...), Env: env,
			Stdin: []byte(stdin), Probe: true})
		return res.Out(), err
	}
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"remote", "add", "origin", "git@github.com:talkable/talkable.git"},
		{"remote", "add", "web", "https://github.com/talkable/talkable.git"}, {"remote", "add", "other", "ssh://git@github.com/talkable/talkable.git"},
		{"config", "credential.helper", "!f() { echo username=u; echo password=from-the-checkout; }; f"}} {
		if _, err := git("", args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	for _, remote := range []string{"origin", "web", "other"} {
		if url, err := git("", "remote", "get-url", "--push", remote); err != nil || !strings.HasPrefix(url, "/nonexistent/") {
			t.Errorf("%s's push URL in the pane = %q, %v; want a path that does not exist", remote, url, err)
		}
	}
	if out, err := git("protocol=https\nhost=github.com\n\n", "credential", "fill"); err == nil || strings.Contains(out, "from-the-checkout") {
		t.Errorf("git credential fill in the pane = %q, %v; want no helper to answer", out, err)
	}
	if _, err := git("", "ls-remote", "origin"); err == nil {
		t.Error("git ls-remote over ssh worked in the pane")
	}
	if _, err := os.Stat(ran); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the shell's own ssh command ran in the pane: %v", err)
	}
}

// The resolver's pane agent works in the job's slot, in the herdr workspace
// "staging merge", with the job's pane env (no GitHub token, the slot's
// own), names its pane and run, waits for the turn's end, and a fresh agent
// of the same job after a restart finds it while it still runs (its scratch
// registry stays in the job's directory) and not once it is closed.
func TestTheStagingResolversPaneAgentWorksInTheSlotWithTheJobsEnv(t *testing.T) {
	h := newHarness(t)
	var ag *fakePaneAgents
	mk := paneStagingAgents(stagingPaneDeps(t, h, &ag))
	role, err := h.e.resolveRole("talkable/talkable", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	dir, slot := filepath.Join(t.TempDir(), "job"), h.cfg.Pools[0].Path(1)
	job := stagingAgentJob{ID: "void-2-x.r1.0a0b0c0d", Dir: dir, Cwd: slot, Role: role, Env: resolvePaneEnv(dir, map[string]string{"WT_BRANCH": "review1"})}
	a, err := mk(job)
	if err != nil {
		t.Fatal(err)
	}
	at, err := a.Prompt(h.ctx, "resolve it")
	if err != nil {
		t.Fatal(err)
	}
	if at.Run == "" || at.Pane == "" || at.Name == "" {
		t.Errorf("Prompt = %+v, want the agent's name, pane and run", at)
	}
	if c := ag.all(); !slices.ContainsFunc(c, func(s string) bool {
		return strings.HasPrefix(s, "ensure_workspace:") && strings.Contains(s, ":"+slot+":"+stagingWorkspace+":review1:")
	}) {
		t.Errorf("calls %v; want the workspace %q in the slot, with the slot's env", c, stagingWorkspace)
	}
	if env := ag.paneEnv(role.Name); env["GH_TOKEN"] != "" || env["GH_CONFIG_DIR"] != filepath.Join(dir, "gh") || env["GIT_CONFIG_GLOBAL"] != "/dev/null" {
		t.Errorf("the pane's env = %v, want no GitHub token or git config of yours", env)
	}
	if timedOut, err := a.Wait(h.ctx, at.Run, h.clock.Now().Add(time.Minute)); err != nil || timedOut {
		t.Fatalf("Wait = %v, %v; want the turn's end", timedOut, err)
	}
	a.Detach()

	h.hd.mu.Lock()
	h.hd.agents = append(h.hd.agents, herdr.AgentInfo{PaneID: at.Pane, Name: at.Name, Agent: "claude", AgentStatus: herdr.StatusIdle})
	h.hd.mu.Unlock()
	again, err := mk(job)
	if err != nil {
		t.Fatal(err)
	}
	if alive, err := again.Resume(h.ctx); err != nil || !alive {
		t.Fatalf("Resume after a restart = %v, %v; want the agent found at work", alive, err)
	}
	if err := again.Close(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.hd.mu.Lock()
	h.hd.agents = nil
	h.hd.mu.Unlock()
	closed, err := mk(job)
	if err != nil {
		t.Fatal(err)
	}
	if alive, err := closed.Resume(h.ctx); err != nil || alive {
		t.Errorf("Resume of a closed agent = %v, %v; want it gone", alive, err)
	}
	if err := closed.Close(h.ctx); err != nil {
		t.Error(err)
	}
}

// laggingHerdr is a fakeHerdr seen through herdr's lag: the agent named
// name, at work, still shows working in the lag snapshots after its esc,
// then idle; a deaf one (a lag below 0) shows working through any key.
type laggingHerdr struct {
	*fakeHerdr
	mu   sync.Mutex
	name string
	lag  int
	left int // snapshots that still show it working; -1 before the esc
	deaf bool
}

func newLaggingHerdr(hd *fakeHerdr, lag int) *laggingHerdr {
	return &laggingHerdr{fakeHerdr: hd, lag: lag, left: -1, deaf: lag < 0}
}

func (l *laggingHerdr) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	l.mu.Lock()
	switch {
	case l.left == 0 && !l.deaf:
		l.fakeHerdr.mu.Lock()
		for i := range l.fakeHerdr.agents {
			if l.fakeHerdr.agents[i].Name == l.name {
				l.fakeHerdr.agents[i].AgentStatus = herdr.StatusIdle
			}
		}
		l.fakeHerdr.mu.Unlock()
	case l.left > 0:
		l.left--
	}
	l.mu.Unlock()
	return l.fakeHerdr.Snapshot(ctx)
}

// esc is the agent's esc: from now on it stops, lag snapshots later.
func (l *laggingHerdr) esc() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.left < 0 {
		l.left = l.lag
	}
}

// at puts the agent at at to work in its own workspace "staging merge" (ws),
// alone there unless foreign names another pane in it.
func (l *laggingHerdr) at(at stagingAgentAt, ws string, foreign bool) {
	l.mu.Lock()
	l.name = at.Name
	l.mu.Unlock()
	l.fakeHerdr.mu.Lock()
	defer l.fakeHerdr.mu.Unlock()
	l.workspaces = append(l.workspaces, herdr.Workspace{ID: ws, Label: stagingWorkspace})
	l.panes = append(l.panes, herdr.Pane{ID: at.Pane, WorkspaceID: ws})
	if foreign {
		l.panes = append(l.panes, herdr.Pane{ID: "p-yours", WorkspaceID: ws})
	}
	l.agents = append(l.agents, herdr.AgentInfo{Name: at.Name, PaneID: at.Pane, WorkspaceID: ws, Agent: "claude", AgentStatus: herdr.StatusWorking})
}

// shows reports whether a fresh snapshot shows the agent at work.
func (l *laggingHerdr) shows(ctx context.Context) bool {
	snap, err := l.Snapshot(ctx)
	if err != nil {
		return true
	}
	l.mu.Lock()
	name := l.name
	l.mu.Unlock()
	a, ok := snap.AgentByName(name)
	return ok && a.AgentStatus == herdr.StatusWorking
}

// sleepTally adds up the waits the agent's Sleeps ask for from its esc on.
type sleepTally struct {
	mu    sync.Mutex
	on    bool
	total time.Duration
}

func (s *sleepTally) arm() {
	s.mu.Lock()
	s.on = true
	s.mu.Unlock()
}

func (s *sleepTally) add(d time.Duration) {
	s.mu.Lock()
	if s.on {
		s.total += d
	}
	s.mu.Unlock()
}

func (s *sleepTally) sum() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// cancelled is the staging resolver's pane agent cancelledResolver made:
// its manager ag, the herdr lh and what it waited after its esc.
type cancelled struct {
	*paneStagingAgent
	ag    *fakePaneAgents
	lh    *laggingHerdr
	slept *sleepTally
}

// cancelledResolver is the staging resolver's pane agent at work on its
// turn, as `magnum staging --cancel` finds it: prompted, its agent shown at
// work in herdr (through lh, lag snapshots behind an esc, in its own
// "staging merge" workspace w1, with a pane of yours beside it when foreign),
// Park and Quit refusing while herdr shows it so, and its Wait cut by the
// Cancel. A lag below 0 keeps it at work through the esc.
func cancelledResolver(t *testing.T, h *harness, lag int, foreign bool) cancelled {
	t.Helper()
	var ag *fakePaneAgents
	lh := newLaggingHerdr(h.hd, lag)
	slept := &sleepTally{}
	d := stagingPaneDeps(t, h, &ag)
	d.Herdr = lh
	sleep := d.Sleep
	d.Sleep = func(ctx context.Context, wait time.Duration) error {
		slept.add(wait)
		return sleep(ctx, wait)
	}
	d.Keys = func(_ context.Context, _ string, keys ...string) error {
		if slices.Contains(keys, "esc") {
			lh.esc()
			slept.arm()
		}
		return nil
	}
	mk := d.Agents
	d.Agents = func(st *store.Store, cfg *config.Config, layout paths.Layout) paneAgents {
		m := mk(st, cfg, layout)
		ag.reply = func(int, string) bool { return false } // the turn never ends on its own
		return m
	}
	role, err := h.e.resolveRole("talkable/talkable", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "job")
	a, err := paneStagingAgents(d)(stagingAgentJob{ID: "void-2-x.r1.0a0b0c0d", Dir: dir, Cwd: h.cfg.Pools[0].Path(1), Role: role,
		Env: resolvePaneEnv(dir, nil)})
	if err != nil {
		t.Fatal(err)
	}
	at, err := a.Prompt(h.ctx, "resolve it")
	if err != nil {
		t.Fatal(err)
	}
	lh.at(at, "w1", foreign)
	ag.setBusy(lh.shows)
	ctx, cancel := context.WithCancel(h.ctx)
	time.AfterFunc(5*time.Millisecond, cancel) // the Cancel
	if _, err := a.Wait(ctx, at.Run, h.clock.Now().Add(time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want the Cancel's", err)
	}
	return cancelled{paneStagingAgent: a.(*paneStagingAgent), ag: ag, lh: lh, slept: slept}
}

// A Cancel of the resolver's job while its agent works ends with the agent
// parked and its scratch registry gone, though herdr still shows it at work
// for a while after its esc: Close waits for herdr to show it idle before
// it tries Park, as a round's stop does (waitAgentsQuiet), instead of a Park
// and a Quit a few milliseconds after the esc that both find it working. So
// one Park parks it, a moment after the esc as when herdr lags for longer
// than Park's tries (abortParkTries) would bridge, where the forced close
// would otherwise end it; and when herdr shows it working through the whole
// wait (paneQuietWait, a check a second), Park is tried again while it
// reports the agent busy, and its second try parks it.
func TestTheStagingResolversCloseParksAnAgentHerdrStillShowsWorkingAfterTheEsc(t *testing.T) {
	storetest.Parallel(t)
	checks := int(paneQuietWait/time.Second) + 1 // the wait's snapshots
	for _, tc := range []struct {
		name  string
		lag   int // the snapshots that still show it working after its esc
		tries int // the Parks that parks it
	}{
		{name: "a moment", lag: 2, tries: 1},
		{name: "longer than Park's tries", lag: abortParkTries + 2, tries: 1},
		{name: "through the wait", lag: checks + 1, tries: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			c := cancelledResolver(t, h, tc.lag, false)
			if err := c.Close(context.WithoutCancel(h.ctx)); err != nil {
				t.Fatalf("Close = %v, want the agent parked", err)
			}
			if c.ag.parks() != 1 || c.ag.count("park:") != tc.tries || c.ag.count("quit:") != 0 {
				t.Errorf("parked %d times in %d tries, quit %d times; want it parked in %d, after herdr showed it idle",
					c.ag.parks(), c.ag.count("park:"), c.ag.count("quit:"), tc.tries)
			}
			if _, err := os.Stat(filepath.Join(c.dir, learnScratch)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the scratch registry stays: %v", err)
			}
			if len(h.hd.closed) != 0 {
				t.Errorf("closed workspaces %v: a parked agent's workspace is Park's", h.hd.closed)
			}
		})
	}
}

// An agent that stays at work through its esc, the wait and Quit is ended
// by the resolver's Close closing its own "staging merge" workspace (the
// job's slot goes back right after: nothing may still write there), and
// only that: with a pane of yours in the workspace, it stays, and Close says
// why first, before where the registry stays; a pane agent that never
// forces (the retro, the curator, the critic, the editor) leaves even its
// own workspace to you.
func TestTheStagingResolversCloseEndsAnAgentThatStaysAtWorkByClosingItsOwnWorkspace(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name           string
		foreign, never bool
		closes         bool
	}{
		{name: "its own workspace", closes: true},
		{name: "a pane of yours beside it", foreign: true},
		{name: "a pane agent that never forces", never: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			a := cancelledResolver(t, h, -1, tc.foreign)
			if tc.never {
				a.spec.closeBusy = false
			}
			scratch := filepath.Join(a.dir, learnScratch)
			err := a.Close(context.WithoutCancel(h.ctx))
			_, serr := os.Stat(scratch)
			if !tc.closes {
				msg := fmt.Sprint(err)
				cause, at := strings.Index(msg, "claude-fix is working"), strings.Index(msg, scratch)
				if err == nil || cause < 0 || at < 0 || cause > at || !strings.Contains(msg, stagingWorkspace) {
					t.Fatalf("Close = %v; want why the agent stays (it works), then its workspace and where its registry stays", err)
				}
				if len(h.hd.closed) != 0 || serr != nil {
					t.Fatalf("closed %v, registry %v; want the workspace open and the registry kept", h.hd.closed, serr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Close = %v, want the agent ended with its workspace", err)
			}
			if !slices.Equal(h.hd.closed, []string{"w1"}) || !errors.Is(serr, os.ErrNotExist) {
				t.Fatalf("closed %v, registry %v; want its workspace w1 closed and the registry gone", h.hd.closed, serr)
			}
		})
	}
}

// A pane agent's Close of an agent that stays at work through its esc
// ends within the wait of a daemon stop, which closes a retro, a curator,
// a critic or an editor mid-turn that way (Engine.shutdown waits
// shutdownWait for them): its wait for herdr to show the agent idle and
// its Park's tries, with a Quit's own ctrl+c, 1 s, ctrl+c and wait for a
// shell (agents.QuitIdleTimeout) after them, take less than shutdownWait,
// so the stop does not leave while the Close still ends the agent.
func TestAPaneAgentsCloseOfAnAgentThatStaysAtWorkFitsInTheDaemonStopsWait(t *testing.T) {
	h := newHarness(t)
	c := cancelledResolver(t, h, -1, false)
	c.spec.closeBusy = false // a pane agent that never forces, as the retro's
	if err := c.Close(context.WithoutCancel(h.ctx)); err == nil {
		t.Fatal("Close = nil, want the agent that stays at work reported")
	}
	if c.ag.count("park:") != abortParkTries || c.ag.count("quit:") != 1 {
		t.Fatalf("%d Parks, %d Quits; want Park's tries, then a Quit", c.ag.count("park:"), c.ag.count("quit:"))
	}
	quit := time.Second + agents.QuitIdleTimeout // agents.Manager.Quit: ctrl+c, 1 s, ctrl+c, then a wait for an idle shell
	if waited := c.slept.sum(); waited+quit >= shutdownWait {
		t.Errorf("Close waited %v before its Quit, %v with the Quit's own; want less than the daemon stop's %v",
			waited, waited+quit, shutdownWait)
	}
}

// The resolver's agent is named after its deploy, not as PR #1 of a learn
// run: its scratch row carries the deploy ("talkable#12204 void"), which the
// manager every pane agent gets (learnAgentDeps) makes its pane's label and
// its title (claude --name), while its herdr name stays the one tagged name
// every job shares (TaggedAgentName of LearnAgentTag, magnum/staging, #1),
// by which a later job's clearLeftover finds an agent an earlier one left,
// and its workspace stays "staging merge". Its deps keep the daemon's
// scratch opener (OpenScratch nil: store.Open migrates the new file), the
// one pane test that does: the others seed theirs (openScratch).
func TestTheStagingResolversAgentIsNamedAfterItsDeploy(t *testing.T) {
	h := newHarness(t)
	eh := newEvalHerdr()
	cli := t.TempDir()
	a := &app.App{Logger: h.d.Logger, Runner: &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"claude", "auth", "status"}, Result: execx.Result{Stdout: []byte(`{"loggedIn": true}`)}},
		{Prefix: []string{"zsh", "-ic", "whence -w claude"}, Result: execx.Result{Stdout: []byte("claude: function\n")}},
	}}}
	d := paneDeps{Config: h.cfg, Layout: h.layout, Logger: h.d.Logger, Herdr: eh, Now: h.clock.Now,
		Sleep:        func(ctx context.Context, _ time.Duration) error { return sleepCtx(ctx, time.Millisecond) },
		ObserveEvery: time.Millisecond, Poll: time.Millisecond, CloseWorkspace: eh.WorkspaceClose, Keys: eh.AgentSendKeys,
		Agents: func(st *store.Store, cfg *config.Config, layout paths.Layout) paneAgents {
			ad := learnAgentDeps(eh, a, st, cfg, layout)
			ad.Clock = h.clock.Now
			ad.CodexConfig, ad.ClaudeConfig, ad.ClaudeSettings = filepath.Join(cli, "codex.toml"), filepath.Join(cli, "claude.json"),
				filepath.Join(cli, "settings.json")
			ad.ClaudeDir, ad.CodexHome = cli, cli
			ad.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
			return agents.New(ad)
		}}
	role, err := h.e.resolveRole("talkable/talkable", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "job")
	sa, err := paneStagingAgents(d)(stagingAgentJob{ID: "void-12204-x.r1.0a0b0c0d", Dir: dir, Cwd: t.TempDir(), Role: role,
		Env: resolvePaneEnv(dir, nil), Title: "talkable#12204 void"})
	if err != nil {
		t.Fatal(err)
	}
	ag := sa.(*paneStagingAgent)
	if err := ag.setup(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.ensureAgent(h.ctx); err != nil {
		t.Fatal(err)
	}
	eh.mu.Lock()
	starts, panes := slices.Clone(eh.starts), slices.Clone(eh.panes)
	eh.mu.Unlock()
	name := agents.TaggedAgentName(LearnAgentTag, learnRepoOwner+"/staging", learnNumber, agents.Role(role.Name))
	label := "learn talkable#12204 void " + role.Name
	if len(starts) != 1 || starts[0].Name != name || !slices.Contains(starts[0].Args, label+" - staging") {
		t.Fatalf("started %+v; want one agent named %s, titled %q", starts, name, label+" - staging")
	}
	i := slices.IndexFunc(panes, func(p herdr.Pane) bool { return p.ID == starts[0].PaneID })
	if i < 0 || panes[i].Label != label || eh.labelOf(panes[i].WorkspaceID) != stagingWorkspace {
		t.Fatalf("panes %+v; want the agent's pane labelled %q in the workspace %q", panes, label, stagingWorkspace)
	}
	if err := ag.Close(h.ctx); err != nil {
		t.Fatal(err)
	}
}

// An agent an earlier job left behind carries the same tagged name as the
// next job's, whatever deploy either is named after: the next job's start
// ends it first, closing its own "staging merge" workspace, and starts its
// own agent once.
func TestTheStagingResolverClearsTheAgentAnEarlierJobLeft(t *testing.T) {
	h := newHarness(t)
	var ag *fakePaneAgents
	role, err := h.e.resolveRole("talkable/talkable", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	old := agents.TaggedAgentName(LearnAgentTag, learnRepoOwner+"/staging", learnNumber, agents.Role(role.Name))
	h.hd.mu.Lock()
	h.hd.workspaces = append(h.hd.workspaces, herdr.Workspace{ID: "w-old", Label: stagingWorkspace})
	h.hd.panes = append(h.hd.panes, herdr.Pane{ID: "p-old", WorkspaceID: "w-old"})
	h.hd.agents = append(h.hd.agents, herdr.AgentInfo{Name: old, PaneID: "p-old", WorkspaceID: "w-old", Agent: "claude", AgentStatus: herdr.StatusIdle})
	h.hd.mu.Unlock()
	dir := filepath.Join(t.TempDir(), "job")
	a, err := paneStagingAgents(stagingPaneDeps(t, h, &ag))(stagingAgentJob{ID: "bart-12204-y.r1.0a0b0c0e", Dir: dir,
		Cwd: h.cfg.Pools[0].Path(1), Role: role, Env: resolvePaneEnv(dir, nil), Title: "talkable#12204 bart"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Prompt(h.ctx, "resolve it"); err != nil {
		t.Fatal(err)
	}
	h.hd.mu.Lock()
	closed, left := slices.Clone(h.hd.closed), slices.ContainsFunc(h.hd.agents, func(x herdr.AgentInfo) bool { return x.Name == old })
	h.hd.mu.Unlock()
	if !slices.Contains(closed, "w-old") || left || ag.count("start:") != 1 {
		t.Fatalf("closed %v, the old agent left %v, starts %d; want its workspace closed and one start", closed, left, ag.count("start:"))
	}
	if err := a.Close(h.ctx); err != nil {
		t.Fatal(err)
	}
}
