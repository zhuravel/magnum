package cli

import (
	"context"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// The board's card says when magnum's Codex sessions of the PR's head ran
// with its checkout untrusted because the PR changes .codex/: the agents'
// record names the head; a record of an older head says nothing.
func TestPRsSourceCarriesTheCodexProjectRecordOfTheHead(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "RC", Owner: "talkable", Name: "talkable", Mode: store.RepoModePool})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i, head := range []string{"abc", "def"} {
		res, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "PC" + head, Number: 80 + i, URL: "u", HeadSHA: head,
			InitialState: store.PRReviewed, Identity: "talkable-app"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetKV(ctx, store.KVPRCodexProject(res.PR.ID), `{"head":"abc","files":1,"compared":true}`); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.PR.ID)
	}
	rows, err := prsSource(st, nil, store.BoardFilter{}, nil, f.Ctx.Layout)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for _, r := range rows {
		got[r.Number] = r.ProjectNote
	}
	if got[80] != "Codex ran without the PR's .codex/ changes (the checkout was untrusted in its sessions)" || got[81] != "" {
		t.Fatalf("project notes on the card: %v; want #80's (its head's record) only", got)
	}
}
