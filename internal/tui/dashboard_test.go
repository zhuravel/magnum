package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

var dashNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func dashData() StatusData {
	return StatusData{
		Daemon:   DaemonInfo{Running: true, PID: 4242, Uptime: "3h12m", Launchd: "running"},
		Activity: ActivityInfo{LastPoll: 12 * time.Second, LastTick: 2 * time.Second},
		GitHub:   GitHubInfo{Remaining: 4800, Limit: 5000, ResetIn: 42 * time.Minute},
		Rounds:   RoundsInfo{Active: 1, Max: 2, ActivePRs: []string{"talkable#1"}},
		Agents:   AgentsInfo{CodexWorking: 1, CodexMax: 2},
		Disk:     DiskInfo{FreeGB: 41.2, MinGB: 8},
		Pauses:   []Pause{{Key: "codex", Reason: "rate limited until 13:00", Fix: "magnum resume --tool codex"}},
		Slots: []SlotRow{
			{Name: "review1", Folder: "~/Projects/talkable.review1", PRRef: "talkable#1", PRState: "reviewing", SlotState: "busy", DBs: "3 1.2G", Disk: "4.1G"},
			{Name: "review2", Folder: "~/Projects/talkable.review2", SlotState: "free", DBs: "-", Disk: "3.9G"},
		},
		Queue: []PRRow{
			{Ref: "talkable#7", Title: "Fix the referral widget", Author: "@ann", State: "queued", Next: "review when quiet", Age: "5m", URL: "https://github.com/talkable/talkable/pull/7"},
			{Ref: "talkable#1", Title: "Bump rails", Author: "@bob", State: "reviewing", Next: "judge", Age: "1h", URL: "https://github.com/talkable/talkable/pull/1"},
		},
		Attention:   []AttentionRow{{Subject: "talkable#3", Kind: "needs_attention", Message: "judge failed", Fix: "magnum review talkable#3"}},
		Manual:      []ManualRow{{Folder: "~/Projects/talkable.repo2", Branch: "feature/x", PRRef: "#9?", GitHub: "OPEN", DBs: "2 300M", Agents: "claude:idle", Disk: "2.0G"}},
		GeneratedAt: dashNow.Add(-3 * time.Second),
	}
}

type fakeSource struct {
	mu    sync.Mutex
	calls int
	data  StatusData
	err   error
}

func (f *fakeSource) Gather(context.Context) (StatusData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.data, f.err
}

func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeActions struct {
	mu    sync.Mutex
	calls []string
	err   error
	// queued are the requests the next actions report (then cleared);
	// answers are what Requests reads back, by id.
	queued  []Request
	answers map[int64]Request
	asked   [][]int64
}

func (f *fakeActions) record(s string) (ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	reqs := f.queued
	f.queued = nil
	if f.err != nil {
		return ActionResult{Text: "progress line\n" + f.err.Error(), Requests: reqs}, f.err
	}
	return ActionResult{Text: "progress line\n" + s + " ok\n", Requests: reqs}, nil
}

func (f *fakeActions) Open(_ context.Context, ref string) (ActionResult, error) {
	return f.record("open " + ref)
}
func (f *fakeActions) Review(_ context.Context, ref string, o ReviewOpts) (ActionResult, error) {
	return f.record(fmt.Sprintf("review %s fresh=%t simplify=%t", ref, o.Fresh, o.Simplify))
}
func (f *fakeActions) Pin(_ context.Context, ref string) (ActionResult, error) {
	return f.record("pin " + ref)
}
func (f *fakeActions) Unpin(_ context.Context, ref string) (ActionResult, error) {
	return f.record("unpin " + ref)
}
func (f *fakeActions) Release(_ context.Context, ref string) (ActionResult, error) {
	return f.record("release " + ref)
}
func (f *fakeActions) Mute(_ context.Context, ref string) (ActionResult, error) {
	return f.record("mute " + ref)
}
func (f *fakeActions) Unmute(_ context.Context, ref string) (ActionResult, error) {
	return f.record("unmute " + ref)
}
func (f *fakeActions) Abort(_ context.Context, ref string) (ActionResult, error) {
	return f.record("abort " + ref)
}
func (f *fakeActions) Approve(_ context.Context, ref string) (ActionResult, error) {
	return f.record("approve " + ref)
}
func (f *fakeActions) RequestChanges(_ context.Context, ref string) (ActionResult, error) {
	return f.record("request-changes " + ref)
}
func (f *fakeActions) Ignore(_ context.Context, ref string) (ActionResult, error) {
	return f.record("ignore " + ref)
}
func (f *fakeActions) Attention(context.Context) (ActionResult, error) { return f.record("attention") }
func (f *fakeActions) OpenBrowser(_ context.Context, url string) error {
	_, err := f.record("browser " + url)
	return err
}
func (f *fakeActions) Requests(_ context.Context, ids []int64) ([]Request, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, ids)
	var out []Request
	for _, id := range ids {
		if q, ok := f.answers[id]; ok {
			out = append(out, q)
		}
	}
	return out, nil
}

func (f *fakeActions) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

// newDash builds a dashboard sized w x h that already received dashData.
func newDash(t *testing.T, w, h int) (dashboardModel, *fakeSource, *fakeActions) {
	t.Helper()
	src := &fakeSource{data: dashData()}
	act := &fakeActions{}
	m := newDashboardModel(context.Background(), src, act, DashboardOptions{Now: func() time.Time { return dashNow }})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: w, Height: h}, dashDataMsg{data: src.data})
	return m, src, act
}

// dashAct presses keys, runs the action command they return and feeds its
// result back, returning the model and the follow-up messages.
func dashAct(t *testing.T, m dashboardModel, names ...string) (dashboardModel, []tea.Msg) {
	t.Helper()
	m, cmd := send(t, m, keys(names...)...)
	var follow []tea.Msg
	for _, msg := range execCmd(cmd) {
		if am, ok := msg.(actionDoneMsg); ok {
			var next tea.Cmd
			m, next = send(t, m, am)
			follow = append(follow, execCmd(next)...)
		}
	}
	return m, follow
}

func TestDashboardInitGathersAndShowsLoading(t *testing.T) {
	src := &fakeSource{data: dashData()}
	m := newDashboardModel(context.Background(), src, nil, DashboardOptions{})
	mustContain(t, viewOf(m), "magnum status", "loading status")
	msgs := execCmd(m.Init())
	var got *dashDataMsg
	for _, msg := range msgs {
		if d, ok := msg.(dashDataMsg); ok {
			got = &d
		}
	}
	if got == nil || src.count() != 1 {
		t.Fatalf("Init did not gather once: msgs %#v, calls %d", msgs, src.count())
	}
	if m.opts.Refresh != defaultRefresh {
		t.Errorf("default refresh = %v, want %v", m.opts.Refresh, defaultRefresh)
	}
	if fast := newDashboardModel(context.Background(), src, nil, DashboardOptions{Refresh: time.Millisecond}); fast.opts.Refresh != minRefresh {
		t.Errorf("refresh not clamped: %v", fast.opts.Refresh)
	}
}

func TestDashboardRendersHeaderAndSections(t *testing.T) {
	m, _, _ := newDash(t, 140, 50)
	v := viewOf(m)
	mustContain(t, v,
		"updated 3s ago",
		"daemon:", "running (pid 4242, up 3h12m) · launchd running",
		"last poll 12s ago · last tick 2s ago",
		"4800/5000 points left · resets in 42m",
		"1/2 active (talkable#1) · agents: codex 1/2, claude 0 working",
		"41.2 GB free (min 8 GB)",
		"PAUSED codex: rate limited until 13:00", "fix: magnum resume --tool codex",
		"SLOTS (2)", "FOLDER", "~/Projects/talkable.review1", "talkable#1 reviewing",
		"QUEUE (2)", "Fix the referral widget", "review when quiet",
		"ATTENTION (1)", "talkable#3 [needs_attention]: judge failed",
		"MANUAL WORKTREES (1 hidden, w shows them)",
		"› review1",
	)
	mustNotContain(t, v, "feature/x")
}

// The header sums the repository notes up in a line of its own, only when
// a repository has notes.
func TestDashboardHeaderShowsTheNotesLine(t *testing.T) {
	m, _, _ := newDash(t, 140, 50)
	mustNotContain(t, viewOf(m), "notes:")
	d := StatusData{Notes: "4 repos · 2 proposals to review (talkable/talkable, example/api stale) · 1 over limit"}
	m, _ = send(t, m, dashDataMsg{data: d})
	mustContain(t, viewOf(m), "notes:    4 repos · 2 proposals to review (talkable/talkable, example/api stale) · 1 over limit")
}

func TestDashboardHeaderUnknownsAndWarnings(t *testing.T) {
	m, _, _ := newDash(t, 120, 40)
	d := StatusData{Agents: AgentsInfo{Error: "herdr unreachable"}, Disk: DiskInfo{FreeGB: 3, MinGB: 8}, Warnings: []string{"mysql: connection refused"}}
	m, _ = send(t, m, dashDataMsg{data: d})
	mustContain(t, viewOf(m), "not running", "last poll never", "rate budget unknown", "agents: herdr unreachable",
		"LOW: provisioning refused", "pauses:   none", "none (`magnum slots provision`", "QUEUE (0)", "warning: mysql: connection refused")
}

func TestDashboardCursorCrossesSectionsAndOpens(t *testing.T) {
	m, src, act := newDash(t, 140, 50)
	m, _ = send(t, m, keys("j", "j")...)
	mustContain(t, viewOf(m), "› talkable#7")
	before := src.count()
	m, follow := dashAct(t, m, "enter")
	if act.last() != "open talkable#7" {
		t.Fatalf("enter called %q", act.last())
	}
	mustContain(t, viewOf(m), "open talkable#7 ok")
	mustNotContain(t, viewOf(m), "progress line")
	var refreshed bool
	for _, msg := range follow {
		if _, ok := msg.(dashDataMsg); ok {
			refreshed = true
		}
	}
	if !refreshed || src.count() != before+1 {
		t.Fatalf("no refresh after the action (calls %d -> %d)", before, src.count())
	}
	m, _ = dashAct(t, m, "k", "k", "o")
	if act.last() != "open talkable#1" {
		t.Fatalf("o on the first slot called %q", act.last())
	}
	m, _ = send(t, m, keys("j", "j", "j", "j", "j")...)
	mustContain(t, viewOf(m), "› talkable#1  reviewing  judge")
	m, _ = send(t, m, keys("home")...)
	mustContain(t, viewOf(m), "› review1")
}

func TestDashboardReviewVariants(t *testing.T) {
	m, _, act := newDash(t, 140, 50)
	for key, want := range map[string]string{
		"j j r y": "review talkable#7 fresh=false simplify=false", // talkable#1 runs a round: r is refused there
		"j j R y": "review talkable#7 fresh=true simplify=false",
		"j j i y": "review talkable#7 fresh=false simplify=true",
		"M y":     "mute talkable#1",
		"U y":     "unmute talkable#1",
		"K y":     "abort talkable#1",
		"I y":     "ignore talkable#1",
		"p":       "pin talkable#1",
		"u":       "unpin talkable#1",
		"a":       "attention",
		"b":       "browser https://github.com/talkable/talkable/pull/1",
	} {
		dashAct(t, m, strings.Fields(key)...)
		if act.last() != want {
			t.Errorf("%s called %q, want %q", key, act.last(), want)
		}
	}
	// s is not simplify here (on the board it sorts): it does nothing
	act.calls = nil
	if s, cmd := send(t, m, keyMsg("s")); s.confirm != nil || cmd != nil || len(act.calls) != 0 {
		t.Errorf("s asked or acted on the dashboard: %v", act.calls)
	}
}

func TestDashboardEmptySlot(t *testing.T) {
	m, _, act := newDash(t, 140, 50)
	m, _ = dashAct(t, m, "j", "enter")
	if len(act.calls) != 0 {
		t.Fatalf("enter on an empty slot called %v", act.calls)
	}
	mustContain(t, viewOf(m), "slot review2 holds no PR")
	m, _ = dashAct(t, m, "p")
	if act.last() != "pin review2" {
		t.Fatalf("p on an empty slot called %q", act.last())
	}
	m, _ = dashAct(t, m, "b")
	mustContain(t, viewOf(m), "no PR URL for review2")
}

func TestDashboardReleaseConfirms(t *testing.T) {
	m, _, act := newDash(t, 140, 50)
	m, _ = send(t, m, keys("x")...) // slot review1 holds talkable#1
	mustContain(t, viewOf(m), "Release talkable#1: hand back its slot now, sessions parked and worktree reset? y/N",
		"y confirms, any other key cancels")
	m, _ = dashAct(t, m, "n")
	if len(act.calls) != 0 {
		t.Fatalf("n released: %v", act.calls)
	}
	mustContain(t, viewOf(m), "release talkable#1 cancelled")
	m, _ = dashAct(t, m, "x", "y")
	if act.last() != "release talkable#1" {
		t.Fatalf("x y called %q", act.last())
	}
}

func TestDashboardActionErrorsAndBusy(t *testing.T) {
	m, _, act := newDash(t, 140, 50)
	act.err = errors.New("daemon not running")
	m, _ = dashAct(t, m, "j", "j", "r", "y") // talkable#7
	mustContain(t, viewOf(m), "review talkable#7: daemon not running")

	m, _ = send(t, m, keys("p")...) // left running: the command is not executed
	mustContain(t, viewOf(m), "pin talkable#7…")
	m, _ = send(t, m, keys("u")...)
	mustContain(t, viewOf(m), "still running: pin talkable#7")
	m, _ = send(t, m, keys("R")...) // busy: fails at once instead of asking
	if m.confirm != nil {
		t.Error("R asked while an action was running")
	}
	mustContain(t, viewOf(m), "still running: pin talkable#7")

	none := newDashboardModel(context.Background(), &fakeSource{}, nil, DashboardOptions{})
	none, _ = send(t, none, dashDataMsg{data: dashData()}, keyMsg("r"))
	mustContain(t, viewOf(none), "actions are not available")
}

func TestDashboardFlashExpires(t *testing.T) {
	m, _, _ := newDash(t, 140, 50)
	m, _ = dashAct(t, m, "a")
	mustContain(t, viewOf(m), "attention ok")
	m, _ = send(t, m, flashExpireMsg{seq: m.flashSeq - 1})
	mustContain(t, viewOf(m), "attention ok")
	m, _ = send(t, m, flashExpireMsg{seq: m.flashSeq})
	mustNotContain(t, viewOf(m), "attention ok")
}

func TestDashboardManualToggleAndHelp(t *testing.T) {
	m, _, _ := newDash(t, 140, 50)
	m, _ = send(t, m, keys("w")...)
	mustContain(t, viewOf(m), "MANUAL WORKTREES (1)", "feature/x", "claude:idle")
	m, _ = send(t, m, keys("w", "?")...)
	v := viewOf(m)
	mustContain(t, v, "Keys", "fresh review in new agent sessions (asks y/N)", "review now (asks y/N)",
		"release (asks y/N)", "answer yes; any other key, enter too, cancels", "g, ctrl+r", "refresh now (F5 too)", "close this help")
	mustNotContain(t, v, "SLOTS (2)")
	m, cmd := send(t, m, keys("esc")...)
	if isQuit(execCmd(cmd)) {
		t.Fatal("esc in help quit")
	}
	mustContain(t, viewOf(m), "SLOTS (2)")
	opened := newDashboardModel(context.Background(), &fakeSource{}, nil, DashboardOptions{ShowManual: true})
	opened, _ = send(t, opened, tea.WindowSizeMsg{Width: 140, Height: 50}, dashDataMsg{data: dashData()})
	mustContain(t, viewOf(opened), "feature/x")
}

func TestDashboardRefreshErrorKeepsData(t *testing.T) {
	m, _, _ := newDash(t, 140, 50)
	m, _ = send(t, m, dashDataMsg{err: errors.New("store locked")})
	mustContain(t, viewOf(m), "refresh failed: store locked", "SLOTS (2)", "Fix the referral widget")

	first := newDashboardModel(context.Background(), &fakeSource{}, nil, DashboardOptions{})
	first, _ = send(t, first, dashDataMsg{err: errors.New("no store")})
	mustContain(t, viewOf(first), "could not load status: no store")
}

func TestDashboardTickAndManualRefreshDoNotOverlap(t *testing.T) {
	m, src, _ := newDash(t, 140, 50)
	m, cmd := send(t, m, keyMsg("g"))
	if !m.loading || cmd == nil {
		t.Fatal("g did not start a refresh")
	}
	m, cmd = send(t, m, dashTickMsg{}) // already loading: only the next tick
	for _, msg := range execCmd(cmd) {
		if _, ok := msg.(dashDataMsg); ok {
			t.Fatal("tick gathered while a gather was in flight")
		}
	}
	if src.count() != 0 {
		t.Fatalf("gathered %d times without running the command", src.count())
	}
	mustContain(t, viewOf(m), "magnum status")
}

func TestDashboardKeepsSelectionAcrossRefresh(t *testing.T) {
	m, _, _ := newDash(t, 140, 50)
	m, _ = send(t, m, keys("j", "j", "j")...) // talkable#1 in the queue
	d := dashData()
	d.Queue = []PRRow{{Ref: "talkable#9", State: "queued"}, d.Queue[1], d.Queue[0]}
	m, _ = send(t, m, dashDataMsg{data: d})
	mustContain(t, viewOf(m), "› talkable#1  reviewing")
	d.Queue = d.Queue[:1]
	m, _ = send(t, m, dashDataMsg{data: d})
	mustContain(t, viewOf(m), "› talkable#9")
}

func TestDashboardScrollsToKeepCursorVisible(t *testing.T) {
	m, _, _ := newDash(t, 100, 18)
	d := dashData()
	d.Queue = nil
	for i := range 30 {
		d.Queue = append(d.Queue, PRRow{Ref: fmt.Sprintf("talkable#%d", 100+i), State: "queued", Title: "t"})
	}
	m, _ = send(t, m, dashDataMsg{data: d})
	m, _ = send(t, m, keys("end")...)
	v := viewOf(m)
	mustContain(t, v, "› talkable#129", "lines ")
	mustNotContain(t, v, "SLOTS (2)")
	if n := len(strings.Split(v, "\n")); n > 18 {
		t.Fatalf("view is %d lines, want <= 18", n)
	}
	m, _ = send(t, m, keys("home")...)
	mustContain(t, viewOf(m), "› review1", "SLOTS (2)")
}

func TestDashboardNarrowWidthFits(t *testing.T) {
	m, _, _ := newDash(t, 60, 40)
	mustContain(t, viewOf(m), "? help · q quit")
	m, _ = send(t, m, keys("w")...)
	if w := maxLineWidth(viewOf(m)); w > 60 {
		t.Fatalf("widest line %d > 60:\n%s", w, viewOf(m))
	}
	m, _ = send(t, m, keys("?")...)
	if w := maxLineWidth(viewOf(m)); w > 60 {
		t.Fatalf("help: widest line %d > 60:\n%s", w, viewOf(m))
	}
}

func TestDashboardQuitKeys(t *testing.T) {
	for _, k := range []string{"q", "esc", "ctrl+c"} {
		m, _, _ := newDash(t, 100, 30)
		_, cmd := send(t, m, keyMsg(k))
		if !isQuit(execCmd(cmd)) {
			t.Errorf("%s did not quit", k)
		}
	}
}

func TestRunDashboardEndsWithContext(t *testing.T) {
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close(); r.Close() })
	old := extraProgramOptions
	extraProgramOptions = []tea.ProgramOption{tea.WithInput(r), tea.WithOutput(io.Discard), tea.WithoutRenderer()}
	t.Cleanup(func() { extraProgramOptions = old })

	src := &fakeSource{data: dashData()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunDashboard(ctx, src, nil, DashboardOptions{Refresh: 200 * time.Millisecond}) }()
	deadline := time.After(5 * time.Second)
	for src.count() < 2 {
		select {
		case <-deadline:
			t.Fatal("the dashboard did not refresh")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunDashboard after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunDashboard did not return after ctx ended")
	}
	if err := RunDashboard(context.Background(), nil, nil, DashboardOptions{}); err == nil {
		t.Fatal("RunDashboard without a source did not fail")
	}
}

func TestDashboardNamesConfiguredKindsAndJudge(t *testing.T) {
	d := dashData()
	d.Agents.Other = []KindCount{{Kind: "droid", Working: 2}}
	m := newDashboardModel(context.Background(), &fakeSource{}, nil, DashboardOptions{Judge: "omp-judge", Now: func() time.Time { return dashNow }})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 140, Height: 50}, dashDataMsg{data: d})
	mustContain(t, viewOf(m), "agents: codex 1/2, claude 0, droid 2 working")
	m, _ = send(t, m, keys("?")...)
	mustContain(t, viewOf(m), "open the PR's omp-judge pane")

	plain := newDashboardModel(context.Background(), &fakeSource{}, nil, DashboardOptions{})
	plain, _ = send(t, plain, tea.WindowSizeMsg{Width: 140, Height: 50}, dashDataMsg{data: dashData()})
	plain, _ = send(t, plain, keys("?")...)
	mustContain(t, viewOf(plain), "open the PR's judge pane")
}

// The review keys only ask; the question says what the round does and
// what the PR's state tells (the dashboard knows no heads).
func TestDashboardReviewKeysAsk(t *testing.T) {
	data := dashData()
	data.Queue[0].State, data.Queue[0].Next = "reviewed", "watching for new pushes"
	data.Queue = append(data.Queue, PRRow{Ref: "talkable#9", State: "rereview_pending", Next: "eligible in 4m"})
	for _, k := range []string{"r", "R", "i", "M", "U"} {
		m, src, act := newDash(t, 220, 50)
		m, _ = send(t, m, dashDataMsg{data: data})
		m, cmd := send(t, m, keys("j", "j", k)...) // talkable#7, reviewed
		if len(execCmd(cmd)) != 0 || len(act.calls) != 0 || m.confirm == nil {
			t.Fatalf("%s alone acted (%v) or did not ask", k, act.calls)
		}
		mustContain(t, viewOf(m), "talkable#7", "y/N", "y confirms, any other key cancels")
		before := src.count()
		if _, follow := dashAct(t, m, "y"); len(act.calls) != 1 || !hasMsg[dashDataMsg](follow) || src.count() != before+1 {
			t.Errorf("%s then y called %v, want one call and a refresh", k, act.calls)
		}
		for _, no := range []string{"enter", "n", "esc", "ctrl+r"} {
			c, _ := send(t, m, keyMsg(no)) // run would set busy; the command is only the flash timer
			if len(act.calls) != 1 || c.confirm != nil || c.busy != "" || c.loading {
				t.Errorf("%s then %s acted or kept asking: %v", k, no, act.calls)
			}
			mustContain(t, viewOf(c), "cancelled")
		}
	}

	m, _, _ := newDash(t, 220, 50)
	m, _ = send(t, m, dashDataMsg{data: data})
	for _, c := range []struct {
		keys []string
		want string
	}{
		{[]string{"j", "j", "R"}, "Fresh review of talkable#7 in new agent sessions (no new commits since the last review)?"},
		{[]string{"j", "j", "i"}, "Simplify review of talkable#7, also running the simplify role (no new commits since the last review)?"},
		{[]string{"j", "j", "j", "j", "r"}, "Review talkable#9 now (reviewed before, re-review pending, next: eligible in 4m)?"},
		{[]string{"j", "x"}, "Release review2: hand back its slot now, sessions parked and worktree reset?"},
	} {
		got, _ := send(t, m, keys(c.keys...)...)
		if got.confirm == nil || got.confirm.question != c.want {
			t.Errorf("keys %v asked %+v, want %q", c.keys, got.confirm, c.want)
			continue
		}
		mustContain(t, viewOf(got), c.want+" y/N")
	}
}

// ctrl+r and F5 refresh like g and do nothing else.
func TestDashboardRefreshKeysOnlyRefresh(t *testing.T) {
	for _, k := range []tea.KeyPressMsg{keyMsg("ctrl+r"), {Code: tea.KeyF5}} {
		m, src, act := newDash(t, 140, 50)
		before := src.count()
		m, cmd := send(t, m, k)
		msgs := execCmd(cmd)
		if !hasMsg[dashDataMsg](msgs) || src.count() != before+1 {
			t.Errorf("%s did not refresh", k)
		}
		if hasMsg[actionDoneMsg](msgs) || len(act.calls) != 0 || m.confirm != nil || m.showHelp {
			t.Errorf("%s did more than refresh: calls %v", k, act.calls)
		}
	}
}

// With the heads known the dashboard says what the board says: no new
// commits when the head is the reviewed one, else how many.
func TestDashboardQuestionUsesReviewFacts(t *testing.T) {
	data := dashData()
	data.Queue[0].State = "reviewed"
	data.Queue[0].Review = &ReviewFacts{HeadSHA: "ffa3270aaa", ReviewedSHA: "ffa3270aaa", ReviewedAt: dashNow.Add(-20 * time.Minute), ReviewedBy: "zhuravel[bot]"}
	data.Queue[1].State = "rereview_pending" // talkable#1 waits for its round (a running one refuses r)
	data.Queue[1].Review = &ReviewFacts{HeadSHA: "9b1c2d3eee", ReviewedSHA: "ffa3270aaa", ReviewedAt: dashNow.Add(-time.Hour),
		SinceReview: &ReviewDelta{Base: "reviewed", Commits: 3}}
	data.Queue = append(data.Queue, PRRow{Ref: "talkable#9", State: "rereview_pending",
		Review: &ReviewFacts{HeadSHA: "1234567aaa", ReviewedSHA: "ffa3270aaa", SinceReview: &ReviewDelta{Base: "reviewed", Commits: 1}}})
	m, _, _ := newDash(t, 220, 50)
	m, _ = send(t, m, dashDataMsg{data: data})
	for _, c := range []struct {
		keys []string
		want string
	}{
		{[]string{"j", "j", "R"}, "Fresh review of talkable#7 in new agent sessions (no new commits since head ffa3270 was reviewed 20m ago by zhuravel[bot])?"},
		{[]string{"j", "j", "j", "j", "r"}, "Review talkable#9 now (1 commit since the last review of ffa3270, head 1234567)?"},
		// a slot row reads its queue row
		{[]string{"r"}, "Review talkable#1 now (3 commits since the last review of ffa3270 1h ago, head 9b1c2d3)?"},
	} {
		got, _ := send(t, m, keys(c.keys...)...)
		if got.confirm == nil || got.confirm.question != c.want {
			t.Errorf("keys %v asked %+v, want %q", c.keys, got.confirm, c.want)
			continue
		}
		mustContain(t, viewOf(got), c.want+" y/N")
	}
}

// The SLOT and PR columns keep the end of a long reference (the PR number),
// not its start.
func TestDashboardReferenceColumnsKeepTheTail(t *testing.T) {
	cols := []column{{title: "SLOT", min: 6, tail: true}, {title: "STATE", min: 6}}
	row := renderRowCols(cols, []string{"talkable/talkable-shopify-extensions#1234", "held"}, []int{16, 6})
	if !strings.HasPrefix(row, "…") || !strings.Contains(row, "#1234") {
		t.Fatalf("row = %q", row)
	}
	if got := fitTail("abc", 5); got != "abc  " {
		t.Fatalf("fitTail pads: %q", got)
	}
	if got := truncateTail("owner/repo#77", 4); got != "…#77" {
		t.Fatalf("truncateTail(4) = %q", got)
	}
}
