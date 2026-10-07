package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// TestStatusShowsInfraPauseDrainAndCodexBudget: status reads what the
// daemon records for an infrastructure pause, a drain and the Codex budget.
func TestStatusShowsInfraPauseDrainAndCodexBudget(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	for k, v := range map[string]string{
		engine.KVInfraPausedUntil:           store.FormatTime(now.Add(4 * time.Minute)),
		engine.KVInfraPausedReason:          "SSH key refused",
		engine.KVInfraPausedDetail:          "git@github.com: Permission denied (publickey).",
		engine.KVDaemonDraining:             store.FormatTime(now.Add(-2 * time.Minute)),
		engine.KVUsageCodexPercent:          "87",
		engine.KVUsageCodexWindow:           "10080",
		engine.KVUsageCodexPlan:             "pro",
		engine.KVUsageCodexResetsAt:         store.FormatTime(now.Add(50 * time.Hour)),
		engine.KVToolPausedUntil("claude"):  store.FormatTime(now.Add(time.Hour)),
		engine.KVToolPausedReason("claude"): engine.BudgetPauseReason,
	} {
		if err := st.SetKV(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Daemon.DrainingSince == nil || r.Codex == nil || r.Codex.Percent != 87 || r.Codex.Soft != 80 || r.Codex.Hard != 95 {
		t.Fatalf("daemon %+v codex %+v", r.Daemon, r.Codex)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	out := b.String()
	actContains(t, out,
		"daemon: draining for a restart since",
		"codex:    87% used of the weekly limit (pro), resets ",
		"at the soft cap 80%: full re-reviews wait",
		"infra: infrastructure failure SSH key refused until",
		"Permission denied (publickey)",
		"magnum probes `git ls-remote` and resumes by itself",
		"claude: "+engine.BudgetPauseReason,
		"once the Codex budget is below [usage] codex_hard",
	)
}

// TestResumeWithoutDaemonClearsModelLimitsAndInfraPause: offline, `resume
// --tool` forgets the kind's model limits and plain `resume` lifts an
// infrastructure pause, as the daemon's handler would.
func TestResumeWithoutDaemonClearsModelLimitsAndInfraPause(t *testing.T) {
	h := newActHarness(t)
	until := store.FormatTime(h.now.Add(time.Hour))
	for k, v := range map[string]string{
		agents.KVModelLimited("claude", "fable"): until,
		agents.KVModelLimits("claude"):           "fable",
		engine.KVInfraPausedUntil:                until,
		engine.KVInfraPausedReason:               "DNS lookup failed",
	} {
		_ = h.st.SetKV(h.ctx, k, v)
	}
	if code := h.cmd("resume", "--tool", "claude"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if got := strings.TrimSpace(h.out.String()); got != "claude had no pause (model limits cleared: claude/fable)" {
		t.Fatalf("resume --tool claude: %q", got)
	}
	for _, k := range []string{agents.KVModelLimited("claude", "fable"), agents.KVModelLimits("claude")} {
		if _, ok := h.kv(k); ok {
			t.Errorf("%s kept", k)
		}
	}
	h.out.Reset()
	if code := h.cmd("resume"); code != 0 || strings.TrimSpace(h.out.String()) != "lifted the infrastructure pause" {
		t.Fatalf("resume: exit %d %q", code, h.out.String())
	}
	if _, ok := h.kv(engine.KVInfraPausedUntil); ok {
		t.Error("infra pause kept")
	}
}
