package tui

import (
	"strings"
	"testing"
	"time"
)

// TestTitlesNameAWatchWhosePollsFail: a watch whose radar calls fail is
// named after a pause (and a drain) and before the PRs that wait for the
// operator, "talkable polls failing 47m (HTTP 502)" whole and "talkable ✗
// 47m" short; a failure without a cause has no parenthesis. The frame key
// changes when one appears, so the cached titles redraw.
func TestTitlesNameAWatchWhosePollsFail(t *testing.T) {
	f := DaemonFacts{Paused: true, PausedSince: boardNow.Add(-time.Hour), NeedsMe: 2, PollsFailing: []WatchFailing{
		{Watch: "talkable", Since: boardNow.Add(-47 * time.Minute), Error: "HTTP 502"},
		{Watch: "example", Since: boardNow.Add(-12 * time.Minute)},
	}}
	facts := f.list(boardNow)
	if len(facts) != 4 || facts[0].short != "paused 1h" || !strings.HasPrefix(facts[3].full, "2 need your") {
		t.Fatalf("facts %+v", facts)
	}
	if got := facts[1]; got.full != "talkable polls failing 47m (HTTP 502)" || got.short != "talkable ✗ 47m" {
		t.Errorf("failing watch = %+v", got)
	}
	if got := facts[2]; got.full != "example polls failing 12m" || got.short != "example ✗ 12m" {
		t.Errorf("failing watch without a cause = %+v", got)
	}
	without := f
	without.PollsFailing = nil
	if f.key(boardNow) == without.key(boardNow) {
		t.Error("the frame key ignores the failing watches")
	}

	m := boardWithFacts(t, 400, 24, without)
	mustNotContain(t, viewOf(m), "polls failing")
	m, _ = send(t, m, prbDataMsg{rows: boardRows(), facts: f})
	mustContain(t, strings.Split(viewOf(m), "\n")[0], "talkable polls failing 47m (HTTP 502)", "example polls failing 12m")

	only := DaemonFacts{PollsFailing: f.PollsFailing[:1]}
	vs := defaultStyles.factVariants(only.list(boardNow), "   ")
	if len(vs) != 3 || !strings.Contains(vs[0], "talkable polls failing 47m (HTTP 502)") || !strings.Contains(vs[1], "talkable ✗ 47m") {
		t.Errorf("variants %q", vs)
	}
}

// TestFailingWatchTextIsCleaned: what the screens say of a failing watch
// keeps no terminal escapes or line breaks from the registry.
func TestFailingWatchTextIsCleaned(t *testing.T) {
	w := WatchFailing{Watch: "talk\x1b[31mable", Since: boardNow.Add(-47 * time.Minute), Error: "HTTP\n502\x1b]8;;x\x07"}
	got := w.Text(boardNow)
	if strings.ContainsAny(got, "\x1b\n\x07") || !strings.HasPrefix(got, "talkable polls failing 47m (HTTP 502") {
		t.Errorf("text %q", got)
	}
}

// TestDashboardPollLineNamesAWatchWhosePollsFail: while a watch's radar
// calls fail, the activity line names it with the daemon's last poll, each
// failing watch in turn.
func TestDashboardPollLineNamesAWatchWhosePollsFail(t *testing.T) {
	d := dashData()
	d.Activity.PollsFailing = []WatchFailing{
		{Watch: "talkable", Since: d.GeneratedAt.Add(-47 * time.Minute), Error: "HTTP 502"},
		{Watch: "example", Since: d.GeneratedAt.Add(-12 * time.Minute)},
	}
	m, _, _ := newDash(t, 200, 50)
	mustNotContain(t, viewOf(m), "polls failing")
	m, _ = send(t, m, dashDataMsg{data: d})
	mustContain(t, viewOf(m), "last poll 12s ago · talkable polls failing 47m (HTTP 502) · example polls failing 12m · last tick 2s ago")
}
