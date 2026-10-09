package execx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
)

// attrBuf is an AttrLogger that records each line with its level and
// attributes.
type attrBuf struct{ levelBuf }

func (l *attrBuf) LogAttrs(level slog.Level, msg string, attrs ...slog.Attr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := level.String() + " " + msg
	for _, a := range attrs {
		line += " " + a.String()
	}
	l.lines = append(l.lines, line)
}

// printfBuf is a plain Logger: no levels.
type printfBuf struct{ lines []string }

func (l *printfBuf) Printf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// An audit event's mirror keeps the event's level and, for a logger that
// takes attributes, its subject and kind; a plain Logger still gets the line.
func TestLogEventKeepsTheEventsLevelAndSubject(t *testing.T) {
	var al attrBuf
	LogEvent(&al, "warn", "slot:a1", "slot.hold", "slots: slot:a1 slot.hold: held")
	LogEvent(&al, "error", "pr:o/r#1", "round.end", "round 1 ended: error")
	LogEvent(&al, "info", "pr:o/r#1", "round.start", "round 1")
	want := []string{
		"WARN slots: slot:a1 slot.hold: held subject=slot:a1 kind=slot.hold",
		"ERROR round 1 ended: error subject=pr:o/r#1 kind=round.end",
		"INFO round 1 subject=pr:o/r#1 kind=round.start",
	}
	if fmt.Sprint(al.lines) != fmt.Sprint(want) {
		t.Fatalf("attr logger got %q, want %q", al.lines, want)
	}

	var ll levelBuf
	LogEvent(&ll, "warn", "slot:a1", "slot.hold", "100% held")
	if want := []string{"WARN 100% held"}; fmt.Sprint(ll.lines) != fmt.Sprint(want) {
		t.Fatalf("level logger got %q, want %q", ll.lines, want)
	}

	var pl printfBuf
	LogEvent(&pl, "error", "slot:a1", "slot.hold", "100% held")
	LogAt(nil, slog.LevelWarn, "dropped") // a nil logger drops the line
	if want := []string{"100% held"}; fmt.Sprint(pl.lines) != fmt.Sprint(want) {
		t.Fatalf("plain logger got %q, want %q", pl.lines, want)
	}
}

func TestEventLevelMapsTheRegistrysLevels(t *testing.T) {
	for level, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError, "": slog.LevelInfo,
	} {
		if got := EventLevel(level); got != want {
			t.Errorf("EventLevel(%q) = %v, want %v", level, got, want)
		}
	}
}

// A tolerated failure logs at warn, unless it is the daemon stopping: a
// cancelled context, or an error that says it was cancelled.
func TestFailLevelKeepsAStopAtInfo(t *testing.T) {
	live := context.Background()
	stopped, cancel := context.WithCancel(live)
	cancel()
	for _, c := range []struct {
		name string
		ctx  context.Context
		err  error
		want slog.Level
	}{
		{"fault", live, errors.New("disk full"), slog.LevelWarn},
		{"stopped context", stopped, errors.New("disk full"), slog.LevelInfo},
		{"canceled error", live, fmt.Errorf("store: %w", context.Canceled), slog.LevelInfo},
		{"timeout", live, context.DeadlineExceeded, slog.LevelWarn},
	} {
		if got := FailLevel(c.ctx, c.err); got != c.want {
			t.Errorf("%s: FailLevel = %v, want %v", c.name, got, c.want)
		}
	}
}
