package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
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
