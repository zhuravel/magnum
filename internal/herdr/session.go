package herdr

import (
	"context"
	"errors"
	"net"
	"syscall"

	"github.com/zhuravel/magnum/internal/paths"
)

// Session is one herdr session: Name is what `herdr --session <name>` takes
// (a client of it, which the reveal opens), Socket the path of its server's
// API socket, which a Client dials.
type Session struct {
	Name   string `json:"name"`
	Socket string `json:"socket"`
}

// SessionSocket is the API socket of the herdr session name in herdr's
// layout: ~/.config/herdr/herdr.sock for its default session ("" or
// "default"), ~/.config/herdr/sessions/<name>/herdr.sock for a named one.
// Unlike DefaultSocket it never reads $HERDR_SOCKET_PATH, which names the
// session a pane runs in, not the one name means.
func SessionSocket(name string) string {
	if name == "" || name == "default" {
		return paths.Expand("~/.config/herdr/herdr.sock")
	}
	return paths.Expand("~/.config/herdr/sessions/" + name + "/herdr.sock")
}

// NoServer reports whether err says that no herdr server runs on the socket:
// the socket file is missing (ENOENT) or stale, refusing the connection
// (ECONNREFUSED) with no socket bound to it. A session without a server has
// no panes, so nothing of it can be busy. A timeout, a permission error or a
// server's own error is not this: the server may be up with panes that are.
// Nor is a server that runs but accepts no connections (a stalled accept
// loop, a stopped process): once its queue is full it refuses them as a
// stale socket does, so a Client's dial asks which it is (socketBound), and
// its error for such a server keeps no ECONNREFUSED.
func NoServer(err error) bool {
	return errors.Is(err, ErrUnavailable) && (errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED))
}

// errNotAccepting is a dial refused by a socket a server is bound to.
var errNotAccepting = errors.New("a server holds the socket but accepts no connections")

// socketBound reports whether a socket is bound at path, for a stream dial
// that path refused. A datagram connect never joins a stream server's queue:
// it fails on the socket's type (EPROTOTYPE) while one is bound, and is
// refused, or finds no file, only when none is. Any other answer counts as
// bound: only those two prove that no server runs.
func socketBound(ctx context.Context, path string) bool {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unixgram", path)
	if err == nil {
		c.Close()
		return true
	}
	return !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENOENT)
}
