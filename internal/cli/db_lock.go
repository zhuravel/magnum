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
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/dblock"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/textx"
)

const dbLockUsage = "db-lock [--checkout DIR] [--timeout 20m] [--role NAME] -- COMMAND [ARG]..."

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
	checkout, role string
	timeout        time.Duration
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
			"holder writes its role, command, pid and start time into it. --checkout defaults to the git work tree of "+
			"the current directory.\n\n"+
			"Exit statuses: COMMAND's own; 75 when the lock stayed held for --timeout (\"db-lock: waited <timeout> for "+
			"<role> (<command>); the check did not run\"): the check did not run, which is no finding; 125 when db-lock "+
			"itself failed and ran nothing; 126 and 127 when COMMAND cannot be started or is not found; 128+n when "+
			"signal n ended it (db-lock passes SIGINT, SIGTERM and SIGHUP on). It reads no config and no registry and "+
			"never talks to the daemon.",
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
	checkout, err := dbLockCheckout(ctx, o.checkout)
	if err != nil {
		return fail("%v", err)
	}
	// The command on one line, as the waiting roles see it.
	me := dblock.Holder{Role: o.role, Command: textx.Clip(strings.Join(strings.Fields(strings.Join(args, " ")), " "), 160), PID: os.Getpid()}
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
	return runLocked(c, args)
}

// dbLockCheckout is dir (else the git work tree of the current directory),
// absolute and with its symlinks resolved, so every role's spelling of one
// checkout names one lock.
func dbLockCheckout(ctx context.Context, dir string) (string, error) {
	if dir == "" {
		res, err := dbLockRunner().Run(ctx, execx.Cmd{Name: "git", Args: []string{"rev-parse", "--show-toplevel"},
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

// runLocked runs args with the role's stdin, stdout and stderr, passing the
// signals db-lock gets on to it, and returns its exit status as a shell
// would.
func runLocked(c *Context, args []string) int {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, c.Stdout, c.Stderr
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(c.Stderr, "db-lock:", execx.Redact(err.Error()))
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return dbLockNotFound
		}
		return dbLockNotExecutable
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	if ps := cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ps.ExitCode()
	}
	fmt.Fprintln(c.Stderr, "db-lock:", execx.Redact(err.Error()))
	return dbLockFailed
}
