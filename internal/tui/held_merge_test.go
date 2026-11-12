package tui

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

const (
	// heldDiff is the held-merge.diff of PR #5's merge that magnum holds
	// for your y.
	heldDiff = "/home/op/.local/share/magnum/reviews/talkable/talkable/5/held-merge.diff"
	// heldMergeSHA is that merge.
	heldMergeSHA = "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"
	// heldAttention is its attention as the board's row has it.
	heldAttention = "fix: merge ready: claude-fix merged master at c0c0c0c and resolved 5 conflicts. It changes .github/workflows/ci.yml and pronto.yml."
)

// heldPushAsk is the question of a held merge's push the daemon's plan
// puts: the workflow files, the bot and the Workflows token.
func heldPushAsk(ref string) string {
	return "Merge the parent of " + ref + ": magnum pushes claude-fix's merge of master at c0c0c0c into feature as fixer-app[bot] with the " +
		"Workflows token; it changes .github/workflows/ci.yml and pronto.yml (held-merge.diff)?"
}

// heldPlanAsked is what that plan asks about: mergePlanAsked's with five
// conflicts and the held merge.
func heldPlanAsked() MergeAsked {
	a := mergePlanAsked
	a.Conflicts, a.Merge = 5, heldMergeSHA
	return a
}

// recordPager makes m's pager one that records the files it opens.
func recordPager(b *actionBar) *[]string {
	var opened []string
	b.pager = func(_ context.Context, path string) *exec.Cmd {
		opened = append(opened, path)
		return exec.Command("true")
	}
	return &opened
}

// heldBoard is the board with PR #5 needing you for the merge magnum holds
// for your y.
func heldBoard(t *testing.T) (prBoardModel, *fakeActions, *[]string) {
	t.Helper()
	m, act := fixBoard(t, func(r *PRBoardRow) {
		r.State, r.AttentionKind, r.LastError, r.HeldMerge = "needs_attention", "fix", heldAttention, heldDiff
	})
	return m, act, recordPager(&m.actionBar)
}

// On a board row whose merge of the parent magnum holds for your y, the
// state cell says "merge ready" on the row's one line, the hints offer e
// and held-merge.diff, and e opens it in the pager, asking nothing; e on
// a row without one says so and opens nothing.
func TestEOnABoardRowWithAHeldMergeOpensItsDiffInThePager(t *testing.T) {
	m, act, opened := heldBoard(t)
	view := ansi.Strip(viewOf(m))
	mustContain(t, view, "merge ready", "e held-merge.diff")
	if line := lineWith(t, view, "#5"); !strings.Contains(line, "merge ready") {
		t.Errorf("the row = %q, want merge ready on its one line", line)
	}
	m, cmd := send(t, m, keyMsg("e"))
	if cmd == nil || !slices.Equal(*opened, []string{heldDiff}) {
		t.Fatalf("e opened %q, want %q", *opened, heldDiff)
	}
	if m.confirm != nil || len(act.calls) != 0 {
		t.Errorf("e asked %+v or acted %v", m.confirm, act.calls)
	}
	m, _ = send(t, m, heldPagerMsg{})
	if m.flash != "" {
		t.Errorf("a pager that ended well left %q", m.flash)
	}
	m, _ = send(t, m, keyMsg("enter")) // the card's hints offer it too
	mustContain(t, ansi.Strip(viewOf(m)), "e held-merge.diff")

	plain, _ := fixBoard(t, nil)
	none := recordPager(&plain.actionBar)
	mustNotContain(t, ansi.Strip(viewOf(plain)), "e held-merge.diff", "merge ready")
	plain, _ = send(t, plain, keyMsg("e"))
	mustContain(t, viewOf(plain), "no merge of the parent is held for your y on talkable#5: e opens its held-merge.diff while magnum holds one")
	if len(*none) != 0 {
		t.Errorf("e on a row without a held merge opened %q", *none)
	}
}

// P on a held merge asks the y/N of its push that the daemon's plan words,
// naming the workflow files, the bot and the Workflows token, and y sends
// it bound to the merge besides the head and the tip; n leaves it held
// (nothing is sent).
func TestPOnAHeldMergeAsksItsPushAndBindsTheMerge(t *testing.T) {
	m, act, _ := heldBoard(t)
	asked := heldPlanAsked()
	act.held = &asked
	m, _ = boardAct(t, m, "P")
	if m.confirm == nil || m.confirm.question != heldPushAsk("talkable/talkable#5") {
		t.Fatalf("P asked %+v, want %q", m.confirm, heldPushAsk("talkable/talkable#5"))
	}
	if n, _ := send(t, m, keyMsg("n")); n.confirm != nil || len(act.calls) != 1 {
		t.Errorf("n acted or left the question up: %v", act.calls)
	}
	m, _ = boardAct(t, m, "y")
	if want := mergeCall("talkable/talkable#5", asked); !strings.HasSuffix(want, ", the held merge d3d3d3d") || act.last() != want {
		t.Errorf("y called %v, want %q", act.calls, want)
	}
}

// The dashboard's e opens the held-merge.diff of the selected slot's PR,
// asking nothing, and its hints offer it there; an empty slot has none.
func TestEOnTheDashboardOpensTheHeldMergeOfTheRowsPR(t *testing.T) {
	m, src, act := newDash(t, 160, 40)
	data := dashData()
	data.Slots[0].PRHeldMerge = heldDiff
	src.data = data
	m, _ = send(t, m, dashDataMsg{data: data})
	opened := recordPager(&m.actionBar)
	mustContain(t, ansi.Strip(viewOf(m)), "e held-merge.diff")
	m, _ = send(t, m, keyMsg("e"))
	if !slices.Equal(*opened, []string{heldDiff}) || m.confirm != nil || len(act.calls) != 0 {
		t.Fatalf("e opened %q (asked %+v, acted %v), want %q alone", *opened, m.confirm, act.calls, heldDiff)
	}
	m, _ = send(t, m, keyMsg("j"))
	mustNotContain(t, ansi.Strip(viewOf(m)), "e held-merge.diff")
	m, _ = send(t, m, keyMsg("e"))
	mustContain(t, viewOf(m), "no merge of the parent is held for your y on this row")
	if len(*opened) != 1 {
		t.Errorf("e on an empty slot opened %q", *opened)
	}
}

// A pager that fails says so until a key is pressed.
func TestAHeldMergesPagerThatFailsSaysSo(t *testing.T) {
	m, _, _ := heldBoard(t)
	m, _ = send(t, m, heldPagerMsg{err: errNothingFailed})
	mustContain(t, viewOf(m), "held-merge.diff: the pager failed")
}

// The factory screen's e opens held-merge.diff first while magnum holds a
// merge of the parent for your y: the PR's card names it in the fixer's
// report's place, so the hints and the menu offer it by its name.
func TestTheFactoryScreensEOpensAHeldMergesDiff(t *testing.T) {
	h, opened := testFixer(t)
	h.src.change(func(d *FactoryData) {
		cards := d.Cards[-72]
		cards.Task.FixReport = heldDiff
		d.Cards[-72] = cards
	})
	h.press("ctrl+r", "g")
	h.selectRow("#4602")
	mustContain(t, h.view(), "e held-merge.diff")
	if got := menuState(h.m.menuItems()); !got["read held-merge.diff"] {
		t.Errorf("the menu of #4602: %v, want held-merge.diff", got)
	}
	h.press("e")
	if !slices.Equal(*opened, []string{heldDiff}) || h.m.bar.asking != nil {
		t.Errorf("e opened %q (asked %v), want %q", *opened, h.m.bar.asking != nil, heldDiff)
	}
}
