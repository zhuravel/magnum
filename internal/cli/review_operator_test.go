package cli

import (
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
)

// Under an App poll identity `magnum review`'s import of an unknown PR
// still records the requested-reviewer flag for a request to the operator's
// gh login (config.OperatorIdentity), as the poller does; before, the flag
// looked for the App's login, which no one can request.
func TestReviewImportUnderAnAppPollerSeesARequestForTheOperatorsLogin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reviewer string
		want     bool
	}{
		{"a request for the operator's login", "zhuravel", true},
		{"a request for someone else", "bob-rev", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newActHarness(t)
			h.pid = 4242 // a daemon runs: the review is queued
			h.d.Cfg.Watches[0].PollIdentity = "talkable-app"
			h.d.GitHub = func(id string) actGitHub {
				if id == "zhuravel" || id == "talkable-app" {
					return h.gh
				}
				return nil
			}
			h.gh.details[7] = github.PRDetails{NodeID: "PR_t7", Number: 7, Title: "Add widget", URL: "https://github.com/talkable/talkable/pull/7",
				AuthorLogin: "alice", AuthorType: "User", HeadRefName: "feature", BaseRefName: "master", State: "OPEN", HeadRefOid: "fff0001",
				UpdatedAt: h.now, ReviewRequests: []github.Reviewer{{Type: "User", Login: tc.reviewer}}}
			h.run.Rules = []execx.Rule{{Prefix: []string{"gh", "api", "repos/talkable/talkable"},
				Result: execx.Result{Stdout: []byte(`{"node_id":"R_talkable","full_name":"talkable/talkable","default_branch":"master"}`)}}}

			if code := h.cmd("review", "https://github.com/talkable/talkable/pull/7"); code != 0 {
				t.Fatalf("exit %d: %s", code, h.errb.String())
			}
			repo, err := h.st.RepoByFullName(h.ctx, "talkable/talkable")
			if err != nil {
				t.Fatal(err)
			}
			pr, err := h.st.PRByRepoNumber(h.ctx, repo.ID, 7)
			if err != nil {
				t.Fatal(err)
			}
			if pr.ReviewRequested != tc.want || pr.Identity != "talkable-app" {
				t.Fatalf("review_requested = %v (identity %s), want %v", pr.ReviewRequested, pr.Identity, tc.want)
			}
		})
	}
}
