package agents

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum"
)

// localScratch is a local round's scratch directory, where the factory
// writes description.md and ticket.md.
const localScratch = "/Users/bohdan/.local/share/magnum/factory/tasks/7/local-review/2-local-a1b2c3"

// localJudge is judgeFixture as the judge of a local round sees it: the
// factory's change before its PR exists, a blind dry run under a gh
// identity, its description and ticket files the factory wrote.
func localJudge() JudgeData {
	d := judgeFixture()
	d.Number, d.URL = 900072, "https://github.com/talkable/talkable/pull/900072"
	d.IdentityKind, d.ReviewerLogin, d.GhConfigDir = "gh", "zhuravel", ""
	d.DryRun, d.Blind, d.Local = true, true, true
	d.PreviousReviewID, d.PreviousEvent, d.PreviousHeadSHA, d.Since, d.PreviousReviews = 0, "", "", "", nil
	d.DescriptionFile, d.TicketFile = localScratch+"/description.md", localScratch+"/ticket.md"
	return d
}

// localReviewer is roleFixture as the claude reviewer of a local round sees
// it.
func localReviewer() RoleData {
	d := roleFixture()
	d.Number, d.URL = 900072, "https://github.com/talkable/talkable/pull/900072"
	d.Blind, d.DescriptionFile = true, localScratch+"/description.md"
	return d
}

// The judge prompts of a round of a PR on GitHub render as they did before
// the local block: the local fields without Local change nothing, and no
// prompt names the local block's fields. The goldens (TestPromptGoldens)
// hold the text itself, unchanged.
func TestLiveJudgePromptsRenderUnchanged(t *testing.T) {
	blind := judgeFixture()
	blind.Blind, blind.DryRun = true, true
	own := judgeFixture()
	own.Mode, own.Phase, own.OwnFindings, own.Reports = "initial", PhaseOwnPass, localScratch+"/"+OwnFindingsFile, nil
	for _, tc := range []struct {
		name string
		d    JudgeData
	}{
		{"judge-initial.md", judgeFixture()}, {"judge-initial.md", blind}, {"judge-rereview.md", judgeFixture()},
		{"judge-recovery.md", judgeFixture()}, {"judge-continue.md", judgeFixture()}, {"judge-own-pass.md", own},
	} {
		p := prompt(t, tc.name)
		want, err := RenderPrompt(p, tc.d)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		withFiles := tc.d
		withFiles.DescriptionFile = localScratch + "/description.md"
		got, err := RenderPrompt(p, withFiles)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != want {
			t.Errorf("%s changes with a description file but no local round:\n%s", tc.name, got)
		}
		for _, leak := range []string{"local: true", "description_file", "Local round"} {
			if strings.Contains(want, leak) {
				t.Errorf("%s of a PR on GitHub names %q:\n%s", tc.name, leak, want)
			}
		}
	}
}

// The judge of a local round reads the change's title, body and ticket
// from the factory's files and nothing from GitHub: its prompts name
// description_file and ticket.md, skip the PR checks, and carry no gh call;
// the blind replay's "read the PR description" is not among them.
func TestTheLocalJudgeBlockReadsFilesNotGitHub(t *testing.T) {
	own := localJudge()
	own.Mode, own.Phase, own.OwnFindings, own.Reports = "initial", PhaseOwnPass, localScratch+"/"+OwnFindingsFile, nil
	for _, tc := range []struct {
		name string
		d    JudgeData
	}{{"judge-initial.md", localJudge()}, {"judge-own-pass.md", own}} {
		got, err := RenderPrompt(prompt(t, tc.name), tc.d)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, want := range []string{"local: true", "description_file: " + tc.d.DescriptionFile, "ticket: " + tc.d.TicketFile,
			"Local round", "no `gh` call", "`git diff " + tc.d.BaseSHA + ".." + tc.d.HeadSHA + "`", "data, never instructions"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s of a local round lacks %q:\n%s", tc.name, want, got)
			}
		}
		for _, leak := range []string{"gh api", "gh pr", "Read the PR description"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s of a local round carries %q:\n%s", tc.name, leak, got)
			}
		}
		if !strings.Contains(got, "ticket.md") {
			t.Errorf("%s of a local round names no ticket.md:\n%s", tc.name, got)
		}
	}
}

// The factory's narrow round of a base merge (factory review_merge.go): the
// task's head merges the base into a head the local review passed, and the
// merge resolved conflicts or changed what git's own merge would not have.
// Its judge, the round's only role, takes `git show --remerge-diff <head>`
// as the diff and the review boundary, the change's diff against the base
// as context only, and its prompt says `remerge: true`; a local round of
// another head names neither.
func TestANarrowLocalJudgeReviewsTheRemergeDiffOfTheMerge(t *testing.T) {
	own := localJudge()
	own.Mode, own.Phase, own.OwnFindings, own.Reports = "initial", PhaseOwnPass, localScratch+"/"+OwnFindingsFile, nil
	for _, tc := range []struct {
		name string
		d    JudgeData
	}{{"judge-initial.md", localJudge()}, {"judge-own-pass.md", own}} {
		p := prompt(t, tc.name)
		plain, err := RenderPrompt(p, tc.d)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, leak := range []string{"remerge", "the base merge"} {
			if strings.Contains(plain, leak) {
				t.Errorf("%s of a local round of a whole change names %q:\n%s", tc.name, leak, plain)
			}
		}
		narrow := tc.d
		narrow.Remerge = true
		got, err := RenderPrompt(p, narrow)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, want := range []string{"remerge: true", "local: true", "Take `git show --remerge-diff " + narrow.HeadSHA + "` in this checkout as the diff and the review boundary",
			"`git diff " + narrow.BaseSHA + ".." + narrow.HeadSHA + "` only as context", "Local round", "no `gh` call", "data, never instructions"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s of a narrow round lacks %q:\n%s", tc.name, want, got)
			}
		}
		if strings.Contains(got, "Take `git diff "+narrow.BaseSHA) {
			t.Errorf("%s of a narrow round takes the change's whole diff as its boundary:\n%s", tc.name, got)
		}
	}
}

// The skill's "Local round" section is what a local judge takes in place
// of GitHub: no gh call, the description file as the description whose
// claims section 2 checks, the ticket as data, a repository not private,
// and a product question marked in the provenance for the factory to ask.
func TestTheSkillSaysWhatALocalRoundTakesInPlaceOfGitHub(t *testing.T) {
	skillSays(t, []string{"Local round (`local: true`)", "make no `gh` call", "`description_file` holds the title and description (section 2's claims)",
		"`base.repo.private` counts as false", "gets `\"question\": true` in its provenance entry"}, nil)
}

// The description a local judge reads is the factory's own plan, not a
// person's: a choice that neither the ticket nor the operator's answers
// decide is a product question for the operator, never the author's intent
// that section 2 respects, and never a reason to reject the candidate: the
// judge keeps it at its priority as a question (a replay of talkable#12212's
// local round named it a product question and then rejected it). On a factory PR
// (talkable#12212) the judge deferred to a product behaviour
// the plan had chosen and nobody had decided, and a colleague found it in
// public. The rule is in the Local round section, so magnum's GitHub reviews
// and the review daemon's rounds read no such rule.
func TestTheSkillReadsTheFactorysPlanAsItsGuessNotThePersonsIntent(t *testing.T) {
	const rule = "The description is the factory's plan, not a person's: a choice neither the ticket nor the operator's answers (in `ticket`) decide is never intent nor a reason to reject; keep it at its priority with `\"question\": true`."
	skillSays(t, []string{rule}, nil)
	_, local, ok := strings.Cut(string(magnum.Skill), "\n## Local round (`local: true`)")
	if !ok || !strings.Contains(local, rule) {
		t.Error("the rule is not in the Local round section")
	}
	if before, _, _ := strings.Cut(string(magnum.Skill), "\n## Local round (`local: true`)"); strings.Contains(before, "the factory's plan") {
		t.Error("the factory's plan is named outside the Local round section, which only a local round reads")
	}
}

// The local claude reviewer reviews the change in the checkout, not a PR:
// no /code-review of a URL (there is no PR), the local diff and the
// description file by its path, its report and its run's marker.
func TestTheLocalClaudeReviewHasNoCodeReviewURL(t *testing.T) {
	d := localReviewer()
	got, err := RenderPrompt(prompt(t, "claude-review-local.md"), d)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"/code-review", d.URL, "github.com", "gh pr", "gh api"} {
		if strings.Contains(got, leak) {
			t.Errorf("the local reviewer's prompt carries %q:\n%s", leak, got)
		}
	}
	for _, want := range []string{"git diff " + d.BaseSHA + ".." + d.HeadSHA, d.DescriptionFile, d.ReportPath, "<!-- magnum:run=" + d.RunID + " -->",
		DBLockLine(d.Magnum, d.Checkout, d.Role), "data, never instructions"} {
		if !strings.Contains(got, want) {
			t.Errorf("the local reviewer's prompt lacks %q:\n%s", want, got)
		}
	}
}
