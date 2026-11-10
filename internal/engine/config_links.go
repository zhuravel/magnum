package engine

// Where the links of a head's tree that steer the review agents lead. A
// CLAUDE.md that links to docs/guide.md, an app/CLAUDE.md that links to
// ../docs/app.md, a .claude/skills that links to ../.agents/skills: an
// agent CLI follows each, so a PR that changes the file or directory one
// leads to changes what the review agents (and every teammate's CLI, once
// merged) load, while its file list names the target alone. So does an
// instruction file's @path import (agents.ImportPaths): a team's AGENTS.md
// that imports @.docs/guide.md loads the PR's guide. The gates read the
// links, and the files the instruction files import, from the head's own
// tree in the PR's clone (gitx.Client.Tree and Blobs), exact whatever head
// the PR's sessions launched on, whichever CLIs magnum runs, and whatever
// links the base branch added since.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

// maxLinkHops is how many links one path's resolution follows before it
// counts as a loop that leads nowhere (the kernel's own limit is 40).
const maxLinkHops = 40

// maxLinkSteps caps the path elements resolveConfigLinks walks for one
// tree; past it, where the links lead is not known (errLinksTooMany).
const maxLinkSteps = 100_000

// maxHeadLinks is how many heads' links headLinks keeps before it starts
// over.
const maxHeadLinks = 256

// errLinksTooMany is resolveConfigLinks' error for a tree whose links take
// more than maxLinkSteps to follow.
var errLinksTooMany = errors.New("the links take too many steps to follow")

// errLinksFoldUnknown is resolveConfigLinks' error for a tree with links
// that steer the review agents and a link whose path or text has a byte
// outside ASCII: a case-insensitive filesystem folds such a name by rules
// of its own (APFS opens .agents/ſkills for .agents/skills, ﬁrst.md for
// first.md), which magnum does not model, so where the links lead cannot
// be told.
var errLinksFoldUnknown = errors.New("a link's path or text has a byte outside ASCII, which the filesystem folds by rules magnum does not model")

// linkRetryAfter is how long configLinks keeps a head's failure that a
// fetch or a git that works again may heal (a commit the clone lacks yet,
// a git that failed) before it reads the head again.
const linkRetryAfter = 2 * time.Minute

// headLinks keeps, by head, configLinks' answer: a commit's tree never
// changes, so neither does where its links lead, nor a failure the tree
// itself gives (too large to list, links too many to follow or outside
// ASCII: lastingLinkFailure); any other failure is kept for
// linkRetryAfter. The dispatch gates, the needs-you words (every tick),
// auto-approval, the trivial-delta check and the reload check all ask it,
// so a head that cannot be read costs at most one read per head and
// linkRetryAfter (the reload check still quits a session that reloads the
// config on each such head: no proof). Its own mutex: they read it on
// their goroutines.
type headLinks struct {
	mu      sync.Mutex
	targets map[string]map[string]string // head -> target -> label
	failed  map[string]linkFailure       // head -> why its links cannot be read
	warned  map[int64]string             // pr -> the last read error logged
}

// linkFailure is a head's failure configLinks keeps: until the zero time,
// for good.
type linkFailure struct {
	err   error
	until time.Time
}

// lastingLinkFailure reports whether err is one a commit's tree always
// gives: a listing past execx.MaxOutput, links too many to follow, a link
// outside ASCII.
func lastingLinkFailure(err error) bool {
	return errors.Is(err, gitx.ErrTreeTooLarge) || errors.Is(err, errLinksTooMany) || errors.Is(err, errLinksFoldUnknown)
}

// configLinks are where the links of pr's head lead that steer the review
// agents, and the files its instruction files import (resolveConfigTargets
// over the head's tree, read in the PR's clone: headClone), each target
// with the label of the link or instruction file it is reached from;
// an error when they cannot be read (no git, no clone, a clone without the
// head, a tree too large to list, links too many to follow or outside
// ASCII, a context that ended), logged once per PR and error. Answers and
// failures are kept by head (headLinks), but for a context that ended.
func (e *Engine) configLinks(ctx context.Context, pr store.PR) (map[string]string, error) {
	head := pr.HeadSHA
	e.links.mu.Lock()
	t, ok := e.links.targets[head]
	f, failed := e.links.failed[head]
	e.links.mu.Unlock()
	switch {
	case ok:
		return t, nil
	case failed && (f.until.IsZero() || e.now().Before(f.until)):
		return nil, f.err
	}
	var err error
	if e.d.Git == nil {
		err = errors.New("no git")
	} else {
		clone := e.headClone(ctx, pr)
		var tree gitx.Tree
		if tree, err = e.d.Git.Tree(ctx, clone, head, func(string) bool { return true }); err == nil {
			kinds := e.projectKinds()
			read := func(paths []string) (map[string]string, error) { return e.d.Git.Blobs(ctx, clone, head, paths) }
			t, err = resolveConfigTargets(ctx, tree, func(p string) (string, bool) { return agentConfigPath(kinds, p) }, read)
		}
	}
	e.links.mu.Lock()
	defer e.links.mu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			return nil, err // the daemon stops or the caller gave up: nothing is known of the head
		}
		if e.links.failed == nil || len(e.links.failed) >= maxHeadLinks {
			e.links.failed = map[string]linkFailure{}
		}
		f := linkFailure{err: err}
		if !lastingLinkFailure(err) {
			f.until = e.now().Add(linkRetryAfter)
		}
		e.links.failed[head] = f
		if e.links.warned == nil {
			e.links.warned = map[int64]string{}
		}
		if e.links.warned[pr.ID] != err.Error() {
			e.links.warned[pr.ID] = err.Error()
			e.log.Warn("project config: cannot read where the links of the head's tree lead, so the gates cannot tell what it changes",
				"pr", pr.ID, "head", head, "err", err)
		}
		return nil, err
	}
	delete(e.links.failed, head)
	delete(e.links.warned, pr.ID)
	if e.links.targets == nil || len(e.links.targets) >= maxHeadLinks {
		e.links.targets = map[string]map[string]string{}
	}
	e.links.targets[head] = t
	return t, nil
}

// headClone is the clone that holds pr's head: its repository's main
// clone, into which a round's checkout fetches the head (a pool slot's and
// a per-PR worktree's alike), else the main clone of the slot the PR holds;
// "" when the registry names neither.
func (e *Engine) headClone(ctx context.Context, pr store.PR) string {
	if repo, err := e.st.RepoByID(ctx, pr.RepoID); err == nil && deref(repo.ClonePath) != "" {
		return deref(repo.ClonePath)
	}
	if sl, err := e.st.SlotByPR(ctx, pr.ID); err == nil {
		return sl.MainClone
	}
	return ""
}

// changedLinks names, by their labels, each once, in the order of their
// targets, the links of targets (configLinks) whose target paths, a PR's
// changed files, change: the target itself, or a file under it, in ASCII
// case (agents.MatchTarget). maybe reports a path with a byte outside
// ASCII that may name a target under the filesystem's own fold, which
// magnum cannot tell.
func changedLinks(targets map[string]string, paths []string) (names []string, maybe bool) {
	for _, t := range slices.Sorted(maps.Keys(targets)) {
		l := targets[t]
		for _, f := range paths {
			switch agents.MatchTarget(f, t) {
			case agents.Under:
				if !slices.Contains(names, l) {
					names = append(names, l)
				}
			case agents.MaybeUnder:
				maybe = true
			}
		}
	}
	return names, maybe
}

// resolveConfigLinks follows the links of a commit's tree (links: each
// link's path from the top, with its text) that steer the review agents
// (isConfig: the link's path is one of their instruction files or lies in
// a project config, and how a refusal names it) as a case-insensitive
// filesystem would: element by element, each element of a link's text
// that is itself a link followed before a ".." after it, names matched in
// ASCII case, the fold every such filesystem agrees on. A name outside
// ASCII folds by the filesystem's own rules (APFS: ſ is s, ß is ss, ﬁ is
// fi, a decomposed name its composed spelling), which magnum does not
// model: a tree with such a link, when any link steers the agents, fails
// (errLinksFoldUnknown), so every target returned is ASCII. It returns each path the agents load through
// such a link, with the link's label: the link itself, each link it passes
// through on the way (re-pointing one changes what loads), where it ends
// (a file, or a directory and what lies under it; "" for the tree's root,
// which covers every path), and, for each link under such a directory (a
// path in a tree is never under a link), the same again. A text that
// leaves the tree (absolute, or past its top) leads nowhere a commit
// reaches; a path that is no link in the tree is
// taken as it is, there or not (a stale target only adds refusals). A loop
// ends after maxLinkHops; past maxLinkSteps it fails (errLinksTooMany),
// and it stops with ctx's error when ctx ends.
func resolveConfigLinks(ctx context.Context, links map[string]string, isConfig func(string) (string, bool)) (map[string]string, error) {
	return resolveConfigTargets(ctx, gitx.Tree{Links: links}, isConfig, nil)
}

// resolveConfigTargets is resolveConfigLinks over tree's links, then the
// files the tree's instruction files import (imports: tree.Files, read
// with read; nil reads none), each with the label of the instruction file
// or link its import is reached from.
func resolveConfigTargets(ctx context.Context, tree gitx.Tree, isConfig func(string) (string, bool),
	read func(paths []string) (map[string]string, error)) (map[string]string, error) {
	links := tree.Links
	r := linkResolver{ctx: ctx, links: links, byFold: map[string][]string{}, out: map[string]string{}, walked: map[string]bool{},
		via: map[string][]string{}}
	paths := slices.Sorted(maps.Keys(links))
	steering := map[string]string{}
	for _, l := range paths {
		k := foldKey(l)
		r.byFold[k] = append(r.byFold[k], l)
		if label, ok := isConfig(l); ok {
			steering[l] = label
		}
	}
	if len(steering) > 0 && slices.ContainsFunc(paths, func(l string) bool { return !isASCII(l) || !isASCII(links[l]) }) {
		return nil, errLinksFoldUnknown
	}
	r.keys = slices.Sorted(maps.Keys(r.byFold))
	for _, l := range paths {
		if label, ok := steering[l]; ok && r.err == nil {
			r.follow(l, label)
		}
	}
	if read != nil && r.err == nil {
		r.imports(tree.Files, isConfig, read)
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.out, nil
}

// importSource is a file of the tree whose @path imports load: its path
// and the label of the instruction file or link it is reached from.
type importSource struct{ path, label string }

// imports adds to the walk the files the tree's instruction files import
// (agents.ImportPaths), and what those import, to agents.MaxImportHops
// hops, each with its source's label. The sources are the tree's Markdown
// files (files: its regular files) that steer the agents themselves
// (isConfig: an instruction file at any depth, a page under .claude/), and
// those at or under where a link that steers them leads (CLAUDE.md ->
// docs/guide.md, .claude/rules -> ../shared/rules), read with read in one
// batch per hop. A relative import is resolved from the source's
// directory and, for a file reached through a link, from the link's (the
// path Claude reads it by: an import in docs/guide.md read as CLAUDE.md
// is from the root), following the tree's links on the way (realpath);
// the links under a directory it names are followed too. A file it names
// becomes a source of the next hop, in any ASCII case. Every import counts
// against the walk's steps.
func (r *linkResolver) imports(files []string, isConfig func(string) (string, bool), read func(paths []string) (map[string]string, error)) {
	byFold := map[string][]string{} // foldKey of a file's path -> the tree's files that fold to it
	for _, f := range files {
		k := foldKey(f)
		byFold[k] = append(byFold[k], f)
	}
	covers := map[string]string{} // foldKey of a target -> its label
	for t, label := range r.out {
		covers[foldKey(t)] = label
	}
	var srcs []importSource
	for _, f := range files {
		if !r.step() {
			return
		}
		if !strings.EqualFold(path.Ext(f), ".md") {
			continue
		}
		if label, ok := isConfig(f); ok {
			srcs = append(srcs, importSource{f, label})
		} else if label, ok := coveredBy(covers, f); ok {
			srcs = append(srcs, importSource{f, label})
		}
	}
	read1 := map[string]bool{}
	for hop := 1; hop <= agents.MaxImportHops && len(srcs) > 0; hop++ {
		var paths []string
		for _, src := range srcs {
			if !read1[src.path] {
				read1[src.path] = true
				paths = append(paths, src.path)
			}
		}
		blobs, err := read(paths)
		if err != nil {
			r.err = err
			return
		}
		var next []importSource
		for _, src := range srcs {
			text, ok := blobs[src.path]
			if !ok {
				continue
			}
			for _, imp := range agents.ImportPaths(text) {
				for _, dir := range r.dirsOf(src.path) {
					for _, t := range r.realpath(dir, imp, 0, src.label) {
						if !r.step() {
							return
						}
						r.add(t, src.label)
						r.expand(t, src.label)
						for _, f := range byFold[foldKey(t)] {
							next = append(next, importSource{f, src.label})
						}
					}
				}
			}
		}
		srcs = next
	}
}

// coveredBy is the label of the target in covers (folded) that f is or
// lies under, in ASCII case; false for none. The target "" covers every
// path.
func coveredBy(covers map[string]string, f string) (string, bool) {
	if label, ok := covers[""]; ok {
		return label, true
	}
	for k := foldKey(f); ; {
		if label, ok := covers[k]; ok {
			return label, true
		}
		i := strings.LastIndexByte(k, '/')
		if i < 0 {
			return "", false
		}
		k = k[:i]
	}
}

// dirsOf are the directories an import in f is resolved from: f's own, and
// for each link the walk followed to f or to a directory above it (or to
// the root), the link's path with the rest of f's (the path Claude reads f
// by).
func (r *linkResolver) dirsOf(f string) []string {
	out := []string{linkDir(f)}
	alias := func(t string) {
		rest := strings.TrimPrefix(f[len(t):], "/")
		for _, l := range r.via[foldKey(t)] {
			a := l
			if rest != "" {
				a += "/" + rest
			}
			if d := linkDir(a); !slices.Contains(out, d) {
				out = append(out, d)
			}
		}
	}
	for t := f; ; {
		alias(t)
		i := strings.LastIndexByte(t, '/')
		if i < 0 {
			break
		}
		t = t[:i]
	}
	alias("")
	return out
}

// expand follows, with label, the links under t once: a directory an
// import names loads what lies under it.
func (r *linkResolver) expand(t, label string) {
	if r.walked[t] {
		return
	}
	r.walked[t] = true
	var found []string
	r.under(t, func(m string) bool {
		found = append(found, m)
		return true
	})
	for _, m := range found {
		r.follow(m, label)
	}
}

// linkResolver is resolveConfigLinks' walk of one tree. Every step of its
// loops counts against maxLinkSteps (step), so one link that leads many
// ways (links whose paths differ in case only) stops the walk as soon as
// it passes the budget, and the context is asked every linkCtxEvery steps.
type linkResolver struct {
	ctx    context.Context
	links  map[string]string
	byFold map[string][]string // foldKey of a path -> the links' paths that fold to it
	keys   []string            // byFold's keys, sorted: the links under a path are a run of them (under)
	out    map[string]string   // target -> label
	walked map[string]bool     // the targets whose links below were followed
	via    map[string][]string // foldKey of a target -> the links that lead to it (dirsOf)
	steps  int
	err    error // errLinksTooMany, or the context's: the walk stopped
}

// linkCtxEvery is how many steps the walk takes between two looks at its
// context.
const linkCtxEvery = 1024

// step counts one step of the walk; false once the walk must stop (r.err):
// past maxLinkSteps, or its context ended.
func (r *linkResolver) step() bool {
	if r.err != nil {
		return false
	}
	r.steps++
	switch {
	case r.steps > maxLinkSteps:
		r.err = fmt.Errorf("%w (more than %d)", errLinksTooMany, maxLinkSteps)
	case r.steps%linkCtxEvery == 0 && r.ctx.Err() != nil:
		r.err = r.ctx.Err()
	}
	return r.err == nil
}

// under calls fn with each link's path that lies under t (not t itself),
// in ASCII case: the run of the sorted folded keys that start with t's
// and a "/", found by a binary search rather than a scan of every link;
// every link for t "" (a link to the tree's root, .claude/skills -> ..).
// It stops when fn or the walk's budget says so.
func (r *linkResolver) under(t string, fn func(l string) bool) {
	prefix := ""
	if t != "" {
		prefix = foldKey(t) + "/"
	}
	i, _ := slices.BinarySearch(r.keys, prefix)
	for ; i < len(r.keys) && strings.HasPrefix(r.keys[i], prefix); i++ {
		for _, l := range r.byFold[r.keys[i]] {
			if !r.step() || !fn(l) {
				return
			}
		}
	}
}

// add records p for label, unless a label is recorded for it already.
func (r *linkResolver) add(p, label string) {
	if _, ok := r.out[p]; !ok {
		r.out[p] = label
	}
}

// follow records l, a link that steers the review agents, where it leads,
// and the links under a directory there, with label.
func (r *linkResolver) follow(l, label string) {
	queue := []string{l}
	for len(queue) > 0 && r.step() {
		l := queue[0]
		queue = queue[1:]
		r.add(l, label)
		for _, t := range r.realpath(linkDir(l), r.links[l], 0, label) {
			if !r.step() {
				return
			}
			r.add(t, label)
			if k := foldKey(t); !slices.Contains(r.via[k], l) {
				r.via[k] = append(r.via[k], l)
			}
			if r.walked[t] {
				continue
			}
			r.walked[t] = true
			r.under(t, func(m string) bool {
				queue = append(queue, m)
				return true
			})
		}
	}
}

// realpath is where text, a link's text in the directory dir ("" for the
// top), leads in the tree: the paths (one per way a name folded in ASCII
// case may go, when the tree holds links whose paths differ in case only), none
// when it leaves the tree, loops or runs out of steps. Each link it passes
// through is recorded with label.
func (r *linkResolver) realpath(dir, text string, hops int, label string) []string {
	if path.IsAbs(text) || hops > maxLinkHops {
		return nil
	}
	cur := []string{dir}
	for elem := range strings.SplitSeq(text, "/") {
		var next []string
		for _, c := range cur {
			if !r.step() {
				return nil
			}
			switch elem {
			case "", ".":
				next = append(next, c)
			case "..":
				if c != "" {
					next = append(next, linkDir(c))
				}
			default:
				p := elem
				if c != "" {
					p = c + "/" + elem
				}
				via := r.byFold[foldKey(p)]
				if !isASCII(elem) { // an import's name: it may fold to a link of c's
					via = append(slices.Clone(via), r.foldSiblings(c, elem)...)
				}
				if len(via) == 0 || !isASCII(elem) {
					next = append(next, p)
				}
				for _, l := range via {
					if !r.step() {
						return nil
					}
					r.add(l, label)
					next = append(next, r.realpath(linkDir(l), r.links[l], hops+1, label)...)
				}
			}
		}
		slices.Sort(next)
		if cur = slices.Compact(next); len(cur) == 0 {
			return nil
		}
	}
	return cur
}

// foldSiblings are the links right in dir ("" for the top) whose names a
// case-insensitive filesystem may open for elem, a name with a byte
// outside ASCII (agents.MayFold): an import's @ſkills/x may lead through a
// link skills, which the walk then follows too.
func (r *linkResolver) foldSiblings(dir, elem string) []string {
	var out []string
	r.under(dir, func(l string) bool {
		rest := l
		if dir != "" {
			rest = l[len(dir)+1:]
		}
		if !strings.Contains(rest, "/") && agents.MayFold(rest, elem) {
			out = append(out, l)
		}
		return true
	})
	return out
}

// linkDir is the directory of p, a path from the top ("" for the top).
func linkDir(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// foldKey is s with its ASCII letters in lower case, every other byte as it
// is: the key two paths a case-insensitive filesystem surely opens as one
// share (resolveConfigLinks refuses a name outside ASCII).
func foldKey(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// isASCII reports whether s has no byte outside ASCII.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
