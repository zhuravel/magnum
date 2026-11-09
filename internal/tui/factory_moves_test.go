package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// factoryMoves are the moves of your PR that needs you, by reason, as
// factory.reasonMove words them (long, short): the check that did not run,
// the one that failed, the threads, the fixer's report, open findings, and
// the PR's page for the rest.
var factoryMoves = [][2]string{
	{"b opens it: re-run CI on GitHub, or push", "b re-run CI"},
	{"b opens its CI, f fixes it", "b CI · f fix"},
	{"b opens the threads", "b threads"},
	{"e opens the report, b the PR", "e report · b PR"},
	{"f fixes, b opens it", "f fix · b PR"},
	{"b opens it", "b PR"},
}

// fixtureMoves is home with one of your PRs for each move.
func fixtureMoves() factoryFrame {
	f := factoryFrame{Repos: []string{"example/talkable"}}
	for i, m := range factoryMoves {
		key := fmt.Sprintf("#46%02d", i)
		f.Rows = append(f.Rows, FactoryRow{
			ID: int64(-70 - i), Key: key, GitHub: key, Title: "Coupon rounding",
			Group: GroupNeedsYou, Mark: MarkNeedsYou,
			WaitsOn: []WaitsOn{{Who: WaitsOnYou, What: "1 item needs you: 1 thread of a person", Short: "1 item needs you", OnYou: true}},
			Move:    m[0], MoveShort: m[1],
			Since: factoryAgo(factoryNow, 25*time.Minute),
			Help:  "you: 1 item needs you: 1 thread of a person",
			PR:    &FactoryPR{Ref: "example/talkable#4602"},
		})
	}
	return f
}

// Every move of your PR fits the YOUR MOVE column whole: the short form at
// 100 columns (the column's floor there is 15 cells), the long form at 140
// and 200, never cut.
func TestEveryMoveOfYourPRFitsTheMoveColumn(t *testing.T) {
	for _, w := range []int{100, 140, 200} {
		frame, _ := factoryFrameOf(fixtureMoves(), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		plain := ansi.Strip(frame)
		checkFactoryFrame(t, fmt.Sprintf("moves-%d.txt", w), plain, w)
		for _, m := range factoryMoves {
			want := m[0]
			if w == 100 {
				want = m[1]
			}
			if !strings.Contains(plain, "  "+want+" ") {
				t.Errorf("%d columns: the move %q is cut or missing", w, want)
			}
		}
	}
}
