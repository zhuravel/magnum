package agents

import (
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// Claude Code expands an @path import in a CLAUDE.md, a CLAUDE.local.md, an
// AGENTS.md, a rules file and in what they import, four hops deep, a
// relative path from the importing file's directory, a home (~) or an
// absolute path as it is (code.claude.com/docs memory "Import additional
// files"). ImportPaths reads them wider than Claude does, so magnum errs
// toward a target: an @ at a line's start or after any character but a
// letter or a digit (an e-mail address imports nothing), up to the first
// blank a backslash does not escape, with and without the punctuation that
// ends a sentence, in a code span or a fenced block too (Claude skips
// those); a home or an absolute path is left out, since no commit's tree
// holds it.
func TestImportPathsReadsClaudeCodesImports(t *testing.T) {
	text := "# Team\n\n@AGENTS.md\nRead @.docs/datagrid-guide.md first, and @docs/style.md.\n" +
		"- API conventions @Design\\ Docs/api-conventions.md\n(@notes/x.md) mail a@example.com\n" +
		"`@README` keeps it literal for Claude.\n- @~/.claude/mine.md and @/etc/hosts\n@\n"
	want := []string{"AGENTS.md", ".docs/datagrid-guide.md", "docs/style.md.", "docs/style.md", "Design Docs/api-conventions.md",
		"notes/x.md)", "notes/x.md", "README`", "README"}
	if got := ImportPaths(text); !slices.Equal(got, want) {
		t.Fatalf("ImportPaths = %q\nwant %q", got, want)
	}
}

// A Claude session loads what the checkout's instruction files import
// (@path), from the importing file's directory, recursively, while git
// compares the instruction file alone: the team's CLAUDE.md that imports
// @.docs/guide.md, which imports @more.md, loads the PR's .docs/more.md.
// The session compares each imported file as a link's target, named by the
// importing file: it declines. ProjectLinks, which the reload check reads,
// names them too.
func TestASessionComparesWhatTheInstructionsImport(t *testing.T) {
	e, dir, ws := projectEnv(t, map[string]string{".claude/settings.json": "{}", "CLAUDE.md": "Read @.docs/guide.md first.\n",
		".docs/guide.md": "See @more.md.\n", ".docs/more.md": "x\n", "docs/other.md": "y\n"}, []string{".docs/more.md"}, nil)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	n, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindClaude)
	if raw, _, _ := e.st.GetKV(e.ctx, store.KVPRProject(e.pr.ID, KindClaude)); !ok || !slices.Equal(n.Paths, []string{"CLAUDE.md"}) {
		t.Fatalf("record %s: want a decline for CLAUDE.md", raw)
	}
	for _, d := range filteredDiffs(e, dir) {
		if !slices.Contains(d.Args, ":(top,literal,icase).docs/guide.md") || !slices.Contains(d.Args, ":(top,literal,icase).docs/more.md") ||
			slices.ContainsFunc(d.Args, func(a string) bool { return strings.Contains(a, "other.md") }) {
			t.Fatalf("git %q: want the imported files compared, and no other", d.Args)
		}
	}
	if targets, partial := ProjectLinks(KindClaude, dir); !slices.Contains(targets, ".docs/guide.md") || !slices.Contains(targets, ".docs/more.md") || partial {
		t.Fatalf("ProjectLinks = %q, %v; want the imported files", targets, partial)
	}
}
