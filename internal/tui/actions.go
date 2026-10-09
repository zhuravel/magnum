package tui

// The action lifecycle the status dashboard and the PR board share: the
// y/N question before an action, the one action in flight, the footer
// flash with its result, the daemon requests it left pending (followed on
// every refresh until the daemon answers, actlog.go), and leaving the screen
// only once that action is done.

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type (
	actionFunc func(ctx context.Context, a DashboardActions) (ActionResult, error)

	// actionDoneMsg is a finished action's outcome: what it printed, the
	// requests it queued and the PR (or slot) it acted on.
	actionDoneMsg struct {
		what, text string
		target     string
		reqs       []Request
		err        error
	}
	// requestsMsg is a re-read of the pending requests asked.
	requestsMsg struct {
		asked []int64
		reqs  []Request
		err   error
	}
	// flashExpireMsg clears the flash numbered seq, unless a newer one
	// replaced it.
	flashExpireMsg struct{ seq int }
	// finishGraceMsg ends the wait for the action in flight after the
	// user quit or switched screens.
	finishGraceMsg struct{}
)

const (
	flashFor = 5 * time.Second
	// finishGrace bounds how long quitting (or tab) waits for the action
	// in flight. The action runs on the screen's context, so leaving
	// earlier would stop it halfway (a release parking sessions, say);
	// after the grace the screen closes and the action is stopped.
	finishGrace = 30 * time.Second
	// loadTimeout bounds one refresh of a screen's data, so a hung source
	// shows "refresh failed" instead of stopping every later refresh.
	loadTimeout = 30 * time.Second
	// logKey shows the action log; a flash cut to the width names it.
	logKey = "!"
)

// actionBar is the action state a screen embeds. One action runs at a
// time, in the background: navigation keys keep working while it runs and
// only action keys are refused.
type actionBar struct {
	ctx context.Context // the screen's: it ends when the screen closes
	act DashboardActions
	log *ActionLog       // shared with the other screen
	now func() time.Time // the screen's clock
	// tick schedules the flash's end and the wait before closing (tea.Tick).
	tick func(time.Duration, func(time.Time) tea.Msg) tea.Cmd

	confirm    *pendingAction // the action awaiting y/N
	busy       string         // the action in flight
	busyTarget string         // the PR (or slot) it acts on
	leaving    bool           // the user quit (or switched): close once busy ends
	switching  bool           // close to switch to the other screen (tab)
	following  bool           // a re-read of the pending requests is in flight

	flash     string
	flashErr  bool
	flashInfo bool // neutral news (a cancelled action), not a result
	// flashStuck keeps a failure on screen until a key is pressed.
	flashStuck bool
	flashSeq   int
}

func newActionBar(ctx context.Context, act DashboardActions, log *ActionLog, now func() time.Time,
	tick func(time.Duration, func(time.Time) tea.Msg) tea.Cmd) actionBar {
	if log == nil {
		log = NewActionLog()
	}
	return actionBar{ctx: ctx, act: act, log: log, now: now, tick: tick}
}

// setFlash shows text in the footer for flashFor.
func (b *actionBar) setFlash(text string, isErr bool) tea.Cmd {
	b.flash, b.flashErr, b.flashInfo, b.flashStuck = text, isErr, false, false
	b.flashSeq++
	seq := b.flashSeq
	return b.tick(flashFor, func(time.Time) tea.Msg { return flashExpireMsg{seq: seq} })
}

// failure shows a failed action's text in red until a key is pressed: it
// may not be read in flashFor (the action log keeps it after).
func (b *actionBar) failure(text string) tea.Cmd {
	b.flash, b.flashErr, b.flashInfo, b.flashStuck = text, true, false, true
	b.flashSeq++
	return nil
}

// note shows neutral news in the footer.
func (b *actionBar) note(text string) tea.Cmd {
	cmd := b.setFlash(text, false)
	b.flashInfo = true
	return cmd
}

// expire clears the flash msg was scheduled for; a failure stays.
func (b *actionBar) expire(msg flashExpireMsg) {
	if msg.seq == b.flashSeq && !b.flashStuck {
		b.flash = ""
	}
}

// keyPressed clears a failure kept on screen: the key that does it acts as
// usual.
func (b *actionBar) keyPressed() {
	if b.flashStuck {
		b.flash, b.flashErr, b.flashStuck = "", false, false
	}
}

// refusal says why an action named what cannot start now; "" when it can.
func (b *actionBar) refusal(what string) string {
	switch {
	case b.act == nil:
		return what + ": actions are not available here"
	case b.busy != "":
		return "still running: " + b.busy + " (action keys wait for it)"
	}
	return ""
}

// start runs fn in the background as what, on target (the PR or slot it
// acts on; "" for none). started is false when it was refused; cmd then
// shows why.
func (b *actionBar) start(what, target string, fn actionFunc) (cmd tea.Cmd, started bool) {
	if r := b.refusal(what); r != "" {
		return b.setFlash(r, true), false
	}
	b.busy, b.busyTarget, b.flash, b.flashStuck = what, target, "", false
	ctx, act := b.ctx, b.act
	return func() tea.Msg {
		res, err := fn(ctx, act)
		return actionDoneMsg{what: what, target: target, text: res.Text, reqs: res.Requests, err: err}
	}, true
}

// answer settles the pending question with key k: y starts the action,
// any other key cancels it.
func (b *actionBar) answer(k string) (cmd tea.Cmd, started bool) {
	p := b.confirm
	b.confirm = nil
	if confirms(k) {
		return b.start(p.what, p.target, p.fn)
	}
	return b.note(p.what + " cancelled"), false
}

// done records a finished action in the log and flashes its outcome: a
// failure until a key is pressed, a request the daemon has not answered yet
// as pending (its row is marked until the answer comes). quit is true when
// the user is waiting to leave; cmd then closes the screen.
func (b *actionBar) done(msg actionDoneMsg) (cmd tea.Cmd, quit bool) {
	b.busy, b.busyTarget = "", ""
	e := logEntry{at: b.clock(), what: msg.what, target: msg.target, text: msg.text, reqs: msg.reqs}
	if msg.err != nil {
		e.err = msg.err.Error()
	}
	b.log.add(e)
	if b.leaving {
		return tea.Quit, true
	}
	if msg.err != nil {
		return b.failure(actionText(msg)), false
	}
	if i := slices.IndexFunc(msg.reqs, Request.pending); i >= 0 {
		return b.note(msg.what + ": " + msg.reqs[i].label() + " is queued; the daemon has not answered yet (its row is marked until it does)"), false
	}
	return b.setFlash(actionText(msg), false), false
}

// follow re-reads the requests still pending in the log, unless a re-read
// is in flight or none is pending; the screens call it on every refresh.
func (b *actionBar) follow() tea.Cmd {
	if b.following || b.act == nil {
		return nil
	}
	ids := b.log.pendingIDs()
	if len(ids) == 0 {
		return nil
	}
	b.following = true
	ctx, act := b.ctx, b.act
	return func() tea.Msg {
		ctx, cancel := loadContext(ctx)
		defer cancel()
		reqs, err := act.Requests(ctx, ids)
		return requestsMsg{asked: ids, reqs: reqs, err: err}
	}
}

// followed takes a re-read of the pending requests: the ones the daemon
// answered settle in the log and flash, a refusal until a key is pressed;
// settled reports that some did. A failed re-read is tried again at the next
// refresh.
func (b *actionBar) followed(msg requestsMsg) (cmd tea.Cmd, settled bool) {
	b.following = false
	if msg.err != nil {
		return nil, false
	}
	answers := b.log.settle(msg.asked, msg.reqs, b.clock())
	if len(answers) == 0 {
		return nil, false
	}
	pick := answers[len(answers)-1]
	if i := slices.IndexFunc(answers, func(a answer) bool { return a.req.State == RequestFailed }); i >= 0 {
		pick = answers[i]
	}
	said := cmp.Or(lastLine(pick.req.Result), "done")
	text := pick.what + ": " + said
	if pick.req.State == RequestFailed {
		text = pick.what + ": " + pick.req.label() + " failed: " + said
	}
	if n := len(answers) - 1; n > 0 {
		text += fmt.Sprintf(" (and %d more: %s shows them)", n, logKey)
	}
	if pick.req.State == RequestFailed {
		return b.failure(text), true
	}
	return b.setFlash(text, false), true
}

// clock is the screen's time (time.Now without one).
func (b *actionBar) clock() time.Time {
	if b.now == nil {
		return time.Now()
	}
	return b.now()
}

// leave closes the screen (to switch to the other one, with switching).
// With an action in flight it first waits for that action, at most
// finishGrace; ctrl+c during the wait closes at once. Either way the screen
// is leaving from now on and the question still up is dropped, so the keys
// Bubble Tea hands it before it handles tea.Quit go to leavingKey: a y
// typed after ctrl+c starts no action on a context about to end.
func (b *actionBar) leave(switching bool) tea.Cmd {
	b.switching, b.confirm = switching, nil
	if b.busy == "" {
		b.leaving = true
		return tea.Quit
	}
	if b.leaving {
		return nil
	}
	b.leaving = true
	return b.tick(finishGrace, func(time.Time) tea.Msg { return finishGraceMsg{} })
}

// leavingKey handles a key while the screen waits to close: ctrl+c
// closes at once (stopping the action), q quits instead of switching,
// tab switches instead of quitting; other keys wait.
func (b *actionBar) leavingKey(k string) tea.Cmd {
	switch k {
	case "ctrl+c":
		return tea.Quit
	case "q", "esc":
		b.switching = false
	case "tab":
		b.switching = true
	}
	return nil
}

// graceOver closes the screen when the wait for the action ran out.
func (b *actionBar) graceOver() tea.Cmd {
	if b.leaving {
		return tea.Quit
	}
	return nil
}

// busySuffix is what a flash adds so the action in flight stays visible;
// "" when nothing runs or the flash already names it.
func (b *actionBar) busySuffix() string {
	if b.busy == "" || strings.Contains(b.flash, b.busy) {
		return ""
	}
	return " · " + b.busyText()
}

// busyText is the footer's line for the action in flight.
func (b *actionBar) busyText() string {
	switch {
	case b.busy == "":
		return ""
	case b.leaving:
		where := "quitting"
		if b.switching {
			where = "switching screens"
		}
		return "finishing " + b.busy + " before " + where + "… ctrl+c stops it now"
	}
	return "busy: " + b.busy + "…"
}

// flashFit is the flash in room cells: whole when it fits, else cut with a
// pointer to the action log, which keeps the whole text.
func (b *actionBar) flashFit(room int) string {
	if room <= 0 || ansi.StringWidth(b.flash) <= room {
		return b.flash
	}
	more := "… (" + logKey + " shows all)"
	if room <= ansi.StringWidth(more)+8 {
		return truncate(b.flash, room)
	}
	return ansi.Truncate(b.flash, room-ansi.StringWidth(more), "") + more
}

// actionText is the footer line for a finished action.
func actionText(msg actionDoneMsg) string {
	if msg.err != nil {
		return msg.what + ": " + oneLine(msg.err.Error())
	}
	if t := lastLine(msg.text); t != "" {
		return t
	}
	return msg.what + ": done"
}

// loadContext bounds one refresh of a screen's data by loadTimeout.
func loadContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, loadTimeout)
}
