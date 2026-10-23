package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// With no daemon, `magnum resume --tool codex` (or all) lifts the codex
// kind's pause for its hooks review as any pause, and lets the hooks.json
// files the daemon told about start new Codex sessions until they change,
// which its answer says; a resume of claude leaves the hold, and a second
// resume overrules nothing more.
func TestTheOfflineResumeOverrulesTheHooksHold(t *testing.T) {
	h := newActHarness(t)
	_ = h.st.SetKV(h.ctx, engine.KVCodexHooksUntrusted, "fp1")
	_ = h.st.SetKV(h.ctx, store.KVToolPausedUntil("codex"), store.FormatTime(h.now.Add(10*time.Minute)))
	_ = h.st.SetKV(h.ctx, store.KVToolPausedReason("codex"), engine.HooksReviewPauseReason)
	_ = h.st.SetKV(h.ctx, store.KVToolPausedDetail("codex"), "1 hook in ~/.codex/hooks.json has no trust entry")
	if code := h.cmd("resume", "--tool", "claude"); code != 0 {
		t.Fatalf("resume claude: exit %d: %s", code, h.errb.String())
	}
	if v, ok := h.kv(engine.KVCodexHooksOverruled); ok {
		t.Fatalf("a resume of claude overruled the hooks hold: %q", v)
	}
	h.out.Reset()
	if code := h.cmd("resume", "--tool", "codex"); code != 0 || strings.TrimSpace(h.out.String()) != "resumed codex ("+engine.HooksOverruledNote+")" {
		t.Fatalf("resume codex: exit %d %q", code, h.out.String())
	}
	if v, _ := h.kv(engine.KVCodexHooksOverruled); v != "fp1" {
		t.Errorf("overruled = %q, want the told hooks.json", v)
	}
	if _, ok := h.kv(store.KVToolPausedReason("codex")); ok {
		t.Error("the hold's pause stayed")
	}
	h.out.Reset()
	if code := h.cmd("resume", "--tool", "all"); code != 0 || strings.Contains(h.out.String(), engine.HooksOverruledNote) {
		t.Fatalf("a second resume: exit %d %q, want nothing more overruled", code, h.out.String())
	}
}

// `magnum status` shows the hooks review's pause as the other pauses, with
// the fix and no end: it ends when the hooks are trusted, and the time the
// daemon renews it to is no end.
func TestStatusShowsTheHooksHoldWithItsFix(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	for k, v := range map[string]string{
		store.KVToolPausedUntil("codex"):  store.FormatTime(now.Add(10 * time.Minute)),
		store.KVToolPausedReason("codex"): engine.HooksReviewPauseReason,
		store.KVToolPausedDetail("codex"): "1 hook in ~/.codex/hooks.json has no trust entry: run `codex` once and choose Trust all",
	} {
		if err := st.SetKV(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	out := b.String()
	actContains(t, out, "codex: "+engine.HooksReviewPauseReason+" — 1 hook in ~/.codex/hooks.json has no trust entry",
		"magnum re-checks every minute", "`magnum resume --tool codex` starts new Codex sessions anyway until hooks.json changes again")
	if strings.Contains(out, engine.HooksReviewPauseReason+" until") {
		t.Errorf("status gives the hold an end:\n%s", out)
	}
}
