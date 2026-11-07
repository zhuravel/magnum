package factory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

// runDir is a QA run's folder under task id's directory, as the qa stage
// passes it.
func (r *copiesRig) runDir(id int64) string {
	return filepath.Join(r.cfg.Layout.FactoryTaskDir(id), "qa", "run-1")
}

// The factory's Chrome is headless, with its own profile in the run's
// folder (never yours, never attached to your running Chrome), started with
// the pinned flags; its DevTools port is read from the profile, and its pid
// and start are recorded.
func TestStartBrowserRecordsChromeAndReadsItsDevToolsPort(t *testing.T) {
	r := newCopiesRig(t, 1)
	tk := r.task("PR-1")
	c := r.claim(tk)
	dir := r.runDir(tk.ID)

	cdp, err := r.c.StartBrowser(r.ctx, c, dir)
	if err != nil {
		t.Fatalf("StartBrowser: %v", err)
	}
	if cdp != "http://127.0.0.1:9333" {
		t.Fatalf("cdp = %q, want http://127.0.0.1:9333", cdp)
	}
	if len(r.browsers) != 1 {
		t.Fatalf("browsers %+v, want one", r.browsers)
	}
	b := r.browsers[0]
	if b.Bin != config.DefaultFactoryBrowsers[0] {
		t.Errorf("binary %q, want the first default that exists", b.Bin)
	}
	for _, want := range []string{"--headless=new", "--user-data-dir=" + dir + "/chrome", "--window-size=1280,800",
		"--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1", "--use-mock-keychain", "--no-first-run",
		"--password-store=basic", "--disable-sync", "--disable-extensions"} {
		if !slices.Contains(b.Args, want) {
			t.Errorf("args %v have no %s", b.Args, want)
		}
	}
	if last := b.Args[len(b.Args)-1]; last != "about:blank" {
		t.Errorf("the last arg is %q, want about:blank", last)
	}
	if want := filepath.Join(r.cfg.Layout.Logs(), "factory-browser-factory1.log"); b.Log != want {
		t.Errorf("log %q, want %q", b.Log, want)
	}
	rec, ok, err := r.c.recordOf(r.ctx, browserKeyPrefix+c.Name)
	if err != nil || !ok || rec.PID != b.PID || rec.Start != lstart(r.clock.Now()) || rec.Path != dir {
		t.Fatalf("record = %+v, %v, %v; want pid %d, its start and %s", rec, ok, err, b.PID, dir)
	}
	if got := r.stopEvents(c.Name); len(got) != 0 {
		t.Fatalf("server events %v at a browser's start", got)
	}
	if !r.eventsSay(c.Name, "factory.browser_start", "9333") {
		t.Fatal("no factory.browser_start event names the DevTools port")
	}
}

// A DevToolsActivePort a browser of an earlier run left is never read as the
// new one's.
func TestStartBrowserNeverReadsAnEarlierRunsPort(t *testing.T) {
	r := newCopiesRig(t, 1)
	tk := r.task("PR-1")
	c := r.claim(tk)
	dir := r.runDir(tk.ID)
	r.write(filepath.Join(dir, "chrome", "DevToolsActivePort"), "9111\n/devtools/browser/old\n")
	r.devTools = ""
	if cdp, err := r.c.StartBrowser(r.ctx, c, dir); err == nil {
		t.Fatalf("StartBrowser = %q, want an error: the browser never wrote its port", cdp)
	}
}

// With no [factory] browser and no default Chrome on the Mac, the error names
// the key to set.
func TestStartBrowserWithNoChromeNamesTheKey(t *testing.T) {
	r := newCopiesRig(t, 1)
	tk := r.task("PR-1")
	c := r.claim(tk)
	r.browserBins = nil
	_, err := r.c.StartBrowser(r.ctx, c, r.runDir(tk.ID))
	if err == nil || !strings.Contains(err.Error(), "[factory] browser") {
		t.Fatalf("StartBrowser = %v, want it to name [factory] browser", err)
	}
	r.cfg.Factory.Browser = "/Applications/Chromium.app/Contents/MacOS/Chromium"
	_, err = r.c.StartBrowser(r.ctx, c, r.runDir(tk.ID))
	if err == nil || !strings.Contains(err.Error(), "[factory] browser") || !strings.Contains(err.Error(), "Chromium") {
		t.Fatalf("StartBrowser with a missing [factory] browser = %v, want it named", err)
	}
	if len(r.browsers) != 0 {
		t.Fatalf("browsers %+v, want none started", r.browsers)
	}
}

// [factory] browser, when set, is the browser started.
func TestStartBrowserStartsTheConfiguredBrowser(t *testing.T) {
	r := newCopiesRig(t, 1)
	tk := r.task("PR-1")
	c := r.claim(tk)
	r.cfg.Factory.Browser = "/opt/chromium/chrome"
	r.browserBins["/opt/chromium/chrome"] = true
	if _, err := r.c.StartBrowser(r.ctx, c, r.runDir(tk.ID)); err != nil {
		t.Fatal(err)
	}
	if r.browsers[0].Bin != "/opt/chromium/chrome" {
		t.Fatalf("started %q, want [factory] browser", r.browsers[0].Bin)
	}
}

// The factory's Chrome keeps its profile in a task's folder only: a folder
// elsewhere (your own Chrome profile among them) is refused, and nothing
// starts.
func TestStartBrowserRefusesAProfileOutsideTheTasksFolders(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	for _, dir := range []string{filepath.Join(r.root, "Library", "Application Support", "Google", "Chrome"), "qa/run-1",
		r.cfg.Layout.FactoryTasks()} {
		if _, err := r.c.StartBrowser(r.ctx, c, dir); err == nil {
			t.Errorf("StartBrowser in %s = nil, want it refused", dir)
		}
	}
	if len(r.browsers) != 0 {
		t.Fatalf("browsers %+v, want none started", r.browsers)
	}
}

// A browser that never writes its DevTools port within browserBoot has its
// group killed and is forgotten, and the error names its log.
func TestStartBrowserThatNeverListensIsStoppedAndForgotten(t *testing.T) {
	r := newCopiesRig(t, 1)
	tk := r.task("PR-1")
	c := r.claim(tk)
	r.devTools = ""
	var slept time.Duration
	r.c.sleep = func(_ context.Context, d time.Duration) error { slept += d; return nil }

	_, err := r.c.StartBrowser(r.ctx, c, r.runDir(tk.ID))
	log := filepath.Join(r.cfg.Layout.Logs(), "factory-browser-factory1.log")
	if err == nil || !strings.Contains(err.Error(), log) {
		t.Fatalf("StartBrowser = %v, want an error naming %s", err, log)
	}
	if slept < browserBoot {
		t.Errorf("waited %s, want browserBoot (%s)", slept, browserBoot)
	}
	if want := []string{strconv.Itoa(r.browsers[0].PID) + " terminated"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want %v", r.kills, want)
	}
	if _, ok, _ := r.c.recordOf(r.ctx, browserKeyPrefix+c.Name); ok {
		t.Fatal("a browser that never listened is still recorded")
	}
	if !r.eventsSay(c.Name, "factory.browser_start", "never") {
		t.Fatal("no factory.browser_start event says it never listened")
	}
}

// A stop ends the browser's process group, deletes its profile and forgets
// it; a second stop kills nothing.
func TestStopBrowserEndsItsGroupAndDeletesTheProfile(t *testing.T) {
	r := newCopiesRig(t, 1)
	tk := r.task("PR-1")
	c := r.claim(tk)
	dir := r.runDir(tk.ID)
	if _, err := r.c.StartBrowser(r.ctx, c, dir); err != nil {
		t.Fatal(err)
	}
	r.write(filepath.Join(dir, "summary.md"), "the run's own file\n")
	for range 2 {
		if err := r.c.StopBrowser(r.ctx, c); err != nil {
			t.Fatalf("StopBrowser: %v", err)
		}
	}
	if want := []string{strconv.Itoa(r.browsers[0].PID) + " terminated"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want %v", r.kills, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "chrome")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the profile is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "summary.md")); err != nil {
		t.Fatalf("the run's folder lost its own files: %v", err)
	}
	if _, ok, _ := r.c.recordOf(r.ctx, browserKeyPrefix+c.Name); ok {
		t.Fatal("a stopped browser is still recorded")
	}
	if !r.eventsSay(c.Name, "factory.browser_stop", "pid "+strconv.Itoa(r.browsers[0].PID)) {
		t.Fatal("no factory.browser_stop event")
	}
}

// After a restart every recorded browser is a stray: Reconcile kills one
// whose pid and start both match the record, leaves alone a process that
// took a dead browser's pid, and forgets both.
func TestReconcileKillsAStrayBrowserOnlyWhenPidAndStartMatch(t *testing.T) {
	r := newCopiesRig(t, 2)
	t1, t2 := r.task("PR-1"), r.task("PR-2")
	one, two := r.claim(t1), r.claim(t2)
	if _, err := r.c.StartBrowser(r.ctx, one, r.runDir(t1.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.StartBrowser(r.ctx, two, r.runDir(t2.ID)); err != nil {
		t.Fatal(err)
	}
	stray, dead := r.browsers[0].PID, r.browsers[1].PID
	r.mu.Lock()
	for i, p := range r.table {
		if p.PID == dead {
			r.table[i] = execx.Process{PID: dead, PPID: 1, PGID: dead, Start: "Thu Nov  5 10:00:00 2026", Name: "postgres"}
		}
	}
	r.mu.Unlock()

	r.c = r.newCopies()
	if err := r.c.Reconcile(r.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if want := []string{strconv.Itoa(stray) + " terminated"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want only the stray %d (pid %d is another process now)", r.kills, stray, dead)
	}
	for _, c := range []Copy{one, two} {
		if _, ok, _ := r.c.recordOf(r.ctx, browserKeyPrefix+c.Name); ok {
			t.Errorf("%s's browser is still recorded after the reconcile", c.Name)
		}
	}
}
