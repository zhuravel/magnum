package store

import (
	"context"
	"maps"
	"strconv"
	"testing"
	"time"
)

// postedRun is a judge run of pr at t0+d whose review was posted (posted
// false: a dry run's), on head sha, with posted findings of the given
// priorities and one rejected finding.
func postedRun(t *testing.T, st *Store, prID int64, d time.Duration, sha string, posted bool, priorities ...string) Run {
	t.Helper()
	r := Run{PRID: prID, Round: 1, Role: RoleJudge, Kind: RunInitial, State: RunVerified, TargetSHA: sha, Identity: "talkable-app",
		ReviewerLogin: "talkable[bot]", PromptText: "p", CreatedAt: t0.Add(d)}
	if posted {
		r.Outcome, r.ReviewID = new("posted"), new(int64(1000+d/time.Minute))
	} else {
		r.Outcome = new("dry_run")
	}
	run, err := st.CreateRun(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	var fs []Finding
	for i, p := range priorities {
		fs = append(fs, Finding{FindingID: "F" + strconv.Itoa(i+1), Severity: p, Verdict: FindingPosted})
	}
	fs = append(fs, Finding{FindingID: "R1", Severity: "P2", Verdict: FindingRejected, ReasonCode: "speculative"})
	if err := st.RecordFindings(context.Background(), run.ID, prID, 1, fs); err != nil {
		t.Fatal(err)
	}
	return run
}

// measureVersion records version id, landed a minute before t0 plus ten
// hours per earlier version, restarted at restarted (nil: not yet).
func measureVersion(t *testing.T, st *Store, id int64, sha string, restarted *time.Time) LearnVersion {
	t.Helper()
	ctx := context.Background()
	v, err := st.CreateLearnVersion(ctx, LearnVersion{LandedSHA: "landed" + strconv.FormatInt(id, 10), SkillSHA: sha, PromptsSHA: "p",
		LandedAt: t0.Add(time.Duration(id-1) * 10 * time.Hour).Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if restarted != nil {
		if err := st.MarkLearnRestarted(ctx, v.ID, *restarted); err != nil {
			t.Fatal(err)
		}
		v.RestartedAt = restarted
	}
	return v
}

// A version's window runs from the restart on it to the restart on the
// next version: only the posted reviews created in it count, with their
// posted findings by priority and the misses on the commits they reviewed.
func TestVersionMeasureCountsOnlyItsWindow(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	r1, r2 := t0, t0.Add(10*time.Hour)
	v1 := measureVersion(t, st, 1, "aaaaaaaaaaaa", &r1)
	v2 := measureVersion(t, st, 2, "bbbbbbbbbbbb", &r2)

	postedRun(t, st, pr.ID, -time.Hour, "h0", true, "P1") // before the restart on v1
	in := postedRun(t, st, pr.ID, time.Hour, "h1", true, "P1", "P2", "")
	postedRun(t, st, pr.ID, 2*time.Hour, "h2", false, "P0") // a dry run posts nothing
	later := postedRun(t, st, pr.ID, 11*time.Hour, "h3", true, "P3")
	for i, m := range []struct{ sha, class, scope string }{{in.TargetSHA, MissMiss, MissScopeGeneral}, {in.TargetSHA, MissMiss, MissScopeRepo},
		{in.TargetSHA, MissNotIssue, ""}, {later.TargetSHA, MissMiss, MissScopeGeneral}, {"elsewhere", MissMiss, MissScopeGeneral}} {
		if _, err := st.UpsertMiss(ctx, Miss{PRID: pr.ID, SourceURL: pr.URL + "#r" + strconv.Itoa(i), SourceKind: MissSourceThread, Reviewer: "rev-ann",
			ReviewedSHA: m.sha, Class: m.class, Severity: "P1", Scope: m.scope}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.VersionMeasure(ctx, v1, &v2)
	if err != nil {
		t.Fatal(err)
	}
	if got.VersionID != v1.ID || got.Commit != "landed1" || !got.From.Equal(r1) || got.To == nil || !got.To.Equal(r2) {
		t.Fatalf("window = %+v", got)
	}
	if got.JudgeRuns != 1 || got.Posted != 3 || !maps.Equal(got.ByPriority, map[string]int{"P1": 1, "P2": 1, "": 1}) ||
		got.MissesGeneral != 1 || got.MissesRepo != 1 {
		t.Fatalf("v1 = %+v", got)
	}
	if _, ok := got.DeclinedShare(); !ok {
		t.Fatal("no share with posted findings")
	}
	// The newest version's window is open; one the daemon has not
	// restarted on yet starts at its landing.
	open, err := st.VersionMeasure(ctx, v2, nil)
	if err != nil || open.To != nil || open.JudgeRuns != 1 || open.Posted != 1 || open.ByPriority["P3"] != 1 || open.MissesGeneral != 1 {
		t.Fatalf("v2 = %+v, %v", open, err)
	}
	v3 := measureVersion(t, st, 3, "cccccccccccc", nil)
	if m, err := st.VersionMeasure(ctx, v2, &v3); err != nil || m.To != nil {
		t.Fatalf("v2 before the restart on v3 = %+v, %v", m, err)
	}
	if m, err := st.VersionMeasure(ctx, v3, nil); err != nil || !m.From.Equal(v3.LandedAt) || m.JudgeRuns != 0 {
		t.Fatalf("v3 = %+v, %v", m, err)
	}
	if _, ok := (VersionMeasure{}).DeclinedShare(); ok {
		t.Fatal("a share without posted findings")
	}
	all, err := st.VersionMeasures(ctx, []LearnVersion{v3, v2, v1}, 2)
	if err != nil || len(all) != 2 || all[0].VersionID != v3.ID || all[1].VersionID != v2.ID || all[1].To != nil {
		t.Fatalf("VersionMeasures = %+v, %v", all, err)
	}
}

// The answers to a version's findings are the declines of its window's
// runs whose round ran the version's skill copy (skill_sha); the skill
// before the first version, which no row names, counts every one.
func TestVersionMeasureJoinsDeclinesBySkillSHA(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	r1 := t0
	v := measureVersion(t, st, 1, "aaaaaaaaaaaa", &r1)
	run := postedRun(t, st, pr.ID, time.Hour, "h1", true, "P1", "P1", "P2", "P2")
	before := postedRun(t, st, pr.ID, -time.Hour, "h0", true, "P1")
	for i, d := range []struct {
		run, skill, outcome string
	}{{run.ID, "aaaaaaaaaaaa", DeclineDeclined}, {run.ID, "aaaaaaaaaaaa", DeclineArgued}, {run.ID, "aaaaaaaaaaaa", DeclineFixed},
		{run.ID, "bbbbbbbbbbbb", DeclineDeclined}, {before.ID, "aaaaaaaaaaaa", DeclineDeclined}} {
		if _, err := st.UpsertDecline(ctx, Decline{PRID: pr.ID, SourceURL: pr.URL + "#d" + strconv.Itoa(i), CommentID: int64(i + 1), RunID: d.run,
			ReviewedSHA: "h1", SkillSHA: d.skill, Outcome: d.outcome}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.VersionMeasure(ctx, v, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got.Outcomes, map[string]int{DeclineDeclined: 1, DeclineArgued: 1, DeclineFixed: 1}) || got.Declined() != 2 {
		t.Fatalf("outcomes = %v", got.Outcomes)
	}
	if share, ok := got.DeclinedShare(); !ok || share != 50 {
		t.Fatalf("share = %v, %v", share, ok)
	}
	base, err := st.VersionMeasure(ctx, LearnVersion{LandedAt: t0.Add(-2 * time.Hour)}, &v)
	if err != nil || base.VersionID != 0 || base.JudgeRuns != 1 || !maps.Equal(base.Outcomes, map[string]int{DeclineDeclined: 1}) {
		t.Fatalf("the skill before = %+v, %v", base, err)
	}
}
