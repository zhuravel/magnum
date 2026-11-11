package agents

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	if targets, partial := ProjectLinks(t.Context(), KindClaude, dir); !slices.Contains(targets, ".docs/guide.md") || !slices.Contains(targets, ".docs/more.md") || partial {
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
// project path. A file under a directory link is read by the link's path
// (.claude/rules/api.md through .claude/rules -> ../docs/ai/rules), whose
// imports resolve from there, by each such path.
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
		{name: "a rules directory link of another depth", files: map[string]string{".claude/settings.json": "{}",
			"docs/ai/rules/api.md": "Follow @../../shared/conventions.md\n", "shared/conventions.md": "x\n", "docs/shared/conventions.md": "y\n"},
			links: map[string]string{".claude/rules": "../docs/ai/rules"}, changed: "shared/conventions.md", label: ".claude/"},
		{name: "a root .claude link of another depth", files: map[string]string{"config/claude/settings.json": "{}",
			"config/claude/rules/api.md": "@../../x.md\n", "x.md": "x\n", "config/x.md": "y\n"},
			links: map[string]string{".claude": "config/claude"}, changed: "x.md", label: ".claude/"},
		{name: "tracked .claude links below the root that lead to one directory", files: map[string]string{".claude/settings.json": "{}",
			".claude/rules/api.md": "@../../README.md\n", "README.md": "r\n", "packages/api/README.md": "a\n", "packages/web/README.md": "w\n"},
			links: map[string]string{"packages/api/.claude": "../../.claude", "packages/web/.claude": "../../.claude"},
			index: []string{"120000 packages/api/.claude", "120000 packages/web/.claude"}, changed: "packages/web/README.md", label: ".claude/"},
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
		_, partial := ProjectLinks(t.Context(), KindClaude, dir)
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
// ends (**@docs/guide.md** loads docs/guide.md), a token after a code span,
// a link, an emphasis or an escape starts a path of its own, a zero-width
// no-break space ends one and starts the next (JS's \s holds it) while
// U+0085 does not, an escaped space at its end is trimmed,
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
		// A text token starts after a zero-width no-break space, a link,
		// a reference link, an emphasis and an escape, and ends at a link.
		{"x\ufeff@docs/g.md\n", []string{"docs/g.md"}},
		{"[x](y)@docs/i.md\n", []string{"docs/i.md"}},
		{"[x]: /u\n\n[x]@docs/j.md\n", []string{"docs/j.md"}},
		{"_x_@docs/k.md\n", []string{"docs/k.md"}},
		{"\\.@docs/l.md\n", []string{"docs/l.md"}},
		{"@docs/m.md[x](y)\n", []string{"docs/m.md"}},
		// marked unescapes \[ and \] in a link's text before it lexes it,
		// and again in a link that text then holds.
		{"[see @docs/ev\\[il.md](u)\n", []string{"docs/ev[il.md"}},
		{"[@docs/a\\]b.md][r]\n\n[r]: u\n", []string{"docs/a]b.md"}},
		{"[\\[@docs/ev\\\\[il.md](u) [b]](v)\n", []string{"docs/ev[il.md"}},
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

// Claude Code strips a file's YAML frontmatter before it lexes the rest
// (/^---\s*\n([\s\S]*?)---\s*\n?/ after a BOM, read from its 2.1.322
// binary): the closing --- need not stand on its own line, and the text
// after it starts a text token, so an @ right after it imports. ImportPaths
// reads that text as its own too; a --- that opens no frontmatter starts
// nothing. JS's \s, which the pattern's blanks are, holds U+FEFF and not
// U+0085.
func TestImportPathsReadsTheTextAfterAFrontmatterAsClaudeCodeDoes(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{"---\npaths: src/**\n---@docs/evil.md\n", []string{"docs/evil.md"}},
		{"---\n---@docs/a.md\n", []string{"docs/a.md"}},
		{"\ufeff---\n---@docs/b.md\n", []string{"docs/b.md"}},
		{"---\ntitle: x---@docs/c.md\n", []string{"docs/c.md"}},
		{"---  \r\ntitle: x\r\n--- @docs/d.md\n", []string{"docs/d.md"}},
		{"---\ntitle: x\n---<!-- a -->@docs/e.md\n", []string{"docs/e.md"}},
		{"---\ufeff\n---@docs/f.md\n", []string{"docs/f.md"}},
		{"---\u0085\n---@docs/no.md\n", nil},
		{"Notes\n---@docs/no.md\n", nil},
		{"----\n---@docs/no.md\n", nil},
	} {
		got, more := ImportPaths(c.text, 100)
		if more || len(c.want) == 0 && slices.Contains(got, "docs/no.md") {
			t.Errorf("ImportPaths(%q) = %q, %v; want no docs/no.md", c.text, got, more)
		}
		for _, w := range c.want {
			if !slices.Contains(got, w) {
				t.Errorf("ImportPaths(%q) = %q, %v; want %q among them", c.text, got, more, w)
			}
		}
	}
}

// Claude Code takes the comments out of each HTML block that starts with
// one and reads the block on its own, so the text after its first comment
// starts a path; an earlier <!-- it does not read as a comment (in a
// paragraph, a heading or a frontmatter) leaves the block's comments as
// they are. ImportPaths' read without comments pairs each <!-- with the
// first --> after it across the whole text, so it starts a path wherever
// a comment was cut.
func TestImportPathsStartsAPathWhereACommentWasCut(t *testing.T) {
	for _, c := range []struct {
		text string
		want string
	}{
		{"Notes (<!--\n\n<!--a-->@docs/ev<!--b-->il.md\n", "docs/evil.md"},
		{"x<!--\n\n<!--a-->@docs/a.md\n", "docs/a.md"},
		{"# é<!--\n\n<!--a-->@docs/b<!--b-->c.md\n", "docs/bc.md"},
		{"---\ntitle: a<!--\n---\n<!--a-->@docs/ev<!--b-->il.md\n", "docs/evil.md"},
	} {
		if got, more := ImportPaths(c.text, 100); !slices.Contains(got, c.want) || more {
			t.Errorf("ImportPaths(%q) = %q, %v; want %q among them", c.text, got, more, c.want)
		}
	}
}

// Claude Code reads each HTML block that starts with a comment on its own:
// the block ends at the end of the line its first comment ends on (at once
// for <!--> and <!--->), and its comments are paired within it. A <!-- the
// file-wide pairing takes as a comment (one in a code span or a paragraph)
// pairs with the --> inside a block's <!-->, and a <!-- the block leaves
// open pairs with a --> further down: both cut the path Claude glues
// together (as Claude Code 2.1.322's lexer reads these texts). marked ends
// a line at a carriage return too.
func TestImportPathsReadsEachCommentBlockOnItsOwn(t *testing.T) {
	for _, c := range []struct {
		text string
		want string
	}{
		{"Use `<!--` to open a comment.\n\n<!-->a<!-->@docs/ev<!--z-->il.md\n", "docs/evil.md"},
		{"x <!--\n\n<!--> T <!-->@docs/ev<!--z-->il.md\n", "docs/evil.md"},
		{"`<!--`\n\n<!---><!--->@docs/ev<!--z-->il.md\n", "docs/evil.md"},
		{"- <!-->a<!-->@docs/ev<!--z-->il.md\n\n`<!--`\n", "docs/evil.md"},
		{"<!--a-->@docs/ev<!--b-->il<!--x.md\n\nMore text.\n<!-- end -->\n", "docs/evil<!--x.md"},
		{"x <!--\r<!-->a<!-->@docs/ev<!--z-->il.md\n", "docs/evil.md"},
		{"<!--a-->@docs/ev<!--b-->il<!--x.md\r\rMore text.\r<!-- end -->\n", "docs/evil<!--x.md"},
	} {
		if got, more := ImportPaths(c.text, 100); !slices.Contains(got, c.want) || more {
			t.Errorf("ImportPaths(%q) = %q, %v; want %q among them", c.text, got, more, c.want)
		}
	}
}

// A path with an escaped space is a copy of its text ("\ " a space), and a
// run of many @ holds many such paths, each the rest of the run: ImportPaths
// copies a run once per read, whose paths all share it, so what it gives
// keeps about its text's size (31 @ in 4 MiB kept 124 MiB when each @
// copied the rest of the run, and both walks keep a source's imports). The
// text is kept alive to the last measure, so its own bytes are not counted
// freed against the copy.
func TestImportPathsKeepsOneCopyOfARunWithEscapedSpaces(t *testing.T) {
	text := strings.Repeat("@a\\ ", 31) + strings.Repeat("b", 4<<20)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, more := ImportPaths(text, 100)
	runtime.GC()
	runtime.ReadMemStats(&after)
	kept := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if len(got) < 31 || more {
		t.Fatalf("ImportPaths = %d paths, more %v; want 31 or more", len(got), more)
	}
	if kept > 3*int64(len(text))/2 {
		t.Fatalf("ImportPaths of %d bytes keeps %d KiB", len(text), kept>>10)
	}
	runtime.KeepAlive(got)
	runtime.KeepAlive(text)
}

// A session loads what a rules file imports right after its frontmatter:
// the imported file is one of ProjectLinks' targets.
func TestASessionReadsAnImportAfterAFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{".claude/settings.json": "{}", ".claude/rules/api.md": "---\npaths: src/**\n---@../../.docs/evil.md\n",
		".docs/evil.md": "x\n"})
	if targets, partial := ProjectLinks(t.Context(), KindClaude, dir); !slices.Contains(targets, ".docs/evil.md") || partial {
		t.Fatalf("ProjectLinks = %q, %v; want .docs/evil.md", targets, partial)
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
		if targets, partial := ProjectLinks(t.Context(), KindClaude, dir); !slices.Contains(targets, ".docs/guide.md") || partial {
			t.Errorf("CLAUDE.md %q: ProjectLinks = %q, %v; want .docs/guide.md", text, targets, partial)
		}
	}
}

// One file may be named many ways (here 300 links to it): the session reads
// it once per walk, however many paths name it (its device and inode), so
// a large file there costs one read and one scan at each Claude launch, not
// one per name, and the walk stays within its budget, whole.
func TestASessionReadsAnImportedFileOncePerWalk(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := range 300 {
		fmt.Fprintf(&b, "@.docs/l%d.md\n", i)
	}
	writeFiles(t, dir, map[string]string{"CLAUDE.md": b.String(), ".docs/big.md": strings.Repeat("@x ", (4<<20)/3), ".docs/x": "x\n"})
	for i := range 300 {
		if err := os.Symlink("big.md", filepath.Join(dir, ".docs", fmt.Sprintf("l%d.md", i))); err != nil {
			t.Fatal(err)
		}
	}
	type walk struct {
		targets []string
		partial bool
	}
	done := make(chan walk, 1)
	start := time.Now()
	go func() {
		targets, partial := ProjectLinks(t.Context(), KindClaude, dir)
		done <- walk{targets, partial}
	}()
	select {
	case w := <-done:
		if !slices.Contains(w.targets, ".docs/big.md") || !slices.Contains(w.targets, ".docs/x") || w.partial {
			t.Fatalf("ProjectLinks = %d targets, partial %v; want .docs/big.md and what it imports, whole", len(w.targets), w.partial)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("ProjectLinks still runs after %s", time.Since(start))
	}
}

// Every 4 KiB the session reads is a step of its walk, and every import:
// files each within the budget may pass it together (eleven rules files
// of 4 MiB, three of 4,000 imports), and the walk is then partial, as it
// is for a context that ended before a read.
func TestASessionsImportsOfManyFilesStayWithinTheWalksBudget(t *testing.T) {
	big := t.TempDir()
	writeFiles(t, big, map[string]string{"CLAUDE.md": "x\n"})
	for i := range 11 {
		name := filepath.Join(big, ".claude", "rules", fmt.Sprintf("r%d.md", i))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(name, maxImportBytes); err != nil { // sparse: nothing is written
			t.Fatal(err)
		}
	}
	many := t.TempDir()
	files := map[string]string{}
	for i := range 3 {
		var b strings.Builder
		for j := range 4000 {
			fmt.Fprintf(&b, "@f%d-%d.md\n", i, j)
		}
		files[fmt.Sprintf(".claude/rules/r%d.md", i)] = b.String()
	}
	writeFiles(t, many, files)
	for name, dir := range map[string]string{"eleven files of 4 MiB": big, "three files of 4,000 imports": many} {
		if _, partial := ProjectLinks(t.Context(), KindClaude, dir); !partial {
			t.Errorf("%s: ProjectLinks not partial", name)
		}
	}
	small := t.TempDir()
	writeFiles(t, small, map[string]string{"CLAUDE.md": "@a.md\n", "a.md": "x\n"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, partial := ProjectLinks(ctx, KindClaude, small); !partial {
		t.Error("ProjectLinks with a context that ended: not partial")
	}
	if targets, partial := ProjectLinks(t.Context(), KindClaude, small); !slices.Equal(targets, []string{"a.md"}) || partial {
		t.Errorf("ProjectLinks = %q, %v; want a.md", targets, partial)
	}
}

// Claude Code loads no directory an import names (a stat that is no file
// is skipped), and ImportPaths reads code and prose wide: a regex in a
// code span (`^.*@.*$`) gives the root, @config[:x] in a fenced block or
// "see @docs" the directory. The session takes such an import as that
// path alone (ExactTarget): a change under it is none to what Claude
// loads, and only a commit that makes the path a file or a link, or a path
// on the way to it a link, changes it (the reload check matches both ways).
func TestASessionTakesAnImportOfADirectoryAsThatPathAlone(t *testing.T) {
	files := map[string]string{"CLAUDE.md": "Validate e-mail with `^.*@.*$` first.\n\n```ruby\n@config[:x]\n```\n\nSee @docs for more.\n",
		"config/database.yml": "x\n", "docs/guide.md": "y\n"}
	e, dir, ws := projectEnv(t, files, []string{"config/database.yml", "docs/guide.md"}, nil)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	if n, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindClaude); ok {
		t.Fatalf("declined for %q: a change under a directory an import names", n.Paths)
	}
	targets, partial := ProjectLinks(t.Context(), KindClaude, dir)
	if slices.Contains(targets, "") || slices.Contains(targets, "config") || !slices.Contains(targets, ExactTarget("config")) || partial {
		t.Fatalf("ProjectLinks = %q, %v; want config alone, and not the root", targets, partial)
	}
	for _, c := range []struct {
		f    string
		want bool
	}{{"config", true}, {"Config", true}, {"config/database.yml", false}, {"conf", false}} {
		if got := UnderTarget(c.f, ExactTarget("config")) || UnderTarget(ExactTarget("config"), c.f); got != c.want {
			t.Errorf("%q matches %q: %v, want %v", c.f, ExactTarget("config"), got, c.want)
		}
	}
}
