package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

const watchUsage = "watch <ref> [--role <role>] [--ansi] [--interval 2s] [--lines N]"

type watchOpts struct {
	role     string
	ansi     bool
	interval time.Duration
	lines    int
}

func newWatchCmd(c *Context) *cobra.Command {
	var o watchOpts
	cmd := newCommand(groupAct, watchUsage, "live read-only mirror of a PR's agent pane in any terminal (q quits)",
		"Mirror a PR's agent pane (its watch's judge by default, or --role) read-only in any terminal, re-read every "+
			"--interval. On a terminal it opens a full-screen viewer: j/k and pgup/pgdown scroll, g/G jump to the "+
			"top or the end, f toggles following the end and q quits. Elsewhere the text is redrawn every "+
			"--interval. The pane must be live: `magnum open` restores a parked one.",
		func(pos []string) int { return runWatch(c, o, pos) })
	fs := cmd.Flags()
	fs.StringVar(&o.role, "role", "", "pane to mirror: "+roleFlagHelp)
	fs.BoolVar(&o.ansi, "ansi", false, "keep colors (herdr ANSI format)")
	fs.DurationVar(&o.interval, "interval", 2*time.Second, "redraw interval")
	fs.IntVar(&o.lines, "lines", 0, "lines to show (default: terminal height)")
	c.completeRole(cmd)
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

func runWatch(c *Context, o watchOpts, pos []string) int {
	if len(pos) != 1 {
		return actUsage(c, "watch", "which PR?", watchUsage)
	}
	if o.interval < 200*time.Millisecond {
		return actUsage(c, "watch", "--interval must be at least 200ms", watchUsage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "watch", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return watchMain(ctx, c, d, pos[0], o)
}

func watchMain(ctx context.Context, c *Context, d *actDeps, ref string, o watchOpts) int {
	if err := actKnownRole(d.Cfg, o.role); err != nil {
		return actUsage(c, "watch", err.Error(), watchUsage)
	}
	t, err := d.resolve(ctx, ref, "", "")
	if err != nil {
		return cmdFail(c, "watch", verbFix("watch", err))
	}
	spec, err := actRoleFor(d.Cfg, t.full(), o.role)
	if err != nil {
		return cmdFail(c, "watch", err)
	}
	role := spec.Name
	label := d.actLabel(t.full(), t.PR.Number)
	sess, err := d.Store.LiveSessionByPRRole(ctx, t.PR.ID, role)
	if errors.Is(err, store.ErrNotFound) || (err == nil && store.Deref(sess.HerdrPaneID) == "") {
		return cmdFail(c, "watch", fmt.Errorf("%s has no live %s pane (PR state %s): `magnum open %s` restores parked sessions", label, actRoleName(role), t.PR.State, label))
	}
	if err != nil {
		return cmdFail(c, "watch", err)
	}
	pane := *sess.HerdrPaneID
	lines := o.lines
	if lines <= 0 {
		lines = d.termRows(42) - 2
	}
	if lines < 5 {
		lines = 5
	}
	format := herdr.FormatText
	if o.ansi {
		format = herdr.FormatANSI
	}
	read := herdr.ReadOptions{Source: herdr.SourceRecentUnwrapped, Lines: lines, Format: format}
	if d.screen() {
		return watchScreen(ctx, c, d, t.PR.ID, sess, role, label, read, o)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	restore := d.cbreak()
	defer restore()
	go func() { // q quits; ctrl+c arrives as a signal (or as 0x03 if the terminal passes it through)
		for {
			b, err := d.prompt().key(ctx)
			if err != nil {
				return
			}
			if b == 'q' || b == 'Q' || b == 3 {
				cancel()
				return
			}
		}
	}()

	w := c.Stdout
	if d.StdoutTTY {
		fmt.Fprint(w, "\x1b[?1049h\x1b[?25l") // alternate screen, hide cursor
		defer fmt.Fprint(w, "\x1b[?25h\x1b[?1049l")
	}
	var lastRev uint64
	drawn := false
	status := ""
	for {
		res, err := d.Herdr.PaneRead(ctx, pane, read)
		switch {
		case err != nil && ctx.Err() != nil:
			return 0
		case err != nil && herdr.IsCode(err, herdr.CodePaneNotFound):
			restore()
			fmt.Fprintln(c.Stderr, watchPaneClosed{role: role, label: label, pane: pane})
			return 1
		case err != nil && errors.Is(err, herdr.ErrUnavailable):
			restore()
			return cmdFail(c, "watch", d.herdrErr(err))
		case err != nil:
			status = "read failed: " + err.Error()
			watchDraw(w, label, role, pane, d.now(), status, "", lines, o.ansi)
			drawn = false
		case !drawn || res.Revision != lastRev || status != "":
			status = ""
			lastRev, drawn = res.Revision, true
			watchDraw(w, label, role, pane, d.now(), "", res.Text, lines, o.ansi)
		}
		if err := d.sleep(ctx, o.interval); err != nil {
			return 0
		}
	}
}

// watchPaneClosed ends a watch whose pane went away.
type watchPaneClosed struct{ role, label, pane string }

func (e watchPaneClosed) Error() string {
	return fmt.Sprintf("the %s pane of %s (%s) closed", actRoleName(e.role), e.label, e.pane)
}

// watchScreen mirrors the pane in the full-screen viewer. Each fetch reads
// the pane as the plain loop does and the session row for the agent status;
// a closed pane or a gone herdr ends the screen with that error.
func watchScreen(ctx context.Context, c *Context, d *actDeps, prID int64, sess store.Session, role, label string, read herdr.ReadOptions, o watchOpts) int {
	pane, status := *sess.HerdrPaneID, store.Deref(sess.AgentStatus)
	fetch := func(ctx context.Context) (tui.WatchFrame, error) {
		f := tui.WatchFrame{Title: label, Role: actRoleName(role), Pane: pane, Status: status, ANSI: o.ansi}
		if s, err := d.Store.LiveSessionByPRRole(ctx, prID, role); err == nil && store.Deref(s.HerdrPaneID) == pane {
			f.Status = store.Deref(s.AgentStatus)
		}
		res, err := d.Herdr.PaneRead(ctx, pane, read)
		switch {
		case err != nil && herdr.IsCode(err, herdr.CodePaneNotFound):
			return f, tui.StopWatch(watchPaneClosed{role: role, label: label, pane: pane})
		case err != nil && errors.Is(err, herdr.ErrUnavailable):
			return f, tui.StopWatch(d.herdrErr(err))
		case err != nil:
			return f, err
		}
		f.Text = res.Text
		return f, nil
	}
	err := tuiWatch(ctx, fetch, tui.WatchOptions{Interval: o.interval, Now: d.now})
	var closed watchPaneClosed
	switch {
	case err == nil:
		return 0
	case errors.As(err, &closed):
		fmt.Fprintln(c.Stderr, closed)
		return 1
	}
	return cmdFail(c, "watch", err)
}

// watchDraw clears the screen and prints a header plus the pane's last lines.
func watchDraw(w io.Writer, label, role, pane string, now time.Time, status, text string, lines int, ansi bool) {
	header := fmt.Sprintf("magnum watch %s %s · pane %s · %s · q quits", label, actRoleName(role), pane, actClock(now))
	if status != "" {
		header += " · " + status
	}
	text = strings.TrimRight(text, "\n")
	if parts := strings.Split(text, "\n"); len(parts) > lines {
		text = strings.Join(parts[len(parts)-lines:], "\n")
	}
	fmt.Fprint(w, "\x1b[H\x1b[2J")
	fmt.Fprintln(w, header)
	fmt.Fprint(w, text)
	if ansi {
		fmt.Fprint(w, "\x1b[0m")
	}
	fmt.Fprintln(w)
}
