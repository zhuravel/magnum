package engine

// Where the links of a head's tree that steer the review agents lead. A
// CLAUDE.md that links to docs/guide.md, an app/CLAUDE.md that links to
// ../docs/app.md, a .claude/skills that links to ../.agents/skills: an
// agent CLI follows each, so a PR that changes the file or directory one
// leads to changes what the review agents (and every teammate's CLI, once
// merged) load, while its file list names the target alone. The gates read
// the links from the head's own tree in the PR's clone (gitx.Symlinks),
// exact whatever head the PR's sessions launched on, whichever CLIs
// magnum runs, and whatever links the base branch added since.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/zhuravel/magnum/internal/agents"
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

// headLinks keeps, by head, configLinks' answer: a commit's tree never
// changes, so neither does where its links lead. Its own mutex: a round's
// dispatch gates, auto-approval and the needs-you words read it on their
// goroutines.
type headLinks struct {
	mu      sync.Mutex
	targets map[string]map[string]string // head -> target -> label
	warned  map[int64]string             // pr -> the last read error logged
}

// configLinks are where the links of pr's head lead that steer the review
// agents (resolveConfigLinks over the head's tree, read in the PR's clone:
// headClone), each target with the label of the link it is reached from;
// an error when they cannot be read (no git, no clone, a clone without the
// head, a tree too large to list, links too many to follow), logged once
// per PR and error.
func (e *Engine) configLinks(ctx context.Context, pr store.PR) (map[string]string, error) {
	head := pr.HeadSHA
	e.links.mu.Lock()
	t, ok := e.links.targets[head]
	e.links.mu.Unlock()
	if ok {
		return t, nil
	}
	var err error
	if e.d.Git == nil {
		err = errors.New("no git")
	} else {
		var links map[string]string
		if links, err = e.d.Git.Symlinks(ctx, e.headClone(ctx, pr), head); err == nil {
			kinds := e.projectKinds()
			t, err = resolveConfigLinks(links, func(p string) (string, bool) { return agentConfigPath(kinds, p) })
		}
	}
	e.links.mu.Lock()
	defer e.links.mu.Unlock()
	if err != nil {
		if e.links.warned == nil {
			e.links.warned = map[int64]string{}
		}
		if e.links.warned[pr.ID] != err.Error() && ctx.Err() == nil {
			e.links.warned[pr.ID] = err.Error()
			e.log.Warn("project config: cannot read where the links of the head's tree lead, so the gates cannot tell what it changes",
				"pr", pr.ID, "head", head, "err", err)
		}
		return nil, err
	}
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
// (a file, or a directory and what lies under it), and, for each link
// under such a directory (a path in a tree is never under a link), the
// same again. A text that leaves the tree (absolute, or past its top)
// leads nowhere a commit reaches; a path that is no link in the tree is
// taken as it is, there or not (a stale target only adds refusals). A loop
// ends after maxLinkHops; past maxLinkSteps it fails (errLinksTooMany).
func resolveConfigLinks(links map[string]string, isConfig func(string) (string, bool)) (map[string]string, error) {
	r := linkResolver{links: links, byFold: map[string][]string{}, out: map[string]string{}, walked: map[string]bool{}}
	r.paths = slices.Sorted(maps.Keys(links))
	steering := map[string]string{}
	for _, l := range r.paths {
		k := foldKey(l)
		r.byFold[k] = append(r.byFold[k], l)
		if label, ok := isConfig(l); ok {
			steering[l] = label
		}
	}
	if len(steering) > 0 && slices.ContainsFunc(r.paths, func(l string) bool { return !isASCII(l) || !isASCII(links[l]) }) {
		return nil, errLinksFoldUnknown
	}
	for _, l := range r.paths {
		if label, ok := steering[l]; ok {
			r.follow(l, label)
		}
	}
	if r.steps > maxLinkSteps {
		return nil, fmt.Errorf("%w (more than %d)", errLinksTooMany, maxLinkSteps)
	}
	return r.out, nil
}

// linkResolver is resolveConfigLinks' walk of one tree.
type linkResolver struct {
	links  map[string]string
	paths  []string            // links' paths, sorted
	byFold map[string][]string // foldKey of a path -> the links' paths that fold to it
	out    map[string]string   // target -> label
	walked map[string]bool     // the targets whose links below were followed
	steps  int
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
	for len(queue) > 0 && r.steps <= maxLinkSteps {
		l := queue[0]
		queue = queue[1:]
		r.add(l, label)
		for _, t := range r.realpath(linkDir(l), r.links[l], 0, label) {
			r.add(t, label)
			if r.walked[t] {
				continue
			}
			r.walked[t] = true
			for _, m := range r.paths {
				r.steps++
				if m != t && agents.UnderTarget(m, t) {
					queue = append(queue, m)
				}
			}
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
			if r.steps++; r.steps > maxLinkSteps {
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
				if len(via) == 0 {
					next = append(next, p)
				}
				for _, l := range via {
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
