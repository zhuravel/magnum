package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A landed proposal records the commit master moved to and when, once; a
// proposal that did not land records nothing.
func TestMarkLearnLandedRecordsTheCommitOnce(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	p := proposeLearn(t, st)
	if err := st.MarkLearnLanded(ctx, p.ID, "abc123", time.Time{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a gating proposal marked landed: %v, want ErrConflict", err)
	}
	if _, err := st.DecideLearnProposal(ctx, p.ID, []string{LearnGating}, LearnLanded, "", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	at := t0.Add(time.Hour)
	if err := st.MarkLearnLanded(ctx, p.ID, "abc123", at); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkLearnLanded(ctx, p.ID, "def456", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := st.LearnProposalByID(ctx, p.ID)
	if err != nil || got.LandedSHA != "abc123" || got.LandedAt == nil || !got.LandedAt.Equal(at) {
		t.Fatalf("landed = %+v, %v, want abc123 at %s", got, err, at)
	}
	if err := st.MarkLearnLanded(ctx, 999, "abc123", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown proposal = %v, want ErrNotFound", err)
	}
}
