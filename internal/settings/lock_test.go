package settings

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The lock is the config's, not the layout's: two writers that run with
// different MAGNUM_HOME layouts (state directories) but save the same file,
// one of them through a symlinked directory, take the same lock and never
// interleave.
func TestSavesOfOneConfigShareALockWhateverTheirLayout(t *testing.T) {
	storetest.Serial(t)
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "magnum")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(user, []byte(fixture(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(cfgDir, link); err != nil {
		t.Fatal(err)
	}
	a, err := Load(paths.Layout{Home: t.TempDir(), UserConfig: user}, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(paths.Layout{Home: t.TempDir(), UserConfig: filepath.Join(link, "config.toml")}, "")
	if err != nil {
		t.Fatal(err)
	}
	la, _ := lockFile(a.Path)
	lb, _ := lockFile(b.Path)
	if la != lb || filepath.Base(la) != ".config.toml.lock" {
		t.Errorf("lock files %s and %s, want one beside the config", la, lb)
	}
	p1, err := a.Plan(Draft{g("pipeline.fix_cycles"): text("7")}, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := b.Plan(Draft{g("pipeline.fix_cycles"): text("8")}, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	inside, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	commitHook = func(stage string) {
		if stage == "rename" {
			once.Do(func() { close(inside); <-release })
		}
	}
	t.Cleanup(func() { commitHook = nil })
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := Commit(p1); first <- err }()
	<-inside
	go func() { _, err := Commit(p2); second <- err }()
	time.Sleep(200 * time.Millisecond)
	close(release)
	if err := <-first; err != nil {
		t.Errorf("the first save: %v", err)
	}
	if err := <-second; !errors.Is(err, ErrChanged) {
		t.Errorf("the second save = %v, want ErrChanged", err)
	}
	if fi, err := os.Stat(la); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("lock file: %v, %v", fi, err)
	}
}
