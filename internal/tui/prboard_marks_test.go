package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// skippedRow is a PR the configuration skips: its author is a bot.
func skippedRow() PRBoardRow {
	return PRBoardRow{Ref: "talkable/example#40", Owner: "talkable", Repo: "example", Number: 40, Title: "Bump rack from 3.0 to 3.1",
		Author: "dependabot[bot]", State: "ineligible", SkipReason: "bot author", GHState: "OPEN", UpdatedAt: ago(15 * time.Minute)}
}

// badgeRow is a pinned, failing draft carrying two badges.
func badgeRow() PRBoardRow {
	return PRBoardRow{Ref: "talkable/example#41", Owner: "talkable", Repo: "example", Number: 41, Title: "Checkout redesign",
		Author: "alice", State: "reviewed", GHState: "OPEN", UpdatedAt: ago(16 * time.Minute), Pinned: true, Draft: true,
		LastError: "judge failed", Labels: []string{"Flagged", "hotfix"},
		Badges: []Badge{{Label: "Flagged", Text: "🚩"}, {Label: "hotfix", Text: "HOT"}}}
}

// The configuration's reason shrinks to a word for the state cell; the
// setting a reason names wins over the words in it.
func TestSkipWord(t *testing.T) {
	for reason, want := range map[string]string{
		"bot author":                               "bot",
		`author "carol" is in skip_authors`:        "author",
		`author "renovate-bot" is in skip_authors`: "author",
		`author "dan" left talkable: not a member (skip_departed_authors = true)`: "left org",
		`label "wip" is in skip_labels`:                                           "label",
		`label "draft" is in skip_labels`:                                         "label",
		"draft pull request":                                                      "draft",
		"head repository is a fork":                                               "fork",
		"":                                                                        "",
		"something else":                                                          "",
	} {
		r := skippedRow()
		r.SkipReason = reason
		if got := skipWord(r); got != want {
			t.Errorf("skipWord(%q) = %q, want %q", reason, got, want)
		}
	}
	if got := skipWord(PRBoardRow{State: "reviewed", SkipReason: "bot author"}); got != "" {
		t.Errorf("a PR that is not skipped has the skip word %q", got)
	}
}

// A PR the configuration skips reads "skipped" with why in a word, the
// whole row dimmed like a muted one but not struck through like an
// ignored one; the summary counts it as skipped and its card says why
// and that R reviews it anyway.
func TestPRBoardSkippedRows(t *testing.T) {
	plain := skippedRow()
	plain.Ref, plain.Number, plain.SkipReason = "talkable/example#44", 44, "something else"
	rows := append(boardRows(), skippedRow(), plain)
	for mode, want := range map[IconMode][]string{
		IconsUnicode: {" skipped · bot", "● 1 not reviewed", "● 2 skipped"},
		IconsNerd:    {" \uf517 skipped · bot", "⚫ 2 skipped"},
		IconsASCII:   {" skipped · bot", " 2 skipped"},
	} {
		m := iconBoard(t, mode, 200, 30, rows...)
		v := viewOf(m)
		mustContain(t, v, want...)
		mustNotContain(t, v, "ineligible")
		if l := lineWith(t, v, "#44"); !strings.Contains(l, "skipped") || strings.Contains(l, "skipped ·") {
			t.Errorf("%s: a reason without a known word reads %q, want plain skipped", mode, l)
		}
	}

	m, _, _ := newBoard(t, 200, 30, PRBoardOptions{})
	m, _ = send(t, m, prbDataMsg{rows: rows})
	p := m.painter()
	lay := m.tableLayout(p, 200)
	dim := lipgloss.NewStyle().Foreground(p.pal.dim)
	struck := func(raw string) bool { return strings.Contains(raw, "\x1b[9m") || strings.Contains(raw, ";9m") }
	if raw := p.rowLine(skippedRow(), lay, 200, false); !hasStyled(raw, dim, "Bump rack from 3.0 to 3.1") || struck(raw) {
		t.Errorf("a skipped row is not dimmed, or struck through: %q", raw)
	}
	ignored := skippedRow()
	ignored.State, ignored.Muted = "ignored", true
	if raw := p.rowLine(ignored, lay, 200, false); !struck(raw) {
		t.Errorf("an ignored row lost its strikethrough: %q", raw)
	}
	reviewed := skippedRow()
	reviewed.State = "reviewed"
	if raw := p.rowLine(reviewed, lay, 200, false); hasStyled(raw, dim, "Bump rack from 3.0 to 3.1") {
		t.Error("a reviewed row is dimmed")
	}

	m, _ = send(t, m, keyMsg("/"))
	m, _ = send(t, m, typed("state:skipped")...)
	if got := refsOf(m.view); !slices.Equal(got, []string{"example#40", "example#44"}) {
		t.Errorf("state:skipped = %v", got)
	}

	for mode, head := range map[IconMode]string{IconsUnicode: "SKIPPED", IconsNerd: "\uf517 SKIPPED", IconsASCII: "SKIPPED"} {
		c := iconBoard(t, mode, 120, 60, skippedRow())
		c, _ = send(t, c, keyMsg("enter"))
		mustContain(t, viewOf(c), head, "magnum's configuration skips this PR: bot author", "R reviews it anyway (asks y/N)")
	}
	c := iconBoard(t, IconsUnicode, 120, 60, boardRows()[0])
	c, _ = send(t, c, keyMsg("enter"))
	mustNotContain(t, viewOf(c), "SKIPPED")
}

// Badges show before the title (after the pin and failure marks, before
// the draft tag) in every icon mode, whole or not at all however narrow
// the column; the card names their labels, the summary counts the PRs
// carrying each and the filter finds them by label.
func TestPRBoardBadges(t *testing.T) {
	for mode, want := range map[IconMode]string{
		IconsUnicode: "📌 ! 🚩 HOT draft Checkout redesign",
		IconsNerd:    "\uf435 \uf530 🚩 HOT draft Checkout redesign",
		IconsASCII:   "pin ! 🚩 HOT draft Checkout redesign",
	} {
		p := newPRBPainter(defaultStyles, newPRBPalette(defaultStyles), newGlyphs(mode), boardNow, nil, nil, SortUpdated, true)
		full := p.titleCell(badgeRow())
		if got := ansi.Strip(full.render(nil)); got != want {
			t.Errorf("%s: title cell %q, want %q", mode, got, want)
		}
		for w := 1; w <= full.width(); w++ {
			got := full.fitWhole(w, len(full)-1)
			if got.width() > w {
				t.Errorf("%s at %d: %q is %d cells", mode, w, ansi.Strip(got.render(nil)), got.width())
			}
			for i, s := range got[:len(got)-1] { // all but the ellipsis
				if i < len(full)-1 && s.text != full[i].text {
					t.Errorf("%s at %d: run %d is %q, a cut of %q", mode, w, i, s.text, full[i].text)
				}
			}
		}
	}

	second := badgeRow()
	second.Ref, second.Number, second.Pinned, second.LastError = "talkable/example#43", 43, false, ""
	second.UpdatedAt = ago(time.Hour)
	second.Badges = []Badge{{Label: "Flagged", Text: "🚩"}, {Label: "flagged-too", Text: "🚩"}} // one PR, counted once
	m := iconBoard(t, IconsUnicode, 200, 30, append(boardRows(), badgeRow(), second)...)
	mustContain(t, lineWith(t, viewOf(m), "stale"), "🚩 2", "HOT 1")

	c := iconBoard(t, IconsUnicode, 200, 30, badgeRow(), second)
	c, _ = send(t, c, keyMsg("enter"))
	mustContain(t, viewOf(c), "📌 pinned · 🚩 Flagged · HOT hotfix · draft")

	m, _ = send(t, m, keyMsg("/"))
	m, _ = send(t, m, typed("flagged-too")...)
	if got := refsOf(m.view); !slices.Equal(got, []string{"example#43"}) {
		t.Errorf("a badge's label filters %v", got)
	}

	r := sanitizeRow(PRBoardRow{Badges: []Badge{{Label: "Flag\x1b[31mged", Text: "🚩\x07"}, {Label: "empty", Text: "\x1b[0m"}}})
	if !slices.Equal(r.Badges, []Badge{{Label: "Flagged", Text: "🚩"}}) {
		t.Errorf("sanitized badges = %+v", r.Badges)
	}
}
