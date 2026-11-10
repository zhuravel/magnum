package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

// resolveConfigLinks follows a tree's links that steer the review agents
// as a case-insensitive filesystem would: a link in a text's path before
// its "..", names in ASCII case, both ways where links differ in case only, the links
// under a directory one leads to; each link passed through is a target
// too, a text that leaves the tree leads nowhere, a loop ends, and a link
// that steers nothing is not followed. Too many steps fail rather than
// leave a link out.
func TestResolveConfigLinksFollowsLinksAsTheFilesystemDoes(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	for _, c := range []struct {
		name  string
		links map[string]string
		want  map[string]string
	}{
		{"a root link", map[string]string{"CLAUDE.md": "docs/guide.md", "lib/x": "docs/other.md"},
			map[string]string{"CLAUDE.md": "CLAUDE.md", "docs/guide.md": "CLAUDE.md"}},
		{"a directory link before ..", map[string]string{"CLAUDE.md": "docs/../x.md", "docs": "a/b"},
			map[string]string{"CLAUDE.md": "CLAUDE.md", "docs": "CLAUDE.md", "a/x.md": "CLAUDE.md"}},
		{"out of the tree", map[string]string{"CLAUDE.md": "../../etc/passwd", ".claude/home": "/Users/x"},
			map[string]string{"CLAUDE.md": "CLAUDE.md", ".claude/home": ".claude/home"}},
		{"a loop", map[string]string{"AGENTS.md": "a", "a": "b", "b": "a"},
			map[string]string{"AGENTS.md": "AGENTS.md", "a": "AGENTS.md", "b": "AGENTS.md"}},
		{"names that differ in case", map[string]string{"CLAUDE.md": "docs/g.md", "Docs": "one", "docs": "two"},
			map[string]string{"CLAUDE.md": "CLAUDE.md", "Docs": "CLAUDE.md", "docs": "CLAUDE.md", "one/g.md": "CLAUDE.md", "two/g.md": "CLAUDE.md"}},
		{"links under a linked directory", map[string]string{".claude/skills": "../.agents/skills", ".agents/skills/lint": "../../tools/lint",
			".agents/other": "../elsewhere"},
			map[string]string{".claude/skills": ".claude/skills", ".agents/skills": ".claude/skills", ".agents/skills/lint": ".claude/skills",
				"tools/lint": ".claude/skills"}},
		{"a skill directory below the root", map[string]string{"apps/web/.claude/skills": "../../../shared/skills"},
			map[string]string{"apps/web/.claude/skills": "apps/web/.claude/skills", "shared/skills": "apps/web/.claude/skills"}},
		{"a link to the root", map[string]string{".claude/skills": "..", "lib/x": "../vendor/x"},
			map[string]string{".claude/skills": ".claude/skills", "": ".claude/skills", "lib/x": ".claude/skills", "vendor/x": ".claude/skills"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveConfigLinks(context.Background(), c.links, isConfig)
			if err != nil || !maps.Equal(got, c.want) {
				t.Fatalf("resolveConfigLinks = %q, %v\nwant %q", got, err, c.want)
			}
		})
	}

	// A name outside ASCII folds by the filesystem's own rules (APFS opens
	// ſkills for skills, ﬁrst for first), which magnum does not model: a
	// tree with such a link among links that steer the agents cannot be
	// followed. One without any link that steers them has nothing to follow.
	for _, odd := range []map[string]string{
		{".claude/skills": "../.agents/skills", ".agents/ſkills": "../elsewhere"},
		{"CLAUDE.md": "docs/ﬁrst.md"},
	} {
		if got, err := resolveConfigLinks(context.Background(), odd, isConfig); !errors.Is(err, errLinksFoldUnknown) {
			t.Fatalf("resolveConfigLinks(%q) = %q, %v; want errLinksFoldUnknown", odd, got, err)
		}
	}
	if got, err := resolveConfigLinks(context.Background(), map[string]string{"lib/ſ": "x"}, isConfig); err != nil || len(got) != 0 {
		t.Fatalf("a tree whose links steer nothing: %q, %v", got, err)
	}

	many := map[string]string{".claude/skills": "../s"}
	for i := range 60_000 {
		many[fmt.Sprintf("s/l%d", i)] = fmt.Sprintf("../t%d", i)
	}
	if _, err := resolveConfigLinks(context.Background(), many, isConfig); !errors.Is(err, errLinksTooMany) {
		t.Fatalf("a tree whose links take more than %d steps: %v, want errLinksTooMany", maxLinkSteps, err)
	}
}

// One link's text can lead to thousands of targets at once: a tree with
// every case variant of a 14-letter name as a link (16,384 of them) gives
// CLAUDE.md -> abcdefghijklmn/x.md 16,384 ways to go, and a walk that
// scanned every link for each target, checking its budget only between
// links, took minutes. The walk finds the links under a target through an
// index, counts every step it takes inside its loops, and gives up past
// the budget at once; it also stops when its context ends.
func TestResolveConfigLinksStaysWithinItsBudget(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	const name = "abcdefghijklmn"
	links := map[string]string{"CLAUDE.md": name + "/x.md"}
	for i := range 1 << len(name) {
		b := []byte(name)
		for j := range b {
			if i&(1<<j) != 0 {
				b[j] -= 'a' - 'A'
			}
		}
		links[string(b)] = fmt.Sprintf("t%d", i)
	}
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		got, err := resolveConfigLinks(context.Background(), links, isConfig)
		if err == nil && len(got) != 2*len(links)-1 {
			err = fmt.Errorf("%d targets, want %d", len(got), 2*len(links)-1)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, errLinksTooMany) {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("resolveConfigLinks of %d case variants still runs after %s", len(links), time.Since(start))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolveConfigLinks(ctx, links, isConfig); !errors.Is(err, context.Canceled) {
		t.Fatalf("a walk whose context ended: %v, want context.Canceled", err)
	}
}

// symlinksGit counts the reads of a head's tree (Tree), failing them with
// err.
type symlinksGit struct {
	fakeGit
	mu    sync.Mutex
	err   error
	reads int
}

func (g *symlinksGit) Tree(context.Context, string, string, func(string) bool) (gitx.Tree, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads++
	return gitx.Tree{}, g.err
}

func (g *symlinksGit) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads
}

// The dispatch gates, the needs-you words (every tick), auto-approval and
// the reload check all ask a head's links: a head whose links cannot be
// read is not read again on every call. A failure that a commit always
// gives (a tree past the listing's cap) is kept as long as an answer; any
// other (a commit the clone lacks yet, a git that failed) for
// linkRetryAfter, then read again.
func TestConfigLinksKeepsAHeadsFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		retake bool
	}{
		{"a tree too large", fmt.Errorf("gitx: ls-tree -r b1: %w", gitx.ErrTreeTooLarge), false},
		{"a commit the clone lacks", errors.New("gitx: ls-tree -r b1: exit status 128"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &symlinksGit{err: tc.err}
			h := newHarness(t, func(h *harness) { h.d.Git = g })
			pr := store.PR{ID: 7, HeadSHA: "b1"}
			for range 3 {
				if _, err := h.e.configLinks(h.ctx, pr); err == nil {
					t.Fatal("configLinks of a head that cannot be read: no error")
				}
			}
			if n := g.count(); n != 1 {
				t.Fatalf("reads = %d, want 1 for three asks", n)
			}
			h.advance(linkRetryAfter + time.Second)
			if _, err := h.e.configLinks(h.ctx, pr); err == nil {
				t.Fatal("configLinks after the wait: no error")
			}
			if want := 1 + btoi(tc.retake); g.count() != want {
				t.Fatalf("reads after %s = %d, want %d", linkRetryAfter, g.count(), want)
			}
		})
	}
}

// The imports of a tree's instruction files are followed agents.MaxImportHops
// hops deep (Claude Code's four, and one more), through the tree's links;
// an import whose name has a byte outside ASCII may name a link a
// case-insensitive filesystem opens for it (@ſkills/x.md, the link skills),
// so both ways are targets.
func TestResolveConfigTargetsFollowsTheInstructionsImports(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	files := map[string]string{"CLAUDE.md": "@a1.md and @ſkills/x.md\n"}
	for i := 1; i < 8; i++ {
		files[fmt.Sprintf("a%d.md", i)] = fmt.Sprintf("@a%d.md\n", i+1)
	}
	tree := gitx.Tree{Links: map[string]string{"skills": "lib/skills"}, Files: slices.Sorted(maps.Keys(files))}
	read := func(paths []string) (map[string]string, error) {
		out := map[string]string{}
		for _, p := range paths {
			if c, ok := files[p]; ok {
				out[p] = c
			}
		}
		return out, nil
	}
	got, err := resolveConfigTargets(context.Background(), tree, isConfig, read)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a1.md", "a5.md", "skills", "lib/skills/x.md", "ſkills/x.md"} {
		if got[want] != "CLAUDE.md" {
			t.Errorf("target %q = %q, want CLAUDE.md's (all: %q)", want, got[want], got)
		}
	}
	if _, ok := got["a6.md"]; ok {
		t.Errorf("a6.md is a target: past %d hops", agents.MaxImportHops)
	}
}

// A tree's files are many more than its links (a monorepo's hundred
// thousand): only the files whose imports are read count against the
// walk's budget, so such a tree with a CLAUDE.md that imports a guide
// resolves, the guide a target.
func TestALargeTreesFilesDoNotSpendTheLinksBudget(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	files := make([]string, 0, maxLinkSteps+2)
	for i := range maxLinkSteps + 1 {
		files = append(files, fmt.Sprintf("app/f%d.rb", i))
	}
	files = append(files, "CLAUDE.md")
	read := func([]string) (map[string]string, error) {
		return map[string]string{"CLAUDE.md": "@docs/guide.md\n"}, nil
	}
	got, err := resolveConfigTargets(context.Background(), gitx.Tree{Files: files}, isConfig, read)
	if err != nil || got["docs/guide.md"] != "CLAUDE.md" {
		t.Fatalf("resolveConfigTargets = %d targets (guide %q), %v", len(got), got["docs/guide.md"], err)
	}
}

// treeOf is a tree of the given files (with their contents) and links,
// and a read of those files as Blobs gives them.
func treeOf(files, links map[string]string) (gitx.Tree, func([]string) (map[string]string, error)) {
	read := func(paths []string) (map[string]string, error) {
		out := map[string]string{}
		for _, p := range paths {
			if c, ok := files[p]; ok {
				out[p] = c
			}
		}
		return out, nil
	}
	return gitx.Tree{Links: links, Files: slices.Sorted(maps.Keys(files))}, read
}

// APFS checks a link docſ -> evil out in place of a directory docs/ (it
// opens docs/ for docſ), so the team's @docs/guide.md loads the PR's
// evil/guide.md though no link steers the agents: a link whose path or
// text is outside ASCII, in a tree whose instructions import anything,
// cannot be followed (errLinksFoldUnknown). One beside instructions that
// import nothing leads nowhere the agents load.
func TestALinkOutsideASCIIBesideAnImportCannotBeFollowed(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	links := map[string]string{"docſ": "evil"}
	tree, read := treeOf(map[string]string{"AGENTS.md": "Read @docs/guide.md first.\n", "docs/guide.md": "x\n"}, links)
	if got, err := resolveConfigTargets(context.Background(), tree, isConfig, read); !errors.Is(err, errLinksFoldUnknown) {
		t.Fatalf("resolveConfigTargets = %q, %v; want errLinksFoldUnknown", got, err)
	}
	tree, read = treeOf(map[string]string{"AGENTS.md": "No imports here.\n"}, links)
	if got, err := resolveConfigTargets(context.Background(), tree, isConfig, read); err != nil || len(got) != 0 {
		t.Fatalf("instructions that import nothing: %q, %v", got, err)
	}
}

// A listed path on the way to a target changes what the agents load as
// much as the target does: a push can turn it into a link that leads
// anywhere (docs, or docſ, which APFS opens for docs, before
// docs/guide.md). changedLinks names it, or cannot tell for a name
// outside ASCII; a path beside the way changes nothing.
func TestChangedLinksCountsAPathOnTheWayToATarget(t *testing.T) {
	targets := map[string]string{"docs/guide.md": "AGENTS.md"}
	for _, c := range []struct {
		path  string
		names []string
		maybe bool
	}{
		{"Docs", []string{"AGENTS.md"}, false},
		{"docſ", nil, true},
		{"doc", nil, false},
		{"docs/other.md", nil, false},
	} {
		names, maybe := changedLinks(targets, []string{c.path})
		if !slices.Equal(names, c.names) || maybe != c.maybe {
			t.Errorf("changedLinks(%q) = %q, %v; want %q, %v", c.path, names, maybe, c.names, c.maybe)
		}
	}
}

// A case-insensitive filesystem opens docſ/guide.md for @docs/guide.md
// (agents.LooseFold): the walk reads such a file's own imports too, and
// those of a file of another case.
func TestAnImportReadsTheFilesTheFilesystemOpensForItsName(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	tree, read := treeOf(map[string]string{"AGENTS.md": "Read @docs/guide.md and @Notes/Index.md.\n", "docſ/guide.md": "@../shared/x.md\n",
		"notes/index.md": "@y.md\n"}, nil)
	got, err := resolveConfigTargets(context.Background(), tree, isConfig, read)
	if err != nil || got["shared/x.md"] != "AGENTS.md" || got["notes/y.md"] != "AGENTS.md" {
		t.Fatalf("resolveConfigTargets = %q, %v; want shared/x.md and notes/y.md of AGENTS.md", got, err)
	}
}

// A file the PR controls may hold any number of @ tokens: each import is a
// step of the walk, which gives up past its budget at once (a 1 MiB
// AGENTS.md of distinct tokens took minutes when each path was compared
// with all before it), and stops when its context ends.
func TestTheImportsOfAFileStayWithinTheWalksBudget(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	var b strings.Builder
	for i := 0; b.Len() < 1<<20; i++ {
		fmt.Fprintf(&b, "@%x\n", i)
	}
	tree, read := treeOf(map[string]string{"AGENTS.md": b.String()}, nil)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := resolveConfigTargets(context.Background(), tree, isConfig, read)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errLinksTooMany) {
			t.Fatalf("a 1 MiB AGENTS.md of imports: %v, want errLinksTooMany", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("resolveConfigTargets of a 1 MiB AGENTS.md of imports still runs after %s", time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolveConfigTargets(ctx, tree, isConfig, read); !errors.Is(err, context.Canceled) {
		t.Fatalf("a walk whose context ended: %v, want context.Canceled", err)
	}
}

// One imported file may be named many ways (16,384 case spellings of one
// name): the walk reads its imports once, not once per spelling, so a
// large file there costs one scan.
func TestAnImportedFileIsReadOncePerWalk(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	const name = "bbbbbbbbbbbbbb"
	var b strings.Builder
	for i := range 1 << len(name) {
		s := []byte(name)
		for j := range s {
			if i&(1<<j) != 0 {
				s[j] -= 'a' - 'A'
			}
		}
		fmt.Fprintf(&b, "@%s.md\n", s)
	}
	big := strings.Repeat("no imports in this line\n", (8<<20)/24)
	tree, read := treeOf(map[string]string{"AGENTS.md": "@.docs/guide.md\n", ".docs/guide.md": b.String(), ".docs/" + name + ".md": big}, nil)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		got, err := resolveConfigTargets(context.Background(), tree, isConfig, read)
		if err == nil && got[".docs/"+name+".md"] != "AGENTS.md" {
			err = fmt.Errorf("targets %d, without .docs/%s.md", len(got), name)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("resolveConfigTargets still runs after %s", time.Since(start))
	}
}

// An import's name outside ASCII may fold to a link beside it
// (foldSiblings): finding those, the walk counts each link it looks at, so
// a directory of more links than the budget fails at once, though none of
// them folds to the name.
func TestTheLinksAnImportMayFoldToCountAgainstTheBudget(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	links := map[string]string{}
	for i := range maxLinkSteps + 1 {
		links[fmt.Sprintf("d/l%d", i)] = "x"
	}
	tree, read := treeOf(map[string]string{"AGENTS.md": "@d/zé.md\n"}, links)
	if _, err := resolveConfigTargets(context.Background(), tree, isConfig, read); !errors.Is(err, errLinksTooMany) {
		t.Fatalf("an import beside %d links: %v, want errLinksTooMany", len(links), err)
	}
}

// Claude Code ends an import's path where its Markdown text token ends: a
// team's AGENTS.md that reads "Read **@docs/guide.md** first." or links
// the import ([@docs/guide.md](docs/guide.md)) loads docs/guide.md, so a
// PR that changes the guide changes what the agents load.
func TestAnImportInMarkdownsEmphasisOrALinksTextIsATarget(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	for _, text := range []string{"Read **@docs/guide.md** first.\n", "See [@docs/guide.md](docs/guide.md).\n"} {
		tree, read := treeOf(map[string]string{"AGENTS.md": text, "docs/guide.md": "x\n"}, nil)
		got, err := resolveConfigTargets(context.Background(), tree, isConfig, read)
		if names, maybe := changedLinks(got, []string{"docs/guide.md"}); err != nil || !slices.Equal(names, []string{"AGENTS.md"}) || maybe {
			t.Errorf("AGENTS.md %q: targets %q, %v: changedLinks = %q, %v; want AGENTS.md", text, got, err, names, maybe)
		}
	}
}

// One step of the walk builds a path as long as the one it extends, so a
// single long import (an AGENTS.md of 2.4 MiB that imports
// @aaa…/aaa…/…/x.md) or a chain of links, each a long way down, took
// minutes within the budget's steps. No filesystem opens a path longer
// than PATH_MAX, so an import that long imports nothing, and a path the
// walk builds costs a step per linkBytesPerStep of it: either ends at
// once.
func TestALongPathStaysWithinTheWalksBudget(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	long := "@" + strings.Repeat(strings.Repeat("a", 40)+"/", 60_000) + "x.md\n"
	chain := map[string]string{"CLAUDE.md": "l0/x.md"}
	dir := ""
	for i := range 40 {
		down := strings.Repeat("a/", 2000)
		chain[path.Join(dir, fmt.Sprintf("l%d", i))] = down + fmt.Sprintf("l%d", i+1)
		dir = path.Join(dir, down)
	}
	for name, c := range map[string]struct {
		files map[string]string
		links map[string]string
	}{
		"a long import":         {map[string]string{"AGENTS.md": long}, nil},
		"a chain of long links": {map[string]string{"AGENTS.md": "x\n"}, chain},
	} {
		t.Run(name, func(t *testing.T) {
			tree, read := treeOf(c.files, c.links)
			done := make(chan error, 1)
			start := time.Now()
			go func() {
				got, err := resolveConfigTargets(context.Background(), tree, isConfig, read)
				if err == nil && slices.ContainsFunc(slices.Collect(maps.Keys(got)), func(t string) bool { return strings.HasSuffix(t, "/x.md") }) {
					err = fmt.Errorf("a target past PATH_MAX among %d", len(got))
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, errLinksTooMany) {
					t.Fatal(err)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("resolveConfigTargets still runs after %s", time.Since(start))
			}
		})
	}
}

// countingRead is treeOf's read that counts how often each path is asked.
func countingRead(files map[string]string) (func([]string) (map[string]string, error), map[string]int) {
	asks := map[string]int{}
	_, read := treeOf(files, nil)
	return func(paths []string) (map[string]string, error) {
		for _, p := range paths {
			asks[p]++
		}
		return read(paths)
	}, asks
}

// What the walk reads of a tree's files, and how: a source is read once
// however many hops reach it; every linkBytesPerStep bytes read is a step
// (fifty instruction files of 8 MiB, each within the budget, pass it
// together); a file whose readings ImportPaths cannot all compare within
// its bound fails as one of too many imports; an import is resolved as
// the filesystem walks dir/imp (lib/../x.md through lib -> a/b/shared is
// a/b/x.md) and from each path a file is read by, through each link on the
// way (pkg/.claude -> ../.claude, then .claude/rules -> ../shared/rules:
// shared/rules/api.md is read as pkg/.claude/rules/api.md), one a
// case-insensitive filesystem opens for a link's target among them
// (a/b/ruleſ for a/b/rules).
func TestTheWalkReadsTheTreesFilesAsClaudeDoes(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	resolve := func(files, links map[string]string, read func([]string) (map[string]string, error)) (map[string]string, error) {
		tree, plain := treeOf(files, links)
		if read == nil {
			read = plain
		}
		return resolveConfigTargets(context.Background(), tree, isConfig, read)
	}

	t.Run("a source reached on two hops is read once", func(t *testing.T) {
		files := map[string]string{"AGENTS.md": "@a.md @b.md\n", "a.md": "@b.md\n", "b.md": "@c.md\n"}
		read, asks := countingRead(files)
		if got, err := resolve(files, nil, read); err != nil || got["c.md"] != "AGENTS.md" || asks["b.md"] != 1 {
			t.Fatalf("resolveConfigTargets = %q, %v; b.md asked %d times, want once", got, err, asks["b.md"])
		}
	})
	t.Run("bytes read count against the budget", func(t *testing.T) {
		big := strings.Repeat("no imports in this line\n", (8<<20)/24)
		files := map[string]string{}
		for i := range 50 {
			files[fmt.Sprintf("d%d/CLAUDE.md", i)] = big
		}
		if _, err := resolve(files, nil, nil); !errors.Is(err, errLinksTooMany) {
			t.Fatalf("fifty instruction files of 8 MiB: %v, want errLinksTooMany", err)
		}
	})
	t.Run("a file of more readings than ImportPaths compares", func(t *testing.T) {
		files := map[string]string{"AGENTS.md": "@" + strings.Repeat("a*", 1<<20) + "\n"}
		if _, err := resolve(files, nil, nil); !errors.Is(err, errLinksTooMany) {
			t.Fatalf("resolveConfigTargets = %v, want errLinksTooMany", err)
		}
	})
	t.Run("an import resolved as the filesystem walks it", func(t *testing.T) {
		files := map[string]string{"AGENTS.md": "@lib/../x.md\n"}
		if got, err := resolve(files, map[string]string{"lib": "a/b/shared"}, nil); err != nil || got["a/b/x.md"] != "AGENTS.md" ||
			got["x.md"] != "AGENTS.md" {
			t.Fatalf("resolveConfigTargets = %q, %v; want a/b/x.md and x.md", got, err)
		}
	})
	t.Run("a file read through two links on the way", func(t *testing.T) {
		files := map[string]string{"shared/rules/api.md": "@../../../x.md\n"}
		links := map[string]string{".claude/rules": "../shared/rules", "pkg/.claude": "../.claude"}
		if got, err := resolve(files, links, nil); err != nil || got["x.md"] != ".claude/rules" {
			t.Fatalf("resolveConfigTargets = %q, %v; want x.md, read as pkg/.claude/rules/api.md", got, err)
		}
	})
	t.Run("a file the filesystem opens for a link's target", func(t *testing.T) {
		files := map[string]string{"a/b/ruleſ/api.md": "@../../x.md\n"}
		got, err := resolve(files, map[string]string{".claude/rules": "../a/b/rules"}, nil)
		if err != nil || got["x.md"] != ".claude/rules" || got["a/x.md"] != ".claude/rules" {
			t.Fatalf("resolveConfigTargets = %q, %v; want x.md (read as .claude/rules/api.md) and a/x.md", got, err)
		}
	})
}

// A link outside ASCII matters to the agents only where a case-insensitive
// filesystem may check it out in place of a path they load: at, on the
// way to or under a target, or a file whose imports the walk reads (docſ
// for docs/guide.md, pKg with the Kelvin sign for pkg/CLAUDE.md,
// docs/guıde.md for the guide).
// One elsewhere in a tree whose
// instructions import (a test fixture café.txt -> data.txt) changes
// nothing they load, so the tree's targets are told.
func TestALinkOutsideASCIIAwayFromWhatTheAgentsLoadIsFollowed(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	files := map[string]string{"AGENTS.md": "Read @docs/guide.md first.\n", "docs/guide.md": "x\n", "pkg/CLAUDE.md": "No imports.\n"}
	for _, c := range []struct {
		name  string
		links map[string]string
		fails bool
	}{
		{"a fixture", map[string]string{"spec/fixtures/café.txt": "data.txt"}, false},
		{"a fixture's text", map[string]string{"spec/fixtures/data.txt": "café.txt"}, false},
		{"on the way to an import", map[string]string{"docſ": "evil"}, true},
		{"on the way to an instruction file", map[string]string{"p\u212ag": "evil"}, true}, // the Kelvin sign: pKg is pkg
		{"in place of an imported file", map[string]string{"docs/guıde.md": "../evil.md"}, true},
		{"beside an imported file", map[string]string{"docs/ſub": "../evil"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, read := treeOf(files, c.links)
			got, err := resolveConfigTargets(context.Background(), tree, isConfig, read)
			switch {
			case c.fails && !errors.Is(err, errLinksFoldUnknown):
				t.Fatalf("resolveConfigTargets = %q, %v; want errLinksFoldUnknown", got, err)
			case !c.fails && (err != nil || got["docs/guide.md"] != "AGENTS.md"):
				t.Fatalf("resolveConfigTargets = %q, %v; want docs/guide.md of AGENTS.md", got, err)
			}
		})
	}
}
