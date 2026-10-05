package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/github"
)

// walkPaths are file paths of every comment syntax, indentation rule and
// documentation kind the walker tells apart.
var walkPaths = []string{
	"app/models/coupon.rb", "internal/example/limit.go", "config/workers.yml", "scripts/sync.py",
	"db/schema.sql", "app/views/index.html", "app/views/show.html.erb", "Makefile", "Dockerfile",
	"README.md", "docs/guide/setup.yml", "bin/run", "web/app.tsx", "lib/tasks/sync.rake",
}

// patchCorpus collects the patches of this package's tests: every string
// literal that starts a hunk ("@@") and every measurePatch call made of
// literals.
func patchCorpus(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	fset := token.NewFileSet()
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "measurePatch" {
					var lines []string
					for _, a := range x.Args {
						lit, ok := a.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							return true
						}
						s, err := strconv.Unquote(lit.Value)
						if err != nil {
							return true
						}
						lines = append(lines, s)
					}
					out = append(out, strings.Join(lines, "\n"))
				}
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				if s, err := strconv.Unquote(x.Value); err == nil && strings.HasPrefix(s, "@@") {
					out = append(out, s)
				}
			}
			return true
		})
	}
	for _, files := range [][]github.FileDelta{yamlComments, rubyMixed, docsOnly, whitespaceOnly, ownBefore, ownAfterMerge, ownAfterConflict, masterFiles()} {
		for _, f := range files {
			out = append(out, f.Patch)
		}
	}
	return out
}

// walkLines are the patch lines randomPatch draws from: hunk headers,
// context, code, comments of each syntax, block delimiters, directives,
// blank lines, the no-newline marker and a line no patch has.
var walkLines = []string{
	"@@ -1,3 +1,3 @@", "@@ -10 +10,2 @@ def call", " context", " x = 1", " /* open", " */", " * still in", " <!--", " -->",
	"+  x = 1", "-  x = 1", "+    x  =  1", "-x = 1", "+  y = 2", "-  y = 2", "+\tx = 1",
	"+  # note", "-  # note", "+# note", "+  // note", "-  // note", "+/*", "-/*", "+*/", "-*/", "+ * star", "+/* c */ x := 1",
	"+<!-- c -->", "-<!-- c -->", "+<%# erb %>", "+<%= x %>", "+-- sql note", "+--1", "+#!/bin/sh", "+# frozen_string_literal: true",
	"+//go:build linux", "+// +build linux", "+", "-", "+   ", `\ No newline at end of file`, "garbage", "",
}

// randomPatch is a patch of up to 14 lines drawn from walkLines.
func randomPatch(r *rand.Rand) string {
	n := 1 + r.IntN(14)
	lines := make([]string, 0, n+1)
	if r.IntN(4) > 0 {
		lines = append(lines, "@@ -1,4 +1,4 @@")
	}
	for range n {
		lines = append(lines, walkLines[r.IntN(len(walkLines))])
	}
	return strings.Join(lines, "\n")
}

// walkCorpus is the test corpus and 20000 random patches.
func walkCorpus(t *testing.T) []string {
	t.Helper()
	corpus := patchCorpus(t)
	if len(corpus) < 50 {
		t.Fatalf("corpus of %d patches; the test files were not read", len(corpus))
	}
	r := rand.New(rand.NewPCG(5, 2026))
	for range 20000 {
		corpus = append(corpus, randomPatch(r))
	}
	return corpus
}

// The merged walker gives what the two walkers it replaced gave on every
// patch of this package's tests and on random ones, under every file type:
// the changed-line count of patchChangedLines, and, exactly when that count
// is 0, the classes deltaPatchClasses accepted the patch with.
func TestWalkPatchEqualsTheOldWalkers(t *testing.T) {
	for _, patch := range walkCorpus(t) {
		for _, p := range walkPaths {
			lines, classes := walkPatch(p, patch)
			wantLines := oldPatchChangedLines(p, patch)
			wantClasses, trivial := oldDeltaPatchClasses(p, patch)
			if lines != wantLines || (lines == 0) != trivial || (trivial && !slices.Equal(classes, wantClasses)) {
				t.Fatalf("%s:\n%s\nwalkPatch = %d %q; old walkers = %d, %q %v", p, patch, lines, classes, wantLines, wantClasses, trivial)
			}
		}
	}
}

// One walk of a delta answers both questions as the two walks did:
// TrivialDelta and MeasureDelta equal the old ones (built on the old
// walkers) on random deltas of the corpus' patches, with random statuses,
// truncation, documentation files and allowed classes.
func TestOneWalkOfADeltaEqualsTheOldTwo(t *testing.T) {
	corpus := walkCorpus(t)
	r := rand.New(rand.NewPCG(7, 2026))
	statuses := []string{"modified", "modified", "modified", "added", "removed", "renamed", "copied", "changed"}
	for range 20000 {
		files := make([]github.FileDelta, r.IntN(4))
		for i := range files {
			files[i] = github.FileDelta{Path: walkPaths[r.IntN(len(walkPaths))], Status: statuses[r.IntN(len(statuses))],
				Patch: corpus[r.IntN(len(corpus))], Truncated: r.IntN(12) == 0}
			if r.IntN(15) == 0 {
				files[i].Patch = ""
			}
		}
		var allowed []string
		for _, c := range []string{DeltaComments, DeltaWhitespace, DeltaDocs} {
			if r.IntN(3) > 0 {
				allowed = append(allowed, c)
			}
		}
		classes, trivial := TrivialDelta(files, allowed)
		wantClasses, wantTrivial := oldTrivialDelta(files, allowed)
		if trivial != wantTrivial || !slices.Equal(classes, wantClasses) {
			t.Fatalf("TrivialDelta(%+v, %q) = %q %v; the old one %q %v", files, allowed, classes, trivial, wantClasses, wantTrivial)
		}
		if got, want := MeasureDelta(files), oldMeasureDelta(files); got != want {
			t.Fatalf("MeasureDelta(%+v) = %+v; the old one %+v", files, got, want)
		}
	}
}

// The walkers that walkPatch and assessDelta replaced, kept as the oracle of
// the two tests above.

func oldTrivialDelta(files []github.FileDelta, allowed []string) (classes []string, trivial bool) {
	if len(files) == 0 {
		return nil, false
	}
	used := map[string]bool{}
	for _, f := range files {
		fc, ok := oldDeltaFileClasses(f, allowed)
		if !ok {
			return nil, false
		}
		for _, c := range fc {
			used[c] = true
		}
	}
	return slices.Sorted(maps.Keys(used)), true
}

func oldDeltaFileClasses(f github.FileDelta, allowed []string) (classes []string, ok bool) {
	if f.Truncated || f.Status != "modified" || f.Patch == "" {
		return nil, false
	}
	if docDeltaPath(f.Path) && slices.Contains(allowed, DeltaDocs) {
		return []string{DeltaDocs}, true
	}
	classes, ok = oldDeltaPatchClasses(f.Path, f.Patch)
	if !ok || len(classes) == 0 {
		return nil, false
	}
	for _, c := range classes {
		if !slices.Contains(allowed, c) {
			return nil, false
		}
	}
	return classes, true
}

func oldMeasureDelta(files []github.FileDelta) DeltaSize {
	s := DeltaSize{Complete: true}
	for _, f := range files {
		switch {
		case f.Status == "added" || f.Status == "renamed" || f.Status == "copied":
			s.AddedFiles++
		case f.Truncated || f.Patch == "":
			s.Complete = false
		case !docDeltaPath(f.Path):
			s.Lines += oldPatchChangedLines(f.Path, f.Patch)
		}
	}
	return s
}

// oldDeltaPatchClasses reads one file's patch hunk by hunk. A blank added or
// removed line is whitespace. The other added and removed lines between two
// context lines (a change run) are judged by oldDeltaRunClasses: their code
// must stay the same sequence, so a moved or swapped line is a change. The
// block state is tracked per hunk, apart for the old side (context and
// removed lines) and the new side (context and added lines); a context line
// inside a block on one side only (code commented out, or back in, by an
// added or removed delimiter) is a change too. ok is false on a change or a
// line it cannot read.
func oldDeltaPatchClasses(p, patch string) (classes []string, ok bool) {
	syntax := fileCommentSyntax(p)
	indent := indentSignificant(p)
	used := map[string]bool{}
	var removed, added []deltaLine
	oldState, newState := blockUnknown, blockUnknown
	// settle ends a change run and checks that both sides agree on being in
	// a block; false when either fails.
	settle := func() bool {
		ok := oldDeltaRunClasses(removed, added, used)
		removed, added = removed[:0], added[:0]
		return ok && (oldState == blockIn) == (newState == blockIn)
	}
	for _, l := range strings.Split(patch, "\n") {
		if strings.HasPrefix(l, "@@") {
			if !settle() {
				return nil, false
			}
			oldState, newState = blockUnknown, blockUnknown
			continue
		}
		if l == "" {
			l = " "
		}
		text := l[1:]
		t := strings.TrimSpace(text)
		switch l[0] {
		case '\\': // "\ No newline at end of file"
		case ' ':
			if !settle() {
				return nil, false
			}
			_, oldState = syntax.classify(t, oldState)
			_, newState = syntax.classify(t, newState)
		case '-', '+':
			if t == "" {
				used[DeltaWhitespace] = true
				continue
			}
			state, side := &newState, &added
			if l[0] == '-' {
				state, side = &oldState, &removed
			}
			var comment bool
			comment, *state = syntax.classify(t, *state)
			*side = append(*side, deltaLine{norm: normalizeDeltaLine(text, indent), comment: comment})
		default:
			return nil, false
		}
	}
	if !settle() {
		return nil, false
	}
	return slices.Sorted(maps.Keys(used)), true
}

// oldDeltaRunClasses judges one change run, adding the classes it uses to used.
// The code lines removed and added must be the same sequence once
// normalized: they changed only in whitespace (whitespace). A comment line
// pairs with an equal comment of the other side (whitespace) or stays a
// comment (comments). It reports false when the code changed.
func oldDeltaRunClasses(removed, added []deltaLine, used map[string]bool) bool {
	var oldCode, newCode []string
	comments := map[string]int{}
	for _, l := range removed {
		if l.comment {
			comments[l.norm]++
		} else {
			oldCode = append(oldCode, l.norm)
		}
	}
	for _, l := range added {
		switch {
		case !l.comment:
			newCode = append(newCode, l.norm)
		case comments[l.norm] > 0:
			comments[l.norm]--
			used[DeltaWhitespace] = true
		default:
			used[DeltaComments] = true
		}
	}
	if !slices.Equal(oldCode, newCode) {
		return false
	}
	if len(oldCode) > 0 {
		used[DeltaWhitespace] = true
	}
	for _, n := range comments {
		if n > 0 {
			used[DeltaComments] = true
		}
	}
	return true
}

// oldPatchChangedLines counts one file's changed code lines (MeasureDelta),
// walking the patch like oldDeltaPatchClasses.
func oldPatchChangedLines(p, patch string) int {
	syntax := fileCommentSyntax(p)
	indent := indentSignificant(p)
	n := 0
	var removed, added []deltaLine
	oldState, newState := blockUnknown, blockUnknown
	// settle ends a change run; a block that one side is in and the other
	// is not makes the next context line a change.
	settle := func() {
		n += oldRunChangedLines(removed, added)
		removed, added = removed[:0], added[:0]
		if (oldState == blockIn) != (newState == blockIn) {
			n++
		}
	}
	for _, l := range strings.Split(patch, "\n") {
		if strings.HasPrefix(l, "@@") {
			settle()
			oldState, newState = blockUnknown, blockUnknown
			continue
		}
		if l == "" {
			l = " "
		}
		text := l[1:]
		t := strings.TrimSpace(text)
		switch l[0] {
		case '\\': // "\ No newline at end of file"
		case ' ':
			settle()
			_, oldState = syntax.classify(t, oldState)
			_, newState = syntax.classify(t, newState)
		case '-', '+':
			if t == "" {
				continue // a blank line
			}
			state, side := &newState, &added
			if l[0] == '-' {
				state, side = &oldState, &removed
			}
			var comment bool
			comment, *state = syntax.classify(t, *state)
			*side = append(*side, deltaLine{norm: normalizeDeltaLine(text, indent), comment: comment})
		default:
			n++
		}
	}
	settle()
	return n
}

// oldRunChangedLines counts the code lines of one change run that changed:
// comment lines count nothing; a code line whose normalized text the other
// side of the run has too only moved in whitespace; the rest count one
// each. Code lines that are all matched but in another order count all.
func oldRunChangedLines(removed, added []deltaLine) int {
	var oldCode, newCode []string
	for _, l := range removed {
		if !l.comment {
			oldCode = append(oldCode, l.norm)
		}
	}
	for _, l := range added {
		if !l.comment {
			newCode = append(newCode, l.norm)
		}
	}
	if slices.Equal(oldCode, newCode) {
		return 0
	}
	left := map[string]int{}
	for _, c := range oldCode {
		left[c]++
	}
	matched := 0
	for _, c := range newCode {
		if left[c] > 0 {
			left[c]--
			matched++
		}
	}
	if changed := len(oldCode) + len(newCode) - 2*matched; changed > 0 {
		return changed
	}
	return len(oldCode) + len(newCode)
}
