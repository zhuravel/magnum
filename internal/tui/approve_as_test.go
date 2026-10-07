package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// needsYou is a reviewed PR magnum's App approved that GitHub still blocks
// on the operator (kind: NeedsMeApprove or NeedsMeLift), on a watch whose
// auto_approve_as is the operator's account zhuravel.
func needsYou(kind string, edit func(r *PRBoardRow)) PRBoardRow {
	return actPR(func(r *PRBoardRow) {
		r.NeedsMe = kind
		r.Findings = &FindingsInfo{Verdict: "clean", Posted: "APPROVE", SHA: "abcdef1234"}
		r.ApproveAs = &ApproveAs{Identity: "zhuravel", Login: "zhuravel"}
		if edit != nil {
			edit(r)
		}
	})
}

// A on a row GitHub blocks on the operator's approval asks to approve as
// their own account (the watch's auto_approve_as), says their approval is
// the one that counts and, on a "lift your ✗" row, that it lifts their own
// changes request; y posts as that identity.
func TestAOnARowThatNeedsYouApprovesAsYou(t *testing.T) {
	for _, c := range []struct {
		kind, want string
	}{
		{NeedsMeApprove, "Approve talkable#5 at abcdef1 as zhuravel? Your approval counts; magnum found no findings"},
		{NeedsMeLift, "Approve talkable#5 at abcdef1 as zhuravel? Your approval counts and lifts your ✗; magnum found no findings"},
	} {
		row := needsYou(c.kind, nil)
		r := boardActRow(row, "talkable#5", boardNow)
		if why := actionRefusal(actApprove, r); why != "" {
			t.Fatalf("%s: refused %q", c.kind, why)
		}
		if got := actionLabel(actApprove, r); got != "approve as zhuravel" {
			t.Errorf("%s: label %q", c.kind, got)
		}
		m, _, act := newBoard(t, 200, 24, PRBoardOptions{})
		m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{row}}, keyMsg("A"))
		if m.confirm == nil || m.confirm.question != c.want {
			t.Fatalf("%s: A asked %+v, want %q", c.kind, m.confirm, c.want)
		}
		if _, _ = boardAct(t, m, "y"); act.last() != "approve talkable/talkable#5 as zhuravel" {
			t.Fatalf("%s: y called %q", c.kind, act.last())
		}
	}
}

// A refuses, before asking, what the operator's approval cannot do: on a
// watch without auto_approve_as only an approval by hand counts (b opens the
// PR), and a precondition the daemon checks (auto-approval's, but for the
// operator's own stop) is named. The card of a row without auto_approve_as
// says so in one line.
func TestARefusesWhatYourApprovalCannotDo(t *testing.T) {
	for _, c := range []struct {
		name string
		row  PRBoardRow
		want string
	}{
		{"no auto_approve_as", needsYou(NeedsMeApprove, func(r *PRBoardRow) { r.ApproveAs = nil }),
			"GitHub counts only your approval: b opens the PR"},
		{"no auto_approve_as, lift", needsYou(NeedsMeLift, func(r *PRBoardRow) { r.ApproveAs = nil }),
			"GitHub counts only your approval: b opens the PR"},
		{"a precondition fails", needsYou(NeedsMeApprove, func(r *PRBoardRow) { r.ApproveAs.Refusal = "it is muted" }),
			"talkable#5 is not approved as zhuravel: it is muted"},
	} {
		if got := actionRefusal(actApprove, boardActRow(c.row, "talkable#5", boardNow)); got != c.want {
			t.Errorf("%s: refusal %q, want %q", c.name, got, c.want)
		}
		m, _, act := newBoard(t, 200, 24, PRBoardOptions{})
		m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{c.row}}, keyMsg("A"))
		if m.confirm != nil || !strings.Contains(m.flash, c.want) || !m.flashErr {
			t.Errorf("%s: asked %+v, flashed %q", c.name, m.confirm, m.flash)
		}
		if m, _ = send(t, m, keyMsg("y")); len(act.calls) != 0 {
			t.Errorf("%s: y ran %v", c.name, act.calls)
		}
	}

	m, _, _ := newBoard(t, 160, 50, PRBoardOptions{})
	p := m.painter()
	card := func(r PRBoardRow) string { return ansi.Strip(strings.Join(p.cardContent(sanitizeRow(r), 120), "\n")) }
	mustContain(t, card(needsYou(NeedsMeLift, func(r *PRBoardRow) { r.ApproveAs = nil })), "GitHub counts only your approval: b opens the PR")
	for name, r := range map[string]PRBoardRow{"auto_approve_as": needsYou(NeedsMeApprove, nil), "no need": actPR(nil)} {
		if c := card(r); strings.Contains(c, "GitHub counts only your approval") {
			t.Errorf("%s: the card says only your approval counts:\n%s", name, c)
		}
	}
}

// A row that does not need the operator approves as the PR's posting
// identity, as before.
func TestAOnARowThatDoesNotNeedYouApprovesAsThePR(t *testing.T) {
	m, _, act := newBoard(t, 200, 24, PRBoardOptions{})
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{actPR(nil)}}, keyMsg("A"))
	if want := "Approve talkable#5 at abcdef1? magnum found 1 P1"; m.confirm == nil || m.confirm.question != want {
		t.Fatalf("A asked %+v, want %q", m.confirm, want)
	}
	if _, _ = boardAct(t, m, "y"); act.last() != "approve talkable/talkable#5" {
		t.Fatalf("y called %q", act.last())
	}
}
