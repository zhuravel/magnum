package tui

import (
	"strings"
	"testing"
)

// A PR whose agents ran without its changes to the checkout's own agent
// config (it changes .codex/, so Codex ran with the checkout untrusted):
// the card says so under LAST REVIEW, wrapped to its width; a PR without
// such a note says nothing of it.
func TestPRBoardCardSaysTheAgentsRanWithoutThePRsProjectConfig(t *testing.T) {
	const note = "Codex ran without the PR's .codex/ changes (the checkout was untrusted in its sessions)"
	row := roundsRow()
	row.ProjectNote = note
	card := strings.Join(roundsCard(t, row, 60), " ")
	_, after, ok := strings.Cut(card, "LAST REVIEW")
	if !ok {
		t.Fatalf("no LAST REVIEW section:\n%s", card)
	}
	if got := strings.Join(strings.Fields(after), " "); !strings.Contains(got, note) {
		t.Fatalf("LAST REVIEW lacks the note:\n%s", got)
	}
	mustNotContain(t, strings.Join(roundsCard(t, roundsRow(), 120), "\n"), ".codex/")
}
