package agents

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zhuravel/magnum"
)

// judgePrompts are the prompts whose <magnum> block the skill reads.
var judgePrompts = []string{"judge-initial.md", "judge-rereview.md", "judge-continue.md", "judge-recovery.md"}

// magnumBlock is the prompt's <magnum> block.
func magnumBlock(t *testing.T, prompt string) string {
	t.Helper()
	i := strings.Index(prompt, "<magnum>\n")
	if i < 0 || !strings.HasSuffix(prompt, "\n</magnum>") {
		t.Fatalf("no <magnum> block at the end of\n%s", prompt)
	}
	return prompt[i:]
}

// The identity's review footer reaches the judge as a `footer:` field of the
// <magnum> block, and only when there is one (review_footer = "" turns it
// off).
func TestJudgePromptsRenderTheFooterOnlyWhenSet(t *testing.T) {
	const footer = "_Automated review by magnum. Reply on a thread with `fixed`, `not a bug: <why>` or `won't fix: <why>`._"
	for _, name := range judgePrompts {
		d := judgeFixture()
		plain, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(plain, "footer") {
			t.Errorf("%s names a footer without one:\n%s", name, plain)
		}
		d.Footer = footer
		got, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if block := magnumBlock(t, got); !strings.Contains(block, "\nfooter: "+footer+"\n") {
			t.Errorf("%s: the <magnum> block lacks the footer:\n%s", name, block)
		}
		if strings.Replace(got, "\nfooter: "+footer, "", 1) != plain {
			t.Errorf("%s: the footer changed more than its field:\n%s\n--- without:\n%s", name, got, plain)
		}
	}
}

// The readiness facts are <magnum> fields; what to do about a check that did
// not pass is the skill's (section 0), not a paragraph of every prompt.
func TestJudgePromptsLeaveReadinessToTheSkill(t *testing.T) {
	d := judgeFixture()
	d.Readiness = Readiness{Failed: 1, File: "/r/readiness.json", Checks: []ReadinessCheck{
		{Kind: ReadinessPrepare, Command: "bin/rails db:test:prepare", Status: ReadinessFailed, Detail: "exit 1", Duration: "4.2s"},
		{Kind: ReadinessRuby, Command: "ruby -v", OK: true, Status: ReadinessOK, Duration: "0.3s"},
	}}
	for _, name := range []string{"judge-initial.md", "judge-rereview.md", "judge-recovery.md"} {
		got, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := "\nreadiness: /r/readiness.json\n  - prepare `bin/rails db:test:prepare`: failed in 4.2s (exit 1)\n  - ruby `ruby -v`: ok in 0.3s\n"
		if block := magnumBlock(t, got); !strings.Contains(block, want) {
			t.Errorf("%s: the <magnum> block lacks the readiness list:\n%s", name, block)
		}
		if before := got[:strings.Index(got, "<magnum>")]; strings.Contains(before, "readiness") {
			t.Errorf("%s: a readiness paragraph before the block:\n%s", name, before)
		}
	}
}

// The prompts describe a reply's class the way the classifier makes it: from
// the reply's first clause, past an acknowledgement ("Good catch, fixed in
// …"), not from its first words.
func TestJudgePromptsSayTheReplyClassComesFromTheFirstClause(t *testing.T) {
	d := judgeFixture()
	d.ThreadsFile, d.ThreadSummary = "/r/review-threads.json", "2 threads; replies: 1 fixed, 1 other"
	for _, name := range []string{"judge-rereview.md", "judge-recovery.md"} {
		got, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(got, "first words") || !strings.Contains(got, "first clause") {
			t.Errorf("%s:\n%s", name, got)
		}
	}
	if s := string(magnum.Skill); strings.Contains(s, "first words") || !strings.Contains(s, "first clause") {
		t.Error("SKILL.md still says a reply's class comes from its first words")
	}
}

// claude-review reports only what the judge can post: of the candidates only
// claude-review raised, 3 were posted and 302 rejected (speculative 108,
// style_only 73, pre_existing 29). Its prompts no longer ask for uncertain
// findings and leave out style-only and pre-existing problems.
func TestClaudeReviewPromptsLeaveOutUncertainStyleAndPreExistingFindings(t *testing.T) {
	restart := roleFixture()
	restart.Mode, restart.RestartedFrom = ModeRestart, "f1cc4f9e0d1c2b3a4f5e6d7c8b9a0f1e2d3c4b5a"
	for name, d := range map[string]RoleData{"claude-review.md": roleFixture(), "claude-rereview.md": roleFixture(), "claude-restart.md": restart} {
		got, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(got, "uncertain") {
			t.Errorf("%s asks for uncertain findings:\n%s", name, got)
		}
		if !strings.Contains(got, "Leave out style-only problems and pre-existing ones (problems this PR neither introduces nor exposes).") {
			t.Errorf("%s does not exclude style-only and pre-existing problems:\n%s", name, got)
		}
	}
}

// skillMaxBytes bounds SKILL.md: 28,040 bytes before the review-format
// changes plus about 10%. Every rule added must replace or shorten text.
const skillMaxBytes = 30_844

func TestSkillStaysTight(t *testing.T) {
	if n := len(magnum.Skill); n > skillMaxBytes {
		t.Fatalf("SKILL.md is %d bytes, over %d: shorten it instead of adding to it", n, skillMaxBytes)
	}
}

var blockField = regexp.MustCompile(`(?m)^([a-z_]+):`)

// Every field a judge prompt's <magnum> block can carry is described in the
// skill, which is the only place that says what to do with it.
func TestSkillDescribesEveryMagnumField(t *testing.T) {
	d := judgeFixture()
	d.NotesPath, d.Footer, d.Blind, d.PostMerge = testNotesPath, "_Automated review._", true, true
	d.ThreadsFile, d.ThreadSummary, d.FormerLogins = "/r/review-threads.json", "1 thread", []string{"talkable-old[bot]"}
	d.MovedFrom, d.ForcePushed = "/Users/bohdan/Projects/talkable.review1", true
	d.Readiness = Readiness{Failed: 1, File: "/r/readiness.json", Checks: []ReadinessCheck{
		{Kind: ReadinessReady, Command: "bin/db-ready", Status: ReadinessFailed, Duration: "1s"}}}
	skill := string(magnum.Skill)
	seen := map[string]bool{}
	for _, name := range judgePrompts {
		got, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, m := range blockField.FindAllStringSubmatch(magnumBlock(t, got), -1) {
			seen[m[1]] = true
		}
	}
	for _, f := range []string{"footer", "notes", "notes_dir", "notes_harness", "notes_lock", "notes_unlock", "readiness", "reports"} {
		if !seen[f] {
			t.Errorf("no judge prompt renders `%s`", f)
		}
	}
	for f := range seen {
		if !strings.Contains(skill, "`"+f+"`") {
			t.Errorf("SKILL.md does not describe the <magnum> field `%s`", f)
		}
	}
}

// The notes procedure lives in the skill once, with the commands named by
// their <magnum> fields.
func TestSkillCarriesTheNotesProcedure(t *testing.T) {
	skill := string(magnum.Skill)
	for _, want := range []string{"`notes_lock`", "`notes_unlock`", "`notes busy`", ".tmp", "mv", "orphan", "# Notes for <owner>/<repo> (updated YYYY-MM-DD)"} {
		if !strings.Contains(skill, want) {
			t.Errorf("SKILL.md lacks %q", want)
		}
	}
}
