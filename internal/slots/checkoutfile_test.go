package slots

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// checkoutDirs returns a checkout, an outside directory (a sibling of the
// checkout) and a main clone, all empty.
func checkoutDirs(t *testing.T) (checkout, outside, main string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkout, outside, main = filepath.Join(root, "checkout"), filepath.Join(root, "outside"), filepath.Join(root, "main")
	for _, d := range []string{checkout, outside, main} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return checkout, outside, main
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// dirNames lists dir (sorted names).
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func assertRegular(t *testing.T, path, content string, perm fs.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != perm {
		t.Fatalf("%s: mode %v, want a regular file with %v", path, fi.Mode(), perm)
	}
	if got := readFile(t, path); got != content {
		t.Fatalf("%s = %q, want %q", path, got, content)
	}
}

func TestWriteCheckoutFile(t *testing.T) {
	t.Parallel()
	checkout, _, _ := checkoutDirs(t)
	// Parents are created; the mode is exactly perm whatever the umask.
	for _, perm := range []fs.FileMode{0o600, 0o640, 0o777, 0o666} {
		if err := WriteCheckoutFile(checkout, "config/deep/local.rb", []byte(perm.String()), perm); err != nil {
			t.Fatalf("WriteCheckoutFile(%v): %v", perm, err)
		}
		assertRegular(t, filepath.Join(checkout, "config", "deep", "local.rb"), perm.String(), perm)
	}
	if names := dirNames(t, filepath.Join(checkout, "config", "deep")); !slices.Equal(names, []string{"local.rb"}) {
		t.Fatalf("temp files left behind: %v", names)
	}
}

func TestWriteCheckoutFileFollowsSymlinkInsideCheckout(t *testing.T) {
	t.Parallel()
	checkout, _, _ := checkoutDirs(t)
	if err := os.Mkdir(filepath.Join(checkout, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, "../real", filepath.Join(checkout, "config", "initializers"))
	if err := WriteCheckoutFile(checkout, "config/initializers/local.rb", []byte("x"), 0o644); err != nil {
		t.Fatalf("symlinked parent inside the checkout: %v", err)
	}
	assertRegular(t, filepath.Join(checkout, "real", "local.rb"), "x", 0o644)
}

func TestWriteCheckoutFileRefusesSymlinkedParentOutside(t *testing.T) {
	t.Parallel()
	for name, link := range map[string]string{
		"leaf parent":         "config/initializers",
		"intermediate parent": "config",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkout, outside, main := checkoutDirs(t)
			writeFile(t, filepath.Join(main, "config", "initializers", "local.rb"), "LOCAL = 1\n")
			symlink(t, outside, filepath.Join(checkout, link))
			err := CopyIntoCheckout(filepath.Join(main, "config", "initializers", "local.rb"), checkout, "config/initializers/local.rb")
			if err == nil {
				t.Fatal("copy through a symlink out of the checkout succeeded")
			}
			if names := dirNames(t, outside); len(names) != 0 {
				t.Fatalf("wrote outside the checkout: %v", names)
			}
		})
	}
}

func TestWriteCheckoutFileReplacesSymlinkAtDestination(t *testing.T) {
	t.Parallel()
	checkout, outside, _ := checkoutDirs(t)
	victim := filepath.Join(outside, "victim")
	writeFile(t, victim, "orig")
	symlink(t, victim, filepath.Join(checkout, MiseLocal))
	symlink(t, filepath.Join(outside, "absent"), filepath.Join(checkout, "dangling"))
	for _, rel := range []string{MiseLocal, "dangling"} {
		if err := WriteCheckoutFile(checkout, rel, []byte("rendered"), 0o600); err != nil {
			t.Fatalf("WriteCheckoutFile(%s): %v", rel, err)
		}
		assertRegular(t, filepath.Join(checkout, rel), "rendered", 0o600)
	}
	if got := readFile(t, victim); got != "orig" {
		t.Fatalf("symlink target outside the checkout rewritten: %q", got)
	}
	if names := dirNames(t, outside); !slices.Equal(names, []string{"victim"}) {
		t.Fatalf("outside = %v", names)
	}
}

func TestWriteCheckoutFileRefusesEscapes(t *testing.T) {
	t.Parallel()
	checkout, outside, _ := checkoutDirs(t)
	for _, rel := range []string{"../outside/x", "a/../../outside/x", filepath.Join(outside, "x"), "", ".", "a/.."} {
		if err := WriteCheckoutFile(checkout, rel, []byte("x"), 0o644); err == nil {
			t.Errorf("WriteCheckoutFile(%q) succeeded", rel)
		}
	}
	if names := dirNames(t, outside); len(names) != 0 {
		t.Fatalf("wrote outside the checkout: %v", names)
	}
	if names := dirNames(t, checkout); len(names) != 0 {
		t.Fatalf("checkout = %v", names)
	}
}

func TestWriteCheckoutFileRemovesTempOnFailure(t *testing.T) {
	t.Parallel()
	checkout, _, _ := checkoutDirs(t)
	writeFile(t, filepath.Join(checkout, "dir", "keep"), "k")
	if err := WriteCheckoutFile(checkout, "dir", []byte("x"), 0o644); err == nil {
		t.Fatal("replacing a non-empty directory succeeded")
	}
	if names := dirNames(t, checkout); !slices.Equal(names, []string{"dir"}) {
		t.Fatalf("temp file left behind: %v", names)
	}
}

func TestCopyIntoCheckout(t *testing.T) {
	t.Parallel()
	checkout, _, main := checkoutDirs(t)
	src := filepath.Join(main, "bin", "tool")
	writeFile(t, src, "#!/bin/sh\n")
	if err := os.Chmod(src, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := CopyIntoCheckout(src, checkout, "bin/tool"); err != nil {
		t.Fatal(err)
	}
	assertRegular(t, filepath.Join(checkout, "bin", "tool"), "#!/bin/sh\n", 0o750)
	// A missing source is skipped without creating anything.
	if err := CopyIntoCheckout(filepath.Join(main, "absent.yml"), checkout, "config/absent.yml"); err != nil {
		t.Fatalf("missing source: %v", err)
	}
	if names := dirNames(t, checkout); !slices.Equal(names, []string{"bin"}) {
		t.Fatalf("checkout = %v", names)
	}
}

// TestPreparePerPRConfinedToCheckout: a per-PR worktree whose tracked files
// turn copy_files' parent and .mise.local.toml into symlinks out of the
// worktree cannot make magnum write there.
func TestPreparePerPRConfinedToCheckout(t *testing.T) {
	t.Parallel()
	checkout, outside, main := checkoutDirs(t)
	writeFile(t, filepath.Join(main, MiseLocal), "[env]\nGH_TOKEN = \"gho_x\"\nKEEP = \"k\"\n")
	writeFile(t, filepath.Join(main, "config", "initializers", "local.rb"), "LOCAL = 1\n")
	victim := filepath.Join(outside, "victim")
	writeFile(t, victim, "orig")
	symlink(t, victim, filepath.Join(checkout, MiseLocal))
	symlink(t, outside, filepath.Join(checkout, "config", "initializers"))

	m := New(Deps{LookPath: func(string) (string, error) { return "", errors.New("no mise") }})
	p := perPRPlan{slug: PRSlug(7), copy: []string{"config/initializers/local.rb"}, strip: stripKeys(nil), env: map[string]string{"WT_BRANCH": PRSlug(7)}}
	err := m.preparePerPR(context.Background(), store.Slot{Name: "zhuravel/widget#7", Path: checkout, MainClone: main}, p)
	if err == nil {
		t.Fatal("copy through a symlink out of the worktree succeeded")
	}
	local := readFile(t, filepath.Join(checkout, MiseLocal))
	if strings.Contains(local, "GH_TOKEN") || !strings.Contains(local, `KEEP = "k"`) {
		t.Fatalf("rendered:\n%s", local)
	}
	assertRegular(t, filepath.Join(checkout, MiseLocal), local, 0o600)
	if got := readFile(t, victim); got != "orig" {
		t.Fatalf("symlink target outside the worktree rewritten: %q", got)
	}
	if names := dirNames(t, outside); !slices.Equal(names, []string{"victim"}) {
		t.Fatalf("outside = %v", names)
	}
}
