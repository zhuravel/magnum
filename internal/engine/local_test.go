package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
)

// localRound is round n of the factory's local review of task 7's change in
// checkout: a case of the change's high synthetic number, its base and the
// description and ticket files the factory wrote.
func localRound(h *harness, checkout string, n int) LocalRound {
	c := evalCase(h, checkout, false)
	c.Number = 900000 + 7*10 + n
	c.URL = fmt.Sprintf("https://github.com/zhuravel/widgets/pull/%d", c.Number)
	scratch := filepath.Join(h.layout.Home, "local", fmt.Sprint(n))
	return LocalRound{EvalCase: c, BaseSHA: strings.Repeat("cd", 20),
		DescriptionFile: filepath.Join(scratch, "description.md"), TicketFile: filepath.Join(scratch, "ticket.md")}
}

// An engine on the live layout never runs a local round, as it never runs a
// replay: the factory's rounds cannot write magnum's registry, reports or
// notes.
func TestRunLocalRefusesALiveLayout(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.e.RunLocal(h.ctx, localRound(h, t.TempDir(), 1)); !errors.Is(err, ErrEvalLayout) {
		t.Fatalf("RunLocal on the live layout: %v", err)
	}
	if len(h.rd.inputs) != 0 {
		t.Fatalf("a round ran: %+v", h.rd.inputs)
	}
}

// A local round is a blind dry run that always carries the change's base:
// the round input's base is the factory's (whatever the checkout's base
// branch says), its Local names the factory's files, and the post_review
// line rendered from it runs with --dry-run and --local-base <base>, so
// post-review reads nothing from GitHub. A round without a base or a
// description file never starts.
func TestRunLocalAlwaysGivesPostReviewTheBase(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.layout = paths.Layout{Home: h.layout.Home, Scratch: t.TempDir()}
		h.d.Layout = h.layout
	})
	h.rd.script = func(pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun}, nil
	}
	for _, bad := range []func(*LocalRound){func(c *LocalRound) { c.BaseSHA = "" }, func(c *LocalRound) { c.BaseSHA = "master" },
		func(c *LocalRound) { c.DescriptionFile = "" }} {
		c := localRound(h, t.TempDir(), 1)
		bad(&c)
		if _, _, err := h.e.RunLocal(h.ctx, c); err == nil {
			t.Fatalf("RunLocal(%+v) ran", c)
		}
	}
	if len(h.rd.inputs) != 0 {
		t.Fatalf("a round without its base ran: %+v", h.rd.inputs)
	}
	c := localRound(h, t.TempDir(), 1)
	res, pr, err := h.e.RunLocal(h.ctx, c)
	if err != nil || res.Outcome != pipeline.OutcomeDryRun || pr.Number != c.Number {
		t.Fatalf("RunLocal: %+v, %+v, %v", res, pr, err)
	}
	if len(h.rd.inputs) != 1 {
		t.Fatalf("rounds: %d", len(h.rd.inputs))
	}
	in := h.rd.inputs[0]
	if !in.DryRun || !in.Blind || in.BaseSHA != c.BaseSHA || in.TargetSHA != c.Head || in.Local == nil ||
		in.Local.DescriptionFile != c.DescriptionFile || in.Local.TicketFile != c.TicketFile {
		t.Fatalf("round input: dry=%v blind=%v base=%s target=%s local=%+v", in.DryRun, in.Blind, in.BaseSHA, in.TargetSHA, in.Local)
	}
	line := agents.PostReviewLine(agents.JudgeData{Owner: "zhuravel", Repo: "widgets", Number: in.PR.Number, HeadSHA: in.TargetSHA,
		RunID: "r-1", ReviewerLogin: "zhuravel", DryRun: in.DryRun, Blind: in.Blind, BaseSHA: in.BaseSHA, ReviewFile: "/reviews/review.json"})
	if !strings.Contains(line, " --dry-run ") || !strings.Contains(line, " --local-base "+c.BaseSHA+" ") {
		t.Fatalf("post_review %q lacks --dry-run --local-base %s", line, c.BaseSHA)
	}
}

// A local round a restart cut runs again as the same round of the same
// task, so with the same synthetic number: its start closes the workspace
// the cut one left in the copy, by its label, before anything else runs
// there. Another round's workspace stays.
func TestALocalRoundRunAgainClosesTheWorkspaceTheCutOneLeft(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.layout = paths.Layout{Home: h.layout.Home, Scratch: t.TempDir()}
		h.d.Layout = h.layout
	})
	checkout := t.TempDir()
	c := localRound(h, checkout, 1)
	h.hd.workspaces = []herdr.Workspace{
		{ID: "w-cut", Label: fmt.Sprintf("eval widgets#%d", c.Number)},
		{ID: "w-next", Label: fmt.Sprintf("eval widgets#%d", c.Number+1)},
	}
	h.hd.panes = []herdr.Pane{
		{ID: "w-cut:p1", WorkspaceID: "w-cut", Cwd: checkout},
		{ID: "w-next:p1", WorkspaceID: "w-next", Cwd: checkout},
	}
	var closedAtRound []string
	h.rd.script = func(pipeline.RoundInput) (pipeline.RoundResult, error) {
		h.hd.mu.Lock()
		closedAtRound = slices.Clone(h.hd.closed)
		h.hd.mu.Unlock()
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun}, nil
	}
	if _, _, err := h.e.RunLocal(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(closedAtRound, []string{"w-cut"}) {
		t.Fatalf("closed %v before the round ran, want the cut round's w-cut only", closedAtRound)
	}
}

// A local round's panes run with the copy's own environment, the one the
// factory's agents get in it (its WT_BRANCH and the copy's env, a
// [pool.env] value with {slot} rendered for the copy), never a synthetic
// PR's worktree env (WT_BRANCH=magnum-pr-<N>): the reviewers' specs and
// migrations use the copy's databases, which the factory's db-lock guards.
// A replay of a PR in a per-PR worktree still gets that worktree's env.
func TestALocalRoundsPanesCarryTheCopysEnvironment(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.layout = paths.Layout{Home: h.layout.Home, Scratch: t.TempDir()}
		h.d.Layout = h.layout
	})
	h.rd.script = func(pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun}, nil
	}
	c := localRound(h, t.TempDir(), 1)
	c.Env = map[string]string{"WT_BRANCH": "factory1", "DATABASE_SUFFIX": "_factory1"}
	if _, _, err := h.e.RunLocal(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"claude-review", "codex-judge"} {
		env := h.ag.paneEnv(role)
		if env["WT_BRANCH"] != "factory1" || env["DATABASE_SUFFIX"] != "_factory1" {
			t.Errorf("the %s pane's env %v, want the copy's WT_BRANCH and DATABASE_SUFFIX", role, env)
		}
	}

	replay := newHarness(t, func(h *harness) {
		h.layout = paths.Layout{Home: h.layout.Home, Scratch: t.TempDir()}
		h.d.Layout = h.layout
	})
	replay.rd.script = h.rd.script
	if _, _, err := replay.e.RunEval(replay.ctx, evalCase(replay, t.TempDir(), false)); err != nil {
		t.Fatal(err)
	}
	if env := replay.ag.paneEnv("claude-review"); env["WT_BRANCH"] != "magnum-pr-7" {
		t.Errorf("a replay's pane env %v, want its per-PR worktree's WT_BRANCH magnum-pr-7", env)
	}
}
