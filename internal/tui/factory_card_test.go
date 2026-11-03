package tui

import (
	"fmt"
	"slices"
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
		if !strings.Contains(b, "You have not read the diff yet. The recommendation shows after d.") {
			t.Errorf("%d: the card does not say the diff is unread:\n%s", w, b)
		}
		a := plainCard(after, w)
		if marked := cardLineWith(t, a, "›"); !strings.Contains(marked, "push + draft PR") || !strings.Contains(marked, "recommended") {
			t.Errorf("%d: the marked line after the diff is %q", w, marked)
		}
		for _, want := range []string{"   why: the spec proves the fix; the file outside the plan is test data for it",
			"── you read the diff 14:21 ─", "Push zhuravel-PR-12360-activity-points-on-copy and open a draft PR? y/N"} {
			if !strings.Contains(a, want) {
				t.Errorf("%d: no %q after the diff in\n%s", w, want, a)
			}
		}
		if strings.Contains(a, "You have not read the diff yet") {
			t.Errorf("%d: the unread notice stays after the diff was opened:\n%s", w, a)
		}
	}
}

func TestReadyCardFlagsFilesNotInThePlan(t *testing.T) {
	d := drawReady(fixtureReadyCard())
	frame := d(100, 0, true, IconsUnicode)
	runs := warnRuns(t, frame)
	if want := []string{"not in the plan"}; !slices.Equal(runs, want) {
		t.Errorf("yellow runs = %q, want only %q", runs, want)
	}
	plain := ansi.Strip(frame)
	if l := cardLineWith(t, plain, "reward_rules.rb"); !strings.HasSuffix(l, "not in the plan") {
		t.Errorf("the file outside the plan is not flagged: %q", l)
	}
	for _, f := range []string{"campaign_copier.rb", "campaign_copier_spec.rb"} {
		if l := cardLineWith(t, plain, f); !strings.HasSuffix(l, "in the plan") || strings.HasSuffix(l, "not in the plan") {
			t.Errorf("%s is flagged wrongly: %q", f, l)
		}
	}
	for _, want := range []string{"+31 −6", "+15 ", "+2 "} {
		if !strings.Contains(plain, want) {
			t.Errorf("no counts %q in\n%s", want, plain)
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

// cardCases are every card the fixtures draw.
func cardCases() map[string]cardDraw {
	q := fixtureQuestionCard()
	empty := QuestionCard{TaskKey: "PR-12345"}
	return map[string]cardDraw{
		"question": drawQuestion(q), "question-noreco": drawQuestion(fixtureNoRecommendationCard()),
		"question-empty": drawQuestion(empty), "recap": drawRecap(fixtureRecap()), "recap-empty": drawRecap(empty),
		"ready-before": drawReady(fixtureReadyCard()), "ready-after": drawReady(fixtureReadyCardAfter()),
		"ready-bare": drawReady(ReadyCard{TaskKey: "PR-12360"}),
		"task":       drawTask(fixtureTaskCard(), 0), "task-scrolled": drawTask(fixtureTaskCard(), 9),
		"task-bare": drawTask(TaskCard{Key: "PR-12330"}, 0),
		"new-jira":  drawNewTask(fixtureNewTaskJira(factoryNow)), "new-prompt": drawNewTask(fixtureNewTaskPrompt()),
		"new-empty": drawNewTask(NewTaskForm{}),
		"new-unknown": drawNewTask(NewTaskForm{Text: "PR-12399 the reminder", Err: "PR-12399 is not in Jira",
			Ask: "Start from your words only?"}),
	}
}

func TestFactoryCardsNeverOverflow(t *testing.T) {
	for name, d := range cardCases() {
		for _, mode := range []IconMode{IconsUnicode, IconsNerd, IconsASCII} {
			for w := 60; w <= 200; w += 7 {
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
		if rule := lines[len(lines)-2]; !strings.HasPrefix(rule, "──") {
			t.Errorf("%s: the line above the footer is not the rule: %q", name, rule)
		}
	}
}

func TestFactoryCardsCountWhatTheirHeightCutsOff(t *testing.T) {
	for _, name := range []string{"question", "ready-before", "new-jira"} {
		frame := ansi.Strip(cardCases()[name](100, 12, true, IconsUnicode))
		lines := strings.Split(frame, "\n")
		if len(lines) != 12 {
			t.Fatalf("%s: %d lines, want 12:\n%s", name, len(lines), frame)
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
		{"ready-after", drawReady(fixtureReadyCardAfter()), 100},
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
	for _, r := range warnRuns(t, ready) {
		if r != "not in the plan" && !strings.HasPrefix(r, "Push zhuravel") {
			t.Errorf("the ready card is yellow on %q: only the file outside the plan and the question are on the operator", r)
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
	tc.Agents = append(tc.Agents, CardAgent{Name: "asks you", Why: "needs a login", State: "waits on you"})
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
