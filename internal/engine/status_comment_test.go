package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// appGitHub gives the talkable watch's posting identity, talkable-app, the
// fake GitHub (the harness answers only for zhuravel), so the status
// comment can go out as it.
func appGitHub(h *harness) {
	live := h.d.GitHub
	h.d.GitHub = func(id string) GitHub {
		if id == "talkable-app" {
			return h.gh
		}
		return live(id)
	}
}

// statusOn switches the review status comment on ([pipeline]
// status_comment) for a harness whose posting identity has a GitHub.
func statusOn(h *harness) {
	h.cfg.Pipeline.StatusComment = true
	appGitHub(h)
}

// statusComment is the one comment standing on PR #n.
func (h *harness) statusComment(n int) fakeComment {
	h.t.Helper()
	cs := h.gh.commentsOn(n)
	if len(cs) != 1 {
		h.t.Fatalf("PR #%d has %d comments, want 1: %+v", n, len(cs), cs)
	}
	return cs[0]
}

// wantStatus checks that comment c reads the status header, its status
// changed now, and then body.
func (h *harness) wantStatus(c fakeComment, body string) {
	h.t.Helper()
	h.wantStatusSince(c, h.clock.Now(), body)
}

// wantStatusSince checks that comment c reads the status header, its status
// changed at changed, and then body.
func (h *harness) wantStatusSince(c fakeComment, changed time.Time, body string) {
	h.t.Helper()
	if want := statusHeader(changed) + body; c.body != want {
		h.t.Fatalf("status comment:\n%s\nwant:\n%s", c.body, want)
	}
}

// statusHeader is the status comment's marker and header, its status
// changed at t.
func statusHeader(t time.Time) string {
	return "<!-- magnum:status -->\n**Magnum review status** (status changed " + t.UTC().Format("2 Jan, 15:04") + " UTC)\n"
}

// utcClock is t as the status comment writes a time of the harness's day.
func utcClock(t time.Time) string { return t.UTC().Format("15:04") + " UTC" }

// reviewURL is the link of review id on PR #n.
func reviewURL(n int, id int64) string {
	return "https://github.com/talkable/talkable/pull/" + itoa(int64(n)) + "#pullrequestreview-" + itoa(id)
}

const (
	statusHead  = "a1b2c3d4e5f6a7b8"
	statusPush  = "b2c3d4e5f6a7b8c9"
	statusThird = "c3d4e5f6a7b8c9d0"
	statusMerge = "A merge before the review posts skips it."
	statusSelf  = "This comment updates by itself."
)

// statusReviewed runs PR #7 at statusHead to its posted review, with the
// comment collapsed to the posted line, and returns the PR.
func (h *harness) statusReviewed() store.PR {
	h.t.Helper()
	h.queuedPR(7, statusHead)
	h.advance(5 * time.Minute)
	h.tick() // the round runs and posts
	h.advance(2 * time.Minute)
	h.tick()
	pr := h.wantState(7, store.PRReviewed)
	if c := h.statusComment(7); !strings.Contains(c.body, "\nReview of `a1b2c3d` posted ") {
		h.t.Fatalf("after the review:\n%s", c.body)
	}
	return pr
}

// A PR that waits out the push quiet period gets one comment, as the
// watch's posting identity, that leads with its head and the earliest start
// in UTC, then the fixed sentences and the operator's note; the next tick,
// with the same wait, edits nothing.
func TestAScheduledReviewPostsAStatusComment(t *testing.T) {
	h := newHarness(t, statusOn)
	h.cfg.Pipeline.StatusCommentNote = "Need it sooner? Ping me on Slack."
	start := h.clock.Now()
	pr := h.queuedPR(7, statusHead)

	c := h.statusComment(7)
	h.wantStatus(c, "- [x] Review queued for `a1b2c3d`: not before "+utcClock(start.Add(5*time.Minute))+" (the push quiet period)\n"+
		"- [ ] In progress\n"+
		"- [ ] Posted\n"+
		"\n"+statusSelf+"\n"+statusMerge+"\n"+
		"Need it sooner? Ping me on Slack.")
	if calls := h.gh.count("issue_comment:talkable/talkable#7"); calls != 1 {
		t.Fatalf("comment calls = %d, want 1", calls)
	}
	if v, _ := h.e.getKV(h.ctx, kvPRStatusComment(pr.ID)); !strings.Contains(v, `"id":`+itoa(c.id)+`,`) {
		t.Fatalf("%s = %s, want the comment's id", kvPRStatusComment(pr.ID), v)
	}
	if evs := h.events("pr.status_comment"); len(evs) != 1 || evs[0].Level != "info" ||
		evs[0].Message != "status comment posted: scheduled (review of a1b2c3d)" {
		t.Fatalf("pr.status_comment events = %+v", evs)
	}

	for range 3 {
		h.advance(30 * time.Second)
		h.tick()
	}
	h.wantState(7, store.PRQueued)
	if n := h.gh.count("edit_issue_comment:"); n != 0 {
		t.Fatalf("the same wait edited the comment %d times", n)
	}
	if n := len(h.gh.commentsOn(7)); n != 1 {
		t.Fatalf("comments = %d, want 1", n)
	}
}

// The round's start ticks "In progress" with its time, the posted review
// collapses the list to its line, and a push starts the list again, as a
// re-review, on the same comment.
func TestTheStatusCommentFollowsTheRound(t *testing.T) {
	h := newHarness(t, statusOn)
	h.queuedPR(7, statusHead)
	first := h.statusComment(7)

	h.advance(5 * time.Minute) // the quiet period is over
	h.holdRounds()
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	h.advance(2 * time.Minute)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	pr := h.wantState(7, store.PRReviewing)
	c := h.statusComment(7)
	if c.id != first.id {
		t.Fatalf("the round posted comment %d, want %d edited", c.id, first.id)
	}
	h.wantStatus(c, "- [x] Review queued for `a1b2c3d`\n- [x] In progress since "+utcClock(*pr.LastRoundStartedAt)+"\n- [ ] Posted\n\n"+
		statusSelf+"\n"+statusMerge)

	h.releaseRound()
	pr = h.wantState(7, store.PRReviewed)
	h.advance(2 * time.Minute)
	h.tick()
	c = h.statusComment(7)
	h.wantStatus(c, "Review of `a1b2c3d` posted "+utcClock(*pr.ReviewedAt)+": "+reviewURL(7, *pr.LastReviewID))

	h.advance(3 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusPush})
	h.tick()
	h.wantState(7, store.PRRereviewPending)
	c = h.statusComment(7)
	if c.id != first.id {
		t.Fatalf("the push posted comment %d, want %d edited", c.id, first.id)
	}
	if !strings.Contains(c.body, "\n- [x] Re-review queued for `b2c3d4e`: not before ") ||
		!strings.Contains(c.body, "\n- [ ] In progress\n- [ ] Posted\n\nEarlier review of `a1b2c3d`: "+reviewURL(7, *pr.LastReviewID)+"\n\n"+statusSelf+"\n"+statusMerge) {
		t.Fatalf("after the push:\n%s", c.body)
	}
	var stages []string
	for _, ev := range h.events("pr.status_comment") {
		stages = append(stages, ev.Message)
	}
	want := []string{"status comment posted: scheduled (review of a1b2c3d)", "status comment updated: in progress (review of a1b2c3d)",
		"status comment updated: posted (review of a1b2c3d)", "status comment updated: scheduled (re-review of b2c3d4e)"}
	if strings.Join(stages, "|") != strings.Join(want, "|") {
		t.Fatalf("pr.status_comment events = %q, want %q", stages, want)
	}
}

// The first line says where the review stands in plain words: a review
// held by the others running says "other reviews are running", a timed
// wait names its earliest start ("not before"), never a promised one. The
// header carries the day and moves only with the stage or the wait: an
// edit of the operator's note leaves it.
func TestTheStatusCommentLeadsWithTheState(t *testing.T) {
	h := newHarness(t, statusOn, func(h *harness) { h.cfg.Daemon.MaxConcurrentReviews = 1 })
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	start := h.clock.Now()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusHead}, prSpec{n: 8, head: statusPush})
	h.tick()
	h.wantStatus(h.statusComment(7), "- [x] Review queued for `a1b2c3d`: not before "+utcClock(start.Add(5*time.Minute))+" (the push quiet period)\n"+
		"- [ ] In progress\n- [ ] Posted\n\n"+statusSelf+"\n"+statusMerge)

	h.advance(5 * time.Minute)
	h.rd.gate = make(chan struct{})
	defer func() { close(h.rd.gate); h.settle() }()
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	n, sha := 8, "b2c3d4e"
	if h.pr(8).State != store.PRQueued {
		n, sha = 7, "a1b2c3d"
	}
	if w := waitOf(t, h, n); w.Reason != WaitCapacity {
		t.Fatalf("#%d waits for %+v, want capacity", n, w)
	}
	held := h.clock.Now()
	h.wantStatus(h.statusComment(n), "- [x] Review queued for `"+sha+"`: other reviews are running\n- [ ] In progress\n- [ ] Posted\n\n"+
		statusSelf+"\n"+statusMerge)

	h.cfg.Pipeline.StatusCommentNote = "Reviews by magnum."
	h.advance(3 * time.Minute)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.wantStatusSince(h.statusComment(n), held, "- [x] Review queued for `"+sha+"`: other reviews are running\n- [ ] In progress\n- [ ] Posted\n\n"+
		statusSelf+"\n"+statusMerge+"\nReviews by magnum.")
}

// A posted review collapses the comment to one line with its time and
// link, without the checklist, the fixed sentences or the operator's note;
// the comment stays, and the next push starts the list again in it.
func TestAPostedReviewCollapsesTheStatusComment(t *testing.T) {
	h := newHarness(t, statusOn)
	h.cfg.Pipeline.StatusCommentNote = "Reviews by magnum."
	h.queuedPR(7, statusHead)
	first := h.statusComment(7)
	h.advance(5 * time.Minute)
	h.tick() // the round runs and posts
	h.advance(2 * time.Minute)
	h.tick()
	pr := h.wantState(7, store.PRReviewed)
	c := h.statusComment(7)
	h.wantStatus(c, "Review of `a1b2c3d` posted "+utcClock(*pr.ReviewedAt)+": "+reviewURL(7, *pr.LastReviewID))
	if c.id != first.id {
		t.Fatalf("the review posted comment %d, want %d edited", c.id, first.id)
	}

	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusPush})
	h.tick()
	c = h.statusComment(7)
	if c.id != first.id || !strings.HasPrefix(c.body, statusHeader(h.clock.Now())+"- [x] Re-review queued for `b2c3d4e`: ") ||
		!strings.HasSuffix(c.body, "\n\n"+statusSelf+"\n"+statusMerge+"\nReviews by magnum.") {
		t.Fatalf("after the push (comment %d):\n%s", c.id, c.body)
	}
}

// A re-review's list is followed by the latest earlier review's link,
// labelled as the earlier head's, and the earlier findings that review
// recorded still open, worded as its record; a later re-review shows only
// the review before it, and leaves the count out when its record is not
// known.
func TestAReReviewShowsTheEarlierReviewLink(t *testing.T) {
	h := newHarness(t, statusOn)
	pr := h.statusReviewed()
	if _, err := h.st.DB().ExecContext(h.ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login,
  state, outcome, review_id, review_url, prompt_text, result_json, created_at)
  VALUES ('run-status-1', ?, 1, 'codex-judge', 'initial', ?, 'talkable-app', 'talkable[bot]', 'verified', 'posted', ?, ?, 'p', ?, ?)`,
		pr.ID, statusHead, *pr.LastReviewID, reviewURL(7, *pr.LastReviewID),
		`{"event":"COMMENTED","findings":{"P2":1},"previous_findings":{"fixed":1,"open":2}}`, store.FormatTime(h.clock.Now())); err != nil {
		t.Fatal(err)
	}

	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusPush})
	h.tick()
	h.wantState(7, store.PRRereviewPending)
	c := h.statusComment(7)
	if !strings.Contains(c.body, "\n- [ ] Posted\n\nEarlier review of `a1b2c3d`: "+reviewURL(7, *pr.LastReviewID)+"\n"+
		"The last review recorded 2 earlier findings still open.\n\n"+statusSelf+"\n") {
		t.Fatalf("the re-review:\n%s", c.body)
	}

	h.advance(time.Hour)
	h.tick() // the re-review runs and posts
	second := h.wantState(7, store.PRReviewed)
	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusThird})
	h.tick()
	c = h.statusComment(7)
	if !strings.Contains(c.body, "\n- [x] Re-review queued for `c3d4e5f`: ") ||
		!strings.Contains(c.body, "\n- [ ] Posted\n\nEarlier review of `b2c3d4e`: "+reviewURL(7, *second.LastReviewID)+"\n\n"+statusSelf+"\n") ||
		strings.Contains(c.body, "a1b2c3d") || strings.Contains(c.body, "still open") {
		t.Fatalf("the next re-review:\n%s", c.body)
	}
}

// A muted PR's comment says, in one line and without the cause, that
// magnum will not review its head; the unmute schedules it again.
func TestAMutedPRsStatusCommentSaysNotReviewing(t *testing.T) {
	h := newHarness(t, statusOn)
	h.queuedPR(7, statusHead)

	h.advance(2 * time.Minute)
	h.muteRequest(ReqMute, 7)
	h.wantState(7, store.PRIneligible)
	h.wantStatus(h.statusComment(7), "Magnum will not review this head. The operator was told.")

	h.advance(2 * time.Minute) // still within the push quiet period
	h.muteRequest(ReqUnmute, 7)
	h.wantState(7, store.PRQueued)
	c := h.statusComment(7)
	if !strings.Contains(c.body, "\n- [x] Review queued for `a1b2c3d`: ") {
		t.Fatalf("after the unmute:\n%s", c.body)
	}
}

// Every comment ends on a true line, written once and at once (the 2-minute
// spacing does not hold it): a PR closed without a merge before its review
// posted says so, one merged before it says so, one merged after it keeps
// the posted line, and a PR that turned into a draft says it is not
// reviewed until it is ready again. A tick that sees the PR released
// writes a last edit GitHub refused while it was closed.
func TestAClosedPRsStatusCommentEndsTruthfully(t *testing.T) {
	keep := prSpec{n: 1, head: "base1"}
	// wantEnded ticks past the close grace (released) and checks that the
	// comment on #7 was not edited again.
	wantEnded := func(t *testing.T, h *harness, body string) {
		t.Helper()
		ended := h.statusComment(7)
		for range 3 {
			h.advance(5 * time.Minute)
			h.tick()
		}
		h.wantState(7, store.PRReleased)
		if c := h.statusComment(7); c.edits != ended.edits || c.body != ended.body || !strings.HasSuffix(c.body, ")\n"+body) {
			t.Fatalf("after the release (%d edits, %d before):\n%s\nwant it to end with:\n%s", c.edits, ended.edits, c.body, body)
		}
	}

	t.Run("closed without a merge", func(t *testing.T) {
		h := newHarness(t, statusOn)
		h.queuedPR(7, statusHead)
		h.closeOnGitHub(7, "CLOSED", keep) // 1 minute after the scheduled comment
		h.wantStatus(h.statusComment(7), "Closed before the review of `a1b2c3d` posted.")
		wantEnded(t, h, "Closed before the review of `a1b2c3d` posted.")
		if evs := h.events("pr.status_comment"); len(evs) != 2 || evs[1].Message != "status comment updated: closed (review of a1b2c3d)" {
			t.Fatalf("pr.status_comment events = %+v", evs)
		}
	})

	t.Run("merged before the review", func(t *testing.T) {
		h := newHarness(t, statusOn)
		h.queuedPR(7, statusHead)
		h.closeOnGitHub(7, "MERGED", keep)
		if len(h.mergedUnreviewedEvents(7)) != 1 {
			t.Fatal("no pr.merged_unreviewed event")
		}
		h.wantStatus(h.statusComment(7), "Merged before the review of `a1b2c3d` posted.")
		wantEnded(t, h, "Merged before the review of `a1b2c3d` posted.")
	})

	t.Run("merged after the review", func(t *testing.T) {
		h := newHarness(t, statusOn)
		h.queuedPR(7, statusHead)
		h.advance(5 * time.Minute)
		h.holdRounds()
		before := len(h.rd.all())
		if err := h.e.Tick(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.awaitRound(before)
		h.advance(2 * time.Minute)
		if err := h.e.Tick(h.ctx); err != nil {
			t.Fatal(err)
		}
		if c := h.statusComment(7); !strings.Contains(c.body, "\n- [x] In progress since ") {
			t.Fatalf("in progress:\n%s", c.body)
		}
		h.releaseRound() // posted, within 2 minutes of the in-progress edit
		pr := h.wantState(7, store.PRReviewed)
		h.closeOnGitHub(7, "MERGED", keep)
		h.wantStatus(h.statusComment(7), "Review of `a1b2c3d` posted "+utcClock(*pr.ReviewedAt)+": "+reviewURL(7, *pr.LastReviewID))
		wantEnded(t, h, "Review of `a1b2c3d` posted "+utcClock(*pr.ReviewedAt)+": "+reviewURL(7, *pr.LastReviewID))
	})

	t.Run("a draft", func(t *testing.T) {
		h := newHarness(t, statusOn, withoutDrafts)
		h.queuedPR(7, statusHead)
		h.advance(30 * time.Second)
		h.open(keep, prSpec{n: 7, head: statusHead, draft: true})
		h.tick()
		h.wantState(7, store.PRIneligible)
		h.wantStatus(h.statusComment(7), "Not reviewing: the PR is a draft.")
		ended := h.statusComment(7)
		for range 3 {
			h.advance(5 * time.Minute)
			h.tick()
		}
		if c := h.statusComment(7); c.edits != ended.edits {
			t.Fatalf("the draft's comment was edited again:\n%s", c.body)
		}

		h.open(keep, prSpec{n: 7, head: statusHead}) // ready for review
		h.tick()
		h.wantState(7, store.PRQueued)
		if c := h.statusComment(7); !strings.Contains(c.body, "\n- [x] Review queued for `a1b2c3d`: ") {
			t.Fatalf("ready again:\n%s", c.body)
		}
	})

	t.Run("released", func(t *testing.T) {
		h := newHarness(t, statusOn)
		h.queuedPR(7, statusHead)
		h.gh.commentErr = &github.APIError{Op: "comment", Status: 502, Message: "Bad Gateway"}
		h.closeOnGitHub(7, "CLOSED", keep)
		for range 3 {
			h.advance(5 * time.Minute)
			h.tick()
		}
		h.wantState(7, store.PRReleased)
		if c := h.statusComment(7); !strings.Contains(c.body, "\n- [x] Review queued for `a1b2c3d`: ") {
			t.Fatalf("GitHub refused the last edit, yet:\n%s", c.body)
		}
		h.gh.commentErr = nil
		h.advance(5 * time.Minute)
		h.tick()
		h.wantStatus(h.statusComment(7), "Closed before the review of `a1b2c3d` posted.")
		ended := h.statusComment(7)
		h.advance(5 * time.Minute)
		h.tick()
		if c := h.statusComment(7); c.edits != ended.edits {
			t.Fatalf("the released PR's comment was edited again: %d edits", c.edits)
		}
	})
}

// `magnum review` and a review request of the reviewed head restart the
// list as "Another review requested", in progress while its round runs,
// and then the new review's line; a reply round on the same head, which
// nobody asked for, leaves the posted line.
func TestAManualReviewOfTheReviewedHeadShowsInTheStatusComment(t *testing.T) {
	h := newHarness(t, statusOn)
	pr := h.statusReviewed()
	postedLine := "Review of `a1b2c3d` posted " + utcClock(*pr.ReviewedAt) + ": " + reviewURL(7, *pr.LastReviewID)
	h.wantStatusSince(h.statusComment(7), h.clock.Now(), postedLine)
	edits := h.statusComment(7).edits

	// A reply round: the posted line stays.
	h.advance(10 * time.Minute)
	reqPoll(h, prSpec{n: 7, head: statusHead, remarks: []github.Remark{threadReply(h, "alice")}})
	h.wantState(7, store.PRRereviewPending)
	repliedRounds(h)
	h.advance(3 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 2 || h.rd.all()[1].Replies != 1 {
		t.Fatalf("rounds = %d, want the reply round", n)
	}
	h.advance(2 * time.Minute)
	h.tick()
	if c := h.statusComment(7); c.edits != edits || !strings.HasSuffix(c.body, ")\n"+postedLine) {
		t.Fatalf("the reply round edited the comment (%d edits, %d before):\n%s", c.edits, edits, c.body)
	}

	// magnum review: the list again, for the same head.
	h.advance(10 * time.Minute)
	h.holdRounds()
	before := len(h.rd.all())
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "7"}})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	h.advance(2 * time.Minute)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	running := h.wantState(7, store.PRReviewing)
	h.wantStatus(h.statusComment(7), "- [x] Another review requested for `a1b2c3d`\n- [x] In progress since "+utcClock(*running.LastRoundStartedAt)+"\n- [ ] Posted\n\n"+
		"Earlier review of `a1b2c3d`: "+reviewURL(7, *pr.LastReviewID)+"\n\n"+statusSelf+"\n"+statusMerge)
	h.releaseRound()
	h.advance(2 * time.Minute)
	h.tick()
	again := h.wantState(7, store.PRReviewed)
	h.wantStatus(h.statusComment(7), "Review of `a1b2c3d` posted "+utcClock(*again.ReviewedAt)+": "+reviewURL(7, *again.LastReviewID))

	// A review request of the poll login: the same, through its debounce.
	h.advance(10 * time.Minute)
	asked := h.clock.Now()
	reqPoll(h, prSpec{n: 7, head: statusHead, requests: []github.ReviewRequestEvent{reqEvent(asked, "alice", askedPoll)}})
	h.wantState(7, store.PRRereviewPending)
	if c := h.statusComment(7); !strings.HasPrefix(c.body, statusHeader(asked)+"- [x] Another review requested for `a1b2c3d`: not before "+
		utcClock(asked.Add(time.Minute))+" (a short delay after the review request)\n- [ ] In progress\n") {
		t.Fatalf("the review request:\n%s", c.body)
	}
	h.advance(time.Minute)
	h.holdRounds()
	before = len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	h.advance(2 * time.Minute)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.wantState(7, store.PRReviewing)
	if c := h.statusComment(7); !strings.Contains(c.body, ")\n- [x] Another review requested for `a1b2c3d`\n- [x] In progress since ") {
		t.Fatalf("the requested round:\n%s", c.body)
	}
	h.releaseRound()
}

// The status comment is off in the defaults; [pipeline] status_comment
// switches it on, a watch's own key wins either way, and a draft never gets
// one.
func TestTheStatusCommentIsOffByDefaultAndPerWatch(t *testing.T) {
	h := newHarness(t, appGitHub)
	h.queuedPR(7, statusHead)
	if cs := h.gh.commentsOn(7); len(cs) != 0 {
		t.Fatalf("the defaults posted %+v", cs)
	}

	h.cfg.Pipeline.StatusComment = true
	h.cfg.Watches[0].StatusComment = new(false)
	h.advance(30 * time.Second)
	h.tick()
	if cs := h.gh.commentsOn(7); len(cs) != 0 {
		t.Fatalf("a watch with status_comment = false got %+v", cs)
	}

	h.cfg.Pipeline.StatusComment = false
	h.cfg.Watches[0].StatusComment = new(true)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusHead}, prSpec{n: 9, head: statusPush, draft: true})
	h.advance(30 * time.Second)
	h.tick()
	h.statusComment(7)
	if cs := h.gh.commentsOn(9); len(cs) != 0 {
		t.Fatalf("a draft (state %s) got %+v", h.pr(9).State, cs)
	}
}

// A GitHub that refuses the comment never holds the round: the failure is
// one warn event per PR an hour, the round posts its review, and a later
// tick posts the comment once GitHub takes it.
func TestAStatusCommentFailureNeverBlocksTheRound(t *testing.T) {
	h := newHarness(t, statusOn)
	h.gh.commentErr = &github.APIError{Op: "comment", Status: 502, Message: "Bad Gateway"}
	h.queuedPR(7, statusHead)
	if evs := h.events("pr.status_comment_failed"); len(evs) != 1 || evs[0].Level != "warn" ||
		!strings.HasPrefix(evs[0].Message, "status comment not posted (scheduled): ") {
		t.Fatalf("pr.status_comment_failed events = %+v", evs)
	}
	h.advance(30 * time.Second)
	h.tick()
	if n := h.gh.count("issue_comment:"); n != 1 {
		t.Fatalf("comment attempts within 2 minutes = %d, want 1", n)
	}

	h.advance(5 * time.Minute)
	h.tick() // the round runs and posts
	h.wantState(7, store.PRReviewed)
	if n := len(h.events("pr.status_comment_failed")); n != 1 {
		t.Fatalf("failures within the hour warned %d times, want once", n)
	}

	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusPush})
	h.tick()
	h.wantState(7, store.PRRereviewPending)
	if n := len(h.events("pr.status_comment_failed")); n != 2 {
		t.Fatalf("a failure an hour later warned %d times in all, want 2", n)
	}

	h.gh.commentErr = nil
	h.advance(2 * time.Minute)
	h.tick()
	if c := h.statusComment(7); !strings.Contains(c.body, "\n- [x] Re-review queued for `b2c3d4e`: ") {
		t.Fatalf("once GitHub took it:\n%s", c.body)
	}
}

// A comment a person deleted is posted once more at its next edit; deleted
// a second time, the PR gets no comment until its next push.
func TestADeletedStatusCommentIsPostedOnceMore(t *testing.T) {
	h := newHarness(t, statusOn)
	h.queuedPR(7, statusHead)
	first := h.statusComment(7)

	h.gh.deleteComment(first.id)
	h.advance(2 * time.Minute)
	h.muteRequest(ReqMute, 7)
	second := h.statusComment(7)
	if second.id == first.id {
		t.Fatal("the deleted comment answered the edit")
	}
	h.wantStatus(second, "Magnum will not review this head. The operator was told.")

	h.gh.deleteComment(second.id)
	h.advance(2 * time.Minute) // still within the push quiet period
	h.muteRequest(ReqUnmute, 7)
	h.wantState(7, store.PRQueued)
	h.advance(30 * time.Second)
	h.tick()
	if cs := h.gh.commentsOn(7); len(cs) != 0 || h.gh.count("issue_comment:") != 2 {
		t.Fatalf("deleted twice, the PR got %d comment calls: %+v", h.gh.count("issue_comment:"), cs)
	}

	h.advance(2 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusPush})
	h.tick()
	if c := h.statusComment(7); !strings.Contains(c.body, "\n- [x] Review queued for `b2c3d4e`: ") {
		t.Fatalf("after the next push:\n%s", c.body)
	}
}

// The comment names only magnum's own words: fixed phrases for every wait
// (whose own words may carry an error, a branch, an identity or a pool),
// "other reviews are running" for the slots and the capacity, and a timing
// rule's end as the earliest start in UTC.
func TestTheStatusCommentNamesOnlyMagnumsOwnWaits(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 34, 0, 0, time.UTC)
	until := now.Add(10*time.Minute + 20*time.Second)
	for _, c := range []struct {
		w    Wait
		want string
	}{
		{Wait{Reason: WaitQuiet, Until: until, Detail: "the push quiet period (15m)"}, "not before 12:45 UTC (the push quiet period)"},
		{Wait{Reason: WaitCap, Until: now.Add(12 * time.Hour), Detail: "the daily round cap (6 of 6 automatic rounds today)"},
			"not before 6 Oct, 00:34 UTC (the daily round cap)"},
		{Wait{Reason: WaitNext, Detail: "the next dispatch"}, "starts shortly"},
		{Wait{Reason: WaitRequested, Until: now, Detail: "the next dispatch"}, "starts shortly"},
		{Wait{Reason: WaitQuietHours, Until: until, Detail: "the end of quiet hours (22:00-08:00)"}, "not before 12:45 UTC (quiet hours)"},
		{Wait{Reason: WaitRetry, Until: until, Detail: "a retry (attempt 2) after: git fetch origin feature/secret-plan: exit 128"},
			"a retry after a failed attempt, not before 12:45 UTC"},
		{Wait{Reason: WaitSlot, Detail: "no free slot in the talkable/talkable pool"}, "other reviews are running"},
		{Wait{Reason: WaitCapacity, Detail: "review capacity: 3 rounds running"}, "other reviews are running"},
		{Wait{Reason: WaitIdentity, Detail: "identity talkable-app: token refresh failed"}, "held on Magnum's side"},
		{Wait{Reason: WaitOther, Detail: "human active in the PR's panes"}, "held on Magnum's side"},
	} {
		if got := statusWaitWords(c.w, now); got != c.want {
			t.Errorf("%s: %q, want %q", c.w.Reason, got, c.want)
		}
	}
}

// A fix loop shows as its own line: while its fixer works, "Applying
// automatic fixes" with the cycle and the loop's cap under the posted
// review, and its re-review of what the fixer pushed as "Reviewing the
// fixes", from the loop's row alone.
func TestAFixLoopShowsInTheStatusComment(t *testing.T) {
	h := newHarness(t, statusOn)
	pr := h.statusReviewed()
	postedLine := "Review of `a1b2c3d` posted " + utcClock(*pr.ReviewedAt) + ": " + reviewURL(7, *pr.LastReviewID)
	st := FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing, Since: statusHead, Head: statusHead, By: fixBy, StartedAt: h.clock.Now(), Role: "claude-fix"}
	if err := h.e.setFixState(h.ctx, pr.ID, st); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Minute)
	h.tick()
	h.wantStatus(h.statusComment(7), "- [x] "+postedLine+"\n- [ ] Applying automatic fixes, cycle 1 of 3\n\n"+statusSelf)

	st.Phase, st.Head = FixPhaseReviewing, statusPush
	if err := h.e.setFixState(h.ctx, pr.ID, st); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusPush})
	h.tick()
	h.wantState(7, store.PRRereviewPending)
	if c := h.statusComment(7); !strings.Contains(c.body, ")\n- [x] Reviewing the fixes in `b2c3d4e`: ") ||
		!strings.Contains(c.body, "\n\nEarlier review of `a1b2c3d`: "+reviewURL(7, *pr.LastReviewID)+"\n\n") {
		t.Fatalf("the fixes' review:\n%s", c.body)
	}
}

// A PR Codex flagged, which magnum never reviews again, says the same one
// line as a muted one; one its judge parked for the operator
// (needs_attention) says that magnum failed, as
// TestTheStatusCommentNamesMagnumsOwnFailure shows for every such park.
func TestAFlaggedOrParkedPRsStatusCommentSaysNotReviewing(t *testing.T) {
	h := newHarness(t, statusOn)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusHead}, prSpec{n: 8, head: statusPush})
	h.tick()
	h.statusComment(7)
	h.statusComment(8)

	h.advance(2 * time.Minute)
	h.setFlag(7, "Codex warned about cyber abuse")
	h.tick()
	h.wantState(7, store.PRIneligible)
	h.wantStatus(h.statusComment(7), "Magnum will not review this head. The operator was told.")

	h.rd.mu.Lock()
	h.rd.script = func(pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeNeedsAttention, Error: "the judge asked for a human"}, nil
	}
	h.rd.mu.Unlock()
	h.advance(5 * time.Minute)
	h.tick() // the round parks #8
	h.wantState(8, store.PRNeedsAttention)
	h.advance(2 * time.Minute)
	h.tick()
	h.wantStatus(h.statusComment(8), "Magnum failed to finish the review of `b2c3d4e`. The fault is on its side, not in your PR. "+
		"The operator was told. A new push starts a new review.")
}

// statusFailedLine is the line of a PR magnum's own failure parked, at
// statusHead.
const statusFailedLine = "Magnum failed to finish the review of `a1b2c3d`. The fault is on its side, not in your PR. " +
	"The operator was told. A new push starts a new review."

// When magnum's own failure parked a PR (a round that failed on every
// attempt, a judge that reported a blocker or stopped without a review),
// its status comment says that magnum failed, not the PR, and that a new
// push starts a new review, which it does. A PR muted while parked and one
// Codex refused keep the line that magnum will not review the head.
func TestTheStatusCommentNamesMagnumsOwnFailure(t *testing.T) {
	notReviewing := "Magnum will not review this head. The operator was told."
	// parked runs PR #7's rounds to res until needs_attention (attempts
	// rounds, a retry's backoff apart) and ticks its comment up to date.
	parked := func(t *testing.T, h *harness, res pipeline.RoundResult, attempts int) store.PR {
		t.Helper()
		h.rd.mu.Lock()
		h.rd.script = func(pipeline.RoundInput) (pipeline.RoundResult, error) { return res, nil }
		h.rd.mu.Unlock()
		h.queuedPR(7, statusHead)
		h.advance(5 * time.Minute)
		h.tick()
		for i := 1; i < attempts; i++ {
			h.advance(backoff(i))
			h.tick()
		}
		pr := h.wantState(7, store.PRNeedsAttention)
		h.advance(2 * time.Minute)
		h.tick()
		return pr
	}
	for _, c := range []struct {
		name     string
		res      pipeline.RoundResult
		attempts int
		kind     string
	}{
		{"a round that failed", pipeline.RoundResult{Outcome: pipeline.OutcomeTimeout, Error: "judge_timeout passed without a result"},
			maxAttempts, attention.KindFailed},
		{"a blocked judge", pipeline.RoundResult{Outcome: pipeline.OutcomeBlocked, Error: "judge blocked: the checkout has no database"},
			1, attention.KindBlocked},
		{"a judge that stopped", pipeline.RoundResult{Outcome: pipeline.OutcomeNeedsAttention, Error: "the judge posted nothing after a nudge"},
			1, attention.KindNoReview},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, statusOn)
			pr := parked(t, h, c.res, c.attempts)
			if kind, _ := h.e.getKV(h.ctx, KVPRAttention(pr.ID)); kind != c.kind {
				t.Fatalf("parked as %q, want %q", kind, c.kind)
			}
			if got := h.statusComment(7); !strings.HasSuffix(got.body, ")\n"+statusFailedLine) {
				t.Fatalf("parked:\n%s", got.body)
			}

			h.advance(2 * time.Minute)
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusPush})
			h.tick()
			h.wantState(7, store.PRQueued)
			if got := h.statusComment(7); !strings.Contains(got.body, ")\n- [x] Review queued for `b2c3d4e`: ") {
				t.Fatalf("after the push:\n%s", got.body)
			}
		})
	}

	t.Run("muted while parked", func(t *testing.T) {
		h := newHarness(t, statusOn)
		parked(t, h, pipeline.RoundResult{Outcome: pipeline.OutcomeBlocked, Error: "judge blocked: the checkout has no database"}, 1)
		h.advance(2 * time.Minute)
		h.muteRequest(ReqMute, 7)
		h.wantStatus(h.statusComment(7), notReviewing)
	})

	t.Run("refused by Codex", func(t *testing.T) {
		h := newHarness(t, statusOn)
		h.queuedPR(7, statusHead)
		h.advance(2 * time.Minute)
		h.setFlag(7, "Codex warned about cyber abuse")
		h.tick()
		h.wantState(7, store.PRIneligible)
		h.wantStatus(h.statusComment(7), notReviewing)
	})
}

// A review someone asks for on a draft opens the status comment as on a
// ready PR, whether the watch skips drafts (the request makes the draft
// eligible) or reviews them; a draft nobody asked about still gets none.
func TestARequestedReviewOnADraftOpensTheStatusComment(t *testing.T) {
	for _, c := range []struct {
		name string
		mods []func(*harness)
	}{
		{"drafts skipped", []func(*harness){statusOn, withoutDrafts}},
		{"drafts included", []func(*harness){statusOn}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := draftSynced(t, c.mods...)
			reqPoll(h, prSpec{n: 7, head: statusHead, draft: true})
			h.advance(time.Minute)
			h.tick()
			if cs := h.gh.commentsOn(7); len(cs) != 0 {
				t.Fatalf("a draft nobody asked about (state %s) got %+v", h.pr(7).State, cs)
			}

			h.advance(time.Minute)
			ask := reqAsk(h, "alice", askedPoll)
			reqPoll(h, prSpec{n: 7, head: statusHead, draft: true, request: true, requests: []github.ReviewRequestEvent{ask}})
			h.wantState(7, store.PRQueued)
			h.wantStatus(h.statusComment(7), "- [x] Review queued for `a1b2c3d`: not before "+utcClock(ask.CreatedAt.Add(time.Minute))+
				" (a short delay after the review request)\n- [ ] In progress\n- [ ] Posted\n\n"+statusSelf+"\n"+statusMerge)

			h.advance(time.Minute)
			h.tick() // the requested round runs and posts
			pr := h.wantState(7, store.PRReviewed)
			h.advance(2 * time.Minute)
			h.tick()
			h.wantStatus(h.statusComment(7), "Review of `a1b2c3d` posted "+utcClock(*pr.ReviewedAt)+": "+reviewURL(7, *pr.LastReviewID))
		})
	}
}

// A review request that comes while the PR is muted, or while the watch's
// filters leave it out, gets an answer on the PR: its status comment opens,
// or turns, to one line that names no reason (the repositories are
// public), and the operator gets a toast; the operator's own request is
// one like any other. The unmute queues the review the request asked for,
// and the comment follows it again. A mute alone keeps the line that
// magnum will not review the head.
func TestARequestOnAMutedOrIneligiblePRGetsTheGenericLine(t *testing.T) {
	generic := "Magnum is not reviewing this PR. The operator was told."
	t.Run("muted", func(t *testing.T) {
		h := newHarness(t, statusOn)
		h.queuedPR(7, statusHead)
		h.advance(2 * time.Minute)
		h.muteRequest(ReqMute, 7)
		h.wantState(7, store.PRIneligible)
		h.wantStatus(h.statusComment(7), "Magnum will not review this head. The operator was told.")
		h.flush()

		h.advance(2 * time.Minute)
		ask := reqAsk(h, "alice", askedPoll)
		reqPoll(h, prSpec{n: 7, head: statusHead, request: true, requests: []github.ReviewRequestEvent{ask}})
		h.wantState(7, store.PRIneligible)
		h.wantStatus(h.statusComment(7), generic)
		h.flush()
		if got := toastsWith(h, "magnum: talkable/talkable#7: review requested by alice (not reviewed: muted)"); len(got) != 1 {
			t.Fatalf("toasts = %q, want one for the request", h.nh.shown())
		}
		reqWantRounds(t, h, 0)

		h.advance(2 * time.Minute)
		h.muteRequest(ReqUnmute, 7) // queues the requested review, which runs at once
		pr := h.wantState(7, store.PRReviewed)
		h.advance(2 * time.Minute)
		h.tick()
		if got := h.statusComment(7); !strings.HasSuffix(got.body, ")\nReview of `a1b2c3d` posted "+utcClock(*pr.ReviewedAt)+": "+reviewURL(7, *pr.LastReviewID)) {
			t.Fatalf("after the unmute:\n%s", got.body)
		}
	})

	t.Run("left out by the filters", func(t *testing.T) {
		h := newHarness(t, statusOn)
		h.cfg.Watches[0].SkipLabels = []string{"wip"}
		h.open(prSpec{n: 1, head: "base1"})
		h.startup()
		h.tick() // first sync: #1 baseline
		reqPoll(h, prSpec{n: 7, head: statusHead, labels: []string{"wip"}})
		h.wantState(7, store.PRIneligible)
		h.advance(time.Minute)
		h.tick()
		if cs := h.gh.commentsOn(7); len(cs) != 0 {
			t.Fatalf("a skipped PR nobody asked about got %+v", cs)
		}

		h.advance(time.Minute)
		ask := reqAsk(h, "zhuravel", askedBot) // the operator's own request
		reqPoll(h, prSpec{n: 7, head: statusHead, labels: []string{"wip"}, requests: []github.ReviewRequestEvent{ask}})
		h.wantState(7, store.PRIneligible)
		h.wantStatus(h.statusComment(7), generic)
		reqWantRounds(t, h, 0)
	})

	t.Run("a baseline PR left out by the filters", func(t *testing.T) {
		h := newHarness(t, statusOn)
		h.cfg.Watches[0].SkipLabels = []string{"wip"}
		old := prSpec{n: 1, head: "base1", labels: []string{"wip"}}
		h.open(old)
		h.startup()
		h.tick() // first sync: #1 baseline
		h.advance(time.Minute)
		old.requests, old.updated = []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}, h.clock.Now()
		h.open(old)
		h.tick()
		h.wantState(1, store.PRBaseline)
		h.wantStatus(h.statusComment(1), generic)
		reqWantRounds(t, h, 0)
	})
}

// The posted line carries the review's verdict in a few words before its
// link, rebuilt from the result its round stored (ReviewSummary): a
// blocking review, one with a problem to fix, one with optional findings
// only and a clean one alike, with no score and no event name.
func TestThePostedLineCarriesTheVerdictWords(t *testing.T) {
	for _, c := range []struct{ name, result, words string }{
		{"blocking", `{"event":"REQUEST_CHANGES","verdict":"blocking","findings":{"P1":1,"P2":1,"P3":1}}`,
			"Blocking: 2 problems must be fixed before merging, 1 optional"},
		{"a problem to fix", `{"event":"COMMENT","verdict":"non_blocking","findings":{"P2":1,"P3":3},"candidates":{"claude-simplify":{"suggested":1}}}`,
			"Fix 1 problem before merging, 4 optional"},
		{"optional only", `{"event":"COMMENT","verdict":"non_blocking","findings":{"P3":2}}`, "No blocking problems, 2 optional"},
		{"clean", `{"event":"APPROVE","verdict":"clean","findings":{}}`, "No problems found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, statusOn)
			pr := h.statusReviewed()
			if _, err := h.st.DB().ExecContext(h.ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login,
  state, outcome, review_id, review_url, prompt_text, result_json, created_at)
  VALUES ('run-verdict-1', ?, 1, 'codex-judge', 'initial', ?, 'talkable-app', 'talkable[bot]', 'verified', 'posted', ?, ?, 'p', ?, ?)`,
				pr.ID, statusHead, *pr.LastReviewID, reviewURL(7, *pr.LastReviewID), c.result, store.FormatTime(h.clock.Now())); err != nil {
				t.Fatal(err)
			}
			h.advance(2 * time.Minute)
			h.tick()
			line := "Review of `a1b2c3d` posted " + utcClock(*pr.ReviewedAt) + ": " + c.words + ". " + reviewURL(7, *pr.LastReviewID)
			if got := h.statusComment(7); !strings.HasSuffix(got.body, ")\n"+line) {
				t.Fatalf("posted line:\n%s\nwant it to end with:\n%s", got.body, line)
			}
		})
	}
}
