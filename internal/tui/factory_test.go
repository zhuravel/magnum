package tui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The factory screen's tests drive the model as Bubble Tea does: a message
// in, the commands it returns run and their messages fed back in, until
// nothing is left. Timers are instantTick's, which execCmd drops unrun (as
// the board's tests see theirs); the clock is fixed and moves only when a
// test moves it. The source and the actions are fakes.

// fixtureFactoryData is "Home at 140 columns" with its cards: PR-12345 waits
// on its questions (the first answered before, as on the mockup's card),
// PR-12360 on its ready card, #4512 on a reply (no card in milestone 1a, so
// its task card), PR-12330 has its task card, the rest a bare one.
func fixtureFactoryData() FactoryData {
	f := fixtureHome()
	cards := map[int64]FactoryCards{}
	for _, r := range f.Rows {
		cards[r.ID] = FactoryCards{Task: TaskCard{Key: r.Key, Title: r.Title}}
	}
	q := fixtureQuestionCard()
	q.Current, q.Cursor, q.Revision = 0, -1, 3 // the screen picks the question and the mark
	cards[2] = FactoryCards{Task: TaskCard{Key: "PR-12345", Title: q.Title}, Question: &q}
	ready := fixtureReadyCard()
	ready.Copy, ready.Frees = "talkable.factory3", "talkable.factory3; PR-12395 starts in it"
	ready.Recommended, ready.Cursor = 0, 0 // the data's zero value: the screen must not take it as a mark
	cards[3] = FactoryCards{Task: TaskCard{Key: "PR-12360", Title: ready.Title}, Ready: &ready,
		Bind: ReadyBinding{HeadSHA: "9c4e2d1f00aa", ReportHash: "report-1", DiffHash: "diff-1"}}
	cards[5] = FactoryCards{Task: fixtureTaskCard()}
	return FactoryData{Repo: f.Repo, Rows: f.Rows, Blocking: f.Blocking, BlockingLong: f.BlockingLong, RuleChip: f.RuleChip,
		Decisions: f.Decisions, Cards: cards}
}

// fakeFactorySource serves the data a test sets.
type fakeFactorySource struct {
	mu     sync.Mutex
	data   FactoryData
	err    error
	frames int
}

func (s *fakeFactorySource) Frame(context.Context) (FactoryData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames++
	d := s.data
	d.Cards = maps.Clone(s.data.Cards)
	d.Rows = slices.Clone(s.data.Rows)
	return d, s.err
}

func (s *fakeFactorySource) change(fn func(d *FactoryData)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.data)
}

type (
	factoryAnswerCall struct {
		task              int64
		rev               int
		answers, needless map[int]string
	}
	factoryDecideCall struct {
		task                 int64
		choice, reason, note string
		bound                ReadyBinding
	}
	factoryStartCall struct {
		text, kind, repo string
		wordsOnly        bool
	}
	// factoryFixOff is a FixOff call: the PR, and off or on.
	factoryFixOff struct {
		ref string
		off bool
	}
)

// fakeFactoryActions records every request the screen makes.
type fakeFactoryActions struct {
	mu       sync.Mutex
	starts   []factoryStartCall
	previews []string
	repos    []string // the repository of each preview
	answers  []factoryAnswerCall
	decides  []factoryDecideCall
	stops    []int64
	retries  []int64
	opens    []int64
	browsed  []string // the URLs OpenBrowser opened
	seen     []time.Time
	focus    []bool
	focusAt  []time.Time
	diffs    []int64
	diffCmd  *exec.Cmd
	fixes    []string        // the PRs Fix was asked for
	fixOffs  []factoryFixOff // FixOff's calls
	aborts   []string        // the PRs Abort was asked for

	preview   func(text, repo string) (NewTaskForm, error)
	answerErr error
	decideErr error
	started   func(text, kind string) // the source learns of the new task
}

func (a *fakeFactoryActions) Start(_ context.Context, text, kind, repo string, wordsOnly bool) (ActionResult, error) {
	a.mu.Lock()
	a.starts = append(a.starts, factoryStartCall{text, kind, repo, wordsOnly})
	started := a.started
	a.mu.Unlock()
	if started != nil {
		started(text, kind)
	}
	return ActionResult{Text: "PR-12350: started"}, nil
}

func (a *fakeFactoryActions) Preview(_ context.Context, text, repo string) (NewTaskForm, error) {
	a.mu.Lock()
	a.previews, a.repos = append(a.previews, text), append(a.repos, repo)
	fn := a.preview
	a.mu.Unlock()
	if fn == nil {
		return NewTaskForm{Text: text}, nil
	}
	return fn(text, repo)
}

func (a *fakeFactoryActions) Answer(_ context.Context, task int64, rev int, answers, needless map[int]string) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answers = append(a.answers, factoryAnswerCall{task, rev, maps.Clone(answers), maps.Clone(needless)})
	return ActionResult{Text: "PR-12345: 2 answers; it goes on"}, a.answerErr
}

func (a *fakeFactoryActions) Decide(_ context.Context, task int64, choice, reason, note string, bound ReadyBinding) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.decides = append(a.decides, factoryDecideCall{task, choice, reason, note, bound})
	return ActionResult{Text: "PR-12360: " + choice}, a.decideErr
}

func (a *fakeFactoryActions) Stop(_ context.Context, task int64) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stops = append(a.stops, task)
	return ActionResult{Text: "dropped"}, nil
}

func (a *fakeFactoryActions) Retry(_ context.Context, task int64) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.retries = append(a.retries, task)
	return ActionResult{Text: "it goes on"}, nil
}

func (a *fakeFactoryActions) Open(_ context.Context, task int64) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, task)
	return ActionResult{Text: "focused its pane"}, nil
}

func (a *fakeFactoryActions) OpenBrowser(_ context.Context, url string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.browsed = append(a.browsed, url)
	return nil
}

// opened is the URLs OpenBrowser opened, in order.
func (a *fakeFactoryActions) opened() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.browsed)
}

func (a *fakeFactoryActions) Seen(_ context.Context, at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, at)
	return nil
}

func (a *fakeFactoryActions) Focus(_ context.Context, focused bool, at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.focus = append(a.focus, focused)
	a.focusAt = append(a.focusAt, at)
	return nil
}

func (a *fakeFactoryActions) Diff(task int64) *exec.Cmd {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.diffs = append(a.diffs, task)
	a.diffCmd = exec.Command("true")
	return a.diffCmd
}

func (a *fakeFactoryActions) Fix(_ context.Context, ref string) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fixes = append(a.fixes, ref)
	return ActionResult{Text: "fix queued for " + ref}, nil
}

func (a *fakeFactoryActions) FixOff(_ context.Context, ref string, off bool) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fixOffs = append(a.fixOffs, factoryFixOff{ref, off})
	return ActionResult{Text: "fix switched for " + ref}, nil
}

func (a *fakeFactoryActions) Abort(_ context.Context, ref string) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.aborts = append(a.aborts, ref)
	return ActionResult{Text: "stopped " + ref}, nil
}

// calls counts the requests that change something (starts, answers,
// decisions, stops, retries, and magnum's fix, fix off and abort).
func (a *fakeFactoryActions) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.starts) + len(a.answers) + len(a.decides) + len(a.stops) + len(a.retries) + len(a.fixes) + len(a.fixOffs) + len(a.aborts)
}

// testClock is the screen's clock in tests: fixed until a test moves it.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// factoryHarness is a screen under test.
type factoryHarness struct {
	t     *testing.T
	m     factoryModel
	src   *fakeFactorySource
	act   *fakeFactoryActions
	clock *testClock
	quit  bool
	execs []tea.Msg // the ExecProcess messages the screen sent (the pager, the editor)
}

// testFactory builds the screen at w x h with the fixture source, fake
// actions, instant timers and a fixed clock, and lets it load.
func testFactory(t *testing.T, w, h int) *factoryHarness {
	t.Helper()
	clk := &testClock{at: factoryNow}
	src := &fakeFactorySource{data: fixtureFactoryData()}
	act := &fakeFactoryActions{}
	hs := &factoryHarness{t: t, src: src, act: act, clock: clk,
		m: newFactoryModel(t.Context(), src, act, FactoryOptions{Now: clk.now, Tick: instantTick})}
	hs.feed(tea.WindowSizeMsg{Width: w, Height: h})
	hs.run(hs.m.Init())
	return hs
}

// feed hands msgs to the screen and runs what it asks for, to the end.
func (h *factoryHarness) feed(msgs ...tea.Msg) {
	h.t.Helper()
	queue := slices.Clone(msgs)
	for n := 0; len(queue) > 0; n++ {
		if n > 500 {
			h.t.Fatal("the screen never settles")
		}
		msg := queue[0]
		queue = queue[1:]
		next, cmd := h.m.Update(msg)
		h.m = next.(factoryModel)
		queue = append(queue, h.results(cmd)...)
	}
}

// run runs cmd and feeds its messages.
func (h *factoryHarness) run(cmd tea.Cmd) {
	h.t.Helper()
	h.feed(h.results(cmd)...)
}

// results are the messages cmd produces that go back to the screen: a quit
// and the ExecProcess messages are kept aside.
func (h *factoryHarness) results(cmd tea.Cmd) []tea.Msg {
	h.t.Helper()
	var out []tea.Msg
	for _, msg := range execCmd(h.t, cmd) {
		switch {
		case isQuit([]tea.Msg{msg}):
			h.quit = true
		case fmt.Sprintf("%T", msg) == "tea.execMsg":
			h.execs = append(h.execs, msg)
		default:
			out = append(out, msg)
		}
	}
	return out
}

// press sends key presses, one after the other.
func (h *factoryHarness) press(names ...string) {
	h.t.Helper()
	for _, n := range names {
		h.feed(keyMsg(n))
	}
}

// view is the screen's frame without colors.
func (h *factoryHarness) view() string { return viewOf(h.m) }

// selectRow moves the home cursor to the row keyed key.
func (h *factoryHarness) selectRow(key string) {
	h.t.Helper()
	for range 20 {
		if r, ok := h.m.selectedRow(); ok && r.Key == key {
			return
		}
		h.press("j")
	}
	h.t.Fatalf("no row %s on home:\n%s", key, h.view())
}

// open opens the card of the row keyed key from home.
func (h *factoryHarness) open(key string) {
	h.t.Helper()
	h.press("g")
	h.selectRow(key)
	h.press("enter")
}

func TestEnterOpensTheDecisionCardOfANeedsYouRow(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12345")
	v := h.view()
	mustContain(t, v, "PR-12345 · question 2 of 3", "When does the reminder go out?", "› 1  Seven days before it expires")
	h.press("esc")
	h.open("PR-12360")
	mustContain(t, h.view(), "PR-12360 · ready to push", "1  push + PR", "unread:")
}

func TestEnterOpensTheTaskCardOfOtherRows(t *testing.T) {
	h := testFactory(t, 140, 50)
	h.open("PR-12330")
	mustContain(t, h.view(), "PR-12330 · bug · Advocate offer email locale", "STAGES", "WAITING FOR")
	h.press("esc")
	// A row that needs you without a card of milestone 1a (a reply) opens
	// its task card.
	h.open("#4512")
	mustContain(t, h.view(), "#4512", "Deploy list on campaigns")
	mustNotContain(t, h.view(), "ready to push", "question 1 of")
}

// On a task card that was open when its task came to need you (its
// questions or its ready card), enter opens that decision card, as enter on
// home does, and the footer says so; on a task card with no decision
// waiting, enter stays.
func TestEnterOnATaskCardOpensTheDecisionItsTaskWaitsOn(t *testing.T) {
	h := testFactory(t, 140, 50)
	taskCardOf := func(key string) {
		t.Helper()
		h.open(key)
		r, _ := h.m.selectedRow()
		h.m.view, h.m.task = fvTask, r.ID // the task card stayed open while the task came to wait on you
	}
	taskCardOf("PR-12345")
	mustContain(t, h.view(), "STAGES", "enter answer")
	h.press("enter")
	mustContain(t, h.view(), "PR-12345 · question 2 of 3")
	h.press("esc")

	taskCardOf("PR-12360")
	mustContain(t, h.view(), "enter ready card")
	h.press("enter")
	mustContain(t, h.view(), "PR-12360 · ready to push")
	h.press("esc")

	h.open("PR-12330")
	mustNotContain(t, h.view(), "enter answer", "enter ready card")
	h.press("enter")
	mustContain(t, h.view(), "PR-12330 · bug · Advocate offer email locale", "STAGES")
}

func TestAWalksTheDecisionsOldestFirst(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.press("a")
	mustContain(t, h.view(), "PR-12345 · question 2 of 3") // asked 41m ago: the oldest card
	h.press("1", "1", "y")
	if len(h.act.answers) != 1 {
		t.Fatalf("the batch was sent %d times", len(h.act.answers))
	}
	mustContain(t, h.view(), "PR-12360 · ready to push") // the next card, waiting 12m
	h.press("3", "y")
	if len(h.act.decides) != 1 || h.act.decides[0].choice != FactoryDrop {
		t.Fatalf("decides = %+v", h.act.decides)
	}
	v := h.view()
	mustContain(t, v, "NEEDS YOU", "Nothing else waits on you")
	mustNotContain(t, v, "ready to push ·")
}

func TestAnAnswerKeyNeedsNoConfirmationButTheBatchDoes(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12345")
	h.press("2") // question 2: one day before
	mustContain(t, h.view(), "question 3 of 3", "Can a merchant turn it off?")
	mustNotContain(t, h.view(), "y/N")
	h.press("enter") // no recommendation, nothing marked: refused
	mustContain(t, h.view(), "question 3 of 3")
	h.press("1")
	v := h.view()
	mustContain(t, v, "your answers", "Send 2 answers? y/N")
	if h.act.calls() != 0 {
		t.Fatal("an answer key sent something before the batch's y")
	}
	h.press("n")
	if h.act.calls() != 0 {
		t.Fatal("n sent the batch")
	}
	mustContain(t, h.view(), "your answers", "Send 2 answers? y/N") // the recap stays, still asking
	h.press("y")
	if len(h.act.answers) != 1 {
		t.Fatalf("the batch was sent %d times", len(h.act.answers))
	}
	got := h.act.answers[0]
	want := map[int]string{1: "One day before it expires", 2: "Yes: a switch in the email settings, on by default"}
	if got.task != 2 || got.rev != 3 || !maps.Equal(got.answers, want) || len(got.needless) != 0 {
		t.Errorf("the batch = %+v, want task 2, revision 3, answers %v by index from 0", got, want)
	}
	mustContain(t, h.view(), "NEEDS YOU") // home, the decision made
}

func TestMinusFlagsAQuestionNeedlessWithAReason(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12345")
	h.press("-")
	mustContain(t, h.view(), "Why needless?", "1 the ticket answers it")
	h.press("2") // the code answers it
	v := h.view()
	mustContain(t, v, "question 2 of 3", "flagged needless")
	mustNotContain(t, v, "Why needless?")
	h.press("−", "esc") // the minus sign works too; esc drops the reason's question
	mustNotContain(t, h.view(), "Why needless?")
	h.press("1", "1", "y")
	if len(h.act.answers) != 1 {
		t.Fatalf("the batch was sent %d times", len(h.act.answers))
	}
	needless := h.act.answers[0].needless
	key, ok := needless[1]
	if !ok || len(needless) != 1 || !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`).MatchString(key) {
		t.Errorf("needless = %v, want question index 1 with a reason key", needless)
	}
}

func TestPushWithoutOpeningTheDiffAsksAgain(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12360")
	h.press("1")
	mustContain(t, h.view(), "Push without reading the diff?", "y/N")
	if h.act.calls() != 0 {
		t.Fatal("push ran before its y")
	}
	h.press("y")
	if len(h.act.decides) != 1 {
		t.Fatalf("decides = %+v", h.act.decides)
	}
	d := h.act.decides[0]
	if d.choice != FactoryPush || !d.bound.DiffOpened.IsZero() || d.bound.DiffDwell != 0 {
		t.Errorf("the push = %+v, want push with no diff opened", d)
	}
}

func TestDigitsPickTheReadyOptions(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12360")
	mustNotContain(t, h.view(), "›") // nothing is marked before the diff (the data's zero Cursor is no mark)
	h.press("3")
	mustContain(t, h.view(), "› 3  drop", "Drop PR-12360: free talkable.factory3, keep the branch? y/N")
	h.press("n")
	h.press("2")
	mustContain(t, h.view(), "› 2  send back", "Why send back?")
	h.press("esc")
	h.press("1")
	mustContain(t, h.view(), "› 1  push + PR", "Push without reading the diff? It frees talkable.factory3; PR-12395 starts in it. y/N")
	h.press("n")
	if h.act.calls() != 0 {
		t.Errorf("a digit decided without y: %+v", h.act.decides)
	}
}

func TestSendBackAsksForAReasonKeyThenANote(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12360")
	h.press("2")
	mustContain(t, h.view(), "Why send back?", "1 wrong cause or fix")
	h.press("4") // tests too thin
	mustContain(t, h.view(), "Your note to the writer")
	h.feed(typed("the spec never copies an archived campaign")...)
	mustContain(t, h.view(), "the spec never copies an archived campaign▏")
	h.press("enter")
	mustContain(t, h.view(), "Send PR-12360 back to the writer (tests too thin)? y/N")
	if h.act.calls() != 0 {
		t.Fatal("the send back ran before its y")
	}
	h.press("y")
	if len(h.act.decides) != 1 {
		t.Fatalf("decides = %+v", h.act.decides)
	}
	d := h.act.decides[0]
	if d.choice != FactorySendBack || d.note != "the spec never copies an archived campaign" ||
		!regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`).MatchString(d.reason) {
		t.Errorf("the send back = %+v", d)
	}
}

func TestThePushQuestionNamesWhatItFrees(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12360")
	h.press("d")
	h.clock.add(90 * time.Second) // reading the diff
	h.feed(factoryPagerMsg{task: 3})
	mustContain(t, h.view(), "› 1  push + PR", "recommended", "you read the diff 14:09")
	h.press("1")
	v := cardSquash(h.view())
	mustContain(t, v, "Push zhuravel-PR-12360-activity-points-on-copy and open a PR? It frees talkable.factory3; PR-12395 starts in it. y/N")
	h.press("y")
	d := h.act.decides[0]
	if !d.bound.DiffOpened.Equal(factoryNow) || d.bound.DiffDwell != 90*time.Second || d.bound.HeadSHA != "9c4e2d1f00aa" ||
		d.bound.ReportHash != "report-1" || d.bound.DiffHash != "diff-1" || !d.bound.Shown.Equal(factoryNow) {
		t.Errorf("the push's binding = %+v, want the card's head and hashes with the screen's times", d.bound)
	}
}

func TestDOpensTheDiffInThePager(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12360")
	h.press("d")
	if !slices.Equal(h.act.diffs, []int64{3}) || len(h.execs) != 1 {
		t.Fatalf("diffs %v, ExecProcess messages %d", h.act.diffs, len(h.execs))
	}
	// The message runs the very command the actions' Diff returned.
	run := reflect.ValueOf(h.execs[0]).FieldByName("cmd").Elem().Elem().Field(0)
	if run.Pointer() != reflect.ValueOf(h.act.diffCmd).Pointer() {
		t.Error("the pager runs another command than the one Diff returned")
	}
	h.feed(factoryPagerMsg{task: 3})
	mustContain(t, h.view(), "you read the diff")
	// The task card's d opens the same pager.
	h.press("esc")
	h.open("PR-12330")
	h.press("d")
	if !slices.Equal(h.act.diffs, []int64{3, 5}) {
		t.Errorf("diffs = %v", h.act.diffs)
	}
}

// previewJira is the daemon's preview of a text that starts with PR-12350.
func previewJira(text, _ string) (NewTaskForm, error) {
	f := fixtureNewTaskJira(factoryNow)
	f.Text, f.Ask, f.Overlap = text, "", ""
	f.Jira.Key, f.Branch = "PR-12350", "zhuravel-PR-12350-credit-expiry-reminder"
	return f, nil
}

func TestNewTaskAsksBeforeItStartsAgents(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.act.preview = previewJira
	h.act.started = func(string, string) {
		h.src.change(func(d *FactoryData) {
			d.Rows = append(d.Rows, FactoryRow{ID: 20, Key: "PR-12350", Title: "Credit expiry reminder email", Group: GroupWaits,
				Mark: MarkWorking, WaitsOn: []WaitsOn{{Who: "agent", What: "reading the ticket"}}})
		})
	}
	h.press("n")
	mustContain(t, h.view(), "new task", "Jira key or describe the task")
	h.feed(typed("PR-12350 the reminder is for advocates only")...)
	h.press("enter")
	mustContain(t, h.view(), "Start feature PR-12350 with 2 agents? y/N")
	mustNotContain(t, h.view(), "pgdn") // the ticket's longer text is not a key of this screen
	if h.act.calls() != 0 {
		t.Fatal("enter started the task before y")
	}
	h.press("n")
	mustContain(t, h.view(), "PR-12350 the reminder is for advocates only▏") // back to the text, nothing lost
	if h.act.calls() != 0 {
		t.Fatal("n started the task")
	}
	h.press("enter", "y")
	if len(h.act.starts) != 1 || h.act.starts[0] != (factoryStartCall{"PR-12350 the reminder is for advocates only", "feature", "talkable", false}) {
		t.Fatalf("starts = %+v", h.act.starts)
	}
	if r, ok := h.m.selectedRow(); !ok || r.Key != "PR-12350" {
		t.Errorf("home's cursor is on %+v, want the new task", r)
	}
	mustContain(t, h.view(), "WAITS ON OTHERS")
}

// The kind you pick with tab draws that kind's agents, the ones the start
// launches for it: the AGENTS row and the y/N follow the pick, never the
// kind the daemon guessed.
func TestThePickedKindDrawsItsOwnAgents(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.act.preview = func(text, repo string) (NewTaskForm, error) {
		f, err := previewJira(text, repo)
		f.AgentsByKind = map[string][]string{
			"bug":     {"reproduce it", "find the cause on its own", "read the history of the code"},
			"feature": f.Agents,
			"chore":   {"read the changelog of what it upgrades"},
		}
		return f, err
	}
	h.press("n")
	h.feed(typed("PR-12350 the reminder is for advocates only")...)
	h.press("enter", "n") // enter reads the preview and asks; n is back on the form, the preview drawn
	mustContain(t, h.view(), "AGENTS    2:")
	h.press("tab") // feature, then chore
	mustContain(t, h.view(), "chore", "your pick", "AGENTS    1: read the changelog of what it upgrades")
	mustNotContain(t, h.view(), "AGENTS    2:")
	h.press("enter")
	mustContain(t, h.view(), "Start chore PR-12350 with 1 agent? y/N")
	h.press("n", "tab") // chore, then bug
	mustContain(t, h.view(), "AGENTS    3: reproduce it")
}

func TestAPRLinkInNewTaskOffersAdoptionLater(t *testing.T) {
	h := testFactory(t, 100, 40)
	const link = "https://github.com/example/talkable/pull/4512"
	h.act.preview = func(text, _ string) (NewTaskForm, error) {
		return NewTaskForm{Text: text, Repo: "talkable", Err: "a PR link: adoption comes in milestone 2",
			Links: []FormLink{{Kind: "pr", URL: link, Offer: "adoption comes in milestone 2"}}}, nil
	}
	h.press("n")
	h.feed(typed(link)...)
	h.press("enter")
	v := h.view()
	mustContain(t, v, "LINKS", "adoption comes in milestone 2")
	mustNotContain(t, v, "y/N")
	h.press("y")
	if h.act.calls() != 0 {
		t.Errorf("a PR link started a task: %+v", h.act.starts)
	}
}

func TestTheDigestClearsAfterTheNextKey(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.src.change(func(d *FactoryData) {
		dg := fixtureDigest()
		d.Rows, d.Digest = dg.Rows, dg.Digest
	})
	h.press("ctrl+r") // your input: seen at its time
	mustContain(t, h.view(), "DONE SINCE", "away 14h")
	h.clock.add(30 * time.Second)
	h.press("j") // the digest's key: seen at once, whatever the minute's throttle says
	want := []time.Time{factoryNow, factoryNow.Add(30 * time.Second)}
	if !slices.EqualFunc(h.act.seen, want, time.Time.Equal) {
		t.Fatalf("seen = %v, want %v: the digest's key at its time", h.act.seen, want)
	}
	mustNotContain(t, h.view(), "DONE SINCE", "away 14h")
	h.press("k", "ctrl+r", "j")
	if len(h.act.seen) != 2 {
		t.Errorf("seen %d times, want no more within the minute", len(h.act.seen))
	}
	mustNotContain(t, h.view(), "DONE SINCE") // the same digest read again stays hidden
}

// A click hits what the frame drew under it, the digest included, and only
// then dismisses the digest: the first click after a night away never
// selects the row the frame without the digest would have there.
func TestTheFirstClickAfterTheDigestHitsWhatTheFrameDrew(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.src.change(func(d *FactoryData) {
		dg := fixtureDigest()
		d.Rows, d.Digest = dg.Rows, dg.Digest
	})
	h.feed(factoryTickMsg{})
	mustContain(t, h.view(), "DONE SINCE")
	_, lines := renderFactory(h.m.homeFrame(), 100, 40, h.m.st, h.m.g, factoryNow)
	gone := h.m // the frame once the digest is dismissed
	gone.digestSeen, gone.digestGone = h.m.data.Digest.Since, true
	_, after := renderFactory(gone.homeFrame(), 100, 40, h.m.st, h.m.g, factoryNow)
	var hidden FactoryRow // a row the digest's lines cover, drawn once it goes
	y := -1
	for _, r := range h.m.data.Rows {
		if ay, ok := after[r.ID]; ok && !slices.Contains(slices.Collect(maps.Values(lines)), ay) {
			hidden, y = r, ay
			break
		}
	}
	if y < 0 {
		t.Fatalf("no line the digest covers holds a row once it goes: %v, then %v", lines, after)
	}
	before, _ := h.m.selectedRow()
	h.feed(tea.MouseClickMsg{X: 10, Y: y, Button: tea.MouseLeft}) // on the digest, as drawn
	if r, ok := h.m.selectedRow(); ok && r.ID == hidden.ID && hidden.ID != before.ID {
		t.Fatalf("the click on the digest selected %s, the row the frame without it has there", hidden.Key)
	}
	mustNotContain(t, h.view(), "DONE SINCE")
}

func TestTheBoardsLettersKeepTheirMeaning(t *testing.T) {
	h := testFactory(t, 100, 40)
	still := func(where string, keys ...string) {
		t.Helper()
		before := h.view()
		for _, k := range keys {
			h.press(k)
			if v := h.view(); v != before {
				t.Errorf("%s: %s changed the screen:\n%s", where, k, v)
			}
		}
		if h.act.calls() != 0 || h.m.bar.asking != nil {
			t.Errorf("%s: a board letter asked or ran something", where)
		}
	}
	still("home", "r", "p")
	// x on a task's row on home is your PR's key (stop its fix loop): it
	// starts nothing and says so.
	h.press("x")
	mustContain(t, h.view(), "f, F and x act on your PRs")
	if h.act.calls() != 0 || h.m.bar.asking != nil {
		t.Error("home: x on a task's row asked or ran something")
	}
	h.open("PR-12345")
	still("the question card", "r", "p", "x")
	h.press("esc")
	h.open("PR-12360")
	still("the ready card", "r", "p", "x")
	h.press("esc")
	h.open("PR-12330")
	h.press("r", "p") // no retry on this task card
	if h.act.calls() != 0 || h.m.bar.asking != nil {
		t.Error("the task card's r or p did something without a retry to offer")
	}
	h.press("x")
	mustContain(t, h.view(), "Drop PR-12330", "y/N")
	h.press("y")
	if !slices.Equal(h.act.stops, []int64{5}) {
		t.Errorf("stops = %v", h.act.stops)
	}
	mustContain(t, h.view(), "NEEDS YOU") // the dropped task's card is left for home
}

func TestTheWindowTitleCountsWhatNeedsYou(t *testing.T) {
	h := testFactory(t, 100, 40)
	v := h.m.View()
	if v.WindowTitle != "factory · 3 need you" || !v.ReportFocus {
		t.Errorf("title %q, focus reported %v", v.WindowTitle, v.ReportFocus)
	}
	h.src.change(func(d *FactoryData) { d.Rows = fixtureNothing().Rows })
	h.press("ctrl+r")
	if got := h.m.View().WindowTitle; got != "factory · nothing waits on you" {
		t.Errorf("title %q", got)
	}
	h.src.change(func(d *FactoryData) { d.Rows = fixtureHome().Rows[1:2] })
	h.press("ctrl+r")
	if got := h.m.View().WindowTitle; got != "factory · 1 needs you" {
		t.Errorf("title %q", got)
	}
	if strings.Contains(h.m.View().WindowTitle, "herdr-magnum-") {
		t.Error("the title looks like reveal's marker")
	}
}

func TestARightClickMenuRunsTheSameKeyPath(t *testing.T) {
	h := testFactory(t, 100, 40)
	// On home, a right click on a row selects it and opens its menu; its
	// "open" item opens the row's card, as enter does.
	_, lines := renderFactory(h.m.homeFrame(), 100, 40, h.m.st, h.m.g, factoryNow)
	h.feed(tea.MouseClickMsg{X: 10, Y: lines[3], Button: tea.MouseRight})
	if !h.m.menu.open {
		t.Fatalf("no menu:\n%s", h.view())
	}
	mustContain(t, h.view(), "open", "enter")
	h.press("enter")
	mustContain(t, h.view(), "PR-12360 · ready to push")
	// On the card, the menu's push asks the same y/N as 1 does.
	h.feed(tea.MouseClickMsg{X: 20, Y: 10, Button: tea.MouseRight})
	i := slices.IndexFunc(h.m.menuItems(), func(it menuItem) bool { return it.label == "push + PR" })
	if i < 0 {
		t.Fatalf("the ready card's menu has no push: %+v", h.m.menuItems())
	}
	for range i {
		h.press("j")
	}
	h.press("enter")
	mustContain(t, h.view(), "Push without reading the diff?", "y/N")
	if h.act.calls() != 0 {
		t.Fatal("the menu pushed without y")
	}
	// A click while the question waits answers it with no.
	h.feed(tea.MouseClickMsg{X: 5, Y: 5, Button: tea.MouseLeft})
	mustNotContain(t, h.view(), "y/N")
	if h.act.calls() != 0 || h.m.menu.open {
		t.Error("a click with the question up did more than cancel it")
	}
}

// factoryStates opens every view of the screen on h, calling at with each.
func factoryStates(h *factoryHarness, at func(name string)) {
	at("home")
	h.press("enter") // #4512: its task card
	at("task-bare")
	h.press("esc")
	h.open("PR-12330")
	at("task")
	h.press("x")
	at("task-asking")
	h.press("n", "esc")
	h.open("PR-12345")
	at("question")
	h.press("-")
	at("question-needless")
	h.press("esc", "5")
	at("question-own")
	h.press("esc", "1", "3")
	at("recap")
	h.press("y") // the factory refuses the batch: the questions changed
	at("recap-changed")
	h.press("esc")
	h.open("PR-12360")
	at("ready")
	h.press("2")
	at("ready-reasons")
	h.press("1")
	at("ready-note")
	h.press("esc", "1")
	at("ready-asking")
	h.press("n", "esc", "n")
	h.feed(typed("PR-12350 the reminder")...)
	at("new")
	h.press("esc", "?")
	at("help")
	h.press("?", "!")
	at("log")
	h.press("!")
	h.feed(tea.MouseClickMsg{X: 3, Y: 4, Button: tea.MouseRight})
	at("menu")
	h.press("esc")
}

// TestFactoryScreenNeverOverflows draws every view at every size: no line
// is wider than the screen, no frame taller, and a question that waits is
// always drawn (a y answers it). The ready card carries a failed check, and
// the recap's batch is refused, so their variants are drawn too.
func TestFactoryScreenNeverOverflows(t *testing.T) {
	heights := []int{8, 9, 10, 12, 20, 30, 50}
	for _, w := range factoryWidths(40) {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			t.Parallel() // each width has screens of its own
			for _, ht := range heights {
				h := testFactory(t, w, ht)
				h.act.preview = previewJira
				h.act.answerErr = &QuestionsChangedError{At: cardAt(14, 20), Why: "Question 2 is open again."}
				h.src.change(func(d *FactoryData) {
					c := d.Cards[3]
					r := *c.Ready
					r.Failed = []string{"rubocop: 2 offenses in app/services/campaign_copier.rb"}
					c.Ready = &r
					d.Cards[3] = c
				})
				h.press("ctrl+r")
				factoryStates(h, func(name string) {
					content := h.m.View().Content
					if h.m.bar.asking != nil && !strings.Contains(ansi.Strip(content), "y/N") {
						t.Errorf("%s at %dx%d: a question waits but is not drawn:\n%s", name, w, ht, ansi.Strip(content))
					}
					lines := strings.Split(content, "\n")
					if len(lines) > ht {
						t.Errorf("%s at %dx%d: %d lines", name, w, ht, len(lines))
					}
					for i, l := range lines {
						if lw := ansi.StringWidth(l); lw > w {
							t.Errorf("%s at %dx%d: line %d is %d cells: %q", name, w, ht, i+1, lw, ansi.Strip(l))
						}
					}
				})
			}
		})
	}
}

// Ruling 1: a question up when the screen quits is dropped, and a y that
// comes after starts nothing.
func TestQuittingDropsAPendingQuestion(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12360")
	h.press("1")
	mustContain(t, h.view(), "y/N")
	h.press("ctrl+c")
	if !h.quit {
		t.Fatal("ctrl+c did not quit")
	}
	h.press("y")
	if h.act.calls() != 0 {
		t.Errorf("a y behind ctrl+c decided: %+v", h.act.decides)
	}
}

// Ruling 5: the frame is drawn again whenever the bar, the palette or the
// log changes, and only then.
func TestTheFramesFingerprintFollowsTheBarThePaletteAndTheLog(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12360")
	base := h.m.frameKey()
	if again := h.m.frameKey(); again != base {
		t.Fatal("two keys of one state differ")
	}
	changed := func(what string, k factoryFrameKey) {
		t.Helper()
		if k == base {
			t.Errorf("the key ignores %s", what)
		}
	}
	m := h.m
	m.bar.ask("Drop PR-12360?", func(context.Context) (ActionResult, error) { return ActionResult{}, nil })
	changed("the question", m.frameKey())
	m = h.m
	m.bar.show("a note", factoryFlashNote)
	changed("the note", m.frameKey())
	m.bar.flashKind = factoryFlashFailed
	changed("the note's kind", m.frameKey())
	m = h.m
	m.bar.busy = "Push PR-12360"
	changed("the action in flight", m.frameKey())
	m = h.m
	m.applyStyles(newStyles(false))
	changed("the palette", m.frameKey())
	m = h.m
	m.bar.log.add(logEntry{what: "Open PR-12360's pane"})
	changed("the log", m.frameKey())
}

// Rulings 5 and 16: ! opens the action log, with every alert and its time.
func TestBangShowsTheActionLogAndTheAlerts(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.src.change(func(d *FactoryData) {
		d.Alerts = []FactoryAlert{{At: factoryNow.Add(-5 * time.Minute), Text: "PR-12345: 3 questions before coding"}}
	})
	h.press("ctrl+r", "o") // o: an action, logged
	h.press("!")
	mustContain(t, h.view(), "Action log", "Open #4512's pane", "Alerts", "14:04", "PR-12345: 3 questions before coding")
	h.press("!")
	mustContain(t, h.view(), "NEEDS YOU")
}

// Rulings 9 and 11: a batch for questions that changed is refused by a
// typed error; the recap says so, and enter goes back to the questions.
func TestAStaleBatchShowsThatTheQuestionsChanged(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.act.answerErr = fmt.Errorf("request 7: %w", &QuestionsChangedError{At: cardAt(14, 20), Why: "Question 2 is open again."})
	h.open("PR-12345")
	h.press("1", "1", "y")
	v := h.view()
	mustContain(t, v, "the questions changed at 14:20 · nothing was sent", "Question 2 is open again.", "esc back")
	mustNotContain(t, v, "y/N")
	h.press("enter")
	mustContain(t, h.view(), "PR-12345 · question")
	if !errors.Is(&QuestionsChangedError{}, ErrQuestionsChanged) {
		t.Error("the typed error is not ErrQuestionsChanged")
	}
}

// Ruling 6: the question card starts its mark on the recommendation, the
// ready card on none until it moves or the diff is opened.
func TestTheCardsStartTheirMarksWhereTheRulingsSay(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12345")
	mustContain(t, h.view(), "› 1  Seven days")
	h.press("j")
	mustContain(t, h.view(), "› 2  One day")
	h.press("esc")
	h.open("PR-12360")
	mustNotContain(t, h.view(), "›")
	h.press("j")
	mustContain(t, h.view(), "› 1  push + PR")
	h.press("j")
	mustContain(t, h.view(), "› 2  send back")
	h.press("esc")
	h.open("PR-12360")
	mustContain(t, h.view(), "› 2  send back") // the operator's own mark stays while the card is the same
	h.press("esc")
	h.src.change(func(d *FactoryData) { // a new card for the task: nothing marked until its diff is read
		c := d.Cards[3]
		c.Bind = ReadyBinding{HeadSHA: "ffff000011112222", ReportHash: "report-2", DiffHash: "diff-2"}
		d.Cards[3] = c
	})
	h.press("ctrl+r")
	h.open("PR-12360")
	mustNotContain(t, h.view(), "›")
	h.press("d")
	h.feed(factoryPagerMsg{task: 3})
	mustContain(t, h.view(), "› 1  push + PR") // the recommendation, once the diff was read
}

// fix round 1

// A ready card the factory draws again (the branch moved: a new head and
// hashes) is a card the operator has not read: no mark, no diff read, a
// push asks "without reading the diff?", and the decision carries the new
// card's head and the time it was shown, with no read of its diff.
func TestARedrawnReadyCardIsUnreadAgain(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12360")
	h.press("d")
	h.clock.add(time.Minute)
	h.feed(factoryPagerMsg{task: 3})
	h.press("1")
	mustContain(t, h.view(), "Push zhuravel-PR-12360-activity-points-on-copy and open a PR?")
	h.press("n")
	h.clock.add(time.Minute)
	h.src.change(func(d *FactoryData) {
		c := d.Cards[3]
		c.Bind = ReadyBinding{HeadSHA: "ffff000011112222", ReportHash: "report-2", DiffHash: "diff-2"}
		d.Cards[3] = c
	})
	h.feed(factoryTickMsg{})
	mustContain(t, h.view(), "PR-12360 · ready to push")
	mustNotContain(t, h.view(), "›", "you read the diff")
	h.press("1")
	mustContain(t, h.view(), "Push without reading the diff?")
	h.press("y")
	if len(h.act.decides) != 1 {
		t.Fatalf("decides = %+v", h.act.decides)
	}
	b := h.act.decides[0].bound
	if b.HeadSHA != "ffff000011112222" || !b.DiffOpened.IsZero() || b.DiffDwell != 0 || !b.Shown.Equal(factoryNow.Add(2*time.Minute)) {
		t.Errorf("the push's binding = %+v, want the new head, no diff read, shown at the redraw", b)
	}
	// A card that goes away takes its state with it.
	h.src.change(func(d *FactoryData) {
		c := d.Cards[3]
		c.Ready = nil
		d.Cards[3] = c
	})
	h.feed(factoryTickMsg{})
	if _, ok := h.m.readies[3]; ok {
		t.Error("the state of a ready card the factory no longer shows is kept")
	}
}

// A question a redraw drops (a ready card drawn again, a new revision of the
// questions) leaves "cancelled, nothing was run" on the rule for its time
// only: the redraw hands back the note's expiry, as a cancel by key does,
// so the note never stays until something replaces it. (The harness drops
// timers unrun, so the test runs the expiry itself.)
func TestAQuestionARedrawDropsLeavesANoteThatExpires(t *testing.T) {
	expires := func(t *testing.T, h *factoryHarness, cmd tea.Cmd) {
		t.Helper()
		if !strings.Contains(h.view(), "cancelled, nothing was run") {
			t.Fatalf("no note of the dropped question:\n%s", h.view())
		}
		if cmd == nil {
			t.Fatal("the redraw dropped the note's expiry")
		}
		msgs := []tea.Msg{cmd()}
		if b, ok := msgs[0].(tea.BatchMsg); ok {
			msgs = nil
			for _, c := range b {
				if c != nil {
					msgs = append(msgs, c())
				}
			}
		}
		h.feed(msgs...)
		mustNotContain(t, h.view(), "cancelled, nothing was run")
	}

	h := testFactory(t, 100, 40)
	h.open("PR-12360")
	h.press("1")
	mustContain(t, h.view(), "Push without reading the diff?")
	d := h.m.data
	d.Cards = maps.Clone(d.Cards)
	c := d.Cards[3]
	c.Bind = ReadyBinding{HeadSHA: "ffff000011112222", ReportHash: "report-2", DiffHash: "diff-2"}
	d.Cards[3] = c
	h.m.data = d
	expires(t, h, h.m.rebindReady())

	q := testFactory(t, 100, 40)
	q.open("PR-12345")
	q.press("1", "1")
	mustContain(t, q.view(), "your answers", "y/N")
	d = q.m.data
	d.Cards = maps.Clone(d.Cards)
	qc := *d.Cards[2].Question
	qc.Revision = 4
	c = d.Cards[2]
	c.Question = &qc
	d.Cards[2] = c
	q.m.data = d
	expires(t, q, q.m.rebind())
}

// A question that waits is on screen at every size: the task card's head
// gives way before its footer.
func TestTheTaskCardKeepsItsQuestionOnAShortPane(t *testing.T) {
	for _, w := range []int{40, 60, 100} {
		for _, ht := range []int{8, 9, 10, 12} {
			h := testFactory(t, w, ht)
			h.open("PR-12330")
			h.press("x")
			if h.m.bar.asking == nil {
				t.Fatalf("%dx%d: x asked nothing", w, ht)
			}
			if v := h.view(); !strings.Contains(v, "y/N") {
				t.Errorf("%dx%d: the question waits but is not drawn:\n%s", w, ht, v)
			}
		}
	}
}

// enter on new task asks y/N for the text enter was pressed on, never for a
// text typed while its preview was read: a letter typed then is text.
func TestATypedYNeverAnswersALateNewTaskQuestion(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.act.preview = previewJira
	h.press("n")
	h.feed(typed("PR-12350 the reminder")...)
	next, cmd := h.m.Update(keyMsg("enter")) // the preview is read meanwhile
	h.m = next.(factoryModel)
	h.feed(typed(" yes")...)
	h.run(cmd)
	if h.m.bar.asking != nil {
		t.Fatalf("a question came up while the text was typed: %q", h.m.bar.question())
	}
	h.press("y")
	if len(h.act.starts) != 0 {
		t.Errorf("a typed y started a task: %+v", h.act.starts)
	}
	mustContain(t, h.view(), "PR-12350 the reminder yesy▏")
}

// A new task's question is asked only on the new-task view: enter waits for
// the text's preview, and a preview that lands after esc (on the question
// card while you type your own answer, or on home) asks nothing, so a typed
// "yes" stays text and no y starts agents. (The final review's probe.)
func TestALateNewTaskQuestionIsNeverAskedOffItsView(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.act.preview = previewJira
	h.press("n")
	h.feed(typed("PR-12350 the reminder")...)
	next, cmd := h.m.Update(keyMsg("enter")) // the preview is read meanwhile
	h.m = next.(factoryModel)
	h.press("esc")
	h.open("PR-12345")
	h.press("5") // type your own answer
	h.feed(typed("we ")...)
	h.run(cmd) // the late preview lands
	if q := h.m.bar.question(); q != "" {
		t.Fatalf("a late preview asked %q on the question card", q)
	}
	h.feed(typed("yes")...)
	if len(h.act.starts) != 0 {
		t.Fatalf("a typed y started a task: %+v", h.act.starts)
	}
	mustContain(t, h.view(), "we yes▏")

	home := testFactory(t, 100, 40)
	home.act.preview = previewJira
	home.press("n")
	home.feed(typed("PR-12350 the reminder")...)
	next, cmd = home.m.Update(keyMsg("enter"))
	home.m = next.(factoryModel)
	home.press("esc")
	home.run(cmd)
	if q := home.m.bar.question(); q != "" {
		t.Fatalf("a late preview asked %q on home", q)
	}
	home.press("y")
	if len(home.act.starts) != 0 {
		t.Fatalf("a y on home started a task: %+v", home.act.starts)
	}
	// Back on the form, enter asks at once: the preview is the text's.
	home.press("n", "enter")
	if q := home.m.bar.question(); !strings.Contains(q, "Start") {
		t.Fatalf("enter on the form with its preview read asks %q, want the start's question", q)
	}
}

// Every view that can hold the bar's question draws it: the question card
// and the help and action log boxes, as home and the other cards do, so no
// question is ever up where you cannot read it.
func TestEveryViewDrawsTheBarsQuestion(t *testing.T) {
	noop := func(context.Context) (ActionResult, error) { return ActionResult{}, nil }
	h := testFactory(t, 100, 40)
	h.open("PR-12345")
	h.m.ask(&factoryFollow{}, "Probe the question card?", noop)
	mustContain(t, h.view(), "Probe the question card?", "y/N")
	h.press("n")
	for _, key := range []string{"?", logKey} {
		b := testFactory(t, 100, 40)
		b.press(key)
		b.m.ask(&factoryFollow{}, "Probe the box?", noop)
		mustContain(t, b.view(), "Probe the box?", "y/N")
	}
}

// A batch that went through drops only the answers it sent: an answer
// changed while the send ran (the recap's digits open a question
// meanwhile) is still yours, never lost with the batch.
func TestASentBatchDropsOnlyTheAnswersItSent(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12345")
	h.press("1", "1")
	mustContain(t, h.view(), "Send 2 answers?")
	next, send := h.m.Update(keyMsg("y")) // the send runs; its answer comes later
	h.m = next.(factoryModel)
	h.press("2", "2") // question 2 again, its second option
	h.run(send)
	if len(h.act.answers) != 1 {
		t.Fatalf("answers sent %+v, want the one batch", h.act.answers)
	}
	b := h.m.batches[2]
	if b == nil || len(b.answers) != 1 || b.answers["When does the reminder go out?"].text != "One day before it expires" {
		t.Fatalf("the batch after the send is %+v, want the answer changed while it ran", b)
	}
}

// The recap's question goes when the questions change under it: the next
// digit answers the question that is open again.
func TestANewRevisionDropsTheRecapsQuestion(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12345")
	h.press("1", "1")
	mustContain(t, h.view(), "your answers", "y/N")
	h.src.change(func(d *FactoryData) {
		c := d.Cards[2]
		q := *c.Question
		q.Revision = 4
		q.Questions = slices.Clone(q.Questions)
		q.Questions[2].Text = "Can a merchant switch it off?"
		c.Question = &q
		d.Cards[2] = c
	})
	h.feed(factoryTickMsg{})
	if h.m.bar.asking != nil || h.m.asked != nil {
		t.Fatalf("the old revision's batch is still asked: %q", h.m.bar.question())
	}
	mustContain(t, h.view(), "question 3 of 3", "Can a merchant switch it off?")
	h.press("2", "y")
	if len(h.act.answers) != 1 {
		t.Fatalf("answers = %+v", h.act.answers)
	}
	if a := h.act.answers[0]; a.rev != 4 || a.answers[1] != "Seven days before it expires" || a.answers[2] != "No: always on" {
		t.Errorf("the batch = %+v, want revision 4 with question 2's answer kept and question 3's new one", a)
	}
}

// The recap of a refused batch names the question open again in the new
// revision, and enter goes to it.
func TestTheChangedRecapNamesTheQuestionOpenAgain(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.act.answerErr = &QuestionsChangedError{At: cardAt(14, 20)}
	h.open("PR-12345")
	h.press("1", "1")
	h.src.change(func(d *FactoryData) { // the factory's revision 4 asks question 2 anew
		c := d.Cards[2]
		q := *c.Question
		q.Revision = 4
		q.Questions = slices.Clone(q.Questions)
		q.Questions[1].Text = "When does the reminder go out, in the merchant's time?"
		c.Question = &q
		d.Cards[2] = c
	})
	h.press("y")
	mustContain(t, h.view(), "the questions changed at 14:20", "enter see question 2 again")
	h.press("enter")
	mustContain(t, h.view(), "question 2 of 3", "When does the reminder go out, in the merchant's time?")
}

// Typing keeps the cursor at the end of the text, where the caret is drawn:
// the keys that would move it are not taken.
func TestTheCursorStaysWhereTheCaretIsDrawn(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.press("n")
	h.feed(typed("abc")...)
	h.press("left", "home", "up")
	h.feed(typed("X")...)
	mustContain(t, h.view(), "abcX▏")
	h.press("esc", "esc")
	h.open("PR-12345")
	h.press("5")
	h.feed(typed("ab")...)
	h.press("left")
	h.feed(typed("X")...)
	mustContain(t, h.view(), "abX▏")
}

func TestLowerFirstLowersTheFirstRune(t *testing.T) {
	for in, want := range map[string]string{"Élan": "élan", "«Yes»": "«Yes»", "Ask rev-ann": "ask rev-ann", "": ""} {
		if got := lowerFirst(in); got != want {
			t.Errorf("lowerFirst(%q) = %q, want %q", in, got, want)
		}
	}
}

// The Jira spinner shows while the factory reads a key found in the text,
// not only one at its start.
func TestTheJiraSpinnerShowsForAKeyInTheText(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.press("n")
	h.feed(typed("fix the expiry date in PR-12345")...)
	next, _ := h.m.Update(keyMsg("enter")) // its preview is read
	h.m = next.(factoryModel)
	mustContain(t, h.view(), "the daemon reads Jira")
}

// The recap's only mouse menu would hold a key that cancels its question:
// it has none, and a right click there leaves the recap asking.
func TestTheRecapOpensNoMenu(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12345")
	h.press("1", "1")
	if items := h.m.menuItems(); len(items) != 0 {
		t.Errorf("the recap's menu = %+v", items)
	}
	h.feed(tea.MouseClickMsg{X: 10, Y: 5, Button: tea.MouseRight})
	if h.m.menu.open {
		t.Error("a menu opened on the recap")
	}
	mustContain(t, h.view(), "Send 2 answers? y/N")
}

// On a card that offers no push, the mark moves over send back and drop
// only.
func TestTheMarkSkipsAPushNotOffered(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.src.change(func(d *FactoryData) {
		c := d.Cards[3]
		r := *c.Ready
		r.NoPush = "the base conflicts: the writer merges it"
		c.Ready = &r
		d.Cards[3] = c
	})
	h.press("ctrl+r")
	h.open("PR-12360")
	h.press("j")
	mustContain(t, h.view(), "› 2  send back")
	h.press("k")
	mustContain(t, h.view(), "› 2  send back")
	h.press("j")
	mustContain(t, h.view(), "› 3  drop")
}

// A send back's note is kept when its y/N is answered no: picking the
// reason again brings it back.
func TestTheSendBacksNoteSurvivesANo(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12360")
	h.press("2", "1")
	h.feed(typed("the spec never copies an archived campaign")...)
	h.press("enter", "n", "2", "1")
	mustContain(t, h.view(), "the spec never copies an archived campaign▏")
	h.press("enter", "y")
	if len(h.act.decides) != 1 || h.act.decides[0].note != "the spec never copies an archived campaign" {
		t.Errorf("decides = %+v", h.act.decides)
	}
}

// Answers not sent yet are kept on esc, and home says so: the row's line
// under the rule counts them, and the status rule says nothing is sent.
func TestHomeShowsTheAnswersNotSentYet(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12345")
	h.press("2", "esc")
	v := h.view()
	mustContain(t, v, "2 answered, 1 to go", "✓ 14:09 you answered 2 of 3 · nothing sent yet")
	h.press("j") // another row: its own line, and the rule's own news
	mustNotContain(t, h.view(), "answered, 1 to go", "nothing sent yet")
}

// A key Jira does not know is said before anything is asked; enter then
// asks to start from the words only, and only that y sends the start so.
func TestAKeyJiraDoesNotKnowStartsFromYourWordsOnly(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.act.preview = func(text, _ string) (NewTaskForm, error) {
		return NewTaskForm{Text: text, Repo: "talkable", Err: "PR-12399 is not in Jira", WordsOnly: true}, nil
	}
	h.press("n")
	h.feed(typed("PR-12399 the reminder for advocates")...)
	h.press("enter")
	mustContain(t, h.view(), "PR-12399 is not in Jira", "Start from your words only? y/N")
	h.press("n")
	if h.act.calls() != 0 {
		t.Fatal("n started a task")
	}
	h.press("enter", "y")
	if len(h.act.starts) != 1 || h.act.starts[0] != (factoryStartCall{"PR-12399 the reminder for advocates", "", "talkable", true}) {
		t.Errorf("starts = %+v, want the words only", h.act.starts)
	}
}

// With two repositories the form names the one the task goes to. Until one
// is chosen enter refuses with the reason, and no y starts a task; shift+tab
// cycles through them, asks nothing and starts nothing (it changes only the
// form, whose preview is read again for the pick), and the start carries the
// repository chosen.
func TestNewTaskStartsNoTaskWithoutARepository(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.act.preview = func(text, repo string) (NewTaskForm, error) {
		if repo == "" {
			return NewTaskForm{Text: text, Repos: fixtureRepos}, nil
		}
		f := fixtureNewTaskRepoPick()
		f.Text, f.Repo = text, repo
		return f, nil
	}
	const text = "the order confirmation page shows the wrong currency"
	h.press("n")
	h.feed(typed(text)...)
	h.press("enter")
	mustContain(t, h.view(), "REPO      none: no link or key of your text names one", "shift+tab repo · esc cancels",
		"cannot start: which repository? shift+tab picks one")
	mustNotContain(t, h.view(), "y/N", "JIRA")
	h.press("y")
	if h.act.calls() != 0 {
		t.Fatalf("a y started a task with no repository: %+v", h.act.starts)
	}
	h.press("backspace") // the y was text
	shiftTab := tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	h.feed(shiftTab)
	if h.m.bar.asking != nil || h.act.calls() != 0 {
		t.Fatalf("shift+tab asked %q or ran %+v", h.m.bar.question(), h.act.starts)
	}
	mustContain(t, h.view(), "REPO      example/app   your pick", "BRANCH")
	if last := h.act.repos[len(h.act.repos)-1]; last != "example/app" {
		t.Errorf("the preview after shift+tab is for %q, want your pick", last)
	}
	h.feed(shiftTab)
	mustContain(t, h.view(), "REPO      example/site  your pick")
	h.press("enter")
	mustContain(t, cardSquash(h.view()), `Start bug "the order confirmation page shows the wrong`, `…" in example/site with 3 agents? y/N`)
	h.press("y")
	if len(h.act.starts) != 1 || h.act.starts[0] != (factoryStartCall{text, "bug", "example/site", false}) {
		t.Errorf("starts = %+v, want the task in example/site", h.act.starts)
	}
	h.press("n")
	h.feed(typed(text)...)
	h.press("enter") // the preview: no repository told
	h.feed(shiftTab, shiftTab, shiftTab)
	mustContain(t, h.view(), "REPO      example/app   your pick") // the cycle wraps, and a start forgets the pick
	mustContain(t, ansi.Strip(strings.Join(h.m.helpContent(100), "\n")), "shift+tab", "the repository, when the factory has several")

	// shift+tab before the first preview is kept: the preview is read at
	// once, and the press applies when it lands.
	h.press("esc")
	h.m.form = factoryDraft{area: h.m.form.area}
	h.m.form.area.SetValue("")
	h.press("n")
	h.feed(typed("the banner is grey")...)
	h.feed(shiftTab)
	mustContain(t, h.view(), "REPO      example/app   your pick")
	if h.m.bar.asking != nil || h.act.calls() != 1 {
		t.Errorf("shift+tab before the first preview asked %q or ran something", h.m.bar.question())
	}
}

// A key of a tracker that is not Jira, alone, is refused with the daemon's
// reason, which the form draws before anything is asked; enter then refuses
// with it, and no y starts a task.
func TestNewTaskSaysWhyAKeyOfAnotherTrackerAloneStartsNothing(t *testing.T) {
	h := testFactory(t, 100, 40)
	const why = "the factory reads no linear.app issue: add what to do after EX-15"
	h.act.preview = func(text, _ string) (NewTaskForm, error) {
		return NewTaskForm{Text: text, Repos: fixtureRepos, Repo: "example/site", RepoWhy: "from the EX tracker", Err: why}, nil
	}
	h.press("n")
	h.feed(typed("EX-15")...)
	h.press("enter")
	v := cardSquash(h.view())
	mustContain(t, v, "JIRA "+why, "cannot start: "+why)
	mustNotContain(t, v, "y/N")
	h.press("y")
	if h.act.calls() != 0 {
		t.Errorf("a y started a task of a key alone: %+v", h.act.starts)
	}
}

// While a preview of a text that starts with a key of a tracker that is not
// Jira is read again (more words typed), the form does not say the daemon
// reads Jira: the last preview said it never does.
func TestNewTaskDoesNotSpinForJiraOnAKeyOfAnotherTracker(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.press("n")
	h.m.form.area.SetValue("EX-12 the export fails on Safari")
	h.m.form.inFlight, h.m.form.previewFor = true, "EX-12 the export"
	if !h.m.reading() {
		t.Fatal("a key's preview in flight does not spin")
	}
	h.m.form.preview = NewTaskForm{Text: "EX-12 the export", NotJira: "EX-12"}
	if h.m.reading() {
		t.Error("the form spins for Jira on a key the last preview said is not Jira's")
	}
	h.m.form.area.SetValue("PR-12345 the export fails on Safari")
	if !h.m.reading() {
		t.Error("another key's preview does not spin")
	}
}

// The pager shows the whole diff: once it returns, no file is unread.
func TestTheDiffsEndLeavesNoFileUnread(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12360")
	mustContain(t, h.view(), "unread:")
	h.press("d")
	h.feed(factoryPagerMsg{task: 3})
	mustNotContain(t, h.view(), "unread:")
}

// Home's hints speak the approved frames' words for the rows they showed.
func TestHomesHintsSpeakTheApprovedFramesWords(t *testing.T) {
	h := testFactory(t, 100, 40)
	hints := func() string { return strings.TrimSpace(strings.Split(h.view(), "\n")[39]) }
	want := map[string]string{
		"#4512":    "enter task card · a oldest ask · o pane fixing bart · n new task · ? more",
		"PR-12345": "enter answer · a oldest ask · o pane that asked · n new task · ? more",
		"PR-12371": "enter task card · a oldest ask · o pane finding the cause · n new task · ? more",
		// Rows with no pane to step into offer no o.
		"PR-12330": "enter task card · a oldest ask · n new task · ? more",
		"PR-12302": "enter task card · a oldest ask · n new task · ? more",
	}
	for key, w := range want {
		h.press("g")
		h.selectRow(key)
		if got := hints(); got != w {
			t.Errorf("%s: hints %q, want %q", key, got, w)
		}
	}
}

// The recap shows the comment the factory drafts for an ask, word for word.
func TestTheRecapShowsTheDraftedComment(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12345")
	h.press("1", "3")
	v := cardSquash(h.view())
	mustContain(t, v, "rev-ann, a question on PR-12345: Can a merchant turn it off?",
		"The options as we see them:", "- No: always on: no switch anywhere")
	mustNotContain(t, v, "the factory drafts this comment", "Type your own answer", "Ask rev-ann in Jira (the task waits):")
}

// Ruling 10: the task card's r asks y/N and retries, only where a retry
// can go on.
func TestTheTaskCardRetriesOnlyWhereItCan(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.src.change(func(d *FactoryData) {
		c := d.Cards[5]
		c.Task = fixtureRetryCard()
		d.Cards[5] = c
	})
	h.press("ctrl+r")
	h.open("PR-12330")
	h.press("r")
	mustContain(t, h.view(), "Retry PR-12393", "y/N")
	if h.act.calls() != 0 {
		t.Fatal("r retried before y")
	}
	h.press("y")
	if !slices.Equal(h.act.retries, []int64{5}) {
		t.Errorf("retries = %v", h.act.retries)
	}
}

// Ruling 13: a card whose conflict sent the task back never offers push.
func TestAConflictCardNeverPushes(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.src.change(func(d *FactoryData) {
		c := d.Cards[3]
		r := *c.Ready
		r.NoPush = "the base conflicts: the writer merges it"
		c.Ready = &r
		d.Cards[3] = c
	})
	h.press("ctrl+r")
	h.open("PR-12360")
	h.press("1")
	mustNotContain(t, h.view(), "y/N")
	mustContain(t, h.view(), "not offered")
	h.press("y")
	if h.act.calls() != 0 {
		t.Errorf("a conflict card pushed: %+v", h.act.decides)
	}
}

// Ruling 14: the y/N of a text naming several keys names the prompt task it
// is, never a key.
func TestTheSeveralKeysQuestionNamesThePromptTask(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.act.preview = func(text, _ string) (NewTaskForm, error) {
		f := fixtureNewTaskSeveral()
		f.Text, f.Ask = text, ""
		f.Jira = &JiraPreview{Key: "PR-12345", Summary: "Credit expiry reminder email"} // a source that sets both
		return f, nil
	}
	h.press("n")
	h.feed(typed("the copier change of PR-12345 also breaks PR-12360: fix both in one place")...)
	h.press("enter")
	v := cardSquash(h.view())
	mustContain(t, v, `Start bug "Copier change of PR-12345 also breaks PR-12360" with 3 agents? y/N`)
	mustNotContain(t, v, "Start bug PR-12345")
}

// Ruling 16: the screen asks for focus reports and passes them on, each
// with the time of its event (taken on the screen's turn, never on the
// report's goroutine, so a late report never passes for a newer one).
func TestTheScreenReportsFocusAndBlur(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.feed(tea.BlurMsg{})
	h.clock.add(time.Second)
	h.feed(tea.FocusMsg{})
	if !slices.Equal(h.act.focus, []bool{false, true}) {
		t.Errorf("focus = %v", h.act.focus)
	}
	if !slices.EqualFunc(h.act.focusAt, []time.Time{factoryNow, factoryNow.Add(time.Second)}, time.Time.Equal) {
		t.Errorf("focus times = %v, want each event's own", h.act.focusAt)
	}
}

// Any key or click tells the factory you saw home, at most once a minute,
// with the input's time: the digest counts your absence from your last
// input, whatever the terminal reports of its focus.
func TestAnyKeyOrClickSaysYouSawHomeOnceAMinute(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.press("j")
	h.clock.add(20 * time.Second)
	h.press("k")
	if !slices.EqualFunc(h.act.seen, []time.Time{factoryNow}, time.Time.Equal) {
		t.Fatalf("seen = %v, want once, at the first key", h.act.seen)
	}
	h.clock.add(time.Minute)
	h.open("PR-12330") // a key on another view counts too
	h.clock.add(time.Minute)
	h.feed(tea.MouseClickMsg{X: 5, Y: 5, Button: tea.MouseLeft})
	want := []time.Time{factoryNow, factoryNow.Add(80 * time.Second), factoryNow.Add(140 * time.Second)}
	if !slices.EqualFunc(h.act.seen, want, time.Time.Equal) {
		t.Errorf("seen = %v, want %v", h.act.seen, want)
	}
}

// Ruling 8: the working marks turn with the screen's frame counter, on home
// and on the task card, and on the new task form while Jira is read.
func TestTheSpinnersTurnWithTheScreensFrames(t *testing.T) {
	h := testFactory(t, 140, 50)
	frames := map[string]bool{}
	for range 3 {
		frames[ansi.Strip(h.view())] = true
		h.feed(factoryAnimMsg{})
	}
	if len(frames) < 2 {
		t.Error("home's working mark did not turn")
	}
	h.open("PR-12330")
	before := h.view()
	h.feed(factoryAnimMsg{})
	if h.view() == before {
		t.Error("the task card's working stage did not turn")
	}
}

// The own-words option takes a typed line, which the batch sends as it is.
func TestTypeYourOwnAnswerTakesALine(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12345")
	h.press("5")
	mustContain(t, h.view(), "Your answer")
	h.feed(typed("two days before, at 09:00")...)
	h.press("enter")
	mustContain(t, h.view(), "question 3 of 3")
	h.press("3", "y") // ask rev-ann
	if len(h.act.answers) != 1 {
		t.Fatalf("answers = %+v", h.act.answers)
	}
	if got := h.act.answers[0].answers; got[1] != "two days before, at 09:00" || got[2] != "ask:rev-ann" {
		t.Errorf("the batch = %v", got)
	}
}

// The new task form's ctrl+g opens the editor on the draft, and the text it
// wrote comes back.
func TestCtrlGEditsTheDraftInTheEditor(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.press("n")
	h.feed(typed("PR-12350")...)
	h.press("ctrl+g")
	if len(h.execs) != 1 {
		t.Fatalf("ctrl+g sent %d ExecProcess messages", len(h.execs))
	}
	h.feed(factoryEditorMsg{text: "PR-12350 the reminder, edited\n"})
	mustContain(t, h.view(), "PR-12350 the reminder, edited▏")
}
