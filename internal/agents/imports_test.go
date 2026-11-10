package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// Claude Code expands an @path import in a CLAUDE.md, a CLAUDE.local.md, an
// AGENTS.md, a rules file and in what they import, four hops deep, a
// relative path from the importing file's directory, a home (~) or an
// absolute path as it is (code.claude.com/docs memory "Import additional
// files"). ImportPaths reads them wider than Claude does, so magnum errs
// toward a target: an @ at a line's start, after a blank or where a text
// token may start (here, after a code span's mark), up to the first blank
// a backslash does not escape, with and without the punctuation that ends
// a sentence, in a code span or a fenced block too (Claude skips those).
// An @ inside a text token (after a letter in an e-mail address, a
// parenthesis or a quote) imports nothing, and a home or an absolute path
// is left out, since no commit's tree holds it.
func TestImportPathsReadsClaudeCodesImports(t *testing.T) {
	text := "# Team\n\n@AGENTS.md\nRead @.docs/datagrid-guide.md first, and @docs/style.md.\n" +
		"- API conventions @Design\\ Docs/api-conventions.md\n(@notes/x.md) \"@y.md\" mail a@example.com\n" +
		"`@README` keeps it literal for Claude.\n- @~/.claude/mine.md and @/etc/hosts\n@\n"
	want := []string{"AGENTS.md", ".docs/datagrid-guide.md", "docs/style.md.", "docs/style.md", "Design Docs/api-conventions.md",
		"README`", "README"}
	if got, more := ImportPaths(text, 100); !slices.Equal(got, want) || more {
		t.Fatalf("ImportPaths = %q, %v\nwant %q", got, more, want)
	}
}

// Claude Code's own parser ends an import's path at a backslash that no
// space follows, and drops a #fragment from it: @docs/guide.md#setup loads
// docs/guide.md, and so does @docs/guide.md\ at a Markdown hard line break.
// ImportPaths gives those paths too, beside its wider reading, so a change
// to docs/guide.md is one to what the instructions import.
func TestImportPathsEndsAPathWhereClaudeCodeDoes(t *testing.T) {
	text := "Setup: @docs/guide.md#setup\nSee @docs/style.md\\\nand @Design\\ Docs/a.md#x.\n"
	got, more := ImportPaths(text, 100)
	for _, want := range []string{"docs/guide.md#setup", "docs/guide.md", "docs/style.md\\", "docs/style.md", "Design Docs/a.md#x.",
		"Design Docs/a.md"} {
		if !slices.Contains(got, want) {
			t.Errorf("ImportPaths = %q, want %q among them", got, want)
		}
	}
	if more {
		t.Error("more = true for a text within the limit")
	}
}

// A file the PR controls may hold any number of @ tokens: ImportPaths
// gives at most limit paths and says when the text has more, in time
// linear in the text (a text of 300,000 distinct tokens, past the limit,
// returns at once rather than comparing each path with all before it).
func TestImportPathsStopsAtItsLimit(t *testing.T) {
	var b strings.Builder
	for i := range 300_000 {
		fmt.Fprintf(&b, "@%x\n", i)
	}
	text := b.String()
	start := time.Now()
	got, more := ImportPaths(text, 1000)
	if len(got) != 1000 || !more {
		t.Fatalf("ImportPaths = %d paths, more %v; want 1000 and more", len(got), more)
	}
	all, more := ImportPaths(text, 1_000_000)
	if len(all) != 300_000 || more {
		t.Fatalf("ImportPaths within the limit = %d paths, more %v; want 300000", len(all), more)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("ImportPaths took %s", d)
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

// Claude Code reads an instruction file by the path it finds it at and
// expands its imports from there: an app/CLAUDE.md the index tracks (a
// file, or a link to a file of any name), a rules file under .claude/ that
// links to a file of another name, a root CLAUDE.md that links to a guide
// (whose imports then resolve from the guide's directory too, where the
// disk has it), and a file an import reaches through a directory link
// (whose own imports resolve from the path the import named, lib/, as well
// as from where the link leads). The session reads each of those files'
// imports and declines when the PR changes one, named by the file's
// project path.
func TestASessionReadsTheImportsOfEveryInstructionFileItLoads(t *testing.T) {
	for _, c := range []struct {
		name    string
		files   map[string]string
		links   map[string]string // path -> text
		index   []string          // ls-files -s entries below the root: "<mode> <path>"
		changed string
		label   string
	}{
		{name: "a tracked instruction file below the root", files: map[string]string{"app/CLAUDE.md": "Read @../shared/x.md first.\n",
			"shared/x.md": "x\n"}, index: []string{"100644 app/CLAUDE.md"}, changed: "shared/x.md", label: "CLAUDE.md"},
		{name: "a tracked link below the root", files: map[string]string{"docs/app.md": "Read @../shared/x.md first.\n", "shared/x.md": "x\n"},
			links: map[string]string{"app/CLAUDE.md": "../docs/app.md"}, index: []string{"120000 app/CLAUDE.md"}, changed: "shared/x.md",
			label: "CLAUDE.md"},
		{name: "a tracked link below the root to a file of another name", files: map[string]string{".cursorrules": "Follow @docs/style.md.\n",
			"docs/style.md": "x\n"}, links: map[string]string{"app/CLAUDE.md": "../.cursorrules"}, index: []string{"120000 app/CLAUDE.md"},
			changed: "docs/style.md", label: "CLAUDE.md"},
		{name: "a rules file that links to a file of another name", files: map[string]string{".claude/settings.json": "{}",
			"shared/api-rules.txt": "@../docs/api.md\n", "docs/api.md": "x\n"},
			links: map[string]string{".claude/rules/api.md": "../../shared/api-rules.txt"}, changed: "docs/api.md", label: ".claude/"},
		{name: "a root link to a guide that imports from its own directory", files: map[string]string{"docs/guide.md": "@more.md\n",
			"docs/more.md": "x\n"}, links: map[string]string{"CLAUDE.md": "docs/guide.md"}, changed: "docs/more.md", label: "CLAUDE.md"},
		{name: "an import through a directory link, resolved from the path it names", files: map[string]string{"CLAUDE.md": "@lib/guide.md\n",
			"a/b/shared/guide.md": "@../x.md\n", "x.md": "x\n"}, links: map[string]string{"lib": "a/b/shared"}, changed: "x.md",
			label: "CLAUDE.md"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, dir, ws := projectEnv(t, c.files, []string{c.changed}, nil)
			for name, text := range c.links {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(text, filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			var index []string
			for _, ent := range c.index {
				mode, name, _ := strings.Cut(ent, " ")
				index = append(index, mode+" 1111111111111111111111111111111111111111 0\t"+name+"\x00")
			}
			e.run.Rules = slices.Insert(e.run.Rules, 0, execx.Rule{Prefix: []string{"git", "-C", dir, "ls-files", "-s"},
				Result: execx.Result{Stdout: []byte(strings.Join(index, ""))}})
			if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
				t.Fatal(err)
			}
			if n, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindClaude); !ok || !slices.Equal(n.Paths, []string{c.label}) {
				raw, _, _ := e.st.GetKV(e.ctx, store.KVPRProject(e.pr.ID, KindClaude))
				t.Fatalf("record %s: want a decline for %s", raw, c.label)
			}
		})
	}
}

// A file the PR controls may hold any number of @ tokens: the session reads
// at most its walk's budget of imports, each one counted, and a file with
// more leaves the config out (partial), at once rather than after minutes
// of comparing each path with all before it.
func TestASessionsImportsStayWithinTheWalksBudget(t *testing.T) {
	var b strings.Builder
	for i := range 300_000 {
		fmt.Fprintf(&b, "@%x\n", i)
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"CLAUDE.md": b.String()})
	done := make(chan bool, 1)
	go func() {
		_, partial := ProjectLinks(KindClaude, dir)
		done <- partial
	}()
	select {
	case partial := <-done:
		if !partial {
			t.Fatal("ProjectLinks of a CLAUDE.md with 300,000 imports: not partial")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("ProjectLinks of a CLAUDE.md with 300,000 imports still runs after 20s")
	}
}

// Claude Code 2.1 lexes an instruction file with marked, without GFM, and
// matches its import pattern only in the text tokens: a path ends where an
// emphasis, a link's text, a code span, an image or inline HTML begins or
// ends (**@docs/guide.md** loads docs/guide.md), a token after a code span
// starts a path of its own, a zero-width no-break space ends one (JS's \s
// holds it) while U+0085 does not, an escaped space at its end is trimmed,
// and an HTML block that starts with a comment is read with its comments
// taken out. ImportPaths gives each path Claude loads among its own;
// an underscore inside a word is no emphasis, so @app_config gives no app.
func TestImportPathsGivesThePathClaudeCodesMarkdownEnds(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{"Read **@docs/guide.md** first.\n", []string{"docs/guide.md"}},
		{"*@docs/b.md*\n", []string{"docs/b.md"}},
		{"_x @docs/h.md_\n", []string{"docs/h.md"}},
		{"See [@docs/guide.md](docs/guide.md).\n", []string{"docs/guide.md"}},
		{"[@docs/d.md][r]\n\n[r]: x\n", []string{"docs/d.md"}},
		{"@docs/e.md<br>\n", []string{"docs/e.md"}},
		{"@docs/f.md<!-- x -->\n", []string{"docs/f.md"}},
		{"<b>@docs/s.md</b>\n", []string{"docs/s.md"}},
		{"@docs/t.md![i](x.png)\n", []string{"docs/t.md"}},
		{"@docs/a.md`x`@docs/b.md\n", []string{"docs/a.md", "docs/b.md"}},
		{"**@docs/my*file.md**\n", []string{"docs/my*file.md"}},
		{"@docs/guide.md\ufeffsee\n", []string{"docs/guide.md"}},
		{"@docs/a\u0085b.md\n", []string{"docs/a\u0085b.md"}},
		{"Load @docs/a.md\\ \n", []string{"docs/a.md"}},
		{"<!-- c --> @docs/a<!-- x -->.md\n", []string{"docs/a.md"}},
	} {
		got, more := ImportPaths(c.text, 100)
		for _, w := range c.want {
			if !slices.Contains(got, w) || more {
				t.Errorf("ImportPaths(%q) = %q, %v; want %q among them", c.text, got, more, w)
			}
		}
	}
	if got, _ := ImportPaths("Use @app_config and @a__b.\n", 100); slices.Contains(got, "app") || slices.Contains(got, "a") {
		t.Errorf("ImportPaths = %q: an underscore inside a word ends no path", got)
	}
}

// Claude may end a path at any of the marks in a word, and start one after
// each of them: read from each @ to each mark, a word of a million is a
// quadratic count of paths. ImportPaths compares its readings within a
// bound linear in the text and says more past it, at once; the imports of
// ordinary text, however dense, stay within it.
func TestImportPathsReadsWithinABoundLinearInItsText(t *testing.T) {
	for _, text := range []string{"@" + strings.Repeat("a*", 1<<20), strings.Repeat("*@a", 1<<20), strings.Repeat("[@", 1<<20)} {
		start := time.Now()
		_, more := ImportPaths(text, 1_000_000)
		if d := time.Since(start); !more || d > 10*time.Second {
			t.Errorf("ImportPaths of %q... = more %v after %s; want more, at once", text[:12], more, d)
		}
	}
	ruby := strings.Repeat("    @user = User.find(params[:id])\n    @posts = @user.posts.where(draft: false).order(:created_at)\n", 2000)
	got, more := ImportPaths(ruby, 100_000)
	for _, want := range []string{"user", "posts", "user.posts.where(draft:"} {
		if !slices.Contains(got, want) || more {
			t.Errorf("ImportPaths of Ruby = %q, %v; want %q", got, more, want)
		}
	}
}

// A session loads what Claude Code reads from an import in Markdown's
// emphasis, a link's text or before a zero-width no-break space: the
// imported file is one of ProjectLinks' targets, which the reload check
// matches the new head's files with.
func TestASessionReadsAnImportInMarkdownsEmphasis(t *testing.T) {
	for _, text := range []string{"Read **@.docs/guide.md** first.\n", "See [@.docs/guide.md](.docs/guide.md).\n", "@.docs/guide.md\ufeffsee\n"} {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"CLAUDE.md": text, ".docs/guide.md": "x\n"})
		if targets, partial := ProjectLinks(KindClaude, dir); !slices.Contains(targets, ".docs/guide.md") || partial {
			t.Errorf("CLAUDE.md %q: ProjectLinks = %q, %v; want .docs/guide.md", text, targets, partial)
		}
	}
}
