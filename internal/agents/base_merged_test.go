package agents

import (
	"strings"
	"testing"
)

// A re-review after a push that merged the base branch reads the PR's own
// diff before and after the push, not previous..head (which carries the
// base branch's commits): the reviewer's, the restart's and the judge's
// prompts say so, the judge's <magnum> block marks it, and a rewritten
// history still asks for the whole PR.
func TestBaseMergedRereviewPromptsCompareThePRsOwnDiff(t *testing.T) {
	role := roleFixture()
	role.BaseMerged = true
	restart := role
	restart.Mode, restart.RestartedFrom = ModeRestart, "f1cc4f9e0d1c2b3a4f5e6d7c8b9a0f1e2d3c4b5a"
	judge := judgeFixture()
	judge.BaseMerged = true
	own := "`git diff " + role.BaseSHA + "..." + role.PreviousHeadSHA + "` with `git diff " + role.BaseSHA + "..." + role.HeadSHA + "`"
	judgeOwn := "`git diff " + judge.BaseSHA + "..." + judge.PreviousHeadSHA + "` with `git diff " + judge.BaseSHA + "..." + judge.HeadSHA + "`"
	for _, tc := range []struct {
		file string
		data any
		want []string
		not  string
	}{
		{"claude-rereview.md", role, []string{"The push merged the base branch", own}, "Review only what they changed: `git diff"},
		{"claude-restart.md", restart, []string{"merged the base branch", own}, "review only `git diff"},
		{"judge-rereview.md", judge, []string{"The push merged the base branch", judgeOwn, "\nbase_merged: true\n"}, "Read the new commits with"},
	} {
		got, err := RenderPrompt(prompt(t, tc.file), tc.data)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s lacks %q:\n%s", tc.file, w, got)
			}
		}
		if strings.Contains(got, tc.not) {
			t.Errorf("%s still reads previous..head:\n%s", tc.file, got)
		}
	}

	forced := role
	forced.ForcePushed = true
	got, err := RenderPrompt(prompt(t, "claude-rereview.md"), forced)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "review the whole PR again") || strings.Contains(got, "merged the base branch") {
		t.Errorf("a rewritten history that also merged the base:\n%s", got)
	}

	noBase := role
	noBase.BaseSHA, noBase.BaseRef = "", "origin/master"
	if got, err := RenderPrompt(prompt(t, "claude-rereview.md"), noBase); err != nil || !strings.Contains(got, "`git diff origin/master..."+role.PreviousHeadSHA+"`") {
		t.Errorf("without a merge base (%v):\n%s", err, got)
	}
}

// Triage of such a push is told the diff is the PR's own.
func TestTriagePromptSaysTheDiffIsThePRsOwn(t *testing.T) {
	data := triageFixtureFor("rereview")
	data.OwnDiff = true
	got, err := RenderPrompt(prompt(t, "triage.md"), data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "the pull request's own diff, against its base, of each file") || strings.Contains(got, "the diff of the commits pushed") {
		t.Errorf("triage prompt:\n%s", got)
	}
}

// The two prompts that still read previous..head after a merge of the base
// branch: simplify's re-review hands its subagents the PR's own diff and
// scopes the proposals to what changed between the two own diffs, and the
// recovery prompt (a judge whose session is gone, a fresh-session delta
// check too) says the push merged the base, marks it in the <magnum> block,
// and calls a delta check's delta the change between the two own diffs.
func TestBaseMergedSimplifyAndRecoveryPromptsReadThePRsOwnDiff(t *testing.T) {
	role := roleFixture()
	role.Mode, role.BaseMerged = ModeRereview, true
	role.ReportPath = "/state/reviews/talkable/talkable/11920/d4e5f6a/claude-simplify.md"
	own := "`git diff " + role.BaseSHA + "..." + role.PreviousHeadSHA + "` with `git diff " + role.BaseSHA + "..." + role.HeadSHA + "`"
	got, err := RenderPrompt(prompt(t, "claude-simplify.md"), role)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Run `git diff " + role.BaseSHA + "..HEAD` once", "The push merged the base branch", own} {
		if !strings.Contains(got, w) {
			t.Errorf("simplify after a base merge lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "Run `git diff "+role.PreviousHeadSHA+"..HEAD`") {
		t.Errorf("simplify after a base merge still hands its subagents previous..head:\n%s", got)
	}
	plain := role
	plain.BaseMerged = false
	if got, err := RenderPrompt(prompt(t, "claude-simplify.md"), plain); err != nil || !strings.Contains(got, "Run `git diff "+role.PreviousHeadSHA+"..HEAD` once") {
		t.Errorf("a plain re-review's simplify no longer reads the delta (%v):\n%s", err, got)
	}

	judge := judgeFixture()
	judge.BaseMerged = true
	judgeOwn := "`git diff " + judge.BaseSHA + "..." + judge.PreviousHeadSHA + "` with `git diff " + judge.BaseSHA + "..." + judge.HeadSHA + "`"
	got, err = RenderPrompt(prompt(t, "judge-recovery.md"), judge)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"The push merged the base branch", judgeOwn, "\nbase_merged: true\n"} {
		if !strings.Contains(got, w) {
			t.Errorf("recovery after a base merge lacks %q:\n%s", w, got)
		}
	}
	check := judge
	check.DeltaCheck, check.DeltaLines = true, 4
	if got, err = RenderPrompt(prompt(t, "judge-recovery.md"), check); err != nil || !strings.Contains(got, "the change between the PR's own diff before and after them") {
		t.Errorf("a delta check after a base merge (%v):\n%s", err, got)
	}
	if got, err = RenderPrompt(prompt(t, "judge-recovery.md"), judgeFixture()); err != nil || strings.Contains(got, "base_merged") || strings.Contains(got, "merged the base") {
		t.Errorf("a recovery without a base merge names one (%v):\n%s", err, got)
	}
}
