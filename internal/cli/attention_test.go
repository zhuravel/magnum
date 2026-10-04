package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

func TestAttentionJumpsToTheMostUrgentPane(t *testing.T) {
	h := newActHarness(t)
	done := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	blocked := h.seedPR("talkable/talkable", 6, store.PRReviewing)
	needs := h.seedPR("talkable/talkable", 7, store.PRNeedsAttention)
	h.session(done.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_5", "w_5")
	h.session(blocked.ID, store.RoleClaude, store.SessionLive, "mg-talkable-6-claude", "p_6", "w_6")
	h.session(needs.ID, store.RoleJudge, store.SessionLive, "mg-talkable-7-judge", "p_7", "w_7")
	h.hd.snap = herdr.Snapshot{Agents: []herdr.AgentInfo{
		{Name: "mg-talkable-5-judge", PaneID: "p_5", AgentStatus: herdr.StatusDone},
		{Name: "mg-talkable-6-claude", PaneID: "p_6", AgentStatus: herdr.StatusBlocked},
		{Name: "mg-talkable-7-judge", PaneID: "p_7", AgentStatus: herdr.StatusIdle},
		{Name: "bohdan-own", PaneID: "p_x", AgentStatus: herdr.StatusBlocked},
	}}
	if code := h.cmd("attention"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if strings.Join(h.hd.focused, ",") != "mg-talkable-6-claude" || len(h.reveals) != 1 {
		t.Fatalf("focused %v reveals %d", h.hd.focused, len(h.reveals))
	}
	actContains(t, h.out.String(), "talkable#6 claude-review (blocked): focused mg-talkable-6-claude", "2 more: `magnum attention --list`")

	h.out.Reset()
	if code := h.cmd("attention", "--list"); code != 0 {
		t.Fatalf("list exit %d", code)
	}
	out := h.out.String()
	i6, i7, i5 := strings.Index(out, "talkable#6"), strings.Index(out, "talkable#7"), strings.Index(out, "talkable#5")
	if i6 < 0 || i7 < i6 || i5 < i7 || strings.Contains(out, "bohdan-own") {
		t.Fatalf("list order (blocked, needs_attention, done; magnum panes only):\n%s", out)
	}
}

func TestAttentionFailedRoundAndParkedPR(t *testing.T) {
	h := newActHarness(t)
	failed := h.seedPR("talkable/talkable", 8, store.PRReviewed)
	h.session(failed.ID, store.RoleJudge, store.SessionLive, "mg-talkable-8-judge", "p_8", "w_8")
	run, err := h.st.CreateRun(h.ctx, store.Run{PRID: failed.ID, Round: 1, Role: store.RoleJudge, Kind: store.RunInitial,
		TargetSHA: failed.HeadSHA, State: store.RunPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionRun(h.ctx, run.ID, nil, store.RunFailed, func(u *store.RunUpdate) {
		u.Set("outcome", "timeout")
		u.Set("error", "judge_timeout after 90m")
	}); err != nil {
		t.Fatal(err)
	}
	h.hd.snap = herdr.Snapshot{Agents: []herdr.AgentInfo{{Name: "mg-talkable-8-judge", PaneID: "p_8", AgentStatus: herdr.StatusIdle}}}
	if code := h.cmd("attention", "--no-reveal", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var res actFocusResult
	if err := json.Unmarshal(h.out.Bytes(), &res); err != nil || res.Reason != "failed" || res.PR != "talkable#8" || len(h.reveals) != 0 {
		t.Fatalf("result %+v err %v reveals %d", res, err, len(h.reveals))
	}

	// A PR needing attention without a live pane cannot be focused.
	h2 := newActHarness(t)
	h2.setPR(h2.seedPR("talkable/talkable", 9, store.PRReviewed).ID, store.PRNeedsAttention, func(u *store.PRUpdate) {
		u.Set("last_error", "identity_error: App lacks pull_requests: write")
	})
	h2.now = h2.now.Add(time.Hour)
	if code := h2.cmd("attention"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h2.out.String(), "talkable#9 needs_attention: identity check failed: App lacks pull_requests: write",
		"magnum open talkable#9", "fix: `magnum identities check`")
}

func TestAttentionNothingAndHerdrDown(t *testing.T) {
	h := newActHarness(t)
	if code := h.cmd("attention"); code != 1 || strings.TrimSpace(h.out.String()) != "nothing needs you" {
		t.Fatalf("exit %d out %q", code, h.out.String())
	}
	h.hd.snapErr = herdr.ErrUnavailable
	if code := h.cmd("attention"); code != 1 || !strings.Contains(h.errb.String(), "herdr is not running") {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if code := h.cmd("attention", "extra"); code != 2 {
		t.Fatalf("usage exit %d", code)
	}
}

// A stale agent name on the most urgent item must not hide the next one.
func TestAttentionSkipsAPaneThatIsGone(t *testing.T) {
	h := newActHarness(t)
	blocked := h.seedPR("talkable/talkable", 6, store.PRReviewing)
	needs := h.seedPR("talkable/talkable", 7, store.PRNeedsAttention)
	h.session(blocked.ID, store.RoleClaude, store.SessionLive, "mg-talkable-6-claude", "p_6", "w_6")
	h.session(needs.ID, store.RoleJudge, store.SessionLive, "mg-talkable-7-judge", "p_7", "w_7")
	h.hd.snap = herdr.Snapshot{Agents: []herdr.AgentInfo{
		{Name: "mg-talkable-6-claude", PaneID: "p_6", AgentStatus: herdr.StatusBlocked},
		{Name: "mg-talkable-7-judge", PaneID: "p_7", AgentStatus: herdr.StatusIdle},
	}}
	h.hd.focusErr = map[string]error{"mg-talkable-6-claude": &herdr.Error{Code: herdr.CodeAgentNotFound, Message: "no such agent"}}
	if code := h.cmd("attention"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if strings.Join(h.hd.focused, ",") != "mg-talkable-7-judge" {
		t.Fatalf("focused %v", h.hd.focused)
	}
	actContains(t, h.errb.String(), "skipped talkable#6")
	// Any other focus failure still fails.
	h.hd.focusErr = map[string]error{"mg-talkable-6-claude": herdr.ErrUnavailable}
	if code := h.cmd("attention"); code != 1 {
		t.Fatalf("herdr down: exit %d", code)
	}
}
