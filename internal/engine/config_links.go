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

// errLinksFoldUnknown is resolveConfigTargets' error for a tree with a
// link whose path or text has a byte outside ASCII and a link that steers
// the agents, or with such a link where they load through imports (at, on
// the way to or under a target or a file whose imports are read:
// foldsOnto): a case-insensitive filesystem folds such a name by rules of
// its own (APFS opens .agents/ſkills for .agents/skills, ﬁrst.md for
// first.md, and checks docſ -> evil out in place of docs/, which
// @docs/guide.md then reads), which magnum does not model, so where the
// links lead cannot be told.
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

// headFetched drops what configLinks keeps of head's failure, unless the
// tree itself gives it (lastingLinkFailure): a fetch or a checkout just
// brought head into the clone, which may have lacked it when a tick asked
// (a fork's head before any round), so its links are read again rather
// than taken as unreadable until linkRetryAfter.
func (e *Engine) headFetched(head string) {
	e.links.mu.Lock()
	defer e.links.mu.Unlock()
	if f, ok := e.links.failed[head]; ok && !f.until.IsZero() {
		delete(e.links.failed, head)
	}
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
// changed files, change: the target itself, a file under it, or a path on
// the way to it (a push can turn docs into a link that leads anywhere
// before docs/guide.md), in ASCII case (agents.MatchTarget). maybe reports
// a path with a byte outside ASCII that may name a target, or a path on
// the way to one, under the filesystem's own fold (docſ, which APFS opens
// for docs), which magnum cannot tell.
func changedLinks(targets map[string]string, paths []string) (names []string, maybe bool) {
	for _, t := range slices.Sorted(maps.Keys(targets)) {
		l := targets[t]
		for _, f := range paths {
			m := agents.MatchTarget(f, t)
			if m == agents.NotUnder {
				m = agents.MatchTarget(t, f)
			}
			switch m {
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
// model: a tree with such a link (at a path git checks out, no longer
// than maxPathBytes), when any link steers the agents (or,
// in resolveConfigTargets, where it may stand for a path the agents load
// through imports: foldsOnto), fails (errLinksFoldUnknown). It returns each path the agents load through
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
		via: map[string][]viaLink{}, sources: map[string]bool{}}
	paths := slices.Sorted(maps.Keys(links))
	steering := map[string]string{}
	var odd []string // the links whose path or text has a byte outside ASCII, at a path git checks out
	for _, l := range paths {
		k := foldKey(l)
		r.byFold[k] = append(r.byFold[k], l)
		if label, ok := isConfig(l); ok {
			steering[l] = label
		}
		if (!isASCII(l) || !isASCII(links[l])) && len(l) <= maxPathBytes {
			odd = append(odd, l)
		}
	}
	if len(steering) > 0 && len(odd) > 0 {
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
	if len(odd) > 0 && len(r.out) > 0 && r.err == nil && r.foldsOnto(odd) {
		// No link steers the agents, but the instructions import: APFS
		// checks docſ -> evil out in place of docs/, and @docs/guide.md
		// then loads evil/guide.md.
		return nil, errLinksFoldUnknown
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.out, nil
}

// foldsOnto reports whether a link of odd (one whose path or text has a
// byte outside ASCII) may be at, on the way to or under a path the agents
// load through the walk, a target or a file whose imports it read, or such
// a path under it (agents.MatchTarget, either way): a case-insensitive
// filesystem may check such a link out in place of one of those (APFS:
// docſ -> evil for docs/, which @docs/guide.md then reads) by rules magnum
// does not model. A link elsewhere (a test fixture café.txt) changes
// nothing they load, nor does a path longer than maxPathBytes, which git
// checks out nowhere (resolveConfigTargets leaves such a link out of
// odd). Each comparison is a step of the walk.
func (r *linkResolver) foldsOnto(odd []string) bool {
	var loaded []string
	for _, p := range slices.Sorted(maps.Keys(r.out)) {
		if len(p) <= maxPathBytes {
			loaded = append(loaded, p)
		}
	}
	for _, f := range slices.Sorted(maps.Keys(r.sources)) {
		if _, ok := r.out[f]; !ok && len(f) <= maxPathBytes {
			loaded = append(loaded, f)
		}
	}
	for _, l := range odd {
		for _, p := range loaded {
			if !r.step() {
				return false
			}
			if agents.MatchTarget(p, l) != agents.NotUnder || agents.MatchTarget(l, p) != agents.NotUnder {
				return true
			}
		}
	}
	return false
}

// importSource is a file of the tree whose @path imports load: its path,
// the label of the instruction file or link it is reached from, and the
// directories it was named from besides its own and its links' (dirsOf):
// an import's @lib/guide.md, through lib -> a/b/shared, is read by
// Claude as lib/guide.md, whose own imports resolve from lib/.
type importSource struct {
	path, label string
	dirs        []string
	has         map[string]bool // foldKey of each of dirs
}

// linkBytesPerStep is how many bytes of a file's text the import walk
// scans for one step of its budget.
const linkBytesPerStep = 4096

// imports adds to the walk the files the tree's instruction files import
// (agents.ImportPaths), and what those import, to agents.MaxImportHops
// hops, each with its source's label. The sources are the tree's Markdown
// files (files: its regular files) that steer the agents themselves
// (isConfig: an instruction file at any depth, a page under .claude/),
// those at or under where a link that steers them leads (CLAUDE.md ->
// docs/guide.md, .claude/rules -> ../shared/rules), and a file of any name
// a link leads to that Claude reads by the link's path, an instruction
// file's or a page's (CLAUDE.md -> .cursorrules), read with read in one
// batch per hop, each file once. An import resolves as Claude opens it:
// joined to the directory of each path the source is read by (dirsOf: its
// own, a link's that leads to it or to a directory above it, the one an
// import named it from), as the filesystem resolves that path (realpath,
// through the tree's links) and as it is with its ".." taken first
// (path.resolve's). A directory of the tree it names (and no file) is
// that path alone (agents.ExactTarget), the root none: Claude Code loads
// no directory an import names, nor anything under it, and code an
// instruction file quotes (`^.*@.*$` gives the root) would make every
// change one. A file the filesystem may open for it (agents.LooseFold: Docs/Guide.md,
// docſ/guide.md for docs/guide.md) becomes a source of the next hop. Every
// source, every import and every linkBytesPerStep bytes scanned count
// against the walk's steps (a tree of more files than maxLinkSteps is no
// error by itself), and a file of more imports than the budget left fails
// at once (errLinksTooMany).
func (r *linkResolver) imports(files []string, isConfig func(string) (string, bool), read func(paths []string) (map[string]string, error)) {
	byName := map[string][]string{} // agents.LooseFold of a file's path -> the tree's files with that key
	for _, f := range files {
		k := agents.LooseFold(f)
		byName[k] = append(byName[k], f)
	}
	covers := map[string]string{} // agents.LooseFold of a target -> its label
	for t, label := range r.out {
		covers[agents.LooseFold(t)] = label
	}
	var dirs map[string]bool // agents.LooseFold of each directory of the tree, read once an import needs it
	isDir := func(t string) bool {
		if t == "" {
			return true
		}
		if dirs == nil {
			dirs = map[string]bool{}
			for _, f := range append(slices.Collect(maps.Keys(byName)), slices.Collect(maps.Keys(r.links))...) {
				k := agents.LooseFold(f)
				for i := strings.LastIndexByte(k, '/'); i > 0 && !dirs[k[:i]]; i = strings.LastIndexByte(k[:i], '/') {
					dirs[k[:i]] = true
				}
			}
		}
		return dirs[agents.LooseFold(t)]
	}
	steers := func(f string) (string, bool) { // a Markdown file that steers the agents, by its own path
		if !strings.EqualFold(path.Ext(f), ".md") {
			return "", false
		}
		if label, ok := isConfig(f); ok {
			return label, true
		}
		return coveredBy(covers, f)
	}
	var srcs []importSource
	at := map[string]int{}
	for _, f := range files { // the tree's every file, which the listing's cap bounds: only a source is a step
		label, ok := steers(f)
		for _, l := range r.via[agents.LooseFold(f)] { // a link of the walk that leads to f: Claude reads f by l
			if ok {
				break
			}
			label, ok = steers(l.path)
		}
		if ok && !r.addSource(&srcs, at, f, label) {
			return
		}
	}
	parsed := map[string][]string{} // a source's path -> its imports, read once
	asked := map[string]bool{}
	done := map[string]map[string]bool{} // a source's path -> foldKey of the directories its imports were resolved from
	for hop := 1; hop <= agents.MaxImportHops && len(srcs) > 0; hop++ {
		var need []string
		for _, src := range srcs {
			if !asked[src.path] {
				asked[src.path] = true
				need = append(need, src.path)
			}
		}
		if len(need) > 0 {
			blobs, err := read(need)
			if err != nil {
				r.err = err
				return
			}
			for _, p := range need {
				text, ok := blobs[p]
				if !ok {
					continue
				}
				for range len(text)/linkBytesPerStep + 1 {
					if !r.step() {
						return
					}
				}
				if err := r.ctx.Err(); err != nil { // a scan of up to the read's cap: ask before it
					r.err = err
					return
				}
				imps, more := agents.ImportPaths(text, max(maxLinkSteps-r.steps, 0))
				if more {
					r.err = fmt.Errorf("%w (more than %d)", errLinksTooMany, maxLinkSteps)
					return
				}
				parsed[p] = imps
			}
		}
		var next []importSource
		nextAt := map[string]int{}
		for _, src := range srcs {
			imps := parsed[src.path]
			if len(imps) == 0 {
				continue
			}
			if done[src.path] == nil {
				done[src.path] = map[string]bool{}
			}
			var dirs []string
			for _, d := range append(r.dirsOf(src.path), src.dirs...) {
				if k := foldKey(d); !done[src.path][k] {
					done[src.path][k] = true
					dirs = append(dirs, d)
				}
			}
			for _, imp := range imps {
				if !r.step() {
					return
				}
				for _, dir := range dirs {
					opened, named := importOpens(dir, imp)
					for _, p := range opened {
						for _, t := range r.realpath("", p, 0, src.label) {
							if !r.step() {
								return
							}
							if len(byName[agents.LooseFold(t)]) == 0 && isDir(t) {
								if t != "" { // Claude loads no directory an import names: the path alone
									r.add(agents.ExactTarget(t), src.label)
								}
								continue
							}
							r.add(t, src.label)
							for _, f := range byName[agents.LooseFold(t)] {
								var from []string
								if named != "" {
									from = append(from, linkDir(named))
								}
								if !r.addSource(&next, nextAt, f, src.label, from...) {
									return
								}
							}
						}
					}
				}
			}
		}
		srcs = next
	}
}

// addSource puts f, with label and the directories dirs, in list once (at
// its index in at), merging the directories of a second entry, and counts
// a step; false once the walk must stop.
func (r *linkResolver) addSource(list *[]importSource, at map[string]int, f, label string, dirs ...string) bool {
	if !r.step() {
		return false
	}
	r.sources[f] = true
	i, ok := at[f]
	if !ok {
		i = len(*list)
		at[f] = i
		*list = append(*list, importSource{path: f, label: label, has: map[string]bool{}})
	}
	src := &(*list)[i]
	for _, d := range dirs {
		if k := foldKey(d); !src.has[k] {
			src.has[k] = true
			src.dirs = append(src.dirs, d)
		}
	}
	return true
}

// importOpens are the paths, from the top, Claude may open for imp, an
// import of a file read from the directory dir: dir/imp as the filesystem
// resolves it (a ".." after a link: the link's target's parent), and
// named, dir/imp with its ".." taken first as Node's path.resolve does
// ("" when that leaves the tree), which is also the path the imported file
// is then read by.
func importOpens(dir, imp string) (opened []string, named string) {
	joined := imp
	if dir != "" {
		joined = dir + "/" + imp
	}
	opened = []string{joined}
	switch named = path.Clean(joined); {
	case named == "." || named == ".." || strings.HasPrefix(named, "../"):
		named = ""
	case named != joined:
		opened = append(opened, named)
	}
	return opened, named
}

// coveredBy is the label of the target in covers (keyed by
// agents.LooseFold) that f is or lies under; false for none. The target
// "" covers every path.
func coveredBy(covers map[string]string, f string) (string, bool) {
	if label, ok := covers[""]; ok {
		return label, true
	}
	for k := agents.LooseFold(f); ; {
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

// dirsOf are the directories an import in f is resolved from: those of
// each path Claude may read f by, f itself and, for each link the walk
// followed to f or to a directory above it (or to the root, or to a path
// a case-insensitive filesystem may open for one: agents.LooseFold), the
// link's path with the rest of f's, and so on for that path's own links
// (.claude/rules -> ../shared/rules, shared/rules/api.md -> ../../x.txt:
// x.txt is read as .claude/rules/api.md). A link that lies at or under
// where it leads (.claude/rules/back -> .., a loop; a link to the root)
// gives no path, nor does a link a path was already built with, or a
// path longer than maxPathBytes: Claude Code's rules walk enters each
// directory once, and opens nothing that long, so the paths end. Each
// path built, each lookup that finds links and every linkBytesPerStep
// bytes of the keys looked up count a step: a path of ordinary length
// costs nothing where no link leads, and a deep one its hashing.
func (r *linkResolver) dirsOf(f string) []string {
	if len(r.via) == 0 {
		return []string{linkDir(f)}
	}
	type named struct {
		path string
		from int    // the index of the path it was built from, -1 for f
		link string // the link it was built with
	}
	paths := []named{{path: f, from: -1}}
	built := func(i int, l string) bool { // whether l built paths[i] or a path it was built from
		for ; i >= 0; i = paths[i].from {
			if paths[i].link == l {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{foldKey(f): true}
	hashed := 0 // bytes of keys looked up that no step counted yet
	for i := 0; i < len(paths); i++ {
		q := paths[i].path
		t, k := q, agents.LooseFold(q) // a directory of q, with its key (LooseFold keeps each /)
		for {
			links := r.via[k]
			hashed += len(k) + 1
			n := hashed / linkBytesPerStep
			if hashed %= linkBytesPerStep; len(links) > 0 {
				n = max(n, 1)
			}
			for range n {
				if !r.step() {
					return nil
				}
			}
			rest := strings.TrimPrefix(q[len(t):], "/")
			for _, v := range links {
				if k == "" || atOrUnder(v.key, k) || built(i, v.path) {
					continue
				}
				a := v.path
				if rest != "" {
					a += "/" + rest
				}
				if fk := foldKey(a); len(a) <= maxPathBytes && !seen[fk] {
					if !r.step() {
						return nil
					}
					seen[fk] = true
					paths = append(paths, named{path: a, from: i, link: v.path})
				}
			}
			if t == "" {
				break
			}
			t = linkDir(t)
			if j := strings.LastIndexByte(k, '/'); j >= 0 {
				k = k[:j]
			} else {
				k = ""
			}
		}
	}
	var out []string
	dirs := map[string]bool{}
	for _, p := range paths {
		if d := linkDir(p.path); !dirs[d] {
			dirs[d] = true
			out = append(out, d)
		}
	}
	return out
}

// linkResolver is resolveConfigLinks' walk of one tree. Every step of its
// loops counts against maxLinkSteps (step), so one link that leads many
// ways (links whose paths differ in case only) stops the walk as soon as
// it passes the budget, and the context is asked every linkCtxEvery steps.
type linkResolver struct {
	ctx     context.Context
	links   map[string]string
	byFold  map[string][]string  // foldKey of a path -> the links' paths that fold to it
	keys    []string             // byFold's keys, sorted: the links under a path are a run of them (under)
	out     map[string]string    // target -> label
	walked  map[string]bool      // the targets whose links below were followed (follow)
	via     map[string][]viaLink // agents.LooseFold of a target -> the links that lead to it (dirsOf)
	sources map[string]bool      // the files whose imports the walk read (foldsOnto)
	steps   int
	err     error // errLinksTooMany, or the context's: the walk stopped
}

// viaLink is a link the walk followed to a target: its path, and the
// path's agents.LooseFold, which dirsOf compares at each lookup.
type viaLink struct {
	path, key string
}

// atOrUnder reports whether the key p is the key dir or lies under it.
func atOrUnder(p, dir string) bool {
	return strings.HasPrefix(p, dir) && (len(p) == len(dir) || p[len(dir)] == '/')
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
			if k := agents.LooseFold(t); !slices.ContainsFunc(r.via[k], func(v viaLink) bool { return v.path == l }) {
				r.via[k] = append(r.via[k], viaLink{path: l, key: agents.LooseFold(l)})
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

// maxPathBytes is PATH_MAX (Linux's; macOS's is 1024): git checks out no
// file or link at a longer path, and no filesystem opens one or holds a
// link's text that long.
const maxPathBytes = 4096

// realpath is where text, a link's text in the directory dir ("" for the
// top), leads in the tree: the paths (one per way a name folded in ASCII
// case may go, when the tree holds links whose paths differ in case only), none
// when it leaves the tree, loops, runs out of steps or is longer than
// maxPathBytes (a text, or a path it builds: nothing is checked out
// there). Each link it passes through is recorded with label.
func (r *linkResolver) realpath(dir, text string, hops int, label string) []string {
	if path.IsAbs(text) || hops > maxLinkHops || len(text) > maxPathBytes {
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
				if len(p) > maxPathBytes {
					continue
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
