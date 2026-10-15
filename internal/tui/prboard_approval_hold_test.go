package tui

import (
	"slices"
	"strings"
	"testing"
)

// The full pass that follows a review that held its approval
// (approve_after_full_pass) runs as a recovery in fresh sessions: the
// card's LAST ROUND calls it a full pass before approval.
func TestTheCardNamesAFullPassBeforeApproval(t *testing.T) {
	row := PRBoardRow{Owner: "talkable", Repo: "talkable", Number: 732, State: "reviewed",
		RoundWhy: &RoundWhy{Kind: "recovery", FullPass: true, BeforeApproval: true, Roles: []string{"codex-judge", "claude-review"}}}
	section := lastRoundSection(t, roundsCard(t, row, 100))
	if !slices.ContainsFunc(section, func(l string) bool {
		return strings.Contains(l, "full pass (before approval): codex-judge, claude-review")
	}) {
		t.Errorf("LAST ROUND = %q", section)
	}
}
