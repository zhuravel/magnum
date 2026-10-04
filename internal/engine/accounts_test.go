package engine

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// An App named like its owner ("zhuravel[bot]", the user "zhuravel") is
// another account: the poller keeps a bot reviewer's "[bot]" (GraphQL drops
// it), so the board and every later comparison tell the two apart.
func TestFillDetailsKeepsABotReviewersSuffix(t *testing.T) {
	var in store.GitHubPR
	fillDetails(&in, github.PRDetails{
		LatestReviews: []github.LatestReview{
			{State: "COMMENTED", AuthorLogin: "zhuravel", AuthorType: "Bot", CommitOid: "c-bot"},
			{State: "APPROVED", AuthorLogin: "zhuravel", AuthorType: "User", CommitOid: "c-user"},
		},
		ReviewRequests: []github.Reviewer{{Type: "Bot", Login: "example-reviewer"}, {Type: "User", Login: "alice"}, {Type: "Team", Login: "core"}},
	}, nil, time.Now())
	if len(in.LatestReviews) != 2 || in.LatestReviews[0].Login != "zhuravel[bot]" || in.LatestReviews[1].Login != "zhuravel" {
		t.Fatalf("latest reviews %+v", in.LatestReviews)
	}
	want := []string{"example-reviewer[bot]", "alice", store.TeamReviewerPrefix + "core"}
	if len(in.RequestedReviewers) != len(want) {
		t.Fatalf("requested %v", in.RequestedReviewers)
	}
	for i := range want {
		if in.RequestedReviewers[i] != want[i] {
			t.Fatalf("requested %v, want %v", in.RequestedReviewers, want)
		}
	}
}

// The approval magnum follows is an App identity's: a review by the user
// named like the App is never dismissed with the App's token, and of two
// installations of one App (one login) the PR's own identity posts.
func TestApprovalIdentityTellsTheUserFromTheApp(t *testing.T) {
	h := newHarness(t)
	h.cfg.Identities = append(h.cfg.Identities,
		config.Identity{Name: "me", Kind: "gh", Login: "zhuravel"},
		config.Identity{Name: "app-org-a", Kind: "app", Login: "zhuravel[bot]"},
		config.Identity{Name: "app-org-b", Kind: "app", Login: "zhuravel[bot]"})
	for name, tc := range map[string]struct {
		identity, login string
		want            string // "" = none
	}{
		"the user's review":                    {identity: "app-org-b", login: "zhuravel"},
		"the PR's installation":                {identity: "app-org-b", login: "zhuravel[bot]", want: "app-org-b"},
		"another identity's PR: the first App": {identity: "me", login: "zhuravel[bot]", want: "app-org-a"},
		"no login: the PR's App":               {identity: "app-org-b", want: "app-org-b"},
		"no login, a user's PR":                {identity: "me"},
	} {
		t.Run(name, func(t *testing.T) {
			pr := store.PR{Identity: tc.identity}
			if tc.login != "" {
				pr.LastReviewLogin = store.Ptr(tc.login)
			}
			got := ""
			if id := h.e.approvalIdentity(pr); id != nil {
				got = id.Name
			}
			if got != tc.want {
				t.Fatalf("approval identity = %q, want %q", got, tc.want)
			}
		})
	}
}

// What changed since the identity's own review is measured from the App's
// review, not from the review of the user named like it.
func TestSinceBaseIsTheIdentitysOwnReviewNotTheNamesakes(t *testing.T) {
	h := newHarness(t)
	h.cfg.Identities = append(h.cfg.Identities, config.Identity{Name: "app", Kind: "app", Login: "zhuravel[bot]"})
	pr := store.PR{Identity: "app", BaseSHA: store.Ptr("base"), LatestReviews: []store.LatestReview{
		{Login: "zhuravel", State: "APPROVED", CommitSHA: "c-user"},
		{Login: "zhuravel[bot]", State: "COMMENTED", CommitSHA: "c-bot"},
	}}
	if src, base := h.e.sinceBase(pr); src != store.SinceFromReview || base != "c-bot" {
		t.Fatalf("since base = %s %s, want the App's review c-bot", src, base)
	}
	pr.LatestReviews = pr.LatestReviews[:1]
	if src, base := h.e.sinceBase(pr); src != store.SinceFromBase || base != "base" {
		t.Fatalf("only the user reviewed: since base = %s %s, want the base", src, base)
	}
}
