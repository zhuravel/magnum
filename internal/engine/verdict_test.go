package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// TestManualVerdictPostsOnTheReviewedHead: approve and request changes post
// the reviewer's verdict on the head magnum reviewed, as the PR's identity,
// with a body that follows magnum's review, and record it as the PR's
// latest review (marked manual, so rounds never dismiss it as their own).
func TestManualVerdictPostsOnTheReviewedHead(t *testing.T) {
	h := newHarness(t, verdictClients)
	h.reviewedPR(2, "b1")

	ok := h.enqueue(ReqApprove, VerdictPayload{PRTarget: PRTarget{Ref: "talkable#2"}})
	h.tick()
	if r := h.request(ok); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "approved talkable/talkable#2 at b1") {
		t.Fatalf("approve: %+v %q", r, deref(r.Result))
	}
	h.gh.mu.Lock()
	created := append([]string(nil), h.gh.created...)
	h.gh.mu.Unlock()
	if len(created) != 1 || !strings.HasPrefix(created[0], "APPROVE@b1:Approved after magnum's review of `b1`") ||
		!strings.Contains(created[0], "<!-- magnum:verdict=APPROVE head=b1 -->") {
		t.Fatalf("posted %q", created)
	}
	pr := h.pr(2)
	if deref(pr.LastReviewEvent) != "APPROVED" || deref(pr.LastReviewID) != 9001 {
		t.Fatalf("recorded %q %d", deref(pr.LastReviewEvent), deref(pr.LastReviewID))
	}
	if v, ok := h.e.getKV(h.ctx, KVPRManualVerdict(pr.ID)); !ok || v != "9001" {
		t.Fatalf("manual verdict mark %q", v)
	}

	rc := h.enqueue(ReqRequestChanges, VerdictPayload{PRTarget: PRTarget{Ref: "talkable#2"}, Message: "The refund path still double-counts."})
	h.tick()
	if r := h.request(rc); r.State != store.RequestDone {
		t.Fatalf("request changes: %+v %q", r, deref(r.Result))
	}
	h.gh.mu.Lock()
	last := h.gh.created[len(h.gh.created)-1]
	h.gh.mu.Unlock()
	if !strings.HasPrefix(last, "REQUEST_CHANGES@b1:The refund path still double-counts.\n\nChanges requested after magnum's review") {
		t.Fatalf("posted %q", last)
	}
	if deref(h.pr(2).LastReviewEvent) != "CHANGES_REQUESTED" {
		t.Fatalf("recorded %q", deref(h.pr(2).LastReviewEvent))
	}

	// The next round knows the latest review is the reviewer's own.
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(time.Hour)
	h.tick()
	h.rd.mu.Lock()
	in := h.rd.inputs[len(h.rd.inputs)-1]
	h.rd.mu.Unlock()
	if in.Previous == nil || !in.Previous.Manual || in.Previous.ID != 9002 {
		t.Fatalf("previous review %+v", in.Previous)
	}
}

// A verdict needs magnum's review of the current head: an unreviewed PR is
// refused, a moved head is refused unless forced (then it posts on the
// reviewed head).
func TestManualVerdictRefusesWhatMagnumDidNotReview(t *testing.T) {
	h := newHarness(t, verdictClients)
	h.reviewedPR(2, "b1")
	none := h.enqueue(ReqApprove, VerdictPayload{PRTarget: PRTarget{Ref: "talkable#1"}})
	h.tick()
	if r := h.request(none); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "has not reviewed") {
		t.Fatalf("unreviewed: %+v %q", r, deref(r.Result))
	}

	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick() // the head moved; the re-review waits for its quiet period
	moved := h.enqueue(ReqApprove, VerdictPayload{PRTarget: PRTarget{Ref: "talkable#2"}})
	h.tick()
	if r := h.request(moved); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "moved to b2 since magnum reviewed b1") {
		t.Fatalf("moved: %+v %q", r, deref(r.Result))
	}
	forced := h.enqueue(ReqRequestChanges, VerdictPayload{PRTarget: PRTarget{Ref: "talkable#2"}, Force: true})
	h.tick()
	if r := h.request(forced); r.State != store.RequestDone {
		t.Fatalf("forced: %+v %q", r, deref(r.Result))
	}
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if len(h.gh.created) != 1 || !strings.HasPrefix(h.gh.created[0], "REQUEST_CHANGES@b1:") {
		t.Fatalf("posted %q", h.gh.created)
	}
}

// verdictClients gives every identity the fake GitHub (the PRs post as the
// App, which the harness otherwise leaves without a write client).
func verdictClients(h *harness) { h.d.GitHub = func(string) GitHub { return h.gh } }
