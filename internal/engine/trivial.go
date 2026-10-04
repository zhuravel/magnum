package engine

// Trivial deltas: a push after a review that only edits comments, whitespace
// or documentation is not worth a re-review ([daemon] skip_trivial_deltas).
// TrivialDelta reads the per-file patches of reviewed...head and says no
// whenever it cannot be sure: a missing or truncated patch, a file added,
// removed or renamed, a file type without known comment syntax, a comment
// that changes behaviour (a shebang, a magic comment, a build directive),
// code commented out or back in, or a line that moved.

import (
	"maps"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
)

// Delta classes ([daemon] skip_trivial_deltas).
const (
	DeltaComments   = "comments"
	DeltaWhitespace = "whitespace"
	DeltaDocs       = "docs"
)

// DeltaClasses lists every class, the default of skip_trivial_deltas
// (config.TrivialDeltaClasses, the list the configuration accepts).
var DeltaClasses = config.TrivialDeltaClasses

// TrivialDelta reports whether a delta needs no re-review under the allowed
// classes, and the classes it used (sorted, unique; nil when not trivial).
// The delta is trivial when it has at least one file and every file is: a
// modified file with a complete patch that is documentation (docs), or whose
// every added and removed line is blank or differs from its counterpart only
// in whitespace (whitespace) or is a comment of the file's type (comments).
// A class missing from allowed makes the files that need it non-trivial, so
// an empty allowed never skips.
func TrivialDelta(files []github.FileDelta, allowed []string) (classes []string, trivial bool) {
	if len(files) == 0 {
		return nil, false
	}
	used := map[string]bool{}
	for _, f := range files {
		fc, ok := deltaFileClasses(f, allowed)
		if !ok {
			return nil, false
		}
		for _, c := range fc {
			used[c] = true
		}
	}
	return slices.Sorted(maps.Keys(used)), true
}

// DeltaLabel is how a review note and events name the classes: "comments
// only", "whitespace only", "docs only", "comments and whitespace only",
// "comments, whitespace and docs only". The order is always comments,
// whitespace, docs, whatever the order given; unknown classes are left out
// and no known class at all is "".
func DeltaLabel(classes []string) string {
	var names []string
	for _, c := range DeltaClasses {
		if slices.Contains(classes, c) {
			names = append(names, c)
		}
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0] + " only"
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " only"
}

// deltaFileClasses returns the classes one file's change uses; ok is false
// when the file needs a re-review. A documentation file is docs without
// looking at its lines when docs is allowed; otherwise its lines are judged
// like any other file's (a .md file has no comment syntax, so only its
// whitespace changes pass).
func deltaFileClasses(f github.FileDelta, allowed []string) (classes []string, ok bool) {
	if f.Truncated || f.Status != "modified" || f.Patch == "" {
		return nil, false
	}
	if docDeltaPath(f.Path) && slices.Contains(allowed, DeltaDocs) {
		return []string{DeltaDocs}, true
	}
	classes, ok = deltaPatchClasses(f.Path, f.Patch)
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

// docDeltaPath reports whether a file is documentation: a .md, .txt, .rst
// or .adoc file (any case), or anything under docs/. A .txt file that a
// build or a site reads (CMakeLists.txt, robots.txt, pip requirements or
// constraints) is not.
func docDeltaPath(p string) bool {
	if strings.HasPrefix(p, "docs/") {
		return true
	}
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".rst", ".adoc":
		return true
	case ".txt":
		base := strings.ToLower(path.Base(p))
		return base != "cmakelists.txt" && base != "robots.txt" &&
			!strings.Contains(base, "requirements") && !strings.Contains(base, "constraints")
	}
	return false
}

// commentSyntax is the comment syntax of a file type.
type commentSyntax int

const (
	commentNone commentSyntax = iota // no comment syntax known: every line is code
	commentHash                      // "# ..."
	commentC                         // "// ..." and "/* ... */"
	commentSQL                       // "-- ..."
	commentHTML                      // "<!-- ... -->"
	commentERB                       // "<!-- ... -->" and "<%# ... %>"
)

// fileCommentSyntax picks the comment syntax by extension (any case) or by
// base name (exact).
func fileCommentSyntax(p string) commentSyntax {
	base := path.Base(p)
	switch {
	case base == "Gemfile", base == "Rakefile", base == "Dockerfile", strings.HasPrefix(base, "Dockerfile."):
		return commentHash
	}
	switch strings.ToLower(path.Ext(p)) {
	case ".rb", ".rake", ".py", ".sh", ".yml", ".yaml", ".toml", ".tf", ".gemspec", ".rbi":
		return commentHash
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".java", ".kt", ".swift", ".c", ".h", ".cpp", ".cs", ".scss":
		return commentC
	case ".sql":
		return commentSQL
	case ".html", ".vue":
		return commentHTML
	case ".erb":
		return commentERB
	}
	return commentNone
}

// indentSignificant reports whether leading whitespace is syntax in the file
// (Python, YAML, a Makefile), so re-indenting a line there is a change.
func indentSignificant(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".py", ".yml", ".yaml":
		return true
	}
	return path.Base(p) == "Makefile"
}

// blockState is what one side of a hunk knows about block comments (/* */ or
// <!-- -->) before a line. A hunk may start inside a block, so each starts
// unknown.
type blockState int

const (
	blockUnknown blockState = iota
	blockOut
	blockIn
)

// deltaLine is one added or removed line that is not blank.
type deltaLine struct {
	norm    string // the line normalized (normalizeDeltaLine)
	comment bool
}

// deltaPatchClasses reads one file's patch hunk by hunk. A blank added or
// removed line is whitespace. The other added and removed lines between two
// context lines (a change run) are judged by deltaRunClasses: their code
// must stay the same sequence, so a moved or swapped line is a change. The
// block state is tracked per hunk, apart for the old side (context and
// removed lines) and the new side (context and added lines); a context line
// inside a block on one side only (code commented out, or back in, by an
// added or removed delimiter) is a change too. ok is false on a change or a
// line it cannot read.
func deltaPatchClasses(p, patch string) (classes []string, ok bool) {
	syntax := fileCommentSyntax(p)
	indent := indentSignificant(p)
	used := map[string]bool{}
	var removed, added []deltaLine
	oldState, newState := blockUnknown, blockUnknown
	// settle ends a change run and checks that both sides agree on being in
	// a block; false when either fails.
	settle := func() bool {
		ok := deltaRunClasses(removed, added, used)
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

// deltaRunClasses judges one change run, adding the classes it uses to used.
// The code lines removed and added must be the same sequence once
// normalized: they changed only in whitespace (whitespace). A comment line
// pairs with an equal comment of the other side (whitespace) or stays a
// comment (comments). It reports false when the code changed.
func deltaRunClasses(removed, added []deltaLine, used map[string]bool) bool {
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

// normalizeDeltaLine collapses every run of whitespace in text to one space
// and trims both ends; in an indentation-significant file the leading
// whitespace is kept exactly.
func normalizeDeltaLine(text string, indent bool) string {
	rest := strings.TrimLeftFunc(text, unicode.IsSpace)
	norm := strings.Join(strings.Fields(rest), " ")
	if indent {
		return text[:len(text)-len(rest)] + norm
	}
	return norm
}

// classify reports whether the trimmed line t is all comment, given the
// block state before it, and returns the state after it. Only comment lines
// open a block: a code line keeps the state (a "/*" in code may sit in a
// string), so the lines after it count as code.
func (s commentSyntax) classify(t string, state blockState) (bool, blockState) {
	switch s {
	case commentHash:
		return strings.HasPrefix(t, "#") && !hashDirective(t), state
	case commentSQL:
		// MySQL needs whitespace after "--"; "--1" is minus minus one.
		return t == "--" || strings.HasPrefix(t, "-- ") || strings.HasPrefix(t, "--\t"), state
	case commentC:
		return blockComment(t, state, cDelims)
	case commentHTML:
		return blockComment(t, state, htmlDelims)
	case commentERB:
		comment, next := blockComment(t, state, htmlDelims)
		if strings.Contains(t, "<%") {
			// ERB runs its tags inside an HTML comment too.
			comment = erbCommentTag(t)
		}
		return comment, next
	}
	return false, state
}

// commentDelims are the tokens of a language with block comments.
type commentDelims struct {
	open, close string
	line        string // the line comment token; "" when there is none
	stars       bool   // "* ..." lines continue a block
}

var (
	cDelims    = commentDelims{open: "/*", close: "*/", line: "//", stars: true}
	htmlDelims = commentDelims{open: "<!--", close: "-->"}
)

// blockComment classifies a trimmed line of a language with block comments.
// Inside a block a line is comment up to the closing token; text after a
// closing token is judged again, so "/* c */ x := 1" is code. A line opening
// a block without closing it sets the state. While the state is unknown (the
// hunk may start inside a block) a line written like a block continuation
// ("*", "* text", "*/") is comment; once a block is known closed it is code
// ("* qty" continuing a product).
func blockComment(t string, state blockState, d commentDelims) (bool, blockState) {
	if state == blockIn {
		i := strings.Index(t, d.close)
		if i < 0 {
			return true, blockIn
		}
		return blockComment(strings.TrimSpace(t[i+len(d.close):]), blockOut, d)
	}
	var rest string
	switch {
	case t == "":
		return true, state
	case d.line != "" && strings.HasPrefix(t, d.line):
		return !slashDirective(t), state
	case strings.HasPrefix(t, d.open):
		rest = t[len(d.open):]
	case d.stars && state == blockUnknown && (t == "*" || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "*/")):
		rest = t
	default:
		return false, state
	}
	i := strings.Index(rest, d.close)
	if i < 0 {
		if strings.HasPrefix(t, d.open) {
			return true, blockIn
		}
		return true, state
	}
	return blockComment(strings.TrimSpace(rest[i+len(d.close):]), blockOut, d)
}

// hashDirective reports whether a "#" comment changes behaviour: a shebang,
// a Ruby magic comment, a Python or Ruby source encoding, a Sorbet sigil or a
// Dockerfile parser directive.
func hashDirective(t string) bool {
	if strings.HasPrefix(t, "#!") {
		return true
	}
	body := strings.ToLower(strings.TrimSpace(t[1:]))
	if strings.HasPrefix(body, "-*-") || strings.Contains(body, "coding:") || strings.Contains(body, "coding=") {
		return true
	}
	body = strings.ReplaceAll(body, "-", "_")
	for _, p := range []string{"frozen_string_literal:", "shareable_constant_value:", "warn_indent:", "warn_past_scope:", "typed:", "syntax=", "escape=", "check="} {
		if strings.HasPrefix(body, p) {
			return true
		}
	}
	return false
}

// slashDirective reports whether a "//" comment is read by a tool: a Go
// directive (//go:build, //go:embed, //line, //export, // +build, a cgo
// "#cgo" or "#include" line), a TypeScript triple-slash reference or @ts-
// pragma, or the Swift tools version.
func slashDirective(t string) bool {
	for _, p := range []string{"//go:", "//line ", "//export "} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	body := strings.TrimSpace(strings.TrimLeft(t, "/"))
	for _, p := range []string{"+build", "#", "<reference", "<amd-", "@ts-", "swift-tools-version"} {
		if strings.HasPrefix(body, p) {
			return true
		}
	}
	return false
}

// erbCommentTag reports whether t is one whole ERB comment tag, "<%# ... %>"
// with no other tag on the line.
func erbCommentTag(t string) bool {
	return strings.HasPrefix(t, "<%#") && strings.Index(t, "%>") == len(t)-2 && strings.Count(t, "<%") == 1
}

// DeltaSize is how big a delta is for the re-review threshold ([daemon]
// rereview_min_lines).
type DeltaSize struct {
	// Lines counts the added and removed lines that change code: not
	// blank, not a comment, not a line that only moved in whitespace
	// within its run of changes, not in a documentation file.
	Lines int `json:"lines"`
	// AddedFiles counts the files added, renamed or copied.
	AddedFiles int `json:"added_files"`
	// Complete is false when a file other than an added one had no
	// complete patch (binary, too large, a cut-off file list): its lines
	// are unknown, so the threshold cannot hold the delta back.
	Complete bool `json:"complete"`
}

// MeasureDelta counts a delta's lines the way TrivialDelta judges them, with
// every class allowed, but line by line instead of all or nothing: a
// comment, a blank line, a documentation file and a line whose
// counterpart in the same run of changes differs only in whitespace count
// nothing; every other added or removed line counts one, and so does each
// line of a run whose code lines only changed order. A context line that
// one side has inside a block comment and the other does not (code
// commented out, or back in) counts one, like a line it cannot read.
func MeasureDelta(files []github.FileDelta) DeltaSize {
	s := DeltaSize{Complete: true}
	for _, f := range files {
		switch {
		case f.Status == "added" || f.Status == "renamed" || f.Status == "copied":
			s.AddedFiles++
		case f.Truncated || f.Patch == "":
			s.Complete = false
		case !docDeltaPath(f.Path):
			s.Lines += patchChangedLines(f.Path, f.Patch)
		}
	}
	return s
}

// patchChangedLines counts one file's changed code lines (MeasureDelta),
// walking the patch like deltaPatchClasses.
func patchChangedLines(p, patch string) int {
	syntax := fileCommentSyntax(p)
	indent := indentSignificant(p)
	n := 0
	var removed, added []deltaLine
	oldState, newState := blockUnknown, blockUnknown
	// settle ends a change run; a block that one side is in and the other
	// is not makes the next context line a change.
	settle := func() {
		n += runChangedLines(removed, added)
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

// runChangedLines counts the code lines of one change run that changed:
// comment lines count nothing; a code line whose normalized text the other
// side of the run has too only moved in whitespace; the rest count one
// each. Code lines that are all matched but in another order count all.
func runChangedLines(removed, added []deltaLine) int {
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
