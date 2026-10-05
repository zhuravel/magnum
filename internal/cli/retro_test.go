package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

const retroStarted = "retro started on 2 PR(s); `magnum misses` lists what it finds"

// retroClosed seeds PR n of full as merged: a retro looks at closed PRs only.
func (h *actHarness) retroClosed(full string, n int) store.PR {
	h.t.Helper()
	pr := h.seedPR(full, n, store.PRReleased)
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) }); err != nil {
		h.t.Fatal(err)
	}
	pr.GHState = store.GHMerged
	return pr
}

func TestRetroQueuesThePayloadWithResolvedPRIDsAndFlags(t *testing.T) {
	h := newActHarness(t)
	a := h.retroClosed("talkable/talkable", 5)
	b := h.retroClosed("zhuravel/widgets", 7)
	h.pid = 4242
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, retroStarted) }

	// "5" and talkable#5 name one PR: it is sent once, in the order typed.
	if code := h.cmd("retro", "zhuravel/widgets#7", "5", "talkable#5", "--again", "--lookback", "14d"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqRetro {
		t.Fatalf("requests = %+v", reqs)
	}
	p := actDecode[engine.RetroPayload](t, reqs[0].Payload)
	if len(p.PRs) != 2 || p.PRs[0] != b.ID || p.PRs[1] != a.ID || !p.Again || p.Lookback != "14d" {
		t.Fatalf("payload = %+v (want PRs [%d %d])", p, b.ID, a.ID)
	}
	if h.kicks != 1 {
		t.Errorf("kicks = %d", h.kicks)
	}
	actContains(t, h.out.String(), retroStarted, "follow it with `magnum logs`; `magnum misses` lists the results")
}

func TestRetroWithoutArgumentsQueuesTheDefaultRetro(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "retro started on the PRs closed within 7d; `magnum misses` lists what it finds")
	}
	if code := h.cmd("retro"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	if got := string(reqs[0].Payload); got != "{}" {
		t.Errorf("payload = %s, want {} (every PR due, [learn] lookback)", got)
	}
}

func TestRetroUnknownRefQueuesNothing(t *testing.T) {
	h := newActHarness(t)
	h.retroClosed("talkable/talkable", 5)
	h.pid = 4242
	if code := h.cmd("retro", "talkable#5", "talkable#99"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "talkable#99 is not in the registry")
	if n := len(h.requests()); n != 0 || h.kicks != 0 {
		t.Errorf("%d requests queued, %d kicks", n, h.kicks)
	}
	h.errb.Reset()
	if code := h.cmd("retro", "not a ref!"); code != 1 || !strings.Contains(h.errb.String(), "not a ref!") {
		t.Fatalf("malformed ref: exit %d: %s", code, h.errb.String())
	}
	if len(h.requests()) != 0 {
		t.Error("a malformed ref queued a request")
	}
}

// TestRetroOpenRefQueuesNothing: a retro looks at merged or closed PRs
// only, so naming an open one is an error instead of a retro that quietly
// skips it.
func TestRetroOpenRefQueuesNothing(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.pid = 4242
	if code := h.cmd("retro", "talkable#5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "talkable#5 is still open")
	if n := len(h.requests()); n != 0 || h.kicks != 0 {
		t.Errorf("%d requests queued, %d kicks", n, h.kicks)
	}
}

func TestRetroBadLookbackQueuesNothing(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	for _, bad := range []string{"soon", "0d", "-3h", "7", "d"} {
		h.errb.Reset()
		if code := h.cmd("retro", "--lookback", bad); code != 2 {
			t.Errorf("--lookback %q: exit %d, want 2", bad, code)
		}
		actContains(t, h.errb.String(), "--lookback", "want a positive duration such as 14d or 36h", "usage: magnum retro")
	}
	if n := len(h.requests()); n != 0 || h.kicks != 0 {
		t.Errorf("%d requests queued, %d kicks", n, h.kicks)
	}
}

func TestRetroWithoutDaemonExitsOneAndKeepsTheRequestQueued(t *testing.T) {
	h := newActHarness(t)
	h.pid = 0
	if code := h.cmd("retro", "--again"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "no daemon is running", "request 1 stays queued", actDaemonFix)
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].State != store.RequestPending || reqs[0].Kind != engine.ReqRetro {
		t.Fatalf("requests = %+v", reqs)
	}
	if strings.Contains(h.out.String(), "magnum misses") {
		t.Errorf("the hint follows a retro that did not start:\n%s", h.out.String())
	}
}

func TestRetroFailedRequestExitsOne(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestFailed, "no retro now: draining for a restart (magnum daemon-restart --drain)")
	}
	if code := h.cmd("retro"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "no retro now: draining for a restart")
	if h.out.Len() != 0 {
		t.Errorf("stdout = %q", h.out.String())
	}
}

func TestRetroAnswerThatNeverComesIsReportedAsQueued(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	if code := h.cmd("retro"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "queued as request 1 (retro)", "the daemon (pid 4242) is running", "`magnum logs request:1 -f`")
	if strings.Contains(h.out.String(), "magnum misses") {
		t.Errorf("the hint follows a retro that may not have started:\n%s", h.out.String())
	}
}

func TestRetroAlreadyRunningStillPointsAtTheResults(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "a retro is already running (started 12:00); `magnum logs` follows it")
	}
	if code := h.cmd("retro"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "a retro is already running", "`magnum misses` lists the results")
}

func TestRetroJSONCarriesPayloadAndRequest(t *testing.T) {
	h := newActHarness(t)
	pr := h.retroClosed("talkable/talkable", 5)
	h.pid = 4242
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, retroStarted) }
	if code := h.cmd("retro", "5", "--lookback", "36h", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var got struct {
		Payload engine.RetroPayload `json:"payload"`
		Request actRequestJSON      `json:"request"`
		Error   string              `json:"error"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, h.out.String())
	}
	if len(got.Payload.PRs) != 1 || got.Payload.PRs[0] != pr.ID || got.Payload.Lookback != "36h" || got.Payload.Again {
		t.Errorf("payload = %+v", got.Payload)
	}
	if got.Request.ID != 1 || got.Request.Kind != engine.ReqRetro || got.Request.State != store.RequestDone ||
		got.Request.Result != retroStarted || got.Request.DaemonPID != 4242 || got.Error != "" {
		t.Errorf("request = %+v, error %q", got.Request, got.Error)
	}
	if strings.Contains(h.out.String(), "follow it with") {
		t.Errorf("--json printed the hint:\n%s", h.out.String())
	}
}

func TestRetroJSONReportsFailuresAndAMissingDaemon(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestFailed, "no retro now: draining") }
	if code := h.cmd("retro", "--json"); code != 1 {
		t.Fatalf("failed request: exit %d", code)
	}
	var got retroJSON
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil || got.Error != "no retro now: draining" || got.Request == nil || got.Request.State != store.RequestFailed {
		t.Fatalf("failed request: %v\n%s", err, h.out.String())
	}

	h.out.Reset()
	h.pid, h.onSleep = 0, nil
	if code := h.cmd("retro", "--json"); code != 1 {
		t.Fatalf("no daemon: exit %d", code)
	}
	got = retroJSON{}
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil || !strings.Contains(got.Error, actDaemonFix) || got.Request == nil || got.Request.DaemonPID != 0 {
		t.Fatalf("no daemon: %v\n%s", err, h.out.String())
	}
}

func TestRetroHelpAndRegistration(t *testing.T) {
	c, out, _ := bareContext(t)
	if code := execute(c, []string{"retro", "--help"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, out.String(), "magnum retro [<ref>...] [--again] [--lookback <duration>] [--json]", "magnum pause", "[learn] enabled", "drains", "magnum misses")
	cmd, _, err := newRoot(c).Find([]string{"retro"})
	if err != nil || cmd.GroupID != groupAct {
		t.Fatalf("retro group = %q (%v)", cmd.GroupID, err)
	}
}
