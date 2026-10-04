package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func lineCount(view string) int { return len(strings.Split(view, "\n")) }

// TestDashboardRowsFitNarrowScreens: the column minimums add up past 40
// cells, so rows are cut to the screen instead of overflowing it.
func TestDashboardRowsFitNarrowScreens(t *testing.T) {
	for _, w := range []int{20, 30, 40, 60} {
		m, _, _ := newDash(t, w, 60)
		m, _ = send(t, m, keys("w")...) // manual worktrees too
		v := viewOf(m)
		if got := maxLineWidth(v); got > w {
			t.Errorf("width %d: a line is %d cells:\n%s", w, got, v)
		}
		mustContain(t, v, "SLOTS (2)", "QUEUE (2)")
	}
}

// TestDashboardFooterStaysOnShortScreens: pauses grow the header; the
// footer (which asks the y/N) must stay on screen, the header is cut.
func TestDashboardFooterStaysOnShortScreens(t *testing.T) {
	for _, h := range []int{3, 4, 6, 8, 12} {
		m, src, _ := newDash(t, 80, h)
		d := src.data
		for i := range 4 {
			d.Pauses = append(d.Pauses, Pause{Key: fmt.Sprintf("identity:bot%d", i), Reason: "token expired"})
		}
		m, _ = send(t, m, dashDataMsg{data: d}, keyMsg("x"))
		v := viewOf(m)
		if n := lineCount(v); n > h {
			t.Errorf("height %d: %d lines:\n%s", h, n, v)
		}
		mustContain(t, v, "y/N")
		if h >= 6 {
			mustContain(t, v, "magnum status", "review1") // the title and the cursor row
		}
	}
}

func TestDashboardHelpFitsAndScrolls(t *testing.T) {
	m, _, _ := newDash(t, 80, 24)
	m, _ = send(t, m, keys("?")...)
	v := viewOf(m)
	if n, w := lineCount(v), maxLineWidth(v); n > 24 || w > 80 {
		t.Fatalf("help is %d lines, %d cells wide at 80x24:\n%s", n, w, v)
	}
	mustContain(t, v, "Keys", "help lines 1-", "j/k scroll", "close help")
	mustNotContain(t, v, "q, esc")

	m, _ = send(t, m, keys("G")...)
	v = viewOf(m)
	mustContain(t, v, "q, esc", "close this help")
	mustNotContain(t, v, "Keys")
	if n := lineCount(v); n > 24 {
		t.Fatalf("scrolled help is %d lines:\n%s", n, v)
	}

	m, _ = send(t, m, keys("g")...)
	mustContain(t, viewOf(m), "Keys")
	m, _ = send(t, m, keys("?")...)
	if m.showHelp || m.helpScroll != 0 {
		t.Fatal("? did not close the help")
	}
}

// TestDashboardScrollsPastTheLastRow: attention, manual worktrees and
// warnings sit under the queue; moving down past the last row scrolls
// them into view, and moving up brings the cursor row back first.
func TestDashboardScrollsPastTheLastRow(t *testing.T) {
	m, src, _ := newDash(t, 100, 20)
	d := src.data
	d.Warnings = []string{"herdr: socket unreachable", "mysql: access denied"}
	m, _ = send(t, m, dashDataMsg{data: d}, keyMsg("w"))
	mustNotContain(t, viewOf(m), "mysql: access denied")

	m, _ = send(t, m, keys("end")...)
	if m.cursor != len(m.rows)-1 {
		t.Fatalf("cursor = %d, want the last row", m.cursor)
	}
	v := viewOf(m)
	mustContain(t, v, "mysql: access denied", "feature/x")

	// Up scrolls back until the cursor row shows, then moves it.
	for range 20 {
		m, _ = send(t, m, keys("k")...)
		if m.cursor != len(m.rows)-1 {
			break
		}
	}
	if m.cursor != len(m.rows)-2 {
		t.Fatalf("cursor = %d after scrolling back up", m.cursor)
	}

	// Step by step works too.
	m, _ = send(t, m, keys("end", "home")...)
	for range 30 {
		m, _ = send(t, m, keys("j")...)
	}
	mustContain(t, viewOf(m), "mysql: access denied")
}

func TestDashboardScrollsWithoutRows(t *testing.T) {
	m, src, _ := newDash(t, 100, 12)
	d := src.data
	d.Slots, d.Queue = nil, nil
	for i := range 15 {
		d.Warnings = append(d.Warnings, fmt.Sprintf("source %d unreadable", i))
	}
	m, _ = send(t, m, dashDataMsg{data: d})
	mustNotContain(t, viewOf(m), "source 14 unreadable")
	for range 30 {
		m, _ = send(t, m, keys("j")...)
	}
	mustContain(t, viewOf(m), "source 14 unreadable")
	m, _ = send(t, m, tea.KeyPressMsg{Code: tea.KeyHome})
	mustContain(t, viewOf(m), "SLOTS (0)")
}

// TestDashboardSanitizesStatus: text quoting git, GitHub or agent output
// must not reach the terminal as escape sequences.
func TestDashboardSanitizesStatus(t *testing.T) {
	m, src, _ := newDash(t, 140, 50)
	d := src.data
	evil := "\x1b]8;;https://example.com/x\x07click\x1b]8;;\x07 \x1b[31mred\x1b[0m\x07"
	d.Attention = []AttentionRow{{Subject: "talkable#3", Kind: "needs_attention", Message: "judge failed: " + evil}}
	d.Pauses = []Pause{{Key: "codex", Reason: "limit " + evil}}
	d.Warnings = []string{"warn " + evil}
	d.Queue[0].Title = "title " + evil
	m, _ = send(t, m, dashDataMsg{data: d})
	raw := m.View().Content
	for _, bad := range []string{"\x1b]8;;https://example.com", "\x1b[31mred", "\x07"} {
		if strings.Contains(raw, bad) {
			t.Errorf("the view carries %q", bad)
		}
	}
	mustContain(t, viewOf(m), "judge failed: click red", "limit click red", "warn click red")
	if !strings.Contains(d.Attention[0].Message, "\x1b") || !strings.Contains(d.Queue[0].Title, "\x1b") {
		t.Error("sanitizing changed the caller's data")
	}
}
