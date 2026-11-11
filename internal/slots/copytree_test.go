package slots

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A copy_files entry that names a directory (the main clone's generated
// fixtures, which git ignores: 884 files a round's specs need) copies its
// tree into a fresh checkout, regular files only, each with its mode. A
// later checkout writes only what it lacks or holds an older copy of, so
// the files are not rewritten each round.
func TestCopyFilesCopiesADirectorysTreeAndSkipsFilesAlreadyCurrent(t *testing.T) {
	t.Parallel()
	checkout, outside, main := checkoutDirs(t)
	mocks := filepath.Join(main, "app", "mocks")
	writeFile(t, filepath.Join(mocks, "a.json"), `{"a":1}`)
	writeFile(t, filepath.Join(mocks, "deep", "b.json"), `{"b":2}`)
	if err := os.Chmod(filepath.Join(mocks, "deep", "b.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outside, "secret"), "s")
	symlink(t, filepath.Join(outside, "secret"), filepath.Join(mocks, "leak.json")) // never followed out of the clone
	symlink(t, "a.json", filepath.Join(mocks, "alias.json"))                        // nor copied as a link

	if err := copyIntoCheckout(main, checkout, []string{"app/mocks"}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(checkout, "app", "mocks")
	if names := dirNames(t, dst); !slices.Equal(names, []string{"a.json", "deep"}) {
		t.Fatalf("copied %v, want the regular files and their directory only", names)
	}
	assertRegular(t, filepath.Join(dst, "a.json"), `{"a":1}`, 0o644)
	assertRegular(t, filepath.Join(dst, "deep", "b.json"), `{"b":2}`, 0o600)

	// A copy as new as its source, of its size, is current: not written
	// again (the edit of the same size stays).
	writeFile(t, filepath.Join(dst, "a.json"), `{"a":9}`)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dst, "a.json"), later, later); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dst, "deep", "b.json")); err != nil {
		t.Fatal(err)
	}
	if err := copyIntoCheckout(main, checkout, []string{"app/mocks"}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dst, "a.json")); got != `{"a":9}` {
		t.Fatalf("a current copy was rewritten: %q", got)
	}
	assertRegular(t, filepath.Join(dst, "deep", "b.json"), `{"b":2}`, 0o600) // a missing one is written

	// A source newer than the copy, or of another size, is copied again.
	newer := later.Add(time.Hour)
	if err := os.Chtimes(filepath.Join(mocks, "a.json"), newer, newer); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(mocks, "deep", "b.json"), `{"b":22}`)
	if err := copyIntoCheckout(main, checkout, []string{"app/mocks"}); err != nil {
		t.Fatal(err)
	}
	assertRegular(t, filepath.Join(dst, "a.json"), `{"a":1}`, 0o644)
	if got := readFile(t, filepath.Join(dst, "deep", "b.json")); got != `{"b":22}` {
		t.Fatalf("a copy of another size stayed: %q", got)
	}
	if names := dirNames(t, dst); !slices.Equal(names, []string{"a.json", "deep"}) {
		t.Fatalf("temp files left behind: %v", names)
	}
}

// A missing source directory is skipped, as a missing file is.
func TestCopyFilesSkipsAMissingSourceDirectory(t *testing.T) {
	t.Parallel()
	checkout, _, main := checkoutDirs(t)
	if err := copyIntoCheckout(main, checkout, []string{"app/absent/"}); err != nil {
		t.Fatalf("missing directory: %v", err)
	}
	if names := dirNames(t, checkout); len(names) != 0 {
		t.Fatalf("checkout = %v, want nothing", names)
	}
}

// No path of a directory's copy leaves either root: a source reached
// through a link out of the main clone is refused, and so is a checkout
// whose tracked link turns the destination's parent into a path outside
// it; nothing is written outside the checkout. The clone itself is no
// directory to copy.
func TestCopyFilesRefusesADirectoryPathThatLeavesEitherRoot(t *testing.T) {
	t.Parallel()
	t.Run("source", func(t *testing.T) {
		t.Parallel()
		checkout, outside, main := checkoutDirs(t)
		writeFile(t, filepath.Join(outside, "fixtures", "x.json"), "x")
		symlink(t, filepath.Join(outside, "fixtures"), filepath.Join(main, "app", "mocks"))
		if err := copyIntoCheckout(main, checkout, []string{"app/mocks"}); err == nil {
			t.Fatal("a directory linked out of the main clone was copied")
		}
		if names := dirNames(t, checkout); len(names) != 0 {
			t.Fatalf("checkout = %v, want nothing", names)
		}
	})
	t.Run("destination", func(t *testing.T) {
		t.Parallel()
		checkout, outside, main := checkoutDirs(t)
		writeFile(t, filepath.Join(main, "app", "mocks", "x.json"), "x")
		symlink(t, outside, filepath.Join(checkout, "app"))
		if err := copyIntoCheckout(main, checkout, []string{"app/mocks"}); err == nil {
			t.Fatal("a checkout whose link leads out of it was written through")
		}
		if names := dirNames(t, outside); len(names) != 0 {
			t.Fatalf("written outside the checkout: %v", names)
		}
	})
	t.Run("the clone itself", func(t *testing.T) {
		t.Parallel()
		checkout, _, main := checkoutDirs(t)
		writeFile(t, filepath.Join(main, "x.json"), "x")
		if err := copyIntoCheckout(main, checkout, []string{"."}); err == nil {
			t.Fatal("copy_files \".\" copied the whole clone")
		}
		if names := dirNames(t, checkout); len(names) != 0 {
			t.Fatalf("checkout = %v, want nothing", names)
		}
	})
}
