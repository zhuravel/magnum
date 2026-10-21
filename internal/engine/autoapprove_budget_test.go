package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// rateLimitRefusal is GitHub's GraphQL refusal of a call for its rate
// limit, which github.ErrRateLimited matches.
func rateLimitRefusal(op string) error {
	return &github.APIError{Op: op, Errors: []github.GraphQLError{{Type: "RATE_LIMIT", Message: "API rate limit already exceeded for user ID 1."}}}
}

// autoBudgetPR reviews PR #2 at b1 next to the baseline #1 with the
// identity zhuravel's meter at remaining points until reset, as a call
// after the start told it (the start reads the meter's records back).
func autoBudgetPR(h *harness, remaining int, reset time.Time, spec prSpec) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.metered("zhuravel", opRound, 1, remaining, reset)
	h.open(prSpec{n: 1, head: "base1"}, spec)
	h.tick() // #2 queued
	h.advance(5 * time.Minute)
	h.tick() // dispatched and reviewed: the round's end decides the approval
	h.wantState(2, store.PRReviewed)
}

// tickUntil ticks once a minute until the clock reaches at.
func tickUntil(h *harness, at time.Time) {
	h.t.Helper()
	for h.clock.Now().Before(at) {
		h.advance(time.Minute)
		h.tick()
	}
}

// A read of GitHub's reviews that GitHub refused for its rate limit was
// read again on every tick (no fingerprint is kept for a failed read): 96
// such reads from 10-19 to 10-21. Now the refusal waits for the identity's
// reset, the one the meter knows (3,838 points left until then before the
// refusal), and the next read after it approves.
func TestAutoApprovalReadsNothingWhileTheBudgetIsExhausted(t *testing.T) {
	h, _ := newAutoHarness(t, func(h *harness) { h.gh.reviewsErr = rateLimitRefusal("reviews talkable/talkable#2") })
	reset := h.clock.Now().Add(40 * time.Minute)
	autoBudgetPR(h, 3838, reset, prSpec{n: 2, head: "b1"})
	if got := h.ghCalls("all_reviews:"); len(got) != 1 {
		t.Fatalf("reads of the reviews at the round's end = %q, want the one GitHub refused", got)
	}
	tickUntil(h, reset.Add(-time.Minute))
	if got := h.ghCalls("all_reviews:"); len(got) != 1 {
		t.Fatalf("reads of the reviews before the reset = %d, want 1: the refused read waits for the reset", len(got))
	}
	if got := h.created(); len(got) != 0 {
		t.Fatalf("posted %q before the reviews were read", got)
	}
	h.gh.mu.Lock()
	h.gh.reviewsErr = nil
	h.gh.mu.Unlock()
	h.advance(2 * time.Minute) // the first tick after the reset
	h.tick()
	if got := h.ghCalls("all_reviews:"); len(got) != 2 {
		t.Fatalf("reads of the reviews after the reset = %d, want 2", len(got))
	}
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want the approval of b1 once the reviews read", got)
	}
}

// Auto-approval reads nothing from GitHub as an identity whose budget is
// below [github] budget_floor, or while the poll waits for GitHub's rate
// limit, and says so once per hold, not on every tick: here the round's
// last call left the operator's gh login at 300 points, or paused the
// poll, until the reset; the first tick after it reads the reviews and
// approves.
func TestAutoApprovalWaitsForTheGitHubFloorAndThePollsPause(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(h *harness, reset time.Time) // what the round's end leaves
		said  string
	}{
		{"below the floor", func(h *harness, reset time.Time) { h.metered("zhuravel", opRound, 1, 300, reset) },
			"github budget: zhuravel at 300 points until"},
		{"the poll paused", func(h *harness, reset time.Time) {
			if err := h.st.SetKV(h.ctx, store.KVGHPollPausedUntil, store.FormatTime(reset)); err != nil {
				h.t.Errorf("pause the poll: %v", err)
			}
		}, "the poll waits for GitHub's rate limit until"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs syncBuffer
			h, _ := newAutoHarness(t, func(h *harness) { h.d.Logger = newTeeLogger(t, &logs) })
			reset := h.clock.Now().Add(40 * time.Minute)
			h.rd.mu.Lock()
			script := h.rd.script
			h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
				res, err := script(in)
				tc.after(h, reset)
				return res, err
			}
			h.rd.mu.Unlock()
			autoBudgetPR(h, 4000, reset, prSpec{n: 2, head: "b1"})
			tickUntil(h, reset.Add(-time.Minute))
			if got := h.ghCalls("all_reviews:"); len(got) != 0 {
				t.Fatalf("read the reviews %d times during the hold, want none", len(got))
			}
			if n := strings.Count(logs.String(), "auto approval: no GitHub reads"); n != 1 {
				t.Fatalf("the hold was logged %d times, want once:\n%s", n, logs.String())
			}
			if !strings.Contains(logs.String(), tc.said) {
				t.Fatalf("the log does not say %q:\n%s", tc.said, logs.String())
			}
			h.advance(2 * time.Minute) // the first tick after the reset
			h.tick()
			if got := h.ghCalls("all_reviews:"); len(got) != 1 {
				t.Fatalf("reads of the reviews after the reset = %d, want 1", len(got))
			}
			if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
				t.Fatalf("posted = %q, want the approval of b1", got)
			}
		})
	}
}

// A standing approval is followed against GitHub when the PR moves; while
// the identity it reads as is below the floor, the follow waits for the
// reset, and reads once after it.
func TestAStandingApprovalIsFollowedOnlyOnceTheBudgetRecovers(t *testing.T) {
	h, _ := newAutoHarness(t)
	autoBudgetPR(h, 4000, h.clock.Now().Add(time.Hour), prSpec{n: 2, head: "b1"})
	h.flush() // the approval's first follow
	if a := h.autoApproval(2); a.State != store.AutoStanding {
		t.Fatalf("approval = %+v, want it standing", a)
	}
	reads := len(h.ghCalls("all_reviews:"))
	reset := h.clock.Now().Add(30 * time.Minute)
	h.metered("zhuravel", opRound, 1, 120, reset)
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", updated: h.clock.Now()}) // a comment moves the PR
	tickUntil(h, reset.Add(-time.Minute))
	if got := len(h.ghCalls("all_reviews:")); got != reads {
		t.Fatalf("reads of the reviews below the floor = %d, want none", got-reads)
	}
	h.advance(2 * time.Minute) // the first tick after the reset
	h.tick()
	if got := len(h.ghCalls("all_reviews:")); got != reads+1 {
		t.Fatalf("reads of the reviews after the reset = %d, want 1", got-reads)
	}
}

// The base tip's checks, which the gates read once per tip and ten
// minutes, are not read again before the identity's reset when GitHub
// refused the read for its rate limit; after it they are, and the
// approval goes ahead.
func TestARefusedReadOfTheBaseTipsChecksWaitsForTheReset(t *testing.T) {
	h, _ := newAutoHarness(t, withBaseChecks(auditCheck(github.CheckFailed)),
		func(h *harness) { h.gh.commitChecksErr = rateLimitRefusal("commit checks") })
	reset := h.clock.Now().Add(45 * time.Minute)
	autoBudgetPR(h, 3838, reset, prSpec{n: 2, head: "b1", ci: "FAILURE",
		checks: []github.Check{auditCheck(github.CheckFailed), specsCheck(github.CheckPassed)}})
	tickUntil(h, reset.Add(-time.Minute))
	base := "commit_checks:talkable/talkable@" + fakeBaseOid
	if n := len(h.ghCalls(base)); n != 1 {
		t.Fatalf("read the base tip's checks %d times before the reset, want once", n)
	}
	h.wantRefused(2, ciFailsWords)
	h.gh.mu.Lock()
	h.gh.commitChecksErr = nil
	h.gh.mu.Unlock()
	h.advance(2 * time.Minute) // the first tick after the reset
	h.tick()
	if n := len(h.ghCalls(base)); n != 2 {
		t.Fatalf("read the base tip's checks %d times after the reset, want twice", n)
	}
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want the approval of b1", got)
	}
}
