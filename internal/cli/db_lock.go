package cli

// `magnum db-lock`: the roles of a review round take turns on the checkout's
// databases (internal/dblock). The roles that run tests get the line in
// their prompts (agents.DBLockLine, `db_lock` in the judge's <magnum>
// block); the command needs the state directory, git for the default
// checkout and nothing else: no config, no registry, no daemon.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/dblock"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/textx"
)

const dbLockUsage = "db-lock [--checkout DIR] [--timeout 20m] [--role NAME] [--run ID] -- COMMAND [ARG]..."

// Exit statuses of db-lock besides the command's own (a shell's 126 and
// 127 for a command it cannot start, 128+n for one a signal ended).
const (
	// dbLockTimedOut (EX_TEMPFAIL): the lock stayed held for --timeout and
	// the command did not run.
	dbLockTimedOut = 75
	// dbLockFailed: db-lock itself failed (its arguments, the checkout, the
	// lock file, an interrupted wait); the command did not run.
	dbLockFailed        = 125
	dbLockNotExecutable = 126
	dbLockNotFound      = 127
)

// dbLockRunner runs git for the default checkout; tests replace it.
var dbLockRunner = func() execx.Runner { return &execx.Real{} }

type dbLockOpts struct {
	checkout, role, run string
	timeout             time.Duration
}

func newDBLockCmd(c *Context) *cobra.Command {
	var o dbLockOpts
	cmd := newCommand(groupAct, dbLockUsage, "run a command under the checkout's database lock (for the review roles)",
		"For the review roles' panes: the judge's own pass and the reviewers run at the same time on one slot, whose "+
			"databases they share. Each of them runs every command that touches the databases (specs, rails runner, rake "+
			"tasks, migrations) through the db_lock line of its prompt, so they take turns: db-lock holds an exclusive lock "+
			"per checkout while COMMAND runs, with the role's stdin, stdout and stderr, and exits with COMMAND's status.\n\n"+
			"A role that finds the lock held prints once \"db-lock: waiting for <role> (<command>) since <time>\" on stderr "+
			"and waits at most --timeout (0: no wait). The lock is a file under magnum's state directory "+
			"(db-locks/<checkout>-<hash>.lock), taken with flock, so a holder that crashes or is killed frees it; the "+
			"holder writes its role, its run, its command, its pid and its start time into it. --checkout defaults to "+
			"the git work tree of the current directory. --run names the run of a shell role's line, magnum's run "+
			"token: the daemon signals a command in a role's pane only when its db-lock line names a run of that PR "+
			"and role. db-lock only records it.\n\n"+
			"Exit statuses: COMMAND's own; 75 when the lock stayed held for --timeout (\"db-lock: waited <timeout> for "+
			"<role> (<command>); the check did not run\"): the check did not run, which is no finding; 125 when db-lock "+
			"itself failed and ran nothing; 126 and 127 when COMMAND cannot be started or is not found; 128+n when "+
			"signal n ended it (db-lock passes SIGINT, SIGTERM and SIGHUP on). A command that still runs 10s after the "+
			"first of those signals gets SIGTERM, and one that still runs 5s later SIGKILL, with every process under it "+
			"(one that left its process group too); db-lock says so on stderr (\"db-lock: <command> still runs 10s "+
			"after SIGINT; sent SIGTERM\") and exits 128 plus the last signal it sent. It reads no config and no "+
			"registry and never talks to the daemon.",
		func(args []string) int { return runDBLock(c, o, args) })
	// Everything from the first word that is not db-lock's own flag is the
	// command, its flags included (`db-lock bin/rspec --fail-fast` works too).
	cmd.Flags().SetInterspersed(false)
	cmd.PersistentPreRunE = func(*cobra.Command, []string) error {
		if c.layoutErr != nil {
			fmt.Fprintln(c.Stderr, "db-lock:", c.layoutErr)
			return exitCode(dbLockFailed)
		}
		return nil
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		fmt.Fprintf(c.Stderr, "db-lock: %v (usage: magnum %s)\n", err, dbLockUsage)
		return exitCode(dbLockFailed)
	})
	fs := cmd.Flags()
	fs.StringVar(&o.checkout, "checkout", "", "the checkout whose databases COMMAND uses (default: the git work tree here)")
	fs.DurationVar(&o.timeout, "timeout", 20*time.Minute, "how long to wait for the lock before giving up (exit 75)")
	fs.StringVar(&o.role, "role", "", "the role running COMMAND, named to the roles that wait")
	fs.StringVar(&o.run, "run", "", "the magnum run COMMAND belongs to (a shell role's line), recorded in the lock file")
	_ = cmd.MarkFlagDirname("checkout")
	return cmd
}

func runDBLock(c *Context, o dbLockOpts, args []string) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintln(c.Stderr, "db-lock:", execx.Redact(fmt.Sprintf(format, a...)))
		return dbLockFailed
	}
	if len(args) == 0 {
		return fail("no command to run (usage: magnum %s)", dbLockUsage)
	}
	ctx, stop := signalContext()
	defer stop()
	checkout, err := gitWorkTree(ctx, dbLockRunner(), o.checkout)
	if err != nil {
		return fail("%v", err)
	}
	// The command on one line, as the waiting roles see it.
	me := dblock.Holder{Role: o.role, Run: o.run, Command: textx.Clip(strings.Join(strings.Fields(strings.Join(args, " ")), " "), 160), PID: os.Getpid()}
	lock, err := dblock.Acquire(ctx, c.Layout.DBLock(checkout), me, o.timeout, func(h dblock.Holder) {
		since := ""
		if !h.Start.IsZero() {
			since = " since " + h.Start.Local().Format("15:04:05")
		}
		fmt.Fprintln(c.Stderr, execx.Redact("db-lock: waiting for "+h.String()+since))
	})
	var timeout *dblock.TimeoutError
	switch {
	case errors.As(err, &timeout):
		fmt.Fprintln(c.Stderr, execx.Redact(timeout.Error()))
		return dbLockTimedOut
	case ctx.Err() != nil:
		return fail("interrupted while waiting for the lock; the check did not run")
	case err != nil:
		return fail("%v", strings.TrimPrefix(err.Error(), "db-lock: "))
	}
	defer lock.Release()
	stop() // from now on the signals go to the command
	return runLocked(c, args, me.Command)
}

// gitWorkTree is dir (else the git work tree of the current directory, as
// r's git names it), absolute and with its symlinks resolved, so every
// role's spelling of one checkout names one lock.
func gitWorkTree(ctx context.Context, r execx.Runner, dir string) (string, error) {
	if dir == "" {
		res, err := r.Run(ctx, execx.Cmd{Name: "git", Args: []string{"rev-parse", "--show-toplevel"},
			Timeout: 30 * time.Second, Label: "git top level", Probe: true})
		if err != nil || res.Out() == "" {
			return "", fmt.Errorf("no --checkout and no git work tree here (%v)", err)
		}
		dir = res.Out()
	}
	abs, err := filepath.Abs(dir)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return "", fmt.Errorf("checkout %s: %w", dir, err)
	}
	return abs, nil
}

// dbLockKillAfter and dbLockKillGrace: a command that still runs
// dbLockKillAfter after db-lock passed it the first signal gets SIGTERM;
// one that still runs dbLockKillGrace after that, SIGKILL.
const (
	dbLockKillAfter = 10 * time.Second
	dbLockKillGrace = 5 * time.Second
)

// dbLockSignals subscribes to the signals db-lock passes on to its command
// (SIGINT, SIGTERM, SIGHUP); stop ends the subscription. Tests replace it.
var dbLockSignals = func() (sigs <-chan os.Signal, stop func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	return c, func() { signal.Stop(c) }
}

// runLocked runs args with the role's stdin, stdout and stderr, passing the
// signals db-lock gets on to it, and returns its exit status as a shell
// would. A command that outlives its interrupt is ended (escalate): codex
// review went on after the restart's three ctrl+c, and the pane's shell
// stayed busy until db-lock exited (DECISIONS "A cut shell command is
// killed, and a review that missed a reviewer is completed"). command
// names it on stderr.
func runLocked(c *Context, args []string, command string) int {
	cmd := exec.Command(args[0], args[1:]...)
	// db-lock's own lines may come while the command writes: a stderr that
	// is no file is copied from a pipe, so both write through one lock (a
	// file, the pane's terminal, stays the command's own).
	stderr := c.Stderr
	if _, ok := stderr.(*os.File); !ok {
		stderr = &lockedWriter{w: stderr}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, c.Stdout, stderr
	sigs, stopSigs := dbLockSignals()
	defer stopSigs()
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(c.Stderr, "db-lock:", execx.Redact(err.Error()))
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return dbLockNotFound
		}
		return dbLockNotExecutable
	}
	done, ended := make(chan struct{}), make(chan syscall.Signal, 1)
	go func() { ended <- escalate(stderr, cmd.Process, command, sigs, done) }()
	err := cmd.Wait()
	close(done)
	if sent := <-ended; sent != 0 {
		return 128 + int(sent)
	}
	if ps := cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ps.ExitCode()
	}
	fmt.Fprintln(c.Stderr, "db-lock:", execx.Redact(err.Error()))
	return dbLockFailed
}

// escalate passes each signal of sigs on to the command (p) until done
// closes. A command that still runs dbLockKillAfter after the first one
// gets SIGTERM, and one that still runs dbLockKillGrace after that SIGKILL,
// each with the processes under it (signalTree), and stderr gets one line
// for each. It returns the last signal it sent on its own (0: none).
func escalate(stderr io.Writer, p *os.Process, command string, sigs <-chan os.Signal, done <-chan struct{}) syscall.Signal {
	var (
		first syscall.Signal
		sent  syscall.Signal
		timer <-chan time.Time
		seen  []execx.Process
	)
	for {
		select {
		case s := <-sigs:
			_ = p.Signal(s)
			if sig, ok := s.(syscall.Signal); ok && first == 0 {
				first, timer = sig, time.After(dbLockKillAfter)
			}
		case <-timer:
			if sent == 0 {
				seen, sent, timer = signalTree(p, syscall.SIGTERM, nil), syscall.SIGTERM, time.After(dbLockKillGrace)
				fmt.Fprintln(stderr, execx.Redact(fmt.Sprintf("db-lock: %s still runs %s after %s; sent SIGTERM", command, dbLockKillAfter, signalName(first))))
				continue
			}
			signalTree(p, syscall.SIGKILL, seen)
			sent, timer = syscall.SIGKILL, nil
			fmt.Fprintln(stderr, execx.Redact(fmt.Sprintf("db-lock: %s still runs %s after SIGTERM; sent SIGKILL", command, dbLockKillGrace)))
		case <-done:
			return sent
		}
	}
}

// lockedWriter serializes the writes to w.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// signalTree sends sig to the command (p) and to every process under it in
// the process table now (execx.Descendants: one in a process group or a
// session of its own, as a sandbox's, too), and to those of earlier (an
// earlier signal's tree) the table still shows under the same name: a
// child whose parent the earlier signal ended is no longer under p. It
// returns the processes it signalled besides p. Never pid 1 nor db-lock.
func signalTree(p *os.Process, sig syscall.Signal, earlier []execx.Process) []execx.Process {
	table, err := execx.ProcessTable(context.Background(), dbLockRunner())
	if err != nil {
		table = nil // the command alone, then
	}
	tree := execx.Descendants(table, p.Pid)
	for _, e := range earlier {
		if slices.ContainsFunc(table, func(t execx.Process) bool { return t.PID == e.PID && t.Name == e.Name }) &&
			!slices.ContainsFunc(tree, func(t execx.Process) bool { return t.PID == e.PID }) {
			tree = append(tree, e)
		}
	}
	_ = p.Signal(sig)
	for _, d := range tree {
		if d.PID > 1 && d.PID != os.Getpid() {
			_ = syscall.Kill(d.PID, sig)
		}
	}
	return tree
}

// signalName is how db-lock's line names a signal it passed on.
func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	}
	return s.String()
}
