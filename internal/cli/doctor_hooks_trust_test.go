package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// doctorTrustCheck runs doctor on the Codex home home and returns its one
// "codex hooks trust" check.
func doctorTrustCheck(t *testing.T, d doctorDeps, ag *fakeDoctorAgents, home string) doctorCheck {
	t.Helper()
	ag.codexHome = home
	var out []doctorCheck
	for _, c := range doctorRun(context.Background(), d) {
		if c.Name == "codex hooks trust" {
			out = append(out, c)
		}
	}
	if len(out) != 1 {
		t.Fatalf("codex hooks trust checks = %+v, want one", out)
	}
	return out[0]
}

// Codex shows its hooks review for any hook of hooks.json whose place has no
// trust entry, not only herdr's SessionStart hook (spec 122's check), and
// on 2026-10-23 the other hooks brought a second wave. Doctor warns per
// Codex home of the Codex session roles how many hooks have no trust entry
// and which, with the fix; with every hook trusted it passes, and a home
// without hooks.json is skipped. It reads, whatever herdr's session source.
func TestDoctorWarnsPerCodexHomeOfHooksWithoutATrustEntry(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	home := codexHookHome(t, []hookSpec{{herdrHook, 10}, {moshiHook, 5}}, trustedAt("session_start:0:0"))
	c := doctorTrustCheck(t, d, ag, home)
	file := filepath.Join(home, "hooks.json")
	if c.Status != doctorWarn ||
		!strings.Contains(c.Detail, "2 hooks in "+file+" have no trust entry (moshi-hook at session_start:1:0, herdr-agent-state.sh at stop:0:0; used by codex-judge)") ||
		!strings.Contains(c.Detail, "new Codex sessions wait") || c.Fix != "run `codex` once in a terminal and choose Trust all" {
		t.Fatalf("check = %s %q fix %q, want a WARN naming both hooks", c.Status, c.Detail, c.Fix)
	}

	home = codexHookHome(t, []hookSpec{{herdrHook, 10}, {moshiHook, 5}}, trustedAt("session_start:0:0", "session_start:1:0", "stop:0:0"))
	if c := doctorTrustCheck(t, d, ag, home); c.Status != doctorPass ||
		!strings.Contains(c.Detail, "the 3 hooks in "+filepath.Join(home, "hooks.json")+" have trust entries (used by codex-judge)") {
		t.Fatalf("trusted = %s %q, want PASS", c.Status, c.Detail)
	}

	if c := doctorTrustCheck(t, d, ag, t.TempDir()); c.Status != doctorSkip || !strings.Contains(c.Detail, "hooks.json") {
		t.Fatalf("no hooks.json = %s %q, want SKIP", c.Status, c.Detail)
	}
}
