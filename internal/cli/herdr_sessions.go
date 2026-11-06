package cli

// What the CLI says of the herdr sessions: magnum's review panes run in the
// one the registry records (app.App.PaneSession), yours ([herdr] socket)
// shows the toasts, and a move of the panes (engine.PanesMove) holds every
// round until the daemon took them where the config says.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/herdr"
)

// herdrStart is the command that opens a client of herdr session name,
// which starts its server: `herdr` for the default session.
func herdrStart(name string) string {
	if name == "" || name == "default" {
		return "herdr"
	}
	return "herdr --session " + name
}

// herdrSameServer reports whether two herdr sessions are one server: their
// sockets are one path.
func herdrSameServer(a, b herdr.Session) bool {
	return filepath.Clean(a.Socket) == filepath.Clean(b.Socket)
}

// panesMoveDue says that the config names another herdr session for
// magnum's panes than the one the registry's are in ("" when they are where
// it says, or no session is known): the daemon moves them once it runs that
// config.
func panesMoveDue(panes, configured herdr.Session) string {
	if panes.Socket == "" || herdrSameServer(panes, configured) {
		return ""
	}
	return fmt.Sprintf("magnum's panes are in herdr session %s and [herdr] review_session names %s", panes.Name, configured.Name)
}

// panesMoveWait is what a move of magnum's panes waits on: the PR whose
// park was refused, if any, and why.
func panesMoveWait(mv engine.PanesMove) string {
	if mv.Subject != "" {
		return "waits on " + strings.TrimPrefix(mv.Subject, "pr:") + ": " + mv.Reason
	}
	return "waits: " + mv.Reason
}
