package factory

import (
	"regexp"
	"strings"
	"testing"
)

// The walker's words in the Visual QA section are inert on GitHub: each is
// a code span of its own (a backtick of its own a quote), clipped, with no
// control, bidi or line-separator character, so no HTML, link, mention,
// issue reference or emoji of a page renders, and no line is reordered.
func TestTheVisualQASectionKeepsTheWalkersWordsInert(t *testing.T) {
	vals := []string{
		"/x?q=<img src=x onerror=alert(1)>",
		"signed in as @alice see [here](https://evil.example) **bold** <!-- x --> https://evil.example #12 :smile:",
		"a`b``c```d", "`", "</details><script>x</script>", "\\`escaped", "&lt;b&gt;entity&#96;",
		"line1\nline2\x1b[31mred\u0085nel\u202ertl\u2066iso\u2069\u2028sep\u2029par",
		strings.Repeat("ab`<", 40) + "@alice",
	}
	res := QAResult{Outcome: QAOutcomeClean, Head: strings.Repeat("a", 40), Pages: len(vals), Walked: true}
	for i, v := range vals {
		id := "P" + string(rune('1'+i))
		res.Walk = append(res.Walk, QAPage{ID: id, Path: qaClip(v), State: qaClip(v)})
		res.Blocked = append(res.Blocked, QABlocked{What: qaClip(v), Why: qaClip(v)})
		res.Fixed = append(res.Fixed, QAFixed{Run: 1, Class: QAEscapedMarkup, Severity: "P2", Path: qaClip(v), State: qaClip(v)})
		res.Known = append(res.Known, QADefect{Class: QABrokenLayout, Severity: "P3", Page: id, Title: v})
		res.Accepted = append(res.Accepted, QADefect{Class: QAFlow, Severity: "P2", Page: id, Title: v})
	}
	s := qaSection(res)
	span := regexp.MustCompile("`[^`]*`")
	for l := range strings.SplitSeq(s, "\n") {
		if !strings.HasPrefix(l, "- ") {
			continue
		}
		if strings.Count(l, "`")%2 != 0 {
			t.Errorf("odd backticks: %q", l)
		}
		out := span.ReplaceAllString(l, "")
		for _, bad := range []string{"<", "@", "](", "http", "&", "*", "\\", "#", ":smile"} {
			if strings.Contains(out, bad) {
				t.Errorf("%q outside the code spans of %q", bad, l)
			}
		}
		for _, m := range span.FindAllString(l, -1) {
			if n := len([]rune(m)); n > qaQuoteRunes+4 {
				t.Errorf("a span of %d runes: %q", n, m)
			}
		}
	}
	for _, r := range "\x1b\u0085\u202a\u202e\u2066\u2069\u2028\u2029" {
		if strings.ContainsRune(s, r) {
			t.Errorf("the section holds %U", r)
		}
	}
}

// Each list of the Visual QA section names at most qaSectionMost items,
// then how many more the run's report has, so a flood of the walker's
// defects or blocked steps keeps the PR body far below GitHub's 65,536
// characters.
func TestTheVisualQASectionListsAtMostTwentyOfEachKind(t *testing.T) {
	res := QAResult{Outcome: QAOutcomeClean, Head: strings.Repeat("a", 40), Pages: 30, Walked: true}
	long := strings.Repeat("x", 300)
	for range 30 {
		res.Walk = append(res.Walk, QAPage{ID: "P1", Path: qaClip(long), State: qaClip(long)})
	}
	for range 500 {
		res.Known = append(res.Known, QADefect{Class: QAConsoleError, Severity: "P3", Page: "P1"})
		res.Blocked = append(res.Blocked, QABlocked{What: qaClip(long), Why: qaClip(long)})
		res.Fixed = append(res.Fixed, QAFixed{Run: 1, Class: QAEscapedMarkup, Severity: "P2", Path: qaClip(long), State: qaClip(long)})
		res.Accepted = append(res.Accepted, QADefect{Class: QAFlow, Severity: "P2", Page: "P1"})
	}
	s := qaSection(res)
	if n := strings.Count(s, "\n- "); n > 5*(qaSectionMost+1) {
		t.Errorf("the section lists %d items", n)
	}
	for _, want := range []string{"- and 10 more in the run's report", "- and 480 more in the run's report"} {
		if !strings.Contains(s, want) {
			t.Errorf("the section lacks %q", want)
		}
	}
	if len(s) > 32_000 {
		t.Errorf("the section is %d bytes", len(s))
	}
}
