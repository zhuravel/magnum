package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Your own open PR on the board is marked with its autofix = "open"
// verdict: a tag before the title ("open: needs you", "open: none" for
// nothing open, "open: looking" for a stale verdict) and a "verdict:" line
// on its card in magnum's words; a PR of someone else's has neither, nor
// has a merged one.
func TestTheBoardMarksOwnPRsWithTheVerdict(t *testing.T) {
	m, _ := snoozeBoard(t, snzOpen, time.Time{})
	p := m.painter()
	own := PRBoardRow{Ref: "talkable/talkable#2", Owner: "talkable", Repo: "talkable", Number: 2, State: "reviewed", GHState: "OPEN",
		Title: "Guard the owner", Author: "zhuravel",
		Open: &PROpen{Verdict: "needs_you", Why: "1 item needs you: 1 thread of a person", Head: "abc1234"}}
	if title := ansi.Strip(p.titleCell(own).render(nil)); !strings.Contains(title, "open: needs you Guard the owner") {
		t.Errorf("title cell = %q", title)
	}
	card := ansi.Strip(strings.Join(p.cardContent(own, 160), "\n"))
	if !strings.Contains(card, "verdict: needs you · 1 item needs you: 1 thread of a person") {
		t.Errorf("the card lacks the verdict:\n%s", card)
	}
	for verdict, tag := range map[string]string{"idle": "open: none", "waits": "open: waits", "unknown": "open: looking", "off": "open: off"} {
		r := own
		r.Open = &PROpen{Verdict: verdict, Why: "x", Head: "abc1234"}
		if title := ansi.Strip(p.titleCell(r).render(nil)); !strings.Contains(title, tag+" Guard the owner") {
			t.Errorf("%s: title cell = %q, want %q", verdict, title, tag)
		}
	}
	stale := own
	stale.Open = &PROpen{Verdict: "idle", Why: "nothing open at abc1234", Head: "0000000", Stale: true}
	if title := ansi.Strip(p.titleCell(stale).render(nil)); !strings.Contains(title, "open: looking") {
		t.Errorf("a stale verdict's title cell = %q, want looking", title)
	}
	if card := ansi.Strip(strings.Join(p.cardContent(stale, 160), "\n")); !strings.Contains(card, "verdict: looking · magnum's last word (idle) no longer tells the PR's state") {
		t.Errorf("a stale verdict's card:\n%s", card)
	}
	other := own
	other.Open, other.Author = nil, "alice"
	if title := ansi.Strip(p.titleCell(other).render(nil)); strings.Contains(title, "open:") {
		t.Errorf("another's PR is tagged: %q", title)
	}
	merged := own
	merged.GHState = "MERGED"
	if title := ansi.Strip(p.titleCell(merged).render(nil)); strings.Contains(title, "open:") {
		t.Errorf("a merged PR is tagged: %q", title)
	}
	off := own
	off.FixOff, off.Open = true, &PROpen{Verdict: "off", Why: "fix off", Head: "abc1234"}
	if title := ansi.Strip(p.titleCell(off).render(nil)); strings.Count(title, "off") != 1 {
		t.Errorf("a fix-off PR says off twice: %q", title)
	}
}

// With [board] own_prs = "factory" the status rule counts your open PRs
// the board leaves to the factory screen, while it has nothing else to say.
func TestTheBoardsFooterCountsYourPRsOnTheFactoryScreen(t *testing.T) {
	m, _, _ := newBoard(t, 170, 40, PRBoardOptions{OwnElsewhere: func() int { return 3 }})
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{actPR(nil)}, ownElsewhere: 3})
	mustContain(t, viewOf(m), "3 of your PRs are on the factory screen (own_prs)")
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{actPR(nil)}})
	mustNotContain(t, viewOf(m), "on the factory screen")
}
