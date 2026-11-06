package cli

// What the CLI says of the herdr sessions: magnum's review panes run in the
// one the registry records (app.App.PaneSession), yours ([herdr] socket)
// shows the toasts, and a move of the panes (engine.PanesMove) holds every
// round until the daemon took them where the config says.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
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

// panesRecorded parses the herdr session the registry records magnum's
// panes in (store.KVHerdrPanes), false for a value that names none (the App
// refuses to open on one). A screen open across a move reads it here: its
// App's clients stay in the session they opened in (app.App.PaneSession).
func panesRecorded(raw string) (herdr.Session, bool) {
	var s herdr.Session
	if json.Unmarshal([]byte(raw), &s) != nil || s.Name == "" || !filepath.IsAbs(s.Socket) {
		return herdr.Session{}, false
	}
	return s, true
}

// panesMoved reports whether the registry records magnum's panes in
// another herdr server than opened, the session a screen's deps opened in:
// the daemon moved them since. False while they are there, the registry
// records none (where the panes are is then the App's), or opened is not
// known.
func panesMoved(ctx context.Context, st *store.Store, opened herdr.Session) bool {
	if st == nil || opened.Socket == "" {
		return false
	}
	v, ok, err := st.GetKV(ctx, store.KVHerdrPanes)
	if err != nil || !ok {
		return false
	}
	s, ok := panesRecorded(v)
	return ok && !herdrSameServer(s, opened)
}
