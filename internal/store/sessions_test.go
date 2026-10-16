package store

import (
	"context"
	"slices"
	"testing"
)

// PRsWithPanes names, once each and in id order, the PRs a session of
// which (in any state) records one of the panes herdr has: the engine's
// reconcile closes their retired panes, one sessions query per such PR.
func TestPRsWithPanesNamesThePRsWhoseSessionsRecordThem(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	a := mustPR(t, st, repo.ID, 1, PRReviewed)
	b := mustPR(t, st, repo.ID, 2, PRReviewed)
	c := mustPR(t, st, repo.ID, 3, PRReleased)
	for _, s := range []Session{
		{PRID: b.ID, Role: RoleJudge, HerdrPaneID: new("w2:p1"), State: SessionParked},
		{PRID: b.ID, Role: RoleClaude, HerdrPaneID: new("w2:p2"), State: SessionLost},
		{PRID: a.ID, Role: RoleJudge, HerdrPaneID: new("w1:p1"), State: SessionLive},
		{PRID: c.ID, Role: RoleJudge, HerdrPaneID: new("w3:p1"), State: SessionClosed},
		{PRID: c.ID, Role: RoleClaude, State: SessionParked},
	} {
		if _, err := st.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.PRsWithPanes(ctx, []string{"w2:p2", "w1:p1", "w2:p1", "w9:p1"})
	if err != nil || !slices.Equal(got, []int64{a.ID, b.ID}) {
		t.Fatalf("PRsWithPanes = %v, %v; want %v", got, err, []int64{a.ID, b.ID})
	}
	if got, err := st.PRsWithPanes(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("no panes: %v, %v", got, err)
	}
}
