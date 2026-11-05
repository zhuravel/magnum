package factory

// The delivery of the alerts (spec 9.F). An alert is the event row
// Engine.alert writes when a task, or the factory itself (task 0), moves
// into NEEDS YOU; the daemon delivers each one, on its loop, since the
// screen may be closed:
//
//   - a toast through magnum's notify, an urgent notify.Batcher: the alerts
//     within alertMerge of the first merge into one toast, in spec 9.F's
//     words ("factory: 3 need you · PR-12345 questions · PR-12360 ready to
//     push"), whose count is everything that waits on you when it fires;
//   - the sidebar token of the task's workspaces in the factory's herdr
//     session (notify.Notifier.Sidebar), silent and persistent: it always
//     says whether the task waits on you, at once and through every hold.
//
// The toasts hold in magnum's quiet hours, while the factory screen has
// focus (its report in focus.json, only while its process runs), and while
// you are in a factory pane (herdr's focus as the observe pass saw it, for
// at most [daemon] human_cooldown after the focus moved there or herdr
// last showed you at work there: herdr keeps a session's focus after you
// left its terminal). A held alert fires when quiet hours end, or 2
// minutes after you left, merged with the rest, and only if its task still
// waits on you for the same reason; one that no longer does is dropped,
// never delivered. Each toast, hold and drop is an event, so `!` lists
// every alert with its time. alerts.json keeps how far the delivery went
// and the dedupe store each alert's toast, so a restart neither repeats an
// alert nor loses one. A toast and a sidebar report have a bounded time,
// and one that fails is logged and tried again on a later tick.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/textx"
)

// The events of the alerts' delivery, which `!` lists with the alerts: a
// toast (the daemon's own), and an alert held or dropped (its task's).
const (
	alertToast   = "alert_toast"
	alertHeld    = "alert_held"
	alertDropped = "alert_dropped"
)

// alertKinds are the events `!` lists: the alerts and their delivery.
var alertKinds = []string{"alert", alertToast, alertHeld, alertDropped}

const (
	// alertMerge is how long the first alert waits for others to merge
	// into its toast, and how long after you left a held alert fires.
	alertMerge = 2 * time.Minute
	// alertOnce is how long the dedupe store keeps an alert's toast from
	// going out again (a restart that lost the record of it).
	alertOnce = 30 * 24 * time.Hour
	// alertHistory: with no alerts.json yet (the first start of a build
	// that delivers), the alerts older than this are history, never
	// toasted.
	alertHistory = 24 * time.Hour
	// alertsRead bounds one read of the alerts, alertReads the reads of a
	// tick (a backlog of history is caught up in a few); alertLines the
	// lines of a toast (the rest are "+N more"); alertWhatRunes a line's
	// reason.
	alertsRead     = 64
	alertReads     = 16
	alertLines     = 5
	alertWhatRunes = 40
	// alertToastTimeout and sidebarTimeout bound a toast and a sidebar
	// report on the loop; sidebarRefresh reports a token that holds again
	// before herdr's TTL drops it.
	alertToastTimeout = 10 * time.Second
	sidebarTimeout    = 5 * time.Second
	sidebarRefresh    = notify.SidebarTTL / 2
	// sidebarKey is the factory's sidebar token.
	sidebarKey = "factory"
)

// What holds the toasts.
const (
	alertHoldQuiet  = "quiet"
	alertHoldScreen = "screen"
	alertHoldPane   = "pane"
)

// alertDelivery is the loop's state of the delivery.
type alertDelivery struct {
	batch *notify.Batcher // nil: no notifier, no toast
	// loaded: through was read from alerts.json; through is the newest alert
	// settled with every older one, read the newest read; before history
	// (with no alerts.json yet) the alerts are settled as read.
	loaded        bool
	through, read int64
	history       time.Time
	pending       []*pendingAlert // read, not yet behind through, oldest first
	// count is what the toast being composed counts, composed what the last
	// composed toast said ("" when none was).
	count    int
	composed string
	// screenLive: the screen's focus was live at the last check;
	// screenGone is when a check first found a focused screen's process
	// gone.
	screenLive bool
	screenGone time.Time
	// tokens are the sidebar tokens as last reported, by task.
	tokens map[int64]sidebarToken
}

// pendingAlert is an alert the delivery read: its event, its dedupe key,
// its line in a toast, the hold its last hold event named, and whether it
// is settled (toasted, dropped, or history).
type pendingAlert struct {
	ev        registry.Event
	key, line string
	held      string
	settled   bool
}

// sidebarToken is a task's sidebar token as last reported, and when.
type sidebarToken struct {
	value string
	at    time.Time
}

// newAlertDelivery is the delivery over notifier (nil: no toast).
func (e *Engine) newAlertDelivery(n *notify.Notifier) alertDelivery {
	a := alertDelivery{tokens: map[int64]sidebarToken{}}
	if n != nil {
		a.batch = &notify.Batcher{Notifier: n, Key: "factory-alerts", Window: alertMerge, Urgent: true, Now: e.now,
			Compose: e.composeAlerts}
	}
	return a
}

// deliverAlerts is the tick's delivery: the sidebar tokens, then the alerts
// written since, each held, dropped or merged into a toast once due.
func (e *Engine) deliverAlerts(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := e.now()
	tasks, err := e.reg.Tasks(ctx)
	if err != nil {
		e.log.Warn("the alerts' delivery: read the tasks", "err", err)
		return
	}
	e.sidebarTokens(ctx, now, tasks)
	a := &e.alerts
	if a.batch == nil {
		return
	}
	e.readAlerts(ctx, now)
	if !slices.ContainsFunc(a.pending, func(p *pendingAlert) bool { return !p.settled }) {
		return
	}
	if cause, why := e.toastHold(now); cause != "" {
		e.holdAlerts(ctx, cause, why)
		return
	}
	if !a.batch.Due() {
		return
	}
	e.dropStale(ctx, tasks)
	if a.batch.Due() { // the oldest left may be younger than the window
		e.flushAlerts(ctx, tasks)
	}
	e.settleAlerts()
}

// readAlerts reads the alerts written since the last read (alerts.json
// says how far the delivery went, on the first) into the batch; with no
// alerts.json yet, those older than alertHistory are history, settled as
// read.
func (e *Engine) readAlerts(ctx context.Context, now time.Time) {
	a := &e.alerts
	if !a.loaded {
		a.loaded = true
		through, ok, err := readAlertsDone(e.d.Layout)
		if err != nil {
			e.log.Warn("the alerts' delivery: alerts.json cannot be read; the alerts of the last day are delivered again", "err", err)
		}
		if !ok {
			a.history = now.Add(-alertHistory)
		}
		a.through, a.read = through, through
	}
	for range alertReads {
		evs, err := e.reg.EventsAfter(ctx, a.read, alertsRead, "alert")
		if err != nil {
			e.log.Warn("the alerts' delivery: read the alerts", "err", err)
			return
		}
		for _, ev := range evs {
			a.read = ev.ID
			p := &pendingAlert{ev: ev, key: fmt.Sprintf("factory-alert:%d:%d", ev.ID, ev.At.Unix()), line: alertLine(ev.Text)}
			if p.settled = !a.history.IsZero() && ev.At.Before(a.history); !p.settled {
				a.batch.Add(notify.Item{Key: p.key, Title: p.line, Line: p.line, Window: alertOnce})
			}
			a.pending = append(a.pending, p)
		}
		e.settleAlerts()
		if len(evs) < alertsRead {
			return
		}
	}
}

// holdAlerts records the hold of each pending alert, once per hold.
func (e *Engine) holdAlerts(ctx context.Context, cause, why string) {
	for _, p := range e.alerts.pending {
		if p.settled || p.held == cause {
			continue
		}
		p.held = cause
		e.event(ctx, p.ev.TaskID, alertHeld, registry.PhaseInfo, p.line+": held, "+why)
	}
}

// dropStale drops each pending alert whose task no longer waits on you for
// its reason: it never fires.
func (e *Engine) dropStale(ctx context.Context, tasks []registry.Task) {
	byID := make(map[int64]registry.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	var gone []string
	for _, p := range e.alerts.pending {
		if p.settled || e.alertStands(byID, p.ev) {
			continue
		}
		p.settled = true
		gone = append(gone, p.key)
		e.event(ctx, p.ev.TaskID, alertDropped, registry.PhaseInfo, p.line+": dropped, it no longer waits on you")
	}
	e.alerts.batch.Forget(gone...)
}

// alertStands reports whether the alert ev still holds: the factory's own
// while its blocker of that line stands; a task's while it waits on you for
// what the alert said, or, for an ended task's leftovers (its copy's
// release refused, what a base run left in its copy), while they last. An
// alert's text is kept redacted (Engine.event), so what it is compared
// with is too.
func (e *Engine) alertStands(byID map[int64]registry.Task, ev registry.Event) bool {
	said := func(text string) bool { return ev.Text == execx.Redact(text) }
	if ev.TaskID == 0 {
		return slices.ContainsFunc(slices.Collect(maps.Values(e.blockers)), func(line string) bool {
			return said("factory: " + waitsYou + ": " + line)
		})
	}
	t, ok := byID[ev.TaskID]
	if !ok {
		return false
	}
	key := TaskKey(t)
	if waitsOnYou(t) && said(key+": "+waitsYou+": "+t.WaitsWhat) {
		return true
	}
	e.claimMu.Lock()
	d := e.debts[t.ID]
	e.claimMu.Unlock()
	if d != nil && d.alerted && strings.Contains(ev.Text, " is still reserved for it, its release refused for ") {
		return true
	}
	k, kept := e.keptCopies[t.ID]
	return kept && said(key+": "+k.what)
}

// flushAlerts sends the due toast of the alerts that stand, counting
// everything that waits on you now, and records it; a toast that fails
// keeps them for a later tick.
func (e *Engine) flushAlerts(ctx context.Context, tasks []registry.Task) {
	a := &e.alerts
	var standing []*pendingAlert
	for _, p := range a.pending {
		if !p.settled {
			standing = append(standing, p)
		}
	}
	a.count, a.composed = e.needYou(tasks, standing), ""
	tctx, cancel := context.WithTimeout(ctx, alertToastTimeout)
	defer cancel()
	sent, err := a.batch.FlushDue(tctx)
	if err != nil {
		e.log.Log(ctx, execx.FailLevel(ctx, err), "an alert's toast failed; it is tried again on a later tick",
			"alerts", len(standing), "err", err)
		return
	}
	switch {
	case a.composed == "" && !a.batch.Notifier.Enabled:
		items := make([]notify.Item, len(standing))
		for i, p := range standing {
			items[i] = notify.Item{Line: p.line}
		}
		title, body := e.composeAlerts(items)
		e.event(ctx, 0, alertToast, registry.PhaseInfo, "no toast, [herdr] notify is off: "+title+" · "+body)
	case a.composed == "": // each was toasted before: a restart lost the record of it (the dedupe store)
	case sent:
		e.event(ctx, 0, alertToast, registry.PhaseOK, "toast: "+a.composed)
	default:
		e.event(ctx, 0, alertToast, registry.PhaseInfo, "no toast, herdr declined it: "+a.composed)
	}
	for _, p := range standing {
		p.settled = true
	}
}

// composeAlerts is the toast of items (the Batcher's Compose): the count
// flushAlerts took, and the alerts' lines.
func (e *Engine) composeAlerts(items []notify.Item) (title, body string) {
	var lines []string
	for _, it := range items {
		if !slices.Contains(lines, it.Line) { // two alerts of one reason say it once
			lines = append(lines, it.Line)
		}
	}
	if len(lines) > alertLines {
		lines = append(lines[:alertLines], fmt.Sprintf("+%d more", len(lines)-alertLines))
	}
	title = fmt.Sprintf("factory: %d need you", e.alerts.count)
	if e.alerts.count == 1 {
		title = "factory: 1 needs you"
	}
	body = strings.Join(lines, " · ")
	e.alerts.composed = title + " · " + body
	return title, body
}

// needYou counts what waits on you: every task that does, the tasks of
// the alerts that stand (an ended task's leftovers), and the factory's own
// blockers.
func (e *Engine) needYou(tasks []registry.Task, standing []*pendingAlert) int {
	ids := map[int64]bool{}
	for _, t := range tasks {
		if waitsOnYou(t) {
			ids[t.ID] = true
		}
	}
	for _, p := range standing {
		if p.ev.TaskID != 0 {
			ids[p.ev.TaskID] = true
		}
	}
	return len(ids) + len(e.blockers)
}

// settleAlerts moves through past the settled alerts at the head of the
// pending ones, and keeps it in alerts.json; a write that fails is tried
// again on the next.
func (e *Engine) settleAlerts() {
	a := &e.alerts
	n, through := 0, a.through
	for _, p := range a.pending {
		if !p.settled {
			break
		}
		n, through = n+1, p.ev.ID
	}
	if n == 0 {
		return
	}
	if err := writeAlertsDone(e.d.Layout, through); err != nil {
		e.log.Warn("the alerts' delivery: write alerts.json; tried again on the next", "err", err)
		return
	}
	a.pending, a.through = a.pending[n:], through
}

// toastHold is what holds the toasts at now, and why in an event's words
// ("" for nothing): quiet hours, the screen's focus, you in a factory pane.
func (e *Engine) toastHold(now time.Time) (cause, why string) {
	if spec := e.cfg.Daemon.QuietHours; spec != "" && eligibility.QuietHours(spec, now.Local()) {
		return alertHoldQuiet, "quiet hours (" + spec + ")"
	}
	if why := e.screenHold(now); why != "" {
		return alertHoldScreen, why
	}
	if why := e.paneHold(now); why != "" {
		return alertHoldPane, why
	}
	return "", ""
}

// screenHold is why the screen's focus holds the toasts ("" for nothing):
// a screen whose process runs has focus, or it lost it (its blur, or the
// check that found its process gone) less than alertMerge ago. Its process
// is looked at only here, while an alert waits.
func (e *Engine) screenHold(now time.Time) string {
	f, err := ReadScreenFocus(e.d.Layout)
	if err != nil {
		e.log.Warn("the alerts' delivery: read the screen's focus", "err", err)
		return ""
	}
	a := &e.alerts
	live := e.screenLive
	if live == nil {
		live = FocusPayload.Live
	}
	var left time.Time
	switch {
	case live(f):
		a.screenLive = true
		return "the factory screen has your focus"
	case f.Focused: // its process ended while it had focus
		if a.screenLive {
			a.screenLive, a.screenGone = false, now
		}
		left = a.screenGone
	default:
		a.screenLive = false
		left = f.At
	}
	if !left.IsZero() && now.Before(left.Add(alertMerge)) {
		return "you left the factory screen at " + clock(left) + ", it fires 2 minutes after"
	}
	return ""
}

// paneHold is why you in a factory pane hold the toasts ("" for nothing):
// herdr's focus is on one (for [daemon] human_cooldown after it moved
// there or herdr last showed you at work there, taken as left at that
// end), or left one less than alertMerge ago. While herdr is held down no
// pass reads it, and you are in none of its panes.
func (e *Engine) paneHold(now time.Time) string {
	if e.d.Runtime == nil {
		return ""
	}
	f := e.d.Runtime.PaneFocus()
	left := f.Left
	if _, down := e.holds[HoldHerdr]; f.Pane != "" && !down {
		end := e.cfg.Daemon.CooldownUntil(&f.Since)
		if now.Before(end) {
			return "you're in a factory pane"
		}
		left = cmp.Or(end, left)
	}
	if !left.IsZero() && now.Before(left.Add(alertMerge)) {
		return "you left a factory pane at " + clock(left) + ", it fires 2 minutes after"
	}
	return ""
}

// sidebarTokens reports each task's sidebar token on its workspaces when
// it changed, after a start, and before herdr's TTL would drop it: "needs
// you: <what>" while the task waits on you, empty otherwise. No hold
// applies to it. A report that fails is tried again on a later tick.
func (e *Engine) sidebarTokens(ctx context.Context, now time.Time, tasks []registry.Task) {
	if e.d.Sidebar == nil || e.d.Runtime == nil {
		return
	}
	if _, down := e.holds[HoldHerdr]; down {
		return
	}
	for _, t := range tasks {
		if t.State == registry.TaskDone || t.State == registry.TaskStopped { // its workspaces closed with it
			delete(e.alerts.tokens, t.ID)
			continue
		}
		want := ""
		if waitsOnYou(t) {
			want = "needs you: " + alertShort(t.WaitsWhat)
		}
		if s, ok := e.alerts.tokens[t.ID]; ok && s.value == want && now.Sub(s.at) < sidebarRefresh {
			continue
		}
		if e.reportToken(ctx, t, want) {
			e.alerts.tokens[t.ID] = sidebarToken{value: want, at: now}
		}
	}
}

// reportToken reports want as task t's sidebar token on each workspace of
// its live sessions; false when one report failed.
func (e *Engine) reportToken(ctx context.Context, t registry.Task, want string) bool {
	ws, err := e.d.Runtime.Workspaces(ctx, t)
	if err != nil {
		e.log.Warn("a task's sidebar token: read its workspaces", "task", t.ID, "err", err)
		return false
	}
	ok := true
	for _, w := range ws {
		sctx, cancel := context.WithTimeout(ctx, sidebarTimeout)
		err := e.d.Sidebar(sctx, w, map[string]string{sidebarKey: want})
		cancel()
		if err != nil {
			e.log.Debug("a task's sidebar token; tried again on a later tick", "task", t.ID, "workspace", w, "err", err)
			ok = false
		}
	}
	return ok
}

// alertLine is an alert's line in a toast: its key and its reason, short
// ("PR-12345 questions", "factory disk").
func alertLine(text string) string {
	who, what, ok := strings.Cut(text, ": ")
	if !ok {
		return textx.Clip(text, alertWhatRunes)
	}
	return who + " " + alertShort(strings.TrimPrefix(what, waitsYou+": "))
}

// alertShort is a wait's reason as a toast's line and the sidebar token
// say it: "questions", "ready to push", else the screen's short form of it
// or its first words.
func alertShort(what string) string {
	switch {
	case questionsWaitRe.MatchString(what):
		return strings.Fields(what)[1]
	case strings.HasPrefix(what, readyWait):
		return readyWait
	}
	return textx.Clip(cmp.Or(shortWhat(waitsYou, what), what), alertWhatRunes)
}

// alertsDone is alerts.json: the newest alert the delivery settled, with
// every older one.
type alertsDone struct{ Through int64 }

// readAlertsDone reads alerts.json; false when there is none yet.
func readAlertsDone(l paths.Layout) (through int64, ok bool, err error) {
	b, err := os.ReadFile(l.FactoryAlerts())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var d alertsDone
	if err := json.Unmarshal(b, &d); err != nil {
		return 0, false, fmt.Errorf("%s: %w", filepath.Base(l.FactoryAlerts()), err)
	}
	return d.Through, true, nil
}

// writeAlertsDone replaces alerts.json.
func writeAlertsDone(l paths.Layout, through int64) error {
	b, err := json.Marshal(alertsDone{Through: through})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.FactoryAlerts()), 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(l.FactoryAlerts(), append(b, '\n'), 0o600)
}
