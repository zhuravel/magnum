package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// writeHomeTrust writes home's hooks.json with herdr's SessionStart hook
// and hook, a PreToolUse hook's command, and a config.toml that trusts
// herdr's hook and, when trusted, the other one too.
func writeHomeTrust(t *testing.T, home, hook string, trusted bool) {
	t.Helper()
	file := filepath.Join(home, "hooks.json")
	hooks := `{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"bash '/Users/x/.codex/herdr-agent-state.sh' session","timeout":10}]}],` +
		`"PreToolUse":[{"hooks":[{"type":"command","command":"` + hook + `"}]}]}}`
	if err := os.WriteFile(file, []byte(hooks), 0o600); err != nil {
		t.Fatal(err)
	}
	state := "[hooks.state.\"" + file + ":session_start:0:0\"]\ntrusted_hash = \"sha256:aa\"\n"
	if trusted {
		state += "[hooks.state.\"" + file + ":pre_tool_use:0:0\"]\ntrusted_hash = \"sha256:bb\"\n"
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hooksHarness is a harness whose Codex roles read home, with PR #1 synced
// and PR #2 at b1 queued (its quiet period not over yet).
func hooksHarness(t *testing.T, home string) *harness {
	t.Helper()
	h := newHarness(t, func(h *harness) { h.d.CodexHome = func(config.Role) string { return home } })
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	return h
}

// wantHooksHold fails unless the codex kind is paused for its hooks review
// and PR #2 waits for it, saying why and the command that lifts it.
func wantHooksHold(t *testing.T, h *harness, detail string) {
	t.Helper()
	p, ok := h.e.toolPause(h.ctx, agents.KindCodex)
	if !ok || p.Reason != HooksReviewPauseReason || !p.Until.After(h.clock.Now()) || !strings.Contains(p.Detail, detail) {
		t.Fatalf("codex pause = %+v, %v; want %s naming %q", p, ok, HooksReviewPauseReason, detail)
	}
	h.wantState(2, store.PRQueued)
	w := waitOf(t, h, 2)
	sentence := w.Sentence("talkable#2", h.clock.Now())
	if w.Reason != WaitKind || w.Subject != agents.KindCodex || !w.Until.IsZero() ||
		!strings.Contains(sentence, "codex paused ("+HooksReviewPauseReason+"): "+detail) ||
		!strings.Contains(sentence, "run `codex` once and choose Trust all") ||
		!strings.HasSuffix(sentence, "`magnum resume --tool codex` lifts the pause") {
		t.Fatalf("wait = %+v (%q), want the hooks review with no end", w, sentence)
	}
}

// On 2026-10-23 a second hooks-review wave ran for 50 minutes: magnum
// answered "Trust all" 13 times over 5 sessions, Codex saved no trust, first
// prompts stalled, and five rounds started in between. Now, while a hook of
// the hooks.json of a Codex home a session role reads has no trust entry,
// the codex kind is paused (hooks_review): no round that needs Codex
// starts, with one codex.hooks_untrusted warning and one toast per
// hooks.json; `magnum resume --tool codex` lets them start until hooks.json
// changes again, and trusted hooks lift the pause by themselves.
func TestAHookWithoutATrustEntryHoldsNewCodexSessionsUntilResume(t *testing.T) {
	home := t.TempDir()
	writeHomeTrust(t, home, "/opt/homebrew/bin/moshi-hook codex-hook", false)
	h := hooksHarness(t, home)
	h.advance(5 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 0)
	wantHooksHold(t, h, "1 hook in $CODEX_HOME/hooks.json has no trust entry")
	evs := h.events(EventCodexHooksUntrusted)
	if len(evs) != 1 || evs[0].Level != "warn" {
		t.Fatalf("events = %+v, want one warning", evs)
	}
	for _, want := range []string{"Codex hooks need review: 1 hook in $CODEX_HOME/hooks.json has no trust entry (moshi-hook at pre_tool_use:0:0)",
		"run `codex` once and choose Trust all", "New Codex sessions wait", "`magnum resume --tool codex`"} {
		if !strings.Contains(evs[0].Message, want) {
			t.Errorf("message %q lacks %q", evs[0].Message, want)
		}
	}
	if strings.Contains(evs[0].Message, home) {
		t.Errorf("message %q names the Codex home's path (a pane env value)", evs[0].Message)
	}
	toasts := toastsWith(h, "Codex hooks need review")
	if len(toasts) != 1 || !strings.Contains(toasts[0], "Codex hooks need review: run `codex` once and choose Trust all. New Codex sessions wait.") {
		t.Fatalf("toasts = %q, want one", h.nh.shown())
	}

	h.advance(10 * time.Minute) // past the pause's first end: the check renews it
	h.tick()
	h.advance(time.Minute)
	h.tick()
	reqWantRounds(t, h, 0)
	wantHooksHold(t, h, "1 hook in $CODEX_HOME/hooks.json has no trust entry")
	if evs, toasts := h.events(EventCodexHooksUntrusted), toastsWith(h, "Codex hooks need review"); len(evs) != 1 || len(toasts) != 1 {
		t.Fatalf("the same hooks.json again: %d events, %d toasts, want one of each", len(evs), len(toasts))
	}
	if evs := h.events("tool.resumed"); len(evs) != 0 {
		t.Fatalf("the hold ended on its own while the hooks had no trust entry: %+v", evs)
	}

	res, err := h.e.requestPause(h.ctx, PausePayload{Tool: agents.KindCodex}, false)
	if err != nil || res != "resumed codex ("+HooksOverruledNote+")" {
		t.Fatalf("resume = %q, %v", res, err)
	}
	h.tick()
	reqWantRounds(t, h, 1)
	h.advance(time.Minute)
	h.tick()
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
		t.Fatalf("paused again under the resume: %+v", p)
	}

	writeHomeTrust(t, home, "/opt/homebrew/bin/moshi-hook codex-hook --v2", false) // changed, still without a trust entry
	h.advance(time.Minute)
	h.tick()
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); !ok || p.Reason != HooksReviewPauseReason {
		t.Fatalf("a changed hooks.json under the resume: pause %+v, %v; want the hold again", p, ok)
	}
	if _, ok := kvValue(h, KVCodexHooksOverruled); ok {
		t.Error("the resume's mark outlived the hooks.json it was for")
	}
	if evs, toasts := h.events(EventCodexHooksUntrusted), toastsWith(h, "Codex hooks need review"); len(evs) != 2 || len(toasts) != 2 {
		t.Fatalf("a changed hooks.json: %d events, %d toasts, want a second of each", len(evs), len(toasts))
	}

	writeHomeTrust(t, home, "/opt/homebrew/bin/moshi-hook codex-hook --v2", true) // the operator ran codex and chose Trust all
	h.advance(time.Minute)
	h.tick()
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
		t.Fatalf("trusted hooks kept the pause %+v", p)
	}
	back := h.events("tool.resumed")
	if len(back) == 0 || !strings.Contains(back[len(back)-1].Message, "codex pause ("+HooksReviewPauseReason+") ended") {
		t.Fatalf("tool.resumed events = %+v", back)
	}
	if _, ok := kvValue(h, KVCodexHooksUntrusted); ok {
		t.Error("the told mark stayed after every hook got a trust entry")
	}
}

// Hooks that all have trust entries hold nothing: the round starts after its
// quiet period, with no warning, no toast and no pause.
func TestTrustedCodexHooksStartCodexSessionsAtOnce(t *testing.T) {
	home := t.TempDir()
	writeHomeTrust(t, home, "/opt/homebrew/bin/moshi-hook codex-hook", true)
	h := hooksHarness(t, home)
	h.advance(5 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
		t.Fatalf("codex paused: %+v", p)
	}
	if evs, toasts := h.events(EventCodexHooksUntrusted), toastsWith(h, "Codex hooks need review"); len(evs) != 0 || len(toasts) != 0 {
		t.Fatalf("events %+v, toasts %q", evs, toasts)
	}
}

// The hold leaves a pause of another reason alone (it says when it ends),
// and once that pause ends the hold takes its place.
func TestTheHooksHoldLeavesAPauseOfAnotherReasonAlone(t *testing.T) {
	home := t.TempDir()
	writeHomeTrust(t, home, "/opt/homebrew/bin/moshi-hook codex-hook", false)
	h := newHarness(t, func(h *harness) { h.d.CodexHome = func(config.Role) string { return home } })
	until := h.clock.Now().Add(30 * time.Minute)
	h.e.setToolPause(h.ctx, agents.KindCodex, "usage_limit", "usage limit reached", until)
	h.startup()
	h.tick()
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); !ok || p.Reason != "usage_limit" || !p.Until.Equal(until) {
		t.Fatalf("pause = %+v, %v; want the usage limit's", p, ok)
	}
	if evs := h.events(EventCodexHooksUntrusted); len(evs) != 1 {
		t.Fatalf("events = %+v, want the warning all the same", evs)
	}
	h.advance(31 * time.Minute)
	h.tick()
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); !ok || p.Reason != HooksReviewPauseReason {
		t.Fatalf("pause after the usage limit = %+v, %v; want the hold", p, ok)
	}
}

// The CLI's resume with no daemon running writes the same mark as the
// daemon's: the hooks.json the daemon told about no longer holds new
// sessions when it starts.
func TestTheOfflineResumesMarkOverrulesTheHooksHold(t *testing.T) {
	home := t.TempDir()
	writeHomeTrust(t, home, "/opt/homebrew/bin/moshi-hook codex-hook", false)
	h := newHarness(t, func(h *harness) { h.d.CodexHome = func(config.Role) string { return home } })
	h.startup()
	h.tick()
	if _, ok := h.e.toolPause(h.ctx, agents.KindCodex); !ok {
		t.Fatal("no hold")
	}
	h.e.clearToolPause(h.ctx, agents.KindCodex) // as the CLI's resume does
	if ok, err := OverruleHooksHold(h.ctx, h.st); err != nil || !ok {
		t.Fatalf("OverruleHooksHold = %v, %v", ok, err)
	}
	h.advance(time.Minute)
	h.tick()
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
		t.Fatalf("paused despite the CLI's mark: %+v", p)
	}
}
