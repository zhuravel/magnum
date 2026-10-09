package notes

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failRenames makes the harness renames that fail return an error: fail
// says which (from, to) pair fails.
func failRenames(t *testing.T, fail func(from, to string) bool) {
	t.Helper()
	renameDir = func(from, to string) error {
		if fail(from, to) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errors.New("device busy")}
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { renameDir = os.Rename })
}

func oldState(t *testing.T) Repo {
	t.Helper()
	r := testRepo(t)
	write(t, r.Notes(), "old\n")
	write(t, filepath.Join(r.Harness(), "old.sh"), "x")
	return r
}

func newState() State {
	return State{Exists: true, Notes: []byte("new\n"), Files: []Blob{{Path: "run.sh", Body: []byte("bin/rspec")}}}
}

func wantOldState(t *testing.T, r Repo) {
	t.Helper()
	if b, err := os.ReadFile(r.Notes()); err != nil || string(b) != "old\n" {
		t.Fatalf("notes = %q, %v; want the old notes", b, err)
	}
	if _, err := os.Stat(filepath.Join(r.Harness(), "old.sh")); err != nil {
		t.Fatalf("the old harness is gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Harness(), "run.sh")); !os.IsNotExist(err) {
		t.Fatalf("the new harness is in place: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(r.Notes())); len(ents) != 2 {
		t.Fatalf("leftovers next to the notes: %v", ents)
	}
}

// A harness swap that fails leaves the notes as they were: the notes are
// written only after the new harness is in place.
func TestAFailedHarnessSwapKeepsTheOldNotes(t *testing.T) {
	r := oldState(t)
	failRenames(t, func(from, _ string) bool { return strings.Contains(filepath.Base(from), ".new-") })
	if err := WriteState(r, newState()); err == nil || !strings.Contains(err.Error(), "device busy") {
		t.Fatalf("WriteState = %v, want the rename's error", err)
	}
	wantOldState(t, r)
}

// A swap whose restore fails too reports both, and where the old harness
// is: the caller learns the harness is not where it was.
func TestAFailedHarnessRestoreIsReported(t *testing.T) {
	r := oldState(t)
	failRenames(t, func(from, _ string) bool {
		base := filepath.Base(from)
		return strings.Contains(base, ".new-") || strings.Contains(base, ".old-")
	})
	err := WriteState(r, newState())
	if err == nil || strings.Count(err.Error(), "device busy") != 2 || !strings.Contains(err.Error(), "restore the old harness") {
		t.Fatalf("WriteState = %v, want the swap's and the restore's errors", err)
	}
	if b, rerr := os.ReadFile(r.Notes()); rerr != nil || string(b) != "old\n" {
		t.Fatalf("notes = %q, %v; want the old notes", b, rerr)
	}
}

// A notes write that fails after the harness swap puts the old harness
// back: the notes and the harness stay one state.
func TestAFailedNotesWriteRestoresTheOldHarness(t *testing.T) {
	r := oldState(t)
	failRenames(t, func(string, string) bool { return false })
	s := newState()
	s.Exists = false // remove the notes: a directory in their place refuses it
	if err := os.Remove(r.Notes()); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.Notes(), "keep"), "x")
	if err := WriteState(r, s); err == nil {
		t.Fatal("WriteState over a notes directory succeeded")
	}
	if _, err := os.Stat(filepath.Join(r.Harness(), "old.sh")); err != nil {
		t.Fatalf("the old harness is gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Harness(), "run.sh")); !os.IsNotExist(err) {
		t.Fatalf("the new harness stayed in place: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(r.Notes())); len(ents) != 2 {
		t.Fatalf("leftovers next to the notes: %v", ents)
	}
}
