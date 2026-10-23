package notes

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// unlockLeftovers makes the old harnesses a test left unremovable (mode
// 0500) writable again, so the test's temporary directory can go.
func unlockLeftovers(r Repo) {
	left, _ := filepath.Glob(r.Harness() + ".old-*")
	for _, d := range left {
		_ = os.Chmod(d, 0o700)
	}
}

func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root removes a directory whatever its mode")
	}
}

// The swap and the notes write are the write: an old harness that cannot be
// removed afterwards (mode 0500 here) is not a failed write. The caller is
// told nil, and the new state is in place.
func TestAnOldHarnessThatCannotBeRemovedIsNotAFailedWrite(t *testing.T) {
	skipAsRoot(t)
	r := oldState(t)
	if err := os.Chmod(r.Harness(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unlockLeftovers(r) })
	if err := WriteState(r, newState()); err != nil {
		t.Fatalf("WriteState = %v, want nil: the new state is written", err)
	}
	got, err := ReadState(r)
	if err != nil || string(got.Notes) != "new\n" || len(got.Files) != 1 || got.Files[0].Path != "run.sh" {
		t.Fatalf("the notes after the write = %+v, %v; want the new state", got, err)
	}
	if left, _ := filepath.Glob(r.Harness() + ".old-*"); len(left) != 1 {
		t.Fatalf("old harnesses next to the notes: %v, want the one that could not be removed", left)
	}
}

// WriteStateReport names the leftover (its path and why); a write that
// removes its old harness reports none.
func TestWriteStateReportNamesTheLeftoverAndOnlyThat(t *testing.T) {
	skipAsRoot(t)
	r := oldState(t)
	if left, err := WriteStateReport(r, newState()); err != nil || left != nil {
		t.Fatalf("a clean write = %+v, %v; want no leftover", left, err)
	}
	if err := os.Chmod(r.Harness(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unlockLeftovers(r) })
	left, err := WriteStateReport(r, State{Exists: true, Notes: []byte("newer\n")})
	if err != nil {
		t.Fatalf("WriteStateReport = %v", err)
	}
	if want, _ := filepath.Glob(r.Harness() + ".old-*"); left == nil || len(want) != 1 || left.Path != want[0] || left.Err == nil {
		t.Fatalf("leftover = %+v, want %v with its error", left, want)
	}
}

func leftoverDir(t *testing.T, r Repo, suffix string, age time.Duration) string {
	t.Helper()
	dir := r.Harness() + suffix
	write(t, filepath.Join(dir, "x.sh"), "x")
	at := time.Now().Add(-age)
	if err := os.Chtimes(dir, at, at); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The sweep removes the old and the staged directories of a write that are
// older than the cutoff and keeps a fresh one, another repository's
// directory that only starts like them, and files.
func TestSweepLeftoversRemovesOnlyOldWriteDirectories(t *testing.T) {
	r := oldState(t)
	oldDir := leftoverDir(t, r, ".old-ABCDEFGH", 2*time.Hour)
	stagedDir := leftoverDir(t, r, ".new-A2C4E6G7", 3*time.Hour)
	fresh := leftoverDir(t, r, ".old-ZYXWVUTS", 5*time.Minute)
	other := leftoverDir(t, r, ".old-notaleftover", 5*time.Hour) // not the shape WriteState names
	sibling := Repo{Root: r.Root, Owner: r.Owner, Name: "talkable-other"}
	siblings := leftoverDir(t, sibling, ".old-ABCDEFGH", 5*time.Hour)
	file := r.Harness() + ".old-QRSTUVWX"
	write(t, file, "not a directory")
	_ = os.Chtimes(file, time.Now().Add(-5*time.Hour), time.Now().Add(-5*time.Hour))

	cutoff := time.Now().Add(-time.Hour)
	want := []string{stagedDir, oldDir} // by name
	if got, err := Leftovers(r, cutoff); err != nil || !slices.Equal(got, want) {
		t.Fatalf("Leftovers = %v, %v; want the two old write directories", got, err)
	}
	removed, err := SweepLeftovers(r, cutoff)
	if err != nil || !slices.Equal(removed, want) {
		t.Fatalf("SweepLeftovers = %v, %v", removed, err)
	}
	for _, gone := range want {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s stays: %v", gone, err)
		}
	}
	for _, kept := range []string{fresh, other, siblings, file, r.Harness(), r.Notes()} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
}

// A repository without an owner directory has nothing to sweep.
func TestSweepLeftoversOfARepositoryWithNoDirectoryIsEmpty(t *testing.T) {
	r := testRepo(t)
	if got, err := Leftovers(r, time.Now()); err != nil || len(got) != 0 {
		t.Fatalf("Leftovers = %v, %v", got, err)
	}
	if got, err := SweepLeftovers(r, time.Now()); err != nil || len(got) != 0 {
		t.Fatalf("SweepLeftovers = %v, %v", got, err)
	}
}
