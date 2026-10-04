package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

var iconModes = []IconMode{IconsUnicode, IconsNerd, IconsASCII}

// findingsRow is a PR whose latest review found something at every
// priority and suggested simplifications: the widest FINDINGS cell, a
// dismissed stale reviewer and every card section that has a heading icon.
func findingsRow() PRBoardRow {
	return PRBoardRow{
		Ref: "talkable/talkable#8", Owner: "talkable", Repo: "talkable", Number: 8, Title: "Refund metrics",
		State: "needs_attention", GHState: "OPEN", UpdatedAt: ago(time.Hour), HeadSHA: "abcdef1234",
		LastReview: &ReviewInfo{Login: "alice", Event: "CHANGES_REQUESTED", SubmittedAt: ago(time.Hour), CommitSHA: "abcdef1234"},
		Reviewers: []ReviewerInfo{
			{Login: "alice", Verdict: "changes_requested", SubmittedAt: ago(time.Hour), CommitSHA: "abcdef1234"},
			{Login: "bob", Verdict: "dismissed", SubmittedAt: ago(2 * time.Hour), CommitSHA: "1234567", Stale: true},
		},
		SinceReview: &ReviewDelta{Base: "reviewed", BaseSHA: "abcdef1234"},
		Findings:    &FindingsInfo{Counts: [4]int{1, 1, 3, 1}, Simplifications: 2, Open: 1, Verdict: "blocking", Posted: "COMMENT", SHA: "abcdef1234"},
		LastRound:   &RoundTimings{Round: 2, Kind: "full", Stages: []StageTiming{{Name: "judge", Duration: time.Minute}}, Total: time.Minute},
		WaitDetail:  "quiet period after a push",
		LastError:   "judge failed", ErrorFix: "magnum review talkable#8",
	}
}

// iconDash is a dashboard in mode, w x h, with dashData and its manual
// worktrees shown.
func iconDash(t *testing.T, mode IconMode, w, h int) dashboardModel {
	t.Helper()
	m := newDashboardModel(context.Background(), &fakeSource{data: dashData()}, &fakeActions{},
		DashboardOptions{Now: func() time.Time { return dashNow }, Icons: mode})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: w, Height: h}, dashDataMsg{data: dashData()}, keyMsg("w"))
	return m
}

// iconBoard is a board in mode, w x h, showing rows.
func iconBoard(t *testing.T, mode IconMode, w, h int, rows ...PRBoardRow) prBoardModel {
	t.Helper()
	m, _, _ := newBoard(t, w, h, PRBoardOptions{Icons: mode})
	m, _ = send(t, m, prbDataMsg{rows: rows})
	return m
}

// iconViews are every screen of mode at w x h: the board, each row's card
// (top and bottom), the help (top and bottom, where the legend is), the
// one-shot render, the dashboard and its help. The rows include every
// kind of CI (ciRows), badges and a skipped PR.
func iconViews(t *testing.T, mode IconMode, w, h int) map[string]string {
	t.Helper()
	rows := append(append(boardRows(), findingsRow(), badgeRow(), skippedRow()), ciRows()...)
	b := iconBoard(t, mode, w, h, rows...)
	views := map[string]string{
		"board":  viewOf(b),
		"render": ansi.Strip(RenderPRBoard(rows, w, PRBoardOptions{Icons: mode, Now: func() time.Time { return boardNow }})),
	}
	for i := range b.view {
		c, _ := send(t, b, keyMsg("enter"))
		views["card "+prRef(b.view[i])] = viewOf(c)
		c, _ = send(t, c, keyMsg("G"))
		views["card end "+prRef(b.view[i])] = viewOf(c)
		b, _ = send(t, b, keyMsg("j"))
	}
	help, _ := send(t, b, keyMsg("?"))
	views["help"] = viewOf(help)
	help, _ = send(t, help, keyMsg("G"))
	views["help end"] = viewOf(help)

	d := iconDash(t, mode, w, h)
	views["dashboard"] = viewOf(d)
	dh, _ := send(t, d, keyMsg("?"))
	views["dashboard help"] = viewOf(dh)
	dh, _ = send(t, dh, keyMsg("G"))
	views["dashboard help end"] = viewOf(dh)
	return views
}

// Every mode's symbols are measured as wide as terminals draw them, so no
// screen ever draws a line wider than the terminal or more lines than it
// has, however narrow.
func TestIconModesFitTheScreen(t *testing.T) {
	for _, mode := range iconModes {
		for _, size := range [][2]int{{40, 16}, {80, 24}, {120, 30}, {200, 50}} {
			w, h := size[0], size[1]
			for name, v := range iconViews(t, mode, w, h) {
				if got := maxLineWidth(v); got > w {
					t.Errorf("%s %s at %dx%d: a line is %d cells:\n%s", mode, name, w, h, got, v)
				}
				if got := lineCount(v); got > h && name != "render" {
					t.Errorf("%s %s at %dx%d: %d lines:\n%s", mode, name, w, h, got, v)
				}
			}
		}
	}
}

// The nerd mode marks what needs a glance: a colored mark before every
// state, verdict and finding priority, an icon in every state pill and
// before the headings; the legends explain the marks.
func TestNerdIconsMarkStatesVerdictsAndPriorities(t *testing.T) {
	rows := append(boardRows(), findingsRow())
	b := iconBoard(t, IconsNerd, 240, 30, rows...)
	mustContain(t, viewOf(b),
		"🔴 2 attention", "🔵 1 reviewing", "🟢 1 reviewed", "⚪ 1 not reviewed", // the summary
		" \uf441 reviewing ", " \uf421 attention ", " \uf4c5 not reviewed ", // the pills
		"❌ 🔥P0 🔴P1 🟠P2×3 ⚪P3 \uf0c4 2", // FINDINGS, whole
		"✅ approved", "alice❌", "bob\uf468 \uf464", "eve⏳", "\uf51f zhuravel✅", // verdicts and chips
		"\uf435 Fix referral", "\uf530 Referral analytics") // pin and failure

	c := iconBoard(t, IconsNerd, 200, 90, findingsRow())
	c, _ = send(t, c, keyMsg("enter"))
	mustContain(t, viewOf(c), "\uf4e3 WAITING", "\uf417 SINCE REVIEW", "\uf4af LAST REVIEW", "\uf46f FINDINGS",
		"\uf520 LAST ROUND (2, full)", "\uf4fd REVIEWERS (2)", "\uf421 NEEDS YOU", "\uf427 ACTIONS",
		"❌ Decision: request changes; posted as comment",
		"🔥 P0 1 · 🔴 P1 1 · 🟠 P2 3 · ⚪ P3 1 · \uf0c4 2 simplifications suggested")

	h, _ := send(t, b, tea.WindowSizeMsg{Width: 240, Height: 60}, keyMsg("?"))
	mustContain(t, viewOf(h), "✅ approved", "❌ changes requested", "⏳ requested", "\uf464 stale", "\uf51f yours",
		"❌ blocking", "💬 non-blocking", "✅ clean", "🔥P0 🔴P1 🟠P2 ⚪P3 by priority", "\uf0c4 simplifications",
		"🔴  \uf421 attention ", "🚫  \uf466 ignored ")

	d := iconDash(t, IconsNerd, 120, 40)
	mustContain(t, viewOf(d), "🟢 running", "🛑 PAUSED codex", "\uf413 SLOTS (2)", "\uf407 QUEUE (2)", "\uf421 ATTENTION (1)",
		"\uf425 MANUAL WORKTREES (1)", "talkable#1 🔵 reviewing", "🔵 busy", "🟢 free", "🟡 queued", "🔴 talkable#3 [needs_attention]")
	dh, _ := send(t, d, tea.WindowSizeMsg{Width: 120, Height: 80}, keyMsg("?"))
	mustContain(t, viewOf(dh), "Legend", "🔴 attention", "⚪ not reviewed", "🟣 held", "🟠 dirty schema")
}

// The default stays as it was: an empty or unknown mode draws Unicode
// symbols, with no emoji marks or Nerd Font icons on either screen.
func TestIconModeDefaultsToUnicode(t *testing.T) {
	for in, want := range map[IconMode]IconMode{"": IconsUnicode, "Unicode": IconsUnicode, "bogus": IconsUnicode, "NERD": IconsNerd, " ascii ": IconsASCII} {
		if got := newGlyphs(in).mode; got != want {
			t.Errorf("mode %q draws %q, want %q", in, got, want)
		}
	}
	rows := append(boardRows(), findingsRow())
	for _, mode := range []IconMode{"", IconsUnicode} {
		b := iconBoard(t, mode, 240, 30, rows...)
		mustContain(t, viewOf(b), "✗ P0 P1 P2×3 P3 ✂2", "● 2 attention", " reviewing ", "📌 Fix referral")
		d := iconDash(t, mode, 120, 40)
		mustContain(t, viewOf(d), "\nSLOTS (2)", "talkable#1 reviewing", "running (pid")
		dh, _ := send(t, d, tea.WindowSizeMsg{Width: 120, Height: 80}, keyMsg("?"))
		for name, v := range map[string]string{"board": viewOf(b), "dashboard": viewOf(d), "dashboard help": viewOf(dh)} {
			mustNotContain(t, v, "🔴", "🔵", "✅", "❌", "Legend")
			if r, ok := nerdRune(v); ok {
				t.Errorf("%q %s draws the Nerd Font icon %U", mode, name, r)
			}
		}
	}
}

// The ASCII mode draws neither emoji nor Nerd Font icons on any screen.
func TestASCIIIconsDrawNoSymbols(t *testing.T) {
	for name, v := range iconViews(t, IconsASCII, 200, 60) {
		if r, ok := nerdRune(v); ok {
			t.Errorf("ascii %s draws the Nerd Font icon %U", name, r)
		}
		for _, g := range []string{"✔", "✗", "💬", "◌", "⟳", "★", "📌", "✂", "🔴"} {
			if strings.Contains(v, g) {
				t.Errorf("ascii %s draws %q", name, g)
			}
		}
	}
	b := iconBoard(t, IconsASCII, 240, 30, findingsRow())
	mustContain(t, viewOf(b), "x P0 P1 P2x3 P3 s2")
}

// nerdRune is the first Private Use Area rune of s, where Nerd Fonts keep
// their icons.
func nerdRune(s string) (rune, bool) {
	for _, r := range s {
		if unicode.Is(unicode.Co, r) {
			return r, true
		}
	}
	return 0, false
}

// Every symbol of every mode is one cell or an emoji of two: never a
// variation selector or a joiner (terminals disagree on their widths),
// never a zero-width rune, and a Nerd Font icon always one cell, as
// ansi.StringWidth measures it. The nerd mode marks every state.
func TestGlyphWidths(t *testing.T) {
	for _, mode := range iconModes {
		var check func(name string, v reflect.Value)
		check = func(name string, v reflect.Value) {
			switch v.Kind() {
			case reflect.String:
				for _, r := range v.String() {
					w := ansi.StringWidth(string(r))
					switch {
					case r == '\ufe0f' || r == '\u200d':
						t.Errorf("%s %s: %q has the selector or joiner %U", mode, name, v.String(), r)
					case unicode.Is(unicode.Co, r) && w != 1:
						t.Errorf("%s %s: icon %U measures %d cells, want 1", mode, name, r, w)
					case w < 1 || w > 2:
						t.Errorf("%s %s: %U measures %d cells", mode, name, r, w)
					}
				}
			case reflect.Map:
				for _, k := range v.MapKeys() {
					check(name+"["+k.String()+"]", v.MapIndex(k))
				}
			case reflect.Array:
				for i := range v.Len() {
					check(name, v.Index(i))
				}
			}
		}
		g := reflect.ValueOf(newGlyphs(mode))
		for i := range g.NumField() {
			if f := g.Type().Field(i); f.Type.Kind() != reflect.Struct { // the spinner's frames are its own
				check(f.Name, g.Field(i))
			}
		}
	}

	nerd := newGlyphs(IconsNerd)
	for _, s := range prStateOrder {
		if nerd.stateIcon[s] == "" || nerd.stateMark[s] == "" {
			t.Errorf("nerd: state %s has icon %q, mark %q", s, nerd.stateIcon[s], nerd.stateMark[s])
		}
	}
	if len(nerd.slotMark) != len(dashSlotStateOrder) {
		t.Errorf("nerd: %d slot marks, the legend lists %d states", len(nerd.slotMark), len(dashSlotStateOrder))
	}
	for _, s := range dashSlotStateOrder {
		if nerd.slotMark[s] == "" {
			t.Errorf("nerd: slot state %s has no mark", s)
		}
	}
}
