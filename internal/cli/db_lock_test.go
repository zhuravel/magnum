package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/dblock"
	"github.com/zhuravel/magnum/internal/execx"
)

// dbLockRun is one `magnum db-lock` run in its own goroutine, as a role's
// pane would start it.
type dbLockRun struct {
	code     chan int
	out, err *bytes.Buffer
}

func startDBLock(t *testing.T, c *Context, args ...string) dbLockRun {
	t.Helper()
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	cc := &Context{Version: c.Version, Layout: c.Layout, Stdout: out, Stderr: errb}
	r := dbLockRun{code: make(chan int, 1), out: out, err: errb}
	go func() { r.code <- execute(cc, append([]string{"db-lock"}, args...)) }()
	return r
}

func (r dbLockRun) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-r.code:
		return code
	case <-time.After(20 * time.Second):
		t.Fatal("db-lock did not finish")
		return 0
	}
}

// waitForHolder waits until role holds checkout's lock.
func waitForHolder(t *testing.T, c *Context, checkout, role string) {
	t.Helper()
	for range 400 {
		if h, ok := dblock.ReadHolder(c.Layout.DBLock(checkout)); ok && h.Role == role {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never took the lock of %s", role, checkout)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// The own pass and the reviewers run specs on the slot's databases at the
// same time (deadlocks, duplicate fixture keys): through db-lock, a second
// role's command starts only after the first one's ended, and the second
// role is told whom it waits for.
func TestDBLockRunsTheRolesOneAfterTheOther(t *testing.T) {
	c, _, _ := bareContext(t)
	checkout, log := checkoutDir(t), filepath.Join(t.TempDir(), "log")
	first := startDBLock(t, c, "--checkout", checkout, "--role", "judge", "--",
		"sh", "-c", `echo judge-start >> "$0"; sleep 0.4; echo judge-end >> "$0"`, log)
	waitForHolder(t, c, checkout, "judge")
	second := startDBLock(t, c, "--checkout", checkout, "--role", "claude-review", "--",
		"sh", "-c", `echo review-start >> "$0"; echo review-end >> "$0"`, log)
	if code := first.wait(t); code != 0 {
		t.Fatalf("judge: exit %d: %s", code, first.err)
	}
	if code := second.wait(t); code != 0 {
		t.Fatalf("claude-review: exit %d: %s", code, second.err)
	}
	if got := readFile(t, log); got != "judge-start\njudge-end\nreview-start\nreview-end\n" {
		t.Errorf("the commands ran interleaved:\n%s", got)
	}
	if got := second.err.String(); strings.Count(got, "db-lock: waiting for judge (sh -c ") != 1 || !strings.Contains(got, ") since ") {
		t.Errorf("the second role's stderr = %q, want the waiting line naming the judge once", got)
	}
	if got := first.err.String(); got != "" {
		t.Errorf("the first role's stderr = %q, want nothing", got)
	}
}

// Each slot has its own databases: roles on different checkouts never wait
// for each other.
func TestDBLockRunsDifferentCheckoutsAtOnce(t *testing.T) {
	c, _, _ := bareContext(t)
	one, two := checkoutDir(t), checkoutDir(t)
	done := filepath.Join(t.TempDir(), "two-ran")
	// The first command ends only once the second ran (or fails after 10s).
	first := startDBLock(t, c, "--checkout", one, "--role", "judge", "--",
		"sh", "-c", `i=0; until [ -e "$0" ] || [ $i -ge 200 ]; do sleep 0.05; i=$((i+1)); done; [ -e "$0" ]`, done)
	waitForHolder(t, c, one, "judge")
	second := startDBLock(t, c, "--checkout", two, "--role", "claude-review", "--timeout", "5s", "--", "touch", done)
	if code := second.wait(t); code != 0 {
		t.Fatalf("the other checkout: exit %d: %s", code, second.err)
	}
	if code := first.wait(t); code != 0 {
		t.Fatalf("the first checkout: exit %d: %s", code, first.err)
	}
	if second.err.Len() != 0 {
		t.Errorf("the other checkout waited: %s", second.err)
	}
}

// db-lock is transparent: the command's output and exit status are the
// role's, and the lock is free again afterwards.
func TestDBLockPassesTheCommandsStatusAndOutputThrough(t *testing.T) {
	c, out, errb := bareContext(t)
	checkout := checkoutDir(t)
	if code := execute(c, []string{"db-lock", "--checkout", checkout, "--", "sh", "-c", "echo 42 examples; echo deadlock >&2; exit 7"}); code != 7 {
		t.Fatalf("exit %d, want the command's 7; stderr %s", code, errb)
	}
	if out.String() != "42 examples\n" || errb.String() != "deadlock\n" {
		t.Errorf("stdout %q, stderr %q", out, errb)
	}
	if h, ok := dblock.ReadHolder(c.Layout.DBLock(checkout)); ok {
		t.Errorf("the lock file still names %+v", h)
	}
	if code := execute(c, []string{"db-lock", "--checkout", checkout, "--timeout", "0s", "--", "true"}); code != 0 {
		t.Errorf("the next run: exit %d, want the lock free", code)
	}
	if code := execute(c, []string{"db-lock", "--checkout", checkout, "--", "magnum-no-such-command"}); code != 127 {
		t.Errorf("an unknown command: exit %d, want 127", code)
	}
}

// A role that waited --timeout gets db-lock's own status, which no check
// result can be mistaken for, and the words that say its check did not run.
func TestDBLockTimesOutWithItsOwnStatus(t *testing.T) {
	c, _, errb := bareContext(t)
	checkout := checkoutDir(t)
	held, err := dblock.Acquire(context.Background(), c.Layout.DBLock(checkout),
		dblock.Holder{Role: "judge", Command: "bin/rspec spec/models/order_spec.rb", PID: os.Getpid(), Start: time.Now()}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	ran := filepath.Join(t.TempDir(), "ran")
	code := execute(c, []string{"db-lock", "--checkout", checkout, "--role", "claude-review", "--timeout", "300ms", "--", "touch", ran})
	if code != dbLockTimedOut || dbLockTimedOut != 75 {
		t.Fatalf("exit %d, want %d (75)", code, dbLockTimedOut)
	}
	if !strings.Contains(errb.String(), "db-lock: waited 300ms for judge (bin/rspec spec/models/order_spec.rb); the check did not run\n") {
		t.Errorf("stderr = %q", errb)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Error("the command ran without the lock")
	}
}

// Without --checkout the lock is the one of the git work tree the role's
// pane is in.
func TestDBLockDefaultsToTheGitTopLevel(t *testing.T) {
	c, out, errb := bareContext(t)
	checkout := t.TempDir()
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"git", "rev-parse", "--show-toplevel"},
		Result: execx.Result{Stdout: []byte(checkout + "\n")}}}}
	prev := dbLockRunner
	dbLockRunner = func() execx.Runner { return f }
	t.Cleanup(func() { dbLockRunner = prev })
	lock := c.Layout.DBLock(resolvedDir(t, checkout))
	if code := execute(c, []string{"db-lock", "--role", "claude-review", "--", "cat", lock}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out.String(), `"role":"claude-review"`) {
		t.Errorf("the command did not run under the work tree's lock %s: it read %q", lock, out)
	}
}

// db-lock's own failures are not a check's result either: no command, a bad
// flag or a checkout that cannot be found exit 125 and run nothing.
func TestDBLockRefusesWithoutACommand(t *testing.T) {
	c, _, errb := bareContext(t)
	for _, args := range [][]string{
		{"db-lock", "--checkout", t.TempDir()},
		{"db-lock", "--timeout", "soon", "--", "true"},
		{"db-lock", "--checkout", filepath.Join(t.TempDir(), "gone"), "--", "true"},
	} {
		errb.Reset()
		if code := execute(c, args); code != dbLockFailed || dbLockFailed != 125 {
			t.Errorf("%q: exit %d, want %d (125); stderr %s", args, code, dbLockFailed, errb)
		}
		if !strings.HasPrefix(errb.String(), "db-lock: ") {
			t.Errorf("%q: stderr %q", args, errb)
		}
	}
}

// The line a role's prompt renders (agents.DBLockLine) is a command this
// build runs: the checkout and the role arrive intact, quoting and all, and
// the role's command follows it.
func TestDBLockRunsTheLineAPromptRenders(t *testing.T) {
	c, out, errb := bareContext(t)
	checkout := filepath.Join(t.TempDir(), "talkable review3")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	line, err := agents.RenderPrompt(config.Prompt{Name: "line", Text: "{{.DBLockCommand}}"},
		agents.RoleData{Magnum: "/opt/magnum/bin/magnum", Checkout: checkout, Role: "claude-review"})
	if err != nil {
		t.Fatal(err)
	}
	argv := shellWords(t, line)
	if argv[0] != "/opt/magnum/bin/magnum" || argv[len(argv)-1] != "--" {
		t.Fatalf("argv = %q", argv)
	}
	lock := c.Layout.DBLock(resolvedDir(t, checkout))
	if code := execute(c, append(argv[1:], "cat", lock)); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out.String(), `"role":"claude-review"`) || strings.Contains(out.String(), `"start":"0001-01-01`) {
		t.Errorf("the command did not run under the checkout's lock %s with its holder and start: it read %q", lock, out)
	}
}

// checkoutDir is a new directory as db-lock names it: absolute, its
// symlinks resolved (on macOS the temp directory is under a link).
func checkoutDir(t *testing.T) string { return resolvedDir(t, t.TempDir()) }

func resolvedDir(t *testing.T, dir string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// signalDBLock makes the signals db-lock passes on come from the channel it
// returns instead of the test process's own (dbLockSignals), so a test
// interrupts one db-lock run without signalling itself.
func signalDBLock(t *testing.T) chan<- os.Signal {
	t.Helper()
	sigs := make(chan os.Signal, 1)
	prev := dbLockSignals
	dbLockSignals = func() (<-chan os.Signal, func()) { return sigs, func() {} }
	t.Cleanup(func() { dbLockSignals = prev })
	return sigs
}

// waitForFile waits until path exists: the command db-lock runs started.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	for range 1000 {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// codex review went on after its ctrl+c (12 codex-review runs ended busy
// from 2026-10-16 to 2026-10-28: the restart's three ctrl+c, 10 seconds
// apart, did not end it): db-lock passes the interrupt on, and a command
// that still runs dbLockKillAfter later gets SIGTERM, with its descendants
// (the sleep here holds db-lock's output open, so db-lock's wait ends only
// once it is gone too). db-lock says so in one line on stderr and exits
// 128+SIGTERM, so the pane's shell is idle again within 16 seconds of the
// interrupt.
func TestDBLockKillsACommandThatTrapsSIGINT(t *testing.T) {
	c, _, _ := bareContext(t)
	sigs := signalDBLock(t)
	checkout := checkoutDir(t)
	started := filepath.Join(t.TempDir(), "started")
	run := startDBLock(t, c, "--checkout", checkout, "--role", "codex-review", "--run", "r-1", "--",
		"sh", "-c", `trap "" INT; touch "$0"; sleep 60`, started)
	waitForFile(t, started)
	began := time.Now()
	sigs <- os.Interrupt
	code := run.wait(t)
	took := time.Since(began)
	if code != 128+int(syscall.SIGTERM) {
		t.Errorf("exit %d, want %d (128+SIGTERM); stderr %s", code, 128+int(syscall.SIGTERM), run.err)
	}
	if took < dbLockKillAfter || took > 16*time.Second {
		t.Errorf("db-lock ended %s after the interrupt, want between %s and 16s", took.Round(time.Millisecond), dbLockKillAfter)
	}
	if got := run.err.String(); strings.Count(got, "\n") != 1 || !strings.HasPrefix(got, "db-lock: sh -c trap ") ||
		!strings.HasSuffix(got, " still runs 10s after SIGINT; sent SIGTERM\n") {
		t.Errorf("stderr = %q, want the one line that says db-lock sent SIGTERM", got)
	}
	if h, ok := dblock.ReadHolder(c.Layout.DBLock(checkout)); ok {
		t.Errorf("the lock file still names %+v", h)
	}
}

// A command that ends on its interrupt is left to end as it does: no
// SIGTERM follows, and db-lock exits with the command's own status at once.
func TestDBLockSendsNoSIGTERMToACommandThatEndsOnItsInterrupt(t *testing.T) {
	c, _, _ := bareContext(t)
	sigs := signalDBLock(t)
	checkout := checkoutDir(t)
	dir := t.TempDir()
	started, termed := filepath.Join(dir, "started"), filepath.Join(dir, "termed")
	run := startDBLock(t, c, "--checkout", checkout, "--role", "codex-review", "--",
		"sh", "-c", `trap 'touch "$1"; exit 9' TERM; trap 'kill $! 2>/dev/null; exit 3' INT; touch "$0"; sleep 60 & wait`,
		started, termed)
	waitForFile(t, started)
	began := time.Now()
	sigs <- os.Interrupt
	if code := run.wait(t); code != 3 {
		t.Errorf("exit %d, want the command's 3; stderr %s", code, run.err)
	}
	if took := time.Since(began); took >= dbLockKillAfter {
		t.Errorf("db-lock ended %s after the interrupt, want at once", took.Round(time.Millisecond))
	}
	if run.err.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", run.err)
	}
	if _, err := os.Stat(termed); !os.IsNotExist(err) {
		t.Error("the command got SIGTERM after it ended on its interrupt")
	}
}

// A shell role's line names its run (--run, the run token magnum's
// StopShell looks for): db-lock records it in its holder and does nothing
// else with it.
func TestDBLockRecordsTheRunInItsHolder(t *testing.T) {
	c, out, errb := bareContext(t)
	checkout := checkoutDir(t)
	lock := c.Layout.DBLock(checkout)
	if code := execute(c, []string{"db-lock", "--checkout", checkout, "--role", "codex-review", "--run", "r-20261028T101500-3", "--",
		"cat", lock}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out.String(), `"role":"codex-review"`) || !strings.Contains(out.String(), `"run":"r-20261028T101500-3"`) {
		t.Errorf("the holder = %q, want the role and the run", out)
	}
}
