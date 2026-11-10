package agents

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// A Claude decline records the targets of the checkout's root links into it
// (CLAUDE.md -> docs/guide.md) with the label of the link's path, read from
// the disk, not from git, so also when git could not compare: a change of
// such a target is the one change of the agents' config that a PR's file
// list names by another path, and the auto-approval's gate matches the
// targets against that list (ChangedLinks) instead of trusting git's compare
// of the slot. The event leaves them out (a link the PR added names a path
// it chose).
func TestAClaudeDeclineRecordsTheCheckoutsRootLinkTargets(t *testing.T) {
	for _, c := range []struct {
		name string
		fail bool
	}{{"git compared", false}, {"git could not compare", true}} {
		t.Run(c.name, func(t *testing.T) {
			e, dir, ws := projectEnv(t, map[string]string{".claude/settings.json": "{}", "docs/guide.md": "x\n"}, []string{"docs/guide.md"}, nil)
			if err := os.Symlink("docs/guide.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
				t.Fatal(err)
			}
			if c.fail {
				rule := execx.Rule{Prefix: []string{"git", "-C", dir, "merge-base"}, Err: errors.New("git broke")}
				e.run.Rules = append([]execx.Rule{rule}, e.run.Rules...)
			}
			if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
				t.Fatal(err)
			}
			n, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindClaude)
			if raw, _, _ := e.st.GetKV(e.ctx, store.KVPRProject(e.pr.ID, KindClaude)); !ok || n.Compared == c.fail ||
				!maps.Equal(n.Links, map[string]string{"docs/guide.md": "CLAUDE.md"}) {
				t.Fatalf("record %s: want the link's target docs/guide.md, for CLAUDE.md", raw)
			}
			if got := n.ChangedLinks([]string{"app/x.rb", "docs/guide.md"}); !slices.Equal(got, []string{"CLAUDE.md"}) {
				t.Fatalf("ChangedLinks of the target = %q, want CLAUDE.md", got)
			}
			if got := n.ChangedLinks([]string{"app/x.rb", "docs/guide.md.bak", "docs/guide.mdx", "docs"}); len(got) != 0 {
				t.Fatalf("ChangedLinks of files beside the target = %q, want none", got)
			}
			for _, ev := range claudeProjectEvents(t, e) {
				var data map[string]any
				if err := json.Unmarshal(ev.Data, &data); err != nil {
					t.Fatal(err)
				}
				if _, named := data["links"]; named {
					t.Fatalf("the event names the link's target: %s", ev.Data)
				}
			}
		})
	}
}

// ChangedLinks names each link whose target, a file or a directory, the
// file list changes, once, in the order of the targets; a record without
// links (an older build's) changes none.
func TestChangedLinksNamesTheLinksWhoseTargetsTheFilesChange(t *testing.T) {
	n := ProjectNote{Links: map[string]string{"docs/guide.md": "CLAUDE.md", "shared/claude": ".claude/", "docs/local.md": "CLAUDE.md"}}
	for _, c := range []struct {
		name  string
		files []string
		want  []string
	}{
		{"a file under a linked directory", []string{"shared/claude/settings.json"}, []string{".claude/"}},
		{"two targets of one label", []string{"docs/local.md", "docs/guide.md"}, []string{"CLAUDE.md"}},
		{"both", []string{"shared/claude/skills/x/SKILL.md", "docs/guide.md"}, []string{"CLAUDE.md", ".claude/"}},
		{"a sibling of the directory", []string{"shared/claude.json", "shared/claudex/a"}, nil},
		{"another case", []string{"SHARED/Claude/settings.json"}, []string{".claude/"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := n.ChangedLinks(c.files); !slices.Equal(got, c.want) {
				t.Fatalf("ChangedLinks(%q) = %q, want %q", c.files, got, c.want)
			}
		})
	}
	if got := (ProjectNote{}).ChangedLinks([]string{"docs/guide.md"}); got != nil {
		t.Fatalf("a record without links: %q", got)
	}
}

// Claude follows a symbolic link inside its project directory too
// (.claude/skills -> ../.agents/skills loads the skills there), while git
// compares the link alone: a PR that changes a skill there changes what the
// session loads with nothing under .claude/ changed. The session compares
// the target of each link under .claude/, and of each link under a
// directory one leads to, as .claude/: it declines, and the record keeps
// the targets with that label, which the auto-approval's gate matches
// against the PR's file list (ChangedLinks). A nested checkout (Claude
// Code's .claude/worktrees/<name>) is not entered, a link out of the
// checkout leads nowhere a commit of the PR reaches, and a link within
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
	if raw, _, _ := e.st.GetKV(e.ctx, store.KVPRProject(e.pr.ID, KindClaude)); !ok || !slices.Equal(n.Paths, []string{".claude/"}) ||
		!maps.Equal(n.Links, map[string]string{".agents/skills": ".claude/", "tools/lint": ".claude/"}) {
		t.Fatalf("record %s: want a decline for .claude/ with the targets .agents/skills and tools/lint", raw)
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
	for _, files := range [][]string{{".agents/skills/y/SKILL.md"}, {"tools/lint/SKILL.md"}} {
		if got := n.ChangedLinks(files); !slices.Equal(got, []string{".claude/"}) {
			t.Fatalf("ChangedLinks(%q) = %q, want .claude/", files, got)
		}
	}
	if got := n.ChangedLinks([]string{"docs/n.md", ".agents/skillsx/a"}); len(got) != 0 {
		t.Fatalf("ChangedLinks of files no link leads to = %q", got)
	}
}

// git names a tracked file in its own case (docs/guide.md), while a link that
// leads to it keeps the case of its text (CLAUDE.md -> Docs/Guide.md, which
// macOS's case-insensitive filesystem opens): the session matches a changed
// file to a link's target in any case, as git's compare matched it, and so
// does ChangedLinks with the PR's file list.
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
	if got := n.ChangedLinks([]string{"docs/guide.md"}); !slices.Equal(got, []string{"CLAUDE.md"}) {
		t.Fatalf("ChangedLinks = %q, want CLAUDE.md", got)
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
