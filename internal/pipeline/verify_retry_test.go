package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
)

// netDown is what gh prints when it cannot reach GitHub (the live case of
// 2026-10-07: a review posted, then "verify on GitHub" failed this way).
const netDown = "gh api graphql --input -: gh exited 1: error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com"

// flakyGitHub fails ReviewsWithMarker, once armed, with one queued error per
// call; calls counts the calls made since it was armed. Armed by the judge's
// turn, so only the round's verification meets the failures.
type flakyGitHub struct {
	*fakeGitHub
	fmu   sync.Mutex
	errs  []error
	calls int
	armed bool
}

// arm queues n copies of err for the next ReviewsWithMarker calls.
func (g *flakyGitHub) arm(n int, err error) {
	g.fmu.Lock()
	defer g.fmu.Unlock()
	g.armed, g.calls = true, 0
	g.errs = nil
	for range n {
		g.errs = append(g.errs, err)
	}
}

func (g *flakyGitHub) ReviewsWithMarker(ctx context.Context, owner, repo string, number int, marker string) ([]github.Review, error) {
	g.fmu.Lock()
	if g.armed {
		g.calls++
		if len(g.errs) > 0 {
			err := g.errs[0]
			g.errs = g.errs[1:]
			g.fmu.Unlock()
			return nil, err
		}
	}
	g.fmu.Unlock()
	return g.fakeGitHub.ReviewsWithMarker(ctx, owner, repo, number, marker)
}

func (g *flakyGitHub) listCalls() int {
	g.fmu.Lock()
	defer g.fmu.Unlock()
	return g.calls
}

// flakyThreads fails ReviewThreads, once armed, with one queued error per
// call.
type flakyThreads struct {
	*replyGitHub
	fmu   sync.Mutex
	errs  []error
	calls int
	armed bool
}

func (g *flakyThreads) arm(n int, err error) {
	g.fmu.Lock()
	defer g.fmu.Unlock()
	g.armed, g.calls = true, 0
	g.errs = nil
	for range n {
		g.errs = append(g.errs, err)
	}
}

func (g *flakyThreads) ReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.Thread, error) {
	g.fmu.Lock()
	if g.armed {
		g.calls++
		if len(g.errs) > 0 {
			err := g.errs[0]
			g.errs = g.errs[1:]
			g.fmu.Unlock()
			return nil, err
		}
	}
	g.fmu.Unlock()
	return g.replyGitHub.ReviewThreads(ctx, owner, repo, number)
}

func (g *flakyThreads) threadCalls() int {
	g.fmu.Lock()
	defer g.fmu.Unlock()
	return g.calls
}

// recordSleeps wraps the runner's Sleep and returns how many sleeps of d it
// saw.
func (e *env) recordSleeps() func(d time.Duration) int {
	var mu sync.Mutex
	var slept []time.Duration
	inner := e.r.Sleep
	e.r.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		return inner(ctx, d)
	}
	return func(d time.Duration) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, s := range slept {
			if s == d {
				n++
			}
		}
		return n
	}
}

// judgeAfterArming runs b, the judge's scripted turn, after arming fail, so
// the round's own verification (and nothing before the judge) is what meets
// the failures.
func judgeAfterArming(fail func(), b behavior) behavior {
	return func(f *fakeAgents, run store.Run, text string) error {
		fail()
		return b(f, run, text)
	}
}

// The live case (2026-10-07): the judge posted
// its review, then every GitHub read of the verification hit a network blip
// and the round ended in error; the next round adopted the review 20 minutes
// later. A network failure that outlasts the reads' own retries now waits
// verifyNetRetry and asks once more, so the round that posted the review
// verifies it.
func TestVerifyAsksAgainAfterANetworkFailureOutlastsTheReadRetries(t *testing.T) {
	e := newEnv(t)
	gh := &flakyGitHub{fakeGitHub: e.gh}
	e.r.GitHub = gh
	slept := e.recordSleeps()
	e.ag.behaviors[agents.RoleJudge] = []behavior{judgeAfterArming(
		func() { gh.arm(verifyAttempts, errors.New(netDown)) },
		e.judgePosts(700, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t))}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 700 {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if n := slept(verifyNetRetry); n != 1 {
		t.Errorf("slept %s %d times, want once", verifyNetRetry, n)
	}
	if got := gh.listCalls(); got != verifyAttempts+1 {
		t.Errorf("review reads = %d, want %d failing and one answered", got, verifyAttempts)
	}
	evs := eventsOfKind(e.events(), "round.verify_retry")
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "GitHub unreachable") {
		t.Fatalf("round.verify_retry events: %+v", evs)
	}
	if strings.Contains(evs[0].Message, "api.github.com") {
		t.Errorf("the event repeats the error text: %q", evs[0].Message)
	}
}

// GitHub's GraphQL limit of 10 s answers an empty body, and gh reports
// "unexpected end of JSON input" 11-12 s later: a GitHub server error, so the
// round's review check waits and asks again as after a network failure
// instead of ending the round with its check failed.
func TestVerifyAsksAgainAfterGitHubAnsweredNothing(t *testing.T) {
	e := newEnv(t)
	gh := &flakyGitHub{fakeGitHub: e.gh}
	e.r.GitHub = gh
	slept := e.recordSleeps()
	empty := errors.New("github reviews talkable/talkable#7: gh api graphql --input - exited 1: unexpected end of JSON input")
	e.ag.behaviors[agents.RoleJudge] = []behavior{judgeAfterArming(
		func() { gh.arm(verifyAttempts, empty) },
		e.judgePosts(703, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t))}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 703 {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if n := slept(verifyNetRetry); n != 1 {
		t.Errorf("slept %s %d times, want once", verifyNetRetry, n)
	}
	if evs := eventsOfKind(e.events(), "round.verify_retry"); len(evs) != 1 || !strings.Contains(evs[0].Message, "GitHub server error") {
		t.Fatalf("round.verify_retry events: %+v", evs)
	}
}

// A network failure that lasts through the second asking too ends the round
// as before: unverified, to be adopted by the next round.
func TestVerifyFailsWhenTheNetworkStaysDownAfterTheRetry(t *testing.T) {
	e := newEnv(t)
	gh := &flakyGitHub{fakeGitHub: e.gh}
	e.r.GitHub = gh
	slept := e.recordSleeps()
	e.ag.behaviors[agents.RoleJudge] = []behavior{judgeAfterArming(
		func() { gh.arm(2*verifyAttempts, errors.New(netDown)) },
		e.judgePosts(701, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t))}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err == nil || res.Outcome != OutcomeError || !strings.Contains(res.Error, "verify on GitHub") {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if !errors.Is(err, errUnverified) {
		t.Errorf("error %v is not errUnverified", err)
	}
	if n := slept(verifyNetRetry); n != 1 {
		t.Errorf("slept %s %d times, want once (one more asking, not a loop)", verifyNetRetry, n)
	}
	if got := gh.listCalls(); got != 2*verifyAttempts {
		t.Errorf("review reads = %d, want %d", got, 2*verifyAttempts)
	}
}

// Only a network cause waits: a verdict from GitHub (not found) fails at
// once with one read, and an unclassified failure keeps the reads' own
// retries and no more.
func TestVerifyDoesNotWaitForFailuresThatAreNotTheNetwork(t *testing.T) {
	for name, c := range map[string]struct {
		err   error
		reads int
	}{
		"not found":    {fmt.Errorf("gh api graphql: HTTP 404: Not Found: %w", github.ErrNotFound), 1},
		"unclassified": {errors.New("gh api graphql: exit status 1: connection reset"), verifyAttempts},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			gh := &flakyGitHub{fakeGitHub: e.gh}
			e.r.GitHub = gh
			slept := e.recordSleeps()
			e.ag.behaviors[agents.RoleJudge] = []behavior{judgeAfterArming(
				func() { gh.arm(10, c.err) },
				e.judgePosts(702, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t))}

			res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
			if err == nil || res.Outcome != OutcomeError || !strings.Contains(res.Error, "verify on GitHub") {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			if n := slept(verifyNetRetry); n != 0 {
				t.Errorf("slept %s %d times for a failure that is not the network", verifyNetRetry, n)
			}
			if got := gh.listCalls(); got != c.reads {
				t.Errorf("review reads = %d, want %d", got, c.reads)
			}
			if evs := eventsOfKind(e.events(), "round.verify_retry"); len(evs) != 0 {
				t.Errorf("round.verify_retry events: %+v", evs)
			}
		})
	}
}

// A reply round's verification of its replies has no read retries of its
// own: a network failure of the thread listing waits and asks once more
// before the replied result goes unverified.
func TestReplyVerificationAsksAgainAfterANetworkFailure(t *testing.T) {
	e := newEnv(t)
	gh := &flakyThreads{replyGitHub: &replyGitHub{fakeGitHub: e.gh, threads: prThreads()}}
	e.r.GitHub = gh
	slept := e.recordSleeps()
	e.ag.behaviors[agents.RoleJudge] = []behavior{judgeAfterArming(
		func() { gh.arm(1, errors.New(netDown)) },
		judgeReplies(t, gh.replyGitHub, [][2]any{{int64(101), postreview.ReplyRebuttal}}, 1))}

	res, err := e.r.RunRound(e.ctx, e.replyInput())
	if err != nil || res.Outcome != OutcomeReplied || len(res.Replies) != 1 {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if n := slept(verifyNetRetry); n != 1 {
		t.Errorf("slept %s %d times, want once", verifyNetRetry, n)
	}
	if got := gh.threadCalls(); got != 2 {
		t.Errorf("thread reads after the judge = %d, want 2", got)
	}

	// Still down on the second asking: the round fails unverified as before.
	e2 := newEnv(t)
	gh2 := &flakyThreads{replyGitHub: &replyGitHub{fakeGitHub: e2.gh, threads: prThreads()}}
	e2.r.GitHub = gh2
	slept2 := e2.recordSleeps()
	e2.ag.behaviors[agents.RoleJudge] = []behavior{judgeAfterArming(
		func() { gh2.arm(2, errors.New(netDown)) },
		judgeReplies(t, gh2.replyGitHub, [][2]any{{int64(101), postreview.ReplyRebuttal}}, 1))}
	res, err = e2.r.RunRound(e2.ctx, e2.replyInput())
	if err == nil || res.Outcome != OutcomeError || !strings.Contains(res.Error, "verify on GitHub") {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if n := slept2(verifyNetRetry); n != 1 {
		t.Errorf("slept %s %d times, want once", verifyNetRetry, n)
	}
}
