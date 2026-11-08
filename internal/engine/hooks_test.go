package engine

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// shortLayout is a layout whose hook socket path fits a Unix socket's:
// t.TempDir's is too long.
func shortLayout(t *testing.T) paths.Layout {
	t.Helper()
	dir, err := os.MkdirTemp("", "mg") //nolint:usetesting // t.TempDir's path is too long for a Unix socket's
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l := paths.Layout{Home: dir}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return l
}

// claim is ClaimDaemon on l, released when the test ends.
func claim(t *testing.T, l paths.Layout) *Claim {
	t.Helper()
	c, err := ClaimDaemon(l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Release)
	return c
}

// serveClaim serves c's listener into a channel of the events it hands on,
// until the test ends.
func serveClaim(t *testing.T, c *Claim) <-chan agenthooks.Event {
	t.Helper()
	if c.listener == nil {
		t.Fatal("the claim has no listener")
	}
	got := make(chan agenthooks.Event, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.listener.Serve(ctx, func(ev agenthooks.Event) { got <- ev })
	}()
	t.Cleanup(func() { cancel(); <-done })
	return got
}

// hookEvent is a Stop of pane, as the hook script sends it.
func hookEvent(pane string) agenthooks.Event {
	return agenthooks.Event{Agent: agenthooks.AgentClaude, Name: agenthooks.NameStop, Pane: pane,
		SessionID: "0b5c1a52-3d2e-4f43-9b7c-4f6a3e2d1c00"}
}

// The review daemon starts its hook listener on Layout.HookSocket under its
// lock: a socket an earlier daemon left there is replaced, and a second
// daemon, which finds the lock taken, neither listens nor touches the first
// one's socket.
func TestTheDaemonListensOnItsHookSocketUnderItsLock(t *testing.T) {
	l := shortLayout(t)
	path := l.HookSocket()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	stale.Close() // the file stays, as a killed daemon's does
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the stale socket: %v", err)
	}

	first := claim(t, l)
	if first.Held() || first.HookSocket() != path || first.Off() != "" {
		t.Fatalf("the first claim: held %v, socket %q, off %q; want the lock and a listener on %s", first.Held(), first.HookSocket(), first.Off(), path)
	}
	got := serveClaim(t, first)

	second := claim(t, l)
	if !second.Held() || second.HookSocket() != "" || second.listener != nil {
		t.Fatalf("the second claim: held %v, socket %q; want the lock taken and no listener", second.Held(), second.HookSocket())
	}
	if err := agenthooks.Send(path, hookEvent("w1:p1")); err != nil {
		t.Fatalf("send to the first daemon's socket after the second claim: %v", err)
	}
	select {
	case ev := <-got:
		if ev.Pane != "w1:p1" {
			t.Fatalf("the first daemon read %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first daemon's listener got nothing")
	}

	first.Release()
	third := claim(t, l)
	if third.Held() {
		t.Fatal("the lock stays taken after the first claim's release")
	}
}

// A hook socket path too long for a Unix socket leaves the daemon on its
// polling alone: no listener, no socket file, and why.
func TestAHookSocketPathTooLongLeavesTheDaemonPolling(t *testing.T) {
	l := paths.Layout{Home: filepath.Join(t.TempDir(), strings.Repeat("x", paths.SocketPathLimit))}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	c := claim(t, l)
	if c.Held() || c.HookSocket() != "" || c.listener != nil {
		t.Fatalf("held %v, socket %q: want the lock and no listener", c.Held(), c.HookSocket())
	}
	if !strings.Contains(c.Off(), "must be under 104") {
		t.Fatalf("off = %q, want the path's length named", c.Off())
	}
	if _, err := os.Lstat(l.HookSocket()); !os.IsNotExist(err) {
		t.Fatalf("a socket file at the long path: %v", err)
	}
}

// A listener that cannot start (a file that is no socket holds its path)
// does not stop the daemon: it runs its ticks with polling alone, and one
// warning says why; the file stays as it was.
func TestAHookListenerThatFailsDoesNotStopTheDaemon(t *testing.T) {
	l := shortLayout(t)
	path := l.HookSocket()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a socket\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := claim(t, l)
	if c.Held() || c.HookSocket() != "" || !strings.Contains(c.Off(), "no socket") {
		t.Fatalf("held %v, socket %q, off %q: want the lock, no listener and why", c.Held(), c.HookSocket(), c.Off())
	}
	logs := &recordLog{}
	h := newHarness(t, func(hs *harness) { hs.d.Logger = slog.New(logs) })
	before := h.ag.count("observe")
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true, Claim: c})
	for deadline := time.Now().Add(clockGuard); h.ag.count("observe") <= before; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the daemon never ticked")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if n := logs.count(slog.LevelWarn, hooksOffMsg); n != 1 {
		t.Fatalf("%q warned %d times, want once", hooksOffMsg, n)
	}
	if why := logs.attr(slog.LevelWarn, hooksOffMsg, "why"); !strings.Contains(why, "no socket") {
		t.Fatalf("the warning's why = %q, want the listener's error", why)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "not a socket\n" {
		t.Fatalf("the file at the socket path: %q, %v", b, err)
	}
}

// At shutdown the daemon stops its listener, whose Serve returns once the
// Wake in flight returned, and only then waits for the later looks Wake
// started (WaitWakes).
func TestTheDaemonStopsItsHookListenerBeforeItWaitsForTheWakes(t *testing.T) {
	l := shortLayout(t)
	c := claim(t, l)
	h := newHarness(t)
	h.prInFlight(1, config.RoleClaudeReview, "mg-1-claude-review-abc123", "w1:p1", t.TempDir())
	gate, started := make(chan struct{}), make(chan struct{}, 1)
	h.ag.mu.Lock()
	h.ag.wakeGate, h.ag.wakeStarted = gate, started
	h.ag.mu.Unlock()
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true, Claim: c})
	for deadline := time.Now().Add(clockGuard); ; time.Sleep(5 * time.Millisecond) {
		if err := agenthooks.Send(l.HookSocket(), hookEvent("w1:p1")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon never listened")
		}
	}
	select {
	case <-started:
	case <-time.After(clockGuard):
		t.Fatal("the event never reached Wake")
	}
	cancel()
	time.Sleep(50 * time.Millisecond) // Run would wait for the wakes now, were it not waiting for Serve
	if n := h.ag.count("wait wakes"); n != 0 {
		t.Fatal("WaitWakes ran while a Wake was in flight")
	}
	close(gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(clockGuard):
		t.Fatal("Run did not stop")
	}
	calls := slices.DeleteFunc(h.ag.all(), func(s string) bool {
		return !strings.HasPrefix(s, "wake ") && !strings.HasPrefix(s, "woke ") && s != "wait wakes"
	})
	if !slices.Equal(calls, []string{"wake stop w1:p1", "woke stop w1:p1", "wait wakes"}) {
		t.Fatalf("calls = %q, want the Wake, its return, then WaitWakes", calls)
	}
	if err := agenthooks.Send(l.HookSocket(), hookEvent("w1:p1")); err == nil {
		t.Fatal("the socket still reads after the daemon stopped")
	}
}

// attr is the value of key in the first record of level and msg ("" for
// none).
func (l *recordLog) attr(level slog.Level, msg, key string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if r.Level != level || r.Message != msg {
			continue
		}
		v := ""
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				v = a.Value.String()
				return false
			}
			return true
		})
		return v
	}
	return ""
}

// paneSession records a live session of an agent on pane in st, under a
// scratch PR of its own, as a pane agent's registry holds one.
func paneSession(t *testing.T, st *store.Store, pane string) store.Session {
	t.Helper()
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "learn:retro", Owner: "magnum", Name: "retro", WatchOwner: "magnum", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	up, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "learn:retro#1", Number: 1, URL: "file:///retro",
		HeadSHA: "r", GHState: store.GHOpen, InitialState: store.PRClaiming, Identity: LearnAgentTag})
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.CreateSession(ctx, store.Session{PRID: up.PR.ID, Role: config.LearnRoleName, AgentName: new("mg-learn"),
		AgentKind: new(agents.KindClaude), HerdrWorkspaceID: new("w2"), HerdrPaneID: new(pane), State: store.SessionLive,
		StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// panes are the panes of the events Wake was given.
func panes(evs []agenthooks.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Pane)
	}
	return out
}

// Each hook event reaches the agents manager whose registry records a live
// session on its pane: the daemon's own for a review pane, a pane agent's
// (the retro's) for its pane while it is registered; an event of a pane no
// registry records reaches none, and a pane agent removed gets no more,
// once its later looks were waited for.
func TestAHookEventReachesTheManagerThatOwnsItsPane(t *testing.T) {
	h := newHarness(t)
	h.prInFlight(1, config.RoleClaudeReview, "mg-1-claude-review-abc123", "w1:p1", t.TempDir())
	scratch := storetest.Open(t, filepath.Join(t.TempDir(), "scratch", "magnum.db"))
	paneSession(t, scratch, "w2:p1")
	retro := &fakeAgents{st: scratch}
	h.e.agentHooks.open(h.ctx)
	sink := h.e.agentHooks.add("retro", scratch, retro)
	if sink == nil {
		t.Fatal("no pane agent registered while the daemon listens")
	}

	for _, pane := range []string{"w1:p1", "w2:p1", "w9:p9"} {
		h.e.routeHook(h.ctx, hookEvent(pane))
	}
	if got := panes(h.ag.wakes()); !slices.Equal(got, []string{"w1:p1"}) {
		t.Fatalf("the daemon's manager woke for %v, want its review pane alone", got)
	}
	if got := panes(retro.wakes()); !slices.Equal(got, []string{"w2:p1"}) {
		t.Fatalf("the retro's manager woke for %v, want its pane alone", got)
	}

	h.e.agentHooks.remove(sink)
	if n := retro.count("wait wakes"); n != 1 {
		t.Fatalf("WaitWakes ran %d times at the pane agent's removal, want once", n)
	}
	h.e.routeHook(h.ctx, hookEvent("w2:p1"))
	if n := len(retro.wakes()); n != 1 {
		t.Fatalf("the removed pane agent's manager woke %d times, want the one before", n)
	}
	if n := len(h.ag.wakes()); n != 1 {
		t.Fatalf("the daemon's manager woke %d times, want the one for its pane", n)
	}
}

// No pane agent registers while the daemon does not listen: its manager has
// no hook env to wake for.
func TestNoPaneAgentRegistersWithoutAListener(t *testing.T) {
	h := newHarness(t)
	scratch := storetest.Open(t, filepath.Join(t.TempDir(), "scratch", "magnum.db"))
	if s := h.e.agentHooks.add("retro", scratch, &fakeAgents{st: scratch}); s != nil {
		t.Fatal("a pane agent registered without a listener")
	}
}

// The retro's agent, a pane agent over its scratch registry, gets the hook
// events of its own pane while it works, and none once it closed; the
// daemon's own manager gets none of them.
func TestTheRetroAgentsHookEventsReachItsOwnManager(t *testing.T) {
	p := newPaneHarness(t)
	p.e.agentHooks.open(p.ctx)
	run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
	job := p.job(run, 7)
	var pane string
	p.ag.reply = func(int, string) bool {
		live, err := p.ag.st.LiveSessions(context.Background())
		if err != nil || len(live) != 1 {
			t.Errorf("the scratch registry's live sessions: %v, %v", live, err)
			return true
		}
		pane = store.Deref(live[0].HerdrPaneID)
		p.e.routeHook(p.ctx, hookEvent(pane))
		validAnswer(t, job)
		return true
	}
	cl := p.classifier(run)
	if _, err := cl.Classify(p.ctx, job); err != nil {
		t.Fatal(err)
	}
	if got := panes(p.ag.wakes()); len(got) != 1 || got[0] != pane || pane == "" {
		t.Fatalf("the retro's manager woke for %v, want its pane %q", got, pane)
	}
	if err := cl.Close(p.ctx); err != nil {
		t.Fatal(err)
	}
	if n := p.ag.count("wait wakes"); n != 1 {
		t.Fatalf("WaitWakes ran %d times at the close, want once", n)
	}
	p.e.routeHook(p.ctx, hookEvent(pane))
	if n := len(p.ag.wakes()); n != 1 {
		t.Fatalf("the closed retro's manager woke %d times, want the one before", n)
	}
	if n := len(p.harness.ag.wakes()); n != 0 {
		t.Fatalf("the daemon's manager woke %d times for the retro's pane", n)
	}
}

// The doctor's probe, a datagram named "ping", is accepted (its send
// succeeds) and dropped: no Wake, and nothing logged above debug level; the
// router itself logs nothing for one.
func TestAPingIsAcceptedAndDroppedQuietly(t *testing.T) {
	l := shortLayout(t)
	c := claim(t, l)
	logs := &recordLog{}
	h := newHarness(t, func(hs *harness) { hs.d.Logger = slog.New(logs) })
	h.prInFlight(1, config.RoleClaudeReview, "mg-1-claude-review-abc123", "w1:p1", t.TempDir())
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true, Claim: c})
	before := logs.len()
	for _, ping := range []agenthooks.Event{{Name: hookPing}, {Agent: agenthooks.AgentClaude, Name: hookPing, Pane: "w1:p1"}} {
		if err := agenthooks.Send(l.HookSocket(), ping); err != nil {
			t.Fatalf("the ping %+v was refused: %v", ping, err)
		}
	}
	if err := agenthooks.Send(l.HookSocket(), hookEvent("w1:p1")); err != nil { // after the pings: Serve reads in order
		t.Fatal(err)
	}
	for deadline := time.Now().Add(clockGuard); len(h.ag.wakes()) == 0; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stop after the pings never reached Wake")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := h.ag.wakes(); len(got) != 1 || got[0].Name != agenthooks.NameStop {
		t.Fatalf("Wake got %+v, want the stop alone", got)
	}
	for _, r := range logs.since(before) {
		if r.Level > slog.LevelDebug && strings.Contains(strings.ToLower(r.Message), "hook") && r.Message != hooksListeningMsg {
			t.Errorf("a ping logged %s %q", r.Level, r.Message)
		}
	}

	quiet := &recordLog{}
	h.e.log = slog.New(quiet)
	h.e.routeHook(h.ctx, agenthooks.Event{Agent: agenthooks.AgentClaude, Name: hookPing, Pane: "w1:p1"})
	if n := quiet.len(); n != 0 || len(h.ag.wakes()) != 1 {
		t.Fatalf("the router logged %d lines and woke %d times for a ping, want none", n, len(h.ag.wakes())-1)
	}
}

// Each event the router hands on logs one debug line with its name, its
// pane, the session's registry, role and run in flight, and never a field
// of the payload (the session id, the transcript, the error).
func TestEachHookEventLogsItsNameItsPaneAndItsRun(t *testing.T) {
	logs := &recordLog{}
	h := newHarness(t, func(hs *harness) { hs.d.Logger = slog.New(logs) })
	run := h.prInFlight(1, config.RoleClaudeReview, "mg-1-claude-review-abc123", "w1:p1", t.TempDir())
	h.e.agentHooks.open(h.ctx)
	ev := hookEvent("w1:p1")
	ev.Name, ev.Error, ev.TranscriptPath = agenthooks.NameStopFailure, "SECRET-ERROR", "/secret/transcript.jsonl"
	h.e.routeHook(h.ctx, ev)
	if n := logs.count(slog.LevelDebug, hookEventMsg); n != 1 {
		t.Fatalf("%q logged %d times, want once", hookEventMsg, n)
	}
	for key, want := range map[string]string{"event": agenthooks.NameStopFailure, "pane": "w1:p1", "registry": "daemon",
		"role": config.RoleClaudeReview, "run": run.ID} {
		if got := logs.attr(slog.LevelDebug, hookEventMsg, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for _, r := range logs.since(0) {
		r.Attrs(func(a slog.Attr) bool {
			if v := a.Value.String(); strings.Contains(v, "SECRET") || strings.Contains(v, ev.SessionID) {
				t.Errorf("%q logged %s=%q", r.Message, a.Key, v)
			}
			return true
		})
	}
}

func (l *recordLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recs)
}

// since are the records from the n-th on.
func (l *recordLog) since(n int) []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.recs[min(n, len(l.recs)):])
}

// The pane agents' managers (the retro's, a curation's, ...) give their
// panes the hook env exactly while the daemon listens: the retro's
// EnsureWorkspace passes no env of its own, and its session's pane gets the
// App's hook socket and the binary, or neither without a listener.
func TestThePaneAgentsPanesGetTheHookEnvOnlyWhileTheDaemonListens(t *testing.T) {
	for _, tc := range []struct {
		name, socket string
	}{{"listening", "/state/run/hooks.sock"}, {"no listener", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			scratch := storetest.Open(t, filepath.Join(t.TempDir(), "scratch", "magnum.db"))
			old := paneSession(t, scratch, "w0:p0") // an earlier session, closed: the PR row is the one to use
			if err := scratch.TransitionSession(h.ctx, old.ID, []string{store.SessionLive}, store.SessionClosed, nil); err != nil {
				t.Fatal(err)
			}
			cfg := *h.cfg
			role := cfg.LearnRole()
			cfg.Roles = []config.Role{role}
			a := &app.App{Runner: &execx.Fake{}, Logger: h.d.Logger, HookSocket: tc.socket}
			m := learnAgents(newEvalHerdr(), a)(scratch, &cfg, h.layout)
			row, err := scratch.PRByID(h.ctx, old.PRID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.EnsureWorkspace(h.ctx, row, t.TempDir(), nil, learnWorkspace, []config.Role{role}); err != nil {
				t.Fatal(err)
			}
			s, err := scratch.LiveSessionByPRRole(h.ctx, row.ID, role.Name)
			if err != nil {
				t.Fatal(err)
			}
			got, has := s.Env[agenthooks.EnvSocket]
			bin, hasBin := s.Env[agenthooks.EnvBin]
			switch {
			case tc.socket != "" && (got != tc.socket || bin != h.layout.Binary()):
				t.Fatalf("the retro pane's env: socket %q, bin %q; want %q and %q", got, bin, tc.socket, h.layout.Binary())
			case tc.socket == "" && (has || hasBin):
				t.Fatalf("the retro pane's env without a listener: %v", s.Env)
			}
		})
	}
}
