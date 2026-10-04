package execx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// shortGrace shortens killGrace (and so WaitDelay) for one test.
func shortGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := killGrace
	killGrace = d
	t.Cleanup(func() { killGrace = old })
}

// firstPID parses the pid a script printed on its first stdout line.
func firstPID(t *testing.T, out []byte) int {
	t.Helper()
	line, _, _ := strings.Cut(string(out), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 1 {
		t.Fatalf("no grandchild pid in output %q", out)
	}
	return pid
}

// waitGone fails unless pid disappears within d. A killed orphan is reaped by
// launchd/init, so this polls instead of checking once.
func waitGone(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d still alive %s after the run ended", pid, d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A timeout must reach the whole process group: the grandchild sleep below is
// not the leader, so killing only the shell would leave it running.
func TestRealTimeoutKillsTheWholeGroup(t *testing.T) {
	r := &Real{}
	res, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "sleep 30 & echo $!; wait"}, Timeout: 300 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
	waitGone(t, firstPID(t, res.Stdout), 3*time.Second)
}

// Cancellation sends SIGTERM first so git can remove its lock files: the
// shell's TERM trap runs, which SIGKILL would never allow.
func TestRealCancelSendsSIGTERMFirst(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "term")
	r := &Real{}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	_, err := r.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", `trap 'echo term > "$M"; exit 0' TERM; sleep 30 & wait`}, Env: map[string]string{"M": marker}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if d := time.Since(start); d > killGrace {
		t.Fatalf("a TERM-handling process took %s to stop", d)
	}
	if b, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(b)) != "term" {
		t.Fatalf("the TERM trap did not run: %q %v", b, err)
	}
}

// A group that ignores SIGTERM is SIGKILLed after the grace period.
func TestRealEscalatesToSIGKILL(t *testing.T) {
	shortGrace(t, 300*time.Millisecond)
	r := &Real{}
	start := time.Now()
	res, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "trap '' TERM; sleep 30 & echo $!; wait"}, Timeout: 200 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("escalation took %s", d)
	}
	waitGone(t, firstPID(t, res.Stdout), 3*time.Second)
}

// A descendant that outlives the leader while holding the output pipe is
// killed once os/exec stops waiting for it, instead of being left behind.
func TestRealKillsDescendantsLeakedPastTheLeader(t *testing.T) {
	shortGrace(t, 300*time.Millisecond)
	r := &Real{}
	res, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "sleep 30 & echo $!"}})
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("want the WaitDelay error, got %v", err)
	}
	waitGone(t, firstPID(t, res.Stdout), 3*time.Second)
}

func TestRealCapsOutput(t *testing.T) {
	r := &Real{}
	n := strconv.Itoa(MaxOutput + 4096)
	res, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "head -c " + n + " /dev/zero; head -c " + n + " /dev/zero >&2"}})
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"stdout": res.Stdout, "stderr": res.Stderr} {
		if len(b) != MaxOutput+len(TruncationMarker) || !strings.HasSuffix(string(b), TruncationMarker) {
			t.Fatalf("%s: len %d, want %d ending in the marker", name, len(b), MaxOutput+len(TruncationMarker))
		}
	}
	if !res.Truncated {
		t.Fatal("Truncated not set")
	}
	small, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo hi"}})
	if err != nil || small.Truncated || small.Out() != "hi" {
		t.Fatalf("small output: %q truncated=%v %v", small.Out(), small.Truncated, err)
	}
}

// NoTTY starts the process in a new session (no controlling terminal: an
// interactive zsh run from a CLI in a terminal then cannot stop on it); a
// timeout still stops the process and its children.
func TestRealNoTTYStartsANewSession(t *testing.T) {
	if a := procAttr(Cmd{NoTTY: true}); !a.Setsid || a.Setpgid {
		t.Fatalf("NoTTY: %+v", a)
	}
	if a := procAttr(Cmd{}); a.Setsid || !a.Setpgid {
		t.Fatalf("default: %+v", a)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 to ask for the session")
	}
	r := &Real{}
	leader := "import os; print('leader' if os.getsid(0) == os.getpid() else 'member')"
	for want, c := range map[string]Cmd{
		"leader": {Name: "python3", Args: []string{"-c", leader}, NoTTY: true},
		"member": {Name: "python3", Args: []string{"-c", leader}},
	} {
		if res, err := r.Run(context.Background(), c); err != nil || res.Out() != want {
			t.Errorf("NoTTY=%v: %q %v, want %s", c.NoTTY, res.Out(), err, want)
		}
	}
	res, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "sleep 30 & echo $!; wait"}, Timeout: 300 * time.Millisecond, NoTTY: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
	waitGone(t, firstPID(t, res.Stdout), 3*time.Second)
}
