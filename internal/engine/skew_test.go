package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// TestCheckPromptsRendersEveryConfiguredPrompt: the shipped prompts render
// with this binary's data.
func TestCheckPromptsRendersEveryConfiguredPrompt(t *testing.T) {
	h := newHarness(t)
	n, err := CheckPrompts(h.cfg)
	if err != nil {
		t.Fatalf("shipped prompts: %v", err)
	}
	if n < 10 {
		t.Fatalf("only %d renders: not every role prompt was checked", n)
	}
}

// TestShippedConfigAndPromptFilesRender: the committed config.toml with the
// checkout's prompts/ (what the daemon reads at prompt time) renders with
// this build: a prompt edit that needs a new template field fails here.
func TestShippedConfigAndPromptFilesRender(t *testing.T) {
	_, f, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(f), "..", ".."))
	cfg, err := config.LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.toml"), config.LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipeline.PromptsDir != filepath.Join(root, "prompts") {
		t.Fatalf("prompts_dir = %s", cfg.Pipeline.PromptsDir)
	}
	if _, err := CheckPrompts(cfg); err != nil {
		t.Fatal(err)
	}
}

// TestNextPromptsRender: a prompt edit staged as prompts/<name>.next (it
// needs a template field the running daemon's build lacks, so it replaces
// <name> only with the restart onto this build) renders with this build,
// through the same check the daemon runs before it starts.
func TestNextPromptsRender(t *testing.T) {
	_, f, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(f), "..", ".."))
	src := filepath.Join(root, "prompts")
	next, err := filepath.Glob(filepath.Join(src, "*.next"))
	if err != nil {
		t.Fatal(err)
	}
	if len(next) == 0 {
		t.Skip("no staged prompt edits")
	}
	dir := t.TempDir()
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".next") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range next {
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, strings.TrimSuffix(filepath.Base(n), ".next")), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.toml"), config.LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pipeline.PromptsDir = dir
	if _, err := CheckPrompts(cfg); err != nil {
		t.Fatalf("staged prompts: %v", err)
	}
}

// TestCheckPromptsCatchesFieldsThisBuildLacks: a prompt that names a field
// the binary's template data does not have fails, also when the field sits
// in a branch only full data reaches (an {{if}}) or only empty data reaches
// (an {{else}}), and the error names the role and the file.
func TestCheckPromptsCatchesFieldsThisBuildLacks(t *testing.T) {
	for _, tc := range []struct{ file, text string }{
		{"judge-initial.md", "Review {{.URL}} with {{.NewTemplateField}}."},
		{"judge-rereview.md", "{{if .ForcePushed}}force pushed: {{.NewTemplateField}}{{end}}"},
		{"claude-review.md", "{{if .NotesPath}}notes{{else}}{{.NewTemplateField}}{{end}}"},
		{"model-fallback.md", "{{range .Missing}}{{end}}"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			h := newHarness(t)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.text), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := *h.cfg
			cfg.Pipeline.PromptsDir = dir
			_, err := CheckPrompts(&cfg)
			if err == nil {
				t.Fatal("a prompt this build cannot render passed")
			}
			if !strings.Contains(err.Error(), filepath.Join(dir, tc.file)) {
				t.Fatalf("error does not name the file: %v", err)
			}
		})
	}
}

// TestCheckPromptsCatchesABrokenShellTemplate: a shell role's full-line
// template that does not print the done marker is refused.
func TestCheckPromptsCatchesABrokenShellTemplate(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex-review.sh"), []byte("codex review --base {{.BaseRef}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := *h.cfg
	cfg.Pipeline.PromptsDir = dir
	cfg.Roles = append([]config.Role(nil), h.cfg.Roles...)
	for i, r := range cfg.Roles {
		if r.IsShell() {
			cfg.Roles[i].Command, cfg.Roles[i].Prompt = "", "codex-review.sh"
		}
	}
	if _, err := CheckPrompts(&cfg); err == nil || !strings.Contains(err.Error(), "done marker") {
		t.Fatalf("CheckPrompts = %v", err)
	}
}

// TestResumeToolClearsModelLimits: `magnum resume --tool claude` forgets
// the kind's per-model limits, so its sessions go back to their models.
func TestResumeToolClearsModelLimits(t *testing.T) {
	h := newHarness(t)
	until := store.FormatTime(h.clock.Now().Add(5 * time.Hour))
	for k, v := range map[string]string{
		agents.KVModelLimited("claude", "fable"): until,
		agents.KVModelLimited("claude", "opus"):  until,
		agents.KVModelLimits("claude"):           "fable,opus",
		agents.KVModelLimited("codex", "x"):      until,
		agents.KVModelLimits("codex"):            "x",
	} {
		h.e.setKV(h.ctx, k, v)
	}
	msg, err := h.e.requestPause(h.ctx, PausePayload{Tool: "claude"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if msg != "resumed claude (model limits cleared: claude/fable, claude/opus)" {
		t.Fatalf("answer = %q", msg)
	}
	for _, k := range []string{agents.KVModelLimited("claude", "fable"), agents.KVModelLimited("claude", "opus"), agents.KVModelLimits("claude")} {
		if _, ok := h.e.getKV(h.ctx, k); ok {
			t.Errorf("%s kept", k)
		}
	}
	if _, ok := h.e.getKV(h.ctx, agents.KVModelLimited("codex", "x")); !ok {
		t.Error("another kind's limit was cleared")
	}
	if got := agents.ModelLimits(h.ctx, h.st, h.cfg, "claude", h.clock.Now()); len(got) != 0 {
		t.Errorf("ModelLimits after resume = %+v", got)
	}
}

// TestDrainStopsNewRoundsAndRestartEndsIt: while daemon.draining is set no
// round starts and the tab bar says so; the next daemon start lifts it.
func TestDrainStopsNewRoundsAndRestartEndsIt(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.e.setKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now()))
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRQueued)
	if bar := h.e.tabBar(h.ctx); !strings.Contains(bar, "draining") {
		t.Fatalf("tab bar = %q", bar)
	}
	if why := h.e.pauseReason(h.ctx); !strings.Contains(why, "draining") {
		t.Fatalf("pauseReason = %q", why)
	}

	h.e = New(h.d) // the restarted daemon
	h.startup()
	if _, ok := h.e.getKV(h.ctx, KVDaemonDraining); ok {
		t.Fatal("drain kept across the restart")
	}
	h.tick()
	h.wantState(2, store.PRReviewed)
}
