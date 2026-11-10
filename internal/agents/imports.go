package agents

// Claude Code's @path imports: a CLAUDE.md, a CLAUDE.local.md, an
// AGENTS.md, a rules file under .claude/rules/ and what they import load
// the files they import (code.claude.com/docs memory "Import additional
// files": a relative path from the importing file's directory, a home (~)
// or an absolute path as it is, recursively, at most four hops deep; code
// spans and fenced blocks are skipped; a backslash keeps a space in the
// path; Claude Code 2.1's parser also ends a path at a backslash no space
// follows and drops a #fragment). A team's AGENTS.md that imports @.docs/guide.md loads the PR's
// guide: the session, the reload check, the files gate and the trivial-delta
// check count an imported file as a link's target.

import (
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

// ImportPaths are the paths text imports (@path), each once, in order, at
// most limit of them (more reports that text has more), read wider than
// Claude Code reads them so that magnum errs toward a target: an @ at the
// start of the text or after any character but a letter or a digit (an
// e-mail address imports nothing; Claude wants a blank before it), up to
// the first blank a backslash does not escape ("\ " is a space of the
// path), in a code span or a fenced block too, which Claude skips. Each
// such token is given as it is and as Claude Code's parser ends it (at a
// backslash no space follows: `(?:^|\s)@((?:[^\s\\]|\\ )+)`), each of
// those without its #fragment too (Claude drops it), and each with and
// without the punctuation that may end a sentence after it. A home (~) or
// an absolute path is left out: no commit's tree holds it, and Claude
// loads a file outside its working directory only once the user approves
// the project's external imports, which magnum never does. Its time is
// linear in text: a file the PR controls may hold any number of tokens.
func ImportPaths(text string, limit int) (paths []string, more bool) {
	seen := map[string]bool{}
	add := func(p string) bool { // false once past limit
		if p == "" || strings.HasPrefix(p, "~") || strings.HasPrefix(p, "/") || seen[p] {
			return true
		}
		if len(paths) >= limit {
			more = true
			return false
		}
		seen[p] = true
		paths = append(paths, p)
		return true
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '@' {
			continue
		}
		if i > 0 {
			if r, _ := utf8.DecodeLastRuneInString(text[:i]); unicode.IsLetter(r) || unicode.IsDigit(r) {
				continue
			}
		}
		var b strings.Builder
		claude := -1 // b's length where Claude's path ends: a backslash no space follows
		j := i + 1
		for j < len(text) {
			c := text[j]
			if c == '\\' {
				if j+1 < len(text) && text[j+1] == ' ' {
					b.WriteByte(' ')
					j += 2
					continue
				}
				if claude < 0 {
					claude = b.Len()
				}
			}
			r, size := utf8.DecodeRuneInString(text[j:])
			if unicode.IsSpace(r) {
				break
			}
			b.WriteString(text[j : j+size])
			j += size
		}
		p := b.String()
		tokens := []string{p}
		if claude >= 0 {
			tokens = append(tokens, p[:claude])
		}
		for _, tok := range tokens {
			frag, _, _ := strings.Cut(tok, "#")
			for _, v := range []string{tok, strings.TrimRight(tok, importTrailing), frag, strings.TrimRight(frag, importTrailing)} {
				if !add(v) {
					return paths, more
				}
			}
		}
		i = j - 1
	}
	return paths, more
}
