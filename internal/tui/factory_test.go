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
	factoryStartCall struct{ text, kind string }
)

// fakeFactoryActions records every request the screen makes.
type fakeFactoryActions struct {
	mu       sync.Mutex
	starts   []factoryStartCall
	previews []string
	answers  []factoryAnswerCall
	decides  []factoryDecideCall
	stops    []int64
	retries  []int64
	opens    []int64
	seen     []time.Time
	focus    []bool
	diffs    []int64
	diffCmd  *exec.Cmd

	preview   func(text string) (NewTaskForm, error)
	answerErr error
	decideErr error
	started   func(text, kind string) // the source learns of the new task
}

func (a *fakeFactoryActions) Start(_ context.Context, text, kind string) (ActionResult, error) {
	a.mu.Lock()
	a.starts = append(a.starts, factoryStartCall{text, kind})
	started := a.started
	a.mu.Unlock()
	if started != nil {
		started(text, kind)
	}
	return ActionResult{Text: "PR-12350: started"}, nil
}

func (a *fakeFactoryActions) Preview(_ context.Context, text string) (NewTaskForm, error) {
	a.mu.Lock()
	a.previews = append(a.previews, text)
	fn := a.preview
	a.mu.Unlock()
	if fn == nil {
		return NewTaskForm{Text: text}, nil
	}
	return fn(text)
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

func (a *fakeFactoryActions) Seen(_ context.Context, at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, at)
	return nil
}

func (a *fakeFactoryActions) Focus(_ context.Context, focused bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.focus = append(a.focus, focused)
	return nil
}

func (a *fakeFactoryActions) Diff(task int64) *exec.Cmd {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.diffs = append(a.diffs, task)
	a.diffCmd = exec.Command("true")
	return a.diffCmd
}

// calls counts the requests that change something (starts, answers,
// decisions, stops, retries).
func (a *fakeFactoryActions) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.starts) + len(a.answers) + len(a.decides) + len(a.stops) + len(a.retries)
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
func previewJira(text string) (NewTaskForm, error) {
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
	if h.act.calls() != 0 {
		t.Fatal("enter started the task before y")
	}
	h.press("n")
	mustContain(t, h.view(), "PR-12350 the reminder is for advocates only▏") // back to the text, nothing lost
	if h.act.calls() != 0 {
		t.Fatal("n started the task")
	}
	h.press("enter", "y")
	if len(h.act.starts) != 1 || h.act.starts[0] != (factoryStartCall{"PR-12350 the reminder is for advocates only", "feature"}) {
		t.Fatalf("starts = %+v", h.act.starts)
	}
	if r, ok := h.m.selectedRow(); !ok || r.Key != "PR-12350" {
		t.Errorf("home's cursor is on %+v, want the new task", r)
	}
	mustContain(t, h.view(), "WAITS ON OTHERS")
}

func TestAPRLinkInNewTaskOffersAdoptionLater(t *testing.T) {
	h := testFactory(t, 100, 40)
	const link = "https://github.com/example/talkable/pull/4512"
	h.act.preview = func(text string) (NewTaskForm, error) {
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
	h.press("ctrl+r")
	mustContain(t, h.view(), "DONE SINCE", "away 14h")
	h.clock.add(time.Minute)
	h.press("j")
	if len(h.act.seen) != 1 || !h.act.seen[0].Equal(factoryNow.Add(time.Minute)) {
		t.Fatalf("seen = %v, want once at the key's time", h.act.seen)
	}
	mustNotContain(t, h.view(), "DONE SINCE", "away 14h")
	h.press("k", "ctrl+r", "j")
	if len(h.act.seen) != 1 {
		t.Errorf("seen %d times, want once", len(h.act.seen))
	}
	mustNotContain(t, h.view(), "DONE SINCE") // the same digest read again stays hidden
}

func TestTheBoardsLettersKeepTheirMeaning(t *testing.T) {
	h := testFactory(t, 100, 40)
	still := func(where string) {
		t.Helper()
		before := h.view()
		for _, k := range []string{"r", "p", "x"} {
			h.press(k)
			if v := h.view(); v != before {
				t.Errorf("%s: %s changed the screen:\n%s", where, k, v)
			}
		}
		if h.act.calls() != 0 || h.m.bar.asking != nil {
			t.Errorf("%s: a board letter asked or ran something", where)
		}
	}
	still("home")
	h.open("PR-12345")
	still("the question card")
	h.press("esc")
	h.open("PR-12360")
	still("the ready card")
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
	h.press("esc", "esc")
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

func TestFactoryScreenNeverOverflows(t *testing.T) {
	heights := []int{8, 12, 20, 30, 50}
	for _, w := range factoryWidths(40) {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			t.Parallel() // each width has screens of its own
			for _, ht := range heights {
				h := testFactory(t, w, ht)
				h.act.preview = previewJira
				factoryStates(h, func(name string) {
					lines := strings.Split(h.m.View().Content, "\n")
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
	h.src.change(func(d *FactoryData) { // a new card for another task: nothing marked until the diff
		c := d.Cards[3]
		r := *c.Ready
		c.Ready = &r
		d.Cards[3] = c
	})
	h.open("PR-12360")
	h.press("d")
	h.feed(factoryPagerMsg{task: 3})
	mustContain(t, h.view(), "› 2  send back") // the operator's own mark stays
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
	h.act.preview = func(text string) (NewTaskForm, error) {
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

// Ruling 16: the screen asks for focus reports and passes them on.
func TestTheScreenReportsFocusAndBlur(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.feed(tea.BlurMsg{}, tea.FocusMsg{})
	if !slices.Equal(h.act.focus, []bool{false, true}) {
		t.Errorf("focus = %v", h.act.focus)
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
