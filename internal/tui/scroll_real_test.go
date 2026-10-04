package tui

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// One-off reproduction against real rows exported as JSON ([]PRBoardRow) in
// MAGNUM_ROWS_JSON; skipped otherwise.
func TestPRBoardRealRowsDoNotHang(t *testing.T) {
	path := os.Getenv("MAGNUM_ROWS_JSON")
	if path == "" {
		t.Skip("MAGNUM_ROWS_JSON not set")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []PRBoardRow
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d rows", len(rows))
	for _, size := range [][2]int{{80, 24}, {120, 40}, {100, 12}, {60, 30}, {200, 60}, {40, 10}} {
		m, src, _ := newBoard(t, size[0], size[1], PRBoardOptions{})
		src.rows = rows
		next, _ := m.Update(prbDataMsg{rows: rows})
		m = next.(prBoardModel)
		keys := []string{"G", "k", "j", "enter", "j", "j", "pgdown", "esc", "end", "enter", "G", "esc", "g"}
		for range len(rows) + 5 {
			keys = append(keys, "j")
		}
		keys = append(keys, "enter", "j", "j", "esc", "k", "k", "G")
		for i, k := range keys {
			done := make(chan struct{})
			go func() {
				defer close(done)
				n, _ := m.Update(keyMsg(k))
				m = n.(prBoardModel)
				_ = m.View()
				// a data refresh while scrolled: same rows, fewer rows, reordered rows, then a resize
				if i%7 == 0 {
					rev := append([]PRBoardRow(nil), rows...)
					for a, b := 0, len(rev)-1; a < b; a, b = a+1, b-1 {
						rev[a], rev[b] = rev[b], rev[a]
					}
					for _, rs := range [][]PRBoardRow{rows, rows[:len(rows)/2], rev, rows} {
						n, _ = m.Update(prbDataMsg{rows: rs})
						m = n.(prBoardModel)
						_ = m.View()
					}
					n, _ = m.Update(tea.WindowSizeMsg{Width: size[0] - 5, Height: size[1] - 2})
					m = n.(prBoardModel)
					_ = m.View()
				}
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("size %v: hung at key %d (%q) cursor=%d scroll=%d rows=%d", size, i, k, m.cursor, m.scroll, len(m.view))
			}
		}
	}
}
