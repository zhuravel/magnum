package fsx

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteFileAtomicGivesExactlyThePermWhateverTheUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	dir := t.TempDir()
	for _, perm := range []os.FileMode{0o600, 0o644, 0o755} {
		path := filepath.Join(dir, "f"+perm.String())
		if err := WriteFileAtomic(path, []byte("x"), perm); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != perm {
			t.Errorf("mode = %v, want %v", st.Mode().Perm(), perm)
		}
	}
}

func TestWriteFileAtomicReplacesAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.txt")
	for _, text := range []string{"one\n", "two\n", ""} {
		if err := WriteFileAtomic(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, path); got != text {
			t.Errorf("content = %q, want %q", got, text)
		}
	}
	if got := entries(t, dir); len(got) != 1 {
		t.Errorf("entries = %v, want only state.txt", got)
	}
}

func TestWriteFileAtomicReplacesASymlinkInsteadOfFollowingIt(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("orig"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "link")
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, victim); got != "orig" {
		t.Errorf("the symlink's target was written: %q", got)
	}
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() {
		t.Errorf("path is not a regular file now: %v %v", st, err)
	}
}

func TestWriteFileAtomicConcurrentWritersNeverTearTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tabbar.txt")
	if err := WriteFileAtomic(path, []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	texts := []string{strings.Repeat("a", 4096), strings.Repeat("b", 4096), strings.Repeat("c", 4096)}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, text := range texts {
		wg.Go(func() {
			for range 50 {
				if err := WriteFileAtomic(path, []byte(text), 0o644); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	done := make(chan struct{})
	go func() { // reader: every observation must be one complete text
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Error(err)
				return
			}
			if s := string(got); s != "seed" && s != texts[0] && s != texts[1] && s != texts[2] {
				t.Errorf("torn read: %d bytes", len(got))
				return
			}
		}
	}()
	wg.Wait()
	close(stop)
	<-done
	if got := entries(t, dir); len(got) != 1 {
		t.Errorf("leftover temp files: %v", got)
	}
}

func TestWriteFileAtomicFailureLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.Mkdir(path, 0o700); err != nil { // a directory squats on the target
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("x"), 0o600); err == nil {
		t.Errorf("expected an error when the target is a directory")
	}
	if got := entries(t, dir); len(got) != 1 {
		t.Errorf("temp file left behind after a failed write: %v", got)
	}
	if err := WriteFileAtomic(filepath.Join(dir, "missing", "f"), []byte("x"), 0o600); err == nil {
		t.Errorf("expected an error for a missing directory (the caller creates it)")
	}
}

func TestWriteFileAtomicInStaysInsideTheRoot(t *testing.T) {
	outer := t.TempDir()
	dir := filepath.Join(outer, "root")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := WriteFileAtomicIn(root, "sub/f.txt", []byte("in"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "sub", "f.txt")); got != "in" {
		t.Errorf("content = %q", got)
	}
	if st, _ := os.Stat(filepath.Join(dir, "sub", "f.txt")); st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", st.Mode().Perm())
	}
	if got := entries(t, filepath.Join(dir, "sub")); len(got) != 1 {
		t.Errorf("leftover temp files: %v", got)
	}
	if err := os.Symlink(outer, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "out/escape", "/abs"} {
		if err := WriteFileAtomicIn(root, name, []byte("x"), 0o600); err == nil {
			t.Errorf("%s: written outside the root", name)
		}
	}
	if got := entries(t, outer); len(got) != 1 {
		t.Errorf("files outside the root: %v", got)
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	if !Exists(dir) {
		t.Errorf("a directory exists")
	}
	if Exists(filepath.Join(dir, "nope")) {
		t.Errorf("a missing file does not exist")
	}
	if err := os.Symlink(filepath.Join(dir, "nope"), filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}
	if Exists(filepath.Join(dir, "dangling")) {
		t.Errorf("a dangling symlink counts as missing (os.Stat follows it)")
	}
}
