package agents

import (
	"strconv"
	"strings"
	"testing"

	"github.com/zhuravel/magnum"
)

// The judge rules of 2026-11-12 (the operator approved each): what a choice
// the PR states or copies covers, what `style_only` never is, a newly
// reached model's defaults, a wrong value at P2, and a case that ends as on
// the base. This file sorts before skill_pins_test.go, so its pins are
// recorded before the pins file is compared.

// skillSection is section n of SKILL.md, from its "## <n>. " heading to the
// next "## " heading, or "" when the skill has no such section.
func skillSection(n int) string {
	_, sec, ok := strings.Cut(string(magnum.Skill), "\n## "+strconv.Itoa(n)+". ")
	if !ok {
		return ""
	}
	sec, _, _ = strings.Cut(sec, "\n## ")
	return sec
}

// A reviewer proved that a staff tool grant tests only a per-site flag while
// the PR's own decision record says the gate keeps the tools from client
// sites, and the judge rejected it as `style_only` because the author chose
// that flag; a person ranked it P1. The judge's own pass waived a guard
// copied from a blocklist as a documented rule, though the copy loosens
// where the blocklist tightens; a person called the result major. A choice
// the PR states or copies covers only what its text names: a consequence
// that breaks a goal the same text states, or a copied guard that relaxes
// where its source restricts, stays at its priority. The rule sits in
// section 3, after the reason codes the judge picks from, and takes in
// section 4's older words on a listed mechanism (a default, a fallback
// chain; TestSkillCountsImpactNobodyMeasuredAsImpact).
func TestAChoiceThePRStatesOrCopiesCoversOnlyWhatItsTextNames(t *testing.T) {
	const rule = "A choice the PR states or copies (a predicate, a default, a fallback chain, a tool set, a sibling's guard) covers only what its text names: " +
		"a consequence that breaks a goal the same text states stands at its priority, " +
		"as does a copied guard that relaxes where its source restricts."
	skillSays(t, []string{rule}, nil)
	if !strings.Contains(skillSection(3), rule) {
		t.Error("the rule on a stated or copied choice is not in section 3, beside the reason codes")
	}
}

// `style_only` was used for substance: a reproduced mutation that left 65
// examples green, a stand-in only a spec calls against the repository's
// stated no-unused-code rule, and six false doc sentences, each ranked P2 or
// P3 by a person. style_only is taste, naming or formatting, never one of
// those.
func TestStyleOnlyIsNeverAGreenMutationAStatedRuleOrAFalseFact(t *testing.T) {
	skillSays(t, []string{
		"- `style_only`: taste, naming or formatting, never a green mutation of production output, a stated repository rule or a false fact;",
	}, []string{"- `style_only`: taste, naming or formatting (section 4);"})
}

// Dropping a sampling parameter let a call reach models that think by
// default; the thinking tokens count toward its small output limit, so a
// reply can end before any text (a person: P1, from the provider's migration
// guide), while magnum posted a P3 on the same lines. The version sentence
// covered an API version, a runtime and an SDK, and only their age. Every
// review prompt with that sentence now has the reviewer read a newly reached
// model's defaults; the factory's local review prompt is left as it is.
func TestReviewersReadTheDefaultsOfAModelACallNowReaches(t *testing.T) {
	const defaults = "the defaults that change the call (thinking on by default and counted in the output limit, parameters it rejects, data-use terms)"
	restart := roleFixture()
	restart.Mode, restart.RestartedFrom = ModeRestart, "f1cc4f9e0d1c2b3a4f5e6d7c8b9a0f1e2d3c4b5a"
	rereview := roleFixture()
	rereview.Mode = ModeRereview
	for name, d := range map[string]RoleData{"claude-review.md": roleFixture(), "claude-rereview.md": rereview, "claude-restart.md": restart} {
		got, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := "When the PR changes which model a call reaches (a model id, a parameter dropped or changed), " +
			"read that model's page on the provider's own site (WebFetch) for " + defaults + ", and check each call's parameters against them."
		if !strings.Contains(got, want) {
			t.Errorf("%s lacks %q", name, want)
		}
	}
	own := judgeFixture()
	own.Mode, own.Phase, own.OwnFindings, own.Reports = ModeInitial, PhaseOwnPass, "/r/judge-own.md", nil
	got, err := RenderPrompt(prompt(t, "judge-own-pass.md"), own)
	if err != nil {
		t.Fatal(err)
	}
	if want := "When the PR changes which model a call reaches (a model id, a parameter dropped or changed), read that model's page on the provider's own site for " +
		defaults + ", and check each call's parameters against them."; !strings.Contains(got, want) {
		t.Errorf("judge-own-pass.md lacks %q", want)
	}
	got, err = RenderPrompt(prompt(t, "claude-review-local.md"), localReviewer())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "which model a call reaches") {
		t.Error("claude-review-local.md, the factory's local review prompt, changed: it is not this rule's to change")
	}
}

// magnum's own probe showed that every unsubscribe webhook of a loyalty
// program sends the member flag as false, and a rewarded trusted referral
// shows as fraud in a portal and its API: both were posted at P3, and people
// ranked them P2 and major. The visible-defect rule covered markup, raw
// keys, error pages and layouts, not wrong values. Its own sentence stays.
func TestAWrongValueShownOrSentThatOthersActOnIsAtLeastP2(t *testing.T) {
	const rule = "So is a wrong value shown or sent in normal use that others act on."
	skillSays(t, []string{
		"A rendering defect users see in normal use (markup shown as text, a raw translation key, an error page, a broken layout) is at least P2. " + rule,
	}, nil)
	if !strings.Contains(skillSection(4), rule) {
		t.Error("the wrong-value rule is not in section 4, which every review reads")
	}
}

// Two P2s on one PR were declined because the case ends as on the base, and
// five P3s because the base does the same. A case the PR's new behaviour
// does not reach that ends as on the base is P3 at most unless the PR claims
// to cover it, and its comment says so; the same result for the same
// trigger on the base is `pre_existing`. The incomplete-fix rule stays: a
// site left failing of the class the PR claims to fix keeps its priority.
func TestACaseThePRDoesNotReachThatEndsAsOnTheBaseIsP3AtMost(t *testing.T) {
	const rule = "A case the PR's new behaviour does not reach that ends as on the base is P3 at most unless the PR claims to cover it; " +
		"its comment says `On <base_ref>, the same.` The same result for the same trigger on the base is `pre_existing`."
	skillSays(t, []string{
		rule,
		"`pre_existing`: the base has the same problem and this PR neither makes it worse nor claims to fix it " +
			"(a request it secures, text it escapes, a value it treats as empty: a site of that class left failing is its incomplete fix);",
	}, nil)
	if !strings.Contains(skillSection(4), "Report only a problem that this PR introduces or exposes. "+rule) {
		t.Error("the rule on a case that ends as on the base does not follow section 4's first sentence")
	}
}
