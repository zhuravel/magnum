package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// deployFirstRow is your own open PR whose head changes migration words
// names, with the look's idle verdict that leaves the merge to you, as
// the engine writes it for that PR (the factory's wait on your merge).
func deployFirstRow(words string) PRBoardRow {
	return PRBoardRow{Ref: "talkable/talkable#2", Owner: "talkable", Repo: "talkable", Number: 2, State: "reviewed", GHState: "OPEN",
		Title: "Add the index", Author: "zhuravel", DeployFirst: words,
		Open: &PROpen{Verdict: "idle", Why: "nothing open at abc1234 · approved, merge is yours", Head: "abc1234", Merge: true}}
}

// A PR that must go to a staging before its merge says so on its card in
// staging's words: in yellow with E's offer on your own PR whose fresh
// verdict leaves the merge to you (DeployNeedsYou: the look's idle verdict,
// approved or no review required), whose tag and verdict line then say it
// needs you; dimmed with E's offer on another own PR (no approval yet, a
// verdict that needs you for something else first, no verdict, a stale
// one); dimmed without it on a colleague's, whose merge is its author's:
// its "needs you" for your approval stays, and E refuses others' PRs. A row
// without the line shows nothing.
func TestTheCardSaysDeployBeforeMerge(t *testing.T) {
	m, _ := snoozeBoard(t, snzOpen, time.Time{})
	p := m.painter()
	const words = "deploy to a staging before the merge (it changes db/migrate/20261109_a.rb)"
	offer := words + " (" + rowActDefs[actStaging].key + " deploys it)"
	own := deployFirstRow(words)
	find := func(r PRBoardRow, s string) string {
		t.Helper()
		for _, l := range p.cardContent(r, 160) {
			if strings.Contains(ansi.Strip(l), s) {
				return l
			}
		}
		return ""
	}
	dim := func(l string) bool { return l != "" && l == p.st.Dim.Render(ansi.Strip(l)) }
	tag := func(r PRBoardRow) string { return ansi.Strip(p.titleCell(r).render(nil)) }
	if l := find(own, offer); l == "" || l != p.pal.yellow.Render(ansi.Strip(l)) || !own.DeployNeedsYou() {
		t.Errorf("your approved PR's deploy line %q (needs you %v), want it yellow with E", l, own.DeployNeedsYou())
	}
	if title := tag(own); !strings.Contains(title, "open: needs you Add the index") {
		t.Errorf("your approved PR's title cell = %q, want the needs-you tag", title)
	}
	if l := find(own, "verdict: "); ansi.Strip(l) != "verdict: needs you · nothing open at abc1234 · approved, merge is yours" {
		t.Errorf("your approved PR's verdict line = %q, want it to need you", ansi.Strip(l))
	}

	waiting := own
	waiting.Open = &PROpen{Verdict: "idle", Why: "nothing open at abc1234 · no approval yet", Head: "abc1234"}
	if l := find(waiting, offer); !dim(l) || waiting.DeployNeedsYou() {
		t.Errorf("your unapproved PR's deploy line %q (needs you %v), want it dimmed with E", l, waiting.DeployNeedsYou())
	}
	if title := tag(waiting); !strings.Contains(title, "open: none Add the index") {
		t.Errorf("your unapproved PR's title cell = %q, want the verdict's own tag", title)
	}
	red := own
	red.Open = &PROpen{Verdict: "needs_you", Why: "required check rspec failed at abc1234", Head: "abc1234"}
	if l := find(red, offer); !dim(l) || red.DeployNeedsYou() {
		t.Errorf("your PR whose CI failed: deploy line %q (needs you %v), want it dimmed: the CI comes first", l, red.DeployNeedsYou())
	}
	noVerdict := own
	noVerdict.Open = nil
	if l := find(noVerdict, offer); !dim(l) || noVerdict.DeployNeedsYou() {
		t.Errorf("your PR without a verdict: deploy line %q (needs you %v), want it dimmed with E", l, noVerdict.DeployNeedsYou())
	}
	stale := own
	stale.Open = &PROpen{Verdict: "idle", Why: own.Open.Why, Head: "0000000", Stale: true, Merge: true}
	if l := find(stale, offer); !dim(l) || stale.DeployNeedsYou() {
		t.Errorf("a stale verdict: deploy line %q (needs you %v), want it dimmed", l, stale.DeployNeedsYou())
	}
	gone := own
	gone.DeployFirst = "" // the board's read no longer says it: a pushed env
	if gone.DeployNeedsYou() || find(gone, "deploys it") != "" || strings.Contains(tag(gone), "needs you") {
		t.Errorf("a PR a staging holds needs you for the deploy, or offers E")
	}

	theirs := own
	theirs.Ref, theirs.Number, theirs.Author, theirs.Open, theirs.NeedsMe = "talkable/talkable#3", 3, "alice", nil, NeedsMeApprove
	if l := find(theirs, words); !dim(l) || strings.Contains(ansi.Strip(l), "deploys it") || theirs.DeployNeedsYou() {
		t.Errorf("a colleague's deploy line %q (needs you %v), want it dimmed without E", l, theirs.DeployNeedsYou())
	}
	if !strings.Contains(ansi.Strip(strings.Join(p.cardContent(theirs, 160), "\n")), "needs you") {
		t.Error("a colleague's PR that needs your approval lost its needs-you")
	}
}

// On a card narrower than the line, the line wraps rather than cut: the
// whole migration and E's offer show at 80 and 120 columns, with a live
// path of 76 characters.
func TestTheDeployLineWrapsOnANarrowCard(t *testing.T) {
	m, _ := snoozeBoard(t, snzOpen, time.Time{})
	p := m.painter()
	const words = "deploy to a staging before the merge (it changes db/sharded_migrate/20261105011921_add_metric_indexes_to_custom_activities.rb)"
	want := strings.ReplaceAll(words+" ("+rowActDefs[actStaging].key+" deploys it)", " ", "")
	for _, width := range []int{80, 120} {
		inner := cardInner(width)
		var text strings.Builder
		for _, l := range p.cardContent(deployFirstRow(words), inner) {
			if w := ansi.StringWidth(l); w > inner {
				t.Errorf("%d columns: a line of %d cells, want at most %d: %q", width, w, inner, ansi.Strip(l))
			}
			text.WriteString(strings.ReplaceAll(ansi.Strip(l), " ", ""))
		}
		if !strings.Contains(text.String(), want) {
			t.Errorf("%d columns: the card lacks the whole deploy line and E's offer: %q", width, text.String())
		}
	}
}
