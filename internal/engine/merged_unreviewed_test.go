package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

const (
	muReviewedHead = "1111111aaaaaaaa" // what magnum reviewed
	muMergedHead   = "2222222bbbbbbbb" // the push it never got to review
)

// closeOnGitHub takes PR #n off the open list (keep stays open), has GitHub
// report it as state and ticks until the engine confirmed the close twice
// and moved the PR to closed.
func (h *harness) closeOnGitHub(n int, state string, keep ...prSpec) {
	h.t.Helper()
	h.open(keep...)
	h.gh.closed[n] = state
	h.tick() // first confirmation
	h.advance(30 * time.Second)
	h.tick() // second, but not a minute after the first
	h.advance(30 * time.Second)
	h.tick()
	h.wantState(n, store.PRClosed)
}

// mergedUnreviewedEvents lists the pr.merged_unreviewed events of PR #n.
func (h *harness) mergedUnreviewedEvents(n int) []store.Event {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#"+itoa(int64(n)), 0)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range evs {
		if ev.Kind == "pr.merged_unreviewed" {
			out = append(out, ev)
		}
	}
	return out
}

// mergedUnreviewedToasts lists the "title | body" toasts about merged,
// unreviewed PRs.
func (h *harness) mergedUnreviewedToasts() []string {
	h.t.Helper()
	var out []string
	for _, t := range h.nh.all() {
		if strings.Contains(t, "merged unreviewed") {
			out = append(out, t)
		}
	}
	return out
}

// reviewedThenPushed returns PR #2 reviewed at muReviewedHead with a push
// (muMergedHead) waiting for its re-review.
func (h *harness) reviewedThenPushed() store.PR {
	h.t.Helper()
	h.reviewedPR(2, muReviewedHead)
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: muMergedHead})
	h.tick()
	return h.wantState(2, store.PRRereviewPending)
}

// TestMergedBeforeItsReReviewIsFlaggedOnceWithAWarningAndAToast: GitHub
// merged a PR whose last push magnum had not reviewed yet: one warn event
// naming both commits and one toast, and neither repeats on later ticks or
// through the release of the slot.
func TestMergedBeforeItsReReviewIsFlaggedOnceWithAWarningAndAToast(t *testing.T) {
	h := newHarness(t)
	h.reviewedThenPushed()

	h.advance(time.Minute)
	h.closeOnGitHub(2, "MERGED", prSpec{n: 1, head: "base1"})

	evs := h.mergedUnreviewedEvents(2)
	if len(evs) != 1 {
		t.Fatalf("pr.merged_unreviewed events = %d, want 1", len(evs))
	}
	if evs[0].Level != "warn" || evs[0].Message != "merged before magnum reviewed 2222222 (last review 1111111)" {
		t.Fatalf("event: %s %q", evs[0].Level, evs[0].Message)
	}
	for _, want := range []string{`"head_sha":"` + muMergedHead + `"`, `"reviewed_sha":"` + muReviewedHead + `"`, `"prev_state":"rereview_pending"`} {
		if !strings.Contains(string(evs[0].Data), want) {
			t.Errorf("event data %s lacks %s", evs[0].Data, want)
		}
	}
	toasts := h.mergedUnreviewedToasts()
	if len(toasts) != 1 {
		t.Fatalf("toasts = %q, want 1", toasts)
	}
	if want := "magnum: talkable#2 merged unreviewed | talkable#2 merged before magnum reviewed its last push: merged head 2222222, last review 1111111"; toasts[0] != want {
		t.Fatalf("toast = %q\nwant    %q", toasts[0], want)
	}

	for range 3 {
		h.advance(time.Minute)
		h.tick()
	}
	h.advance(10 * time.Minute) // past the close grace
	h.tick()
	h.wantState(2, store.PRReleased)
	if n := len(h.mergedUnreviewedEvents(2)); n != 1 {
		t.Fatalf("pr.merged_unreviewed events after the release = %d, want 1", n)
	}
	if got := h.mergedUnreviewedToasts(); len(got) != 1 {
		t.Fatalf("toasts after the release = %q, want 1", got)
	}
}

// TestMergedPRThatWasNeverReviewedSaysSo: a queued PR that merged before its
// first round has no reviewed commit to name.
func TestMergedPRThatWasNeverReviewedSaysSo(t *testing.T) {
	h := newHarness(t)
	h.queuedPR(2, muMergedHead)

	h.closeOnGitHub(2, "MERGED", prSpec{n: 1, head: "base1"})

	evs := h.mergedUnreviewedEvents(2)
	if len(evs) != 1 || evs[0].Message != "merged before magnum reviewed 2222222 (never reviewed)" {
		t.Fatalf("events: %+v", evs)
	}
	toasts := h.mergedUnreviewedToasts()
	if len(toasts) != 1 || !strings.HasSuffix(toasts[0], "merged head 2222222, never reviewed") {
		t.Fatalf("toasts: %q", toasts)
	}
}

// TestMergedBaselinePRIsNotFlagged: a PR that was already open when magnum
// first saw the repository is never reviewed on purpose, so merging it is
// not a miss.
func TestMergedBaselinePRIsNotFlagged(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: muMergedHead})
	h.startup()
	h.tick()
	h.wantState(2, store.PRBaseline)

	h.closeOnGitHub(2, "MERGED", prSpec{n: 1, head: "base1"})

	if n := len(h.mergedUnreviewedEvents(2)); n != 0 {
		t.Fatalf("pr.merged_unreviewed events = %d, want 0", n)
	}
	if got := h.mergedUnreviewedToasts(); len(got) != 0 {
		t.Fatalf("toasts = %q, want none", got)
	}
}

// TestMergedPRWhoseHeadWasReviewedIsNotFlagged: the last push was reviewed
// before GitHub merged the PR.
func TestMergedPRWhoseHeadWasReviewedIsNotFlagged(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, muReviewedHead)
	if deref(pr.ReviewedSHA) != pr.HeadSHA {
		t.Fatalf("the PR must be reviewed at its head: reviewed %q head %q", deref(pr.ReviewedSHA), pr.HeadSHA)
	}

	h.advance(time.Minute)
	h.closeOnGitHub(2, "MERGED", prSpec{n: 1, head: "base1"})

	if n := len(h.mergedUnreviewedEvents(2)); n != 0 {
		t.Fatalf("pr.merged_unreviewed events = %d, want 0", n)
	}
	if got := h.mergedUnreviewedToasts(); len(got) != 0 {
		t.Fatalf("toasts = %q, want none", got)
	}
}

// TestClosedWithoutMergeIsNotFlaggedAsMergedUnreviewed: a PR closed on
// GitHub with its last push unreviewed was abandoned, not merged.
func TestClosedWithoutMergeIsNotFlaggedAsMergedUnreviewed(t *testing.T) {
	h := newHarness(t)
	h.reviewedThenPushed()

	h.advance(time.Minute)
	h.closeOnGitHub(2, "CLOSED", prSpec{n: 1, head: "base1"})

	if n := len(h.mergedUnreviewedEvents(2)); n != 0 {
		t.Fatalf("pr.merged_unreviewed events = %d, want 0", n)
	}
	if got := h.mergedUnreviewedToasts(); len(got) != 0 {
		t.Fatalf("toasts = %q, want none", got)
	}
}
