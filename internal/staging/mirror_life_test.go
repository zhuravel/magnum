package staging

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	s.git.clone = func(context.Context) error { close(entered); <-release; return nil }
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
	s.git.clone = func(context.Context) error {
		return errors.New("fatal: unable to access 'https://github.com/acme/app.git/'")
	}
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

// Close stops a background clone (its context ends, so execx ends git's
// process group) and waits for it; a closed client starts no clone.
func TestCloseStopsTheBackgroundCloneAndWaits(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.git.absent = true
	entered, ended := make(chan struct{}), make(chan error, 1)
	s.git.clone = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		ended <- ctx.Err()
		return ctx.Err()
	}
	c := s.client()
	s.refresh(c, 7)
	<-entered
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	if err := <-ended; !errors.Is(err, context.Canceled) {
		t.Errorf("the clone ended with %v, want its context cancelled", err)
	}
	s.now = s.now.Add(time.Hour)
	_ = c.Refresh(context.Background(), "acme/app", []OpenPR{{Number: 7, Base: "master"}})
	c.bg.Wait()
	if n := s.git.n("Clone"); n != 1 {
		t.Errorf("%d clones, want none after Close", n)
	}
}

// While one client clones, another on the same mirror answers "cloning" at
// once from the state file, instead of waiting for the lock all its ctx.
func TestASecondClientSeesTheCloneAtOnce(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.git.absent = true
	entered, release := make(chan struct{}), make(chan struct{})
	s.git.clone = func(context.Context) error { close(entered); <-release; return nil }
	dir := filepath.Join(t.TempDir(), "acme", "app.git")
	a, b := s.clientAt(dir), s.clientAt(dir)
	s.refresh(a, 7)
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := b.Refresh(ctx, "acme/app", []OpenPR{{Number: 7, Base: "master"}}); err != nil {
		t.Errorf("b during a's clone: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("b waited %s for the lock", took)
	}
	if got := one(b, s.pr()); got.State != StateUnknown || !strings.Contains(got.Why, "cloning") {
		t.Errorf("b: %s %q, want unknown, cloning", got.State, got.Why)
	}
	close(release)
	a.bg.Wait()
}

// A dir that is no mirror is moved aside, and only the newest such dir is
// kept: the older ones are deleted under the flock.
func TestOnlyTheNewestDirMovedAsideIsKept(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	dir := filepath.Join(t.TempDir(), "acme", "app.git")
	older := dir + ".broken-20261101T000000"
	foreign := dir + ".broken-someone-elses"
	for _, d := range []string{older, dir, foreign} {
		if err := os.MkdirAll(filepath.Join(d, "objects"), 0o750); err != nil {
			t.Fatal(err)
		}
		if d != foreign {
			if err := os.WriteFile(filepath.Join(d, markerFile), []byte("acme/app\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.git.absent = true
	c := s.clientAt(dir)
	s.refresh(c, 7)
	c.bg.Wait()
	aside, _ := filepath.Glob(dir + ".broken-2*")
	if len(aside) != 1 || aside[0] == older {
		t.Errorf("dirs moved aside: %q, want only the newest", aside)
	}
	if _, err := os.Stat(filepath.Join(aside[0], "objects")); err != nil {
		t.Errorf("the newest dir moved aside lost its content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(foreign, "objects")); err != nil {
		t.Errorf("a dir beside it without magnum's marker was deleted: %v", err)
	}
}

// A dir at the mirror's path without magnum's marker is not magnum's mirror:
// the Refresh fails, nothing is moved or cloned, and Why says so.
func TestADirWithoutTheMarkerIsNotMagnumsMirror(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	dir := filepath.Join(t.TempDir(), "acme", "app.git")
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o750); err != nil {
		t.Fatal(err)
	}
	s.git.absent = true
	c := s.clientAt(dir)
	for range 2 {
		if err := c.Refresh(context.Background(), "acme/app", []OpenPR{{Number: 7, Base: "master"}}); err == nil || !strings.Contains(err.Error(), "not magnum's mirror") {
			t.Errorf("Refresh: %v, want not magnum's mirror", err)
		}
		c.bg.Wait()
		s.now = s.now.Add(RefreshEvery)
	}
	if aside, _ := filepath.Glob(dir + ".broken-*"); len(aside) != 0 || s.git.n("Clone") != 0 {
		t.Errorf("moved aside %q, %d clones; want neither", aside, s.git.n("Clone"))
	}
	if got := one(c, s.pr()); got.State != StateUnknown || !strings.Contains(got.Why, "not magnum's mirror") {
		t.Errorf("%s %q, want unknown, not magnum's mirror", got.State, got.Why)
	}
}

// A clone that Close cut is no failure: no backoff follows it, so the next
// client clones at once; and a closed client's startClone starts nothing.
func TestAClonedCutByCloseIsNoFailure(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.git.absent = true
	entered := make(chan struct{})
	var once sync.Once
	s.git.clone = func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}
	dir := filepath.Join(t.TempDir(), "acme", "app.git")
	a := s.clientAt(dir)
	s.refresh(a, 7)
	<-entered
	a.Close()
	s.git.clone = nil
	s.now = s.now.Add(30 * time.Second) // inside the first backoff a failure would set
	b := s.clientAt(dir)
	s.refresh(b, 7)
	b.bg.Wait()
	if n := s.git.n("Clone"); n != 2 {
		t.Errorf("%d clones, want the next client's at once, no backoff after a cut", n)
	}

	unlocked := false
	a.startClone(context.Background(), "acme/app", dir, map[string]string{}, func() { unlocked = true }, cloningState(s.now, s.now))
	a.bg.Wait()
	if n := s.git.n("Clone"); n != 2 || !unlocked {
		t.Errorf("a closed client's startClone: %d clones, unlocked %v; want none and the lock released", n, unlocked)
	}
}

// The stale-lock sweep runs only on a dir Base confirmed as the mirror: a dir
// that is none is moved aside as it was, its old locks with it.
func TestTheLockSweepNeverTouchesADirThatIsNoMirror(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	dir := filepath.Join(t.TempDir(), "acme", "app.git")
	lock := filepath.Join(dir, "config.lock")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, markerFile), []byte("acme/app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := s.now.Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	s.git.absent = true
	c := s.clientAt(dir)
	s.refresh(c, 7)
	c.bg.Wait()
	aside, _ := filepath.Glob(dir + ".broken-*")
	if len(aside) != 1 {
		t.Fatalf("dirs moved aside: %q, want one", aside)
	}
	if _, err := os.Stat(filepath.Join(aside[0], "config.lock")); err != nil {
		t.Errorf("the lock of a dir that is no mirror was swept: %v", err)
	}
}

// The tips before a failed merge that have no ci.yml run, first seen over
// 45 minutes ago, are not read again at every Refresh; the run a new failure is
// told against is read without its annotations, which only the job names
// of serve.
func TestTheTipsBeforeAFailedMergeCostLittle(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	m := s.push(2*time.Hour, s.p2)
	s.gh.runs[m] = []Run{run("completed", "failure", Job{Name: "rspec", Paths: []string{"spec/x_spec.rb"}})}
	s.git.paths[s.p2] = []string{"app/a.rb"}
	c := s.client()
	s.refresh(c, 7)
	if got := one(c, s.pr()); got.Detail != DetailCIFailedNotOurs {
		t.Fatalf("%s/%s %q", got.State, got.Detail, got.Why)
	}
	if n := s.gh.n("RunsOf"); n != 1 {
		t.Errorf("%d full run reads, want the merge's only: the tips before it are read without annotations", n)
	}
	if s.gh.n("runsWithoutPaths") == 0 {
		t.Fatal("no tip before the merge was read")
	}
	s.now = s.now.Add(ConfirmWait + time.Minute) // the tips were first seen 46 min ago
	s.refresh(c, 7)
	before := s.gh.n("runsWithoutPaths")
	s.now = s.now.Add(2 * RefreshEvery)
	s.refresh(c, 7)
	if n := s.gh.n("runsWithoutPaths"); n != before {
		t.Errorf("the run-less tips, first seen over 45 min ago, were read again a Refresh later: %d reads, want %d", n, before)
	}
}

// "No run yet" is kept from when a Refresh first saw the tip, never from its
// committer time: a merge made an hour before its push gets its run read
// again at the next Refresh.
func TestNoRunYetCountsFromWhenTheTipWasFirstSeen(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	m := s.push(2*time.Hour, s.p2) // committed two hours ago, seen now
	c := s.client()
	s.refresh(c, 7)
	s.gh.runs[m] = []Run{run("in_progress", "")}
	s.now = s.now.Add(RefreshEvery)
	s.refresh(c, 7)
	if got := one(c, s.pr()); got.Detail != DetailCIRunning || got.RunURL == "" {
		t.Errorf("a minute later: %s/%s %q, want ci_running with its run", got.State, got.Detail, got.Why)
	}
}
