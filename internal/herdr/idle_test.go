package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleShell(t *testing.T) {
	cases := []struct {
		name string
		pi   ProcessInfo
		want bool
	}{
		{"idle zsh", ProcessInfo{ShellPID: 100, ForegroundPGID: 100, Foreground: []Process{{PID: 100, Name: "zsh"}}}, true},
		{"agent running", ProcessInfo{ShellPID: 100, ForegroundPGID: 200, Foreground: []Process{{PID: 200, Name: "node"}}}, false},
		{"unknown shell", ProcessInfo{ShellPID: 0, ForegroundPGID: 0}, false},
		{"unknown pgid", ProcessInfo{ShellPID: 100}, false},
	}
	for _, tc := range cases {
		if got := IdleShell(tc.pi); got != tc.want {
			t.Errorf("%s: IdleShell = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func processInfoResult(shell, pgid int, name string) map[string]any {
	return map[string]any{"type": "pane_process_info", "process_info": map[string]any{
		"pane_id": "w1:p1", "shell_pid": shell, "foreground_process_group_id": pgid,
		"foreground_processes": []any{map[string]any{"pid": pgid, "name": name, "argv": []string{name}}},
	}}
}

func TestWaitIdleShellPollsUntilIdle(t *testing.T) {
	defer func(d time.Duration) { idlePollInterval = d }(idlePollInterval)
	idlePollInterval = 10 * time.Millisecond

	s := newFakeServer(t)
	var n atomic.Int32
	s.handle("pane.process_info", func(json.RawMessage) (any, *Error) {
		if n.Add(1) < 3 {
			return processInfoResult(100, 200, "git"), nil
		}
		return processInfoResult(100, 100, "zsh"), nil
	})
	pi, err := s.client().WaitIdleShell(context.Background(), "w1:p1", time.Second)
	if err != nil || !IdleShell(pi) || n.Load() != 3 {
		t.Fatalf("WaitIdleShell = %+v, %v after %d polls", pi, err, n.Load())
	}
}

func TestWaitIdleShellTimesOut(t *testing.T) {
	defer func(d time.Duration) { idlePollInterval = d }(idlePollInterval)
	idlePollInterval = 10 * time.Millisecond

	s := newFakeServer(t)
	s.reply("pane.process_info", processInfoResult(100, 200, "git"))
	start := time.Now()
	pi, err := s.client().WaitIdleShell(context.Background(), "w1:p1", 80*time.Millisecond)
	if !IsTimeout(err) {
		t.Fatalf("want timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "git") {
		t.Fatalf("timeout error should name the foreground process: %v", err)
	}
	if pi.ForegroundPGID != 200 {
		t.Fatalf("last process info not returned: %+v", pi)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("took %v", el)
	}
}

func TestWaitIdleShellReturnsServerErrors(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.process_info", func(json.RawMessage) (any, *Error) {
		return nil, &Error{Code: CodePaneNotFound, Message: "pane not found"}
	})
	if _, err := s.client().WaitIdleShell(context.Background(), "w1:p1", time.Second); !IsCode(err, CodePaneNotFound) {
		t.Fatalf("want pane_not_found, got %v", err)
	}
}

// A slow pane.process_info answer must not outlast WaitIdleShell's timeout,
// and an idle answer that arrives after the deadline is not a success.
func TestWaitIdleShellEnforcesTimeoutOnSlowResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		pi   map[string]any
	}{
		{"busy answer too late", processInfoResult(100, 200, "git")},
		{"idle answer too late", processInfoResult(100, 100, "zsh")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeServer(t)
			s.handle("pane.process_info", func(json.RawMessage) (any, *Error) {
				return slow{d: 600 * time.Millisecond, v: tc.pi}, nil
			})
			c := s.client()
			c.Timeout = 5 * time.Second // the client default must not stretch the wait
			start := time.Now()
			pi, err := c.WaitIdleShell(context.Background(), "w1:p1", 100*time.Millisecond)
			if el := time.Since(start); el > 450*time.Millisecond {
				t.Fatalf("WaitIdleShell took %v with a 100ms timeout", el)
			}
			if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "shell not idle after 100ms") {
				t.Fatalf("want the idle-shell ErrTimeout, got %v", err)
			}
			if IdleShell(pi) {
				t.Fatalf("a late answer must not be reported: %+v", pi)
			}
		})
	}
}

func TestWaitIdleShellCallerCancelIsNotATimeout(t *testing.T) {
	defer func(d time.Duration) { idlePollInterval = d }(idlePollInterval)
	idlePollInterval = 10 * time.Millisecond
	s := newFakeServer(t)
	s.reply("pane.process_info", processInfoResult(100, 200, "git"))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(60*time.Millisecond, cancel)
	_, err := s.client().WaitIdleShell(ctx, "w1:p1", 10*time.Second)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
