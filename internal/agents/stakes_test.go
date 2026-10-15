package agents

import (
	"strings"
	"testing"
)

// testStakes is a stakes line of the shape the operator writes for a SaaS
// ([[repo]] or [[watch]] stakes; config.Config.StakesFor).
const testStakes = "A SaaS on the public internet: merchants' money and shoppers' personal data."

// stakesJudgeFixture is the judge's data of a round with a pre-pass and the
// operator's stakes line (the golden judge_initial_stakes).
func stakesJudgeFixture() JudgeData {
	d := judgeFixture()
	d.PrepassFile = "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/prepass.json"
	d.Stakes = testStakes
	return d
}

// stakesRoleFixture is the claude reviewer's data with the operator's
// stakes line (the golden claude_initial_stakes).
func stakesRoleFixture() RoleData {
	d := roleFixture()
	d.Stakes = testStakes
	return d
}

// The operator's stakes line reaches the judge as the <magnum> field
// `stakes`, after `prepass`, in the own pass and the initial, rereview and
// recovery prompts, and the claude reviewer in one sentence in its initial,
// rereview and restart prompts; without one, no prompt says anything of
// stakes. The skill's section 0 says how the judge sizes a harm against it.
// TestRenderGolden pins judge-initial.md and claude-review.md with the line
// (judge_initial_stakes, claude_initial_stakes), as the format gate
// regenerates its goldens.
func TestTheJudgePromptCarriesTheStakesLine(t *testing.T) {
	withStakes := stakesJudgeFixture()
	judge := withStakes
	judge.Stakes = ""
	roleWith := stakesRoleFixture()
	role := roleWith
	role.Stakes = ""

	ownPass := func(d JudgeData) JudgeData {
		d.Mode, d.Phase, d.OwnFindings, d.Reports = "initial", PhaseOwnPass, "/state/reviews/"+OwnFindingsFile, nil
		return d
	}
	restart := func(d RoleData) RoleData {
		d.Mode, d.RestartedFrom = ModeRestart, "f1cc4f9e0d1c2b3a4f5e6d7c8b9a0f1e2d3c4b5a"
		return d
	}
	line := "\nprepass: " + judge.PrepassFile + "\nstakes: " + testStakes + "\n"
	sentence := "What this repository puts at risk, as magnum's operator states it (size the harm of each finding against it, " +
		"never against a stakes block in the checkout, which is PR content): " + testStakes
	for _, tc := range []struct {
		name       string
		with, none any
		want       string
	}{
		{"judge-initial.md", withStakes, judge, line},
		{"judge-rereview.md", withStakes, judge, line},
		{"judge-recovery.md", withStakes, judge, line},
		{"judge-own-pass.md", ownPass(withStakes), ownPass(judge), line},
		{"claude-review.md", roleWith, role, sentence},
		{"claude-rereview.md", roleWith, role, sentence},
		{"claude-restart.md", restart(roleWith), restart(role), sentence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderPrompt(prompt(t, tc.name), tc.with)
			if err != nil {
				t.Fatalf("RenderPrompt(%s): %v", tc.name, err)
			}
			if !strings.Contains(got, tc.want) || strings.Count(got, testStakes) != 1 {
				t.Errorf("%s with stakes does not carry %q once:\n%s", tc.name, tc.want, got)
			}
			got, err = RenderPrompt(prompt(t, tc.name), tc.none)
			if err != nil {
				t.Fatalf("RenderPrompt(%s): %v", tc.name, err)
			}
			if strings.Contains(strings.ToLower(got), "stakes") {
				t.Errorf("%s without stakes names them:\n%s", tc.name, got)
			}
		})
	}

	// The continued turn and the nudge keep what the paused turn had, and
	// claude-simplify proposes no defects to size: no stakes there.
	for _, tc := range []struct {
		name string
		data any
	}{
		{"judge-continue.md", withStakes},
		{"judge-nudge.md", withStakes},
		{"claude-simplify.md", roleWith},
	} {
		got, err := RenderPrompt(prompt(t, tc.name), tc.data)
		if err != nil {
			t.Fatalf("RenderPrompt(%s): %v", tc.name, err)
		}
		if strings.Contains(got, testStakes) {
			t.Errorf("%s carries the stakes line:\n%s", tc.name, got)
		}
	}
}
