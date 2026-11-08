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
//     says whether the task waits on you, through every hold.
//
// The toasts hold in magnum's quiet hours, while the factory screen has
// focus (its report in focus.json, only while its process runs), and while
// you are in a factory pane: herdr's focus on one, for at most [daemon]
// human_cooldown after herdr last showed you at work in a factory pane.
// herdr's focus alone proves nothing of you (it rests where it was when
// nobody is attached, a daemon start included, and herdr moves it by
// itself). A held alert fires when quiet hours end, or 2 minutes after you
// left, merged with the rest, and only if its task still waits on you for
// the same kind of wait (its line: "3 questions" and "2 questions" are
// both "questions"); one that no longer does is dropped, never delivered.
// Each toast, hold and drop is an event, so `!` lists every alert with its
// time. alerts.json keeps how far the delivery went and the dedupe store
// each alert's toast, so a restart neither repeats an alert nor loses one.
// A toast and the sidebar pass have a bounded time; a toast that fails is
// logged and tried again after a backoff, a sidebar report on the next
// tick.

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

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/textx"
)

// The events of the alerts' delivery, which `!` lists with the alerts: a
// toast (the daemon's own; an alert's whose toast the dedupe store holds
// as sent), and an alert held or dropped (its task's).
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
	// alertHistory: with no readable alerts.json (the first start of a
	// build that delivers), the alerts older than this that no longer
	// stand are settled in silence, with no drop event.
	alertHistory = 24 * time.Hour
	// alertsRead bounds one read of the alerts, alertReads the reads of a
	// tick (a backlog is caught up in a few); alertLines the lines of a
	// toast (the rest are "+N more"); alertWhatRunes a line's reason.
	alertsRead     = 64
	alertReads     = 16
	alertLines     = 5
	alertWhatRunes = 40
	// alertToastTimeout bounds a toast on the loop. alertRetryBudget is how
	// much of herdr's rate limit it waits out (notify's UrgentBudget), so
	// osascript (10s at most) still shows it within the bound. A toast that
	// failed is tried again after alertRetryFirst, doubling up to
	// alertRetryMax.
	alertToastTimeout = 15 * time.Second
	alertRetryBudget  = 3 * time.Second
	alertRetryFirst   = time.Minute
	alertRetryMax     = 15 * time.Minute
	// sidebarPassTimeout bounds a tick's sidebar reports, all of them;
	// sidebarRefresh reports a token that holds again before herdr's TTL
	// drops it.
	sidebarPassTimeout = 10 * time.Second
	sidebarRefresh     = notify.SidebarTTL / 2
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
	// (no readable alerts.json) the alerts that no longer stand are settled
	// in silence. seeded: the holds of the alerts read at the start were
	// taken from their hold events.
	loaded, seeded bool
	through, read  int64
	history        time.Time
	pending        []*pendingAlert // read, not yet behind through, oldest first
	// fails counts the toasts in a row that failed; retryAt is when the
	// next may be tried.
	fails   int
	retryAt time.Time
	// screenLive: the screen's focus was live at the last check;
	// screenGone is when a check first found a focused screen's process
	// gone.
	screenLive bool
	screenGone time.Time
	// tokens are the sidebar tokens as last reported, by task and
	// workspace; herdrDown: a pass found herdr held down (its tokens may be
	// gone when it is back).
	tokens    map[int64]map[string]sidebarToken
	herdrDown bool
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

// sidebarToken is a sidebar token as last reported, and when.
type sidebarToken struct {
	value string
	at    time.Time
}

// newAlertDelivery is the delivery over notifier n (nil: no toast). The
// delivery toasts through a copy of n whose urgent toasts wait out at most
// alertRetryBudget of herdr's rate limit.
func (e *Engine) newAlertDelivery(n *notify.Notifier) alertDelivery {
	a := alertDelivery{tokens: map[int64]map[string]sidebarToken{}}
	if n != nil {
		own := *n
		if own.UrgentBudget <= 0 || own.UrgentBudget > alertRetryBudget {
			own.UrgentBudget = alertRetryBudget
		}
		a.batch = &notify.Batcher{Notifier: &own, Key: "factory-alerts", Window: alertMerge, Urgent: true, Now: e.now}
	}
	return a
}

// deliverAlerts is the tick's delivery over the tasks the tick read last:
// the sidebar tokens, then the alerts written since, each held, dropped or
// merged into a toast once due. A due toast reads the tasks again: what it
// counts and what still stands is what is true when it fires.
func (e *Engine) deliverAlerts(ctx context.Context, tasks []registry.Task) {
	if ctx.Err() != nil {
		return
	}
	now := e.now()
	e.sidebarTokens(ctx, now, tasks)
	a := &e.alerts
	if a.batch == nil {
		return
	}
	e.readAlerts(ctx, now, tasks)
	if !slices.ContainsFunc(a.pending, func(p *pendingAlert) bool { return !p.settled }) {
		return
	}
	if cause, why := e.toastHold(now); cause != "" {
		e.holdAlerts(ctx, cause, why)
		return
	}
	if !a.batch.Due() || now.Before(a.retryAt) {
		return
	}
	fresh, err := e.reg.Tasks(ctx)
	if err != nil {
		e.log.Warn("the alerts' delivery: read the tasks", "err", err)
		return
	}
	e.dropStale(ctx, fresh)
	if a.batch.Due() { // the oldest left may be younger than the window
		e.flushAlerts(ctx, now, fresh)
	}
	e.settleAlerts()
}

// readAlerts reads the alerts written since the last read (alerts.json
// says how far the delivery went, on the first) into the batch. With no
// readable alerts.json, an alert older than alertHistory that no longer
// stands is settled in silence; one that stands is delivered as any. The
// first read after a start takes the holds the alerts' events named.
func (e *Engine) readAlerts(ctx context.Context, now time.Time, tasks []registry.Task) {
	a := &e.alerts
	if !a.loaded {
		a.loaded = true
		through, ok, err := readAlertsDone(e.d.Layout)
		if err != nil {
			e.log.Warn("the alerts' delivery: alerts.json cannot be read; the alerts that still stand are delivered again "+
				"(the dedupe store keeps the ones shown from showing twice)", "err", err)
		}
		if !ok {
			a.history = now.Add(-alertHistory)
		}
		a.through, a.read = through, through
	}
	byID := tasksByID(tasks)
	for range alertReads {
		evs, err := e.reg.EventsAfter(ctx, a.read, alertsRead, "alert")
		if err != nil {
			e.log.Warn("the alerts' delivery: read the alerts", "err", err)
			return
		}
		for _, ev := range evs {
			a.read = ev.ID
			p := &pendingAlert{ev: ev, key: fmt.Sprintf("factory-alert:%d:%d", ev.ID, ev.At.Unix()), line: alertLine(ev.Text)}
			old := !a.history.IsZero() && ev.At.Before(a.history)
			if p.settled = old && !e.alertStands(byID, ev); !p.settled {
				a.batch.Add(notify.Item{Key: p.key, Title: p.line, Line: p.line, Window: alertOnce})
			}
			a.pending = append(a.pending, p)
		}
		e.settleAlerts()
		if len(evs) < alertsRead {
			break
		}
	}
	e.seedHolds(ctx)
}

// seedHolds takes, once after a start, the hold each pending alert's
// newest hold event named, so a restart writes no second hold event.
func (e *Engine) seedHolds(ctx context.Context) {
	a := &e.alerts
	if a.seeded {
		return
	}
	a.seeded = true
	i := slices.IndexFunc(a.pending, func(p *pendingAlert) bool { return !p.settled })
	if i < 0 {
		return
	}
	held, err := e.reg.EventsAfter(ctx, a.pending[i].ev.ID, 0, alertHeld)
	if err != nil {
		e.log.Warn("the alerts' delivery: read the hold events", "err", err)
		return
	}
	for _, p := range a.pending[i:] {
		for _, h := range held {
			if why, ok := strings.CutPrefix(h.Text, p.line+": held, "); ok && h.TaskID == p.ev.TaskID && h.ID > p.ev.ID {
				p.held = holdCause(why)
			}
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
// its kind of wait: it never fires.
func (e *Engine) dropStale(ctx context.Context, tasks []registry.Task) {
	byID := tasksByID(tasks)
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
// while a blocker of its kind stands; a task's while it waits on you for
// the same kind of wait the alert named (its line: what a wait's count or
// card revision changed is the same), or, for an ended task's leftovers
// (its copy's release refused, what a base run left in its copy), while
// they last; a server a QA teardown failed to stop, while it is left. An
// alert's text is kept redacted (Engine.event), so what it is compared with
// is too.
func (e *Engine) alertStands(byID map[int64]registry.Task, ev registry.Event) bool {
	line := alertLine(ev.Text)
	same := func(text string) bool { return line == alertLine(execx.Redact(text)) }
	if _, left := e.servers[ev.TaskID]; left && ev.TaskID != 0 && strings.Contains(ev.Text, serverLeftWords) {
		return true
	}
	if ev.TaskID == 0 {
		return slices.ContainsFunc(slices.Collect(maps.Values(e.blockers)), func(b string) bool {
			return same("factory: " + waitsYou + ": " + b)
		})
	}
	t, ok := byID[ev.TaskID]
	if !ok {
		return false
	}
	key := TaskKey(t)
	if waitsOnYou(t) && same(key+": "+waitsYou+": "+t.WaitsWhat) {
		return true
	}
	e.claimMu.Lock()
	d := e.debts[t.ID]
	e.claimMu.Unlock()
	if d != nil && d.alerted && strings.Contains(ev.Text, " is still reserved for it, its release refused for ") {
		return true
	}
	k, kept := e.keptCopies[t.ID]
	return kept && same(key+": "+k.what)
}

// alertBackoff is the wait after fails toasts failed in a row: 1, 2, 4, 8,
// then 15 minutes however long they keep failing. The shift is capped as
// infraBackoff's is (1m<<4 is past alertRetryMax): uncapped, it turned the
// wait negative at the 29th failure, and every tick then tried a toast.
func alertBackoff(fails int) time.Duration {
	return min(alertRetryFirst<<min(max(fails-1, 0), alertBackoffShifts), alertRetryMax)
}

// alertBackoffShifts caps alertBackoff's doubling.
const alertBackoffShifts = 4

// flushAlerts sends the due toast of the alerts that stand, counting what
// waits on you now, and records what became of each: toasted, held as
// sent by the dedupe store, or not shown (toasts off, herdr declining). A
// toast that fails keeps them for a later try, after a backoff.
func (e *Engine) flushAlerts(ctx context.Context, now time.Time, tasks []registry.Task) {
	a := &e.alerts
	var standing []*pendingAlert
	for _, p := range a.pending {
		if !p.settled {
			standing = append(standing, p)
		}
	}
	count := e.needYou(tasks, standing)
	a.batch.Compose = func(items []notify.Item) (string, string) { return alertToastText(count, items) }
	tctx, cancel := context.WithTimeout(ctx, alertToastTimeout)
	defer cancel()
	res, err := a.batch.FlushDueResult(tctx)
	shown := map[string]bool{}
	for _, it := range res.Suppressed {
		shown[it.Key] = true
	}
	if err != nil {
		// What the dedupe store took as sent is settled, with its event,
		// whatever became of the fresh ones' toast: they wait for a try.
		for _, p := range standing {
			if shown[p.key] {
				e.event(ctx, p.ev.TaskID, alertToast, registry.PhaseInfo, "toast of "+p.line+" taken as sent before the restart")
				p.settled = true
			}
		}
		a.fails++
		a.retryAt = now.Add(alertBackoff(a.fails))
		e.log.Log(ctx, execx.FailLevel(ctx, err), "an alert's toast failed; it is tried again later",
			"alerts", len(standing), "at", clock(a.retryAt), "err", err)
		return
	}
	a.fails, a.retryAt = 0, time.Time{}
	var lines []notify.Item
	for _, p := range standing {
		if shown[p.key] {
			e.event(ctx, p.ev.TaskID, alertToast, registry.PhaseInfo, "toast of "+p.line+" taken as sent before the restart")
		} else {
			lines = append(lines, notify.Item{Line: p.line})
		}
		p.settled = true
	}
	switch {
	case res.Off:
		title, body := alertToastText(count, lines)
		e.event(ctx, 0, alertToast, registry.PhaseInfo, "no toast, [herdr] notify is off: "+title+" · "+body)
	case res.Title == "": // each was taken as sent, above
	case res.Sent:
		e.event(ctx, 0, alertToast, registry.PhaseOK, "toast: "+res.Title+" · "+res.Body)
	default:
		e.event(ctx, 0, alertToast, registry.PhaseInfo, "no toast, herdr declined it: "+res.Title+" · "+res.Body)
	}
}

// alertToastText is the toast of items, counting count: "factory: N need
// you" and the alerts' lines, each once.
func alertToastText(count int, items []notify.Item) (title, body string) {
	var lines []string
	for _, it := range items {
		if !slices.Contains(lines, it.Line) { // two alerts of one reason say it once
			lines = append(lines, it.Line)
		}
	}
	if len(lines) > alertLines {
		lines = append(lines[:alertLines], fmt.Sprintf("+%d more", len(lines)-alertLines))
	}
	title = fmt.Sprintf("factory: %d need you", count)
	if count == 1 {
		title = "factory: 1 needs you"
	}
	return title, strings.Join(lines, " · ")
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

// The holds' words in an event (toastHold), and the hold each names
// (holdCause, which reads them back after a restart).
const (
	quietWhy        = "quiet hours"
	screenWhy       = "factory screen"
	screenFocusWhy  = "the " + screenWhy + " has your focus"
	paneWhy         = "factory pane"
	paneInWhy       = "you're in a " + paneWhy
	leftWhyTemplate = "you left %s at %s, it fires 2 minutes after"
)

// toastHold is what holds the toasts at now, and why in an event's words
// ("" for nothing): quiet hours, the screen's focus, you in a factory pane.
func (e *Engine) toastHold(now time.Time) (cause, why string) {
	if spec := e.cfg.Daemon.QuietHours; spec != "" && eligibility.QuietHours(spec, now.Local()) {
		return alertHoldQuiet, quietWhy + " (" + spec + ")"
	}
	if why := e.screenHold(now); why != "" {
		return alertHoldScreen, why
	}
	if why := e.paneHold(now); why != "" {
		return alertHoldPane, why
	}
	return "", ""
}

// holdCause is the hold an event's words name ("" for none known).
func holdCause(why string) string {
	switch {
	case strings.HasPrefix(why, quietWhy):
		return alertHoldQuiet
	case strings.Contains(why, screenWhy):
		return alertHoldScreen
	case strings.Contains(why, paneWhy):
		return alertHoldPane
	}
	return ""
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
		return screenFocusWhy
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
		return fmt.Sprintf(leftWhyTemplate, "the "+screenWhy, clock(left))
	}
	return ""
}

// paneHold is why you in a factory pane hold the toasts ("" for nothing).
// Only herdr showing you at work in a factory pane (PaneFocus.Since)
// proves you; then you are in one while herdr's focus is on one, for
// [factory] human_cooldown after that proof (taken as left at its end),
// and a hold lasts alertMerge after you left. While herdr is held down no
// pass reads it, and you are in none of its panes.
func (e *Engine) paneHold(now time.Time) string {
	if e.d.Runtime == nil {
		return ""
	}
	f := e.d.Runtime.PaneFocus()
	if f.Since.IsZero() {
		return ""
	}
	end := config.Daemon{HumanCooldown: e.cfg.Factory.HumanCooldown}.CooldownUntil(&f.Since) // the factory's, as the runtime's hold
	left := f.Left
	if _, down := e.holds[HoldHerdr]; f.Pane != "" && !down {
		if now.Before(end) {
			return paneInWhy
		}
		left = end
	}
	if !left.IsZero() && !end.IsZero() && end.Before(left) {
		left = end
	}
	if !left.IsZero() && now.Before(left.Add(alertMerge)) {
		return fmt.Sprintf(leftWhyTemplate, "a "+paneWhy, clock(left))
	}
	return ""
}

// sidebarTokens reports each task's sidebar token on each workspace of its
// live sessions when it changed there, when the workspace is new, after a
// start or after a herdr hold, and before herdr's TTL would drop it:
// "needs you: <what>" while the task waits on you, empty otherwise. No
// toast hold applies to it, and one deadline bounds the whole pass. A
// report that fails is tried again on the next tick.
func (e *Engine) sidebarTokens(ctx context.Context, now time.Time, tasks []registry.Task) {
	if e.d.Sidebar == nil || e.d.Runtime == nil {
		return
	}
	a := &e.alerts
	if _, down := e.holds[HoldHerdr]; down {
		a.herdrDown = true
		return
	}
	if a.herdrDown { // herdr is back: what it showed may be gone
		a.herdrDown = false
		clear(a.tokens)
	}
	pctx, cancel := context.WithTimeout(ctx, sidebarPassTimeout)
	defer cancel()
	for _, t := range tasks {
		if pctx.Err() != nil {
			e.log.Debug("the sidebar tokens' pass ran out of time; the rest go on the next tick")
			return
		}
		if t.State == registry.TaskDone || t.State == registry.TaskStopped { // its workspaces closed with it
			delete(a.tokens, t.ID)
			continue
		}
		ws, err := e.d.Runtime.Workspaces(pctx, t)
		if err != nil {
			e.log.Warn("a task's sidebar token: read its workspaces", "task", t.ID, "err", err)
			continue
		}
		want := ""
		if waitsOnYou(t) {
			want = "needs you: " + alertShort(t.WaitsWhat)
		}
		was, is := a.tokens[t.ID], map[string]sidebarToken{} // a workspace it no longer has is forgotten
		for _, w := range ws {
			if s, ok := was[w]; ok && s.value == want && now.Sub(s.at) < sidebarRefresh {
				is[w] = s
				continue
			}
			if err := e.d.Sidebar(pctx, w, map[string]string{sidebarKey: want}); err != nil {
				e.log.Debug("a task's sidebar token; tried again on the next tick", "task", t.ID, "workspace", w, "err", err)
				continue
			}
			is[w] = sidebarToken{value: want, at: now}
		}
		a.tokens[t.ID] = is
	}
}

// tasksByID indexes tasks by id.
func tasksByID(tasks []registry.Task) map[int64]registry.Task {
	byID := make(map[int64]registry.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	return byID
}

// alertLine is an alert's line in a toast, which also names its kind of
// wait (alertStands): its key and its reason, short ("PR-12345 questions",
// "factory disk").
func alertLine(text string) string {
	who, what, ok := strings.Cut(text, ": ")
	if !ok {
		return textx.Clip(text, alertWhatRunes)
	}
	return who + " " + alertShort(strings.TrimPrefix(what, waitsYou+": "))
}

// alertShort is a wait's reason as a toast's line and the sidebar token
// say it: "questions" (however many), "ready to push", else the screen's
// short form of it or its first words.
func alertShort(what string) string {
	switch {
	case questionsWaitRe.MatchString(what):
		return "questions"
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
