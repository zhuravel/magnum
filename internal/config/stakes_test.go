package config

import (
	"strings"
	"testing"
)

// The operator tells the judge what a repository puts at risk: a [[watch]]
// states it for every repository it covers, and a [[repo]] block's own line
// wins for that repository, as keep_approvals falls back. A repository with
// neither has none, and a repo block without the key keeps the watch's.
func TestARepoStakesLineWinsOverTheWatchs(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": `
[[identity]]
name = "z"
kind = "gh"
login = "z"
[[watch]]
owner = "example"
include = ["*"]
identity = "z"
stakes = "A SaaS on the public internet: merchants' money and shoppers' personal data."
[[watch]]
owner = "other"
include = ["*"]
identity = "z"
[[repo]]
repo = "example/tools"
stakes = "Internal tooling with no customers: the harm is a colleague's hour at most."
[[repo]]
repo = "example/web"
keep_approvals = true
`})
	saas := "A SaaS on the public internet: merchants' money and shoppers' personal data."
	for repo, want := range map[string]string{
		"example/tools": "Internal tooling with no customers: the harm is a colleague's hour at most.",
		"Example/Tools": "Internal tooling with no customers: the harm is a colleague's hour at most.",
		"example/app":   saas,
		"example/web":   saas,
		"other/app":     "",
		"nobody/app":    "",
	} {
		if got := cfg.StakesFor(repo); got != want {
			t.Errorf("StakesFor(%s) = %q, want %q", repo, got, want)
		}
	}
	var none *Config
	if got := none.StakesFor("example/app"); got != "" {
		t.Errorf("a nil config's StakesFor = %q", got)
	}
}

// A stakes line goes into the judge's <magnum> block as one line, so the
// loader refuses a value of more than StakesMax characters or with a line
// break, and the error names the block.
func TestAStakesLineOverTheLimitIsRefused(t *testing.T) {
	valid := func() *Config {
		cfg := validPipelineConfig()
		cfg.Repos = []Repo{{Repo: "acme/web"}}
		return cfg
	}
	if StakesMax != 300 {
		t.Fatalf("StakesMax = %d, want 300", StakesMax)
	}
	ok := valid()
	ok.Watches[0].Stakes = strings.Repeat("w", StakesMax)
	ok.Repos[0].Stakes = strings.Repeat("é", StakesMax) // characters, not bytes
	if err := ok.Validate(); err != nil {
		t.Fatalf("stakes of %d characters: %v", StakesMax, err)
	}
	cases := map[string]struct {
		edit func(*Config)
		want []string
	}{
		"repo over the limit": {func(c *Config) { c.Repos[0].Stakes = strings.Repeat("r", 301) },
			[]string{"repo acme/web", "stakes", "at most 300 characters", "301"}},
		"watch over the limit": {func(c *Config) { c.Watches[0].Stakes = strings.Repeat("w", 301) },
			[]string{"watch acme", "stakes", "at most 300 characters"}},
		"repo newline": {func(c *Config) { c.Repos[0].Stakes = "Real money.\nShoppers' data." },
			[]string{"repo acme/web", "stakes", "one line"}},
		"watch carriage return": {func(c *Config) { c.Watches[0].Stakes = "Real money.\rShoppers' data." },
			[]string{"watch acme", "stakes", "one line"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			tc.edit(cfg)
			wantError(t, cfg.Validate(), tc.want...)
		})
	}
}
