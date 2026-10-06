package config

import (
	"strings"
	"testing"
)

// The judge does its own pass while the reviewers work by default
// ([pipeline] judge_own_pass = "parallel"); "after" keeps one judge prompt
// after the reviewers, and a [[watch]] overrides the [pipeline] value either
// way.
func TestJudgeOwnPassDefaultsToParallelAndAWatchMayOverrideIt(t *testing.T) {
	if got := Defaults().JudgeOwnPassFor(nil); got != OwnPassParallel {
		t.Fatalf("Defaults(): judge_own_pass = %q, want %q", got, OwnPassParallel)
	}
	if cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig}); cfg.Pipeline.JudgeOwnPass != OwnPassParallel ||
		cfg.Watches[0].JudgeOwnPass != "" || cfg.JudgeOwnPassFor(&cfg.Watches[0]) != OwnPassParallel {
		t.Fatalf("unset: pipeline %q, watch %q", cfg.Pipeline.JudgeOwnPass, cfg.Watches[0].JudgeOwnPass)
	}
	cfg := mustLoad(t, map[string]string{"config.toml": `
[pipeline]
judge_own_pass = "after"

[[identity]]
name = "z"
kind = "gh"
login = "z"

[[watch]]
owner = "acme"
include = ["*"]
identity = "z"
judge_own_pass = "parallel"

[[watch]]
owner = "example"
include = ["*"]
identity = "z"
`})
	if cfg.Pipeline.JudgeOwnPass != OwnPassAfter {
		t.Errorf(`[pipeline] judge_own_pass = "after" did not load: %q`, cfg.Pipeline.JudgeOwnPass)
	}
	if a, e := cfg.JudgeOwnPassFor(&cfg.Watches[0]), cfg.JudgeOwnPassFor(&cfg.Watches[1]); a != OwnPassParallel || e != OwnPassAfter {
		t.Errorf("JudgeOwnPassFor: acme %q, example %q; want the watch's parallel, the pipeline's after", a, e)
	}
}

func TestJudgeOwnPassRefusesAnUnknownValue(t *testing.T) {
	for name, body := range map[string]string{
		"pipeline": "[pipeline]\njudge_own_pass = \"sometimes\"\n" + minimalConfig,
		"watch":    minimalConfig + "judge_own_pass = \"before\"\n",
	} {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": body})
		if err == nil || !strings.Contains(err.Error(), "judge_own_pass") {
			t.Errorf("%s: err = %v, want one naming judge_own_pass", name, err)
		}
	}
}

// A judge role prompts its own pass with judge-own-pass.md unless it names
// another file; no other role has an own-pass prompt.
func TestJudgeRolesHaveAnOwnPassPrompt(t *testing.T) {
	cfg := Defaults()
	judge, ok := cfg.RoleByNameOrAlias(nil, RoleCodexJudge)
	if !ok || judge.PromptFile(PromptOwnPass) != "judge-own-pass.md" {
		t.Fatalf("codex-judge own-pass prompt = %q", judge.PromptFile(PromptOwnPass))
	}
	if r, _ := cfg.RoleByNameOrAlias(nil, RoleClaudeReview); r.PromptFile(PromptOwnPass) != "" {
		t.Errorf("claude-review has an own-pass prompt: %q", r.PromptFile(PromptOwnPass))
	}
	if _, err := cfg.RolePrompt(judge, PromptOwnPass); err != nil {
		t.Errorf("the own-pass prompt does not resolve: %v", err)
	}
	custom := mustLoad(t, map[string]string{"config.toml": minimalConfig + `
[[role]]
name = "codex-judge"
own_pass = "my-own-pass.md"
`, "prompts/my-own-pass.md": "own pass {{.URL}}"})
	if j, _ := custom.RoleByNameOrAlias(nil, RoleCodexJudge); j.PromptFile(PromptOwnPass) != "my-own-pass.md" {
		t.Errorf("own_pass key: %q", j.PromptFile(PromptOwnPass))
	}
}
