package notify

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/herdr"
)

// The osascript fallback logs at Warn when herdr failed, and at Info when
// herdr is up with nobody watching, which is no fault.
func TestTheOsascriptFallbackLogsAtWarnOnlyWhenHerdrFailed(t *testing.T) {
	for name, c := range map[string]struct {
		set    func(*fakeHerdr)
		urgent bool
		line   string
		want   slog.Level
	}{
		"unavailable":        {func(h *fakeHerdr) { h.showErr = unavailable() }, false, "notify: herdr unavailable: ", slog.LevelWarn},
		"urgent unavailable": {func(h *fakeHerdr) { h.showErr = unavailable() }, true, `notify: urgent "T": `, slog.LevelWarn},
		"no client": {func(h *fakeHerdr) { h.showRes = herdr.NotificationResult{Reason: "no_foreground_client"} }, false,
			"notify: herdr has no foreground client; falling back to osascript", slog.LevelInfo},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHerdr()
			c.set(h)
			log := &logBuf{}
			n := &Notifier{Herdr: h, Runner: osascriptFake(), Enabled: true, Sleep: (&sleeper{}).sleep, Log: log}
			send := n.Toast
			if c.urgent {
				send = n.ToastUrgent
			}
			if sent, err := send(context.Background(), "", "T", "B", 0); err != nil || !sent {
				t.Fatalf("toast = %v, %v", sent, err)
			}
			for i, l := range log.lines {
				if strings.HasPrefix(l, c.line) {
					if log.levels[i] != c.want {
						t.Fatalf("%q logged at %v, want %v", l, log.levels[i], c.want)
					}
					return
				}
			}
			t.Fatalf("no line %q in %q", c.line, log.lines)
		})
	}
}
