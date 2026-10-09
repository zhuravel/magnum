package tui

import (
	"context"
	"io"
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

// instantTick is the Tick of the screens tests build: its command answers
// at once with the timer's message, so a test that runs it sees what the
// timer sends, and execCmd drops it unrun like a timer still running.
func instantTick(_ time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { return fn(time.Now()) }
}

// neverTick is the Tick of a test that waits for something else: the timer
// never fires, so the screen asks for no further fetch or refresh.
func neverTick(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }

// scriptedInput makes the next program run with a pipe for its input and
// no output, and returns the pipe's writer. The test writes the keys it
// wants the program to read; a write waits until the program reads it, and
// fails once the pipe is closed. It swaps the package variable
// extraProgramOptions, so its tests stay serial. The cleanup restores the
// options and closes the pipe.
func scriptedInput(t *testing.T) *io.PipeWriter {
	t.Helper()
	r, w := io.Pipe()
	old := extraProgramOptions
	extraProgramOptions = []tea.ProgramOption{tea.WithInput(r), tea.WithOutput(io.Discard), tea.WithoutRenderer()}
	t.Cleanup(func() {
		extraProgramOptions = old
		r.Close()
		w.Close()
	})
	return w
}

// isInstantTick reports whether cmd is a timer instantTick scheduled: every
// command it returns is the same function literal.
func isInstantTick(cmd tea.Cmd) bool {
	return reflect.ValueOf(cmd).Pointer() == reflect.ValueOf(instantTick(0, nil)).Pointer()
}

// testDashboard, testPRBoard and testWatch build a screen as its Run
// function does, with instantTick for timers unless opts set a Tick.
func testDashboard(ctx context.Context, src DashboardSource, act DashboardActions, opts DashboardOptions) dashboardModel {
	if opts.Tick == nil {
		opts.Tick = instantTick
	}
	return newDashboardModel(ctx, src, act, opts)
}

func testPRBoard(ctx context.Context, src PRBoardSource, act DashboardActions, opts PRBoardOptions) prBoardModel {
	if opts.Tick == nil {
		opts.Tick = instantTick
	}
	return newPRBoardModel(ctx, src, act, opts)
}

func testWatch(ctx context.Context, fetch WatchFetch, opts WatchOptions) watchModel {
	if opts.Tick == nil {
		opts.Tick = instantTick
	}
	return newWatchModel(ctx, fetch, opts)
}

// execCmdWait bounds how long execCmd waits for a command: one still
// running then fails the test, so an action descheduled on a busy machine
// is never mistaken for one that sent nothing.
const execCmdWait = 5 * time.Second

// execCmd runs cmd and returns the messages it produces, expanding batches.
// Timers (instantTick's), spinner ticks and terminal queries are dropped, so
// tests see only the immediate results; a command still running after
// execCmdWait fails tb.
func execCmd(tb testing.TB, cmd tea.Cmd) []tea.Msg {
	tb.Helper()
	if cmd == nil || isInstantTick(cmd) {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(execCmdWait):
		tb.Fatalf("a command still runs after %v", execCmdWait)
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
			out = append(out, execCmd(tb, c)...)
		}
		return out
	}
	// tea.Sequence's message type is unexported; expand it by reflection.
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeOf(tea.Cmd(nil)) {
		var out []tea.Msg
		for i := range v.Len() {
			out = append(out, execCmd(tb, v.Index(i).Interface().(tea.Cmd))...)
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

// mustNotContain fails when any string appears in view, also as a PR
// reference the board draws in two columns (see containsSplitRef).
func mustNotContain(t *testing.T, view string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(view, w) || containsSplitRef(view, w) {
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
