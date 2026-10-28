package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// ownChildren reads the pids a test's command wrote into pidFile (on one
// line, once it started) and their identity in the process table, and
// kills in t.Cleanup those that still run as themselves (execx.Find): a
// failed test leaves nothing of its own behind, and nothing else is
// signalled.
func ownChildren(t *testing.T, pidFile string) []execx.Process {
	t.Helper()
	waitForFile(t, pidFile)
	table, err := execx.ProcessTable(context.Background(), &execx.Real{})
	if err != nil {
		t.Fatal(err)
	}
	var kids []execx.Process
	for w := range strings.FieldsSeq(readFile(t, pidFile)) {
		for _, p := range table {
			if strconv.Itoa(p.PID) == w {
				kids = append(kids, p)
			}
		}
	}
	if len(kids) == 0 {
		t.Fatalf("no process of %q in the table", readFile(t, pidFile))
	}
	t.Cleanup(func() {
		now, err := execx.ProcessTable(context.Background(), &execx.Real{})
		if err != nil {
			return
		}
		for _, k := range kids {
			if _, ok := execx.Find(now, k); ok {
				_ = syscall.Kill(k.PID, syscall.SIGKILL)
			}
		}
	})
	return kids
}

// running are those of kids that still run as themselves.
func running(t *testing.T, kids []execx.Process) []execx.Process {
	t.Helper()
	now, err := execx.ProcessTable(context.Background(), &execx.Real{})
	if err != nil {
		t.Fatal(err)
	}
	var out []execx.Process
	for _, k := range kids {
		if _, ok := execx.Find(now, k); ok {
			out = append(out, k)
		}
	}
	return out
}

// goneSoon waits up to 5s until none of kids runs (a killed orphan is
// reaped by launchd a moment later).
func goneSoon(t *testing.T, kids []execx.Process) {
	t.Helper()
	for range 50 {
		if len(running(t, kids)) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("still running: %+v", running(t, kids))
}

// fastKill shortens db-lock's escalation for a test of its later steps.
func fastKill(t *testing.T, after, grace time.Duration) {
	t.Helper()
	prevAfter, prevGrace := dbLockKillAfter, dbLockKillGrace
	dbLockKillAfter, dbLockKillGrace = after, grace
	t.Cleanup(func() { dbLockKillAfter, dbLockKillGrace = prevAfter, prevGrace })
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
	pids := filepath.Join(t.TempDir(), "pids")
	run := startDBLock(t, c, "--checkout", checkout, "--role", "codex-review", "--run", "r-1", "--",
		"sh", "-c", `trap "" INT; sleep 60 & echo $$ $! > "$0.tmp"; mv "$0.tmp" "$0"; wait`, pids)
	kids := ownChildren(t, pids)
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
	goneSoon(t, kids)
}

// A command that ignores SIGTERM too gets SIGKILL dbLockKillGrace later,
// with the processes under it: db-lock says so in a second line and exits
// 128+SIGKILL.
func TestDBLockKillsACommandThatOutlivesSIGTERM(t *testing.T) {
	c, _, _ := bareContext(t)
	fastKill(t, time.Second, time.Second)
	sigs := signalDBLock(t)
	checkout := checkoutDir(t)
	pids := filepath.Join(t.TempDir(), "pids")
	run := startDBLock(t, c, "--checkout", checkout, "--role", "codex-review", "--",
		"sh", "-c", `trap "" INT TERM; sleep 60 & echo $$ $! > "$0.tmp"; mv "$0.tmp" "$0"; wait`, pids)
	kids := ownChildren(t, pids)
	began := time.Now()
	sigs <- os.Interrupt
	code := run.wait(t)
	if code != 128+int(syscall.SIGKILL) {
		t.Errorf("exit %d, want %d (128+SIGKILL); stderr %s", code, 128+int(syscall.SIGKILL), run.err)
	}
	if took := time.Since(began); took < 2*time.Second {
		t.Errorf("db-lock ended %s after the interrupt, want after SIGTERM's grace", took.Round(time.Millisecond))
	}
	lines := strings.Split(strings.TrimSuffix(run.err.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " still runs 1s after SIGINT; sent SIGTERM") ||
		!strings.HasSuffix(lines[1], " still runs 1s after SIGTERM; sent SIGKILL") {
		t.Errorf("stderr = %q, want the SIGTERM line and the SIGKILL line", run.err)
	}
	goneSoon(t, kids)
}

// A command that traps SIGTERM and exits on it is ended all the same, and
// db-lock exits 128 plus the signal it sent, never with the command's own
// status: a cut command never reads as a success.
func TestDBLockExitsWithTheSignalItSent(t *testing.T) {
	c, _, _ := bareContext(t)
	fastKill(t, time.Second, 5*time.Second)
	sigs := signalDBLock(t)
	checkout := checkoutDir(t)
	pids := filepath.Join(t.TempDir(), "pids")
	run := startDBLock(t, c, "--checkout", checkout, "--role", "codex-review", "--",
		"sh", "-c", `trap "" INT; trap 'exit 0' TERM; echo $$ > "$0.tmp"; mv "$0.tmp" "$0"; while :; do sleep 0.1; done`, pids)
	kids := ownChildren(t, pids)
	sigs <- os.Interrupt
	if code := run.wait(t); code != 128+int(syscall.SIGTERM) {
		t.Errorf("exit %d, want %d (128+SIGTERM) for a command that exited 0 on it; stderr %s", code, 128+int(syscall.SIGTERM), run.err)
	}
	goneSoon(t, kids)
}

// codex's sandbox child may outlive codex. With the pane's own descriptors
// (files here, so no pipe keeps db-lock's wait going) the wait ends as soon
// as the command does, and db-lock keeps the lock and its escalation until
// what it captured under the command at the interrupt is gone: a child
// that ignores SIGTERM gets SIGKILL after the grace, and only then is the
// lock free. No survivor works on the databases without the lock.
func TestDBLockHoldsTheLockUntilWhatItCapturedIsGone(t *testing.T) {
	c, _, _ := bareContext(t)
	fastKill(t, time.Second, 2*time.Second)
	sigs := signalDBLock(t)
	checkout := checkoutDir(t)
	dir := t.TempDir()
	out, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	errf, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close(); _ = errf.Close() })
	cc := &Context{Version: c.Version, Layout: c.Layout, Stdout: out, Stderr: errf}
	codes := make(chan int, 1)
	pids := filepath.Join(dir, "pids")
	go func() {
		codes <- execute(cc, []string{"db-lock", "--checkout", checkout, "--role", "codex-review", "--",
			"sh", "-c", `trap "" INT; (trap "" TERM; exec sleep 60) & echo $$ $! > "$0.tmp"; mv "$0.tmp" "$0"; wait`, pids})
	}()
	kids := ownChildren(t, pids)
	if len(kids) != 2 {
		t.Fatalf("children = %+v, want the shell and its child", kids)
	}
	sigs <- os.Interrupt
	for i := 0; ; i++ {
		if _, ok := execx.Find(running(t, kids), kids[0]); !ok {
			break // the command ended on SIGTERM
		}
		if i == 100 {
			t.Fatal("the command never ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := execx.Find(running(t, kids), kids[1]); !ok {
		t.Fatal("the child that ignores SIGTERM is gone before its SIGKILL")
	}
	if h, ok := dblock.ReadHolder(c.Layout.DBLock(checkout)); !ok || h.Role != "codex-review" {
		t.Errorf("the lock is free (%+v, %t) while a child it captured still runs", h, ok)
	}
	select {
	case code := <-codes:
		if code != 128+int(syscall.SIGKILL) {
			t.Errorf("exit %d, want %d (128+SIGKILL)", code, 128+int(syscall.SIGKILL))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("db-lock did not finish")
	}
	goneSoon(t, kids)
	if h, ok := dblock.ReadHolder(c.Layout.DBLock(checkout)); ok {
		t.Errorf("the lock file still names %+v", h)
	}
	if got := readFile(t, filepath.Join(dir, "stderr")); !strings.Contains(got, "; sent SIGTERM\n") || !strings.Contains(got, "; sent SIGKILL\n") {
		t.Errorf("stderr = %q, want the SIGTERM and the SIGKILL lines", got)
	}
}

// db-lock notes its command's identity (pid and start) when it starts it,
// and takes the processes under that pid only from a process table that
// still shows the same command there: once the table shows another start
// under the pid (the command ended and the pid went to another process),
// the children listed under it are another's and get nothing. The victim
// here is the test's own sleep, listed under the command's pid with
// another start.
func TestDBLockSignalsNoChildOfAPIDItsCommandNoLongerHas(t *testing.T) {
	c, _, _ := bareContext(t)
	fastKill(t, time.Second, time.Second)
	sigs := signalDBLock(t)
	checkout := checkoutDir(t)
	victim := exec.Command("sleep", "30")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	victimEnded := make(chan error, 1)
	go func() { victimEnded <- victim.Wait() }()
	t.Cleanup(func() { _ = victim.Process.Kill(); <-victimEnded })
	pids := filepath.Join(t.TempDir(), "pids")
	host := &execx.Real{}
	var mu sync.Mutex
	reads := 0
	prev := dbLockRunner
	dbLockRunner = func() execx.Runner {
		return &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"ps"}, Fn: func(cmd execx.Cmd) (execx.Result, error) {
			res, err := host.Run(context.Background(), cmd)
			mu.Lock()
			reads++
			n := reads
			mu.Unlock()
			b, _ := os.ReadFile(pids)
			root, _, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
			if err != nil || n == 1 || root == "" {
				return res, err // the identity taken at the start is the command's own
			}
			var out strings.Builder
			for line := range strings.SplitSeq(string(res.Stdout), "\n") {
				if f := strings.Fields(line); len(f) > 0 && f[0] == root {
					line = fmt.Sprintf("%s %s %s S Thu Jan  1 00:00:00 2099 /bin/other", f[0], f[1], f[2])
				}
				out.WriteString(line + "\n")
			}
			fmt.Fprintf(&out, "%d %s %d S Thu Jan  1 00:00:01 2099 /bin/sleep\n", victim.Process.Pid, root, victim.Process.Pid)
			return execx.Result{Stdout: []byte(out.String())}, nil
		}}}}
	}
	t.Cleanup(func() { dbLockRunner = prev })
	// Files, not buffers: the command's own child, which db-lock no longer
	// takes for the command's, keeps no pipe open that the wait drains.
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	cc := &Context{Version: c.Version, Layout: c.Layout, Stdout: out, Stderr: out}
	codes := make(chan int, 1)
	go func() {
		codes <- execute(cc, []string{"db-lock", "--checkout", checkout, "--role", "codex-review", "--",
			"sh", "-c", `trap "" INT; sleep 60 & echo $$ $! > "$0.tmp"; mv "$0.tmp" "$0"; wait`, pids})
	}()
	ownChildren(t, pids) // the real table: its own children, killed at the end
	sigs <- os.Interrupt
	select {
	case <-codes:
	case <-time.After(20 * time.Second):
		t.Fatal("db-lock did not finish")
	}
	select {
	case err := <-victimEnded:
		victimEnded <- err // for the cleanup
		t.Errorf("the process listed under the command's reused pid was signalled: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// A command that ends on its interrupt is left to end as it does: no
// SIGTERM follows, and db-lock exits with the command's own status at once.
func TestDBLockSendsNoSIGTERMToACommandThatEndsOnItsInterrupt(t *testing.T) {
	c, _, _ := bareContext(t)
	sigs := signalDBLock(t)
	checkout := checkoutDir(t)
	dir := t.TempDir()
	pids, termed := filepath.Join(dir, "pids"), filepath.Join(dir, "termed")
	run := startDBLock(t, c, "--checkout", checkout, "--role", "codex-review", "--",
		"sh", "-c", `trap 'touch "$1"; exit 9' TERM; trap 'kill $! 2>/dev/null; exit 3' INT; sleep 60 & echo $$ $! > "$0.tmp"; mv "$0.tmp" "$0"; wait`,
		pids, termed)
	ownChildren(t, pids)
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
