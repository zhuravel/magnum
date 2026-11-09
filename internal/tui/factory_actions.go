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
//	cmd := bar.start(label, run)  // an action that asks nothing (o: step into the pane)
//	bar.drop()                    // the screen quits: a question up is dropped, and nothing runs after
//
// The bar follows no request: it logs an action's text and error, never the
// Requests an ActionResult names, which only the board's actionBar re-reads.
// f, F, K and o on a PR's row run the board's own code, which names the
// requests it queued; their text already says the daemon's answer, or how to
// follow a request still pending, and a logged request nothing re-reads would
// say "no answer yet" for good.

import (
	"cmp"
	"context"
	"slices"
	"strconv"
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
		next        *factoryQuestion // the y/N the action's result asks before its next step (ActionResult.nextFactory)
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
	// wrap: the question wraps over the footer's lines instead of being
	// cut at the screen's edge, as the board's pendingAction.wrap, since y
	// answers all it names: a staging deploy's migrations and its warning.
	wrap bool
	// plan says which row's plan the question is the next step of (E's
	// staging deploy, P's merge of the parent); nil for any other question.
	plan *planOf
}

// planOf is the row a plan's question belongs to: the row's id
// (FactoryRow.ID), the key that planned it (E or P), the row's name for its
// PR and the PR's reference.
type planOf struct {
	row            int64
	key, name, ref string
}

// heldPlan is a plan's question that answered away from its row, and when
// it answered, by the screen's clock: it waits for its key on its row
// (factoryBar.raiseHeld), and the screen drops it once it cannot wait any
// more (factoryModel.dropHeld).
type heldPlan struct {
	q  *factoryQuestion
	at time.Time
}

// factoryBar is the action state the factory screen embeds. One action runs
// at a time, in the background: the screen's navigation keys keep working
// while it runs, and only a new question is refused.
type factoryBar struct {
	ctx context.Context // the screen's: it ends when the screen closes
	log *ActionLog
	now func() time.Time // read on the screen's turn only (key, start), never on an action's goroutine
	// tick schedules the end of a note, as the screens' Tick does (tea.Tick):
	// its command answers with the bar's own message.
	tick func(time.Duration, func(time.Time) tea.Msg) tea.Cmd
	st   styles

	asking    *factoryQuestion // the question awaiting y/N
	choose    *stagingChoice   // E's chooser of a PR's staging envs, before its plan (staging_key.go); nil when none is up
	chooseRow int64            // the row the chooser is of (FactoryRow.ID)
	busy      string           // the label of the action in flight
	dropped   bool             // the screen quits: no question is asked or answered any more
	// held are the plans' questions (E's, P's) that answered away from
	// their rows, oldest first, one per row and key: none comes up by
	// itself, only by its key on its row (raiseHeld), so no y/N appears
	// that the operator did not ask for there. The rule says they wait
	// (heldMarker) while it has nothing else to say.
	held []heldPlan

	flash     string
	flashKind factoryFlashKind
	flashSeq  int
}

func newFactoryBar(ctx context.Context, log *ActionLog, now func() time.Time, tick func(time.Duration, func(time.Time) tea.Msg) tea.Cmd) factoryBar {
	if ctx == nil { // an action never runs on a nil context
		ctx = context.Background()
	}
	if log == nil {
		log = NewActionLog()
	}
	if now == nil {
		now = time.Now
	}
	if tick == nil {
		tick = tea.Tick
	}
	return factoryBar{ctx: ctx, log: log, now: now, tick: tick, st: newStyles(true)}
}

// drop is the screen quitting: a question up is dropped unanswered, and the
// bar asks, answers and starts nothing after, so a y queued behind ctrl+c
// never starts a push. An action already running ends with the screen's
// context, and the log keeps its outcome.
func (b *factoryBar) drop() {
	b.asking, b.choose, b.held, b.dropped = nil, nil, nil, true
}

// hold keeps q, a plan's question that answered, for its key on its row:
// one per row and key, a newer one in place of an older.
func (b *factoryBar) hold(q *factoryQuestion) {
	if i := b.heldOf(q.plan); i >= 0 {
		b.held = slices.Delete(b.held, i, i+1)
	}
	b.held = append(b.held, heldPlan{q: q, at: b.now()})
}

// heldOf is the index of the held plan of p's row and key; -1 for none.
func (b *factoryBar) heldOf(p *planOf) int {
	if p == nil {
		return -1
	}
	return slices.IndexFunc(b.held, func(h heldPlan) bool {
		return h.q.plan != nil && h.q.plan.row == p.row && h.q.plan.key == p.key
	})
}

// raiseHeld puts up the held question of row's plan by key, for y/N, as
// ask puts a question up (a failure kept in the footer stays beside it),
// and reports whether there was one to put up. It puts none up while
// another question, the chooser or an action holds the bar.
func (b *factoryBar) raiseHeld(row int64, key string) bool {
	i := b.heldOf(&planOf{row: row, key: key})
	if i < 0 || b.dropped || b.asking != nil || b.choose != nil || b.busy != "" {
		return false
	}
	b.asking = b.held[i].q
	b.held = slices.Delete(b.held, i, i+1)
	if len(b.held) == 0 {
		b.held = nil
	}
	if b.flashKind != factoryFlashFailed {
		b.flash = ""
	}
	return true
}

// heldMarker is the rule's word of the held plans: the oldest's row and
// key ("plan of PR-12345 ready: E on its row asks") and how many more;
// "" for none.
func (b *factoryBar) heldMarker() string {
	if len(b.held) == 0 {
		return ""
	}
	p := b.held[0].q.plan
	text := "plan of " + p.name + " ready: " + p.key + " on its row asks"
	if n := len(b.held) - 1; n > 0 {
		text += " (+" + strconv.Itoa(n) + " more)"
	}
	return text
}

// dropHeldNote is the note of held plan h dropped for why: in the action
// log always, and in the footer only while it says nothing else, so it
// never takes the place of another action's outcome or failure.
func (b *factoryBar) dropHeldNote(h heldPlan, why string) tea.Cmd {
	key := "its key"
	if h.q.plan != nil {
		key = h.q.plan.key
	}
	text := h.q.label + ": dropped, " + why + "; nothing was run (" + key + " on its row plans again)"
	b.log.add(logEntry{at: b.now(), what: h.q.label, text: text})
	if b.flash != "" {
		return nil
	}
	return b.show(text, factoryFlashNote)
}

// applyStyles switches the bar to st, once the terminal reports its background.
func (b *factoryBar) applyStyles(st styles) { b.st = st }

// ask puts the y/N question sentence in the footer and holds run for the
// answer. It runs nothing itself: only y or Y answered through key does. While
// an action runs, or another question waits, the new one is refused with a
// note and run is dropped.
//
// A question asked while another waits is dropped without a note: the
// question on the bar is what waits, and it stays drawn.
func (b *factoryBar) ask(sentence string, run factoryAction) tea.Cmd {
	switch {
	case run == nil || b.dropped || b.asking != nil || b.choose != nil:
		return nil
	case b.busy != "":
		return b.show("wait: "+b.busy+" runs", factoryFlashWait)
	}
	sentence = cleanText(sentence)
	label := cmp.Or(strings.TrimSpace(strings.TrimSuffix(sentence, "?")), "action")
	b.asking = &factoryQuestion{sentence: sentence, label: label, run: run}
	if b.flashKind != factoryFlashFailed { // a failure stays until a key, beside the question on a card
		b.flash = ""
	}
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
	return b.run(q.label, q.run), true
}

// cancel answers a question up with no, as any key but y does (a click
// while it waits), and closes E's chooser up; it reports whether there was
// either.
func (b *factoryBar) cancel() (tea.Cmd, bool) {
	if c := b.choose; c != nil {
		b.choose = nil
		return b.show("Deploy "+c.label+" to staging: cancelled, nothing was run", factoryFlashNote), true
	}
	if b.asking == nil {
		return nil, false
	}
	q := b.asking
	b.asking = nil
	return b.show(q.label+": cancelled, nothing was run", factoryFlashNote), true
}

// start runs an action that asks nothing (stepping into a pane): at once,
// unless another runs or a question waits; its outcome goes to the footer
// and the log as any other's.
func (b *factoryBar) start(label string, run factoryAction) tea.Cmd {
	switch {
	case run == nil || b.dropped || b.asking != nil || b.choose != nil:
		return nil
	case b.busy != "":
		return b.show("wait: "+b.busy+" runs", factoryFlashWait)
	}
	return b.run(cmp.Or(strings.TrimSpace(cleanText(label)), "action"), run)
}

// run starts run in the background. Its time is taken here, on the
// screen's turn: the clock is never read on the action's goroutine.
func (b *factoryBar) run(label string, run factoryAction) tea.Cmd {
	b.busy, b.flash = label, ""
	// The command holds copies, not b: the screen's model is copied on every
	// update, and the action ends on its own goroutine.
	ctx, log, at := b.ctx, b.log, b.now()
	return func() tea.Msg {
		res, err := run(ctx)
		e := logEntry{at: at, what: label, text: res.Text} // no reqs: the bar follows none
		if err != nil {
			e.err = err.Error()
		}
		log.add(e)
		return factoryDoneMsg{label: label, text: res.Text, err: err, next: res.nextFactory}
	}
}

// update takes the messages the bar sent itself; taken is false for any other.
func (b *factoryBar) update(msg tea.Msg) (cmd tea.Cmd, taken bool) {
	switch msg := msg.(type) {
	case factoryDoneMsg:
		b.busy = ""
		if msg.err != nil {
			return b.show(msg.label+" failed: "+errLine(msg.err), factoryFlashFailed), true
		}
		if msg.next != nil && !b.dropped { // a plan: its question, cleaned (the daemon's words), is held; the screen raises it on its row
			q := *msg.next
			q.sentence, q.label = cleanText(q.sentence), cleanText(q.label)
			b.hold(&q)
			b.flash = ""
			return nil, true
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
	seq := b.flashSeq
	return b.tick(flashFor, func(time.Time) tea.Msg { return factoryExpireMsg{seq: seq} })
}

// view is the footer line in width cells: the question, else E's chooser,
// else the note, else the action in flight; "" when there is none.
func (b *factoryBar) view(width int) string {
	switch {
	case b.asking != nil:
		return b.st.confirmLine(b.asking.sentence, width)
	case b.choose != nil:
		return b.choose.line(b.st, width)
	}
	return b.note(width)
}

// askLines are the question's lines in width cells, over at most room
// lines (the board's askLines): one, unless it wraps and does not fit on
// one, "y/N" ending the last; none when no question waits.
func (b *factoryBar) askLines(width, room int) []string {
	if b.asking == nil {
		return nil
	}
	return b.st.askLines(&pendingAction{question: b.asking.sentence, wrap: b.asking.wrap}, width, room)
}

// asksWrapped reports whether the question waiting wraps (factoryQuestion.wrap).
func (b *factoryBar) asksWrapped() bool { return b.asking != nil && b.asking.wrap }

// question is the sentence waiting for y/N; "" when none waits.
func (b *factoryBar) question() string {
	if b.asking == nil {
		return ""
	}
	return b.asking.sentence
}

// note is the footer's note in width cells, else the action in flight,
// else the held plans' marker, without the question: a card draws its
// question in its own footer and this on its rule.
func (b *factoryBar) note(width int) string {
	switch {
	case b.flash != "":
		return b.flashStyle().Render(b.flashFit(width))
	case b.busy != "":
		return b.st.Dim.Render(truncate("busy: "+b.busy+"…", width))
	case len(b.held) > 0:
		return b.st.Warn.Render(truncate(b.heldMarker(), width))
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
