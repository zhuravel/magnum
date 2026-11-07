package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// fixturePRCard is the card of your PR #4602 of example/widget: what its
// fixer left you under WAITING FOR, a link of what needs you, and the PR's
// own lines: its head and branches, CI failing on a required check,
// magnum's last review, the people's reviews, the replies magnum's judge
// has not read and the fix loop. It has no stages, agents or report: it is
// no task's.
func fixturePRCard() TaskCard {
	return TaskCard{
		Key: "widget#4602", Title: "Coupon rounding", Repo: "example/widget", PR: "#4602",
		PRURL: "https://github.com/example/widget/pull/4602", Issue: "PR-12400", IssueURL: "https://example.atlassian.net/browse/PR-12400",
		WaitingFor: "you: claude-fix left you 1 item: e opens its report",
		FixNeeds:   []string{"P2 app/models/coupon.rb:42: round half up or to even? the ticket does not say"},
		FixReport:  fixerReport,
		Links:      []string{"https://github.com/example/widget/pull/4602#discussion_r101"},
		PRInfo: &CardPR{
			State: "open · head a1b2c3d · alice-PR-12400-coupon-rounding → master · last activity 13:17",
			CI: &CIInfo{State: "failed", Total: 41, Passed: 39, Failed: 1, Skipped: 1, Failing: []string{"ci / Completion"},
				Required: []CheckState{{Name: "Completion", State: "failed"}}},
			Review:    "magnum commented at a1b2c3d, 13:02 · earlier findings: 1 fixed, 1 open",
			Findings:  &FindingsInfo{Counts: [4]int{0, 0, 1, 2}, Verdict: "blocking", Posted: "COMMENTED", SHA: "a1b2c3d"},
			Approvals: &FactoryApprovals{Approved: 1, Decision: "REVIEW_REQUIRED"},
			Reviewers: "approved by rev-bob · asked rev-ann 2h ago",
			Threads:   "2 replies magnum's judge has not read: rev-ann (in magnum's thread) and alice",
			FixLoop:   "on: magnum's fixer takes its findings (autofix \"open\") · F turns it off",
		},
	}
}

// Your PR's card draws the PR's own lines under WAITING FOR, each under
// its label (PR, CI, REVIEW, APPROVALS, THREADS, FIX LOOP) with the board's
// CI, findings and approvals cells, and no STAGES of a task it is not; at
// every width no line passes the screen. A published task's card draws the
// same lines before its STAGES.
func TestThePRCardDrawsThePRsLines(t *testing.T) {
	for _, w := range []int{100, 120, 140, 200} {
		checkFactoryFrame(t, fmt.Sprintf("prcard-%d.txt", w), plainCard(drawTask(fixturePRCard(), 0), w), w)
	}
	frame := plainCard(drawTask(fixturePRCard(), 0), 140)
	for _, label := range []string{"PR", "CI", "REVIEW", "APPROVALS", "THREADS", "FIX LOOP"} {
		if l := cardLineWith(t, frame, " "+label+" "); !strings.HasPrefix(l, " "+label) {
			t.Errorf("the %s line starts %q", label, l)
		}
	}
	mustNotContain(t, frame, "STAGES", "AGENTS")
	for _, mode := range []IconMode{IconsUnicode, IconsNerd, IconsASCII} {
		for _, w := range factoryWidths(40) {
			for i, l := range strings.Split(ansi.Strip(drawTask(fixturePRCard(), 0)(w, 0, true, mode)), "\n") {
				if lw := ansi.StringWidth(l); lw > w {
					t.Errorf("%s %d: line %d is %d cells wide: %q", mode, w, i+1, lw, l)
				}
			}
		}
	}
	task := fixturePublishedCard(1)
	task.PRInfo = fixturePRCard().PRInfo
	frame = plainCard(drawTask(task, 0), 140)
	if pr, stages := strings.Index(frame, " FIX LOOP "), strings.Index(frame, "STAGES"); pr < 0 || stages < 0 || pr > stages {
		t.Errorf("a published task's card draws its PR's lines at %d and STAGES at %d, want the PR's first:\n%s", pr, stages, frame)
	}
}

// yoursCardData is yoursData with #4602's card as fixturePRCard draws it.
func yoursCardData() FactoryData {
	d := yoursData()
	c := fixturePRCard()
	c.Key, c.Repo, c.PRURL = "#4602", "example/talkable", "https://github.com/example/talkable/pull/4602"
	d.Cards[-72] = FactoryCards{Task: c}
	return d
}

// enter on your PR's row opens its card, whose keys are the PR's: f, F and
// K ask y/N as on home, o steps into magnum's pane, b and t open its pages,
// e its fixer's report; x and d, a task's drop and diff, say so and run
// nothing; esc goes back. Before, your PR's row had no card and enter only
// named its keys.
func TestEnterOpensYourPRsCard(t *testing.T) {
	h := testYours(t, 140, 40)
	h.src.change(func(d *FactoryData) { *d = yoursCardData() })
	h.press("ctrl+r", "g")
	h.selectRow("#4602")
	mustContain(t, h.view(), "enter PR card")
	h.press("enter")
	if h.m.view != fvTask || h.m.task != -72 {
		t.Fatalf("enter on your PR's row opened view %v of %d, want its card", h.m.view, h.m.task)
	}
	v := h.view()
	mustContain(t, v, "#4602", "Coupon rounding", "APPROVALS", "approved by rev-bob", "FIX LOOP", "f fix", "F fix off", "o magnum's pane",
		"b PR", "t Jira", "e claude-fix.md")
	mustNotContain(t, v, "STAGES", "d diff", "r retry")
	h.press("x")
	mustContain(t, h.view(), "x drops a task: #4602 is your PR (K stops magnum's loop)")
	h.press("d")
	mustContain(t, h.view(), "no diff of the factory's for #4602: b opens the PR")
	h.press("f")
	mustContain(t, h.view(), "Fix #4602: magnum's fixer takes what is open now? y/N")
	h.press("y", "o", "b", "t")
	if len(h.act.fixes) != 1 || len(h.act.openPRs) != 1 || len(h.act.stops) != 0 || len(h.act.diffs) != 0 {
		t.Errorf("fixes %q, magnum's panes %q, drops %v, diffs %v; want one fix and one pane", h.act.fixes, h.act.openPRs, h.act.stops, h.act.diffs)
	}
	if got := h.act.opened(); !slices.Equal(got, []string{"https://github.com/example/talkable/pull/4602", "https://example.atlassian.net/browse/PR-12400"}) {
		t.Errorf("b and t opened %q", got)
	}
	h.press("esc")
	if h.m.view != fvHome {
		t.Errorf("esc left the card for view %v", h.m.view)
	}
}

// Your PR's card that is no longer on home (merged, closed) goes back home
// and says so, rather than drawing an empty card.
func TestYourPRsCardGoesWhenItsPRDoes(t *testing.T) {
	h := testYours(t, 140, 40)
	h.press("g")
	h.selectRow("#4603")
	h.press("enter")
	h.src.change(func(d *FactoryData) {
		d.Rows = slices.DeleteFunc(d.Rows, func(r FactoryRow) bool { return r.ID == -73 })
		delete(d.Cards, -73)
	})
	h.clock.add(time.Second)
	h.press("ctrl+r")
	if h.m.view != fvHome {
		t.Fatalf("the card of a PR gone from home stays: view %v", h.m.view)
	}
	mustContain(t, h.view(), "#4603 is no longer open")
}
