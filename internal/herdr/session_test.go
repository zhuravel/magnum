package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
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
