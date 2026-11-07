package config

import (
	"bufio"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/paths"
)

// factoryRepoToml is a user config's [[factory.repo]]: the copies of
// talkable/talkable, which testLocalConfig's [[watch]] covers and whose
// [[pool]] names its review slots review{n}.
const factoryRepoToml = `
[[factory.repo]]
repo = "talkable/talkable"
main_clone = "~/Projects/talkable"
copies = "~/Projects/talkable.factory{n}"
max_copies = 4
`

// validFactoryConfig is a configuration with one factory repository that
// Validate accepts: the built-in defaults, an identity, a watch for acme/*
// and the repository acme/app.
func validFactoryConfig() *Config {
	cfg := validPipelineConfig()
	cfg.Factory.Repos = []FactoryRepo{{Repo: "acme/app", Base: "master", MainClone: "/p/app", Copies: "/p/app.factory{n}",
		CopyName: "factory{n}", MaxCopies: 2, Precheck: FactoryPrecheck{Cap: Duration{10 * time.Minute}}}}
	return cfg
}

// editKind changes the factory kind name in place; the stages are cloned
// first, so the edit never reaches another configuration.
func editKind(cfg *Config, name string, edit func(stages []FactoryStage) []FactoryStage) {
	k := cfg.Factory.Kinds[name]
	k.Stages = edit(slices.Clone(k.Stages))
	cfg.Factory.Kinds[name] = k
}

func equalAny(a, b any) bool { return reflect.DeepEqual(a, b) }

// uncommentedLines is the lines of the embedded defaults that are not
// comments, each without its trailing comment.
func uncommentedLines(t *testing.T) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(magnum.DefaultConfig)))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), " #")
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// The defaults are distributable: they name no repository, so they name no
// copy, no precheck and no staging environment (those live in the
// repository's block).
func TestFactoryDefaultsShipNoRepositoryAndNoStaging(t *testing.T) {
	cfg, err := LoadWithOptions(paths.Layout{Home: repoRoot(t)}, "", LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cfg.Factory.Repos); n != 0 {
		t.Fatalf("the built-in defaults declare %d factory repositories", n)
	}
	if n := len(Defaults().Factory.Repos); n != 0 {
		t.Fatalf("Defaults() declares %d factory repositories", n)
	}
	for _, line := range uncommentedLines(t) {
		if strings.Contains(line, "factory.repo") || strings.Contains(line, "staging") {
			t.Errorf("config.defaults.toml sets %q: a repository and its staging environments belong in the user's config", line)
		}
	}
}

// Each kind of task runs its investigation, the one batch of questions, the
// writer, the local review, the PR body and the operator's decision, and
// only then publishes; every role a stage names is declared.
func TestFactoryDefaultsDefineBugFeatureAndChore(t *testing.T) {
	cfg, err := LoadWithOptions(paths.Layout{Home: repoRoot(t)}, "", LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Config{cfg, Defaults()} {
		for _, name := range []string{"bug", "feature", "chore"} {
			k, ok := c.Factory.Kinds[name]
			if !ok {
				t.Fatalf("no factory kind %s among %v", name, c.Factory.Kinds)
			}
			var names []string
			index := map[string]int{}
			for i, st := range k.Stages {
				names = append(names, st.Name)
				index[st.Type+":"+st.Name] = i
				if st.Type == FactoryStageAgents {
					for _, r := range st.Roles {
						if _, ok := c.FactoryRole(r); !ok {
							t.Errorf("kind %s stage %s names role %s, which no [[factory.role]] declares", name, st.Name, r)
						}
					}
				}
			}
			last := k.Stages[len(k.Stages)-1]
			if last.Type != FactoryStageEffect || last.Effect != FactoryEffectPublish {
				t.Errorf("kind %s ends with %+v, want the publish effect", name, last)
			}
			decision, review := -1, -1
			for i, st := range k.Stages {
				switch {
				case st.Type == FactoryStageDecision && st.Decision == FactoryDecisionReadyPush:
					decision = i
				case st.Type == FactoryStageReview:
					review = i
				}
			}
			if decision < 0 || decision >= len(k.Stages)-1 {
				t.Errorf("kind %s: the %s decision must come before the last stage: %v", name, FactoryDecisionReadyPush, names)
			}
			if review < 0 || review >= decision {
				t.Errorf("kind %s: a review stage must come before the decision: %v", name, names)
			}
			for _, want := range []string{"investigate", "judge", "critic", "questions", "coding", "local_review", "pr_body"} {
				if !slices.Contains(names, want) {
					t.Errorf("kind %s has no stage %s: %v", name, want, names)
				}
			}
			for _, st := range k.Stages {
				if st.Name == "critic" && st.When != FactoryWhenWide {
					t.Errorf("kind %s: the critic stage has when = %q, want %q", name, st.When, FactoryWhenWide)
				}
			}
		}
	}
	if got := cfg.Factory.KindByJiraType; got["Bug"] != "bug" || got["Story"] != "feature" || got["Task"] != "chore" {
		t.Errorf("kind_by_jira_type = %v", got)
	}
	// A bug needs a reproduction before any code.
	if !slices.ContainsFunc(cfg.Factory.Kinds["bug"].Stages, func(s FactoryStage) bool { return s.Gate == FactoryGateReproduced }) {
		t.Errorf("the bug kind has no %s gate", FactoryGateReproduced)
	}
}

// What belongs to one repository or one team stays out of the tracked
// defaults: the error tracker's skill, the gh-ost marker, the table-size
// probe and the remote consoles are keys of a [[factory.repo]] with no
// default.
func TestFactoryDefaultsNameNoRepositorySpecifics(t *testing.T) {
	for _, line := range uncommentedLines(t) {
		for _, key := range []string{"ghost_marker", "table_stats", "consoles", "error_skill", "pr_body_skill", "large_table_rows", "reviewer"} {
			if strings.HasPrefix(line, key) {
				t.Errorf("config.defaults.toml sets %q", line)
			}
		}
	}
	r := FactoryRepo{}
	if r.ErrorSkill != "" || r.PRBodySkill != "" || r.GhostMarker != "" || r.TableStats != "" || len(r.Consoles) != 0 || r.LargeTableRows != 0 {
		t.Fatalf("the zero repository names specifics: %+v", r)
	}
}

// pr_body_skill is the skill that writes the PRs' descriptions: a key of a
// [[factory.repo]] with no default, trimmed when it loads, and a name of one
// word (a value with whitespace inside it is refused, naming the key).
func TestFactoryRepoPRBodySkill(t *testing.T) {
	load := func(skill string) (*Config, error) {
		return loadCommittedWithLocal(t, testLocalConfig+factoryRepoToml+"pr_body_skill = "+skill+"\n")
	}
	cfg, err := load(`"  example:create-pr  "`)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := cfg.FactoryRepo("talkable/talkable"); !ok || r.PRBodySkill != "example:create-pr" {
		t.Errorf("pr_body_skill = %q, %v, want example:create-pr (trimmed)", r.PRBodySkill, ok)
	}
	cfg, err = loadCommittedWithLocal(t, testLocalConfig+factoryRepoToml)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := cfg.FactoryRepo("talkable/talkable"); r.PRBodySkill != "" {
		t.Errorf("pr_body_skill = %q without the key, want none", r.PRBodySkill)
	}
	_, err = load(`"example create-pr"`)
	wantError(t, err, "pr_body_skill", "example create-pr")
}

// An overlay that defines one kind of task or one role changes that one:
// the rest of the built-in set stays, and a role's other keys stay.
func TestFactoryKindsAndRolesMergeKeyByKey(t *testing.T) {
	base, err := loadCommittedWithLocal(t, testLocalConfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+`
[[factory.kinds.bug.stages]]
name = "investigate"
type = "agents"
roles = ["fx-repro"]
end = "findings"

[[factory.role]]
name = "fx-writer"
model = "sonnet"

[[factory.role]]
name = "fx-extra"
kind = "claude"
prompt = "factory-judge.md"
why = "look at the extra things"
doing = "looking at extras"
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Factory.Kinds["bug"].Stages; len(got) != 1 || got[0].Name != "investigate" || !slices.Equal(got[0].Roles, []string{"fx-repro"}) {
		t.Errorf("the overlay's bug kind = %+v, want its one stage", got)
	}
	for _, name := range []string{"feature", "chore"} {
		if !equalAny(cfg.Factory.Kinds[name], base.Factory.Kinds[name]) || len(cfg.Factory.Kinds[name].Stages) == 0 {
			t.Errorf("kind %s changed: %+v, was %+v", name, cfg.Factory.Kinds[name], base.Factory.Kinds[name])
		}
	}
	w, ok := cfg.FactoryRole("fx-writer")
	was, _ := base.FactoryRole("fx-writer")
	if !ok || w.Model != "sonnet" || was.Model == "sonnet" {
		t.Fatalf("fx-writer model = %q (was %q), want sonnet", w.Model, was.Model)
	}
	w.Model = was.Model
	if !equalAny(w, was) {
		t.Errorf("fx-writer changed more than its model:\n%+v\nwas\n%+v", w, was)
	}
	for _, r := range base.Factory.Roles {
		if r.Name == "fx-writer" {
			continue
		}
		if got, ok := cfg.FactoryRole(r.Name); !ok || !equalAny(got, r) {
			t.Errorf("role %s changed: %+v, was %+v", r.Name, got, r)
		}
	}
	if x, ok := cfg.FactoryRole("fx-extra"); !ok || x.Doing != "looking at extras" || x.Mode != ModeSession {
		t.Errorf("fx-extra = %+v, %v: a new role is appended and normalized", x, ok)
	}
	if len(cfg.Factory.Roles) != len(base.Factory.Roles)+1 {
		t.Errorf("%d factory roles, want %d", len(cfg.Factory.Roles), len(base.Factory.Roles)+1)
	}
}

// Both the overlay's [factory.local_review] and a lone key of it merge into
// the built-in values, and [[factory.repo]] blocks are appended.
func TestFactoryOverlayKeepsWhatItDoesNotSet(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+factoryRepoToml+`
[factory]
max_active = 2

[factory.local_review]
cycles = 2
`)
	if err != nil {
		t.Fatal(err)
	}
	f := cfg.Factory
	if f.MaxActive != 2 || f.Poll.Duration != 45*time.Second || f.HeavyStages != 1 || f.HerdrSession != "factory" {
		t.Errorf("factory = %+v", f)
	}
	if f.LocalReview.Cycles != 2 || !slices.Equal(f.LocalReview.LaterWithout, []string{"claude-simplify"}) {
		t.Errorf("local_review = %+v, want 2 cycles and the built-in later_without", f.LocalReview)
	}
	r, ok := cfg.FactoryRepo("Talkable/Talkable")
	if !ok || r.Base != "master" || r.CopyName != "factory{n}" || r.Precheck.Cap.Duration != 10*time.Minute || r.MaxCopies != 4 {
		t.Fatalf("repo = %+v, %v: base, copy_name and the precheck cap take their defaults", r, ok)
	}
	if !strings.HasSuffix(r.MainClone, "/Projects/talkable") || strings.Contains(r.MainClone, "~") {
		t.Errorf("main_clone = %q, want ~ expanded", r.MainClone)
	}
	if _, ok := cfg.FactoryRepo("talkable/other"); ok {
		t.Error("FactoryRepo found a repository that is not configured")
	}
}

// A complete base file (--config) layers over the built-in values the same
// way, and a role whose prompt a layer changes takes the new prompt for its
// other turns too (they derive from it after the layers merged).
func TestFactoryBaseFileLayersOverTheBuiltInValues(t *testing.T) {
	cfg, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + `
[factory]
max_active = 5

[[factory.role]]
name = "fx-writer"
effort = "medium"
prompt = "factory-judge.md"
`})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Factory.MaxActive != 5 || cfg.Factory.HeavyStages != 1 || len(cfg.Factory.Kinds) != 3 {
		t.Errorf("factory = %+v", cfg.Factory)
	}
	w, _ := cfg.FactoryRole("fx-writer")
	if w.Effort != "medium" || w.Model != "opus" || w.Prompt != "factory-judge.md" || w.Rereview != "factory-judge.md" || w.Doing != "writing the change" {
		t.Errorf("fx-writer = %+v", w)
	}
	if len(cfg.Factory.Roles) != len(Defaults().Factory.Roles) {
		t.Errorf("%d factory roles, want %d", len(cfg.Factory.Roles), len(Defaults().Factory.Roles))
	}
}

// Two configurations never share a map or a slice of the built-in factory.
func TestFactoryDefaultsAreFreshEveryTime(t *testing.T) {
	a, b := Defaults().Factory, Defaults().Factory
	a.Kinds["bug"] = FactoryKind{}
	a.CodexFallback["fx-cause"] = "changed"
	a.KindWords["bug"][0] = "changed"
	a.Roles[0].Why = "changed"
	a.LocalReview.LaterWithout[0] = "changed"
	if len(b.Kinds["bug"].Stages) == 0 || b.CodexFallback["fx-cause"] != "fx-cause-claude" || b.KindWords["bug"][0] != "bug" ||
		b.Roles[0].Why == "changed" || b.LocalReview.LaterWithout[0] != "claude-simplify" {
		t.Errorf("a change to one Defaults() reached another: %+v", b)
	}
}

func TestFactoryRejectsAStageOfUnknownType(t *testing.T) {
	cfg := validFactoryConfig()
	editKind(cfg, "bug", func(s []FactoryStage) []FactoryStage { s[0].Type = "command"; return s })
	wantError(t, cfg.Validate(), "factory kind bug", `stage "investigate"`, `type "command"`)
}

func TestFactoryRejectsAnUnknownEndFact(t *testing.T) {
	cfg := validFactoryConfig()
	editKind(cfg, "bug", func(s []FactoryStage) []FactoryStage { s[0].End = "vibes"; return s })
	wantError(t, cfg.Validate(), "factory kind bug", `stage "investigate"`, `end "vibes"`)
}

// The other fields a stage needs follow its type: an agents stage names
// roles that exist and an end fact, a gate its gate, and a field of another
// type is a mistake, not a no-op.
func TestFactoryChecksEachStageAgainstItsType(t *testing.T) {
	cases := map[string]struct {
		edit func(s []FactoryStage) []FactoryStage
		want []string
	}{
		"no roles":     {func(s []FactoryStage) []FactoryStage { s[0].Roles = nil; return s }, []string{`stage "investigate"`, "needs roles"}},
		"unknown role": {func(s []FactoryStage) []FactoryStage { s[0].Roles = []string{"fx-nope"}; return s }, []string{`stage "investigate"`, `unknown role "fx-nope"`}},
		"no end fact":  {func(s []FactoryStage) []FactoryStage { s[0].End = ""; return s }, []string{`stage "investigate"`, "needs an end fact"}},
		"unknown gate": {func(s []FactoryStage) []FactoryStage { s[findStage(s, "questions")].Gate = "vibes"; return s }, []string{`stage "questions"`, `gate "vibes"`}},
		"gate on agents": {func(s []FactoryStage) []FactoryStage { s[0].Gate = FactoryGateReproduced; return s },
			[]string{`stage "investigate"`, "gate is for gate stages"}},
		"unknown decision": {func(s []FactoryStage) []FactoryStage { s[findStage(s, "ready")].Decision = "vibes"; return s }, []string{`stage "ready"`, `decision "vibes"`}},
		"unknown effect":   {func(s []FactoryStage) []FactoryStage { s[findStage(s, "publish")].Effect = "vibes"; return s }, []string{`stage "publish"`, `effect "vibes"`}},
		"unknown when":     {func(s []FactoryStage) []FactoryStage { s[0].When = "sometimes"; return s }, []string{`stage "investigate"`, `when "sometimes"`}},
		"prompt on a gate": {stagePrompt("questions", "factory-judge.md"), []string{`stage "questions"`, "prompt is for agents stages only"}},
		"unknown prompt":   {stagePrompt("pr_body", "factory-nope.md"), []string{`stage "pr_body"`, "prompt", "factory-nope.md"}},
		"twice":            {func(s []FactoryStage) []FactoryStage { s[1].Name = s[0].Name; return s }, []string{`stage "investigate"`, "twice"}},
		"no stages":        {func(s []FactoryStage) []FactoryStage { return nil }, []string{"factory kind bug", "at least one stage"}},
		"publish without a decision": {func(s []FactoryStage) []FactoryStage {
			return slices.DeleteFunc(s, func(x FactoryStage) bool { return x.Type == FactoryStageDecision })
		}, []string{`stage "publish"`, "after a " + FactoryDecisionReadyPush + " decision"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validFactoryConfig()
			editKind(cfg, "bug", tc.edit)
			wantError(t, cfg.Validate(), append([]string{"factory kind bug"}, tc.want...)...)
		})
	}
}

// An agents stage may give its roles a prompt of its own, in place of each
// role's: a file that resolves and parses.
func TestAnAgentsStageMayNameAPromptForItsRoles(t *testing.T) {
	cfg := validFactoryConfig()
	editKind(cfg, "bug", stagePrompt("pr_body", "factory-amend.md"))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a stage prompt that resolves: %v", err)
	}
}

// stagePrompt is an edit of a kind's stages: stage gets prompt.
func stagePrompt(stage, prompt string) func(s []FactoryStage) []FactoryStage {
	return func(s []FactoryStage) []FactoryStage {
		s[findStage(s, stage)].Prompt = prompt
		return s
	}
}

func findStage(stages []FactoryStage, name string) int {
	return slices.IndexFunc(stages, func(s FactoryStage) bool { return s.Name == name })
}

// A copy takes the name and the directory of a slot of its own: the review
// pool's slot names are unique across pools, and a copy that rendered one
// would be taken for that slot.
func TestFactoryRejectsACopyNameThatIsAReviewSlotName(t *testing.T) {
	cfg := validFactoryConfig()
	cfg.Pools = []Pool{{Repo: "acme/app", MainClone: "/p/app", SlotName: "review{n}", SlotPath: "/p/app.review{n}", Max: 2}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("different names are valid: %v", err)
	}
	cfg.Factory.Repos[0].CopyName = "review{n}"
	wantError(t, cfg.Validate(), "factory repo acme/app", `copy_name "review{n}"`, "pool acme/app", `slot_name "review{n}"`)

	cfg = validFactoryConfig()
	cfg.Pools = []Pool{{Repo: "acme/other", MainClone: "/p/other", SlotName: "factory{n}", SlotPath: "/p/other.review{n}", Max: 2}}
	wantError(t, cfg.Validate(), "factory repo acme/app", `copy_name "factory{n}"`, "pool acme/other")

	cfg = validFactoryConfig()
	cfg.Pools = []Pool{{Repo: "acme/app", MainClone: "/p/app", SlotName: "review{n}", SlotPath: "/p/app.factory{n}", Max: 2}}
	wantError(t, cfg.Validate(), "factory repo acme/app", `copies "/p/app.factory{n}"`, "pool acme/app", `slot_path`)
}

func TestFactoryRejectsAStagingAfterAnUnknownName(t *testing.T) {
	cfg := validFactoryConfig()
	cfg.Factory.Repos[0].Staging = []FactoryStaging{
		{Name: "void", Branch: "staging-void", QA: "t:qa"},
		{Name: "bastion", Branch: "staging-bastion", QA: "t:qa", After: "void"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an environment after a declared one is valid: %v", err)
	}
	cfg.Factory.Repos[0].Staging[1].After = "mars"
	wantError(t, cfg.Validate(), "factory repo acme/app", `staging "bastion"`, `after names unknown staging "mars"`)

	cfg.Factory.Repos[0].Staging[1].After = "bastion"
	wantError(t, cfg.Validate(), `staging "bastion"`, "after names itself")

	cfg.Factory.Repos[0].Staging = []FactoryStaging{{Name: "a", Branch: "staging-a", After: "b"}, {Name: "b", Branch: "staging-b", After: "a"}}
	wantError(t, cfg.Validate(), "staging after forms a cycle")

	cfg.Factory.Repos[0].Staging = []FactoryStaging{{Name: "a", Branch: "x"}, {Name: "a", Branch: "y"}, {Name: "", Branch: "z"}, {Name: "c"}}
	err := cfg.Validate()
	wantError(t, err, `staging "a" is declared twice`, "staging needs a name", `staging "c" needs a branch`)
}

func TestFactoryHerdrSocketMustBeAbsolute(t *testing.T) {
	cfg := validFactoryConfig()
	cfg.Factory.HerdrSocket = "run/factory.sock"
	wantError(t, cfg.Validate(), "factory.herdr_socket", "absolute", "run/factory.sock")
	cfg.Factory.HerdrSocket = "/run/factory.sock"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Factory.HerdrSocket = "" // the named session's own socket
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Factory.HerdrSession = ""
	wantError(t, cfg.Validate(), "factory.herdr_session")
	cfg.Factory.HerdrSession = "two words"
	wantError(t, cfg.Validate(), "factory.herdr_session", "two words")
}

func TestFactoryLocalReviewDefaultsToThreeRoundsWithoutSimplifyLater(t *testing.T) {
	for _, lr := range []FactoryLocalReview{Defaults().Factory.LocalReview, mustLoadDefaults(t).Factory.LocalReview} {
		if lr.Cycles != 3 || !slices.Equal(lr.LaterWithout, []string{"claude-simplify"}) {
			t.Errorf("local_review = %+v, want 3 cycles and later rounds without claude-simplify", lr)
		}
	}
	cfg := validFactoryConfig()
	cfg.Factory.LocalReview.Cycles = 0
	wantError(t, cfg.Validate(), "factory.local_review.cycles")
	cfg.Factory.LocalReview = FactoryLocalReview{Cycles: 3, LaterWithout: []string{"claude-simplify", "claude-simplify"}}
	wantError(t, cfg.Validate(), "factory.local_review.later_without", "twice")
	cfg.Factory.LocalReview.LaterWithout = []string{"Not A Role"}
	wantError(t, cfg.Validate(), "factory.local_review.later_without", "Not A Role")
}

func mustLoadDefaults(t *testing.T) *Config {
	t.Helper()
	cfg, err := LoadWithOptions(paths.Layout{Home: repoRoot(t)}, "", LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The first local round runs the review roles of the repository's watch, so
// a factory repository no watch covers cannot review its change.
func TestFactoryRepoNeedsAWatchForItsLocalReview(t *testing.T) {
	cfg := validFactoryConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Factory.Repos[0].Repo = "elsewhere/app"
	wantError(t, cfg.Validate(), "factory repo elsewhere/app", "no [[watch]] covers it")
	// An unknown later_without role is only a warning, and only for a
	// factory that has a repository.
	cfg = validFactoryConfig()
	cfg.Factory.LocalReview.LaterWithout = []string{"claude-simplyfy"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if w := cfg.Warnings(); !slices.ContainsFunc(w, func(s string) bool { return strings.Contains(s, "claude-simplyfy") }) {
		t.Errorf("warnings = %q, want one naming claude-simplyfy", w)
	}
	cfg.Factory.Repos = nil
	if w := cfg.Warnings(); slices.ContainsFunc(w, func(s string) bool { return strings.Contains(s, "claude-simplyfy") }) {
		t.Errorf("warnings = %q for a factory with no repository", w)
	}
}

func TestFactoryPrecheckCapDefaultsToTenMinutes(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+factoryRepoToml)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := cfg.FactoryRepo("talkable/talkable")
	if r.Precheck.Cap.Duration != 10*time.Minute {
		t.Errorf("precheck cap = %s, want 10m", r.Precheck.Cap.Duration)
	}
	cfg, err = loadCommittedWithLocal(t, testLocalConfig+factoryRepoToml+"\n[factory.repo.precheck]\ncap = \"3m\"\nlint = [\"bin/lint {files}\"]\nspecs = \"bin/specs {files}\"\n")
	if err != nil {
		t.Fatal(err)
	}
	r, _ = cfg.FactoryRepo("talkable/talkable")
	if r.Precheck.Cap.Duration != 3*time.Minute || r.Precheck.Specs != "bin/specs {files}" || len(r.Precheck.Lint) != 1 {
		t.Errorf("precheck = %+v", r.Precheck)
	}
	cfg.Factory.Repos[0].Precheck.Cap = Duration{}
	wantError(t, cfg.Validate(), "factory repo talkable/talkable", "precheck.cap must be positive")
}

func TestFactoryPrecheckSkipsSpecsOnlyForKnownKinds(t *testing.T) {
	cfg := validFactoryConfig()
	cfg.Factory.Repos[0].Precheck.SkipSpecsForKinds = []string{"chore"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Factory.Repos[0].Precheck.SkipSpecsForKinds = []string{"chore", "gem"}
	wantError(t, cfg.Validate(), "factory repo acme/app", "precheck.skip_specs_for_kinds", `"gem"`, "bug, chore, feature")
	cfg.Factory.Repos[0].Precheck = FactoryPrecheck{Cap: Duration{time.Minute}, Lint: []string{""}, Migrations: []string{" "}}
	wantError(t, cfg.Validate(), "precheck.lint", "precheck.migrations")
}

// A kind's proof says whether the factory's proof needs the spec red on the
// base (red, the default: a bug, a feature) or green at the head only
// (green: a chore, whose specs stay green); anything else is a mistake.
func TestAFactoryKindsProofIsRedOrGreen(t *testing.T) {
	for _, cfg := range []*Config{Defaults(), mustLoadDefaults(t)} {
		if p := cfg.Factory.Kinds["chore"].Proof; p != FactoryProofGreen {
			t.Errorf("the chore kind's proof is %q, want green", p)
		}
		for _, kind := range []string{"bug", "feature"} {
			if k := cfg.Factory.Kinds[kind]; !k.RedRun() {
				t.Errorf("the %s kind's proof %q needs no red run", kind, k.Proof)
			}
		}
	}
	if (FactoryKind{}).RedRun() != true || (FactoryKind{Proof: FactoryProofRed}).RedRun() != true {
		t.Error("an unset or red proof needs no red run")
	}
	cfg := validFactoryConfig()
	k := cfg.Factory.Kinds["bug"]
	k.Proof = "amber"
	cfg.Factory.Kinds["bug"] = k
	wantError(t, cfg.Validate(), "factory kind bug", `proof "amber"`, "red", "green")
}

func TestFactoryHeavyStagesMustBeAtLeastOne(t *testing.T) {
	cfg := validFactoryConfig()
	if cfg.Factory.HeavyStages != 1 {
		t.Fatalf("heavy_stages defaults to %d, want 1", cfg.Factory.HeavyStages)
	}
	cfg.Factory.HeavyStages = 0
	wantError(t, cfg.Validate(), "factory.heavy_stages must be at least 1")
	cfg = validFactoryConfig()
	cfg.Factory.MaxActive, cfg.Factory.MemoryGateSwap = 0, 101
	err := cfg.Validate()
	wantError(t, err, "factory.max_active must be at least 1", "factory.memory_gate_swap must be between 0")
}

func TestFactoryReviewerMustBeALogin(t *testing.T) {
	cfg := validFactoryConfig()
	cfg.Factory.Repos[0].Reviewer = "rev-ann"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"two words", "app/helper", "@rev-ann", "team/x"} {
		cfg.Factory.Repos[0].Reviewer = bad
		wantError(t, cfg.Validate(), "factory repo acme/app", "reviewer", bad)
	}
}

func TestFactoryCodexFallbackMustNameAClaudeRole(t *testing.T) {
	cfg := validFactoryConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for key, fallback := range cfg.Factory.CodexFallback {
		if r, _ := cfg.FactoryRole(key); r.Kind != KindCodex {
			t.Errorf("codex_fallback key %s is a %s role", key, r.Kind)
		}
		if r, _ := cfg.FactoryRole(fallback); r.Kind != KindClaude {
			t.Errorf("fallback %s of %s is a %s role", fallback, key, r.Kind)
		}
	}
	for name, edit := range map[string]func(*Config){
		"a codex fallback":    func(c *Config) { c.Factory.CodexFallback["fx-critic"] = "fx-cause" },
		"an unknown fallback": func(c *Config) { c.Factory.CodexFallback["fx-critic"] = "fx-nope" },
		"an empty fallback":   func(c *Config) { c.Factory.CodexFallback["fx-critic"] = "" },
		"a codex value under a claude key": func(c *Config) {
			c.Factory.CodexFallback["fx-judge"] = "fx-cause"
		},
		"an unknown role as key": func(c *Config) { c.Factory.CodexFallback["fx-nope"] = "fx-writer" },
	} {
		t.Run(name, func(t *testing.T) {
			c := validFactoryConfig()
			edit(c)
			wantError(t, c.Validate(), "factory.codex_fallback")
		})
	}
	// A Codex role with no fallback is a warning: after a Codex refusal the
	// task would wait for the operator.
	cfg = validFactoryConfig()
	delete(cfg.Factory.CodexFallback, "fx-critic")
	if w := cfg.Warnings(); !slices.ContainsFunc(w, func(s string) bool { return strings.Contains(s, "fx-critic") && strings.Contains(s, "codex_fallback") }) {
		t.Errorf("warnings = %q, want one for fx-critic", w)
	}
}

// A user with no Codex, or who wants Claude on a role the defaults run on
// Codex, moves the role to Claude with a [[factory.role]] that sets its kind.
// Its codex_fallback entry then has no use: that is a warning, not an error
// (nothing else in the configuration could get rid of it), and the entry is
// not used. A key that names no role, and a value that names no Claude role,
// stay errors.
func TestAFactoryRoleCanMoveFromCodexToClaude(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+`
[[factory.role]]
name = "fx-cause"
kind = "claude"
`)
	if err != nil {
		t.Fatalf("a Codex role moved to Claude: %v", err)
	}
	if r, _ := cfg.FactoryRole("fx-cause"); r.Kind != KindClaude || r.Doing != "finding the cause" {
		t.Fatalf("fx-cause = %+v, want the claude kind and its other keys", r)
	}
	want := "factory.codex_fallback key fx-cause is a claude role: its fallback is not used"
	if w := cfg.Warnings(); !slices.Contains(w, want) {
		t.Errorf("warnings = %q, want %q", w, want)
	}
	// The other Codex role keeps its fallback and its silence.
	if w := cfg.Warnings(); slices.ContainsFunc(w, func(s string) bool { return strings.Contains(s, "fx-critic") }) {
		t.Errorf("warnings = %q, fx-critic is still on Codex with its fallback", w)
	}

	// The same through Validate on a built configuration, and the entry
	// stays refused when its value is no Claude role.
	c := validFactoryConfig()
	c.Factory.Roles[slices.IndexFunc(c.Factory.Roles, func(r Role) bool { return r.Name == "fx-cause" })].Kind = KindClaude
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Factory.CodexFallback["fx-cause"] = "fx-critic"
	wantError(t, c.Validate(), "factory.codex_fallback fx-cause", "must be a claude role")
}

// An empty value in the user's codex_fallback removes the built-in entry
// (the tables merge entry by entry, so an empty table changes nothing).
func TestAnEmptyCodexFallbackRemovesTheDefault(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+`
[factory]
codex_fallback = { fx-cause = "" }
`)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := cfg.Factory.CodexFallback["fx-cause"]; ok {
		t.Errorf("codex_fallback still has fx-cause = %q", got)
	}
	if got := cfg.Factory.CodexFallback["fx-critic"]; got != "fx-critic-claude" {
		t.Errorf("codex_fallback fx-critic = %q, want the default kept", got)
	}
	if w := cfg.Warnings(); !slices.ContainsFunc(w, func(s string) bool { return strings.Contains(s, "fx-cause") && strings.Contains(s, "codex_fallback") }) {
		t.Errorf("warnings = %q, want one for fx-cause, a Codex role with no fallback now", w)
	}

	cfg, err = loadCommittedWithLocal(t, testLocalConfig+"\n[factory]\ncodex_fallback = {}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Factory.CodexFallback) != 2 {
		t.Errorf("an empty table changed the entries: %v", cfg.Factory.CodexFallback)
	}

	// A later layer can set the entry again; the base file removes it too.
	cfg, err = loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + "\n[factory]\ncodex_fallback = { fx-critic = \"\" }\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Factory.CodexFallback["fx-critic"]; ok || cfg.Factory.CodexFallback["fx-cause"] != "fx-cause-claude" {
		t.Errorf("codex_fallback = %v, want fx-critic removed by the base file", cfg.Factory.CodexFallback)
	}
}

// A factory role is an agent session that carries the two phrases the
// screen names its agent by; a review role carries neither.
func TestFactoryRolesNeedWhyAndDoingAndReviewRolesTakeNeither(t *testing.T) {
	cfg := validFactoryConfig()
	for _, r := range cfg.Factory.Roles {
		if r.Why == "" || r.Doing == "" {
			t.Errorf("default factory role %s has why %q and doing %q", r.Name, r.Why, r.Doing)
		}
	}
	i := slices.IndexFunc(cfg.Factory.Roles, func(r Role) bool { return r.Name == "fx-cause" })
	cfg.Factory.Roles[i].Why, cfg.Factory.Roles[i].Doing = "", "two\nlines"
	wantError(t, cfg.Validate(), "factory.role fx-cause", "why", "doing")

	cfg = validFactoryConfig()
	cfg.Roles[0].Doing = "reviewing"
	wantError(t, cfg.Validate(), "role "+cfg.Roles[0].Name, "why and doing are for [[factory.role]]")
}

func TestFactoryRolesAreCheckedLikeReviewRoles(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(r *Role)
		want string
	}{
		"unknown kind":   {func(r *Role) { r.Kind = "gemini" }, `kind "gemini" is not declared`},
		"a shell role":   {func(r *Role) { r.Kind, r.Mode, r.Command = KindShell, ModeShell, "true" }, "must be an agent session"},
		"a judge":        {func(r *Role) { r.Judge = true }, "cannot be a judge"},
		"a fix role":     {func(r *Role) { r.Fix = true }, "cannot be a fix role"},
		"an identity":    {func(r *Role) { r.Identity = "me" }, "identity is not used"},
		"missing prompt": {func(r *Role) { r.Prompt = "factory-nope.md" }, "factory-nope.md"},
		"bad name":       {func(r *Role) { r.Name = "FX" }, "name must match"},
		"a review name":  {func(r *Role) { r.Name = "claude-review" }, "claude-review"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validFactoryConfig()
			i := slices.IndexFunc(cfg.Factory.Roles, func(r Role) bool { return r.Name == "fx-history" })
			tc.edit(&cfg.Factory.Roles[i])
			wantError(t, cfg.Validate(), tc.want)
		})
	}
	cfg := validFactoryConfig()
	cfg.Factory.Roles = append(cfg.Factory.Roles, cfg.Factory.Roles[0])
	wantError(t, cfg.Validate(), "factory.role fx-repro", "already")
}

// A role overrides its kind's on_permission_prompt, in a [[factory.role]] as in
// a [[role]]: "deny" or "wait", merged by name key by key like the other keys,
// and a role that sets none answers as its kind does.
func TestAFactoryRoleTakesOnPermissionPrompt(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+`
[[factory.role]]
name = "fx-cause"
on_permission_prompt = "wait"

[[role]]
name = "claude-simplify"
on_permission_prompt = " Wait "
`)
	if err != nil {
		t.Fatalf("a factory role that waits: %v", err)
	}
	r, ok := cfg.FactoryRole("fx-cause")
	if !ok || r.OnPermissionPrompt != PermissionWait || r.Doing != "finding the cause" {
		t.Fatalf("fx-cause = %+v, want on_permission_prompt wait and its other keys kept", r)
	}
	kind, _ := cfg.KindSpec(r.Kind)
	if got := r.PermissionPrompt(kind); got != PermissionWait {
		t.Errorf("fx-cause PermissionPrompt = %q, want wait", got)
	}
	if other, _ := cfg.FactoryRole("fx-critic"); other.OnPermissionPrompt != "" {
		t.Errorf("fx-critic on_permission_prompt = %q, want none: the override is the one role's", other.OnPermissionPrompt)
	}
	if rev, ok := cfg.RoleByNameOrAlias(nil, "claude-simplify"); !ok || rev.OnPermissionPrompt != PermissionWait {
		t.Errorf("claude-simplify = %+v, %v, want on_permission_prompt wait (case and space ignored)", rev, ok)
	}

	for _, section := range []string{"[[factory.role]]\nname = \"fx-cause\"", "[[role]]\nname = \"claude-simplify\""} {
		_, err = loadCommittedWithLocal(t, testLocalConfig+"\n"+section+"\non_permission_prompt = \"maybe\"\n")
		wantError(t, err, "on_permission_prompt", `deny or wait, got "maybe"`)
	}
	c := validFactoryConfig()
	c.Factory.Roles[0].OnPermissionPrompt = "approve"
	wantError(t, c.Validate(), "factory.role "+c.Factory.Roles[0].Name, "on_permission_prompt must be deny or wait")
	c = validFactoryConfig()
	c.Roles[0].OnPermissionPrompt = PermissionDeny
	if err := c.Validate(); err != nil {
		t.Errorf("a review role that denies: %v", err)
	}
}

// PermissionPrompt is the role's value, else its kind's.
func TestARolePermissionPromptFallsBackToItsKind(t *testing.T) {
	for _, tc := range []struct{ role, kind, want string }{
		{"", PermissionDeny, PermissionDeny},
		{"", PermissionWait, PermissionWait},
		{PermissionWait, PermissionDeny, PermissionWait},
		{PermissionDeny, PermissionWait, PermissionDeny},
	} {
		got := Role{OnPermissionPrompt: tc.role}.PermissionPrompt(Kind{OnPermissionPrompt: tc.kind})
		if got != tc.want {
			t.Errorf("role %q on kind %q = %q, want %q", tc.role, tc.kind, got, tc.want)
		}
	}
}

// A shell role runs a command, not an agent that stops at a prompt, so the
// key would be dropped without a word.
func TestAShellRoleTakesNoOnPermissionPrompt(t *testing.T) {
	c := validFactoryConfig()
	i := slices.IndexFunc(c.Roles, func(r Role) bool { return r.IsShell() })
	if i < 0 {
		t.Fatal("no shell role among the defaults")
	}
	c.Roles[i].OnPermissionPrompt = PermissionWait
	wantError(t, c.Validate(), "role "+c.Roles[i].Name, "on_permission_prompt is for session roles only")
}

// The repository's checks: the owner/name form once, the copy paths and
// names with {n}, positive limits, databases the way a pool names them, and
// the commands that are lists of words.
func TestFactoryChecksTheRepositoryBlock(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(r *FactoryRepo)
		want []string
	}{
		"no owner":         {func(r *FactoryRepo) { r.Repo = "app" }, []string{`factory repo "app"`, "owner/name"}},
		"no main clone":    {func(r *FactoryRepo) { r.MainClone = "" }, []string{"main_clone"}},
		"copies lack {n}":  {func(r *FactoryRepo) { r.Copies = "/p/app.factory" }, []string{"copies", "{n}"}},
		"copy name lacks":  {func(r *FactoryRepo) { r.CopyName = "factory" }, []string{"copy_name", "{n}"}},
		"no copies":        {func(r *FactoryRepo) { r.MaxCopies = -1 }, []string{"max_copies must be at least 1"}},
		"negative disk":    {func(r *FactoryRepo) { r.MinFreeDiskGB = -1 }, []string{"min_free_disk_gb"}},
		"bad database":     {func(r *FactoryRepo) { r.Databases = []string{"app_development"} }, []string{"database template", DatabaseSuffix}},
		"bad env key":      {func(r *FactoryRepo) { r.Env = map[string]string{"1X": "y"} }, []string{"env key", "1X"}},
		"empty install":    {func(r *FactoryRepo) { r.Installs = []FactoryInstall{{Lock: "Gemfile.lock"}} }, []string{"installs", "run"}},
		"blank setup":      {func(r *FactoryRepo) { r.Setup = []string{" "} }, []string{"setup"}},
		"negative rows":    {func(r *FactoryRepo) { r.LargeTableRows = -5 }, []string{"large_table_rows"}},
		"blank console":    {func(r *FactoryRepo) { r.Consoles = []string{""} }, []string{"consoles"}},
		"error skill":      {func(r *FactoryRepo) { r.ErrorSkill = "two words" }, []string{"error_skill"}},
		"pr body skill":    {func(r *FactoryRepo) { r.PRBodySkill = "two words" }, []string{"pr_body_skill", "two words"}},
		"copy file escape": {func(r *FactoryRepo) { r.CopyFiles = []string{"../secret"} }, []string{"copy_files"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validFactoryConfig()
			tc.edit(&cfg.Factory.Repos[0])
			wantError(t, cfg.Validate(), tc.want...)
		})
	}
	cfg := validFactoryConfig()
	cfg.Factory.Repos = append(cfg.Factory.Repos, cfg.Factory.Repos[0])
	wantError(t, cfg.Validate(), "factory repo acme/app", "configured twice")
}

// The Jira type and the words of a prompt pick a kind of task that exists.
func TestFactoryKindPickersNameKnownKinds(t *testing.T) {
	cfg := validFactoryConfig()
	cfg.Factory.KindByJiraType["Epic"] = "saga"
	cfg.Factory.KindWords["saga"] = []string{"epic"}
	cfg.Factory.KindWords["bug"] = []string{" "}
	err := cfg.Validate()
	wantError(t, err, "factory.kind_by_jira_type", `"saga"`, "factory.kind_words", "bug")
}

// A factory repository's copies come out as the Pool the slot code provisions.
func TestFactoryRepoPoolIsTheCopiesAsAPool(t *testing.T) {
	r := FactoryRepo{Repo: "acme/app", Base: "main", MainClone: "/p/app", Copies: "/p/app.factory{n}", CopyName: "factory{n}", MaxCopies: 4,
		MinFreeDiskGB: 20, Setup: []string{"bin/setup"}, Teardown: []string{"bin/down"}, PostCheckout: []string{"bin/post"}, ResetDB: []string{"bin/reset"},
		Databases: []string{"app__{slug}"}, Env: map[string]string{"A": "{slot}"}, CopyFiles: []string{".env"}, StripEnv: []string{"X"}}
	p := r.Pool()
	if p.Repo != "acme/app" || p.MainClone != "/p/app" || p.SlotName != "factory{n}" || p.SlotPath != "/p/app.factory{n}" || p.Base != "main" ||
		p.Min != 0 || p.Max != 4 || p.MinFreeDiskGB != 20 || p.IdleRemoveAfter.Duration != DefaultIdleRemoveAfter {
		t.Errorf("pool = %+v", p)
	}
	if !slices.Equal(p.Setup, r.Setup) || !slices.Equal(p.Teardown, r.Teardown) || !slices.Equal(p.PostCheckout, r.PostCheckout) ||
		!slices.Equal(p.ResetDB, r.ResetDB) || !slices.Equal(p.Databases, r.Databases) || !slices.Equal(p.CopyFiles, r.CopyFiles) ||
		!slices.Equal(p.StripEnv, r.StripEnv) || p.Env["A"] != "{slot}" {
		t.Errorf("pool = %+v does not carry the repository's commands", p)
	}
	if p.Slot(2) != "factory2" || !strings.HasSuffix(p.Path(2), "/p/app.factory2") {
		t.Errorf("slot 2 = %q at %q", p.Slot(2), p.Path(2))
	}
	p.Setup[0] = "changed"
	if r.Setup[0] != "bin/setup" {
		t.Error("Pool shares the repository's slices")
	}
}

// The Go defaults and config.defaults.toml are two writings of the same
// factory: the roles (normalized, as every role is) and the kinds equal.
func TestFactoryGoDefaultsEqualTheEmbeddedFile(t *testing.T) {
	file := mustLoadDefaults(t).Factory
	goDefaults := Defaults().Factory
	if !equalAny(file.Roles, goDefaults.Roles) {
		t.Errorf("roles differ:\nfile %+v\nGo   %+v", file.Roles, goDefaults.Roles)
	}
	if !equalAny(file.Kinds, goDefaults.Kinds) || !equalAny(file.CodexFallback, goDefaults.CodexFallback) ||
		!equalAny(file.KindByJiraType, goDefaults.KindByJiraType) || !equalAny(file.KindWords, goDefaults.KindWords) {
		t.Errorf("tables differ:\nfile %+v\nGo   %+v", file, goDefaults)
	}
	// A base file that writes no [factory] starts from the Go defaults.
	cfg, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig})
	if err != nil {
		t.Fatal(err)
	}
	if !equalAny(cfg.Factory, goDefaults) {
		t.Errorf("a config without [factory] has %+v, want the defaults %+v", cfg.Factory, goDefaults)
	}
}

// The factory reads its prompts from files that exist, are templates and
// stay out of the review pipeline's data shapes.
func TestFactoryRolesNameEmbeddedPrompts(t *testing.T) {
	cfg := mustLoadDefaults(t)
	for _, r := range cfg.Factory.Roles {
		p, err := cfg.RolePrompt(r, PromptInitial)
		if err != nil {
			t.Errorf("role %s: %v", r.Name, err)
			continue
		}
		if !strings.HasPrefix(p.Name, "factory-") {
			t.Errorf("role %s prompt %s is not a factory prompt", r.Name, p.Name)
		}
		if err := parseTemplate(p.Name, p.Text); err != nil {
			t.Errorf("prompt %s: %v", p.Name, err)
		}
	}
}

// The files the ready card's FLAGS line names as sensitive are a [factory]
// key, sensitive: path globs (MatchPath's), by default Gemfile.lock, the
// workflows and the config files; a user's list replaces it, and a
// malformed glob is refused.
func TestFactorySensitiveFilesAreConfigured(t *testing.T) {
	want := []string{"Gemfile.lock", ".github/workflows/**", "config/**"}
	for _, cfg := range []*Config{mustLoadDefaults(t), Defaults()} {
		if !slices.Equal(cfg.Factory.Sensitive, want) {
			t.Fatalf("factory.sensitive = %q, want %q", cfg.Factory.Sensitive, want)
		}
	}
	cfg, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + "\n[factory]\nsensitive = [\"db/structure.sql\"]\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Factory.Sensitive, []string{"db/structure.sql"}) {
		t.Fatalf("a config's sensitive = %q, want it to replace the defaults", cfg.Factory.Sensitive)
	}
	for _, bad := range []string{"", "config/**x", "a//b"} {
		cfg := validFactoryConfig()
		cfg.Factory.Sensitive = []string{bad}
		wantError(t, cfg.Validate(), "factory.sensitive")
	}
}

// The liaison, fx-liaison, talks with you about a task: a Claude agent that
// asks you questions itself, so magnum never answers its dialogs, and one
// that waits a day for you. No stage names it, so no stage launches it: the
// factory starts it for a task by a request.
func TestTheLiaisonRoleWaitsForYouAndNoStageLaunchesIt(t *testing.T) {
	for _, cfg := range []*Config{mustLoadDefaults(t), Defaults()} {
		r, ok := cfg.FactoryRole("fx-liaison")
		if !ok {
			t.Fatal("the factory has no fx-liaison role")
		}
		if r.Kind != KindClaude || r.Effort != "high" || r.Prompt != "factory-liaison.md" || r.Timeout.Duration != 24*time.Hour ||
			r.OnPermissionPrompt != PermissionWait ||
			r.Why != "talk with you about the task: ask its questions, explain, act on your word" || r.Doing != "talking with you" {
			t.Errorf("fx-liaison = %+v", r)
		}
		if got := r.PermissionPrompt(cfg.Kinds[KindClaude]); got != PermissionWait {
			t.Errorf("fx-liaison answers a permission-shaped dialog by %q, want %q", got, PermissionWait)
		}
		for name, kind := range cfg.Factory.Kinds {
			for _, st := range kind.Stages {
				if slices.Contains(st.Roles, "fx-liaison") {
					t.Errorf("kind %s, stage %s names fx-liaison: a stage would launch it", name, st.Name)
				}
			}
		}
		if errs := cfg.validateFactoryRoles(); len(errs) > 0 {
			t.Errorf("the factory's roles = %v", errs)
		}
	}
}

// The pr_body stage of every built-in kind gives its judge the PR body's
// own prompt.
func TestThePRBodyStageHasItsOwnPrompt(t *testing.T) {
	for _, cfg := range []*Config{mustLoadDefaults(t), Defaults()} {
		for _, name := range []string{"bug", "feature", "chore"} {
			i := slices.IndexFunc(cfg.Factory.Kinds[name].Stages, func(s FactoryStage) bool { return s.End == FactoryEndPRBody })
			if i < 0 || cfg.Factory.Kinds[name].Stages[i].Prompt != "factory-pr-body.md" {
				t.Errorf("kind %s: the pr_body stage %+v, want prompt factory-pr-body.md", name, cfg.Factory.Kinds[name].Stages)
			}
		}
	}
}

// qaRepoToml is factoryRepoToml with what QA needs of the repository: the
// server and the copy's PORT.
const qaRepoToml = factoryRepoToml + `server = "bin/dev"
env = { PORT = "31{n}0" }
`

// validQAConfig is validFactoryConfig whose repository declares QA, every
// key at its default: a server, the copy's PORT, a when and a url.
func validQAConfig() *Config {
	cfg := validFactoryConfig()
	r := &cfg.Factory.Repos[0]
	r.Server = "bin/dev"
	r.Env = map[string]string{"PORT": "31{n}0"}
	r.QA = &FactoryQA{When: "bin/user-facing", URL: "http://app.example.localhost:{port}", Ready: DefaultQAReady,
		Boot: Duration{DefaultQABoot}, Cap: Duration{DefaultQACap}, MaxPages: DefaultQAMaxPages, Fixes: DefaultQAFixes}
	return cfg
}

// A repository runs no visual QA unless its block says how: no
// [factory.repo.qa], no QA.
func TestARepoWithoutAQABlockHasNoQA(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+qaRepoToml)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := cfg.FactoryRepo("talkable/talkable"); !ok || r.QA != nil {
		t.Fatalf("repo = %+v, %v: no [factory.repo.qa] must mean no QA", r.QA, ok)
	}
	if cfg.Factory.Browser != "" {
		t.Errorf("factory.browser = %q, want none: the first browser of DefaultFactoryBrowsers that exists", cfg.Factory.Browser)
	}
}

// [factory.repo.qa] needs only when and url: the ready path, the boot, the
// cap, the pages and the fix turns take the design's values, and the skill
// is trimmed.
func TestTheQABlockTakesItsDefaults(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+qaRepoToml+`
  [factory.repo.qa]
  when = "bin/user-facing"
  url = "http://app.example.localhost:{port}"
  skill = "  example:qa  "
`)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := cfg.FactoryRepo("talkable/talkable")
	q := r.QA
	if q == nil {
		t.Fatal("the repository has no QA")
	}
	if q.When != "bin/user-facing" || q.URL != "http://app.example.localhost:{port}" || q.Skill != "example:qa" || q.SignIn != "" || q.ServerStop != "" {
		t.Errorf("qa = %+v", *q)
	}
	if q.Ready != "/" || q.Boot.Duration != 5*time.Minute || q.Cap.Duration != 50*time.Minute || q.MaxPages != 12 || q.Fixes != 2 {
		t.Errorf("qa = %+v, want ready /, boot 5m, cap 50m, 12 pages and 2 fixes", *q)
	}
	if got := q.PageURL(3110, q.Ready); got != "http://app.example.localhost:3110/" {
		t.Errorf("PageURL = %q", got)
	}
}

// PageURL is the url with the copy's port in place of {port}, then the
// path, with one slash between them: a path without its leading slash stays
// a path on the copy's host, never a login or a host of its own.
func TestQAPageURLRendersThePortThenThePath(t *testing.T) {
	for _, tc := range []struct{ url, path, want string }{
		{"http://app.example.localhost:{port}", "/settings/security", "http://app.example.localhost:3120/settings/security"},
		{"http://127.0.0.1:{port}/", "/", "http://127.0.0.1:3120/"},
		{"https://app.example.localhost:{port}/shop", "/cart", "https://app.example.localhost:3120/shop/cart"},
		{"http://localhost:{port}", "settings", "http://localhost:3120/settings"},
		{"http://localhost:{port}", "@evil.example.com", "http://localhost:3120/@evil.example.com"},
	} {
		got := (FactoryQA{URL: tc.url}).PageURL(3120, tc.path)
		if got != tc.want {
			t.Errorf("PageURL(%q, %q) = %q, want %q", tc.url, tc.path, got, tc.want)
		}
		if u, err := url.Parse(got); err != nil || !LocalHost(u.Hostname()) || u.Port() != "3120" {
			t.Errorf("PageURL(%q, %q) = %q is not on the copy's host and port", tc.url, tc.path, got)
		}
	}
}

// QA walks the copy's own server and nothing else: its url is on this Mac,
// on the copy's port, and the repository has the server and the PORT that
// make that port; the limits stay within the design's.
func TestTheQABlockIsValidated(t *testing.T) {
	if err := validQAConfig().Validate(); err != nil {
		t.Fatalf("a valid QA block: %v", err)
	}
	for name, tc := range map[string]struct {
		edit func(r *FactoryRepo)
		want string
	}{
		"no when":            {func(r *FactoryRepo) { r.QA.When = " " }, "qa.when"},
		"url off the Mac":    {func(r *FactoryRepo) { r.QA.URL = "http://staging.example.com:{port}" }, "qa.url"},
		"url not http":       {func(r *FactoryRepo) { r.QA.URL = "ftp://localhost:{port}" }, "qa.url"},
		"url with a login":   {func(r *FactoryRepo) { r.QA.URL = "http://qa:secret@localhost:{port}" }, "qa.url"},
		"url without {port}": {func(r *FactoryRepo) { r.QA.URL = "http://app.example.localhost:3000" }, "{port}"},
		"{port} not as port": {func(r *FactoryRepo) { r.QA.URL = "http://localhost:3000/{port}" }, "{port}"},
		"a written port 1":   {func(r *FactoryRepo) { r.QA.URL = "http://localhost:1" }, "{port}"},
		"ready":              {func(r *FactoryRepo) { r.QA.Ready = "up" }, "qa.ready"},
		"skill":              {func(r *FactoryRepo) { r.QA.Skill = "a b" }, "qa.skill"},
		"sign_in two lines":  {func(r *FactoryRepo) { r.QA.SignIn = "/dev/sign-in\nthen the form" }, "qa.sign_in"},
		"boot over cap":      {func(r *FactoryRepo) { r.QA.Boot = Duration{time.Hour} }, "qa.boot"},
		"no boot":            {func(r *FactoryRepo) { r.QA.Boot = Duration{-time.Minute} }, "qa.boot"},
		"cap over 3h":        {func(r *FactoryRepo) { r.QA.Boot, r.QA.Cap = Duration{time.Hour}, Duration{4 * time.Hour} }, "qa.cap"},
		"31 pages":           {func(r *FactoryRepo) { r.QA.MaxPages = 31 }, "qa.max_pages"},
		"no pages":           {func(r *FactoryRepo) { r.QA.MaxPages = 0 }, "qa.max_pages"},
		"no fixes":           {func(r *FactoryRepo) { r.QA.Fixes = 0 }, "qa.fixes"},
		"6 fixes":            {func(r *FactoryRepo) { r.QA.Fixes = 6 }, "qa.fixes"},
		"env without PORT":   {func(r *FactoryRepo) { r.Env = map[string]string{"WT_BRANCH": "{copy}"} }, "PORT"},
		"PORT not a number":  {func(r *FactoryRepo) { r.Env["PORT"] = "31{n}O" }, `PORT "31{n}O"`},
		"PORT below 1024":    {func(r *FactoryRepo) { r.Env["PORT"] = "8{n}" }, `PORT "8{n}"`},
		"PORT over 65535":    {func(r *FactoryRepo) { r.Env["PORT"] = "70{n}00" }, `PORT "70{n}00"`},
		"one PORT for all":   {func(r *FactoryRepo) { r.Env["PORT"] = "3000" }, `PORT "3000"`},
		"no server":          {func(r *FactoryRepo) { r.Server = "" }, "server"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validQAConfig()
			tc.edit(&cfg.Factory.Repos[0])
			err := cfg.Validate()
			wantError(t, err, "factory repo acme/app", tc.want)
			if errs := validateErrs(err); len(errs) != 1 {
				t.Errorf("%d errors, want the one of %s alone: %v", len(errs), name, errs)
			}
		})
	}
}

// A qa stage runs the fx-qa walker (or the roles it names) and ends on the
// qa end fact; it looks at a change the review passed, so it comes after a
// review stage, once per kind, and a field of another type is a mistake.
func TestAQAStageNeedsRolesTheQAEndAndAReviewBeforeIt(t *testing.T) {
	qa := func(before string, edits ...func(st *FactoryStage)) func(s []FactoryStage) []FactoryStage {
		return func(s []FactoryStage) []FactoryStage {
			st := FactoryStage{Name: "qa", Type: FactoryStageQA, Roles: []string{"fx-qa"}, End: FactoryEndQA}
			for _, edit := range edits {
				edit(&st)
			}
			return slices.Insert(s, findStage(s, before), st)
		}
	}
	cfg := validFactoryConfig()
	editKind(cfg, "bug", qa("pr_body"))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a qa stage after the local review: %v", err)
	}
	cases := map[string]struct {
		edit func(s []FactoryStage) []FactoryStage
		want []string
	}{
		"before the review": {qa("local_review"), []string{`stage "qa"`, "must come after a review stage"}},
		"no roles":          {qa("pr_body", func(st *FactoryStage) { st.Roles = nil }), []string{`stage "qa"`, "needs roles"}},
		"unknown role":      {qa("pr_body", func(st *FactoryStage) { st.Roles = []string{"fx-nope"} }), []string{`stage "qa"`, `unknown role "fx-nope"`}},
		"end clean":         {qa("pr_body", func(st *FactoryStage) { st.End = FactoryEndClean }), []string{`stage "qa"`, `end "clean" must be qa`}},
		"no end":            {qa("pr_body", func(st *FactoryStage) { st.End = "" }), []string{`stage "qa"`, `end "" must be qa`}},
		"a prompt":          {qa("pr_body", func(st *FactoryStage) { st.Prompt = "factory-qa.md" }), []string{`stage "qa"`, "prompt is for agents stages only"}},
		"when wide":         {qa("pr_body", func(st *FactoryStage) { st.When = FactoryWhenWide }), []string{`stage "qa"`, "when"}},
		"two roles": {qa("pr_body", func(st *FactoryStage) { st.Roles = []string{"fx-qa", "fx-writer"} }),
			[]string{`stage "qa"`, "names exactly one role"}},
		"after the ready card": {qa("publish"), []string{`stage "qa"`, "must come before the " + FactoryDecisionReadyPush + " decision"}},
		"two qa stages": {func(s []FactoryStage) []FactoryStage {
			s = qa("pr_body")(s)
			return qa("pr_body", func(st *FactoryStage) { st.Name = "qa_again" })(s)
		}, []string{`stage "qa_again"`, "at most one qa stage"}},
		"the qa end on an agents stage": {func(s []FactoryStage) []FactoryStage { s[0].End = FactoryEndQA; return s },
			[]string{`stage "investigate"`, `end "qa" is for qa stages only`}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validFactoryConfig()
			editKind(cfg, "bug", tc.edit)
			err := cfg.Validate()
			wantError(t, err, append([]string{"factory kind bug"}, tc.want...)...)
			if errs := validateErrs(err); len(errs) != 1 {
				t.Errorf("%d errors, want the one of %s alone: %v", len(errs), name, errs)
			}
		})
	}
}

// The factory's headless browser is a path it starts as it is: absolute
// after ~ expands, or none (the first of DefaultFactoryBrowsers that exists).
func TestFactoryBrowserMustBeAbsolute(t *testing.T) {
	cfg := validFactoryConfig()
	cfg.Factory.Browser = "chrome"
	wantError(t, cfg.Validate(), "factory.browser", "chrome")
	cfg.Factory.Browser = DefaultFactoryBrowsers[1]
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadCommittedWithLocal(t, testLocalConfig+"\n[factory]\nbrowser = \"~/Applications/Chromium.app/Contents/MacOS/Chromium\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if b := loaded.Factory.Browser; strings.Contains(b, "~") || !strings.HasSuffix(b, "/Applications/Chromium.app/Contents/MacOS/Chromium") {
		t.Errorf("factory.browser = %q, want ~ expanded", b)
	}
}

// LocalHost is true for this Mac's own names only: localhost, a name under
// .localhost, and the loopback addresses QA's pages may sit on.
func TestLocalHostTakesOnlyTheMacsOwnNames(t *testing.T) {
	for _, h := range []string{"localhost", "LOCALHOST", "app.example.localhost", "127.0.0.1", "::1"} {
		if !LocalHost(h) {
			t.Errorf("LocalHost(%q) = false", h)
		}
	}
	for _, h := range []string{"example.com", "localhost.example.com", "10.0.0.1", "127.0.0.2", "", ".localhost", "[::1]"} {
		if LocalHost(h) {
			t.Errorf("LocalHost(%q) = true", h)
		}
	}
}

// fx-qa, the QA walker, looks at the changed pages in a browser: a Claude
// session at high effort with 35 minutes, which no built-in kind names yet.
func TestTheQAWalkerRoleIsAClaudeSessionOf35Minutes(t *testing.T) {
	for _, cfg := range []*Config{mustLoadDefaults(t), Defaults()} {
		r, ok := cfg.FactoryRole("fx-qa")
		if !ok {
			t.Fatal("the factory has no fx-qa role")
		}
		if r.Kind != KindClaude || r.Effort != "high" || r.Prompt != "factory-qa.md" || r.Timeout.Duration != 35*time.Minute ||
			r.Why != "look at the changed pages in a browser, in the states the ticket names" || r.Doing != "walking the pages" {
			t.Errorf("fx-qa = %+v", r)
		}
	}
}
