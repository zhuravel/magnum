package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

func (h *actHarness) kv(key string) (string, bool) {
	h.t.Helper()
	v, ok, err := h.st.GetKV(h.ctx, key)
	if err != nil {
		h.t.Fatal(err)
	}
	return v, ok
}

func TestPauseAndResumeWithoutDaemon(t *testing.T) {
	h := newActHarness(t)
	// An expired timed pause is left over: the new pause must replace it.
	_ = h.st.SetKV(h.ctx, engine.KVDaemonPausedUntil, store.FormatTime(h.now.Add(-time.Hour)))
	if code := h.cmd("pause", "--for", "2h", "--reason", "lunch"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if v, _ := h.kv(engine.KVDaemonPaused); v != "1" {
		t.Errorf("paused = %q", v)
	}
	if v, _ := h.kv(engine.KVDaemonPausedReason); v != "lunch" {
		t.Errorf("reason = %q", v)
	}
	if v, _ := h.kv(engine.KVDaemonPausedUntil); v != store.FormatTime(h.now.Add(2*time.Hour)) {
		t.Errorf("until = %q", v)
	}
	if len(h.requests()) != 0 {
		t.Errorf("queued a request without a daemon: %+v", h.requests())
	}
	actContains(t, h.out.String(), "paused automation until", "(lunch)", "magnum resume")

	h.out.Reset()
	if code := h.cmd("pause"); code != 0 {
		t.Fatalf("pause again: exit %d", code)
	}
	for _, k := range []string{engine.KVDaemonPausedReason, engine.KVDaemonPausedUntil} {
		if v, ok := h.kv(k); ok {
			t.Errorf("an indefinite pause kept %s = %q", k, v)
		}
	}

	h.out.Reset()
	if code := h.cmd("resume"); code != 0 {
		t.Fatalf("resume exit %d", code)
	}
	for _, k := range []string{engine.KVDaemonPaused, engine.KVDaemonPausedReason, engine.KVDaemonPausedUntil} {
		if _, ok := h.kv(k); ok {
			t.Errorf("%s still set", k)
		}
	}
	h.out.Reset()
	if code := h.cmd("resume"); code != 0 || strings.TrimSpace(h.out.String()) != "automation was not paused" {
		t.Fatalf("second resume: exit %d %q", code, h.out.String())
	}

	// Tool and watch pauses.
	seedToolPause := func(tool string) {
		_ = h.st.SetKV(h.ctx, engine.KVToolPausedUntil(tool), store.FormatTime(h.now.Add(time.Hour)))
		_ = h.st.SetKV(h.ctx, engine.KVToolPausedReason(tool), "usage_limit")
		_ = h.st.SetKV(h.ctx, store.KVToolPausedDetail(tool), "You've hit your usage limit")
		// The pause toast was sent: it is deduplicated for an hour.
		if ok, err := h.st.ShouldSend(h.ctx, "pause:"+tool+":usage_limit", time.Hour); err != nil || !ok {
			t.Fatalf("seed toast: %v %v", ok, err)
		}
	}
	seedToolPause("codex")
	h.out.Reset()
	if code := h.cmd("resume", "--tool", "codex"); code != 0 || strings.TrimSpace(h.out.String()) != "resumed codex" {
		t.Fatalf("resume codex: exit %d %q", code, h.out.String())
	}
	for _, k := range []string{engine.KVToolPausedUntil("codex"), engine.KVToolPausedReason("codex"), store.KVToolPausedDetail("codex")} {
		if _, ok := h.kv(k); ok {
			t.Errorf("%s kept", k)
		}
	}
	// The next codex pause toasts at once.
	if ok, err := h.st.ShouldSend(h.ctx, "pause:codex:usage_limit", time.Hour); err != nil || !ok {
		t.Errorf("toast dedup not reset: %v %v", ok, err)
	}
	h.out.Reset()
	if code := h.cmd("resume", "--tool", "codex"); code != 0 || strings.TrimSpace(h.out.String()) != "codex had no pause" {
		t.Fatalf("resume codex again: exit %d %q", code, h.out.String())
	}

	seedToolPause("claude")
	_ = h.st.SetKV(h.ctx, engine.KVWatchPaused("talkable"), "identity_leak")
	h.out.Reset()
	if code := h.cmd("resume", "--tool", "all"); code != 0 || !strings.Contains(h.out.String(), "resumed claude") {
		t.Fatalf("resume tool: exit %d %q", code, h.out.String())
	}
	for _, k := range []string{engine.KVToolPausedUntil("claude"), engine.KVToolPausedReason("claude"), store.KVToolPausedDetail("claude")} {
		if _, ok := h.kv(k); ok {
			t.Errorf("%s kept", k)
		}
	}
	if code := h.cmd("resume", "--watch", "Talkable"); code != 0 {
		t.Fatalf("resume watch exit %d", code)
	}
	if _, ok := h.kv(engine.KVWatchPaused("talkable")); ok {
		t.Error("watch pause kept")
	}
	if code := h.cmd("resume", "--tool", "codex", "--watch", "talkable"); code != 2 {
		t.Errorf("--tool with --watch: exit %d", code)
	}
}

// With a daemon running, pause and resume go through its request handler
// (the engine's transitions); the CLI writes nothing itself.
func TestPauseAndResumeThroughTheDaemon(t *testing.T) {
	h := newActHarness(t)
	h.pid = 3
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, "resumed codex, claude") }
	start := h.now // the harness clock moves on while the CLI waits
	if code := h.cmd("pause", "--for", "2h", "--reason", "lunch"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if _, ok := h.kv(engine.KVDaemonPaused); ok {
		t.Error("the CLI wrote the pause although the daemon runs")
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqPause {
		t.Fatalf("requests %+v", reqs)
	}
	p := actDecode[engine.PausePayload](t, reqs[0].Payload)
	if p.Reason != "lunch" || p.Until == nil || !p.Until.Equal(start.Add(2*time.Hour)) {
		t.Fatalf("payload %+v", p)
	}
	actContains(t, h.out.String(), "paused automation until", "(lunch)")

	h.out.Reset()
	if code := h.cmd("resume", "--tool", "all"); code != 0 {
		t.Fatalf("resume exit %d: %s", code, h.errb.String())
	}
	reqs = h.requests()
	if len(reqs) != 2 || reqs[1].Kind != engine.ReqResume {
		t.Fatalf("requests %+v", reqs)
	}
	if p := actDecode[engine.PausePayload](t, reqs[1].Payload); p.Tool != "all" || p.Watch != "" {
		t.Fatalf("resume payload %+v", p)
	}
	actContains(t, h.out.String(), "resumed codex, claude")

	// A refused request fails the command with the daemon's reason.
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestFailed, `no [[watch]] for "nobody"`) }
	if code := h.cmd("resume", "--watch", "nobody"); code != 1 || !strings.Contains(h.errb.String(), "no [[watch]]") {
		t.Fatalf("refused resume: exit %d %s", code, h.errb.String())
	}
}

func TestPauseUsage(t *testing.T) {
	h := newActHarness(t)
	if code := h.cmd("pause", "--for", "1h", "--until", "15:00"); code != 2 {
		t.Errorf("both: exit %d", code)
	}
	if code := h.cmd("pause", "--until", "tomorrow"); code != 2 || !strings.Contains(h.errb.String(), "want HH:MM") {
		t.Errorf("bad until: exit %d %s", code, h.errb.String())
	}
	if code := h.cmd("resume", "--tool", "gemini"); code != 2 {
		t.Errorf("bad tool: exit %d", code)
	}
	now := time.Date(2026, 10, 3, 16, 0, 0, 0, time.Local)
	got, err := pauseParseUntil("15:30", now)
	if err != nil || !got.Equal(time.Date(2026, 10, 4, 15, 30, 0, 0, time.Local)) {
		t.Errorf("15:30 after 16:00 = %v, %v (want tomorrow)", got, err)
	}
	// No daemon: the pause still applies (it is kv) and says when.
	if code := h.cmd("pause"); code != 0 || !strings.Contains(h.errb.String(), "no daemon is running; the change applies when it starts") {
		t.Errorf("no daemon: exit %d %s", code, h.errb.String())
	}
}
