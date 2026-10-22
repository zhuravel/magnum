package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/prompts"
)

// The partials file goes with every judge prompt (judge-*.md, the learn
// editor's glob) and with nothing else; it does not go with itself.
func TestUsesSharedIsEveryJudgePromptButThePartialsFile(t *testing.T) {
	for name, want := range map[string]bool{
		"judge-initial.md": true, "judge-rereview.md": true, "judge-recovery.md": true, "judge-continue.md": true,
		"judge-own-pass.md": true, "judge-nudge.md": true, "judge-custom.md": true,
		SharedPrompt: false, "claude-review.md": false, "codex-review.sh": false, "judge-initial.txt": false, "fix.md": false,
	} {
		if got := UsesShared(name); got != want {
			t.Errorf("UsesShared(%q) = %v, want %v", name, got, want)
		}
	}
}

// Read from disk (the CLI), a judge prompt comes with prompts_dir's partials
// file while it is there and with the embedded one once it is gone; a
// reviewer's prompt comes with none.
func TestAJudgePromptComesWithThePartialsFileOfPromptsDirElseTheEmbeddedOne(t *testing.T) {
	const mine = `{{define "skill"}}my skill line{{end}}`
	cfg, home := snapLoad(t, map[string]string{"prompts/" + SharedPrompt: mine})
	embedded, ok := prompts.Read(SharedPrompt)
	if !ok || embedded == mine {
		t.Fatal("no embedded partials file")
	}
	for _, name := range []string{"judge-initial.md", "judge-rereview.md", "judge-recovery.md", "judge-continue.md", "judge-own-pass.md", "judge-nudge.md"} {
		if p, err := cfg.ResolvePrompt(name); err != nil || p.Shared != mine {
			t.Errorf("%s: Shared = %q, %v, want prompts_dir's partials file", name, p.Shared, err)
		}
	}
	if p, err := cfg.ResolvePrompt("claude-review.md"); err != nil || p.Shared != "" {
		t.Errorf("claude-review.md: Shared = %q, %v, want none", p.Shared, err)
	}
	if err := os.Remove(filepath.Join(home, "prompts", SharedPrompt)); err != nil {
		t.Fatal(err)
	}
	if p, err := cfg.ResolvePrompt("judge-initial.md"); err != nil || p.Shared != embedded {
		t.Errorf("without prompts_dir's partials file: Shared = %.60q, %v, want the embedded one", p.Shared, err)
	}
}

// The daemon loads the partials file with the judge prompts at startup and
// renders from that copy: an edit on disk reaches no round before the next
// restart, and the snapshot reports the file as changed, as it does a
// prompt; a partials file that appears in prompts_dir over the embedded one
// is a change too.
func TestTheSnapshotHoldsThePartialsFileAndSeesItChange(t *testing.T) {
	const v1 = `{{define "skill"}}v1{{end}}`
	cfg, home := snapLoad(t, map[string]string{"prompts/" + SharedPrompt: v1})
	file := filepath.Join(home, "prompts", SharedPrompt)
	snap, err := cfg.SnapshotPrompts("", snapNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.prompts[SharedPrompt]; !ok {
		t.Fatalf("the snapshot holds %v, not the partials file", snap.prompts)
	}
	if got := snap.Changed(); len(got) != 0 {
		t.Fatalf("Changed() = %q, want none", got)
	}
	snapWrite(t, file, `{{define "skill"}}v2, longer{{end}}`)
	if p, err := cfg.ResolvePrompt("judge-initial.md"); err != nil || p.Shared != v1 {
		t.Errorf("after an edit: Shared = %q, %v, want the snapshot's %q", p.Shared, err, v1)
	}
	if got := snap.Changed(); !slices.Equal(got, []string{SharedPrompt}) {
		t.Errorf("Changed() = %q, want [%s]", got, SharedPrompt)
	}

	cfg, home = snapLoad(t, nil)
	snap, err = cfg.SnapshotPrompts("", snapNow)
	if err != nil {
		t.Fatal(err)
	}
	snapWrite(t, filepath.Join(home, "prompts", SharedPrompt), v1)
	if got := snap.Changed(); !slices.Equal(got, []string{SharedPrompt}) {
		t.Errorf("a partials file over the embedded one: Changed() = %q, want [%s]", got, SharedPrompt)
	}
	if p, err := cfg.ResolvePrompt("judge-initial.md"); err != nil || p.Shared == v1 {
		t.Errorf("a partials file that appeared after startup reached a round: %q, %v", p.Shared, err)
	}
}

// The partials file holds blocks for the judge prompts and is never a prompt
// of its own: ResolvePrompt refuses its name, so does a role's prompt in
// the config, and so does the snapshot.
func TestThePartialsFileIsNoPromptOfItsOwn(t *testing.T) {
	cfg := validPipelineConfig()
	if _, err := cfg.ResolvePrompt(SharedPrompt); !errors.Is(err, ErrSharedPrompt) {
		t.Errorf("ResolvePrompt(%s) = %v, want ErrSharedPrompt", SharedPrompt, err)
	}
	for i := range cfg.Roles {
		if cfg.Roles[i].Judge {
			cfg.Roles[i].Nudge = SharedPrompt
			break
		}
	}
	wantError(t, cfg.Validate(), "nudge prompt", SharedPrompt)

	snapCfg, _ := snapLoad(t, nil)
	if _, err := snapCfg.SnapshotPrompts("", snapNow, SharedPrompt); !errors.Is(err, ErrSharedPrompt) || snapCfg.PromptSnapshot() != nil {
		t.Errorf("SnapshotPrompts naming the partials file = %v (installed %v), want ErrSharedPrompt and nothing installed", err, snapCfg.PromptSnapshot() != nil)
	}
}

// A judge prompt is parsed with its partials file when the config is
// checked, as when it is rendered: a partials file that does not parse fails
// the check of every judge prompt and names itself.
func TestTheConfigCheckParsesTheJudgePromptsWithThePartialsFile(t *testing.T) {
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": snapTOML, "prompts/" + SharedPrompt: `{{define "skill"}}{{.URL{{end}}`})
	wantError(t, err, "judge-initial.md", SharedPrompt)
	if strings.Contains(err.Error(), "claude-review.md") {
		t.Errorf("a reviewer's prompt failed on the judge prompts' partials file: %v", err)
	}
}
