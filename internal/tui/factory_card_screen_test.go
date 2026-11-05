package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The painter states the screen meets (Task 18's rulings 7, 9 and 13): a
// card on a short pane, the recap after the questions changed, a failed
// check on the ready card, a card whose conflict sent the task back, and
// the screen's own footer: a question, a typed line and a note on the rule.

// On a short pane the question card keeps the question, its options and the
// mark in view: the other questions, the example, the context and the
// blanks give way first.
func TestTheQuestionCardKeepsTheMarkedOptionOnAShortPane(t *testing.T) {
	for _, h := range []int{8, 10, 12, 14} {
		c := fixtureQuestionCard()
		c.Cursor = 4 // the last option: "Type your own answer"
		frame := ansi.Strip(drawQuestion(c)(100, h, true, IconsUnicode))
		lines := strings.Split(frame, "\n")
		if len(lines) != h {
			t.Fatalf("100x%d: %d lines:\n%s", h, len(lines), frame)
		}
		if !strings.Contains(frame, "› 5  Type your own answer") {
			t.Errorf("100x%d: the marked option is not in view:\n%s", h, frame)
		}
		if !strings.Contains(lines[len(lines)-1], "enter take the marked one") {
			t.Errorf("100x%d: the footer is not at the bottom: %q", h, lines[len(lines)-1])
		}
		if h >= 12 && !strings.Contains(frame, "When does the reminder go out?") {
			t.Errorf("100x%d: the question is not in view:\n%s", h, frame)
		}
	}
	// Tall enough, nothing gives way: the frame is the approved one.
	tall := ansi.Strip(drawQuestion(fixtureQuestionCard())(100, 0, true, IconsUnicode))
	for _, want := range []string{"✓ 1  Who gets the reminder?", "Example: a credit that expires", "› 1  Seven days"} {
		if !strings.Contains(tall, want) {
			t.Errorf("the full card lacks %q", want)
		}
	}
}

// Below about twelve rows the ready card's lists give way entirely, and the
// options with the mark stay above the rule: no height is never mistaken for
// "draw everything".
func TestTheReadyCardKeepsItsOptionsOnAVeryShortPane(t *testing.T) {
	for _, h := range []int{8, 9, 10, 11, 12} {
		c := fixtureReadyCardAfter()
		c.Ask, c.Cursor = "", 2
		frame := ansi.Strip(drawReady(c)(100, h, true, IconsUnicode))
		lines := strings.Split(frame, "\n")
		if len(lines) != h {
			t.Fatalf("100x%d: %d lines:\n%s", h, len(lines), frame)
		}
		if !strings.Contains(frame, "› 3  drop") {
			t.Errorf("100x%d: the marked option went below the rule:\n%s", h, frame)
		}
		if strings.Contains(frame, "CAUSE") && h < 12 {
			t.Errorf("100x%d: a list is drawn whole on a short pane:\n%s", h, frame)
		}
	}
}

// The recap after the factory refused the batch: the rule says the
// questions changed and that nothing was sent, then why, then the keys.
func TestTheRecapShowsTheQuestionsChanged(t *testing.T) {
	c := fixtureRecap()
	c.changed = &recapChange{at: cardAt(14, 20), why: "The agent finding the cause saw the coupon reminder uses 3 days, not 7.", again: 2}
	frame := ansi.Strip(drawRecap(c)(100, 0, true, IconsUnicode))
	lines := strings.Split(frame, "\n")
	n := len(lines)
	if !strings.Contains(lines[n-3], "the questions changed at 14:20 · nothing was sent") {
		t.Errorf("the rule reads %q", lines[n-3])
	}
	if !strings.Contains(lines[n-2], "uses 3 days, not 7") {
		t.Errorf("the why reads %q", lines[n-2])
	}
	if !strings.Contains(lines[n-1], "enter see question 2 again") || !strings.Contains(lines[n-1], "esc back") {
		t.Errorf("the keys read %q", lines[n-1])
	}
	if strings.Contains(frame, "y/N") {
		t.Errorf("the variant still asks y/N:\n%s", frame)
	}
	c.changed = &recapChange{}
	if frame := ansi.Strip(drawRecap(c)(100, 0, true, IconsUnicode)); !strings.Contains(frame, "the questions changed · nothing was sent") ||
		!strings.Contains(frame, "enter see the questions again") {
		t.Errorf("the variant without a time or a question:\n%s", frame)
	}
}

// An answer the factory already has is shown on the recap but neither sent
// nor counted: the y/N counts only the batch.
func TestTheRecapCountsOnlyTheBatch(t *testing.T) {
	c := fixtureRecap()
	c.Questions[0].sent = true
	frame := cardSquash(ansi.Strip(drawRecap(c)(100, 0, true, IconsUnicode)))
	for _, want := range []string{"advocates only", "Send 1 answer and copy 1 Jira comment for you to post? y/N", "the agents get answer 2"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the recap lacks %q:\n%s", want, frame)
		}
	}
}

// A failed check is drawn under PROOF behind a red ✗, and says so in text.
func TestTheReadyCardDrawsAFailedCheck(t *testing.T) {
	c := fixtureReadyCardAfter()
	c.Ask, c.Failed = "", []string{"rubocop: 2 offenses in app/services/campaign_copier.rb"}
	frame := drawReady(c)(100, 0, true, IconsUnicode)
	line := cardLineWith(t, ansi.Strip(frame), "rubocop: 2 offenses")
	if !strings.Contains(line, "✗ failed: rubocop: 2 offenses") {
		t.Errorf("the failed check reads %q", line)
	}
	if got := cardLineWith(t, frame, "failed: rubocop"); !strings.Contains(got, newStyles(true).Err.Render("✗")) {
		t.Errorf("the failed check's mark is not red: %q", got)
	}
	ascii := ansi.Strip(drawReady(c)(100, 0, true, IconsASCII))
	if !strings.Contains(ascii, "x failed: rubocop") {
		t.Errorf("ASCII mode draws the failure as %q", cardLineWith(t, ascii, "rubocop"))
	}
}

// A card whose conflict sent the task back does not offer push: the option
// says why, and the mark is never on it.
func TestAConflictCardDoesNotOfferPush(t *testing.T) {
	c := fixtureReadyCardAfter()
	c.Ask, c.NoPush = "", "the base conflicts in 2 files: the writer merges it"
	frame := ansi.Strip(drawReady(c)(100, 0, true, IconsUnicode))
	line := cardLineWith(t, frame, "1  push + PR")
	if !strings.Contains(line, "not offered: the base conflicts") {
		t.Errorf("the push option reads %q", line)
	}
	if strings.Contains(line, "›") || strings.Contains(line, "recommended") {
		t.Errorf("push is marked or recommended on a card that does not offer it: %q", line)
	}
}

// The task card asks its y/N (a retry, a drop) in its footer.
func TestTheTaskCardAsksInItsFooter(t *testing.T) {
	c := fixtureRetryCard()
	c.Ask = "Retry PR-12393: it goes on at its tests stage, which checks again what stopped it?"
	frame := ansi.Strip(drawTask(c, 0)(100, 30, true, IconsUnicode))
	if foot := readyFooter(frame); !strings.Contains(foot, "Retry PR-12393") || !strings.HasSuffix(foot, "y confirms, any other key cancels") &&
		!strings.HasSuffix(foot, "y/N") {
		t.Errorf("the task card's footer reads %q", foot)
	}
	if strings.Contains(frame, "j k move") {
		t.Errorf("the hints are still drawn under the question:\n%s", frame)
	}
}

// A card takes a typed line in its footer: what it is for, then the text
// behind the bar and the caret; the note on the rule is the screen's.
func TestACardTakesTypedTextInItsFooter(t *testing.T) {
	c := fixtureReadyCardAfter()
	c.Ask, c.Cursor = "", 1
	c.input = &cardInput{label: "Your note to the writer · enter sends it · esc drops it", text: "the copier spec misses archived rules"}
	frame := ansi.Strip(drawReady(c)(100, 30, true, IconsUnicode))
	lines := strings.Split(frame, "\n")
	n := len(lines)
	if !strings.Contains(lines[n-2], "Your note to the writer") || !strings.Contains(lines[n-1], "┃ the copier spec misses archived rules▏") {
		t.Errorf("the footer reads %q / %q", lines[n-2], lines[n-1])
	}
	q := fixtureQuestionCard()
	q.input = &cardInput{label: "Your answer · enter takes it · esc drops it", text: "two days before"}
	q.rule = newStyles(true).Dim.Render("question 2 flagged needless")
	frame = ansi.Strip(drawQuestion(q)(100, 30, true, IconsUnicode))
	lines = strings.Split(frame, "\n")
	n = len(lines)
	if !strings.Contains(lines[n-1], "two days before▏") || !strings.Contains(lines[n-3], "question 2 flagged needless") {
		t.Errorf("the question card's footer and rule read %q / %q", lines[n-3], lines[n-1])
	}
}

// The screen's note goes on a card's rule; the ready card's own ("you read
// the diff") gives way to it.
func TestACardCarriesTheScreensNoteOnItsRule(t *testing.T) {
	st := newStyles(true)
	c := fixtureReadyCardAfter()
	c.Ask, c.rule = "", st.Err.Render("Push failed: the remote head moved")
	frame := ansi.Strip(drawReady(c)(100, 0, true, IconsUnicode))
	if !strings.Contains(frame, "── Push failed: the remote head moved") || strings.Contains(frame, "you read the diff 14:21") {
		t.Errorf("the rule:\n%s", frame)
	}
	task := fixtureTaskCard()
	task.rule = st.Dim.Render("busy: Retry PR-12330…")
	if frame, _ := renderTaskCard(task, 100, 30, 0, st, newGlyphs(IconsUnicode), cardTaskNow); !strings.Contains(ansi.Strip(frame), "── busy: Retry PR-12330…") {
		t.Errorf("the task card's rule:\n%s", ansi.Strip(frame))
	}
	f := fixtureNewTaskPrompt()
	f.Ask, f.rule = "", st.Dim.Render("Queue bug: cancelled, nothing was run")
	if frame := ansi.Strip(drawNewTask(f)(100, 30, true, IconsUnicode)); !strings.Contains(frame, "── Queue bug: cancelled") {
		t.Errorf("the new task form's rule:\n%s", frame)
	}
	home := fixtureHome()
	home.rule = st.Warn.Render("wait: Open PR-12345's pane runs")
	if frame, _ := factoryFrameOf(home, 140, factoryHomeHeight, true, IconsUnicode, factoryNow); !strings.Contains(ansi.Strip(frame), "── wait: Open PR-12345's pane runs") ||
		strings.Contains(ansi.Strip(frame), "bart deploy resumed") {
		t.Errorf("home's rule:\n%s", ansi.Strip(frame))
	}
	empty := fixtureEmpty()
	empty.status = st.Dim.Render("loading the factory's tasks…")
	if frame, _ := factoryFrameOf(empty, 100, 20, true, IconsUnicode, factoryNow); !strings.Contains(ansi.Strip(frame), "loading the factory's tasks…") ||
		strings.Contains(ansi.Strip(frame), "No tasks yet") {
		t.Errorf("home while loading:\n%s", ansi.Strip(frame))
	}
}

// The spinner of a working stage turns with the frame the screen hands it.
func TestTheTaskCardSpinnerTurnsWithTheScreensFrame(t *testing.T) {
	c := fixtureTaskCard()
	frames := map[string]bool{}
	for i := range 4 {
		c.Spinner = i
		frames[cardLineWith(t, ansi.Strip(drawTask(c, 0)(140, 0, true, IconsUnicode)), "merging your latest head (bastion")] = true
	}
	if len(frames) < 2 {
		t.Errorf("the working stage's mark did not move over 4 frames: %v", frames)
	}
}
