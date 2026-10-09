package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

var errSentinel = errors.New("sentinel")

// numberedFrame has n lines "line 01" ... "line nn".
func numberedFrame(n int) WatchFrame {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %02d", i+1)
	}
	return WatchFrame{Title: "talkable#11920", Role: "judge", Pane: "p_7", Status: "working", Text: strings.Join(lines, "\n")}
}

func okFetch(f WatchFrame) WatchFetch {
	return func(context.Context) (WatchFrame, error) { return f, nil }
}

// watchAt builds a model with a movable clock, sized w x h.
func watchAt(t *testing.T, w, h int, clock *time.Time) watchModel {
	t.Helper()
	opts := WatchOptions{}
	if clock != nil {
		opts.Now = func() time.Time { return *clock }
	}
	m := testWatch(context.Background(), okFetch(WatchFrame{}), opts)
	m, _ = send(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

func frameMsg(f WatchFrame) tea.Msg { return watchResultMsg{frame: f} }

func TestWatchShowsHeaderAndFollowsTheTail(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	m, cmd := send(t, m, frameMsg(numberedFrame(30)))
	if cmd == nil {
		t.Error("a result must schedule the next fetch")
	}
	v := viewOf(m)
	mustContain(t, v, "talkable#11920 · judge · pane p_7 · working · updated 0s · follow",
		"line 30", "line 23",
		"j/k scroll · pgup/pgdn page · g/G top/bottom · f follow · q quit")
	mustNotContain(t, v, "line 01", "line 22", "paused")
	if n := len(strings.Split(v, "\n")); n != 10 {
		t.Errorf("view is %d lines, want 10 (header, 8 rows, footer):\n%s", n, v)
	}
	if w := maxLineWidth(v); w > 80 {
		t.Errorf("widest line is %d cells:\n%s", w, v)
	}
}

func TestWatchWaitsForTheFirstFrame(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	mustContain(t, viewOf(m), "waiting…", "· follow")
	mustNotContain(t, viewOf(m), "updated")
}

func TestWatchHeaderAgeFollowsTheClock(t *testing.T) {
	clock := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	m := watchAt(t, 80, 10, &clock)
	m, _ = send(t, m, frameMsg(numberedFrame(3)))
	mustContain(t, viewOf(m), "updated 0s")
	clock = clock.Add(75 * time.Second)
	mustContain(t, viewOf(m), "updated 1m")

	// A failed fetch does not reset the age: the frame is still the old one.
	m, _ = send(t, m, watchResultMsg{err: errors.New("boom")})
	mustContain(t, viewOf(m), "updated 1m")
}

func TestWatchScrollingUpPausesFollow(t *testing.T) {
	for _, k := range []string{"k", "up", "pgup", "g", "home"} {
		t.Run(k, func(t *testing.T) {
			m := watchAt(t, 80, 10, nil)
			m, _ = send(t, m, frameMsg(numberedFrame(30)))
			m, _ = send(t, m, keys(k)...)
			mustContain(t, viewOf(m), "paused (f follows)")
			mustNotContain(t, viewOf(m), "· follow")
		})
	}
}

func TestWatchPausedKeepsOffsetOnNewFrame(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(numberedFrame(30)))
	m, _ = send(t, m, keys("k")...) // one line up: rows 22-29
	before := viewOf(m)
	mustContain(t, before, "line 22", "line 29", "paused")
	mustNotContain(t, before, "line 30")

	m, _ = send(t, m, frameMsg(numberedFrame(40)))
	v := viewOf(m)
	mustContain(t, v, "line 22", "line 29", "paused")
	mustNotContain(t, v, "line 30", "line 40")
}

func TestWatchGResumesFollow(t *testing.T) {
	for _, k := range []string{"G", "end"} {
		t.Run(k, func(t *testing.T) {
			m := watchAt(t, 80, 10, nil)
			m, _ = send(t, m, frameMsg(numberedFrame(30)))
			m, _ = send(t, m, keys("g")...)
			mustContain(t, viewOf(m), "line 01", "paused")
			m, _ = send(t, m, keys(k)...)
			mustContain(t, viewOf(m), "line 30", "· follow")
			mustNotContain(t, viewOf(m), "paused", "line 01")

			// Following again: new content scrolls into view.
			m, _ = send(t, m, frameMsg(numberedFrame(35)))
			mustContain(t, viewOf(m), "line 35")
		})
	}
}

func TestWatchGShowsTheFirstLine(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(numberedFrame(30)))
	m, _ = send(t, m, keys("g")...)
	mustContain(t, viewOf(m), "line 01", "line 08")
	mustNotContain(t, viewOf(m), "line 09", "line 30")
}

func TestWatchFToggles(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(numberedFrame(30)))
	m, _ = send(t, m, keys("f")...)
	mustContain(t, viewOf(m), "paused (f follows)", "line 30") // pausing leaves the view where it is
	m, _ = send(t, m, keys("g")...)
	mustContain(t, viewOf(m), "line 01")
	m, _ = send(t, m, keys("f")...) // follow again jumps to the bottom
	mustContain(t, viewOf(m), "· follow", "line 30")
	mustNotContain(t, viewOf(m), "line 01", "paused")
}

func TestWatchJAndPageDownScrollWithoutResumingFollow(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(numberedFrame(30)))
	m, _ = send(t, m, keys("g", "j", "j")...)
	mustContain(t, viewOf(m), "line 03", "line 10", "paused")
	mustNotContain(t, viewOf(m), "line 02")
	m, _ = send(t, m, keys("pgdown")...)
	mustContain(t, viewOf(m), "line 11", "line 18", "paused")
	m, _ = send(t, m, keys("pgup")...)
	mustContain(t, viewOf(m), "line 03", "line 10", "paused")
	mustNotContain(t, viewOf(m), "line 30")
}

func TestWatchFetchErrorKeepsTheLastFrame(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(numberedFrame(30)))
	m, cmd := send(t, m, watchResultMsg{err: errors.New("herdr: timeout\nafter 5s")})
	if cmd == nil {
		t.Error("a failed fetch must still schedule the next one")
	}
	v := viewOf(m)
	mustContain(t, v, "fetch failed: herdr: timeout after 5s", "line 30", "talkable#11920")
	if n := len(strings.Split(v, "\n")); n != 10 {
		t.Errorf("view is %d lines, want 10", n)
	}

	m, _ = send(t, m, frameMsg(numberedFrame(30)))
	mustNotContain(t, viewOf(m), "fetch failed")
}

func TestWatchErrorBeforeAnyFrame(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, watchResultMsg{err: errors.New("boom")})
	mustContain(t, viewOf(m), "waiting…", "fetch failed: boom")
}

func TestWatchUnchangedTextKeepsScrollPosition(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(numberedFrame(60)))
	m, _ = send(t, m, keys("g", "pgdown", "j")...)
	want := m.vp.YOffset()
	if want == 0 {
		t.Fatal("test setup: expected a scrolled position")
	}
	f := numberedFrame(60)
	f.Status = "idle" // header data may change; the text does not
	m, _ = send(t, m, frameMsg(f))
	if m.vp.YOffset() != want {
		t.Errorf("YOffset = %d, want %d", m.vp.YOffset(), want)
	}
	mustContain(t, viewOf(m), "idle", "paused")
}

func TestWatchShorterContentWhilePausedStaysInRange(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(numberedFrame(60)))
	m, _ = send(t, m, keys("k")...) // paused near the bottom
	m, _ = send(t, m, frameMsg(numberedFrame(12)))
	mustContain(t, viewOf(m), "line 12")
}

func TestWatchStopWatchEndsTheScreen(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(numberedFrame(5)))
	m, cmd := send(t, m, watchResultMsg{err: StopWatch(errSentinel)})
	if !isQuit(execCmd(t, cmd)) {
		t.Fatal("a StopWatch error must quit")
	}
	if got := m.stopError(); got != errSentinel {
		t.Errorf("stopError() = %v, want the wrapped sentinel itself", got)
	}

	// Wrapped further by the fetch function, it still stops.
	m = watchAt(t, 80, 10, nil)
	m, cmd = send(t, m, watchResultMsg{err: fmt.Errorf("pane gone: %w", StopWatch(errSentinel))})
	if !isQuit(execCmd(t, cmd)) || !errors.Is(m.stopError(), errSentinel) {
		t.Errorf("wrapped StopWatch: quit=%v err=%v", isQuit(execCmd(t, cmd)), m.stopError())
	}

	// A plain error never stops.
	m = watchAt(t, 80, 10, nil)
	m, cmd = send(t, m, watchResultMsg{err: errSentinel})
	if m.stopError() != nil || isQuit(execCmd(t, cmd)) {
		t.Error("a plain fetch error must not stop the watch")
	}
}

func TestWatchStopWatchWrapsAndUnwraps(t *testing.T) {
	if StopWatch(nil) != nil {
		t.Error("StopWatch(nil) must be nil")
	}
	err := StopWatch(errSentinel)
	if !errors.Is(err, errSentinel) {
		t.Error("errors.Is must see through StopWatch")
	}
	if err.Error() != errSentinel.Error() {
		t.Errorf("Error() = %q, want the wrapped message", err.Error())
	}
}

func TestWatchStripsANSIUnlessAsked(t *testing.T) {
	f := WatchFrame{Title: "t", Text: "\x1b[31mred\x1b[0m plain\nsecond"}
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(f))
	mustContain(t, viewOf(m), "red plain", "second")
	mustNotContain(t, viewOf(m), "\x1b[31m")

	f.ANSI = true
	m = watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(f))
	mustContain(t, m.View().Content, "\x1b[31mred") // raw: the styling passes through
	if !strings.HasSuffix(watchContent(f), "\x1b[0m") {
		t.Errorf("ANSI content must end with a reset: %q", watchContent(f))
	}
	if strings.Contains(watchContent(WatchFrame{Text: "x\x1b[0m"}), "\x1b") {
		t.Error("plain content must carry no escape sequences")
	}
}

func TestWatchContentTrimsTrailingNewlinesAndTabs(t *testing.T) {
	if got := watchContent(WatchFrame{Text: "a\tb\r\nlast\n\n\n"}); got != "a    b\nlast" {
		t.Errorf("content = %q", got)
	}
}

func TestWatchIntervalDefaultAndClamp(t *testing.T) {
	cases := []struct{ in, want time.Duration }{
		{0, 2 * time.Second},
		{-time.Second, 2 * time.Second},
		{50 * time.Millisecond, 200 * time.Millisecond},
		{200 * time.Millisecond, 200 * time.Millisecond},
		{5 * time.Second, 5 * time.Second},
	}
	for _, c := range cases {
		m := testWatch(context.Background(), okFetch(WatchFrame{}), WatchOptions{Interval: c.in})
		if m.interval != c.want {
			t.Errorf("Interval %v -> %v, want %v", c.in, m.interval, c.want)
		}
	}
}

func TestWatchQuitKeys(t *testing.T) {
	for _, k := range []string{"q", "esc", "ctrl+c"} {
		t.Run(k, func(t *testing.T) {
			m := watchAt(t, 80, 10, nil)
			m, cmd := send(t, m, keys(k)...)
			if !isQuit(execCmd(t, cmd)) {
				t.Errorf("%s did not quit", k)
			}
			if m.stopError() != nil {
				t.Errorf("quitting is not an error, got %v", m.stopError())
			}
		})
	}
}

func TestWatchFetchLoop(t *testing.T) {
	var calls atomic.Int32
	fetch := func(context.Context) (WatchFrame, error) {
		calls.Add(1)
		return numberedFrame(3), nil
	}
	m := testWatch(context.Background(), fetch, WatchOptions{Interval: time.Hour})

	// Init fetches once; nothing else fetches until a tick arrives.
	msgs := execCmd(t, m.Init())
	if len(msgs) != 1 || calls.Load() != 1 {
		t.Fatalf("Init produced %v after %d fetches", msgs, calls.Load())
	}
	res, ok := msgs[0].(watchResultMsg)
	if !ok || res.err != nil || res.frame.Title != "talkable#11920" {
		t.Fatalf("Init result = %#v", msgs[0])
	}
	m, cmd := send(t, m, msgs[0])
	if cmd == nil || calls.Load() != 1 {
		t.Fatalf("after a result: cmd=%v fetches=%d, want a pending tick and no new fetch", cmd != nil, calls.Load())
	}
	if got := execCmd(t, cmd); len(got) != 0 {
		t.Fatalf("the tick fired before the interval: %v", got)
	}
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 80, Height: 10}, keys("j")[0])
	if calls.Load() != 1 {
		t.Error("keys and resizes must not fetch")
	}

	_, cmd = send(t, m, watchTickMsg{})
	msgs = execCmd(t, cmd)
	if len(msgs) != 1 || calls.Load() != 2 {
		t.Fatalf("a tick must start exactly one fetch; msgs=%v fetches=%d", msgs, calls.Load())
	}
}

func TestWatchFetchGetsTheModelContext(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "v")
	var got any
	m := testWatch(ctx, func(c context.Context) (WatchFrame, error) {
		got = c.Value(key{})
		return WatchFrame{}, nil
	}, WatchOptions{})
	execCmd(t, m.Init())
	if got != "v" {
		t.Errorf("fetch ctx value = %v", got)
	}
}

// TestWatchFetchHasADeadline: a hung fetch must end as an error (the
// header then says so) instead of freezing the frame and its age.
func TestWatchFetchHasADeadline(t *testing.T) {
	var limit time.Duration
	m := testWatch(context.Background(), func(c context.Context) (WatchFrame, error) {
		if dl, ok := c.Deadline(); ok {
			limit = time.Until(dl)
		}
		return WatchFrame{}, nil
	}, WatchOptions{Interval: 3 * time.Second})
	execCmd(t, m.Init())
	if limit <= 10*time.Second || limit > 15*time.Second {
		t.Errorf("fetch deadline in %v, want five 3s intervals", limit)
	}
}

func TestWatchResultAfterContextEndedQuits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := testWatch(ctx, okFetch(WatchFrame{}), WatchOptions{})
	cancel()
	m, cmd := send(t, m, watchResultMsg{err: context.Canceled})
	if !isQuit(execCmd(t, cmd)) || m.stopError() != nil {
		t.Errorf("quit=%v stop=%v", isQuit(execCmd(t, cmd)), m.stopError())
	}
}

func TestWatchResizeKeepsFollowing(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(numberedFrame(30)))

	m, _ = send(t, m, tea.WindowSizeMsg{Width: 60, Height: 6}) // 4 rows
	v := viewOf(m)
	mustContain(t, v, "line 30", "line 27")
	mustNotContain(t, v, "line 26")
	if n, w := len(strings.Split(v, "\n")), maxLineWidth(v); n != 6 || w > 60 {
		t.Errorf("after shrinking: %d lines, widest %d:\n%s", n, w, v)
	}

	m, _ = send(t, m, tea.WindowSizeMsg{Width: 100, Height: 20})
	v = viewOf(m)
	mustContain(t, v, "line 30", "line 13")
	mustNotContain(t, v, "line 12")
	if n := len(strings.Split(v, "\n")); n != 20 {
		t.Errorf("after growing: %d lines, want 20", n)
	}
}

func TestWatchResizeWhilePausedStaysInRange(t *testing.T) {
	m := watchAt(t, 80, 10, nil)
	m, _ = send(t, m, frameMsg(numberedFrame(30)))
	m, _ = send(t, m, keys("k")...)
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 80, Height: 40}) // everything fits now
	mustContain(t, viewOf(m), "line 01", "line 30", "paused")
}

func TestWatchNarrowHeaderKeepsStateAndError(t *testing.T) {
	m := watchAt(t, 40, 10, nil)
	f := numberedFrame(30)
	f.Title = "talkable-with-a-very-long-title#11920"
	m, _ = send(t, m, frameMsg(f))
	m, _ = send(t, m, watchResultMsg{err: errors.New("herdr is unreachable at /some/long/socket/path")}, keys("k")[0])
	header := strings.SplitN(viewOf(m), "\n", 2)[0]
	mustContain(t, header, "paused (f follows)", "fetch failed")
	if w := len([]rune(header)); w > 40 {
		t.Errorf("header is %d cells wide: %q", w, header)
	}
	if w := maxLineWidth(viewOf(m)); w > 40 {
		t.Errorf("view is %d cells wide", w)
	}
}

func TestWatchDefaultSizeBeforeResize(t *testing.T) {
	m := testWatch(context.Background(), okFetch(WatchFrame{}), WatchOptions{})
	m, _ = send(t, m, frameMsg(numberedFrame(100)))
	v := viewOf(m)
	if n := len(strings.Split(v, "\n")); n != 24 {
		t.Errorf("default view is %d lines, want 24", n)
	}
	mustContain(t, v, "line 100")
}

func TestWatchLongLinesAreCutToWidth(t *testing.T) {
	m := watchAt(t, 30, 10, nil)
	m, _ = send(t, m, frameMsg(WatchFrame{Title: "t", Text: strings.Repeat("x", 200) + "\nshort"}))
	if w := maxLineWidth(viewOf(m)); w > 30 {
		t.Errorf("widest line %d > 30", w)
	}
	mustContain(t, viewOf(m), "short")
}

func runWatchAsync(ctx context.Context, fetch WatchFetch, opts WatchOptions) <-chan error {
	done := make(chan error, 1)
	go func() { done <- RunWatch(ctx, fetch, opts) }()
	return done
}

// announced wraps fetch so that the returned channel closes when the
// program first calls it: the program has run Init and reads its input.
func announced(fetch WatchFetch) (WatchFetch, <-chan struct{}) {
	started := make(chan struct{})
	var once sync.Once
	return func(ctx context.Context) (WatchFrame, error) {
		once.Do(func() { close(started) })
		return fetch(ctx)
	}, started
}

// waitFor fails the test unless ch closes within five seconds.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

func TestWatchRunReturnsTheStopWatchError(t *testing.T) {
	scriptedInput(t)
	var calls, inflight, overlap atomic.Int32
	fetch := func(context.Context) (WatchFrame, error) {
		if inflight.Add(1) > 1 {
			overlap.Add(1)
		}
		defer inflight.Add(-1)
		if calls.Add(1) == 2 {
			return WatchFrame{}, StopWatch(fmt.Errorf("pane closed: %w", errSentinel))
		}
		return numberedFrame(3), nil
	}
	select {
	case err := <-runWatchAsync(context.Background(), fetch, WatchOptions{Tick: instantTick}):
		if !errors.Is(err, errSentinel) {
			t.Fatalf("RunWatch = %v, want an error matching the sentinel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatch did not return")
	}
	if calls.Load() != 2 || overlap.Load() != 0 {
		t.Errorf("fetches = %d, overlapping = %d, want 2 and 0", calls.Load(), overlap.Load())
	}
}

func TestWatchRunQuitKeyReturnsNil(t *testing.T) {
	w := scriptedInput(t)
	// The timer never fires: an instant one would fetch again and again
	// until q arrives.
	fetch, started := announced(okFetch(numberedFrame(3)))
	done := runWatchAsync(context.Background(), fetch, WatchOptions{Tick: neverTick})
	waitFor(t, started, "the first fetch")
	go func() { _, _ = w.Write([]byte("q")) }() // waits until the program reads it
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWatch = %v, want nil after q", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatch did not return after q")
	}
}

func TestWatchRunEndsQuietlyWhenContextEnds(t *testing.T) {
	scriptedInput(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sawCancel := make(chan struct{})
	fetch, started := announced(func(ctx context.Context) (WatchFrame, error) {
		select {
		case <-ctx.Done():
			close(sawCancel)
			return WatchFrame{}, ctx.Err()
		case <-time.After(10 * time.Second):
			return WatchFrame{}, errors.New("fetch was never cancelled")
		}
	})
	done := runWatchAsync(ctx, fetch, WatchOptions{Tick: neverTick})
	waitFor(t, started, "the first fetch")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWatch = %v, want nil when ctx ends", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatch did not return after ctx ended")
	}
	select {
	case <-sawCancel:
	case <-time.After(time.Second):
		t.Error("the in-flight fetch never saw the cancellation")
	}
}

func TestWatchRunNeedsAFetch(t *testing.T) {
	if err := RunWatch(context.Background(), nil, WatchOptions{}); err == nil {
		t.Error("want an error for a nil fetch")
	}
}
