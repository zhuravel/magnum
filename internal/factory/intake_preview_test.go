package factory

import (
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
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
