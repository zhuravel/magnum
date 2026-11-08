package staging

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// scene is acme/app with master (m0, m1), PR 7 off m1 (p1, then the head
// p2) and staging-void, whose base s0 holds another PR's merge.
type scene struct {
	*world
	m1, p1, p2, s0 string
}

func newScene(t *testing.T) *scene {
	t.Helper()
	w := newWorld(t)
	m := w.chain("", 2, 30*24*time.Hour)
	other := w.commit(6*24*time.Hour, m[1])
	s := &scene{world: w, m1: m[1]}
	s.s0 = w.merge(5*24*time.Hour, m[1], other)
	p := w.chain(m[1], 2, 3*time.Hour)
	s.p1, s.p2 = p[0], p[1]
	w.ref("refs/heads/master", m[1])
	w.ref("refs/pull/7/head", s.p2)
	w.ref("refs/heads/staging-void", s.s0)
	return s
}

// push merges commit into staging-void ago before now, and returns the merge.
func (s *scene) push(ago time.Duration, commit string) string {
	tip := s.git.refs["refs/heads/staging-void"]
	m := s.merge(ago, tip, commit)
	s.ref("refs/heads/staging-void", m)
	return m
}

// deployed records a successful deployment of void at sha.
func (s *scene) deployed(sha string) {
	s.gh.deploys["void"] = append([]Deployment{{ID: int64(len(s.gh.deploys["void"]) + 1), SHA: sha, Ref: sha,
		CreatedAt: s.now.Add(-time.Minute), Success: true}}, s.gh.deploys["void"]...)
}

func (s *scene) pr() PR { return PR{Repo: "acme/app", Number: 7, Head: s.p2, Base: "master"} }

func run(status, conclusion string, failed ...Job) Run {
	return Run{ID: 99, URL: "https://github.com/acme/app/actions/runs/99", Status: status, Conclusion: conclusion, Failed: failed}
}

// The state rules, one row each: where a PR stands on void after a Refresh
// over the fakes.
func TestTheStateRules(t *testing.T) {
	t.Parallel()
	const runURL = "https://github.com/acme/app/actions/runs/99"
	for _, tc := range []struct {
		name string
		// set lays out staging, the deployments and the run; it returns the
		// merge that brought the PR in, if any.
		set        func(s *scene) string
		state      State
		detail     Detail
		why        string // a fragment; "" = Why must be empty
		inTip      string // "p1", "p2" or ""
		behind     int
		runURL     bool
		deployedAt bool
	}{
		{name: "latest: the newest successful deployment is the merge's tip", set: func(s *scene) string {
			m := s.push(20*time.Minute, s.p2)
			s.deployed(m)
			return m
		}, state: StateLatest, inTip: "p2", deployedAt: true},
		{name: "latest: a later tip with the merge in its history is deployed", set: func(s *scene) string {
			m := s.push(20*time.Minute, s.p2)
			other := s.commit(time.Hour, s.m1)
			s.deployed(s.push(10*time.Minute, other))
			return m
		}, state: StateLatest, inTip: "p2", deployedAt: true},
		{name: "pushed, ci running", set: func(s *scene) string {
			s.deployed(s.s0)
			m := s.push(5*time.Minute, s.p2)
			s.gh.runs[m] = []Run{run("in_progress", "")}
			return m
		}, state: StatePushed, detail: DetailCIRunning, inTip: "p2", runURL: true, deployedAt: true},
		{name: "pushed, ci not started yet", set: func(s *scene) string {
			return s.push(time.Minute, s.p2)
		}, state: StatePushed, detail: DetailCIRunning, why: "has not started", inTip: "p2"},
		{name: "pushed, ci failed on the PR's paths", set: func(s *scene) string {
			m := s.push(10*time.Minute, s.p2)
			s.gh.runs[m] = []Run{run("completed", "failure", Job{Name: "rspec", Paths: []string{"spec/other_spec.rb"}},
				Job{Name: "rubocop", Paths: []string{"./app/models/a.rb"}})}
			s.git.paths[s.p2] = []string{"app/models/a.rb", "spec/models/a_spec.rb"}
			return m
		}, state: StatePushed, detail: DetailCIFailedOurs, inTip: "p2", runURL: true},
		{name: "pushed, ci failed elsewhere", set: func(s *scene) string {
			m := s.push(10*time.Minute, s.p2)
			s.gh.runs[m] = []Run{run("completed", "failure", Job{Name: "rspec", Paths: []string{"spec/other_spec.rb"}})}
			s.git.paths[s.p2] = []string{"app/models/a.rb"}
			return m
		}, state: StatePushed, detail: DetailCIFailedNotOurs, inTip: "p2", runURL: true},
		{name: "pushed, ci failed and the run names no path", set: func(s *scene) string {
			m := s.push(10*time.Minute, s.p2)
			s.gh.runs[m] = []Run{run("completed", "failure", Job{Name: "jest"})}
			s.git.paths[s.p2] = []string{"app/models/a.rb"}
			return m
		}, state: StatePushed, detail: DetailCIFailedNotOurs, why: "cannot tell", inTip: "p2", runURL: true},
		{name: "pushed, ci failed and the job's annotations were not read", set: func(s *scene) string {
			m := s.push(10*time.Minute, s.p2)
			s.gh.runs[m] = []Run{run("completed", "failure", Job{Name: "rspec", Paths: []string{}})}
			s.git.paths[s.p2] = []string{"app/models/a.rb"}
			return m
		}, state: StatePushed, detail: DetailCIFailedNotOurs, why: "cannot tell", inTip: "p2", runURL: true},
		{name: "pushed, ci failed and the PR's paths are not readable", set: func(s *scene) string {
			m := s.push(10*time.Minute, s.p2)
			s.gh.runs[m] = []Run{run("completed", "failure", Job{Name: "rspec", Paths: []string{"app/models/a.rb"}})}
			return m
		}, state: StatePushed, detail: DetailCIFailedNotOurs, why: "cannot tell", inTip: "p2", runURL: true},
		{name: "pushed, ci passed, waiting for Jenkins", set: func(s *scene) string {
			s.deployed(s.s0)
			m := s.push(10*time.Minute, s.p2)
			s.gh.runs[m] = []Run{run("completed", "success")}
			return m
		}, state: StatePushed, detail: DetailWaitingJenkins, inTip: "p2", runURL: true, deployedAt: true},
		{name: "pushed, ci passed, not confirmed after 45 min", set: func(s *scene) string {
			s.deployed(s.s0)
			m := s.push(46*time.Minute, s.p2)
			s.gh.runs[m] = []Run{run("completed", "success")}
			return m
		}, state: StatePushed, detail: DetailNotConfirmed, inTip: "p2", runURL: true, deployedAt: true},
		{name: "pushed, deployments not readable", set: func(s *scene) string {
			m := s.push(10*time.Minute, s.p2)
			s.deployed(m) // there, but the token may not read it
			s.gh.deployErr["void"] = fmt.Errorf("gh api: HTTP 403: %w", ErrNotReadable)
			s.gh.runs[m] = []Run{run("completed", "success")}
			return m
		}, state: StatePushed, detail: DetailNotConfirmed, why: WhyNotReadable, inTip: "p2", runURL: true},
		{name: "pushed, the merge's run cancelled by a newer merge's: the newer run decides", set: func(s *scene) string {
			m := s.push(10*time.Minute, s.p2)
			s.gh.runs[m] = []Run{run("completed", "cancelled")}
			next := s.push(9*time.Minute, s.commit(time.Hour, s.m1))
			s.gh.runs[next] = []Run{run("in_progress", "")}
			return m
		}, state: StatePushed, detail: DetailCIRunning, inTip: "p2", runURL: true},
		{name: "outdated: an older commit of the PR is on staging", set: func(s *scene) string {
			m := s.push(2*time.Hour, s.p1)
			s.deployed(m)
			return m
		}, state: StateOutdated, inTip: "p1", behind: 1, deployedAt: true},
		{name: "none: no commit of the PR on staging", set: func(s *scene) string {
			s.deployed(s.s0)
			return ""
		}, state: StateNone, deployedAt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(t)
			merge := tc.set(s)
			c := s.client()
			s.refresh(c, 7)
			got := one(c, s.pr())
			if got.State != tc.state || got.Detail != tc.detail {
				t.Fatalf("state %s/%s (why %q), want %s/%s", got.State, got.Detail, got.Why, tc.state, tc.detail)
			}
			if tc.why == "" && got.Why != "" {
				t.Errorf("Why = %q, want none", got.Why)
			}
			if tc.why != "" && !strings.Contains(got.Why, tc.why) {
				t.Errorf("Why = %q, want %q in it", got.Why, tc.why)
			}
			if want := map[string]string{"p1": s.p1, "p2": s.p2}[tc.inTip]; got.InTip != want {
				t.Errorf("InTip = %q, want %q", got.InTip, want)
			}
			if got.Behind != tc.behind {
				t.Errorf("Behind = %d, want %d", got.Behind, tc.behind)
			}
			if want := s.git.refs["refs/heads/staging-void"]; got.Tip != want {
				t.Errorf("Tip = %q, want staging-void's %q", got.Tip, want)
			}
			if merge != "" && !got.PushedAt.Equal(s.git.commits[merge].At) {
				t.Errorf("PushedAt = %v, want the merge's committer time %v", got.PushedAt, s.git.commits[merge].At)
			}
			if (got.RunURL == runURL) != tc.runURL {
				t.Errorf("RunURL = %q, want it set: %v", got.RunURL, tc.runURL)
			}
			if tc.deployedAt != (got.DeployedTip != "" && !got.DeployedAt.IsZero()) {
				t.Errorf("DeployedTip = %q at %v, want one: %v", got.DeployedTip, got.DeployedAt, tc.deployedAt)
			}
			if got.Env != void {
				t.Errorf("Env = %+v, want %+v", got.Env, void)
			}
		})
	}
}

// Without the data of a Refresh that worked, every environment is unknown,
// and Why says why.
func TestUnknownSaysWhy(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.push(10*time.Minute, s.p2)
	c := s.client()
	if got := one(c, s.pr()); got.State != StateUnknown || !strings.Contains(got.Why, "no refresh") {
		t.Errorf("before any Refresh: %s %q, want unknown, no refresh", got.State, got.Why)
	}

	s.git.fetchErr = errors.New("fatal: unable to access 'https://github.com/acme/app.git/': Could not resolve host")
	if err := c.Refresh(context.Background(), "acme/app", []OpenPR{{Number: 7, Base: "master"}}); err == nil {
		t.Error("a failed fetch: Refresh returned no error")
	}
	if got := one(c, s.pr()); got.State != StateUnknown || !strings.Contains(got.Why, "fetch failed") || !strings.Contains(got.Why, "resolve host") {
		t.Errorf("after a failed fetch: %s %q, want unknown naming the fetch", got.State, got.Why)
	}

	s.git.fetchErr = nil
	s.now = s.now.Add(RefreshEvery)
	s.refresh(c, 7)
	if got := c.Status(s.pr(), []Env{{Name: "mars", Branch: "staging-mars"}})[0]; got.State != StateUnknown || !strings.Contains(got.Why, "staging-mars") {
		t.Errorf("a branch the remote lacks: %s %q, want unknown naming it", got.State, got.Why)
	}
	if got := one(c, PR{Repo: "acme/app", Number: 8, Head: s.p2, Base: "master"}); got.State != StateUnknown || !strings.Contains(got.Why, "#8") {
		t.Errorf("a PR the Refresh did not fetch: %s %q, want unknown naming it", got.State, got.Why)
	}
	if got := one(c, PR{Repo: "acme/app", Number: 7, Head: s.p2, Base: "release"}); got.State != StateUnknown || !strings.Contains(got.Why, `"master"`) || !strings.Contains(got.Why, `"release"`) {
		t.Errorf("a PR asked on another base than the Refresh's, without Commits: %s %q, want unknown naming both", got.State, got.Why)
	}
	if got := one(c, PR{Repo: "acme/app", Number: 7, Head: s.p2, Base: "release", Commits: []string{s.p1, s.p2}}); got.State != StatePushed {
		t.Errorf("a PR on another base with Commits: %s %q, want pushed", got.State, got.Why)
	}
	if got := one(c, PR{Repo: "acme/other", Number: 7, Head: s.p2, Base: "master"}); got.State != StateUnknown || !strings.Contains(got.Why, "no refresh") {
		t.Errorf("another repository: %s %q, want unknown, no refresh", got.State, got.Why)
	}
}

// A head the Refresh did not fetch (the PR moved since) is unknown until the
// next Refresh, not judged on the fetched head's commits; a caller behind the
// mirror gets its own head's answer.
func TestAHeadNewerThanTheMirrorIsUnknownUntilTheNextRefresh(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.push(10*time.Minute, s.p1)
	c := s.client()
	s.refresh(c, 7)
	p3 := s.commit(time.Minute, s.p2)
	got := one(c, PR{Repo: "acme/app", Number: 7, Head: p3, Base: "master"})
	if got.State != StateUnknown || !strings.Contains(got.Why, "not fetched yet") {
		t.Errorf("a newer head: %s %q, want unknown until it is fetched", got.State, got.Why)
	}
	got = one(c, PR{Repo: "acme/app", Number: 7, Head: s.p1, Base: "master"})
	if got.State != StatePushed || got.InTip != s.p1 {
		t.Errorf("an older head the mirror has: %s at %q, want pushed at p1", got.State, got.InTip)
	}
}

// Commits nil: Status takes the PR's commits from the mirror,
// master..refs/pull/7/head; the master commits under the PR never count.
func TestCommitsNilAreTheMirrorsBaseToPullHead(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	// PR 7 merges master in (m2), then adds p3; staging gets only master's
	// m2 and the PR at p2.
	m2 := s.commit(2*time.Hour, s.m1)
	s.ref("refs/heads/master", m2)
	pm := s.merge(90*time.Minute, s.p2, m2)
	p3 := s.commit(80*time.Minute, pm)
	s.ref("refs/pull/7/head", p3)
	s.push(70*time.Minute, m2)
	s.push(60*time.Minute, s.p2)
	c := s.client()
	s.refresh(c, 7)
	got := one(c, PR{Repo: "acme/app", Number: 7, Head: p3, Base: "master"})
	if got.State != StateOutdated || got.InTip != s.p2 || got.Behind != 2 {
		t.Errorf("derived commits: %s at %q behind %d (why %q), want outdated at p2, 2 behind (pm, p3; never m2)",
			got.State, got.InTip, got.Behind, got.Why)
	}
	explicit := one(c, PR{Repo: "acme/app", Number: 7, Head: p3, Base: "master", Commits: []string{s.p1, s.p2, pm, p3}})
	if explicit != got {
		t.Errorf("explicit commits: %+v, want the derived answer %+v", explicit, got)
	}
}

// Status is a lookup: no git and no GitHub call, however often it runs.
func TestStatusMakesNoGitOrGitHubCall(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	m := s.push(10*time.Minute, s.p2)
	s.gh.runs[m] = []Run{run("completed", "failure", Job{Name: "rspec", Paths: []string{"x.rb"}})}
	c := s.client()
	s.refresh(c, 7)
	gitCalls, ghCalls := s.git.total(), s.gh.total()
	for range 5 {
		c.Status(s.pr(), []Env{void, {Name: "bart", Branch: "staging-bart"}})
		c.Status(PR{Repo: "acme/app", Number: 9, Head: s.p1, Base: "master"}, []Env{void})
	}
	s.now = s.now.Add(2 * time.Hour) // the data is old: Status still only reads it
	c.Status(s.pr(), []Env{void})
	if s.git.total() != gitCalls || s.gh.total() != ghCalls {
		t.Errorf("Status ran %d git and %d GitHub calls, want none", s.git.total()-gitCalls, s.gh.total()-ghCalls)
	}
}

// The PRs' commits that cannot be listed (one PR's history too large) cost
// the PRs without Commits their answer, never the environments.
func TestPRCommitsThatCannotBeReadCostOnlyThePRsWithoutCommits(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.push(10*time.Minute, s.p2)
	s.git.graphErr = func(include []string) error {
		if slices.Contains(include, s.p2) {
			return errors.New("rev-list's output was cut: the graph is too large")
		}
		return nil
	}
	c := s.client()
	s.refresh(c, 7)
	if got := one(c, s.pr()); got.State != StateUnknown || !strings.Contains(got.Why, "too large") {
		t.Errorf("without Commits: %s %q, want unknown naming the cut", got.State, got.Why)
	}
	pr := s.pr()
	pr.Commits = []string{s.p1, s.p2}
	if got := one(c, pr); got.State != StatePushed {
		t.Errorf("with Commits: %s %q, want pushed", got.State, got.Why)
	}
}
