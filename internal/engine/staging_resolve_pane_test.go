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

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// stagingPaneDeps are pane agent deps over a fakePaneAgents (made new for
// each manager the agent makes) on h's herdr and clock.
func stagingPaneDeps(t *testing.T, h *harness, ag **fakePaneAgents) paneDeps {
	t.Helper()
	return paneDeps{Config: h.cfg, Layout: h.layout, Logger: h.d.Logger, Herdr: h.hd, Store: h.st, Now: h.clock.Now, Hooks: h.e.agentHooks,
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

// cancelledResolver is the staging resolver's pane agent at work on its
// turn, as `magnum staging --cancel` finds it: prompted, its agent shown at
// work in herdr (through lh, lag snapshots behind an esc, in its own
// "staging merge" workspace w1, with a pane of yours beside it when foreign),
// Park and Quit refusing while herdr shows it so, and its Wait cut by the
// Cancel. A lag below 0 keeps it at work through the esc. It returns the
// agent, its manager and the herdr lh.
func cancelledResolver(t *testing.T, h *harness, lag int, foreign bool) (*paneStagingAgent, *fakePaneAgents, *laggingHerdr) {
	t.Helper()
	var ag *fakePaneAgents
	lh := newLaggingHerdr(h.hd, lag)
	d := stagingPaneDeps(t, h, &ag)
	d.Herdr = lh
	d.Keys = func(_ context.Context, _ string, keys ...string) error {
		if slices.Contains(keys, "esc") {
			lh.esc()
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
	return a.(*paneStagingAgent), ag, lh
}

// A Cancel of the resolver's job while its agent works ends with the agent
// parked and its scratch registry gone, though herdr still shows it at work
// a moment after its esc: Close waits for herdr to show it idle before it
// parks it, as a round's stop does (waitAgentsQuiet), instead of a Park and
// a Quit a few milliseconds after the esc that both find it working.
func TestTheStagingResolversCloseParksAnAgentHerdrStillShowsWorkingAfterTheEsc(t *testing.T) {
	h := newHarness(t)
	a, ag, _ := cancelledResolver(t, h, 2, false)
	if err := a.Close(context.WithoutCancel(h.ctx)); err != nil {
		t.Fatalf("Close = %v, want the agent parked", err)
	}
	if ag.parks() != 1 || ag.count("quit:") != 0 {
		t.Errorf("parked %d times, quit %d times; want one park", ag.parks(), ag.count("quit:"))
	}
	if _, err := os.Stat(filepath.Join(a.dir, learnScratch)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the scratch registry stays: %v", err)
	}
	if len(h.hd.closed) != 0 {
		t.Errorf("closed workspaces %v: a parked agent's workspace is Park's", h.hd.closed)
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
			a, _, _ := cancelledResolver(t, h, -1, tc.foreign)
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
