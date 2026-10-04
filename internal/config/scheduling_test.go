package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

func intp(n int) *int { return &n }

// schedulingConfig is a two-watch config: the first watch sets every new
// per-watch key, the second sets none of them.
const schedulingConfig = `
[daemon]
request_debounce = "90s"
rereview_min_lines = 45
rereview_max_wait = "3h"
max_rounds_per_pr_per_day = 20

[[identity]]
name = "z"
kind = "gh"
login = "z"

[[watch]]
owner = "acme"
include = ["*"]
identity = "z"
rereview_min_lines = 0
rereview_max_wait = "45m"
request_teams = ["reviewers", "backend-team"]

[[watch]]
owner = "example"
include = ["*"]
identity = "z"
`

func TestSchedulingDefaults(t *testing.T) {
	d := Defaults().Daemon
	if d.RequestDebounce.Duration != time.Minute {
		t.Errorf("request_debounce = %s, want 1m", d.RequestDebounce)
	}
	if d.RereviewMinLines != 30 {
		t.Errorf("rereview_min_lines = %d, want 30", d.RereviewMinLines)
	}
	if d.RereviewMaxWait.Duration != 2*time.Hour {
		t.Errorf("rereview_max_wait = %s, want 2h", d.RereviewMaxWait)
	}
	if d.MaxRoundsPerPRPerDay != 12 {
		t.Errorf("max_rounds_per_pr_per_day = %d, want 12", d.MaxRoundsPerPRPerDay)
	}
	if err := validPipelineConfig().Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestSchedulingKeysUnsetKeepDefaultsWhenLoaded(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig})
	d := cfg.Daemon
	if d.RequestDebounce.Duration != time.Minute || d.RereviewMinLines != 30 || d.RereviewMaxWait.Duration != 2*time.Hour || d.MaxRoundsPerPRPerDay != 12 {
		t.Fatalf("daemon = request_debounce %s, rereview_min_lines %d, rereview_max_wait %s, max_rounds %d",
			d.RequestDebounce, d.RereviewMinLines, d.RereviewMaxWait, d.MaxRoundsPerPRPerDay)
	}
	w := cfg.Watches[0]
	if w.RereviewMinLines != nil || w.RereviewMaxWait.Duration != 0 || len(w.RequestTeams) != 0 {
		t.Fatalf("a watch without the keys must leave them unset: %+v", w)
	}
}

func TestSchedulingKeysLoad(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": schedulingConfig})
	d := cfg.Daemon
	if d.RequestDebounce.Duration != 90*time.Second {
		t.Errorf("daemon request_debounce = %s, want 1m30s", d.RequestDebounce)
	}
	if d.RereviewMinLines != 45 {
		t.Errorf("daemon rereview_min_lines = %d, want 45", d.RereviewMinLines)
	}
	if d.RereviewMaxWait.Duration != 3*time.Hour {
		t.Errorf("daemon rereview_max_wait = %s, want 3h", d.RereviewMaxWait)
	}
	if d.MaxRoundsPerPRPerDay != 20 {
		t.Errorf("daemon max_rounds_per_pr_per_day = %d, want 20", d.MaxRoundsPerPRPerDay)
	}

	if len(cfg.Watches) != 2 {
		t.Fatalf("watches = %d", len(cfg.Watches))
	}
	w := cfg.Watches[0]
	if w.RereviewMinLines == nil || *w.RereviewMinLines != 0 {
		t.Errorf("watch rereview_min_lines = %v, want a set 0 (distinct from unset)", w.RereviewMinLines)
	}
	if w.RereviewMaxWait.Duration != 45*time.Minute {
		t.Errorf("watch rereview_max_wait = %s, want 45m", w.RereviewMaxWait)
	}
	if !reflect.DeepEqual(w.RequestTeams, []string{"reviewers", "backend-team"}) {
		t.Errorf("watch request_teams = %q", w.RequestTeams)
	}

	plain := cfg.Watches[1]
	if plain.RereviewMinLines != nil || plain.RereviewMaxWait.Duration != 0 || len(plain.RequestTeams) != 0 {
		t.Errorf("second watch must have none of the keys: %+v", plain)
	}

	// the effective throttle of each watch
	if got := cfg.ThrottleFor(&cfg.Watches[0]); got.RereviewMinLines != 0 || got.RereviewMaxWait.Duration != 45*time.Minute || got.RequestDebounce.Duration != 90*time.Second {
		t.Errorf("ThrottleFor(acme) = lines %d wait %s debounce %s", got.RereviewMinLines, got.RereviewMaxWait, got.RequestDebounce)
	}
	if got := cfg.ThrottleFor(&cfg.Watches[1]); got.RereviewMinLines != 45 || got.RereviewMaxWait.Duration != 3*time.Hour {
		t.Errorf("ThrottleFor(example) = lines %d wait %s, want the daemon's 45 / 3h", got.RereviewMinLines, got.RereviewMaxWait)
	}
}

func TestSchedulingKeysAcceptDayDurations(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": strings.Replace(schedulingConfig, `rereview_max_wait = "3h"`, `rereview_max_wait = "1d"`, 1)})
	if got := cfg.Daemon.RereviewMaxWait.Duration; got != 24*time.Hour {
		t.Fatalf("rereview_max_wait = %s, want 24h", got)
	}
}

func TestSchedulingLocalOverlaySetsZeroValues(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"config.toml": schedulingConfig,
		"config.local.toml": `
[daemon]
request_debounce = "0s"
rereview_min_lines = 0

[[watch]]
owner = "local-owner"
include = ["*"]
identity = "z"
rereview_min_lines = 12
request_teams = ["on-call"]
`,
	})
	d := cfg.Daemon
	if d.RequestDebounce.Duration != 0 || d.RereviewMinLines != 0 {
		t.Errorf("the overlay's explicit zeros must win: debounce %s, min lines %d", d.RequestDebounce, d.RereviewMinLines)
	}
	if d.RereviewMaxWait.Duration != 3*time.Hour || d.MaxRoundsPerPRPerDay != 20 {
		t.Errorf("keys the overlay does not name keep the committed values: wait %s, rounds %d", d.RereviewMaxWait, d.MaxRoundsPerPRPerDay)
	}
	if len(cfg.Watches) != 3 {
		t.Fatalf("watches = %d, want the overlay's appended", len(cfg.Watches))
	}
	w := cfg.Watches[2]
	if w.RereviewMinLines == nil || *w.RereviewMinLines != 12 || !reflect.DeepEqual(w.RequestTeams, []string{"on-call"}) {
		t.Errorf("overlay watch = %+v", w)
	}
}

func TestThrottleForRereviewOverrides(t *testing.T) {
	// Defaults: 30 lines, 2h max wait, 1m debounce.
	for _, tc := range []struct {
		name      string
		daemon    func(*Daemon)
		watch     *Watch
		wantLines int
		wantWait  time.Duration
	}{
		{"nil watch is the daemon's", nil, nil, 30, 2 * time.Hour},
		{"watch without overrides is the daemon's", nil, &Watch{Owner: "acme"}, 30, 2 * time.Hour},
		{"watch with min lines 0 turns the threshold off", nil, &Watch{RereviewMinLines: intp(0)}, 0, 2 * time.Hour},
		{"watch with 10 lines and 30m max wait", nil, &Watch{RereviewMinLines: intp(10), RereviewMaxWait: Duration{30 * time.Minute}}, 10, 30 * time.Minute},
		{"watch with only a max wait keeps the daemon's lines", nil, &Watch{RereviewMaxWait: Duration{15 * time.Minute}}, 30, 15 * time.Minute},
		{"zero max wait on a watch keeps the daemon's", nil, &Watch{RereviewMinLines: intp(5), RereviewMaxWait: Duration{0}}, 5, 2 * time.Hour},
		{"watch can turn a daemon-off threshold on", func(d *Daemon) { d.RereviewMinLines = 0 }, &Watch{RereviewMinLines: intp(50)}, 50, 2 * time.Hour},
		{"unset watch lines keep a daemon-off threshold off", func(d *Daemon) { d.RereviewMinLines = 0 }, &Watch{}, 0, 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			if tc.daemon != nil {
				tc.daemon(&cfg.Daemon)
			}
			before := cfg.Daemon
			got := cfg.ThrottleFor(tc.watch)
			if got.RereviewMinLines != tc.wantLines || got.RereviewMaxWait.Duration != tc.wantWait {
				t.Errorf("ThrottleFor = lines %d wait %s, want %d / %s", got.RereviewMinLines, got.RereviewMaxWait, tc.wantLines, tc.wantWait)
			}
			if got.RequestDebounce.Duration != time.Minute {
				t.Errorf("request_debounce is daemon-wide, got %s", got.RequestDebounce)
			}
			if got.PushQuietPeriod != before.PushQuietPeriod || got.BurstPushes != before.BurstPushes || got.MaxRoundsPerPRPerDay != before.MaxRoundsPerPRPerDay {
				t.Errorf("unrelated keys changed: %+v", got)
			}
			if !reflect.DeepEqual(cfg.Daemon, before) {
				t.Errorf("ThrottleFor changed the daemon section:\n%+v\nwas\n%+v", cfg.Daemon, before)
			}
		})
	}
}

func TestValidateSchedulingKeys(t *testing.T) {
	neg := -1
	for _, tc := range []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"daemon rereview_min_lines", func(c *Config) { c.Daemon.RereviewMinLines = -1 }, "daemon.rereview_min_lines must be >= 0"},
		{"daemon rereview_max_wait", func(c *Config) { c.Daemon.RereviewMaxWait.Duration = -time.Minute }, "daemon.rereview_max_wait must not be negative"},
		{"daemon request_debounce", func(c *Config) { c.Daemon.RequestDebounce.Duration = -time.Second }, "daemon.request_debounce must not be negative"},
		{"watch rereview_min_lines", func(c *Config) { c.Watches[0].RereviewMinLines = &neg }, "watch acme: rereview_min_lines must be >= 0"},
		{"watch rereview_max_wait", func(c *Config) { c.Watches[0].RereviewMaxWait.Duration = -time.Hour }, "watch acme: rereview_max_wait must not be negative"},
		{"team with owner", func(c *Config) { c.Watches[0].RequestTeams = []string{"acme/reviewers"} }, `watch acme: request_teams entry "acme/reviewers"`},
		{"team with space", func(c *Config) { c.Watches[0].RequestTeams = []string{"core reviewers"} }, `watch acme: request_teams entry "core reviewers"`},
		{"team with at sign", func(c *Config) { c.Watches[0].RequestTeams = []string{"@reviewers"} }, `watch acme: request_teams entry "@reviewers"`},
		{"team empty", func(c *Config) { c.Watches[0].RequestTeams = []string{""} }, `watch acme: request_teams entry ""`},
		{"team blank", func(c *Config) { c.Watches[0].RequestTeams = []string{"   "} }, `watch acme: request_teams entry "   "`},
		{"one bad team among good ones", func(c *Config) { c.Watches[0].RequestTeams = []string{"backend", "org/frontend", "docs"} }, `request_teams entry "org/frontend"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.mod(cfg)
			wantError(t, cfg.Validate(), tc.want)
		})
	}
}

func TestValidateSchedulingKeysAcceptEdgeValues(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Daemon.RequestDebounce.Duration = 0
	cfg.Daemon.RereviewMinLines = 0
	cfg.Daemon.RereviewMaxWait.Duration = 0
	cfg.Watches[0].RereviewMinLines = intp(0)
	cfg.Watches[0].RereviewMaxWait.Duration = time.Second
	cfg.Watches[0].RequestTeams = []string{"reviewers", "backend-team", "team_1", "T2"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero debounce, no threshold and plain slugs must validate: %v", err)
	}
}

func TestValidateSchedulingReportsEveryProblem(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Daemon.RereviewMinLines = -5
	cfg.Daemon.RereviewMaxWait.Duration = -time.Hour
	cfg.Daemon.RequestDebounce.Duration = -time.Minute
	cfg.Watches[0].RereviewMinLines = intp(-2)
	cfg.Watches[0].RequestTeams = []string{"a/b", "@c"}
	err := cfg.Validate()
	wantError(t, err,
		"daemon.rereview_min_lines", "daemon.rereview_max_wait", "daemon.request_debounce",
		"watch acme: rereview_min_lines", `request_teams entry "a/b"`, `request_teams entry "@c"`)
	if n := len(validateErrs(err)); n != 6 {
		t.Errorf("errors = %d, want 6: %v", n, err)
	}
}

func TestLoadRejectsInvalidSchedulingKeys(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(string) string
		wants []string
	}{
		{"negative daemon lines", func(s string) string {
			return strings.Replace(s, "rereview_min_lines = 45", "rereview_min_lines = -1", 1)
		}, []string{"daemon.rereview_min_lines"}},
		{"negative daemon debounce", func(s string) string {
			return strings.Replace(s, `request_debounce = "90s"`, `request_debounce = "-1m"`, 1)
		}, []string{"daemon.request_debounce"}},
		{"negative watch max wait", func(s string) string {
			return strings.Replace(s, `rereview_max_wait = "45m"`, `rereview_max_wait = "-45m"`, 1)
		}, []string{"watch acme", "rereview_max_wait"}},
		{"team with owner", func(s string) string { return strings.Replace(s, `"backend-team"`, `"acme/backend"`, 1) }, []string{"watch acme", "request_teams", "acme/backend"}},
		{"misspelled key", func(s string) string { return strings.Replace(s, "request_teams", "request_team", 1) }, []string{"unknown keys", "request_team"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tc.edit(schedulingConfig)), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(paths.Layout{Home: dir}, filepath.Join(dir, "config.toml"))
			wantError(t, err, tc.wants...)
		})
	}
}

// The committed config.toml mirrors the built-in defaults for the keys that
// decide when a round runs, so editing one side without the other changes
// what users get from the file the repository ships.
func TestCommittedConfigAgreesWithSchedulingDefaults(t *testing.T) {
	root := repoRoot(t)
	cfg, err := LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.toml"), LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	got, want := cfg.Daemon, Defaults().Daemon
	if got.MaxRoundsPerPRPerDay != want.MaxRoundsPerPRPerDay {
		t.Errorf("config.toml max_rounds_per_pr_per_day = %d, the built-in default is %d", got.MaxRoundsPerPRPerDay, want.MaxRoundsPerPRPerDay)
	}
	if got.RequestDebounce != want.RequestDebounce || got.RereviewMinLines != want.RereviewMinLines || got.RereviewMaxWait != want.RereviewMaxWait {
		t.Errorf("config.toml request_debounce/rereview_min_lines/rereview_max_wait = %s/%d/%s, defaults %s/%d/%s",
			got.RequestDebounce, got.RereviewMinLines, got.RereviewMaxWait, want.RequestDebounce, want.RereviewMinLines, want.RereviewMaxWait)
	}
}
