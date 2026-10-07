package engine

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
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

// verdictAs asks the daemon for a verdict (kind's request) on PR #n as
// identity as, and returns the answered request.
func (h *harness) verdictAs(kind string, n int, as string) store.Request {
	h.t.Helper()
	id := h.enqueue(kind, VerdictPayload{PRTarget: PRTarget{Ref: strconv.Itoa(n)}, As: as})
	h.tick()
	return h.request(id)
}

// A on a row GitHub blocks on the operator (`magnum approve --as` the
// watch's auto_approve_as, here without auto_approve) posts the operator's
// own approval of the reviewed head, which GitHub counts, and records it
// like an automatic approval: the PR's latest review stays magnum's
// round's, and a later round that finds something to fix withdraws it as
// magnum withdraws its own.
func TestApproveAsTheOperatorIsFollowedLikeAnAutoApproval(t *testing.T) {
	h, plan := newAutoHarness(t, func(h *harness) { h.cfg.Watches[0].AutoApprove = nil })
	plan.set(2, cleanRound, blockingRound)
	h.reviewedPR(2, "b1")
	h.tick()
	if got := h.created(); len(got) != 0 {
		t.Fatalf("approved without being asked: %q", got)
	}
	before := h.pr(2)

	r := h.verdictAs(ReqApprove, 2, "zhuravel")
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "approved talkable/talkable#2 at b1 as zhuravel: "+prURL2+"#pullrequestreview-9001") {
		t.Fatalf("approve as the operator: %s %q", r.State, deref(r.Result))
	}
	got := h.created()
	if len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:Approved after magnum's review of `b1`") ||
		!strings.HasSuffix(got[0], "<!-- magnum:auto-approval head=b1 -->") {
		t.Fatalf("posted %q", got)
	}
	a := h.autoApproval(2)
	if a.State != store.AutoStanding || a.Identity != "zhuravel" || a.Login != "zhuravel" || a.ReviewID != 9001 || a.HeadSHA != "b1" ||
		a.RunID != "run-1" || a.SourceReviewID != 501 || a.PostedAt == nil {
		t.Fatalf("approval = %+v", a)
	}
	pr := h.pr(2)
	if deref(pr.LastReviewID) != deref(before.LastReviewID) || deref(pr.LastReviewEvent) != deref(before.LastReviewEvent) {
		t.Fatalf("the latest review moved: %d %q, was %d %q", deref(pr.LastReviewID), deref(pr.LastReviewEvent),
			deref(before.LastReviewID), deref(before.LastReviewEvent))
	}
	if _, ok := h.e.getKV(h.ctx, KVPRManualVerdict(pr.ID)); ok {
		t.Fatal("the operator's approval is marked as the PR's manual verdict")
	}
	if !h.hasEvent("pr:talkable/talkable#2", "review.manual_verdict") {
		t.Error("no review.manual_verdict event")
	}

	h.push(2, "b2")
	h.reviewTo(2, "b2")
	h.tick()
	want := "dismiss:talkable/talkable#2:9001:magnum's review of b2 found blocking problems; this automatic approval is withdrawn ([review](" +
		prURL2 + "#pullrequestreview-502))."
	if d := h.ghCalls("dismiss:"); !slices.Equal(d, []string{want}) {
		t.Fatalf("dismissals = %q\nwant %q", d, want)
	}
	if a := h.autoApproval(2); a.State != store.AutoDismissed || a.EndedBy != store.AutoEndedMagnum {
		t.Fatalf("approval = %+v", a)
	}
}

// The operator's own changes request by hand stops auto-approval of the PR
// ("lift your ✗" on the board), but not A: the operator lifts it on purpose.
// Their approval stands, and the stop stays (magnum still does not approve
// the PR on its own).
func TestApproveAsTheOperatorLiftsTheirOwnChangesRequest(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.gh.addReview(2, github.Review{DatabaseID: 40, State: "CHANGES_REQUESTED", AuthorLogin: "zhuravel", AuthorType: "User",
		Body: "Not like this.", CommitOid: "b0", SubmittedAt: h.clock.Now(), URL: prURL2 + "#pullrequestreview-40"})
	h.reviewedPR(2, "b1")
	h.tick()
	if hd := h.hold(2); !hd.Held || !strings.Contains(hd.Reason, "you requested changes by hand") {
		t.Fatalf("hold = %+v", hd)
	}
	if got := h.created(); len(got) != 0 {
		t.Fatalf("approved on its own: %q", got)
	}

	if r := h.verdictAs(ReqApprove, 2, "zhuravel"); r.State != store.RequestDone {
		t.Fatalf("approve as the operator: %s %q", r.State, deref(r.Result))
	}
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted %q", got)
	}
	h.advance(time.Minute)
	h.tick()
	h.tick()
	if a := h.autoApproval(2); a.State != store.AutoStanding {
		t.Fatalf("approval = %+v", a)
	}
	if hd := h.hold(2); !hd.Held {
		t.Fatalf("the approval lifted the stop: %+v", hd)
	}
}

// --as refuses what it cannot do, saying why, and posts nothing: a watch
// without auto_approve_as (GitHub counts only the operator's approval), an
// identity that is neither the watch's auto_approve_as nor the PR's
// posting identity, a review that found something to fix (auto-approval's
// own preconditions), and a changes request.
func TestApproveAsRefusesWhatItCannotDo(t *testing.T) {
	h, plan := newAutoHarness(t, func(h *harness) { h.cfg.Watches[0].AutoApprove, h.cfg.Watches[0].AutoApproveAs = nil, "" })
	plan.set(2, blockingRound)
	h.reviewedPR(2, "b1")
	h.tick()
	refused := func(name, kind, as, want string) {
		t.Helper()
		if r := h.verdictAs(kind, 2, as); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), want) {
			t.Errorf("%s: %s %q, want %q", name, r.State, deref(r.Result), want)
		}
	}
	refused("no auto_approve_as", ReqApprove, "zhuravel", "GitHub counts only your approval: the watch of talkable/talkable#2 names no auto_approve_as")

	h.cfg.Watches[0].AutoApproveAs = "zhuravel"
	refused("another identity", ReqApprove, "bob-rev", "--as takes zhuravel (the watch's auto_approve_as) or talkable-app (the PR's posting identity)")
	refused("a review with something to fix", ReqApprove, "zhuravel", "talkable/talkable#2 is not approved as zhuravel: magnum's review found something to fix")
	refused("a changes request", ReqRequestChanges, "zhuravel", "--as zhuravel only approves")
	if got := h.created(); len(got) != 0 {
		t.Fatalf("posted %q", got)
	}
}

// --as the PR's posting identity is the manual verdict as before.
func TestApproveAsThePostingIdentityIsTheManualVerdict(t *testing.T) {
	h := newHarness(t, verdictClients)
	h.reviewedPR(2, "b1")
	if r := h.verdictAs(ReqApprove, 2, h.pr(2).Identity); r.State != store.RequestDone {
		t.Fatalf("approve: %s %q", r.State, deref(r.Result))
	}
	if got := h.created(); len(got) != 1 || !strings.Contains(got[0], "<!-- magnum:verdict=APPROVE head=b1 -->") ||
		strings.Contains(got[0], "auto-approval") {
		t.Fatalf("posted %q", got)
	}
	if pr := h.pr(2); deref(pr.LastReviewEvent) != "APPROVED" || deref(pr.LastReviewID) != 9001 {
		t.Fatalf("recorded %q %d", deref(pr.LastReviewEvent), deref(pr.LastReviewID))
	}
}
