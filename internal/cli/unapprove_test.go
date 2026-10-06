package cli

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// standingAutoApproval records an automatic approval of pr's head standing
// as review 9001 by zhuravel.
func (h *actHarness) standingAutoApproval(pr store.PR) store.AutoApproval {
	h.t.Helper()
	a, err := h.st.InsertAutoApproval(h.ctx, store.AutoApproval{PRID: pr.ID, RunID: "r1", HeadSHA: pr.HeadSHA, Identity: "zhuravel", Login: "zhuravel"})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.TransitionAutoApproval(h.ctx, a.ID, []string{store.AutoPosting}, store.AutoStanding, func(u *store.AutoApprovalUpdate) {
		u.Set("review_id", int64(9001))
		u.Set("review_url", "https://github.com/talkable/talkable/pull/5#pullrequestreview-9001")
		u.Set("posted_at", h.now)
	}); err != nil {
		h.t.Fatal(err)
	}
	a, _ = h.st.AutoApprovalByID(h.ctx, a.ID)
	return a
}

// unapprove asks y/N on a terminal before it hands the daemon the
// withdrawal (only y confirms), naming the approval that stands; --yes and a
// script do not ask; --resume asks before it lets magnum approve again.
func TestUnapproveAsksThenHandsTheDaemonTheRequest(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.standingAutoApproval(pr)
	h.d.StdinTTY, h.d.StdoutTTY = true, true

	h.stdin("\n")
	if code := h.cmd("unapprove", "talkable#5"); code != 1 {
		t.Fatalf("exit %d after Enter: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "Withdraw your automatic approval of talkable#5 (review 9001 on abc1234) and stop magnum approving it as you?")
	actContains(t, h.errb.String(), "nothing withdrawn")
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("queued without a y: %+v", reqs)
	}

	h.stdin("y\n")
	if code := h.cmd("unapprove", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	h.d.StdinTTY = false // a script: no question
	if code := h.cmd("unapprove", "5", "--resume"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 2 || reqs[0].Kind != engine.ReqUnapprove || reqs[1].Kind != engine.ReqUnapprove {
		t.Fatalf("requests %+v", reqs)
	}
	w := actDecode[engine.UnapprovePayload](t, reqs[0].Payload)
	r := actDecode[engine.UnapprovePayload](t, reqs[1].Payload)
	if w.Repo != "talkable/talkable" || w.Number != 5 || w.Resume || r.Number != 5 || !r.Resume {
		t.Fatalf("payloads %+v / %+v", w, r)
	}
}

// Without an approval standing, unapprove still stops auto-approval of the
// PR, and its question says so.
func TestUnapproveWithoutAnApprovalAsksToStopIt(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("y\n")
	if code := h.cmd("unapprove", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "No automatic approval of talkable#5 stands. Stop magnum approving it as you?")
	h.stdin("y\n")
	if code := h.cmd("unapprove", "5", "--resume"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "Let magnum approve talkable#5 as you again after a clean review?")
	if !strings.Contains(h.out.String(), "queued as request 2") {
		t.Errorf("output: %s", h.out.String())
	}
}
