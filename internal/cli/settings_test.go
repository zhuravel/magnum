package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
	"github.com/zhuravel/magnum/internal/tui"
)

// settingsScreenCall is one run of the settings screen a test caught.
type settingsScreenCall struct {
	src  tui.SettingsSource
	rt   tui.SettingsRuntime
	opts tui.SettingsOptions
}

// catchSettingsScreen replaces the settings screen with one that records
// what it was handed and returns at once.
func catchSettingsScreen(t *testing.T) *[]settingsScreenCall {
	t.Helper()
	var calls []settingsScreenCall
	old := tuiSettings
	tuiSettings = func(_ context.Context, src tui.SettingsSource, rt tui.SettingsRuntime, o tui.SettingsOptions) error {
		calls = append(calls, settingsScreenCall{src: src, rt: rt, opts: o})
		return nil
	}
	t.Cleanup(func() { tuiSettings = old })
	return &calls
}

// Off a terminal `magnum settings` cannot show the screen, so it says
// where the file is and stops: no screen, no registry, nothing written.
func TestSettingsRefusesWithoutATerminalAndWritesNothing(t *testing.T) {
	storetest.Serial(t)
	h := newActHarness(t)
	calls := catchSettingsScreen(t)
	oldScreen, oldDeps := inspScreen, actNewDeps
	inspScreen = func(*Context) bool { return false }
	opened := 0
	actNewDeps = func(c *Context, m actMode) (*actDeps, error) { opened++; return oldDeps(c, m) }
	t.Cleanup(func() { inspScreen, actNewDeps = oldScreen, oldDeps })

	for _, args := range [][]string{nil, {"--watch", "1"}, {"--restore", "2"}} {
		h.out.Reset()
		h.errb.Reset()
		if code := h.cmd("settings", args...); code != 1 {
			t.Fatalf("settings %v: exit %d: %s", args, code, h.errb.String())
		}
		if got, want := h.errb.String(), "magnum settings needs a terminal; edit ~/.config/magnum/config.toml and run `magnum config`\n"; got != want {
			t.Errorf("settings %v: stderr %q, want %q", args, got, want)
		}
		if h.out.Len() != 0 {
			t.Errorf("settings %v printed %q", args, h.out.String())
		}
	}
	if len(*calls) != 0 || opened != 0 {
		t.Errorf("without a terminal the screen ran %d times and the act deps were built %d times", len(*calls), opened)
	}
}

// --watch N opens the screen on the N-th [[watch]] and --repo on a
// repository; the two name different scopes, so together they are refused.
func TestSettingsWatchAndRepoNameTheScope(t *testing.T) {
	storetest.Serial(t)
	h := newActHarness(t)
	onScreen(t)
	calls := catchSettingsScreen(t)

	for _, tc := range []struct {
		args  []string
		scope string
	}{
		{nil, ""},
		{[]string{"--watch", "2"}, "watch:2"},
		{[]string{"--repo", "talkable/widgets"}, "repo:talkable/widgets"},
	} {
		*calls = nil
		if code := h.cmd("settings", tc.args...); code != 0 {
			t.Fatalf("settings %v: exit %d: %s", tc.args, code, h.errb.String())
		}
		if len(*calls) != 1 || (*calls)[0].opts != (tui.SettingsOptions{Scope: tc.scope}) {
			t.Fatalf("settings %v: screen runs %+v, want scope %q", tc.args, *calls, tc.scope)
		}
		c := (*calls)[0]
		if _, ok := c.src.(*settingsSource); !ok {
			t.Errorf("settings %v: source %T, want the settings core's", tc.args, c.src)
		}
		if _, ok := c.rt.(screenRuntime); !ok {
			t.Errorf("settings %v: runtime %T", tc.args, c.rt)
		}
	}

	*calls = nil
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--watch", "1", "--repo", "talkable/widgets"}, "use either --watch or --repo"},
		{[]string{"--watch", "0"}, "--watch"},
		{[]string{"--watch", "-1"}, "--watch"},
		{[]string{"--repo", "widgets"}, "--repo"},
		{[]string{"--repo", "talkable/"}, "--repo"},
		{[]string{"talkable"}, "takes no arguments"},
	} {
		h.errb.Reset()
		if code := h.cmd("settings", tc.args...); code != 2 {
			t.Errorf("settings %v: exit %d, want a usage error", tc.args, code)
		}
		actContains(t, h.errb.String(), tc.want, "usage: magnum "+settingsUsage)
	}
	if len(*calls) != 0 {
		t.Errorf("a refused command ran the screen: %+v", *calls)
	}
}

// --restore N opens the save view of the N-th kept version; --history
// lists the kept versions, with no screen.
func TestSettingsRestoreReachesTheScreen(t *testing.T) {
	storetest.Serial(t)
	h := newActHarness(t)
	onScreen(t)
	calls := catchSettingsScreen(t)

	if code := h.cmd("settings", "--restore", "3", "--watch", "1"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if len(*calls) != 1 || (*calls)[0].opts != (tui.SettingsOptions{Scope: "watch:1", Restore: 3}) {
		t.Fatalf("screen runs %+v, want restore 3 on watch 1", *calls)
	}

	*calls = nil
	h.errb.Reset()
	if code := h.cmd("settings", "--restore", "0"); code != 2 || !strings.Contains(h.errb.String(), "--restore") {
		t.Errorf("--restore 0: exit %d %s", code, h.errb.String())
	}
	h.errb.Reset()
	if code := h.cmd("settings", "--history", "--restore", "1"); code != 2 {
		t.Errorf("--history with --restore: exit %d %s", code, h.errb.String())
	}
	if len(*calls) != 0 {
		t.Errorf("the screen ran: %+v", *calls)
	}
}

// The settings screen's Runtime section reads what `magnum status` lists:
// the automation pause, each agent kind's pause (a model limit is not one)
// and each watch owner's identity-leak pause, once per owner.
func TestScreenRuntimeReadsThePausesStatusLists(t *testing.T) {
	storetest.Serial(t)
	h := newActHarness(t)
	h.pid = 4242
	h.d.Cfg.Watches = []config.Watch{
		{Owner: "talkable", Include: []string{"talkable"}, Identity: "talkable-app", PollIdentity: "zhuravel", CloneRoot: h.home},
		{Owner: "example", Include: []string{"*"}, Identity: "zhuravel", PollIdentity: "zhuravel", CloneRoot: h.home},
		{Owner: "Talkable", Include: []string{"widgets"}, Identity: "zhuravel", PollIdentity: "zhuravel", CloneRoot: h.home},
	}
	set := func(k, v string) {
		t.Helper()
		if err := h.st.SetKV(h.ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	set(store.KVDaemonPausedReason, "lunch")
	set(store.KVDaemonPausedUntil, store.FormatTime(h.now.Add(2*time.Hour)))
	set(engine.KVDaemonPausedAt, store.FormatTime(h.now.Add(-time.Hour)))
	set(store.KVDaemonPaused, "1")
	set(store.KVToolPausedUntil("codex"), store.FormatTime(h.now.Add(time.Hour)))
	set(store.KVToolPausedReason("codex"), engine.BudgetPauseReason)
	set(store.KVToolPausedDetail("codex"), "Codex budget 97% used (hard cap 95%)")
	set(store.KVWatchPaused("example"), "a review of example/widgets#5 was posted as zhuravel\x1b[2J")
	seedModelLimits(t, h.st, "claude", map[string]time.Time{"fable": h.now.Add(3 * time.Hour)})

	acts := newScreenActions(h.c)
	t.Cleanup(func() { _ = acts.Close() })
	s, err := screenRuntime{acts}.Runtime(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Daemon {
		t.Error("a daemon runs (pid 4242)")
	}
	a := s.Automation
	if a.Name != "automation" || !a.Paused || !strings.HasPrefix(a.State, "paused: lunch since ") || !strings.Contains(a.State, " until ") ||
		!strings.Contains(a.Detail, "magnum resume") {
		t.Errorf("automation %+v", a)
	}

	var names []string
	kinds := map[string]tui.RuntimeEntry{}
	for _, k := range s.Kinds {
		names = append(names, k.Name)
		kinds[k.Name] = k
	}
	if want := rolesAllKinds(h.d.Cfg); !slices.Equal(names, want) {
		t.Errorf("kinds %v, want %v", names, want)
	}
	if c := kinds["codex"]; !c.Paused || !strings.HasPrefix(c.State, "paused: "+engine.BudgetPauseReason+" until ") ||
		strings.Contains(c.State, "97%") || !strings.Contains(c.Detail, "Codex budget 97% used") || !strings.Contains(c.Detail, "magnum resume --tool codex") {
		t.Errorf("codex %+v", c)
	}
	if c := kinds["claude"]; c.Paused || c.State != "no pause" || !strings.Contains(c.Detail, "fable limited until ") {
		t.Errorf("claude: a model limit is no pause of the kind, only its detail: %+v", c)
	}

	want := []tui.RuntimeEntry{
		{Name: "talkable", Label: "watch talkable", State: "no pause"}, // the screen numbers it from its scopes
		{Name: "example", Label: "watch example", Paused: true, State: "paused: identity leak"},
	}
	if len(s.Watches) != len(want) {
		t.Fatalf("watches %+v, want one per owner", s.Watches)
	}
	for i, w := range want {
		got := s.Watches[i]
		got.Detail = ""
		if got != w {
			t.Errorf("watch %d: %+v, want %+v", i, s.Watches[i], w)
		}
	}
	if d := s.Watches[1].Detail; !strings.Contains(d, "a review of example/widgets#5 was posted as zhuravel") ||
		!strings.Contains(d, "magnum resume --watch example") || strings.ContainsRune(d, '\x1b') {
		t.Errorf("the watch's detail must carry the leak and its fix, clean: %q", d)
	}

	h.pid = 0
	for _, k := range []string{store.KVDaemonPaused, store.KVToolPausedUntil("codex"), store.KVWatchPaused("example")} {
		if err := h.st.DeleteKV(h.ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	s, err = screenRuntime{acts}.Runtime(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Daemon || s.Automation.Paused || s.Automation.State != "running" {
		t.Errorf("nothing paused, no daemon: %+v", s)
	}
	for _, e := range append(slices.Clone(s.Kinds), s.Watches...) {
		if e.Paused {
			t.Errorf("%+v paused", e)
		}
	}

	// An infrastructure pause holds automation too, and the automation
	// row's r (`magnum resume`) lifts it.
	set(engine.KVInfraPausedUntil, store.FormatTime(h.now.Add(10*time.Minute)))
	set(engine.KVInfraPausedReason, "(ssh)")
	if s, err = (screenRuntime{acts}).Runtime(h.ctx); err != nil {
		t.Fatal(err)
	}
	if a := s.Automation; !a.Paused || !strings.HasPrefix(a.State, "paused: infrastructure failure (ssh) until ") || !strings.Contains(a.Detail, "magnum resume") {
		t.Errorf("an infrastructure pause: automation %+v", a)
	}
}

// The screen counts each agent kind's live sessions a restart takes (a
// pinned PR's left out), and its x sends what `magnum restart-sessions`
// sends, refused without a daemon.
func TestScreenRuntimeCountsAndRestartsAKindsLiveSessions(t *testing.T) {
	storetest.Serial(t)
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 12, store.PRPaused)
	pinned := h.seedPR("talkable/talkable", 13, store.PRReviewed)
	if err := h.st.UpdatePR(h.ctx, pinned.ID, func(u *store.PRUpdate) { u.Set("pinned", true) }); err != nil {
		t.Fatal(err)
	}
	for i, id := range []int64{pr.ID, pinned.ID} {
		s := h.session(id, "codex-judge", store.SessionLive, fmt.Sprintf("mg-judge-%d", i), "", "w1")
		if err := h.st.UpdateSession(h.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("agent_kind", "codex") }); err != nil {
			t.Fatal(err)
		}
	}
	acts := newScreenActions(h.c)
	t.Cleanup(func() { _ = acts.Close() })
	rt := screenRuntime{acts}
	s, err := rt.Runtime(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range s.Kinds {
		if want := map[string]int{"codex": 1}[k.Name]; k.Live != want {
			t.Errorf("%s: %d live sessions, want %d", k.Name, k.Live, want)
		}
	}

	if text, err := rt.RestartSessions(h.ctx, "codex"); err == nil || !strings.Contains(err.Error(), "no daemon is running") {
		t.Fatalf("without a daemon: %q, %v", text, err)
	}
	h.pid = 3
	answer := "restarting 1 live codex session: 1 park now, 0 after their turn; each resumes in a new process at its next prompt"
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, answer) }
	if text, err := rt.RestartSessions(h.ctx, "codex"); err != nil || text != answer {
		t.Fatalf("restart: %q, %v", text, err)
	}
	reqs := h.requests()
	if last := reqs[len(reqs)-1]; last.Kind != engine.ReqRestartSessions || actDecode[engine.RestartSessionsPayload](t, last.Payload).Tool != "codex" {
		t.Fatalf("requests %+v", reqs)
	}
}

// The screen's p and r send what `magnum pause` and `magnum resume` send:
// the registry writes without a daemon, its requests with one.
func TestScreenRuntimePausesAndResumesAsTheCommandsDo(t *testing.T) {
	storetest.Serial(t)
	h := newActHarness(t)
	acts := newScreenActions(h.c)
	t.Cleanup(func() { _ = acts.Close() })
	rt := screenRuntime{acts}

	text, err := rt.Pause(h.ctx, tui.PauseSpec{For: 2 * time.Hour, Reason: "lunch"})
	if err != nil {
		t.Fatal(err)
	}
	actContains(t, text, "paused automation until", "(lunch)", "no daemon is running")
	if v, _ := h.kv(store.KVDaemonPaused); v != "1" {
		t.Errorf("paused = %q", v)
	}
	if v, _ := h.kv(store.KVDaemonPausedReason); v != "lunch" {
		t.Errorf("reason = %q", v)
	}
	if v, _ := h.kv(store.KVDaemonPausedUntil); v != store.FormatTime(h.now.Add(2*time.Hour)) {
		t.Errorf("until = %q", v)
	}
	if h.out.Len() != 0 || h.errb.Len() != 0 {
		t.Errorf("the screen owns the terminal, yet it printed %q %q", h.out.String(), h.errb.String())
	}

	for _, p := range []tui.PauseSpec{{For: time.Hour, Until: "15:00"}, {For: -time.Hour}, {Until: "2020-01-01T00:00:00Z"}} {
		if text, err := rt.Pause(h.ctx, p); err == nil || strings.HasPrefix(err.Error(), "usage:") {
			t.Errorf("pause %+v: %q, %v", p, text, err)
		}
	}
	if v, _ := h.kv(store.KVDaemonPausedReason); v != "lunch" {
		t.Errorf("a refused pause changed the reason: %q", v)
	}

	if text, err := rt.Resume(h.ctx, tui.ResumeSpec{}); err != nil || !strings.Contains(text, "resumed automation") {
		t.Fatalf("resume: %q, %v", text, err)
	}
	if _, ok := h.kv(store.KVDaemonPaused); ok {
		t.Error("resume left the pause")
	}
	_ = h.st.SetKV(h.ctx, store.KVToolPausedUntil("codex"), store.FormatTime(h.now.Add(time.Hour)))
	_ = h.st.SetKV(h.ctx, store.KVToolPausedReason("codex"), "usage_limit")
	if text, err := rt.Resume(h.ctx, tui.ResumeSpec{Tool: "codex"}); err != nil || !strings.HasPrefix(text, "resumed codex") {
		t.Fatalf("resume codex: %q, %v", text, err)
	}
	if _, ok := h.kv(store.KVToolPausedUntil("codex")); ok {
		t.Error("resume codex left its pause")
	}
	_ = h.st.SetKV(h.ctx, store.KVWatchPaused("example"), "leak")
	if text, err := rt.Resume(h.ctx, tui.ResumeSpec{Watch: "example"}); err != nil || !strings.HasPrefix(text, "resumed watch example") {
		t.Fatalf("resume watch: %q, %v", text, err)
	}
	if _, ok := h.kv(store.KVWatchPaused("example")); ok {
		t.Error("resume watch left its pause")
	}
	if _, err := rt.Resume(h.ctx, tui.ResumeSpec{Tool: "all"}); err != nil {
		t.Errorf("resume all: %v", err)
	}
	if text, err := rt.Resume(h.ctx, tui.ResumeSpec{Tool: "gemini"}); err == nil || !strings.Contains(err.Error(), `unknown tool "gemini"`) {
		t.Errorf("unknown tool: %q, %v", text, err)
	}
	if text, err := rt.Resume(h.ctx, tui.ResumeSpec{Tool: "codex", Watch: "example"}); err == nil {
		t.Errorf("a kind and a watch at once: %q, want a refusal", text)
	}
	if len(h.requests()) != 0 {
		t.Errorf("queued requests without a daemon: %+v", h.requests())
	}

	// With a daemon the same calls become its requests.
	h.pid = 3
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, "resumed codex, claude") }
	start := h.now
	if text, err := rt.Pause(h.ctx, tui.PauseSpec{For: 2 * time.Hour, Reason: "lunch"}); err != nil || !strings.Contains(text, "paused automation until") {
		t.Fatalf("pause through the daemon: %q, %v", text, err)
	}
	if _, ok := h.kv(store.KVDaemonPaused); ok {
		t.Error("the screen wrote the pause although the daemon runs")
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqPause {
		t.Fatalf("requests %+v", reqs)
	}
	if p := actDecode[engine.PausePayload](t, reqs[0].Payload); p.Reason != "lunch" || p.Until == nil || !p.Until.Equal(start.Add(2*time.Hour)) {
		t.Fatalf("pause payload %+v", p)
	}
	if text, err := rt.Resume(h.ctx, tui.ResumeSpec{Tool: "all"}); err != nil || text != "resumed codex, claude" {
		t.Fatalf("resume through the daemon: %q, %v", text, err)
	}
	reqs = h.requests()
	if len(reqs) != 2 || reqs[1].Kind != engine.ReqResume {
		t.Fatalf("requests %+v", reqs)
	}
	if p := actDecode[engine.PausePayload](t, reqs[1].Payload); p.Tool != "all" || p.Watch != "" {
		t.Fatalf("resume payload %+v", p)
	}
}

// `,` on the dashboard and on the board opens one settings screen whose
// runtime shares the screens' actions, so the act deps are built once.
func TestTheScreensHandBothScreensOneSettingsScreen(t *testing.T) {
	storetest.Serial(t)
	f := newInspFixture(t)
	prsSeed(t, f)
	onScreen(t)
	var dashActs, boardActs tui.DashboardActions
	var dashSettings, boardSettings *tui.SettingsScreen
	oldDash, oldBoard := tuiDashboard, tuiPRBoard
	t.Cleanup(func() { tuiDashboard, tuiPRBoard = oldDash, oldBoard })
	tuiDashboard = func(_ context.Context, _ tui.DashboardSource, act tui.DashboardActions, o tui.DashboardOptions) error {
		dashActs, dashSettings = act, o.Settings
		return tui.ErrSwitchToBoard
	}
	tuiPRBoard = func(_ context.Context, _ tui.PRBoardSource, act tui.DashboardActions, o tui.PRBoardOptions) error {
		boardActs, boardSettings = act, o.Settings
		return nil
	}
	if code := f.run("status", "--watch"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if dashSettings == nil || dashSettings != boardSettings {
		t.Fatalf("both screens need the one settings screen: %v %v", dashSettings, boardSettings)
	}
	if _, ok := dashSettings.Source.(*settingsSource); !ok {
		t.Errorf("source %T, want the settings core's", dashSettings.Source)
	}
	rt, ok := dashSettings.Runtime.(screenRuntime)
	if !ok || rt.screenActions == nil || tui.DashboardActions(rt.screenActions) != dashActs || dashActs != boardActs {
		t.Errorf("the settings runtime must share the screens' actions: %T %v %v %v", dashSettings.Runtime, rt.screenActions, dashActs, boardActs)
	}
	if dashSettings.Options != (tui.SettingsOptions{}) {
		t.Errorf("`,` opens the global settings: %+v", dashSettings.Options)
	}
}
