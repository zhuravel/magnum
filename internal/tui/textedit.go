package tui

// The screens' text inputs: one set of editing keys, close to Claude Code's
// prompt, and one cursor, drawn in reverse video and never blinking. The
// inputs that draw themselves (the board's filter, the settings search,
// the cleanup input) take them through their component; the factory
// screen's painter draws its own (typedRows, typedLineCell) the same way.

import (
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// editKeys is the one set of editing keys of every text input. A paste
// comes from the terminal only (tea.PasteMsg), so no key reads the
// clipboard (the components' ctrl+v); no key selects; a screen's own keys
// come first (its key handler takes them before the input).
var editKeys = struct {
	charLeft, charRight, wordLeft, wordRight, lineStart, lineEnd   key.Binding
	delBack, delFwd, delWordBack, delWordFwd, delToStart, delToEnd key.Binding
	lineUp, lineDown, textStart, textEnd, newLine                  key.Binding
}{
	charLeft:    key.NewBinding(key.WithKeys("left", "ctrl+b")),
	charRight:   key.NewBinding(key.WithKeys("right", "ctrl+f")),
	wordLeft:    key.NewBinding(key.WithKeys("alt+left", "alt+b", "ctrl+left")),
	wordRight:   key.NewBinding(key.WithKeys("alt+right", "alt+f", "ctrl+right")),
	lineStart:   key.NewBinding(key.WithKeys("home", "ctrl+a")),
	lineEnd:     key.NewBinding(key.WithKeys("end", "ctrl+e")),
	delBack:     key.NewBinding(key.WithKeys("backspace", "ctrl+h")),
	delFwd:      key.NewBinding(key.WithKeys("delete", "ctrl+d")),
	delWordBack: key.NewBinding(key.WithKeys("ctrl+w", "alt+backspace", "ctrl+backspace")),
	delWordFwd:  key.NewBinding(key.WithKeys("alt+d", "alt+delete", "ctrl+delete")),
	delToStart:  key.NewBinding(key.WithKeys("ctrl+u")),
	delToEnd:    key.NewBinding(key.WithKeys("ctrl+k")),
	// A text's own: its lines, its ends and a new line (enter is the
	// screen's: it starts).
	lineUp:    key.NewBinding(key.WithKeys("up", "ctrl+p")),
	lineDown:  key.NewBinding(key.WithKeys("down", "ctrl+n")),
	textStart: key.NewBinding(key.WithKeys("alt+<", "ctrl+home")),
	textEnd:   key.NewBinding(key.WithKeys("alt+>", "ctrl+end")),
	newLine:   key.NewBinding(key.WithKeys("ctrl+j", "shift+enter")),
}

// newLineInput is a one-line input with the shared keys and cursor, of at
// most limit runes (0: no limit); its screen focuses it. Every binding the
// set does not name (the paste, the suggestions) is left unset, so off.
func newLineInput(limit int) textinput.Model {
	in := textinput.New()
	k := editKeys
	in.KeyMap = textinput.KeyMap{
		CharacterForward: k.charRight, CharacterBackward: k.charLeft, WordForward: k.wordRight, WordBackward: k.wordLeft,
		DeleteWordBackward: k.delWordBack, DeleteWordForward: k.delWordFwd, DeleteAfterCursor: k.delToEnd,
		DeleteBeforeCursor: k.delToStart, DeleteCharacterBackward: k.delBack, DeleteCharacterForward: k.delFwd,
		LineStart: k.lineStart, LineEnd: k.lineEnd,
	}
	in.CharLimit = limit
	in.SetStyles(lineStyles(defaultStyles))
	return in
}

// lineStyles are the components' default styles for st's background with
// the shared cursor: a static block, the character under it in reverse
// video and no color of its own, as cursorCell draws it.
func lineStyles(st styles) textinput.Styles {
	s := textinput.DefaultStyles(st.dark)
	s.Cursor = textinput.CursorStyle{Shape: s.Cursor.Shape}
	return s
}

// newTextInput is a text of several lines with the shared keys, focused.
// Its painter draws it (inputRows): the component's own view, cursor and
// blink are never shown.
func newTextInput() textarea.Model {
	area := textarea.New()
	k := editKeys
	area.KeyMap = textarea.KeyMap{
		CharacterForward: k.charRight, CharacterBackward: k.charLeft, WordForward: k.wordRight, WordBackward: k.wordLeft,
		DeleteWordBackward: k.delWordBack, DeleteWordForward: k.delWordFwd, DeleteAfterCursor: k.delToEnd,
		DeleteBeforeCursor: k.delToStart, DeleteCharacterBackward: k.delBack, DeleteCharacterForward: k.delFwd,
		LineStart: k.lineStart, LineEnd: k.lineEnd, LineNext: k.lineDown, LinePrevious: k.lineUp,
		InputBegin: k.textStart, InputEnd: k.textEnd, InsertNewline: k.newLine,
	}
	s := area.Styles()
	s.Cursor = textarea.CursorStyle{Shape: s.Cursor.Shape}
	area.SetStyles(s)
	area.CharLimit, area.ShowLineNumbers = 0, false
	_ = area.Focus()
	return area
}

// cursorLook is the cursor: the cell under it in reverse video.
var cursorLook = lipgloss.NewStyle().Reverse(true)

// typedText is a line or a text being typed and its cursor: after is how
// many of its runes follow the cursor (0: the cursor is at its end).
type typedText struct {
	text  string
	after int
}

// lineTyped is what in holds and where its cursor is.
func lineTyped(in textinput.Model) typedText {
	return typedText{in.Value(), utf8.RuneCountInString(in.Value()) - in.Position()}
}

// pos is the rune offset of the cursor in the text.
func (t typedText) pos() int {
	n := utf8.RuneCountInString(t.text)
	return n - max(0, min(t.after, n))
}

// cleaned is t with clean applied to the text on either side of the
// cursor, so the cursor stays between the same characters and its column
// counts the cleaned text's cells.
func (t typedText) cleaned(clean func(string) string) typedText {
	r := []rune(t.text)
	pos := t.pos()
	after := clean(string(r[pos:]))
	return typedText{clean(string(r[:pos])) + after, utf8.RuneCountInString(after)}
}

// textCursor is the rune offset of area's cursor in its value.
func textCursor(area textarea.Model) int {
	pos := 0
	for i, l := range strings.Split(area.Value(), "\n") {
		if i == area.Line() {
			return pos + min(area.Column(), utf8.RuneCountInString(l))
		}
		pos += utf8.RuneCountInString(l) + 1
	}
	return pos
}

// setTextCursor puts area's cursor at rune offset pos of its value: the
// text after it goes in first, then the text before it at the start,
// which leaves the cursor between them.
func setTextCursor(area *textarea.Model, pos int) {
	r := []rune(area.Value())
	pos = max(0, min(pos, len(r)))
	area.Reset()
	area.InsertString(string(r[pos:]))
	area.MoveToBegin()
	area.InsertString(string(r[:pos]))
}

// cluster is one grapheme of typed text: what is drawn in w cells (a
// combining mark takes none of its own, a wide rune two), from rune at.
type cluster struct {
	text  string
	w, at int
}

// clusters splits s into its graphemes, from rune offset at on.
func clusters(s string, at int) []cluster {
	var out []cluster
	for s != "" {
		c, w := ansi.FirstGraphemeCluster(s, ansi.GraphemeWidth)
		if c == "" { // never: a cluster takes at least a byte
			break
		}
		out = append(out, cluster{c, w, at})
		at += utf8.RuneCountInString(c)
		s = s[len(c):]
	}
	return out
}

// cursorIn is the index of the grapheme of cs that the cursor at rune pos
// is on: the one that starts there or holds it (between a letter and its
// combining mark the cursor is on the letter); len(cs) past their end.
func cursorIn(cs []cluster, pos int) int {
	for i, c := range cs {
		if pos < c.at+utf8.RuneCountInString(c.text) {
			return i
		}
	}
	return len(cs)
}

// clusterText is the text of cs.
func clusterText(cs []cluster) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(c.text)
	}
	return b.String()
}

// cursorCell is cs drawn in st with the cursor on cs[cur] in reverse
// video, or after them as a reverse-video space when cur is len(cs).
func cursorCell(cs []cluster, cur int, st lipgloss.Style) cell {
	if cur >= len(cs) {
		return cell{{clusterText(cs), st}, {" ", cursorLook}}
	}
	return cell{{clusterText(cs[:cur]), st}, {cs[cur].text, cursorLook}, {clusterText(cs[cur+1:]), st}}
}

// typedRow is one row of a text as the painter draws it: its graphemes,
// the rune offsets it starts and ends at, and last when it ends its line
// (the cursor at the line's end is drawn after it, never at the start of
// the next row).
type typedRow struct {
	cs         []cluster
	start, end int
	last       bool
}

// typedRows lays text out in rows of at most w cells as the painter draws
// it: a line at a time, a line wider than w broken at w (ansi.Hardwrap),
// not at a word, so every space stays where it was typed.
func typedRows(text string, w int) []typedRow {
	var rows []typedRow
	at := 0
	for l := range strings.SplitSeq(text, "\n") {
		parts := []string{l}
		if ansi.StringWidth(l) > w {
			parts = strings.Split(ansi.Hardwrap(l, w, true), "\n")
		}
		for i, part := range parts {
			n := utf8.RuneCountInString(part)
			rows = append(rows, typedRow{clusters(part, at), at, at + n, i == len(parts)-1})
			at += n
		}
		at++ // the line break
	}
	return rows
}

// cursorRowAt is the row of rows the cursor at rune pos is on, and its
// cell column there.
func cursorRowAt(rows []typedRow, pos int) (row, col int) {
	row = len(rows) - 1
	for i, r := range rows {
		if pos < r.end || (pos == r.end && r.last) {
			row = i
			break
		}
	}
	r := rows[row]
	for _, c := range r.cs[:cursorIn(r.cs, pos)] {
		col += c.w
	}
	return row, col
}

// offsetAt is the rune offset of the cursor put on row r at cell column
// col, or as near it as the row allows: on the grapheme that covers the
// column, else at the row's end (on its last grapheme when its line goes
// on in the next row).
func (r typedRow) offsetAt(col int) int {
	x := 0
	for _, c := range r.cs {
		if col < x+c.w {
			return c.at
		}
		x += c.w
	}
	if !r.last && len(r.cs) > 0 {
		return r.cs[len(r.cs)-1].at
	}
	return r.end
}

// rowWindow is the first of n rows of total to draw so that row cur is in
// view: the last n when it is among them (typing at the end shows the
// last lines), else the first n when it is among those, else cur near the
// middle.
func rowWindow(total, cur, n int) int {
	switch {
	case total <= n || cur >= total-n:
		return max(total-n, 0)
	case cur < n:
		return 0
	}
	return cur - n/2
}

// typedLineCell is a typed line drawn in st with its cursor, at most w
// cells: a wider line scrolls sideways to keep the cursor in view, as
// rowWindow does with a text's rows: its end while the cursor is there,
// else its start, else the cursor near the middle.
func typedLineCell(t typedText, w int, st lipgloss.Style) cell {
	cs := clusters(t.text, 0)
	pos := t.pos()
	cur := cursorIn(cs, pos)
	at, total, under := 0, 0, 1 // the cursor's own cell after the text
	for i, c := range cs {
		if i < cur {
			at += c.w
		}
		total += c.w
	}
	if cur < len(cs) {
		under = cs[cur].w
	} else {
		total++
	}
	from := 0
	switch {
	case total <= w:
	case at >= total-w:
		from = total - w
	case at+under <= w:
	default:
		from = at - w/2
	}
	var shown []cluster
	x := 0
	for _, c := range cs {
		if x >= from && x+c.w <= from+w {
			shown = append(shown, c)
		}
		x += c.w
	}
	return cursorCell(shown, cursorIn(shown, pos), st)
}

// inputTextW is the width of the new task's text on a screen width cells
// wide: the bar and its gaps take three cells, and the cursor the one
// after a row that fills it.
func inputTextW(width int) int { return max(width-4, 8) }
