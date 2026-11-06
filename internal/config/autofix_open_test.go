package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

// autofix = "open" is the third value of [pipeline] autofix and of a
// [[watch]]'s: a loop starts on anything open once the reviewers settle.
// Before it existed the key took only "never" and "own".
func TestAutofixOpenIsValid(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"pipeline": func(c *Config) { c.Pipeline.Autofix = AutofixOpen },
		"watch":    func(c *Config) { c.Watches[0].Autofix = AutofixOpen },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			edit(cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate = %v", err)
			}
			if got := cfg.AutofixFor(&cfg.Watches[0]); got != AutofixOpen {
				t.Errorf("AutofixFor = %q, want open", got)
			}
		})
	}
	cfg := validPipelineConfig()
	cfg.Pipeline.Autofix = "sometimes"
	wantError(t, cfg.Validate(), `pipeline: autofix must be never, own or open, got "sometimes"`)
	if !AutofixStarts(AutofixOwn) || !AutofixStarts(AutofixOpen) || AutofixStarts(AutofixNever) || AutofixStarts("") {
		t.Error("AutofixStarts must be true for own and open only")
	}
}

// A watch with autofix = "open" starts the fix job by itself as one with
// "own" does, so the same warnings name its fix job's problems.
func TestAutofixOpenWarnsAsOwnDoes(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Watches[0].FixRole = new("nope-fix")
	if got := fixWarningsAbout(cfg, "nope-fix"); len(got) != 0 {
		t.Fatalf("Warnings with autofix never = %q, want none", got)
	}
	cfg.Watches[0].Autofix = AutofixOpen
	got := fixWarningsAbout(cfg, "nope-fix")
	if len(got) != 1 || !strings.Contains(got[0], "watch acme") || !strings.Contains(got[0], `autofix = "open"`) {
		t.Fatalf("Warnings with autofix open = %q, want one naming watch acme and autofix open", got)
	}
}

// fixWarningsAbout is the warnings of cfg that mention what.
func fixWarningsAbout(cfg *Config, what string) []string {
	var got []string
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, what) {
			got = append(got, w)
		}
	}
	return got
}

// The settle keys bound autofix = "open": each bad value is refused with
// the key named.
func TestTheSettleKeysAreValidated(t *testing.T) {
	cases := map[string]struct {
		edit func(*Config)
		want string
	}{
		"settle_max below settle_quiet": {func(c *Config) {
			c.Pipeline.SettleQuiet, c.Pipeline.SettleMax = Duration{10 * time.Minute}, Duration{5 * time.Minute}
		},
			"pipeline: settle_max must be at least settle_quiet"},
		"a negative settle_quiet": {func(c *Config) { c.Pipeline.SettleQuiet = Duration{-time.Minute} }, "pipeline: settle_quiet must not be negative"},
		"autofix_max_loops 0":     {func(c *Config) { c.Pipeline.AutofixMaxLoops = 0 }, "pipeline: autofix_max_loops must be from 1 to 20, got 0"},
		"autofix_max_loops 21":    {func(c *Config) { c.Pipeline.AutofixMaxLoops = 21 }, "pipeline: autofix_max_loops must be from 1 to 20, got 21"},
		"magnum_late_after 10m": {func(c *Config) { c.Pipeline.MagnumLateAfter = Duration{10 * time.Minute} },
			"pipeline: magnum_late_after must be 0 (off) or at least 30m, got 10m"},
		"hands_off_after_push 25h": {func(c *Config) { c.Pipeline.HandsOffAfterPush = Duration{25 * 60 * time.Minute} },
			"pipeline: hands_off_after_push must be from 0 to 24h, got 25h"},
		"too many settle_checks": {func(c *Config) { c.Pipeline.SettleChecks = make([]string, 21) },
			"pipeline: settle_checks holds at most 20 names"},
		"an empty settle check": {func(c *Config) { c.Pipeline.SettleChecks = []string{""} },
			"pipeline: settle_checks: a name must be 1 to 100 characters"},
		"a settle check with a newline": {func(c *Config) { c.Pipeline.SettleChecks = []string{"lint\nci"} },
			"pipeline: settle_checks: a name must be 1 to 100 characters"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.edit(cfg)
			wantError(t, cfg.Validate(), tc.want)
		})
	}
	for name, edit := range map[string]func(*Config){
		"magnum_late_after off": func(c *Config) { c.Pipeline.MagnumLateAfter = Duration{} },
		"no hands off":          func(c *Config) { c.Pipeline.HandsOffAfterPush = Duration{} },
		"settle_max = settle_quiet": func(c *Config) {
			c.Pipeline.SettleQuiet, c.Pipeline.SettleMax = Duration{time.Minute}, Duration{time.Minute}
		},
		"a settle check": func(c *Config) { c.Pipeline.SettleChecks = []string{"pronto", "CI / lint"} },
	} {
		t.Run("valid/"+name, func(t *testing.T) {
			cfg := validPipelineConfig()
			edit(cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
}

// quiet_logins name GitHub logins (a bot's with its "[bot]"), and
// quiet_markers are texts that never start with magnum's own marker: magnum's
// own items are never a reviewer's.
func TestQuietLoginsAndMarkersAreValidated(t *testing.T) {
	cases := map[string]struct {
		edit func(*Config)
		want string
	}{
		"a login with a space": {func(c *Config) { c.Pipeline.QuietLogins = []string{"example bot"} },
			`pipeline: quiet_logins: "example bot" is not a GitHub login`},
		"an empty marker": {func(c *Config) { c.Pipeline.QuietMarkers = []string{""} },
			"pipeline: quiet_markers: a marker must be 1 to 200 characters"},
		"magnum's own marker": {func(c *Config) { c.Pipeline.QuietMarkers = []string{"<!-- magnum:fix -->"} },
			`pipeline: quiet_markers: "<!-- magnum:fix -->" is magnum's own marker`},
		"too many markers": {func(c *Config) { c.Pipeline.QuietMarkers = slicesRepeat("<!-- receipt -->", 21) },
			"pipeline: quiet_markers holds at most 20 markers"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.edit(cfg)
			wantError(t, cfg.Validate(), tc.want)
		})
	}
	cfg := validPipelineConfig()
	cfg.Pipeline.QuietLogins = []string{"example-bot", "review-tool[bot]"}
	cfg.Pipeline.QuietMarkers = []string{"<!-- codex-review-receipt:v1 -->"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
}

// slicesRepeat is n copies of s.
func slicesRepeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// The built-in defaults, as the embedded config.defaults.toml loads them,
// keep today's behaviour and carry the settle keys' values.
func TestTheDefaultsCarryTheSettleKeys(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(file, []byte(minimalConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(paths.Layout{Home: dir}, file, LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Pipeline
	if p.Autofix != AutofixNever || p.SettleQuiet.Duration != 3*time.Minute || p.SettleMax.Duration != 20*time.Minute || len(p.SettleChecks) != 0 ||
		p.MagnumLateAfter.Duration != 120*time.Minute || p.AutofixMaxLoops != 3 || p.HandsOffAfterPush.Duration != 30*time.Minute ||
		len(p.QuietLogins) != 0 || len(p.QuietMarkers) != 0 {
		t.Errorf("defaults: autofix %q, settle %s/%s %v, late %s, loops %d, hands off %s, quiet %v %v", p.Autofix, p.SettleQuiet, p.SettleMax,
			p.SettleChecks, p.MagnumLateAfter, p.AutofixMaxLoops, p.HandsOffAfterPush, p.QuietLogins, p.QuietMarkers)
	}
	if d := Defaults().Pipeline; d.SettleQuiet != p.SettleQuiet || d.AutofixMaxLoops != p.AutofixMaxLoops || d.MagnumLateAfter != p.MagnumLateAfter {
		t.Errorf("Defaults() = %+v, want the file's values", d)
	}
}
