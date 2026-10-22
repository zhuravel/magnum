package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// A critique is recorded when it starts and once when it ends, with its
// decision, reason, checked and losses lists, outcome and model; the list
// reads them back by repository and proposal, newest first, and the daily
// cap counts the critiques started since a time. A daemon start ends the
// critiques still running as stopped.
func TestCritiquesAreRecordedWithTheCriticsRecord(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	base, _, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromImport, Content: NotesContent{Notes: []byte("# Notes\n")}})
	if err != nil {
		t.Fatal(err)
	}
	proposed := NotesContent{Notes: []byte("# Notes\n- more\n")}
	p, err := st.CreateNotesProposal(ctx, NotesProposalInput{RepoID: repo.ID, Kind: ProposalCuration, BaseVersionID: base.ID, Proposed: &proposed, State: ProposalPending})
	if err != nil {
		t.Fatal(err)
	}
	start := clk.Now()
	first, err := st.StartCritique(ctx, CritiqueStart{ProposalID: p.ID, RepoID: repo.ID, RunID: "20261022-120000", RunDir: "/notes/.critic/r", Model: "fable",
		Target: "fp1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EndCritique(ctx, first, CritiqueEnd{Outcome: CritiqueUndecided, Detail: "decision.json was not written"}); err != nil {
		t.Fatal(err)
	}
	if err := st.EndCritique(ctx, first, CritiqueEnd{Outcome: CritiqueStopped}); !errors.Is(err, ErrConflict) {
		t.Errorf("a second end = %v", err)
	}
	clk.Add(time.Hour)
	second, err := st.StartCritique(ctx, CritiqueStart{ProposalID: p.ID, RepoID: repo.ID, RunID: "20261022-130000", Model: "fable", Target: "fp1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EndCritique(ctx, second, CritiqueEnd{Outcome: CritiqueApplied, Decision: "apply", Reason: "keeps every decision", Model: "opus",
		Checked: []string{"the diff", "the archive"}, Losses: []string{"a probe's history"}}); err != nil {
		t.Fatal(err)
	}
	cs, err := st.NotesCritiques(ctx, NotesCritiqueFilter{RepoID: repo.ID})
	if err != nil || len(cs) != 2 {
		t.Fatalf("critiques = %+v, %v", cs, err)
	}
	c := cs[0]
	if c.ID != second || c.Outcome != CritiqueApplied || c.Decision != "apply" || c.Reason != "keeps every decision" || c.Model != "opus" ||
		c.Target != "fp1" || c.EndedAt == nil || !slices.Equal(c.Checked, []string{"the diff", "the archive"}) ||
		!slices.Equal(c.Losses, []string{"a probe's history"}) {
		t.Errorf("the newest critique = %+v", c)
	}
	if u := cs[1]; u.Outcome != CritiqueUndecided || u.Detail != "decision.json was not written" || u.Model != "fable" || u.RunDir != "/notes/.critic/r" ||
		len(u.Checked) != 0 || u.Checked == nil {
		t.Errorf("the first critique = %+v", u)
	}
	if n, err := st.CountCritiquesSince(ctx, start.Add(time.Minute)); err != nil || n != 1 {
		t.Errorf("critiques since = %d, %v; want 1", n, err)
	}
	if err := st.EndCritique(ctx, second, CritiqueEnd{Outcome: "bogus"}); err == nil {
		t.Error("an unknown outcome was accepted")
	}

	open, err := st.StartCritique(ctx, CritiqueStart{ProposalID: p.ID, RepoID: repo.ID, RunID: "20261022-140000"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := st.StopOpenCritiques(ctx, "the daemon stopped", time.Time{}); err != nil || n != 1 {
		t.Fatalf("stopped = %d, %v", n, err)
	}
	cs, _ = st.NotesCritiques(ctx, NotesCritiqueFilter{ProposalID: p.ID, Limit: 1})
	if len(cs) != 1 || cs[0].ID != open || cs[0].Outcome != CritiqueStopped || cs[0].Detail != "the daemon stopped" {
		t.Errorf("the open critique after a start = %+v", cs)
	}
}
