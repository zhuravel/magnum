package tui

// The factory screen's own action bar (spec 13.2): the y/N question before an
// action, the one action in flight, the footer line with its outcome, and the
// action log every outcome goes to. It is a narrow adapter beside the board's
// actionBar, not a change of it: the live board and dashboard keep their
// lifecycle (actions.go, confirm.go) as it is.
//
// The bar knows no factory request, only closures. The screen asks a question
// with the closure that does the work, forwards its keys and the bar's
// messages, and draws view in its footer:
//
//	cmd := bar.ask("Push PR-12301's branch and open the PR?", func(ctx context.Context) (ActionResult, error) { ... })
//	cmd, taken := bar.key(msg)    // in Update, for a key press, before the screen's own keys
//	cmd, taken := bar.update(msg) // in Update, for the messages the bar sent itself
//
// An action returns its text only: the bar logs the ActionResult as it is but
// does not follow a Request it names (the board's follow does that), so a
// closure of the factory leaves Requests empty.

import (
	"cmp"
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// factoryAction is the work a question stands for. It runs on the bar's
// context, so it stops when the screen closes; its outcome goes to the log
// either way.
type factoryAction func(ctx context.Context) (ActionResult, error)

type (
	// factoryDoneMsg is the end of the action in flight, for the bar's footer
	// line. The log already has it: the action's command writes it there
	// itself, so a screen that quit before the message arrived loses nothing.
	factoryDoneMsg struct {
		label, text string
		err         error
	}
	// factoryExpireMsg ends the footer note numbered seq, unless a newer note
	// replaced it.
	factoryExpireMsg struct{ seq int }
)

// factoryFlashKind says how the footer line is drawn; each kind also reads
// as such in its words ("wait:", "cancelled", "failed:").
type factoryFlashKind int

const (
	factoryFlashDone   factoryFlashKind = iota // a finished action: green
	factoryFlashNote                           // a cancelled question: dim
	factoryFlashWait                           // a refused question: yellow
	factoryFlashFailed                         // a failed action: red, stays until a key
)

// factoryQuestion is the question waiting for y/N.
type factoryQuestion struct {
	sentence string // as the bar asks it, ending in "?"
	label    string // the action's name in the footer and the log: the sentence without its "?"
	run      factoryAction
}

// factoryBar is the action state the factory screen embeds. One action runs
// at a time, in the background: the screen's navigation keys keep working
// while it runs, and only a new question is refused.
type factoryBar struct {
	ctx  context.Context // the screen's: it ends when the screen closes
	log  *ActionLog
	now  func() time.Time
	tick func(time.Duration) tea.Cmd // answers with a message after the delay; the bar wraps it in its own
	st   styles

	asking *factoryQuestion // the question awaiting y/N
	busy   string           // the label of the action in flight

	flash     string
	flashKind factoryFlashKind
	flashSeq  int
}

func newFactoryBar(ctx context.Context, log *ActionLog, now func() time.Time, tick func(time.Duration) tea.Cmd) factoryBar {
	if log == nil {
		log = NewActionLog()
	}
	if now == nil {
		now = time.Now
	}
	if tick == nil {
		tick = func(d time.Duration) tea.Cmd { return tea.Tick(d, func(time.Time) tea.Msg { return nil }) }
	}
	return factoryBar{ctx: ctx, log: log, now: now, tick: tick, st: newStyles(true)}
}

// applyStyles switches the bar to st, once the terminal reports its background.
func (b *factoryBar) applyStyles(st styles) { b.st = st }

// ask puts the y/N question sentence in the footer and holds run for the
// answer. It runs nothing itself: only y or Y answered through key does. While
// an action runs, or another question waits, the new one is refused with a
// note and run is dropped.
func (b *factoryBar) ask(sentence string, run factoryAction) tea.Cmd {
	if run == nil {
		return nil
	}
	switch {
	case b.busy != "":
		return b.show("wait: "+b.busy+" runs", factoryFlashWait)
	case b.asking != nil:
		return b.show("wait: "+b.asking.label+" waits for y/N", factoryFlashWait)
	}
	sentence = cleanText(sentence)
	label := cmp.Or(strings.TrimSpace(strings.TrimSuffix(sentence, "?")), "action")
	b.asking = &factoryQuestion{sentence: sentence, label: label, run: run}
	b.flash = ""
	return nil
}

// key takes a key press: with a question up, every key answers it (y or Y
// starts the action, any other key cancels it) and taken is true. Without
// one, taken is false and the screen handles the key; a failure kept in the
// footer is cleared by it first.
func (b *factoryBar) key(msg tea.KeyPressMsg) (cmd tea.Cmd, taken bool) {
	q := b.asking
	if q == nil {
		if b.flashKind == factoryFlashFailed {
			b.flash = ""
		}
		return nil, false
	}
	b.asking = nil
	if !confirms(msg.String()) {
		return b.show(q.label+": cancelled, nothing was run", factoryFlashNote), true
	}
	b.busy, b.flash = q.label, ""
	// The command holds copies, not b: the screen's model is copied on every
	// update, and the action ends on its own goroutine.
	ctx, log, now, label, run := b.ctx, b.log, b.now, q.label, q.run
	return func() tea.Msg {
		res, err := run(ctx)
		e := logEntry{at: now(), what: label, text: res.Text, reqs: res.Requests}
		if err != nil {
			e.err = err.Error()
		}
		log.add(e)
		return factoryDoneMsg{label: label, text: res.Text, err: err}
	}, true
}

// update takes the messages the bar sent itself; taken is false for any other.
func (b *factoryBar) update(msg tea.Msg) (cmd tea.Cmd, taken bool) {
	switch msg := msg.(type) {
	case factoryDoneMsg:
		b.busy = ""
		if msg.err != nil {
			return b.show(msg.label+" failed: "+errLine(msg.err), factoryFlashFailed), true
		}
		return b.show(cmp.Or(lastLine(msg.text), msg.label+": done"), factoryFlashDone), true
	case factoryExpireMsg:
		if msg.seq == b.flashSeq && b.flashKind != factoryFlashFailed {
			b.flash = ""
		}
		return nil, true
	}
	return nil, false
}

// show puts text in the footer. A failure stays until a key; any other note
// ends flashFor later. text may quote an action's output: its escape
// sequences and control characters are dropped here, once.
func (b *factoryBar) show(text string, kind factoryFlashKind) tea.Cmd {
	b.flash, b.flashKind = cleanText(text), kind
	b.flashSeq++
	if kind == factoryFlashFailed {
		return nil
	}
	seq, wait := b.flashSeq, b.tick(flashFor)
	if wait == nil {
		return nil
	}
	// Whatever message the tick answers with, the bar learns which note ended.
	return func() tea.Msg {
		wait()
		return factoryExpireMsg{seq: seq}
	}
}

// view is the footer line in width cells: the question, else the note, else
// the action in flight; "" when there is none.
func (b *factoryBar) view(width int) string {
	switch {
	case b.asking != nil:
		return b.st.confirmLine(b.asking.sentence, width)
	case b.flash != "":
		return b.flashStyle().Render(b.flashFit(width))
	case b.busy != "":
		return b.st.Dim.Render(truncate("busy: "+b.busy+"…", width))
	}
	return ""
}

func (b *factoryBar) flashStyle() lipgloss.Style {
	switch b.flashKind {
	case factoryFlashFailed:
		return b.st.Err
	case factoryFlashWait:
		return b.st.Warn
	case factoryFlashNote:
		return b.st.Dim
	}
	return b.st.OK
}

// flashFit is the note in room cells: whole when it fits, else cut with a
// pointer to the action log, which keeps the whole text (as actionBar's does).
func (b *factoryBar) flashFit(room int) string {
	if room <= 0 || ansi.StringWidth(b.flash) <= room {
		return b.flash
	}
	more := "… (" + logKey + " shows all)"
	if room <= ansi.StringWidth(more)+8 {
		return truncate(b.flash, room)
	}
	return ansi.Truncate(b.flash, room-ansi.StringWidth(more), "") + more
}
