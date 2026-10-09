package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A landing records the proposal's new state, the commit master moved to,
// when, and the version that landed in one transaction: a version the
// registry does not take leaves the proposal as it was, so no landed
// proposal lacks its commit or its version; a proposal that is not passed
// or held, or landed already, records nothing.
func TestLandLearnProposalRecordsTheLandingAndItsVersionTogether(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	noted := generalMiss(t, st, pr, 40)
	p := proposeLearn(t, st, LearnInput{Kind: LearnInputMiss, InputID: noted.ID, Outcome: MissNoted, Where: "section 4"})
	at := t0.Add(time.Hour)
	v := LearnVersion{LandedSHA: "abc123", SkillSHA: "s1", PromptsSHA: "p1", LandedAt: at}
	from := []string{LearnPassed, LearnHeld}
	if _, _, err := st.LandLearnProposal(ctx, p.ID, from, v); !errors.Is(err, ErrConflict) {
		t.Fatalf("a gating proposal landed: %v, want ErrConflict", err)
	}
	if _, err := st.DecideLearnProposal(ctx, p.ID, []string{LearnGating}, LearnPassed, "", time.Time{}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `CREATE TRIGGER fail_version BEFORE INSERT ON learn_versions
BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.LandLearnProposal(ctx, p.ID, from, v); err == nil {
		t.Fatal("a landing whose version failed succeeded")
	}
	if got, err := st.LearnProposalByID(ctx, p.ID); err != nil || got.State != LearnPassed || got.LandedSHA != "" || got.DecidedAt != nil {
		t.Fatalf("after the failed landing = %+v, %v, want passed as it was", got, err)
	}
	if s := missByID(t, st, noted.ID).State; s != MissNew {
		t.Fatalf("the noted miss = %s after the failed landing, want new", s)
	}
	if _, err := st.db.ExecContext(ctx, "DROP TRIGGER fail_version"); err != nil {
		t.Fatal(err)
	}

	got, ver, err := st.LandLearnProposal(ctx, p.ID, from, v)
	if err != nil || got.State != LearnLanded || got.LandedSHA != "abc123" || got.LandedAt == nil || !got.LandedAt.Equal(at) ||
		got.DecidedAt == nil || !got.DecidedAt.Equal(at) {
		t.Fatalf("landed = %+v, %v", got, err)
	}
	if ver.ID == 0 || ver.ProposalID != p.ID || ver.LandedSHA != "abc123" || ver.SkillSHA != "s1" || !ver.LandedAt.Equal(at) {
		t.Fatalf("version = %+v", ver)
	}
	if s := missByID(t, st, noted.ID).State; s != MissUsed {
		t.Fatalf("the noted miss = %s, want used", s)
	}
	if _, _, err := st.LandLearnProposal(ctx, p.ID, from, LearnVersion{LandedSHA: "def456", SkillSHA: "s2", PromptsSHA: "p2"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second landing = %v, want ErrConflict", err)
	}
	if vs, err := st.LearnVersions(ctx); err != nil || len(vs) != 1 {
		t.Fatalf("versions = %+v, %v", vs, err)
	}
	if _, _, err := st.LandLearnProposal(ctx, 999, from, v); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown proposal = %v, want ErrNotFound", err)
	}
	if _, _, err := st.LandLearnProposal(ctx, p.ID, from, LearnVersion{SkillSHA: "s", PromptsSHA: "p"}); err == nil {
		t.Fatal("a landing without its commit succeeded")
	}
	// Nothing else lands a proposal.
	q := proposeLearn(t, st)
	if _, err := st.DecideLearnProposal(ctx, q.ID, []string{LearnGating}, LearnLanded, "", time.Time{}, nil); err == nil {
		t.Fatal("DecideLearnProposal landed a proposal without its commit and version")
	}
}
