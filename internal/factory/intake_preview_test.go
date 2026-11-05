package factory

import (
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/tui"
)

// The new task's preview names the links of the text, in its order (a Jira
// link is its key, a GitHub PR link offers adoption in milestone 2, an
// Actions run link a CI task in milestone 3), and the overlap line of the
// key's work, so the screen's LINKS and OVERLAP rows show what the factory
// read; a prompt without a key reads no overlap.
func TestThePreviewNamesTheLinksAndTheOverlap(t *testing.T) {
	r := newIntakeRig(t)
	r.run.Rules = []execx.Rule{
		{Prefix: []string{"git", "log"}, Result: execx.Result{Stdout: []byte("abc1234\tPR-12345 first try\n")}},
		{Prefix: []string{"gh", "pr", "list"}, Result: execx.Result{Stdout: []byte(`[{"number":4501,"state":"OPEN","title":"PR-12345 an older try"}]`)}},
	}
	const pr, run = "https://github.com/example/talkable/pull/4512", "https://github.com/example/talkable/actions/runs/1234567"
	jiraLink := jiraTestSite + "/browse/PR-12345"
	f, err := r.e.preview(r.ctx, jiraLink+" the same fix as "+pr+"; CI is red since "+run+".")
	if err != nil {
		t.Fatal(err)
	}
	want := []tui.FormLink{
		{Kind: "jira", URL: jiraLink, Offer: "its key, PR-12345"},
		{Kind: "pr", URL: pr, Offer: "adoption comes in milestone 2"},
		{Kind: "run", URL: run, Offer: "a CI task comes in milestone 3"},
	}
	if !slices.Equal(f.Links, want) {
		t.Errorf("links = %+v\nwant %+v", f.Links, want)
	}
	if want := "1 commit names PR-12345 (newest abc1234) · PR #4501 (open) names PR-12345"; f.Overlap != want {
		t.Errorf("overlap = %q, want %q", f.Overlap, want)
	}
	if f.Jira == nil || f.Jira.Key != "PR-12345" {
		t.Errorf("the Jira link's ticket was not read: %+v", f.Jira)
	}

	// A PR link at the start is a later milestone's: the preview says so,
	// and names the link.
	f, _ = r.e.preview(r.ctx, pr)
	if f.Err == "" || len(f.Links) != 1 || f.Links[0].Kind != "pr" {
		t.Errorf("a PR link's preview: %+v", f)
	}
	// A prompt with no key reads no overlap and has no links.
	calls := len(r.run.CallsWithPrefix("git", "log"))
	f, _ = r.e.preview(r.ctx, "The weekly digest is off")
	if f.Overlap != "" || len(f.Links) != 0 || len(r.run.CallsWithPrefix("git", "log")) != calls {
		t.Errorf("a prompt's preview read an overlap or links: %+v", f)
	}
}

// The previews of one key, one after the other as you type its guidance,
// read its overlap once: the line is kept for a while, then read again.
func TestThePreviewsOfAKeyReadItsOverlapOnce(t *testing.T) {
	r := newIntakeRig(t)
	r.run.Rules = []execx.Rule{
		{Prefix: []string{"git", "log"}, Result: execx.Result{Stdout: []byte("abc1234\tPR-12345 first try\n")}},
		{Prefix: []string{"gh", "pr", "list"}, Result: execx.Result{Stdout: []byte("[]")}},
	}
	for _, text := range []string{"PR-12345 the", "PR-12345 the advocates", "PR-12345 the advocates only"} {
		if f, _ := r.e.preview(r.ctx, text); f.Overlap != "1 commit names PR-12345 (newest abc1234)" {
			t.Fatalf("%q: overlap %q", text, f.Overlap)
		}
	}
	if n := len(r.run.CallsWithPrefix("git", "log")); n != 1 {
		t.Errorf("three previews of one key ran git log %d times, want once", n)
	}
	r.clock.Add(previewOverlapTTL + time.Second)
	r.e.preview(r.ctx, "PR-12345 the advocates only")
	if n := len(r.run.CallsWithPrefix("git", "log")); n != 2 {
		t.Errorf("a preview after the line went stale ran git log %d times in all, want 2", n)
	}
}

// The previews of one text, one after the other as you type, read each of
// its keys from Jira once, a failure included (a key Jira does not know,
// Jira's silence), and its overlap once even when a read of it failed: the
// daemon answers the previews on its loop, so each answer is kept for
// previewOverlapTTL as the overlap line is, then read again.
func TestThePreviewsOfAKeyReadJiraOnceFailuresIncluded(t *testing.T) {
	r := newIntakeRig(t)
	previews := func(texts ...string) (last tui.NewTaskForm) {
		t.Helper()
		for _, text := range texts {
			f, err := r.e.preview(r.ctx, text)
			if err != nil {
				t.Fatal(err)
			}
			last = f
		}
		return last
	}
	asked := func(key string) int {
		r.site.mu.Lock()
		defer r.site.mu.Unlock()
		return len(slices.DeleteFunc(slices.Clone(r.site.asked), func(k string) bool { return k != key }))
	}
	if f := previews("PR-12345 the", "PR-12345 the advocates", "PR-12345 the advocates only"); f.Jira == nil || f.Jira.Key != "PR-12345" {
		t.Fatalf("a known key's preview: %+v", f)
	}
	if f := previews("PR-12399 the", "PR-12399 the reminder"); f.Err != "PR-12399 is not in Jira" || !f.WordsOnly {
		t.Fatalf("an unknown key's preview: %+v", f)
	}
	if f := previews("the refund of PR-12346", "the refund of PR-12346 is wrong"); f.Jira == nil || f.Jira.Key != "PR-12346" {
		t.Fatalf("a found key's preview: %+v", f)
	}
	for _, key := range []string{"PR-12345", "PR-12399", "PR-12346"} {
		if n := asked(key); n != 1 {
			t.Errorf("the previews read %s from Jira %d times, want once", key, n)
		}
	}

	r.site.setDown(28)
	calls := r.site.calls
	if f := previews("PR-12347 the", "PR-12347 the coupon", "PR-12347 the coupon code"); f.Err != "Jira does not answer: the task waits for it, then starts" {
		t.Fatalf("a silent Jira's preview: %+v", f)
	}
	r.site.mu.Lock()
	silent := r.site.calls - calls
	r.site.mu.Unlock()
	if silent == 0 || silent > 1 {
		t.Errorf("three previews while Jira is silent called it %d times, want once", silent)
	}

	r.run.Rules = []execx.Rule{{Prefix: []string{"git", "log"}, Result: execx.Result{Code: 128, Stderr: []byte("fatal: bad object")}}}
	before := len(r.run.CallsWithPrefix("git", "log"))
	previews("PR-12348 the", "PR-12348 the referral", "PR-12348 the referral link")
	if n := len(r.run.CallsWithPrefix("git", "log")) - before; n != 1 {
		t.Errorf("three previews whose overlap read failed ran git log %d times, want once", n)
	}

	r.site.setDown(0)
	r.clock.Add(previewOverlapTTL + time.Second)
	previews("PR-12345 the advocates only")
	if n := asked("PR-12345"); n != 2 {
		t.Errorf("a preview after the answer went stale read PR-12345 %d times in all, want 2", n)
	}
}

// A key at the start that Jira does not know: the preview says so and offers
// your words only; one Jira refused for another cause, or a key found in the
// middle of the text, does not.
func TestThePreviewOffersYourWordsOnlyForAKeyJiraDoesNotKnow(t *testing.T) {
	r := newIntakeRig(t)
	if f, _ := r.e.preview(r.ctx, "PR-12399 the reminder"); f.Err != "PR-12399 is not in Jira" || !f.WordsOnly {
		t.Errorf("an unknown key's preview: %+v", f)
	}
	if f, _ := r.e.preview(r.ctx, "PR-12345 the reminder"); f.WordsOnly {
		t.Errorf("a known key's preview offers words only: %+v", f)
	}
	if f, _ := r.e.preview(r.ctx, "the refund of PR-12399 is wrong"); f.WordsOnly {
		t.Errorf("a found key's preview offers words only: %+v", f)
	}
}

// A start from your words only is a prompt task of the whole text: its key
// is words, never read from Jira as its ticket or as a key found in it.
func TestAStartFromYourWordsOnlyNeverReadsTheKey(t *testing.T) {
	r := newIntakeRig(t)
	const text = "PR-12399 the reminder for advocates, as PR-12345 did"
	task, err := r.e.start(r.ctx, StartPayload{Text: text, WordsOnly: true, Surface: "screen"})
	if err != nil {
		t.Fatal(err)
	}
	r.e.waitBackground()
	if task.JiraKey != "" || task.Prompt != text {
		t.Errorf("task %+v, want a prompt task of the whole text", task)
	}
	for _, k := range r.site.asked {
		if k == "PR-12399" || k == "PR-12345" {
			t.Errorf("the start read %s from Jira", k)
		}
	}
	if got := r.taskFile(task.ID, "prompt.md"); got != text+"\n" {
		t.Errorf("prompt.md = %q", got)
	}
}

// The comment the factory drafts for an ask is the question, why, its
// example and its options, word for word (the recap shows the same text).
func TestTheDraftedCommentAsksTheQuestionWordForWord(t *testing.T) {
	q := registry.Question{Text: "Can a merchant turn it off?", Context: "The ticket does not say.",
		Example: "a merchant opens Settings.", Options: []registry.Option{{Label: "Yes", Means: "a switch"}, {Label: "No"}}}
	got := draftComment(registry.Task{JiraKey: "PR-12345"}, q, "rev-ann")
	want := "rev-ann, a question on PR-12345: Can a merchant turn it off?\n\nThe ticket does not say.\n\n" +
		"For example: a merchant opens Settings.\n\nThe options as we see them:\n- Yes: a switch\n- No\n"
	if got != want {
		t.Errorf("draft\n%q\nwant\n%q", got, want)
	}
}
