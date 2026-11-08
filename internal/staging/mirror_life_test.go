package staging

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The first clone runs in the background on a context of its own, under
// the mirror's flock: a Refresh whose ctx is short returns at once with
// every status "cloning", a Refresh meanwhile starts no second clone, and
// once it is done the next Refresh reads the mirror.
func TestAFirstCloneRunsInTheBackgroundWhateverTheCallersContext(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.push(10*time.Minute, s.p2)
	s.git.absent = true
	entered, release := make(chan struct{}), make(chan struct{})
	s.git.clone = func() error { close(entered); <-release; return nil }
	c := s.client()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	start := time.Now()
	err := c.Refresh(ctx, "acme/app", []OpenPR{{Number: 7, Base: "master"}})
	cancel()
	if err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("Refresh starting the clone: %v after %s, want nil at once", err, time.Since(start))
	}
	if got := one(c, s.pr()); got.State != StateUnknown || !strings.Contains(got.Why, "cloning") {
		t.Errorf("while cloning: %s %q, want unknown, cloning", got.State, got.Why)
	}
	<-entered
	s.now = s.now.Add(2 * RefreshEvery)
	s.refresh(c, 7)
	if n := s.git.n("Clone"); n != 1 {
		t.Errorf("%d clones, want one while it runs", n)
	}
	close(release)
	c.bg.Wait()
	s.refresh(c, 7)
	if got := one(c, s.pr()); got.State != StatePushed {
		t.Errorf("after the clone: %s %q, want pushed", got.State, got.Why)
	}
	if n := s.git.n("Fetch"); n != 1 {
		t.Errorf("%d fetches, want one, after the clone", n)
	}
}

// A clone that fails is tried again after a wait that grows: 1 min, then
// 5 min; meanwhile every status says the clone failed and when it is tried
// again.
func TestAFailedCloneBacksOff(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.git.absent = true
	s.git.clone = func() error { return errors.New("fatal: unable to access 'https://github.com/acme/app.git/'") }
	c := s.client()
	try := func(after time.Duration, clones int) {
		t.Helper()
		s.now = s.now.Add(after)
		_ = c.Refresh(context.Background(), "acme/app", []OpenPR{{Number: 7, Base: "master"}})
		c.bg.Wait()
		if n := s.git.n("Clone"); n != clones {
			t.Errorf("%s later: %d clones, want %d", after, n, clones)
		}
	}
	try(0, 1)
	if got := one(c, s.pr()); got.State != StateUnknown || !strings.Contains(got.Why, "could not be cloned") || !strings.Contains(got.Why, "next try") {
		t.Errorf("after a failed clone: %s %q", got.State, got.Why)
	}
	try(30*time.Second, 1)
	try(31*time.Second, 2)
	try(4*time.Minute, 2)
	try(time.Minute+time.Second, 3)
}

// Under the flock, the git lock files a killed git left in the mirror (an
// unplugged disk, a power cut) are removed once they are 10 minutes old; a
// newer one may be a git at work and stays.
func TestStaleGitLocksInTheMirrorAreClearedUnderTheFlock(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.push(10*time.Minute, s.p2)
	dir := filepath.Join(t.TempDir(), "acme", "app.git")
	stale := []string{"refs/heads/staging-void.lock", "packed-refs.lock", "config.lock"}
	fresh := []string{"refs/heads/master.lock", "shallow.lock"}
	for i, name := range append(stale, fresh...) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		at := s.now.Add(-11 * time.Minute)
		if i >= len(stale) {
			at = s.now.Add(-9 * time.Minute)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	c := s.clientAt(dir)
	s.refresh(c, 7)
	for _, name := range stale {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s, 11 min old, is still there (%v)", name, err)
		}
	}
	for _, name := range fresh {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s, 9 min old, was removed: %v", name, err)
		}
	}
}

// Nothing is created above mirror_dir: when the directory that holds it is
// absent, Refresh fails and creates nothing.
func TestNothingIsCreatedAboveTheMirrorDir(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	gone := filepath.Join(t.TempDir(), "gone")
	dir := filepath.Join(gone, "staging", "acme", "app.git")
	c := s.clientAt(dir)
	if err := c.Refresh(context.Background(), "acme/app", []OpenPR{{Number: 7, Base: "master"}}); err == nil {
		t.Error("Refresh under an absent directory: no error")
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("%s was created (%v)", gone, err)
	}
	if got := one(c, s.pr()); got.State != StateUnknown {
		t.Errorf("%s %q, want unknown", got.State, got.Why)
	}
}

// Data older than 10 minutes (Refreshes keep failing or being cut) is
// unknown, "stale", never an old latest.
func TestOldDataIsStale(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	m := s.push(20*time.Minute, s.p2)
	s.deployed(m)
	c := s.client()
	s.refresh(c, 7)
	s.now = s.now.Add(StaleAfter - time.Second)
	if got := one(c, s.pr()); got.State != StateLatest {
		t.Errorf("before %s: %s, want latest", StaleAfter, got.State)
	}
	s.now = s.now.Add(2 * time.Second)
	if got := one(c, s.pr()); got.State != StateUnknown || !strings.Contains(got.Why, "stale") {
		t.Errorf("after %s: %s %q, want unknown, stale", StaleAfter, got.State, got.Why)
	}
}
