package engine

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// stagingPaneDeps are pane agent deps over a fakePaneAgents (made new for
// each manager the agent makes) on h's herdr and clock.
func stagingPaneDeps(t *testing.T, h *harness, ag **fakePaneAgents) paneDeps {
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
