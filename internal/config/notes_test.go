package config

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

// The notes' curation triggers and the curator's defaults are the committed
// config.defaults.toml's.
func TestNotesDefaults(t *testing.T) {
	want := Notes{
		MaxBytes: 16384, MaxLine: 300, MaxHarnessFiles: 15, MaxHarnessBytes: 131072,
		Curate: CurateOverLimit, Kind: "claude", Model: "sonnet", Prompt: "notes-curate.md", Timeout: Duration{30 * time.Minute},
	}
	if got := Defaults().Notes; !reflect.DeepEqual(got, want) {
		t.Fatalf("built-in notes = %+v, want %+v", got, want)
	}
	root := repoRoot(t)
	explicit, err := LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.defaults.toml"), LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(explicit.Notes, want) {
		t.Fatalf("config.defaults.toml notes = %+v, want %+v", explicit.Notes, want)
	}
	if r := explicit.NotesRole(); r.Name != NotesRoleName || r.Kind != "claude" || r.Model != "sonnet" || r.Timeout.Duration != 30*time.Minute {
		t.Errorf("NotesRole = %+v", r)
	}
}

// A user's [notes] overrides the keys it sets; a kind without a model gets
// that kind's default model, as [learn] does.
func TestNotesOverlayKeepsTheKeysItDoesNotSet(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+`
[notes]
max_bytes = 24000
curate = "weekly"
kind = "codex"
`)
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultNotes()
	want.MaxBytes, want.Curate, want.Kind, want.Model = 24000, CurateWeekly, "codex", ""
	if !reflect.DeepEqual(cfg.Notes, want) {
		t.Fatalf("notes = %+v, want %+v", cfg.Notes, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
}

// Every [notes] key is checked, whatever curate says (`magnum notes --curate`
// runs regardless).
func TestValidateNotes(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Notes.MaxBytes, cfg.Notes.MaxLine, cfg.Notes.MaxHarnessFiles, cfg.Notes.MaxHarnessBytes = 0, -1, 0, 0
	cfg.Notes.Curate, cfg.Notes.Kind, cfg.Notes.Prompt, cfg.Notes.Timeout = "daily", "nope", "", Duration{}
	wantError(t, cfg.Validate(), "notes.max_bytes", "notes.max_line", "notes.max_harness_files", "notes.max_harness_bytes",
		"notes.curate must be over_limit, weekly or off", `notes.kind "nope"`, "notes.prompt must name", "notes.timeout")

	cfg = validPipelineConfig()
	cfg.Notes.Kind, cfg.Notes.Model = "droid", "sonnet"
	wantError(t, cfg.Validate(), "notes:", "kind droid has no model args")

	cfg = validPipelineConfig()
	cfg.Notes.Curate = CurateOff
	if err := cfg.Validate(); err != nil {
		t.Fatalf("curate off: %v", err)
	}
}
