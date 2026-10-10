package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// A PR whose fixer works says so in its state cell, "fixing 1/3 ·
// claude-fix" (a narrow cell drops the role), and on its card; GitHub's
// merge outranks it. A PR switched out of autofix carries "fix off" among
// its tags beside "muted", on the board and on the card.
func TestTheBoardCellReadsFixing(t *testing.T) {
	m, _ := snoozeBoard(t, snzOpen, time.Time{})
	p := m.painter()
	fixing := PRBoardRow{Ref: "talkable/talkable#2", Owner: "talkable", Repo: "talkable", Number: 2, State: "reviewed", GHState: "OPEN",
		Title: "Guard the owner", Fixing: "fixing 1/3 · claude-fix"}
	if cell := strings.TrimSpace(ansi.Strip(p.stateWaitCell(fixing).render(nil))); cell != "fixing 1/3 · claude-fix" {
		t.Errorf("state cell = %q", cell)
	}
	if cell := strings.TrimSpace(ansi.Strip(p.cells(fixing, [3]int{}).stateFit(len(" fixing 1/3 ")).render(nil))); cell != "fixing 1/3" {
		t.Errorf("narrow state cell = %q", cell)
	}
	if card := ansi.Strip(strings.Join(p.cardContent(fixing, 160), "\n")); !strings.Contains(card, "fixing 1/3 · claude-fix") {
		t.Errorf("the card does not say the fixer works:\n%s", card)
	}
	merged := fixing
	merged.GHState = "MERGED"
	if cell := ansi.Strip(p.stateWaitCell(merged).render(nil)); strings.Contains(cell, "fixing") {
		t.Errorf("a merged PR's state cell = %q", cell)
	}
	waiting := PRBoardRow{State: "reviewed", GHState: "OPEN"}
	if cell := ansi.Strip(p.stateWaitCell(waiting).render(nil)); strings.Contains(cell, "fixing") {
		t.Errorf("a PR without a fixer at work: state cell = %q", cell)
	}

	off := fixing
	off.Fixing, off.Muted, off.FixOff = "", true, true
	if title := ansi.Strip(p.titleCell(off).render(nil)); !strings.Contains(title, "muted fix off Guard the owner") {
		t.Errorf("title cell = %q", title)
	}
	if card := ansi.Strip(strings.Join(p.cardContent(off, 160), "\n")); !strings.Contains(card, "fix off") {
		t.Errorf("the card lacks fix off:\n%s", card)
	}
	if title := ansi.Strip(p.titleCell(fixing).render(nil)); strings.Contains(title, "fix off") {
		t.Errorf("a PR with autofix on: title cell = %q", title)
	}
}
