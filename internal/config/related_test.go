package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The judge hears of the PRs merged within [pipeline] related_lookback (14
// days by default) that change its PR's paths, leaving out the paths
// related_ignore lists (lockfiles by default); a [[watch]] overrides either,
// and related_ignore = [] there ignores nothing.
func TestRelatedDefaultsAndAWatchMayOverrideThem(t *testing.T) {
	def := Defaults().RelatedFor(nil)
	if def.Lookback != 14*24*time.Hour {
		t.Fatalf("Defaults(): related_lookback = %v, want 14d", def.Lookback)
	}
	for _, lock := range []string{"Gemfile.lock", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "go.sum", "poetry.lock"} {
		if !slices.ContainsFunc(def.Ignore, func(g string) bool { return MatchPath(g, lock) && MatchPath(g, "frontend/"+lock) }) {
			t.Errorf("Defaults(): related_ignore %v does not ignore %s at the root and in a directory", def.Ignore, lock)
		}
	}
	if slices.ContainsFunc(def.Ignore, func(g string) bool { return MatchPath(g, "app/models/order.rb") }) {
		t.Errorf("Defaults(): related_ignore %v ignores a source file", def.Ignore)
	}
	if cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig}); !slices.Equal(cfg.RelatedFor(&cfg.Watches[0]).Ignore, def.Ignore) ||
		cfg.RelatedFor(&cfg.Watches[0]).Lookback != def.Lookback {
		t.Fatalf("unset: %+v, want the defaults", cfg.RelatedFor(&cfg.Watches[0]))
	}
	cfg := mustLoad(t, map[string]string{"config.toml": `
[pipeline]
related_lookback = "7d"
related_ignore = ["**/*.lock"]

[[identity]]
name = "z"
kind = "gh"
login = "z"

[[watch]]
owner = "acme"
include = ["*"]
identity = "z"
related_lookback = "30d"
related_ignore = []

[[watch]]
owner = "example"
include = ["*"]
identity = "z"
`})
	if a := cfg.RelatedFor(&cfg.Watches[0]); a.Lookback != 30*24*time.Hour || a.Ignore == nil || len(a.Ignore) != 0 {
		t.Errorf("acme = %+v; want the watch's 30d and no ignored path", a)
	}
	if e := cfg.RelatedFor(&cfg.Watches[1]); e.Lookback != 7*24*time.Hour || !slices.Equal(e.Ignore, []string{"**/*.lock"}) {
		t.Errorf("example = %+v; want the pipeline's 7d and **/*.lock", e)
	}
}

func TestRelatedRefusesANegativeLookbackAndABadPattern(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"pipeline lookback": {"[pipeline]\nrelated_lookback = \"-1h\"\n" + minimalConfig, "related_lookback"},
		"watch lookback":    {minimalConfig + "related_lookback = \"-2h\"\n", "related_lookback"},
		"pipeline pattern":  {"[pipeline]\nrelated_ignore = [\"a**b\"]\n" + minimalConfig, "related_ignore"},
		"watch pattern":     {minimalConfig + "related_ignore = [\"/abs\"]\n", "related_ignore"},
	} {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": tc.body})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %s", name, err, tc.want)
		}
	}
}
