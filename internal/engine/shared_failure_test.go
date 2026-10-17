package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// clearedLaunch records what agents records when it closed the pane of PR
// n's launch that never came up: the role and the CLI's last error line.
func clearedLaunch(h *harness, n int, role, line string) {
	h.t.Helper()
	appendPREvent(h, n, agents.EventLaunchCleared, map[string]any{"role": role, "agent": "mg-" + role, "pane": "p1", "last_line": line})
}

// lostPaneClosed records what agents records when it closed the pane of PR n's
// lost session of role, which showed line.
func lostPaneClosed(h *harness, n int, role, line string) {
	h.t.Helper()
	appendPREvent(h, n, agents.EventPanesClosed, map[string]any{"count": 1, "roles": []string{role}, "last_lines": map[string]string{role: line}})
}

func appendPREvent(h *harness, n int, kind string, data map[string]any) {
	h.t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		h.t.Fatal(err)
	}
	subject := prSubject(store.Repo{Owner: "talkable", Name: "talkable"}, n)
	if _, err := h.st.AppendEvent(h.ctx, store.Event{Level: "warn", Subject: &subject, Kind: kind, Message: kind + " in a test", Data: raw}); err != nil {
		h.t.Fatal(err)
	}
}

// refusedPrompt records a run of role on PR n whose prompt herdr refused
// because the agent was gone, with the pane's error line (agents.Prompt).
func refusedPrompt(h *harness, n int, role, line string) {
	h.t.Helper()
	now := h.clock.Now()
	run, err := h.st.CreateRun(h.ctx, store.Run{PRID: h.pr(n).ID, Round: 1, Role: role, Kind: store.RunInitial, TargetSHA: "a1",
		Identity: "talkable-app", ReviewerLogin: magnumApp, State: store.RunPending, PromptText: "go", CreatedAt: now})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.TransitionRun(h.ctx, run.ID, nil, store.RunFailed, func(u *store.RunUpdate) {
		u.Set("ended_at", now)
		u.Set("error", "agents: prompt mg-"+role+": herdr agent.prompt: agent_not_found: agent target not found; the pane shows: "+line)
	}); err != nil {
		h.t.Fatal(err)
	}
}

// On 2026-10-16 four PRs' codex-judge launches died with the same pane line
// ("Error: account/read failed during TUI bootstrap …") within the hour, and
// each PR's attention named only its own failure. Two PRs whose launches end
// with the same line within 30 minutes are one agents.shared_failure warning
// and one toast, naming the PRs, the roles and the line; a third PR the same
// way within 24 h adds nothing, nor do different lines or the same line 31
// minutes apart. A refused prompt's pane line and a lost agent's (the line
// pr.panes_closed keeps) count as a cleared launch's does. The warning and
// the toast name a change of the roles' CLI within the day.
func TestTwoPRsFailingTheSameWayRaiseOneSharedFailure(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b2"}, prSpec{n: 3, head: "c3"})
	h.startup()
	h.tick() // the first sync: the open PRs are a baseline, nothing runs
	const line = "Error: account/read failed during TUI bootstrap: account/read timed out"
	changed := h.clock.Now().Add(-27 * time.Minute)
	if err := writeKVJSON(h.ctx, h.e, KVKindVersion("codex"), "kind version", KindVersion{Version: "codex-cli 0.163.0",
		Path: "/opt/codex/0.163.0/codex", SeenAt: changed, Previous: "codex-cli 0.162.1", ChangedAt: changed}); err != nil {
		t.Fatal(err)
	}
	cliChanged := "; the codex CLI changed at " + changed.Format("15:04") + ", to codex-cli 0.163.0"

	clearedLaunch(h, 1, "codex-judge", line)
	h.tick()
	if evs := h.events(EventSharedFailure); len(evs) != 0 {
		t.Fatalf("one PR is no shared failure: %+v", evs)
	}
	h.advance(20 * time.Minute)
	clearedLaunch(h, 2, "codex-judge", line)
	h.tick()
	evs := h.events(EventSharedFailure)
	if len(evs) != 1 || evs[0].Level != "warn" {
		t.Fatalf("events %+v, want one warning", evs)
	}
	for _, want := range []string{"2 PRs' codex-judge launches end the same way", "talkable#1, talkable#2", line, cliChanged} {
		if !strings.Contains(evs[0].Message, want) {
			t.Errorf("message %q lacks %q", evs[0].Message, want)
		}
	}
	if got := toastsWith(h, "launches end the same way"); len(got) != 1 || !strings.Contains(got[0], line) || !strings.Contains(got[0], cliChanged) {
		t.Fatalf("toasts %q, want one naming the line", h.nh.shown())
	}

	h.advance(time.Hour)
	refusedPrompt(h, 3, "codex-judge", line)
	clearedLaunch(h, 1, "codex-judge", line)
	h.tick()
	clearedLaunch(h, 1, "codex-judge", "Error: one")
	clearedLaunch(h, 2, "codex-judge", "Error: two")
	h.tick()
	clearedLaunch(h, 1, "codex-judge", "Error: three")
	h.advance(31 * time.Minute)
	clearedLaunch(h, 2, "codex-judge", "Error: three")
	h.tick()
	if evs, toasts := h.events(EventSharedFailure), toastsWith(h, "end the same way"); len(evs) != 1 || len(toasts) != 1 {
		t.Fatalf("the same line within 24 h, different lines and a line 31 minutes apart: events %+v, toasts %q", evs, toasts)
	}

	const lost = "Error: unexpected argument '--frobnicate' found"
	h.advance(time.Minute)
	refusedPrompt(h, 2, "codex-judge", lost)
	lostPaneClosed(h, 3, "claude-review", lost)
	h.tick()
	evs = h.events(EventSharedFailure)
	if len(evs) != 2 || !strings.Contains(evs[1].Message, "2 PRs' claude-review and codex-judge launches end the same way") ||
		!strings.Contains(evs[1].Message, "talkable#2, talkable#3") || !strings.Contains(evs[1].Message, lost) {
		t.Fatalf("a refused prompt and a lost agent's pane: events %+v", evs)
	}
}
