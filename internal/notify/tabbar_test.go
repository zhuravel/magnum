package notify

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

func tabNotifier(t *testing.T) (*Notifier, string) {
	t.Helper()
	l := paths.Layout{Home: t.TempDir()}
	return &Notifier{Layout: l}, l.TabBar()
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteTabBarCreatesStateDirAndFile(t *testing.T) {
	n, path := tabNotifier(t)
	if err := n.WriteTabBar("magnum: 2 reviewing · 1 queued"); err != nil {
		t.Fatalf("WriteTabBar: %v", err)
	}
	if got := readFile(t, path); got != "magnum: 2 reviewing · 1 queued\n" {
		t.Errorf("content = %q", got)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", st.Mode().Perm())
	}
	if d, _ := os.Stat(filepath.Dir(path)); d.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode = %v, want 0700", d.Mode().Perm())
	}
}

func TestWriteTabBarReplacesAndLeavesNoTempFiles(t *testing.T) {
	n, path := tabNotifier(t)
	for _, text := range []string{"one", "two", "three"} {
		if err := n.WriteTabBar(text); err != nil {
			t.Fatal(err)
		}
	}
	if got := readFile(t, path); got != "three\n" {
		t.Errorf("content = %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("state dir = %v; want only tabbar.txt", names)
	}
}

func TestWriteTabBarNormalizesToOneLine(t *testing.T) {
	// herdr renders the last output line only and strips ESC, so the file
	// must be a single plain-text line.
	n, path := tabNotifier(t)
	secret := "ghp_" + strings.Repeat("c", 36)
	cases := []struct{ in, want string }{
		{"a\nb\r\nc", "a b  c\n"},
		{"\x1b[31mred\x1b[0m", "[31mred[0m\n"},
		{"  padded \t tab  ", "padded   tab\n"},
		{"token " + secret, "token " + "<redacted>" + "\n"},
		{"", ""},
		{" \n ", ""},
	}
	for _, tc := range cases {
		if err := n.WriteTabBar(tc.in); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, path); got != tc.want {
			t.Errorf("WriteTabBar(%q) wrote %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWriteTabBarSkipsUnchangedContent(t *testing.T) {
	n, path := tabNotifier(t)
	if err := n.WriteTabBar("same"); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := n.WriteTabBar("same"); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if !st.ModTime().Equal(old) {
		t.Errorf("identical text rewrote the file (mtime %v)", st.ModTime())
	}
	if err := n.WriteTabBar("different"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "different\n" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteTabBarConcurrentWritersNeverTearTheFile(t *testing.T) {
	n, path := tabNotifier(t)
	if err := n.WriteTabBar("seed"); err != nil {
		t.Fatal(err)
	}
	texts := []string{strings.Repeat("a", 4096), strings.Repeat("b", 4096), strings.Repeat("c", 4096)}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, text := range texts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := n.WriteTabBar(text); err != nil {
					t.Error(err)
					return
				}
			}
		}()
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
			s := strings.TrimSuffix(string(got), "\n")
			if s != "seed" && s != texts[0] && s != texts[1] && s != texts[2] {
				t.Errorf("torn read: %d bytes", len(got))
				return
			}
		}
	}()
	wg.Wait()
	close(stop)
	<-done
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("leftover temp files: %d entries", len(entries))
	}
}

func TestWriteTabBarErrors(t *testing.T) {
	if err := (&Notifier{}).WriteTabBar("x"); err == nil {
		t.Errorf("zero Layout must be rejected rather than writing into the cwd")
	}
	n, path := tabNotifier(t)
	if err := os.MkdirAll(path, 0o700); err != nil { // a directory squats on the target
		t.Fatal(err)
	}
	if err := n.WriteTabBar("x"); err == nil {
		t.Errorf("expected an error when the target is a directory")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temp file left behind after failed write: %d entries", len(entries))
	}
}
