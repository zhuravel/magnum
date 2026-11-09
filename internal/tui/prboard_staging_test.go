package tui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// stagingRows are PRs on a repository with three staging envs, one row per
// state the STAGE column tells apart, and one on a repository without
// stagings.
func stagingRows() []PRBoardRow {
	row := func(n int, title string, ss ...StagingInfo) PRBoardRow {
		return PRBoardRow{Ref: "talkable/talkable#" + strconv.Itoa(n), Owner: "talkable", Repo: "talkable", Number: n, Title: title,
			State: "reviewed", GHState: "OPEN", ActivityAt: ago(time.Duration(n) * time.Minute), HeadSHA: "aaa1111bbbb2222", Stagings: ss}
	}
	return []PRBoardRow{
		row(31, "Live on void, pushed on bart, failed on bastion",
			StagingInfo{Env: "void", State: "latest", InTip: "aaa1111bbbb2222", PushedAt: ago(time.Hour), DeployedAt: ago(40 * time.Minute),
				RunURL: "https://github.com/talkable/talkable/actions/runs/7"},
			StagingInfo{Env: "bart", State: "pushed", Detail: "ci_running", InTip: "aaa1111bbbb2222", PushedAt: ago(3 * time.Minute),
				RunURL: "https://github.com/talkable/talkable/actions/runs/8"},
			StagingInfo{Env: "bastion", State: "pushed", Detail: "ci_failed_ours", InTip: "aaa1111bbbb2222", PushedAt: ago(2 * time.Hour),
				RunURL: "https://github.com/talkable/talkable/actions/runs/9", Why: "spec/models/coupon_spec.rb"}),
		row(32, "Outdated, none and unknown",
			StagingInfo{Env: "void", State: "outdated", InTip: "old0000cccc3333", Behind: 2, PushedAt: ago(48 * time.Hour)},
			StagingInfo{Env: "bart", State: "none"},
			StagingInfo{Env: "bastion", State: "unknown", Why: "cloning the mirror (since 09:58)"}),
		{Ref: "example/shop#4", Owner: "example", Repo: "shop", Number: 4, Title: "No stagings", State: "reviewed", GHState: "OPEN",
			ActivityAt: ago(time.Hour)}, // last in the updated sort
	}
}

// The STAGE column draws one glyph per staging env, in the config's order,
// in every icon mode: live (latest) green, pushed (CI running, waiting for
// Jenkins, not confirmed, or failed not on the PR's changes) yellow, CI
// failed on the PR's changes red, an older head, not there and unknown; a
// PR of a repository without stagings reads as a dash.
func TestTheStageColumnDrawsOneGlyphPerStagingEnv(t *testing.T) {
	want := map[IconMode][]string{
		IconsUnicode: {"●◐✗", "○·?", "—"},
		IconsNerd:    {"●◐✗", "○·?", "—"},
		IconsASCII:   {"Lpx", "o.?", "-"},
	}
	rows := stagingRows()
	pal := newPRBPalette(defaultStyles)
	for mode, cells := range want {
		p := newPRBPainter(defaultStyles, pal, newGlyphs(mode), boardNow, nil, rows, SortUpdated, true)
		for i, r := range rows {
			c := p.stagingCell(r.Stagings)
			if got := ansi.Strip(c.render(nil)); got != cells[i] {
				t.Errorf("%s %s: STAGE cell %q, want %q", mode, prRef(r), got, cells[i])
			}
		}
	}
	p := p0(pal)
	c := p.stagingCell(rows[0].Stagings)
	for i, want := range []prbColor{{"green", pal.green}, {"yellow", pal.yellow}, {"red", pal.red}} {
		if c[i].st.GetForeground() != want.st.GetForeground() {
			t.Errorf("glyph %d is not %s", i, want.name)
		}
	}
	notOurs := p.stagingCell([]StagingInfo{{Env: "void", State: "pushed", Detail: "ci_failed_not_ours"}})
	if got := ansi.Strip(notOurs.render(nil)); got != "◐" {
		t.Errorf("a CI failure not on the PR's changes draws %q, want the pushed glyph", got)
	}

	m := iconBoard(t, IconsUnicode, 200, 30, rows...)
	v := viewOf(m)
	mustContain(t, v, "STAGE", "●◐✗", "○·?")
}

// A board whose rows carry no staging status has no STAGE column: a
// config without stagings changes nothing on the screen.
func TestNoStagingsNoStageColumn(t *testing.T) {
	m := iconBoard(t, IconsUnicode, 200, 30, ciRows()...)
	mustNotContain(t, viewOf(m), "STAGE")
}

// The card's STAGINGS block says, per env: its state, CI's or Jenkins'
// part of it, the head in the tip or how far behind, when it was deployed
// or pushed, the CI run's page and why it cannot be told.
func TestTheCardShowsWhereThePRStandsOnEachStaging(t *testing.T) {
	m := iconBoard(t, IconsUnicode, 140, 90, stagingRows()...)
	m, _ = send(t, m, keyMsg("enter"))
	v := viewOf(m)
	mustContain(t, v, "STAGINGS",
		"● void     live · head aaa1111 · deployed 40m ago",
		"https://github.com/talkable/talkable/actions/runs/7",
		"◐ bart     pushed · CI running · head aaa1111 · pushed 3m ago",
		"✗ bastion  pushed · CI failed on this PR's changes · head aaa1111 · pushed 2h ago",
		"spec/models/coupon_spec.rb")

	m = iconBoard(t, IconsUnicode, 140, 90, stagingRows()...)
	m, _ = send(t, m, keyMsg("j"), keyMsg("enter"))
	v = viewOf(m)
	mustContain(t, v, "○ void     an older head · old0000 · 2 commits behind · pushed 2d ago",
		"· bart     not there", "? bastion  unknown", "cloning the mirror (since 09:58)")

	m = iconBoard(t, IconsUnicode, 140, 90, stagingRows()...)
	m, _ = send(t, m, keyMsg("j"), keyMsg("j"), keyMsg("enter"))
	mustNotContain(t, viewOf(m), "STAGINGS")
}

// The help's legend explains the STAGE column.
func TestTheHelpExplainsTheStageColumn(t *testing.T) {
	for mode, want := range map[IconMode]string{
		IconsUnicode: "STAGE   ● live   ◐ pushed: CI or Jenkins   ✗ CI failed on the PR's changes   ○ an older head   · not there   ? unknown",
		IconsASCII:   "STAGE   L live   p pushed: CI or Jenkins   x CI failed on the PR's changes   o an older head   . not there   ? unknown",
	} {
		m := iconBoard(t, mode, 200, 90, stagingRows()...)
		m, _ = send(t, m, keyMsg("?"), keyMsg("G"))
		v := viewOf(m)
		mustContain(t, v, want, "one per staging env, in the config's order")
	}
}

// A staging env's words reach the screen cleaned, its run page only when
// it is a web address.
func TestTheStagingsAreSanitized(t *testing.T) {
	r := PRBoardRow{Ref: "talkable/talkable#1", Stagings: []StagingInfo{{Env: "vo\x1b[31mid", State: "pushed", Why: "spec/\x07a.rb",
		RunURL: "javascript:alert(1)"}, {Env: "bart", State: "latest", RunURL: "https://github.com/talkable/talkable/actions/runs/7"}}}
	got := sanitizeRow(r).Stagings
	if got[0].Env != "void" || got[0].Why != "spec/ a.rb" || got[0].RunURL != "" ||
		got[1].RunURL != "https://github.com/talkable/talkable/actions/runs/7" {
		t.Errorf("sanitized %+v", got)
	}
	if !strings.Contains(r.Stagings[0].Env, "\x1b") {
		t.Error("sanitizing changed the source row")
	}
}

// StagingSummary is the card's words without styles or the env's name,
// for `magnum staging`.
func TestStagingSummaryIsTheCardsWords(t *testing.T) {
	rows := stagingRows()
	for i, want := range []string{
		"live · head aaa1111 · deployed 40m ago",
		"pushed · CI running · head aaa1111 · pushed 3m ago",
		"pushed · CI failed on this PR's changes · head aaa1111 · pushed 2h ago",
	} {
		if got := StagingSummary(rows[0].Stagings[i], boardNow); got != want {
			t.Errorf("%s: %q, want %q", rows[0].Stagings[i].Env, got, want)
		}
	}
	if got := StagingSummary(rows[1].Stagings[0], boardNow); got != "an older head · old0000 · 2 commits behind · pushed 2d ago" {
		t.Errorf("outdated: %q", got)
	}
	if got := StagingSummary(StagingInfo{Env: "void", State: "pushed", Detail: "waiting_jenkins"}, boardNow); got != "pushed · CI passed, waiting for Jenkins" {
		t.Errorf("waiting for Jenkins: %q", got)
	}
}
