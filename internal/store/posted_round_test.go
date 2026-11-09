package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// roundRun creates a run of pr in round with role at t0+d on head; a
// reviewID above zero makes it a run that posted that review.
func roundRun(t *testing.T, st *Store, prID int64, round int, role, head string, reviewID int64, d time.Duration) Run {
	t.Helper()
	r := Run{PRID: prID, Round: round, Role: role, Kind: RunInitial, State: RunVerified, TargetSHA: head, Identity: "talkable-app",
		ReviewerLogin: "talkable[bot]", PromptText: "p", CreatedAt: t0.Add(d)}
	if reviewID > 0 {
		r.Outcome, r.ReviewID = new("posted"), new(reviewID)
	}
	run, err := st.CreateRun(context.Background(), r)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run
}

// The first round of a PR that posted a review and began at or after a
// time is read in one query with the findings it posted, every priority,
// oldest first, and the counts of its result when it has one: a round that began before the time counts not, though it
// posted after it, and neither does a later round, a rejected finding, a
// run that posted nothing, another PR's round or a round of no review.
func TestFirstPostedReviewIsTheFirstRoundThatBeganSinceAndPosted(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	other := mustPR(t, st, repo.ID, 8, PRReviewed)

	early := roundRun(t, st, pr.ID, 1, RoleClaude, "h1", 0, -time.Hour) // round 1 began before the publish
	earlyJudge := roundRun(t, st, pr.ID, 1, RoleJudge, "h1", 100, 10*time.Minute)
	record := func(run Run, fs []Finding) {
		t.Helper()
		if err := st.RecordFindings(ctx, run.ID, run.PRID, run.Round, fs); err != nil {
			t.Fatal(err)
		}
	}
	record(earlyJudge, []Finding{{FindingID: "F1", Title: "An early finding", Severity: "P1", Path: "app/a.rb", Line: 3, Verdict: FindingPosted}})
	_ = early
	roundRun(t, st, other.ID, 1, RoleJudge, "o1", 300, 20*time.Minute)     // another PR's
	start := roundRun(t, st, pr.ID, 2, RoleClaude, "h1", 0, time.Hour)     // round 2 began on h1
	roundRun(t, st, pr.ID, 2, RoleJudge, "h2", 0, time.Hour+5*time.Minute) // a judge run that failed
	judge := roundRun(t, st, pr.ID, 2, RoleJudge, "h2", 101, time.Hour+10*time.Minute)
	record(judge, []Finding{
		{FindingID: "F1", Title: "The refund misses the cap", Severity: "P2", Path: "app/models/credit.rb", Line: 12, Verdict: FindingPosted},
		{FindingID: "F2", Title: "A body note", Severity: "P3", Verdict: FindingPosted},
		{FindingID: "F3", Title: "Rejected", Severity: "P1", Path: "app/b.rb", Line: 9, Verdict: FindingRejected, ReasonCode: "speculative"},
	})
	later := roundRun(t, st, pr.ID, 3, RoleJudge, "h3", 102, 2*time.Hour)
	record(later, []Finding{{FindingID: "F1", Title: "A later finding", Severity: "P0", Path: "app/c.rb", Line: 1, Verdict: FindingPosted}})

	got, fs, ok, err := st.FirstPostedReview(ctx, pr.ID, t0)
	if err != nil || !ok {
		t.Fatalf("FirstPostedReview = %v, %v", ok, err)
	}
	want := PostedReview{RunID: judge.ID, Round: 2, SHA: "h2", ReviewID: 101, CreatedAt: t0.Add(time.Hour + 10*time.Minute),
		RoundStart: t0.Add(time.Hour), RoundTarget: start.TargetSHA, Summary: ReviewSummary{RunID: judge.ID, ReviewID: 101, SHA: "h2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the round\n got %+v\nwant %+v", got, want)
	}
	var titles []string
	for _, f := range fs {
		if f.RunID != judge.ID || f.PRID != pr.ID || f.Round != 2 || f.Verdict != FindingPosted {
			t.Errorf("finding %+v is not of the posted round", f)
		}
		titles = append(titles, f.Title)
	}
	if want := []string{"The refund misses the cap", "A body note"}; !reflect.DeepEqual(titles, want) {
		t.Errorf("its findings %q, want %q", titles, want)
	}
	if fs[0].ID == 0 || fs[0].Severity != "P2" || fs[0].Path != "app/models/credit.rb" || fs[0].Line != 12 || fs[1].Path != "" || fs[1].Line != 0 {
		t.Errorf("the findings' places %+v", fs)
	}

	if got, fs, ok, err := st.FirstPostedReview(ctx, pr.ID, t0.Add(-2*time.Hour)); err != nil || !ok || got.Round != 1 || len(fs) != 1 {
		t.Errorf("since before round 1: round %d with %d findings, %v, %v; want round 1", got.Round, len(fs), ok, err)
	}
	roundRun(t, st, pr.ID, 4, RoleJudge, "h4", 103, 150*time.Minute) // a review with no finding
	if got, fs, ok, err := st.FirstPostedReview(ctx, pr.ID, t0.Add(140*time.Minute)); err != nil || !ok || got.Round != 4 || got.ReviewID != 103 ||
		len(fs) != 0 || got.OK {
		t.Errorf("a round that posted no finding: round %d review %d with %d findings, result read %v, %v, %v; want round 4 with none and no result",
			got.Round, got.ReviewID, len(fs), got.OK, ok, err)
	}
	// A round whose result counts its findings, none of which magnum recorded
	// (a result without provenance): the counts come with the round.
	counted := roundRun(t, st, pr.ID, 5, RoleJudge, "h5", 104, 170*time.Minute)
	if err := st.UpdateRun(ctx, counted.ID, func(u *RunUpdate) {
		u.Set("result_json", `{"event":"COMMENT","findings":{"P0":0,"P1":1,"P2":2,"P3":4}}`)
	}); err != nil {
		t.Fatal(err)
	}
	if got, fs, ok, err := st.FirstPostedReview(ctx, pr.ID, t0.Add(160*time.Minute)); err != nil || !ok || got.Round != 5 || len(fs) != 0 ||
		!got.OK || got.Summary.Counts != [4]int{0, 1, 2, 4} {
		t.Errorf("a round with counts and no rows: round %d, %d findings, result %v %+v, %v, %v", got.Round, len(fs), got.OK, got.Summary, ok, err)
	}
	if got, _, ok, err := st.FirstPostedReview(ctx, pr.ID, t0.Add(3*time.Hour)); err != nil || ok {
		t.Errorf("since after every round: %+v, %v, %v; want none", got, ok, err)
	}
	if _, _, ok, err := st.FirstPostedReview(ctx, 999, t0); err != nil || ok {
		t.Errorf("a PR the registry lacks: %v, %v; want none", ok, err)
	}
}
