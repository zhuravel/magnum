package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// doctorKindArgs sets a kind's args in the fixture's config.
func doctorKindArgs(t *testing.T, d doctorDeps, kind string, args ...string) {
	t.Helper()
	k, ok := d.Config.Kinds[kind]
	if !ok {
		t.Fatalf("no kind %s in the fixture config", kind)
	}
	k.Args = args
	d.Config.Kinds[kind] = k
}

// A plain codex or claude binary without its autonomy flag stops at every
// approval prompt (which magnum answers No) and a sandboxed judge cannot
// post: doctor fails it and names the flag to add.
func TestDoctorFailsAPlainBinaryWithoutAutonomyFlags(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	ag.wrapper["codex"], ag.wrapper["claude"] = false, false
	doctorKindArgs(t, d, "codex")
	doctorKindArgs(t, d, "claude", "--verbose")

	m := doctorByName(doctorAgentChecks(context.Background(), d))
	for kind, flag := range map[string]string{"codex": "--dangerously-bypass-approvals-and-sandbox", "claude": "--dangerously-skip-permissions"} {
		c := m[kind+" wrapper"]
		if c.Status != doctorFail || !strings.Contains(c.Fix, `args = ["`+flag+`"]`) || !strings.Contains(c.Detail, "without "+flag) {
			t.Errorf("%s wrapper = %s %q fix %q, want FAIL naming %s", kind, c.Status, c.Detail, c.Fix, flag)
		}
	}
	if c := m["codex wrapper"]; !strings.Contains(c.Detail, "codex-judge") || strings.Contains(c.Detail, "codex-review") {
		t.Errorf("codex wrapper detail = %q, want the session role only (codex-review is a shell command)", c.Detail)
	}
	if c := m["claude wrapper"]; !strings.Contains(c.Detail, "claude-review") || !strings.Contains(c.Detail, "claude-simplify") {
		t.Errorf("claude wrapper detail = %q, want both claude roles", c.Detail)
	}
}

func TestDoctorPassesAPlainBinaryWithAutonomyFlags(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	ag.wrapper["codex"], ag.wrapper["claude"] = false, false
	// codex keeps the built-in default; claude gets the flag from every role
	// in its two-arg form instead of from the kind.
	doctorKindArgs(t, d, "claude")
	for i, r := range d.Config.Roles {
		if r.Kind == config.KindClaude {
			d.Config.Roles[i].Args = []string{"--permission-mode", "bypassPermissions"}
		}
	}
	m := doctorByName(doctorAgentChecks(context.Background(), d))
	for _, kind := range []string{"codex", "claude"} {
		if c := m[kind+" wrapper"]; c.Status != doctorPass || !strings.Contains(c.Detail, "plain binary") {
			t.Errorf("%s wrapper = %s %q fix %q, want PASS", kind, c.Status, c.Detail, c.Fix)
		}
	}
	// One role without the flag is enough to fail.
	for i, r := range d.Config.Roles {
		if r.Name == "claude-simplify" {
			d.Config.Roles[i].Args = nil
		}
	}
	if c := doctorByName(doctorAgentChecks(context.Background(), d))["claude wrapper"]; c.Status != doctorFail ||
		!strings.Contains(c.Detail, "claude-simplify") || strings.Contains(c.Detail, "claude-review") {
		t.Errorf("claude wrapper = %s %q, want FAIL for claude-simplify only", c.Status, c.Detail)
	}
}

// A wrapper adds its own flags, and a kind that only a shell role uses as
// its tool is never started as an interactive session.
func TestDoctorAutonomyNeedsNoFlagsForWrappersOrShellTools(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	doctorKindArgs(t, d, "codex")
	doctorKindArgs(t, d, "claude")
	if c := doctorByName(doctorAgentChecks(context.Background(), d))["codex wrapper"]; c.Status != doctorPass || !strings.Contains(c.Detail, "wrapper function") {
		t.Errorf("codex wrapper with a zsh function = %s %q", c.Status, c.Detail)
	}

	ag.wrapper["codex"] = false
	var roles []config.Role
	for _, r := range d.Config.Roles {
		if r.Kind == config.KindCodex { // drop the codex judge; codex-review stays (tool codex)
			continue
		}
		roles = append(roles, r)
	}
	roles = append(roles, config.Role{Name: "claude-judge", Kind: config.KindClaude, Judge: true})
	d.Config.Roles = roles
	d.Config.Normalize()
	if c := doctorByName(doctorAgentChecks(context.Background(), d))["codex wrapper"]; c.Status != doctorPass || !strings.Contains(c.Detail, "no session role") {
		t.Errorf("codex wrapper with only a shell role = %s %q", c.Status, c.Detail)
	}
}

func TestDoctorAutonomous(t *testing.T) {
	flags := doctorAutonomy[config.KindClaude]
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"--dangerously-skip-permissions"}, true},
		{[]string{"--name", "t", "--permission-mode", "bypassPermissions"}, true},
		{[]string{"--permission-mode=bypassPermissions"}, true},
		{[]string{"--permission-mode", "plan"}, false},
		{[]string{"bypassPermissions", "--permission-mode"}, false},
		{nil, false},
	} {
		if got := doctorAutonomous(c.argv, flags); got != c.want {
			t.Errorf("doctorAutonomous(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
	// The built-in args satisfy the check (they are its first entries).
	for kind, flags := range doctorAutonomy {
		k := config.DefaultKinds()[kind]
		if !doctorAutonomous(k.Argv(config.LaunchArgs{}), flags) || k.Args[0] != flags[0][0] {
			t.Errorf("%s: the default args %q do not carry the autonomy flag %q", kind, k.Args, flags[0])
		}
	}
}
