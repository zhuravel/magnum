package agents

// Claude Code's @path imports: a CLAUDE.md, a CLAUDE.local.md, an
// AGENTS.md, a rules file under .claude/rules/ and what they import load
// the files they import (code.claude.com/docs memory "Import additional
// files": a relative path from the importing file's directory, a home (~)
// or an absolute path as it is, recursively, at most four hops deep; code
// spans and fenced blocks are skipped; a backslash keeps a space in the
// path). Claude Code 2.1 lexes the file with marked, without GFM, and
// matches its import pattern in the text tokens only, so a path also ends
// where Markdown's emphasis, a link's text or inline HTML begins or ends.
// A team's AGENTS.md that imports @.docs/guide.md loads the PR's guide: the
// session, the reload check, the files gate and the trivial-delta check
// count an imported file as a link's target.

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxImportHops is how many hops of imports magnum follows from an
// instruction file: Claude Code's four, and one more, so an off-by-one in
// either reading errs toward a target.
const MaxImportHops = 5

// importTrailing is the punctuation that may end a sentence after an
// import: ImportPaths gives the path with and without it.
const importTrailing = ".,;:!?)]}>\"'`"

// importWorkPerByte and importWorkSlack bound what ImportPaths compares
// for a text of n bytes: importWorkPerByte*n+importWorkSlack bytes of the
// paths it reads (a path Claude may end at any of many marks, read from
// each of many @ in one word, is a quadratic count of them).
const (
	importWorkPerByte = 64
	importWorkSlack   = 256 << 10
)

// ImportPaths are the paths text imports (@path), each once, in order, at
// most limit of them (more reports that text has more, or more readings
// than ImportPaths compares in time linear in it), read wider than Claude
// Code reads them so that magnum errs toward a target. Claude Code 2.1
// matches `(?:^|\s)@((?:[^\s\\]|\\ )+)` in each text token marked (without
// GFM) gives, but in code spans and fenced blocks, and in an HTML block
// that starts with a comment, with its comments taken out; it cuts the
// path at its first #, makes "\ " a space and trims it. ImportPaths reads
// an @ where a text token may start or after a blank (the text's start, a
// blank, or a mark an inline token starts or ends with: > ) ] [ * _ ` or
// an escaped one; not ( " or a letter), in code too, up to the first blank
// a backslash does not escape (JS's \s: U+FEFF is one, U+0085 is not). It gives that token as it is and as
// Claude's pattern ends it (at a backslash no space follows), each without
// its #fragment too, and, since a text token ends where another begins,
// the path up to each mark that may begin or end an emphasis, a code span,
// a link's text, an image or inline HTML (* _ ` < [ ] and ![; an underscore
// inside a word is none, so @app_config gives no app), each with and
// without the punctuation that may end a sentence after it and an escaped
// space at its end. It reads on after such a mark for a further @, reads
// the text after a YAML frontmatter on its own (Claude strips one before
// it lexes the rest, so an @ right after the closing --- imports), and
// reads each again with its HTML comments taken out, where a cut starts a
// path too (Claude reads a block that starts with a comment on its own).
// A home (~) or an absolute path is left out: no commit's tree holds it,
// and Claude loads a file outside its working directory only once the
// user approves the project's external imports, which magnum never does.
func ImportPaths(text string, limit int) (paths []string, more bool) {
	texts := []string{text}
	if body, ok := frontmatterBody(text); ok {
		texts = append(texts, body)
	}
	work := importWorkSlack
	for _, t := range texts {
		work += importWorkPerByte * len(t)
	}
	r := importReader{seen: map[string]bool{}, limit: limit, work: work}
	for _, t := range texts {
		if r.read(t, nil); r.more {
			break
		}
		if strings.Contains(t, "<!--") {
			r.read(withoutComments(t))
		}
	}
	return r.paths, r.more
}

// frontmatterBody is what follows the YAML frontmatter text opens with,
// which Claude Code strips before it lexes the rest: after a BOM,
// /^---\s*\n([\s\S]*?)---\s*\n?/ (JS's \s: jsSpace), whose closing ---
// need not stand on its own line. false when text opens with none.
func frontmatterBody(text string) (string, bool) {
	t := strings.TrimPrefix(text, "\ufeff")
	if !strings.HasPrefix(t, "---") {
		return "", false
	}
	open := -1 // the last line feed of the blanks after the opening ---
	i := 3
	for i < len(t) {
		c, size := utf8.DecodeRuneInString(t[i:])
		if !jsSpace(c) {
			break
		}
		if c == '\n' {
			open = i
		}
		i += size
	}
	if open < 0 {
		return "", false
	}
	k := strings.Index(t[open+1:], "---")
	if k < 0 {
		return "", false
	}
	end := open + 1 + k + 3
	for end < len(t) {
		c, size := utf8.DecodeRuneInString(t[end:])
		if !jsSpace(c) {
			break
		}
		end += size
	}
	return t[end:], true
}

// jsSpace reports whether c is a blank JS's \s matches: Go's
// unicode.IsSpace but U+0085, and U+FEFF.
func jsSpace(c rune) bool {
	return c == '\ufeff' || c != '\u0085' && unicode.IsSpace(c)
}

// importReader is one ImportPaths read: the paths it gave, each once, and
// what it may still compare.
type importReader struct {
	seen  map[string]bool
	paths []string
	limit int
	work  int  // bytes of paths left to compare
	more  bool // past limit, or past work: the read stopped
}

// add gives p once; false once the read must stop.
func (r *importReader) add(p string) bool {
	if r.work -= len(p) + 1; r.work < 0 {
		r.more = true
		return false
	}
	if p == "" || strings.HasPrefix(p, "~") || strings.HasPrefix(p, "/") || r.seen[p] {
		return true
	}
	if len(r.paths) >= r.limit {
		r.more = true
		return false
	}
	r.seen[p] = true
	r.paths = append(r.paths, p)
	return true
}

// importRun is a stretch of text with no blank a backslash does not escape,
// from the character after an @ to end, and the offsets in it where a path
// Claude reads may end: a backslash no space follows (lone), a # (hashes),
// a mark of an inline token (cuts), and each "\ " pair (pairs), all
// ascending.
type importRun struct {
	end                       int
	lone, hashes, cuts, pairs []int
}

// read gives the paths of t's imports; an @ at one of joints (ascending
// offsets where a comment was cut out of t) starts one too.
func (r *importReader) read(t string, joints []int) {
	var run importRun
	for i := 0; i < len(t); {
		k := strings.IndexByte(t[i:], '@')
		if k < 0 {
			return
		}
		at := i + k
		i = at + 1
		if _, joint := slices.BinarySearch(joints, at); !joint && !importStarts(t, at) {
			continue
		}
		if at >= run.end {
			run = scanImportRun(t, at+1)
			if r.work -= run.end - at; r.work < 0 {
				r.more = true
				return
			}
		}
		if !r.paths1(t, at, &run) {
			return
		}
	}
}

// importStarts reports whether the @ at t[at] may start an import Claude
// reads: at the start of a text token or after a blank in it. A text
// token starts the text, a line (a blank before it), a block quote's
// content (>), a link's text ([) and an emphasis's (* _), and follows
// any other inline token, each of which marked ends with a mark: an
// emphasis (* _), a code span (`), a link or an image () ]), inline HTML
// or an autolink (>), a hard break (a line's end) or an escape (\ and an
// ASCII punctuation mark). An @ after any other character ((@x), "@x",
// a@b) is inside a token Claude reads no import from.
func importStarts(t string, at int) bool {
	if at == 0 {
		return true
	}
	c, _ := utf8.DecodeLastRuneInString(t[:at])
	switch {
	case unicode.IsSpace(c) || c == '\ufeff':
		return true
	case c == '>' || c == ')' || c == ']' || c == '[' || c == '*' || c == '_' || c == '`':
		return true
	}
	return at >= 2 && t[at-2] == '\\' && strings.ContainsRune(asciiPunct, c)
}

// asciiPunct are the marks a backslash escapes in Markdown.
const asciiPunct = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

// scanImportRun is the run of t from from.
func scanImportRun(t string, from int) importRun {
	run := importRun{end: len(t)}
	for j := from; j < len(t); {
		c, size := utf8.DecodeRuneInString(t[j:])
		switch {
		case c == '\\' && j+1 < len(t) && t[j+1] == ' ':
			run.pairs = append(run.pairs, j)
			j += 2
			continue
		case c == '\\':
			run.lone = append(run.lone, j)
		case c == '#':
			run.hashes = append(run.hashes, j)
		case c == '*' || c == '`' || c == '<' || c == '[' || c == ']' || c == '\u0085' || c == '!' && strings.HasPrefix(t[j+1:], "["):
			run.cuts = append(run.cuts, j)
		case c == '_':
			k := j + 1
			for k < len(t) && t[k] == '_' {
				k++
			}
			if !inWordUnderscores(t, j, k) {
				for i := j; i < k; i++ {
					run.cuts = append(run.cuts, i)
				}
			}
			j = k
			continue
		case jsSpace(c):
			run.end = j
			return run
		}
		j += size
	}
	return run
}

// inWordUnderscores reports whether t[i:k], a run of _, lies between two
// letters or digits (Unicode's L and N): marked opens no emphasis there
// and closes none, so a text token goes on through it.
func inWordUnderscores(t string, i, k int) bool {
	before, _ := utf8.DecodeLastRuneInString(t[:i])
	after, _ := utf8.DecodeRuneInString(t[k:])
	word := func(c rune) bool { return unicode.IsLetter(c) || unicode.IsNumber(c) }
	return i > 0 && k < len(t) && word(before) && word(after)
}

// paths1 gives the paths of the import at t[at] in run: false once the
// read must stop.
func (r *importReader) paths1(t string, at int, run *importRun) bool {
	from := at + 1
	after := func(list []int, limit int) int { // the first offset of list past at, before limit
		k, _ := slices.BinarySearch(list, from)
		if k < len(list) && list[k] < limit {
			return list[k]
		}
		return limit
	}
	claude := after(run.lone, run.end) // Claude's pattern ends at a lone backslash
	frag := after(run.hashes, run.end) // the wide token without its #fragment
	cut := after(run.hashes, claude)   // Claude's path, without its #fragment
	text := t[from:run.end]
	k, _ := slices.BinarySearch(run.pairs, from)
	pairs := run.pairs[k:]
	if len(pairs) > 0 {
		text = strings.ReplaceAll(text, "\\ ", " ")
		if r.work -= len(text); r.work < 0 {
			r.more = true
			return false
		}
	}
	length := func(end int) int { // the length in text of the token up to end
		n, _ := slices.BinarySearch(pairs, end)
		return end - from - n
	}
	done := map[int]bool{} // the lengths given: every path here is a prefix of text
	give := func(end int) bool {
		v := text[:length(end)]
		for _, w := range []string{v, strings.TrimRight(v, importTrailing), strings.TrimRight(v, " ")} {
			if done[len(w)] {
				continue
			}
			done[len(w)] = true
			if !r.add(w) {
				return false
			}
		}
		return true
	}
	for _, end := range []int{run.end, frag, claude, cut} {
		if !give(end) {
			return false
		}
	}
	k, _ = slices.BinarySearch(run.cuts, from)
	for _, end := range run.cuts[k:] {
		if end >= cut {
			break
		}
		if !give(end) {
			return false
		}
	}
	return true
}

// withoutComments is t with its HTML comments (each <!-- to the first -->
// after it) taken out, as Claude Code reads an HTML block that starts with
// one, and the offsets in it where one was cut (joints). Claude pairs them
// within each such block, whose text after its first comment starts a
// path; an earlier <!-- it does not read as a comment (in a paragraph, a
// heading, a frontmatter) pairs here with that first comment's -->, which
// glues the text before it to the block's, so the cut is a start.
func withoutComments(t string) (string, []int) {
	var b strings.Builder
	var joints []int
	for {
		i := strings.Index(t, "<!--")
		if i < 0 {
			break
		}
		j := strings.Index(t[i+4:], "-->")
		if j < 0 {
			break
		}
		b.WriteString(t[:i])
		if n := b.Len(); len(joints) == 0 || joints[len(joints)-1] != n {
			joints = append(joints, n)
		}
		t = t[i+4+j+3:]
	}
	b.WriteString(t)
	return b.String(), joints
}
