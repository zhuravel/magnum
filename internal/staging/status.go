package staging

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

// stateVersion is the version of the state file; a file of another version
// is not read (the next Refresh rebuilds it).
const stateVersion = 4

// state is what one Refresh of a repository read: the lookup data Status
// answers from, and the caches the next Refresh starts from. Refresh writes
// it beside the mirror, so another process within the minute reuses it.
// It holds shas, times, run URLs and paths, never PR text.
type state struct {
	V   int       `json:"v"`
	At  time.Time `json:"at"` // when it was read
	Err string    `json:"err,omitempty"`
	// Cloning: a clone runs (Err says since when). CloneFails counts the
	// clones that failed in a row, CloneErr says how the last one did, and
	// CloneRetryAt is when the next may start.
	Cloning      bool      `json:"cloning,omitempty"`
	CloneFails   int       `json:"clone_fails,omitempty"`
	CloneErr     string    `json:"clone_err,omitempty"`
	CloneRetryAt time.Time `json:"clone_retry_at"`
	// Base is the mirror's default branch (the remote's HEAD): the base of
	// a PR that names none, and what the branches' walk leaves out.
	Base string `json:"base,omitempty"`
	// Envs are the remote's staging-* branches, by name.
	Envs map[string]*envData `json:"envs,omitempty"`
	// PRs are the fetched PRs' heads, bases and commits,
	// base..refs/pull/N/head; BaseErr says, by base, why a base's PRs are
	// not among them (refused, not fetched, not read).
	PRs     map[int]*prData   `json:"prs,omitempty"`
	BaseErr map[string]string `json:"base_err,omitempty"`
	// PRErr says, by PR, why one the fetch dropped is not among them (the
	// remote has no pull ref).
	PRErr map[int]string `json:"pr_err,omitempty"`
	// Runs are ci.yml's runs, by merge sha; Paths the PRs' changed paths,
	// by base...head (pathsKey); Deploys the deployments, by environment.
	Runs    map[string]*runEntry    `json:"runs,omitempty"`
	Paths   map[string]*pathsEntry  `json:"paths,omitempty"`
	Deploys map[string]*deployEntry `json:"deploys,omitempty"`
	// Wanted are the PRs each client asked for, with their bases and when:
	// a Refresh fetches the ones asked for within wantedFor, whoever asked.
	Wanted map[int]wantedPR `json:"wanted,omitempty"`
}

// wantedPR is an open PR a client asked for: its base and when.
type wantedPR struct {
	Base string    `json:"base,omitempty"`
	At   time.Time `json:"at"`
}

// envData is one staging branch: its tip and the merges of its window.
type envData struct {
	Tip string `json:"tip"`
	// Env is the deployment environment read for it: the branch without
	// "staging-".
	Env string `json:"env"`
	Err string `json:"err,omitempty"`
	// Boundary is the first first-parent commit past the window ("" = the
	// chain ended inside it). Chain is the window's first-parent commits,
	// newest first: the tips the branch had.
	Boundary string    `json:"boundary,omitempty"`
	Chain    []tipData `json:"chain,omitempty"`
	// Merges are the window's merges, newest first.
	Merges []mergeData `json:"merges,omitempty"`
	// In maps each commit a window merge brought in (an ancestor of its
	// second parent that its first parent lacks, the base's commits left
	// out) to that merge, the oldest one, as its index in Merges.
	In map[string]int `json:"in,omitempty"`
	// DeployedTip is the newest successful deployment's commit, DeployedAt
	// its time ("" = none, or not readable).
	DeployedTip string    `json:"deployed_tip,omitempty"`
	DeployedAt  time.Time `json:"deployed_at"`
}

// tipData is a first-parent commit of a branch's window: a tip it had.
type tipData struct {
	SHA string    `json:"sha"`
	At  time.Time `json:"at"`
}

// mergeData is a merge of a branch's window.
type mergeData struct {
	SHA string    `json:"sha"`
	At  time.Time `json:"at"` // committer time: when it was pushed, near enough
	// Pos is its index in the window's Chain.
	Pos int `json:"pos"`
	// Deployed: an ancestor of the newest successful deployment.
	Deployed bool `json:"deployed,omitempty"`
}

// prData is a fetched PR: its head, its base, and its commits oldest first.
type prData struct {
	Head    string   `json:"head"`
	Base    string   `json:"base"`
	Commits []string `json:"commits,omitempty"`
}

// runEntry is a read of ci.yml's runs on a merge: the newest run (nil =
// none), or why it was not read.
type runEntry struct {
	Run    *Run      `json:"run,omitempty"`
	ReadAt time.Time `json:"read_at"`
	Err    string    `json:"err,omitempty"`
	// NoPaths: read for the job names alone (a previous tip's run), so its
	// failed jobs carry no paths.
	NoPaths bool `json:"no_paths,omitempty"`
}

// pathsEntry is a PR head's changed paths, or why they were not read.
type pathsEntry struct {
	Paths  []string  `json:"paths,omitempty"`
	ReadAt time.Time `json:"read_at"`
	Err    string    `json:"err,omitempty"`
}

// deployEntry is the last read of an environment's deployments.
type deployEntry struct {
	ETag     string       `json:"etag,omitempty"`
	List     []Deployment `json:"list,omitempty"`
	Readable bool         `json:"readable,omitempty"`
	Why      string       `json:"why,omitempty"` // not readable: why
	ReadAt   time.Time    `json:"read_at"`
}

// source answers the runs and the paths a status needs: the state's caches
// in Status, the caches or GitHub and git in Refresh.
type source interface {
	run(sha string) *runEntry            // nil = not read
	prevRun(sha string) *runEntry        // a previous tip's: its job names suffice; nil = not read
	paths(base, head string) *pathsEntry // nil = not read
}

// pathsKey is the key of a PR's changed paths: its base and head.
func pathsKey(base, head string) string { return base + "..." + head }

// cached is the source of Status: only what the last Refresh read.
type cached struct{ st *state }

func (c cached) run(sha string) *runEntry     { return c.st.Runs[sha] }
func (c cached) prevRun(sha string) *runEntry { return c.st.Runs[sha] }
func (c cached) paths(base, head string) *pathsEntry {
	return c.st.Paths[pathsKey(base, head)]
}

// Status returns where pr stands on each of envs, in their order, from the
// last Refresh of pr.Repo: no git, no network. Without a Refresh (or with
// one that failed), each env is unknown.
func (c *Client) Status(pr PR, envs []Env) []EnvStatus {
	st, now := c.get(pr.Repo), c.d.Now()
	out := make([]EnvStatus, len(envs))
	for i, env := range envs {
		if st == nil {
			out[i] = EnvStatus{Env: env, State: StateUnknown, Why: "no refresh yet"}
			continue
		}
		out[i] = st.status(pr, env, now, cached{st})
	}
	return out
}

// status is where pr stands on env, the runs and paths from src.
func (st *state) status(pr PR, env Env, now time.Time, src source) EnvStatus {
	s := EnvStatus{Env: env, State: StateUnknown}
	switch age := now.Sub(st.At); {
	case st.Err != "":
		s.Why = st.Err
		return s
	case age > StaleAfter:
		s.Why = fmt.Sprintf("stale: the last good read is %s old", age.Round(time.Minute))
		return s
	}
	e := st.Envs[env.Branch]
	switch {
	case e == nil:
		s.Why = "the remote has no branch " + env.Branch
		return s
	case e.Err != "":
		s.Why = e.Err
		return s
	}
	s.Tip = e.Tip
	dep := st.deploysOf(env, e)
	if dep.Readable {
		s.DeployedTip, s.DeployedAt = e.DeployedTip, e.DeployedAt
	}
	head, base, commits, why := st.commitsOf(pr)
	if why != "" {
		s.Why = why
		return s
	}
	// The newest PR commit a window merge brought in. The list is
	// topological, so what follows it is never its ancestor.
	at, mi := -1, 0
	for k, c := range slices.Backward(commits) {
		if i, ok := e.In[c]; ok {
			at, mi = k, i
			break
		}
	}
	if at < 0 {
		s.State = StateNone
		return s
	}
	m := e.Merges[mi]
	s.InTip, s.PushedAt = commits[at], m.At
	if commits[at] != head {
		s.State = StateOutdated
		for _, c := range commits[at+1:] {
			if _, in := e.In[c]; !in {
				s.Behind++
			}
		}
		return s
	}
	if dep.Readable && m.Deployed {
		s.State = StateLatest
		return s
	}
	s.State = StatePushed
	st.pushed(&s, base, e, mi, dep, now, src)
	return s
}

// deploysOf is the deployments read for env's branch, when they are env's:
// a Refresh reads the environment the branch names (staging-<name>), so an
// env named otherwise has none read.
func (st *state) deploysOf(env Env, e *envData) *deployEntry {
	d := st.Deploys[e.Env]
	switch {
	case env.Name != e.Env:
		return &deployEntry{Why: fmt.Sprintf("deployments of %s not read: a Refresh reads %s for %s", env.Name, e.Env, env.Branch)}
	case d == nil:
		return &deployEntry{Why: "deployments of " + env.Name + " not read"}
	}
	return d
}

// commitsOf is pr's head, its base ("" = the mirror's default branch) and
// its commits, oldest first and ending at the head: pr.Commits, else the
// ones the Refresh derived on its base; why says why there are none.
func (st *state) commitsOf(pr PR) (head, base string, commits []string, why string) {
	head, base = pr.Head, cmp.Or(pr.Base, st.Base)
	if pr.Commits != nil {
		if head == "" && len(pr.Commits) > 0 {
			head = pr.Commits[len(pr.Commits)-1]
		}
		return head, base, upTo(pr.Commits, head), ""
	}
	p := st.PRs[pr.Number]
	switch {
	case p == nil && st.BaseErr[base] != "":
		return "", "", nil, st.BaseErr[base]
	case p == nil && st.PRErr[pr.Number] != "":
		return "", "", nil, st.PRErr[pr.Number]
	case p == nil:
		return "", "", nil, fmt.Sprintf("PR #%d is not fetched yet: the next Refresh that has it open fetches it", pr.Number)
	case p.Base != base:
		return "", "", nil, fmt.Sprintf("PR #%d's commits were read on base %q, not %q: the next Refresh with its base reads them", pr.Number, p.Base, base)
	}
	if head == "" {
		head = p.Head
	}
	if head != p.Head && !slices.Contains(p.Commits, head) {
		return "", "", nil, fmt.Sprintf("PR #%d's head %s is not fetched yet: the next Refresh fetches it", pr.Number, short(head))
	}
	return head, base, upTo(p.Commits, head), ""
}

// upTo is commits up to and with head, or commits and head when head is
// not among them.
func upTo(commits []string, head string) []string {
	if i := slices.Index(commits, head); i >= 0 {
		return commits[:i+1]
	}
	return append(slices.Clone(commits), head)
}

// pushed fills the detail of a PR whose head the merge e.Merges[mi] brought
// in and no deployment confirms: from ci.yml's run on it.
func (st *state) pushed(s *EnvStatus, base string, e *envData, mi int, dep *deployEntry, now time.Time, src source) {
	r, ri := st.ciRun(e, mi, src)
	if r != nil && r.Run != nil {
		s.RunURL = r.Run.URL
	}
	waited := now.Sub(s.PushedAt) >= ConfirmWait
	switch {
	case r == nil:
		s.Detail = DetailNotConfirmed
		s.Why = "ci.yml's run on the merge was not read: the Refresh did not have this PR open"
	case r.Run == nil && r.Err != "":
		s.Detail = DetailNotConfirmed
		s.Why = "ci.yml's run on the merge was not read: " + r.Err
	case r.Run == nil && waited:
		s.Detail = DetailNotConfirmed
		s.Why = "no ci.yml run on the merge"
	case r.Run == nil:
		s.Detail = DetailCIRunning
		s.Why = "ci.yml has not started on the merge yet"
	case r.Run.Status != "completed":
		s.Detail = DetailCIRunning
	case passed(r.Run) && !dep.Readable:
		s.Detail = DetailNotConfirmed
		s.Why = dep.Why
	case passed(r.Run) && waited:
		s.Detail = DetailNotConfirmed
	case passed(r.Run):
		s.Detail = DetailWaitingJenkins
	case r.Run.Conclusion == "cancelled":
		s.Detail = DetailCIFailedNotOurs
		s.Why = "ci.yml's run on the merge was cancelled"
	default:
		s.Detail = DetailCIFailedNotOurs
		prev, prevSHA, prevPos := st.previousRun(e, ri, src)
		merges := 0 // the run's: those from its merge down to the tip before
		for _, m := range e.Merges {
			if m.Pos >= e.Merges[ri].Pos && m.Pos < prevPos {
				merges++
			}
		}
		var ours bool
		ours, s.Why = failureIsOurs(r.Run, src.paths(base, s.InTip), prev, prevSHA, merges)
		if ours {
			s.Detail = DetailCIFailedOurs
		}
	}
}

// ciRun is the run that says the CI of the merge e.Merges[mi]: its own,
// unless it has none or it was cancelled while a newer merge followed (a
// push of several merges runs ci.yml on the last, and a newer push cancels
// the run before it): then the next newer merge's, which holds it.
func (st *state) ciRun(e *envData, mi int, src source) (*runEntry, int) {
	for i := mi; ; i-- {
		r := src.run(e.Merges[i].SHA)
		if i == 0 || r == nil || r.Run == nil && r.Err != "" || r.Run != nil && r.Run.Conclusion != "cancelled" {
			return r, i
		}
	}
}

// prevWalk is how many tips before a merge previousRun looks at for one with
// a completed ci.yml run.
const prevWalk = 5

// previousRun is the completed ci.yml run of the branch's tip before the
// merge e.Merges[mi] (its first parent), or of the nearest older tip in the
// window that has one, at most prevWalk back, with its sha and its index in
// the chain: the run a new failure is told against. A cancelled run, or a
// failed one whose jobs were not read, tells nothing and is passed over;
// nil when none is found.
func (st *state) previousRun(e *envData, mi int, src source) (*Run, string, int) {
	pos := e.Merges[mi].Pos
	for i := pos + 1; i < min(pos+1+prevWalk, len(e.Chain)); i++ {
		r := src.prevRun(e.Chain[i].SHA)
		if r == nil || r.Run == nil || r.Run.Status != "completed" || r.Run.Conclusion == "cancelled" {
			continue
		}
		if passed(r.Run) || r.Run.Failed != nil {
			return r.Run, e.Chain[i].SHA, i
		}
	}
	return nil, "", 0
}

// passed reports whether a completed run passed: success, or skipped or
// neutral (a run that ran no check holds nothing back).
func passed(r *Run) bool {
	switch r.Conclusion {
	case "success", "skipped", "neutral":
		return true
	}
	return false
}

// failureIsOurs reports whether run's failure is the PR's: a failing job
// names a path the PR changes (p), or a job failed here that did not on
// prev, the run of the tip before the merge (prevSHA; nil = none to tell
// against), and the run covers that merge alone (merges, from the run's
// merge down to prevSHA): a flaky job reads the same, so Why says "new
// against". Over several merges a new failure may be any of theirs, and
// the same jobs failing on prev is not the PR's. Why says when it cannot be
// told: the run's jobs not read, no failing job listed, the PR's paths not
// read, or a failing job that names no path.
func failureIsOurs(run *Run, p *pathsEntry, prev *Run, prevSHA string, merges int) (bool, string) {
	const cannot = "cannot tell whether the failure is the PR's: "
	switch {
	case run.Failed == nil:
		return false, cannot + "the run's jobs were not read"
	case len(run.Failed) == 0:
		return false, cannot + "the run lists no failing job"
	}
	known := p != nil && p.Err == ""
	changed := map[string]bool{}
	if known {
		for _, path := range p.Paths {
			changed[cleanPath(path)] = true
		}
	}
	blind := false
	for _, job := range run.Failed {
		if len(job.Paths) == 0 {
			blind = true
			continue
		}
		for _, path := range job.Paths {
			if changed[cleanPath(path)] {
				return true, ""
			}
		}
	}
	if prev != nil {
		before := map[string]bool{}
		for _, job := range prev.Failed {
			before[job.Name] = true
		}
		for _, job := range run.Failed {
			if before[job.Name] {
				continue
			}
			if merges > 1 {
				return false, fmt.Sprintf("new against %s, in a run of %d merges: cannot tell whose", short(prevSHA), merges)
			}
			return true, "new against " + short(prevSHA) + ": a job failed that passed there (a flaky job reads the same)"
		}
		return false, "the same jobs failed on " + short(prevSHA) + ", the tip before"
	}
	switch {
	case !known:
		return false, cannot + "the PR's changed paths were not read"
	case blind:
		return false, cannot + "a failing job names no path"
	}
	return false, ""
}

// cleanPath is a repository path without a leading ./ or /.
func cleanPath(p string) string {
	return strings.TrimLeft(strings.TrimPrefix(strings.TrimSpace(p), "./"), "/")
}
