package store

import (
	"context"
	"slices"
	"strconv"
	"testing"
)

// The registry's people are the humans its pull requests name, as
// scripts/check-logins.sh reads them: the authors that are users, the
// assignees and the requested reviewers (not teams), without a "[bot]"
// suffix, once each and sorted.
func TestPeopleLoginsListsTheHumansOfThePullRequests(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	for i, pr := range []GitHubPR{
		{AuthorLogin: new("alice"), AuthorType: new("User"), Assignees: []string{"bob-rev"}, RequestedReviewers: []string{"rev-ann", "team:core"}},
		{AuthorLogin: new("dependabot[bot]"), AuthorType: new("Bot"), Assignees: []string{"alice"}, RequestedReviewers: []string{"helper[bot]"}},
		{AuthorLogin: new("zhuravel")},
	} {
		n := i + 1
		pr.RepoID, pr.NodeID, pr.Number, pr.HeadSHA, pr.GHState = repo.ID, "PR_"+strconv.Itoa(n), n, "sha", GHOpen
		pr.URL, pr.InitialState, pr.Identity = "https://github.com/talkable/talkable/pull/"+strconv.Itoa(n), PRReviewed, "talkable-app"
		if _, err := st.UpsertPRFromGitHub(ctx, pr); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.PeopleLogins(ctx)
	if want := []string{"alice", "bob-rev", "helper", "rev-ann", "zhuravel"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("PeopleLogins = %q, %v; want %q", got, err, want)
	}
}
