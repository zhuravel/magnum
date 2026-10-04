package tui

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestPRBoardRenderAtOddSizes(t *testing.T) {
	path := os.Getenv("MAGNUM_ROWS_JSON")
	if path == "" {
		t.Skip("MAGNUM_ROWS_JSON not set")
	}
	b, _ := os.ReadFile(path)
	var rows []PRBoardRow
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]int{{0, 0}, {1, 1}, {5, 3}, {10, 100}, {300, 80}, {500, 200}, {2000, 60}, {120, 0}, {0, 40}} {
		m, src, _ := newBoard(t, 120, 40, PRBoardOptions{})
		src.rows = rows
		n, _ := m.Update(prbDataMsg{rows: rows})
		m = n.(prBoardModel)
		n, _ = m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = n.(prBoardModel)
		n, _ = m.Update(keyMsg("G"))
		m = n.(prBoardModel)
		start := time.Now()
		v := m.View()
		d := time.Since(start)
		t.Logf("size %v: frame %d bytes in %v", size, len(v.Content), d)
		if d > 500*time.Millisecond || len(v.Content) > 2_000_000 {
			t.Errorf("size %v: pathological frame (%d bytes, %v)", size, len(v.Content), d)
		}
	}
}
