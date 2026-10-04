package tui

import (
	"fmt"
	"testing"
	"time"
)

// Reproduction: scrolling to the bottom of a board with more rows than the
// screen must not hang.
func TestPRBoardScrollToBottomDoesNotHang(t *testing.T) {
	for _, h := range []int{8, 12, 24, 40} {
		m, src, _ := newBoard(t, 120, h, PRBoardOptions{})
		// many rows: repeat the fixture with distinct numbers
		base := boardRows()
		var rows []PRBoardRow
		for i := range 60 {
			r := base[i%len(base)]
			r.Number = 20000 + i
			r.Ref = fmt.Sprintf("talkable/talkable#%d", r.Number)
			rows = append(rows, r)
		}
		src.rows = rows
		next, _ := m.Update(prbDataMsg{rows: rows})
		m = next.(prBoardModel)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for _, k := range []string{"j", "j", "pgdown", "pgdown", "pgdown", "pgdown", "pgdown", "pgdown", "pgdown", "G", "j", "j", "end", "k", "pgup", "g"} {
				n, _ := m.Update(keyMsg(k))
				m = n.(prBoardModel)
				_ = m.View()
			}
			for range 70 {
				n, _ := m.Update(keyMsg("j"))
				m = n.(prBoardModel)
				_ = m.View()
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("h=%d: scrolling to the bottom hung", h)
		}
	}
}
