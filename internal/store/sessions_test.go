package store

import (
	"context"
	"slices"
	"strconv"
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

// PRsWithHerdrIDs names, once each and in id order, every PR a session of
// which, in any state, records a herdr workspace, tab or pane: a move of
// magnum's panes to another herdr session parks those PRs and closes their
// panes in the old one, before the ids are forgotten.
func TestPRsWithHerdrIDsNamesEveryPRWithAPaneOrWorkspace(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	live := mustPR(t, st, repo.ID, 1, PRReviewed)
	parked := mustPR(t, st, repo.ID, 2, PRReviewed)
	closed := mustPR(t, st, repo.ID, 3, PRReleased)
	none := mustPR(t, st, repo.ID, 4, PRReviewed)
	mustPR(t, st, repo.ID, 5, PRQueued) // no session at all
	for _, s := range []Session{
		{PRID: parked.ID, Role: RoleJudge, HerdrWorkspaceID: new("w2"), State: SessionParked},
		{PRID: live.ID, Role: RoleJudge, HerdrWorkspaceID: new("w1"), HerdrTabID: new("w1:t1"), HerdrPaneID: new("w1:p1"),
			State: SessionLive},
		{PRID: live.ID, Role: RoleClaude, HerdrPaneID: new("w1:p2"), State: SessionLost},
		{PRID: closed.ID, Role: RoleJudge, HerdrTabID: new("w3:t1"), State: SessionClosed},
		{PRID: none.ID, Role: RoleJudge, SessionID: new("conv-4"), State: SessionParked},
	} {
		if _, err := st.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.PRsWithHerdrIDs(ctx)
	if want := []int64{live.ID, parked.ID, closed.ID}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("PRsWithHerdrIDs = %v, %v; want %v", got, err, want)
	}
}

// ForgetHerdrIDs is the last step of a move of magnum's panes to another
// herdr session: no row keeps an id of the old server (each server numbers
// its own from w1), and a starting or live row is lost, so the role resumes
// its conversation by session id in the new session; parked, closed and lost
// rows keep their state, and every row its conversation. A second call
// finds nothing to change.
func TestForgetHerdrIDsDropsTheIdsAndLosesTheLiveRows(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	a := mustPR(t, st, repo.ID, 1, PRReviewed)
	b := mustPR(t, st, repo.ID, 2, PRReviewed)
	ids := func(n string) (*string, *string, *string) {
		return new("w" + n), new("w" + n + ":t1"), new("w" + n + ":p1")
	}
	var rows []Session
	wants := map[int64]string{}
	for _, s := range []Session{
		{PRID: a.ID, Role: RoleJudge, SessionID: new("conv-1"), State: SessionLive},
		{PRID: a.ID, Role: RoleClaude, SessionID: new("conv-2"), State: SessionStarting},
		{PRID: a.ID, Role: RoleCodexReview, SessionID: new("conv-3"), State: SessionParked},
		{PRID: b.ID, Role: RoleJudge, SessionID: new("conv-4"), State: SessionClosed},
		{PRID: b.ID, Role: RoleClaude, SessionID: new("conv-5"), State: SessionLost},
		{PRID: b.ID, Role: RoleSimplify, SessionID: new("conv-6"), State: SessionLive}, // no herdr ids recorded
	} {
		if s.Role != RoleSimplify {
			s.HerdrWorkspaceID, s.HerdrTabID, s.HerdrPaneID = ids(strconv.Itoa(len(rows) + 1))
		}
		x, err := st.CreateSession(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, x)
		wants[x.ID] = map[string]string{SessionStarting: SessionLost, SessionLive: SessionLost}[s.State]
		if wants[x.ID] == "" {
			wants[x.ID] = s.State
		}
	}

	n, err := st.ForgetHerdrIDs(ctx)
	if err != nil || n != int64(len(rows)) {
		t.Fatalf("ForgetHerdrIDs = %d, %v; want %d rows changed", n, err, len(rows))
	}
	for _, before := range rows {
		got, err := st.SessionByID(ctx, before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.HerdrWorkspaceID != nil || got.HerdrTabID != nil || got.HerdrPaneID != nil {
			t.Errorf("%s session (%s) keeps herdr ids %v %v %v", got.Role, before.State,
				Deref(got.HerdrWorkspaceID), Deref(got.HerdrTabID), Deref(got.HerdrPaneID))
		}
		if got.State != wants[before.ID] {
			t.Errorf("%s session %s -> %s, want %s", got.Role, before.State, got.State, wants[before.ID])
		}
		if Deref(got.SessionID) != Deref(before.SessionID) {
			t.Errorf("%s session lost its conversation %q: %q", got.Role, Deref(before.SessionID), Deref(got.SessionID))
		}
	}
	if live, err := st.LiveSessions(ctx); err != nil || len(live) != 0 {
		t.Fatalf("live sessions after ForgetHerdrIDs: %v, %v", live, err)
	}
	if prs, err := st.PRsWithHerdrIDs(ctx); err != nil || len(prs) != 0 {
		t.Fatalf("PRs with herdr ids after ForgetHerdrIDs: %v, %v", prs, err)
	}
	if n, err := st.ForgetHerdrIDs(ctx); err != nil || n != 0 {
		t.Fatalf("a second ForgetHerdrIDs = %d, %v; want nothing changed", n, err)
	}
}
