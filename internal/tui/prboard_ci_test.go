package tui

import (
	"strconv"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// ciRows are PRs with every kind of CI the board draws: required checks
// failing (one more pending, of an older commit), every check passed,
// checks running, checks failed, no checks, a required check passed while
// others still run, a required check that never ran, a skipped one and a
// run whose every check skipped.
func ciRows() []PRBoardRow {
	row := func(n int, title string, ci *CIInfo) PRBoardRow {
		return PRBoardRow{Ref: "talkable/example#" + strconv.Itoa(n), Owner: "talkable", Repo: "example", Number: n, Title: title,
			State: "reviewed", GHState: "OPEN", ActivityAt: ago(time.Duration(n) * time.Minute), HeadSHA: "abcdef1234", CI: ci}
	}
	return []PRBoardRow{
		row(20, "Required checks failing", &CIInfo{State: "failed", Total: 65, Passed: 61, Failed: 3, Pending: 1,
			Failing: []string{"CI / rspec (3)", "CI / capybara (1)", "CI / brakeman"},
			Workflows: []WorkflowCI{
				{Name: "CI", State: "failed", Passed: 59, Failed: 3, Pending: 1, Total: 63},
				{Name: "", State: "passed", Passed: 2, Total: 2},
			},
			Required:       []CheckState{{Name: "workflow:CI", State: "pending"}, {Name: "Completion", State: "failed"}},
			RequiredSource: "github", Stale: true}),
		row(21, "Every check passed", &CIInfo{State: "passed", Total: 65, Passed: 60, Skipped: 5,
			Workflows: []WorkflowCI{{Name: "CI", State: "passed", Passed: 60, Total: 65}}}),
		row(22, "Checks running", &CIInfo{State: "pending", Total: 65, Passed: 40, Pending: 25}),
		row(23, "Checks failed", &CIInfo{State: "failed", Total: 65, Passed: 63, Failed: 2,
			Failing: []string{"CI / rspec (3)", "Lint / rubocop"},
			Workflows: []WorkflowCI{
				{Name: "CI", State: "failed", Passed: 62, Failed: 1, Total: 63},
				{Name: "Lint", State: "failed", Passed: 1, Failed: 1, Total: 2},
			}}),
		row(24, "No checks", &CIInfo{State: "none"}),
		row(25, "Required check passed", &CIInfo{State: "pending", Total: 3, Passed: 1, Pending: 2,
			Required: []CheckState{{Name: "Completion", State: "passed"}}, RequiredSource: "config"}),
		row(26, "Required check never ran", &CIInfo{State: "passed", Total: 1, Passed: 1,
			Required: []CheckState{{Name: "Completion", State: "missing"}}, RequiredSource: "github"}),
		row(27, "Required check skipped", &CIInfo{State: "passed", Total: 4, Passed: 2, Skipped: 2,
			Required: []CheckState{{Name: "Lint", State: "passed"}, {Name: "Completion", State: "skipped"}}, RequiredSource: "github"}),
		row(28, "Every check skipped", &CIInfo{State: "skipped", Total: 4, Skipped: 4}),
	}
}

// The CI column shows the worst required check by name ("+1": one more
// required check), or the counts when the repository requires none, in
// every icon mode: failed red, pending yellow, passed green. A CI of an
// older commit carries the stale mark; no checks and an unknown CI read
// as a dash.
func TestPRBoardCICell(t *testing.T) {
	want := map[IconMode][]string{
		IconsUnicode: {"✗ Completion +1 ⟳", "✓ 65/65", "◌ 40/65", "✗ 2 failed", "—", "✓ Completion",
			"– Completion not run", "⊘ Completion skipped +1", "– not run", "—"},
		IconsNerd: {"\U000F0159 Completion +1 \uf464", "\uf058 65/65", "\ue641 40/65", "\U000F0159 2 failed", "—", "\uf058 Completion",
			"\uf48b Completion not run", "\uf517 Completion skipped +1", "\uf48b not run", "—"},
		IconsASCII: {"x Completion +1 ~", "+ 65/65", "o 40/65", "x 2 failed", "-", "+ Completion",
			"- Completion not run", "/ Completion skipped +1", "- not run", "-"},
	}
	rows := append(ciRows(), PRBoardRow{Ref: "talkable/example#29", Number: 29}) // CI unknown
	pal := newPRBPalette(defaultStyles)
	// The mark's color: failed red, pending, not run and skipped yellow,
	// passed green; every check skipped is dim.
	colors := map[int]prbColor{0: {"red", pal.red}, 1: {"green", pal.green}, 2: {"yellow", pal.yellow}, 3: {"red", pal.red},
		5: {"green", pal.green}, 6: {"yellow", pal.yellow}, 7: {"yellow", pal.yellow}, 8: {"dim", defaultStyles.Dim}}
	for mode, cells := range want {
		p := newPRBPainter(defaultStyles, pal, newGlyphs(mode), boardNow, nil, rows, SortUpdated, true)
		for i, r := range rows {
			c := p.ciCell(r.CI)
			if got := ansi.Strip(c.render(nil)); got != cells[i] {
				t.Errorf("%s %s: CI cell %q, want %q", mode, prRef(r), got, cells[i])
			}
			if want, ok := colors[i]; ok && c[0].st.GetForeground() != want.st.GetForeground() {
				t.Errorf("%s %s: CI cell is not %s", mode, prRef(r), want.name)
			}
		}
	}

	// Worst first: failed, then not run, skipped, pending and passed. A
	// check that did not run is not dimmed away: its name stays dim, its
	// mark yellow.
	if c := p0(pal).ciCell(ciRows()[6].CI); c[1].st.GetForeground() != defaultStyles.Dim.GetForeground() {
		t.Error("a required check that never ran: its name is not dim")
	}
	for _, tc := range []struct {
		req  []CheckState
		want string
	}{
		{[]CheckState{{Name: "a", State: "passed"}, {Name: "b", State: "pending"}, {Name: "c", State: "missing"}}, "– c not run +2"},
		{[]CheckState{{Name: "a", State: "pending"}, {Name: "b", State: "skipped"}}, "⊘ b skipped +1"},
		{[]CheckState{{Name: "a", State: "pending"}, {Name: "b", State: "passed"}}, "◌ a +1"},
		{[]CheckState{{Name: "a", State: "missing"}, {Name: "b", State: "failed"}, {Name: "c", State: "failed"}}, "✗ b +2"},
		{[]CheckState{{Name: "a", State: "passed"}, {Name: "b", State: "passed"}}, "✓ a +1"},
		// A glob shows its label and how many of its checks finished.
		{[]CheckState{{Name: "ci / *", Label: "ci", State: "passed", Done: 3, Total: 3}}, "✓ ci 3/3"},
		{[]CheckState{{Name: "ci / *", Label: "ci", State: "pending", Done: 1, Total: 3}}, "◌ ci 1/3"},
		{[]CheckState{{Name: "ci / *", Label: "ci", State: "failed", Done: 3, Total: 3}}, "✗ ci"},
		{[]CheckState{{Name: "ci / *", Label: "ci", State: "missing"}}, "– ci not run"},
	} {
		if got := ansi.Strip(p0(pal).ciCell(&CIInfo{State: "passed", Required: tc.req}).render(nil)); got != tc.want {
			t.Errorf("required %v: %q, want %q", tc.req, got, tc.want)
		}
	}
}

// p0 is a Unicode painter with no rows.
func p0(pal prbPalette) prbPainter {
	return newPRBPainter(defaultStyles, pal, newGlyphs(IconsUnicode), boardNow, nil, nil, SortUpdated, true)
}

type prbColor struct {
	name string
	st   lipgloss.Style
}

// The card's CI section lists each workflow's counts, the failed checks
// by name (without the workflow when one holds them all) and the required
// checks' states with who requires them, and says when the CI is an
// older commit's or a skipped required check hides a failure; a PR
// without CI has no section.
func TestPRBoardCardShowsCI(t *testing.T) {
	card := func(mode IconMode, n int) string {
		t.Helper()
		m := iconBoard(t, mode, 120, 80, ciRows()...)
		for range n {
			m, _ = send(t, m, keyMsg("j"))
		}
		m, _ = send(t, m, keyMsg("enter"))
		return viewOf(m)
	}
	mustContain(t, card(IconsUnicode, 0), "CI (older commit)",
		"✗ CI 59/63 passed · 3 failed · 1 pending", "✓ other checks 2/2 passed",
		"Failed: rspec (3), capybara (1), brakeman",
		"Required: ◌ workflow:CI pending · ✗ Completion failed (required by GitHub)")
	v := card(IconsUnicode, 1)
	mustContain(t, v, "✓ CI 60/65 passed")
	mustNotContain(t, v, "older commit", "Failed:", "Required:")
	mustContain(t, card(IconsUnicode, 2), "◌ 40/65 passed · 25 pending") // no workflow known: the whole run
	mustContain(t, card(IconsUnicode, 3), "✗ CI 62/63 passed · 1 failed", "✗ Lint 1/2 passed · 1 failed",
		"Failed: CI / rspec (3), Lint / rubocop")
	mustContain(t, card(IconsUnicode, 4), "no checks")
	mustContain(t, card(IconsUnicode, 5), "Required: ✓ Completion passed (required by config)")
	mustContain(t, card(IconsUnicode, 6), "Required: – Completion not run (required by GitHub)")
	v = card(IconsUnicode, 7)
	mustContain(t, v, "Required: ✓ Lint passed · ⊘ Completion skipped (required by GitHub)",
		"(GitHub accepts a skipped required check; a cancelled dependency skips it)")
	mustContain(t, card(IconsUnicode, 8), "– not run")
	mustNotContain(t, card(IconsUnicode, 6), "GitHub accepts")
	mustContain(t, card(IconsNerd, 0), "\uf52e CI (older commit)", "\U000F0159 CI 59/63 passed", "\uf058 other checks 2/2 passed",
		"Required: \ue641 workflow:CI pending · \U000F0159 Completion failed")
	mustContain(t, card(IconsNerd, 6), "\uf48b Completion not run")
	mustContain(t, card(IconsNerd, 7), "\uf517 Completion skipped")
	mustContain(t, card(IconsASCII, 0), "x CI 59/63 passed", "Required: o workflow:CI pending | x Completion failed")
	mustContain(t, card(IconsASCII, 7), "/ Completion skipped")

	m, _, _ := newBoard(t, 120, 80, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("enter"))
	mustContain(t, viewOf(m), "LAST REVIEW", "REVIEWERS")
	mustNotContain(t, viewOf(m), "│ CI ")
}

// The help's legend explains the CI column in every icon mode.
func TestPRBoardHelpExplainsCI(t *testing.T) {
	for mode, want := range map[IconMode][]string{
		IconsUnicode: {"CI   ✓ passed   ✗ failed   ◌ pending   – not run   ⊘ skipped", "⟳ of an older commit"},
		IconsNerd:    {"CI   \uf058 passed   \U000F0159 failed   \ue641 pending   \uf48b not run   \uf517 skipped", "\uf464 of an older commit"},
		IconsASCII:   {"CI   + passed   x failed   o pending   - not run   / skipped", "~ of an older commit"},
	} {
		m := iconBoard(t, mode, 200, 90, ciRows()...)
		m, _ = send(t, m, keyMsg("?"), keyMsg("G"))
		mustContain(t, viewOf(m), append(want, "65/65 checks done", "Completion +1: the worst required check and how many more")...)
	}
}

// GitHub's check and workflow names reach the screen without escape
// sequences or control characters, and cleaning a row leaves its source
// untouched.
func TestPRBoardSanitizesCI(t *testing.T) {
	r := PRBoardRow{Ref: "talkable/example#1", CI: &CIInfo{State: "fail\x1b[0med", RequiredSource: "git\x1b[1mhub",
		Failing:   []string{"CI / \x1b[31mrspec"},
		Workflows: []WorkflowCI{{Name: "C\x07I", State: "failed"}},
		Required:  []CheckState{{Name: "comp\x1b]8;;http://x\x07letion", State: "pending\n"}}}}
	got := sanitizeRow(r).CI
	if got.State != "failed" || got.RequiredSource != "github" || got.Failing[0] != "CI / rspec" || got.Workflows[0].Name != "C I" ||
		got.Required[0].Name != "completion" || got.Required[0].State != "pending" {
		t.Errorf("sanitized CI = %+v", *got)
	}
	if r.CI.Failing[0] != "CI / \x1b[31mrspec" || r.CI.Workflows[0].Name != "C\x07I" || r.CI.Required[0].State != "pending\n" {
		t.Errorf("sanitizing changed the source row: %+v", *r.CI)
	}
}
