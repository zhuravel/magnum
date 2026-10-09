package store

import (
	"context"
	"slices"
	"testing"
)

// A poll reads its repository's open rows, which a PR missing from the radar
// is confirmed against, and the rows the radar lists whatever their GitHub
// state (a reopened PR keeps its row); the closed, merged and released rows
// it would only decode and drop stay unread.
func TestListPRsOfAPollKeepsTheOpenRowsAndTheRadars(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	other, err := st.UpsertRepo(ctx, Repo{NodeID: "R_2", Owner: "talkable", Name: "example", WatchOwner: "talkable",
		DefaultBranch: "master", Mode: RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	for n, gh := range map[int]string{1: GHOpen, 2: GHClosed, 3: GHMerged, 4: GHUnknown, 5: GHOpen, 6: GHClosed} {
		p := mustPR(t, st, repo.ID, n, PRReviewed)
		if gh != GHOpen {
			if _, err := st.DB().ExecContext(ctx, "UPDATE prs SET gh_state = ?, state = ? WHERE id = ?", gh, PRReleased, p.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := st.UpsertPRFromGitHub(ctx, GitHubPR{RepoID: other.ID, NodeID: "PR_other", Number: 9, URL: "u9", HeadSHA: "h",
		GHState: GHOpen, InitialState: PRReviewed, Identity: "talkable-app"}); err != nil {
		t.Fatal(err)
	}
	numbers := func(f PRFilter) []int {
		t.Helper()
		prs, err := st.ListPRs(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []int
		for _, p := range prs {
			out = append(out, p.Number)
		}
		return out
	}
	// #2 is back on the radar (reopened), #6 and the unknown #4 are not.
	if got := numbers(PRFilter{RepoID: repo.ID, GHOpen: true, OrNodeIDs: []string{"PR_1", "PR_2", "PR_other"}}); !slices.Equal(got, []int{1, 2, 5}) {
		t.Errorf("a poll's rows = %v, want [1 2 5]", got)
	}
	if got := numbers(PRFilter{RepoID: repo.ID, GHOpen: true}); !slices.Equal(got, []int{1, 5}) {
		t.Errorf("open rows = %v, want [1 5]", got)
	}
	if got := numbers(PRFilter{RepoID: repo.ID, OrNodeIDs: []string{"PR_2"}}); !slices.Equal(got, []int{1, 2, 3, 4, 5, 6}) {
		t.Errorf("node ids without GHOpen = %v, want every row", got)
	}
}
