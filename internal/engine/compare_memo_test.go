package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// rangeCalls lists g's compare calls of any kind for base...head.
func rangeCalls(g *fakeGH, base, head string) []string {
	return slices.DeleteFunc(append(callsWith(g, "compare:"), callsWith(g, "compare_files:")...), func(c string) bool {
		return !strings.HasSuffix(c, ":"+base+"..."+head)
	})
}

// A push to a reviewed PR with an App approval: the trivial-delta check,
// the approval check and the since-review size all ask about the reviewed
// commit against the new head, and the poll asks GitHub once.
func TestOnePollComparesAPushOnce(t *testing.T) {
	h, app := newApprovalHarness(t)
	approvedPR(h, 2, "b1")
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 2, Files: 1, Additions: 2, Deletions: 2}
	h.gh.files = map[string][]github.FileDelta{"b1...b2": rubyMixed}

	pollPR(h, time.Minute, 2, "b2")
	pr := h.wantState(2, store.PRRereviewPending)
	if got := rangeCalls(h.gh, "b1", "b2"); len(got) != 1 {
		t.Fatalf("compares of b1...b2 = %q, want one", got)
	}
	if got := app.comparesMade(); len(got) != 0 {
		t.Fatalf("the App identity compared %q again", got)
	}
	if deref(pr.LastReviewEvent) != "DISMISSED" {
		t.Fatalf("last_review_event %q: the approval check did not see the 2 new commits", deref(pr.LastReviewEvent))
	}
	if s := pr.SinceReview; s == nil || s.Base != "b1" || s.Commits != 2 || s.Files != 1 || s.Additions != 2 || s.Deletions != 2 {
		t.Fatalf("since_review = %+v, want the comparison's size", s)
	}
}

// A review whose head moved during its round counts the commits that
// arrived from the comparison the delta check made, without asking again.
func TestAReviewWhoseHeadMovedComparesTheRangeOnce(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.rd.gate = make(chan struct{})
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	h.advance(time.Minute)
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 3}
	h.gh.files = map[string][]github.FileDelta{"b1...b2": rubyMixed}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	close(h.rd.gate)
	h.settle()
	// The note's wording follows the PR's wait (TestMovedHeadNoteFollowsThePRsWait); this test is about the comparisons.
	if got := h.rd.appended(); len(got) != 1 || !strings.HasPrefix(got[0], "101:_Reviewed b1; 3 commits arrived during the review") {
		t.Fatalf("review notes = %q", got)
	}
	if got := rangeCalls(h.gh, "b1", "b2"); len(got) != 1 {
		t.Fatalf("compares of b1...b2 = %q, want one", got)
	}
}

// The tick's comparisons: a range is asked once whatever the kind of call,
// a failed call is never served again (the next one asks GitHub), and the
// next tick asks afresh.
func TestTheTicksComparisons(t *testing.T) {
	h := newHarness(t)
	repo := store.Repo{Owner: "talkable", Name: "talkable"}
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 1, Files: 1, Additions: 3}
	h.gh.files = map[string][]github.FileDelta{"b1...b2": rubyMixed}
	gh := h.e.gh("zhuravel")

	h.gh.mu.Lock()
	h.gh.compareErr = errors.New("HTTP 502")
	h.gh.mu.Unlock()
	if _, err := h.e.compareStats(h.ctx, gh, repo, "b1", "b2"); err == nil {
		t.Fatal("the failure was not passed on")
	}
	h.gh.mu.Lock()
	h.gh.compareErr = nil
	h.gh.mu.Unlock()
	if cs, err := h.e.compareStats(h.ctx, gh, repo, "b1", "b2"); err != nil || cs.Additions != 3 {
		t.Fatalf("after a failure: %+v, %v", cs, err)
	}
	if n := len(callsWith(h.gh, "compare:")); n != 2 {
		t.Fatalf("compare calls = %d, want the failed one and its retry", n)
	}
	if _, err := h.e.compareStats(h.ctx, gh, repo, "b1", "b2"); err != nil || len(callsWith(h.gh, "compare:")) != 2 {
		t.Fatalf("the kept comparison was asked again (%v)", err)
	}

	// A push comparison answers the files and the stats of its range.
	if _, err := h.e.comparePush(h.ctx, gh, repo, "b1", "b2"); err != nil {
		t.Fatal(err)
	}
	if files, err := h.e.compareFiles(h.ctx, gh, repo, "b1", "b2"); err != nil || len(files) != 1 {
		t.Fatalf("files %v, %v", files, err)
	}
	if n := len(callsWith(h.gh, "compare_files:")); n != 1 {
		t.Fatalf("compare_files calls = %d, want the push comparison only", n)
	}

	h.tick()
	before := len(callsWith(h.gh, "compare:"))
	if _, err := h.e.compareStats(h.ctx, gh, repo, "b1", "b2"); err != nil || len(callsWith(h.gh, "compare:")) != before+1 {
		t.Fatalf("a new tick reused the last tick's comparison (%v)", err)
	}
}
