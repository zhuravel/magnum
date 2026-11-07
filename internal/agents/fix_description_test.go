package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// ownPRSentence is what the fix prompt of the operator's own PR says about
// its description (RoleData.OwnPR).
const ownPRSentence = " The PR is the operator's own: when a finding or another reviewer's item names its description, " +
	"edit the description as the skill's description fix (section 5) says, instead of leaving it under Needs you."

// ownPRFixPrompt renders a gh fixer's prompt, of the operator's own PR or not.
func ownPRFixPrompt(t *testing.T, own bool) string {
	t.Helper()
	e := newEnv(t)
	role := Role(config.RoleClaudeFix)
	run, err := e.m.NewRun(e.ctx, e.pr, role, store.RunFix, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.m.FixPrompt(e.pr, role, run, FixPromptInput{
		HeadRef: "feature/coupon-expiry", ReviewURL: "https://github.com/talkable/talkable/pull/11920#pullrequestreview-4100000001",
		ReviewFile: "/state/review.json", ReviewerLogin: "talkable[bot]",
		DBLockCommand: "/opt/magnum/bin/magnum db-lock --", FixReplyCommand: "/opt/magnum/bin/magnum fix-reply",
		OwnPR: own,
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// An own PR's fix loop ended with one Needs you entry that asked the
// operator to change a phrase of the PR body, and the operator edited it on
// GitHub by hand (2026-11-07). The fix prompt of the operator's own PR (the
// job's check, FixPromptInput.OwnPR) now lets the fixer edit the
// description when a finding or another reviewer's item names it, as the
// skill says, in its one paragraph, and no further: a line its own fix made
// untrue was not part of what the operator approved. A prompt without that
// word grants no edit. The goldens of the fix prompt are an own PR's, as
// every fix job's is.
func TestTheFixPromptOfAnOwnPRLetsTheFixerEditItsDescription(t *testing.T) {
	if got := ownPRFixPrompt(t, true); !strings.Contains(got, ownPRSentence) {
		t.Errorf("the fix prompt of an own PR lacks %q:\n%s", ownPRSentence, got)
	}
	if got := ownPRFixPrompt(t, false); strings.Contains(got, "operator's own") || strings.Contains(got, "description") {
		t.Errorf("the fix prompt of a PR that is not the operator's lets the fixer edit its description:\n%s", got)
	}
	for _, golden := range []string{"fix", "fix_no_findings", "fix_conflict", "fix_merge_only", "fix_app"} {
		b, err := os.ReadFile(filepath.Join("testdata", golden+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), ownPRSentence) {
			t.Errorf("the golden %s lacks %q:\n%s", golden, ownPRSentence, b)
		}
	}
}

// The review body's `Description: ✗` finding, which has no thread to reply
// in, went under Needs you as "fixed in". Now, only when the prompt says
// that the PR is the operator's own, the fixer edits the description, never
// the title, for a finding or another reviewer's item that names it (fix
// when right, not scored: it changes no code, so the bar of 3 would decline
// every one), and only then, as the operator approved: a line one of its
// fixes made untrue, which nothing names, stays the author's under Needs
// you. It makes one edit after the push, of a saved copy whose diff shows
// only those lines, never a hidden marker or the block it marks, nor the
// template's checklist, read again right before the write so a hand edit
// meanwhile is not reverted. Its reply says what changed; the description
// is not "anyone else's comment" to an App fixer, and a fixed
// `Description: ✗` finding is not under Needs you.
func TestTheFixSkillEditsAnOwnPRsDescriptionAndKeepsTheRestOfIt(t *testing.T) {
	skill := string(magnum.FixSkill)
	for _, want := range []string{
		"- Change only what a finding or an external item names, or the same defect in the PR's own lines: the code, " +
			"or the PR description when a finding or an external item names it (section 5).",
		"Reply at most once to each item, and never edit, hide or delete anyone else's comment. " +
			"The PR description is not such a comment: it is the author's, whom you act for, and section 5 says when you change it.",
		"- whether the PR is the operator's own, which lets you edit its description (section 5)",
		"A finding or an external item about the PR description (section 5) changes no code, so it would never clear the bar of 3 either.",
		"When it is right (the description says what the head does not do, or lacks what the item asks for and the code or the PR shows), " +
			"it is `fix`: do not score it. When the change needs what only the author knows (why the PR does something, a decision, a plan, a link), " +
			"it is step 6 of section 4.",
		"`fix` with a net score of 3 or more, a simplification or a description finding that is `fix`, and each finding that section 3 always makes `fix`",
		"- An item that asks to change the PR description is a description finding (section 3): fix it as section 5 says when it is right.",
		"### The PR description",
		"The prompt says whether the PR is the operator's own. Only then may you change its description (its body, never its title), " +
			"and only where a finding or an external item names it: the review body's `Description: ✗` line, a comment about a deploy section, " +
			"a reviewer who asks to change a phrase or a claim of it.",
		"A line of it that one of your fixes made untrue, which no finding or item names, stays the author's: put it under `## Needs you`.",
		"2. In the copy, change only the lines that the findings and items name.",
		"When the prompt does not say so, change nothing in the description: put each such finding or item under `## Needs you` " +
			"with `not fixed. the prompt does not say that the PR is the operator's own`.",
		"Make every change to the description in one edit, after the push. When the push failed, change nothing in it.",
		"`gh pr view {number} --json body -q .body > <report dir>/description-before.md`",
		"Never change, move or delete a hidden `<!-- … -->` line or the block it marks: magnum's " +
			"(the factory's Known list after `<!-- magnum factory: the Known list -->`) and any other tool's. " +
			"Never change a checklist line of the PR's template (`- [ ]` or `- [x]`): the author ticks those.",
		"`diff <report dir>/description-before.md <report dir>/description.md`",
		"`gh pr view {number} --json body -q .body | diff -q <report dir>/description-before.md -`",
		"If it differs, someone edited the description while you worked: start again at step 1, once.",
		"`gh pr edit {number} --body-file <report dir>/description.md`",
		"The review body's `Description: ✗` finding has no thread: it gets no reply, and your `description:` line under `## Pushed` answers it.",
		"A `Description: ✗` finding you fixed is not listed here: it has no thread to reply in, " +
			"and its `description:` line or its commit under `## Pushed` answers it.",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the fix skill lacks %q", want)
		}
	}
	for _, gone := range []string{
		"or the PR description when a finding names it (section 5)",
		"makes a line of it untrue",
		"or that your fixes made untrue",
		"goes into that fix's reply",
		"add `; the description now <what changed>` at its end",
		"A finding about the PR description (a deploy section, a claim the description makes): read the body",
	} {
		if strings.Contains(skill, gone) {
			t.Errorf("the fix skill still says %q", gone)
		}
	}
}
