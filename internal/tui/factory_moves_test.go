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
// the one that failed (c, the CI fix, or its CI when the CI fix is
// refused), the threads, the fixer's report, magnum's review, open
// findings, and the PR's page for the rest.
var factoryMoves = [][2]string{
	{"b: re-run CI on GitHub, or push", "b re-run CI"},
	{"c fixes its CI, b opens it", "c CI fix"},
	{"b opens its CI", "b CI"},
	{"b opens the threads", "b threads"},
	{"e opens the report, b the PR", "e report · b PR"},
	{"P pushes it, e the diff", "P push · e diff"},
	{"r reviews it", "r review"},
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
// 100 columns (the column's floor there is 15 cells), and at 140 and 200,
// where this table keeps the wide layout, the long form of a move up to the
// wide column's 28 cells and the short form of a wider one ("b: re-run CI on
// GitHub, or push" is 31), never cut.
func TestEveryMoveOfYourPRFitsTheMoveColumn(t *testing.T) {
	for _, w := range []int{100, 140, 200} {
		frame, _ := factoryFrameOf(fixtureMoves(), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		plain := ansi.Strip(frame)
		checkFactoryFrame(t, fmt.Sprintf("moves-%d.txt", w), plain, w)
		for _, m := range factoryMoves {
			want := m[0]
			if w == 100 || ansi.StringWidth(m[0]) > factoryMoveWide {
				want = m[1]
			}
			if !strings.Contains(plain, "  "+want+" ") {
				t.Errorf("%d columns: the move %q is cut or missing", w, want)
			}
		}
	}
}

// In the wide layout a move wider than the column's floor takes its short
// form, so one row's long move never steps the whole table down: with a
// 31-cell move on one row, the table of two repositories keeps its wide
// layout (CI "41/41", the other rows' long moves) from the first width that
// holds it. Before, the move widened the column and the layout fell to the
// 140-column one between 184 and 190 columns.
func TestAWideMoveNeverStepsTheWholeTableDown(t *testing.T) {
	const long, short = "b: re-run CI on GitHub, or push", "b re-run CI"
	f := fixtureTwoRepos()
	for i := range f.Rows {
		if f.Rows[i].Key == "widget#4602" {
			f.Rows[i].Move, f.Rows[i].MoveShort = long, short
		}
	}
	for _, w := range []int{184, 186, 188, 190, 200} {
		frame, _ := factoryFrameOf(f, w, factoryReposHeight, true, IconsUnicode, factoryNow)
		plain := ansi.Strip(frame)
		mustContain(t, plain, "41/41", "read the diff, then push", "  "+short+" ")
		mustNotContain(t, plain, long)
	}
}
