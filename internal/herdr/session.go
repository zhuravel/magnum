package herdr

import (
	"errors"
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
// (ECONNREFUSED). A session without a server has no panes, so nothing of
// it can be busy. A timeout, a permission error or a server's own error is
// not this: the server may be up with panes that are.
func NoServer(err error) bool {
	return errors.Is(err, ErrUnavailable) && (errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED))
}
