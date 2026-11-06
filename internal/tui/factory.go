package tui

// The factory's screen (spec 9): `magnum factory` with no subcommand. One
// line per task on home, grouped by who it waits on, and the cards behind
// them: the questions before coding with their recap, the ready card, the
// task card and new task. The painters (factory_view.go, factory_card*.go)
// draw the frames; this model holds what the operator does between two
// frames of the factory's data: the cursor, the answers not sent yet, the
// ready card's mark and the time its diff was read, the new task's text.
//
// Two interfaces stand between it and the factory: FactorySource reads the
// data, FactoryActions makes the requests (the cli adapts the factory's
// registries and requests into them). Every action that leaves the Mac,
// starts or stops agents or frees a copy asks y/N on the factory's own bar
// (factory_actions.go); only y confirms. An answer to a question asks
// nothing (the batch's y sends it), and stepping into a pane or opening a
// page in the browser asks nothing.
//
// The keys of spec 9.D for milestone 1a: on home j k g G move, enter opens
// the row's card, a walks the decisions oldest first, o steps into the
// row's pane, b and t open its PR and its Jira issue in the browser, n
// starts a new task, ! is the action log with the alerts, ? the help and
// its words, `,` settings, m the mouse, ctrl+r a refresh and q quits; the
// cards' keys are in factory_keys.go. The board's letters keep their
// meaning: b and t open the PR and the issue (on home and the task card),
// and r, p and x do nothing here, except x on the task card (drop, y/N) and
// r there for a task that can go on (retry, y/N).

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// FactorySource reads what the factory screen shows. The screen calls Frame
// from a command, on its refresh and after each action, with a context that
// ends when the screen closes.
type FactorySource interface {
	Frame(ctx context.Context) (FactoryData, error)
}

// FactoryData is one read of the factory: home's rows and chips and every
// task's cards. Rows are drawn grouped by Group (factory_rows.go); Cards
// are by task id (FactoryRow.ID).
type FactoryData struct {
	Repo     string
	Rows     []FactoryRow
	Blocking []string // the machine facts that stop work ("memory full: new tasks wait")
	RuleChip string   // "1 rule to approve (L)"
	Digest   *Digest  // what finished while the operator was away; nil: none
	Cards    map[int64]FactoryCards

	// BlockingLong are Blocking with their detail, for a wide screen.
	BlockingLong []string
	// Decisions counts the single answers owed across all tasks ("5
	// decisions"), apart from the tasks.
	Decisions int
	// Next is what the operator does next when nothing waits on them.
	Next string
	// Flash is the last news for the status rule ("14:02 you said MFA done →
	// bart deploy resumed"), drawn green when the screen has none of its own.
	Flash string
	// Alerts are the alert events, newest first: ! lists every one with its
	// time (spec 9.F: nothing vanishes).
	Alerts []FactoryAlert
}

// FactoryAlert is one alert the factory raised: when, and what it said.
type FactoryAlert struct {
	At   time.Time
	Text string
}

// FactoryCards are a task's cards: its task card always, and the decision
// card it waits on, if any: its questions or its ready card.
type FactoryCards struct {
	Task     TaskCard
	Question *QuestionCard
	Ready    *ReadyCard
	// Bind is the ready card's head and hashes as the factory drew it; the
	// screen fills in its own times and sends it with the decision.
	Bind ReadyBinding
}

// ReadyBinding binds a decision on the ready card to what the card showed:
// the head and the hashes of the report and the diff (from the source), and
// the screen's own times: when the card was first shown, when the diff was
// first opened and how long the pager was open in all (zero: never).
type ReadyBinding struct {
	HeadSHA, ReportHash, DiffHash string
	DiffOpened, Shown             time.Time
	DiffDwell                     time.Duration
}

// The ready card's choices, as Decide sends them.
const (
	FactoryPush     = "push"
	FactorySendBack = "send_back"
	FactoryDrop     = "drop"
)

// ErrQuestionsChanged is the refusal of a batch of answers to questions that
// newer evidence replaced: nothing of it was kept. Answer returns it (or a
// *QuestionsChangedError, which is it) and the recap says so (spec 9.C).
var ErrQuestionsChanged = errors.New("the questions changed, nothing was sent")

// QuestionsChangedError is ErrQuestionsChanged with when the questions
// changed and, when the factory knows, why ("Question 2 is open again.").
type QuestionsChangedError struct {
	At  time.Time
	Why string
}

func (e *QuestionsChangedError) Error() string {
	if e.At.IsZero() {
		return ErrQuestionsChanged.Error()
	}
	return "the questions changed at " + e.At.Local().Format("15:04") + ", nothing was sent"
}

// Is makes every QuestionsChangedError match ErrQuestionsChanged.
func (e *QuestionsChangedError) Is(target error) bool { return target == ErrQuestionsChanged }

// FactoryActions are the screen's requests to the factory. Each runs from a
// command on the screen's context; the ones that change something return
// what they did (its last line goes to the footer, all of it to the log).
//
//   - Start starts a task from the new task's text, of kind ("" = the
//     factory picks it), in repository repo ("" = the one the text tells);
//     Preview is what the factory makes of a text in repo, making nothing.
//   - Answer sends a batch for the questions of revision rev, by question
//     index from 0: an option's label, the operator's words, or
//     "ask:<login>"; needless flags questions with the key of the reason.
//   - Decide decides on the ready card: FactoryPush, FactorySendBack (with
//     the reason's key and the note) or FactoryDrop.
//   - Stop drops a task (frees its copy, keeps its branch); Retry lets a
//     task that waits on the operator go on at its stage.
//   - Open steps into the task's pane (asks nothing).
//   - OpenBrowser opens url in the browser: b a task's PR, t its Jira issue
//     (asks nothing, as the board's b and t).
//   - Seen says the operator saw home at at: the first key after a digest,
//     and any key or click at most once a minute (the digest counts the
//     absence from the last input); Focus that the screen's terminal
//     gained or lost focus at at, the event's time (the factory holds its
//     toasts then).
//   - Diff is the pager on the task's diff, run with tea.ExecProcess; nil:
//     there is no diff to show. It is called on the screen's own turn, so
//     it only builds the command: it reads and waits for nothing.
type FactoryActions interface {
	Start(ctx context.Context, text, kind, repo string, wordsOnly bool) (ActionResult, error)
	Preview(ctx context.Context, text, repo string) (NewTaskForm, error)
	Answer(ctx context.Context, taskID int64, rev int, answers map[int]string, needless map[int]string) (ActionResult, error)
	Decide(ctx context.Context, taskID int64, choice, reason, note string, bound ReadyBinding) (ActionResult, error)
	Stop(ctx context.Context, taskID int64) (ActionResult, error)
	Retry(ctx context.Context, taskID int64) (ActionResult, error)
	Open(ctx context.Context, taskID int64) (ActionResult, error)
	OpenBrowser(ctx context.Context, url string) error
	Seen(ctx context.Context, at time.Time) error
	Focus(ctx context.Context, focused bool, at time.Time) error
	Diff(taskID int64) *exec.Cmd
}

// FactoryOptions tune the factory screen.
type FactoryOptions struct {
	Icons   IconMode // the symbols: unicode (default), nerd or ascii
	NoMouse bool     // start with mouse support off ([terminal] mouse = false); m turns it on and off
	Now     func() time.Time
	// Tick schedules the screen's timers (the refresh, the spinner's frames,
	// the end of a footer note, the wait before a preview); default tea.Tick.
	Tick func(time.Duration, func(time.Time) tea.Msg) tea.Cmd
	// Settings, when set, lets `,` open the settings screen over the factory.
	Settings *SettingsScreen
}

// RunFactory shows the factory screen until the operator quits or ctx ends.
// act may be nil: the screen then only shows, and its action keys say so.
func RunFactory(ctx context.Context, src FactorySource, act FactoryActions, opts FactoryOptions) error {
	if src == nil {
		return errors.New("tui: RunFactory needs a source")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends an action still running when the screen closes; the log keeps its outcome
	_, err := runProgram(ctx, withSettings(ctx, newFactoryModel(ctx, src, act, opts), opts.Settings))
	return err
}

const (
	factoryRefresh     = 3 * time.Second
	factoryAnimEvery   = 125 * time.Millisecond
	factoryPreviewWait = 400 * time.Millisecond // the pause in typing before the new task's preview is read
	factoryTitleMax    = 48                     // the runes of a prompt task's title a y/N names
)

type (
	factoryDataMsg struct {
		data FactoryData
		err  error
	}
	factoryTickMsg struct{}
	// factoryAnimMsg is the next frame of the working marks.
	factoryAnimMsg       struct{}
	factoryPreviewDueMsg struct{ seq int }
	factoryPreviewMsg    struct {
		text, repo string
		form       NewTaskForm
		err        error
	}
	// factoryPagerMsg ends the pager on a task's diff.
	factoryPagerMsg struct {
		task int64
		err  error
	}
	// factoryEditorMsg ends the editor on the new task's text: what it wrote.
	factoryEditorMsg struct {
		text string
		err  error
	}
)

// factoryView is what the screen shows.
type factoryView int

const (
	fvHome factoryView = iota
	fvQuestion
	fvRecap
	fvReady
	fvTask
	fvNew
	fvHelp
	fvLog
)

// factoryAnswer is an answer of the screen's batch, not sent yet: the
// option picked (-1 for typed words), what it sends and when it was given.
type factoryAnswer struct {
	option int
	text   string
	at     time.Time
}

// factoryBatch is a task's answers and needless flags not sent yet, for the
// questions of revision rev, by question text: an answer is bound to its
// question, and a new revision keeps only the answers to questions it asks
// again with the same option.
type factoryBatch struct {
	rev      int
	answers  map[string]factoryAnswer
	needless map[string]string // the reason's key
}

// factoryQuestions is the question card's own state: the question on it,
// the mark, whether − waits for a reason's digit, whether your own answer
// is typed, and the recap's "the questions changed".
type factoryQuestions struct {
	cur, cursor int
	reasons     bool
	typing      bool
	changed     *recapChange
}

// factoryReady is the ready card's own state, by task: the mark (-1: none,
// until the operator moves it or opens the diff), when the card was first
// shown, when the diff was first opened and for how long it was read, and
// the send back's reason and note being asked.
//
// The state is bound to the card the factory drew (bind: its head and
// hashes): a card drawn again for a new head is a card not read yet, and
// its state starts over.
type factoryReady struct {
	bind    ReadyBinding
	cursor  int
	shown   time.Time
	diffAt  time.Time
	opening time.Time // the pager runs since
	dwell   time.Duration
	reasons bool
	reason  int    // the send back's reason, an index of factorySendBackReasons
	noting  bool   // the send back's note is typed
	note    string // the note typed last, kept when its y/N is answered no
}

// newReady is the state of a ready card first shown at now.
func newReady(bind ReadyBinding, now time.Time) *factoryReady {
	bind.Shown, bind.DiffOpened, bind.DiffDwell = time.Time{}, time.Time{}, 0
	return &factoryReady{bind: bind, cursor: -1, shown: now}
}

// sameCard reports whether two bindings name one drawn card.
func sameCard(a, b ReadyBinding) bool {
	return a.HeadSHA == b.HeadSHA && a.ReportHash == b.ReportHash && a.DiffHash == b.DiffHash
}

// factoryDraft is the new task's text and what the factory made of it.
type factoryDraft struct {
	area       textarea.Model
	preview    NewTaskForm
	previewFor string // the text preview is for
	pickFor    string // the repository preview is for ("" = the one the text tells)
	inFlight   bool   // a preview is being read
	again      bool   // the text changed while it was: read it again after
	seq        int    // the text's changes, for the pause before a preview
	kind       string // the kind the operator picked with tab; "" = the preview's
	repo       string // the repository the operator picked with shift+tab; "" = the preview's
	cycles     int    // shift+tab presses before the text's first preview, applied when it lands
	askOnce    bool   // enter waits for the preview of the text to ask y/N
}

// fresh reports whether the preview is the one of the text and the pick on
// the form.
func (d factoryDraft) fresh() bool { return d.previewFor == d.area.Value() && d.pickFor == d.repo }

// factoryFollowKind is what happens when an action ends.
type factoryFollowKind int

const (
	followAnswer factoryFollowKind = iota + 1
	followDecide
	followStart
	followTask // a retry: the task card stays
	followStop // a drop from the task card: home, once it is done
)

// factoryFollow is what the screen does when an action it asked ends: an
// answered batch or a decision leaves the card for the next one, a start
// goes home onto its new row.
type factoryFollow struct {
	kind factoryFollowKind
	task int64
	key  string // a start: the key or title of the new task's row
	rev  int    // an answer: the revision of the questions the batch was for
	// sent and sentNeedless are what an answer's batch sent, by question
	// text: once it went through, only they leave the batch.
	sent, sentNeedless map[string]string
}

type factoryModel struct {
	mouseState
	bar  factoryBar
	ctx  context.Context
	src  FactorySource
	act  FactoryActions
	opts FactoryOptions
	st   styles
	g    glyphs

	width, height int

	data     FactoryData
	haveData bool
	loading  bool
	again    bool // an action ended while a load ran: read once more
	loadErr  error

	selID  int64 // home's cursor: a task
	selIdx int   // its place, for when the task leaves home
	view   factoryView
	under  factoryView // where the help and the log go back to
	scroll int         // the help's and the log's
	task   int64       // the task whose card shows
	walk   bool        // a: a decision leads to the next oldest
	walked map[int64]bool

	batches map[int64]*factoryBatch
	readies map[int64]*factoryReady
	cursors map[int64]int // the task card's mark, by task
	q       factoryQuestions
	input   textinput.Model // your own answer, a send back's note
	form    factoryDraft

	asked, inflight *factoryFollow
	selectKey       string // after a start: the row to put the cursor on once it shows

	digestSeen time.Time // the digest the operator dismissed (its Since)
	digestGone bool      // a digest was dismissed
	seenSent   time.Time // when the screen last told the factory you saw home (seeHome)
	anim       int
	animating  bool

	editor   func(ctx context.Context, path string) *exec.Cmd // the editor ctrl+g runs on the new task's text
	draftDir string                                           // where its file is written; "" = the system's temporary directory

	cache *frameCache[factoryFrameKey] // shared by every copy of the model
}

func newFactoryModel(ctx context.Context, src FactorySource, act FactoryActions, opts FactoryOptions) factoryModel {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Tick == nil {
		opts.Tick = tea.Tick
	}
	area := textarea.New()
	area.KeyMap.InsertNewline.SetKeys("ctrl+j")
	area.CharLimit, area.ShowLineNumbers = 0, false
	area.SetWidth(200)
	_ = area.Focus() // the text is drawn by the painter: the blink is not needed
	in := textinput.New()
	in.CharLimit = 500
	_ = in.Focus()
	// The painter draws the caret at the end of the text: the keys that would
	// move the cursor elsewhere (or select, or read the clipboard) are off,
	// so what is typed goes where the caret is.
	ak, ik := &area.KeyMap, &in.KeyMap
	for _, b := range []*key.Binding{&ak.CharacterBackward, &ak.CharacterForward, &ak.WordForward, &ak.WordBackward,
		&ak.LineNext, &ak.LinePrevious, &ak.LineStart, &ak.LineEnd, &ak.InputBegin, &ak.InputEnd, &ak.PageUp, &ak.PageDown,
		&ak.SelectCharacterForward, &ak.SelectCharacterBackward, &ak.SelectWordForward, &ak.SelectWordBackward,
		&ak.SelectLineUp, &ak.SelectLineDown, &ak.SelectAll, &ak.Paste,
		&ik.CharacterForward, &ik.CharacterBackward, &ik.WordForward, &ik.WordBackward, &ik.LineStart, &ik.LineEnd,
		&ik.Paste, &ik.AcceptSuggestion, &ik.NextSuggestion, &ik.PrevSuggestion} {
		b.SetEnabled(false)
	}
	m := factoryModel{
		bar: newFactoryBar(ctx, nil, opts.Now, opts.Tick), ctx: ctx, src: src, act: act, opts: opts,
		g: newGlyphs(opts.Icons), loading: true, selIdx: 0,
		walked: map[int64]bool{}, batches: map[int64]*factoryBatch{}, readies: map[int64]*factoryReady{}, cursors: map[int64]int{},
		input: in, form: factoryDraft{area: area}, editor: defaultEditor, cache: &frameCache[factoryFrameKey]{},
	}
	m.mouseOn = !opts.NoMouse
	m.applyStyles(defaultStyles)
	return m
}

// defaultEditor is $VISUAL, else $EDITOR, else vi, on path.
func defaultEditor(ctx context.Context, path string) *exec.Cmd {
	args := strings.Fields(cmp.Or(strings.TrimSpace(os.Getenv("VISUAL")), strings.TrimSpace(os.Getenv("EDITOR")), "vi"))
	return exec.CommandContext(ctx, args[0], append(args[1:], path)...) //nolint:gosec // the operator's own editor on the operator's own draft
}

func (m *factoryModel) applyStyles(st styles) {
	m.st = st
	m.bar.applyStyles(st)
}

func (m factoryModel) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.loadCmd(), m.tickCmd())
}

func (m factoryModel) now() time.Time { return m.opts.Now() }

func (m factoryModel) loadCmd() tea.Cmd {
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		ctx, cancel := loadContext(ctx)
		defer cancel()
		d, err := src.Frame(ctx)
		return factoryDataMsg{data: d, err: err}
	}
}

// reload reads the factory again, once the load in flight ends if one is.
func (m *factoryModel) reload() tea.Cmd {
	if m.loading {
		m.again = true
		return nil
	}
	m.loading = true
	return m.loadCmd()
}

func (m factoryModel) tickCmd() tea.Cmd {
	return m.opts.Tick(factoryRefresh, func(time.Time) tea.Msg { return factoryTickMsg{} })
}

func (m factoryModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.keepRecapAsking()
	if a := m.startAnim(); a != nil {
		cmd = tea.Batch(cmd, a)
	}
	return m, cmd
}

// update changes the model for msg in place and returns what to run.
func (m *factoryModel) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		m.applyStyles(newStyles(msg.IsDark()))
	case tea.FocusMsg:
		return m.focusCmd(true)
	case tea.BlurMsg:
		return m.focusCmd(false)
	case factoryDataMsg:
		return m.loaded(msg)
	case factoryTickMsg:
		return tea.Batch(m.reload(), m.tickCmd())
	case factoryAnimMsg:
		m.animating = false // Update starts the next frame while something moves
		if m.moving() {
			m.anim++
		}
	case factoryDoneMsg:
		f := m.inflight
		m.inflight = nil
		cmd, _ := m.bar.update(msg)
		return tea.Batch(cmd, m.followed(f, msg.err))
	case factoryExpireMsg:
		cmd, _ := m.bar.update(msg)
		return cmd
	case factoryPreviewDueMsg:
		if msg.seq == m.form.seq {
			return m.previewCmd()
		}
	case factoryPreviewMsg:
		return m.previewed(msg)
	case factoryPagerMsg:
		return m.pagerDone(msg)
	case factoryEditorMsg:
		return m.edited(msg)
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	case tea.MouseMsg:
		return m.updateMouse(msg)
	case tea.PasteMsg:
		return m.paste(msg)
	}
	return nil
}

// focusCmd tells the factory the screen's terminal gained or lost focus,
// with the event's time, taken here on the screen's turn: two reports run
// on their own goroutines in any order, and the factory keeps the newer.
func (m factoryModel) focusCmd(focused bool) tea.Cmd {
	if m.act == nil {
		return nil
	}
	ctx, act, at := m.ctx, m.act, m.now()
	return func() tea.Msg {
		_ = act.Focus(ctx, focused, at) // best effort: a toast held or not is the factory's call
		return nil
	}
}

// loaded takes a read of the factory.
func (m *factoryModel) loaded(msg factoryDataMsg) tea.Cmd {
	m.loading = false
	var cmd tea.Cmd
	if m.again {
		m.again = false
		cmd = m.reload()
	}
	if msg.err != nil {
		m.loadErr = msg.err
		return cmd
	}
	m.data, m.haveData, m.loadErr = msg.data, true, nil
	cmd = tea.Batch(cmd, m.rebind(), m.rebindReady()) // a question they drop leaves its note, which expires
	m.fixCursor()
	if m.selectKey != "" {
		if i := slices.IndexFunc(m.data.Rows, func(r FactoryRow) bool { return r.Key == m.selectKey || r.Title == m.selectKey }); i >= 0 {
			m.selID, m.selectKey = m.data.Rows[i].ID, ""
			m.fixCursor()
		}
	}
	return tea.Batch(cmd, m.cardGone())
}

// rebind keeps the answers not sent yet of a task whose questions changed
// to a new revision: those to a question it asks again, with the same
// option, stay; the rest are dropped. The open card's question and mark
// start over when its revision changed; the command expires the note of a
// question it drops.
func (m *factoryModel) rebind() tea.Cmd {
	var cmds []tea.Cmd
	for id, b := range m.batches {
		c := m.data.Cards[id].Question
		if c == nil || c.Revision == b.rev {
			continue
		}
		kept := &factoryBatch{rev: c.Revision, answers: map[string]factoryAnswer{}, needless: map[string]string{}}
		for _, q := range c.Questions {
			if a, ok := b.answers[q.Text]; ok && (a.option < 0 || a.option < len(q.Options) && optionSends(q.Options[a.option]) == a.text) {
				kept.answers[q.Text] = a
			}
			if r, ok := b.needless[q.Text]; ok {
				kept.needless[q.Text] = r
			}
		}
		m.batches[id] = kept
		if m.task != id || (m.view != fvQuestion && m.view != fvRecap) {
			continue
		}
		if ch := m.q.changed; ch != nil { // the recap of the refused batch names the question open again
			if i := m.nextOpen(c, -1); i >= 0 {
				ch.again = i + 1
			}
			continue
		}
		// The old revision's batch is no longer asked: the next key answers
		// the question that is open again.
		cancel, _ := m.bar.cancel()
		cmds = append(cmds, cancel)
		m.asked = nil
		m.startQuestions(id)
	}
	return tea.Batch(cmds...)
}

// rebindReady keeps the state of each ready card the factory still draws as
// it was: a card gone, or drawn again for a new head or new hashes, takes its
// state with it, and the card on screen starts over unread (no mark, no
// diff read, shown now), its question dropped (the command expires the
// note it leaves).
func (m *factoryModel) rebindReady() tea.Cmd {
	var cmds []tea.Cmd
	for id, rs := range m.readies {
		cards := m.data.Cards[id]
		if cards.Ready != nil && sameCard(rs.bind, cards.Bind) {
			continue
		}
		delete(m.readies, id)
		if cards.Ready != nil && m.view == fvReady && m.task == id {
			cancel, _ := m.bar.cancel()
			cmds = append(cmds, cancel)
			m.asked = nil
			m.readies[id] = newReady(cards.Bind, m.now())
		}
	}
	return tea.Batch(cmds...)
}

// cardGone leaves a decision card the factory no longer shows (the task
// moved on), unless the action that moved it is still in flight; a question
// about it is dropped.
func (m *factoryModel) cardGone() tea.Cmd {
	if m.inflight != nil && m.inflight.task == m.task {
		return nil
	}
	cards, ok := m.data.Cards[m.task]
	gone := false
	switch m.view {
	case fvQuestion, fvRecap:
		gone = (!ok || cards.Question == nil) && m.q.changed == nil
	case fvReady:
		gone = !ok || cards.Ready == nil
	}
	if !gone {
		return nil
	}
	m.bar.cancel()
	m.asked = nil
	key := m.taskKey(m.task)
	m.goHome()
	return m.bar.show(key+" no longer waits on that card", factoryFlashNote)
}

// keepRecapAsking keeps the recap's question up: the recap is one y for the
// batch, and a key that is not y leaves it open and asking (spec 9.C).
func (m *factoryModel) keepRecapAsking() {
	if m.view != fvRecap || m.q.changed != nil || m.bar.asking != nil || m.bar.busy != "" || m.act == nil {
		return
	}
	c, ok := m.questionCard()
	if !ok {
		return
	}
	sentence := recapQuestion(splitAnswers(c.Questions))
	if sentence == "" {
		return
	}
	task, rev := m.task, c.Revision
	answers, needless := m.batchOf(c)
	f := &factoryFollow{kind: followAnswer, task: task, rev: rev, sent: map[string]string{}, sentNeedless: map[string]string{}}
	for i, text := range answers {
		f.sent[c.Questions[i].Text] = text
	}
	for i, why := range needless {
		f.sentNeedless[c.Questions[i].Text] = why
	}
	act := m.act
	_ = m.ask(f, sentence, func(ctx context.Context) (ActionResult, error) {
		return act.Answer(ctx, task, rev, answers, needless)
	}) // a question asks for no timer
}

// moving reports whether something on screen moves: a working row on home,
// a working stage or agent on the task card, the new task while Jira is read.
func (m factoryModel) moving() bool {
	if len(m.g.working) == 0 && len(m.g.spinner.Frames) == 0 {
		return false
	}
	switch m.view {
	case fvHome:
		ids := m.order()
		return slices.ContainsFunc(m.data.Rows, func(r FactoryRow) bool { return r.Mark == MarkWorking && slices.Contains(ids, r.ID) })
	case fvTask:
		c := m.data.Cards[m.task].Task
		return slices.ContainsFunc(c.Stages, func(s CardStage) bool { return s.State == "working" }) ||
			slices.ContainsFunc(c.Agents, func(a CardAgent) bool { return a.State == "working" })
	case fvNew:
		return m.reading()
	}
	return false
}

// startAnim starts the working marks' frames when something moves and none
// is pending; factoryAnimMsg ends the chain once nothing does.
func (m *factoryModel) startAnim() tea.Cmd {
	if m.animating || !m.moving() {
		return nil
	}
	m.animating = true
	return m.opts.Tick(factoryAnimEvery, func(time.Time) tea.Msg { return factoryAnimMsg{} })
}

// home's rows

// order is home's tasks in the order they are drawn: grouped by who they
// wait on, the done ones a visible digest lists left out (its line stands
// for them).
func (m factoryModel) order() []int64 {
	rows := slices.Clone(m.data.Rows)
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].Group < rows[b].Group })
	dig := &factoryPainter{}
	if d := m.digest(); d != nil {
		dig.f.Digest = &Digest{Since: d.Since, Done: cleanAll(d.Done)}
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		if r.Group == GroupDone && dig.digestLists(cleanText(r.Key)) {
			continue
		}
		out = append(out, r.ID)
	}
	return out
}

// row is the task id's row.
func (m factoryModel) row(id int64) (FactoryRow, bool) {
	i := slices.IndexFunc(m.data.Rows, func(r FactoryRow) bool { return r.ID == id })
	if i < 0 {
		return FactoryRow{}, false
	}
	return m.data.Rows[i], true
}

// selectedRow is the row under home's cursor.
func (m factoryModel) selectedRow() (FactoryRow, bool) {
	if !slices.Contains(m.order(), m.selID) {
		return FactoryRow{}, false
	}
	return m.row(m.selID)
}

// fixCursor keeps the cursor on its task, or at its place when the task left.
func (m *factoryModel) fixCursor() {
	ids := m.order()
	if len(ids) == 0 {
		m.selID, m.selIdx = 0, 0
		return
	}
	if i := slices.Index(ids, m.selID); i >= 0 {
		m.selIdx = i
		return
	}
	m.selIdx = min(max(m.selIdx, 0), len(ids)-1)
	m.selID = ids[m.selIdx]
}

// moveTo puts the cursor on the row at place i of the order.
func (m *factoryModel) moveTo(i int) {
	ids := m.order()
	if len(ids) == 0 {
		return
	}
	m.selIdx = min(max(i, 0), len(ids)-1)
	m.selID = ids[m.selIdx]
}

func (m factoryModel) taskKey(id int64) string {
	if r, ok := m.row(id); ok && r.Key != "" {
		return cleanText(r.Key)
	}
	if c, ok := m.data.Cards[id]; ok && c.Task.Key != "" {
		return cleanText(c.Task.Key)
	}
	return fmt.Sprintf("task %d", id)
}

// digest is the digest to draw: the factory's, unless the operator
// dismissed it with a key.
func (m factoryModel) digest() *Digest {
	d := m.data.Digest
	if d == nil || (m.digestGone && d.Since.Equal(m.digestSeen)) {
		return nil
	}
	return d
}

// needsYou counts the tasks that need the operator.
func (m factoryModel) needsYou() int {
	n := 0
	for _, r := range m.data.Rows {
		if r.Group == GroupNeedsYou {
			n++
		}
	}
	return n
}

// windowTitle says how many tasks need the operator (spec 9.F); it never
// looks like the marker reveal puts on herdr's client.
func (m factoryModel) windowTitle() string {
	switch n := m.needsYou(); n {
	case 0:
		return "factory · nothing waits on you"
	case 1:
		return "factory · 1 needs you"
	default:
		return fmt.Sprintf("factory · %d need you", n)
	}
}

// view

// factoryFrameKey is everything a frame shows (AGENTS.md: frames are cached
// by fingerprint): the screen, its size and palette, the bar's state, the
// action log's generation, the mouse and its menu, and a fingerprint of the
// painter's data (the spinner's frame among it); the clock is in it by the second.
type factoryFrameKey struct {
	view          factoryView
	width, height int
	dark          bool
	question      string
	note          string
	noteKind      factoryFlashKind
	busy          string
	logGen        int64
	mouseOn       bool
	menu          ctxMenu
	clock         int64
	data          uint64
}

func (m factoryModel) frameKey() factoryFrameKey {
	return factoryFrameKey{view: m.view, width: m.width, height: m.height, dark: m.st.dark,
		question: m.bar.question(), note: m.bar.flash, noteKind: m.bar.flashKind, busy: m.bar.busy,
		logGen: m.bar.log.generation(), mouseOn: m.mouseOn, menu: m.menu, clock: clockKey(m.opts.Now),
		data: fingerprint(m.payload())}
}

// View draws the screen, or the last frame when nothing it shows changed.
func (m factoryModel) View() tea.View {
	v := newView(m.cache.get(m.frameKey(), m.render))
	v.WindowTitle = m.windowTitle()
	v.ReportFocus = true
	return withMouse(v, m.mouseOn)
}

func (m factoryModel) viewWidth() int  { return cmp.Or(m.width, factoryDefaultW) }
func (m factoryModel) viewHeight() int { return cmp.Or(m.height, 40) }

// payload is what the painter of the current view draws.
func (m factoryModel) payload() any {
	switch m.view {
	case fvQuestion:
		c, _ := m.questionCard()
		return c
	case fvRecap:
		c, _ := m.questionCard()
		return c
	case fvReady:
		c, _ := m.readyCard()
		return c
	case fvTask:
		return m.taskCard()
	case fvNew:
		return m.newTaskForm()
	case fvHelp:
		return [2]any{m.scroll, m.helpContent(m.viewWidth())}
	case fvLog:
		return [2]any{m.scroll, m.logContent(m.viewWidth())}
	}
	return m.homeFrame()
}

func (m factoryModel) render() string {
	w, h := m.viewWidth(), m.viewHeight()
	now := m.now()
	var frame string
	switch p := m.payload().(type) {
	case factoryFrame:
		frame, _ = renderFactory(p, w, h, m.st, m.g, now)
	case QuestionCard:
		if m.view == fvRecap {
			frame = renderQuestionRecap(p, w, h, m.st, m.g, now)
		} else {
			frame = renderQuestionCard(p, w, h, m.st, m.g, now)
		}
	case ReadyCard:
		frame = renderReadyCard(p, w, h, m.st, m.g, now)
	case TaskCard:
		frame, _ = renderTaskCard(p, w, h, 0, m.st, m.g, now)
	case NewTaskForm:
		frame = renderNewTask(p, w, h, m.st, m.g, now)
	default:
		frame = m.boxScreen(w, h)
	}
	if m.menu.open {
		lines := strings.Split(frame, "\n")
		for len(lines) < h {
			lines = append(lines, "")
		}
		items := m.menuItems()
		l := placeMenu(items, m.menu.sel, m.menu.x, m.menu.y, w, h)
		overlay(lines, m.st.menuBox(items, m.menu.sel, l, m.g.mode == IconsASCII), l.x, l.y, w)
		frame = strings.Join(lines[:min(len(lines), h)], "\n")
	}
	return frame
}

// rule is the bar's note for a card's rule; "" when it has none.
func (m factoryModel) rule() string { return m.bar.note(max(m.viewWidth()-4, 1)) }

// homeFrame is home as the painter draws it.
func (m factoryModel) homeFrame() factoryFrame {
	d := m.data
	f := factoryFrame{Repo: d.Repo, Rows: d.Rows, Blocking: d.Blocking, BlockingLong: d.BlockingLong, RuleChip: d.RuleChip,
		Flash: d.Flash, Decisions: d.Decisions, Next: d.Next, Digest: m.digest(), Spinner: m.anim, Cursor: -1}
	if _, ok := m.selectedRow(); ok {
		f.Cursor = slices.IndexFunc(d.Rows, func(r FactoryRow) bool { return r.ID == m.selID })
	}
	// Answers not sent yet are the screen's: the row's line under the rule
	// counts them, and the rule says nothing is sent (spec 9.C, 9.G).
	f.Rows = slices.Clone(d.Rows)
	for i, r := range f.Rows {
		answered, total, last, ok := m.unsent(r.ID)
		if !ok {
			continue
		}
		count := fmt.Sprintf("%d answered, %d to go", answered, total-answered)
		for _, help := range []*string{&f.Rows[i].Help, &f.Rows[i].HelpShort, &f.Rows[i].HelpLong} {
			if *help != "" {
				*help += m.g.sep + count
			}
		}
		if f.Rows[i].Help == "" {
			f.Rows[i].Help = count
		}
		if r.ID == m.selID {
			f.Flash = fmt.Sprintf("%s you answered %d of %d%snothing sent yet", last.In(m.now().Location()).Format("15:04"),
				answered, total, m.g.sep)
		}
	}
	f.Hints = m.homeHints()
	f.rule = m.bar.view(max(m.viewWidth()-4, 1))
	switch {
	case !m.haveData && m.loadErr != nil:
		f.status = m.st.Err.Render("could not read the factory: " + errLine(m.loadErr))
	case !m.haveData:
		f.status = m.st.Dim.Render("reading the factory's tasks…")
	case m.loadErr != nil && f.rule == "":
		f.rule = m.st.Err.Render(m.g.fail + " refresh failed: " + errLine(m.loadErr))
	}
	return f
}

// homeHints are home's key hints for the row under the cursor.
func (m factoryModel) homeHints() []hint {
	var hs []hint
	r, ok := m.selectedRow()
	if ok {
		hs = append(hs, hint{"enter", m.openVerb(r)})
	}
	if _, ok := m.nextDecision(); ok {
		hs = append(hs, hint{"a", "oldest ask"})
	}
	if pane := m.paneHint(r); ok && m.act != nil && pane != "" {
		hs = append(hs, hint{"o", pane})
	}
	if m.act != nil {
		hs = append(hs, hint{"n", "new task"})
	}
	return append(hs, hint{"?", "more"})
}

// paneHint names the pane o steps into, in the approved frames' words: the
// pane that asked a task's questions, the pane of the agent a row waits on
// ("pane fixing bart"), else, for a task that needs you, the step in
// itself; "" for a row with no pane to step into (a review by a person, a
// queued or a done task): o is not offered there.
func (m factoryModel) paneHint(r FactoryRow) string {
	if r.Group == GroupNeedsYou && m.data.Cards[r.ID].Question != nil {
		return "pane that asked"
	}
	for _, w := range r.WaitsOn {
		if who := strings.ToLower(cleanText(w.Who)); who == "agent" || who == "agents" {
			what, _, _ := strings.Cut(cleanText(w.What), ",")
			what, _, _ = strings.Cut(what, strings.TrimSpace(m.g.sep))
			if what = strings.TrimSpace(what); what != "" {
				return "pane " + what
			}
		}
	}
	if r.Group == GroupNeedsYou {
		return "step in"
	}
	return ""
}

// unsent counts a task's answers: answered of total questions, when the
// last answer not sent yet was given; ok is false when it has none unsent.
func (m factoryModel) unsent(id int64) (answered, total int, last time.Time, ok bool) {
	c, b := m.data.Cards[id].Question, m.batches[id]
	if c == nil || b == nil || b.rev != c.Revision || len(b.answers) == 0 {
		return 0, 0, time.Time{}, false
	}
	for _, q := range c.Questions {
		total++
		if q.Answer != "" {
			answered++
			continue
		}
		if a, has := b.answers[q.Text]; has {
			answered, ok = answered+1, true
			if a.at.After(last) {
				last = a.at
			}
		}
	}
	return answered, total, last, ok
}

// openVerb says what enter opens for a row.
func (m factoryModel) openVerb(r FactoryRow) string {
	switch c := m.data.Cards[r.ID]; {
	case r.Group == GroupNeedsYou && c.Question != nil:
		return "answer"
	case r.Group == GroupNeedsYou && c.Ready != nil:
		return "decide"
	}
	return "task card"
}

// fingerprint is a hash of everything v holds, its pointers followed and
// its unexported fields read: two values that draw the same frame hash the
// same, and a change of anything they draw changes it.
func fingerprint(v any) uint64 {
	h := fnv.New64a()
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Invalid:
			h.Write([]byte{0})
		case reflect.Pointer, reflect.Interface:
			if v.IsNil() {
				h.Write([]byte("nil"))
				return
			}
			walk(v.Elem())
		case reflect.Struct:
			if v.Type() == reflect.TypeFor[time.Time]() { // its instant; its location is the clock's
				fmt.Fprint(h, v.Field(0).Uint(), v.Field(1).Int())
				return
			}
			for _, f := range v.Fields() {
				walk(f)
			}
		case reflect.Slice, reflect.Array:
			fmt.Fprint(h, "[", v.Len())
			for i := range v.Len() {
				walk(v.Index(i))
			}
		case reflect.Map:
			keys := v.MapKeys()
			sort.Slice(keys, func(a, b int) bool { return fmt.Sprint(keys[a]) < fmt.Sprint(keys[b]) })
			for _, k := range keys {
				walk(k)
				walk(v.MapIndex(k))
			}
		case reflect.String:
			fmt.Fprintf(h, "%q", v.String())
		case reflect.Bool:
			fmt.Fprint(h, v.Bool())
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			fmt.Fprint(h, v.Int(), ";")
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			fmt.Fprint(h, v.Uint(), ";")
		case reflect.Float32, reflect.Float64:
			fmt.Fprint(h, v.Float(), ";")
		default: // funcs and channels draw nothing
		}
	}
	walk(reflect.ValueOf(v))
	return h.Sum64()
}

// boxScreen is the help or the action log: a title, the box, the rule (the
// bar's question when one is asked, else its note) and the keys.
func (m factoryModel) boxScreen(w, h int) string {
	p := &prbPainter{st: m.st, pal: newPRBPalette(m.st), g: m.g}
	title, content, closer := "help", m.helpContent(w), hint{"?", "close"}
	if m.view == fvLog {
		title, content, closer = "action log", m.logContent(w), hint{logKey, "close"}
	}
	bodyH := max(h-3, 1)
	body, below := p.box(content, w, bodyH, m.scroll)
	for len(body) < bodyH {
		body = append(body, "")
	}
	lines := append([]string{" " + m.st.Title.Render("magnum factory") + m.st.Dim.Render(m.g.sep+title)}, body[:bodyH]...)
	lines = append(lines, p.footerRule(w, m.bar.view(max(w-4, 1)), below), " "+m.st.fitHints(w-1, []hint{{"j k", "scroll"}, closer, {"q", "quit"}}, []hint{closer}))
	if len(lines) > h {
		lines = lines[len(lines)-h:]
	}
	for i, l := range lines {
		lines[i] = truncate(l, w)
	}
	return strings.Join(lines, "\n")
}

// dropHints are hs without the keys milestone 1a's screen does not have.
func dropHints(hs []hint, keys ...string) []hint {
	return slices.DeleteFunc(slices.Clone(hs), func(h hint) bool { return slices.Contains(keys, ansi.Strip(h.key)) })
}
