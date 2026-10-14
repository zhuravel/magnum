package engine

import (
	"strings"
	"testing"
	"time"

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

// wantStatus checks that comment c reads the status header, updated now,
// and then body.
func (h *harness) wantStatus(c fakeComment, body string) {
	h.t.Helper()
	want := "<!-- magnum:status -->\n**magnum review status** (updated " + utcClock(h.clock.Now()) + ")\n" + body
	if c.body != want {
		h.t.Fatalf("status comment:\n%s\nwant:\n%s", c.body, want)
	}
}

// utcClock is t as the status comment writes a time of the harness's day.
func utcClock(t time.Time) string { return t.UTC().Format("15:04") + " UTC" }

const (
	statusHead  = "a1b2c3d4e5f6a7b8"
	statusPush  = "b2c3d4e5f6a7b8c9"
	statusMerge = "A merge before the review posts skips it."
)

// A PR that waits out the push quiet period gets one comment, as the
// watch's posting identity, that names its head and the wait in the wait's
// own words, until when in UTC, the fixed sentence and the operator's note;
// the next tick, with the same wait, edits nothing.
func TestAScheduledReviewPostsAStatusComment(t *testing.T) {
	h := newHarness(t, statusOn)
	h.cfg.Pipeline.StatusCommentNote = "Need it sooner? Ping me on Slack."
	start := h.clock.Now()
	pr := h.queuedPR(7, statusHead)

	c := h.statusComment(7)
	h.wantStatus(c, "- [x] Scheduled: review of `a1b2c3d` waits for the push quiet period (5m), until "+utcClock(start.Add(5*time.Minute))+"\n"+
		"- [ ] In progress\n"+
		"- [ ] Posted\n"+
		"\n"+statusMerge+"\n"+
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
// ticks "Posted" with its link, and a push starts the list again, as a
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
	if !strings.HasSuffix(c.body, "\n- [x] Scheduled: review of `a1b2c3d`\n- [x] In progress since "+utcClock(*pr.LastRoundStartedAt)+"\n- [ ] Posted\n\n"+statusMerge) {
		t.Fatalf("in progress:\n%s", c.body)
	}

	h.releaseRound()
	pr = h.wantState(7, store.PRReviewed)
	h.advance(2 * time.Minute)
	h.tick()
	c = h.statusComment(7)
	h.wantStatus(c, "- [x] Scheduled: review of `a1b2c3d`\n- [x] In progress\n"+
		"- [x] Posted: https://github.com/talkable/talkable/pull/7#pullrequestreview-"+itoa(*pr.LastReviewID))

	h.advance(3 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusPush})
	h.tick()
	h.wantState(7, store.PRRereviewPending)
	c = h.statusComment(7)
	if c.id != first.id {
		t.Fatalf("the push posted comment %d, want %d edited", c.id, first.id)
	}
	if !strings.Contains(c.body, "\n- [x] Scheduled: re-review of `b2c3d4e` waits for ") || !strings.Contains(c.body, "\n- [ ] In progress\n- [ ] Posted\n\n"+statusMerge) {
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

// A muted PR's comment says, in one line and without the cause, that
// magnum will not review its head; the unmute schedules it again.
func TestAMutedPRsStatusCommentSaysNotReviewing(t *testing.T) {
	h := newHarness(t, statusOn)
	h.queuedPR(7, statusHead)

	h.advance(2 * time.Minute)
	h.muteRequest(ReqMute, 7)
	h.wantState(7, store.PRIneligible)
	h.wantStatus(h.statusComment(7), "magnum will not review this head. The operator was told.")

	h.advance(2 * time.Minute) // still within the push quiet period
	h.muteRequest(ReqUnmute, 7)
	h.wantState(7, store.PRQueued)
	c := h.statusComment(7)
	if !strings.Contains(c.body, "\n- [x] Scheduled: review of `a1b2c3d` ") {
		t.Fatalf("after the unmute:\n%s", c.body)
	}
}

// A PR GitHub merged before its review posted says so; a PR closed without
// a merge, and either one after, gets no further edit.
func TestAMergeBeforeTheReviewMarksTheStatusComment(t *testing.T) {
	h := newHarness(t, statusOn)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusHead}, prSpec{n: 8, head: statusPush})
	h.tick()
	h.wantState(7, store.PRQueued)
	closed := h.statusComment(8)

	h.advance(3 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"})
	h.gh.closed[7], h.gh.closed[8] = "MERGED", "CLOSED"
	h.tick()
	h.advance(30 * time.Second)
	h.tick()
	h.advance(30 * time.Second)
	h.tick()
	h.wantState(7, store.PRClosed)
	h.wantState(8, store.PRClosed)
	if len(h.mergedUnreviewedEvents(7)) != 1 {
		t.Fatal("no pr.merged_unreviewed event")
	}
	merged := h.statusComment(7)
	h.wantStatus(merged, "Merged before the review of `a1b2c3d` posted.")
	if c := h.statusComment(8); c.body != closed.body || c.edits != 0 {
		t.Fatalf("the closed PR's comment was edited:\n%s", c.body)
	}

	for range 3 {
		h.advance(5 * time.Minute) // past the close grace: released
		h.tick()
	}
	h.wantState(7, store.PRReleased)
	if c := h.statusComment(7); c.edits != merged.edits || h.statusComment(8).edits != 0 {
		t.Fatalf("a closed PR's comment was edited again: %d edits", c.edits)
	}
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
	if c := h.statusComment(7); !strings.Contains(c.body, "\n- [x] Scheduled: re-review of `b2c3d4e` ") {
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
	h.wantStatus(second, "magnum will not review this head. The operator was told.")

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
	if c := h.statusComment(7); !strings.Contains(c.body, "\n- [x] Scheduled: review of `b2c3d4e` ") {
		t.Fatalf("after the next push:\n%s", c.body)
	}
}

// The comment names only magnum's own words: the timing rules' waits in
// their own words, every other wait (whose words may carry an error, a
// branch, an identity or a pool) in a fixed phrase, and the end in UTC.
func TestTheStatusCommentNamesOnlyMagnumsOwnWaits(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 34, 0, 0, time.UTC)
	until := now.Add(10*time.Minute + 20*time.Second)
	for _, c := range []struct {
		w    Wait
		want string
	}{
		{Wait{Reason: WaitQuiet, Until: until, Detail: "the push quiet period (15m)"}, "waits for the push quiet period (15m), until 12:45 UTC"},
		{Wait{Reason: WaitCap, Until: now.Add(12 * time.Hour), Detail: "the daily round cap (6 of 6 automatic rounds today)"},
			"waits for the daily round cap (6 of 6 automatic rounds today), until Oct 6 00:34 UTC"},
		{Wait{Reason: WaitNext, Detail: "the next dispatch"}, "starts shortly"},
		{Wait{Reason: WaitRequested, Until: now, Detail: "the next dispatch"}, "starts shortly"},
		{Wait{Reason: WaitQuietHours, Until: until, Detail: "the end of quiet hours (22:00-08:00)"}, "waits for the end of quiet hours, until 12:45 UTC"},
		{Wait{Reason: WaitRetry, Until: until, Detail: "a retry (attempt 2) after: git fetch origin feature/secret-plan: exit 128"},
			"waits for a retry after a failed attempt, until 12:45 UTC"},
		{Wait{Reason: WaitSlot, Detail: "no free slot in the talkable/talkable pool"}, "waits for a free review slot"},
		{Wait{Reason: WaitIdentity, Detail: "identity talkable-app: token refresh failed"}, "is held on magnum's side"},
		{Wait{Reason: WaitOther, Detail: "human active in the PR's panes"}, "is held on magnum's side"},
	} {
		if got := statusWaitWords(c.w, now); got != c.want {
			t.Errorf("%s: %q, want %q", c.w.Reason, got, c.want)
		}
	}
}

// A PR Codex flagged, which magnum never reviews again, and one parked for
// the operator (needs_attention) say the same one line as a muted one.
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
	h.wantStatus(h.statusComment(7), "magnum will not review this head. The operator was told.")

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
	h.wantStatus(h.statusComment(8), "magnum will not review this head. The operator was told.")
}
