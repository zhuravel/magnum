package tui

// The action lifecycle the status dashboard and the PR board share: the
// y/N question before an action, the one action in flight, the footer
// flash with its result, and leaving the screen only once that action is
// done.

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

type (
	actionFunc    func(ctx context.Context, a DashboardActions) (string, error)
	refActionFunc func(ctx context.Context, a DashboardActions, ref string) (string, error)

	// actionDoneMsg is a finished action's outcome.
	actionDoneMsg struct {
		what, text string
		err        error
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
)

// actionBar is the action state a screen embeds. One action runs at a
// time, in the background: navigation keys keep working while it runs and
// only action keys are refused.
type actionBar struct {
	ctx context.Context // the screen's: it ends when the screen closes
	act DashboardActions

	confirm   *pendingAction // the action awaiting y/N
	busy      string         // the action in flight
	leaving   bool           // the user quit (or switched): close once busy ends
	switching bool           // close to switch to the other screen (tab)

	flash     string
	flashErr  bool
	flashInfo bool // neutral news (a cancelled action), not a result
	flashSeq  int
}

func newActionBar(ctx context.Context, act DashboardActions) actionBar {
	return actionBar{ctx: ctx, act: act}
}

// setFlash shows text in the footer for flashFor.
func (b *actionBar) setFlash(text string, isErr bool) tea.Cmd {
	b.flash, b.flashErr, b.flashInfo = text, isErr, false
	b.flashSeq++
	seq := b.flashSeq
	return tea.Tick(flashFor, func(time.Time) tea.Msg { return flashExpireMsg{seq: seq} })
}

// note shows neutral news in the footer.
func (b *actionBar) note(text string) tea.Cmd {
	cmd := b.setFlash(text, false)
	b.flashInfo = true
	return cmd
}

// expire clears the flash msg was scheduled for.
func (b *actionBar) expire(msg flashExpireMsg) {
	if msg.seq == b.flashSeq {
		b.flash = ""
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

// start runs fn in the background as what. started is false when it was
// refused; cmd then shows why.
func (b *actionBar) start(what string, fn actionFunc) (cmd tea.Cmd, started bool) {
	if r := b.refusal(what); r != "" {
		return b.setFlash(r, true), false
	}
	b.busy, b.flash = what, ""
	ctx, act := b.ctx, b.act
	return func() tea.Msg {
		text, err := fn(ctx, act)
		return actionDoneMsg{what: what, text: text, err: err}
	}, true
}

// ask puts fn to the user: the footer asks question and y runs it as
// what. Missing actions or an action still running fail at once instead
// of asking.
func (b *actionBar) ask(question, what string, fn actionFunc) tea.Cmd {
	if r := b.refusal(what); r != "" {
		return b.setFlash(r, true)
	}
	b.confirm = &pendingAction{question: question, what: what, fn: fn}
	return nil
}

// answer settles the pending question with key k: y starts the action,
// any other key cancels it.
func (b *actionBar) answer(k string) (cmd tea.Cmd, started bool) {
	p := b.confirm
	b.confirm = nil
	if confirms(k) {
		return b.start(p.what, p.fn)
	}
	return b.note(p.what + " cancelled"), false
}

// done records a finished action. quit is true when the user is waiting
// to leave; cmd then closes the screen.
func (b *actionBar) done(msg actionDoneMsg) (cmd tea.Cmd, quit bool) {
	b.busy = ""
	if b.leaving {
		return tea.Quit, true
	}
	return b.setFlash(actionText(msg), msg.err != nil), false
}

// leave closes the screen (to switch to the other one, with switching).
// With an action in flight it first waits for that action, at most
// finishGrace; ctrl+c during the wait closes at once.
func (b *actionBar) leave(switching bool) tea.Cmd {
	b.switching = switching
	if b.busy == "" {
		return tea.Quit
	}
	if b.leaving {
		return nil
	}
	b.leaving = true
	return tea.Tick(finishGrace, func(time.Time) tea.Msg { return finishGraceMsg{} })
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
