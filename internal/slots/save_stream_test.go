package slots

import (
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zhuravel/magnum/internal/store/storetest"
)

// fileLimit is the file size limit the two tests below give the test
// binary and the git it starts (RLIMIT_FSIZE): past it, a write fails.
const fileLimit = 64 << 10

// limitFileSize caps every file this process or a command it starts writes
// at fileLimit bytes (RLIMIT_FSIZE), until undo. With ignore a write past
// it fails with EFBIG, in git as here; without it SIGXFSZ ends git (a
// command starts with Go's handlers back at their default). The limit and
// the signal are the whole process's: the test is serial
// (storetest.Serial, before its fixture) and undoes them before it ends.
func limitFileSize(t *testing.T, ignore bool) (undo func()) {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if ignore {
		signal.Ignore(syscall.SIGXFSZ)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: fileLimit, Max: old.Max}); err != nil {
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

// bigChange rewrites the tracked README of the tree at dir with 200 KB that
// do not compress: a binary patch of about 270 KB, past fileLimit.
func bigChange(t *testing.T, dir string) []byte {
	t.Helper()
	b := make([]byte, 200<<10)
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
// save. Here a file may not pass 64 KB, which the old patch of 270 KB did
// (SIGXFSZ ended git and the save failed).
func TestTheSaveStopsGitAtTheCap(t *testing.T) {
	storetest.Serial(t)
	h := newHarness(t)
	h.m.saveCap = 1500
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	strayEdit(t, sl.Path)
	bigChange(t, sl.Path)

	undo := limitFileSize(t, false)
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
	want := bigChange(t, sl.Path)

	undo := limitFileSize(t, true)
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
