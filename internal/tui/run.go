package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
)

// extraProgramOptions are appended to every program's options; tests use it
// to feed scripted input and capture output without a terminal.
var extraProgramOptions []tea.ProgramOption

// runProgram runs m until it quits or ctx ends (each screen's View asks for
// the alternate screen). An ended ctx or a SIGINT is not an error: the
// returned model is the last one the program held (nil when it never
// started).
func runProgram(ctx context.Context, m tea.Model) (tea.Model, error) {
	opts := append([]tea.ProgramOption{tea.WithContext(ctx)}, extraProgramOptions...)
	tm := traced(m)
	if t, ok := tm.(*traceModel); ok {
		defer t.f.Close()
	}
	final, err := tea.NewProgram(tm, opts...).Run()
	if t, ok := final.(*traceModel); ok {
		final = t.Model
	}
	switch {
	case err == nil:
	case errors.Is(err, tea.ErrInterrupted):
		err = nil
	case ctx.Err() != nil && errors.Is(err, tea.ErrProgramKilled):
		err = nil
	}
	return final, err
}

// newView wraps rendered content in a full-screen tea.View.
func newView(content string) tea.View {
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// traceModel wraps a model and appends one line per message to the file
// named by MAGNUM_TUI_TRACE (type, time, and how long Update and View
// took), so a frozen or spinning screen can be diagnosed after the fact
// without a debugger. Off unless the variable is set.
type traceModel struct {
	tea.Model
	f     *os.File
	start time.Time
	n     int
}

func traced(m tea.Model) tea.Model {
	path := os.Getenv("MAGNUM_TUI_TRACE")
	if path == "" {
		return m
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return m
	}
	fmt.Fprintf(f, "--- %s trace start %T\n", time.Now().Format(time.RFC3339), m)
	return &traceModel{Model: m, f: f, start: time.Now()}
}

func (t *traceModel) Init() tea.Cmd { return t.Model.Init() }

func (t *traceModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	t.n++
	began := time.Now()
	next, cmd := t.Model.Update(msg)
	t.Model = next
	fmt.Fprintf(t.f, "%8.3fs #%d update %T %v\n", began.Sub(t.start).Seconds(), t.n, msg, time.Since(began))
	return t, cmd
}

func (t *traceModel) View() tea.View {
	began := time.Now()
	v := t.Model.View()
	fmt.Fprintf(t.f, "%8.3fs view %d bytes %v\n", began.Sub(t.start).Seconds(), len(v.Content), time.Since(began))
	return v
}
