package cli

import (
	"context"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
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

// approve --as hands the daemon the identity to approve as (the watch's
// auto_approve_as posts the operator's own approval; the daemon checks
// which); request-changes takes no --as.
func TestApproveAsQueuesTheIdentity(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	if code := h.cmd("approve", "talkable#5", "--as", "zhuravel"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqApprove {
		t.Fatalf("requests %+v", reqs)
	}
	if a := actDecode[engine.VerdictPayload](t, reqs[0].Payload); a.As != "zhuravel" || a.Number != 5 || a.Force {
		t.Fatalf("payload %+v", a)
	}
	if code := h.cmd("request-changes", "5", "--as", "zhuravel"); code == 0 {
		t.Fatal("request-changes took --as")
	}
	if reqs := h.requests(); len(reqs) != 1 {
		t.Fatalf("requests %+v", reqs)
	}
}

// The board's rows that need the operator name whom A approves them as:
// the watch's auto_approve_as, with why the daemon would refuse it now
// (here #11960 and #11961 have no posted round to follow); none on a watch
// without auto_approve_as, and none on a row that does not need them.
func TestTheBoardNamesWhomAApprovesAs(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	seedNeedsMe(t, st)
	ctx := context.Background()
	load := func() map[int]tui.PRBoardRow {
		t.Helper()
		rows, err := prsSource(st, d.Config, store.BoardFilter{}, prsSelfLogins(d.Config), d.Layout)(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int]tui.PRBoardRow{}
		for _, r := range rows {
			out[r.Number] = r
		}
		return out
	}
	for n, r := range load() {
		if r.ApproveAs != nil {
			t.Errorf("#%d without auto_approve_as: approve as %+v", n, r.ApproveAs)
		}
	}

	d.Config.Watches[0].AutoApproveAs = "zhuravel"
	rows := load()
	if a := rows[11963].ApproveAs; a == nil || *a != (tui.ApproveAs{Identity: "zhuravel", Login: "zhuravel"}) {
		t.Errorf("#11963: approve as %+v", a)
	}
	for _, n := range []int{11960, 11961} {
		if a := rows[n].ApproveAs; a == nil || a.Identity != "zhuravel" || a.Refusal != "magnum posted no review of it" {
			t.Errorf("#%d: approve as %+v", n, a)
		}
	}
	for _, n := range []int{11962, 11964, 11920} {
		if a := rows[n].ApproveAs; a != nil {
			t.Errorf("#%d does not need you: approve as %+v", n, a)
		}
	}
}
