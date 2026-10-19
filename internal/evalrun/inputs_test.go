package evalrun

import (
	"path/filepath"
	"slices"
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

// The judge and the reviewers read the repository's stakes line (config.StakesFor), so a replay's inputs
// cover it: a stakes edit on a [[watch]] or a [[repo]] block is another baseline, in the round's inputs and
// in the reviewers', and the same config gives the same hashes. A config with no stakes line hashes as it
// did before the lines counted, so only the configs that set one replay their baseline once.
func TestAStakesChangeChangesTheInputs(t *testing.T) {
	cfg := testConfig(t)
	none, noneRev := Inputs(cfg), ReviewerInputs(cfg)
	cfg.Watches = []config.Watch{{Owner: "example", Stakes: "a billing service"}}
	cfg.Repos = []config.Repo{{Repo: "example/widgets", Stakes: "money moves here"}}
	all, rev := Inputs(cfg), ReviewerInputs(cfg)
	if Inputs(cfg) != all || ReviewerInputs(cfg) != rev {
		t.Fatalf("the same config gave inputs %q then %q: want them stable", all, Inputs(cfg))
	}
	if all == none || rev == noneRev {
		t.Errorf("a config with stakes lines has the inputs %s/%s of one without (%s/%s)", all, rev, none, noneRev)
	}
	seen := map[string]string{all: "the start"}
	changed := func(what string) {
		t.Helper()
		got, r := Inputs(cfg), ReviewerInputs(cfg)
		if prev, ok := seen[got]; ok {
			t.Errorf("after %s the inputs are %s, as after %s", what, got, prev)
		}
		seen[got] = what
		if r == rev {
			t.Errorf("after %s the reviewer inputs are still %s", what, r)
		}
		rev = r
	}
	cfg.Watches[0].Stakes = "a billing service, and the ledger"
	changed("editing the watch's stakes")
	cfg.Repos[0].Stakes = "money moves here, and PII"
	changed("editing the repository's stakes")
	cfg.Repos[0].Stakes = ""
	changed("removing the repository's stakes")
	cfg.Repos = append(cfg.Repos, config.Repo{Repo: "example/gadgets", Stakes: "a billing service, and the ledger"})
	changed("adding another repository's stakes")
	cfg.Repos, cfg.Watches = nil, nil
	if got, r := Inputs(cfg), ReviewerInputs(cfg); got != none || r != noneRev {
		t.Errorf("with no stakes line the inputs are %s/%s, want the %s/%s of a config without blocks", got, r, none, noneRev)
	}
}

// Only the judge reads how many simplifications it may post (config.MaxSimplificationsFor), so a cap edit
// on [pipeline] or a [[repo]] block is another baseline in the round's inputs and leaves the reviewers'
// alone: a judge-only replay of it still takes a full replay's reports. The cap in effect counts, not how
// it is written (a [pipeline] 0 is the default), and the blocks' order is not an input.
func TestASimplificationCapChangeChangesTheInputsButNotTheReviewers(t *testing.T) {
	cfg := testConfig(t)
	cfg.Repos = []config.Repo{{Repo: "example/widgets"}}
	all, rev := Inputs(cfg), ReviewerInputs(cfg)
	seen := map[string]string{all: "the start"}
	changed := func(what string, want bool) {
		t.Helper()
		got := Inputs(cfg)
		if prev, ok := seen[got]; ok == want {
			if want {
				t.Errorf("after %s the inputs are %s, as after %s", what, got, prev)
			} else {
				t.Errorf("after %s the inputs are %s, want them unchanged", what, got)
			}
		}
		seen[got] = what
		if r := ReviewerInputs(cfg); r != rev {
			t.Errorf("after %s the reviewer inputs are %s, want %s: the reviewers do not read the cap", what, r, rev)
		}
	}
	cfg.Repos[0].MaxSimplifications = 2
	changed("setting the repository's cap", true)
	cfg.Repos[0].MaxSimplifications = 3
	changed("editing the repository's cap", true)
	cfg.Repos = append(cfg.Repos, config.Repo{Repo: "example/gadgets", MaxSimplifications: 4})
	changed("adding another repository's cap", true)
	cfg.Pipeline.MaxSimplifications = 7
	changed("editing [pipeline]'s cap", true)
	ordered := Inputs(cfg)
	slices.Reverse(cfg.Repos)
	if got := Inputs(cfg); got != ordered {
		t.Errorf("listed the other way round the inputs are %s, want %s", got, ordered)
	}

	cfg = testConfig(t)
	cfg.Pipeline.MaxSimplifications = config.DefaultMaxSimplifications
	all = Inputs(cfg)
	cfg.Pipeline.MaxSimplifications = 0 // as a config built in code: the default is in effect
	if got := Inputs(cfg); got != all {
		t.Errorf("a [pipeline] cap of 0, which is the default in effect, changed the inputs to %s from %s", got, all)
	}
}

// The blocks' order in the file is not an input: two watches or repositories listed the other way round
// read the same stakes.
func TestTheStakesLinesCountInTheOrderOfTheirKeys(t *testing.T) {
	cfg := testConfig(t)
	cfg.Watches = []config.Watch{{Owner: "example", Stakes: "one"}, {Owner: "acme", Stakes: "two"}}
	cfg.Repos = []config.Repo{{Repo: "example/widgets", Stakes: "three"}, {Repo: "acme/app", Stakes: "four"}}
	all, rev := Inputs(cfg), ReviewerInputs(cfg)
	slices.Reverse(cfg.Watches)
	slices.Reverse(cfg.Repos)
	if got, r := Inputs(cfg), ReviewerInputs(cfg); got != all || r != rev {
		t.Errorf("listed the other way round the inputs are %s/%s, want %s/%s", got, r, all, rev)
	}
}
