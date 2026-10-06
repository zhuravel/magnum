package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

// The retro is off unless asked for, and its defaults are the committed
// config.defaults.toml's: loading the file and a config without the section
// give the same values.
func TestLearnDefaults(t *testing.T) {
	want := Learn{
		DailyAt:         "07:00",
		Lookback:        Duration{7 * 24 * time.Hour},
		Settle:          Duration{24 * time.Hour},
		MaxPRs:          20,
		MinCommentChars: 20,
		Kind:            "claude",
		Model:           "sonnet",
		Prompt:          "retro.md",
		Timeout:         Duration{20 * time.Minute},
	}
	if got := Defaults().Learn; !reflect.DeepEqual(got, want) {
		t.Fatalf("built-in learn = %+v, want %+v", got, want)
	}
	root := repoRoot(t)
	explicit, err := LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.defaults.toml"), LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(explicit.Learn, want) {
		t.Fatalf("config.defaults.toml learn = %+v, want %+v", explicit.Learn, want)
	}
}

// daily_at is "HH:MM" and nothing else: two digits each, 00:00 to 23:59.
func TestLearnDailyTimeParsesStrictClockTimes(t *testing.T) {
	day := time.Date(2026, time.October, 5, 15, 45, 30, 123, time.UTC)
	for _, tc := range []struct {
		at         string
		hour, min  int
		wantFailed bool
	}{
		{at: "07:00", hour: 7},
		{at: "00:00"},
		{at: "23:59", hour: 23, min: 59},
		{at: "12:30", hour: 12, min: 30},
		{at: "7:00", wantFailed: true},
		{at: "07:0", wantFailed: true},
		{at: "24:00", wantFailed: true},
		{at: "07:60", wantFailed: true},
		{at: "", wantFailed: true},
		{at: "0700", wantFailed: true},
		{at: "07-00", wantFailed: true},
		{at: "07:00:00", wantFailed: true},
		{at: " 07:00", wantFailed: true},
		{at: "+7:00", wantFailed: true},
		{at: "-1:00", wantFailed: true},
		{at: "ab:cd", wantFailed: true},
	} {
		t.Run(tc.at, func(t *testing.T) {
			got, err := Learn{DailyAt: tc.at}.DailyTime(day)
			if tc.wantFailed {
				if err == nil {
					t.Fatalf("DailyTime(%q) = %v, want an error", tc.at, got)
				}
				wantError(t, err, "learn.daily_at", "HH:MM")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := time.Date(2026, time.October, 5, tc.hour, tc.min, 0, 0, time.UTC); !got.Equal(want) {
				t.Fatalf("DailyTime(%q) = %v, want %v", tc.at, got, want)
			}
		})
	}
}

// The time is on the calendar day of the argument, in the argument's own
// location, whatever time of day the argument carries.
func TestLearnDailyTimeIsOnTheCallersLocalDay(t *testing.T) {
	zone := time.FixedZone("UTC+3", 3*3600)
	l := Learn{DailyAt: "07:00"}
	for _, day := range []time.Time{
		time.Date(2026, time.October, 5, 0, 0, 0, 0, zone),
		time.Date(2026, time.October, 5, 23, 59, 59, 0, zone),
	} {
		got, err := l.DailyTime(day)
		if err != nil {
			t.Fatal(err)
		}
		if want := time.Date(2026, time.October, 5, 7, 0, 0, 0, zone); !got.Equal(want) || got.Location() != zone {
			t.Fatalf("DailyTime(%v) = %v (%v), want %v", day, got, got.Location(), want)
		}
	}
}

func TestValidateLearn(t *testing.T) {
	badPrompts := t.TempDir()
	if err := os.WriteFile(filepath.Join(badPrompts, "broken.md"), []byte("{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string // "" = valid
	}{
		{name: "defaults", edit: func(*Config) {}},
		{name: "enabled defaults", edit: func(c *Config) { c.Learn.Enabled = true }},
		{name: "another daily_at", edit: func(c *Config) { c.Learn.DailyAt = "23:59" }},
		{name: "codex kind", edit: func(c *Config) { c.Learn.Kind = KindCodex }},
		{name: "empty model and effort", edit: func(c *Config) { c.Learn.Model, c.Learn.Effort = "", "" }},
		{name: "bad daily_at", edit: func(c *Config) { c.Learn.DailyAt = "7:00" }, want: `learn.daily_at "7:00": want HH:MM`},
		{name: "daily_at out of range", edit: func(c *Config) { c.Learn.DailyAt = "24:00" }, want: `learn.daily_at "24:00"`},
		{name: "no daily_at", edit: func(c *Config) { c.Learn.DailyAt = "" }, want: `learn.daily_at ""`},
		{name: "undeclared kind", edit: func(c *Config) { c.Learn.Kind = "nope" }, want: `learn.kind "nope" is not a declared agent kind`},
		{name: "no kind", edit: func(c *Config) { c.Learn.Kind = "" }, want: `learn.kind ""`},
		{name: "shell kind", edit: func(c *Config) { c.Learn.Kind = KindShell }, want: `learn.kind "shell" is not a declared agent kind`},
		{name: "missing prompt", edit: func(c *Config) { c.Learn.Prompt = "nope.md" }, want: "learn.prompt: prompt nope.md: prompt not found"},
		{name: "no prompt", edit: func(c *Config) { c.Learn.Prompt = "" }, want: "learn.prompt must name a prompt file"},
		{name: "prompt with a path", edit: func(c *Config) { c.Learn.Prompt = "../retro.md" }, want: "learn.prompt"},
		{name: "prompt that does not parse", edit: func(c *Config) {
			c.Pipeline.PromptsDir, c.Learn.Prompt = badPrompts, "broken.md"
		}, want: "learn.prompt broken.md"},
		{name: "zero lookback", edit: func(c *Config) { c.Learn.Lookback = Duration{} }, want: "learn.lookback must be positive, got 0s"},
		{name: "negative lookback", edit: func(c *Config) { c.Learn.Lookback = Duration{-time.Hour} }, want: "learn.lookback must be positive, got -1h0m0s"},
		{name: "zero settle", edit: func(c *Config) { c.Learn.Settle = Duration{} }},
		{name: "settle just under lookback", edit: func(c *Config) { c.Learn.Settle = Duration{7*24*time.Hour - time.Minute} }},
		{name: "negative settle", edit: func(c *Config) { c.Learn.Settle = Duration{-time.Hour} }, want: "learn.settle must not be negative, got -1h0m0s"},
		{name: "settle as long as lookback", edit: func(c *Config) { c.Learn.Settle = Duration{7 * 24 * time.Hour} },
			want: "learn.settle must be shorter than learn.lookback (168h0m0s), got 168h0m0s"},
		{name: "settle past a shorter lookback", edit: func(c *Config) { c.Learn.Lookback = Duration{12 * time.Hour} },
			want: "learn.settle must be shorter than learn.lookback (12h0m0s), got 24h0m0s"},
		{name: "zero timeout", edit: func(c *Config) { c.Learn.Timeout = Duration{} }, want: "learn.timeout must be positive, got 0s"},
		{name: "negative timeout", edit: func(c *Config) { c.Learn.Timeout = Duration{-time.Minute} }, want: "learn.timeout must be positive, got -1m0s"},
		{name: "zero max_prs", edit: func(c *Config) { c.Learn.MaxPRs = 0 }, want: "learn.max_prs must be positive, got 0"},
		{name: "negative max_prs", edit: func(c *Config) { c.Learn.MaxPRs = -3 }, want: "learn.max_prs must be positive, got -3"},
		{name: "zero min_comment_chars", edit: func(c *Config) { c.Learn.MinCommentChars = 0 }, want: "learn.min_comment_chars must be positive, got 0"},
		{name: "negative min_comment_chars", edit: func(c *Config) { c.Learn.MinCommentChars = -1 }, want: "learn.min_comment_chars must be positive, got -1"},
		// a forced `magnum retro` runs whether or not the schedule is on
		{name: "disabled is checked too", edit: func(c *Config) { c.Learn.Enabled, c.Learn.MaxPRs = false, 0 }, want: "learn.max_prs must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.edit(cfg)
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid: %v", err)
				}
				return
			}
			wantError(t, err, tc.want)
		})
	}
}

// Every bad key is reported in one pass, not one at a time.
func TestValidateLearnReportsEveryProblem(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Learn = Learn{}
	wantError(t, cfg.Validate(), "learn.daily_at", "learn.kind", "learn.prompt", "learn.lookback", "learn.timeout", "learn.max_prs", "learn.min_comment_chars")
}

// A misspelled key under [learn] is refused, like everywhere else: it would
// otherwise silently leave the retro on its default.
func TestLearnUnknownKeyIsRejected(t *testing.T) {
	_, err := loadCommittedWithLocal(t, testLocalConfig+`
[learn]
enabeld = true
`)
	wantError(t, err, "unknown keys", "learn.enabeld")
}

// A user config sets [learn] key by key: the keys it leaves out keep their
// defaults.
func TestLearnOverlayKeepsTheKeysItDoesNotSet(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+`
[learn]
enabled = true
daily_at = "06:30"
lookback = "3d"
include_bots = true
model = "haiku"
effort = "low"
args = ["--search"]
`)
	if err != nil {
		t.Fatal(err)
	}
	want := Learn{
		Enabled:         true,
		DailyAt:         "06:30",
		Lookback:        Duration{72 * time.Hour},
		Settle:          Duration{24 * time.Hour},
		MaxPRs:          20,
		MinCommentChars: 20,
		IncludeBots:     true,
		Kind:            "claude",
		Model:           "haiku",
		Effort:          "low",
		Args:            []string{"--search"},
		Prompt:          "retro.md",
		Timeout:         Duration{20 * time.Minute},
	}
	if !reflect.DeepEqual(cfg.Learn, want) {
		t.Fatalf("learn = %+v, want %+v", cfg.Learn, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
}

// A user config that says nothing about [learn] leaves the retro off, on its
// defaults.
func TestLearnOverlayWithoutTheSectionKeepsDefaults(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig)
	if err != nil {
		t.Fatal(err)
	}
	if want := DefaultLearn(); !reflect.DeepEqual(cfg.Learn, want) {
		t.Fatalf("learn = %+v, want %+v", cfg.Learn, want)
	}
	if cfg.Learn.Enabled {
		t.Fatal("the daily retro is on without being asked for")
	}
}

// The classifying agent runs as an ordinary session role named retro, taking
// its kind, model, effort, args and timeout from [learn], and writes retro.json
// itself.
func TestLearnRoleFollowsTheSection(t *testing.T) {
	cfg := validPipelineConfig()
	r := cfg.LearnRole()
	if r.Name != "retro" || LearnRoleName != "retro" {
		t.Fatalf("name = %q", r.Name)
	}
	if r.Kind != "claude" || r.Mode != ModeSession || !r.IsAgent() || r.IsShell() || r.Judge {
		t.Fatalf("role = %+v, want a claude session role", r)
	}
	if r.Model != "sonnet" || r.Effort != "" || r.Args != nil || r.Timeout.Duration != 20*time.Minute {
		t.Fatalf("model, effort, args, timeout = %q, %q, %v, %s", r.Model, r.Effort, r.Args, r.Timeout.Duration)
	}
	if r.Output != "retro.json" || r.Capture != CaptureFile || r.Runs != RunsAlways || r.Prompt != "retro.md" {
		t.Fatalf("output, capture, runs, prompt = %q, %q, %q, %q", r.Output, r.Capture, r.Runs, r.Prompt)
	}

	cfg.Learn.Kind, cfg.Learn.Model, cfg.Learn.Effort = KindCodex, "cheap", "low"
	cfg.Learn.Args, cfg.Learn.Timeout, cfg.Learn.Prompt = []string{"--search"}, Duration{5 * time.Minute}, "mine.md"
	r = cfg.LearnRole()
	if r.Kind != KindCodex || r.Mode != ModeSession || r.Model != "cheap" || r.Effort != "low" ||
		!slices.Equal(r.Args, []string{"--search"}) || r.Timeout.Duration != 5*time.Minute || r.Prompt != "mine.md" {
		t.Fatalf("role = %+v, want codex cheap low --search 5m mine.md", r)
	}
	if r.Output != "retro.json" || r.Capture != CaptureFile {
		t.Fatalf("output, capture = %q, %q", r.Output, r.Capture)
	}
	r.Args[0] = "--changed"
	if cfg.Learn.Args[0] != "--search" {
		t.Fatal("the role shares its args with the [learn] section")
	}
}

// The role passes the same checks a configured role does, so the pipeline can
// drive it like one.
func TestLearnRoleIsAValidRole(t *testing.T) {
	cfg := validPipelineConfig()
	if errs := cfg.validateRole(cfg.LearnRole(), cfg.kinds(), map[string]bool{}); len(errs) > 0 {
		t.Fatalf("the retro role is invalid: %v", errs)
	}
}
