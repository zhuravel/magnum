package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// seedReplies makes pr a PR magnum reviewed at head (reviewed 2h ago) with
// replies of the given authors an hour ago, which its judge has not
// re-decided.
func (h *actHarness) seedReplies(pr store.PR, head string, authors ...string) {
	h.t.Helper()
	var replies []store.Reply
	for _, a := range authors {
		replies = append(replies, store.Reply{At: h.now.Add(-time.Hour), By: a, Thread: true})
	}
	b, err := json.Marshal(replies)
	if err != nil {
		h.t.Fatal(err)
	}
	h.setPR(pr.ID, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("reviewed_sha", head)
		u.Set("last_review_event", "CHANGES_REQUESTED")
		u.Set("reviewed_at", store.FormatTime(h.now.Add(-2*time.Hour)))
		u.Set("replies_json", string(b))
	})
}

// --replies has the judge alone re-decide the replies on the head magnum
// reviewed: the request carries Replies and nothing a full round asks for,
// and the command says what the round is.
func TestReviewRepliesQueuesAReplyRound(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.seedReplies(pr, pr.HeadSHA, "alice", "alice")

	if code := h.cmd("review", "talkable#5", "--replies"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqReview {
		t.Fatalf("requests = %+v", reqs)
	}
	p := actDecode[engine.ReviewPayload](t, reqs[0].Payload)
	if !p.Replies || p.Fresh || p.Simplify || len(p.Roles) != 0 || p.Number != 5 {
		t.Fatalf("payload = %+v", p)
	}
	if !strings.Contains(string(reqs[0].Payload), `"replies":true`) {
		t.Errorf("payload JSON = %s", reqs[0].Payload)
	}
	actContains(t, h.out.String(), "the judge alone re-decides 2 replies on magnum's review of abc1234 (a reply round)")
	if strings.Contains(h.out.String(), "was already reviewed") {
		t.Errorf("a reply round says it reviews the head again:\n%s", h.out.String())
	}

	// without the flag the same PR is an ordinary forced round, as before
	h.out.Reset()
	if code := h.cmd("review", "talkable#5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if p := actDecode[engine.ReviewPayload](t, h.requests()[1].Payload); p.Replies {
		t.Errorf("a plain review asked for a reply round: %+v", p)
	}
	actContains(t, h.out.String(), "head abc1234 was already reviewed (CHANGES_REQUESTED); reviewing it again")
}

// The dry run prints the payload with the flag, and queues nothing.
func TestReviewRepliesDryRunShowsTheFlag(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.seedReplies(pr, pr.HeadSHA, "alice")
	if code := h.cmd("review", "5", "--replies", "--dry-run"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), `"replies":true`, "the judge alone re-decides 1 reply")
	if len(h.requests()) != 0 {
		t.Errorf("a dry run queued %d requests", len(h.requests()))
	}
}

// --role, --simplify and --fresh ask for a full round, which a reply round
// is not: the command refuses the mix before it resolves or queues anything.
func TestReviewRepliesRefusesWhatAsksForAFullRound(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.seedReplies(pr, pr.HeadSHA, "alice")
	for _, tc := range []struct {
		args []string
		with string
	}{
		{[]string{"--role", "claude-simplify"}, "--role,"},
		{[]string{"--simplify"}, "--simplify,"},
		{[]string{"--fresh"}, "--fresh,"},
		{[]string{"--simplify", "--fresh"}, "--simplify, --fresh,"},
	} {
		h.errb.Reset()
		code := h.cmd("review", append([]string{"5", "--replies"}, tc.args...)...)
		if code != 2 || !strings.Contains(h.errb.String(), "--replies (the judge alone re-decides the replies) cannot be combined with "+tc.with+" which ask for a full round") {
			t.Errorf("review --replies %v: exit %d, stderr %q", tc.args, code, h.errb.String())
		}
	}
	if len(h.requests()) != 0 || len(h.gh.calls) != 0 {
		t.Errorf("a refused review queued %d requests and read GitHub %v", len(h.requests()), h.gh.calls)
	}
}

// When the daemon cannot run a reply round (the head moved, nothing waits,
// magnum never reviewed the PR) it runs an ordinary forced one: the command
// says so, and still sends the flag, which the daemon decides on.
func TestReviewRepliesSaysWhenItIsAnOrdinaryRound(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(h *actHarness, pr store.PR)
		want string
	}{
		{"the head moved", func(h *actHarness, pr store.PR) { h.seedReplies(pr, "0ldhead1234567", "alice") },
			"the head abc1234 moved since magnum reviewed 0ldhead, so --replies has nothing to re-decide there: this is an ordinary forced review"},
		{"no reply waits", func(h *actHarness, pr store.PR) { h.seedReplies(pr, pr.HeadSHA) },
			"no reply on magnum's review of abc1234 waits for the judge: this is an ordinary forced review"},
		{"never reviewed", func(h *actHarness, pr store.PR) {},
			"magnum has not reviewed it yet, so --replies has nothing to re-decide: this is an ordinary forced review"},
	} {
		h := newActHarness(t)
		h.pid = 4242
		pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
		tc.set(h, pr)
		if code := h.cmd("review", "5", "--replies"); code != 0 {
			t.Fatalf("%s: exit %d: %s", tc.name, code, h.errb.String())
		}
		actContains(t, h.out.String(), tc.want)
		reqs := h.requests()
		if len(reqs) != 1 || !actDecode[engine.ReviewPayload](t, reqs[0].Payload).Replies {
			t.Errorf("%s: requests %+v", tc.name, reqs)
		}
	}
}

// The screens' review action sends the flag the board's r asked for.
func TestScreenReviewActionSendsTheRepliesFlag(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.seedReplies(pr, pr.HeadSHA, "alice")
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.held = true
	h.pid = 4242
	acts := newScreenActions(h.c)
	defer acts.Close()
	ctx := context.Background()
	if _, err := acts.Review(ctx, "talkable#5", tui.ReviewOpts{Replies: true}); err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, err := acts.Review(ctx, "talkable#5", tui.ReviewOpts{}); err != nil {
		t.Fatalf("review: %v", err)
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests %+v", reqs)
	}
	if p := actDecode[engine.ReviewPayload](t, reqs[0].Payload); !p.Replies || p.Fresh || p.Simplify {
		t.Errorf("r on a row with replies sent %+v", p)
	}
	if p := actDecode[engine.ReviewPayload](t, reqs[1].Payload); p.Replies {
		t.Errorf("an ordinary review sent %+v", p)
	}
}
