package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// A PR waiting for a round shows why in its state cell (compact) and its
// card (the sentence with the command that lifts it).
func TestPRBoardShowsWhyAPRWaits(t *testing.T) {
	m, _, _ := newBoard(t, 160, 50, PRBoardOptions{})
	p := m.painter()
	var row PRBoardRow
	for _, r := range boardRows() {
		if r.Number == 11950 {
			row = r
		}
	}
	row.Wait = "re-review · cap 6/6 → 00:00"
	row.WaitDetail = "re-review waits for the daily round cap (6 of 6 today) until 00:00; `magnum review talkable#11950` runs it now"

	cell := ansi.Strip(p.stateWaitCell(sanitizeRow(row)).render(nil))
	if !strings.Contains(cell, "re-review") || !strings.HasSuffix(cell, " cap 6/6 → 00:00") {
		t.Errorf("state cell = %q", cell)
	}
	plain := ansi.Strip(p.stateWaitCell(PRBoardRow{State: "reviewed"}).render(nil))
	if strings.Contains(plain, "·") || strings.Contains(plain, "→") {
		t.Errorf("a PR without a wait shows one: %q", plain)
	}

	card := ansi.Strip(strings.Join(p.cardContent(sanitizeRow(row), 120), "\n"))
	mustContain(t, card, "Next review", "cap 6/6 → 00:00", "WAITING",
		"re-review waits for the daily round cap (6 of 6 today) until 00:00; `magnum review talkable#11950` runs it now")
	if c := ansi.Strip(strings.Join(p.cardContent(sanitizeRow(PRBoardRow{State: "reviewed"}), 120), "\n")); strings.Contains(c, "WAITING") {
		t.Errorf("a PR without a wait has a WAITING section:\n%s", c)
	}
}
