package factory

import (
	"slices"
	"testing"
)

// A key in the middle of the text is found, and the whole text stays the
// prompt: nothing is cut out of it. ParseStart reads no Jira, so a word of
// the shape of a key ("UTF-8") is found too; Jira decides which count.
func TestParseStartFindsAKeyInTheMiddleOfTheText(t *testing.T) {
	trackers := jiraTestSite + "/browse/PR-{num} " + jiraTestSite
	text := "fix the expiry date in PR-12345, only the refund path"
	got := ParseStart(text, trackers)
	if got.Key != "" || got.Prompt != text || got.Guidance != "" || !slices.Equal(got.Found, []string{"PR-12345"}) {
		t.Fatalf("ParseStart(%q) = %s, want the whole text as the prompt and PR-12345 found", text, showStart(got))
	}

	for _, tc := range []struct {
		text  string
		found []string
	}{
		{"the reminder (PR-12345) is wrong", []string{"PR-12345"}},
		{"see PR-12345: it loses points", []string{"PR-12345"}},
		{"is it PR-12345?", []string{"PR-12345"}},
		{"PR-12345's reminder is wrong, so the PR-12345 email too", []string{"PR-12345"}},         // distinct
		{"the reminder of PR-12346 and the refund of PR-12345", []string{"PR-12346", "PR-12345"}}, // in the text's order
		{"line one\nthe reminder is PR-12345\nline three", []string{"PR-12345"}},
		{"parse UTF-8 and ISO-8601 dates", []string{"UTF-8", "ISO-8601"}},
		{"[PR-12345](" + jiraTestSite + "/browse/PR-12345) is wrong", []string{"PR-12345"}},
		// Not keys: lower case, a longer word, a longer number, a dash before.
		{"pr-12345 is lowercase", nil},
		{"fixPR-12345 is glued", nil},
		{"PR-12345x is no key", nil},
		{"the ABC-DEF-12 build", nil},
		{"covid-19 and Covid-19", nil},
		{"no key here at all", nil},
	} {
		if got := ParseStart(tc.text, trackers); got.Key != "" || got.Prompt != tc.text || !slices.Equal(got.Found, tc.found) {
			t.Errorf("ParseStart(%q) = %s, want the whole text as the prompt, found %q", tc.text, showStart(got), tc.found)
		}
	}
}

// A Jira link in the middle of the text is its key, with a comment before
// it, after it or both; a link on another site is no Jira link, and neither
// is the key in it. A GitHub link in the middle stays words: adoption is
// for a link at the start.
func TestParseStartFindsAJiraLinkWithACommentAroundIt(t *testing.T) {
	trackers := jiraTestSite + "/browse/PR-{num} " + jiraTestSite
	for _, tc := range []struct {
		text  string
		found []string
	}{
		{"this one is wrong " + jiraTestSite + "/browse/PR-12346 only the advocates", []string{"PR-12346"}},
		{"see " + jiraTestSite + "/browse/PR-12346?focusedCommentId=10#comment, it loses points", []string{"PR-12346"}},
		{"see (" + jiraTestSite + "/browse/PR-12346).", []string{"PR-12346"}},
		{"look\n" + jiraTestSite + "/browse/PR-12346\nand fix it", []string{"PR-12346"}},
		{"compare " + jiraTestSite + "/browse/PR-12346 with PR-12345 and " + jiraTestSite + "/browse/PR-12346/", []string{"PR-12346", "PR-12345"}},
		{"see https://wiki.example.com/browse/PR-12347 for the page", nil},
		{"see https://github.com/talkable/talkable/pull/4527 for the rename", nil},
		{"see https://github.com/talkable/talkable/pull/4527 for PR-12345", []string{"PR-12345"}},
		{"see https://github.com/talkable/talkable/actions/runs/1234567890 for the failure", nil},
	} {
		got := ParseStart(tc.text, trackers)
		if got.Key != "" || got.PR != nil || got.Run != nil || got.Prompt != tc.text || !slices.Equal(got.Found, tc.found) {
			t.Errorf("ParseStart(%q) = %s, want the whole text as the prompt, found %q", tc.text, showStart(got), tc.found)
		}
	}
	// Without a site to tell it by, any Jira issue link counts, as at the start.
	if got := ParseStart("see https://other.atlassian.net/browse/PR-12347 please", ""); !slices.Equal(got.Found, []string{"PR-12347"}) {
		t.Errorf("a Jira link without trackers = %s", showStart(got))
	}
}

// A key or a link as the first word keeps its meaning: it is the task's key,
// the words after it the guidance, and nothing is found in them.
func TestParseStartKeepsTheFirstWordRule(t *testing.T) {
	trackers := jiraTestSite + "/browse/PR-{num} " + jiraTestSite
	for _, tc := range []struct {
		text string
		want StartInput
	}{
		{"PR-12345 and PR-12346 too", StartInput{Key: "PR-12345", Guidance: "and PR-12346 too"}},
		{"PR-12345: see " + jiraTestSite + "/browse/PR-12346", StartInput{Key: "PR-12345", Guidance: "see " + jiraTestSite + "/browse/PR-12346"}},
		{jiraTestSite + "/browse/PR-12345 and PR-12346", StartInput{Key: "PR-12345", Guidance: "and PR-12346"}},
		{"https://github.com/talkable/talkable/pull/4527 take it over, PR-12345", StartInput{Guidance: "take it over, PR-12345",
			PR: &Link{URL: "https://github.com/talkable/talkable/pull/4527", Repo: "talkable/talkable", Number: 4527}}},
	} {
		if got := ParseStart(tc.text, trackers); !sameStart(got, tc.want) {
			t.Errorf("ParseStart(%q) = %s, want %s", tc.text, showStart(got), showStart(tc.want))
		}
	}
}
