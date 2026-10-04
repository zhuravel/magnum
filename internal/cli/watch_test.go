package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// errStopWatch ends a watch loop from a test's sleep.
var errStopWatch = errors.New("test: stop watching")

func TestWatchMirrorsThePane(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	h.session(pr.ID, store.RoleClaude, store.SessionLive, "mg-talkable-5-claude", "p_2", "w_1")
	h.hd.reads = []herdr.ReadResult{{Text: "first\n", Revision: 1}, {Text: "first\n", Revision: 1}, {Text: "line a\nline b\nline c\nline d\nline e\nline f\n", Revision: 2}}
	reads := 0
	h.d.Sleep = func(context.Context, time.Duration) error {
		reads++
		if reads >= 3 {
			return errStopWatch
		}
		return nil
	}
	if code := h.cmd("watch", "talkable#5", "--role", "claude", "--ansi", "--interval", "1s", "--lines", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	if n := strings.Count(out, "\x1b[H\x1b[2J"); n != 2 {
		t.Errorf("redraws = %d (want 2: the unchanged revision is skipped)\n%q", n, out)
	}
	actContains(t, out, "magnum watch talkable#5 claude-review · pane p_2", "first", "line f", "\x1b[0m")
	if strings.Contains(out, "line a") {
		t.Errorf("more than --lines shown:\n%s", out)
	}
	for _, o := range h.hd.readOpts {
		if o.Source != herdr.SourceRecentUnwrapped || o.Format != herdr.FormatANSI || o.Lines != 5 {
			t.Fatalf("read options %+v", o)
		}
	}
}

func TestWatchQuitsOnQ(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	h.hd.reads = []herdr.ReadResult{{Text: "hello", Revision: 1}}
	h.stdin("q")
	h.d.Sleep = func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done() // returns once the key reader saw q
		return ctx.Err()
	}
	if code := h.cmd("watch", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "hello")
	if o := h.hd.readOpts[0]; o.Format != herdr.FormatText || o.Lines != 40 {
		t.Errorf("read options %+v (default lines are the fallback height minus the header)", o)
	}
}

func TestWatchCbreakOnATerminal(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	h.d.StdinTTY = true
	h.tty.Rules = []execx.Rule{
		{Prefix: []string{"stty", "-g"}, Result: execx.Result{Stdout: []byte("gfmt1:saved\n")}},
		{Prefix: []string{"stty", "size"}, Result: execx.Result{Stdout: []byte("30 120\n")}},
		{Prefix: []string{"stty"}},
	}
	h.d.Sleep = func(context.Context, time.Duration) error { return errStopWatch }
	if code := h.cmd("watch", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var argv []string
	for _, c := range h.tty.Calls {
		argv = append(argv, strings.Join(append([]string{c.Name}, c.Args...), " "))
	}
	got := strings.Join(argv, " | ")
	if got != "stty size | stty -g | stty -icanon -echo min 1 time 0 | stty gfmt1:saved" {
		t.Fatalf("stty calls: %s", got)
	}
	if o := h.hd.readOpts[0]; o.Lines != 28 {
		t.Errorf("lines = %d, want terminal rows minus 2", o.Lines)
	}
}

func TestWatchErrors(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	if code := h.cmd("watch", "5"); code != 1 || !strings.Contains(h.errb.String(), "has no live codex-judge pane") {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	h.hd.readErr = &herdr.Error{Method: "pane.read", Code: herdr.CodePaneNotFound}
	h.errb.Reset()
	if code := h.cmd("watch", "5"); code != 1 || !strings.Contains(h.errb.String(), "(p_1) closed") {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	h.errb.Reset()
	if code := h.cmd("watch", "5", "--interval", "10ms"); code != 2 {
		t.Fatalf("short interval exit %d", code)
	}
}

// withWatchScreen puts watch on a terminal and replaces the viewer with run.
func (h *actHarness) withWatchScreen(run func(ctx context.Context, fetch tui.WatchFetch, o tui.WatchOptions) error) {
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.tty.Rules = []execx.Rule{{Prefix: []string{"stty", "size"}, Result: execx.Result{Stdout: []byte("30 120\n")}}, {Prefix: []string{"stty"}}}
	old := tuiWatch
	tuiWatch = run
	h.t.Cleanup(func() { tuiWatch = old })
}

func TestWatchScreenFramesAndClosedPane(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	if _, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: store.RoleClaude, State: store.SessionLive,
		AgentName: store.Ptr("mg-talkable-5-claude"), HerdrPaneID: store.Ptr("p_2"), AgentStatus: store.Ptr("working")}); err != nil {
		t.Fatal(err)
	}
	h.hd.reads = []herdr.ReadResult{{Text: "line a\nline b\n", Revision: 1}}
	var frame tui.WatchFrame
	var opts tui.WatchOptions
	h.withWatchScreen(func(ctx context.Context, fetch tui.WatchFetch, o tui.WatchOptions) error {
		opts = o
		var err error
		if frame, err = fetch(ctx); err != nil {
			t.Fatalf("first fetch: %v", err)
		}
		h.hd.readErr = &herdr.Error{Method: "pane.read", Code: herdr.CodePaneNotFound}
		_, err = fetch(ctx)
		stop := errors.Unwrap(err) // RunWatch returns what StopWatch wrapped
		if stop == nil {
			t.Fatalf("a closed pane must stop the screen: %v", err)
		}
		return stop
	})
	code := h.cmd("watch", "talkable#5", "--role", "claude", "--ansi", "--interval", "1s")
	if code != 1 || !strings.Contains(h.errb.String(), "the claude-review pane of talkable#5 (p_2) closed") {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	want := tui.WatchFrame{Title: "talkable#5", Role: "claude-review", Pane: "p_2", Status: "working", Text: "line a\nline b\n", ANSI: true}
	if frame != want || opts.Interval != time.Second || opts.Now == nil {
		t.Fatalf("frame %+v options %+v", frame, opts)
	}
	if o := h.hd.readOpts[0]; o.Source != herdr.SourceRecentUnwrapped || o.Format != herdr.FormatANSI || o.Lines != 28 {
		t.Fatalf("read options %+v (terminal rows minus 2)", o)
	}
	if h.out.Len() != 0 || len(h.tty.CallsWithPrefix("stty", "-g")) != 0 {
		t.Errorf("the plain redraw ran: out %q, stty %+v", h.out.String(), h.tty.Calls)
	}
}

func TestWatchScreenHerdrGone(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	h.hd.readErr = fmt.Errorf("dial: %w", herdr.ErrUnavailable)
	h.withWatchScreen(func(ctx context.Context, fetch tui.WatchFetch, _ tui.WatchOptions) error {
		_, err := fetch(ctx)
		return errors.Unwrap(err)
	})
	if code := h.cmd("watch", "5"); code != 1 || !strings.Contains(h.errb.String(), "magnum watch: herdr is not running") {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	// Quitting the viewer exits 0.
	h.withWatchScreen(func(context.Context, tui.WatchFetch, tui.WatchOptions) error { return nil })
	if code := h.cmd("watch", "5"); code != 0 {
		t.Fatalf("quit: exit %d", code)
	}
}
