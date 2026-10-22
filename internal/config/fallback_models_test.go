package config

import (
	"slices"
	"strings"
	"testing"
)

// A role names its own fallback models: [[role]] fallback_models, the
// curator's [notes] fallback_models, the retro classifier's [learn]
// fallback_models and the skill editor's [learn] editor_fallback_models. None
// is set by default (the kind's list applies), and each reaches its role.
func TestEveryFallbackModelsKeyIsReadIntoItsRoleAndEmptyByDefault(t *testing.T) {
	d := Defaults()
	for _, r := range d.Roles {
		if r.FallbackModels != nil {
			t.Errorf("built-in role %s has fallback models %q, want none (the kind's)", r.Name, r.FallbackModels)
		}
	}
	for name, r := range map[string]Role{"notes": d.NotesRole(), "learn": d.LearnRole(), "editor": d.EditorRole()} {
		if r.FallbackModels != nil {
			t.Errorf("%s role has fallback models %q by default, want none (the kind's)", name, r.FallbackModels)
		}
	}
	committed, err := loadCommittedWithLocal(t, testLocalConfig)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Notes.FallbackModels != nil || committed.Learn.FallbackModels != nil || committed.Learn.EditorFallbackModels != nil {
		t.Fatalf("config.defaults.toml sets fallback models: notes %q, learn %q, editor %q",
			committed.Notes.FallbackModels, committed.Learn.FallbackModels, committed.Learn.EditorFallbackModels)
	}

	cfg, err := loadCommittedWithLocal(t, testLocalConfig+`
[[role]]
name = "claude-review"
fallback_models = ["sonnet"]

[notes]
fallback_models = ["haiku"]

[learn]
fallback_models = ["opus"]
editor_fallback_models = ["opus", "haiku"]
`)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := cfg.RoleByNameOrAlias(nil, "claude-review")
	if !ok || !slices.Equal(r.FallbackModels, []string{"sonnet"}) || r.Prompt != "claude-review.md" {
		t.Fatalf("claude-review = %+v, want the built-in role with fallback models [sonnet]", r)
	}
	for name, c := range map[string]struct {
		got, want []string
	}{
		"NotesRole":  {cfg.NotesRole().FallbackModels, []string{"haiku"}},
		"LearnRole":  {cfg.LearnRole().FallbackModels, []string{"opus"}},
		"EditorRole": {cfg.EditorRole().FallbackModels, []string{"opus", "haiku"}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s fallback models = %q, want %q", name, c.got, c.want)
		}
	}
}

// Fallback model names are typed into a pane (`/model <name>`): each entry is
// one word, listed once, and the error names the key that holds it.
func TestAFallbackModelsEntryOfTwoWordsIsRefusedUnderItsOwnKey(t *testing.T) {
	for name, tc := range map[string]struct {
		local string
		want  string
	}{
		"role": {"[[role]]\nname = \"claude-review\"\nfallback_models = [\"opus 4\"]\n",
			`role claude-review: fallback_models: "opus 4" must be one word`},
		"role twice": {"[[role]]\nname = \"claude-review\"\nfallback_models = [\"opus\", \"Opus\"]\n",
			`role claude-review: fallback_models lists "Opus" twice`},
		"shell role": {"[[role]]\nname = \"codex-review\"\nfallback_models = [\"o3\"]\n",
			`role codex-review: fallback_models needs a session role`},
		"notes": {"[notes]\nfallback_models = [\"opus 4\"]\n", `notes.fallback_models: "opus 4" must be one word`},
		"learn": {"[learn]\nfallback_models = [\"so\\tnnet\"]\n", `learn.fallback_models: "so\tnnet" must be one word`},
		"editor": {"[learn]\neditor_fallback_models = [\"opus\", \"\"]\n",
			`learn.editor_fallback_models: "" must be one word`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadCommittedWithLocal(t, testLocalConfig+tc.local)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("load = %v, want an error containing %q", err, tc.want)
			}
			// The section's own wording only: a session role's list is not
			// reported a second time as a [[role]] key.
			if name != "role" && name != "role twice" && name != "shell role" && strings.Count(err.Error(), "must be one word") != 1 {
				t.Fatalf("load = %v, want the problem reported once", err)
			}
		})
	}
}
