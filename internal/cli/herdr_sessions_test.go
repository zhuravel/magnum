package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The CLI and the herdr sessions: magnum's review panes run in the session
// the registry records (store.KVHerdrPanes, [herdr] review_session once the
// daemon moved them there), while yours ([herdr] socket) stays where the
// plugin runs and the toasts show.

// reviewsSession is a named review session of the act harness's.
func reviewsSession(h *actHarness) herdr.Session {
	return herdr.Session{Name: "reviews", Socket: filepath.Join(h.home, "reviews.sock")}
}

// `magnum open` reveals a client of the session magnum's panes are in, the
// one that shows the PR's agent: the act commands' deps take it from the
// App, where the registry records it, and the focus names it to the reveal.
func TestOpenRevealsTheClientOfThePaneSession(t *testing.T) {
	c := &Context{Version: "test", Layout: paths.Layout{Home: t.TempDir()}, Config: config.Defaults(),
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	recorded := herdr.Session{Name: "reviews", Socket: filepath.Join(c.Layout.Home, "reviews.sock")}
	st := storetest.Open(t, c.Layout.DB())
	raw, _ := json.Marshal(recorded)
	if err := st.SetKV(context.Background(), store.KVHerdrPanes, string(raw)); err != nil {
		t.Fatal(err)
	}
	st.Close()
	d, err := actNewDeps(c, actFull)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if hc, ok := d.Herdr.(*herdr.Client); d.PaneSession != recorded || !ok || hc.Socket != recorded.Socket {
		t.Fatalf("the act commands drive %+v in session %+v, want the recorded %+v", d.Herdr, d.PaneSession, recorded)
	}

	h := newActHarness(t)
	h.d.PaneSession = reviewsSession(h)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	if code := h.cmd("open", "5", "--new-window"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if len(h.reveals) != 1 || h.reveals[0].Session != "reviews" || !h.reveals[0].NewWindow {
		t.Fatalf("reveals %+v, want one of herdr session reviews in a new window", h.reveals)
	}
}

// A herdr that does not answer names the session of magnum's panes, its
// socket and the command that starts it: `herdr --session <name>` for a
// named one, `herdr` for your default session, as before.
func TestHerdrErrNamesThePaneSession(t *testing.T) {
	h := newActHarness(t)
	reviews := reviewsSession(h)
	h.d.PaneSession = reviews
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	h.hd.focusErr = map[string]error{"mg-talkable-5-judge": fmt.Errorf("herdr agent.focus: %w: %w", herdr.ErrUnavailable, syscall.ENOENT)}
	if code := h.cmd("open", "5"); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "herdr session reviews is not running (socket "+reviews.Socket+")",
		"start `herdr --session reviews`, then retry")

	h.errb.Reset()
	h.d.PaneSession = herdr.Session{Name: "default", Socket: h.d.Cfg.Herdr.Socket}
	if code := h.cmd("open", "5"); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "herdr is not running (socket "+h.d.Cfg.Herdr.Socket+"): start herdr, then retry")
	if strings.Contains(h.errb.String(), "--session") {
		t.Fatalf("your default session's fix names a session: %s", h.errb.String())
	}
}
