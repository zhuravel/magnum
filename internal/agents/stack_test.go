package agents

import (
	"strings"
	"testing"
)

// stackLine is the judge prompts' one line on the PRs stacked below this
// one (the related step's stack, pipeline's related.go), for #751.
const stackLine = "A finding that sits only on lines a PR stacked below this one introduced (#751) is that PR's: " +
	"`outside_diff` (the skill's section 3), named in the body's related-PR line as the PR it belongs to."

// One PR's review posted a P2 on a line the related PR it was stacked on had
// introduced, and related.json did not say it was stacked. The related_prs
// line says which related PRs are stacked below and above this one, and the
// own pass and the initial, rereview and recovery prompts give a finding
// only on a lower PR's lines to that PR in one line, which names the PRs
// below even when related.json is left out (an earlier review named them);
// the continued turn had the line in the turn it continues. Without a PR
// below, no prompt carries it.
func TestJudgePromptsGiveAFindingOnlyOnAStackedBelowPRsLinesToThatPR(t *testing.T) {
	stacked := judgeFixture()
	stacked.RelatedPRs, stacked.StackedBelow, stacked.StackedAbove = "/r/related.json", []int{751}, []int{753}
	own := stacked
	own.Mode, own.Phase, own.OwnFindings, own.Reports = ModeInitial, PhaseOwnPass, "/r/judge-own.md", nil
	const line = "\nrelated_prs: /r/related.json (#751 stacked below this PR; #753 above it)\n"
	for _, tc := range []struct {
		name string
		data JudgeData
		rule bool
	}{
		{"judge-initial.md", stacked, true}, {"judge-rereview.md", stacked, true}, {"judge-recovery.md", stacked, true},
		{"judge-own-pass.md", own, true}, {"judge-continue.md", stacked, false},
	} {
		got, err := RenderPrompt(prompt(t, tc.name), tc.data)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		mustHave(t, tc.name, got, line)
		if strings.Contains(got, stackLine) != tc.rule {
			t.Errorf("%s carries the stack line: %v, want %v:\n%s", tc.name, !tc.rule, tc.rule, got)
		}
	}

	notNamed := judgeFixture()
	notNamed.StackedBelow = []int{751}
	got, err := RenderPrompt(prompt(t, "judge-rereview.md"), notNamed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, stackLine) || strings.Contains(got, "related_prs") {
		t.Errorf("a PR below without related.json: want the stack line and no related_prs:\n%s", got)
	}

	above := judgeFixture()
	above.RelatedPRs, above.StackedAbove = "/r/related.json", []int{753, 760}
	for _, name := range []string{"judge-initial.md", "judge-rereview.md", "judge-recovery.md"} {
		got, err := RenderPrompt(prompt(t, name), above)
		if err != nil {
			t.Fatal(err)
		}
		mustHave(t, name, got, "\nrelated_prs: /r/related.json (#753, #760 stacked above this PR)\n")
		if strings.Contains(got, "stacked below") {
			t.Errorf("%s names a PR below without one:\n%s", name, got)
		}
	}
}

// RelatedStackLine says the stacks in words: the PRs below, then those
// above, each list by number; nothing without either.
func TestRelatedStackLineSaysBothDirections(t *testing.T) {
	for _, tc := range []struct {
		below, above []int
		want         string
	}{
		{nil, nil, ""},
		{[]int{751}, nil, "#751 stacked below this PR"},
		{[]int{750, 751}, nil, "#750, #751 stacked below this PR"},
		{nil, []int{753}, "#753 stacked above this PR"},
		{[]int{751}, []int{753, 760}, "#751 stacked below this PR; #753, #760 above it"},
	} {
		if got := RelatedStackLine(tc.below, tc.above); got != tc.want {
			t.Errorf("RelatedStackLine(%v, %v) = %q, want %q", tc.below, tc.above, got, tc.want)
		}
	}
}

func mustHave(t *testing.T, what, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("%s lacks %q:\n%s", what, sub, s)
	}
}
