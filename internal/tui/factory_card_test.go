package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// The golden frames of the cards are the spec's mockups (section 9.C) drawn by
// the real painters, at 100 and at 140 columns, at the height their content
// needs. checkFactoryFrame (factory_golden_test.go) compares them.

// cardDraw draws one card of the fixtures.
type cardDraw func(w, h int, dark bool, mode IconMode) string

func cardKit(dark bool, mode IconMode) (styles, glyphs) { return newStyles(dark), newGlyphs(mode) }

func drawQuestion(c QuestionCard) cardDraw {
	return func(w, h int, dark bool, mode IconMode) string {
		st, g := cardKit(dark, mode)
		return renderQuestionCard(c, w, h, st, g, cardQuestionNow)
	}
}

func drawRecap(c QuestionCard) cardDraw {
	return func(w, h int, dark bool, mode IconMode) string {
		st, g := cardKit(dark, mode)
		return renderQuestionRecap(c, w, h, st, g, cardQuestionNow)
	}
}

func drawReady(c ReadyCard) cardDraw {
	return func(w, h int, dark bool, mode IconMode) string {
		st, g := cardKit(dark, mode)
		return renderReadyCard(c, w, h, st, g, cardQuestionNow)
	}
}

func drawTask(c TaskCard, scroll int) cardDraw {
	return func(w, h int, dark bool, mode IconMode) string {
		st, g := cardKit(dark, mode)
		frame, _ := renderTaskCard(c, w, h, scroll, st, g, cardTaskNow)
		return frame
	}
}

func drawNewTask(f NewTaskForm) cardDraw {
	return func(w, h int, dark bool, mode IconMode) string {
		st, g := cardKit(dark, mode)
		return renderNewTask(f, w, h, st, g, factoryNow)
	}
}

// cardSquash folds a frame's whitespace, so a phrase that wraps at one width can be found at all.
func cardSquash(s string) string { return strings.Join(strings.Fields(s), " ") }

// plainCard is the card without colors, at the height its content needs.
func plainCard(d cardDraw, w int) string { return ansi.Strip(d(w, 0, true, IconsUnicode)) }

// checkCardGolden compares a card at 100 and at 140 columns with its goldens
// (<name>-100.txt and <name>-140.txt).
func checkCardGolden(t *testing.T, name string, d cardDraw) {
	t.Helper()
	for _, w := range []int{100, 140} {
		checkFactoryFrame(t, fmt.Sprintf("%s-%d.txt", name, w), plainCard(d, w), w)
	}
}

// cardLineWith is the first line of frame that holds sub.
func cardLineWith(t *testing.T, frame, sub string) string {
	t.Helper()
	for l := range strings.SplitSeq(frame, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	t.Fatalf("no line holds %q:\n%s", sub, frame)
	return ""
}

func TestQuestionCardMarksTheRecommendationFirst(t *testing.T) {
	d := drawQuestion(fixtureQuestionCard())
	checkCardGolden(t, "question", d)
	for _, w := range []int{100, 140} {
		frame := plainCard(d, w)
		if n := strings.Count(frame, "›"); n != 1 {
			t.Errorf("%d: %d lines are marked, want exactly the recommended one:\n%s", w, n, frame)
		}
		marked := cardLineWith(t, frame, "›")
		if !strings.Contains(marked, "Seven days before it expires") || !strings.Contains(marked, "recommended") {
			t.Errorf("%d: the marked line is not the recommended option: %q", w, marked)
		}
		lines := strings.Split(frame, "\n")
		at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "›") })
		if !strings.HasPrefix(lines[at+1], "      why: the coupon reminder already uses 7 days") {
			t.Errorf("%d: the reason is not directly under the recommended option: %q", w, lines[at+1])
		}
		for _, want := range []string{"question 2 of 3", "asked 41m ago", "coding starts after all 3", "your answer 13:31", "Example: a credit that expires on Fri 14 Nov."} {
			if !strings.Contains(frame, want) {
				t.Errorf("%d: no %q in\n%s", w, want, frame)
			}
		}
	}
}

func TestQuestionCardWithoutRecommendationMarksNothing(t *testing.T) {
	d := drawQuestion(fixtureNoRecommendationCard())
	checkCardGolden(t, "question-noreco", d)
	for _, w := range []int{100, 140} {
		frame := plainCard(d, w)
		if strings.Contains(frame, "›") {
			t.Errorf("%d: an option is marked though nothing is recommended:\n%s", w, frame)
		}
		// The list above shows the second question was answered with its recommendation.
		_, options, found := strings.Cut(frame, "Yes: a switch")
		if !found || strings.Contains(options, "recommended") || strings.Contains(options, "why:") {
			t.Errorf("%d: a recommendation is drawn:\n%s", w, frame)
		}
		if want := "no recommendation: the ticket and the code give no hint · nothing is marked: pick one (j k)"; !strings.Contains(frame, want) {
			t.Errorf("%d: no %q in\n%s", w, want, frame)
		}
		if strings.Contains(frame, "enter take the marked one") {
			t.Errorf("%d: enter is offered though nothing is marked:\n%s", w, frame)
		}
	}
}

func TestQuestionRecapNamesWhatIsSent(t *testing.T) {
	d := drawRecap(fixtureRecap())
	checkCardGolden(t, "recap", d)
	for _, w := range []int{100, 140} {
		frame := cardSquash(plainCard(d, w))
		for _, want := range []string{
			"your answers", "nothing is sent yet", "the agents get answers 1 and 2",
			"copied for you to post", "waits for rev-ann until you mark question 3 answered", "as of 14:12",
			"Send 2 answers and copy 1 Jira comment for you to post? y/N",
			"┃ rev-ann, should merchants be able to turn off the credit expiry reminder, ┃ or is it always on? The ticket does not say.",
			"recommended", "your answer", "there was no recommendation",
		} {
			if !strings.Contains(frame, want) {
				t.Errorf("%d: no %q in\n%s", w, want, frame)
			}
		}
		if strings.Contains(frame, "as zhuravel") || strings.Contains(frame, "post 1 Jira comment") {
			t.Errorf("%d: the recap says the factory writes to Jira, which it does not in milestone 1:\n%s", w, frame)
		}
	}
}

func TestReadyCardHidesTheRecommendationUntilTheDiffWasOpened(t *testing.T) {
	before := drawReady(fixtureReadyCard())
	checkCardGolden(t, "ready-before", before)
	after := drawReady(fixtureReadyCardAfter())
	checkCardGolden(t, "ready-after", after)
	for _, w := range []int{100, 140} {
		b := plainCard(before, w)
		for _, not := range []string{"recommended", "why:", "›", "you read the diff"} {
			if strings.Contains(b, not) {
				t.Errorf("%d: %q is drawn before the diff was opened:\n%s", w, not, b)
			}
		}
		if !strings.Contains(cardSquash(b), "the recommendation shows after d") {
			t.Errorf("%d: the card does not say when the recommendation shows:\n%s", w, b)
		}
		a := plainCard(after, w)
		if marked := cardLineWith(t, a, "›"); !strings.Contains(marked, "1  push + PR") || !strings.Contains(marked, "recommended") {
			t.Errorf("%d: the marked line after the diff is %q", w, marked)
		}
		for _, want := range []string{"      why: the spec proves the fix; the file outside the plan is test data for it",
			"── you read the diff 14:21 ─"} {
			if !strings.Contains(a, want) {
				t.Errorf("%d: no %q after the diff in\n%s", w, want, a)
			}
		}
		if strings.Contains(a, "the recommendation shows after d") || strings.Contains(a, "draft") {
			t.Errorf("%d: the unread notice stays after the diff was opened, or a draft PR is offered:\n%s", w, a)
		}
	}
}

func TestReadyCardFlagsFilesNotInThePlan(t *testing.T) {
	d := drawReady(fixtureReadyCard())
	frame := d(100, 0, true, IconsUnicode)
	runs := warnRuns(t, frame)
	if !slices.Contains(runs, "not in the plan") || !slices.Contains(runs, "flagged") {
		t.Errorf("yellow runs = %q, want the file outside the plan and the flagged one among them", runs)
	}
	plain := ansi.Strip(frame)
	if l := cardLineWith(t, plain, "reward_rules.rb  "); !strings.HasSuffix(l, "not in the plan") {
		t.Errorf("the file outside the plan is not flagged: %q", l)
	}
	if l := cardLineWith(t, plain, "campaign_copier.rb  "); !strings.HasSuffix(l, "in the plan · flagged") {
		t.Errorf("the flagged file in the plan reads %q", l)
	}
	if l := cardLineWith(t, plain, "campaign_copier_spec.rb  "); !strings.HasSuffix(l, "in the plan") || strings.Contains(l, "not in") || strings.Contains(l, "flagged") {
		t.Errorf("campaign_copier_spec.rb is flagged wrongly: %q", l)
	}
	for _, want := range []string{"+31 −6", "+15 ", "+2 "} {
		if !strings.Contains(plain, want) {
			t.Errorf("no counts %q in\n%s", want, plain)
		}
	}
}

// cardLines are the lines of frame, plain.
func cardLines(frame string) []string { return strings.Split(ansi.Strip(frame), "\n") }

// labelAt is the index of the line that starts with the label.
func labelAt(lines []string, label string) int {
	return slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, " "+label+" ") })
}

// The FLAGS line comes first on the ready card, one flag a line, in yellow:
// they are what needs a second look before the push.
func TestReadyCardShowsTheFlagsLine(t *testing.T) {
	c := fixtureReadyCard()
	for _, w := range []int{100, 140} {
		frame := drawReady(c)(w, 0, true, IconsUnicode)
		lines := cardLines(frame)
		flags, cause := labelAt(lines, "FLAGS"), labelAt(lines, "CAUSE")
		if flags < 0 || cause < 0 || flags > cause {
			t.Fatalf("%d: FLAGS (line %d) should come before CAUSE (line %d):\n%s", w, flags, cause, ansi.Strip(frame))
		}
		squashed := cardSquash(strings.Join(lines[flags:cause], "\n"))
		for _, f := range c.Flags {
			if !strings.Contains(squashed, f) {
				t.Errorf("%d: the flag %q is not on the FLAGS line:\n%s", w, f, ansi.Strip(frame))
			}
		}
		if runs := warnRuns(t, frame); !slices.Contains(runs, c.Flags[0]) {
			t.Errorf("%d: the flags are not yellow: %q", w, runs)
		}
	}
	none := c
	none.Flags = nil
	if l := cardLineWith(t, plainCard(drawReady(none), 100), " FLAGS "); !strings.HasSuffix(l, "none") {
		t.Errorf("a card with nothing flagged should say none: %q", l)
	}
}

// The KNOWN row lists what the local review left open that does not block,
// every kind of the PR body's Known list.
func TestReadyCardShowsTheLocalReviewLeftovers(t *testing.T) {
	c := fixtureReadyCard()
	for _, w := range []int{100, 140} {
		lines := cardLines(drawReady(c)(w, 0, true, IconsUnicode))
		known, shots := labelAt(lines, "KNOWN"), labelAt(lines, "SCREENSHOTS")
		if known < 0 || shots < known {
			t.Fatalf("%d: no KNOWN row before SCREENSHOTS:\n%s", w, strings.Join(lines, "\n"))
		}
		squashed := cardSquash(strings.Join(lines[known:shots], "\n"))
		for _, kind := range c.Leftovers {
			if !strings.Contains(squashed, kind) {
				t.Errorf("%d: the leftover %q is not in the KNOWN row:\n%s", w, kind, strings.Join(lines, "\n"))
			}
		}
	}
	none := c
	none.Leftovers = nil
	if l := cardLineWith(t, plainCard(drawReady(none), 100), " KNOWN "); !strings.HasSuffix(l, "none: the local review left nothing open") {
		t.Errorf("a card the local review left nothing on reads %q", l)
	}
}

// The files a FLAGS line names, and those outside the plan, come first, in
// their order; the others follow.
func TestReadyCardListsFlaggedFilesFirst(t *testing.T) {
	lines := cardLines(drawReady(fixtureReadyCard())(100, 0, true, IconsUnicode))
	at := labelAt(lines, "FILES")
	var order []string
	for _, l := range lines[at : at+3] {
		order = append(order, strings.Fields(strings.TrimPrefix(l, " FILES"))[0])
	}
	if want := []string{"app/services/campaign_copier.rb", "spec/factories/reward_rules.rb", "spec/services/campaign_copier_spec.rb"}; !slices.Equal(order, want) {
		t.Errorf("files are drawn in the order %q, want %q", order, want)
	}
}

// "unread: <file>" names the files not opened in the diff, in place of "You
// have not read the diff yet".
func TestReadyCardNamesTheUnreadFiles(t *testing.T) {
	for _, w := range []int{100, 140} {
		before := plainCard(drawReady(fixtureReadyCard()), w)
		if want := "unread: app/services/campaign_copier.rb, spec/factories/reward_rules.rb, spec/services/campaign_copier_spec.rb · the recommendation shows after d"; !strings.Contains(cardSquash(before), want) {
			t.Errorf("%d: no %q in\n%s", w, want, before)
		}
		if strings.Contains(before, "You have not read the diff yet") {
			t.Errorf("%d: the old notice is drawn:\n%s", w, before)
		}
		after := plainCard(drawReady(fixtureReadyCardAfter()), w)
		if l := cardLineWith(t, after, "unread:"); strings.TrimSpace(l) != "unread: spec/services/campaign_copier_spec.rb" {
			t.Errorf("%d: after the diff the unread line reads %q", w, l)
		}
	}
	read := fixtureReadyCardAfter()
	read.Unread = nil
	if frame := plainCard(drawReady(read), 100); strings.Contains(frame, "unread") {
		t.Errorf("a diff read whole has an unread line:\n%s", frame)
	}
}

// The options are numbered 1 to 3 and say what each does; the digits pick them.
func TestReadyCardNumbersItsOptions(t *testing.T) {
	for _, w := range []int{100, 140} {
		frame := plainCard(drawReady(fixtureReadyCardAfter()), w)
		for _, want := range []string{
			"1  push + PR     → push the branch, open a PR ready for review",
			"2  send back     → the writer tries again with your reason and note",
			"3  drop          → free the copy, keep the branch",
		} {
			if !strings.Contains(frame, want) {
				t.Errorf("%d: no option %q in\n%s", w, want, frame)
			}
		}
		if hints := cardLineWith(t, plainCard(drawReady(fixtureReadyCard()), w), "read the diff"); !strings.Contains(hints, "1-3 pick") {
			t.Errorf("%d: the hints do not offer the digits: %q", w, hints)
		}
	}
}

// reasonFooter checks that the footer of frame asks question and offers
// every reason by its digit, in order, then esc: none of them is cut.
func reasonFooter(t *testing.T, frame, question string, reasons []string) {
	t.Helper()
	lines := cardLines(frame)
	rule := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "──") })
	foot := cardSquash(strings.Join(lines[rule+1:], "\n"))
	rest, ok := strings.CutPrefix(foot, question+" ")
	for i, r := range append(slices.Clone(reasons), "cancels") {
		key := strconv.Itoa(i + 1)
		if i == len(reasons) {
			key = "esc"
		}
		var found bool
		if _, rest, found = strings.Cut(rest, key+" "+r); !found {
			ok = false
		}
	}
	if !ok {
		t.Errorf("the footer reads %q, want %q and then each reason by its digit", foot, question)
	}
}

// After "send back" the footer asks for the reason's key, every reason by
// its digit, before the note.
func TestSendBackShowsReasonKeys(t *testing.T) {
	defer checkCardGolden(t, "ready-sendback", drawReady(fixtureReadyCardSendBack()))
	for _, w := range []int{100, 140} {
		frame := drawReady(fixtureReadyCardSendBack())(w, 0, true, IconsUnicode)
		reasonFooter(t, frame, "Why send back?", sendBackReasons)
		if marked := cardLineWith(t, ansi.Strip(frame), "›"); !strings.Contains(marked, "2  send back") {
			t.Errorf("%d: the mark is not on send back: %q", w, marked)
		}
		for i, l := range strings.Split(frame, "\n") {
			if lw := ansi.StringWidth(l); lw > w {
				t.Errorf("%d: line %d is %d cells wide", w, i+1, lw)
			}
		}
	}
}

// The y/N of a push names what it frees: the copy and the queued task that
// starts in it. A send back frees nothing and says nothing of it.
func TestTheReadyQuestionNamesWhatItFrees(t *testing.T) {
	want := "Push zhuravel-PR-12360-activity-points-on-copy and open a PR? It frees talkable.factory3; PR-12395 starts in it. y/N"
	for _, w := range []int{100, 140} {
		frame := drawReady(fixtureReadyCardAfter())(w, 0, true, IconsUnicode)
		lines := cardLines(frame)
		rule := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "── you read") })
		if foot := cardSquash(strings.Join(lines[rule+1:], "\n")); !strings.HasPrefix(foot, want) {
			t.Errorf("%d: the question reads %q, want %q", w, foot, want)
		}
		for i, l := range strings.Split(frame, "\n") {
			if lw := ansi.StringWidth(l); lw > w {
				t.Errorf("%d: line %d is %d cells wide", w, i+1, lw)
			}
		}
	}
	back := fixtureReadyCardAfter()
	back.Cursor, back.Ask = 1, "Send it back with your note?"
	if frame := plainCard(drawReady(back), 140); strings.Contains(frame, "It frees") {
		t.Errorf("a send back names what it frees:\n%s", frame)
	}
}

// The question card offers the − key (flag the question as needless); after
// −, its footer asks for the reason by digit.
func TestQuestionCardOffersTheNeedlessKey(t *testing.T) {
	for _, w := range []int{100, 140} {
		frame := plainCard(drawQuestion(fixtureQuestionCard()), w)
		if hints := cardLineWith(t, frame, "j k move"); !strings.Contains(hints, "− needless") {
			t.Errorf("%d: the hints do not offer the needless key: %q", w, hints)
		}
		off := fixtureQuestionCard()
		off.Needless = false
		if strings.Contains(plainCard(drawQuestion(off), w), "needless") {
			t.Errorf("%d: the needless key is offered though the card does not take it", w)
		}
		reasonFooter(t, drawQuestion(fixtureNeedlessCard())(w, 0, true, IconsUnicode), "Why needless?", needlessReasons)
	}
	checkCardGolden(t, "question-needless", drawQuestion(fixtureNeedlessCard()))
}

// The marks of the question and ready cards stand on their Cursor, the
// operator's, not on the recommendation, which keeps its tag.
func TestCardsKeepTheirCursor(t *testing.T) {
	q := fixtureQuestionCard()
	q.Cursor = 2
	frame := plainCard(drawQuestion(q), 100)
	if marked := cardLineWith(t, frame, "›"); !strings.Contains(marked, "3  On the day it expires") || strings.Count(frame, "›") != 1 {
		t.Errorf("the mark is not on option 3 alone: %q", marked)
	}
	if l := cardLineWith(t, frame, "Seven days"); !strings.HasSuffix(l, "recommended") {
		t.Errorf("the recommendation lost its tag: %q", l)
	}
	q.Cursor = -1
	if frame := plainCard(drawQuestion(q), 100); strings.Contains(frame, "›") || strings.Contains(frame, "enter take the marked one") {
		t.Errorf("no cursor marks an option, or offers enter:\n%s", frame)
	}
	r := fixtureReadyCardAfter()
	r.Cursor, r.Ask = 2, ""
	if marked := cardLineWith(t, plainCard(drawReady(r), 100), "›"); !strings.Contains(marked, "3  drop") {
		t.Errorf("the ready card's mark is not on drop: %q", marked)
	}
	tc := fixtureTaskCard()
	tc.Cursor = 3
	if marked := cardLineWith(t, plainCard(drawTask(tc, 0), 140), "›"); !strings.Contains(marked, "coding") {
		t.Errorf("the task card's mark is not on coding: %q", marked)
	}
}

// The task card's pinned head never covers a line of its body: at every
// height and mark, the lines under the head are one run of the body, the
// marked line among them, and the rule counts the rest below.
func TestTaskCardNeverHidesARowBehindTheHeader(t *testing.T) {
	st, g := cardKit(true, IconsUnicode)
	for _, c := range []TaskCard{fixtureTaskCard(), fixtureRetryCard()} {
		full, _ := renderTaskCard(c, 140, 0, 0, st, g, cardTaskNow)
		all := strings.Split(ansi.Strip(full), "\n")
		headN := slices.IndexFunc(all, func(l string) bool { return strings.HasPrefix(l, " STAGES") })
		body := all[headN : len(all)-2]
		// run says whether shown is body from line first on, padded with blank
		// lines past its end, with below lines left under it.
		run := func(shown []string, first, below int) bool {
			n := 0
			for i, l := range shown {
				switch {
				case first+i < len(body) && unmark(body[first+i]) == unmark(l):
					n++
				case first+i < len(body) || strings.TrimSpace(l) != "":
					return false
				}
			}
			return first+n+below == len(body)
		}
		for h := headN + 3; h <= len(all); h++ {
			for cur := range 22 {
				c.Cursor = cur
				frame, below := renderTaskCard(c, 140, h, 0, st, g, cardTaskNow)
				lines := strings.Split(ansi.Strip(frame), "\n")
				shown := lines[headN : len(lines)-2]
				if !slices.ContainsFunc(range0(len(body)+1), func(first int) bool { return run(shown, first, below) }) {
					t.Errorf("%s %d cursor %d: the lines under the head are not one run of the body with %d below:\n%s",
						c.Key, h, cur, below, ansi.Strip(frame))
				}
				// Both cards have at least 10 lines a mark stands on.
				if cur < 10 && !slices.ContainsFunc(shown, func(l string) bool { return strings.HasPrefix(l, "› ") }) {
					t.Errorf("%s %d cursor %d: the marked line is out of view:\n%s", c.Key, h, cur, ansi.Strip(frame))
				}
			}
		}
	}
}

// range0 is 0 to n-1.
func range0(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// unmark is a task card line without its cursor and its indent.
func unmark(l string) string { return strings.TrimSpace(strings.TrimPrefix(l, "› ")) }

// Only the marks of working stages and agents move with the spinner.
func TestTaskCardSpinsOnlyForWorkingAgents(t *testing.T) {
	c := fixtureTaskCard()
	c.Agents = append(c.Agents, CardAgent{Name: "QA on bastion", Why: "t:qa on bastion", Now: "4 of 9 checks", State: "working", For: 12 * time.Minute})
	c.Cursor = -1
	a := plainCard(drawTask(c, 0), 140)
	c.Spinner = 1
	b := plainCard(drawTask(c, 0), 140)
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var moved []string
	for i := range al {
		if al[i] != bl[i] {
			moved = append(moved, strings.Fields(bl[i])[1])
		}
	}
	if want := []string{"bastion", "QA"}; !slices.Equal(moved, want) {
		t.Errorf("the lines that moved with the spinner start %q, want %q", moved, want)
	}
}

// A task that waits on the operator offers r, retry, on its card, and a row
// whose move is a retry says "r retry".
// A task whose data says a retry can go on offers r, retry, on its card; a
// task that waits on the operator for anything else (its questions) does
// not, nor one that waits on someone else. On home the row's move says
// "retry" in words, with no key: on home only enter acts (9.A.4).
func TestTaskCardOffersRetryWhenItWaitsOnYou(t *testing.T) {
	defer checkCardGolden(t, "task-retry", drawTask(fixtureRetryCard(), 0))
	if hints := cardLineWith(t, plainCard(drawTask(fixtureRetryCard(), 0), 140), "j k move"); !strings.Contains(hints, "r retry") {
		t.Errorf("a task a retry goes on for does not offer r: %q", hints)
	}
	if hints := cardLineWith(t, plainCard(drawTask(fixtureTaskCard(), 0), 140), "j k move"); strings.Contains(hints, "retry") {
		t.Errorf("a task that waits on rev-ann offers r: %q", hints)
	}
	questions := fixtureTaskCard()
	questions.WaitingFor = WaitsOnYou + ": 3 questions before coding"
	if hints := cardLineWith(t, plainCard(drawTask(questions, 0), 140), "j k move"); strings.Contains(hints, "retry") {
		t.Errorf("a task that waits on its questions offers r: %q", hints)
	}
	frame, rowLines := factoryFrameOf(fixtureTen(), 140, 50, true, IconsUnicode, factoryNow)
	l := strings.Split(ansi.Strip(frame), "\n")[rowLines[8]]
	if !strings.Contains(l, "you: review unavailable (timeout)") || !strings.Contains(l, "  retry ") || strings.Contains(l, "r retry") {
		t.Errorf("the row whose move is a retry reads %q, want the move in words", l)
	}
}

// The form draws the guidance the daemon parsed, as it is: the painter
// never parses the text again, so "PR-12345: words" shows its words and a
// text the daemon gives no guidance shows none.
func TestNewTaskDrawsTheGuidanceTheDaemonParsed(t *testing.T) {
	f := fixtureNewTaskJira(factoryNow)
	f.Text, f.Guidance = "PR-12345: only the refund path", "only the refund path"
	if l := cardLineWith(t, plainCard(drawNewTask(f), 100), "GUIDANCE"); !strings.Contains(l, `GUIDANCE  "only the refund path" goes to every agent`) {
		t.Errorf("the guidance row reads %q", l)
	}
	f.Text, f.Guidance = "PR-12345 the words after the key", ""
	if frame := plainCard(drawNewTask(f), 100); strings.Contains(frame, "GUIDANCE") {
		t.Errorf("a GUIDANCE row the daemon did not give:\n%s", frame)
	}
}

// readyFooter is the lines of a card frame under its rule, squashed.
func readyFooter(frame string) string {
	lines := cardLines(frame)
	rule := len(lines) - 1 - slices.IndexFunc(reversed(lines), func(l string) bool { return strings.HasPrefix(l, "──") })
	return cardSquash(strings.Join(lines[rule+1:], "\n"))
}

// On a short terminal the ready card keeps its options, the mark, the
// recommendation with its reason, the unread files and the y/N in view: it
// cuts the lists above them (KNOWN first, then FILES, then FLAGS) and says
// how much it does not show.
func TestTheReadyCardKeepsItsOptionsAndQuestionInView(t *testing.T) {
	for _, h := range []int{24, 30} {
		frame := drawReady(fixtureReadyCardAfter())(100, h, true, IconsUnicode)
		plain := ansi.Strip(frame)
		if n := len(strings.Split(plain, "\n")); n != h {
			t.Fatalf("100x%d: %d lines:\n%s", h, n, plain)
		}
		for _, want := range []string{"› 1  push + PR", "recommended", "why: the spec proves the fix", "2  send back", "3  drop",
			"unread: spec/services/campaign_copier_spec.rb", " FLAGS "} {
			if !strings.Contains(plain, want) {
				t.Errorf("100x%d: %q is not in view:\n%s", h, want, plain)
			}
		}
		if foot := readyFooter(frame); !strings.Contains(foot, "It frees talkable.factory3; PR-12395 starts in it. y/N") {
			t.Errorf("100x%d: the y/N reads %q", h, foot)
		}
		if h == 24 && !strings.Contains(plain, "4 notes, listed under Known in the PR body") {
			t.Errorf("100x%d: the KNOWN row does not say where its notes are:\n%s", h, plain)
		}
		if lines := strings.Split(plain, "\n"); strings.Contains(lines[len(lines)-3], "more") {
			t.Errorf("100x%d: the options or the unread files went below the rule: %q", h, lines[len(lines)-3])
		}
	}
	before := ansi.Strip(drawReady(fixtureReadyCard())(100, 24, true, IconsUnicode))
	for _, want := range []string{"1 push + PR", "3 drop", "the recommendation shows after d", "d read the diff"} {
		if !strings.Contains(cardSquash(before), want) {
			t.Errorf("100x24 before the diff: %q is not in view:\n%s", want, before)
		}
	}
	checkFactoryFrame(t, "ready-before-100x24.txt", before, 100)
	checkFactoryFrame(t, "ready-after-100x24.txt", ansi.Strip(drawReady(fixtureReadyCardAfter())(100, 24, true, IconsUnicode)), 100)
	// Tall enough for everything, nothing is cut.
	if tall := ansi.Strip(drawReady(fixtureReadyCardAfter())(100, 50, true, IconsUnicode)); strings.Contains(tall, "listed under Known") || strings.Contains(tall, "more") {
		t.Errorf("100x50 cuts a list:\n%s", tall)
	}
}

// A drop's y/N is the spec's sentence, which names the copy: the painter
// adds nothing to it. Only the push's y/N says what it frees.
func TestTheDropQuestionNamesTheCopyOnce(t *testing.T) {
	c := fixtureReadyCardAfter()
	c.Cursor, c.Ask = 2, "Drop PR-12360: free talkable.factory3, keep the branch?"
	defer checkFactoryFrame(t, "ready-drop-100.txt", plainCard(drawReady(c), 100), 100)
	if foot := readyFooter(drawReady(c)(100, 0, true, IconsUnicode)); strings.Count(foot, "talkable.factory3") != 1 || strings.Contains(foot, "It frees") {
		t.Errorf("the drop's y/N reads %q", foot)
	}
}

// A y/N never loses what it frees: on a narrow screen it takes a third line
// rather than cut a word.
func TestAQuestionTooLongForTwoLinesTakesAThird(t *testing.T) {
	frame := drawReady(fixtureReadyCardAfter())(60, 0, true, IconsUnicode)
	foot := readyFooter(frame)
	if want := "Push zhuravel-PR-12360-activity-points-on-copy and open a PR? It frees talkable.factory3; PR-12395 starts in it. y/N"; !strings.HasPrefix(foot, want) {
		t.Errorf("at 60 columns the y/N reads %q, want %q", foot, want)
	}
	for i, l := range strings.Split(frame, "\n") {
		if lw := ansi.StringWidth(l); lw > 60 {
			t.Errorf("line %d is %d cells wide", i+1, lw)
		}
	}
}

func TestTaskCardListsStagesWithProof(t *testing.T) {
	d := drawTask(fixtureTaskCard(), 0)
	checkCardGolden(t, "task", d)
	frame := plainCard(d, 140)
	for _, s := range fixtureTaskCard().Stages {
		if !strings.Contains(frame, s.Proof) {
			t.Errorf("the proof %q of stage %s is not drawn:\n%s", s.Proof, s.Name, frame)
		}
	}
	if l := cardLineWith(t, frame, "bastion        merging"); !strings.HasPrefix(l, "› ") || !strings.HasSuffix(strings.TrimRight(l, " "), "now") {
		t.Errorf("the working stage is not under the mark, or does not end in now: %q", l)
	}
	if n := strings.Count(frame, "› "); n != 1 {
		t.Errorf("%d lines are marked, want 1", n)
	}
	var order []string
	for l := range strings.SplitSeq(frame, "\n") {
		for _, h := range []string{" WAITING FOR ", " STAGES", " AGENTS ", " REPORT", " LEARNED "} {
			if strings.HasPrefix(l, h) {
				order = append(order, strings.TrimSpace(h))
			}
		}
	}
	if want := []string{"WAITING FOR", "STAGES", "AGENTS", "REPORT", "LEARNED"}; !slices.Equal(order, want) {
		t.Errorf("sections = %q, want %q", order, want)
	}
	for _, want := range []string{"took", "finished", "08:40", "waiting", "1h11m", "started 08:40", "your time on it 6m", "#4527", "copy 2 of 3, server off", "↻ 14:09"} {
		if !strings.Contains(frame, want) {
			t.Errorf("no %q in\n%s", want, frame)
		}
	}
}

func TestNewTaskFromAJiraKey(t *testing.T) {
	d := drawNewTask(fixtureNewTaskJira(factoryNow))
	checkCardGolden(t, "new-jira", d)
	for _, w := range []int{100, 140} {
		frame := plainCard(d, w)
		for _, want := range []string{
			"PR-12345 · Story · To Do · High · reporter rev-ann · read by the daemon 2s ago",
			`GUIDANCE  "the reminder is for advocates only, not their friends" goes to every agent`,
			"from the Jira type Story · tab: bug, feature, chore", "3 talkable rules go into its prompts",
			"Start feature PR-12345 with 2 agents in talkable.factory2? y/N", "pgdn reads more", "L shows them",
		} {
			if !strings.Contains(frame, want) {
				t.Errorf("%d: no %q in\n%s", w, want, frame)
			}
		}
	}
}

// A key the text names elsewhere than at its start is the JIRA line's key
// with the note "found in your text", on the line of its summary; a key at
// the start has no note.
func TestNewTaskSaysAKeyWasFoundInTheText(t *testing.T) {
	f := fixtureNewTaskJira(factoryNow)
	f.Text = "fix the expiry date in PR-12345, only the refund path"
	f.Jira.Found = "found in your text"
	for _, w := range []int{100, 140} {
		frame := plainCard(drawNewTask(f), w)
		if !strings.Contains(frame, "Credit expiry reminder email · found in your text") {
			t.Errorf("%d: no note on the summary line in\n%s", w, frame)
		}
		if !strings.Contains(frame, "PR-12345 · Story · To Do · High · reporter rev-ann · read by the daemon 2s ago") {
			t.Errorf("%d: the facts line changed in\n%s", w, frame)
		}
		if strings.Contains(plainCard(drawNewTask(fixtureNewTaskJira(factoryNow)), w), "found in your text") {
			t.Errorf("%d: a key at the start has a note", w)
		}
	}
	f.Jira.Summary = ""
	if frame := plainCard(drawNewTask(f), 100); !strings.Contains(frame, "found in your text") {
		t.Errorf("a key with no summary lost its note in\n%s", frame)
	}
}

// The links of the text are a row of their own: what each is and what the
// factory makes of it.
func TestNewTaskShowsItsLinks(t *testing.T) {
	d := drawNewTask(fixtureNewTaskLinks(factoryNow))
	defer checkCardGolden(t, "new-links", d)
	for _, w := range []int{100, 140} {
		lines := cardLines(d(w, 0, true, IconsUnicode))
		at := labelAt(lines, "LINKS")
		if at < 0 {
			t.Fatalf("%d: no LINKS row:\n%s", w, strings.Join(lines, "\n"))
		}
		for i, want := range [][]string{
			{"Jira", "example.atlassian.net/browse/PR-12345", "its key, PR-12345"},
			{"PR", "github.com/example/talkable/pull/4512", "adoption comes in milestone 2"},
			{"CI run", "github.com/example/talkable/actions/runs/1234567", "a CI task comes in milestone 3"},
		} {
			for _, part := range want {
				if !strings.Contains(lines[at+i], part) {
					t.Errorf("%d: link %d lacks %q: %q", w, i+1, part, lines[at+i])
				}
			}
		}
		if g := cardLineWith(t, strings.Join(lines, "\n"), "GUIDANCE"); !strings.Contains(g, `"the same fix as`) {
			t.Errorf("%d: the words after the Jira link are not its guidance: %q", w, g)
		}
	}
	jira := plainCard(drawNewTask(fixtureNewTaskJira(factoryNow)), 100)
	if l := cardLineWith(t, jira, "OVERLAP"); !strings.Contains(l, "1 commit names PR-12345") {
		t.Errorf("the overlap line reads %q", l)
	}
}

// While the daemon reads Jira the JIRA row says so behind a spinner, and
// never that the agents work from the words.
func TestNewTaskShowsReadingWhileJiraIsRead(t *testing.T) {
	d := drawNewTask(fixtureNewTaskReading())
	defer checkFactoryFrame(t, "new-reading-100.txt", plainCard(d, 100), 100)
	frame := plainCard(d, 100)
	if l := cardLineWith(t, frame, " JIRA "); !strings.Contains(l, "⣷ the daemon reads Jira") {
		t.Errorf("the JIRA row while Jira is read: %q", l)
	}
	if strings.Contains(frame, "none: the agents work from your words") {
		t.Errorf("the form says the task is a prompt while Jira is read:\n%s", frame)
	}
	f := fixtureNewTaskReading()
	f.Spinner = 1
	if l := cardLineWith(t, plainCard(drawNewTask(f), 100), " JIRA "); strings.Contains(l, "⣷") {
		t.Errorf("the reading spinner does not move: %q", l)
	}
}

// The keys a text names elsewhere than at its start (spec 6.1): one key Jira
// knows is the task's, noted as found, with the whole text as guidance; one
// Jira refused is the error colour's note and a prompt task; several known
// keys are the prompt task they are, which asks which issue it is.
func TestNewTaskShowsTheKeysFoundInTheText(t *testing.T) {
	found := drawNewTask(fixtureNewTaskFound(factoryNow))
	refused := drawNewTask(fixtureNewTaskRefused())
	several := drawNewTask(fixtureNewTaskSeveral())
	defer checkCardGolden(t, "new-found", found)
	defer checkCardGolden(t, "new-refused", refused)
	defer checkCardGolden(t, "new-several", several)
	for _, w := range []int{100, 140} {
		f := plainCard(found, w)
		for _, want := range []string{"found in your text", `GUIDANCE "fix the expiry reminder of PR-12345: advocates only, not their friends" goes to every agent`} {
			if !strings.Contains(cardSquash(f), want) {
				t.Errorf("%d: no %q in\n%s", w, want, f)
			}
		}
		r := refused(w, 0, true, IconsUnicode)
		note := "Jira refused PR-12345: the Jira token file is missing; starts from your words"
		if l := cardLineWith(t, ansi.Strip(r), " JIRA "); !strings.Contains(l, note) {
			t.Errorf("%d: the JIRA row of a refused key reads %q", w, l)
		}
		errOpen, _, _ := strings.Cut(newStyles(true).Err.Render("x"), "x")
		if !strings.Contains(r, errOpen+note) {
			t.Errorf("%d: the refusal is not in the error colour", w)
		}
		if !strings.Contains(ansi.Strip(r), "TITLE     Refund of PR-12345") || strings.Contains(ansi.Strip(r), "GUIDANCE") {
			t.Errorf("%d: a refused key's task is not a prompt task:\n%s", w, ansi.Strip(r))
		}
		s := plainCard(several, w)
		if l := cardLineWith(t, s, " JIRA "); !strings.Contains(l, "PR-12345 and PR-12360 found in your text · the task asks which issue it is") {
			t.Errorf("%d: the JIRA row of several keys reads %q", w, l)
		}
		if !strings.Contains(s, "TITLE     Copier change of PR-12345 also breaks PR-12360") {
			t.Errorf("%d: several keys draw no TITLE:\n%s", w, s)
		}
		for _, not := range []string{"GUIDANCE", "Story", "reporter", "pgdn"} {
			if strings.Contains(s, not) {
				t.Errorf("%d: several keys draw %q, as if a ticket were the task's:\n%s", w, not, s)
			}
		}
	}
	// A ticket in the data beside several keys is still not drawn as the task's.
	f := fixtureNewTaskSeveral()
	f.Jira = fixtureNewTaskJira(factoryNow).Jira
	if s := plainCard(drawNewTask(f), 100); strings.Contains(s, "Story") || !strings.Contains(s, "TITLE") {
		t.Errorf("a first key's ticket is drawn as the task's:\n%s", s)
	}
}

func TestNewTaskFromAPrompt(t *testing.T) {
	d := drawNewTask(fixtureNewTaskPrompt())
	checkCardGolden(t, "new-prompt", d)
	for _, w := range []int{100, 140} {
		frame := plainCard(d, w)
		for _, want := range []string{
			"JIRA      none: the agents work from your words", "TITLE     Weekly digest uses the wrong week east of UTC+10",
			`guessed from "shows last week's numbers" · tab: bug, feature, chore`,
			"queued: memory full (swap 91%) · starts when swap is under 80%",
			`Queue bug "Weekly digest uses the wrong week…"? It starts when memory allows. y/N`,
		} {
			if !strings.Contains(frame, want) {
				t.Errorf("%d: no %q in\n%s", w, want, frame)
			}
		}
		if strings.Contains(frame, "GUIDANCE") || strings.Contains(frame, "pgdn") {
			t.Errorf("%d: a prompt has no guidance and no Jira text to page:\n%s", w, frame)
		}
	}
}

// reversed is ss from its end.
func reversed(ss []string) []string {
	out := slices.Clone(ss)
	slices.Reverse(out)
	return out
}

// cardCases are every card the fixtures draw.
func cardCases() map[string]cardDraw {
	q := fixtureQuestionCard()
	empty := QuestionCard{TaskKey: "PR-12345"}
	return map[string]cardDraw{
		"question": drawQuestion(q), "question-noreco": drawQuestion(fixtureNoRecommendationCard()),
		"question-empty": drawQuestion(empty), "recap": drawRecap(fixtureRecap()), "recap-empty": drawRecap(empty),
		"ready-before": drawReady(fixtureReadyCard()), "ready-after": drawReady(fixtureReadyCardAfter()),
		"ready-bare":     drawReady(ReadyCard{TaskKey: "PR-12360", Cursor: -1}),
		"ready-sendback": drawReady(fixtureReadyCardSendBack()), "question-needless": drawQuestion(fixtureNeedlessCard()),
		"task": drawTask(fixtureTaskCard(), 0), "task-scrolled": drawTask(fixtureTaskCard(), 9),
		"task-bare": drawTask(TaskCard{Key: "PR-12330"}, 0), "task-retry": drawTask(fixtureRetryCard(), 0),
		"new-jira": drawNewTask(fixtureNewTaskJira(factoryNow)), "new-prompt": drawNewTask(fixtureNewTaskPrompt()),
		"new-empty": drawNewTask(NewTaskForm{}),
		"new-unknown": drawNewTask(NewTaskForm{Text: "PR-12399 the reminder", Err: "PR-12399 is not in Jira",
			Ask: "Start from your words only?"}),
		"new-links": drawNewTask(fixtureNewTaskLinks(factoryNow)), "new-found": drawNewTask(fixtureNewTaskFound(factoryNow)),
		"new-refused": drawNewTask(fixtureNewTaskRefused()), "new-several": drawNewTask(fixtureNewTaskSeveral()),
		"new-reading": drawNewTask(fixtureNewTaskReading()),
	}
}

func TestFactoryCardsNeverOverflow(t *testing.T) {
	for name, d := range cardCases() {
		for _, mode := range []IconMode{IconsUnicode, IconsNerd, IconsASCII} {
			for _, w := range factoryWidths(60) {
				for _, h := range []int{0, 12, 20, 30, 45} {
					frame := d(w, h, true, mode)
					lines := strings.Split(frame, "\n")
					if h > 0 && len(lines) > h {
						t.Errorf("%s %s %dx%d: %d lines, over %d", name, mode, w, h, len(lines), h)
					}
					for i, l := range lines {
						if lw := ansi.StringWidth(l); lw > w {
							t.Errorf("%s %s %dx%d: line %d is %d cells wide: %q", name, mode, w, h, i+1, lw, ansi.Strip(l))
						}
					}
				}
			}
		}
	}
}

func TestFactoryCardsFillTheirHeightSoTheFooterSitsAtTheBottom(t *testing.T) {
	for name, d := range cardCases() {
		if strings.Contains(name, "empty") || strings.Contains(name, "bare") {
			continue
		}
		frame := ansi.Strip(d(120, 60, true, IconsUnicode))
		lines := strings.Split(frame, "\n")
		if len(lines) != 60 {
			t.Errorf("%s: %d lines at height 60, want 60", name, len(lines))
			continue
		}
		// The footer is one line, or two or three for a long question or reasons.
		rule := len(lines) - 1 - slices.IndexFunc(reversed(lines), func(l string) bool { return strings.HasPrefix(l, "──") })
		if foot := len(lines) - 1 - rule; rule >= len(lines) || foot < 1 || foot > 3 {
			t.Errorf("%s: the rule is not right above a footer of 1 to 3 lines (%d):\n%s", name, foot, frame)
		}
	}
}

func TestFactoryCardsCountWhatTheirHeightCutsOff(t *testing.T) {
	// The question card gives way above its options first (12 rows hold the
	// question and its options): at 9 even those are cut, and counted.
	heights := map[string]int{"question": 9, "ready-before": 12, "new-jira": 12}
	for _, name := range []string{"question", "ready-before", "new-jira"} {
		h := heights[name]
		frame := ansi.Strip(cardCases()[name](100, h, true, IconsUnicode))
		lines := strings.Split(frame, "\n")
		if len(lines) != h {
			t.Fatalf("%s: %d lines, want %d:\n%s", name, len(lines), h, frame)
		}
		// The ready card keeps its options above the rule: its lists count
		// what they do not show.
		if name == "ready-before" {
			if !strings.Contains(frame, "more lines, a taller window shows them") {
				t.Errorf("%s: the card does not count the lines it cuts:\n%s", name, frame)
			}
			continue
		}
		if rule := lines[len(lines)-2]; !strings.Contains(rule, "▼") || !strings.Contains(rule, "more") {
			t.Errorf("%s: the rule does not count the lines below: %q", name, rule)
		}
	}
}

func TestFactoryCardsDarkAndLightColors(t *testing.T) {
	for _, c := range []struct {
		name string
		d    cardDraw
		w    int
	}{
		{"question", drawQuestion(fixtureQuestionCard()), 100},
		{"recap", drawRecap(fixtureRecap()), 100},
		{"ready-before", drawReady(fixtureReadyCard()), 100},
		{"ready-after", drawReady(fixtureReadyCardAfter()), 100},
		{"new-refused", drawNewTask(fixtureNewTaskRefused()), 100},
		{"new-jira", drawNewTask(fixtureNewTaskJira(factoryNow)), 100},
		{"task", drawTask(fixtureTaskCard(), 0), 140},
	} {
		checkFactoryFrame(t, fmt.Sprintf("%s-%d-dark.ansi", c.name, c.w), c.d(c.w, 0, true, IconsUnicode), c.w)
	}
	checkFactoryFrame(t, "question-100-light.ansi", drawQuestion(fixtureQuestionCard())(100, 0, false, IconsUnicode), 100)
}

func TestFactoryCardsYellowMeansOnYouOnly(t *testing.T) {
	q := drawQuestion(fixtureQuestionCard())(100, 0, true, IconsUnicode)
	if got := warnRuns(t, q); !slices.Equal(got, []string{"●"}) {
		t.Errorf("the question card is yellow on %q, want only the current question's mark", got)
	}
	okRuns := func(frame string) []string {
		open, _, _ := strings.Cut(newStyles(true).OK.Render("x"), "x")
		var out []string
		rest := frame
		for {
			i := strings.Index(rest, open)
			if i < 0 {
				return out
			}
			rest = rest[i+len(open):]
			end := strings.Index(rest, "\x1b[")
			if end < 0 {
				end = len(rest)
			}
			out = append(out, rest[:end])
		}
	}
	if got := okRuns(q); !slices.Contains(got, "✓") || !slices.Contains(got, "recommended") {
		t.Errorf("the answered mark and the recommendation are not green: %q", got)
	}
	ready := drawReady(fixtureReadyCardAfter())(100, 0, true, IconsUnicode)
	flagged := cardSquash(strings.Join(fixtureReadyCardAfter().Flags, " "))
	for _, r := range warnRuns(t, ready) {
		switch {
		case r == "not in the plan", r == "flagged", r == "unread:":
		case strings.TrimSpace(r) != "" && strings.Contains(flagged, cardSquash(r)):
		case strings.HasPrefix(r, "Push zhuravel"), strings.HasPrefix(r, "It frees"), strings.HasSuffix(r, "y/N"):
		default:
			t.Errorf("the ready card is yellow on %q: only the flags, the files to look at, the unread word and the question are on the operator", r)
		}
	}
	task := drawTask(fixtureTaskCard(), 0)(140, 0, true, IconsUnicode)
	if got := warnRuns(t, task); len(got) != 0 {
		t.Errorf("the task card waits on rev-ann, but it is yellow on %q", got)
	}
	onYou := fixtureTaskCard()
	onYou.WaitingFor = "you: read the diff before it is pushed"
	if got := warnRuns(t, drawTask(onYou, 0)(140, 0, true, IconsUnicode)); !slices.Equal(got, []string{"you:"}) {
		t.Errorf("a task that waits on the operator should be yellow on you: only, got %q", got)
	}
}

func TestQuestionRecapCountsAnswersAndCommentsApart(t *testing.T) {
	st, g := cardKit(true, IconsUnicode)
	c := fixtureRecap()
	only := QuestionCard{TaskKey: "PR-12345", Questions: c.Questions[:2]}
	frame := ansi.Strip(renderQuestionRecap(only, 100, 0, st, g, cardQuestionNow))
	if !strings.Contains(frame, "Send 2 answers? y/N") || strings.Contains(frame, "Jira") || strings.Contains(frame, "waits for") {
		t.Errorf("two answers and no ask should read as just answers:\n%s", frame)
	}
	asks := QuestionCard{TaskKey: "PR-12345", Questions: []CardQuestion{c.Questions[2], c.Questions[2]}}
	asks.Questions[1].Asks = "alice"
	frame = cardSquash(ansi.Strip(renderQuestionRecap(asks, 100, 0, st, g, cardQuestionNow)))
	for _, want := range []string{"Copy 2 Jira comments for you to post? y/N", "waits for rev-ann and alice until you mark questions 1 and 2 answered"} {
		if !strings.Contains(frame, want) {
			t.Errorf("no %q in\n%s", want, frame)
		}
	}
	open := fixtureRecap()
	open.Questions[1].Answer = ""
	frame = ansi.Strip(renderQuestionRecap(open, 100, 0, st, g, cardQuestionNow))
	if !strings.Contains(frame, "not answered yet") || !strings.Contains(frame, "Send 1 answer and copy 1 Jira comment for you to post? y/N") {
		t.Errorf("an open question should read so and stay out of the count:\n%s", frame)
	}
}

func TestTaskCardScrollsAndCountsWhatIsBelow(t *testing.T) {
	st, g := cardKit(true, IconsUnicode)
	draw := func(cursor, scroll int) (string, int) {
		c := fixtureTaskCard()
		c.Cursor = cursor
		frame, below := renderTaskCard(c, 140, 14, scroll, st, g, cardTaskNow)
		return ansi.Strip(frame), below
	}
	// The mark is on bastion, far down: the window moves to show it.
	onBastion, below := draw(12, 0)
	lines := strings.Split(onBastion, "\n")
	if len(lines) != 14 {
		t.Fatalf("%d lines, want 14:\n%s", len(lines), onBastion)
	}
	if rule := lines[len(lines)-2]; below <= 0 || !strings.Contains(rule, fmt.Sprintf("▼ %d more", below)) {
		t.Errorf("below = %d, the rule reads %q", below, rule)
	}
	for _, pinned := range []string{"magnum factory · PR-12330", "zhuravel-PR-12330", "WAITING FOR"} {
		if !strings.Contains(onBastion, pinned) {
			t.Errorf("%q is not pinned at the top:\n%s", pinned, onBastion)
		}
	}
	if !strings.Contains(onBastion, "› ⣷ bastion") {
		t.Errorf("the line under the mark is out of view:\n%s", onBastion)
	}
	// The mark on the first stage: the window starts at the top and leaves more below.
	onStart, belowStart := draw(0, 0)
	if !strings.Contains(onStart, "› ✓ start") || !strings.Contains(onStart, "STAGES") || belowStart <= below {
		t.Errorf("the top of the card is not shown, or less is below it (%d, was %d):\n%s", belowStart, below, onStart)
	}
	// A mark out of range leaves the scroll alone: scrolling down shows less of the top.
	scrolled, belowScrolled := draw(99, 9)
	if strings.Contains(scrolled, "✓ start") || !strings.Contains(scrolled, "WAITING FOR") || belowScrolled >= belowStart {
		t.Errorf("scroll 9 should hide the first stages, keep the pinned lines and leave less below (%d):\n%s", belowScrolled, scrolled)
	}
	// Scrolled past the end: the last lines fill the window and nothing is below.
	end, belowEnd := draw(99, 500)
	if belowEnd != 0 || !strings.Contains(end, "LEARNED") {
		t.Errorf("past the end should show the last line with nothing below, got %d:\n%s", belowEnd, end)
	}
	all, belowAll := renderTaskCard(fixtureTaskCard(), 140, 0, 0, st, g, cardTaskNow)
	if belowAll != 0 || len(strings.Split(all, "\n")) < 30 {
		t.Errorf("no height should draw everything and leave nothing below, got below = %d", belowAll)
	}
}

func TestFactoryCardsCleanTheirText(t *testing.T) {
	q := fixtureQuestionCard()
	q.Title = "Credit\x1b[31m expiry\n reminder"
	q.Questions[1].Text = "When\x1b[2J does it\x07 go out?"
	q.Questions[1].Options[0].Label = "Seven\x1b[31m days"
	tc := fixtureTaskCard()
	tc.WaitingFor = "rev-ann\x1b[2J: review"
	tc.Stages[0].Proof = "Jira\x07 read"
	tc.Report[0] = "cause\x1b[31m locale"
	f := fixtureNewTaskPrompt()
	f.Text = "weekly\x1b[2J digest\x00"
	f.Agents[0] = "re\x07produce"
	r := fixtureReadyCard()
	r.Files[0].Path = "app/\x1b[31mx.rb"
	r.Doubts[0] = "was\x07 unsure"
	for name, d := range map[string]cardDraw{"question": drawQuestion(q), "recap": drawRecap(q), "task": drawTask(tc, 0),
		"new": drawNewTask(f), "ready": drawReady(r)} {
		frame := d(120, 0, true, IconsUnicode)
		for _, bad := range []string{"\x1b[2J", "\x1b[31m", "\x07", "\x00"} {
			if strings.Contains(frame, bad) {
				t.Errorf("%s: a control sequence from the data reached the frame: %q", name, bad)
			}
		}
	}
}

func TestFactoryCardsDoNotChangeTheirData(t *testing.T) {
	q := fixtureQuestionCard()
	q.Questions[1].Text = "When\x1b[2J does it go out?"
	q.Questions[1].Options[0].Label = "Seven\x07 days"
	before := fmt.Sprintf("%#v", q)
	plainCard(drawQuestion(q), 100)
	plainCard(drawRecap(q), 100)
	if after := fmt.Sprintf("%#v", q); before != after {
		t.Errorf("drawing the card changed the caller's data:\n%s\n%s", before, after)
	}
}

func TestFactoryCardsDrawNoUnicodeChromeInASCIIMode(t *testing.T) {
	for name, d := range map[string]cardDraw{
		"question": drawQuestion(fixtureQuestionCard()), "ready-after": drawReady(fixtureReadyCardAfter()),
		"new-jira": drawNewTask(fixtureNewTaskJira(factoryNow)), "new-prompt": drawNewTask(fixtureNewTaskPrompt()),
	} {
		frame := ansi.Strip(d(100, 0, true, IconsASCII))
		for _, r := range frame {
			// The fixtures' data has an ellipsis of its own, and the key hints join
			// with " · " in every mode (styles.hints); the chrome has no other.
			if r > 127 && r != '…' && r != '·' {
				t.Errorf("%s: %q is drawn in ASCII mode:\n%s", name, string(r), frame)
				break
			}
		}
	}
	task := ansi.Strip(drawTask(fixtureTaskCard(), 0)(140, 0, true, IconsASCII))
	for _, not := range []string{"›", "◷", "⣷", "┃"} {
		if strings.Contains(task, not) {
			t.Errorf("the task card draws %q in ASCII mode", not)
		}
	}
	if !strings.Contains(task, "> ") {
		t.Errorf("the task card has no ASCII cursor:\n%s", task)
	}
}

func TestFactoryCardsAgeWordsAreReadable(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{{0, "just now"}, {2 * time.Second, "2s ago"}, {59 * time.Second, "59s ago"}, {41 * time.Minute, "41m ago"}, {3 * time.Hour, "3h ago"}} {
		if got := agoText(c.d); got != c.want {
			t.Errorf("agoText(%v) = %q, want %q", c.d, got, c.want)
		}
	}
	for _, c := range []struct {
		d    time.Duration
		want string
	}{{0, "0s"}, {45 * time.Second, "45s"}, {24 * time.Minute, "24m"}, {3 * time.Hour, "3h"}, {71 * time.Minute, "1h11m"}, {65 * time.Minute, "1h05m"}} {
		if got := tookText(c.d); got != c.want {
			t.Errorf("tookText(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestTaskCardMarkStandsOnStagesThenAgentsThenTheReport(t *testing.T) {
	for _, c := range []struct {
		cursor int
		want   string // "" = no line is marked
	}{
		{0, "› ✓ start"}, {14, "› · cleanup"}, {15, "› ✓ QA on void"}, {19, "› ✓ 3 investigators"},
		{20, "› REPORT"}, {21, ""}, {-1, ""},
	} {
		tc := fixtureTaskCard()
		tc.Cursor = c.cursor
		frame := plainCard(drawTask(tc, 0), 140)
		marked := 0
		for l := range strings.SplitSeq(frame, "\n") {
			if strings.HasPrefix(l, "› ") {
				marked++
				if !strings.HasPrefix(l, c.want) {
					t.Errorf("cursor %d: the marked line reads %q, want it to start with %q", c.cursor, l, c.want)
				}
			}
		}
		if want := map[bool]int{true: 1, false: 0}[c.want != ""]; marked != want {
			t.Errorf("cursor %d: %d lines are marked, want %d", c.cursor, marked, want)
		}
	}
}

func TestTaskCardAnAgentIsYellowOnlyWhenItWaitsOnYou(t *testing.T) {
	tc := fixtureTaskCard()
	tc.Agents = []CardAgent{
		{Name: "stopped one", Why: "was reading the history", State: "stopped", Output: cardAt(12, 0)},
		{Name: "parked one", Why: "waits for memory", State: "parked"},
		{Name: "no state one", Why: "the registry gave none"},
		{Name: "failed one", Why: "died on start", State: "failed", Output: cardAt(11, 0)},
	}
	frame := drawTask(tc, 0)(140, 0, true, IconsUnicode)
	if got := warnRuns(t, frame); len(got) != 0 {
		t.Errorf("no agent waits on the operator, but the card is yellow on %q", got)
	}
	plain := ansi.Strip(frame)
	for name, want := range map[string][]string{
		"stopped one":  {"  · stopped one", "stopped 12:00"},
		"parked one":   {"  · parked one", "parked"},
		"no state one": {"  · no state one", "unknown"},
		"failed one":   {"  ✗ failed one", "failed 11:00"},
	} {
		l := cardLineWith(t, plain, name)
		for _, w := range want {
			if !strings.Contains(l, w) {
				t.Errorf("the line of %q lacks %q: %q", name, w, l)
			}
		}
	}
	tc.Agents = append(tc.Agents, CardAgent{Name: "asks you", Why: "needs a login", State: AgentWaitsOnYou})
	frame = drawTask(tc, 0)(140, 0, true, IconsUnicode)
	if got := warnRuns(t, frame); !slices.Equal(got, []string{"●"}) {
		t.Errorf("only the agent that waits on the operator is yellow, got %q", got)
	}
	if l := cardLineWith(t, ansi.Strip(frame), "asks you"); !strings.Contains(l, "● asks you") || !strings.Contains(l, "waits on you") {
		t.Errorf("the agent that waits on the operator reads %q", l)
	}
}

func TestNewTaskEchoShowsWhatWasTyped(t *testing.T) {
	input := func(text string, w int) []string {
		f := NewTaskForm{Text: text}
		var rows []string
		for l := range strings.SplitSeq(plainCard(drawNewTask(f), w), "\n") {
			if strings.HasPrefix(l, " ┃") {
				rows = append(rows, l)
			}
		}
		return rows
	}
	for _, c := range []struct {
		name, text string
		want       []string
	}{
		{"a line break at the end", "foo\n", []string{" ┃ foo", " ┃ ▏"}},
		{"two line breaks at the end", "foo\n\n", []string{" ┃ foo", " ┃", " ┃ ▏"}},
		{"a space at the end", "foo ", []string{" ┃ foo ▏"}},
		{"two spaces inside", "foo  bar", []string{" ┃ foo  bar▏"}},
		{"an indent", "  foo\n    bar", []string{" ┃   foo", " ┃     bar▏"}},
		{"a blank line inside", "foo\n\nbar", []string{" ┃ foo", " ┃", " ┃ bar▏"}},
		{"a tab", "a\tb", []string{" ┃ a    b▏"}},
	} {
		if got := input(c.text, 100); !slices.Equal(got, c.want) {
			t.Errorf("%s: the echo of %q reads %q, want %q", c.name, c.text, got, c.want)
		}
	}
	// Control characters never reach the echo, and a lone carriage return is a line break.
	if got := input("a\x1b[2Jb\x00c\rd", 100); !slices.Equal(got, []string{" ┃ ab c", " ┃ d▏"}) {
		t.Errorf("control characters in the echo: %q", got)
	}
	// A line wider than the screen breaks at the width and loses no character.
	long := strings.Repeat("ab  ", 12)
	rows := input(long, 30)
	if len(rows) < 2 || !strings.Contains(rows[0], "ab  ab  ab") {
		t.Fatalf("the long line should wrap and keep its double spaces: %q", rows)
	}
	var kept strings.Builder
	for _, r := range rows {
		kept.WriteString(strings.TrimPrefix(strings.TrimSuffix(r, "▏"), " ┃ "))
	}
	if strings.Count(kept.String(), "a") != 12 || strings.Count(kept.String(), "b") != 12 {
		t.Errorf("the wrapped echo lost letters: %q", rows)
	}
	for _, r := range rows {
		if w := ansi.StringWidth(r); w > 30 {
			t.Errorf("an echo row is %d cells wide, over 30: %q", w, r)
		}
	}
}

// A prompt you let go of is named on the task card's WAITING FOR line,
// after what the task waits on, whatever its agent is doing (the factory's
// data puts it there: the AGENTS line draws a "now" only while an agent
// works).
func TestTheTaskCardNamesALetGoPromptOnItsWaitingLine(t *testing.T) {
	c := fixtureRetryCard()
	c.WaitingFor = "agent: finding the cause · let go: Find the cause of PR-12371 on your own."
	c.Agents = []CardAgent{{Name: "finding the cause", Why: "find the cause on its own, apart from the others", State: "done"}}
	for _, w := range []int{100, 140} {
		line := cardLineWith(t, plainCard(drawTask(c, 0), w), "WAITING FOR")
		if !strings.Contains(line, "let go: Find the cause") {
			t.Errorf("%d columns: waiting line %q lacks the let-go prompt", w, line)
		}
	}
}
