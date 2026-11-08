package staging

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Refresh fetches and reads GitHub at most once a minute per repository: a
// second call within the minute reuses the data, and one a minute later
// fetches again.
func TestRefreshRunsAtMostOnceAMinutePerRepository(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	m := s.push(10*time.Minute, s.p2)
	s.gh.runs[m] = []Run{run("in_progress", "")}
	c := s.client()
	s.refresh(c, 7)
	if n := s.git.n("Fetch"); n != 1 {
		t.Fatalf("first Refresh: %d fetches, want 1", n)
	}
	gitCalls, ghCalls := s.git.total(), s.gh.total()
	s.now = s.now.Add(RefreshEvery - time.Second)
	s.refresh(c, 7)
	if s.git.total() != gitCalls || s.gh.total() != ghCalls {
		t.Errorf("a Refresh within the minute ran %d git and %d GitHub calls, want none", s.git.total()-gitCalls, s.gh.total()-ghCalls)
	}
	if got := one(c, s.pr()); got.State != StatePushed {
		t.Errorf("after the reused Refresh: %s, want pushed", got.State)
	}
	s.now = s.now.Add(time.Second)
	s.refresh(c, 7)
	if n := s.git.n("Fetch"); n != 2 {
		t.Errorf("a Refresh a minute later: %d fetches in all, want 2", n)
	}
}

// Two clients on one mirror (the review daemon and the factory daemon): the
// flock in the mirror dir makes the second wait for the first, and then it
// reuses the first one's data instead of fetching again. A wait never
// outlasts the caller's context.
func TestTheFlockSerializesTwoClientsOnOneMirror(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.push(10*time.Minute, s.p2)
	dir := t.TempDir() + "/acme/app.git"
	a, b := s.clientAt(dir), s.clientAt(dir)

	inFetch, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.git.inFetch = func() {
		once.Do(func() {
			close(inFetch)
			<-release
		})
	}
	aDone := make(chan error, 1)
	go func() { aDone <- a.Refresh(context.Background(), "acme/app", []int{7}) }()
	<-inFetch

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	err := b.Refresh(ctx, "acme/app", []int{7})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("b while a holds the lock: %v, want its context's deadline", err)
	}
	if got := one(b, s.pr()); got.State != StateUnknown {
		t.Errorf("b after a cut Refresh: %s, want unknown (no data yet)", got.State)
	}

	bDone := make(chan error, 1)
	go func() { bDone <- b.Refresh(context.Background(), "acme/app", []int{7}) }()
	select {
	case err := <-bDone:
		t.Fatalf("b returned (%v) while a still held the lock", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("a: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("b: %v", err)
	}
	if n := s.git.n("Fetch"); n != 1 {
		t.Errorf("%d fetches for two clients within the minute, want 1", n)
	}
	if got := one(b, s.pr()); got.State != StatePushed || got.InTip != s.p2 {
		t.Errorf("b on a's data: %s at %q, want pushed at the head", got.State, got.InTip)
	}
}

// Each daemon's open PRs are fetched by whichever refreshes next: the PRs
// another client asked for within the last minutes join the fetch.
func TestARefreshFetchesThePRsAnotherClientAskedFor(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	q := s.commit(time.Hour, s.m1)
	s.ref("refs/pull/8/head", q)
	dir := t.TempDir() + "/acme/app.git"
	a, b := s.clientAt(dir), s.clientAt(dir)
	s.refresh(a, 7)
	s.now = s.now.Add(30 * time.Second)
	s.refresh(b, 8) // reuses a's data, and asks for 8
	s.now = s.now.Add(RefreshEvery)
	s.refresh(a, 7)
	last := s.git.fetched[len(s.git.fetched)-1]
	for _, want := range []string{"+refs/pull/7/head:refs/pull/7/head", "+refs/pull/8/head:refs/pull/8/head",
		"+refs/heads/staging-*:refs/heads/staging-*", "+refs/heads/master:refs/heads/master"} {
		if !slices.Contains(last, want) {
			t.Errorf("a's fetch %q lacks %q", last, want)
		}
	}
	if got := one(a, PR{Repo: "acme/app", Number: 8, Head: q, Base: "master"}); got.State != StateNone {
		t.Errorf("PR 8 on a's data: %s %q, want none", got.State, got.Why)
	}
}

// A PR whose pull ref the remote lacks does not fail the fetch for the
// others: it is dropped and the fetch runs again.
func TestAPullRefTheRemoteLacksIsDroppedFromTheFetch(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.push(10*time.Minute, s.p2)
	s.git.missing["refs/pull/9/head"] = true
	c := s.client()
	s.refresh(c, 7, 9)
	if n := s.git.n("Fetch"); n != 2 {
		t.Errorf("%d fetches, want 2 (the second without PR 9)", n)
	}
	last := s.git.fetched[len(s.git.fetched)-1]
	if slices.ContainsFunc(last, func(spec string) bool { return strings.Contains(spec, "pull/9/") }) {
		t.Errorf("the retry still fetches PR 9: %q", last)
	}
	if got := one(c, s.pr()); got.State != StatePushed {
		t.Errorf("PR 7: %s %q, want pushed", got.State, got.Why)
	}
}

// The deployments are read with the last ETag: a 304 keeps the last list.
func TestDeploymentsAreReadWithTheLastETag(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	m := s.push(20*time.Minute, s.p2)
	s.deployed(m)
	c := s.client()
	s.refresh(c, 7)
	s.now = s.now.Add(RefreshEvery)
	s.refresh(c, 7)
	if len(s.gh.etagSent) != 2 || s.gh.etagSent[0] != "" || s.gh.etagSent[1] == "" {
		t.Errorf("ETags sent %q, want none and then the first answer's", s.gh.etagSent)
	}
	if got := one(c, s.pr()); got.State != StateLatest {
		t.Errorf("after a 304: %s %q, want latest on the kept list", got.State, got.Why)
	}
}

// Nothing is fetched without the env of the poll identity: the operator's
// own credentials never answer implicitly.
func TestNoGitEnvFetchesNothing(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	dir := t.TempDir() + "/acme/app.git"
	c := New(Deps{Git: s.git, GitHub: s.gh, Mirror: func(string) (string, bool) { return dir, true },
		GitEnv: func(string) map[string]string { return nil }, Now: func() time.Time { return s.now }})
	if err := c.Refresh(context.Background(), "acme/app", []int{7}); err == nil {
		t.Error("Refresh without a git env: no error")
	}
	if s.git.total() != 0 {
		t.Errorf("%d git calls without a git env, want none", s.git.total())
	}
	if got := one(c, s.pr()); got.State != StateUnknown || !strings.Contains(got.Why, "git env") {
		t.Errorf("%s %q, want unknown naming the git env", got.State, got.Why)
	}
}
