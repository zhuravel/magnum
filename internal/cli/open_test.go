package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/reveal"
	"github.com/zhuravel/magnum/internal/store"
)

func TestOpenRoleLabelsAndAliases(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-codex-judge", "p_1", "w_1")
	h.session(pr.ID, store.RoleClaude, store.SessionLive, "mg-talkable-5-claude-review", "p_2", "w_1")
	h.session(pr.ID, store.RoleSimplify, store.SessionLive, "mg-talkable-5-claude-simplify", "p_4", "w_1")
	for role, want := range map[string]string{
		"codex-judge": "mg-talkable-5-codex-judge", "judge": "mg-talkable-5-codex-judge",
		"claude-review": "mg-talkable-5-claude-review", "claude": "mg-talkable-5-claude-review", "Claude-Review": "mg-talkable-5-claude-review",
		"claude-simplify": "mg-talkable-5-claude-simplify", "simplify": "mg-talkable-5-claude-simplify",
	} {
		h.hd.focused = nil
		h.out.Reset()
		if code := h.cmd("open", "5", "--role", role, "--no-reveal"); code != 0 {
			t.Fatalf("--role %s: exit %d: %s", role, code, h.errb.String())
		}
		if strings.Join(h.hd.focused, ",") != want {
			t.Errorf("--role %s focused %v, want %s", role, h.hd.focused, want)
		}
	}
	h.out.Reset()
	if code := h.cmd("open", "5", "--role", "claude", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "talkable#5 claude-review: focused mg-talkable-5-claude-review")
}

func TestOpenFocusesTheAgentThenReveals(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	h.session(pr.ID, store.RoleClaude, store.SessionLive, "mg-talkable-5-claude", "p_2", "w_1")

	if code := h.cmd("open", "talkable#5", "--role", "claude", "--new-window"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if strings.Join(h.hd.focused, ",") != "mg-talkable-5-claude" {
		t.Fatalf("focused %v", h.hd.focused)
	}
	if len(h.reveals) != 1 || !h.reveals[0].NewWindow {
		t.Fatalf("reveals %+v", h.reveals)
	}
	actContains(t, h.out.String(), "talkable#5 claude-review: focused mg-talkable-5-claude")

	h.out.Reset()
	if code := h.cmd("open", "5", "--no-reveal", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var got actFocusResult
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, h.out.String())
	}
	if got.PR != "talkable#5" || got.Agent != "mg-talkable-5-judge" || got.PaneID != "p_1" || got.Reveal != nil || len(h.reveals) != 1 {
		t.Fatalf("json result %+v (reveals %d)", got, len(h.reveals))
	}
}

func TestOpenCodexPaneFallsBackToTheJudge(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	h.session(pr.ID, store.RoleCodexReview, store.SessionLive, "", "p_3", "w_1")
	h.hd.focusErr = map[string]error{"p_3": &herdr.Error{Method: "agent.focus", Code: herdr.CodeAgentNotFound, Message: "no agent"}}
	if code := h.cmd("open", "5", "--role", "codex"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if strings.Join(h.hd.focused, ",") != "mg-talkable-5-judge" {
		t.Fatalf("focused %v", h.hd.focused)
	}
	actContains(t, h.out.String(), "focused codex-judge beside the codex-review pane", string(reveal.ActionLaunched))
}

func TestOpenParkedPRWithoutDaemonPrintsTheManualResume(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	s := h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	if err := h.st.TransitionSession(h.ctx, s.ID, nil, store.SessionParked, func(u *store.SessionUpdate) {
		u.Set("session_id", "019a-uuid")
		u.Set("cwd", "/Users/x/Projects/talkable.review3")
		u.Set("agent_kind", "codex")
	}); err != nil {
		t.Fatal(err)
	}
	if code := h.cmd("open", "5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "talkable#5 is parked and no daemon is running", "resume by hand: cd /Users/x/Projects/talkable.review3 && codex resume 019a-uuid")
	// Nothing stays queued to restore and pin the PR whenever a daemon starts.
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("requests %+v", reqs)
	}
}

func TestOpenParkedPRWaitsForTheDaemon(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	s := h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	_ = h.st.TransitionSession(h.ctx, s.ID, nil, store.SessionParked, func(u *store.SessionUpdate) { u.Set("session_id", "u1") })
	h.pid = 99
	h.onSleep = func(h *actHarness) {
		if h.sleeps == 2 {
			h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_9", "w_9")
		}
	}
	if code := h.cmd("open", "5", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if strings.Join(h.hd.focused, ",") != "mg-talkable-5-judge" {
		t.Fatalf("focused %v", h.hd.focused)
	}
	p := actDecode[engine.OpenPayload](t, h.requests()[0].Payload)
	if p.Repo != "talkable/talkable" || p.Number != 5 || p.Role != store.RoleJudge {
		t.Fatalf("open payload %+v", p)
	}
	actContains(t, h.errb.String(), "asked the daemon to restore its sessions")
}

func TestOpenParkedPRWhenTheDaemonCannotRestore(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	s := h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	_ = h.st.TransitionSession(h.ctx, s.ID, nil, store.SessionParked, func(u *store.SessionUpdate) { u.Set("session_id", "u1") })
	h.pid = 99
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestFailed, `unknown request kind "open"`) }
	if code := h.cmd("open", "5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "this daemon cannot restore parked sessions yet", "magnum review talkable#5", "codex resume u1")
}

func TestOpenWithoutSessionsAndUsage(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRQueued)
	if code := h.cmd("open", "5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "talkable#5 has no codex-judge session yet", "magnum review talkable#5")
	h.errb.Reset()
	if code := h.cmd("open", "5", "--role", "nope"); code != 2 || !strings.Contains(h.errb.String(), `unknown role "nope"`) {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	h.errb.Reset()
	if code := h.cmd("open", "99"); code != 1 || !strings.Contains(h.errb.String(), "magnum review 99") {
		t.Fatalf("unknown PR: exit %d: %s", code, h.errb.String())
	}
}
