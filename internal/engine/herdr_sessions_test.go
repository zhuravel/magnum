package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The engine with magnum's review panes in a herdr session other than
// yours: the panes' session gets the workspaces' sidebar tokens and is the
// one revealed, and yours still counts toward the Codex cap.

// appEngine wires an engine with FromApp to an App on a registry of its own,
// whose herdr ids, for a non-nil panes, the registry records as that
// session's (store.KVHerdrPanes); your session is on home/yours.sock.
// Nothing listens on either socket. edit, when set, changes the loaded
// config.
func appEngine(t *testing.T, panes *herdr.Session, opts app.Options, edit func(*config.Config), mods ...func(*Deps)) (*Engine, *app.App, string) {
	t.Helper()
	storetest.Parallel(t)
	home := t.TempDir()
	cfgText := strings.ReplaceAll(testConfigTOML, "HOME", home)
	cfgText = strings.Replace(cfgText, "[herdr]\n", "[herdr]\nsocket = \""+filepath.Join(home, "yours.sock")+"\"\n", 1)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := paths.Layout{Home: home}
	cfg, err := config.Load(layout, filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(cfg)
	}
	st := storetest.Open(t, layout.DB())
	if panes != nil {
		b, _ := json.Marshal(panes)
		if err := st.SetKV(context.Background(), store.KVHerdrPanes, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	if opts.Runner == nil {
		opts.Runner = &execx.Fake{}
	}
	opts.Logger = slog.New(slog.DiscardHandler)
	opts.MySQLDSN = "root:@tcp(127.0.0.1:1)/?timeout=1s"
	a, err := app.New(cfg, layout, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return FromApp(a, mods...), a, home
}

// The sidebar tokens label magnum's workspaces, so they go to the session
// those are in, not to the toasts' (yours): a round's "reviewing" token
// reaches the panes' session, and the daemon's wiring sends them through
// the App's client of it.
func TestTheSidebarTokensGoToThePaneSession(t *testing.T) {
	panes := &fakeNotifyHerdr{}
	h := newHarness(t, func(h *harness) { h.d.Sidebar = (&notify.Notifier{Herdr: panes}).Sidebar })
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1", request: true})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)
	if got := panes.tokens(); !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "reviewing b1") }) {
		t.Fatalf("the panes' session got the tokens %v, want the round's \"reviewing b1\"", got)
	}
	if got := h.nh.tokens(); len(got) != 0 {
		t.Fatalf("the toasts' session got sidebar tokens %v", got)
	}

	e, _, home := appEngine(t, &herdr.Session{Name: "reviews", Socket: "/nonexistent/reviews/herdr.sock"}, app.Options{DryRun: true}, nil)
	if e.d.Sidebar == nil {
		t.Fatal("FromApp sets no sidebar: magnum's workspaces would carry no tokens")
	}
	err := e.d.Sidebar(context.Background(), "w1", map[string]string{"magnum": "reviewing b1"})
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/reviews/herdr.sock") || strings.Contains(err.Error(), home) {
		t.Fatalf("the daemon's sidebar = %v, want a call on the panes' socket", err)
	}
}

// max_total_working_codex caps the Codex agents working on this machine,
// your own included: with magnum's panes in a session of their own, your
// session's working Codex agents count beside the panes'. The daemon's
// wiring hands the engine your session's client only when it is another.
func TestTheCodexCapCountsYourSessionsWorkingCodexToo(t *testing.T) {
	yours := &fakeHerdr{agents: []herdr.AgentInfo{
		{PaneID: "w3:p1", Name: "alice", Agent: "codex", AgentStatus: herdr.StatusWorking},
		{PaneID: "w3:p2", Name: "bob", Agent: "claude", AgentStatus: herdr.StatusWorking},
	}}
	h := newHarness(t, func(h *harness) {
		h.cfg.Daemon.MaxTotalWorkingCodex = 3
		h.d.Yours = yours
	})
	h.hd.mu.Lock()
	h.hd.agents = append(h.hd.agents,
		herdr.AgentInfo{PaneID: "px1", Name: "mg-other-9-codex-review", Agent: "codex", AgentStatus: herdr.StatusWorking})
	h.hd.mu.Unlock()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick() // #2 queued
	h.advance(5 * time.Minute)
	h.tick()
	pr := h.pr(2)
	if gate := fdGate(h, pr.ID); pr.State != store.PRQueued || !strings.HasPrefix(gate, "working Codex agents at the limit (2 working") {
		t.Fatalf("#2 = %s, gate %q; want held at the limit by magnum's Codex agent and yours", pr.State, gate)
	}

	e, _, home := appEngine(t, &herdr.Session{Name: "reviews", Socket: "/nonexistent/reviews/herdr.sock"}, app.Options{DryRun: true}, nil)
	if c, ok := e.d.Yours.(*herdr.Client); !ok || c.Socket != filepath.Join(home, "yours.sock") {
		t.Fatalf("FromApp's Yours = %#v, want a client of your session", e.d.Yours)
	}
	if e, _, _ := appEngine(t, nil, app.Options{DryRun: true}, nil); e.d.Yours != nil {
		t.Fatalf("FromApp's Yours = %#v with the panes in your session, want none (its agents would count twice)", e.d.Yours)
	}
}

// Your session not answering (or not running) leaves the cap to the
// panes' session: it holds nothing back, and herdr is not down for it.
func TestYourSessionDownLeavesTheCapToThePaneSession(t *testing.T) {
	yours := &fakeHerdr{err: fmt.Errorf("herdr session.snapshot: %w: context deadline exceeded", herdr.ErrTimeout)}
	h := newHarness(t, func(h *harness) {
		h.cfg.Daemon.MaxTotalWorkingCodex = 3
		h.d.Yours = yours
	})
	h.hd.mu.Lock()
	h.hd.agents = append(h.hd.agents,
		herdr.AgentInfo{PaneID: "px1", Name: "mg-other-9-codex-review", Agent: "codex", AgentStatus: herdr.StatusWorking})
	h.hd.mu.Unlock()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	if pr := h.pr(2); pr.State != store.PRReviewed {
		t.Fatalf("#2 = %s, gate %q; want reviewed (1 working Codex agent of 3)", pr.State, fdGate(h, pr.ID))
	}
	if v, _, _ := h.st.GetKV(h.ctx, store.KVHerdrUp); v != "1" {
		t.Fatalf("herdr_up = %q with only your session down", v)
	}
	yours.mu.Lock()
	calls := yours.calls
	yours.mu.Unlock()
	if calls == 0 {
		t.Fatal("your session was never read for the cap")
	}
}

// A tagged engine (an eval or learn-gate replay, the factory's local review)
// never dispatches, so nothing caps it on your session's Codex agents: its
// observe passes, every EvalObserveEvery, leave your herdr session alone,
// where one that does not answer would hold each pass up to herdr's
// timeout. Its App's guards still read your session.
func TestATaggedEngineNeverReadsYourSessionWhenItObserves(t *testing.T) {
	yours := &fakeHerdr{}
	e, a, _ := appEngine(t, nil, app.Options{DryRun: true, AgentTag: "eval-1"}, func(cfg *config.Config) {
		cfg.Herdr.ReviewSession, cfg.Herdr.ReviewSocket = "reviews", "/nonexistent/reviews/herdr.sock"
		cfg.Daemon.MaxTotalWorkingCodex = 5
	}, func(d *Deps) {
		d.Herdr = &fakeHerdr{}
		if d.Yours != nil {
			d.Yours = yours
		}
	})
	if a.Yours == nil || a.Guard.Yours == nil {
		t.Fatalf("the tagged App's Yours = %#v, guards' %#v: want your session beside the review session's panes", a.Yours, a.Guard.Yours)
	}
	if ts := e.observe(t.Context()); !ts.herdrUp {
		t.Fatal("the replay's own herdr session was not read")
	}
	yours.mu.Lock()
	calls := yours.calls
	yours.mu.Unlock()
	if calls != 0 || e.d.Yours != nil {
		t.Fatalf("the tagged engine read your session %d times (Yours %T), want never", calls, e.d.Yours)
	}
}

// reveal_on_attention brings forward a client of the session magnum's
// panes are in, the one that shows the PR's agent, not [terminal] session's
// while they are elsewhere.
func TestRevealOnAttentionRevealsThePaneSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		panes   *herdr.Session
		session string
	}{
		{"panes in session reviews", &herdr.Session{Name: "reviews", Socket: "/nonexistent/reviews/herdr.sock"}, "reviews"},
		{"panes in your session", nil, "work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"/bin/sh"}}}}
			var focused []string
			e, _, _ := appEngine(t, tc.panes, app.Options{Runner: run}, func(cfg *config.Config) {
				cfg.Terminal.RevealOnAttention, cfg.Terminal.App = true, "custom"
				cfg.Terminal.Launcher, cfg.Terminal.Session = "launch {herdr} {args}", "work"
			}, func(d *Deps) {
				d.Focus = func(_ context.Context, target string) error { focused = append(focused, target); return nil }
			})
			e.revealAttention(context.Background(), 1, &store.Session{AgentName: new("mg-talkable-1-codex-judge")}, "", "attention:1")
			if !slices.Equal(focused, []string{"mg-talkable-1-codex-judge"}) {
				t.Fatalf("focused %v, want the PR's agent", focused)
			}
			launches := run.CallsWithPrefix("/bin/sh")
			if len(launches) != 1 {
				t.Fatalf("launches = %d (%v), want one client opened", len(launches), run.Calls)
			}
			args := launches[0].Args
			if i := slices.Index(args, "--session"); i < 0 || i+1 >= len(args) || args[i+1] != tc.session {
				t.Fatalf("revealed %v, want a client of session %s", args, tc.session)
			}
		})
	}
}

// openWiring is a daemon's App wiring with a custom terminal launcher, so a
// reveal or an open shows as one /bin/sh call of run; review names the
// review session the config names ("" = yours).
func openWiring(run *execx.Fake, review string) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.Terminal.App, cfg.Terminal.Launcher, cfg.Terminal.Session = "custom", "launch {herdr} {args}", "work"
		if review != "" {
			cfg.Herdr.ReviewSession, cfg.Herdr.ReviewSocket = review, "/nonexistent/"+review+"/herdr.sock"
		}
	}
}

// launchedSessions are the herdr sessions run's /bin/sh calls opened a
// client of, in order.
func launchedSessions(run *execx.Fake) []string {
	var out []string
	for _, c := range run.CallsWithPrefix("/bin/sh") {
		if i := slices.Index(c.Args, "--session"); i >= 0 && i+1 < len(c.Args) {
			out = append(out, c.Args[i+1])
		}
	}
	return out
}

// A named review session whose server does not answer (none runs after a
// reboot, or it is stuck) gets a client opened in a new terminal window at
// once, which starts its server and shows it, then at most every 30
// minutes while it stays down, each with a herdr.opened event; a session
// that answers again opens nothing. The daemon's wiring opens a client of
// the panes' session, `herdr --session reviews`.
func TestADownReviewSessionIsOpenedAtMostTwiceAnHour(t *testing.T) {
	var opened atomic.Int32
	h := newHarness(t, func(h *harness) {
		h.d.PaneSession = reviewsPanes
		h.moveToReviews()
		h.d.OpenHerdr = func(context.Context) error { opened.Add(1); return nil }
	})
	h.hd.mu.Lock()
	h.hd.err = fmt.Errorf("herdr session.snapshot: %w: dial unix %s: %w", herdr.ErrUnavailable, reviewsPanes.Socket, syscall.ENOENT)
	h.hd.mu.Unlock()
	h.startup()
	for _, step := range []struct {
		after time.Duration
		want  int32
	}{{0, 1}, {10 * time.Minute, 1}, {20 * time.Minute, 2}, {29 * time.Minute, 2}, {time.Minute, 3}} {
		h.advance(step.after)
		h.tick()
		if got := opened.Load(); got != step.want {
			t.Fatalf("after %s more: opened %d times, want %d", step.after, got, step.want)
		}
	}
	h.hd.mu.Lock()
	h.hd.err = nil
	h.hd.mu.Unlock()
	h.advance(time.Hour)
	h.tick()
	if got := opened.Load(); got != 3 {
		t.Fatalf("a session that answers was opened: %d opens", got)
	}
	if evs := h.events("herdr.opened"); len(evs) != 3 || !strings.Contains(evs[0].Message, "herdr --session reviews") {
		t.Fatalf("herdr.opened events %+v", evs)
	}

	run := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"/bin/sh"}}}}
	e, _, _ := appEngine(t, &herdr.Session{Name: "reviews", Socket: "/nonexistent/reviews/herdr.sock"}, app.Options{Runner: run},
		openWiring(run, "reviews"))
	if e.d.OpenHerdr == nil {
		t.Fatal("FromApp sets no opener for a named review session")
	}
	if ts := e.observe(t.Context()); ts.herdrUp {
		t.Fatal("a session with no socket answered")
	}
	if got := launchedSessions(run); !slices.Equal(got, []string{"reviews"}) {
		t.Fatalf("opened clients of %v, want one of session reviews", got)
	}
}

// The daemon never opens your own herdr session, as before the review
// session: while magnum's panes are in it (a move to session reviews
// pending included), its wiring sets no opener and an observe that finds
// it down opens nothing. Nor does a replay's engine (a tagged App) or a
// dry run open the review session.
func TestTheDaemonNeverOpensYourOwnSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		review string
		opts   app.Options
	}{
		{"panes in your session", "", app.Options{}},
		{"a move to session reviews pending", "reviews", app.Options{}},
		{"a replay", "reviews", app.Options{AgentTag: "eval-1"}},
		{"a dry run", "reviews", app.Options{DryRun: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"/bin/sh"}}}}
			tc.opts.Runner = run
			e, _, _ := appEngine(t, nil, tc.opts, openWiring(run, tc.review))
			if e.d.OpenHerdr != nil {
				t.Fatal("FromApp sets an opener")
			}
			e.observe(t.Context())
			if got := launchedSessions(run); len(got) != 0 {
				t.Fatalf("opened clients of %v", got)
			}
		})
	}
}

// A named review session whose server runs but does not answer (a
// timeout, or one that holds its socket and accepts nothing) is not
// opened: a client of it may be open already, and the open would bring it
// to the front of your terminal at random, up to twice an hour, while a
// new client would not make that server answer. Herdr is noted down and
// dispatch waits, as before.
func TestAReviewSessionThatRunsButDoesNotAnswerIsNotOpened(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"timeout", fmt.Errorf("herdr session.snapshot: %w: context deadline exceeded", herdr.ErrTimeout)},
		{"no accept", fmt.Errorf("herdr session.snapshot: %w: a server holds the socket but accepts no connections (dial unix: connection refused)",
			herdr.ErrUnavailable)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opened atomic.Int32
			h := newHarness(t, func(h *harness) {
				h.d.PaneSession = reviewsPanes
				h.moveToReviews()
				h.d.OpenHerdr = func(context.Context) error { opened.Add(1); return nil }
			})
			h.hd.mu.Lock()
			h.hd.err = tc.err
			h.hd.mu.Unlock()
			h.startup()
			h.tick()
			h.advance(time.Hour)
			h.tick()
			if got := opened.Load(); got != 0 {
				t.Fatalf("opened %d times a session whose server runs", got)
			}
			if evs := h.events("herdr.down"); len(evs) != 1 {
				t.Fatalf("herdr.down events %+v, want one", evs)
			}
		})
	}
}

// While magnum's panes are to move away from the review session (the
// config names yours again), an observe that finds no server running there
// does not open it: the move does, and only while herdr ids recorded there
// may come back with it (TestAMoveFromASessionWithNoServerWaitsForItsServer).
func TestADownReviewSessionIsNotOpenedWhileThePanesMoveAway(t *testing.T) {
	var opened atomic.Int32
	h := newHarness(t, func(h *harness) {
		h.d.PaneSession = reviewsPanes
		h.d.OpenHerdr = func(context.Context) error { opened.Add(1); return nil }
	})
	h.hd.mu.Lock()
	h.hd.err = fmt.Errorf("herdr session.snapshot: %w: dial unix %s: %w", herdr.ErrUnavailable, reviewsPanes.Socket, syscall.ENOENT)
	h.hd.mu.Unlock()
	if ts := h.e.observe(h.ctx); ts.herdrUp || h.e.panesMoving() == "" {
		t.Fatalf("herdr up %v, move %q", ts.herdrUp, h.e.panesMoving())
	}
	if got := opened.Load(); got != 0 {
		t.Fatalf("opened %d times during a move away from the session", got)
	}
}
