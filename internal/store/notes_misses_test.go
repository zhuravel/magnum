package store

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// The settle delay: with Until set, a PR closed after it is not due yet but
// counts as settling; one closed at or before it is due. PRIDs ignores
// Until, as `magnum retro <ref>` names its PRs.
func TestRetroDueWaitsForTheSettleDelay(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	closedAgo := func(number int, ago time.Duration) PR {
		pr := mustPR(t, st, repo.ID, number, PRReviewed)
		mustClosePR(t, st, pr.ID, GHMerged, nil, new(t0.Add(-ago)))
		mustPostedRun(t, st, pr.ID, 1, int64(number))
		return pr
	}
	fresh := closedAgo(21, 23*time.Hour)
	closedAgo(22, 25*time.Hour)
	closedAgo(23, 24*time.Hour) // exactly the settle delay: due
	done := closedAgo(24, time.Hour)
	if err := st.RecordRetroPR(ctx, RetroPR{PRID: done.ID, Status: RetroNothing}); err != nil {
		t.Fatal(err)
	}
	q := RetroQuery{Since: t0.Add(-7 * 24 * time.Hour), Until: t0.Add(-24 * time.Hour)}
	prs, err := st.RetroDue(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, p := range prs {
		got = append(got, p.Number)
	}
	if want := []int{23, 22}; !reflect.DeepEqual(got, want) {
		t.Fatalf("due = %v, want %v", got, want)
	}
	if n, err := st.RetroSettling(ctx, q); err != nil || n != 1 {
		t.Fatalf("settling = %d, %v; want 1 (the PR a retro looked at does not count)", n, err)
	}
	if n, _ := st.RetroSettling(ctx, RetroQuery{Since: q.Since}); n != 0 {
		t.Errorf("settling without a delay = %d", n)
	}
	named, err := st.RetroDue(ctx, RetroQuery{Until: q.Until, PRIDs: []int64{fresh.ID}, Again: true})
	if err != nil || len(named) != 1 || named[0].ID != fresh.ID {
		t.Fatalf("a named PR closed 23 hours ago = %+v, %v", named, err)
	}
}

// seedRepoMisses stores a repository miss on pr per title.
func seedRepoMisses(t *testing.T, st *Store, pr PR, titles ...string) []Miss {
	t.Helper()
	var out []Miss
	for i, title := range titles {
		m, err := st.UpsertMiss(context.Background(), Miss{PRID: pr.ID, SourceURL: "https://github.com/talkable/talkable/pull/7#discussion_r" + strconv.Itoa(100+i),
			SourceKind: MissSourceThread, Reviewer: "rev-ann", ReviewedSHA: "abc", Class: MissMiss, Severity: "P2", Path: "app/models/coupon.rb",
			Line: 40 + i, Title: title, Lesson: "Check the tenant scope of every finder.", Scope: MissScopeRepo})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func missByID(t *testing.T, st *Store, id int64) Miss {
	t.Helper()
	ms, err := st.Misses(context.Background(), MissFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("no miss %d", id)
	return Miss{}
}

// A proposal keeps what it did with every miss it was given; the misses
// stay new while it waits, become used when it is applied, return to new
// when it is rejected or expires, and a miss in two rejected proposals is
// dismissed. The links stay: the listing names the latest proposal of a
// miss and its state, and the rejections' reasons are there for the next
// curation.
func TestProposalMissesFollowTheProposal(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	ms := seedRepoMisses(t, st, pr, "Coupon lookup ignores the site", "Cache key misses the tenant", "Expired codes redeem", "Totals round twice")
	if _, err := st.UpsertMiss(ctx, Miss{PRID: pr.ID, SourceURL: "https://github.com/talkable/talkable/pull/7#discussion_r9", SourceKind: MissSourceThread,
		Reviewer: "rev-ann", ReviewedSHA: "abc", Class: MissMiss, Title: "General", Lesson: "x", Scope: MissScopeGeneral}); err != nil {
		t.Fatal(err)
	}
	pending, err := st.Misses(ctx, MissFilter{RepoID: repo.ID, Classes: []string{MissMiss}, Scopes: []string{MissScopeRepo}, States: []string{MissNew}})
	if err != nil || len(pending) != 4 {
		t.Fatalf("repo misses = %d, %v; want 4 (the general one is not the notes')", len(pending), err)
	}
	if n, _ := st.CountMisses(ctx, MissFilter{RepoID: repo.ID + 1}); n != 0 {
		t.Errorf("another repository's misses = %d", n)
	}
	base, _, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromImport, Content: notesContent("# Notes\n")})
	if err != nil {
		t.Fatal(err)
	}
	proposed := notesContent("# Notes\n\n## Pitfalls\n- scope finders\n")
	propose := func(links ...ProposalMiss) NotesProposal {
		t.Helper()
		p, err := st.CreateNotesProposal(ctx, NotesProposalInput{RepoID: repo.ID, Kind: ProposalCuration, Trigger: "misses",
			BaseVersionID: base.ID, Proposed: &proposed, Changes: []byte(`{}`), State: ProposalPending, Misses: links})
		if err != nil {
			t.Fatal(err)
		}
		clk.Add(time.Minute)
		return p
	}
	decide := func(p NotesProposal, to, reason string) {
		t.Helper()
		var applied *NotesVersionInput
		if to == ProposalApplied {
			applied = &NotesVersionInput{Source: NotesFromCuration, Content: proposed}
		}
		if _, err := st.DecideNotesProposal(ctx, p.ID, []string{ProposalPending}, to, reason, time.Time{}, applied); err != nil {
			t.Fatal(err)
		}
	}
	state := func(m Miss) string { return missByID(t, st, m.ID).State }

	// Applied: the noted and the skipped misses become used, linked to it.
	p1 := propose(ProposalMiss{MissID: ms[0].ID, Outcome: MissNoted, Section: "Pitfalls"},
		ProposalMiss{MissID: ms[1].ID, Outcome: MissSkipped, Reason: "the cache is gone"})
	links, err := st.ProposalMisses(ctx, p1.ID)
	if err != nil || len(links) != 2 || links[0].MissID != ms[0].ID || links[0].Outcome != MissNoted || links[0].Section != "Pitfalls" ||
		links[1].Reason != "the cache is gone" || links[1].Miss == nil || links[1].Miss.Title != "Cache key misses the tenant" {
		t.Fatalf("links = %+v, %v", links, err)
	}
	if m := missByID(t, st, ms[0].ID); m.State != MissNew || m.ProposalID == nil || *m.ProposalID != p1.ID || m.ProposalState != ProposalPending {
		t.Fatalf("a miss of a waiting proposal = %+v", m)
	}
	decide(p1, ProposalApplied, "")
	if m := missByID(t, st, ms[0].ID); m.State != MissUsed || m.ProposalState != ProposalApplied || *m.ProposalID != p1.ID {
		t.Fatalf("noted miss after the apply = %+v", m)
	}
	if state(ms[1]) != MissUsed {
		t.Fatalf("skipped miss after the apply = %s", state(ms[1]))
	}

	// Rejected once: back to new, with the reason; twice: dismissed.
	p2 := propose(ProposalMiss{MissID: ms[2].ID, Outcome: MissNoted, Section: "Pitfalls"})
	decide(p2, ProposalRejected, "too specific")
	if state(ms[2]) != MissNew {
		t.Fatalf("after one rejection: %s", state(ms[2]))
	}
	p3 := propose(ProposalMiss{MissID: ms[2].ID, Outcome: MissSkipped, Reason: "covered"}, ProposalMiss{MissID: ms[3].ID, Outcome: MissNoted, Section: "Pitfalls"})
	decide(p3, ProposalRejected, "")
	if state(ms[2]) != MissDismissed {
		t.Fatalf("after two rejections: %s", state(ms[2]))
	}
	if state(ms[3]) != MissNew {
		t.Fatalf("a miss in one rejected proposal: %s", state(ms[3]))
	}
	rej, err := st.MissRejections(ctx, []int64{ms[2].ID, ms[3].ID, ms[0].ID})
	if err != nil || !reflect.DeepEqual(rej[ms[2].ID], []string{"too specific"}) || len(rej[ms[3].ID]) != 0 || len(rej[ms[0].ID]) != 0 {
		t.Fatalf("rejections = %v, %v", rej, err)
	}

	// Expired: back to new, and an expiry never dismisses.
	p4 := propose(ProposalMiss{MissID: ms[3].ID, Outcome: MissNoted, Section: "Pitfalls"})
	decide(p4, ProposalExpired, "not reviewed within 7 days")
	if m := missByID(t, st, ms[3].ID); m.State != MissNew || *m.ProposalID != p4.ID || m.ProposalState != ProposalExpired {
		t.Fatalf("after the expiry = %+v", m)
	}

	if _, err := st.CreateNotesProposal(ctx, NotesProposalInput{RepoID: repo.ID, Kind: ProposalCuration, State: ProposalPending,
		Misses: []ProposalMiss{{MissID: ms[3].ID, Outcome: "maybe"}}}); err == nil {
		t.Error("a link with an unknown outcome was stored")
	}
}
