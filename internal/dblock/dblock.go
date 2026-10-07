// Package dblock is the lock behind `magnum db-lock`: the roles of a review
// round (the judge's own pass and the reviewers run at the same time) take
// turns on a checkout's databases. One exclusive flock per checkout
// (paths.Layout.DBLock), so a holder that crashes or is killed frees it;
// the holder writes who it is into the file for the roles that wait. It
// reads no config and opens no registry.
package dblock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Holder is who holds a checkout's database lock.
type Holder struct {
	Role    string    `json:"role,omitempty"` // the magnum role, e.g. claude-review ("" = not given)
	Command string    `json:"command"`        // the command it runs under the lock
	PID     int       `json:"pid"`
	Start   time.Time `json:"start"` // when it took the lock
}

// String names the holder as db-lock's messages do: "judge (bin/rspec …)",
// "pid 123 (…)" without a role, "another command" when unknown.
func (h Holder) String() string {
	name := h.Role
	switch {
	case name != "":
	case h.PID > 0:
		name = "pid " + strconv.Itoa(h.PID)
	default:
		return "another command"
	}
	if h.Command == "" {
		return name
	}
	return name + " (" + h.Command + ")"
}

// TimeoutError is Acquire's answer when the lock stayed held for the whole
// wait: the command did not run.
type TimeoutError struct {
	Waited time.Duration
	Holder Holder // who held it at the end ("another command" when unreadable)
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("db-lock: waited %s for %s; the check did not run", shortDuration(e.Waited), e.Holder)
}

// shortDuration is d as --timeout takes it: 20m, 1h30m, 1h, 300ms.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// poll is how often a waiting role tries the lock again.
const poll = 100 * time.Millisecond

// announceAfter is how long a waiter looks for a readable holder before it
// reports an unknown one.
const announceAfter = time.Second

// Lock is a held database lock.
type Lock struct{ f *os.File }

// Acquire takes the exclusive lock at path for me, creating the file (and
// its directory, private) when needed, and writes me into it (a zero
// me.Start becomes the time it took the lock). While another
// process holds it, Acquire tries again every poll until timeout has passed
// (0: one try), then returns a *TimeoutError; ctx ends the wait early.
// waiting, when set, is called once with the holder as soon as the lock is
// found held and the holder readable (after a second, unknown).
func Acquire(ctx context.Context, path string, me Holder, timeout time.Duration, waiting func(Holder)) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("db-lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("db-lock: %w", err)
	}
	began := time.Now()
	deadline := began.Add(max(timeout, 0))
	announced := waiting == nil
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			l := &Lock{f: f}
			if me.Start.IsZero() {
				me.Start = time.Now()
			}
			if err := l.write(me); err != nil {
				l.Release()
				return nil, fmt.Errorf("db-lock: write the holder to %s: %w", path, err)
			}
			return l, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("db-lock: lock %s: %w", path, err)
		}
		if !announced {
			if h, ok := ReadHolder(path); ok || time.Since(began) >= announceAfter {
				waiting(h)
				announced = true
			}
		}
		left := time.Until(deadline)
		if left <= 0 {
			h, _ := ReadHolder(path)
			f.Close()
			return nil, &TimeoutError{Waited: timeout, Holder: h}
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(min(poll, left)):
		}
	}
}

// write replaces the file's content with h.
func (l *Lock) write(h Holder) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	_, err = l.f.WriteAt(append(b, '\n'), 0)
	return err
}

// Release empties the file (no waiter reads a holder that is gone) and
// releases the lock. A nil or released Lock is a no-op.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = l.f.Truncate(0)
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

// ReadHolder reads the holder written into the lock file at path; ok is
// false for an empty, partly written or missing file.
func ReadHolder(path string) (h Holder, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &h) != nil || h.PID <= 0 {
		return Holder{}, false
	}
	return h, true
}
