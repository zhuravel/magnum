package evalrun

import (
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// The inputs a replay records name what its review read besides the PR and the notes: the judge
// skill, the roles' prompts and the role config. Any of them changed is another baseline. The
// reviewers' part moves with a reviewer's prompt or role config, never with a judge's prompt or
// skill, so a judge-only replay of a judge change still takes the reports of a full replay.
func TestInputsChangeWithTheSkillAPromptOrARoleAndReviewerInputsWithTheReviewersOnly(t *testing.T) {
	cfg := testConfig(t)
	all, rev := Inputs(cfg), ReviewerInputs(cfg)
	if len(all) != 12 || Inputs(cfg) != all || len(rev) != 12 || ReviewerInputs(cfg) != rev || all == rev {
		t.Fatalf("inputs %q and reviewer inputs %q: want two 12-hex hashes, each stable", all, rev)
	}
	seen := map[string]string{all: "the start"}
	changed := func(what string, reviewers bool) {
		t.Helper()
		got := Inputs(cfg)
		if prev, ok := seen[got]; ok {
			t.Errorf("after %s the inputs are %s, as after %s", what, got, prev)
		}
		seen[got] = what
		if r := ReviewerInputs(cfg); (r != rev) != reviewers {
			t.Errorf("after %s the reviewer inputs are %s (were %s), want them changed: %v", what, r, rev, reviewers)
		}
		rev = ReviewerInputs(cfg)
	}
	judge := cfg.JudgeFor(nil)
	writeFile(t, config.SkillPath(judge.Skill, cfg.Layout), "# Review skill\n")
	changed("writing the skill", false)
	writeFile(t, filepath.Join(cfg.Pipeline.PromptsDir, judge.PromptFile(config.PromptInitial)), "Review {{.URL}}.\n")
	changed("writing the judge's prompt", false)
	reviewer := -1
	for i, r := range cfg.Roles {
		if !r.Judge && r.PromptFile(config.PromptInitial) != "" {
			reviewer = i
			break
		}
	}
	if reviewer < 0 {
		t.Fatal("no reviewer role in the built-in config")
	}
	writeFile(t, filepath.Join(cfg.Pipeline.PromptsDir, cfg.Roles[reviewer].PromptFile(config.PromptInitial)), "Look at {{.URL}}.\n")
	changed("writing a reviewer's prompt", true)
	cfg.Roles[reviewer].Effort += "-changed"
	changed("changing a reviewer's effort", true)
	if Inputs(nil) != "" || ReviewerInputs(nil) != "" {
		t.Error("a nil config has inputs")
	}
}
