package tui

import (
	"slices"
	"strings"
	"testing"
)

// A scheduled audit pass runs as a recovery in fresh sessions: the card's
// LAST ROUND calls it an audit pass, not a recovery.
func TestTheCardNamesAnAuditPass(t *testing.T) {
	row := PRBoardRow{Owner: "talkable", Repo: "talkable", Number: 731, State: "reviewed",
		RoundWhy: &RoundWhy{Kind: "recovery", AuditPass: true, Roles: []string{"codex-judge", "claude-review"}}}
	section := lastRoundSection(t, roundsCard(t, row, 100))
	if !slices.ContainsFunc(section, func(l string) bool { return strings.Contains(l, "audit pass: codex-judge, claude-review") }) ||
		slices.ContainsFunc(section, func(l string) bool { return strings.Contains(l, "recovery") }) {
		t.Errorf("LAST ROUND = %q", section)
	}
}
