package tui

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestPRBoardRenderCostRealRows(t *testing.T) {
	path := os.Getenv("MAGNUM_ROWS_JSON")
	if path == "" {
		t.Skip("MAGNUM_ROWS_JSON not set")
	}
	b, _ := os.ReadFile(path)
	var rows []PRBoardRow
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	m, src, _ := newBoard(t, 160, 45, PRBoardOptions{})
	src.rows = rows
	n, _ := m.Update(prbDataMsg{rows: rows})
	m = n.(prBoardModel)
	n, _ = m.Update(keyMsg("G"))
	m = n.(prBoardModel)
	start := time.Now()
	for range 20 {
		_ = m.View()
	}
	t.Logf("render at the bottom: %v per frame (%d rows)", time.Since(start)/20, len(rows))
	n, _ = m.Update(keyMsg("g"))
	m = n.(prBoardModel)
	start = time.Now()
	for range 20 {
		_ = m.View()
	}
	t.Logf("render at the top: %v per frame", time.Since(start)/20)
}
