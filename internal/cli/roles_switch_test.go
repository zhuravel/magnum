package cli

import (
	"strings"
	"testing"
)

// TestRolesKindsShowModelSwitch: every kind says what happens when one of
// its models hits its own limit: the switch command with its fallbacks, or
// "-" for a kind that cannot switch (the limit pauses it).
func TestRolesKindsShowModelSwitch(t *testing.T) {
	f := newRolesFixture(t)
	if code := f.run("roles", "--kinds"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.Err.String())
	}
	lines := map[string]string{}
	for _, section := range strings.Split(f.Out.String(), "\n\n") {
		kind, _, _ := strings.Cut(section, " ")
		for _, line := range strings.Split(section, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "model switch:"); ok {
				lines[kind] = strings.TrimSpace(v)
			}
		}
	}
	if len(lines) != 4 {
		t.Fatalf("model switch lines = %v\n%s", lines, f.Out.String())
	}
	if lines["codex"] != "- (a model limit pauses the kind)" {
		t.Errorf("codex: %q", lines["codex"])
	}
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	k, _ := f.Ctx.Config.KindSpec("claude")
	if k.SwitchModel == "" {
		t.Fatal("claude has no switch_model by default")
	}
	want := k.SwitchModel + ", fallbacks " + inspOrDash(strings.Join(k.FallbackModels, ", ")) +
		", default_model " + inspOrDash(k.DefaultModel) + ", reset_model " + inspOrDash(k.ResetModel)
	if lines["claude"] != want {
		t.Errorf("claude: %q, want %q", lines["claude"], want)
	}
}
