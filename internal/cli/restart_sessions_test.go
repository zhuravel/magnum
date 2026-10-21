package cli

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// TestRestartSessionsAsksFirstAndPrintsTheDaemonsAnswer: on a terminal the
// command names the live sessions it restarts and asks y/N; n queues
// nothing, y hands the restart to the daemon and prints its answer, and
// --yes asks nothing.
func TestRestartSessionsAsksFirstAndPrintsTheDaemonsAnswer(t *testing.T) {
	h := newActHarness(t)
	h.pid = 3
	pr := h.seedPR("talkable/talkable", 12, store.PRPaused)
	for _, role := range []string{"codex-judge", "claude-review"} {
		s := h.session(pr.ID, role, store.SessionLive, "mg-"+role, "p-"+role, "w1")
		kind := map[string]string{"codex-judge": "codex", "claude-review": "claude"}[role]
		if err := h.st.UpdateSession(h.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("agent_kind", kind) }); err != nil {
			t.Fatal(err)
		}
	}
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("n\n")
	if code := h.cmd("restart-sessions", "--tool", "codex"); code != 1 {
		t.Fatalf("n: exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "Restart the 1 live codex session? They resume in new processes. [y/N]", "nothing restarted")
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("n queued %+v", reqs)
	}

	answer := "restarting 1 live codex session: 1 park now, 0 after their turn; each resumes in a new process at its next prompt"
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, answer) }
	h.stdin("y\n")
	h.errb.Reset()
	if code := h.cmd("restart-sessions", "--tool", "codex"); code != 0 {
		t.Fatalf("y: exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqRestartSessions {
		t.Fatalf("requests %+v", reqs)
	}
	if p := actDecode[engine.RestartSessionsPayload](t, reqs[0].Payload); p.Tool != "codex" || p.Repo != "" || p.Number != 0 {
		t.Fatalf("payload %+v", p)
	}
	actContains(t, h.out.String(), answer)

	h.stdin("")
	h.errb.Reset()
	if code := h.cmd("restart-sessions", "--tool", "all", "--yes"); code != 0 || strings.Contains(h.errb.String(), "[y/N]") {
		t.Fatalf("--yes: exit %d, asked: %s", code, h.errb.String())
	}
	if reqs := h.requests(); len(reqs) != 2 || actDecode[engine.RestartSessionsPayload](t, reqs[1].Payload).Tool != "all" {
		t.Fatalf("requests %+v", reqs)
	}
}

// TestRestartSessionsOfOnePR: --pr names the PR exactly in the request, and
// the question counts only its sessions.
func TestRestartSessionsOfOnePR(t *testing.T) {
	h := newActHarness(t)
	h.pid = 3
	pr := h.seedPR("talkable/talkable", 12, store.PRPaused)
	other := h.seedPR("talkable/talkable", 13, store.PRReviewed)
	for _, id := range []int64{pr.ID, other.ID} {
		s := h.session(id, "codex-judge", store.SessionLive, "mg-judge-"+string(rune('a'+id)), "p", "w1")
		if err := h.st.UpdateSession(h.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("agent_kind", "codex") }); err != nil {
			t.Fatal(err)
		}
	}
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("y\n")
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "restarting 1 live codex session of talkable/talkable#12: ...")
	}
	if code := h.cmd("restart-sessions", "--tool", "codex", "--pr", "12"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "Restart the 1 live codex session of talkable#12?")
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests %+v", reqs)
	}
	if p := actDecode[engine.RestartSessionsPayload](t, reqs[0].Payload); p.Tool != "codex" || p.Repo != "talkable/talkable" || p.Number != 12 {
		t.Fatalf("payload %+v", p)
	}
}

// TestRestartSessionsNeedsADaemon: with no daemon running nothing is queued
// (a restart must not run later, when nobody waits for it), and a kind that
// is not declared is a usage error.
func TestRestartSessionsNeedsADaemon(t *testing.T) {
	h := newActHarness(t)
	if code := h.cmd("restart-sessions", "--tool", "codex", "--yes"); code != 1 || !strings.Contains(h.errb.String(), "no daemon is running") {
		t.Fatalf("no daemon: exit %d: %s", code, h.errb.String())
	}
	for _, r := range h.requests() {
		if r.State == store.RequestPending {
			t.Fatalf("left a pending request %+v", r)
		}
	}
	h.errb.Reset()
	if code := h.cmd("restart-sessions", "--tool", "gemini"); code != 2 || !strings.Contains(h.errb.String(), `unknown tool "gemini"`) {
		t.Fatalf("unknown tool: exit %d: %s", code, h.errb.String())
	}
	h.errb.Reset()
	if code := h.cmd("restart-sessions"); code != 2 || !strings.Contains(h.errb.String(), "which agent kind?") {
		t.Fatalf("no tool: exit %d: %s", code, h.errb.String())
	}
}
