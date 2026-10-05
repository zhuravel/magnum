package cli

import (
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// approve and request-changes hand the daemon a verdict request for the
// resolved PR with the reviewer's words and --force; without a daemon the
// request waits for it.
func TestVerdictCommandsQueueTheVerdict(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242 // a daemon runs: reviews and verdicts are queued only then
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	if code := h.cmd("approve", "talkable#5", "-m", "Looks right to me."); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if code := h.cmd("request-changes", "5", "--force"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 2 || reqs[0].Kind != engine.ReqApprove || reqs[1].Kind != engine.ReqRequestChanges {
		t.Fatalf("requests %+v", reqs)
	}
	a := actDecode[engine.VerdictPayload](t, reqs[0].Payload)
	r := actDecode[engine.VerdictPayload](t, reqs[1].Payload)
	if a.Repo != "talkable/talkable" || a.Number != 5 || a.Message != "Looks right to me." || a.Force ||
		r.Number != 5 || !r.Force || r.Message != "" {
		t.Fatalf("payloads %+v / %+v", a, r)
	}
	actContains(t, h.out.String(), "the daemon (pid 4242) is running, so the work was queued as request 2 (request_changes)")
}
