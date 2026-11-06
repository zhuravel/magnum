package factory

import (
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// The task card carries the pages the screen's b and t open: the task's PR
// on GitHub once the factory published it, and its Jira issue by its key
// and the repository's trackers ([board] trackers, as the board links a
// PR's issue). A task with no PR has no PR page, a task of your words no
// issue, and a key no template names no issue page; a task the screen
// cannot read still links its issue.
func TestTheTaskCardLinksItsPRAndItsJiraIssue(t *testing.T) {
	r := newScreenRig(t)
	r.cfg.Board.Trackers = []string{"https://example.atlassian.net/browse/PR-{num}"}
	pushed := r.published(r.task("PR-12305", "bug", "Coupon CSV header names", registry.TaskActive, "publish", "", ""), 4519)
	coding := r.task("PR-12371", "bug", "Coupon CSV export times out", registry.TaskActive, "coding", "", "")
	other := r.task("EX-12", "chore", "A key of no tracker", registry.TaskActive, "coding", "", "")
	words, err := r.reg.CreateTask(r.ctx, registry.Task{Prompt: "rename the sync job", Kind: "chore", Repo: "example/talkable",
		Title: "Rename the sync job", State: registry.TaskActive, Stage: "coding", Branch: "zhuravel-rename-the-sync-job"})
	if err != nil {
		t.Fatal(err)
	}
	broken := r.task("PR-12345", "feature", "Credit expiry reminder email", registry.TaskActive, "questions", "", "")
	broken = r.questions(broken, question("When?", "Seven days"))
	r.move(broken, registry.TaskNeedsYou, waitsYou, "1 question before coding")
	r.file(broken.ID, asksFile, "{not json")

	d := r.data(screenNow)
	for _, c := range []struct {
		task                registry.Task
		pr, issue, issueURL string
	}{
		{pushed, "https://github.com/example/talkable/pull/4519", "PR-12305", "https://example.atlassian.net/browse/PR-12305"},
		{coding, "", "PR-12371", "https://example.atlassian.net/browse/PR-12371"},
		{other, "", "EX-12", ""},
		{words, "", "", ""},
		{broken, "", "PR-12345", "https://example.atlassian.net/browse/PR-12345"},
	} {
		if card := d.Cards[c.task.ID].Task; card.PRURL != c.pr || card.Issue != c.issue || card.IssueURL != c.issueURL {
			t.Errorf("%s: PR %q, issue %q at %q; want PR %q, issue %q at %q", TaskKey(c.task), card.PRURL, card.Issue, card.IssueURL,
				c.pr, c.issue, c.issueURL)
		}
	}
}

// A repository's own trackers ([[repo]]'s) win over [board]'s for its
// tasks' issue pages, as they do for its PRs on the board
// (Config.TrackersFor).
func TestTheTaskCardLinksItsIssueByItsRepositorysTrackers(t *testing.T) {
	r := newScreenRig(t)
	r.cfg.Board.Trackers = []string{"https://example.atlassian.net/browse/PR-{num}"}
	r.cfg.Repos = append(r.cfg.Repos, config.Repo{Repo: "example/talkable", Trackers: []string{"https://linear.app/example/issue/PR-{num}"}})
	task := r.task("PR-12371", "bug", "Coupon CSV export times out", registry.TaskActive, "coding", "", "")

	if card := r.data(screenNow).Cards[task.ID].Task; card.IssueURL != "https://linear.app/example/issue/PR-12371" {
		t.Errorf("the issue page %q, want the [[repo]] tracker's", card.IssueURL)
	}
}
