package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// stuckHerdr is an evalHerdr whose agents leave their panes on ctrl+c, but
// for those in the stuck panes, which stay their pane's foreground process
// through any key (the fixer's Claude Code on talkable#11932).
type stuckHerdr struct {
	*evalHerdr
	stuck map[string]bool
}

var _ agents.Herdr = (*stuckHerdr)(nil)

func (h *stuckHerdr) PaneSendKeys(ctx context.Context, pane string, keys ...string) error {
	if err := h.evalHerdr.PaneSendKeys(ctx, pane, keys...); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stuck[pane] && slices.Contains(keys, "ctrl+c") {
		h.agents = slices.DeleteFunc(h.agents, func(a herdr.AgentInfo) bool { return a.PaneID == pane })
	}
	return nil
}

func (h *stuckHerdr) WaitIdleShell(ctx context.Context, pane string, timeout time.Duration) (herdr.ProcessInfo, error) {
	h.mu.Lock()
	stuck := h.stuck[pane]
	h.mu.Unlock()
	if stuck {
		return herdr.ProcessInfo{PaneID: pane, ShellPID: 10, ForegroundPGID: 20},
			fmt.Errorf("herdr pane %s: shell not idle after %s (foreground: 2.1.296): %w", pane, timeout, herdr.ErrTimeout)
	}
	return h.evalHerdr.WaitIdleShell(ctx, pane, timeout)
}

// keysTo are the key presses sent to pane, one call per entry.
func (h *stuckHerdr) keysTo(pane string) []string {
	var out []string
	for _, k := range h.sent() {
		if keys, ok := strings.CutPrefix(k, pane+":"); ok {
			out = append(out, keys)
		}
	}
	return out
}

// startOpts are the agent.start calls so far.
func (h *stuckHerdr) startOpts() []herdr.AgentStartOptions {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.starts)
}

func (h *stuckHerdr) closedPanes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.paneClosed)
}

// newStuckHarness is a daemon whose agents are a real agents.Manager over a
// stuckHerdr; their waits return at once, and no head changes the project
// config the agents reload (parkReloading quits none).
func newStuckHarness(t *testing.T) (*harness, *stuckHerdr) {
	t.Helper()
	sh := &stuckHerdr{evalHerdr: newEvalHerdr(), stuck: map[string]bool{}}
	h := newHarness(t, func(hs *harness) {
		hs.d.Herdr = sh
		hs.d.Git = &projectGit{} // no head changes the project config: no session is quit before the checkout
		run := &execx.Fake{Rules: []execx.Rule{
			{Prefix: []string{"codex", "login", "status"}, Result: execx.Result{Stdout: []byte("Logged in using ChatGPT\n")}},
			{Prefix: []string{"claude", "auth", "status"}, Result: execx.Result{Stdout: []byte(`{"loggedIn": true}`)}},
			{Prefix: []string{"zsh", "-ic", "whence -w codex"}, Result: execx.Result{Stdout: []byte("codex: function\n")}},
			{Prefix: []string{"zsh", "-ic", "whence -w claude"}, Result: execx.Result{Stdout: []byte("claude: function\n")}},
		}}
		cli := t.TempDir()
		hs.d.Agents = agents.New(agents.Deps{Herdr: sh, Store: hs.st, Runner: run, Config: hs.cfg, Layout: hs.layout,
			Clock: hs.clock.Now, CodexConfig: cli + "/codex.toml", ClaudeConfig: cli + "/claude.json",
			ClaudeSettings: cli + "/settings.json", ClaudeDir: cli, CodexHome: cli,
			Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }})
	})
	return h, sh
}

// stuckFixer gives reviewed PR #n the fixer's session of an earlier fix
// loop, live in the PR's workspace, its agent deaf to ctrl+c and the exit
// keys, and a shell no session owns in the same workspace (the operator's),
// as on talkable#11932; it returns the fixer's session.
func stuckFixer(t *testing.T, h *harness, sh *stuckHerdr, n int) store.Session {
	t.Helper()
	pr := h.pr(n)
	judge, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, "codex-judge")
	if err != nil {
		t.Fatal(err)
	}
	ws, cwd := deref(judge.HerdrWorkspaceID), deref(judge.Cwd)
	sh.mu.Lock()
	fix := sh.newPaneLocked(ws, cwd)
	sh.newPaneLocked(ws, cwd) // the operator's shell
	sh.stuck[fix.ID] = true
	sh.mu.Unlock()
	name := agents.AgentName("talkable/talkable", n, "claude-fix")
	sh.addAgent(fix.ID, name, "claude", herdr.StatusIdle)
	s, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: "claude-fix", AgentName: &name, AgentKind: new("claude"),
		HerdrPaneID: &fix.ID, HerdrWorkspaceID: &ws, Cwd: &cwd, State: store.SessionLive})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// freshParkBusy is PR #n's count of fresh-start parks an agent did not quit
// for ("" = none).
func freshParkBusy(h *harness, n int) string {
	v, _ := kvValue(h, kvPRFreshParkBusy(h.pr(n).ID))
	return v
}

// TestAFreshStartClosesAnAgentThatWillNotQuit: `magnum review --fresh`
// parks every agent of the PR first. On talkable#11932 the fixer's Claude
// Code stayed in its pane through ctrl+c, so the setup failed as a busy
// wait every 90 seconds for 40 minutes. The first such failure still waits;
// the second parks in the hard mode: the exit keys, a second wait, then the
// fixer's pane alone is closed (a shell no session owns shares the
// workspace), its session closed, pr.park_forced recorded, and the round
// starts in fresh sessions.
func TestAFreshStartClosesAnAgentThatWillNotQuit(t *testing.T) {
	h, sh := newStuckHarness(t)
	h.reviewedPR(2, "b1")
	fixer := stuckFixer(t, h, sh, 2)
	fixPane := deref(fixer.HerdrPaneID)
	startsBefore := len(sh.startNames())

	h.advance(time.Minute)
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Fresh: true})
	h.tick()
	if got := len(h.rd.all()); got != 1 {
		t.Fatalf("rounds = %d, want the fresh one still waiting", got)
	}
	pr := h.pr(2)
	if !strings.Contains(deref(pr.LastError), agents.ErrBusy.Error()) || pr.Attempts != 0 || freshParkBusy(h, 2) != "1" {
		t.Fatalf("after the first try: last_error %q, attempts %d, busy count %q: want an uncharged busy wait counted once",
			deref(pr.LastError), pr.Attempts, freshParkBusy(h, 2))
	}
	if got := sh.keysTo(fixPane); !slices.Equal(got, []string{"ctrl+c", "ctrl+c"}) || len(sh.closedPanes()) != 0 {
		t.Fatalf("first try: keys %q, closed panes %v: want two ctrl+c, nothing closed", got, sh.closedPanes())
	}

	h.advance(busyRetry + time.Second)
	h.tick()
	want := []string{"ctrl+c", "ctrl+c", "ctrl+c", "ctrl+c", "/+e+x+i+t+enter"}
	if got := sh.keysTo(fixPane); !slices.Equal(got, want) {
		t.Fatalf("keys to the fixer's pane = %q, want %q", got, want)
	}
	if got := sh.closedPanes(); !slices.Equal(got, []string{fixPane}) || len(sh.closedIDs()) != 0 {
		t.Fatalf("closed panes %v, workspaces %v: want the fixer's pane alone", got, sh.closedIDs())
	}
	got, err := h.st.SessionByID(h.ctx, fixer.ID)
	if err != nil || got.State != store.SessionClosed || got.ClosedAt == nil {
		t.Fatalf("fixer session = %+v, %v: want closed", got, err)
	}
	if evs := h.events(agents.EventParkForced); len(evs) != 1 || evs[0].Level != "info" ||
		!strings.Contains(evs[0].Message, "claude-fix") || !strings.Contains(evs[0].Message, fixPane) {
		t.Fatalf("%s events = %+v", agents.EventParkForced, evs)
	}
	if got := len(h.rd.all()); got != 2 {
		t.Fatalf("rounds = %d, want the fresh round to run", got)
	}
	starts := sh.startOpts()
	for _, o := range starts[startsBefore:] {
		if slices.Contains(o.Args, "resume") || slices.Contains(o.Args, "--resume") {
			t.Fatalf("%s resumed a conversation (%q): want a fresh session", o.Name, o.Args)
		}
	}
	if len(starts) == startsBefore {
		t.Fatal("no agent started for the fresh round")
	}
	if v := freshParkBusy(h, 2); v != "" {
		t.Fatalf("busy count after the round started = %q, want none", v)
	}
}

// TestAFixLoopInFlightIsNeverForced: a park refused because a run of the PR
// is in flight (a fixer or a round at work) is a plain wait on every try,
// never a hard park: that agent works, it is not stuck.
func TestAFixLoopInFlightIsNeverForced(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.failParks(fmt.Errorf("agents: park pr 2: run r-fix is working: %w", agents.ErrBusy))
	h.advance(time.Minute)
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Fresh: true})
	h.tick()
	for range 3 {
		h.advance(busyRetry + time.Second)
		h.tick()
	}
	h.tick() // records the wait of the last try
	pr := h.pr(2)
	if n := h.ag.count(fmt.Sprintf("park:%d", pr.ID)); n < 4 {
		t.Fatalf("parks = %d, want a try per retry: %v", n, h.ag.all())
	}
	if n := h.ag.count("park_hard:"); n != 0 || freshParkBusy(h, 2) != "" {
		t.Fatalf("hard parks %d, busy count %q: want none", n, freshParkBusy(h, 2))
	}
	if w := waitOf(t, h, 2); w.Reason != WaitRetry || w.Count != 1 || w.Subject != "agent busy" {
		t.Fatalf("wait = %+v, want retry 1/3 agent busy", w)
	}
}

// TestAbortForcesTheParkOnItsLastTry: `magnum abort` tries the park three
// times while an agent does not quit, the last one in the hard mode, so a
// stop hands the PR back even when an agent would not leave its pane; the
// fresh park's busy count goes with the abort.
func TestAbortForcesTheParkOnItsLastTry(t *testing.T) {
	h := newHarness(t)
	pr := h.runningRound(7, "h7", false)
	h.failParks(quitTimedOut(7))
	if err := h.st.SetKV(h.ctx, kvPRFreshParkBusy(pr.ID), "1"); err != nil {
		t.Fatal(err)
	}
	before := len(h.ag.all())
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "7"}})
	h.tick()
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "sessions parked") {
		t.Fatalf("abort: %s %q", r.State, deref(r.Result))
	}
	var parks []string
	for _, c := range h.ag.all()[before:] {
		if strings.HasPrefix(c, "park") {
			parks = append(parks, c)
		}
	}
	n := fmt.Sprint(pr.ID)
	if want := []string{"park:" + n, "park:" + n, "park_hard:" + n}; !slices.Equal(parks, want) {
		t.Fatalf("parks = %q, want %q", parks, want)
	}
	if v := freshParkBusy(h, 7); v != "" {
		t.Fatalf("busy count after the abort = %q, want none", v)
	}
}

// TestTheBusyRetryLabelAdvances: a busy wait is never charged, so the
// retry label used to say 1/3 for good while the same agent refused to
// quit (40 minutes on talkable#11932). It counts the fresh park's busy
// tries: 1/3 after the first, 2/3 after the second, which the hard park
// could not heal either (herdr refused to close the pane).
func TestTheBusyRetryLabelAdvances(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.failParks(quitTimedOut(2))
	h.ag.mu.Lock()
	h.ag.parkHardErr = quitTimedOut(2)
	h.ag.mu.Unlock()
	h.advance(time.Minute)
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Fresh: true})
	h.tick()
	h.tick() // records the wait of the try
	now := h.clock.Now()
	if w := waitOf(t, h, 2); !strings.Contains(w.Short(now), "retry 1/3 agent busy") {
		t.Fatalf("after the first try: %q", w.Short(now))
	}
	h.advance(busyRetry + time.Second)
	h.tick()
	h.tick()
	now = h.clock.Now()
	w := waitOf(t, h, 2)
	if !strings.Contains(w.Short(now), "retry 2/3 agent busy") || !strings.Contains(w.Detail, "attempt 2") {
		t.Fatalf("after the second try: %q (%s)", w.Short(now), w.Detail)
	}
	if n := h.ag.count("park_hard:"); n != 1 {
		t.Fatalf("hard parks = %d, want the second try's", n)
	}
	if pr := h.pr(2); pr.Attempts != 0 {
		t.Fatalf("attempts = %d, want none charged", pr.Attempts)
	}
}

// TestAFreshParkCountGoesWhenThePRLeavesTheRoundStates: the busy count
// belongs to the round that waits; a PR no longer waiting for one (here
// reviewed, its request served some other way) loses it at the next tick,
// so a later fresh start begins with the plain park, while a PR still in
// line keeps its count.
func TestAFreshParkCountGoesWhenThePRLeavesTheRoundStates(t *testing.T) {
	h := newHarness(t)
	reviewed := h.reviewedPR(2, "b1")
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick() // #3 queued, in its push quiet period
	queued := h.wantState(3, store.PRQueued)
	for _, pr := range []store.PR{reviewed, queued} {
		if err := h.st.SetKV(h.ctx, kvPRFreshParkBusy(pr.ID), "1"); err != nil {
			t.Fatal(err)
		}
	}
	h.tick()
	if v := freshParkBusy(h, 2); v != "" {
		t.Fatalf("reviewed PR's busy count = %q, want none", v)
	}
	if v := freshParkBusy(h, 3); v != "1" {
		t.Fatalf("queued PR's busy count = %q, want it kept", v)
	}
}
