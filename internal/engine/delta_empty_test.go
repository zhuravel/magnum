package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// The live case (an author's empty commit to re-run CI after a posted
// review): GitHub compares the reviewed commit with the head as "ahead",
// one commit, no file. The head's tree is the reviewed commit's tree, so
// the push is the class empty: the review stands for the new head, nothing
// is queued.

// pushedEmpty reviews PR #2 at b1 and polls its head b2, which GitHub
// compares with b1 as status, commits commits and no file.
func pushedEmpty(t *testing.T, h *harness, status string, commits int) store.PR {
	t.Helper()
	h.reviewedPR(2, "b1")
	h.gh.mu.Lock()
	h.gh.files = map[string][]github.FileDelta{"b1...b2": {}}
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: commits}
	if status != "" {
		h.gh.statuses = map[string]string{"b1...b2": status}
	}
	h.gh.mu.Unlock()
	pollPR(h, time.Minute, 2, "b2")
	return h.pr(2)
}

func TestAnEmptyCommitAfterAReviewIsNotReReviewed(t *testing.T) {
	h := newHarness(t)
	pr := pushedEmpty(t, h, "ahead", 1)
	if pr.State != store.PRReviewed || deref(pr.ReviewedSHA) != "b2" || deref(pr.LastReviewID) != 101 {
		t.Fatalf("after an empty commit: state %s reviewed_sha %q review %v, want the review settled on b2", pr.State, deref(pr.ReviewedSHA), pr.LastReviewID)
	}
	evs := trivialEvents(t, h)
	want := "reviewed → reviewed: the push to b2 changes no file (1 commit) since the review of b1; no re-review, the review stands"
	if len(evs) != 1 || evs[0].Message != want {
		t.Fatalf("pr.trivial_delta events: %+v\nwant %q", evs, want)
	}
	var data struct {
		Classes  []string
		Files    int
		From, To string
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || !slices.Equal(data.Classes, []string{DeltaEmpty}) || data.Files != 0 || data.From != "b1" || data.To != "b2" {
		t.Fatalf("event data %s (%v)", evs[0].Data, err)
	}
	v, _ := kvValue(h, KVPRTrivial(pr.ID))
	if skip, ok := ParseTrivialSkip(v); !ok || skip.Note() != "empty-commit push skipped (b1 → b2)" {
		t.Fatalf("%s = %q", KVPRTrivial(pr.ID), v)
	}
	if rec, ok := h.e.deltaRecord(h.ctx, pr.ID); ok {
		t.Fatalf("delta record %+v for a push the review stands for", rec)
	}
	// Nothing is queued: an hour later no second round ran.
	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want only the first", n)
	}
}

// The App's approval of the reviewed commit still describes the head: an
// empty commit keeps it, as any trivial push does.
func TestAnEmptyCommitKeepsTheAppApproval(t *testing.T) {
	h, app := newApprovalHarness(t)
	approving(h, "APPROVED")
	pr := pushedEmpty(t, h, "ahead", 1)
	if pr.State != store.PRReviewed || deref(pr.LastReviewEvent) != "APPROVED" || deref(pr.ReviewedSHA) != "b2" {
		t.Fatalf("state %s event %q reviewed_sha %q", pr.State, deref(pr.LastReviewEvent), deref(pr.ReviewedSHA))
	}
	if n := len(callsWith(h.gh, "dismiss:")); n != 0 || len(app.comparesMade()) != 0 {
		t.Fatalf("dismissals %d, approval compares %v", n, app.comparesMade())
	}
}

// A watch whose skip_trivial_deltas leaves out empty re-reviews such a push
// as before: the threshold holds it as a 0-line delta.
func TestAnEmptyCommitIsReReviewedWhenTheWatchLeavesOutTheClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(h *harness)
	}{
		{"watch", func(h *harness) {
			for i := range h.cfg.Watches {
				h.cfg.Watches[i].SkipTrivialDeltas = []string{DeltaComments, DeltaWhitespace, DeltaDocs, DeltaBase}
			}
		}},
		{"daemon", func(h *harness) {
			h.cfg.Daemon.SkipTrivialDeltas = []string{DeltaComments, DeltaWhitespace, DeltaDocs, DeltaBase}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mod)
			pr := pushedEmpty(t, h, "ahead", 1)
			if pr.State != store.PRRereviewPending || deref(pr.ReviewedSHA) != "b1" || len(trivialEvents(t, h)) != 0 {
				t.Fatalf("state %s reviewed_sha %q, want a re-review of b2", pr.State, deref(pr.ReviewedSHA))
			}
			if _, ok := kvValue(h, KVPRTrivial(pr.ID)); ok {
				t.Fatal("trivial note recorded for a push that is re-reviewed")
			}
			rec, ok := h.e.deltaRecord(h.ctx, pr.ID)
			if !ok || rec.Lines != 0 || rec.AddedFiles != 0 || !rec.Complete || rec.To != "b2" {
				t.Fatalf("delta record %+v (%v), want the measured 0-line delta", rec, ok)
			}
			if w := waitOf(t, h, 2); w.Reason != WaitDelta {
				t.Fatalf("wait = %+v, want the small delta's hold", w)
			}
		})
	}
}

// Only a head GitHub finds ahead of the reviewed commit has the reviewed
// tree: behind (a force push back to an ancestor) and a range with no
// commit measure nothing and keep their re-review.
func TestAnEmptyRangeThatIsNotAheadKeepsItsReReview(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  string
		commits int
	}{
		{"behind", "behind", 1},
		{"behind with no commit", "behind", 0},
		{"ahead with no commit", "ahead", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			pr := pushedEmpty(t, h, tc.status, tc.commits)
			if pr.State != store.PRRereviewPending || deref(pr.ReviewedSHA) != "b1" || len(trivialEvents(t, h)) != 0 {
				t.Fatalf("state %s reviewed_sha %q, want a re-review of b2", pr.State, deref(pr.ReviewedSHA))
			}
			if rec, ok := h.e.deltaRecord(h.ctx, pr.ID); ok {
				t.Fatalf("delta record %+v for a push GitHub cannot measure", rec)
			}
		})
	}
}

// A diverged range (a rebase, a force push) lists the merge base's changes:
// no file does not mean the reviewed tree. It goes the way of the base
// merge logic: an own diff that cannot be read is re-reviewed, one that is
// unchanged is the class base, not empty.
func TestADivergedRangeWithNoFileIsNotEmpty(t *testing.T) {
	t.Run("own diff unreadable", func(t *testing.T) {
		h := newHarness(t)
		pr := pushedEmpty(t, h, "diverged", 2)
		if pr.State != store.PRRereviewPending || deref(pr.ReviewedSHA) != "b1" || len(trivialEvents(t, h)) != 0 {
			t.Fatalf("state %s reviewed_sha %q, want a re-review of b2", pr.State, deref(pr.ReviewedSHA))
		}
	})
	t.Run("own diff unchanged", func(t *testing.T) {
		h := newHarness(t)
		h.reviewedPR(2, "b1")
		h.gh.mu.Lock()
		h.gh.files = map[string][]github.FileDelta{"b1...b2": {}, "master...b1": ownBefore, "master...b2": ownAfterMerge}
		h.gh.compare["b1...b2"] = github.CompareStats{Commits: 2}
		h.gh.statuses = map[string]string{"b1...b2": "diverged"}
		h.gh.mu.Unlock()
		pollPR(h, time.Minute, 2, "b2")
		pr := h.pr(2)
		if pr.State != store.PRReviewed || deref(pr.ReviewedSHA) != "b2" {
			t.Fatalf("state %s reviewed_sha %q, want the base-merge settle", pr.State, deref(pr.ReviewedSHA))
		}
		evs := trivialEvents(t, h)
		var data struct{ Classes []string }
		if len(evs) != 1 || json.Unmarshal(evs[0].Data, &data) != nil || !slices.Equal(data.Classes, []string{DeltaBase}) {
			t.Fatalf("pr.trivial_delta events: %+v, want the class base", evs)
		}
	})
}

// An empty commit that arrives while the judge works settles the same way:
// the note on the review says no file changed and no re-review is needed.
func TestAnEmptyCommitDuringTheReviewNeedsNoReReview(t *testing.T) {
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
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 1}
	h.gh.files = map[string][]github.FileDelta{"b1...b2": {}}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.wantState(2, store.PRReviewing)
	close(h.rd.gate)
	h.settle()

	if got := h.rd.appended(); len(got) != 1 || got[0] != "101:_Reviewed b1; 1 commit arrived during the review (no file changes), no re-review needed._" {
		t.Fatalf("review notes = %q", got)
	}
	pr := h.wantState(2, store.PRReviewed)
	if deref(pr.ReviewedSHA) != "b2" || deref(pr.LastReviewID) != 101 {
		t.Fatalf("reviewed_sha %q review %v", deref(pr.ReviewedSHA), pr.LastReviewID)
	}
	if evs := trivialEvents(t, h); len(evs) != 1 || !strings.Contains(evs[0].Message, "during the review changes no file (1 commit)") {
		t.Fatalf("pr.trivial_delta events: %+v", evs)
	}
	h.advance(time.Hour)
	h.tick()
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want no re-review", n)
	}
}

func TestTheLabelOfAnEmptyPushNamesNoFileChanges(t *testing.T) {
	if got := DeltaLabel([]string{DeltaEmpty}); got != "no file changes" {
		t.Fatalf("DeltaLabel(empty) = %q", got)
	}
	if got := (deltaCheck{classes: []string{DeltaEmpty}, commits: 3}).change(); got != "changes no file (3 commits)" {
		t.Fatalf("change() = %q", got)
	}
}
