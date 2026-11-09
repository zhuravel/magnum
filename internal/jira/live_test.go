package jira

import (
	"os"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// The changelog facts Status pages by, against a real Jira site: the issue's
// own changelog holds its newest entries, newest first, and the paged
// changelog numbers them oldest first, so the issue's newest entry is the
// paged changelog's last. Read only (three GETs), and only with
// MAGNUM_LIVE_JIRA=1 and the site, the account, its token file and an issue
// with more than 100 changelog entries in MAGNUM_LIVE_JIRA_URL, _USER,
// _TOKEN_FILE and _KEY; the gate never sets them.
func TestLiveTheChangelogsOrderIsAsStatusPagesIt(t *testing.T) {
	if os.Getenv("MAGNUM_LIVE_JIRA") != "1" {
		t.Skip("set MAGNUM_LIVE_JIRA=1 and MAGNUM_LIVE_JIRA_URL, _USER, _TOKEN_FILE and _KEY to read a real issue's changelog")
	}
	key := os.Getenv("MAGNUM_LIVE_JIRA_KEY")
	c := &Client{Runner: &execx.Real{}, URL: os.Getenv("MAGNUM_LIVE_JIRA_URL"), User: os.Getenv("MAGNUM_LIVE_JIRA_USER"),
		TokenFile: os.Getenv("MAGNUM_LIVE_JIRA_TOKEN_FILE")}
	ans, err := c.issue(t.Context(), key, true)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Changelog == nil || ans.Changelog.Total <= len(ans.Changelog.Histories) {
		t.Skipf("%s's changelog is whole in its answer: pick an issue with more than 100 entries", key)
	}
	hs := ans.Changelog.Histories
	if oldestFirst(hs) {
		t.Errorf("%s's own changelog came oldest first: Status then pages from the changelog's end, a case to look at", key)
	}
	last, err := c.changelog(t.Context(), key, ans.Changelog.Total-1, 1)
	if err != nil || len(last) != 1 {
		t.Fatalf("the paged changelog's last entry: %v (%d entries)", err, len(last))
	}
	first, err := c.changelog(t.Context(), key, 0, 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("the paged changelog's first entries: %v (%d entries)", err, len(first))
	}
	var newest, paged, a, b history
	if !decode(hs[0], &newest) || !decode(last[0], &paged) || !decode(first[0], &a) || !decode(first[1], &b) {
		t.Fatal("an entry of another shape")
	}
	if !jiraTime(newest.Created).Equal(jiraTime(paged.Created)) {
		t.Errorf("the issue's first entry (%s) is not the paged changelog's last (%s) of %d", newest.Created, paged.Created, ans.Changelog.Total)
	}
	if jiraTime(a.Created).After(jiraTime(b.Created)) {
		t.Errorf("the paged changelog is not oldest first: %s before %s", a.Created, b.Created)
	}
}
