package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

var factoryBarClock = time.Date(2026, 11, 5, 14, 12, 0, 0, time.UTC)

// testFactoryBar builds the bar as the screen does, with the screens' test
// timer (instantTick): execCmd drops its notes' timers unrun, as the
// board's tests see theirs, and a test that runs one sees the expiry.
func testFactoryBar(ctx context.Context) (factoryBar, *ActionLog) {
	log := NewActionLog()
	return newFactoryBar(ctx, log, func() time.Time { return factoryBarClock }, instantTick), log
}

// barView is the bar's line without styling.
func barView(b *factoryBar, width int) string { return ansi.Strip(b.view(width)) }

// deliver hands msgs to the bar as the screen's Update does and returns the
// command the last one asked for.
func deliver(t *testing.T, b *factoryBar, msgs ...tea.Msg) tea.Cmd {
	t.Helper()
	var cmd tea.Cmd
	for _, m := range msgs {
		next, handled := b.update(m)
		if !handled {
			t.Fatalf("the bar did not take %T", m)
		}
		cmd = next
	}
	return cmd
}

// confirmed asks sentence, answers y and runs the action's command; it
// returns what the command produced.
func confirmed(t *testing.T, b *factoryBar, sentence string, act factoryAction) []tea.Msg {
	t.Helper()
	b.ask(sentence, act)
	cmd, handled := b.key(keyMsg("y"))
	if !handled {
		t.Fatalf("y did not answer %q", sentence)
	}
	return execCmd(t, cmd)
}

// background asks sentence, answers y and runs the action's command on its
// own goroutine, as Bubble Tea does; the message it returns arrives on the
// channel when the action ends.
func background(t *testing.T, b *factoryBar, sentence string, act factoryAction) <-chan tea.Msg {
	t.Helper()
	b.ask(sentence, act)
	cmd, handled := b.key(keyMsg("y"))
	if !handled || cmd == nil {
		t.Fatalf("y did not start %q", sentence)
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	return done
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(execCmdWait):
		t.Fatalf("nothing arrived in %v", execCmdWait)
		panic("unreachable")
	}
}

func TestFactoryBarRunsOnlyOnY(t *testing.T) {
	const sentence = "Push PR-12301's branch and open the PR?"
	for _, k := range []string{"n", "N", "esc", "enter", "space", "x", "ctrl+c"} {
		t.Run("cancel on "+k, func(t *testing.T) {
			b, log := testFactoryBar(t.Context())
			var ran atomic.Int32
			b.ask(sentence, func(context.Context) (ActionResult, error) {
				ran.Add(1)
				return ActionResult{Text: "pushed"}, nil
			})
			mustContain(t, barView(&b, 140), sentence+" y/N", "y confirms, any other key cancels")

			cmd, handled := b.key(keyMsg(k))
			if !handled {
				t.Errorf("%s was not taken as the answer to the question", k)
			}
			mustContain(t, barView(&b, 140), "cancelled", "nothing was run")
			deliver(t, &b, execCmd(t, cmd)...) // the note's timer
			if ran.Load() != 0 {
				t.Errorf("%s ran the action", k)
			}
			if entries, _ := log.snapshot(); len(entries) != 0 {
				t.Errorf("a cancelled question left log entries: %+v", entries)
			}
			if _, handled := b.key(keyMsg("y")); handled {
				t.Error("a y after the answer was taken as an answer again")
			}
			if ran.Load() != 0 {
				t.Error("a later y ran the cancelled action")
			}
		})
	}
	for _, k := range []string{"y", "Y"} {
		t.Run("run on "+k, func(t *testing.T) {
			b, log := testFactoryBar(t.Context())
			var ran atomic.Int32
			b.ask(sentence, func(context.Context) (ActionResult, error) {
				ran.Add(1)
				return ActionResult{Text: "pushed"}, nil
			})
			if ran.Load() != 0 {
				t.Fatal("ask ran the action before the answer")
			}
			cmd, handled := b.key(keyMsg(k))
			if !handled {
				t.Fatalf("%s was not taken as the answer", k)
			}
			if ran.Load() != 0 {
				t.Fatal("the action ran on the key's own turn, not as a command")
			}
			deliver(t, &b, execCmd(t, cmd)...)
			if ran.Load() != 1 {
				t.Errorf("%s ran the action %d times, want 1", k, ran.Load())
			}
			if entries, _ := log.snapshot(); len(entries) != 1 {
				t.Errorf("log has %d entries, want 1", len(entries))
			}
		})
	}
	t.Run("no question, no answer", func(t *testing.T) {
		b, _ := testFactoryBar(t.Context())
		for _, k := range []string{"y", "n", "esc", "j"} {
			if cmd, handled := b.key(keyMsg(k)); handled || cmd != nil {
				t.Errorf("%s with no question was taken by the bar", k)
			}
		}
		if v := b.view(80); v != "" {
			t.Errorf("an idle bar draws %q", v)
		}
	})
}

func TestFactoryBarRefusesASecondActionWhileOneRuns(t *testing.T) {
	b, log := testFactoryBar(t.Context())
	started, release := make(chan struct{}), make(chan struct{})
	var second atomic.Int32
	done := background(t, &b, "Push PR-12301's branch?", func(context.Context) (ActionResult, error) {
		close(started)
		<-release
		return ActionResult{Text: "pushed"}, nil
	})
	receive(t, started)

	if _, handled := b.key(keyMsg("j")); handled {
		t.Error("a navigation key was taken while an action ran")
	}
	waitCmd := b.ask("Start PR-12302?", func(context.Context) (ActionResult, error) {
		second.Add(1)
		return ActionResult{}, nil
	})
	mustContain(t, barView(&b, 140), "wait: Push PR-12301's branch runs")
	if strings.Contains(barView(&b, 140), "y/N") {
		t.Error("a second question was asked while one action ran")
	}
	if cmd, handled := b.key(keyMsg("y")); handled || cmd != nil {
		t.Error("a y started something while an action ran")
	}
	// The refusal's note ends; the running action stays on the bar.
	deliver(t, &b, waitCmd())
	mustContain(t, barView(&b, 140), "busy: Push PR-12301's branch")

	close(release)
	deliver(t, &b, receive(t, done))
	if second.Load() != 0 {
		t.Error("the refused action ran")
	}
	entries, _ := log.snapshot()
	if len(entries) != 1 || entries[0].what != "Push PR-12301's branch" {
		t.Errorf("log = %+v, want only the first action", entries)
	}

	// The bar is free again.
	b.ask("Start PR-12302?", func(context.Context) (ActionResult, error) { return ActionResult{}, nil })
	mustContain(t, barView(&b, 140), "Start PR-12302? y/N")
}

// A second question while one waits is dropped: the question on the bar is
// what waits, and it stays drawn (no note hides behind it).
func TestFactoryBarKeepsTheFirstQuestion(t *testing.T) {
	b, _ := testFactoryBar(t.Context())
	var first, second atomic.Int32
	b.ask("Push PR-12301?", func(context.Context) (ActionResult, error) { first.Add(1); return ActionResult{}, nil })
	if cmd := b.ask("Drop PR-12302?", func(context.Context) (ActionResult, error) { second.Add(1); return ActionResult{}, nil }); cmd != nil {
		t.Error("a second question while one waits asked for a timer: its note would never be drawn")
	}
	if b.flash != "" {
		t.Errorf("a second question left a note that the question hides: %q", b.flash)
	}
	mustContain(t, barView(&b, 140), "Push PR-12301? y/N")
	cmd, _ := b.key(keyMsg("y"))
	deliver(t, &b, execCmd(t, cmd)...)
	if first.Load() != 1 || second.Load() != 0 {
		t.Errorf("ran first=%d second=%d, want 1 and 0", first.Load(), second.Load())
	}
}

func TestFactoryBarLogsEveryAction(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b, log := testFactoryBar(ctx)

	deliver(t, &b, confirmed(t, &b, "Push PR-12301's branch?", func(context.Context) (ActionResult, error) {
		return ActionResult{Text: "checked the head\npushed 3 commits"}, nil
	})...)
	deliver(t, &b, confirmed(t, &b, "Drop PR-12302?", func(context.Context) (ActionResult, error) {
		return ActionResult{Text: "freed nothing"}, errors.New("the copy is still in use")
	})...)

	// The screen quits in the middle of the third: its context ends, the
	// action stops, and the log has its outcome though no message is ever
	// delivered to the bar.
	started := make(chan struct{})
	done := background(t, &b, "Start PR-12303?", func(ctx context.Context) (ActionResult, error) {
		close(started)
		<-ctx.Done()
		return ActionResult{Text: "stopped after the first step"}, ctx.Err()
	})
	receive(t, started)
	cancel()
	receive(t, done)

	entries, _ := log.snapshot() // newest first
	if len(entries) != 3 {
		t.Fatalf("log has %d entries, want 3: %+v", len(entries), entries)
	}
	want := []struct{ what, text, err string }{
		{"Start PR-12303", "stopped after the first step", context.Canceled.Error()},
		{"Drop PR-12302", "freed nothing", "the copy is still in use"},
		{"Push PR-12301's branch", "checked the head\npushed 3 commits", ""},
	}
	for i, w := range want {
		e := entries[i]
		if e.what != w.what || e.text != w.text || e.err != w.err {
			t.Errorf("entry %d = {%q %q %q}, want {%q %q %q}", i, e.what, e.text, e.err, w.what, w.text, w.err)
		}
		if !e.at.Equal(factoryBarClock) {
			t.Errorf("entry %d at %v, want the bar's clock %v", i, e.at, factoryBarClock)
		}
	}
	// The log paints them: the failures as failures, with their text.
	lines := strings.Join(logContent(newStyles(true), newGlyphs(IconsUnicode), entries, 100), "\n")
	mustContain(t, ansi.Strip(lines), "Start PR-12303", "context canceled", "the copy is still in use", "pushed 3 commits")
}

func TestFactoryBarCleansWhatItShows(t *testing.T) {
	b, log := testFactoryBar(t.Context())
	deliver(t, &b, confirmed(t, &b, "Push \x1b[31mPR-12301\x1b[0m\nnow?", func(context.Context) (ActionResult, error) {
		return ActionResult{Text: "step one\npushed \x1b[31mred\x1b[0m\x1b]0;title\x07"}, nil
	})...)
	entries, _ := log.snapshot()
	if len(entries) != 1 || entries[0].what != "Push PR-12301 now" {
		t.Errorf("the log's action name = %+v, want %q without control characters", entries, "Push PR-12301 now")
	}
	// The footer's own text, before the styling: nothing of the action's
	// escape sequences is left in it.
	if b.flash != "pushed red" {
		t.Errorf("the bar's note = %q, want %q", b.flash, "pushed red")
	}
}

func TestFactoryBarShowsAResultThenClearsIt(t *testing.T) {
	b, _ := testFactoryBar(t.Context())
	msgs := confirmed(t, &b, "Push PR-12301's branch?", func(context.Context) (ActionResult, error) {
		return ActionResult{Text: "checked the head\npushed 3 commits"}, nil
	})
	if v := barView(&b, 100); !strings.Contains(v, "busy: Push PR-12301's branch") {
		t.Errorf("the bar does not show the action in flight: %q", v)
	}
	expire := deliver(t, &b, msgs...)
	if v := barView(&b, 100); strings.TrimSpace(v) != "pushed 3 commits" {
		t.Errorf("the bar shows %q after the action, want its last line", v)
	}
	// A timer of an older note does not clear a newer one.
	oldExpire := []tea.Msg{expire()}
	deliver(t, &b, confirmed(t, &b, "Start PR-12302?", func(context.Context) (ActionResult, error) {
		return ActionResult{Text: "started"}, nil
	})...)
	deliver(t, &b, oldExpire...)
	if v := barView(&b, 100); strings.TrimSpace(v) != "started" {
		t.Errorf("an older timer cleared the newer note: %q", v)
	}
	b.update(factoryExpireMsg{seq: b.flashSeq})
	if v := b.view(100); v != "" {
		t.Errorf("the bar still draws %q after its note expired", v)
	}
}

func TestFactoryBarKeepsAFailureUntilAKey(t *testing.T) {
	b, _ := testFactoryBar(t.Context())
	cmd := deliver(t, &b, confirmed(t, &b, "Push PR-12301's branch?", func(context.Context) (ActionResult, error) {
		return ActionResult{}, errors.New("the remote head moved after the card, nothing was pushed because the push would have raced it")
	})...)
	if cmd != nil {
		t.Error("a failure asked for a timer, but it stays until a key")
	}
	mustContain(t, barView(&b, 200), "Push PR-12301's branch failed: the remote head moved")
	b.update(factoryExpireMsg{seq: b.flashSeq})
	mustContain(t, barView(&b, 200), "failed")

	narrow := barView(&b, 60)
	if w := ansi.StringWidth(narrow); w > 60 {
		t.Errorf("the failure is %d cells wide at 60", w)
	}
	mustContain(t, narrow, logKey+" shows all")

	if cmd, handled := b.key(keyMsg("j")); handled || cmd != nil {
		t.Error("the key that clears a failure was taken: it must act as usual")
	}
	if v := b.view(200); v != "" {
		t.Errorf("the failure stays after a key: %q", v)
	}
}

// The screen quits with a question up: the question is dropped, and a y
// that comes after (queued behind ctrl+c) answers nothing and starts
// nothing; a later ask is refused.
func TestFactoryBarDropsItsQuestionWhenTheScreenQuits(t *testing.T) {
	b, log := testFactoryBar(t.Context())
	var ran atomic.Int32
	run := func(context.Context) (ActionResult, error) { ran.Add(1); return ActionResult{}, nil }
	b.ask("Push PR-12301's branch and open the PR?", run)
	b.drop()
	if cmd, taken := b.key(keyMsg("y")); taken || cmd != nil {
		t.Error("a y after the drop was taken as an answer")
	}
	if cmd := b.ask("Drop PR-12302?", run); cmd != nil || b.asking != nil {
		t.Error("a question was asked after the drop")
	}
	if cmd, taken := b.key(keyMsg("y")); taken || cmd != nil {
		t.Error("a y after a refused ask was taken")
	}
	if v := b.view(100); v != "" {
		t.Errorf("a dropped bar draws %q", v)
	}
	if cmd := b.start("Open PR-12301's pane", run); cmd != nil {
		t.Error("an action started after the drop")
	}
	if ran.Load() != 0 {
		t.Errorf("a dropped question ran %d times", ran.Load())
	}
	if entries, _ := log.snapshot(); len(entries) != 0 {
		t.Errorf("a dropped question left log entries: %+v", entries)
	}
}

// The action's time is the y's, taken on the screen's turn, not when the
// command's goroutine ends.
func TestFactoryBarTakesTheActionsTimeOnTheKey(t *testing.T) {
	var mu sync.Mutex
	clock := factoryBarClock
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	log := NewActionLog()
	b := newFactoryBar(t.Context(), log, now, instantTick)
	b.ask("Push PR-12301?", func(context.Context) (ActionResult, error) { return ActionResult{Text: "pushed"}, nil })
	cmd, _ := b.key(keyMsg("y"))
	mu.Lock()
	clock = clock.Add(time.Hour) // the action ends an hour later
	mu.Unlock()
	deliver(t, &b, execCmd(t, cmd)...)
	entries, _ := log.snapshot()
	if len(entries) != 1 || !entries[0].at.Equal(factoryBarClock) {
		t.Errorf("the log has %+v, want one entry at the y's time %v", entries, factoryBarClock)
	}
}

// A bar built without a context runs its actions on one that is not nil.
func TestFactoryBarNeverRunsAnActionOnANilContext(t *testing.T) {
	var none context.Context
	b := newFactoryBar(none, nil, nil, nil)
	got := make(chan context.Context, 1)
	b.ask("Push PR-12301?", func(ctx context.Context) (ActionResult, error) { got <- ctx; return ActionResult{}, nil })
	cmd, _ := b.key(keyMsg("y"))
	execCmd(t, cmd)
	if ctx := receive(t, got); ctx == nil {
		t.Error("the action ran on a nil context")
	}
}

// An action that asks nothing (o: step into the pane) runs at once, is
// logged as any other, and is refused while another runs.
func TestFactoryBarStartsAnActionThatAsksNothing(t *testing.T) {
	b, log := testFactoryBar(t.Context())
	cmd := b.start("Open PR-12301's pane", func(context.Context) (ActionResult, error) {
		return ActionResult{Text: "focused the writer's pane"}, nil
	})
	if cmd == nil || b.busy == "" {
		t.Fatal("start did not run the action")
	}
	if refused := b.start("Open PR-12302's pane", func(context.Context) (ActionResult, error) {
		t.Error("a second action ran while one ran")
		return ActionResult{}, nil
	}); refused == nil {
		t.Error("a refused start asked for no note's timer")
	}
	mustContain(t, barView(&b, 140), "wait: Open PR-12301's pane runs")
	deliver(t, &b, execCmd(t, cmd)...)
	mustContain(t, barView(&b, 140), "focused the writer's pane")
	if entries, _ := log.snapshot(); len(entries) != 1 || entries[0].what != "Open PR-12301's pane" {
		t.Errorf("log = %+v, want the one action", entries)
	}
}

func TestFactoryBarFollowsTheTerminalBackground(t *testing.T) {
	b, _ := testFactoryBar(t.Context())
	b.ask("Push PR-12301?", func(context.Context) (ActionResult, error) { return ActionResult{}, nil })
	dark := b.view(80)
	b.applyStyles(newStyles(false))
	if light := b.view(80); light == dark || ansi.Strip(light) != ansi.Strip(dark) {
		t.Errorf("the light palette draws %q, the dark one %q: want the same text in other colors", light, dark)
	}
}
