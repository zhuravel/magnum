package cli

// The interactive screens of internal/tui: `status --watch` (dashboard),
// `prs` (PR board; tab switches between the two), `pick` (picker), `cleanup`
// (plan review) and `watch` (pane mirror). A command runs its screen only
// when stdin and stdout are terminals; anywhere else, and always with
// --json, it keeps its plain-text output.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/zhuravel/magnum/internal/tui"
)

// The screens; tests replace them to check what the adapters hand over and
// what runs after a screen exits.
var (
	tuiDashboard   = tui.RunDashboard
	tuiPRBoard     = tui.RunPRBoard
	tuiPicker      = tui.RunPicker
	tuiCleanupPlan = tui.RunCleanupPlan
	tuiWatch       = tui.RunWatch
)

// The screens tab switches between.
const (
	screenDashboard = "dashboard"
	screenBoard     = "prs"
)

// runScreens runs the screen named first, then whichever one the user
// switches to with tab, until a screen returns anything but a switch: nil
// when the user quit or ctx ended, else the screen's error.
func runScreens(ctx context.Context, first string, dashboard, board func(context.Context) error) error {
	next := first
	for {
		run := dashboard
		if next == screenBoard {
			run = board
		}
		err := run(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, tui.ErrSwitchToBoard):
			next = screenBoard
		case errors.Is(err, tui.ErrSwitchToDashboard):
			next = screenDashboard
		default:
			return err
		}
	}
}

// inspScreen reports whether an inspect command (status, cleanup) can run a
// screen: stdin and the command's stdout are terminals. Tests replace it.
var inspScreen = func(c *Context) bool {
	out, ok := c.Stdout.(*os.File)
	return ok && tui.IsTerminal(os.Stdin) && tui.IsTerminal(out)
}

// screen reports whether an act command (pick, watch) can run a screen.
func (d *actDeps) screen() bool { return d.StdinTTY && d.StdoutTTY }

// actCapture collects what a command prints while a screen owns the
// terminal; writes may come from any goroutine.
type actCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *actCapture) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// take returns what was written since the last take.
func (b *actCapture) take() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	b.buf.Reset()
	return s
}

func (b *actCapture) empty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String()) == ""
}

// lastLine is the last non-empty line of s, trimmed.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
