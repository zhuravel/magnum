package engine

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/staging"
)

const (
	// stagingTip is staging-void's tip in the mirror, and stagingMerge the
	// merge of PR #2's head onto it that a staging deploy pushes.
	stagingTip   = "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"
	stagingMerge = "6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b"
	// stagingMirror is talkable/talkable's staging mirror.
	stagingMirror = "/mirrors/talkable/talkable.git"
)

// mirrorGit is mergeGit over a staging mirror: RevParse answers refs by
// name, and DifferingPaths of the merge against a tip its workflow files
// that differ there (differ, by the tip).
type mirrorGit struct {
	*mergeGit
	refs   map[string]string
	differ map[string][]string
	dirs   []string
}

func (g *mirrorGit) RevParse(ctx context.Context, dir, ref string) (string, error) {
	g.mu.Lock()
	sha, ok := g.refs[ref]
	g.dirs = append(g.dirs, dir)
	g.mu.Unlock()
	if ok {
		return sha, nil
	}
	return g.mergeGit.RevParse(ctx, dir, ref)
}

func (g *mirrorGit) DifferingPaths(_ context.Context, dir, a, b string, paths ...string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dirs = append(g.dirs, dir)
	return slices.DeleteFunc(slices.Clone(g.differ[b]), func(p string) bool {
		return !slices.ContainsFunc(paths, func(q string) bool { return p == q || strings.HasPrefix(p, q+"/") })
	}), nil
}

// stagingHarness is mergeHarness with the fixer App and a mirror whose
// staging-void holds stagingTip, and PR #2 (head fixHead, base master).
func stagingHarness(t *testing.T, mods ...func(*harness)) (*harness, *mirrorGit) {
	t.Helper()
	mg := &mirrorGit{refs: map[string]string{
		stagingMerge + "^1": stagingTip, stagingMerge + "^2": fixHead,
		"refs/heads/staging-void": stagingTip, "refs/heads/master": fixBaseTip,
	}, differ: map[string][]string{}}
	h, _ := fixHarness(t, append(append([]func(*harness){withFixerApp}, mods...), func(h *harness) {
		mg.mergeGit = &mergeGit{fixGit: h.d.Git.(*fixGit), gh: h.gh}
		h.d.Git = mg
	})...)
	fixablePR(h, "zhuravel", nil)
	return h, mg
}

// stagingMirrors is the deploy's mirror of each repository.
func stagingMirrors(repo string) (string, bool) { return stagingMirror, repo == "talkable/talkable" }

// A staging deploy's merge is the fix identity's bot's, as every commit
// magnum makes on a PR's behalf: its name and noreply email, which GitHub
// links to the bot (an unattributed commit asks for another approval).
// Without a dedicated fixer App there is no bot to name.
func TestAStagingMergeIsByTheFixIdentitysBot(t *testing.T) {
	h, _ := stagingHarness(t)
	name, email, err := h.e.CommitIdentity(h.ctx, "talkable/talkable")
	if err != nil || name != fixerAppLogin || email != fixerAppEmail {
		t.Errorf("CommitIdentity = %q, %q, %v; want %s and %s", name, email, err, fixerAppLogin, fixerAppEmail)
	}
	if _, _, err := h.e.CommitIdentity(h.ctx, "talkable/elsewhere"); err == nil {
		t.Error("an unwatched repository has a bot")
	}
	g, _ := fixHarness(t)
	if _, _, err := g.e.CommitIdentity(g.ctx, "talkable/talkable"); err == nil || !strings.Contains(err.Error(), "no fixer App") {
		t.Errorf("CommitIdentity without a fixer App = %v", err)
	}
}

// A staging push goes as the fix identity's bot. A merge whose workflow
// files are the staging tip's goes with the App's token; one that brings
// workflow files the tip lacks goes only when each is as the PR's base
// tip has it (the base's own changes, which the PR merged), with the
// daemon's own Workflows token; a merge carrying a workflow file that
// differs from both (the PR's own change) is refused, and so is any push
// while the App lacks Workflows, of a merge that is not on the target's
// tip, or to a branch the bot never pushes to (master, main, the default
// branch).
func TestAStagingPushIsTheBotsAndCarriesOnlyTheBasesOrTheTipsWorkflows(t *testing.T) {
	const ci, own = ".github/workflows/ci.yml", ".github/workflows/own.yml"
	const elsewhere = "7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c"
	for _, tc := range []struct {
		name           string
		tip, base      []string // the merge's workflow files that differ from the tip's, the base's
		target         string
		def, first     string // the repository's default branch (the registry's when ""), the merge's first parent (stagingTip when "")
		noWorkflows    bool
		wantDir, wantE string
		wantMint       bool
	}{
		{name: "the tip's workflows", target: "staging-void", wantDir: "/state/gh/" + fixerAppName},
		{name: "the base's workflows", tip: []string{ci}, target: "staging-void", wantDir: daemonPushAs, wantMint: true},
		{name: "the PR's own", tip: []string{ci, own}, base: []string{own}, target: "staging-void",
			wantE: "carries workflow files that differ from both staging-void's and master's (.github/workflows/own.yml)"},
		{name: "no Workflows on the App", tip: []string{ci}, target: "staging-void", noWorkflows: true, wantMint: true,
			wantE: "needs Workflows on the App"},
		{name: "master", target: "master", wantE: "never pushes to master"},
		{name: "main", target: "main", def: "develop", wantE: "never pushes to main"},
		{name: "the default branch", target: "develop", def: "develop", wantE: "never pushes to develop"},
		{name: "a merge off the tip", target: "staging-void", first: elsewhere, wantE: "the merge is on 7c7c7c7, not on staging-void's tip 5a5a5a5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mods []func(*harness)
			if tc.noWorkflows {
				mods = append(mods, withoutWorkflows)
			}
			h, mg := stagingHarness(t, mods...)
			mg.differ[stagingTip], mg.differ[fixBaseTip] = tc.tip, tc.base
			if tc.first != "" {
				mg.refs[stagingMerge+"^1"] = tc.first
			}
			if tc.def != "" {
				if _, err := h.st.DB().ExecContext(h.ctx, `UPDATE repos SET default_branch = ?`, tc.def); err != nil {
					t.Fatal(err)
				}
			}
			env, err := h.e.StagingPushEnv(stagingMirrors)(h.ctx, "talkable/talkable", stagingMerge, tc.target)
			switch {
			case tc.wantE != "" && (err == nil || !strings.Contains(err.Error(), tc.wantE)):
				t.Errorf("err = %v, want it to say %q", err, tc.wantE)
			case tc.wantE == "" && (err != nil || pushedAs(env) != tc.wantDir):
				t.Errorf("env = %v, %v; want it to push as %s", env, err, tc.wantDir)
			}
			if mints, _ := h.ids[fixerAppName].daemonPushes(); len(mints) > 0 != tc.wantMint || slices.Contains(mints, false) {
				t.Errorf("daemon push tokens = %v, want one with workflows: %v", mints, tc.wantMint)
			}
			mg.mu.Lock()
			defer mg.mu.Unlock()
			if slices.ContainsFunc(mg.dirs, func(d string) bool { return d != stagingMirror }) {
				t.Errorf("git ran in %v, want the mirror alone", mg.dirs)
			}
		})
	}
	h, _ := stagingHarness(t)
	if _, err := h.e.StagingPushEnv(stagingMirrors)(h.ctx, "talkable/talkable", "6b6b", "staging-void"); err == nil {
		t.Error("a short sha was taken")
	}
	if _, err := h.e.StagingPushEnv(func(string) (string, bool) { return "", false })(h.ctx, "talkable/talkable", stagingMerge, "staging-void"); err == nil {
		t.Error("a push with no mirror was taken")
	}
}

// The engine's commit identity and push env are the staging package's
// Deps as they are: the daemon's staging wiring passes them with no
// adapter.
func TestTheStagingPushEnvAndCommitIdentityAreStagingsDeps(t *testing.T) {
	h, _ := stagingHarness(t)
	d := staging.Deps{PushEnv: h.e.StagingPushEnv(stagingMirrors), CommitIdentity: h.e.CommitIdentity}
	if _, err := d.PushEnv(h.ctx, "talkable/talkable", stagingMerge, "staging-void"); err != nil {
		t.Errorf("PushEnv: %v", err)
	}
	if name, _, err := d.CommitIdentity(h.ctx, "talkable/talkable"); err != nil || name != fixerAppLogin {
		t.Errorf("CommitIdentity = %q, %v", name, err)
	}
}
