package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
)

// AcquireLock takes a non-blocking exclusive flock on path (the daemon uses
// layout.Lock(); the CLI takes the same lock before running slot or cleanup
// operations in-process). held reports that another process has it; unlock
// releases it.
func AcquireLock(path string) (unlock func(), held bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, false, nil
}

func writePid(path string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// DaemonPID returns the pid of the running daemon from the pidfile, or 0
// when there is no pidfile or that process is gone. It never touches the
// lock (probing it could make a starting daemon exit as "already running").
func DaemonPID(layout paths.Layout) (int, error) {
	b, err := os.ReadFile(layout.Pid())
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("pidfile %s: bad content %q", layout.Pid(), strings.TrimSpace(string(b)))
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return 0, nil
		}
		if !errors.Is(err, syscall.EPERM) {
			return 0, err
		}
	}
	return pid, nil
}

// ProcessCommand reads what ps knows about pid: comm (`ps -o comm=`, the
// executable as started, its full path when it was started by path) and
// args (`ps -o args=`, the whole command line). LooksLikeDaemon judges them.
func ProcessCommand(ctx context.Context, run execx.Runner, pid int) (comm, args string, err error) {
	ps := func(col string) (string, error) {
		res, err := run.Run(ctx, execx.Cmd{
			Name: "ps", Args: []string{"-p", strconv.Itoa(pid), "-o", col + "="},
			Timeout: 10 * time.Second, Label: "ps daemon pid",
		})
		return strings.TrimSpace(res.Out()), err
	}
	if comm, err = ps("comm"); err != nil {
		return "", "", err
	}
	if args, err = ps("args"); err != nil {
		return "", "", err
	}
	return comm, args, nil
}

// processCommand is ProcessCommand with the real ps; tests replace it.
var processCommand = func(pid int) (comm, args string, err error) {
	return ProcessCommand(context.Background(), &execx.Real{}, pid)
}

// LooksLikeDaemon reports whether a process is a magnum daemon from what
// ProcessCommand read: the executable (comm) is named magnum, the command
// line starts with it (so a path with spaces is one word) and its first
// argument that is not a flag is "daemon" (--config takes a value). An
// editor opening a file named magnum ("/usr/bin/vim /tmp/magnum daemon")
// is not one: its executable is vim.
func LooksLikeDaemon(comm, args string) bool {
	comm, args = strings.TrimSpace(comm), strings.TrimSpace(args)
	if comm == "" || filepath.Base(comm) != "magnum" {
		return false
	}
	rest, ok := strings.CutPrefix(args, comm)
	if ok && rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		ok = false // "/x/magnum-old daemon" does not start with the word "/x/magnum"
	}
	if !ok && !strings.Contains(comm, "/") {
		// A bare (or truncated) comm: the command line's first word must
		// be that executable.
		first, after, _ := strings.Cut(args, " ")
		rest, ok = after, filepath.Base(first) == comm
	}
	if !ok {
		return false
	}
	fields := strings.Fields(rest)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "--config":
			i++ // its value
		case strings.HasPrefix(f, "-"):
		default:
			return f == "daemon"
		}
	}
	return false
}

// KickDaemon asks the running daemon for a tick now (SIGUSR1, `magnum
// kick`); it returns the daemon's pid, or 0 when none runs. The pidfile can
// outlive the daemon (SIGKILL) and its pid be reused, and SIGUSR1 terminates
// a process that does not handle it: the pid is signalled only after ps shows
// a magnum daemon, as `magnum daemon stop` checks.
func KickDaemon(layout paths.Layout) (int, error) {
	pid, err := DaemonPID(layout)
	if err != nil || pid == 0 {
		return 0, err
	}
	comm, args, err := processCommand(pid)
	if err != nil {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return 0, nil // gone meanwhile
		}
		return 0, fmt.Errorf("pid %d in %s: cannot check that it is a magnum daemon (%v); not signalling it", pid, layout.Pid(), err)
	}
	if !LooksLikeDaemon(comm, args) {
		return 0, fmt.Errorf("pid %d in %s is %q, not a magnum daemon; refusing to signal it\nfix: remove the stale pidfile %s",
			pid, layout.Pid(), execx.Redact(args), layout.Pid())
	}
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return 0, nil
		}
		return 0, fmt.Errorf("kick daemon %d: %w", pid, err)
	}
	return pid, nil
}
