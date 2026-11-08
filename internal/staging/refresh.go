package staging

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The cache lifetimes of a Refresh.
const (
	// wantedFor is how long a PR one client asked for stays in every
	// client's fetch.
	wantedFor = 10 * time.Minute
	// runKeep is how long a completed run is reused before it is read again
	// (a re-run makes it run again); a run in progress, a merge without one
	// and a failed read are read again at each Refresh.
	runKeep = 10 * time.Minute
	// pathsRetry is how long a failed read of a PR's changed paths waits
	// before the next try; a read that worked is kept while the head is.
	pathsRetry = 10 * time.Minute
	// unsettledFor is how long a deployment without a success status keeps
	// the deployments read without the ETag: its status may come after the
	// list was read, which a 304 would hide.
	unsettledFor = time.Hour
	// maxMissing is how many pull refs the remote lacks a Refresh drops
	// before it gives up on the fetch.
	maxMissing = 5
	// keepDeploys is how many deployments of an environment the state keeps.
	keepDeploys = 20
)

// Refresh fetches the mirror once (the staging-* tips, the base and
// refs/pull/N/head of the open PRs, and of those another client asked for
// in the last minutes), reads each staging environment's deployments (with
// their ETag) and the ci.yml runs of the merges that brought the open PRs'
// heads in, and builds the lookup data Status reads for the repository. It
// runs at most once a minute per repository across processes (a flock in
// the mirror dir); a call within the minute reuses the data and returns
// nil. It never blocks longer than ctx: a Refresh that ctx cuts keeps the data
// it had and returns ctx's error. The first one clones the mirror.
func (c *Client) Refresh(ctx context.Context, repo string, open []int) error {
	now := c.d.Now()
	if st := c.get(repo); st != nil && fresh(st, now) {
		return nil
	}
	dir, ok := c.d.Mirror(repo)
	if !ok {
		why := "no staging mirror is configured"
		if dir != "" {
			why = WhyVolumeNotMounted
		}
		c.set(repo, &state{V: stateVersion, At: now, Err: why})
		return errors.New("staging " + repo + ": " + why)
	}
	var env map[string]string
	if c.d.GitEnv != nil {
		env = c.d.GitEnv(repo)
	}
	if env == nil {
		why := "no git env for the fetch: the poll identity's credentials are not configured"
		c.set(repo, &state{V: stateVersion, At: now, Err: why})
		return errors.New("staging " + repo + ": " + why)
	}
	unlock, err := lockMirror(ctx, dir)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("staging %s: waiting for the mirror's lock: %w", repo, ctx.Err())
		}
		why := "the mirror's lock: " + errLine(err)
		c.set(repo, &state{V: stateVersion, At: now, Err: why})
		return errors.New("staging " + repo + ": " + why)
	}
	defer unlock()
	prev := readState(dir, c.d.Log)
	if prev != nil && fresh(prev, now) {
		prev.want(open, now)
		if err := writeState(dir, prev); err != nil {
			c.d.Log.Warn("staging: state file not written", "repo", repo, "err", errLine(err))
		}
		c.set(repo, prev)
		return nil
	}
	st := c.build(ctx, repo, dir, env, open, prev, now)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("staging %s: refresh cut: %w", repo, err)
	}
	if err := writeState(dir, st); err != nil {
		c.d.Log.Warn("staging: state file not written", "repo", repo, "err", errLine(err))
	}
	c.set(repo, st)
	if st.Err != "" {
		return errors.New("staging " + repo + ": " + st.Err)
	}
	c.d.Log.Debug("staging: refreshed", "repo", repo, "envs", len(st.Envs), "prs", len(st.PRs))
	return nil
}

// fresh reports whether st was read less than RefreshEvery before now.
func fresh(st *state, now time.Time) bool {
	age := now.Sub(st.At)
	return age >= 0 && age < RefreshEvery
}

// want records that a client asked for the open PRs at now, and drops the
// ones nobody asked for within wantedFor.
func (st *state) want(open []int, now time.Time) {
	if st.Wanted == nil {
		st.Wanted = map[int]time.Time{}
	}
	maps.DeleteFunc(st.Wanted, func(_ int, at time.Time) bool { return now.Sub(at) >= wantedFor })
	for _, n := range open {
		if n > 0 {
			st.Wanted[n] = now
		}
	}
}

// build reads the repository into a new state, starting from prev's caches.
func (c *Client) build(ctx context.Context, repo, dir string, env map[string]string, open []int, prev *state, now time.Time) *state {
	st := &state{V: stateVersion, At: now, Envs: map[string]*envData{}, PRs: map[int]*prData{},
		Runs: map[string]*runEntry{}, Paths: map[string]*pathsEntry{}, Deploys: map[string]*deployEntry{}}
	if prev != nil {
		st.Wanted, st.Runs, st.Paths, st.Deploys = prev.Wanted, nonNil(prev.Runs), nonNil(prev.Paths), nonNil(prev.Deploys)
	}
	st.want(open, now)
	base, err := c.d.Git.Ensure(ctx, dir, repo, env)
	if err != nil {
		st.Err = "the mirror could not be made: " + errLine(err)
		return st
	}
	st.Base = base
	prs, err := c.fetch(ctx, dir, env, base, slices.Sorted(maps.Keys(st.Wanted)))
	if err != nil {
		st.Err = "the fetch failed: " + errLine(err)
		return st
	}
	refs, err := c.d.Git.Refs(ctx, dir, "refs/heads/", "refs/pull/")
	if err != nil {
		st.Err = "the mirror's refs could not be read: " + errLine(err)
		return st
	}
	if st.PRs, err = c.derive(ctx, dir, base, prs, refs); err != nil {
		// One PR's history too large to list costs the PRs without
		// Commits their answer, never the environments.
		st.PRs, st.PRsErr = map[int]*prData{}, "the PRs' commits could not be read: "+errLine(err)
		c.d.Log.Warn("staging: the PRs' commits could not be read", "repo", repo, "err", errLine(err))
	}
	for _, name := range slices.Sorted(maps.Keys(refs)) {
		branch, ok := strings.CutPrefix(name, "refs/heads/")
		if !ok || !strings.HasPrefix(branch, "staging-") {
			continue
		}
		e := c.envOf(ctx, dir, base, refs[name], now)
		e.Env = strings.TrimPrefix(branch, "staging-")
		if e.Err == "" {
			st.Deploys[e.Env] = c.deploymentsOf(ctx, repo, e.Env, st.Deploys[e.Env], now)
			c.markDeployed(ctx, dir, e, st.Deploys[e.Env])
		}
		st.Envs[branch] = e
	}
	// Read the runs and paths the fetched PRs' statuses need, as Status
	// will ask for them.
	r := &reader{c: c, ctx: ctx, repo: repo, dir: dir, env: env, st: st, now: now,
		ran: map[string]*runEntry{}, diffed: map[string]*pathsEntry{}}
	for _, n := range slices.Sorted(maps.Keys(st.PRs)) {
		for _, branch := range slices.Sorted(maps.Keys(st.Envs)) {
			e := st.Envs[branch]
			st.status(PR{Repo: repo, Number: n, Head: st.PRs[n].Head, Base: base}, Env{Name: e.Env, Branch: branch}, now, r)
		}
	}
	st.prune()
	return st
}

// nonNil is m, or an empty map for nil.
func nonNil[V any](m map[string]V) map[string]V {
	if m == nil {
		return map[string]V{}
	}
	return m
}

// prune drops the caches nothing in st refers to any more: the runs of
// merges no window holds, the paths of heads no fetched PR has, the
// deployments of environments the remote no longer has.
func (st *state) prune() {
	merges, heads, envs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range st.Envs {
		envs[e.Env] = true
		for _, m := range e.Merges {
			merges[m.SHA] = true
		}
	}
	for _, p := range st.PRs {
		heads[p.Head] = true
	}
	maps.DeleteFunc(st.Runs, func(sha string, _ *runEntry) bool { return !merges[sha] })
	maps.DeleteFunc(st.Paths, func(head string, _ *pathsEntry) bool { return !heads[head] })
	maps.DeleteFunc(st.Deploys, func(env string, _ *deployEntry) bool { return !envs[env] })
}

// pullRef is the mirror's ref of PR n's head.
func pullRef(n int) string { return "refs/pull/" + strconv.Itoa(n) + "/head" }

// refspecs are a fetch's: the staging branches, the base and the PRs'
// heads, each forced into the same name in the mirror (its own refs only).
func refspecs(base string, prs []int) []string {
	out := []string{"+refs/heads/staging-*:refs/heads/staging-*", "+refs/heads/" + base + ":refs/heads/" + base}
	for _, n := range prs {
		out = append(out, "+"+pullRef(n)+":"+pullRef(n))
	}
	return out
}

// fetch fetches the mirror, dropping the PRs whose pull ref the remote
// lacks (at most maxMissing), and returns the PRs it fetched.
func (c *Client) fetch(ctx context.Context, dir string, env map[string]string, base string, prs []int) ([]int, error) {
	for range maxMissing + 1 {
		err := c.d.Git.Fetch(ctx, dir, env, refspecs(base, prs))
		var miss *MissingRefError
		if !errors.As(err, &miss) {
			return prs, err
		}
		i := slices.IndexFunc(prs, func(n int) bool { return pullRef(n) == miss.Ref })
		if i < 0 {
			return prs, err
		}
		c.d.Log.Info("staging: the remote has no pull ref; fetching without it", "dir", dir, "ref", miss.Ref)
		prs = slices.Delete(slices.Clone(prs), i, i+1)
	}
	return prs, fmt.Errorf("the remote lacks more than %d of the PRs' pull refs", maxMissing)
}

// derive reads each fetched PR's commits, base..refs/pull/N/head, oldest
// first, from one graph of all their heads.
func (c *Client) derive(ctx context.Context, dir, base string, prs []int, refs map[string]string) (map[int]*prData, error) {
	out := map[int]*prData{}
	var heads []string
	for _, n := range prs {
		if sha := refs[pullRef(n)]; sha != "" {
			out[n] = &prData{Head: sha}
			heads = append(heads, sha)
		}
	}
	if len(heads) == 0 {
		return out, nil
	}
	slices.Sort(heads)
	graph, err := c.d.Git.Graph(ctx, dir, slices.Compact(heads), []string{"refs/heads/" + base})
	if err != nil {
		return nil, err
	}
	parents := make(map[string][]string, len(graph))
	for _, cm := range graph {
		parents[cm.SHA] = cm.Parents
	}
	for _, p := range out {
		mine := map[string]bool{}
		for stack := []string{p.Head}; len(stack) > 0; {
			sha := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			ps, listed := parents[sha]
			if !listed || mine[sha] {
				continue
			}
			mine[sha] = true
			stack = append(stack, ps...)
		}
		for _, cm := range slices.Backward(graph) { // parents before children: oldest first
			if mine[cm.SHA] {
				p.Commits = append(p.Commits, cm.SHA)
			}
		}
	}
	return out, nil
}

// envOf reads a staging branch at tip: its window (the last Window
// first-parent commits, or WindowAge, whichever ends first), the window's
// merges, and which commits each brought in.
func (c *Client) envOf(ctx context.Context, dir, base, tip string, now time.Time) *envData {
	e := &envData{Tip: tip}
	chain, err := c.d.Git.FirstParent(ctx, dir, tip, Window+1)
	if err != nil {
		e.Err = "its first-parent chain could not be read: " + errLine(err)
		return e
	}
	cutoff := now.Add(-WindowAge)
	n := 0
	for n < len(chain) && n < Window && !chain[n].At.Before(cutoff) {
		n++
	}
	window := chain[:n]
	if n < len(chain) {
		e.Boundary = chain[n].SHA
	}
	if len(window) == 0 {
		return e
	}
	exclude := []string{"refs/heads/" + base}
	if e.Boundary != "" {
		exclude = append(exclude, e.Boundary)
	}
	graph, err := c.d.Git.Graph(ctx, dir, []string{tip}, exclude)
	if err != nil {
		e.Err = "its history could not be read: " + errLine(err)
		return e
	}
	parents := make(map[string][]string, len(graph))
	for _, cm := range graph {
		parents[cm.SHA] = cm.Parents
	}
	onChain := map[string]bool{}
	var seconds [][]string
	for _, f := range window {
		onChain[f.SHA] = true
		if len(f.Parents) >= 2 {
			e.Merges = append(e.Merges, mergeData{SHA: f.SHA, At: f.At})
			seconds = append(seconds, f.Parents[1:])
		}
	}
	// Oldest merge first: a commit belongs to the first merge that brings
	// it, and a later merge's walk stops at what an earlier one brought,
	// at the chain and at what the graph leaves out (the base, the commits
	// before the window).
	e.In = map[string]int{}
	for i := range slices.Backward(e.Merges) {
		stack := slices.Clone(seconds[i])
		for len(stack) > 0 {
			sha := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if onChain[sha] {
				continue
			}
			if _, done := e.In[sha]; done {
				continue
			}
			ps, listed := parents[sha]
			if !listed {
				continue
			}
			e.In[sha] = i
			stack = append(stack, ps...)
		}
	}
	return e
}

// deploymentsOf reads an environment's deployments with prev's ETag: a 304
// keeps prev's list, a 403 or 404 makes them not readable, another failure
// keeps prev's list when it had one.
func (c *Client) deploymentsOf(ctx context.Context, repo, environment string, prev *deployEntry, now time.Time) *deployEntry {
	etag := ""
	if prev != nil && prev.Readable && settled(prev.List, now) {
		etag = prev.ETag
	}
	got, err := c.d.GitHub.DeploymentsOf(ctx, repo, environment, etag)
	switch {
	case errors.Is(err, ErrNotReadable):
		return &deployEntry{Why: WhyNotReadable, ReadAt: now}
	case err != nil:
		c.d.Log.Warn("staging: deployments not read", "repo", repo, "environment", environment, "err", errLine(err))
		if prev != nil && prev.Readable {
			return prev
		}
		return &deployEntry{Why: "deployments not read: " + errLine(err), ReadAt: now}
	case got.NotModified && prev != nil:
		kept := *prev
		kept.ReadAt = now
		return &kept
	}
	list := got.List
	if i := slices.IndexFunc(list, func(d Deployment) bool { return d.Success }); i >= 0 {
		list = list[:i+1] // nothing older than the newest success counts
	}
	return &deployEntry{ETag: got.ETag, List: slices.Clone(list[:min(len(list), keepDeploys)]), Readable: true, ReadAt: now}
}

// settled reports whether the newest deployment has its success, or is old
// enough that none will come.
func settled(list []Deployment, now time.Time) bool {
	return len(list) == 0 || list[0].Success || now.Sub(list[0].CreatedAt) >= unsettledFor
}

// markDeployed records the newest successful deployment on e, and marks the
// window's merges that are its ancestors.
func (c *Client) markDeployed(ctx context.Context, dir string, e *envData, d *deployEntry) {
	if !d.Readable {
		return
	}
	i := slices.IndexFunc(d.List, func(d Deployment) bool { return d.Success })
	if i < 0 {
		return
	}
	dep := d.List[i]
	e.DeployedTip, e.DeployedAt = cmp.Or(dep.SHA, dep.Ref), dep.CreatedAt
	if len(e.Merges) == 0 {
		return
	}
	if e.DeployedTip == e.Tip {
		for k := range e.Merges {
			e.Merges[k].Deployed = true
		}
		return
	}
	var exclude []string
	if e.Boundary != "" {
		exclude = []string{e.Boundary}
	}
	graph, err := c.d.Git.Graph(ctx, dir, []string{e.DeployedTip}, exclude)
	if err != nil {
		// Not in the mirror (a deploy of a commit the branch never had):
		// it confirms none of its merges.
		c.d.Log.Info("staging: the deployed commit's history could not be read", "dir", dir, "sha", short(e.DeployedTip), "err", errLine(err))
		return
	}
	held := make(map[string]bool, len(graph))
	for _, cm := range graph {
		held[cm.SHA] = true
	}
	for k := range e.Merges {
		e.Merges[k].Deployed = held[e.Merges[k].SHA]
	}
}

// reader is the source of a Refresh: the state's caches while fresh, else
// GitHub's runs and git's changed paths, read once per Refresh.
type reader struct {
	c    *Client
	ctx  context.Context
	repo string
	dir  string
	env  map[string]string
	st   *state
	now  time.Time
	// ran and diffed are what this Refresh already read.
	ran    map[string]*runEntry
	diffed map[string]*pathsEntry
}

func (r *reader) run(sha string) *runEntry {
	if e, ok := r.ran[sha]; ok {
		return e
	}
	prev := r.st.Runs[sha]
	if prev != nil && prev.Run != nil && prev.Run.Status == "completed" && r.now.Sub(prev.ReadAt) < runKeep {
		r.ran[sha] = prev
		return prev
	}
	if r.ctx.Err() != nil {
		return prev
	}
	runs, err := r.c.d.GitHub.RunsOf(r.ctx, r.repo, Workflow, sha)
	e := &runEntry{ReadAt: r.now}
	switch {
	case err != nil:
		e.Err = errLine(err)
		if prev != nil {
			e.Run = prev.Run
		}
	case len(runs) > 0:
		newest := slices.MaxFunc(runs, func(a, b Run) int {
			return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
		})
		e.Run = &newest
	}
	r.st.Runs[sha], r.ran[sha] = e, e
	return e
}

func (r *reader) paths(head string) *pathsEntry {
	if e, ok := r.diffed[head]; ok {
		return e
	}
	prev := r.st.Paths[head]
	if prev != nil && (prev.Err == "" || r.now.Sub(prev.ReadAt) < pathsRetry) {
		r.diffed[head] = prev
		return prev
	}
	if r.ctx.Err() != nil {
		return prev
	}
	files, err := r.c.d.Git.ChangedPaths(r.ctx, r.dir, r.env, "refs/heads/"+r.st.Base, head)
	e := &pathsEntry{Paths: files, ReadAt: r.now}
	if err != nil {
		e = &pathsEntry{Err: errLine(err), ReadAt: r.now}
	}
	r.st.Paths[head], r.diffed[head] = e, e
	return e
}
