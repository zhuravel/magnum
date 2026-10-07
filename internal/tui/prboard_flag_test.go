package tui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	flagShort    = "Codex flagged · never reviewed again"
	flagSentence = "Codex flagged this PR as a possible cybersecurity risk on Oct 7 15:58 (codex-judge, run r-own-1, head abc1234): " +
		"■ This content was flagged for possible cybersecurity risk. magnum never reviews it again, not even `magnum review`: " +
		"review it by hand; `magnum codex-flag clear talkable#11950` lifts the flag (Codex may block an account it flags)"
)

// flagBoard is a board of boardRows whose #11950 Codex flagged (skipped,
// muted with reason when muteReason is set), the cursor on it.
func flagBoard(t *testing.T, muteReason string) (prBoardModel, *fakeActions) {
	t.Helper()
	rows := boardRows()
	for i := range rows {
		if rows[i].Number == 11950 {
			rows[i].State, rows[i].SkipReason = "ineligible", "Codex flagged it as a possible cybersecurity risk: never reviewed again"
			rows[i].CodexFlag, rows[i].CodexFlagSentence = flagShort, flagSentence
			if muteReason != "" {
				rows[i].Muted, rows[i].MuteReason = true, muteReason
			}
		}
	}
	src := &fakeBoardSource{rows: rows}
	act := &fakeActions{}
	m := newPRBoardModel(context.Background(), src, act, PRBoardOptions{Now: func() time.Time { return boardNow }, SelfLogins: boardSelf})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 220, Height: 50}, prbDataMsg{rows: src.rows})
	at := slices.Index(boardRefs(m), snzOpen)
	if at < 0 {
		t.Fatalf("%s is not on the board: %v", snzOpen, boardRefs(m))
	}
	m.moveTo(at)
	return m, act
}

// A flagged PR's state cell reads "Codex flagged · never reviewed again" in
// one line (a narrow cell keeps "Codex flagged"), its card shows the pill and
// the sentence with the command that lifts it (no SKIPPED section offering R),
// and r and R refuse it, asking nothing.
func TestPRBoardShowsTheCodexFlagAndRefusesItsReview(t *testing.T) {
	m, act := flagBoard(t, "")
	p := m.painter()
	r, _ := m.selected()
	if cell := ansi.Strip(p.stateWaitCell(r).render(nil)); strings.TrimSpace(cell) != flagShort {
		t.Errorf("state cell = %q, want %q", cell, flagShort)
	}
	cs := p.cells(r, [3]int{})
	if narrow := ansi.Strip(cs.stateFit(16).render(nil)); strings.TrimSpace(narrow) != "Codex flagged" {
		t.Errorf("narrow state cell = %q", narrow)
	}
	card := ansi.Strip(strings.Join(p.cardContent(r, 160), "\n"))
	mustContain(t, card, flagShort, "Codex flagged this PR as a possible cybersecurity risk on Oct 7 15:58", "magnum codex-flag clear talkable#11950")
	if strings.Contains(card, "SKIPPED") || strings.Contains(card, "reviews it anyway") {
		t.Errorf("the card offers a review of a flagged PR:\n%s", card)
	}
	for _, k := range []string{"r", "R"} {
		m2, _ := send(t, m, keyMsg(k))
		if m2.confirm != nil || len(act.calls) != 0 {
			t.Fatalf("%s on a flagged PR asked %+v, ran %v", k, m2.confirm, act.calls)
		}
		mustContain(t, viewOf(m2), "talkable#11950: Codex flagged · never reviewed again")
	}

	// h (hide skipped) keeps a flagged row: it waits for a review by hand.
	if hiddenRow(r) {
		t.Error("a flagged row is hidden with the skipped ones")
	}
}

// A muted PR's card says why it was muted, on one line under its head, and
// a muted, flagged PR shows both lines; a mute without a reason adds none.
func TestPRBoardCardShowsTheMuteReason(t *testing.T) {
	m, _ := flagBoard(t, "waits for the security team")
	p := m.painter()
	r, _ := m.selected()
	card := ansi.Strip(strings.Join(p.cardContent(r, 160), "\n"))
	mustContain(t, card, "muted: waits for the security team", "Codex flagged this PR")
	if strings.Count(card, "muted: waits") != 1 || strings.Count(card, "Codex flagged this PR") != 1 {
		t.Errorf("a line shows twice:\n%s", card)
	}
	if i, j := strings.Index(card, "muted: waits"), strings.Index(card, "Codex flagged this PR"); i > j {
		t.Errorf("the mute's reason is not under the head:\n%s", card)
	}

	plain := boardRows()[5]
	plain.Muted = true
	if c := ansi.Strip(strings.Join(p.cardContent(plain, 160), "\n")); strings.Contains(c, "muted:") {
		t.Errorf("a mute without a reason shows one:\n%s", c)
	}
	long := plain
	long.MuteReason = strings.Repeat("a long reason ", 40)
	for _, l := range p.cardContent(long, 80) {
		if s := ansi.Strip(l); strings.Contains(s, "muted: a long") && ansi.StringWidth(s) > 80 {
			t.Errorf("the mute's reason is not clipped to the card: %q", s)
		}
	}
}
