package pipeline

// The pre-pass (DECISIONS "A round reads its own diff before the
// reviewers"): a colleague's review tool lists, before any model reads
// code, the references a PR broke, the changed files without a test change
// and the closest existing file to each new one, and on talkable#11483 its
// reviewers found defects from those leads that magnum's missed, while
// magnum's roles had only the base's history, docs and related PRs and
// read every caller of a changed method by hand. Before the reviewers of
// each head, beside docs.json, a round writes prepass.json in its report
// directory, read with git alone from the PR's diff (base_sha...head_sha)
// and the head's tree, never the checkout's work tree:
//
//   - removed_symbols: each definition (Ruby, Go, JavaScript and
//     TypeScript, Python, by a pattern per language) the diff removes or
//     renames and does not add back under the same name, with the files of
//     the head outside the diff that still name it as a whole word (one git
//     grep -w per name, the first 20 files);
//   - test_gaps: each changed source file whose test file, by its
//     language's naming convention, the diff leaves alone;
//   - siblings: for each added file, the existing file of the same suffix
//     in its directory, else in the parent, with the longest common prefix;
//   - methods (DECISIONS "The pre-pass carries the full bodies of changed
//     methods"): the method enclosing each hunk of a changed source file at
//     the head, with its lines and body, found by the language's shape
//     (indentation, or braces outside strings and comments), since a
//     reviewer who reads only the hunk misses a write elsewhere in the same
//     method, as on talkable#11483.
//
// The watch's related_ignore paths (lockfiles) are left out, as history.json
// and docs.json do. The file says `skipped` and why instead when there is
// no base sha, the diff has more than 50,000 changed lines, git fails, or
// the reads take more than PrepassTimeout; the review goes on either way.
// The judge and claude-review prompts name it and say each item is a lead
// to check, never a finding by itself. Paths and names are data in the
// file, never in a prompt or an event.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
)

// PrepassFile is the round's reading of its own diff in its report
// directory.
const PrepassFile = "prepass.json"

// prepass.json's caps.
const (
	maxPrepassLines   = 50000     // changed lines past which the diff is not read
	maxPrepassUses    = 20        // files listed per removed symbol
	maxPrepassSymbols = 200       // removed symbols searched; the rest are counted
	minPrepassName    = 4         // a shorter name is never searched
	prepassWorkers    = 4         // git greps and file reads at a time
	maxPrepassMethods = 120       // changed methods listed; the rest are counted
	maxMethodLines    = 300       // a longer method is listed without its body
	maxMethodBytes    = 400 << 10 // the bodies' bytes; past them the largest are dropped and counted
	maxSignatureLines = 20        // a method whose body opens no sooner is none
)

// PrepassTimeout bounds the pre-pass's git reads: the reviewers wait for it.
var PrepassTimeout = 60 * time.Second

// prepassStop are names never searched: too common to lead anywhere, the
// constructors each language calls by another name among them.
var prepassStop = []string{"new", "call", "run", "get", "set", "init", "main", "test", "initialize", "constructor", "__init__"}

// Prepass is prepass.json.
type Prepass struct {
	PR      string `json:"pr"`       // owner/repo#N
	HeadSHA string `json:"head_sha"` // the head under review, whose tree is searched
	BaseSHA string `json:"base_sha"` // the merge base: the diff is base_sha...head_sha
	// Skipped says why the pre-pass did not run ("no base sha", "the diff
	// has 61400 changed lines, over 50000", a failure, the timeout); the
	// lists are absent then.
	Skipped        string          `json:"skipped,omitempty"`
	RemovedSymbols []RemovedSymbol `json:"removed_symbols,omitzero"`
	// UnsearchedSymbols counts the removed definitions past the 200
	// searched.
	UnsearchedSymbols int       `json:"unsearched_symbols,omitempty"`
	TestGaps          []string  `json:"test_gaps,omitzero"` // changed source files whose test the diff leaves alone, in git's order
	Siblings          []Sibling `json:"siblings,omitzero"`  // the added files that have one, in git's order
	// Methods are the methods the diff's hunks change in its source files,
	// as the head has them, in git's order and by line, at most 120.
	Methods         []Method `json:"methods,omitzero"`
	UnlistedMethods int      `json:"unlisted_methods,omitempty"` // changed methods past the 120
	DroppedBodies   int      `json:"dropped_bodies,omitempty"`   // bodies dropped past 400 KB, the largest first
}

// Method is a method the diff changes, as the head has it: the innermost
// method enclosing a changed line, or the lines on both sides of a hunk
// that only removes lines.
type Method struct {
	File      string `json:"file"`
	Name      string `json:"name"`
	FirstLine int    `json:"first_line"`
	LastLine  int    `json:"last_line"`
	// Body is the method's lines, first to last; absent for a method over
	// 300 lines and for one whose body was dropped past 400 KB.
	Body string `json:"body,omitempty"`
}

// RemovedSymbol is a definition the diff removes or renames that files of
// the head outside the diff still name.
type RemovedSymbol struct {
	Name string `json:"name"`
	File string `json:"file"` // the first file the diff removes it from
	// Uses are "<path>:<line>": each file's first line naming it as a
	// whole word, at most 20 files, in git's order.
	Uses []string `json:"uses"`
	More int      `json:"more,omitempty"` // files past the 20
}

// Sibling is an added file and the closest existing file to it.
type Sibling struct {
	File    string `json:"file"`
	Sibling string `json:"sibling"`
}

// prepassLang is a language the pre-pass reads: its definition patterns
// (group 1 the name), how a changed test file matches a source file, and
// the methods of a file's lines.
type prepassLang struct {
	defs    []*regexp.Regexp
	tests   func(src string, changed map[string]bool) bool
	methods func(lines []string) []methodSpan
}

var (
	goFunc     = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)`)
	jsFunction = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`)
	// jsArrow is a name bound to a function or a block-bodied arrow
	// function (a variable or a class field), its parameters on one line
	// or opening at the line's end; jsMethod a class or object method
	// whose body opens on its line.
	jsArrow = regexp.MustCompile(`^\s*(?:export\s+)?(?:(?:const|let|var|static|readonly|public|private|protected|override)\s+)*` +
		`(#?[A-Za-z_$][\w$]*)\s*(?::[^=]*)?=\s*(?:async\s+)?(?:function\b|\(\s*$|(?:\([^()]*\)|[A-Za-z_$][\w$]*)\s*(?::[^=]*)?=>\s*\{)`)
	jsMethod = regexp.MustCompile(`^\s*(?:(?:static|async|public|private|protected|override|get|set)\s+)*\*?\s*(#?[A-Za-z_$][\w$]*)\s*` +
		`(?:<[^>]*>)?\s*\((?:[^()]|\([^()]*\))*\)\s*(?::[^{=;]*)?\{\s*(?://.*)?$`)
	// jsNotMethods are the keywords jsMethod's shape also matches.
	jsNotMethods = []string{"if", "for", "while", "switch", "catch", "with", "function", "return", "typeof", "await", "yield",
		"new", "super", "do", "else", "try", "finally"}

	rubyLang = &prepassLang{
		defs: []*regexp.Regexp{
			regexp.MustCompile(`^\s*def\s+(?:self\.)?([A-Za-z_]\w*[?!]?)`),
			regexp.MustCompile(`^\s*(?:class|module)\s+(?:[A-Z]\w*::)*([A-Z]\w*)`),
		},
		tests:   rubyTests,
		methods: rubyMethods,
	}
	goLang = &prepassLang{
		defs: []*regexp.Regexp{goFunc, regexp.MustCompile(`^type\s+([A-Za-z_]\w*)`)},
		tests: func(src string, changed map[string]bool) bool {
			return changed[strings.TrimSuffix(src, ".go")+"_test.go"]
		},
		methods: func(lines []string) []methodSpan { return braceMethods(lines, true, nil, goFunc) },
	}
	jsLang = &prepassLang{
		defs: []*regexp.Regexp{
			jsFunction,
			regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`),
			regexp.MustCompile(`^\s*export\s+(?:const|let|var)\s+([A-Za-z_$][\w$]*)`),
			regexp.MustCompile(`^\s*(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?\(`),
		},
		tests: jsTests,
		methods: func(lines []string) []methodSpan {
			return braceMethods(lines, false, jsNotMethods, jsFunction, jsArrow, jsMethod)
		},
	}
	pyLang = &prepassLang{
		defs: []*regexp.Regexp{
			regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)`),
			regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`),
		},
		tests:   pyTests,
		methods: pyMethods,
	}
	prepassLangs = map[string]*prepassLang{
		".rb": rubyLang, ".rake": rubyLang, ".go": goLang, ".py": pyLang,
		".js": jsLang, ".jsx": jsLang, ".mjs": jsLang, ".cjs": jsLang, ".ts": jsLang, ".tsx": jsLang,
	}
)

// searchableName is a name git grep -w can find: a word, a Ruby
// predicate's or bang method's ? or ! included (a JavaScript name with a $
// is not one).
var searchableName = regexp.MustCompile(`^[A-Za-z_]\w*[?!]?$`)

// searchable: name is worth a git grep -w: a searchableName of at least 4
// characters, not on the stop list.
func searchable(name string) bool {
	return len(name) >= minPrepassName && searchableName.MatchString(name) && !slices.Contains(prepassStop, strings.ToLower(name))
}

// definition is the name line defines in lang, "" when it defines none.
func (l *prepassLang) definition(line string) string {
	for _, re := range l.defs {
		if m := re.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// notSourceDirs are the directories whose files are tests, fixtures,
// migrations, generated or vendored code; notSourceRubyDirs those of a
// Rails layout's views, configuration and database files, which the other
// languages use as package names (Go's internal/config).
var (
	notSourceDirs = []string{"spec", "test", "tests", "__tests__", "testdata", "fixtures", "__fixtures__", "__mocks__", "mocks",
		"migrate", "migrations", "vendor", "node_modules", "generated", "__generated__", "dist"}
	notSourceRubyDirs = []string{"views", "config", "db"}
	// notSourceNames are the name endings of tests, generated code and
	// configuration files.
	notSourceNames = []string{"_test.go", "_spec.rb", "_test.rb", "_test.py", ".pb.go", ".pb.gw.go", "_gen.go", ".gen.go",
		"_generated.go", ".min.js", ".d.ts", "_pb2.py", "_pb2_grpc.py", "conftest.py", "setup.py"}
	// notSourceInfixes mark a JavaScript or TypeScript test, story,
	// configuration or generated file inside its name ("cart.test.ts").
	notSourceInfixes = []string{".test.", ".spec.", ".stories.", ".config.", ".generated."}
)

// sourceLang is the language of p, a changed file, when it is a source
// file: a language the pre-pass reads, and not a test, fixture, migration,
// generated or vendored file, a view, a style, a document or a
// configuration file, decided by its path and extension; nil otherwise.
func sourceLang(p string) *prepassLang {
	lang := prepassLangs[path.Ext(p)]
	base := path.Base(p)
	if lang == nil || strings.HasPrefix(base, ".") || strings.HasPrefix(base, "test_") || strings.HasPrefix(base, "zz_generated") {
		return nil
	}
	if slices.ContainsFunc(notSourceNames, func(s string) bool { return strings.HasSuffix(base, s) }) ||
		slices.ContainsFunc(notSourceInfixes, func(s string) bool { return strings.Contains(base, s) }) {
		return nil
	}
	for seg := range strings.SplitSeq(path.Dir(p), "/") {
		if slices.Contains(notSourceDirs, seg) || lang == rubyLang && slices.Contains(notSourceRubyDirs, seg) {
			return nil
		}
	}
	return lang
}

// rubyTests: a changed test of src by the Rails layout: <x>/app/<y>.rb and
// <x>/lib/<y>.rb are tested by <x>/spec/<y>_spec.rb or <x>/test/<y>_test.rb
// (lib's also under spec/lib/ and test/lib/), any other file by
// spec/<path>_spec.rb or test/<path>_test.rb.
func rubyTests(src string, changed map[string]bool) bool {
	stem := strings.TrimSuffix(src, path.Ext(src))
	prefix, rest, lib := "", stem, false
	segs := strings.Split(stem, "/")
	if i := slices.IndexFunc(segs[:len(segs)-1], func(s string) bool { return s == "app" || s == "lib" }); i >= 0 {
		prefix, rest, lib = strings.Join(segs[:i], "/"), strings.Join(segs[i+1:], "/"), segs[i] == "lib"
		if prefix != "" {
			prefix += "/"
		}
	}
	candidates := []string{prefix + "spec/" + rest + "_spec.rb", prefix + "test/" + rest + "_test.rb"}
	if lib {
		candidates = append(candidates, prefix+"spec/lib/"+rest+"_spec.rb", prefix+"test/lib/"+rest+"_test.rb")
	}
	return slices.ContainsFunc(candidates, func(c string) bool { return changed[c] })
}

// jsTests: a changed <x>.test.* or <x>.spec.* beside src (<x>.<ext>), or a
// <x>.* in the __tests__ directory beside it.
func jsTests(src string, changed map[string]bool) bool {
	dir, stem := path.Dir(src), strings.TrimSuffix(path.Base(src), path.Ext(src))
	for c := range changed {
		switch base := path.Base(c); path.Dir(c) {
		case dir:
			if strings.HasPrefix(base, stem+".test.") || strings.HasPrefix(base, stem+".spec.") {
				return true
			}
		case path.Join(dir, "__tests__"):
			if strings.HasPrefix(base, stem+".") {
				return true
			}
		}
	}
	return false
}

// pyTests: a changed test_<x>.py or <x>_test.py in any directory.
func pyTests(src string, changed map[string]bool) bool {
	stem := strings.TrimSuffix(path.Base(src), ".py")
	for c := range changed {
		if base := path.Base(c); base == "test_"+stem+".py" || base == stem+"_test.py" {
			return true
		}
	}
	return false
}

// testGaps are the source files of files (the PR's diff) that it adds or
// modifies while it changes no test of theirs, in its order; an empty list
// when there are none.
func testGaps(files []gitx.FileLines) []string {
	changed := make(map[string]bool, len(files))
	for _, f := range files {
		changed[f.Path] = true
	}
	gaps := []string{}
	for _, f := range files {
		if lang := sourceLang(f.Path); lang != nil && f.Status != 'D' && !lang.tests(f.Path, changed) {
			gaps = append(gaps, f.Path)
		}
	}
	return gaps
}

// nameSuffix is a file name's suffix from its first dot, a leading dot
// aside ("cart.test.ts": ".test.ts"); "" without one.
func nameSuffix(base string) string {
	name := strings.TrimPrefix(base, ".")
	if i := strings.Index(name, "."); i >= 0 {
		return name[i:]
	}
	return ""
}

// commonPrefix and commonSuffix are the lengths a and b share at their
// start and their end.
func commonPrefix(a, b string) int {
	n := 0
	for n < min(len(a), len(b)) && a[n] == b[n] {
		n++
	}
	return n
}

func commonSuffix(a, b string) int {
	n := 0
	for n < min(len(a), len(b)) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return n
}

// closest is the file of candidates whose name shares file's suffix and the
// longest prefix with it, then the longest ending, then the first by name;
// "" when none shares the suffix.
func closest(file string, candidates []string) string {
	base := path.Base(file)
	suffix := nameSuffix(base)
	best := ""
	for _, c := range candidates {
		cb := path.Base(c)
		if c == file || nameSuffix(cb) != suffix {
			continue
		}
		if best == "" {
			best = c
			continue
		}
		bb := path.Base(best)
		if cmp.Or(cmp.Compare(commonPrefix(base, cb), commonPrefix(base, bb)), cmp.Compare(commonSuffix(base, cb), commonSuffix(base, bb)),
			strings.Compare(bb, cb)) > 0 {
			best = c
		}
	}
	return best
}

// siblings finds, with one ls-tree of the head, the closest existing file
// to each file files add: in its directory, else in the parent; another
// file the PR adds is not an existing one. An empty list when no added
// file has one.
func (rd *round) siblings(ctx context.Context, files []gitx.FileLines) ([]Sibling, error) {
	added := map[string]bool{}
	var dirs []string
	for _, f := range files {
		if f.Status != 'A' {
			continue
		}
		added[f.Path] = true
		d := path.Dir(f.Path)
		for _, x := range []string{d, path.Dir(d)} {
			if !slices.Contains(dirs, x) {
				dirs = append(dirs, x)
			}
		}
	}
	out := []Sibling{}
	if len(dirs) == 0 {
		return out, nil
	}
	existing, err := rd.r.Git.DirFiles(ctx, rd.in.SlotPath, rd.in.TargetSHA, dirs...)
	if err != nil {
		return nil, err
	}
	byDir := map[string][]string{}
	for _, p := range existing {
		if !added[p] {
			byDir[path.Dir(p)] = append(byDir[path.Dir(p)], p)
		}
	}
	for _, f := range files {
		if f.Status != 'A' {
			continue
		}
		d := path.Dir(f.Path)
		s := closest(f.Path, byDir[d])
		if s == "" && d != "." {
			s = closest(f.Path, byDir[path.Dir(d)])
		}
		if s != "" {
			out = append(out, Sibling{File: f.Path, Sibling: s})
		}
	}
	return out, nil
}

// methodSpan is a method of a file: its name and its first and last line
// (1-based).
type methodSpan struct {
	name        string
	first, last int
}

var (
	// rubyDef starts a method: group 1 its indentation, group 2 its name.
	rubyDef = regexp.MustCompile(`^(\s*)(?:(?:private|protected|public|module_function)\s+)?def\s+(?:self\.)?([^\s(;]+)`)
	// rubyOneLine is a method on one line: def x; y; end, or the endless
	// def x = y (a setter, def x=(v), is not one).
	rubyOneLine = regexp.MustCompile(`;\s*end\s*$|^\s*(?:(?:private|protected|public|module_function)\s+)?def\s+(?:self\.)?` +
		`[A-Za-z_]\w*[?!]?(?:\s*\([^)]*\)\s*|\s+)=(?:[^=~>]|$)`)
	rubyEnd = regexp.MustCompile(`^end\b`)
	// rubyLeaves, at a lower indentation than a def, means its scope ended
	// without the def's end: the code's indentation does not say where the
	// method ends.
	rubyLeaves = regexp.MustCompile(`^(?:end|def|class|module)\b`)
	pyDef      = regexp.MustCompile(`^(\s*)(?:async\s+)?def\s+([A-Za-z_]\w*)`)
)

// rubyMethods: a def runs to the first end at its indentation, comments
// and lower-indented lines (a heredoc's) aside.
func rubyMethods(lines []string) []methodSpan {
	var out []methodSpan
	for i, l := range lines {
		m := rubyDef.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if rubyOneLine.MatchString(l) {
			out = append(out, methodSpan{name: m[2], first: i + 1, last: i + 1})
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			t := strings.TrimLeft(lines[j], " \t")
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			indent := len(lines[j]) - len(t)
			if indent == len(m[1]) && rubyEnd.MatchString(t) {
				out = append(out, methodSpan{name: m[2], first: i + 1, last: j + 1})
				break
			}
			if indent < len(m[1]) && rubyLeaves.MatchString(t) {
				break
			}
		}
	}
	return out
}

// pyMethods: a def's signature runs to the line that closes its
// parentheses, and its body to the last line before the first one at the
// def's indentation or less, blank lines, comments and the lines of a
// triple-quoted string aside.
func pyMethods(lines []string) []methodSpan {
	var out []methodSpan
	for i, l := range lines {
		m := pyDef.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		sig, depth := -1, 0
		for j := i; j < min(len(lines), i+maxSignatureLines); j++ {
			depth += strings.Count(lines[j], "(") - strings.Count(lines[j], ")")
			if depth <= 0 {
				sig = j
				break
			}
		}
		if sig < 0 {
			continue
		}
		last, quote := sig, ""
		for j := sig + 1; j < len(lines); j++ {
			if quote != "" { // inside a triple-quoted string
				last = j
				if strings.Count(lines[j], quote)%2 == 1 {
					quote = ""
				}
				continue
			}
			t := strings.TrimLeft(lines[j], " \t")
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if len(lines[j])-len(t) <= len(m[1]) {
				break
			}
			last = j
			for _, q := range []string{`"""`, `'''`} {
				if strings.Count(lines[j], q)%2 == 1 {
					quote = q
					break
				}
			}
		}
		out = append(out, methodSpan{name: m[2], first: i + 1, last: last + 1})
	}
	return out
}

// braceLine is a line of a brace language as lexed by lexBraces, braces,
// parentheses and semicolons in strings and comments aside.
type braceLine struct {
	inside     bool // the line starts inside a block comment or a multi-line string
	startDepth int  // the brace depth at the line's start
	endDepth   int  // and at its end
	startParen int  // the parenthesis depth at its start
	openParen  int  // the lowest parenthesis depth at which a brace opens on it; math.MaxInt for none
	semi       bool // its code ends with a semicolon
	blank      bool // it holds no code
}

// lexBraces reads a Go (raw: a backquoted string has no escapes) or
// JavaScript and TypeScript file's lines: // and /* */ comments, "" and ”
// strings (to the line's end when unclosed) and “ strings across lines (a
// template's ${} read as part of it).
func lexBraces(lines []string, raw bool) []braceLine {
	out := make([]braceLine, len(lines))
	depth, paren := 0, 0
	inBlock, inTick := false, false
	for i, l := range lines {
		bl := braceLine{inside: inBlock || inTick, startDepth: depth, startParen: paren, openParen: math.MaxInt, blank: true}
		var last byte
		for j := 0; j < len(l); j++ {
			c := l[j]
			switch {
			case inBlock:
				if c == '*' && j+1 < len(l) && l[j+1] == '/' {
					inBlock = false
					j++
				}
				continue
			case inTick:
				if c == '\\' && !raw {
					j++
				} else if c == '`' {
					inTick = false
				}
				continue
			}
			switch c {
			case ' ', '\t', '\r':
				continue
			case '/':
				if j+1 < len(l) && l[j+1] == '/' {
					j = len(l)
					continue
				}
				if j+1 < len(l) && l[j+1] == '*' {
					inBlock = true
					j++
					continue
				}
			case '"', '\'':
				for j++; j < len(l) && l[j] != c; j++ {
					if l[j] == '\\' {
						j++
					}
				}
			case '`':
				inTick = true
			case '{':
				depth++
				bl.openParen = min(bl.openParen, paren)
			case '}':
				depth--
			case '(':
				paren++
			case ')':
				paren--
			}
			bl.blank, last = false, c
		}
		bl.endDepth, bl.semi = depth, last == ';'
		out[i] = bl
	}
	return out
}

// braceMethods: a line one of starts matches (group 1 the name, none of
// skip) begins a method whose body is the first brace it opens at the
// line's parenthesis depth (a brace in its parameters, a destructuring, is
// not one), and which ends on the line that closes it; none when its
// signature ends first (a semicolon, a blank line, maxSignatureLines).
func braceMethods(lines []string, raw bool, skip []string, starts ...*regexp.Regexp) []methodSpan {
	lx := lexBraces(lines, raw)
	var out []methodSpan
	for i, l := range lines {
		if lx[i].inside {
			continue
		}
		name := ""
		for _, re := range starts {
			if m := re.FindStringSubmatch(l); m != nil {
				name = m[1]
				break
			}
		}
		if name == "" || slices.Contains(skip, name) {
			continue
		}
		open := false
		for j := i; j < len(lines); j++ {
			if !open {
				if lx[j].openParen <= lx[i].startParen {
					open = true
				} else if lx[j].semi || j > i && lx[j].blank || j-i >= maxSignatureLines {
					break
				}
			}
			if open && lx[j].endDepth <= lx[i].startDepth {
				out = append(out, methodSpan{name: name, first: i + 1, last: j + 1})
				break
			}
		}
	}
	return out
}

// changedMethods are the methods of text (f's file at the head) that f's
// hunks change, by line: the innermost method enclosing each changed line,
// and for a hunk that only removes lines the innermost one holding the
// lines on both sides of it. A method the diff adds whole is none, its
// body being in the diff, so its lines count for the method around it.
// A method over maxMethodLines lines gets no body.
func changedMethods(f gitx.FileLines, text string) []Method {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	n := len(lines)
	changed := make([]bool, n+1)
	for _, h := range f.Hunks {
		for l := max(h.Start, 1); l < h.Start+h.Count && l <= n; l++ {
			changed[l] = true
		}
	}
	spans := slices.DeleteFunc(sourceLang(f.Path).methods(lines), func(s methodSpan) bool {
		return !slices.Contains(changed[s.first:s.last+1], false)
	})
	// inner[l] is line l's innermost method, its index in spans plus one:
	// the longer spans are painted first.
	inner := make([]int, n+1)
	byLength := make([]int, len(spans))
	for k := range spans {
		byLength[k] = k
	}
	slices.SortStableFunc(byLength, func(a, b int) int {
		return cmp.Compare(spans[b].last-spans[b].first, spans[a].last-spans[a].first)
	})
	for _, k := range byLength {
		for l := spans[k].first; l <= spans[k].last; l++ {
			inner[l] = k + 1
		}
	}
	hit := make([]bool, len(spans))
	for _, h := range f.Hunks {
		if h.Count > 0 {
			for l := max(h.Start, 1); l < h.Start+h.Count && l <= n; l++ {
				if inner[l] > 0 {
					hit[inner[l]-1] = true
				}
			}
			continue
		}
		best := -1
		for k, s := range spans {
			if s.first <= h.Start && h.Start+1 <= s.last && (best < 0 || s.last-s.first < spans[best].last-spans[best].first) {
				best = k
			}
		}
		if best >= 0 {
			hit[best] = true
		}
	}
	var out []Method
	for k, s := range spans {
		if !hit[k] {
			continue
		}
		m := Method{File: f.Path, Name: s.name, FirstLine: s.first, LastLine: s.last}
		if s.last-s.first < maxMethodLines {
			m.Body = strings.Join(lines[s.first-1:s.last], "\n")
		}
		out = append(out, m)
	}
	return out
}

// methods lists the methods files' hunks change in their modified source
// files (changedMethods), read from the head's tree prepassWorkers at a
// time, in git's order and by line; a file past execx.MaxOutput is left
// out, and an added file, whose methods the diff adds whole, is never
// read. Past maxPrepassMethods the rest are counted in unlisted; past
// maxMethodBytes of bodies the largest are dropped first and counted in
// dropped. An empty list when no hunk changes a method.
func (rd *round) methods(ctx context.Context, files []gitx.FileLines) (ms []Method, unlisted, dropped int, err error) {
	var read []gitx.FileLines
	for _, f := range files {
		if f.Status == 'M' && len(f.Hunks) > 0 && sourceLang(f.Path) != nil {
			read = append(read, f)
		}
	}
	found := make([][]Method, len(read))
	if err := rd.parallel(len(read), "a changed file's methods", func(i int) error {
		text, err := rd.r.Git.FileAt(ctx, rd.in.SlotPath, rd.in.TargetSHA, read[i].Path)
		if errors.Is(err, gitx.ErrFileTooLarge) {
			return nil
		}
		if err != nil {
			return err
		}
		found[i] = changedMethods(read[i], text)
		return nil
	}); err != nil {
		return nil, 0, 0, err
	}
	ms = append([]Method{}, slices.Concat(found...)...)
	if len(ms) > maxPrepassMethods {
		unlisted, ms = len(ms)-maxPrepassMethods, ms[:maxPrepassMethods]
	}
	total := 0
	bySize := make([]int, len(ms))
	for i := range ms {
		total += len(ms[i].Body)
		bySize[i] = i
	}
	slices.SortStableFunc(bySize, func(a, b int) int { return cmp.Compare(len(ms[b].Body), len(ms[a].Body)) })
	for _, i := range bySize {
		if total <= maxMethodBytes {
			break
		}
		total -= len(ms[i].Body)
		ms[i].Body = ""
		dropped++
	}
	return ms, unlisted, dropped, nil
}

// parallel runs do for 0 to n-1, prepassWorkers at a time, and returns the
// first error by index; a panic fails its call (the pre-pass only warns),
// never the daemon.
func (rd *round) parallel(n int, what string, do func(i int) error) error {
	errs := make([]error, n)
	sem := make(chan struct{}, prepassWorkers)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					rd.logAt(slog.LevelError, "pipeline: %s %s panicked: %v\n%s", rd.subject, what, r, debug.Stack())
					errs[i] = fmt.Errorf("%s panicked: %v", what, r)
				}
			}()
			errs[i] = do(i)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// removedSymbols lists the definitions files remove and add back under no
// name of the same, in their order, that files of the head outside own
// (the diff's paths) still name: one git grep -w per name, prepassWorkers
// at a time, for at most maxPrepassSymbols names (more counts the rest).
// An empty list when none is named.
func (rd *round) removedSymbols(ctx context.Context, files []gitx.FileLines, own map[string]bool) (syms []RemovedSymbol, more int, err error) {
	from := map[string]string{} // a removed name: the first file it leaves
	var names []string
	added := map[string]bool{}
	for _, f := range files {
		lang := prepassLangs[path.Ext(f.Path)]
		if lang == nil {
			continue
		}
		for _, l := range f.Removed {
			if n := lang.definition(l); n != "" && from[n] == "" {
				from[n] = f.Path
				names = append(names, n)
			}
		}
		for _, l := range f.Added {
			if n := lang.definition(l); n != "" {
				added[n] = true
			}
		}
	}
	names = slices.DeleteFunc(names, func(n string) bool { return added[n] || !searchable(n) })
	if len(names) > maxPrepassSymbols {
		more, names = len(names)-maxPrepassSymbols, names[:maxPrepassSymbols]
	}
	found := make([]RemovedSymbol, len(names))
	if err := rd.parallel(len(names), "a removed symbol's search", func(i int) error {
		n := names[i]
		uses, err := rd.r.Git.WordUses(ctx, rd.in.SlotPath, rd.in.TargetSHA, n)
		s := RemovedSymbol{Name: n, File: from[n], Uses: []string{}}
		for _, u := range uses {
			switch {
			case own[u.Path]:
			case len(s.Uses) < maxPrepassUses:
				s.Uses = append(s.Uses, u.Path+":"+strconv.Itoa(u.Line))
			default:
				s.More++
			}
		}
		found[i] = s
		return err
	}); err != nil {
		return nil, 0, err
	}
	syms = []RemovedSymbol{}
	for _, s := range found {
		if len(s.Uses) > 0 {
			syms = append(syms, s)
		}
	}
	return syms, more, nil
}

// prepass reads prepass.json's content for the head under review; lines
// is the number of changed lines it read. A diff past maxPrepassLines is
// Skipped, not an error.
func (rd *round) prepass(ctx context.Context) (p Prepass, lines int, err error) {
	defer func() { // a panic fails the pre-pass, which only warns, never the round
		if r := recover(); r != nil {
			rd.logAt(slog.LevelError, "pipeline: %s the pre-pass panicked: %v\n%s", rd.subject, r, debug.Stack())
			err = fmt.Errorf("panicked: %v", r)
		}
	}()
	in := rd.in
	p = Prepass{PR: fmt.Sprintf("%s/%s#%d", rd.owner, rd.name, in.PR.Number), HeadSHA: in.TargetSHA, BaseSHA: in.BaseSHA}
	if in.BaseSHA == "" {
		p.Skipped = "no base sha"
		return p, 0, nil
	}
	stats, err := rd.r.Git.DiffStat(ctx, in.SlotPath, in.BaseSHA, in.TargetSHA)
	if err != nil {
		return p, 0, err
	}
	own := make(map[string]bool, len(stats))
	var skip []string
	for _, s := range stats {
		own[s.Path] = true
		if slices.ContainsFunc(in.Related.Ignore, func(g string) bool { return config.MatchPath(g, s.Path) }) {
			skip = append(skip, s.Path)
			continue
		}
		lines += s.Added + s.Removed
	}
	if lines > maxPrepassLines {
		p.Skipped = fmt.Sprintf("the diff has %d changed lines, over %d", lines, maxPrepassLines)
		return p, lines, nil
	}
	files, err := rd.r.Git.DiffLines(ctx, in.SlotPath, in.BaseSHA, in.TargetSHA, skip...)
	if err != nil {
		return p, lines, err
	}
	if p.RemovedSymbols, p.UnsearchedSymbols, err = rd.removedSymbols(ctx, files, own); err != nil {
		return p, lines, err
	}
	p.TestGaps = testGaps(files)
	if p.Siblings, err = rd.siblings(ctx, files); err != nil {
		return p, lines, err
	}
	if p.Methods, p.UnlistedMethods, p.DroppedBodies, err = rd.methods(ctx, files); err != nil {
		return p, lines, err
	}
	return p, lines, nil
}

// writePrepass writes prepass.json for the head under review, beside its
// docs.json (a restart writes the new head's), and keeps its path for the
// prompts in rd.prepassFile; "" when there is none: a continued turn, no
// Git, the round ending, or a file that could not be written. A pre-pass
// that cannot run writes the file with Skipped and why; each head's
// round.prepass event counts the lists, or gives the reason (a warning for
// a failure or the timeout).
func (rd *round) writePrepass(ctx context.Context) {
	rd.prepassFile = ""
	if rd.r.Git == nil || rd.in.Kind == KindContinue {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, PrepassTimeout)
	defer cancel()
	p, lines, err := rd.prepass(pctx)
	level := "info"
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		level = "warn"
		p.Skipped = "the pre-pass failed: " + err.Error()
		if pctx.Err() != nil {
			p.Skipped = "the pre-pass took more than " + PrepassTimeout.String()
		}
		p.RemovedSymbols, p.UnsearchedSymbols, p.TestGaps, p.Siblings = nil, 0, nil, nil
		p.Methods, p.UnlistedMethods, p.DroppedBodies = nil, 0, 0
	}
	file := filepath.Join(rd.dir, PrepassFile)
	// The bodies' <, > and & stay as they are, as the reviewers read them.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err = enc.Encode(p)
	if err == nil {
		err = fsx.WriteFileAtomic(file, b.Bytes(), 0o600)
	}
	if err != nil {
		rd.event(ctx, "warn", "round.prepass", fmt.Sprintf("could not write %s; the reviewers go without it: %v", PrepassFile, err), nil)
		return
	}
	rd.prepassFile = file
	if p.Skipped != "" {
		rd.event(ctx, level, "round.prepass", PrepassFile+" skipped: "+p.Skipped, map[string]any{"skipped": p.Skipped, "lines": lines, "file": file})
		return
	}
	rd.event(ctx, "info", "round.prepass", fmt.Sprintf("%s: %d removed symbol(s) still named, %d changed source file(s) without a test change, %d new file(s) with a sibling, %d changed method(s), from %d changed line(s)",
		PrepassFile, len(p.RemovedSymbols), len(p.TestGaps), len(p.Siblings), len(p.Methods), lines),
		map[string]any{"removed_symbols": len(p.RemovedSymbols), "unsearched_symbols": p.UnsearchedSymbols, "test_gaps": len(p.TestGaps),
			"siblings": len(p.Siblings), "methods": len(p.Methods), "unlisted_methods": p.UnlistedMethods, "dropped_bodies": p.DroppedBodies,
			"lines": lines, "file": file})
}
