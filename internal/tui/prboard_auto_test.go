package tui

import (
	"strings"
	"testing"
	"time"
)

// autoRow is a reviewed PR magnum approved as the operator.
func autoRow(edit func(r *PRBoardRow)) PRBoardRow {
	return actPR(func(r *PRBoardRow) {
		r.Ref, r.Number = "talkable/talkable#9", 9
		r.Findings = &FindingsInfo{Verdict: "clean", Posted: "COMMENT", SHA: "abcdef1234"}
		r.AutoApproved = &AutoApproval{ReviewID: 9001, Head: "abcdef1234", URL: "https://github.com/talkable/talkable/pull/9#pullrequestreview-9001",
			At: ago(2 * time.Hour)}
		if edit != nil {
			edit(r)
		}
	})
}

// A PR magnum approved as the operator says "✓ auto" in its state cell, in a
// pill of its own color that does not shimmer, over "✓ needs you" (the
// gate read before the approval); a round due or in flight shows its own.
func TestTheAutoCellSaysMagnumApprovedAsYou(t *testing.T) {
	rows := []PRBoardRow{autoRow(func(r *PRBoardRow) { r.NeedsMe = NeedsMeApprove })}
	p := needsMePainter(IconsUnicode, rows)
	got := p.stateWaitCell(rows[0])
	if cellText(got) != " ✓ auto " || len(got) != 1 {
		t.Fatalf("cell %q", cellText(got))
	}
	if st := got[0].st; !st.GetBold() || !st.GetReverse() || st.GetForeground() == p.pal.pills["reviewed"].GetForeground() {
		t.Errorf("style: bold %v reverse %v, foreground %v (the reviewed pill's %v)", st.GetBold(), st.GetReverse(),
			st.GetForeground(), p.pal.pills["reviewed"].GetForeground())
	}
	if needsMeShown(rows[0]) {
		t.Error("an auto-approved row still shimmers as needing you")
	}
	if c := cellText(needsMePainter(IconsASCII, rows).stateWaitCell(rows[0])); c != " + auto " {
		t.Errorf("ASCII cell %q", c)
	}
	for _, state := range []string{"reviewing", "rereview_pending"} {
		r := autoRow(func(r *PRBoardRow) { r.State = state })
		if c := cellText(p.stateWaitCell(r)); strings.Contains(c, "auto") {
			t.Errorf("%s: cell %q", state, c)
		}
	}
}

// The titles say how many PRs magnum approved as the operator.
func TestTheTitleCountsTheAutoApprovedPRs(t *testing.T) {
	facts := DaemonFacts{NeedsMe: 1, AutoApproved: 2}.list(boardNow)
	var full, short []string
	for _, f := range facts {
		full, short = append(full, f.full), append(short, f.short)
	}
	if strings.Join(full, " · ") != "1 needs your ✓ · 2 auto-approved" || strings.Join(short, " · ") != "your ✓ ×1 · auto ×2" {
		t.Fatalf("facts %q / %q", full, short)
	}
	m, _, _ := newBoard(t, 170, 30, PRBoardOptions{})
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{autoRow(nil)}, facts: DaemonFacts{AutoApproved: 1}})
	mustContain(t, viewOf(m), "1 auto-approved")
}

// D withdraws the approval magnum posted as the operator after a y/N that
// names it (only y confirms); it is refused on a PR without one, and the
// card says the approval stands, or that magnum stopped approving the PR.
func TestDWithdrawsTheAutoApprovalAfterAsking(t *testing.T) {
	m, _, act := newBoard(t, 170, 40, PRBoardOptions{})
	plain := actPR(func(r *PRBoardRow) { r.ActivityAt = ago(time.Hour) })
	stopped := actPR(func(r *PRBoardRow) {
		r.Ref, r.Number, r.AutoStopped, r.ActivityAt = "talkable/talkable#7", 7, "you commented on it by hand", ago(2*time.Hour)
	})
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{autoRow(nil), plain, stopped}})
	mustContain(t, viewOf(m), "✓ auto")

	m, _ = send(t, m, keyMsg("D"))
	q := "Withdraw the approval magnum posted as you on talkable#9 (review 9001 on abcdef1) and stop it approving talkable#9?"
	mustContain(t, viewOf(m), q)
	m, _ = boardAct(t, m, "n")
	if got := act.last(); got != "" {
		t.Fatalf("n ran %q", got)
	}
	m, _ = send(t, m, keyMsg("D"))
	m, _ = boardAct(t, m, "y")
	if got := act.last(); got != "unapprove talkable/talkable#9" {
		t.Fatalf("D called %q", got)
	}

	c, _ := send(t, m, keyMsg("enter"))
	mustContain(t, viewOf(c), "Approved as you by magnum on abcdef1 2h ago", "D withdraw approval")
	m, _ = send(t, m, keyMsg("j"), keyMsg("D"))
	mustContain(t, viewOf(m), "magnum has no approval standing as you on talkable#5")
	if got := act.last(); got != "unapprove talkable/talkable#9" {
		t.Fatalf("D on a PR without one called %q", got)
	}
	m, _ = send(t, m, keyMsg("esc"), keyMsg("j"), keyMsg("enter"))
	mustContain(t, viewOf(m), "magnum no longer approves it as you: you commented on it by hand")
	mustNotContain(t, viewOf(m), "D withdraw approval")
}
