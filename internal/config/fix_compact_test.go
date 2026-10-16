package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
)

// [pipeline] fix_compact_at is a percent of the fixer's context, 0 (off) to
// 99: a negative one and 100 or more are refused with the key named, and the
// default is 70.
func TestFixCompactAtRefusesABadPercent(t *testing.T) {
	if got := Defaults().Pipeline.FixCompactAt; got != 70 {
		t.Fatalf("Defaults().Pipeline.FixCompactAt = %d, want 70", got)
	}
	for _, n := range []int{-1, 100, 250} {
		cfg := validPipelineConfig()
		cfg.Pipeline.FixCompactAt = n
		wantError(t, cfg.Validate(), "pipeline: fix_compact_at must be a percent from 0 (off) to 99, got")
	}
	for _, n := range []int{0, 1, 70, 99} {
		cfg := validPipelineConfig()
		cfg.Pipeline.FixCompactAt = n
		if err := cfg.Validate(); err != nil {
			t.Errorf("fix_compact_at = %d: %v", n, err)
		}
	}
}

// [kinds.<name>] context_pattern must compile and name a capture group
// "used" or "left": a pattern that does not compile, and one that captures
// the figure in a group of another name or in none, are refused with the
// kind named. The claude kind's default reads both forms, "" turns the
// reading off, and a config file sets the key over the default.
func TestContextPatternMustCompileAndNameAGroup(t *testing.T) {
	bad := map[string]struct{ pattern, want string }{
		"no compile":       {`(?P<used>\d+%`, `kinds.claude: context_pattern "(?P<used>\\d+%": error parsing regexp`},
		"an unnamed group": {`\((\d+)%\)`, `kinds.claude: context_pattern "\\((\\d+)%\\)" names neither a (?P<used>...) nor a (?P<left>...) group`},
		"another name":     {`\((?P<pct>\d+)%\)`, "names neither a (?P<used>...) nor a (?P<left>...) group"},
	}
	for name, tc := range bad {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			cfg.Kinds = DefaultKinds()
			k := cfg.Kinds[KindClaude]
			k.ContextPattern = tc.pattern
			cfg.Kinds[KindClaude] = k
			wantError(t, cfg.Validate(), tc.want)
		})
	}
	for name, pattern := range map[string]string{
		"the default": DefaultClaudeContextPattern, "off": "",
		"used alone": `(?P<used>\d+)% used`, "left alone": `(?P<left>\d+)% left`,
	} {
		t.Run("valid/"+name, func(t *testing.T) {
			cfg := validPipelineConfig()
			cfg.Kinds = DefaultKinds()
			k := cfg.Kinds[KindClaude]
			k.ContextPattern = pattern
			cfg.Kinds[KindClaude] = k
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
	if k, _ := Defaults().KindSpec(KindClaude); k.ContextPattern != DefaultClaudeContextPattern {
		t.Errorf("claude context_pattern = %q, want the default", k.ContextPattern)
	}
	if k, _ := Defaults().KindSpec(KindCodex); k.ContextPattern != "" {
		t.Errorf("codex context_pattern = %q, want none (Codex compacts by itself)", k.ContextPattern)
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(file, []byte(minimalConfig+"\n[kinds.claude]\ncontext_pattern = '(?P<used>\\d+)% used'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(paths.Layout{Home: dir}, file, LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := cfg.KindSpec(KindClaude); k.ContextPattern != `(?P<used>\d+)% used` || len(k.ExitKeys) == 0 {
		t.Errorf("claude after the file: context_pattern %q, exit_keys %q (the other keys stay the default's)", k.ContextPattern, k.ExitKeys)
	}
	if err := os.WriteFile(file, []byte(minimalConfig+"\n[kinds.claude]\ncontext_pattern = '(\\d+)%'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithOptions(paths.Layout{Home: dir}, file, LoadOptions{NoOverlay: true}); err == nil {
		t.Error("a context_pattern without a used or left group loads")
	}
}
