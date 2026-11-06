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
