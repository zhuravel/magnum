package agents

import (
	"strings"
	"testing"
)

// An author may refuse a simplification for cost alone ("the gain is 2
// words in a spec helper, so it waits for the next edit"): on talkable#12069
// the retro gave eight such declines `over_ranked` with a general lesson
// ("check that the saving is more than a couple of lines"), which taught the
// skill editor to propose fewer simplifications (2026-10-29). A declined or
// argued simplification whose replies name none of the risks is `right`, so
// it keeps no lesson; one whose reply names a risk is judged as any finding.
// A simplification is told by what store.Decline.IsSimplification checks: no
// priority, and the heading's title in one of its forms. A ranked finding
// whose title starts with the word ("[P2] Simplification breaks the
// empty-input fallback") keeps the ordinary verdicts, so an author who
// disproves it still teaches a `wrong` lesson.
func TestTheRetroCallsADeclinedOrArguedSimplificationThatNamesNoRiskRight(t *testing.T) {
	threads := retroFixtureWith("a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0")
	threads.Threads = 2
	got, err := RenderPrompt(prompt(t, "retro.md"), threads)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"A thread without a `priority` whose `title` is `Simplification`, or starts with `Simplification` followed by `:`, ` (` or `*`, " +
			"is a simplification; a thread with a `priority` is a ranked finding, whatever its title says. " +
			"A declined or argued simplification is `right` when no reply names a concrete risk of it:\n",
		"- It changes behavior for a named input or caller.\n",
		"- It drops a guard, a check or an assertion.\n",
		"- It edits lines that the pull request did not add or change.\n",
		"- It edits authorization, sandboxing, money or usage recording, or concurrency code.\n",
		"- It is out of date with the head, and its idea no longer fits.\n",
		"- It conflicts with a repository rule or another open pull request.\n",
		"- The specs fail after it.\n",
		"A reply that gives only cost names no risk: few lines saved; a push, a CI run or a review round; an approved pull request; " +
			"\"style only\"; code that only tests or runs locally; consistency without the lines it means. " +
			"A refusal for cost alone is the author's choice for one thread, no lesson. " +
			"When a reply names a risk, judge the simplification like any other finding.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("retro.md lacks %q:\n%s", want, got)
		}
	}
	// The rule is about the automated review's own threads: a retro of
	// other reviewers' comments alone carries none of it.
	plain, err := RenderPrompt(prompt(t, "retro.md"), retroFixtureWith("a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "Simplification") {
		t.Errorf("a retro without threads names simplifications:\n%s", plain)
	}
}

// The judge wrote two such refusals into talkable's notes as standing
// decisions (a one-off URL builder kept "for consistency", fixture
// allocations kept because "those examples run only locally"), so later
// rounds would not propose them again. A decline for cost alone is the
// author's choice for one thread, not the repository's.
func TestSkillKeepsADeclineOfASimplificationForCostAloneOutOfTheNotes(t *testing.T) {
	skillSays(t, []string{
		"A simplification declined for cost alone, with no risk named, is no standing decision: do not note it.",
	}, nil)
}
