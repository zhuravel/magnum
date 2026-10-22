package store

import (
	"context"
	"testing"
	"time"
)

// Only the operator's rejections dismiss a miss: two rejections by the
// critic leave it new for the next curation (a bad rewrite must not cost
// the repository its lesson), while the operator's second rejection still
// dismisses it.
func TestOnlyTheOperatorsRejectionsDismissAMiss(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	ms := seedRepoMisses(t, st, pr, "Coupon lookup ignores the site")
	base, _, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromImport, Content: notesContent("# Notes\n")})
	if err != nil {
		t.Fatal(err)
	}
	proposed := notesContent("# Notes\n\n## Pitfalls\n- scope finders\n")
	reject := func(by string) {
		t.Helper()
		p, err := st.CreateNotesProposal(ctx, NotesProposalInput{RepoID: repo.ID, Kind: ProposalCuration, Trigger: "misses", BaseVersionID: base.ID,
			Proposed: &proposed, Changes: []byte(`{}`), State: ProposalPending, Misses: []ProposalMiss{{MissID: ms[0].ID, Outcome: MissNoted, Section: "Pitfalls"}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DecideNotesProposal(ctx, p.ID, []string{ProposalPending}, ProposalRejected, by, "too much went", time.Time{}, nil); err != nil {
			t.Fatal(err)
		}
		clk.Add(time.Minute)
	}
	reject(DecidedByCritic)
	reject(DecidedByCritic)
	if m := missByID(t, st, ms[0].ID); m.State != MissNew {
		t.Fatalf("after two rejections by the critic the miss is %s, want new", m.State)
	}
	reject(DecidedByOperator)
	if m := missByID(t, st, ms[0].ID); m.State != MissNew {
		t.Fatalf("after one rejection by the operator the miss is %s, want new", m.State)
	}
	reject(DecidedByOperator)
	if m := missByID(t, st, ms[0].ID); m.State != MissDismissed {
		t.Fatalf("after two rejections by the operator the miss is %s, want dismissed", m.State)
	}
}
