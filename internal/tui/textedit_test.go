package tui

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The screens' text inputs: one set of editing keys, close to Claude Code's
// prompt, and one cursor, drawn in reverse video and never blinking.

// reverseRun is a run of text drawn in reverse video alone: the cursor.
var reverseRun = regexp.MustCompile("\x1b\\[7m([^\x1b]*)\x1b\\[m")

// cursorMarked is frame without its colors, the cell the cursor is on
// between ⟦ and ⟧.
func cursorMarked(frame string) string {
	return ansi.Strip(reverseRun.ReplaceAllString(frame, "⟦$1⟧"))
}

// marked is the screen's frame with its cursor marked (cursorMarked).
func (h *factoryHarness) marked() string { return cursorMarked(h.m.View().Content) }

// In the new task's text the cursor moves: left three times and a typed
// word go in there; alt+b and alt+f move a word, ctrl+a and ctrl+e to the
// line's ends, up and down between its lines; ctrl+j and shift+enter make
// a new line and enter still starts.
func TestTheNewTasksTextMovesItsCursorWithTheEditingKeys(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.press("n")
	h.feed(typed("one two three")...)
	h.press("left", "left", "left", "left", "left")
	h.feed(typed("and ")...)
	mustContain(t, h.marked(), "┃ one two and ⟦t⟧hree")
	h.press("alt+b")
	mustContain(t, h.marked(), "┃ one two ⟦a⟧nd three")
	h.press("alt+f")
	mustContain(t, h.marked(), "┃ one two and⟦ ⟧three")
	h.press("ctrl+a")
	mustContain(t, h.marked(), "┃ ⟦o⟧ne two and three")
	h.press("ctrl+e")
	mustContain(t, h.marked(), "┃ one two and three⟦ ⟧")
	h.press("ctrl+j")
	h.feed(typed("four")...)
	h.press("shift+enter")
	h.feed(typed("fives")...)
	h.press("up")
	mustContain(t, h.marked(), "┃ four⟦ ⟧", "┃ fives")
	h.press("up")
	mustContain(t, h.marked(), "┃ one t⟦w⟧o and three")
	h.press("down", "down")
	mustContain(t, h.marked(), "┃ fives⟦ ⟧")
	h.press("ctrl+p", "home")
	h.feed(typed("> ")...)
	mustContain(t, h.marked(), "┃ > ⟦f⟧our")
	h.press("alt+<")
	mustContain(t, h.marked(), "┃ ⟦o⟧ne two")
	h.press("alt+>")
	mustContain(t, h.marked(), "┃ fives⟦ ⟧")
	if got, want := h.m.form.area.Value(), "one two and three\n> four\nfives"; got != want {
		t.Errorf("the text is %q, want %q", got, want)
	}
	h.press("enter")
	if got := h.m.form.area.Value(); strings.Count(got, "\n") != 2 {
		t.Errorf("enter made a new line (%q): it starts the task", got)
	}
}

// The delete keys of the shared set: backspace and delete (ctrl+d) a
// character, ctrl+w and alt+backspace the word before, alt+d the word
// after, ctrl+u and ctrl+k to the line's start and end.
func TestTheSharedKeysDeleteAsClaudeCodesPromptDoes(t *testing.T) {
	for _, c := range []struct {
		keys []string
		want string
	}{
		{[]string{"backspace"}, "alpha beta⟦g⟧amma"},
		{[]string{"delete"}, "alpha beta ⟦a⟧mma"},
		{[]string{"ctrl+d"}, "alpha beta ⟦a⟧mma"},
		{[]string{"ctrl+w"}, "alpha ⟦g⟧amma"},
		{[]string{"alt+backspace"}, "alpha ⟦g⟧amma"},
		{[]string{"alt+d"}, "alpha beta ⟦ ⟧"},
		{[]string{"ctrl+u"}, "┃ ⟦g⟧amma"},
		{[]string{"ctrl+k"}, "alpha beta ⟦ ⟧"},
	} {
		h := testFactory(t, 100, 40)
		h.press("n")
		h.feed(typed("alpha beta gamma")...)
		h.press("alt+b")
		h.press(c.keys...)
		if v := h.marked(); !strings.Contains(v, c.want) {
			t.Errorf("%v: the text reads %q, want %q", c.keys, cardLineWith(t, v, "┃"), c.want)
		}
	}
}

// The painter draws the cursor on the cell it is on, in reverse video, a
// wide rune before it taking two cells, and at the end of the text a
// reverse-video space.
func TestTheNewTasksPainterDrawsTheCursorWhereItIs(t *testing.T) {
	st, g := cardKit(true, IconsUnicode)
	frame := renderNewTask(NewTaskForm{Text: "日本 x y", after: 3}, 100, 0, st, g, factoryNow)
	line := cardLineWith(t, frame, "┃")
	if !strings.Contains(line, "\x1b[7mx\x1b[m") {
		t.Fatalf("the x under the cursor is not in reverse video: %q", line)
	}
	before, _, _ := strings.Cut(line, "\x1b[7m")
	if got := ansi.StringWidth(before); got != 3+5 {
		t.Errorf("the cursor is at cell %d, want 8: the bar, a space, 日 and 本 two cells each, a space", got)
	}
	frame = renderNewTask(NewTaskForm{Text: "日本 x y"}, 100, 0, st, g, factoryNow)
	mustContain(t, cursorMarked(frame), "┃ 日本 x y⟦ ⟧")
	mustNotContain(t, ansi.Strip(frame), "▏")
	// A combining mark takes no cell: the cursor after é is on the next one.
	frame = renderNewTask(NewTaskForm{Text: "éxy", after: 2}, 100, 0, st, g, factoryNow)
	mustContain(t, cursorMarked(frame), "┃ é⟦x⟧y")
	// A cursor between a letter and its combining mark is on the letter.
	frame = renderNewTask(NewTaskForm{Text: "éxy", after: 3}, 100, 0, st, g, factoryNow)
	mustContain(t, cursorMarked(frame), "┃ ⟦é⟧xy")
}

// A text longer than the 6 lines drawn shows the lines around the cursor:
// the first ones when it is on the first line, the last ones when it is at
// the end, as before.
func TestALongTextShowsTheLinesAroundTheCursor(t *testing.T) {
	st, g := cardKit(true, IconsUnicode)
	text := "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nline 7\nline 8\nline 9"
	rows := func(after int) []string {
		var out []string
		for l := range strings.SplitSeq(cursorMarked(renderNewTask(NewTaskForm{Text: text, after: after}, 100, 0, st, g, factoryNow)), "\n") {
			if strings.HasPrefix(l, " ┃") {
				out = append(out, strings.TrimPrefix(l, " ┃ "))
			}
		}
		return out
	}
	n := len([]rune(text))
	if got, want := rows(n), []string{"⟦l⟧ine 1", "line 2", "line 3", "line 4", "line 5", "line 6"}; !slices.Equal(got, want) {
		t.Errorf("the cursor on the first line shows %q, want %q", got, want)
	}
	if got, want := rows(0), []string{"line 4", "line 5", "line 6", "line 7", "line 8", "line 9⟦ ⟧"}; !slices.Equal(got, want) {
		t.Errorf("the cursor at the end shows %q, want %q", got, want)
	}
	before5, _, _ := strings.Cut(text, "line 5")
	if got := rows(n - len([]rune(before5))); !slices.Contains(got, "⟦l⟧ine 5") || len(got) != cardInputLines {
		t.Errorf("the cursor on line 5 shows %q, want its line among 6", got)
	}
	// A line wider than the screen wraps at the width: the cursor on its
	// second row is drawn there.
	long := strings.Repeat("abcdefghij", 5)
	frame := cursorMarked(renderNewTask(NewTaskForm{Text: long, after: 15}, 30, 0, st, g, factoryNow))
	if got := strings.Count(frame, "⟦"); got != 1 {
		t.Fatalf("the cursor is drawn %d times:\n%s", got, frame)
	}
	mustContain(t, frame, "⟦f⟧ghij")
}

// A paste goes in at the cursor, not at the end.
func TestAPasteGoesInAtTheCursor(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.press("n")
	h.feed(typed("abc")...)
	h.press("left", "left")
	h.feed(tea.PasteMsg{Content: "XY"})
	mustContain(t, h.marked(), "┃ aXY⟦b⟧c")
	h.press("esc", "esc")
	h.open("PR-12345")
	h.press("5") // type your own answer
	h.feed(typed("ab")...)
	h.press("left")
	h.feed(tea.PasteMsg{Content: "XY"})
	mustContain(t, h.marked(), "┃ aXY⟦b⟧")
}

// The answer line and home's filter move their cursor with left and right;
// the filter's up and down (and ctrl+p, ctrl+n) still move the selected row.
func TestTheAnswerLineAndHomesFilterMoveTheirCursor(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.open("PR-12345")
	h.press("5")
	h.feed(typed("ab")...)
	h.press("left")
	h.feed(typed("X")...)
	mustContain(t, h.marked(), "┃ aX⟦b⟧")
	h.press("right")
	mustContain(t, h.marked(), "┃ aXb⟦ ⟧")
	h.press("home")
	mustContain(t, h.marked(), "┃ ⟦a⟧Xb")
	mustNotContain(t, h.view(), "▏")

	h = testRepos(t)
	h.press("g")
	h.press("/")
	h.typeText("COUPON")
	h.press("left", "left")
	mustContain(t, h.marked(), "/ COUP⟦O⟧N")
	h.press("ctrl+a")
	h.typeText("x")
	if got := h.m.filter.Value(); got != "xCOUPON" {
		t.Errorf("the filter is %q, want xCOUPON: the x goes at the cursor", got)
	}
	h.press("backspace")
	first, _ := h.m.selectedRow()
	h.press("down")
	if r, _ := h.m.selectedRow(); r.Key == first.Key {
		t.Errorf("down in the filter left the cursor on %s", r.Key)
	}
	h.press("ctrl+p")
	if r, _ := h.m.selectedRow(); r.Key != first.Key {
		t.Errorf("ctrl+p in the filter moved to %s, want %s back", r.Key, first.Key)
	}
	if got := h.m.filter.Value(); got != "COUPON" || !h.m.filtering {
		t.Errorf("the rows' keys changed the filter: %q, filtering %v", got, h.m.filtering)
	}
}

// A typed line wider than its footer scrolls sideways to keep the cursor in
// view: the end while it is there, else the start, else the cursor near the
// middle.
func TestALongTypedLineScrollsSidewaysToTheCursor(t *testing.T) {
	long := "start " + strings.Repeat("middle ", 30) + "end"
	frame := func(after int) string {
		q := fixtureQuestionCard()
		q.input = &cardInput{label: "Your answer", text: long, after: after}
		lines := strings.Split(drawQuestion(q)(60, 30, true, IconsUnicode), "\n")
		return lines[len(lines)-1]
	}
	draw := func(after int) string { return cursorMarked(frame(after)) }
	if got := draw(0); !strings.HasSuffix(got, "end⟦ ⟧") || strings.Contains(got, "start") {
		t.Errorf("the cursor at the end shows %q, want the end of the line", got)
	}
	if got := draw(len(long)); !strings.Contains(got, "┃ ⟦s⟧tart middle") {
		t.Errorf("the cursor at the start shows %q, want the start of the line", got)
	}
	mid := draw(len(long) / 2)
	if !strings.Contains(mid, "⟦") || strings.Contains(mid, "start") || strings.Contains(mid, "end") {
		t.Errorf("the cursor in the middle shows %q, want the middle around it", mid)
	}
	for _, after := range []int{0, 10, len(long) / 2, len(long) - 10, len(long)} {
		if w := ansi.StringWidth(frame(after)); w > 60 {
			t.Errorf("the line with %d runes after the cursor is %d cells wide, over 60", after, w)
		}
	}
}

// A frame changes when only the cursor moves: the cached frame would hide
// the move.
func TestAFrameChangesWhenOnlyTheCursorMoves(t *testing.T) {
	h := testFactory(t, 100, 40)
	h.press("n")
	h.feed(typed("abc")...)
	key := h.m.frameKey()
	h.press("left")
	if h.m.frameKey() == key {
		t.Error("the new task's frame key is the same after left")
	}
	mustContain(t, h.marked(), "┃ ab⟦c⟧")
	h.press("esc", "esc")
	h.open("PR-12345")
	h.press("5")
	h.feed(typed("abc")...)
	key = h.m.frameKey()
	h.press("left")
	if h.m.frameKey() == key {
		t.Error("the answer line's frame key is the same after left")
	}
	mustContain(t, h.marked(), "┃ ab⟦c⟧")
	h.press("esc", "esc", "esc")
	h.press("/")
	h.typeText("abc")
	key = h.m.frameKey()
	h.press("left")
	if h.m.frameKey() == key {
		t.Error("home's filter's frame key is the same after left")
	}
	mustContain(t, h.marked(), "/ ab⟦c⟧")

	b, _, _ := newBoard(t, 160, 24, PRBoardOptions{})
	b, _ = send(t, b, append(keys("/"), typed("oauth")...)...)
	bkey := b.frameKey()
	b, _ = send(t, b, keyMsg("left"))
	if b.frameKey() == bkey {
		t.Error("the board's frame key is the same after left in its filter")
	}
	mustContain(t, cursorMarked(b.View().Content), "/ oaut⟦h⟧")
}

// The board's filter, the settings search and the cleanup input take the
// shared keys and cursor: ctrl+v reads nothing (a paste comes from the
// terminal), the cursor never blinks, and it is drawn as the factory draws
// it. The board's filter keeps its own keys for the rows: up and down,
// ctrl+p and ctrl+n, ctrl+k and ctrl+j.
func TestTheBoardSettingsAndCleanupInputsTakeTheSharedKeys(t *testing.T) {
	b, _, _ := newBoard(t, 160, 24, PRBoardOptions{})
	b, cmd := send(t, b, keyMsg("/"))
	if cmd != nil {
		t.Error("/ on the board asks for a command: the cursor's blink")
	}
	b, _ = send(t, b, typed("rails")...)
	if _, cmd = send(t, b, keyMsg("ctrl+v")); cmd != nil {
		t.Error("ctrl+v in the board's filter asks for a command: a read of the clipboard")
	}
	b, _ = send(t, b, keyMsg("ctrl+a"), keyMsg("alt+f"))
	mustContain(t, cursorMarked(b.View().Content), "/ rails⟦ ⟧")
	b, _ = send(t, b, keyMsg("alt+b"), keyMsg("right"))
	mustContain(t, cursorMarked(b.View().Content), "/ r⟦a⟧ils")
	if b.filter.Styles().Cursor.Blink {
		t.Error("the board's filter blinks")
	}
	b, _ = send(t, b, keyMsg("esc"), keyMsg("/"))
	at := b.cursor
	b, _ = send(t, b, keyMsg("ctrl+j"))
	if b.cursor != at+1 {
		t.Errorf("ctrl+j in the board's filter moved the row cursor to %d, want %d", b.cursor, at+1)
	}
	b, _ = send(t, b, keyMsg("ctrl+k"))
	if b.cursor != at {
		t.Errorf("ctrl+k in the board's filter moved the row cursor to %d, want %d", b.cursor, at)
	}

	s := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	s = press(t, s, "/")
	s = typeIn(t, s, "park")
	if _, cmd := s.Update(keyMsg("ctrl+v")); cmd != nil {
		t.Error("ctrl+v in the settings search asks for a command: a read of the clipboard")
	}
	s = press(t, s, "left", "left")
	mustContain(t, cursorMarked(s.View().Content), "/ pa⟦r⟧k")
	if s.search.Styles().Cursor.Blink {
		t.Error("the settings search blinks")
	}

	c := newCleanupTest(t, typedFixture(), 100, 30)
	c, cmd = send(t, c, keys("enter")...)
	if cmd != nil {
		t.Error("the cleanup's typed confirm asks for a command: the cursor's blink")
	}
	c, _ = send(t, c, typed("review")...)
	if _, cmd = send(t, c, keyMsg("ctrl+v")); cmd != nil {
		t.Error("ctrl+v in the cleanup input asks for a command: a read of the clipboard")
	}
	c, _ = send(t, c, keyMsg("alt+b"))
	mustContain(t, cursorMarked(c.View().Content), "> ⟦r⟧eview")
	if c.input.Styles().Cursor.Blink {
		t.Error("the cleanup input blinks")
	}
}
