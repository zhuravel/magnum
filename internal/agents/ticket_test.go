package agents

import (
	"strings"
	"testing"
)

// Every judge prompt names the PR's Jira ticket by its path only, as the
// `ticket` field of its <magnum> block, when the round wrote one: the
// issue is data, so the prompt carries no word of it and no other line
// changes; without one there is no field.
func TestJudgePromptsNameTheTicketFileOnly(t *testing.T) {
	d := judgeFixture()
	own := d
	own.Mode, own.Phase, own.OwnFindings, own.Reports = ModeInitial, PhaseOwnPass, "/r/judge-own.md", nil
	for _, tc := range []struct {
		name string
		data JudgeData
	}{
		{"judge-initial.md", d}, {"judge-rereview.md", d}, {"judge-recovery.md", d}, {"judge-continue.md", d}, {"judge-own-pass.md", own},
	} {
		without, err := RenderPrompt(prompt(t, tc.name), tc.data)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if strings.Contains(without, "ticket") {
			t.Errorf("%s names a ticket without one:\n%s", tc.name, without)
		}
		with := tc.data
		with.TicketFile = "/r/ticket.md"
		got, err := RenderPrompt(prompt(t, tc.name), with)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		const line = "\nticket: /r/ticket.md\n"
		if !strings.Contains(magnumBlock(t, got), line) {
			t.Errorf("%s: the <magnum> block lacks %q:\n%s", tc.name, line, got)
		}
		if strings.Replace(got, line, "\n", 1) != without {
			t.Errorf("%s: the ticket changes more than its field:\n%s", tc.name, got)
		}
	}
}
