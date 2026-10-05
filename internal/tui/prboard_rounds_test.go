package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// roundsCard is the card of row at width inner, as plain lines.
func roundsCard(t *testing.T, row PRBoardRow, inner int) []string {
	t.Helper()
	m, _, _ := newBoard(t, 160, 50, PRBoardOptions{})
	p := m.painter()
	return strings.Split(ansi.Strip(strings.Join(p.cardContent(sanitizeRow(row), inner), "\n")), "\n")
}

// lastRoundSection is the lines of the LAST ROUND section: from its heading
// to the blank line before the next one.
func lastRoundSection(t *testing.T, lines []string) []string {
	t.Helper()
	for i, l := range lines {
		if !strings.HasPrefix(l, "LAST ROUND") && !strings.Contains(l, "LAST ROUND") {
			continue
		}
		end := i + 1
		for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
			end++
		}
		return lines[i:end]
	}
	t.Fatalf("no LAST ROUND section:\n%s", strings.Join(lines, "\n"))
	return nil
}

func roundsRow() PRBoardRow {
	return PRBoardRow{Owner: "talkable", Repo: "talkable", Number: 729, Title: "Speed up the grid", State: "reviewed",
		LastRound: &RoundTimings{Round: 3, Kind: "rereview", Total: 34 * time.Minute,
			Stages: []StageTiming{{Name: "claude-review", Duration: 18 * time.Minute}}}}
}

// The LAST ROUND section of the card says which roles the round ran and why:
// the round's kind and roles, what triage dropped (in the model's words), the
// roles a change of their code added and the ones asked for.
func TestCardLastRoundSaysWhichRolesRanAndWhy(t *testing.T) {
	cases := []struct {
		name string
		why  RoundWhy
		want []string
		not  []string
	}{
		{"a re-review that triage narrowed",
			RoundWhy{Kind: "rereview", Roles: []string{"codex-judge", "claude-review"}, Triaged: true,
				Skipped: []string{"codex-review", "claude-simplify"}, Reason: "small Ruby-only change"},
			[]string{"re-review: codex-judge, claude-review", `triage skipped codex-review, claude-simplify: "small Ruby-only change"`},
			[]string{"asked for", "rerun"}},
		{"a first review",
			RoundWhy{Kind: "initial", Roles: []string{"codex-judge", "claude-review", "codex-review"}},
			[]string{"first review: codex-judge, claude-review, codex-review"},
			[]string{"triage"}},
		{"a continue runs the judge alone",
			RoundWhy{Kind: "continue", Roles: []string{"codex-judge"}},
			[]string{"continue (finishing an interrupted round): judge only"},
			[]string{"triage", "codex-judge"}}, // "judge only", not the judge's name
		{"a recovery",
			RoundWhy{Kind: "recovery", Roles: []string{"codex-judge", "claude-review"}},
			[]string{"recovery (a fresh judge session): codex-judge, claude-review"}, nil},
		{"triage that kept every role",
			RoundWhy{Kind: "rereview", Roles: []string{"codex-judge", "claude-review"}, EveryRole: "the diff could not be read: 503"},
			[]string{"re-review: codex-judge, claude-review", "triage ran every role: the diff could not be read: 503"},
			[]string{"skipped"}},
		{"triage that dropped nothing",
			RoundWhy{Kind: "rereview", Roles: []string{"codex-judge", "claude-review"}, Triaged: true, Reason: "touches the payment code"},
			[]string{`triage kept every role: "touches the payment code"`}, []string{"skipped"}},
		{"a rerun that added a role",
			RoundWhy{Kind: "rereview", Roles: []string{"codex-judge", "claude-review", "claude-simplify"},
				Requested: []string{"claude-simplify"}, Reruns: []RoleRerun{{Role: "claude-simplify", Lines: 212}}},
			[]string{"rerun added claude-simplify (212 lines changed)"},
			[]string{"asked for"}}, // the rerun is not the operator's request
		{"a requested role",
			RoundWhy{Kind: "rereview", Roles: []string{"codex-judge", "claude-simplify"}, Requested: []string{"claude-simplify"}},
			[]string{"asked for: claude-simplify"}, []string{"rerun"}},
		{"a post-merge review",
			RoundWhy{Kind: "rereview", PostMerge: true, Roles: []string{"codex-judge"}},
			[]string{"post-merge re-review: codex-judge"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := roundsRow()
			row.RoundWhy = &c.why
			section := strings.Join(lastRoundSection(t, roundsCard(t, row, 120)), "\n")
			mustContain(t, section, c.want...)
			mustNotContain(t, section, c.not...)
			// The timings stay under the heading.
			mustContain(t, section, "LAST ROUND (3, rereview)", "claude-review 18m00s")
		})
	}
}

// The lines wrap to the card's width like the other sections, and the
// section names the round even when the PR has no timings.
func TestCardLastRoundWrapsTheReasonAndNeedsNoTimings(t *testing.T) {
	row := roundsRow()
	row.LastRound = nil
	row.RoundWhy = &RoundWhy{Kind: "rereview", Roles: []string{"codex-judge", "claude-review"}, Triaged: true,
		Skipped: []string{"codex-review", "claude-simplify"},
		Reason:  "only Ruby specs and one migration changed, nothing the skipped roles review"}
	const inner = 50
	lines := roundsCard(t, row, inner)
	section := lastRoundSection(t, lines)
	if len(section) < 4 {
		t.Fatalf("the reason did not wrap:\n%s", strings.Join(section, "\n"))
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > inner {
			t.Errorf("line is %d cells wide, card is %d: %q", w, inner, l)
		}
	}
	if section[0] != "LAST ROUND" && !strings.HasSuffix(strings.TrimSpace(section[0]), "LAST ROUND") {
		t.Errorf("heading = %q, want LAST ROUND without a round", section[0])
	}
	joined := strings.Join(strings.Fields(strings.Join(section[1:], " ")), " ")
	mustContain(t, joined, `triage skipped codex-review, claude-simplify: "only Ruby specs and one migration changed, nothing the skipped roles review"`)
}

// A PR without what the round ran gets no such lines, and no LAST ROUND
// section without a round at all.
func TestCardLastRoundWithoutRoundWhy(t *testing.T) {
	row := roundsRow()
	mustNotContain(t, strings.Join(lastRoundSection(t, roundsCard(t, row, 120)), "\n"), "triage", "asked for", "rerun", "first review", "re-review")
	row.LastRound = nil
	mustNotContain(t, strings.Join(roundsCard(t, row, 120), "\n"), "LAST ROUND")
}

// Triage's reason is the model's reading of the PR: escape sequences and
// control characters in it (or in a role name) never reach the screen.
func TestSanitizeRowCleansRoundWhy(t *testing.T) {
	row := roundsRow()
	row.RoundWhy = &RoundWhy{Kind: "rere\x1b[2Jview", Roles: []string{"codex-judge\x07"}, Triaged: true,
		Skipped: []string{"claude-\x1b[31msimplify"}, Reason: "\x1b[31mred\x1b[0m\n\ttext\x1b]0;title\x07",
		EveryRole: "bad\x1b[H", Reruns: []RoleRerun{{Role: "x\x1b[1my", Lines: 3}}, Requested: []string{"a\x00b"}}
	got := sanitizeRow(row).RoundWhy
	if got.Reason != "red text" || got.Kind != "rereview" || got.Skipped[0] != "claude-simplify" ||
		got.EveryRole != "bad" || got.Reruns[0].Role != "xy" || got.Roles[0] != "codex-judge" || got.Requested[0] != "a b" {
		t.Fatalf("sanitized = %+v", *got)
	}
	if row.RoundWhy.Reason != "\x1b[31mred\x1b[0m\n\ttext\x1b]0;title\x07" {
		t.Fatal("sanitizeRow changed the caller's row")
	}
	m, _, _ := newBoard(t, 160, 50, PRBoardOptions{})
	card := strings.Join(m.painter().cardContent(sanitizeRow(row), 120), "\n")
	if strings.ContainsAny(strings.ReplaceAll(ansi.Strip(card), "\n", ""), "\x1b\x07\x00") {
		t.Fatalf("the card holds a control character: %q", ansi.Strip(card))
	}
}

// The facts table shows what the PR's reviews cost over the window, next to
// the rounds of today; a dash when it had no run in the window.
func TestCardFactsShowAgentTime(t *testing.T) {
	row := roundsRow()
	row.Spend = &SpendInfo{Window: 7 * 24 * time.Hour, AgentTime: 9*time.Hour + 2*time.Minute + 30*time.Second, Rounds: 15}
	row.RoundsToday = 2
	var spend, today string
	for _, l := range roundsCard(t, row, 120) {
		switch {
		case strings.Contains(l, "Agent time 7d"):
			spend = l
		case strings.Contains(l, "Rounds today"):
			today = l
		}
	}
	if !strings.Contains(spend, "9h02m · 15 rounds") {
		t.Errorf("agent time line = %q, want 9h02m · 15 rounds", spend)
	}
	if today == "" {
		t.Error("no Rounds today line")
	}

	row.Spend = &SpendInfo{Window: 7 * 24 * time.Hour, AgentTime: 4*time.Minute + 5*time.Second, Rounds: 1}
	mustContain(t, strings.Join(roundsCard(t, row, 120), "\n"), "Agent time 7d", "4m05s · 1 round")
	mustNotContain(t, strings.Join(roundsCard(t, row, 120), "\n"), "1 rounds")

	row.Spend = nil
	for _, l := range roundsCard(t, row, 120) {
		if strings.Contains(l, "Agent time 7d") && !strings.ContainsAny(l, "-—") {
			t.Errorf("a PR without runs shows %q, want a dash", l)
		}
	}
	mustContain(t, strings.Join(roundsCard(t, row, 120), "\n"), "Agent time 7d")
}
