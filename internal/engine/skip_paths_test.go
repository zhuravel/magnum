package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// filesGH is the harness's fake GitHub plus ListFiles: the file list of each
// PR number, a failure switch and the numbers asked for.
type filesGH struct {
	*fakeGH
	lmu        sync.Mutex
	files      map[int][]string
	incomplete map[int]bool
	err        error
	listed     []int
}

func (g *filesGH) ListFiles(_ context.Context, owner, repo string, number int) ([]string, bool, error) {
	g.record(fmt.Sprintf("files:%s/%s#%d", owner, repo, number))
	g.lmu.Lock()
	defer g.lmu.Unlock()
	g.listed = append(g.listed, number)
	if g.err != nil {
		return nil, false, g.err
	}
	return slices.Clone(g.files[number]), !g.incomplete[number], nil
}

func (g *filesGH) set(number int, files ...string) {
	g.lmu.Lock()
	defer g.lmu.Unlock()
	if g.files == nil {
		g.files = map[int][]string{}
	}
	g.files[number] = files
}

func (g *filesGH) fail(err error) {
	g.lmu.Lock()
	g.err = err
	g.lmu.Unlock()
}

func (g *filesGH) setIncomplete(number int) {
	g.lmu.Lock()
	defer g.lmu.Unlock()
	if g.incomplete == nil {
		g.incomplete = map[int]bool{}
	}
	g.incomplete[number] = true
}

// fetches is how many times ListFiles asked for number (-1: for any PR).
func (g *filesGH) fetches(number int) int {
	g.lmu.Lock()
	defer g.lmu.Unlock()
	if number < 0 {
		return len(g.listed)
	}
	n := 0
	for _, l := range g.listed {
		if l == number {
			n++
		}
	}
	return n
}

var docsGlobs = []string{"docs/**", "**/*.md"}

// skipHarness is a harness whose talkable watch has the skip_paths globs and
// whose poll client lists files; the repository's first sync (#1 baseline) is
// done.
func skipHarness(t *testing.T, globs ...string) (*harness, *filesGH) {
	t.Helper()
	fg := &filesGH{}
	h := newHarness(t, func(h *harness) {
		h.cfg.Watches[0].SkipPaths = globs
		fg.fakeGH = h.gh
		h.d.GitHub = func(id string) GitHub {
			if id == "zhuravel" {
				return fg
			}
			return nil
		}
	})
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync
	if n := fg.fetches(-1); n != 0 {
		t.Fatalf("first sync listed files %d times; baseline PRs are never fetched", n)
	}
	return h, fg
}

// openWith lists #1 (baseline) and the given PRs.
func (h *harness) openWith(prs ...prSpec) {
	h.open(append([]prSpec{{n: 1, head: "base1"}}, prs...)...)
}

func wantSkipReason(t *testing.T, pr store.PR, want string) {
	t.Helper()
	if got := deref(pr.SkipReason); got != want {
		t.Fatalf("PR #%d skip_reason = %q, want %q", pr.Number, got, want)
	}
}

func skipSubject(n int) string { return prSubject(store.Repo{Owner: "talkable", Name: "talkable"}, n) }

// A new PR whose files all match skip_paths is ineligible, never reviewed.
func TestSkipPathsNewPRAllFilesMatch(t *testing.T) {
	h, fg := skipHarness(t, docsGlobs...)
	fg.set(2, "docs/guide/a.png", "README.md")
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()

	pr := h.wantState(2, store.PRIneligible)
	wantSkipReason(t, pr, "skip_paths: all 2 changed files match")
	if n := fg.fetches(2); n != 1 {
		t.Fatalf("ListFiles calls = %d, want 1", n)
	}
	if n := fixPollEventCount(t, h, skipSubject(2), "pr.ineligible"); n != 1 {
		t.Fatalf("pr.ineligible events = %d, want 1", n)
	}
	h.advance(time.Hour)
	h.tick()
	h.wantState(2, store.PRIneligible)
	if n := len(h.rd.all()); n != 0 {
		t.Fatalf("rounds = %d, want 0 for a skipped PR", n)
	}
}

// One file outside the globs keeps the PR in line.
func TestSkipPathsOneNonMatchingFileQueues(t *testing.T) {
	h, fg := skipHarness(t, docsGlobs...)
	fg.set(2, "docs/a.md", "src/app.go")
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()

	pr := h.wantState(2, store.PRQueued)
	wantSkipReason(t, pr, "")
	if n := fg.fetches(2); n != 1 {
		t.Fatalf("ListFiles calls = %d, want 1", n)
	}
}

// A rename out of a skipped directory still touches the old path.
func TestSkipPathsRenameOutOfSkippedTreeQueues(t *testing.T) {
	h, fg := skipHarness(t, "docs/**")
	fg.set(2, "docs/main.go", "src/main.go") // ListFiles lists both names of a rename
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()
	h.wantState(2, store.PRQueued)
}

// More files than GitHub listed: never skipped.
func TestSkipPathsIncompleteListQueues(t *testing.T) {
	h, fg := skipHarness(t, docsGlobs...)
	fg.set(2, "docs/a.md", "docs/b.md")
	fg.setIncomplete(2)
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()

	pr := h.wantState(2, store.PRQueued)
	wantSkipReason(t, pr, "")
	c, ok := h.e.cachedFiles(h.ctx, pr.ID)
	if !ok || c.Complete || c.Head != "s1" {
		t.Fatalf("cache = %+v ok=%v, want an incomplete list for s1", c, ok)
	}
}

// The file list is cached per head: a poll never refetches the same head, a
// push does, and the new list decides again.
func TestSkipPathsCacheIsPerHead(t *testing.T) {
	h, fg := skipHarness(t, docsGlobs...)
	fg.set(2, "docs/a.md", "src/app.go")
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()
	h.wantState(2, store.PRQueued)
	if n := fg.fetches(2); n != 1 {
		t.Fatalf("after the first poll: %d fetches, want 1", n)
	}

	// Same head, new updated timestamp, state waiting: still cached.
	for range 3 {
		h.advance(time.Minute)
		h.openWith(prSpec{n: 2, head: "s1"})
		h.tick()
	}
	if n := fg.fetches(2); n != 1 {
		t.Fatalf("polls with the same head: %d fetches, want 1", n)
	}

	// A push whose files all match: fetched again and skipped.
	fg.set(2, "docs/a.md")
	h.advance(time.Minute)
	h.openWith(prSpec{n: 2, head: "s2"})
	h.tick()
	if n := fg.fetches(2); n != 2 {
		t.Fatalf("after the push: %d fetches, want 2", n)
	}
	wantSkipReason(t, h.wantState(2, store.PRIneligible), "skip_paths: all 1 changed files match")

	// Polling again with that head stays free; the PR stays skipped.
	h.advance(time.Minute)
	h.openWith(prSpec{n: 2, head: "s2"})
	h.tick()
	h.wantState(2, store.PRIneligible)
	if n := fg.fetches(2); n != 2 {
		t.Fatalf("polls with the second head: %d fetches, want 2", n)
	}

	// A push that touches code again: back in line.
	fg.set(2, "docs/a.md", "src/app.go")
	h.advance(time.Minute)
	h.openWith(prSpec{n: 2, head: "s3"})
	h.tick()
	pr := h.wantState(2, store.PRQueued)
	wantSkipReason(t, pr, "")
	if n := fg.fetches(2); n != 3 {
		t.Fatalf("after the third head: %d fetches, want 3", n)
	}
}

// A failed fetch is not cached: the PR waits, the failure is reported once,
// and the poll that gets the list classifies the PR at once.
func TestSkipPathsFetchErrorIsNotCachedAndHeals(t *testing.T) {
	h, fg := skipHarness(t, docsGlobs...)
	fg.set(2, "docs/a.md")
	fg.fail(errors.New("files down"))
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()
	pr := h.wantState(2, store.PRQueued)
	if _, ok := h.e.cachedFiles(h.ctx, pr.ID); ok {
		t.Fatal("a failed fetch was cached")
	}

	for range 3 {
		h.advance(time.Minute)
		h.openWith(prSpec{n: 2, head: "s1"})
		h.tick()
		h.wantState(2, store.PRQueued)
	}
	if n := fg.fetches(2); n != 4 {
		t.Fatalf("fetches = %d, want one retry per poll (4)", n)
	}
	if n := fixPollEventCount(t, h, "repo:talkable/talkable", "poll.files_error"); n != 1 {
		t.Fatalf("poll.files_error events = %d, want 1 (repeats are deduplicated)", n)
	}

	// The outage ends; the PR is classified in the same poll.
	fg.fail(nil)
	h.advance(time.Minute)
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()
	pr = h.wantState(2, store.PRIneligible)
	wantSkipReason(t, pr, "skip_paths: all 1 changed files match")
	if n := fg.fetches(2); n != 5 {
		t.Fatalf("fetches = %d, want 5", n)
	}
	// A recurrence of the same message is reported again after a success.
	fg.fail(errors.New("files down"))
	fg.set(2, "docs/a.md", "docs/b.md")
	h.advance(time.Minute)
	h.openWith(prSpec{n: 2, head: "s2"})
	h.tick()
	if n := fixPollEventCount(t, h, "repo:talkable/talkable", "poll.files_error"); n != 2 {
		t.Fatalf("poll.files_error events = %d, want 2 (a new failure after a success)", n)
	}
	h.wantState(2, store.PRQueued)
}

// A forced `magnum review` ignores skip_paths: the PR is claimed and reviewed.
func TestSkipPathsForcedReviewStillRuns(t *testing.T) {
	h, fg := skipHarness(t, docsGlobs...)
	fg.set(2, "docs/a.md")
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()
	h.wantState(2, store.PRIneligible)

	if _, err := h.e.requestReview(h.ctx, ReviewPayload{PRTarget: PRTarget{Ref: "2"}}); err != nil {
		t.Fatalf("requestReview: %v", err)
	}
	pr := h.wantState(2, store.PRQueued)
	if !pr.Forced {
		t.Fatal("PR is not forced")
	}
	h.advance(time.Minute)
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()
	pr = h.wantState(2, store.PRReviewed)
	if deref(pr.ReviewedSHA) != "s1" {
		t.Fatalf("reviewed_sha = %q, want s1", deref(pr.ReviewedSHA))
	}
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want 1", n)
	}
}

// A waiting PR the cache says to skip is turned away at dispatch, so a list
// that arrived after the PR was queued still counts.
func TestSkipPathsDispatchReclassifies(t *testing.T) {
	h, fg := skipHarness(t, docsGlobs...)
	fg.set(2, "src/app.go")
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()
	pr := h.wantState(2, store.PRQueued)

	b, err := json.Marshal(changedFiles{Head: "s1", Complete: true, Files: []string{"docs/a.md", "b.md"}})
	if err != nil {
		t.Fatal(err)
	}
	h.e.setKV(h.ctx, kvPRFiles(pr.ID), string(b))
	h.advance(10 * time.Minute)
	h.tick()
	pr = h.wantState(2, store.PRIneligible)
	wantSkipReason(t, pr, "skip_paths: all 2 changed files match")
	if n := len(h.rd.all()); n != 0 {
		t.Fatalf("rounds = %d, want 0", n)
	}
}

// Without skip_paths, or with a client that cannot list files, nothing is
// fetched and nothing is skipped.
func TestSkipPathsOffOrUnsupportedNeverSkips(t *testing.T) {
	h, fg := skipHarness(t) // no globs
	fg.set(2, "docs/a.md")
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()
	h.wantState(2, store.PRQueued)
	if n := fg.fetches(-1); n != 0 {
		t.Fatalf("ListFiles called %d times for a watch without skip_paths", n)
	}

	plain := newHarness(t, func(h *harness) { h.cfg.Watches[0].SkipPaths = docsGlobs })
	plain.open(prSpec{n: 1, head: "base1"})
	plain.startup()
	plain.tick()
	plain.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "s1"})
	plain.tick()
	pr := plain.wantState(2, store.PRQueued)
	if _, ok := plain.e.cachedFiles(plain.ctx, pr.ID); ok {
		t.Fatal("a client without ListFiles cached a list")
	}
}

// A reviewed PR whose push only touches skipped paths is not queued again.
func TestSkipPathsPushOfSkippedFilesToReviewedPR(t *testing.T) {
	h, fg := skipHarness(t, docsGlobs...)
	fg.set(2, "src/app.go")
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)

	fg.set(2, "docs/a.md", "src/app.go") // still has code: queued again
	h.advance(time.Minute)
	h.openWith(prSpec{n: 2, head: "s2"})
	h.tick()
	h.wantState(2, store.PRRereviewPending)

	fg.set(2, "docs/a.md")
	h.advance(time.Minute)
	h.openWith(prSpec{n: 2, head: "s3"})
	h.tick()
	wantSkipReason(t, h.wantState(2, store.PRIneligible), "skip_paths: all 1 changed files match")
}

// pathSkipReason only trusts a complete list of the current head.
func TestPathSkipReasonReadsOnlyAValidCache(t *testing.T) {
	h, _ := skipHarness(t, docsGlobs...)
	h.openWith(prSpec{n: 2, head: "s1"})
	h.tick()
	pr := h.pr(2)
	w := *h.cfg.WatchFor("talkable/talkable")
	put := func(v string) { h.e.setKV(h.ctx, kvPRFiles(pr.ID), v) }
	put(`{"head":"s1","complete":true,"files":["docs/a.md"]}`)
	if got := h.e.pathSkipReason(h.ctx, w, pr); got != "skip_paths: all 1 changed files match" {
		t.Fatalf("matching cache: %q", got)
	}
	for name, v := range map[string]string{
		"other head":   `{"head":"s0","complete":true,"files":["docs/a.md"]}`,
		"incomplete":   `{"head":"s1","complete":false,"files":["docs/a.md"]}`,
		"code file":    `{"head":"s1","complete":true,"files":["docs/a.md","a.go"]}`,
		"no files":     `{"head":"s1","complete":true,"files":[]}`,
		"unreadable":   `not json`,
		"empty object": `{}`,
	} {
		put(v)
		if got := h.e.pathSkipReason(h.ctx, w, pr); got != "" {
			t.Errorf("%s: reason %q, want none", name, got)
		}
	}
	h.e.delKV(h.ctx, kvPRFiles(pr.ID))
	if got := h.e.pathSkipReason(h.ctx, w, pr); got != "" {
		t.Errorf("no cache: reason %q", got)
	}
	put(`{"head":"s1","complete":true,"files":["docs/a.md"]}`)
	w.SkipPaths = nil
	if got := h.e.pathSkipReason(h.ctx, w, pr); got != "" {
		t.Errorf("watch without skip_paths: reason %q", got)
	}
}

// The per-repository budget bounds the fetches of one poll; the rest follow.
func TestSkipPathsFetchBudgetPerPoll(t *testing.T) {
	h, fg := skipHarness(t, docsGlobs...)
	n := maxFileFetchesPerRepo + 5
	specs := make([]prSpec, 0, n)
	for i := range n {
		fg.set(10+i, "src/a.go")
		specs = append(specs, prSpec{n: 10 + i, head: fmt.Sprintf("h%d", i)})
	}
	h.openWith(specs...)
	h.tick()
	if got := fg.fetches(-1); got != maxFileFetchesPerRepo {
		t.Fatalf("fetches in one poll = %d, want the budget %d", got, maxFileFetchesPerRepo)
	}
	h.advance(time.Minute)
	h.openWith(specs...)
	h.tick()
	if got := fg.fetches(-1); got != n {
		t.Fatalf("fetches after the next poll = %d, want %d", got, n)
	}
}

// classify keeps the reason "ignored" for PRs `magnum ignore` muted, and says
// "muted" for any other muted PR; both survive a push.
func TestClassifyIgnoredKeepsItsReason(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	pr := h.pr(2)
	w := *h.cfg.WatchFor("talkable/talkable")

	if dec := h.e.classify(h.ctx, w, pr, h.clock.Now()); !dec.Eligible {
		t.Fatalf("plain PR: %+v", dec)
	}
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("muted", true) }); err != nil {
		t.Fatal(err)
	}
	if dec := h.e.classify(h.ctx, w, h.pr(2), h.clock.Now()); dec.Eligible || dec.Reason != "muted" {
		t.Fatalf("muted PR: %+v, want reason muted", dec)
	}
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("skip_reason", skipIgnored) }); err != nil {
		t.Fatal(err)
	}
	if dec := h.e.classify(h.ctx, w, h.pr(2), h.clock.Now()); dec.Eligible || dec.Reason != skipIgnored {
		t.Fatalf("ignored PR: %+v, want reason %q", dec, skipIgnored)
	}

	// A push to the ignored PR: ineligible with the reason "ignored".
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	pr = h.wantState(2, store.PRIneligible)
	wantSkipReason(t, pr, skipIgnored)
}

func TestClassifyPlainMutedKeepsMuted(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	pr := h.pr(2)
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("muted", true) }); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	wantSkipReason(t, h.wantState(2, store.PRIneligible), "muted")
}

// A PR the watch's other filters reject (a skipped author, a muted PR) costs
// no file list, even when new or pushed to.
func TestSkipPathsNoFetchForPRsRejectedOtherwise(t *testing.T) {
	h, fg := skipHarness(t, docsGlobs...)
	fg.set(2, "docs/a.md")
	fg.set(3, "src/a.go")
	h.openWith(prSpec{n: 2, head: "s1", author: "dependabot"}, prSpec{n: 3, head: "t1"})
	h.tick()
	wantSkipReason(t, h.wantState(2, store.PRIneligible), `author "dependabot" is in skip_authors`)
	if n := fg.fetches(2); n != 0 {
		t.Fatalf("listed the files of a skipped author's PR %d times", n)
	}
	pr3 := h.wantState(3, store.PRQueued)
	if err := h.st.UpdatePR(h.ctx, pr3.ID, func(u *store.PRUpdate) { u.Set("muted", true) }); err != nil {
		t.Fatal(err)
	}
	h.openWith(prSpec{n: 2, head: "s2", author: "dependabot"}, prSpec{n: 3, head: "t2"})
	h.tick()
	if n2, n3 := fg.fetches(2), fg.fetches(3); n2 != 0 || n3 != 1 {
		t.Fatalf("fetches after a push: #2 %d (want 0), #3 %d (want 1: muted since)", n2, n3)
	}
}
