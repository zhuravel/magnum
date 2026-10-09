package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// A per-PR worktree's panes get the environment its setup hooks ran with:
// WT_BRANCH=magnum-pr-<N>, the outranking workspace-name variables blanked
// and the [[repo]] env.
func TestPaneEnvPerPRWorktree(t *testing.T) {
	h := newHarness(t)
	h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "zhuravel/app", Env: map[string]string{"DATABASE_NAME": "app_{slug}", "MAIN": "{clone}"}})
	h.gh.set("zhuravel/app", prSpec{n: 1, head: "x1", updated: h.clock.Now()})
	h.startup()
	h.tick()
	h.gh.set("zhuravel/app", prSpec{n: 1, head: "x1", updated: h.clock.Now()}, prSpec{n: 2, head: "y1", updated: h.clock.Now()})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()

	repo, err := h.st.RepoByFullName(h.ctx, "zhuravel/app")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := h.st.PRByRepoNumber(h.ctx, repo.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	sl, err := h.st.SlotByPR(h.ctx, pr.ID)
	if err != nil {
		t.Fatalf("slot of #2: %v", err)
	}
	want := "ensure_workspace:" + itoa(pr.ID) + ":" + sl.Path + ":app#2:magnum-pr-2:"
	if !slices.ContainsFunc(h.ag.all(), func(c string) bool { return strings.HasPrefix(c, want) }) {
		t.Fatalf("agent calls %v lack %s", h.ag.all(), want)
	}

	job := &roundJob{pr: pr, repo: repo, watch: *h.cfg.WatchFor("zhuravel/app"), slot: sl, hasSlot: true}
	env, err := h.e.paneEnv(h.ctx, job, h.ids["zhuravel"], "y1")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"WT_BRANCH": "magnum-pr-2", "CONDUCTOR_WORKSPACE_NAME": "", "EMDASH_TASK_NAME": "", "SUPERSET_WORKSPACE_NAME": "",
		"COMMANDER_CONTEXT_NAME": "", "CLAUDE_CODE_WORKTREE_NAME": "", "WM_HANDLE": "",
		"DATABASE_NAME": "app_magnum-pr-2", "MAIN": sl.MainClone,
	} {
		if got, ok := env[k]; !ok || got != v {
			t.Errorf("env[%s] = %q (set %v), want %q", k, got, ok, v)
		}
	}

	// A pool slot keeps the pool's env and gets nothing per-PR.
	job.slot = store.Slot{Name: "review1", Kind: store.SlotKindPool}
	job.pool = h.cfg.PoolFor("talkable/talkable")
	env, err = h.e.paneEnv(h.ctx, job, h.ids["zhuravel"], "y1")
	if err != nil {
		t.Fatal(err)
	}
	if env["WT_BRANCH"] != "review1" || env["DATABASE_NAME"] != "" {
		t.Fatalf("pool env = %v", env)
	}
}
