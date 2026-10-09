package tui

import (
	"errors"
	"strings"
	"testing"
)

// paneAttack is pane text that would act on the terminal if passed
// through: a clipboard write (OSC 52, BEL-terminated), a link (OSC 8,
// ST-terminated), a screen erase, a cursor move, a hidden cursor, a full
// reset, a C1 CSI as a byte and as a rune, a bell, a backspace, a vertical
// tab, a form feed and DEL, with styling and text around them.
const paneAttack = "\x1b[1;31mred\x1b[0m \x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\ " +
	"\x1b]52;c;aGk=\x07\x1b[2J\x1b[H\x1b[?25l\x1bc\x9b2J\u009b" + "a\x07b\x08c\x0bd\x0ce\x7ff\tg\r\nnext\x1b[3"

// The watch screen's plain mode drops every escape sequence and turns
// every control character but the newline into a space (a tab into four),
// line by line; the text around them stays.
func TestWatchPlainTextDropsEveryControl(t *testing.T) {
	got := watchContent(WatchFrame{Text: paneAttack})
	if want := "red link  a b c d e f    g\nnext"; got != want {
		t.Errorf("plain content = %q, want %q", got, want)
	}
	if got := watchContent(WatchFrame{Text: "a\x07b\x08c\x1b[0m"}); got != "a b c" {
		t.Errorf("plain content = %q", got)
	}
}

// The watch screen's ANSI mode keeps only the styling (SGR: CSI … m) and
// drops every other sequence: no clipboard write, link, erase, cursor move
// or reset reaches the terminal; controls are spaces as in the plain mode.
func TestWatchANSIKeepsOnlyStyling(t *testing.T) {
	got := watchContent(WatchFrame{Text: paneAttack, ANSI: true})
	if want := "\x1b[1;31mred\x1b[0m link  a b c d e f    g\nnext\x1b[0m"; got != want {
		t.Errorf("ANSI content = %q, want %q", got, want)
	}
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(WatchFrame{Title: "t\x1b]52;c;aGk=\x07", Role: "judge\x07", Pane: "p_1\x1b[2J", Status: "working\x08", Text: paneAttack, ANSI: true}))
	mustDrawSafely(t, "the watch screen", m.View().Content)
	m, _ = send(t, m, watchResultMsg{err: errors.New("herdr says \x1b]52;c;aGk=\x07\x1b[2J")})
	mustDrawSafely(t, "the watch screen's fetch error", m.View().Content)
	if strings.Contains(m.View().Content, "\x1bc") {
		t.Error("the watch screen passes a full reset")
	}
}

func TestIsSGR(t *testing.T) {
	for seq, want := range map[string]bool{
		"\x1b[m": true, "\x1b[0m": true, "\x1b[1;31m": true, "\x1b[38:2::255:0:0m": true,
		"\x1b[2J": false, "\x1b[?25l": false, "\x1b[>4;2m": false, "\x1b[ m": false, "\x9b31m": false, "\x1bm": false, "m": false,
	} {
		if got := isSGR(seq); got != want {
			t.Errorf("isSGR(%q) = %v, want %v", seq, got, want)
		}
	}
}
