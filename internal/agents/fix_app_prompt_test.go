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

// fixAppPrompt renders the fix prompt of a fixer App (the push line and
// the login set) or of a gh fixer (both empty).
func fixAppPrompt(t *testing.T, push, login string) string {
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
		PushCommand: push, FixerLogin: login,
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A fixer App's pane refuses a plain `git push`, and its commits, push and
// replies carry the App's login, not the author's: its prompt names the
// login it works as and the `magnum push` line, never `git push`.
func TestTheFixPromptNamesMagnumPushForAnAppFixer(t *testing.T) {
	line := PushLine("/opt/magnum/bin/magnum", "/slots/talkable.review3", "master")
	got := fixAppPrompt(t, line, "fixer-app[bot]")
	for _, want := range []string{
		"You work for the author as `fixer-app[bot]`: your commits, your push and your replies carry that login.",
		"Push to the PR's branch with `" + line + "`.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the fix prompt lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"git push", "you act as the author"} {
		if strings.Contains(got, not) {
			t.Errorf("the fix prompt of an App fixer says %q:\n%s", not, got)
		}
	}
}

// A gh fixer's prompt keeps today's words: it acts as the author and pushes
// with git push, byte for byte as the goldens hold it.
func TestTheFixPromptKeepsGitPushForAGhFixer(t *testing.T) {
	got := fixAppPrompt(t, "", "")
	const want = "This checkout is the PR author's, and you act as the author. The PR head `"
	if !strings.Contains(got, want) || !strings.Contains(got, "Push to the PR's branch with `git push origin HEAD:feature/coupon-expiry`.") ||
		strings.Contains(got, "magnum push") || strings.Contains(got, "You work for the author as") {
		t.Errorf("the gh fixer's prompt =\n%s", got)
	}
	for _, golden := range []string{"fix", "fix_no_findings", "fix_conflict", "fix_merge_only"} {
		b, err := os.ReadFile(filepath.Join("testdata", golden+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "This checkout is the PR author's, and you act as the author.") ||
			!strings.Contains(string(b), "Push to the PR's branch with `git push origin HEAD:feature/coupon-expiry`.") {
			t.Errorf("the golden %s lost the gh fixer's words", golden)
		}
	}
}

// The skill pushes with the prompt's push line, whichever it is, never
// with force: a line `magnum push` refuses (exit 2) goes under Needs you, a
// rejected one (exit 3, fetch first) runs the fix-sync line, as a push
// GitHub rejected because the branch moved does, and a base merge it
// hands to magnum (exit 4: it carries the base's workflow files) ends the
// run, for magnum to push. The fixes go out in one push after the base
// merge's, which went out alone first.
func TestTheFixSkillPushesWithThePromptsPushLine(t *testing.T) {
	skill := string(magnum.FixSkill)
	for _, want := range []string{
		"- the push line, which every push goes through: `git push origin HEAD:<branch>`, or `magnum push` for a fixer that acts as a GitHub App",
		"When every fix is committed, push once with the push line of the prompt. The base merge's commit went out alone before (section 2).",
		"\n<push line>\n",
		"When the push line is `magnum push` and it refuses the push (exit status 2, `refused: <reason>`), push nothing more: " +
			"write `push failed: refused: <reason>` under `## Pushed`, put each finding you committed under `## Needs you`, and stop.",
		"When it rejects the push (exit status 3, `rejected: <reason>: fetch first`), GitHub's branch moved: run the fix-sync line as below.",
		"When it hands the push to magnum (exit status 4, `daemon: <reason>`), your base merge carries the base's own workflow files, " +
			"which only magnum may push: push nothing more and judge no finding. List the merge under `## Pushed` (section 2), write " +
			"`0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)` under `## Replied`, and stop: magnum pushes the merge " +
			"when your run ends, and its re-review brings the findings back.",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the fix skill lacks %q", want)
		}
	}
	if strings.Contains(skill, "```\ngit push origin HEAD:<branch>\n```") {
		t.Error("the fix skill still pushes with git push whatever the prompt says")
	}
}

// The skill posts as the prompt's login: the author's for a gh fixer, the
// App's for a fixer App, and the external file says which (`fixer`).
func TestTheFixSkillPostsAsThePromptsLogin(t *testing.T) {
	skill := string(magnum.FixSkill)
	for _, want := range []string{
		"You are the assistant of the PR's author. You work in the author's checkout, and you act for the author. " +
			"The prompt says which login your commits, your push and your replies carry: the author's, or the GitHub App's that magnum gave you.",
		"`fixer` is the login you post as",
		"You post as the prompt's login, and magnum reads the marker to tell your replies from the author's own",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the fix skill lacks %q", want)
		}
	}
	if strings.Contains(skill, "your commits, your push and your replies carry the author's name") {
		t.Error("the fix skill still says the fixer always carries the author's name")
	}
}
