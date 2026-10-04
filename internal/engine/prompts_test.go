package engine

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/prompts"
)

func TestPromptsLine(t *testing.T) {
	loaded := time.Date(2026, 10, 5, 9, 30, 0, 0, time.Local)
	tests := []struct {
		name     string
		loadedAt time.Time
		changed  int
		want     string
	}{
		{"no daemon recorded a load", time.Time{}, 0, ""},
		{"no daemon recorded a load, files changed", time.Time{}, 3, ""},
		{"nothing changed", loaded, 0, "loaded 2026-10-05 09:30"},
		{"one file changed", loaded, 1, "loaded 2026-10-05 09:30, 1 file changed on disk since"},
		{"two files changed", loaded, 2, "loaded 2026-10-05 09:30, 2 files changed on disk since"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PromptsLine(tc.loadedAt, tc.changed); got != tc.want {
				t.Fatalf("PromptsLine(%v, %d) = %q, want %q", tc.loadedAt, tc.changed, got, tc.want)
			}
		})
	}
}

// promptsFile writes text to path (creating its directory).
func promptsFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func promptsRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// judgeSkill is the path the harness's judges name as their skill (the
// default, under the test home), checked to be one file.
func judgeSkill(t *testing.T, h *harness) string {
	t.Helper()
	src := ""
	for _, r := range h.cfg.Roles {
		if !r.Judge {
			continue
		}
		if p := config.SkillPath(r.Skill, h.layout); src == "" {
			src = p
		} else if p != src {
			t.Fatalf("judges name different skills: %s and %s", src, p)
		}
	}
	if src != h.layout.Skill() {
		t.Fatalf("judge skill = %q, want the default %q", src, h.layout.Skill())
	}
	return src
}

// judgePrompt is a prompt file name the judge uses (its nudge prompt) and
// the embedded default text of it.
func judgePrompt(t *testing.T, h *harness) (name, text string) {
	t.Helper()
	for _, r := range h.cfg.Roles {
		if r.Judge {
			name = r.PromptFile(config.PromptNudge)
			break
		}
	}
	text, ok := prompts.Read(name)
	if name == "" || !ok {
		t.Fatalf("judge nudge prompt %q has no embedded default", name)
	}
	return name, text
}

func TestLoadPromptsSnapshotsPromptsAndCopiesTheJudgeSkill(t *testing.T) {
	h := newHarness(t)
	skill := judgeSkill(t, h)
	promptsFile(t, skill, "# skill v1\n")
	// What an earlier run left behind: this load starts the count again.
	h.e.setKV(h.ctx, KVPromptsChanged, "3")
	h.e.setKV(h.ctx, KVPromptsChangedFiles, "judge-nudge.md,"+skill)
	if h.cfg.PromptSnapshot() != nil {
		t.Fatal("a snapshot before the load")
	}

	if err := h.e.loadPrompts(h.ctx); err != nil {
		t.Fatal(err)
	}
	snap := h.cfg.PromptSnapshot()
	if snap == nil {
		t.Fatal("loadPrompts installed no snapshot")
	}
	if !snap.LoadedAt.Equal(h.clock.Now()) || len(snap.Warnings) != 0 || snap.Files() == 0 {
		t.Fatalf("snapshot: loaded %v (now %v), warnings %q, %d files", snap.LoadedAt, h.clock.Now(), snap.Warnings, snap.Files())
	}
	v, ok := kvValue(h, KVPromptsLoadedAt)
	if at, err := store.ParseTime(v); !ok || err != nil || !at.Equal(h.clock.Now()) {
		t.Fatalf("%s = %q (set %v, err %v), want %v", KVPromptsLoadedAt, v, ok, err, h.clock.Now())
	}
	for _, key := range []string{KVPromptsChanged, KVPromptsChangedFiles} {
		if v, ok := kvValue(h, key); ok {
			t.Fatalf("%s = %q after a load, want it cleared", key, v)
		}
	}

	// The judge reads the copy taken at the load, not the file.
	first := h.cfg.SkillFile(skill)
	if first == skill || filepath.Base(first) != config.SkillCopyName ||
		!strings.HasPrefix(first, SkillCopyDir(h.layout.State())+string(filepath.Separator)) {
		t.Fatalf("SkillFile(%s) = %q, want a copy under %s", skill, first, SkillCopyDir(h.layout.State()))
	}
	if got := promptsRead(t, first); got != "# skill v1\n" {
		t.Fatalf("skill copy = %q", got)
	}
	promptsFile(t, skill, "# skill v2, edited later\n")
	if got := h.cfg.SkillFile(skill); got != first || promptsRead(t, got) != "# skill v1\n" {
		t.Fatalf("after an edit SkillFile = %q (%q), want the first copy as it was", got, promptsRead(t, got))
	}

	// The next load (a restart) takes the edited skill.
	h.advance(time.Hour)
	if err := h.e.loadPrompts(h.ctx); err != nil {
		t.Fatal(err)
	}
	second := h.cfg.SkillFile(skill)
	if second == first || promptsRead(t, second) != "# skill v2, edited later\n" {
		t.Fatalf("after the second load SkillFile = %q (%q), want a copy of v2 next to %s", second, promptsRead(t, second), first)
	}
	v, _ = kvValue(h, KVPromptsLoadedAt)
	if at, err := store.ParseTime(v); err != nil || !at.Equal(h.clock.Now()) {
		t.Fatalf("%s = %q, want the second load time %v", KVPromptsLoadedAt, v, h.clock.Now())
	}
}

func TestLoadPromptsServesAPromptFromTheSnapshotNotTheDisk(t *testing.T) {
	h := newHarness(t)
	name, text := judgePrompt(t, h)
	path := filepath.Join(h.cfg.Pipeline.PromptsDir, name)
	promptsFile(t, path, text+"\nLoaded wording.\n")
	if err := h.e.loadPrompts(h.ctx); err != nil {
		t.Fatal(err)
	}
	promptsFile(t, path, text+"\nEdited wording.\n")
	p, err := h.cfg.ResolvePrompt(name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "Loaded wording.") || strings.Contains(p.Text, "Edited wording.") {
		t.Fatalf("%s resolves to the edited file, want the text loaded at startup", name)
	}
}

func TestLoadPromptsWithoutASkillFileWarnsAndKeepsThePath(t *testing.T) {
	h := newHarness(t) // the test home has no skills/magnum-review/SKILL.md
	skill := judgeSkill(t, h)
	if err := h.e.loadPrompts(h.ctx); err != nil {
		t.Fatalf("a missing skill file must not stop the daemon: %v", err)
	}
	snap := h.cfg.PromptSnapshot()
	if snap == nil || len(snap.Warnings) == 0 {
		t.Fatalf("snapshot %+v, want a warning about the missing skill", snap)
	}
	if got := h.cfg.SkillFile(skill); got != skill {
		t.Fatalf("SkillFile = %q, want the configured path %q", got, skill)
	}
	if _, ok := kvValue(h, KVPromptsLoadedAt); !ok {
		t.Fatalf("%s not set", KVPromptsLoadedAt)
	}
}

func TestLoadPromptsInADryRunCopiesNoSkill(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.d.DryRun = true })
	skill := judgeSkill(t, h)
	promptsFile(t, skill, "# skill\n")
	if err := h.e.loadPrompts(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.cfg.PromptSnapshot() == nil {
		t.Fatal("a dry run still takes the prompt snapshot")
	}
	if got := h.cfg.SkillFile(skill); got != skill {
		t.Fatalf("SkillFile = %q, want the configured path %q in a dry run", got, skill)
	}
	if _, err := os.Stat(SkillCopyDir(h.layout.State())); !os.IsNotExist(err) {
		t.Fatalf("a dry run wrote %s (err %v)", SkillCopyDir(h.layout.State()), err)
	}
}

func TestLoadPromptsRefusesAPromptThatDoesNotRender(t *testing.T) {
	h := newHarness(t)
	name, _ := judgePrompt(t, h)
	promptsFile(t, filepath.Join(h.cfg.Pipeline.PromptsDir, name), "{{ .NoSuchField }}\n")
	err := h.e.loadPrompts(h.ctx)
	if err == nil || !strings.Contains(err.Error(), name) {
		t.Fatalf("loadPrompts = %v, want an error naming %s", err, name)
	}
	if _, ok := kvValue(h, KVPromptsLoadedAt); ok {
		t.Fatalf("%s set although the load failed", KVPromptsLoadedAt)
	}
}

// promptChangeEvents lists the daemon.prompts_changed events since start,
// oldest first (they have no subject, so no subject query finds them).
func promptChangeEvents(t *testing.T, h *harness, start time.Time) []store.Event {
	t.Helper()
	rows, err := h.st.DB().QueryContext(h.ctx,
		`SELECT level, subject, message, data_json FROM events WHERE kind = ? AND at >= ? ORDER BY id`,
		"daemon.prompts_changed", store.FormatTime(start))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var evs []store.Event
	for rows.Next() {
		var ev store.Event
		var data *string
		if err := rows.Scan(&ev.Level, &ev.Subject, &ev.Message, &data); err != nil {
			t.Fatal(err)
		}
		if data != nil {
			ev.Data = json.RawMessage(*data)
		}
		evs = append(evs, ev)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestNotePromptChangesRecordsWhatDiffersAndClearsItOnRevert(t *testing.T) {
	h := newHarness(t)
	start := h.clock.Now()
	skill := judgeSkill(t, h)
	name, text := judgePrompt(t, h)
	promptPath := filepath.Join(h.cfg.Pipeline.PromptsDir, name)
	promptsFile(t, skill, "# skill v1\n")
	promptsFile(t, promptPath, text+"\nLoaded wording.\n") // a disk prompt, not the embedded default
	if err := h.e.loadPrompts(h.ctx); err != nil {
		t.Fatal(err)
	}
	wantKV := func(count string, files ...string) {
		t.Helper()
		slices.Sort(files)
		gotCount, okCount := kvValue(h, KVPromptsChanged)
		gotFiles, okFiles := kvValue(h, KVPromptsChangedFiles)
		if count == "" {
			if okCount || okFiles {
				t.Fatalf("%s = %q, %s = %q, want both unset", KVPromptsChanged, gotCount, KVPromptsChangedFiles, gotFiles)
			}
			return
		}
		if gotCount != count || gotFiles != strings.Join(files, ",") {
			t.Fatalf("%s = %q, %s = %q, want %q and %q", KVPromptsChanged, gotCount, KVPromptsChangedFiles, gotFiles, count, strings.Join(files, ","))
		}
	}

	// Nothing differs: nothing recorded.
	h.e.notePromptChanges(h.ctx)
	wantKV("")
	if evs := promptChangeEvents(t, h, start); len(evs) != 0 {
		t.Fatalf("daemon.prompts_changed events with nothing changed: %+v", evs)
	}

	// A prompt edited on disk.
	promptsFile(t, promptPath, text+"\nEdited wording, longer than before.\n")
	h.e.notePromptChanges(h.ctx)
	wantKV("1", name)
	evs := promptChangeEvents(t, h, start)
	if len(evs) != 1 || evs[0].Subject != nil || evs[0].Level != "info" || !strings.Contains(evs[0].Message, name) {
		t.Fatalf("daemon.prompts_changed events: %+v", evs)
	}
	var data struct{ Files []string }
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || !slices.Equal(data.Files, []string{name}) {
		t.Fatalf("event data %s: files %v, err %v", evs[0].Data, data.Files, err)
	}

	// The same set again is not another event.
	h.e.notePromptChanges(h.ctx)
	h.e.notePromptChanges(h.ctx)
	wantKV("1", name)
	if evs := promptChangeEvents(t, h, start); len(evs) != 1 {
		t.Fatalf("daemon.prompts_changed events after repeating the same set: %d, want 1", len(evs))
	}

	// A new set (the judge skill joins) is.
	promptsFile(t, skill, "# skill v2, edited later\n")
	h.e.notePromptChanges(h.ctx)
	wantKV("2", name, skill)
	evs = promptChangeEvents(t, h, start)
	if len(evs) != 2 || !strings.Contains(evs[1].Message, skill) {
		t.Fatalf("daemon.prompts_changed events after the skill changed: %+v", evs)
	}

	// Putting both files back as they were clears the record, with no event.
	promptsFile(t, promptPath, text+"\nLoaded wording.\n")
	promptsFile(t, skill, "# skill v1\n")
	h.e.notePromptChanges(h.ctx)
	wantKV("")
	if evs := promptChangeEvents(t, h, start); len(evs) != 2 {
		t.Fatalf("daemon.prompts_changed events after the revert: %d, want still 2", len(evs))
	}
}

func TestNotePromptChangesDoesNothingWithoutASnapshot(t *testing.T) {
	h := newHarness(t)
	start := h.clock.Now()
	h.e.notePromptChanges(h.ctx) // no daemon loaded prompts (loadPrompts did not run)
	for _, key := range []string{KVPromptsChanged, KVPromptsChangedFiles} {
		if v, ok := kvValue(h, key); ok {
			t.Fatalf("%s = %q without a snapshot", key, v)
		}
	}
	if evs := promptChangeEvents(t, h, start); len(evs) != 0 {
		t.Fatalf("daemon.prompts_changed events without a snapshot: %+v", evs)
	}
}

func TestReconcileNotesPromptChanges(t *testing.T) {
	h := newHarness(t)
	name, text := judgePrompt(t, h)
	path := filepath.Join(h.cfg.Pipeline.PromptsDir, name)
	promptsFile(t, path, text+"\nLoaded wording.\n")
	if err := h.e.loadPrompts(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.startup()

	reconcile := func() {
		t.Helper()
		h.e.maybeReconcile(h.ctx)
		h.settle()
	}
	wantChanged := func(want string) {
		t.Helper()
		v, ok := kvValue(h, KVPromptsChanged)
		if (want == "") == ok || v != want {
			t.Fatalf("%s = %q (set %v), want %q", KVPromptsChanged, v, ok, want)
		}
	}

	// The start-up reconcile has just run: an edit waits for the next one.
	promptsFile(t, path, text+"\nEdited wording, longer than before.\n")
	reconcile()
	wantChanged("")
	h.advance(h.cfg.Daemon.ReconcileInterval.Duration)
	reconcile()
	wantChanged("1")

	// Between reconciles the record is not looked at again.
	promptsFile(t, path, text+"\nLoaded wording.\n")
	reconcile()
	wantChanged("1")
	h.advance(h.cfg.Daemon.ReconcileInterval.Duration)
	reconcile()
	wantChanged("")
}

// A round of a pool repository's PR carries the repository's readiness step
// (prepare, ready and their timeout from the [[pool]], each replaced by what
// the [[repo]] block sets) and the pool's env for the slot.
func TestRoundCarriesTheReadinessPlanAndThePoolEnv(t *testing.T) {
	tests := []struct {
		name        string
		mod         func(c *config.Config)
		wantPrepare []string
		wantReady   []string
		wantTimeout time.Duration
	}{
		{"nothing configured", func(*config.Config) {}, nil, nil, config.DefaultReadyTimeout},
		{"the pool's commands and timeout", func(c *config.Config) {
			p := &c.Pools[0]
			p.Prepare, p.Ready = []string{"bin/prepare --quick", "bin/seed"}, []string{"bin/ready"}
			p.ReadyTimeout = config.Duration{Duration: 90 * time.Second}
		}, []string{"bin/prepare --quick", "bin/seed"}, []string{"bin/ready"}, 90 * time.Second},
		{"a [[repo]] block replaces what it sets", func(c *config.Config) {
			p := &c.Pools[0]
			p.Prepare, p.Ready = []string{"bin/prepare"}, []string{"bin/ready"}
			p.ReadyTimeout = config.Duration{Duration: 90 * time.Second}
			c.Repos = append(c.Repos, config.Repo{Repo: "talkable/talkable", Ready: []string{"bin/ready-repo"},
				ReadyTimeout: config.Duration{Duration: 2 * time.Minute}})
		}, []string{"bin/prepare"}, []string{"bin/ready-repo"}, 2 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) { tc.mod(h.cfg) })
			h.reviewedPR(2, "b1")
			ins := h.rd.all()
			if len(ins) != 1 {
				t.Fatalf("rounds: %d", len(ins))
			}
			rd := ins[0].Readiness
			if !slices.Equal(rd.Prepare, tc.wantPrepare) || !slices.Equal(rd.Ready, tc.wantReady) || rd.Timeout != tc.wantTimeout {
				t.Fatalf("readiness = prepare %q ready %q timeout %v, want %q %q %v", rd.Prepare, rd.Ready, rd.Timeout,
					tc.wantPrepare, tc.wantReady, tc.wantTimeout)
			}
			// The pool's env with {slot} filled in: the commands run as the slot's own setup does.
			if want := map[string]string{"WT_BRANCH": "review1"}; !maps.Equal(rd.Env, want) {
				t.Fatalf("readiness env = %v, want %v", rd.Env, want)
			}
		})
	}
}

func TestRoundCarriesThePerPRWorktreeEnvForReadiness(t *testing.T) {
	h := newHarness(t, withExampleWatch, func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "example/notes", Prepare: []string{"bin/prepare"}, Ready: []string{"bin/ready"},
			Env: map[string]string{"MAIN": "{clone}", "DATABASE_NAME": "notes_{slug}"}})
	})
	reviewedExamplePR(h, 2, "y1")
	ins := h.rd.all()
	if len(ins) != 1 {
		t.Fatalf("rounds: %d", len(ins))
	}
	rd := ins[0].Readiness
	if !slices.Equal(rd.Prepare, []string{"bin/prepare"}) || !slices.Equal(rd.Ready, []string{"bin/ready"}) || rd.Timeout != config.DefaultReadyTimeout {
		t.Fatalf("readiness = prepare %q ready %q timeout %v", rd.Prepare, rd.Ready, rd.Timeout)
	}
	if rd.Env["WT_BRANCH"] != "magnum-pr-2" || rd.Env["MAIN"] != "/tmp/main" || rd.Env["DATABASE_NAME"] != "notes_magnum-pr-2" {
		t.Fatalf("readiness env = %v, want the per-PR worktree env (WT_BRANCH=magnum-pr-2 and the [[repo]] env rendered)", rd.Env)
	}
}
