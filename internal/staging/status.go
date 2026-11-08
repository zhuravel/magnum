package staging

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// stateVersion is the version of the state file; a file of another version
// is not read (the next Refresh rebuilds it).
const stateVersion = 1

// state is what one Refresh of a repository read: the lookup data Status
// answers from, and the caches the next Refresh starts from. Refresh writes
// it beside the mirror, so another process within the minute reuses it.
// It holds shas, times, run URLs and paths, never PR text.
type state struct {
	V   int       `json:"v"`
	At  time.Time `json:"at"` // when it was read
	Err string    `json:"err,omitempty"`
	// Base is the mirror's base branch: the remote's default.
	Base string `json:"base,omitempty"`
	// Envs are the remote's staging-* branches, by name.
	Envs map[string]*envData `json:"envs,omitempty"`
	// PRs are the fetched PRs' heads and commits, Base..refs/pull/N/head;
	// PRsErr says why there are none when they could not be read.
	PRs    map[int]*prData `json:"prs,omitempty"`
	PRsErr string          `json:"prs_err,omitempty"`
	// Runs are ci.yml's runs, by merge sha; Paths the PRs' changed paths,
	// by head; Deploys the deployments, by environment.
	Runs    map[string]*runEntry    `json:"runs,omitempty"`
	Paths   map[string]*pathsEntry  `json:"paths,omitempty"`
	Deploys map[string]*deployEntry `json:"deploys,omitempty"`
	// Wanted are the PRs each client asked for, with when: a Refresh
	// fetches the ones asked for within wantedFor, whoever asked.
	Wanted map[int]time.Time `json:"wanted,omitempty"`
}

// envData is one staging branch: its tip and the merges of its window.
type envData struct {
	Tip string `json:"tip"`
	// Env is the deployment environment read for it: the branch without
	// "staging-".
	Env string `json:"env"`
	Err string `json:"err,omitempty"`
	// Boundary is the first first-parent commit past the window ("" = the
	// chain ended inside it).
	Boundary string `json:"boundary,omitempty"`
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

// mergeData is a merge of a branch's window.
type mergeData struct {
	SHA string    `json:"sha"`
	At  time.Time `json:"at"` // committer time: when it was pushed, near enough
	// Deployed: an ancestor of the newest successful deployment.
	Deployed bool `json:"deployed,omitempty"`
}

// prData is a fetched PR: its head, and its commits oldest first.
type prData struct {
	Head    string   `json:"head"`
	Commits []string `json:"commits,omitempty"`
}

// runEntry is a read of ci.yml's runs on a merge: the newest run (nil =
// none), or why it was not read.
type runEntry struct {
	Run    *Run      `json:"run,omitempty"`
	ReadAt time.Time `json:"read_at"`
	Err    string    `json:"err,omitempty"`
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
	run(sha string) *runEntry      // nil = not read
	paths(head string) *pathsEntry // nil = not read
}

// cached is the source of Status: only what the last Refresh read.
type cached struct{ st *state }

func (c cached) run(sha string) *runEntry      { return c.st.Runs[sha] }
func (c cached) paths(head string) *pathsEntry { return c.st.Paths[head] }

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
	if st.Err != "" {
		s.Why = st.Err
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
	head, commits, why := st.commitsOf(pr)
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
	st.pushed(&s, e, mi, dep, now, src)
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

// commitsOf is pr's head and its commits, oldest first and ending at the
// head: pr.Commits, else the ones the Refresh derived; why says why there
// are none.
func (st *state) commitsOf(pr PR) (head string, commits []string, why string) {
	head = pr.Head
	if pr.Commits != nil {
		if head == "" && len(pr.Commits) > 0 {
			head = pr.Commits[len(pr.Commits)-1]
		}
		return head, upTo(pr.Commits, head), ""
	}
	if pr.Base != "" && pr.Base != st.Base {
		return "", nil, fmt.Sprintf("PR #%d's base %s is not the mirror's base %s: its commits need PR.Commits", pr.Number, pr.Base, st.Base)
	}
	p := st.PRs[pr.Number]
	switch {
	case p == nil && st.PRsErr != "":
		return "", nil, st.PRsErr
	case p == nil:
		return "", nil, fmt.Sprintf("PR #%d is not fetched yet: the next Refresh that has it open fetches it", pr.Number)
	}
	if head == "" {
		head = p.Head
	}
	if head != p.Head && !slices.Contains(p.Commits, head) {
		return "", nil, fmt.Sprintf("PR #%d's head %s is not fetched yet: the next Refresh fetches it", pr.Number, short(head))
	}
	return head, upTo(p.Commits, head), ""
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
func (st *state) pushed(s *EnvStatus, e *envData, mi int, dep *deployEntry, now time.Time, src source) {
	r := st.ciRun(e, mi, src)
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
		var ours bool
		ours, s.Why = failureIsOurs(r.Run, src.paths(s.InTip))
		if ours {
			s.Detail = DetailCIFailedOurs
		}
	}
}

// ciRun is the run that says the CI of the merge e.Merges[mi]: its own,
// unless it has none or it was cancelled while a newer merge followed (a
// push of several merges runs ci.yml on the last, and a newer push cancels
// the run before it): then the next newer merge's, which holds it.
func (st *state) ciRun(e *envData, mi int, src source) *runEntry {
	for i := mi; ; i-- {
		r := src.run(e.Merges[i].SHA)
		if i == 0 || r == nil || r.Run == nil && r.Err != "" || r.Run != nil && r.Run.Conclusion != "cancelled" {
			return r
		}
	}
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

// failureIsOurs reports whether a failing job of run names a path the PR
// changes; why says when that cannot be told (no failing job listed, a job
// that names no path, or the PR's paths not read).
func failureIsOurs(run *Run, p *pathsEntry) (bool, string) {
	const cannot = "cannot tell whether the failure is the PR's: "
	if len(run.Failed) == 0 {
		return false, cannot + "the run lists no failing job"
	}
	if p == nil || p.Err != "" {
		return false, cannot + "the PR's changed paths were not read"
	}
	changed := map[string]bool{}
	for _, path := range p.Paths {
		changed[cleanPath(path)] = true
	}
	blind := false
	for _, job := range run.Failed {
		if job.Paths == nil {
			blind = true
			continue
		}
		for _, path := range job.Paths {
			if changed[cleanPath(path)] {
				return true, ""
			}
		}
	}
	if blind {
		return false, cannot + "a failing job names no path"
	}
	return false, ""
}

// cleanPath is a repository path without a leading ./ or /.
func cleanPath(p string) string {
	return strings.TrimLeft(strings.TrimPrefix(strings.TrimSpace(p), "./"), "/")
}
