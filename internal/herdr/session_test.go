package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// herdr keeps the socket of its default session ("" or "default") at the
// top of its config folder and a named session's in that session's own
// folder. $HERDR_SOCKET_PATH names the session a pane runs in, never the one
// a name means.
func TestSessionSocketFollowsHerdrsLayout(t *testing.T) {
	t.Setenv("HOME", "/home/alice")
	t.Setenv("HERDR_SOCKET_PATH", "/run/herdr/pane.sock")
	for name, want := range map[string]string{
		"":        "/home/alice/.config/herdr/herdr.sock",
		"default": "/home/alice/.config/herdr/herdr.sock",
		"reviews": "/home/alice/.config/herdr/sessions/reviews/herdr.sock",
	} {
		if got := SessionSocket(name); got != want {
			t.Errorf("SessionSocket(%q) = %q, want %q", name, got, want)
		}
	}
}

// A session whose server is not running has either no socket file or a
// stale one that refuses the connection: no server, so no panes. A server
// that answers, even with an error, is one.
func TestNoServerIsAMissingSocketOrARefusedConnection(t *testing.T) {
	dir := shortTempDir(t)
	_, missing := (&Client{Socket: filepath.Join(dir, "missing.sock")}).Ping(context.Background())
	if !NoServer(missing) {
		t.Errorf("a missing socket (%v) is not no server", missing)
	}

	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	_, refused := (&Client{Socket: stale}).Ping(context.Background())
	if !NoServer(refused) {
		t.Errorf("a stale socket that refuses the connection (%v) is not no server", refused)
	}

	s := newFakeServer(t)
	s.handle("ping", func(json.RawMessage) (any, *Error) { return nil, &Error{Code: CodeInvalidRequest, Message: "no"} })
	_, answered := s.client().Ping(context.Background())
	if answered == nil || NoServer(answered) {
		t.Errorf("a server that answers with an error (%v) counts as no server", answered)
	}
	if NoServer(nil) {
		t.Error("no error counts as no server")
	}
	if notHerdr := fmt.Errorf("read config: %w", fs.ErrNotExist); NoServer(notHerdr) {
		t.Errorf("a missing file that is not herdr's socket (%v) counts as no server", notHerdr)
	}

	// A dial that fails for another reason says nothing about the server: a
	// socket path too long for the address (EINVAL), a socket of another user
	// (EACCES).
	long := filepath.Join(dir, strings.Repeat("s", 120)+".sock")
	if _, tooLong := (&Client{Socket: long}).Ping(context.Background()); !errors.Is(tooLong, ErrUnavailable) || NoServer(tooLong) {
		t.Errorf("a dial of a path too long for a socket (%v) counts as no server", tooLong)
	}
	if denied := fmt.Errorf("herdr ping: %w: %w", ErrUnavailable, syscall.EACCES); NoServer(denied) {
		t.Errorf("a dial the socket's permissions refuse (%v) counts as no server", denied)
	}
}

// A server that is running but no longer accepts (a stalled accept loop, a
// stopped process) refuses a dial once its queue of connections is full, as
// a stale socket does (macOS answers ECONNREFUSED); its panes are alive, so
// it is never "no server".
func TestARunningServerThatAcceptsNoConnectionsIsNotNoServer(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "wedged.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() // never accepts
	for range 4096 {
		c, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			break // the queue is full
		}
		c.Close() // an unaccepted connection stays queued after the client closes it
	}
	c := &Client{Socket: path, Timeout: 100 * time.Millisecond}
	_, err = c.Ping(context.Background())
	if err == nil {
		t.Fatal("a server that never accepts answered a ping")
	}
	if NoServer(err) {
		t.Fatalf("a running server that accepts no connections (%v) counts as no server", err)
	}
}

// A server that does not answer in time is running: its panes may be busy,
// so a timeout is an error, never "no server".
func TestATimeoutIsNotNoServer(t *testing.T) {
	s := newFakeServer(t)
	s.handle("ping", func(json.RawMessage) (any, *Error) { return hang{}, nil })
	c := s.client()
	c.Timeout = 50 * time.Millisecond
	_, err := c.Ping(context.Background())
	if !IsTimeout(err) {
		t.Fatalf("want a timeout, got %v", err)
	}
	if NoServer(err) {
		t.Fatalf("a timeout (%v) counts as no server", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Ping(ctx); NoServer(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled call (%v) counts as no server", err)
	}
}
