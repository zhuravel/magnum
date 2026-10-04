package tui

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// keyMsg builds the key press a terminal sends for a key as Bubble Tea
// names it: "enter", "esc", "up", "pgdown", "ctrl+r", "space", "j", "R".
func keyMsg(s string) tea.KeyPressMsg {
	named := map[string]rune{
		"enter": tea.KeyEnter, "esc": tea.KeyEscape, "tab": tea.KeyTab, "backspace": tea.KeyBackspace,
		"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
		"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd,
	}
	if code, ok := named[s]; ok {
		return tea.KeyPressMsg{Code: code}
	}
	if s == "space" || s == " " {
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	}
	if c, ok := strings.CutPrefix(s, "ctrl+"); ok && len(c) == 1 {
		return tea.KeyPressMsg{Code: rune(c[0]), Mod: tea.ModCtrl}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

// viewOf is the plain text of a model's view (styling stripped).
func viewOf(m interface{ View() tea.View }) string { return ansi.Strip(m.View().Content) }

// send feeds msgs through m.Update in order and returns the final model and
// the command the last message returned.
func send[M tea.Model](t *testing.T, m M, msgs ...tea.Msg) (M, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, msg := range msgs {
		var next tea.Model
		next, cmd = m.Update(msg)
		nm, ok := next.(M)
		if !ok {
			t.Fatalf("Update returned %T, want %T", next, m)
		}
		m = nm
	}
	return m, cmd
}

// keys turns key names into messages for send.
func keys(names ...string) []tea.Msg {
	out := make([]tea.Msg, len(names))
	for i, n := range names {
		out[i] = keyMsg(n)
	}
	return out
}

// typed turns text into one rune key message per character.
func typed(s string) []tea.Msg {
	var out []tea.Msg
	for _, r := range s {
		out = append(out, keyMsg(string(r)))
	}
	return out
}

// execCmd runs cmd and returns the messages it produces, expanding batches.
// Commands still running after a short wait (tickers, timers), spinner
// ticks and terminal queries are dropped, so tests see only the immediate
// results.
func execCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(150 * time.Millisecond):
		return nil
	}
	if reflect.TypeOf(msg) == reflect.TypeOf(tea.RequestBackgroundColor()) {
		return nil // a query for the terminal, which tests do not have
	}
	switch m := msg.(type) {
	case nil, spinner.TickMsg:
		return nil
	case tea.BatchMsg:
		var out []tea.Msg
		for _, c := range m {
			out = append(out, execCmd(c)...)
		}
		return out
	}
	// tea.Sequence's message type is unexported; expand it by reflection.
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeOf(tea.Cmd(nil)) {
		var out []tea.Msg
		for i := range v.Len() {
			out = append(out, execCmd(v.Index(i).Interface().(tea.Cmd))...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// isQuit reports whether msgs hold tea.Quit's message.
func isQuit(msgs []tea.Msg) bool {
	for _, m := range msgs {
		if _, ok := m.(tea.QuitMsg); ok {
			return true
		}
	}
	return false
}

// mustContain fails unless every want string appears in view.
func mustContain(t *testing.T, view string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(view, w) && !containsSplitRef(view, w) {
			t.Errorf("view lacks %q:\n%s", w, view)
		}
	}
}

// containsSplitRef accepts a PR reference "repo#N" (or "owner/repo#N", or
// with a leading cursor mark) drawn as the board's two columns: the
// repository, then spaces, then "#N" on the same line.
func containsSplitRef(view, want string) bool {
	m := splitRefRe.FindStringSubmatch(want)
	if m == nil {
		return false
	}
	re := regexp.MustCompile(regexp.QuoteMeta(m[1]) + `\s+` + regexp.QuoteMeta(m[2]) + `(\s|$)`)
	return re.MatchString(view)
}

var splitRefRe = regexp.MustCompile(`^((?:[> ]\s*)?[\w./-]+)(#[0-9]+)$`)

// mustNotContain fails when any string appears in view.
func mustNotContain(t *testing.T, view string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(view, w) {
			t.Errorf("view has %q:\n%s", w, view)
		}
	}
}

// maxLineWidth is the widest line of view in cells.
func maxLineWidth(view string) int {
	w := 0
	for _, l := range strings.Split(view, "\n") {
		w = max(w, ansi.StringWidth(l))
	}
	return w
}
