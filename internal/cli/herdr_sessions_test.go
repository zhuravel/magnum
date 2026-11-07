package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
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
	st := storetest.OpenFile(t, c.Layout.DB())
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

// A plugin action passes the herdr workspace it ran in, whose id is a
// number of that session's server: each server numbers its own from w1. It
// names a PR only when the plugin ran in the session of magnum's panes
// ($HERDR_SOCKET_PATH is theirs, or unset: the flag typed by hand), else
// the directory decides, and with none the command says why.
func TestAWorkspaceIDFromAnotherHerdrSessionIsNotTrusted(t *testing.T) {
	h := newActHarness(t)
	reviews := reviewsSession(h)
	h.d.PaneSession = reviews
	inReviews := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(inReviews.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	inSlot := h.seedPR("talkable/talkable", 6, store.PRReviewed)
	slotDir := filepath.Join(h.home, "talkable.review3")
	sl, err := h.st.CreateSlot(h.ctx, store.Slot{Name: "review3", RepoFullName: "talkable/talkable", Kind: store.SlotKindPool,
		Path: slotDir, MainClone: h.home, State: store.SlotFree})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionSlot(h.ctx, sl.ID, nil, store.SlotHeld, func(u *store.SlotUpdate) { u.Set("pr_id", inSlot.ID) }); err != nil {
		t.Fatal(err)
	}
	muted := func(env string, args ...string) int {
		t.Helper()
		h.env["HERDR_SOCKET_PATH"] = env
		before := len(h.requests())
		h.errb.Reset()
		if code := h.cmd("mute", args...); code != 0 {
			t.Fatalf("mute %v with HERDR_SOCKET_PATH %q: exit %d: %s", args, env, code, h.errb.String())
		}
		reqs := h.requests()
		if len(reqs) != before+1 {
			t.Fatalf("requests %+v", reqs)
		}
		return actDecode[engine.TargetPayload](t, reqs[len(reqs)-1].Payload).Number
	}

	if n := muted(h.d.Cfg.Herdr.Socket, "--workspace", "w_1", "--cwd", slotDir); n != 6 {
		t.Fatalf("a workspace of your session named #%d, want the directory's #6", n)
	}
	if n := muted(reviews.Socket, "--workspace", "w_1", "--cwd", slotDir); n != 5 {
		t.Fatalf("a workspace of the panes' session named #%d, want its #5", n)
	}
	if n := muted("", "--workspace", "w_1", "--cwd", slotDir); n != 5 {
		t.Fatalf("a workspace given by hand named #%d, want the panes' #5", n)
	}

	h.env["HERDR_SOCKET_PATH"] = h.d.Cfg.Herdr.Socket
	before := len(h.requests())
	h.errb.Reset()
	if code := h.cmd("mute", "--workspace", "w_1"); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "herdr workspace w_1 is in another herdr session than magnum's panes (reviews)")
	if len(h.requests()) != before {
		t.Fatalf("muted a PR by another session's workspace: %+v", h.requests())
	}
}

// `magnum ui open` (the plugin's popups) opens the popup in the herdr
// session the plugin ran in ($HERDR_SOCKET_PATH, which herdr sets), your
// session ([herdr] socket) when it runs outside herdr.
func TestUIOpenOpensThePopupInTheSessionThePluginRanIn(t *testing.T) {
	storetest.Serial(t)
	cfg := config.Defaults()
	home := t.TempDir()
	cfg.Herdr.Socket = filepath.Join(home, "yours.sock")
	cfg.Herdr.ReviewSession, cfg.Herdr.ReviewSocket = "reviews", filepath.Join(home, "reviews.sock")
	c := &Context{Version: "test", Layout: paths.Layout{Home: home}, Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	for _, tc := range []struct {
		env  string
		want herdr.Session
	}{
		{cfg.Herdr.ReviewSocket, herdr.Session{Name: "reviews", Socket: cfg.Herdr.ReviewSocket}},
		{"", cfg.YourHerdr()},
	} {
		t.Setenv("HERDR_SOCKET_PATH", tc.env)
		d, err := actNewDeps(c, actLight)
		if err != nil {
			t.Fatal(err)
		}
		if hc, ok := d.Herdr.(*herdr.Client); !ok || hc.Socket != tc.want.Socket || d.PaneSession != tc.want {
			t.Fatalf("with HERDR_SOCKET_PATH %q the popup opens through %+v in %+v, want %+v", tc.env, d.Herdr, d.PaneSession, tc.want)
		}
	}
}

// With magnum's panes in a session of their own, status counts the working
// agents of both, as max_total_working_codex does: yours with no server
// running has none, and yours that does not answer is said, its agents not
// counted.
func TestStatusCountsWorkingAgentsInBothSessions(t *testing.T) {
	_, _, d, _ := statusFixture(t)
	d.PaneSession = herdr.Session{Name: "reviews", Socket: "/run/herdr/reviews.sock"}
	d.Yours = fakeSnap{snap: herdr.Snapshot{Agents: []herdr.AgentInfo{{Agent: "codex", AgentStatus: herdr.StatusWorking},
		{Agent: "claude", AgentStatus: herdr.StatusWorking}, {Agent: "codex", AgentStatus: herdr.StatusIdle}}}}
	text := func() (statusReport, string) {
		t.Helper()
		r, err := statusGather(context.Background(), d, statusOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		statusRender(&b, r)
		return r, b.String()
	}
	r, out := text()
	if ag := r.Agents; ag == nil || ag.WorkingCodex != 2 || ag.WorkingClaude != 2 {
		t.Fatalf("agents %+v, want the working codex and claude of both sessions", r.Agents)
	}
	actContains(t, out, "working agents: codex 2/5, claude 2")

	d.Yours = fakeSnap{err: fmt.Errorf("herdr session.snapshot: %w: %w", herdr.ErrUnavailable, syscall.ECONNREFUSED)}
	r, out = text()
	if ag := r.Agents; !ag.HerdrUp || ag.WorkingCodex != 1 || ag.YoursError != "" || strings.Contains(out, "unreachable") {
		t.Fatalf("agents %+v with no server of yours:\n%s", r.Agents, out)
	}

	d.Yours = fakeSnap{err: fmt.Errorf("herdr session.snapshot: %w", herdr.ErrTimeout)}
	r, out = text()
	if ag := r.Agents; !ag.HerdrUp || ag.WorkingCodex != 1 || ag.YoursError == "" {
		t.Fatalf("agents %+v with your session not answering", r.Agents)
	}
	actContains(t, out, "working agents: codex 1/5, claude 1", "your herdr session unreachable")
}

// "herdr unreachable" names the session that is down, the one of magnum's
// panes, in the text and on the dashboard.
func TestStatusNamesTheHerdrSessionThatIsDown(t *testing.T) {
	_, _, d, _ := statusFixture(t)
	d.PaneSession = herdr.Session{Name: "reviews", Socket: "/run/herdr/reviews.sock"}
	d.Herdr = fakeSnap{err: fmt.Errorf("herdr session.snapshot: %w: %w", herdr.ErrUnavailable, syscall.ENOENT)}
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	actContains(t, b.String(), "herdr session reviews unreachable")
	if got := statusDashData(r, "").Agents.Error; got != "herdr session reviews unreachable" {
		t.Fatalf("the dashboard says %q", got)
	}
}

// A move of magnum's panes holds new rounds: status shows it among the
// pauses, with what it waits on; one the config asks for that the daemon
// does not run yet is a warning that says how it starts.
func TestStatusShowsAMoveOfThePanes(t *testing.T) {
	_, st, d, now := statusFixture(t)
	from := d.Config.YourHerdr()
	d.PaneSession = from
	d.Config.Herdr.ReviewSession = "reviews"
	pauses := func() (statusReport, string) {
		t.Helper()
		r, err := statusGather(context.Background(), d, statusOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		statusRender(&b, r)
		return r, b.String()
	}
	_, out := pauses()
	actContains(t, out, "moving magnum's panes from herdr session default to reviews", "no new round starts")

	mv := engine.PanesMove{From: from, To: d.Config.ReviewHerdr(), Since: now.Add(-40 * time.Minute),
		Subject: "pr:talkable/talkable#5", Reason: "an agent works in its pane"}
	raw, _ := json.Marshal(mv)
	if err := st.SetKV(context.Background(), engine.KVHerdrPanesMove, string(raw)); err != nil {
		t.Fatal(err)
	}
	_, out = pauses()
	actContains(t, out, "moving magnum's panes from herdr session default to reviews since", "waits on talkable/talkable#5: an agent works in its pane")

	if err := st.DeleteKV(context.Background(), engine.KVHerdrPanesMove); err != nil {
		t.Fatal(err)
	}
	d.DaemonPID = func() (int, error) { return 0, nil }
	r, out := pauses()
	if slices.ContainsFunc(r.Pauses, func(p statusPause) bool { return p.Scope == "herdr" }) {
		t.Fatalf("a move no daemon runs holds rounds: %+v", r.Pauses)
	}
	actContains(t, out, "warning: herdr: magnum's panes are in herdr session default and [herdr] review_session names reviews")
}

// A status opened before the daemon moved magnum's panes reads where the
// registry records them now (store.KVHerdrPanes), not where they were when
// it opened: once they moved no herdr pause holds the rounds, and a warning
// says that its herdr reads are the old session's and to reopen it.
func TestStatusOpenAcrossAMoveOfThePanesReadsWhereTheyAreNow(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	d.PaneSession = d.Config.YourHerdr() // where the panes were when it opened
	d.Config.Herdr.ReviewSession = "reviews"
	raw, _ := json.Marshal(d.Config.ReviewHerdr())
	if err := st.SetKV(context.Background(), store.KVHerdrPanes, string(raw)); err != nil {
		t.Fatal(err)
	}
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(r.Pauses, func(p statusPause) bool { return p.Scope == "herdr" }) {
		t.Fatalf("a move that is done holds the rounds: %+v", r.Pauses)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	actContains(t, b.String(), "warning: herdr: magnum's panes moved to herdr session reviews after this status opened: "+
		"it still reads herdr session default for working agents and panes; reopen it")
	if strings.Contains(b.String(), "review_session names") {
		t.Fatalf("a move that is done is said to be due:\n%s", b.String())
	}
}

// The screens' keys follow a move of magnum's panes made while the screen
// is open: once the registry records another herdr session than the one
// their act deps opened in, the next key opens them again (as app.New does,
// in the session the registry records), and its focus reveals that session.
func TestTheScreensKeysFollowAMoveOfThePanes(t *testing.T) {
	storetest.Serial(t)
	h := newActHarness(t)
	reviews := reviewsSession(h)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	opened := 0
	actNewDeps = func(*Context, actMode) (*actDeps, error) {
		opened++
		d := *h.d
		d.PaneSession = herdr.Session{Name: "default", Socket: h.d.Cfg.Herdr.Socket}
		if v, ok, err := h.st.GetKV(h.ctx, store.KVHerdrPanes); err == nil && ok {
			if err := json.Unmarshal([]byte(v), &d.PaneSession); err != nil {
				t.Error(err)
			}
		}
		return &d, nil
	}
	acts := newScreenActions(h.c)
	defer acts.Close()
	open := func() string {
		t.Helper()
		before := len(h.reveals)
		if _, err := acts.Open(h.ctx, "talkable#5"); err != nil {
			t.Fatal(err)
		}
		if len(h.reveals) != before+1 {
			t.Fatalf("reveals %+v", h.reveals)
		}
		return h.reveals[len(h.reveals)-1].Session
	}
	if got := open(); got != "default" || opened != 1 {
		t.Fatalf("the first key revealed %q after %d opens of the act deps", got, opened)
	}
	raw, _ := json.Marshal(reviews)
	if err := h.st.SetKV(h.ctx, store.KVHerdrPanes, string(raw)); err != nil {
		t.Fatal(err)
	}
	if got := open(); got != "reviews" || opened != 2 {
		t.Fatalf("after the move a key revealed %q after %d opens of the act deps, want reviews after 2", got, opened)
	}
	if got := open(); got != "reviews" || opened != 2 {
		t.Fatalf("with no move since, a key revealed %q after %d opens of the act deps, want reviews after 2", got, opened)
	}
}
