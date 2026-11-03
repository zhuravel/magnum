package factory

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
)

// fakeProcesses is a process table: what ps would say about each pid, and
// the signals that were sent. It touches no real process.
type fakeProcesses struct {
	table   map[int][2]string // pid -> comm, args
	psErr   error
	signals []signal
}

type signal struct {
	pid int
	sig syscall.Signal
}

func (f *fakeProcesses) processes() engine.Processes {
	return engine.Processes{
		Command: func(pid int) (string, string, error) {
			if f.psErr != nil {
				return "", "", f.psErr
			}
			p, ok := f.table[pid]
			if !ok {
				return "", "", errors.New("ps: no such process")
			}
			return p[0], p[1], nil
		},
		Kill: func(pid int, sig syscall.Signal) error {
			if _, ok := f.table[pid]; !ok {
				return syscall.ESRCH
			}
			if sig != 0 { // signal 0 only asks whether the process exists
				f.signals = append(f.signals, signal{pid, sig})
			}
			return nil
		},
	}
}

func layoutWithPid(t *testing.T, pid int) paths.Layout {
	t.Helper()
	l := paths.Layout{Home: t.TempDir()}
	if err := os.MkdirAll(l.State(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.FactoryPid(), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

// SIGUSR1 terminates a process that does not handle it, and a pidfile can
// outlive its daemon and name a recycled pid: the review daemon's pid in the
// factory's pidfile, or any other process, is never signalled.
func TestKickSignalsOnlyAFactoryDaemon(t *testing.T) {
	const bin = "/Users/example/Projects/magnum/bin/magnum"
	cases := []struct {
		name       string
		comm, args string
		wantSignal bool
	}{
		{"the factory daemon", bin, bin + " factory daemon", true},
		{"the factory daemon with a flag", bin, bin + " factory daemon --once", true},
		{"--config before the subcommands", "magnum", "magnum --config /x/config.toml factory daemon", true},
		{"the review daemon", bin, bin + " daemon", false},
		{"another factory subcommand", bin, bin + " factory status", false},
		{"factory without a subcommand", bin, bin + " factory", false},
		{"daemon as a later argument", bin, bin + " factory logs daemon", false},
		{"an editor on a file named like the daemon", "/usr/bin/vim", "/usr/bin/vim magnum factory daemon", false},
		{"another executable", "/usr/bin/vim", "/usr/bin/vim notes.txt", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const pid = 4242
			procs := &fakeProcesses{table: map[int][2]string{pid: {tc.comm, tc.args}}}
			l := layoutWithPid(t, pid)
			got, err := KickWith(l, procs.processes())
			if tc.wantSignal {
				if err != nil || got != pid {
					t.Fatalf("kick: pid=%d err=%v, want pid %d", got, err, pid)
				}
				if len(procs.signals) != 1 || procs.signals[0] != (signal{pid, syscall.SIGUSR1}) {
					t.Fatalf("signals %v, want one SIGUSR1 to %d", procs.signals, pid)
				}
				return
			}
			if err == nil || got != 0 || !strings.Contains(err.Error(), "not a magnum factory daemon") {
				t.Fatalf("kick of %q: pid=%d err=%v, want a refusal", tc.args, got, err)
			}
			if !strings.Contains(err.Error(), l.FactoryPid()) {
				t.Errorf("the refusal does not name the pidfile %s: %v", l.FactoryPid(), err)
			}
			if len(procs.signals) != 0 {
				t.Fatalf("signalled a process that is not the factory daemon: %v", procs.signals)
			}
		})
	}
}

func TestKickWithoutAFactoryDaemon(t *testing.T) {
	t.Run("no pidfile", func(t *testing.T) {
		procs := &fakeProcesses{}
		l := paths.Layout{Home: t.TempDir()}
		if pid, err := KickWith(l, procs.processes()); err != nil || pid != 0 {
			t.Fatalf("pid=%d err=%v, want 0 and no error", pid, err)
		}
	})
	t.Run("a pid that is gone", func(t *testing.T) {
		procs := &fakeProcesses{}
		l := layoutWithPid(t, 4242)
		if pid, err := KickWith(l, procs.processes()); err != nil || pid != 0 {
			t.Fatalf("pid=%d err=%v, want 0 and no error", pid, err)
		}
		if pid, err := DaemonPIDWith(l, procs.processes()); err != nil || pid != 0 {
			t.Fatalf("DaemonPID: pid=%d err=%v, want 0", pid, err)
		}
	})
	t.Run("ps cannot say what a live pid is", func(t *testing.T) {
		procs := &fakeProcesses{table: map[int][2]string{4242: {"", ""}}, psErr: errors.New("ps: boom")}
		l := layoutWithPid(t, 4242)
		pid, err := KickWith(l, procs.processes())
		if err == nil || pid != 0 || !strings.Contains(err.Error(), "not signalling it") {
			t.Fatalf("pid=%d err=%v, want a refusal that does not signal", pid, err)
		}
		if len(procs.signals) != 0 {
			t.Fatalf("signalled %v", procs.signals)
		}
	})
	t.Run("a bad pidfile", func(t *testing.T) {
		procs := &fakeProcesses{}
		l := layoutWithPid(t, 1)
		if err := os.WriteFile(l.FactoryPid(), []byte("not a pid\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if pid, err := KickWith(l, procs.processes()); err == nil || pid != 0 {
			t.Fatalf("pid=%d err=%v, want an error", pid, err)
		}
	})
}

// The factory reads its own pidfile, not the review daemon's: both daemons
// can run at once.
func TestDaemonPIDReadsTheFactoryPidfile(t *testing.T) {
	procs := &fakeProcesses{table: map[int][2]string{4242: {"magnum", "magnum factory daemon"}, 7: {"magnum", "magnum daemon"}}}
	l := layoutWithPid(t, 4242)
	if err := os.WriteFile(l.Pid(), []byte("7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, err := DaemonPIDWith(l, procs.processes()); err != nil || pid != 4242 {
		t.Fatalf("pid=%d err=%v, want 4242", pid, err)
	}
	if filepath.Base(l.FactoryPid()) == filepath.Base(l.Pid()) {
		t.Fatal("the two pidfiles share a name")
	}
}
