package engine

// Prompts are loaded once, at startup: the daemon renders rounds from the
// snapshot it took right after the start check rendered the files with this
// build, so an edit on disk takes effect at the next restart, which checks it
// again. Every reconcile compares the snapshot with the files and records
// what changed for `magnum status`.

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// KVPromptsLoadedAt is when the running daemon loaded its prompts
	// (store.FormatTime). KVPromptsChanged counts the prompt and skill files
	// that differ on disk since (absent when none) and KVPromptsChangedFiles
	// names them, comma-separated. See PromptsLine.
	KVPromptsLoadedAt     = "daemon.prompts_loaded_at"
	KVPromptsChanged      = "daemon.prompts_changed"
	KVPromptsChangedFiles = "daemon.prompts_changed_files"
)

// SkillCopyDir is where the daemon keeps the copies of the judges' skills it
// took at startup (<state>/skill/<hash>/SKILL.md).
func SkillCopyDir(state string) string { return filepath.Join(state, "skill") }

// PromptsLine is what `magnum status` shows after "prompts: ": when the
// daemon loaded them, and how many files changed on disk since (they take
// effect at the next restart). "" when loadedAt is zero (no daemon has
// recorded it).
func PromptsLine(loadedAt time.Time, changed int) string {
	if loadedAt.IsZero() {
		return ""
	}
	s := "loaded " + loadedAt.Local().Format("2006-01-02 15:04")
	switch {
	case changed == 1:
		s += ", 1 file changed on disk since"
	case changed > 1:
		s += fmt.Sprintf(", %d files changed on disk since", changed)
	}
	return s
}

// loadPrompts takes the prompt snapshot rounds render from
// (config.Config.SnapshotPrompts: every prompt the roles name,
// model-fallback.md, the triage, retro and notes curation prompts and a copy of each judge's skill) and renders it once
// more with this build (CheckPrompts): the start check read the files a
// moment earlier, and an edit in between must not reach a round unchecked.
// A dry run copies no skill (it writes nothing outside its store).
func (e *Engine) loadPrompts(ctx context.Context) error {
	skillDir := ""
	if !e.d.DryRun && e.d.Layout.Valid() {
		skillDir = SkillCopyDir(e.d.Layout.State())
	}
	snap, err := e.cfg.SnapshotPrompts(skillDir, e.now(), agents.FallbackPromptName, e.cfg.Triage.Prompt, e.cfg.Learn.Prompt, e.cfg.Notes.Prompt)
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	if _, err := CheckPrompts(e.cfg); err != nil {
		return fmt.Errorf("engine: the prompts changed after the start check and do not render with this build "+
			"(fix them, check with `magnum config`, then `magnum daemon-restart`):\n%w", err)
	}
	for _, w := range snap.Warnings {
		e.log.Warn("judge skill not copied; the judge reads the file as it is on disk", "warning", w)
	}
	e.setKV(ctx, KVPromptsLoadedAt, store.FormatTime(snap.LoadedAt))
	e.delKV(ctx, KVPromptsChanged, KVPromptsChangedFiles)
	e.log.Info("prompts loaded", "files", snap.Files())
	return nil
}

// notePromptChanges compares the prompt snapshot with the files on disk
// (config.PromptSnapshot.Changed) and records the result for `magnum
// status`; a new set of changed files is also a daemon.prompts_changed
// event.
func (e *Engine) notePromptChanges(ctx context.Context) {
	snap := e.cfg.PromptSnapshot()
	if snap == nil {
		return
	}
	changed := snap.Changed()
	names := strings.Join(changed, ",")
	if prev, _ := e.getKV(ctx, KVPromptsChangedFiles); prev == names {
		return
	}
	if len(changed) == 0 {
		e.delKV(ctx, KVPromptsChanged, KVPromptsChangedFiles)
		return
	}
	e.setKV(ctx, KVPromptsChanged, strconv.Itoa(len(changed)))
	e.setKV(ctx, KVPromptsChangedFiles, names)
	e.event(ctx, "info", "", "daemon.prompts_changed",
		fmt.Sprintf("%d prompt file(s) changed on disk since the daemon loaded them at %s (%s); they take effect at the next `magnum daemon-restart`",
			len(changed), snap.LoadedAt.Local().Format("15:04:05"), strings.Join(changed, ", ")),
		map[string]any{"files": changed})
}
