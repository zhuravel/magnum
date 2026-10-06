package agents

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
)

func TestNotesFiles(t *testing.T) {
	dir, lock := NotesFiles("/h/state/notes/talkable/my.repo.md")
	if dir != "/h/state/notes/talkable/my.repo" || lock != "/h/state/notes/talkable/my.repo.lock" {
		t.Fatalf("NotesFiles = %q, %q", dir, lock)
	}
	if dir, lock := NotesFiles(""); dir != "" || lock != "" {
		t.Fatalf("NotesFiles(\"\") = %q, %q", dir, lock)
	}
}

// runLock runs a lock line built like NotesLockLine's, with a short wait.
func runLock(t *testing.T, lock string, tries int) string {
	t.Helper()
	out, _ := exec.Command("sh", "-c", notesLockLine(lock, tries, 0, 10)).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// The judge's lock is a directory: mkdir takes it, a held one makes the
// next judge wait and then give up ("notes busy"), one older than ten
// minutes (its judge died) is taken over, rmdir releases it.
func TestNotesLockLineTakesWaitsAndBreaksStaleLocks(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	lock := filepath.Join(t.TempDir(), "it's a", "talkable.lock") // quoting
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := runLock(t, lock, 3); got != "notes locked" {
		t.Fatalf("free lock: %q", got)
	}
	if fi, err := os.Stat(lock); err != nil || !fi.IsDir() {
		t.Fatalf("lock not held: %v", err)
	}
	if got := runLock(t, lock, 3); got != "notes busy" {
		t.Fatalf("held lock: %q, want notes busy", got)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("a busy judge removed the held lock: %v", err)
	}
	old := time.Now().Add(-20 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if got := runLock(t, lock, 3); got != "notes locked" {
		t.Fatalf("stale lock: %q, want it taken over", got)
	}
	if out, err := exec.Command("sh", "-c", NotesUnlockLine(lock)).CombinedOutput(); err != nil {
		t.Fatalf("unlock: %v %s", err, out)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock still there after unlock: %v", err)
	}
	if line := NotesLockLine("/s/notes/talkable/talkable.lock"); !strings.Contains(line, "mkdir /s/notes/talkable/talkable.lock") ||
		!strings.Contains(line, "-ge 90") || !strings.Contains(line, "sleep 2") || !strings.Contains(line, "-mmin +10") {
		t.Fatalf("NotesLockLine = %s", line)
	}
}

func TestNotesHarnessListsEntriesSortedAndBounded(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"jest-setup.js", "a-run.sh", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "fixtures"), 0o700); err != nil {
		t.Fatal(err)
	}
	names, more := NotesHarness(dir)
	if want := []string{".hidden", "a-run.sh", "fixtures/", "jest-setup.js"}; !slices.Equal(names, want) || more != 0 {
		t.Fatalf("NotesHarness = %q, %d; want %q", names, more, want)
	}
	if names, more := NotesHarness(filepath.Join(dir, "missing")); names != nil || more != 0 {
		t.Fatalf("missing dir = %q, %d", names, more)
	}
	for i := range NotesHarnessMax + 2 {
		if err := os.WriteFile(filepath.Join(dir, "z"+strings.Repeat("x", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	names, more = NotesHarness(dir)
	if len(names) != NotesHarnessMax || more != 6 || names[0] != ".hidden" {
		t.Fatalf("bounded listing: %d names, %d more (first %q)", len(names), more, names[0])
	}
}

// RenderPrompt derives the harness directory, the lock and its commands
// from NotesPath, and the judge prompts' <magnum> blocks name them with the
// listing (and how many entries it left out).
func TestJudgePromptsCarryTheNotesFields(t *testing.T) {
	d := judgeFixture()
	d.NotesPath = testNotesPath
	d.NotesHarness, d.NotesHarnessMore = []string{"fixtures/", "jest-setup.js", "run-spec.sh"}, 3
	dir, lock := NotesFiles(testNotesPath)
	want := "\nnotes: " + testNotesPath + "\nnotes_dir: " + dir + "\nnotes_harness: fixtures/, jest-setup.js, run-spec.sh (+3 more)\n" +
		"notes_lock: " + NotesLockLine(lock) + "\nnotes_unlock: " + NotesUnlockLine(lock) + "\n"
	for _, name := range []string{"judge-initial.md", "judge-rereview.md", "judge-recovery.md", "judge-continue.md"} {
		got, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if block := got[strings.Index(got, "<magnum>"):]; !strings.Contains(block, want) {
			t.Errorf("%s: the <magnum> block lacks\n%s\nin\n%s", name, want, block)
		}
	}
	d.NotesHarness, d.NotesHarnessMore = nil, 0
	got, err := RenderPrompt(prompt(t, "judge-initial.md"), &d)
	if err != nil || !strings.Contains(got, "\nnotes_harness:\nnotes_lock: ") {
		t.Fatalf("empty harness: %v\n%s", err, got)
	}
}

// codex review runs against the merge base when it is known: the base ref
// moves when the base branch gains commits, and the review then flagged
// files the PR does not touch.
func TestCodexReviewUsesTheMergeBase(t *testing.T) {
	role := defaultRole(t, RoleCodexReview)
	d := ShellData{BaseRef: "origin/master", BaseSHA: "0123456789abcdef0123456789abcdef01234567", ReportPath: "/r/codex.md", Marker: DoneMarker("r-1")}
	got, err := ShellLine(role, d)
	if err != nil || !strings.Contains(got, "command codex review --base 0123456789abcdef0123456789abcdef01234567; } |") {
		t.Fatalf("with a merge base: %v\n%s", err, got)
	}
	d.BaseSHA = ""
	if got, err := ShellLine(role, d); err != nil || !strings.Contains(got, "command codex review --base origin/master; } |") {
		t.Fatalf("without a merge base: %v\n%s", err, got)
	}

	// The full-line template does the same.
	sh := role
	sh.Command, sh.Prompt = "", "codex-review.sh"
	d.BaseSHA = "abc1234"
	if got, err := ShellLine(sh, d); err != nil || !strings.Contains(got, "--base abc1234; } |") {
		t.Fatalf("codex-review.sh: %v\n%s", err, got)
	}
	d.BaseSHA, d.BaseRef = "", ""
	if _, err := ShellLine(sh, d); err == nil || !strings.Contains(err.Error(), "both empty") {
		t.Fatalf("codex-review.sh without any base: err = %v", err)
	}

	// A command naming only one of them needs that one.
	only := config.Role{Name: "lint", Kind: config.KindShell, Mode: config.ModeShell, Command: "lint --since {{.BaseSHA}}", Capture: config.CaptureFile}
	if _, err := ShellLine(only, ShellData{BaseRef: "origin/master", Marker: DoneMarker("r-1")}); err == nil || !strings.Contains(err.Error(), ".BaseSHA") {
		t.Fatalf("only .BaseSHA, empty: err = %v", err)
	}
	if got, err := ShellLine(only, ShellData{BaseSHA: "it's", Marker: DoneMarker("r-1")}); err != nil || !strings.Contains(got, `lint --since 'it'\''s'`) {
		t.Fatalf("BaseSHA is shell-quoted: %v\n%s", err, got)
	}
}
