package factory

// The factory's own Chrome for a qa stage's walker (R55): headless, its
// profile in the run's folder under a task's directory, never yours, and
// never your running Chrome (a Chrome with a user-data-dir of its own is an
// instance of its own), its DevTools on 127.0.0.1 only. Its pid and start
// are recorded as a server's (serverRecord, under browserKeyPrefix), so a
// stop, or a restart's Reconcile, ends its process group only while both
// match, never by its pid alone, and deletes the profile.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// browserKeyPrefix starts the kv key of every recorded browser. A browser
// has browserBoot to write its DevTools port, read every browserPoll.
const (
	browserKeyPrefix = "factory.browser."
	browserBoot      = 30 * time.Second
	browserPoll      = 100 * time.Millisecond
)

// devToolsActivePort is the file of its profile a Chrome started with
// --remote-debugging-port=0 writes its port to: the port on the first line,
// the browser's path on the second.
const devToolsActivePort = "DevToolsActivePort"

// browserKey is the kv key of copy name's browser.
func browserKey(name string) string { return browserKeyPrefix + name }

// browserLog is the log a copy's browser writes to, in the logs directory.
func (c *copySet) browserLog(name string) string {
	return filepath.Join(c.cfg.Layout.Logs(), "factory-browser-"+name+".log")
}

// browserArgs are the flags the factory's Chrome starts with, its profile
// at profile.
func browserArgs(profile string) []string {
	return []string{"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check", "--disable-extensions",
		"--disable-sync", "--disable-background-networking", "--disable-component-update", "--metrics-recording-only",
		"--password-store=basic", "--use-mock-keychain", "--renderer-process-limit=2", "--window-size=1280,800",
		"--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "--user-data-dir=" + profile, "about:blank"}
}

// StartBrowser starts the factory's headless Chrome for copy cp's QA run,
// its profile in dir/chrome (dir is the run's folder under a task's
// directory: any other is refused), and records its pid and its start. A
// browser an earlier run of the copy left is stopped first. It waits up to
// browserBoot for the DevTools port; a browser that never writes it is
// stopped and forgotten, and the error names its log.
func (c *copySet) StartBrowser(ctx context.Context, cp Copy, dir string) (string, error) {
	if !c.taskFolder(dir) {
		return "", fmt.Errorf("start the factory's Chrome for %s: its profile goes in a QA run's folder under %s, never in %q",
			cp.Name, c.cfg.Layout.FactoryTasks(), dir)
	}
	bin, err := c.browserBinary()
	if err != nil {
		return "", fmt.Errorf("start the factory's Chrome for %s: %w", cp.Name, err)
	}
	c.brw.Lock()
	defer c.brw.Unlock()
	if err := c.stopBrowser(ctx, cp.Name, "a new QA run starts its own"); err != nil {
		return "", err
	}
	profile := filepath.Join(dir, "chrome")
	for _, d := range []string{profile, c.cfg.Layout.Logs()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", fmt.Errorf("start the factory's Chrome for %s: %w", cp.Name, err)
		}
	}
	if err := os.Remove(filepath.Join(profile, devToolsActivePort)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("start the factory's Chrome for %s: %w", cp.Name, err)
	}
	log := c.browserLog(cp.Name)
	c.phase(ctx, cp.Name, kindBrowserStart, store.PhaseBegin, fmt.Sprintf("start the factory's headless Chrome for %s (%s), its profile in %s",
		cp.Name, bin, profile))
	// fail records a start that failed after the spawn: the browser's
	// group is ended (rec; the pid alone when the table could not be read)
	// on its own bound, the context may be the one that ended the wait, and
	// its profile and record go (endBrowser).
	fail := func(err error, rec serverRecord) (string, error) {
		if rec.PID > 0 {
			sctx, cancel := stopContext(ctx)
			if _, serr := c.endBrowser(sctx, cp.Name, rec); serr != nil {
				err = errors.Join(err, fmt.Errorf("and its stop failed: %w", serr))
			}
			cancel()
		}
		c.phase(ctx, cp.Name, kindBrowserStart, store.PhaseFail, err.Error())
		return "", err
	}
	pid, err := c.spawnBrowser(ctx, bin, browserArgs(profile), log)
	if err != nil {
		return fail(fmt.Errorf("start the factory's Chrome for %s: %w", cp.Name, err), serverRecord{})
	}
	table, err := c.processes(ctx)
	if err != nil {
		return fail(fmt.Errorf("start the factory's Chrome for %s: %w", cp.Name, err), serverRecord{PID: pid, Path: dir})
	}
	i := slices.IndexFunc(table, func(p execx.Process) bool { return p.PID == pid && !p.Zombie && p.Start != "" })
	if i < 0 {
		return fail(fmt.Errorf("the factory's Chrome for %s (pid %d) ended at once; its log is %s", cp.Name, pid, log),
			serverRecord{})
	}
	rec := serverRecord{PID: pid, Start: table[i].Start, Path: dir, Since: c.now()}
	if err := c.setRecord(ctx, browserKey(cp.Name), rec); err != nil {
		return fail(fmt.Errorf("record the factory's Chrome for %s: %w", cp.Name, err), rec)
	}
	port, err := c.devToolsPort(ctx, profile)
	if err != nil {
		return fail(fmt.Errorf("the factory's Chrome for %s (pid %d) %w; its log is %s", cp.Name, pid, err, log), rec)
	}
	cdp := "http://127.0.0.1:" + strconv.Itoa(port)
	c.phase(ctx, cp.Name, kindBrowserStart, store.PhaseOK, fmt.Sprintf("the factory's Chrome for %s runs as pid %d, its DevTools on %s; its log is %s",
		cp.Name, pid, cdp, log))
	return cdp, nil
}

// errNoDevToolsPort is a browser that never wrote its DevTools port.
var errNoDevToolsPort = fmt.Errorf("never wrote its DevTools port in %s, and is stopped", browserBoot)

// devToolsPort reads the DevTools port the browser writes to its profile,
// every browserPoll for up to browserBoot: a first line that is a whole
// port.
func (c *copySet) devToolsPort(ctx context.Context, profile string) (int, error) {
	path := filepath.Join(profile, devToolsActivePort)
	for i := 0; ; i++ {
		if b, err := os.ReadFile(path); err == nil {
			if line, _, whole := strings.Cut(string(b), "\n"); whole {
				if port, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && port > 0 && port < 1<<16 {
					return port, nil
				}
			}
		}
		if i >= int(browserBoot/browserPoll) {
			return 0, errNoDevToolsPort
		}
		if err := c.sleep(ctx, browserPoll); err != nil {
			return 0, err
		}
	}
}

// StopBrowser stops copy cp's browser, if the factory started one: its
// process group, while its leader is the recorded one, then its profile,
// then its record, on its own bound (stopContext).
func (c *copySet) StopBrowser(ctx context.Context, cp Copy) error {
	ctx, cancel := stopContext(ctx)
	defer cancel()
	c.brw.Lock()
	defer c.brw.Unlock()
	return c.stopBrowser(ctx, cp.Name, "its QA run ended")
}

// stopBrowser is StopBrowser of copy name, its begin, ok and fail events
// factory.browser_stop's. A copy no record names had no browser the factory
// started: nothing is stopped. The caller holds c.brw.
func (c *copySet) stopBrowser(ctx context.Context, name, why string) error {
	rec, ok, err := c.recordOf(ctx, browserKey(name))
	if err != nil || !ok {
		return err
	}
	what := fmt.Sprintf("the factory's Chrome for %s (pid %d)", name, rec.PID)
	c.phase(ctx, name, kindBrowserStop, store.PhaseBegin, "stop "+what+": "+why)
	ran, err := c.endBrowser(ctx, name, rec)
	if err != nil {
		c.phase(ctx, name, kindBrowserStop, store.PhaseFail, "stop "+what+": "+err.Error())
		return fmt.Errorf("stop %s: %w", what, err)
	}
	done := "stopped " + what
	if !ran {
		done = what + " no longer ran"
	}
	c.phase(ctx, name, kindBrowserStop, store.PhaseOK, done+"; its profile is deleted")
	return nil
}

// endBrowser ends browser rec of copy name while its leader is the
// recorded one (end), deletes its profile and forgets it; ran says it was
// running. A rec with no start is a browser just spawned whose start the
// table could not show: its group is ended by the pid alone. A group that
// outlives SIGKILL keeps its profile and its record.
func (c *copySet) endBrowser(ctx context.Context, name string, rec serverRecord) (ran bool, err error) {
	ran = rec.Start == "" && rec.PID > 0
	if !ran {
		if _, ran, err = c.running(ctx, rec); err != nil {
			return false, err
		}
	}
	if ran {
		if err := c.end(ctx, rec); err != nil {
			return true, err
		}
	}
	if c.taskFolder(rec.Path) { // a record's folder elsewhere is never deleted
		if err := os.RemoveAll(filepath.Join(rec.Path, "chrome")); err != nil {
			return ran, err
		}
	}
	return ran, c.st.DeleteKV(ctx, browserKey(name))
}

// reconcileBrowsers ends the browsers a stopped daemon left: each is a
// stray, ended only while its pid and its start match its record, and
// forgotten with its profile, each on its own bound.
func (c *copySet) reconcileBrowsers(ctx context.Context) []error {
	c.brw.Lock()
	defer c.brw.Unlock()
	recs, err := c.st.KVValues(ctx, browserKeyPrefix+"*")
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, key := range slices.Sorted(maps.Keys(recs)) {
		sctx, cancel := stopContext(ctx)
		if err := c.stopBrowser(sctx, strings.TrimPrefix(key, browserKeyPrefix), "a stray the last daemon left"); err != nil {
			errs = append(errs, err)
		}
		cancel()
	}
	return errs
}

// taskFolder reports whether dir is a folder below the factory's tasks
// directory (a task's, or one of a task's runs), as an absolute path.
func (c *copySet) taskFolder(dir string) bool {
	tasks := c.cfg.Layout.FactoryTasks()
	return filepath.IsAbs(dir) && filepath.Clean(dir) != filepath.Clean(tasks) && inDir(filepath.Clean(dir), tasks)
}

// browserBinary is the Chrome QA starts: [factory] browser, else the first
// of config.DefaultFactoryBrowsers that exists.
func (c *copySet) browserBinary() (string, error) {
	if b := c.cfg.Factory.Browser; b != "" {
		if !c.exists(b) {
			return "", fmt.Errorf("[factory] browser is %s, which does not exist", b)
		}
		return b, nil
	}
	if i := slices.IndexFunc(config.DefaultFactoryBrowsers, c.exists); i >= 0 {
		return config.DefaultFactoryBrowsers[i], nil
	}
	return "", fmt.Errorf("no Chrome for QA: none of %s exists; set [factory] browser to a Chrome or Chromium binary",
		strings.Join(config.DefaultFactoryBrowsers, ", "))
}

// spawnChrome starts the browser bin with args, with the daemon's
// environment less what the copies' commands never inherit (unset), in a
// process group it leads, so a stop ends its helpers too. Its output goes
// to log. It does not wait for the browser, which outlives the call and its
// context; a goroutine reaps it when it ends.
func (c *copySet) spawnChrome(ctx context.Context, bin string, args []string, log string) (int, error) {
	f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), bin, args...)
	unset := c.unset()
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(unset, k)
	})
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return 0, err
	}
	go func() {
		_ = cmd.Wait()
		_ = f.Close()
	}()
	return cmd.Process.Pid, nil
}
