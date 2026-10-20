package engine

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
)

// opsGH is the fake GitHub recording the op each read's context carries
// (github.OpOf), "<method>:<op>", for the reads whose op the test names.
type opsGH struct {
	*fakeGH
	mu  sync.Mutex
	ops []string
}

func (g *opsGH) note(ctx context.Context, method string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ops = append(g.ops, method+":"+github.OpOf(ctx))
}

func (g *opsGH) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.ops)
}

func (g *opsGH) ComparePush(ctx context.Context, owner, repo, base, head string) (github.PushComparison, error) {
	g.note(ctx, "ComparePush")
	return g.fakeGH.ComparePush(ctx, owner, repo, base, head)
}

func (g *opsGH) CompareFiles(ctx context.Context, owner, repo, base, head string) ([]github.FileDelta, error) {
	g.note(ctx, "CompareFiles")
	return g.fakeGH.CompareFiles(ctx, owner, repo, base, head)
}

func (g *opsGH) AllReviews(ctx context.Context, owner, repo string, number int) ([]github.Review, bool, error) {
	g.note(ctx, "AllReviews")
	return g.fakeGH.AllReviews(ctx, owner, repo, number)
}

func (g *opsGH) AllReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.Thread, bool, error) {
	g.note(ctx, "AllReviewThreads")
	return g.fakeGH.AllReviewThreads(ctx, owner, repo, number)
}

// The engine labels its reads with what they are for, so the meter tells
// the spenders apart: a push's comparison reads as the poll's (its
// since-review size or its trivial-delta check), a re-review's setup as
// round.* (its delta check, its setup, its full pass) and a fix job's
// reads of the other reviewers as fix.external; none of the reads the test
// sees goes unlabelled.
func TestTheEnginesReadsCarryWhatTheyAreFor(t *testing.T) {
	var gh *opsGH
	h, git := fixHarness(t, func(h *harness) {
		gh = &opsGH{fakeGH: h.gh}
		h.d.GitHub = func(id string) GitHub {
			if id == "zhuravel" {
				return gh
			}
			return nil
		}
	})
	fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	pushingFixer(h, git, fixPushed)
	h.fix(FixPayload{})
	h.handle()
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	h.advance(5 * time.Minute)
	h.tick()

	seen := gh.seen()
	for _, want := range []string{"AllReviewThreads:" + opFixExternal, "AllReviews:" + opFixExternal} {
		if !slices.Contains(seen, want) {
			t.Errorf("no %s among the reads %v", want, seen)
		}
	}
	// The push's comparison serves the poll's since-review size and its
	// delta check (one call a tick, compare.go): it counts as the first's.
	if !slices.Contains(seen, "ComparePush:"+opSinceReview) && !slices.Contains(seen, "ComparePush:"+opPushDelta) {
		t.Errorf("the push's comparison read as neither %s nor %s: %v", opSinceReview, opPushDelta, seen)
	}
	if !slices.ContainsFunc(seen, func(s string) bool { return strings.Contains(s, ":round.") }) {
		t.Errorf("the re-review's setup read nothing as round.*: %v", seen)
	}
	for _, s := range seen {
		if strings.HasSuffix(s, ":") {
			t.Errorf("an unlabelled read: %s (all: %v)", s, seen)
		}
	}
}
