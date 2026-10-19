package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
)

// The judge posts at most max_simplifications simplifications: five by default
// since the operator asked on 2026-10-19 (three had been the cap since
// 2026-10-05), and a [[repo]] block's own cap wins for that
// repository, as its stakes line does. A repo block that leaves it unset or 0
// keeps [pipeline]'s, and a config built in code without the key gets the
// default, never a cap of 0.
func TestTheJudgePostsFiveSimplificationsByDefaultAndARepoBlocksCapWins(t *testing.T) {
	if DefaultMaxSimplifications != 5 || MaxSimplificationsMax != 10 {
		t.Fatalf("DefaultMaxSimplifications = %d, MaxSimplificationsMax = %d, want 5 and 10", DefaultMaxSimplifications, MaxSimplificationsMax)
	}
	if got := Defaults().Pipeline.MaxSimplifications; got != 5 {
		t.Errorf("Defaults().Pipeline.MaxSimplifications = %d, want 5", got)
	}
	root := repoRoot(t)
	standalone, err := LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.defaults.toml"), LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatalf("config.defaults.toml does not load standalone: %v", err)
	}
	if got := standalone.MaxSimplificationsFor("example/app"); got != 5 {
		t.Errorf("config.defaults.toml alone caps simplifications at %d, want 5", got)
	}

	const head = `
[[identity]]
name = "z"
kind = "gh"
login = "z"
[[watch]]
owner = "example"
include = ["*"]
identity = "z"
`
	plain := mustLoad(t, map[string]string{"config.toml": head})
	if got := plain.MaxSimplificationsFor("example/app"); got != 5 {
		t.Errorf("a config without the key caps simplifications at %d, want 5", got)
	}

	cfg := mustLoad(t, map[string]string{"config.toml": `
[pipeline]
max_simplifications = 4
` + head + `
[[repo]]
repo = "example/web"
max_simplifications = 2
[[repo]]
repo = "example/tools"
max_simplifications = 0
[[repo]]
repo = "example/docs"
keep_approvals = true
`})
	for repo, want := range map[string]int{
		"example/web":   2,
		"Example/Web":   2,
		"example/tools": 4,
		"example/docs":  4,
		"example/app":   4,
		"nobody/app":    4,
	} {
		if got := cfg.MaxSimplificationsFor(repo); got != want {
			t.Errorf("MaxSimplificationsFor(%s) = %d, want %d", repo, got, want)
		}
	}
	var none *Config
	if got := none.MaxSimplificationsFor("example/web"); got != DefaultMaxSimplifications {
		t.Errorf("a nil config's MaxSimplificationsFor = %d, want %d", got, DefaultMaxSimplifications)
	}
	inCode := &Config{Repos: []Repo{{Repo: "example/web"}}}
	if got := inCode.MaxSimplificationsFor("example/web"); got != DefaultMaxSimplifications {
		t.Errorf("a config built in code without the key caps simplifications at %d, want %d", got, DefaultMaxSimplifications)
	}
}

// The daemon reads the built-in defaults with the user's config over them:
// a user [pipeline] table that sets other keys keeps the built-in cap of 5,
// and the user's [[repo]] block's cap wins for that repository alone.
func TestAUserRepoCapLayersOverTheBuiltinDefaults(t *testing.T) {
	home := t.TempDir()
	user := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(user, []byte(`
[pipeline]
audit_every = 4
[[identity]]
name = "z"
kind = "gh"
login = "z"
[[watch]]
owner = "example"
include = ["*"]
identity = "z"
[[repo]]
repo = "example/web"
max_simplifications = 2
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(paths.Layout{Home: home, UserConfig: user}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 2 || cfg.Sources[0] != BuiltinDefaults || cfg.Sources[1] != user || cfg.Pipeline.AuditEvery != 4 {
		t.Fatalf("the user config did not layer over the built-in defaults: sources %q, audit_every %d", cfg.Sources, cfg.Pipeline.AuditEvery)
	}
	if got := cfg.Pipeline.MaxSimplifications; got != 5 {
		t.Errorf("[pipeline] max_simplifications under a user [pipeline] without it = %d, want the built-in 5", got)
	}
	for repo, want := range map[string]int{"example/web": 2, "example/app": 5} {
		if got := cfg.MaxSimplificationsFor(repo); got != want {
			t.Errorf("MaxSimplificationsFor(%s) = %d, want %d", repo, got, want)
		}
	}
}

// A cap of 0 would post nothing claude-simplify proved, and one PR got 20
// simplifications before the cap of 2026-10-05: [pipeline] takes 1 to
// MaxSimplificationsMax, a [[repo]] block 0 (unset) to it, and the error
// names the block, the key and the range.
func TestAMaxSimplificationsOutsideItsRangeIsRefused(t *testing.T) {
	valid := func() *Config {
		cfg := validPipelineConfig()
		cfg.Repos = []Repo{{Repo: "acme/web"}}
		return cfg
	}
	ok := valid()
	ok.Pipeline.MaxSimplifications = MaxSimplificationsMax
	ok.Repos[0].MaxSimplifications = MaxSimplificationsMax
	if err := ok.Validate(); err != nil {
		t.Fatalf("caps of %d: %v", MaxSimplificationsMax, err)
	}
	ok.Pipeline.MaxSimplifications, ok.Repos[0].MaxSimplifications = 1, 0
	if err := ok.Validate(); err != nil {
		t.Fatalf("[pipeline] 1 and a repo 0: %v", err)
	}
	cases := map[string]struct {
		edit func(*Config)
		want []string
	}{
		"pipeline 0":  {func(c *Config) { c.Pipeline.MaxSimplifications = 0 }, []string{"pipeline: max_simplifications", "from 1 to 10", "got 0"}},
		"pipeline 11": {func(c *Config) { c.Pipeline.MaxSimplifications = 11 }, []string{"pipeline: max_simplifications", "from 1 to 10", "got 11"}},
		"repo -1":     {func(c *Config) { c.Repos[0].MaxSimplifications = -1 }, []string{"repo acme/web: max_simplifications", "from 0", "to 10", "got -1"}},
		"repo 11":     {func(c *Config) { c.Repos[0].MaxSimplifications = 11 }, []string{"repo acme/web: max_simplifications", "from 0", "to 10", "got 11"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			tc.edit(cfg)
			wantError(t, cfg.Validate(), tc.want...)
		})
	}
}

// The cap is the judge's, not a per-PR worktree's: a [[repo]] block for a
// repository with a [[pool]] may set it alone.
func TestAPooledRepoMaySetMaxSimplifications(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": `
[[identity]]
name = "z"
kind = "gh"
login = "z"
[[watch]]
owner = "example"
include = ["*"]
identity = "z"
[[pool]]
repo = "example/big"
main_clone = "~/Projects/big"
slot_name = "review{n}"
slot_path = "~/Projects/big.review{n}"
max = 1
[[repo]]
repo = "example/big"
max_simplifications = 3
`})
	if got := cfg.MaxSimplificationsFor("example/big"); got != 3 {
		t.Errorf("the pooled repository's cap = %d, want 3", got)
	}
}
