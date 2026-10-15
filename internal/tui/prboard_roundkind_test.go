package tui

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// kindStart is when the rounds of the round-kind tests started: 4m before
// boardNow.
var kindStart = boardNow.Add(-4 * time.Minute)

// kindRow is PR n running a round of why's kind since kindStart, its judge
// at work, why its own round's (started with it).
func kindRow(n int, why RoundWhy) PRBoardRow {
	r := runningRow(kindStart, RoleProgress{Role: "codex-judge", Label: "cj", Judge: true, Started: kindStart, Working: true})
	r.Number, r.Title = n, "Round kind "+strconv.Itoa(n)
	r.Ref = "talkable/talkable#" + strconv.Itoa(n)
	r.GHState, r.ActivityAt = "OPEN", kindStart
	why.Roles, why.At = []string{"codex-judge"}, kindStart
	r.RoundWhy = &why
	return r
}

// roundKindCases are the kinds a running round's row tells apart, with the
// mark or word each mode draws after the pill ("" for none).
var roundKindCases = []struct {
	name string
	why  RoundWhy
	mark map[IconMode]string
}{
	{"first review", RoundWhy{Kind: "initial"}, marks("✦", "\uf51b", "1")},
	{"re-review", RoundWhy{Kind: "rereview"}, marks("»", "\uf46a", "r")},
	{"recovery", RoundWhy{Kind: "recovery"}, marks("✚", "\uf41c", "rc")},
	{"delta check", RoundWhy{Kind: "rereview", DeltaCheck: true, DeltaLines: 4}, marks("∆", "\uf440", "d")},
	{"full pass", RoundWhy{Kind: "recovery", FullPass: true}, marks("◉", "\uf45e", "F")},
	{"full pass before approval", RoundWhy{Kind: "recovery", FullPass: true, BeforeApproval: true}, marks("⚑", "\uf510", "F+")},
	{"audit pass", RoundWhy{Kind: "recovery", AuditPass: true}, marks("☑", "\uf46b", "a")},
	{"continue", RoundWhy{Kind: "continue"}, marks("continue", "continue", "continue")},
	{"nudge", RoundWhy{Kind: "nudge"}, marks("nudge", "nudge", "nudge")},
	{"reply round", RoundWhy{Kind: "rereview", Replies: 2}, marks("replies", "replies", "replies")},
	// The precedence the card's phrase follows: delta check, full pass
	// before approval, full pass, audit pass, then the base kind.
	{"a delta check over a full pass", RoundWhy{Kind: "rereview", DeltaCheck: true, FullPass: true}, marks("∆", "\uf440", "d")},
	{"a full pass over an audit pass", RoundWhy{Kind: "recovery", FullPass: true, AuditPass: true}, marks("◉", "\uf45e", "F")},
	{"a continued full pass", RoundWhy{Kind: "continue", FullPass: true}, marks("◉", "\uf45e", "F")},
	{"a full pass of a reply round", RoundWhy{Kind: "recovery", FullPass: true, Replies: 1}, marks("◉", "\uf45e", "F")},
	{"an unknown kind", RoundWhy{Kind: "bogus"}, marks("", "", "")},
}

func marks(unicode, nerd, ascii string) map[IconMode]string {
	return map[IconMode]string{IconsUnicode: unicode, IconsNerd: nerd, IconsASCII: ascii}
}

// kindForms are the three forms of a running round's state cell after its
// pill, widest first: with the stage, with the time alone, bare. A mark (or
// a word in its place) comes first and keeps a plain space after it.
func kindForms(mark string) [3]string {
	if mark == "" {
		return [3]string{" judge · 4m", " 4m", ""}
	}
	return [3]string{" " + mark + " judge · 4m", " " + mark + " 4m", " " + mark + " "}
}

// The row of a running round marks its kind right after the reviewing pill,
// one cell in the nerd and Unicode modes (one or two letters in ASCII, a
// word for a continue, a nudge and a reply round), in the three forms of
// the state cell, so the stage and then the time give way before the mark
// does; never wider than the "delta check" words it replaces. On the board
// at 100, 140 and 200 columns every running row keeps its mark.
func TestBoardRoundKindsAcrossModesAndWidths(t *testing.T) {
	var rows []PRBoardRow
	for i, tc := range roundKindCases {
		rows = append(rows, kindRow(12000+i, tc.why))
	}
	for _, mode := range iconModes {
		m, _, _ := newBoard(t, 200, 40, PRBoardOptions{Icons: mode})
		p := m.painter()
		pill := ansi.Strip(p.stateCell("reviewing").render(nil))
		for i, tc := range roundKindCases {
			r := sanitizeRow(rows[i])
			want := kindForms(tc.mark[mode])
			for j, d := range []stateDetail{stateFull, stateTime, stateBare} {
				if got := ansi.Strip(p.stateWaitDetail(r, d).render(nil)); got != pill+want[j] {
					t.Errorf("%s %s (detail %d): state cell = %q, want %q", mode, tc.name, d, got, pill+want[j])
				}
			}
			t.Logf("%s %-28s %q | %q | %q", mode, tc.name+":", pill+want[0], pill+want[1], pill+want[2])
			if old := ansi.StringWidth(pill + " delta check · judge · 4m"); ansi.StringWidth(pill+want[0]) > old {
				t.Errorf("%s %s: %q is wider than the delta check words (%d cells)", mode, tc.name, pill+want[0], old)
			}
			if tc.mark[mode] != "" && !strings.Contains(tc.name, "unknown") {
				if st := p.stateWaitDetail(r, stateFull); !hasSeg(st, tc.mark[mode], p.pal.dots["reviewing"].Render(tc.mark[mode])) {
					t.Errorf("%s %s: the mark is not in the reviewing pill's color: %q", mode, tc.name, st.render(nil))
				}
			}
		}

		for _, w := range []int{100, 140, 200} {
			b := iconBoard(t, mode, w, 40, rows...)
			v := viewOf(b)
			header := lineWith(t, v, "STATE")
			t.Logf("%s at %d: %s", mode, w, strings.Join(strings.Fields(header), " "))
			for i, tc := range roundKindCases {
				line := lineWith(t, v, "#"+strconv.Itoa(rows[i].Number))
				want := kindForms(tc.mark[mode])
				switch {
				case strings.Contains(line, pill+want[0]):
				case w < 200 && strings.Contains(line, pill+want[1]):
				default:
					t.Errorf("%s at %d, %s: %q has neither the kind with the stage nor with the time", mode, w, tc.name, line)
				}
			}
		}
	}
}

// hasSeg reports whether c has a run of text drawn as styled.
func hasSeg(c cell, text, styled string) bool {
	for _, s := range c {
		if strings.TrimSpace(s.text) == text && s.st.Render(text) == styled {
			return true
		}
	}
	return false
}

// A post-merge round keeps its word, now before the kind's mark: "post-merge
// » judge · 4m", in each of the three forms; while its kind is not known
// yet, and while it waits, it reads as before ("post-merge · judge · 4m",
// "post-merge · next tick").
func TestPostMergeKeepsItsWordBeforeTheKindMark(t *testing.T) {
	for _, mode := range iconModes {
		m, _, _ := newBoard(t, 200, 40, PRBoardOptions{Icons: mode})
		p := m.painter()
		pill := ansi.Strip(p.stateCell("reviewing").render(nil))
		mark := marks("»", "\uf46a", "r")[mode]
		row := kindRow(12100, RoundWhy{Kind: "rereview", PostMerge: true})
		row.GHState = "MERGED"
		for j, d := range []stateDetail{stateFull, stateTime, stateBare} {
			want := pill + " post-merge" + kindForms(mark)[j]
			if got := ansi.Strip(p.stateWaitDetail(sanitizeRow(row), d).render(nil)); got != want {
				t.Errorf("%s (detail %d): state cell = %q, want %q", mode, d, got, want)
			}
		}
		delta := kindRow(12101, RoundWhy{Kind: "rereview", PostMerge: true, DeltaCheck: true, DeltaLines: 3})
		delta.GHState = "MERGED"
		if got, want := ansi.Strip(p.stateWaitCell(sanitizeRow(delta)).render(nil)), pill+" post-merge "+marks("∆", "\uf440", "d")[mode]+" judge · 4m"; got != want {
			t.Errorf("%s: a post-merge delta check = %q, want %q", mode, got, want)
		}

		stale := row
		why := *row.RoundWhy
		why.At = kindStart.Add(-time.Hour)
		stale.RoundWhy = &why
		if got := ansi.Strip(p.stateWaitCell(sanitizeRow(stale)).render(nil)); got != pill+" post-merge · judge · 4m" {
			t.Errorf("%s: a post-merge round whose kind is not known yet = %q", mode, got)
		}
		queued := PRBoardRow{Owner: "talkable", Repo: "talkable", Number: 12102, State: "rereview_pending", GHState: "MERGED",
			Wait: "re-review · next tick"}
		if got := ansi.Strip(p.stateWaitCell(sanitizeRow(queued)).render(nil)); !strings.HasSuffix(got, " post-merge · next tick") {
			t.Errorf("%s: a post-merge round that waits = %q", mode, got)
		}
	}
}

// A refresh that changes only a running round's kind (its round_start
// landed, or said another kind) redraws the row: the cached frame is the
// one a fresh render draws and shows the new mark.
func TestRoundKindRefreshRedrawsTheRow(t *testing.T) {
	row := kindRow(12200, RoundWhy{Kind: "rereview"})
	stale := row
	prev := *row.RoundWhy
	prev.At, prev.DeltaCheck = kindStart.Add(-time.Hour), true
	stale.RoundWhy = &prev
	m := testPRBoard(context.Background(), &fakeBoardSource{rows: []PRBoardRow{stale}}, &fakeActions{},
		PRBoardOptions{Now: func() time.Time { return boardNow }, SelfLogins: boardSelf})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 20})
	m = next.(prBoardModel)

	p := m.painter()
	pill := ansi.Strip(p.stateCell("reviewing").render(nil))
	for _, step := range []struct {
		name string
		row  PRBoardRow
		want string
	}{
		{"the previous round's delta check", stale, pill + " judge · 4m"},
		{"its own round_start: a re-review", row, pill + " » judge · 4m"},
		{"then a recovery", kindRow(12200, RoundWhy{Kind: "recovery"}), pill + " ✚ judge · 4m"},
	} {
		next, _ = m.Update(prbDataMsg{rows: []PRBoardRow{step.row}})
		m = next.(prBoardModel)
		got := m.View().Content
		if fresh := uncachedBoard(m); got != fresh {
			t.Fatalf("%s: the cached frame differs from a fresh one:\n%s\n---\n%s", step.name, got, fresh)
		}
		if line := lineWith(t, ansi.Strip(got), "#12200"); !strings.Contains(line, step.want) {
			t.Errorf("%s: row = %q, want %q", step.name, line, step.want)
		}
	}
}
