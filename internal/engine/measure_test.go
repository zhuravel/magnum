package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/github"
)

// measurePatch joins patch lines; a trailing newline is not part of GitHub's
// patches, so none is added.
func measurePatch(lines ...string) string { return strings.Join(lines, "\n") }

// measureCase is one MeasureDelta row: the files and the expected size.
type measureCase struct {
	name     string
	files    []github.FileDelta
	lines    int
	added    int
	complete bool
}

func runMeasure(t *testing.T, cases []measureCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MeasureDelta(tc.files)
			if got.Lines != tc.lines || got.AddedFiles != tc.added || got.Complete != tc.complete {
				t.Fatalf("MeasureDelta = %+v, want {Lines:%d AddedFiles:%d Complete:%v}", got, tc.lines, tc.added, tc.complete)
			}
		})
	}
}

// measureFile is a case over one modified file with a complete patch.
func measureFile(name, path string, lines int, patch ...string) measureCase {
	return measureCase{name: name, files: []github.FileDelta{modifiedFile(path, measurePatch(patch...))}, lines: lines, complete: true}
}

func TestMeasureDeltaCodeLines(t *testing.T) {
	runMeasure(t, []measureCase{
		measureFile("a one-line modification counts the removed and the added line", "app/models/coupon.rb", 2,
			"@@ -1,3 +1,3 @@", " class Coupon", "-  LIMIT = 5", "+  LIMIT = 6", " end"),
		measureFile("added code lines count one each", "app/models/coupon.rb", 3,
			"@@ -1,2 +1,5 @@", " class Coupon", "+  A = 1", "+  B = 2", "+  C = 3", " end"),
		measureFile("removed code lines count one each", "app/models/coupon.rb", 2,
			"@@ -1,4 +1,2 @@", " class Coupon", "-  A = 1", "-  B = 2", " end"),
		measureFile("a comment next to a code change counts only the code", "app/models/coupon.rb", 2,
			"@@ -1,4 +1,4 @@", " class Coupon", "-  # old note", "+  # new note", "-  LIMIT = 5", "+  LIMIT = 6", " end"),
		measureFile("a line unchanged on the other side of the run counts nothing", "app/models/coupon.rb", 2,
			"@@ -1,3 +1,3 @@", " def call", "-  a", "-  b", "+  a", "+  c", " end"),
		measureFile("a duplicate line is matched once", "app/models/coupon.rb", 1,
			"@@ -1,3 +1,2 @@", " def call", "-  a", "-  a", "+  a", " end"),
		measureFile("a line moved within its run keeps matching the other side", "app/models/coupon.rb", 2,
			"@@ -1,3 +1,3 @@", " def call", "-  a", "-  b", "+  b", "+  c", " end"),
		measureFile("a go line with the no-newline marker is the same line", "internal/example/limit.go", 0,
			"@@ -1 +1 @@", "-const Limit = 10", `\ No newline at end of file`, "+const Limit = 10", `\ No newline at end of file`),
		measureFile("an unreadable patch line counts one", "app/models/coupon.rb", 1,
			"@@ -1 +1 @@", "garbage"),
		measureFile("a trailing newline in the patch adds nothing", "app/models/coupon.rb", 2,
			"@@ -1,3 +1,3 @@", " class Coupon", "-  LIMIT = 5", "+  LIMIT = 6", ""),
	})
}

func TestMeasureDeltaCommentsAndBlankLinesCountNothing(t *testing.T) {
	runMeasure(t, []measureCase{
		{name: "yaml comments reworded over two hunks", files: yamlComments, lines: 0, complete: true},
		measureFile("a comment line added to ruby", "app/models/coupon.rb", 0,
			"@@ -1,2 +1,3 @@", " class Coupon", "+  # Caps the batch.", " end"),
		measureFile("a comment line removed from ruby", "app/models/coupon.rb", 0,
			"@@ -1,3 +1,2 @@", " class Coupon", "-  # Caps the batch.", " end"),
		measureFile("blank lines added", "app/models/coupon.rb", 0,
			"@@ -1,2 +1,4 @@", " class Coupon", "+", "+", " end"),
		measureFile("blank lines removed", "app/models/coupon.rb", 0,
			"@@ -1,4 +1,2 @@", " class Coupon", "-", "-", " end"),
		measureFile("whitespace-only lines are blank", "app/models/coupon.rb", 0,
			"@@ -1,2 +1,3 @@", " class Coupon", "+    \t", " end"),
		measureFile("go line comment", "internal/example/limit.go", 0,
			"@@ -1,2 +1,3 @@", " package example", "+// Limit caps the batch.", " const Limit = 10"),
		measureFile("go one-line block comment", "internal/example/limit.go", 0,
			"@@ -1,2 +1,3 @@", " package example", "+/* Limit caps the batch. */", " const Limit = 10"),
		measureFile("go multi-line block comment added whole", "internal/example/limit.go", 0,
			"@@ -1,2 +1,5 @@", " package example", "+/*", "+ * Limit caps the batch.", "+ */", " const Limit = 10"),
		measureFile("javadoc text edited between unchanged delimiters", "src/Example.java", 0,
			"@@ -1,5 +1,5 @@", " /**", "- * Old text.", "+ * New text.", "  */", " class Example {}"),
		measureFile("a hunk that starts inside a block comment", "src/Example.java", 0,
			"@@ -10,4 +10,4 @@", "  * line one", "- * old", "+ * new", "  */"),
		measureFile("sql comment", "db/query.sql", 0,
			"@@ -1,2 +1,3 @@", " select 1", "+-- a note", " from dual"),
		measureFile("html comment", "app/views/index.html", 0,
			"@@ -1,2 +1,3 @@", " <div>", "+<!-- a note -->", " </div>"),
		measureFile("erb comment tag", "app/views/index.erb", 0,
			"@@ -1,2 +1,3 @@", " <div>", "+<%# a note %>", " </div>"),
		measureFile("toml comment", "config/example.toml", 0,
			"@@ -1,2 +1,3 @@", " [daemon]", "+# a note", " poll = 1"),
		measureFile("dockerfile comment", "Dockerfile", 0,
			"@@ -1,2 +1,3 @@", " FROM scratch", "+# a note", " COPY . ."),
	})
}

func TestMeasureDeltaCodeThatLooksLikeAComment(t *testing.T) {
	runMeasure(t, []measureCase{
		measureFile("code before a block comment on the same line", "internal/example/limit.go", 1,
			"@@ -1,2 +1,3 @@", " package example", "+/* note */ x := 1", " const Limit = 10"),
		measureFile("sql minus minus one is arithmetic", "db/query.sql", 1,
			"@@ -1,2 +1,3 @@", " select 1", "+--1", " from dual"),
		measureFile("a file type without comment syntax counts a hash line", "config/example.conf", 1,
			"@@ -1,2 +1,3 @@", " port 1", "+# a note", " host x"),
		measureFile("commenting a line out counts the line", "app/models/coupon.rb", 1,
			"@@ -1,3 +1,3 @@", " class Coupon", "-  LIMIT = 5", "+  # LIMIT = 5", " end"),
		measureFile("uncommenting a line counts the line", "app/models/coupon.rb", 1,
			"@@ -1,3 +1,3 @@", " class Coupon", "-  # LIMIT = 5", "+  LIMIT = 5", " end"),
	})
}

func TestMeasureDeltaDirectivesAreCode(t *testing.T) {
	runMeasure(t, []measureCase{
		measureFile("shebang changed", "bin/run.sh", 2,
			"@@ -1,3 +1,3 @@", "-#!/bin/bash", "+#!/usr/bin/env bash", " set -e"),
		measureFile("shebang added", "bin/run.sh", 1,
			"@@ -1,2 +1,3 @@", "+#!/bin/bash", " set -e", " echo ok"),
		measureFile("frozen_string_literal flipped", "app/models/coupon.rb", 2,
			"@@ -1,3 +1,3 @@", "-# frozen_string_literal: true", "+# frozen_string_literal: false", " class Coupon"),
		measureFile("go build constraint changed", "internal/example/limit_linux.go", 2,
			"@@ -1,3 +1,3 @@", "-//go:build linux", "+//go:build darwin", " package example"),
		measureFile("a plain comment next to a directive is still free", "bin/run.sh", 0,
			"@@ -1,3 +1,4 @@", " #!/bin/bash", "+# runs the sync", " set -e"),
	})
}

func TestMeasureDeltaWhitespaceOnlyChangesCountNothing(t *testing.T) {
	runMeasure(t, []measureCase{
		measureFile("re-indented ruby", "app/models/coupon.rb", 0,
			"@@ -1,4 +1,4 @@", " def call", "-    run(1)", "+  run(1)", " end"),
		measureFile("trailing spaces removed", "app/models/coupon.rb", 0,
			"@@ -1,3 +1,3 @@", " def call", "-  run(1)  ", "+  run(1)", " end"),
		measureFile("inner whitespace collapsed", "app/models/coupon.rb", 0,
			"@@ -1,3 +1,3 @@", " def call", "-  a  =  1", "+  a = 1", " end"),
		measureFile("whitespace file without comment syntax", "config/example.json", 0,
			"@@ -1,3 +1,3 @@", " {", `-  "port":   1 `, `+  "port": 1`, " }"),
		{name: "ruby whitespace only fixture", files: whitespaceOnly, lines: 0, complete: true},
		measureFile("a block re-indented as a whole", "app/models/coupon.rb", 0,
			"@@ -1,5 +1,5 @@", " def call", "-run(1)", "-finish", "+  run(1)", "+  finish", " end"),
	})
}

func TestMeasureDeltaIndentIsCodeWhereItIsSyntax(t *testing.T) {
	runMeasure(t, []measureCase{
		measureFile("re-indented python line", "app/example.py", 2,
			"@@ -1,3 +1,3 @@", " def f():", "-    return 1", "+        return 1", " "),
		measureFile("re-indented yaml line", "config/example.yml", 2,
			"@@ -1,2 +1,2 @@", " server:", "-  port: 1", "+    port: 1"),
		measureFile("python trailing whitespace is still free", "app/example.py", 0,
			"@@ -1,3 +1,3 @@", " def f():", "-    return 1  ", "+    return 1", " "),
		measureFile("makefile recipe re-indented", "Makefile", 2,
			"@@ -1,2 +1,2 @@", " test:", "-\tgo test", "+        go test"),
	})
}

func TestMeasureDeltaReorderedLinesCountAll(t *testing.T) {
	runMeasure(t, []measureCase{
		measureFile("two go lines swapped in one run", "internal/example/call.go", 4,
			"@@ -1,4 +1,4 @@", " func call() {", "-\ta()", "-\tb()", "+\tb()", "+\ta()", " }"),
		measureFile("three lines rotated in one run", "internal/example/call.go", 6,
			"@@ -1,5 +1,5 @@", " func call() {", "-\ta()", "-\tb()", "-\tc()", "+\tb()", "+\tc()", "+\ta()", " }"),
		measureFile("a line moved past a context line", "app/models/coupon.rb", 2,
			"@@ -1,4 +1,4 @@", " def call", "-  charge", "   notify", "+  charge", " end"),
		measureFile("a swap with a comment in between is still a swap", "app/models/coupon.rb", 4,
			"@@ -1,5 +1,5 @@", " def call", "-  charge", "-  # then tell them", "-  notify", "+  notify", "+  # then tell them", "+  charge", " end"),
	})
}

func TestMeasureDeltaCodeCommentedOutByDelimiters(t *testing.T) {
	runMeasure(t, []measureCase{
		measureFile("go code wrapped in a block comment counts its context lines", "internal/example/call.go", 2,
			"@@ -1,5 +1,7 @@", " package example", "+/*", " func a() {}", " func b() {}", "+*/", " const X = 1"),
		measureFile("go code taken out of a block comment counts its context lines", "internal/example/call.go", 2,
			"@@ -1,7 +1,5 @@", " package example", "-/*", " func a() {}", " func b() {}", "-*/", " const X = 1"),
		measureFile("one line wrapped in a block comment", "internal/example/call.go", 1,
			"@@ -1,3 +1,5 @@", " package example", "+/*", " func a() {}", "+*/", " const X = 1"),
		measureFile("html markup wrapped in a comment", "app/views/index.html", 2,
			"@@ -1,4 +1,6 @@", " <div>", "+<!--", " <p>a</p>", " <p>b</p>", "+-->", " </div>"),
	})
}

func TestMeasureDeltaFilesByStatus(t *testing.T) {
	added := github.FileDelta{Path: "app/models/new.rb", Status: "added", Patch: measurePatch("@@ -0,0 +1,3 @@", "+class New", "+  A = 1", "+end")}
	renamed := github.FileDelta{Path: "app/models/new.rb", PreviousPath: "app/models/old.rb", Status: "renamed", Patch: measurePatch("@@ -1,2 +1,2 @@", " class New", "-  A = 1", "+  A = 2")}
	copied := github.FileDelta{Path: "app/models/copy.rb", PreviousPath: "app/models/old.rb", Status: "copied"}
	removed := github.FileDelta{Path: "app/models/old.rb", Status: "removed", Patch: measurePatch("@@ -1,4 +0,0 @@", "-class Old", "-  # note", "-  A = 1", "-end")}
	runMeasure(t, []measureCase{
		{name: "an added file", files: []github.FileDelta{added}, lines: 0, added: 1, complete: true},
		{name: "a renamed file counts as added and its lines are not counted", files: []github.FileDelta{renamed}, lines: 0, added: 1, complete: true},
		{name: "a copied file without a patch", files: []github.FileDelta{copied}, lines: 0, added: 1, complete: true},
		{name: "an added file without a patch is still complete", files: []github.FileDelta{{Path: "logo.png", Status: "added", Truncated: true}}, lines: 0, added: 1, complete: true},
		{name: "added, renamed and copied add up", files: []github.FileDelta{added, renamed, copied}, lines: 0, added: 3, complete: true},
		{name: "a removed file counts its removed code lines", files: []github.FileDelta{removed}, lines: 3, complete: true},
		{name: "an added file next to a modification keeps both counts", files: []github.FileDelta{added, rubyMixed[0]}, lines: 2, added: 1, complete: true},
	})
}

func TestMeasureDeltaIncompletePatches(t *testing.T) {
	complete := modifiedFile("app/models/coupon.rb", measurePatch("@@ -1,3 +1,3 @@", " class Coupon", "-  LIMIT = 5", "+  LIMIT = 6", " end"))
	cutOff := complete
	cutOff.Truncated = true
	noPatch := github.FileDelta{Path: "app/assets/logo.png", Status: "modified"}
	noPatchRemoved := github.FileDelta{Path: "app/assets/logo.png", Status: "removed"}
	runMeasure(t, []measureCase{
		{name: "a truncated modified file", files: []github.FileDelta{cutOff}, lines: 0, complete: false},
		{name: "a modified file with an empty patch", files: []github.FileDelta{noPatch}, lines: 0, complete: false},
		{name: "a removed file with an empty patch", files: []github.FileDelta{noPatchRemoved}, lines: 0, complete: false},
		{name: "other files are still counted next to an incomplete one", files: []github.FileDelta{complete, cutOff, noPatch}, lines: 2, complete: false},
		{name: "the truncated existing fixture", files: truncated, lines: 0, complete: false},
	})
}

func TestMeasureDeltaDocumentationFilesCountNothing(t *testing.T) {
	code := func(path string) []github.FileDelta {
		return []github.FileDelta{modifiedFile(path, measurePatch("@@ -1,3 +1,3 @@", " intro", "-old words", "+new words", " outro"))}
	}
	runMeasure(t, []measureCase{
		{name: "README.md", files: code("README.md"), lines: 0, complete: true},
		{name: "upper-case extension", files: code("NOTES.MD"), lines: 0, complete: true},
		{name: "rst", files: code("doc/guide.rst"), lines: 0, complete: true},
		{name: "adoc", files: code("doc/guide.adoc"), lines: 0, complete: true},
		{name: "plain text notes", files: code("notes.txt"), lines: 0, complete: true},
		{name: "anything under docs/", files: code("docs/guide/setup.yml"), lines: 0, complete: true},
		{name: "docs only existing fixture", files: docsOnly, lines: 0, complete: true},
		{name: "requirements.txt is read by a tool", files: code("requirements.txt"), lines: 2, complete: true},
		{name: "constraints file is read by a tool", files: code("pip/constraints-dev.txt"), lines: 2, complete: true},
		{name: "CMakeLists.txt is read by a build", files: code("CMakeLists.txt"), lines: 2, complete: true},
		{name: "robots.txt is read by crawlers", files: code("public/robots.txt"), lines: 2, complete: true},
		{name: "a docs directory that is not at the root", files: code("app/docs/example.rb"), lines: 2, complete: true},
	})
}

func TestMeasureDeltaSumsFiles(t *testing.T) {
	swapped := modifiedFile("internal/example/call.go", measurePatch("@@ -1,4 +1,4 @@", " func call() {", "-\ta()", "-\tb()", "+\tb()", "+\ta()", " }"))
	added := github.FileDelta{Path: "app/models/new.rb", Status: "added", Patch: "@@ -0,0 +1 @@\n+class New"}
	runMeasure(t, []measureCase{
		{
			name:     "code, comments, docs, a swap and an added file",
			files:    []github.FileDelta{rubyMixed[0], yamlComments[0], docsOnly[0], swapped, added},
			lines:    2 + 0 + 0 + 4,
			added:    1,
			complete: true,
		},
		{
			name:     "no files",
			files:    nil,
			lines:    0,
			complete: true,
		},
		{
			name:     "empty list",
			files:    []github.FileDelta{},
			lines:    0,
			complete: true,
		},
	})
}

// Every delta the all-or-nothing classifier skips measures zero lines.
func TestMeasureDeltaIsZeroForEveryTrivialDelta(t *testing.T) {
	for name, files := range map[string][]github.FileDelta{
		"yaml comments":    yamlComments,
		"docs":             docsOnly,
		"ruby whitespace":  whitespaceOnly,
		"blank line added": {modifiedFile("app/models/coupon.rb", measurePatch("@@ -1,2 +1,3 @@", " class Coupon", "+", " end"))},
		"go comment":       {modifiedFile("internal/example/limit.go", measurePatch("@@ -1,2 +1,3 @@", " package example", "+// Limit caps the batch.", " const Limit = 10"))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, trivial := TrivialDelta(files, DeltaClasses); !trivial {
				t.Fatalf("fixture %q must be a trivial delta", name)
			}
			if got := MeasureDelta(files); got.Lines != 0 || got.AddedFiles != 0 || !got.Complete {
				t.Fatalf("MeasureDelta = %+v, want an empty complete size", got)
			}
		})
	}
	// and a delta the classifier refuses measures something
	if _, trivial := TrivialDelta(rubyMixed, DeltaClasses); trivial {
		t.Fatal("rubyMixed must not be a trivial delta")
	}
	if got := MeasureDelta(rubyMixed); got.Lines != 2 {
		t.Fatalf("MeasureDelta(rubyMixed) = %+v, want 2 lines", got)
	}
}

func TestDeltaSizeJSONKeys(t *testing.T) {
	b, err := json.Marshal(DeltaSize{Lines: 8, AddedFiles: 1, Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"lines":8,"added_files":1,"complete":true}`; got != want {
		t.Fatalf("json = %s, want %s", got, want)
	}
}

// A block comment opened and not closed in the hunk may comment out code
// beyond it, so the code lines it covers count; how the end of the hunk is
// settled is left open (at least the two covered context lines count).
func TestMeasureDeltaUnclosedBlockCommentCountsTheCoveredLines(t *testing.T) {
	files := []github.FileDelta{modifiedFile("internal/example/call.go",
		measurePatch("@@ -1,4 +1,5 @@", " package example", "+/*", " func a() {}", " func b() {}"))}
	if got := MeasureDelta(files); got.Lines < 2 || !got.Complete || got.AddedFiles != 0 {
		t.Fatalf("MeasureDelta = %+v, want at least the 2 commented-out context lines", got)
	}
}
