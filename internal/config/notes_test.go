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
		Curate: CurateTriggers{CurateOverLimit, CurateMisses}, Kind: "claude", Model: "sonnet", Prompt: "notes-curate.md", Timeout: Duration{30 * time.Minute},
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
	// The earlier string form keeps its meaning: weekly curates a marked
	// repository too, and does not take the misses trigger.
	want.MaxBytes, want.Curate, want.Kind, want.Model = 24000, CurateTriggers{CurateOverLimit, CurateWeekly}, "codex", ""
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
	cfg.Notes.Curate, cfg.Notes.Kind, cfg.Notes.Prompt, cfg.Notes.Timeout = CurateTriggers{"daily"}, "nope", "", Duration{}
	wantError(t, cfg.Validate(), "notes.max_bytes", "notes.max_line", "notes.max_harness_files", "notes.max_harness_bytes",
		`notes.curate: "daily" is not a trigger (over_limit, weekly, misses; [] or "off" for none)`, `notes.kind "nope"`, "notes.prompt must name", "notes.timeout")

	cfg = validPipelineConfig()
	cfg.Notes.Curate = CurateTriggers{CurateMisses, CurateOff}
	wantError(t, cfg.Validate(), `notes.curate: "off" goes alone`)
	cfg.Notes.Curate = CurateTriggers{CurateMisses, CurateMisses}
	wantError(t, cfg.Validate(), `notes.curate: "misses" is listed twice`)

	cfg = validPipelineConfig()
	cfg.Notes.Kind, cfg.Notes.Model = "droid", "sonnet"
	wantError(t, cfg.Validate(), "notes:", "kind droid has no model args")

	cfg = validPipelineConfig()
	cfg.Notes.Curate = CurateTriggers{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("curate off: %v", err)
	}
}

// [notes] curate is a list of triggers (over_limit, weekly, misses; [] for
// none, only `magnum notes --curate`); the earlier string form still
// reads as it meant: "over_limit" that trigger alone, "weekly" over_limit
// and weekly, "off" none.
func TestNotesCurateReadsAListOrTheEarlierString(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  CurateTriggers
	}{
		{`["over_limit", "misses"]`, CurateTriggers{CurateOverLimit, CurateMisses}},
		{`["misses"]`, CurateTriggers{CurateMisses}},
		{`[]`, CurateTriggers{}},
		{`["off"]`, CurateTriggers{}},
		{`"off"`, CurateTriggers{}},
		{`"over_limit"`, CurateTriggers{CurateOverLimit}},
		{`"weekly"`, CurateTriggers{CurateOverLimit, CurateWeekly}},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := loadCommittedWithLocal(t, testLocalConfig+"\n[notes]\ncurate = "+tc.value+"\n")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Notes.Curate, tc.want) {
				t.Fatalf("curate = %#v, want %#v", cfg.Notes.Curate, tc.want)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("valid: %v", err)
			}
		})
	}
	cfg := Defaults()
	if !cfg.Notes.Curate.Has(CurateMisses) || !cfg.Notes.Curate.Has(CurateOverLimit) || cfg.Notes.Curate.Has(CurateWeekly) {
		t.Errorf("default triggers = %v", cfg.Notes.Curate)
	}
	if _, err := loadCommittedWithLocal(t, testLocalConfig+"\n[notes]\ncurate = 3\n"); err == nil {
		t.Error("a number for curate loaded")
	}
}
