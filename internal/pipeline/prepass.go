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
//     in its directory, else in the parent, with the longest common prefix.
//
// The watch's related_ignore paths (lockfiles) are left out, as history.json
// and docs.json do. The file says `skipped` and why instead when there is
// no base sha, the diff has more than 20,000 changed lines, git fails, or
// the reads take more than PrepassTimeout; the review goes on either way.
// The judge and claude-review prompts name it and say each item is a lead
// to check, never a finding by itself. Paths and names are data in the
// file, never in a prompt or an event.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	maxPrepassLines   = 20000 // changed lines past which the diff is not read
	maxPrepassUses    = 20    // files listed per removed symbol
	maxPrepassSymbols = 200   // removed symbols searched; the rest are counted
	minPrepassName    = 4     // a shorter name is never searched
	prepassWorkers    = 4     // git greps at a time
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
	// has 24113 changed lines, over 20000", a failure, the timeout); the
	// lists are absent then.
	Skipped        string          `json:"skipped,omitempty"`
	RemovedSymbols []RemovedSymbol `json:"removed_symbols,omitzero"`
	// UnsearchedSymbols counts the removed definitions past the 200
	// searched.
	UnsearchedSymbols int       `json:"unsearched_symbols,omitempty"`
	TestGaps          []string  `json:"test_gaps,omitzero"` // changed source files whose test the diff leaves alone, in git's order
	Siblings          []Sibling `json:"siblings,omitzero"`  // the added files that have one, in git's order
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
// (group 1 the name) and how a changed test file matches a source file.
type prepassLang struct {
	defs  []*regexp.Regexp
	tests func(src string, changed map[string]bool) bool
}

var (
	rubyLang = &prepassLang{
		defs: []*regexp.Regexp{
			regexp.MustCompile(`^\s*def\s+(?:self\.)?([A-Za-z_]\w*[?!]?)`),
			regexp.MustCompile(`^\s*(?:class|module)\s+(?:[A-Z]\w*::)*([A-Z]\w*)`),
		},
		tests: rubyTests,
	}
	goLang = &prepassLang{
		defs: []*regexp.Regexp{
			regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)`),
			regexp.MustCompile(`^type\s+([A-Za-z_]\w*)`),
		},
		tests: func(src string, changed map[string]bool) bool {
			return changed[strings.TrimSuffix(src, ".go")+"_test.go"]
		},
	}
	jsLang = &prepassLang{
		defs: []*regexp.Regexp{
			regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`),
			regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`),
			regexp.MustCompile(`^\s*export\s+(?:const|let|var)\s+([A-Za-z_$][\w$]*)`),
			regexp.MustCompile(`^\s*(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?\(`),
		},
		tests: jsTests,
	}
	pyLang = &prepassLang{
		defs: []*regexp.Regexp{
			regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)`),
			regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`),
		},
		tests: pyTests,
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
	errs := make([]error, len(names))
	sem := make(chan struct{}, prepassWorkers)
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() { // a panic fails the pre-pass, which only warns, never the daemon
				if r := recover(); r != nil {
					rd.logAt(slog.LevelError, "pipeline: %s a removed symbol's search panicked: %v\n%s", rd.subject, r, debug.Stack())
					errs[i] = fmt.Errorf("a removed symbol's search panicked: %v", r)
				}
			}()
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
			found[i], errs[i] = s, err
		})
	}
	wg.Wait()
	syms = []RemovedSymbol{}
	for i, s := range found {
		if errs[i] != nil {
			return nil, 0, errs[i]
		}
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
	}
	file := filepath.Join(rd.dir, PrepassFile)
	b, err := json.MarshalIndent(p, "", "  ")
	if err == nil {
		err = fsx.WriteFileAtomic(file, append(b, '\n'), 0o600)
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
	rd.event(ctx, "info", "round.prepass", fmt.Sprintf("%s: %d removed symbol(s) still named, %d changed source file(s) without a test change, %d new file(s) with a sibling, from %d changed line(s)",
		PrepassFile, len(p.RemovedSymbols), len(p.TestGaps), len(p.Siblings), lines),
		map[string]any{"removed_symbols": len(p.RemovedSymbols), "unsearched_symbols": p.UnsearchedSymbols, "test_gaps": len(p.TestGaps),
			"siblings": len(p.Siblings), "lines": lines, "file": file})
}
