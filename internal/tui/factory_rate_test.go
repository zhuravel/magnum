package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// factoryRateCall is a Rate call the fake records.
type factoryRateCall struct {
	task       int64
	value      int
	reason     string
	diffOpened bool
}

func (a *fakeFactoryActions) Rate(_ context.Context, task int64, value int, reason string, diffOpened bool) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rates = append(a.rates, factoryRateCall{task, value, reason, diffOpened})
	if a.rateText != "" {
		return ActionResult{Text: a.rateText}, a.rateErr
	}
	return ActionResult{Text: fmt.Sprintf("PR-12330: rated %+d (%s)", value, reason)}, a.rateErr
}

// rated is the Rate calls the screen made, in order.
func (a *fakeFactoryActions) rated() []factoryRateCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.rates)
}

// testRatable is the screen with the task cards of PR-12330 (published, its
// PR in review) and PR-12302 (done) offering + and −, as the factory's
// screen data sets Rate for a published or a done task; PR-12371 is at work.
func testRatable(t *testing.T) *factoryHarness {
	t.Helper()
	h := testFactory(t, 140, 50)
	h.src.change(func(d *FactoryData) {
		for _, key := range []string{"PR-12330", "PR-12302"} {
			for _, r := range d.Rows {
				if r.Key == key {
					c := d.Cards[r.ID]
					c.Task.Rate = true
					d.Cards[r.ID] = c
				}
			}
		}
	})
	h.press("ctrl+r")
	return h
}

// + then a digit on the card of a published task stores one rating of 1 with
// that reason's key and asks no y/N: a rating is undoable, rating again
// stores a new one. The footer says what the factory stored.
func TestPlusThenADigitRatesAPublishedTaskWithItsReasonAndNoYN(t *testing.T) {
	h := testRatable(t)
	h.open("PR-12330")

	h.press("+")
	mustContain(t, h.view(), "Why +?", "1 the cause and the fix", "2 how much it changed", "3 how it followed my words", "4 the tests",
		"5 the time it took me", "esc skips")
	if got := h.act.rated(); len(got) != 0 {
		t.Fatalf("+ alone stored %+v: the reason's digit comes first", got)
	}
	h.press("4")

	want := []factoryRateCall{{task: 5, value: 1, reason: "tests"}}
	if got := h.act.rated(); !slices.Equal(got, want) {
		t.Fatalf("rated %+v, want %+v", got, want)
	}
	if h.m.bar.asking != nil || h.m.asked != nil {
		t.Error("a rating asked y/N")
	}
	mustContain(t, h.view(), "PR-12330: rated +1 (tests)")
	mustNotContain(t, h.view(), "Why +?")

	h.press("+", "1") // again: a new rating, the first kept
	if got := h.act.rated(); len(got) != 2 || got[1] != (factoryRateCall{task: 5, value: 1, reason: "cause"}) {
		t.Errorf("rating again: %+v, want a second rating for cause", got)
	}
}

// − (and the ASCII -) then esc stores a rating of −1 with no reason key: esc
// skips the reason.
func TestMinusThenEscRatesWithoutAReason(t *testing.T) {
	h := testRatable(t)
	h.open("PR-12330")

	h.press("−")
	mustContain(t, h.view(), "Why −?", "esc skips")
	h.press("esc")
	h.press("-", "esc")

	want := []factoryRateCall{{task: 5, value: -1}, {task: 5, value: -1}}
	if got := h.act.rated(); !slices.Equal(got, want) {
		t.Fatalf("rated %+v, want %+v", got, want)
	}
	if h.m.view != fvTask {
		t.Errorf("a rating left the task card: view %v", h.m.view)
	}
}

// A done task's card rates as a published task's does; a key that is no
// digit of the reasons waits (it stores nothing and starts nothing else),
// and so does a digit past the last reason.
func TestARatingWaitsForADigitOfTheReasonsOrEsc(t *testing.T) {
	h := testRatable(t)
	h.open("PR-12302")

	h.press("+", "x", "9", "0")
	if got := h.act.rated(); len(got) != 0 || h.m.bar.asking != nil {
		t.Fatalf("a key that is no reason stored %+v (asking %v)", got, h.m.bar.asking)
	}
	mustContain(t, h.view(), "Why +?")
	h.press("5")

	if got := h.act.rated(); !slices.Equal(got, []factoryRateCall{{task: 7, value: 1, reason: "time"}}) {
		t.Fatalf("rated %+v, want the done task's + for time", got)
	}
}

// diff_opened_first: the screen tells the daemon whether the operator opened
// this task's diff (d) before the rating; the daemon adds the diff its
// decision on the ready card recorded. A pager that failed, and a pager that
// showed another file (the fixer's report), do not count.
func TestTheRatingSaysWhetherTheDiffWasOpenedFirst(t *testing.T) {
	h := testRatable(t)
	h.open("PR-12330")

	h.press("+", "esc")
	h.press("d")
	if len(h.act.diffs) != 1 || len(h.execs) != 1 {
		t.Fatalf("d asked for %d diffs and %d pagers, want one each", len(h.act.diffs), len(h.execs))
	}
	h.press("−", "esc") // the pager is still open: the diff is not read yet
	h.feed(factoryPagerMsg{task: 5, err: fmt.Errorf("less: not found")})
	h.press("+", "esc") // the pager failed
	h.press("d")
	h.feed(factoryPagerMsg{task: 5})
	h.press("−", "esc") // read

	var got []bool
	for _, c := range h.act.rated() {
		got = append(got, c.diffOpened)
	}
	if want := []bool{false, false, false, true}; !slices.Equal(got, want) {
		t.Fatalf("diff opened first %v, want %v: after d and its pager's end only", got, want)
	}

	// Another task's diff does not count for this one.
	h.press("esc")
	h.open("PR-12302")
	h.press("+", "esc")
	if c := h.act.rated()[4]; c.task != 7 || c.diffOpened {
		t.Errorf("the done task's rating %+v, want the diff not opened: the diff read was another task's", c)
	}
}

// + and − do nothing on the card of a task that is neither published nor
// done, and its hints do not offer them; a card that offers them says so in
// its hints, and the help lists the keys.
func TestPlusDoesNothingOnATasksCardThatIsNotPublishedOrDone(t *testing.T) {
	h := testRatable(t)
	h.open("PR-12371") // at work

	before := h.view()
	h.press("+", "1", "−", "-", "5")
	if got := h.act.rated(); len(got) != 0 {
		t.Fatalf("rated %+v on a task at work", got)
	}
	if h.view() != before {
		t.Errorf("+ and − changed the card of a task at work:\n%s\nbefore:\n%s", h.view(), before)
	}
	mustNotContain(t, h.view(), "+ − rate", "Why +?", "Why −?")

	h.press("esc")
	h.open("PR-12330")
	mustContain(t, h.view(), "+ − rate")
	h.press("?")
	help := ansi.Strip(strings.Join(h.m.helpContent(140), "\n"))
	if !strings.Contains(help, "+ −") || !strings.Contains(help, "rate the task") {
		t.Errorf("the help does not list the rating keys:\n%s", help)
	}
}

// A rating the daemon refused shows its words in the footer, and a rating on
// a screen that runs no actions says so.
func TestARefusedRatingShowsTheDaemonsWords(t *testing.T) {
	h := testRatable(t)
	h.act.rateErr = fmt.Errorf("PR-12330 is active: a task is rated once it is published or done")
	h.open("PR-12330")

	h.press("+", "1")

	mustContain(t, h.view(), "failed", "a task is rated once it is published or done")
}

// The pending reason is for the card it was asked on: going home drops it.
func TestAPendingRatingEndsWithItsCard(t *testing.T) {
	h := testRatable(t)
	h.open("PR-12330")
	h.press("+")
	h.m.goHome()
	h.open("PR-12302")

	h.press("1")
	if got := h.act.rated(); len(got) != 0 {
		t.Errorf("a digit on another card stored %+v for a + asked on the first", got)
	}
}

// The diff read on the ready card counts for the rating after the push, in
// the same run of the screen; a card drawn again for a new head (the task was
// sent back) is a diff not read yet, so the first card's reading does not
// count for what is pushed after.
func TestTheDiffReadOnTheReadyCardCountsForTheRatingAfterThePush(t *testing.T) {
	h := testFactory(t, 140, 50)
	published := func() {
		h.src.change(func(d *FactoryData) {
			c := d.Cards[3]
			c.Ready, c.Task.Rate = nil, true
			d.Cards[3] = c
		})
		h.press("ctrl+r")
	}
	rate := func() factoryRateCall {
		h.m.goHome()
		h.open("PR-12360")
		h.press("+", "esc")
		got := h.act.rated()
		return got[len(got)-1]
	}

	h.open("PR-12360")
	h.press("d")
	h.feed(factoryPagerMsg{task: 3}) // the ready card's diff was read
	published()
	if c := rate(); !c.diffOpened {
		t.Errorf("the rating after reading the ready card's diff: %+v, want the diff opened first", c)
	}

	// Sent back: the card is gone, then drawn for a new head, and not read.
	h.src.change(func(d *FactoryData) {
		c := d.Cards[3]
		r := fixtureReadyCard()
		r.Recommended, r.Cursor = 0, 0
		c.Ready, c.Task.Rate = &r, false
		c.Bind = ReadyBinding{HeadSHA: "11aa22bb33cc", ReportHash: "report-2", DiffHash: "diff-2"}
		d.Cards[3] = c
	})
	h.press("ctrl+r")
	h.m.goHome()
	h.open("PR-12360") // the new card is shown
	published()
	if c := rate(); c.diffOpened {
		t.Errorf("the rating after a push of a new card nobody read: %+v, want the diff not opened", c)
	}
}

// While another action runs, + shows the wait note at the first key and
// asks no reason, so a digit pressed after it is no lost rating: nothing
// waits for it, and the rating is made again once the action ends.
func TestPlusWhileAnotherActionRunsShowsTheWaitAtTheFirstKey(t *testing.T) {
	h := testRatable(t)
	h.open("PR-12330")
	h.m.bar.busy = "Retry PR-12330" // an action in flight

	h.press("+")

	mustContain(t, h.view(), "wait: Retry PR-12330 runs")
	mustNotContain(t, h.view(), "Why +?")
	if h.m.pendingRating() != 0 {
		t.Fatal("+ waits for a reason's digit while another action runs: the rating would be lost at the digit")
	}
	h.press("3")
	if got := h.act.rated(); len(got) != 0 {
		t.Fatalf("rated %+v while another action ran", got)
	}

	h.m.bar.busy = "" // the action ended
	h.press("+", "3")
	if got := h.act.rated(); !slices.Equal(got, []factoryRateCall{{task: 5, value: 1, reason: "words"}}) {
		t.Errorf("rated %+v after the action ended, want + for words", got)
	}
}

// The task card's right-click menu lists + and − from the same keys, dimmed
// where the card offers no rating; an item asks for the reason as the key
// does. While a rating waits for its digit no menu opens, so a menu's
// "back" can never store a rating.
func TestTheTaskCardMenuRatesAndOpensNoMenuWhileARatingWaits(t *testing.T) {
	h := testRatable(t)
	h.open("PR-12330")
	menu := menuState(h.m.menuItems())
	if ok, listed := menu["rate +"]; !listed || !ok || !menu["rate −"] {
		t.Fatalf("the published task's menu %v, want rate + and rate − offered", menu)
	}
	h.feed(tea.MouseClickMsg{X: 20, Y: 10, Button: tea.MouseRight})
	i := slices.IndexFunc(h.m.menuItems(), func(it menuItem) bool { return it.label == "rate −" })
	if !h.m.menu.open || i < 0 {
		t.Fatalf("no menu with rate −: %+v", h.m.menuItems())
	}
	for range i {
		h.press("j")
	}
	h.press("enter")
	if h.m.menu.open {
		t.Error("the menu stayed open after an item ran")
	}
	mustContain(t, h.view(), "Why −?")
	if got := h.act.rated(); len(got) != 0 {
		t.Fatalf("the menu's item stored %+v before the reason", got)
	}

	// A rating waits: a right click opens nothing, and there is no "back".
	h.feed(tea.MouseClickMsg{X: 20, Y: 10, Button: tea.MouseRight})
	if h.m.menu.open || len(h.m.menuItems()) != 0 {
		t.Fatalf("a menu opened while a rating waits: %+v", h.m.menuItems())
	}
	h.press("2")
	if got := h.act.rated(); !slices.Equal(got, []factoryRateCall{{task: 5, value: -1, reason: "scope"}}) {
		t.Errorf("rated %+v, want − for scope", got)
	}

	// A task at work: both items are listed, dimmed.
	h.press("esc")
	h.open("PR-12371")
	if menu := menuState(h.m.menuItems()); menu["rate +"] || menu["rate −"] {
		t.Errorf("the menu of a task at work %v offers rating", menu)
	}
	if _, listed := menuState(h.m.menuItems())["rate +"]; !listed {
		t.Error("the menu of a task at work does not list the dimmed rate +")
	}
}

// In the ASCII glyphs the screen writes the minus as a hyphen, in the
// footer's question, the action's label and the daemon's answer.
func TestTheRatingsMinusIsAHyphenInTheASCIIGlyphs(t *testing.T) {
	h := testRatable(t)
	h.m.g = newGlyphs(IconsASCII)
	h.act.rateText = "PR-12330: rated −, the diff not opened first: noise"
	h.open("PR-12330")

	foot := func() string { // the card's body holds a "−4" of its own
		lines := strings.Split(h.view(), "\n")
		return strings.Join(lines[len(lines)-4:], "\n")
	}
	h.press("−")
	mustContain(t, foot(), "Why -?")
	mustNotContain(t, foot(), "−")
	h.press("esc")

	mustContain(t, foot(), "PR-12330: rated -, the diff not opened first: noise")
	mustNotContain(t, foot(), "−")
}
