package dblock

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// heldEnv makes the test binary a lock holder (TestHolderProcess).
const heldEnv = "MAGNUM_DBLOCK_HOLD"

// TestHolderProcess is not a test: run with heldEnv set to a lock file, it
// takes the lock, prints "held" and sleeps until it is killed.
func TestHolderProcess(t *testing.T) {
	path := os.Getenv(heldEnv)
	if path == "" {
		t.Skip("the lock holder of TestAKilledHolderFreesTheLock")
	}
	l, err := Acquire(context.Background(), path, Holder{Role: "claude-review", Command: "bin/rspec", PID: os.Getpid(), Start: time.Now()}, 0, nil)
	if err != nil {
		os.Exit(3)
	}
	_ = l
	os.Stdout.WriteString("held\n")
	time.Sleep(time.Minute)
	os.Exit(4)
}

// flock belongs to the open file: a holder killed with SIGKILL, which runs
// no cleanup, frees the lock, and the next role gets it at once.
func TestAKilledHolderFreesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db-locks", "review3-0123456789abcdef.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHolderProcess$")
	cmd.Env = append(os.Environ(), heldEnv+"="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "held\n" {
		t.Fatalf("holder said %q, %v", line, err)
	}
	var timeout *TimeoutError
	if _, err := Acquire(context.Background(), path, Holder{Role: "judge"}, 0, nil); !errors.As(err, &timeout) {
		t.Fatalf("while the holder lives: %v, want a timeout", err)
	} else if timeout.Holder.Role != "claude-review" || timeout.Holder.PID != cmd.Process.Pid {
		t.Errorf("the timeout names %+v, want claude-review with pid %d", timeout.Holder, cmd.Process.Pid)
	}
	if err := cmd.Process.Signal(os.Kill); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	l, err := Acquire(context.Background(), path, Holder{Role: "judge", PID: os.Getpid()}, time.Second, nil)
	if err != nil {
		t.Fatalf("after the holder was killed: %v", err)
	}
	l.Release()
}

// The holder is in the lock file while it holds the lock, for the roles
// that wait, and gone once it released it; a waiter hears who holds the
// lock once, however long it waits.
func TestTheHolderIsWrittenAndAWaiterHearsItOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db-locks", "review3.lock")
	start := time.Date(2026, 10, 7, 14, 2, 11, 0, time.UTC)
	first := Holder{Role: "judge", Command: "bin/rspec spec/models/order_spec.rb", PID: 4242, Start: start}
	l, err := Acquire(context.Background(), path, first, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := ReadHolder(path); !ok || got != first {
		t.Fatalf("the lock file holds %+v (%v), want %+v", got, ok, first)
	}
	var heard []Holder
	got := make(chan error, 1)
	go func() {
		second, err := Acquire(context.Background(), path, Holder{Role: "claude-review", PID: 4343}, 5*time.Second, func(h Holder) { heard = append(heard, h) })
		if err == nil {
			second.Release()
		}
		got <- err
	}()
	time.Sleep(3 * poll)
	l.Release()
	if err := <-got; err != nil {
		t.Fatalf("the second role: %v", err)
	}
	if len(heard) != 1 || heard[0] != first {
		t.Errorf("the waiter heard %+v, want %+v once", heard, first)
	}
	if h, ok := ReadHolder(path); ok {
		t.Errorf("the released lock file still names %+v", h)
	}
}

// A timeout names the holder and how long the role waited, in the words
// db-lock prints.
func TestTimeoutSaysTheCheckDidNotRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review3.lock")
	l, err := Acquire(context.Background(), path, Holder{Role: "judge", Command: "bin/rspec", PID: 1}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	_, err = Acquire(context.Background(), path, Holder{Role: "claude-review"}, 3*poll, nil)
	want := "db-lock: waited " + (3 * poll).String() + " for judge (bin/rspec); the check did not run"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestTimeoutSaysTheWaitAsItWasGiven(t *testing.T) {
	for d, want := range map[time.Duration]string{
		20 * time.Minute: "20m", 90 * time.Minute: "1h30m", time.Hour: "1h", 10 * time.Second: "10s", 300 * time.Millisecond: "300ms",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func TestHolderNames(t *testing.T) {
	for _, tc := range []struct {
		h    Holder
		want string
	}{
		{Holder{Role: "judge", Command: "bin/rspec", PID: 7}, "judge (bin/rspec)"},
		{Holder{Command: "bin/rspec", PID: 7}, "pid " + strconv.Itoa(7) + " (bin/rspec)"},
		{Holder{}, "another command"},
	} {
		if got := tc.h.String(); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.h, got, tc.want)
		}
	}
}
