package slots

import (
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// fileLimit is the file size limit the two tests below give the test
// binary and the git it starts (RLIMIT_FSIZE), past which a write fails:
// 64 KB, or, when go test keeps a log of the files the test reads (its
// cache's -test.testlogfile, which grows all through the run), 4 MB past
// that log's size now, so the log's writes go on.
func fileLimit() int64 {
	limit := int64(64 << 10)
	if f := flag.Lookup("test.testlogfile"); f != nil && f.Value.String() != "" {
		if fi, err := os.Stat(f.Value.String()); err == nil {
			limit = max(limit, fi.Size()+4<<20)
		}
	}
	return limit
}

// limitFileSize caps every file this process or a command it starts writes
// at limit bytes (RLIMIT_FSIZE), until undo. With ignore a write past it
// fails with EFBIG, in git as here; without it SIGXFSZ ends git (a command
// starts with Go's handlers back at their default). The limit and the
// signal are the whole process's: the test is serial (storetest.Serial,
// before its fixture) and undoes them before it ends.
func limitFileSize(t *testing.T, limit int64, ignore bool) (undo func()) {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if ignore {
		signal.Ignore(syscall.SIGXFSZ)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: uint64(limit), Max: old.Max}); err != nil {
		t.Fatal(err)
	}
	done := false
	undo = func() {
		if done {
			return
		}
		done = true
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
			t.Error(err)
		}
		if ignore { // Go's own handler back: Notify installs it, Stop keeps it
			c := make(chan os.Signal, 1)
			signal.Notify(c, syscall.SIGXFSZ)
			signal.Stop(c)
		}
	}
	t.Cleanup(undo)
	return undo
}

// bigChange rewrites the tracked README of the tree at dir with 200 KB past
// limit that do not compress: a binary patch a quarter longer, past limit.
func bigChange(t *testing.T, dir string, limit int64) []byte {
	t.Helper()
	b := make([]byte, limit+200<<10)
	_, _ = rand.NewChaCha8([32]byte{7}).Read(b)
	if err := os.WriteFile(filepath.Join(dir, "README"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

// git diff writes the patch into a pipe, which the save reads into its
// file and stops at the cap: past it, git is stopped and the patch goes
// path by path, each stopped at what is left. git wrote the whole patch
// to the file (--output) before its size was read, and again for the path
// past the cap: a tracked file of a GB wrote 2.5 GB to the data disk per
// save. Here a file may not pass fileLimit, which the old patch past it
// did (SIGXFSZ ended git and the save failed).
func TestTheSaveStopsGitAtTheCap(t *testing.T) {
	storetest.Serial(t)
	h := newHarness(t)
	h.m.saveCap = 1500
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	strayEdit(t, sl.Path)
	limit := fileLimit()
	bigChange(t, sl.Path, limit)

	undo := limitFileSize(t, limit, false)
	err := h.m.Resync(h.ctx, sl, h.shaPR7)
	undo()
	if err != nil {
		t.Fatalf("Resync: %v", err)
	}
	save := h.onlySave(t, h.layout.PRSaves("talkable", "talkable", pr.Number), sl.Name)
	if patch := readFile(t, filepath.Join(save, "changes.diff")); !strings.Contains(patch, "GEM stray") || strings.Contains(patch, "README") {
		t.Fatalf("the patch past the cap kept:\n%s\nwant Gemfile.lock's alone", patch)
	}
	if note := readFile(t, filepath.Join(save, "saved.txt")); !strings.Contains(note, "README (") {
		t.Fatalf("saved.txt does not name README as left out:\n%s", note)
	}
}

// A patch the save could not write whole (a full disk, a file size limit)
// fails the save, and the switch discards nothing. git diff --output
// ignores its write errors and exits 0, so a patch cut short counted as
// whole and the switch discarded what it lacked.
func TestAPatchCutShortFailsTheSave(t *testing.T) {
	storetest.Serial(t)
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	limit := fileLimit()
	want := bigChange(t, sl.Path, limit)

	undo := limitFileSize(t, limit, true)
	err := h.m.Resync(h.ctx, sl, h.shaPR7)
	undo()
	if err == nil {
		t.Fatal("Resync over a patch cut short: no error")
	}
	if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != h.shaPR8 {
		t.Fatalf("HEAD = %s, want it left at %s", head, h.shaPR8)
	}
	if got, err := os.ReadFile(filepath.Join(sl.Path, "README")); err != nil || string(got) != string(want) {
		t.Fatalf("README after the failed save: %d bytes, %v; want the change kept", len(got), err)
	}
	if evs := h.discarded(t, sl.Name); len(evs) != 0 {
		t.Fatalf("slot.discarded events = %+v, want none", evs)
	}
}

// A write into the save that fails (a full disk, a quota; a file size
// limit here) fails the save as a whole, and the removal deletes nothing:
// a file the save cannot write is not a file of the tree that cannot be
// read. The write error was a per-file reason, so saved.txt, a few KB,
// still went through, and `worktree remove --force` deleted the file the
// save never held.
func TestAWriteIntoTheSaveThatFailsRefusesTheRemoval(t *testing.T) {
	storetest.Serial(t)
	f := newPerPR(t, true)
	h := f.h
	limit := fileLimit()
	h.m.saveCap = 2*limit + 1<<20 // the file fits the cap: only its write fails
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	dump := make([]byte, limit+200<<10)
	_, _ = rand.NewChaCha8([32]byte{9}).Read(dump)
	if err := os.WriteFile(filepath.Join(sl.Path, "dump.sql"), dump, 0o644); err != nil {
		t.Fatal(err)
	}

	undo := limitFileSize(t, limit, true)
	err = h.m.RemovePRWorktree(h.ctx, sl, false)
	undo()
	if err == nil {
		t.Fatal("RemovePRWorktree over a save that could not write dump.sql: no error")
	}
	if got, err := os.ReadFile(filepath.Join(sl.Path, "dump.sql")); err != nil || string(got) != string(dump) {
		t.Fatalf("dump.sql after the failed save: %d bytes, %v; want it kept", len(got), err)
	}
	if got := h.slot(sl.Name); got.State == store.SlotRemoved {
		t.Fatalf("state = %s, want the removal refused", got.State)
	}
	if got := saves(t, h.layout.PRSaves("zhuravel", "widget", f.pr.Number)); len(got) != 0 {
		t.Fatalf("saves = %q, want none left by the failed save", got)
	}
	if evs := h.discarded(t, sl.Name); len(evs) != 0 {
		t.Fatalf("slot.discarded events = %+v, want none", evs)
	}
}

// A forced removal whose save fails part way goes on, as you asked, and
// keeps what the save copied: its saved.txt names every file the save
// does not hold, past the event's 20 too, with the error, and the
// slot.discarded warning names the save. The failed save was removed
// whole, the files it had copied with it, before `worktree remove --force`
// deleted them all.
func TestAForcedRemovalKeepsWhatItsFailedSaveCopied(t *testing.T) {
	storetest.Serial(t)
	f := newPerPR(t, true)
	h := f.h
	limit := fileLimit()
	h.m.saveCap = 2*limit + 1<<20 // the file fits the cap: only its write fails
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sl.Path, "a-notes.txt"), "mine\n")
	dump := make([]byte, limit+200<<10)
	_, _ = rand.NewChaCha8([32]byte{9}).Read(dump)
	if err := os.WriteFile(filepath.Join(sl.Path, "b-dump.sql"), dump, 0o644); err != nil {
		t.Fatal(err)
	}
	var after []string
	for i := range 24 {
		name := fmt.Sprintf("c-%02d.txt", i)
		writeFile(t, filepath.Join(sl.Path, name), "later\n")
		after = append(after, name)
	}

	undo := limitFileSize(t, limit, true)
	err = h.m.RemovePRWorktree(h.ctx, sl, true)
	undo()
	if err != nil {
		t.Fatalf("a forced removal: %v", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotRemoved || fsx.Exists(sl.Path) {
		t.Fatalf("state %s, directory left %v; want a forced removal to go on", got.State, fsx.Exists(sl.Path))
	}
	got := saves(t, h.layout.PRSaves("zhuravel", "widget", f.pr.Number))
	if len(got) != 1 {
		t.Fatalf("saves = %q, want the one the failed save copied into", got)
	}
	if b, err := os.ReadFile(filepath.Join(got[0], "untracked", "a-notes.txt")); err != nil || string(b) != "mine\n" {
		t.Fatalf("a-notes.txt saved = %q, %v; want it kept", b, err)
	}
	if _, err := os.Lstat(filepath.Join(got[0], "untracked", "b-dump.sql")); !os.IsNotExist(err) {
		t.Fatalf("the dump cut short is in the save: %v", err)
	}
	note := readFile(t, filepath.Join(got[0], "saved.txt"))
	for _, p := range append([]string{"b-dump.sql"}, after...) {
		if !strings.Contains(note, "- "+p+" (not saved: the save failed)\n") {
			t.Fatalf("saved.txt does not name %s as not saved:\n%s", p, note)
		}
	}
	if !strings.Contains(note, "failed part way") || !strings.Contains(note, "file too large") {
		t.Fatalf("saved.txt does not say the save failed part way, with its error:\n%s", note)
	}
	evs := h.discarded(t, sl.Name)
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, got[0]) ||
		!strings.Contains(evs[0].Message, "(+5 more)") || !strings.Contains(evs[0].Message, "forced") {
		t.Fatalf("slot.discarded events = %+v, want one warning that names the save, 20 files and how many more", evs)
	}
}
