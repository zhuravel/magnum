package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
	"github.com/zhuravel/magnum/internal/tui"
)

// toastHerdr is a herdr magnum's notify reaches, on fakes: the toasts it
// was asked to show, and the sidebar tokens reported to it by workspace.
// err fails every call, as a herdr that does not answer, and declined is
// the reason it gives for showing no toast ("" shows it); calls counts the
// toasts asked for. The rig's notifier has no runner unless a test gives it
// a fake one, so no osascript runs and no real toast can fire.
type toastHerdr struct {
	mu       sync.Mutex
	toasts   []shownToast
	tokens   map[string]map[string]string // workspace -> its tokens, as last reported
	reports  int
	calls    int
	err      error
	declined string
}

type shownToast struct{ Title, Body string }

func (f *toastHerdr) NotificationShow(_ context.Context, title, body string) (herdr.NotificationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return herdr.NotificationResult{}, f.err
	}
	if f.declined != "" {
		return herdr.NotificationResult{Reason: f.declined}, nil
	}
	f.toasts = append(f.toasts, shownToast{title, body})
	return herdr.NotificationResult{Shown: true, Reason: "shown"}, nil
}

func (f *toastHerdr) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *toastHerdr) WorkspaceReportMetadata(_ context.Context, ws, source string, tokens map[string]string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if source != notify.SidebarSource {
		return fmt.Errorf("source %q", source)
	}
	if f.tokens == nil {
		f.tokens = map[string]map[string]string{}
	}
	f.tokens[ws] = tokens
	f.reports++
	return nil
}

func (f *toastHerdr) shown() []shownToast {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.toasts)
}

// token is the factory's sidebar token on workspace ws as last reported,
// and whether one was.
func (f *toastHerdr) token(ws string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.tokens[ws]["factory"]
	return v, ok
}

func (f *toastHerdr) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// alertRig is the harness with magnum's notifier on fakes: your herdr for
// the toasts, the factory's for the sidebar tokens, and a store of
// magnum's schema (the factory's agents store, as the daemon wires it) for
// the toasts' dedupe. live is the pid of the screen that runs (0: none).
type alertRig struct {
	*harness
	toasts, side *toastHerdr
	dedupe       *store.Store
	live         atomic.Int64
}

// newAlertRig builds the rig; edit changes the toasts' notifier first.
func newAlertRig(t *testing.T, edit ...func(n *notify.Notifier)) *alertRig {
	t.Helper()
	r := &alertRig{toasts: &toastHerdr{}, side: &toastHerdr{}}
	r.harness = newHarness(t, func(d *Deps) {
		r.dedupe = storetest.Open(t, filepath.Join(t.TempDir(), "agents.db"))
		r.dedupe.Clock = d.Clock
		d.Notifier = &notify.Notifier{Herdr: r.toasts, Store: r.dedupe, Enabled: true}
		for _, f := range edit {
			f(d.Notifier)
		}
		d.Sidebar = (&notify.Notifier{Herdr: r.side}).Sidebar
	})
	r.seams()
	return r
}

// seams makes the stages idle (the tests are of what happens around them)
// and a screen live while its pid is r.live.
func (r *alertRig) seams() {
	for _, typ := range config.FactoryStageTypes {
		r.stage(typ, idleStage)
	}
	r.e.screenLive = func(f FocusPayload) bool { return f.Focused && f.PID > 0 && int64(f.PID) == r.live.Load() }
}

func (r *alertRig) restart() {
	r.t.Helper()
	r.harness.restart()
	r.seams()
}

// needsYou creates a task of key and moves it to NEEDS YOU, waiting on you
// for what, as a stage does (toOperator: its alert).
func (r *alertRig) needsYou(key, what string) registry.Task {
	r.t.Helper()
	t := r.task(key, registry.TaskActive, "investigate")
	r.e.toOperator(r.ctx, t, what, "a test")
	t = r.reload(t.ID)
	if t.State != registry.TaskNeedsYou {
		r.t.Fatalf("%s is %s, want needs_you", key, t.State)
	}
	return t
}

// backToWork moves a task that waits on you back to work, as your answer
// does.
func (r *alertRig) backToWork(t registry.Task) {
	r.t.Helper()
	if err := r.reg.TransitionTask(r.ctx, t.ID, []string{registry.TaskNeedsYou}, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
		u.Because("your answer")
	}); err != nil {
		r.t.Fatal(err)
	}
}

// focus has the screen of process pid report its focus now, and a tick
// take it.
func (r *alertRig) focus(focused bool, pid int) {
	r.t.Helper()
	if _, err := r.reg.EnqueueRequest(r.ctx, ReqFocus, FocusPayload{Focused: focused, PID: pid, At: r.clock.Now()}); err != nil {
		r.t.Fatal(err)
	}
	r.tick()
}

// after moves the clock by d and ticks.
func (r *alertRig) after(d time.Duration) {
	r.t.Helper()
	r.clock.Add(d)
	r.tick()
}

// wantToasts fails unless exactly want were shown, in order.
func (r *alertRig) wantToasts(when string, want ...shownToast) {
	r.t.Helper()
	if got := r.toasts.shown(); !slices.Equal(got, want) {
		r.t.Fatalf("%s: toasts %q, want %q", when, got, want)
	}
}

// texts are the texts of the events of kind of task id (0: the daemon's).
func (r *alertRig) texts(id int64, kind string) []string {
	r.t.Helper()
	var out []string
	for _, ev := range r.events(id, kind) {
		out = append(out, ev.Text)
	}
	return out
}

// An alert becomes one toast through magnum's notify once the 2 minutes it
// waits for others are up, in spec 9.F's words, and an event `!` lists;
// its sidebar token shows on the task's workspace at once, silent, and
// goes when the task no longer waits on you.
func TestAnAlertBecomesAToastAndASidebarToken(t *testing.T) {
	r := newAlertRig(t)
	task := r.task("PR-12345", registry.TaskActive, "investigate")
	r.rt.setWorkspaces(task.ID, "w1")
	r.e.toOperator(r.ctx, task, "3 questions before coding", "a test")
	r.tick()
	if tok, ok := r.side.token("w1"); tok != "needs you: questions" {
		t.Fatalf("sidebar token %q (reported %v), want it at once", tok, ok)
	}
	r.wantToasts("before the 2 minutes")

	r.after(time.Minute + 59*time.Second)
	r.wantToasts("a second before the 2 minutes")
	r.after(time.Second)
	r.wantToasts("after the 2 minutes", shownToast{"factory: 1 needs you", "PR-12345 questions"})
	if got := r.texts(0, alertToast); !slices.Equal(got, []string{"toast: factory: 1 needs you · PR-12345 questions"}) {
		t.Fatalf("toast events %q", got)
	}
	r.after(10 * time.Minute)
	r.wantToasts("10 minutes later", shownToast{"factory: 1 needs you", "PR-12345 questions"})
	reports := r.side.reports

	r.backToWork(task)
	r.tick()
	if tok, ok := r.side.token("w1"); !ok || tok != "" {
		t.Fatalf("sidebar token %q (reported %v) of a task back at work, want it cleared", tok, ok)
	}
	r.after(time.Minute)
	if r.side.reports != reports+1 {
		t.Errorf("sidebar reports %d, want %d: one for the change, none for a state that holds", r.side.reports, reports+1)
	}

	d, err := ScreenData(r.ctx, r.reg, nil, r.clock.Now(), time.Time{}, ScreenOptions{Config: r.cfg, Layout: r.layout})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(d.Alerts, func(a tui.FactoryAlert) bool { return strings.HasPrefix(a.Text, "toast: factory: 1 needs you") }) {
		t.Errorf("`!` lists %+v, want the toast with its time", d.Alerts)
	}
}

// Alerts within 2 minutes of the first merge into one toast; its count is
// every task that waits on you when it fires, one a toast before already
// named among them.
func TestAlertsWithinTwoMinutesMergeIntoOneToast(t *testing.T) {
	r := newAlertRig(t)
	r.needsYou("PR-12340", "the factory failed twice on this task")
	r.tick()
	r.after(2 * time.Minute)
	first := shownToast{"factory: 1 needs you", "PR-12340 the factory failed twice on this task"}
	r.wantToasts("the first alert", first)

	r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.clock.Add(time.Minute)
	r.needsYou("PR-12346", "1 question before coding")
	r.tick()
	r.clock.Add(50 * time.Second)
	r.needsYou("PR-12360", "ready to push (card 1): Custom activities list")
	r.tick()
	r.after(9 * time.Second)
	r.wantToasts("a second before the 2 minutes", first)
	r.after(time.Second)
	r.wantToasts("the merged toast", first,
		shownToast{"factory: 4 need you", "PR-12345 questions · PR-12346 questions · PR-12360 ready to push"})
	if got := r.texts(0, alertToast); len(got) != 2 ||
		got[1] != "toast: factory: 4 need you · PR-12345 questions · PR-12346 questions · PR-12360 ready to push" {
		t.Fatalf("toast events %q", got)
	}
}

// No toast while the factory screen reports focus: the alert is held, with
// one event that says so, while its sidebar token shows at once.
func TestNoToastWhileTheScreenHasFocus(t *testing.T) {
	r := newAlertRig(t)
	r.live.Store(int64(os.Getpid()))
	r.focus(true, os.Getpid())
	task := r.task("PR-12345", registry.TaskActive, "investigate")
	r.rt.setWorkspaces(task.ID, "w1")
	r.e.toOperator(r.ctx, task, "3 questions before coding", "a test")
	r.tick()
	for range 4 {
		r.after(5 * time.Minute)
	}
	r.wantToasts("while the screen has focus")
	if got := r.texts(task.ID, alertHeld); !slices.Equal(got, []string{"PR-12345 questions: held, the factory screen has your focus"}) {
		t.Fatalf("hold events %q, want one", got)
	}
	if tok, _ := r.side.token("w1"); tok != "needs you: questions" {
		t.Errorf("sidebar token %q, want it shown: the holds are the toasts'", tok)
	}
}

// The focus of a screen whose process ended holds nothing: its alert
// toasts after the 2 minutes as any.
func TestAFocusOfAScreenThatEndedHoldsNothing(t *testing.T) {
	r := newAlertRig(t)
	r.e.screenLive = nil // the real check: the pid runs no process
	r.focus(true, 1<<22)
	task := r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.after(2 * time.Minute)
	r.wantToasts("a dead screen's focus", shownToast{"factory: 1 needs you", "PR-12345 questions"})
	if got := r.texts(task.ID, alertHeld); len(got) != 0 {
		t.Errorf("hold events %q, want none", got)
	}
}

// No toast while you are in a factory pane (the observe pass's reading:
// herdr's focus on one, where herdr showed you at work): the alert is held,
// with one event.
func TestNoToastWhileTheOperatorIsInAFactoryPane(t *testing.T) {
	r := newAlertRig(t)
	r.rt.setPaneFocus(runtime.PaneFocus{Pane: "w1:p1", Since: r.clock.Now()})
	task := r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.after(2 * time.Minute)
	r.after(5 * time.Minute)
	r.wantToasts("while you are in a factory pane")
	if got := r.texts(task.ID, alertHeld); !slices.Equal(got, []string{"PR-12345 questions: held, you're in a factory pane"}) {
		t.Fatalf("hold events %q, want one", got)
	}
}

// herdr keeps a session's focused pane after you left its terminal, so a
// focus that rests on a factory pane counts as you in it for
// [daemon] human_cooldown after herdr last showed you at work there, not
// for ever: a held alert then fires 2 minutes after.
func TestAFocusThatRestsOnAFactoryPaneHoldsForTheHumanCooldownOnly(t *testing.T) {
	r := newAlertRig(t)
	since := r.clock.Now()
	r.rt.setPaneFocus(runtime.PaneFocus{Pane: "w1:p1", Since: since})
	r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.after(14 * time.Minute)
	r.after(2*time.Minute + 59*time.Second) // 16:59 after: within 2 minutes of the cooldown's end
	r.wantToasts("within 2 minutes of the cooldown's end")
	r.after(time.Second)
	r.wantToasts("2 minutes after the cooldown's end", shownToast{"factory: 1 needs you", "PR-12345 questions"})
}

// A held alert fires 2 minutes after you leave: from the screen's blur,
// from herdr's focus leaving the factory's panes, and from when the daemon
// saw a focused screen's process gone.
func TestAHeldAlertFiresTwoMinutesAfterYouLeave(t *testing.T) {
	r := newAlertRig(t)
	pid := os.Getpid()
	r.live.Store(int64(pid))
	r.focus(true, pid)
	r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.after(10 * time.Minute)
	r.focus(false, pid)
	r.after(time.Minute + 59*time.Second)
	r.wantToasts("a second before 2 minutes after the blur")
	r.after(time.Second)
	one := shownToast{"factory: 1 needs you", "PR-12345 questions"}
	r.wantToasts("2 minutes after the blur", one)

	atWork := r.clock.Now()
	r.rt.setPaneFocus(runtime.PaneFocus{Pane: "w1:p1", Since: atWork})
	r.needsYou("PR-12346", "2 questions before coding")
	r.tick()
	r.after(5 * time.Minute)
	r.rt.setPaneFocus(runtime.PaneFocus{Since: atWork, Left: r.clock.Now()})
	r.after(time.Minute + 59*time.Second)
	r.wantToasts("a second before 2 minutes after you left the pane", one)
	r.after(time.Second)
	two := shownToast{"factory: 2 need you", "PR-12346 questions"}
	r.wantToasts("2 minutes after you left the pane", one, two)

	r.focus(true, pid)
	r.needsYou("PR-12347", "4 questions before coding")
	r.tick()
	r.after(5 * time.Minute)
	r.live.Store(0)      // the screen's process ended while it had focus
	r.after(time.Minute) // the first tick that sees it gone
	r.after(time.Minute + 59*time.Second)
	r.wantToasts("a second before 2 minutes after the screen was seen gone", one, two)
	r.after(time.Second)
	r.wantToasts("2 minutes after the screen was seen gone", one, two, shownToast{"factory: 3 need you", "PR-12347 questions"})
}

// A held alert of a task that no longer waits on you for its reason never
// fires: answered meanwhile, or waiting on you for another reason (whose
// own alert fires instead). Each is dropped with an event, once, and a
// restart does not bring it back.
func TestAHeldAlertOfATaskThatNoLongerNeedsYouNeverFires(t *testing.T) {
	r := newAlertRig(t)
	pid := os.Getpid()
	r.live.Store(int64(pid))
	r.focus(true, pid)
	answered := r.needsYou("PR-12345", "3 questions before coding")
	moved := r.needsYou("PR-12346", "2 questions before coding")
	r.tick()
	r.backToWork(answered)
	r.backToWork(moved)
	r.e.toOperator(r.ctx, r.reload(moved.ID), "ready to push (card 1): Credit expiry reminder email", "the ready card")
	r.tick()
	r.focus(false, pid)
	r.after(2 * time.Minute)
	r.wantToasts("after you left", shownToast{"factory: 1 needs you", "PR-12346 ready to push"})
	if got := r.texts(answered.ID, alertDropped); !slices.Equal(got, []string{"PR-12345 questions: dropped, it no longer waits on you"}) {
		t.Errorf("drop events of the answered task %q", got)
	}
	if got := r.texts(moved.ID, alertDropped); !slices.Equal(got, []string{"PR-12346 questions: dropped, it no longer waits on you"}) {
		t.Errorf("drop events of the task that waits on you for its card %q", got)
	}
	r.restart()
	r.tick()
	r.after(5 * time.Minute)
	r.wantToasts("after a restart", shownToast{"factory: 1 needs you", "PR-12346 ready to push"})
	if got := r.texts(answered.ID, alertDropped); len(got) != 1 {
		t.Errorf("drop events after a restart %q, want the one", got)
	}
}

// magnum's quiet hours hold the toasts too: what came during them is never
// lost, and fires once they end, merged into one toast.
func TestQuietHoursHoldToastsAndLoseNone(t *testing.T) {
	r := newAlertRig(t)
	start := r.clock.Now().Local()
	r.cfg.Daemon.QuietHours = start.Format("15:04") + "-" + start.Add(time.Hour).Format("15:04")
	first := r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.after(30 * time.Minute)
	second := r.needsYou("PR-12360", "ready to push (card 1): Custom activities list")
	r.tick()
	r.after(29*time.Minute + 59*time.Second)
	r.wantToasts("during quiet hours")
	quiet := "held, quiet hours (" + r.cfg.Daemon.QuietHours + ")"
	for _, x := range []struct {
		t    registry.Task
		line string
	}{{first, "PR-12345 questions"}, {second, "PR-12360 ready to push"}} {
		if got := r.texts(x.t.ID, alertHeld); !slices.Equal(got, []string{x.line + ": " + quiet}) {
			t.Errorf("hold events of %s %q", x.t.JiraKey, got)
		}
	}
	r.after(time.Second)
	r.wantToasts("when quiet hours end", shownToast{"factory: 2 need you", "PR-12345 questions · PR-12360 ready to push"})
}

// A daemon restart neither repeats a delivered alert nor loses a pending
// one, and an alert whose toast went out just before the daemon stopped,
// with no record of it yet, is kept from repeating by the dedupe store.
func TestADaemonRestartDeliversEachAlertOnce(t *testing.T) {
	r := newAlertRig(t)
	r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.after(2 * time.Minute)
	one := shownToast{"factory: 1 needs you", "PR-12345 questions"}
	r.wantToasts("the first alert", one)
	r.restart()
	r.tick()
	r.after(5 * time.Minute)
	r.wantToasts("after a restart", one)

	r.needsYou("PR-12346", "2 questions before coding")
	r.tick()
	r.restart()
	r.tick()
	r.after(2 * time.Minute)
	two := shownToast{"factory: 2 need you", "PR-12346 questions"}
	r.wantToasts("the alert pending at the restart", one, two)

	before, err := os.ReadFile(r.layout.FactoryAlerts())
	if err != nil {
		t.Fatal(err)
	}
	r.needsYou("PR-12347", "4 questions before coding")
	r.tick()
	r.after(2 * time.Minute)
	three := shownToast{"factory: 3 need you", "PR-12347 questions"}
	r.wantToasts("the third alert", one, two, three)
	if err := os.WriteFile(r.layout.FactoryAlerts(), before, 0o600); err != nil { // as if it stopped before the record
		t.Fatal(err)
	}
	r.restart()
	r.tick()
	r.after(5 * time.Minute)
	r.wantToasts("after a restart that lost the record of a toast", one, two, three)
	if got := r.texts(0, alertToast); len(got) != 3 {
		t.Errorf("toast events %q, want 3", got)
	}
}

// An alert whose toast the dedupe store holds as sent, with no record of
// the delivery (the daemon stopped between the reservation and the show,
// or after the show), is not toasted again, and `!` says what became of
// it.
func TestAToastTheDedupeStoreHoldsAsSentIsRecordedSo(t *testing.T) {
	r := newAlertRig(t)
	task := r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	ev := r.events(task.ID, "alert")[0]
	if ok, err := r.dedupe.ShouldSend(r.ctx, fmt.Sprintf("factory-alert:%d:%d", ev.ID, ev.At.Unix()), alertOnce); err != nil || !ok {
		t.Fatalf("reserve the alert's toast: %v, %v", ok, err)
	}
	r.after(2 * time.Minute)
	r.wantToasts("a toast the dedupe store holds")
	if got := r.texts(task.ID, alertToast); !slices.Equal(got, []string{"toast of PR-12345 questions taken as sent before the restart"}) {
		t.Fatalf("its events %q", got)
	}
	r.after(5 * time.Minute)
	if got := r.texts(task.ID, alertToast); len(got) != 1 {
		t.Errorf("its events %q, want the one", got)
	}
}

// With [herdr] notify off no toast is shown, and `!` says so: the alert is
// settled, never left pending.
func TestWithToastsOffAnAlertIsRecordedNotShown(t *testing.T) {
	r := newAlertRig(t, func(n *notify.Notifier) { n.Enabled = false })
	r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.after(2 * time.Minute)
	r.after(5 * time.Minute)
	r.wantToasts("with toasts off")
	if got := r.texts(0, alertToast); !slices.Equal(got, []string{"no toast, [herdr] notify is off: factory: 1 needs you · PR-12345 questions"}) {
		t.Fatalf("toast events %q", got)
	}
}

// A toast that fails (herdr does not answer, and no osascript shows it) is
// logged and tried again on a later tick after a backoff, so a toast that
// keeps failing does not cost every tick its bound; never lost and never
// doubled.
func TestAToastThatFailsIsTriedAgainOnALaterTick(t *testing.T) {
	r := newAlertRig(t)
	r.toasts.setErr(fmt.Errorf("herdr: dial: %w", herdr.ErrUnavailable))
	r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.after(2 * time.Minute)
	if n := r.toasts.callCount(); n != 1 {
		t.Fatalf("toasts asked for %d, want 1", n)
	}
	r.after(45 * time.Second)
	r.wantToasts("while herdr does not answer")
	if n := r.toasts.callCount(); n != 1 {
		t.Fatalf("toasts asked for %d within the backoff, want 1", n)
	}
	if got := r.texts(0, alertToast); len(got) != 0 {
		t.Fatalf("toast events %q while none went out", got)
	}
	r.after(30 * time.Second) // past the first backoff: tried again, fails again
	if n := r.toasts.callCount(); n != 2 {
		t.Fatalf("toasts asked for %d after the backoff, want 2", n)
	}
	r.toasts.setErr(nil)
	r.after(time.Minute) // the second backoff is longer
	if n := r.toasts.callCount(); n != 2 {
		t.Fatalf("toasts asked for %d within the second backoff, want 2", n)
	}
	r.after(time.Minute + 30*time.Second)
	r.after(45 * time.Second)
	r.wantToasts("once herdr answers", shownToast{"factory: 1 needs you", "PR-12345 questions"})
}

// herdr's rate limit is waited out only within the toast's bound: the
// factory's notifier gives up on herdr in time for osascript to show the
// toast before the bound ends.
func TestARateLimitedToastReachesOsascriptWithinItsBound(t *testing.T) {
	var waits []time.Duration
	run := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"/usr/bin/osascript"}}}}
	r := newAlertRig(t, func(n *notify.Notifier) {
		n.Runner = run
		n.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	})
	r.toasts.declined = "rate_limited"
	r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.after(2 * time.Minute)
	if len(run.Calls) != 1 {
		t.Fatalf("osascript ran %d times, want once", len(run.Calls))
	}
	var waited time.Duration
	for _, d := range waits {
		waited += d
	}
	if waited == 0 || waited+10*time.Second > alertToastTimeout { // osascript's own timeout is 10s
		t.Fatalf("waited %s for herdr (%v); want it, and osascript after it, within %s", waited, waits, alertToastTimeout)
	}
	if got := r.texts(0, alertToast); !slices.Equal(got, []string{"toast: factory: 1 needs you · PR-12345 questions"}) {
		t.Errorf("toast events %q", got)
	}
}

// The factory's own alert (a blocker of its own, taskID 0) toasts while its
// blocker stands, counted among what needs you, and is dropped once it
// cleared before its toast.
func TestTheFactorysOwnAlertFiresWhileItsBlockerStands(t *testing.T) {
	r := newAlertRig(t)
	r.e.raiseBlocker(r.ctx, "probe", "probe: the copies cannot be read")
	r.tick()
	r.after(2 * time.Minute)
	one := shownToast{"factory: 1 needs you", "factory probe"}
	r.wantToasts("a blocker of the factory's", one)

	r.e.clearBlocker(r.ctx, "probe")
	r.e.raiseBlocker(r.ctx, "probe", "probe: the copies cannot be read again")
	r.tick()
	r.e.clearBlocker(r.ctx, "probe")
	r.after(2 * time.Minute)
	r.wantToasts("a blocker that cleared before its toast", one)
	if got := r.texts(0, alertDropped); !slices.Equal(got, []string{"factory probe: dropped, it no longer waits on you"}) {
		t.Errorf("drop events %q", got)
	}
}

// The first start of a daemon that delivers (no alerts.json yet) delivers
// the alerts older than a day that still stand, merged with the day's (two
// of one reason say it once), and settles the old ones that no longer
// stand in silence: no flood of drop events.
func TestTheFirstStartDeliversTheOldAlertsThatStillStand(t *testing.T) {
	r := newAlertRig(t)
	r.needsYou("PR-12340", "the factory failed twice on this task")
	gone := r.needsYou("PR-12341", "3 questions before coding")
	r.backToWork(gone)
	r.clock.Add(25 * time.Hour)
	recent := r.needsYou("PR-12346", "2 questions before coding")
	r.e.alert(r.ctx, recent.ID, "PR-12346: you: 2 questions before coding") // the same reason again
	r.tick()
	r.after(2 * time.Minute)
	r.wantToasts("the first start", shownToast{"factory: 2 need you", "PR-12340 the factory failed twice on this task · PR-12346 questions"})
	if got := append(r.texts(gone.ID, alertDropped), r.texts(gone.ID, alertHeld)...); len(got) != 0 {
		t.Errorf("events of an old alert that no longer stands %q, want none", got)
	}
}

// An alerts.json that cannot be read loses no pending alert: one held for
// more than a day still fires once its hold ends, and one delivered before
// is not shown again.
func TestAnUnreadableAlertsFileLosesNoPendingAlert(t *testing.T) {
	r := newAlertRig(t)
	pid := os.Getpid()
	r.needsYou("PR-12340", "the factory failed twice on this task")
	r.tick()
	r.after(2 * time.Minute)
	first := shownToast{"factory: 1 needs you", "PR-12340 the factory failed twice on this task"}
	r.wantToasts("the first alert", first)
	r.live.Store(int64(pid))
	r.focus(true, pid)
	r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.after(25 * time.Hour)
	if err := os.WriteFile(r.layout.FactoryAlerts(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.restart()
	r.focus(false, pid)
	r.after(2 * time.Minute)
	r.wantToasts("after a restart on an unreadable alerts.json", first, shownToast{"factory: 2 need you", "PR-12345 questions"})
}

// A held alert of a task that still waits on you for the same kind of
// wait fires when the hold ends, though the wait's words changed: you
// answered one question of three, or two, and left the others.
func TestAHeldAlertFiresWhileItsTaskStillWaitsOnYouForFewerQuestions(t *testing.T) {
	r := newAlertRig(t)
	pid := os.Getpid()
	r.live.Store(int64(pid))
	r.focus(true, pid)
	task := r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	for _, left := range []string{"2 questions before coding", "1 question before coding"} {
		if err := r.reg.TransitionTask(r.ctx, task.ID, []string{registry.TaskNeedsYou}, "", func(u *registry.TaskUpdate) {
			u.Set("waits_what", left)
			u.Because("your answers")
		}); err != nil {
			t.Fatal(err)
		}
		r.tick()
	}
	r.focus(false, pid)
	r.after(2 * time.Minute)
	r.wantToasts("after you left", shownToast{"factory: 1 needs you", "PR-12345 questions"})
	if got := r.texts(task.ID, alertDropped); len(got) != 0 {
		t.Errorf("drop events %q of an alert whose task still waits on you", got)
	}
}

// herdr's focus alone is no proof of you: at a daemon start (or after a
// move herdr made by itself) a focus that rests on a factory pane, with
// herdr showing you at work in none, holds no toast.
func TestADaemonStartWithHerdrRestingOnAFactoryPaneDelaysNoToast(t *testing.T) {
	r := newAlertRig(t)
	r.rt.setPaneFocus(runtime.PaneFocus{Pane: "w1:p1"})
	task := r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.after(2 * time.Minute)
	r.wantToasts("herdr resting on a factory pane", shownToast{"factory: 1 needs you", "PR-12345 questions"})
	if got := r.texts(task.ID, alertHeld); len(got) != 0 {
		t.Errorf("hold events %q, want none", got)
	}
}

// A restart writes no second hold event for an alert it finds held: `!`
// keeps one per alert and hold.
func TestARestartWritesNoSecondHoldEventForAHeldAlert(t *testing.T) {
	r := newAlertRig(t)
	pid := os.Getpid()
	r.live.Store(int64(pid))
	r.focus(true, pid)
	task := r.needsYou("PR-12345", "3 questions before coding")
	r.tick()
	r.restart()
	r.after(time.Minute)
	r.restart()
	r.after(time.Minute)
	if got := r.texts(task.ID, alertHeld); len(got) != 1 {
		t.Errorf("hold events %q after two restarts, want one", got)
	}
}

// The sidebar token reaches a workspace its task gains after the token was
// first reported, and every workspace again once herdr is back after a
// herdr hold; one deadline bounds the whole pass.
func TestTheSidebarTokenFollowsTheTasksWorkspacesAndHerdrComingBack(t *testing.T) {
	r := newAlertRig(t)
	task := r.task("PR-12345", registry.TaskActive, "investigate")
	r.rt.setWorkspaces(task.ID, "w1")
	r.e.toOperator(r.ctx, task, "3 questions before coding", "a test")
	r.tick()
	r.rt.setWorkspaces(task.ID, "w1", "w2")
	r.tick()
	if tok, _ := r.side.token("w2"); tok != "needs you: questions" {
		t.Fatalf("token on the workspace the task gained %q", tok)
	}
	reports := r.side.reports

	down := atomic.Bool{}
	r.e.d.HerdrUp = func(context.Context) error {
		if down.Load() {
			return herdr.ErrUnavailable
		}
		return nil
	}
	down.Store(true)
	r.after(time.Minute)
	down.Store(false)
	r.after(time.Minute)
	if r.side.reports != reports+2 {
		t.Errorf("reports %d once herdr is back, want %d: each workspace's token again", r.side.reports, reports+2)
	}

	var deadlines []time.Time
	start := time.Now()
	r.e.d.Sidebar = func(ctx context.Context, _ string, _ map[string]string) error {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("a sidebar report with no deadline")
		}
		deadlines = append(deadlines, dl)
		return errors.New("herdr: report_metadata: timeout")
	}
	r.after(time.Minute) // nothing changed: no report
	r.backToWork(task)
	r.after(time.Minute)
	if len(deadlines) != 2 {
		t.Fatalf("reports %d, want one per workspace", len(deadlines))
	}
	if !deadlines[0].Equal(deadlines[1]) || deadlines[0].After(start.Add(sidebarPassTimeout+time.Second)) {
		t.Errorf("the pass's reports have deadlines %v; want the pass's one, within %s", deadlines, sidebarPassTimeout)
	}
}
