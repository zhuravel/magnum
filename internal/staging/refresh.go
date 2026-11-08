package staging

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
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

// Refresh fetches the mirror once (the staging-* tips, each distinct base of
// the open PRs and their refs/pull/N/head, and those another client asked
// for in the last minutes), reads each staging environment's deployments
// (with their ETag) and the ci.yml runs of the merges that brought the open
// PRs' heads in, and builds the lookup data Status reads for the
// repository, each PR's commits derived as its Base..refs/pull/N/head. A
// base that is no plain branch name (git check-ref-format --branch, no
// leading '-') is refused and never reaches git; its PRs, and those of a
// base the fetch cannot read, are unknown, Why naming the base. It runs at
// most once a minute per repository across processes (a flock in the
// mirror dir); a call within the minute reuses the data and returns its
// error again. It never blocks longer than ctx: a Refresh that ctx cuts
// keeps the data it had and returns ctx's error.
//
// The first Refresh of a repository, or one that finds its mirror dir no
// bare repository of its own (it is moved aside, never repaired in place),
// starts the clone in the background on a context of its own (at most
// cloneTimeout) under the flock, and returns: until it is done every
// status is unknown, "cloning the mirror". A clone that fails is tried
// again after 1, 5, 15 and then 60 minutes.
func (c *Client) Refresh(ctx context.Context, repo string, open []OpenPR) error {
	now := c.d.Now()
	if st := c.get(repo); st != nil && fresh(st, now) {
		return st.err(repo)
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
	if since, busy := c.clonesInto(dir); busy {
		c.set(repo, cloningState(now, since))
		return nil
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
			// Another process holds the mirror (its clone, a slow fetch):
			// its last word stands here too.
			if st := readState(dir, c.d.Log); st != nil {
				if cur := c.get(repo); cur == nil || st.At.After(cur.At) {
					c.set(repo, st)
				}
			}
			return fmt.Errorf("staging %s: waiting for the mirror's lock: %w", repo, ctx.Err())
		}
		why := "the mirror's lock: " + errLine(err)
		c.set(repo, &state{V: stateVersion, At: now, Err: why})
		return errors.New("staging " + repo + ": " + why)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	clearStaleLocks(dir, now, c.d.Log)
	prev := readState(dir, c.d.Log)
	if prev != nil && fresh(prev, now) {
		prev.want(open, now)
		if err := writeState(dir, prev); err != nil {
			c.d.Log.Warn("staging: state file not written", "repo", repo, "err", errLine(err))
		}
		c.set(repo, prev)
		return prev.err(repo)
	}
	base, err := c.d.Git.Base(ctx, dir)
	switch {
	case errors.Is(err, ErrNoMirror):
		st, ok := c.prepareClone(repo, dir, open, prev, err, now)
		if ok {
			locked = false
			c.startClone(ctx, repo, dir, env, unlock, st)
		}
		return st.err(repo)
	case err != nil:
		st := &state{V: stateVersion, At: now, Err: "the mirror could not be read: " + errLine(err)}
		c.set(repo, st)
		return st.err(repo)
	}
	st := c.build(ctx, repo, dir, env, base, open, prev, now)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("staging %s: refresh cut: %w", repo, err)
	}
	if err := writeState(dir, st); err != nil {
		c.d.Log.Warn("staging: state file not written", "repo", repo, "err", errLine(err))
	}
	c.set(repo, st)
	if st.Err == "" {
		c.d.Log.Debug("staging: refreshed", "repo", repo, "envs", len(st.Envs), "prs", len(st.PRs))
	}
	return st.err(repo)
}

// err is st's error for Refresh to return: nil when it holds data or a
// clone runs.
func (st *state) err(repo string) error {
	if st.Err == "" || st.Cloning {
		return nil
	}
	return errors.New("staging " + repo + ": " + st.Err)
}

// cloneBackoff is the wait before the next try of a clone that failed n
// times.
func cloneBackoff(n int) time.Duration {
	steps := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
	return steps[min(max(n, 1), len(steps))-1]
}

// cloningState is the state while a clone that started at since runs.
func cloningState(now, since time.Time) *state {
	return &state{V: stateVersion, At: now, Cloning: true, Err: "cloning the mirror (since " + since.Format("15:04") + ")"}
}

// clonesInto reports whether this client clones into dir, and since when.
func (c *Client) clonesInto(dir string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	since, ok := c.cloning[dir]
	return since, ok
}

// prepareClone answers a mirror dir that is no mirror (why), under the
// flock: within the backoff of a failed clone, the failure stands; else a
// dir that exists is moved aside (<dir>.broken-<time>, never repaired in
// place) and ok says to start the clone, whose state it returns.
func (c *Client) prepareClone(repo, dir string, open []OpenPR, prev *state, why error, now time.Time) (*state, bool) {
	st := cloningState(now, now)
	if prev != nil {
		st.Wanted, st.CloneFails = prev.Wanted, prev.CloneFails
		if prev.CloneRetryAt.After(now) {
			waiting := &state{V: stateVersion, At: now, Wanted: prev.Wanted, CloneFails: prev.CloneFails,
				CloneRetryAt: prev.CloneRetryAt, CloneErr: prev.CloneErr,
				Err: prev.CloneErr + "; the next try is at " + prev.CloneRetryAt.Format("15:04")}
			waiting.want(open, now)
			c.set(repo, waiting)
			return waiting, false
		}
	}
	st.want(open, now)
	if _, err := os.Lstat(dir); err == nil {
		aside := dir + ".broken-" + now.Format("20060102T150405")
		if err := os.Rename(dir, aside); err != nil {
			bad := &state{V: stateVersion, At: now, Err: "the mirror dir is no mirror and could not be moved aside: " + errLine(err)}
			c.set(repo, bad)
			return bad, false
		}
		c.d.Log.Warn("staging: the mirror dir is no mirror; moved aside, cloning again", "dir", dir, "aside", aside, "why", errLine(why))
	}
	if err := writeState(dir, st); err != nil {
		c.d.Log.Warn("staging: state file not written", "repo", repo, "err", errLine(err))
	}
	c.set(repo, st)
	return st, true
}

// startClone clones the mirror in the background, on a context of its own
// (ctx's values, never its end, and cloneTimeout), holding the flock until
// it is done (unlock). Done, the state says so and is never fresh, so the
// next Refresh reads the mirror; failed, it says when the clone is tried
// again.
func (c *Client) startClone(ctx context.Context, repo, dir string, env map[string]string, unlock func(), st *state) {
	c.mu.Lock()
	c.cloning[dir] = st.At
	c.mu.Unlock()
	c.bg.Go(func() {
		defer unlock()
		defer func() {
			c.mu.Lock()
			delete(c.cloning, dir)
			c.mu.Unlock()
		}()
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloneTimeout)
		defer cancel()
		err := c.d.Git.Clone(cctx, dir, repo, env)
		now := c.d.Now()
		next := &state{V: stateVersion, Wanted: st.Wanted, Err: "the mirror is cloned: the next Refresh reads it"}
		if err != nil {
			next.At, next.CloneFails = now, st.CloneFails+1
			next.CloneRetryAt = now.Add(cloneBackoff(next.CloneFails))
			next.CloneErr = "the mirror could not be cloned: " + errLine(err)
			next.Err = next.CloneErr + "; the next try is at " + next.CloneRetryAt.Format("15:04")
			c.d.Log.Warn("staging: the clone failed", "repo", repo, "dir", dir, "fails", next.CloneFails, "err", errLine(err))
		} else {
			c.d.Log.Info("staging: the mirror is cloned", "repo", repo, "dir", dir)
		}
		if err := writeState(dir, next); err != nil {
			c.d.Log.Warn("staging: state file not written", "repo", repo, "err", errLine(err))
		}
		c.set(repo, next)
	})
}

// fresh reports whether st was read less than RefreshEvery before now.
func fresh(st *state, now time.Time) bool {
	age := now.Sub(st.At)
	return age >= 0 && age < RefreshEvery
}

// want records that a client asked for the open PRs at now, each on its
// base ("" = the mirror's default branch, which build fills in), and drops
// the ones nobody asked for within wantedFor.
func (st *state) want(open []OpenPR, now time.Time) {
	if st.Wanted == nil {
		st.Wanted = map[int]wantedPR{}
	}
	maps.DeleteFunc(st.Wanted, func(_ int, w wantedPR) bool { return now.Sub(w.At) >= wantedFor })
	for _, pr := range open {
		if pr.Number > 0 {
			st.Wanted[pr.Number] = wantedPR{Base: pr.Base, At: now}
		}
	}
}

// build reads the repository into a new state, starting from prev's caches.
func (c *Client) build(ctx context.Context, repo, dir string, env map[string]string, base string, open []OpenPR, prev *state, now time.Time) *state {
	st := &state{V: stateVersion, At: now, Envs: map[string]*envData{}, PRs: map[int]*prData{}, BaseErr: map[string]string{}, PRErr: map[int]string{},
		Runs: map[string]*runEntry{}, Paths: map[string]*pathsEntry{}, Deploys: map[string]*deployEntry{}}
	if prev != nil {
		st.Wanted, st.Runs, st.Paths, st.Deploys = prev.Wanted, nonNil(prev.Runs), nonNil(prev.Paths), nonNil(prev.Deploys)
	}
	st.want(open, now)
	st.Base = base
	prs, err := c.fetch(ctx, dir, env, st, st.basesOf(base))
	if err != nil {
		st.Err = "the fetch failed: " + errLine(err)
		return st
	}
	refs, err := c.d.Git.Refs(ctx, dir, "refs/heads/", "refs/pull/")
	if err != nil {
		st.Err = "the mirror's refs could not be read: " + errLine(err)
		return st
	}
	c.derive(ctx, dir, st, prs, refs)
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
			st.status(PR{Repo: repo, Number: n, Head: st.PRs[n].Head, Base: st.PRs[n].Base}, Env{Name: e.Env, Branch: branch}, now, r)
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
// tips no window holds, the paths of ranges no fetched PR has, the
// deployments of environments the remote no longer has.
func (st *state) prune() {
	merges, ranges, envs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range st.Envs {
		envs[e.Env] = true
		for _, sha := range e.Chain {
			merges[sha] = true // a merge's, or a tip's that a new failure is told against
		}
	}
	for _, p := range st.PRs {
		ranges[pathsKey(p.Base, p.Head)] = true
	}
	maps.DeleteFunc(st.Runs, func(sha string, _ *runEntry) bool { return !merges[sha] })
	maps.DeleteFunc(st.Paths, func(key string, _ *pathsEntry) bool { return !ranges[key] })
	maps.DeleteFunc(st.Deploys, func(env string, _ *deployEntry) bool { return !envs[env] })
}

// pullRef is the mirror's ref of PR n's head.
func pullRef(n int) string { return "refs/pull/" + strconv.Itoa(n) + "/head" }

// pullNumber is the PR number of a pull ref.
func pullNumber(ref string) (int, bool) {
	s, ok := strings.CutPrefix(ref, "refs/pull/")
	if !ok {
		return 0, false
	}
	s, ok = strings.CutSuffix(s, "/head")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n > 0
}

// basesOf is the wanted PRs by number with their bases, "" read as def (the
// mirror's default branch); a base that is no plain branch name is refused
// (BaseErr) and its PRs left out, so it never reaches git.
func (st *state) basesOf(def string) map[int]string {
	prs := map[int]string{}
	for n, w := range st.Wanted {
		base := cmp.Or(w.Base, def)
		if !validBranch(base) {
			st.BaseErr[base] = fmt.Sprintf("the base %q is not a branch name: refused", base)
			continue
		}
		prs[n] = base
	}
	return prs
}

// refspecs are a fetch's: the staging branches, the default branch and each
// distinct base, and the PRs' heads, each forced into the same name in the
// mirror (its own refs only).
func refspecs(def string, prs map[int]string) []string {
	out := []string{"+refs/heads/staging-*:refs/heads/staging-*"}
	for _, b := range slices.Sorted(maps.Keys(basesIn(def, prs))) {
		out = append(out, "+refs/heads/"+b+":refs/heads/"+b)
	}
	for _, n := range slices.Sorted(maps.Keys(prs)) {
		out = append(out, "+"+pullRef(n)+":"+pullRef(n))
	}
	return out
}

// basesIn is the set of the default branch and the PRs' bases.
func basesIn(def string, prs map[int]string) map[string]bool {
	bases := map[string]bool{def: true}
	for _, b := range prs {
		bases[b] = true
	}
	return bases
}

// fetch fetches the mirror for prs (number to base). A pull ref the remote
// lacks drops its PR, a base it lacks drops the base (BaseErr) and its PRs,
// and the fetch runs again, at most maxMissing times; the default branch
// missing fails it. It returns the PRs it fetched.
func (c *Client) fetch(ctx context.Context, dir string, env map[string]string, st *state, prs map[int]string) (map[int]string, error) {
	for range maxMissing + 1 {
		err := c.d.Git.Fetch(ctx, dir, env, refspecs(st.Base, prs))
		var miss *MissingRefError
		if !errors.As(err, &miss) {
			return prs, err
		}
		prs = maps.Clone(prs)
		dropped := false
		for n, b := range prs {
			if pullRef(n) == miss.Ref || b != st.Base && "refs/heads/"+b == miss.Ref {
				delete(prs, n)
				dropped = true
			}
		}
		if !dropped {
			return prs, err
		}
		if b, ok := strings.CutPrefix(miss.Ref, "refs/heads/"); ok {
			st.BaseErr[b] = fmt.Sprintf("the base %q could not be fetched: the remote has no branch %s", b, b)
		} else if n, ok := pullNumber(miss.Ref); ok {
			st.PRErr[n] = fmt.Sprintf("PR #%d has no pull ref on the remote (%s): it is not fetched", n, miss.Ref)
		}
		c.d.Log.Info("staging: the remote lacks a ref; fetching without it", "dir", dir, "ref", miss.Ref)
	}
	return prs, fmt.Errorf("the remote lacks more than %d of the PRs' refs", maxMissing)
}

// derive reads each fetched PR's commits, base..refs/pull/N/head, oldest
// first, into st.PRs: one graph per distinct base of all its PRs' heads. A
// base whose graph cannot be read (one PR's history too large) leaves its
// PRs out, BaseErr naming it.
func (c *Client) derive(ctx context.Context, dir string, st *state, prs map[int]string, refs map[string]string) {
	byBase := map[string][]int{}
	for n, b := range prs {
		if refs[pullRef(n)] != "" {
			byBase[b] = append(byBase[b], n)
		}
	}
	for _, base := range slices.Sorted(maps.Keys(byBase)) {
		var heads []string
		for _, n := range byBase[base] {
			heads = append(heads, refs[pullRef(n)])
		}
		slices.Sort(heads)
		graph, err := c.d.Git.Graph(ctx, dir, slices.Compact(heads), []string{"refs/heads/" + base})
		if err != nil {
			st.BaseErr[base] = fmt.Sprintf("the commits of the PRs on base %q could not be read: %s", base, errLine(err))
			c.d.Log.Warn("staging: the PRs' commits could not be read", "dir", dir, "base", base, "err", errLine(err))
			continue
		}
		parents := make(map[string][]string, len(graph))
		for _, cm := range graph {
			parents[cm.SHA] = cm.Parents
		}
		for _, n := range byBase[base] {
			p := &prData{Head: refs[pullRef(n)], Base: base}
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
			st.PRs[n] = p
		}
	}
}

// validBranch reports whether name is a plain branch name as git
// check-ref-format --branch takes one, never an option: no leading '-', not
// HEAD or @, no control character, space or any of ~^:?*[\, no "..", "@{"
// or "//", no component that starts with '.' or ends with ".lock", and no
// '/' or '.' at either end.
func validBranch(name string) bool {
	switch {
	case name == "" || name == "@" || name == "HEAD" || strings.HasPrefix(name, "-"):
		return false
	case strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) }):
		return false
	case strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//"):
		return false
	case strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, "."):
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
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
	for pos, f := range window {
		onChain[f.SHA] = true
		e.Chain = append(e.Chain, f.SHA)
		if len(f.Parents) >= 2 {
			e.Merges = append(e.Merges, mergeData{SHA: f.SHA, At: f.At, Pos: pos})
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
	tip := cmp.Or(dep.SHA, dep.Ref)
	if !isOID(tip) {
		// GitHub's answer is data: only a sha may reach git.
		c.d.Log.Info("staging: the newest successful deployment names no sha; it confirms nothing", "dir", dir)
		return
	}
	e.DeployedTip, e.DeployedAt = tip, dep.CreatedAt
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

func (r *reader) paths(base, head string) *pathsEntry {
	key := pathsKey(base, head)
	if e, ok := r.diffed[key]; ok {
		return e
	}
	prev := r.st.Paths[key]
	if prev != nil && (prev.Err == "" || r.now.Sub(prev.ReadAt) < pathsRetry) {
		r.diffed[key] = prev
		return prev
	}
	if r.ctx.Err() != nil {
		return prev
	}
	if !validBranch(base) {
		return nil
	}
	files, err := r.c.d.Git.ChangedPaths(r.ctx, r.dir, r.env, "refs/heads/"+base, head)
	e := &pathsEntry{Paths: files, ReadAt: r.now}
	if err != nil {
		e = &pathsEntry{Err: errLine(err), ReadAt: r.now}
	}
	r.st.Paths[key], r.diffed[key] = e, e
	return e
}
