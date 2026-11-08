package staging

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGit is a mirror in memory: a commit graph and the refs the remote
// has, which a fetch makes the mirror's. It counts its calls.
type fakeGit struct {
	mu       sync.Mutex
	base     string              // the remote's default branch
	commits  map[string]Commit   // by sha
	refs     map[string]string   // the remote's refs, by full name
	paths    map[string][]string // ChangedPaths by head
	missing  map[string]bool     // refs a fetch says the remote lacks
	fetchErr error               // every fetch fails with it
	calls    map[string]int      // by method
	fetched  [][]string          // each fetch's refspecs
	inFetch  func()              // called inside each fetch, outside the lock
	// graphErr, when set, fails the Graph calls it returns an error for.
	graphErr func(include []string) error
	// absent: there is no mirror until a Clone; clone, when set, runs inside
	// each Clone, outside the lock, and its error fails it.
	absent bool
	clone  func(ctx context.Context) error
}

func newFakeGit() *fakeGit {
	return &fakeGit{base: "master", commits: map[string]Commit{}, refs: map[string]string{},
		paths: map[string][]string{}, missing: map[string]bool{}, calls: map[string]int{}}
}

func (g *fakeGit) count(method string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls[method]++
}

// total is how many calls the fake answered.
func (g *fakeGit) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, v := range g.calls {
		n += v
	}
	return n
}

func (g *fakeGit) n(method string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[method]
}

func (g *fakeGit) Base(_ context.Context, dir, repo string) (string, error) {
	g.count("Base")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.absent {
		return "", fmt.Errorf("%s: %w", dir, ErrNoMirror)
	}
	return g.base, nil
}

func (g *fakeGit) Clone(ctx context.Context, dir, repo string, env map[string]string) error {
	g.count("Clone")
	if f := g.clone; f != nil {
		if err := f(ctx); err != nil {
			return err
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.absent = false
	return nil
}

func (g *fakeGit) Fetch(_ context.Context, dir string, env map[string]string, refspecs []string) error {
	g.count("Fetch")
	if f := g.inFetch; f != nil {
		f()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fetched = append(g.fetched, slices.Clone(refspecs))
	if g.fetchErr != nil {
		return g.fetchErr
	}
	for _, spec := range refspecs {
		src, _, _ := strings.Cut(strings.TrimPrefix(spec, "+"), ":")
		if g.missing[src] {
			return &MissingRefError{Ref: src}
		}
	}
	return nil
}

func (g *fakeGit) Refs(_ context.Context, dir string, prefixes ...string) (map[string]string, error) {
	g.count("Refs")
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]string{}
	for name, sha := range g.refs {
		if slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
			out[name] = sha
		}
	}
	return out, nil
}

// resolve is a ref's sha, or rev itself.
func (g *fakeGit) resolve(rev string) string {
	if sha, ok := g.refs[rev]; ok {
		return sha
	}
	return rev
}

func (g *fakeGit) FirstParent(_ context.Context, dir, rev string, limit int) ([]Commit, error) {
	g.count("FirstParent")
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Commit
	for sha := g.resolve(rev); sha != "" && len(out) < limit; {
		c, ok := g.commits[sha]
		if !ok {
			return nil, fmt.Errorf("bad revision %s", sha)
		}
		out = append(out, c)
		sha = ""
		if len(c.Parents) > 0 {
			sha = c.Parents[0]
		}
	}
	return out, nil
}

// reach is every commit reachable from revs.
func (g *fakeGit) reach(revs []string) (map[string]bool, error) {
	seen := map[string]bool{}
	stack := make([]string, 0, len(revs))
	for _, r := range revs {
		stack = append(stack, g.resolve(r))
	}
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[sha] {
			continue
		}
		c, ok := g.commits[sha]
		if !ok {
			return nil, fmt.Errorf("bad revision %s", sha)
		}
		seen[sha] = true
		stack = append(stack, c.Parents...)
	}
	return seen, nil
}

func (g *fakeGit) Graph(_ context.Context, dir string, include, exclude []string) ([]Commit, error) {
	g.count("Graph")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.graphErr != nil {
		if err := g.graphErr(include); err != nil {
			return nil, err
		}
	}
	in, err := g.reach(include)
	if err != nil {
		return nil, err
	}
	out, err := g.reach(exclude)
	if err != nil {
		return nil, err
	}
	// Children before parents: a depth-first post-order over the parents,
	// reversed.
	var post []string
	done := map[string]bool{}
	var visit func(sha string)
	visit = func(sha string) {
		if done[sha] || !in[sha] || out[sha] {
			return
		}
		done[sha] = true
		for _, p := range g.commits[sha].Parents {
			visit(p)
		}
		post = append(post, sha)
	}
	for _, r := range include {
		visit(g.resolve(r))
	}
	slices.Reverse(post)
	list := make([]Commit, len(post))
	for i, sha := range post {
		c := g.commits[sha]
		list[i] = Commit{SHA: sha, Parents: c.Parents}
	}
	return list, nil
}

func (g *fakeGit) ChangedPaths(_ context.Context, dir string, env map[string]string, base, head string) ([]string, error) {
	g.count("ChangedPaths")
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.paths[head]
	if !ok {
		return nil, fmt.Errorf("no paths for %s", head)
	}
	return p, nil
}

// fakeGitHub answers deployments and runs from maps and counts its calls.
type fakeGitHub struct {
	mu        sync.Mutex
	deploys   map[string][]Deployment // by environment
	etags     map[string]string       // the environment's current ETag
	deployErr map[string]error
	runs      map[string][]Run // by sha
	calls     map[string]int
	etagSent  []string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{deploys: map[string][]Deployment{}, etags: map[string]string{}, deployErr: map[string]error{},
		runs: map[string][]Run{}, calls: map[string]int{}}
}

func (h *fakeGitHub) DeploymentsOf(_ context.Context, repo, environment, etag string) (Deployments, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls["DeploymentsOf"]++
	h.etagSent = append(h.etagSent, etag)
	if err := h.deployErr[environment]; err != nil {
		return Deployments{}, err
	}
	cur := h.etags[environment]
	if cur == "" { // the list's content, as GitHub's ETag follows it
		var ids []int64
		for _, d := range h.deploys[environment] {
			ids = append(ids, d.ID)
		}
		cur = fmt.Sprintf("W/%q", fmt.Sprint(ids))
	}
	if etag != "" && etag == cur {
		return Deployments{ETag: cur, NotModified: true}, nil
	}
	return Deployments{ETag: cur, List: slices.Clone(h.deploys[environment])}, nil
}

func (h *fakeGitHub) RunsOf(_ context.Context, repo, workflow, sha string) ([]Run, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls["RunsOf"]++
	if workflow != Workflow {
		return nil, fmt.Errorf("workflow %s, want %s", workflow, Workflow)
	}
	return slices.Clone(h.runs[sha]), nil
}

// runsWithoutPaths is RunsOf without the failed jobs' annotations: what a
// previous tip's run needs (the job names only).
func (h *fakeGitHub) runsWithoutPaths(ctx context.Context, repo, workflow, sha string) ([]Run, error) {
	h.mu.Lock()
	h.calls["runsWithoutPaths"]++
	h.mu.Unlock()
	runs, err := h.RunsOf(ctx, repo, workflow, sha)
	h.mu.Lock()
	h.calls["RunsOf"]--
	h.mu.Unlock()
	return runs, err
}

func (h *fakeGitHub) n(method string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[method]
}

func (h *fakeGitHub) total() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for v := range maps.Values(h.calls) {
		n += v
	}
	return n
}

// world is a repository in the fakes: its commits get synthetic shas and
// committer times relative to now.
type world struct {
	t   *testing.T
	git *fakeGit
	gh  *fakeGitHub
	now time.Time
	n   int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return &world{t: t, git: newFakeGit(), gh: newFakeGitHub(), now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
}

// commit adds a commit made ago before now with parents.
func (w *world) commit(ago time.Duration, parents ...string) string {
	w.n++
	sha := fmt.Sprintf("%040x", w.n)
	w.git.commits[sha] = Commit{SHA: sha, Parents: parents, At: w.now.Add(-ago)}
	return sha
}

// chain adds n commits in a line on top of parent, each a minute apart,
// the last one ago before now, and returns them oldest first.
func (w *world) chain(parent string, n int, ago time.Duration) []string {
	out := make([]string, n)
	for i := range n {
		at := ago + time.Duration(n-1-i)*time.Minute
		if parent == "" {
			out[i] = w.commit(at)
		} else {
			out[i] = w.commit(at, parent)
		}
		parent = out[i]
	}
	return out
}

// merge adds a two-parent merge of second into first, ago before now.
func (w *world) merge(ago time.Duration, first, second string) string {
	return w.commit(ago, first, second)
}

func (w *world) ref(name, sha string) { w.git.refs[name] = sha }

// client is a Client on the world's fakes and a mirror dir of its own.
func (w *world) client() *Client {
	dir := w.t.TempDir() + "/acme/app.git"
	return w.clientAt(dir)
}

func (w *world) clientAt(dir string) *Client {
	return New(Deps{
		Git:    w.git,
		GitHub: w.gh,
		Mirror: func(string) (string, bool) { return dir, true },
		GitEnv: func(string) map[string]string { return map[string]string{} },
		Now:    func() time.Time { return w.now },
	})
}

// refresh refreshes acme/app for the open PRs, each on master, and fails the
// test on an error.
func (w *world) refresh(c *Client, open ...int) {
	w.t.Helper()
	prs := make([]OpenPR, len(open))
	for i, n := range open {
		prs[i] = OpenPR{Number: n, Base: "master"}
	}
	w.refreshPRs(c, prs...)
}

// refreshPRs refreshes acme/app for the open PRs and fails the test on an
// error.
func (w *world) refreshPRs(c *Client, open ...OpenPR) {
	w.t.Helper()
	if err := c.Refresh(context.Background(), "acme/app", open); err != nil {
		w.t.Fatalf("Refresh: %v", err)
	}
}

var void = Env{Name: "void", Branch: "staging-void"}

// one is the PR's status on void.
func one(c *Client, pr PR) EnvStatus {
	return c.Status(pr, []Env{void})[0]
}
