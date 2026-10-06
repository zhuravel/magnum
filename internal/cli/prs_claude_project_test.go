package cli

import (
	"context"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// The board's card says when magnum's Claude sessions of the PR's head
// loaded the user's settings only because the PR changes .claude/ or
// .mcp.json, after the Codex sentence when both CLIs ran without the PR's
// project config; the record of an older head says nothing, and prs --json
// carries the same note.
func TestPRsSourceCarriesTheClaudeProjectRecordOfTheHead(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "RC", Owner: "talkable", Name: "talkable", Mode: store.RepoModePool})
	if err != nil {
		t.Fatal(err)
	}
	for i, kinds := range [][]string{{"claude"}, {"codex", "claude"}, {}} {
		res, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "PK" + string(rune('a'+i)), Number: 90 + i, URL: "u",
			HeadSHA: "abc", InitialState: store.PRReviewed, Identity: "talkable-app"})
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range kinds {
			if err := st.SetKV(ctx, store.KVPRProject(res.PR.ID, kind), `{"head":"abc","files":1,"compared":true}`); err != nil {
				t.Fatal(err)
			}
		}
		if i == 2 {
			if err := st.SetKV(ctx, store.KVPRProject(res.PR.ID, "claude"), `{"head":"0ld","files":1,"compared":true}`); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := prsSource(st, nil, store.BoardFilter{}, nil, f.Ctx.Layout)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for _, r := range rows {
		got[r.Number] = prsJSONOf(r).ProjectNote
	}
	const claude = "Claude ran without the PR's .claude/ and .mcp.json changes (its sessions loaded your user settings only)"
	const codex = "Codex ran without the PR's .codex/ changes (the checkout was untrusted in its sessions)"
	if got[90] != claude || got[91] != codex+" "+claude || got[92] != "" {
		t.Fatalf("project notes: %v", got)
	}
}
