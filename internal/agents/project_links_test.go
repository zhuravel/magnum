package agents

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// Claude follows a symbolic link inside its project directory too
// (.claude/skills -> ../.agents/skills loads the skills there), while git
// compares the link alone: a PR that changes a skill there changes what the
// session loads with nothing under .claude/ changed. The session compares
// the target of each link under .claude/, and of each link under a
// directory one leads to, as .claude/: it declines. A nested checkout
// (Claude Code's .claude/worktrees/<name>) is not entered, a link out of
// the checkout leads nowhere a commit of the PR reaches, and a link within
// .claude/ is compared as itself.
func TestALinkInsideTheClaudeDirectoryIsComparedAsItsTarget(t *testing.T) {
	e, dir, ws := projectEnv(t, map[string]string{
		".claude/settings.json":        "{}",
		".claude/commands/a.md":        "a\n",
		".claude/worktrees/w/.git":     "gitdir: elsewhere\n",
		".claude/worktrees/w/app/x.rb": "x\n",
		".agents/skills/x/SKILL.md":    "x\n",
		"tools/lint/SKILL.md":          "y\n",
		"docs/n.md":                    "n\n",
	}, []string{".agents/skills/x/SKILL.md"}, nil)
	for name, target := range map[string]string{
		".claude/skills":          "../.agents/skills",
		".agents/skills/lint":     "../../tools/lint",
		".claude/worktrees/w/doc": "../../../docs",
		".claude/home":            t.TempDir(),
		".claude/alias.md":        "commands/a.md",
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	n, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindClaude)
	if raw, _, _ := e.st.GetKV(e.ctx, store.KVPRProject(e.pr.ID, KindClaude)); !ok || !slices.Equal(n.Paths, []string{".claude/"}) {
		t.Fatalf("record %s: want a decline for .claude/", raw)
	}
	diffs := e.run.CallsWithPrefix("git", "-C", dir, "diff")
	if len(diffs) == 0 {
		t.Fatalf("no git diff: %v", e.run.Calls)
	}
	for _, d := range diffs {
		if !slices.Contains(d.Args, ":(top,literal,icase).agents/skills") || !slices.Contains(d.Args, ":(top,literal,icase)tools/lint") ||
			slices.ContainsFunc(d.Args, func(a string) bool { return strings.Contains(a, "docs") || strings.Contains(a, "commands") }) {
			t.Fatalf("git %q: want the links' targets in the checkout compared, and no other", d.Args)
		}
	}
}

// git names a tracked file in its own case (docs/guide.md), while a link that
// leads to it keeps the case of its text (CLAUDE.md -> Docs/Guide.md, which
// macOS's case-insensitive filesystem opens): the session matches a changed
// file to a link's target in any case, as git's compare matched it, and so
// does UnderTarget, which the auto-approval's gate matches the PR's file
// list with.
func TestALinksTargetMatchesAChangedFileInAnyCase(t *testing.T) {
	e, dir, ws := projectEnv(t, map[string]string{".claude/settings.json": "{}", "Docs/Guide.md": "x\n"}, []string{"docs/guide.md"}, nil)
	if err := os.Symlink("Docs/Guide.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	n, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindClaude)
	if raw, _, _ := e.st.GetKV(e.ctx, store.KVPRProject(e.pr.ID, KindClaude)); !ok || !slices.Equal(n.Paths, []string{"CLAUDE.md"}) {
		t.Fatalf("record %s: want a decline for CLAUDE.md", raw)
	}
	for _, c := range []struct {
		f, t string
		want bool
	}{{"docs/guide.md", "Docs/Guide.md", true}, {"shared/claude/x.md", "SHARED/Claude", true},
		{"docs/guide.mdx", "docs/guide.md", false}, {"docs", "docs/guide.md", false}, {"shared/claudex/a", "shared/claude", false}} {
		if got := UnderTarget(c.f, c.t); got != c.want {
			t.Errorf("UnderTarget(%q, %q) = %v, want %v", c.f, c.t, got, c.want)
		}
	}
}

// A stacked PR's base branch is another PR's head, whose own .claude/ or
// .codex/ changes are no more the team's than the PR's: compared with
// origin/<that branch> once it is fresh (the operator's fetch, a fix's),
// they would load as the team's, its hooks outside the sandbox. A session of
// a PR based on another branch than the repository's default compares the
// project config with origin/<default branch>, which a pool slot keeps
// fresh, for an agent's launch and a shell role's line alike, so the base
// branch's changes count as the PR's and stay out. A PR on the default
// branch compares as before (TestJudgeOfAPRChangingCodexStartsWithTheCheckoutUntrusted,
// TestCodexReviewOfAPRChangingCodexRunsWithTheCheckoutUntrusted).
func TestAStackedPRsSessionsCompareTheProjectConfigWithTheDefaultBranch(t *testing.T) {
	e, dir, ws := projectEnv(t, map[string]string{".claude/settings.json": "{}", ".codex/config.toml": "model = \"x\"\n"},
		[]string{".claude/settings.json", ".codex/config.toml"}, nil)
	up, err := e.st.UpsertPRFromGitHub(e.ctx, store.GitHubPR{RepoID: e.pr.RepoID, NodeID: "PR_11920", Number: e.pr.Number, URL: e.pr.URL,
		HeadSHA: e.pr.HeadSHA, GHState: store.GHOpen, BaseRef: new("feature-a"), Identity: e.pr.Identity})
	if err != nil {
		t.Fatal(err)
	}
	e.pr = up.PR
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	d := ShellData{BaseRef: "origin/feature-a", BaseSHA: projectMergeBase, HeadSHA: projectHead,
		ReportPath: "/r/codex.md", Marker: DoneMarker("r-1"), Checkout: dir}
	if _, err := e.m.ShellLine(e.ctx, e.pr.ID, e.spec(RoleCodexReview), d); err != nil {
		t.Fatal(err)
	}
	calls := e.run.CallsWithPrefix("git", "-C", dir, "merge-base")
	if len(calls) != 2 {
		t.Fatalf("merge-base calls = %v, want the launch's and the line's", calls)
	}
	for _, c := range calls {
		if !slices.Equal(c.Args, []string{"-C", dir, "merge-base", "HEAD", "origin/master"}) {
			t.Fatalf("merge-base %q: want HEAD and origin/master, the default branch", c.Args)
		}
	}
}

// Claude Code loads the skills of a .claude/skills/ below the directory it
// starts in the first time it reads a file there, and a skill's frontmatter
// can declare hooks: a .claude/ in any directory is Claude's project config
// (ProjectTouched, ProjectPaths, the session's compare), in any case, while
// Codex reads .codex/ only from its start directory up, and Claude reads
// .mcp.json at the root only. A PR that adds
// packages/web/.claude/skills/review/SKILL.md starts Claude with the user's
// settings only, named as .claude/.
func TestAClaudeDirectoryBelowTheRootIsClaudesProjectConfig(t *testing.T) {
	for _, c := range []struct {
		kind string
		path string
		want bool
	}{
		{KindClaude, "packages/web/.claude/skills/review/SKILL.md", true},
		{KindClaude, "lib/.CLAUDE/agents/a.md", true},
		{KindClaude, "apps/.claude", true},
		{KindClaude, "packages/web/claude/skills/x.md", false},
		{KindClaude, "packages/web/.mcp.json", false},
		{KindCodex, "packages/web/.codex/config.toml", false},
	} {
		if got := ProjectTouched(c.kind, []string{c.path}); got != c.want {
			t.Errorf("ProjectTouched(%s, %q) = %v, want %v", c.kind, c.path, got, c.want)
		}
	}
	if got := ProjectPaths(KindClaude); !slices.Contains(got, "**/.claude") || !slices.Contains(got, "**/.claude/**") {
		t.Fatalf("ProjectPaths(claude) = %q: want a .claude/ in any directory", got)
	}
	if got := ProjectPaths(KindCodex); slices.ContainsFunc(got, func(p string) bool { return strings.Contains(p, ".codex") && strings.HasPrefix(p, "**/") }) {
		t.Fatalf("ProjectPaths(codex) = %q: Codex reads its root's .codex/ only", got)
	}

	e, dir, ws := projectEnv(t, map[string]string{"packages/web/.claude/skills/review/SKILL.md": "---\nhooks: {}\n---\n"},
		[]string{"packages/web/.claude/skills/review/SKILL.md"}, nil)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	if n, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindClaude); !ok || !slices.Equal(n.Paths, []string{".claude/"}) {
		raw, _, _ := e.st.GetKV(e.ctx, store.KVPRProject(e.pr.ID, KindClaude))
		t.Fatalf("record %s: want a decline for .claude/", raw)
	}
	for _, d := range e.run.CallsWithPrefix("git", "-C", dir, "diff") {
		if !slices.Contains(d.Args, ":(top,icase,glob)**/.claude/**") {
			t.Fatalf("git %q: want every .claude/ compared", d.Args)
		}
	}
}
