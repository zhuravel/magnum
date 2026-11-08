package engine

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
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

// onLayout builds a harness on layout l, the one its daemon claim took
// (claim), as the daemon command builds its App on the layout it claimed:
// Run then holds the claim's lock. On another layout Run would take a lock
// of its own and the test would miss a Run that takes the claim's again.
func onLayout(l paths.Layout) func(*harness) {
	return func(hs *harness) { hs.layout, hs.d.Layout, hs.ag.layout = l, l, l }
}

// waitTicked waits for the daemon h runs (done: Run's result) to tick past
// the before-th observe pass; Run returning first fails the test.
func waitTicked(t *testing.T, h *harness, before int, done <-chan error) {
	t.Helper()
	for deadline := time.Now().Add(clockGuard); h.ag.count("observe") <= before; time.Sleep(2 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("Run returned before it ticked (%v): it did not hold the daemon lock", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon never ticked")
		}
	}
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
	h := newHarness(t, onLayout(l), func(hs *harness) { hs.d.Logger = slog.New(logs) })
	before := h.ag.count("observe")
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true, Claim: c})
	waitTicked(t, h, before, done)
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
	h := newHarness(t, onLayout(l))
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
	return paneSessionAs(t, st, pane, "")
}

// paneSessionAs is paneSession with the agent's session id sid ("" for
// none).
func paneSessionAs(t *testing.T, st *store.Store, pane, sid string) store.Session {
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
	x := store.Session{PRID: up.PR.ID, Role: config.LearnRoleName, AgentName: new("mg-learn"),
		AgentKind: new(agents.KindClaude), HerdrWorkspaceID: new("w2"), HerdrPaneID: new(pane), State: store.SessionLive,
		StartedAt: time.Now()}
	if sid != "" {
		x.SessionID = &sid
	}
	s, err := st.CreateSession(ctx, x)
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
	h := newHarness(t, onLayout(l), func(hs *harness) { hs.d.Logger = slog.New(logs) })
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

// A StopFailure the agents' hooks proved goes where the screen-read failure
// of its kind goes today: a usage limit and a login loss pause the agent
// kind (pauseTool, as a reviewer's run that proved them does at once, and
// the retro's and the notes jobs' do), the usage limit until the reset its
// screen names; an overload, a model limit and any other failure pause
// nothing, and are left to the run's own end (the round's backoff, its
// model switch, the job's stop).
func TestAHookFailureGoesWhereTheScreenReadFailureOfItsKindGoes(t *testing.T) {
	reset := time.Date(2026, 10, 5, 13, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name    string
		failure agents.StopFailure
		screen  agents.Health
		reason  string // "" = no pause
		until   func(now time.Time) time.Time
	}{
		{"usage limit", agents.StopFailureRateLimit, agents.Health{Kind: agents.HealthUsageLimit, ResetAt: &reset, Detail: "limit reached"},
			string(agents.HealthUsageLimit), func(time.Time) time.Time { return reset }},
		{"billing", agents.StopFailureBilling, agents.Health{Kind: agents.HealthUsageLimit, Detail: "credit"},
			string(agents.HealthUsageLimit), func(now time.Time) time.Time { return now.Add(usageFallback) }},
		{"login", agents.StopFailureAuthentication, agents.Health{Kind: agents.HealthLoginRequired, Detail: "login"},
			string(agents.HealthLoginRequired), func(now time.Time) time.Time { return now.Add(loginRecheck) }},
		{"overload", agents.StopFailureOverloaded, agents.Health{Kind: agents.HealthOverloaded, Detail: "overloaded"}, "", nil},
		{"rate limit on one model", agents.StopFailureRateLimit, agents.Health{Kind: agents.HealthModelLimit, Detail: "model"}, "", nil},
		{"other", agents.StopFailureOther, agents.Health{Kind: agents.HealthStalled, Detail: "stalled"}, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			s := store.Session{ID: 3, Role: config.RoleClaudeReview, AgentKind: new(agents.KindClaude)}
			h.e.onHookFailure(h.ctx, app.HookFailure{Session: s, Failure: tc.failure, Screen: tc.screen})
			p, paused := h.e.toolPause(h.ctx, agents.KindClaude)
			if tc.reason == "" {
				if paused {
					t.Fatalf("paused %+v, want no pause", p)
				}
				return
			}
			if !paused || p.Reason != tc.reason || !p.Until.Equal(tc.until(h.clock.Now())) || p.Detail != tc.screen.Detail {
				t.Fatalf("pause %+v (%v), want %s until %s with the screen's line", p, paused, tc.reason, tc.until(h.clock.Now()))
			}
		})
	}
}

// The daemon consumes the hook failures on a goroutine of its own while it
// listens: a usage limit sent to the queue pauses the kind.
func TestTheDaemonPausesAKindOnAHookFailureWhileItListens(t *testing.T) {
	l := shortLayout(t)
	c := claim(t, l)
	failures := make(chan app.HookFailure, 1)
	h := newHarness(t, onLayout(l), func(hs *harness) { hs.d.HookFailures = failures })
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true, Claim: c})
	failures <- app.HookFailure{Session: store.Session{ID: 3, Role: config.RoleClaudeReview, AgentKind: new(agents.KindClaude)},
		Failure: agents.StopFailureAuthentication, Screen: agents.Health{Kind: agents.HealthLoginRequired, Detail: "login"}}
	for deadline := time.Now().Add(clockGuard); ; time.Sleep(2 * time.Millisecond) {
		if p, ok := h.e.toolPause(h.ctx, agents.KindClaude); ok && p.Reason == string(agents.HealthLoginRequired) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hook failure never paused the kind")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// FromApp hands the App's queue of hook failures to the engine, which
// handles them while it listens; an App without a hook socket has none.
func TestFromAppHandsTheHookFailuresToTheEngine(t *testing.T) {
	for _, socket := range []string{"/state/run/hooks.sock", ""} {
		home := t.TempDir()
		cfgText := strings.ReplaceAll(testConfigTOML, "HOME", home)
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfgText), 0o600); err != nil {
			t.Fatal(err)
		}
		layout := paths.Layout{Home: home}
		cfg, err := config.Load(layout, filepath.Join(home, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		a, err := app.New(cfg, layout, app.Options{DryRun: true, Runner: &execx.Fake{}, Logger: slog.New(slog.DiscardHandler),
			HookSocket: socket, MySQLDSN: "root:@tcp(127.0.0.1:1)/?timeout=1s"})
		if err != nil {
			t.Fatal(err)
		}
		e := FromApp(a)
		if (socket != "") != (e.d.HookFailures != nil) || (socket != "" && e.d.HookFailures != (<-chan app.HookFailure)(a.HookFailures)) {
			t.Errorf("socket %q: the engine's hook failures %v, the App's %v", socket, e.d.HookFailures, a.HookFailures)
		}
		a.Close()
	}
}

// The daemon records at its start whether its hook listener is up, for
// `magnum status`: its socket, or why it has none; a start without a claim
// removes an earlier start's record.
func TestTheDaemonRecordsWhetherItsHookListenerIsUp(t *testing.T) {
	l := shortLayout(t)
	h := newHarness(t, onLayout(l))
	run := func(c *Claim) {
		t.Helper()
		ctx, cancel := context.WithCancel(h.ctx)
		done := h.goRun(ctx, Options{NoSignals: true, Once: true, Claim: c})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	c := claim(t, l)
	run(c)
	if got, ok := ReadDaemonHooks(h.ctx, h.st); !ok || got.Socket != l.HookSocket() || got.Off != "" {
		t.Fatalf("listening: %+v, %v; want the socket %s", got, ok, l.HookSocket())
	}
	c.Release()

	long := paths.Layout{Home: filepath.Join(t.TempDir(), strings.Repeat("x", paths.SocketPathLimit))}
	off := claim(t, long)
	run(off)
	if got, ok := ReadDaemonHooks(h.ctx, h.st); !ok || got.Socket != "" || !strings.Contains(got.Off, "must be under 104") {
		t.Fatalf("off: %+v, %v; want why", got, ok)
	}
	off.Release()

	run(nil)
	if got, ok := ReadDaemonHooks(h.ctx, h.st); ok {
		t.Fatalf("no claim: %+v, want no record", got)
	}
}

// Run holds the daemon lock the daemon command's claim took on its layout
// and never takes it a second time: flock's locks belong to the open file,
// so a second take in the same process finds the lock held, and Run would
// exit as a second daemon does ("already running", status 0, which launchd
// never restarts) with nothing done. It writes its pidfile, ticks, and
// gives the lock up when it returns.
func TestRunHoldsTheDaemonLockItsClaimTook(t *testing.T) {
	l := shortLayout(t)
	c := claim(t, l)
	logs := &recordLog{}
	h := newHarness(t, onLayout(l), func(hs *harness) { hs.d.Logger = slog.New(logs) })
	before := h.ag.count("observe")
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true, Claim: c})
	waitTicked(t, h, before, done)
	if b, err := os.ReadFile(l.Pid()); err != nil || strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the pidfile while Run runs: %q, %v", b, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if n := logs.count(slog.LevelInfo, "magnum daemon already running; exiting"); n != 0 {
		t.Fatal("Run took the claim's lock again and found it held")
	}
	if again := claim(t, l); again.Held() {
		t.Fatal("the lock stays taken after Run returned")
	}
}

// While Run serves its claim's listener, the pane agents register with its
// router (startHooks opens it), so the event of a pane agent's pane sent to
// the socket reaches that agent's manager; once Run returned, none
// registers.
func TestAPaneAgentsHookEventReachesItWhileTheDaemonServesItsClaim(t *testing.T) {
	l := shortLayout(t)
	c := claim(t, l)
	h := newHarness(t, onLayout(l))
	h.prInFlight(1, config.RoleClaudeReview, "mg-1-claude-review-abc123", "w1:p1", t.TempDir())
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true, Claim: c})
	if err := agenthooks.Send(l.HookSocket(), hookEvent("w1:p1")); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(clockGuard); len(h.ag.wakes()) == 0; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the review pane's event never reached Wake: the daemon does not serve its listener")
		}
	}

	scratch := storetest.Open(t, filepath.Join(t.TempDir(), "scratch", "magnum.db"))
	paneSession(t, scratch, "w2:p1")
	retro := &fakeAgents{st: scratch}
	sink := h.e.agentHooks.add("retro", scratch, retro)
	if sink == nil {
		t.Fatal("no pane agent registers while the daemon serves its listener")
	}
	if err := agenthooks.Send(l.HookSocket(), hookEvent("w2:p1")); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(clockGuard); len(retro.wakes()) == 0; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the retro pane's event never reached the retro's manager")
		}
	}
	h.e.agentHooks.remove(sink)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got := panes(h.ag.wakes()); !slices.Equal(got, []string{"w1:p1"}) {
		t.Fatalf("the daemon's manager woke for %v, want its review pane alone", got)
	}
	if s := h.e.agentHooks.add("retro", scratch, retro); s != nil {
		t.Fatal("a pane agent registered after the daemon stopped listening")
	}
}

// FromApp makes the pane agents (the retro's, a curation's, a critique's
// and a skill edit's) on the engine's own hook router, the one Run serves,
// so they register where the daemon routes its events, and their managers
// give their panes the App's hook socket.
func TestFromAppMakesThePaneAgentsOnTheEnginesHookRouter(t *testing.T) {
	const socket = "/state/run/hooks.sock"
	e, a, _ := appEngine(t, nil, app.Options{HookSocket: socket}, nil)
	ctx := t.Context()
	made := map[string]*paneAgent{}
	if c, err := e.d.Classifier(ctx, RetroRun{ID: "r"}); err == nil {
		made["retro"] = c.(*paneClassifier).paneAgent
	}
	if c, err := e.d.Curator(ctx, CurateRun{ID: "c"}); err == nil {
		made["curation"] = c.(*paneCurator).paneAgent
	}
	if c, err := e.d.Critic(ctx, CriticRun{ID: "k"}); err == nil {
		made["critique"] = c.(*paneCritic).paneAgent
	}
	if c, err := e.d.Editor(ctx, EditRun{ID: "e"}); err == nil {
		made["skill edit"] = c.(*paneEditor).paneAgent
	}
	if len(made) != 4 {
		t.Fatalf("FromApp made the pane agents %v, want the four", slices.Sorted(maps.Keys(made)))
	}
	scratch := storetest.Open(t, filepath.Join(t.TempDir(), "scratch", "magnum.db"))
	for what, p := range made {
		if p.d.Hooks == nil || p.d.Hooks != e.agentHooks {
			t.Errorf("the %s agent's hook router is %p, want the engine's %p", what, p.d.Hooks, e.agentHooks)
		}
		m, ok := p.d.Agents(scratch, a.Config, a.Layout).(*agents.Manager)
		if !ok {
			t.Fatalf("the %s agent's manager is no agents.Manager", what)
		}
		env, err := m.PaneEnv(nil, a.Config.LearnRole())
		if err != nil {
			t.Fatal(err)
		}
		if env[agenthooks.EnvSocket] != socket {
			t.Errorf("the %s agent's pane env: %v, want the socket %s", what, env, socket)
		}
	}
}

// limitHerdr is an evalHerdr whose agents all show screen and whose
// snapshots fail, so a hook's look leaves the session to the observe pass
// and only its StopFailure's read of the screen counts.
type limitHerdr struct {
	*evalHerdr
	screen string
}

func (h limitHerdr) AgentRead(_ context.Context, _ string, o herdr.ReadOptions) (herdr.ReadResult, error) {
	return herdr.ReadResult{Source: o.Source, Text: h.screen}, nil
}

func (limitHerdr) Snapshot(context.Context) (herdr.Snapshot, error) {
	return herdr.Snapshot{}, errors.New("herdr: no snapshot")
}

// claudeLimitScreen is a Claude pane that hit its usage limit.
const claudeLimitScreen = "⏺ Reading the diff.\n\n  ⎿  You've hit your session limit · resets 3pm (Europe/Kyiv)\n\n> \n"

// A pane agent's manager hands the StopFailures its agents' hooks prove to
// the App's queue, as the App's own manager does (learnAgents): a
// StopFailure of the retro's session whose screen shows a usage limit,
// given to Wake, comes out of the queue as its class and the screen's
// verdict.
func TestThePaneAgentsHookFailuresGoToTheAppsQueue(t *testing.T) {
	h := newHarness(t)
	scratch := storetest.Open(t, filepath.Join(t.TempDir(), "scratch", "magnum.db"))
	ev := hookEvent("w2:p1")
	ev.Name, ev.Error = agenthooks.NameStopFailure, string(agents.StopFailureRateLimit)
	s := paneSessionAs(t, scratch, "w2:p1", ev.SessionID)
	cfg := *h.cfg
	cfg.Roles = []config.Role{cfg.LearnRole()}
	a := &app.App{Runner: &execx.Fake{}, Logger: h.d.Logger, HookSocket: "/state/run/hooks.sock", HookFailures: make(chan app.HookFailure, 1)}
	m := learnAgents(limitHerdr{evalHerdr: newEvalHerdr(), screen: claudeLimitScreen}, a)(scratch, &cfg, h.layout)
	m.Wake(h.ctx, ev)
	m.WaitWakes()
	select {
	case f := <-a.HookFailures:
		if f.Session.ID != s.ID || f.Failure != agents.StopFailureRateLimit || f.Screen.Kind != agents.HealthUsageLimit {
			t.Fatalf("queued %+v, want session %d's rate limit with the screen's usage limit", f, s.ID)
		}
	default:
		t.Fatal("the retro's StopFailure never reached the App's queue")
	}
}

// gatedWake is a pane agent's manager whose Wake waits for gate, after it
// hands its context on started; it records "wake", "woke" and "wait wakes".
type gatedWake struct {
	started chan context.Context
	gate    chan struct{}
	order   *callOrder
}

func (g *gatedWake) Wake(ctx context.Context, _ agenthooks.Event) {
	g.order.add("wake")
	g.started <- ctx
	select {
	case <-g.gate:
	case <-time.After(leakWait):
	}
	g.order.add("woke")
}

func (g *gatedWake) WaitWakes() { g.order.add("wait wakes") }

// callOrder is the order of calls several goroutines make.
type callOrder struct {
	mu    sync.Mutex
	calls []string
}

func (o *callOrder) add(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, s)
}

func (o *callOrder) all() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.calls)
}

// A pane agent that leaves the router while a Wake of its pane is in flight
// (its close, before its scratch registry closes) ends that Wake's later
// looks first (its sink's context is done at once) and returns only after
// the Wake returned and its later looks were waited for: nothing reads or
// writes the scratch registry after the close goes on.
func TestARemovedPaneAgentWaitsForItsWakeInFlight(t *testing.T) {
	h := newHarness(t)
	scratch := storetest.Open(t, filepath.Join(t.TempDir(), "scratch", "magnum.db"))
	paneSession(t, scratch, "w2:p1")
	order := &callOrder{}
	mgr := &gatedWake{started: make(chan context.Context, 1), gate: make(chan struct{}), order: order}
	h.e.agentHooks.open(h.ctx)
	sink := h.e.agentHooks.add("retro", scratch, mgr)
	if sink == nil {
		t.Fatal("no pane agent registered while the daemon listens")
	}
	routed := make(chan struct{})
	go func() {
		defer close(routed)
		h.e.routeHook(h.ctx, hookEvent("w2:p1"))
	}()
	var wakeCtx context.Context
	select {
	case wakeCtx = <-mgr.started:
	case <-time.After(clockGuard):
		t.Fatal("the event never reached the pane agent's Wake")
	}
	removed := make(chan struct{})
	go func() {
		defer close(removed)
		h.e.agentHooks.remove(sink)
		order.add("removed")
	}()
	select {
	case <-wakeCtx.Done():
	case <-time.After(clockGuard):
		t.Fatal("the Wake's context is not done while the pane agent leaves: its later looks would go on")
	}
	time.Sleep(50 * time.Millisecond) // remove would have returned by now, were it not waiting for the Wake
	select {
	case <-removed:
		t.Fatal("remove returned while the Wake was in flight")
	default:
	}
	close(mgr.gate)
	for _, ch := range []chan struct{}{removed, routed} {
		select {
		case <-ch:
		case <-time.After(clockGuard):
			t.Fatal("remove or the routing never returned")
		}
	}
	if got := order.all(); !slices.Equal(got, []string{"wake", "woke", "wait wakes", "removed"}) {
		t.Fatalf("calls = %q, want the Wake, its return, WaitWakes, then remove's return", got)
	}
}
