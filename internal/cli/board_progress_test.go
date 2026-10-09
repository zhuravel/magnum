package cli

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// describeProgress is g one fact a line, times as offsets from start.
func describeProgress(g *tui.RoundProgress, start time.Time) []string {
	if g == nil {
		return nil
	}
	off := func(t time.Time) string { return t.Sub(start).String() }
	out := []string{"started " + off(g.StartedAt)}
	for _, r := range g.Roles {
		s := r.Role + " " + r.Label
		if r.Judge {
			s += " (judge)"
		}
		if r.OwnPass {
			s += " (own pass)"
		}
		switch {
		case r.Started.IsZero():
			s += " not started"
		default:
			s += " started " + off(r.Started)
		}
		if !r.Ended.IsZero() {
			s += " ended " + off(r.Ended)
		}
		if r.Working {
			s += " working"
		}
		if r.Failed {
			s += " failed"
		}
		out = append(out, s)
	}
	return out
}

// roundRun records a run of the PR: created at start+created, submitted and
// ended at start plus those (nil: not yet).
func roundRun(t *testing.T, h *actHarness, prID int64, round int, role, state string, start time.Time, created time.Duration, submitted, ended *time.Duration) {
	t.Helper()
	at := func(d *time.Duration) *time.Time {
		if d == nil {
			return nil
		}
		return new(start.Add(*d))
	}
	r := store.Run{PRID: prID, Round: round, Role: role, Kind: store.RunRereview, State: state, TargetSHA: "h1",
		Identity: "i", ReviewerLogin: "l", PromptText: "p", CreatedAt: start.Add(created), SubmittedAt: at(submitted), EndedAt: at(ended)}
	if _, err := h.st.CreateRun(h.ctx, r); err != nil {
		t.Fatal(err)
	}
}

// The board's progress of a round in flight comes from the runs of that
// round only: a run of an earlier round, or of the round's number but
// created before last_round_started_at, is left out. A pending run, and a
// role the round named (engine.round_start) without a run yet, has not
// started; the judge comes last. A PR not reviewing or verifying (a
// claiming one's last_round_started_at is still its previous round's) has
// none.
func TestBoardRoundProgressReadsOnlyTheCurrentRoundsRuns(t *testing.T) {
	f := newRoundsFixture(t)
	h := f.h
	start := roundsNow.Add(-20 * time.Minute)
	h.setPR(f.pr.ID, store.PRReviewing, func(u *store.PRUpdate) { u.Set("last_round_started_at", start) })
	reviewed := h.seedPR("talkable/talkable", 730, store.PRReviewed)
	claiming := h.seedPR("talkable/talkable", 731, store.PRReviewed)
	h.setPR(claiming.ID, store.PRClaiming, func(u *store.PRUpdate) { u.Set("last_round_started_at", start.Add(-2*time.Hour)) })

	roundRun(t, h, f.pr.ID, 1, store.RoleClaude, store.RunVerified, start, -2*time.Hour, new(-2*time.Hour), new(-90*time.Minute))
	roundRun(t, h, f.pr.ID, 2, store.RoleJudge, store.RunAbandoned, start, -time.Minute, new(-time.Minute), nil)
	roundRun(t, h, f.pr.ID, 2, store.RoleClaude, store.RunEnded, start, time.Minute, new(time.Minute), new(12*time.Minute))
	roundRun(t, h, f.pr.ID, 2, store.RoleSimplify, store.RunWorking, start, time.Minute, new(2*time.Minute), nil)
	roundRun(t, h, f.pr.ID, 2, store.RoleCodexReview, store.RunPending, start, 3*time.Minute, nil, nil)
	roundRun(t, h, reviewed.ID, 1, store.RoleJudge, store.RunVerified, start, 0, new(time.Duration(0)), new(5*time.Minute))
	roundRun(t, h, claiming.ID, 1, store.RoleJudge, store.RunVerified, start, -2*time.Hour, new(-2*time.Hour), new(-time.Hour))

	f.at = start.Add(10 * time.Second)
	f.roundStart("rereview", []string{store.RoleJudge, store.RoleClaude, store.RoleSimplify, store.RoleCodexReview}, nil, false)

	rows := []tui.PRBoardRow{{Owner: "talkable", Repo: "talkable", Number: 729, State: "reviewing"},
		{Owner: "talkable", Repo: "talkable", Number: 730, State: "reviewed"},
		{Owner: "talkable", Repo: "talkable", Number: 731, State: "reviewing"}}
	ids := []int64{f.pr.ID, reviewed.ID, claiming.ID}
	if err := boardRoundFacts(h.ctx, h.st, ids, rows, roundsNow); err != nil {
		t.Fatal(err)
	}
	if err := boardRoundProgress(h.ctx, h.st, nil, ids, rows); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"started 0s",
		"claude-review claude started 1m0s ended 12m0s",
		"claude-simplify simplify started 2m0s working",
		"codex-review codex not started",
		"codex-judge judge (judge) not started",
	}
	if got := describeProgress(rows[0].Progress, start); !slices.Equal(got, want) {
		t.Errorf("Progress of the reviewing PR:\n%q\nwant\n%q", got, want)
	}
	for _, i := range []int{1, 2} {
		if rows[i].Progress != nil {
			t.Errorf("row %d (%s): Progress = %q, want none", i, rows[i].State, describeProgress(rows[i].Progress, start))
		}
	}
}

// A round_start older than the round (the previous round's, before this
// one's was written) names no role of it.
func TestBoardRoundProgressIgnoresThePreviousRoundsRoles(t *testing.T) {
	f := newRoundsFixture(t)
	start := roundsNow.Add(-time.Minute)
	f.roundStart("rereview", []string{store.RoleJudge, store.RoleClaude}, nil, false) // hours before start
	f.h.setPR(f.pr.ID, store.PRReviewing, func(u *store.PRUpdate) { u.Set("last_round_started_at", start) })

	rows := []tui.PRBoardRow{{Owner: "talkable", Repo: "talkable", Number: 729, State: "reviewing"}}
	if err := boardRoundFacts(f.h.ctx, f.h.st, []int64{f.pr.ID}, rows, roundsNow); err != nil {
		t.Fatal(err)
	}
	if err := boardRoundProgress(f.h.ctx, f.h.st, nil, []int64{f.pr.ID}, rows); err != nil {
		t.Fatal(err)
	}
	if got := describeProgress(rows[0].Progress, start); !slices.Equal(got, []string{"started 0s"}) {
		t.Errorf("Progress = %q, want the start alone", got)
	}
}

// A board without a round in flight reads no runs for it.
func TestBoardRoundProgressWithoutARunningRoundReadsNothing(t *testing.T) {
	rows := []tui.PRBoardRow{{State: "reviewed"}, {State: "queued"}}
	if err := boardRoundProgress(context.Background(), nil, nil, []int64{1, 2}, rows); err != nil {
		t.Fatal(err) // a nil registry would fail any read
	}
}

// RoundWhy says when the last round started: its round_start's time.
func TestBoardRoundFactsTellWhenTheRoundStarted(t *testing.T) {
	f := newRoundsFixture(t)
	f.roundStart("initial", []string{store.RoleJudge}, nil, false)
	at := f.at
	f.roundStart("rereview", []string{store.RoleJudge, store.RoleClaude}, nil, false)
	if got := f.facts().RoundWhy; got == nil || !got.At.Equal(at) {
		t.Errorf("RoundWhy = %+v, want At %v", got, at)
	}
}

// A role's stage label is the shortest of its name and aliases, the name
// winning a tie, then the aliases in their order; a role the configuration
// does not know keeps its stored name.
func TestStageLabelIsTheShortestOfARolesNames(t *testing.T) {
	for _, tc := range []struct {
		cfg        *config.Config
		role, want string
	}{
		{nil, store.RoleSimplify, "simplify"},
		{nil, store.RoleCodexReview, "codex"},
		{nil, store.RoleClaude, "claude"},
		{nil, store.RoleJudge, "judge"},
		{nil, "codex_review", "codex"},
		{nil, "gone-role", "gone-role"},
		{config.Defaults(), store.RoleSimplify, "simplify"},
		{&config.Config{Roles: []config.Role{{Name: "sec", Aliases: []string{"security-review"}}}}, "sec", "sec"},
		{&config.Config{Roles: []config.Role{{Name: "lint", Aliases: []string{"ruff", "rl"}}}}, "ruff", "rl"},
		{&config.Config{Roles: []config.Role{{Name: "lint", Aliases: []string{"ruff"}}}}, "ruff", "lint"},
	} {
		if got := stageLabel(tc.cfg, tc.role); got != tc.want {
			t.Errorf("stageLabel(%v, %q) = %q, want %q", tc.cfg != nil, tc.role, got, tc.want)
		}
	}
}

// The dashboard's rounds carry each round's progress, by the PR their label
// names (a delta check's label included).
func TestStatusDashRoundsCarryEachRoundsProgress(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	prs, err := st.ListPRs(ctx, store.PRFilter{States: []string{store.PRReviewing}})
	if err != nil || len(prs) != 1 {
		t.Fatalf("reviewing PRs = %d, %v", len(prs), err)
	}
	start := now.Add(-17 * time.Minute)
	if err := st.UpdatePR(ctx, prs[0].ID, func(u *store.PRUpdate) { u.Set("last_round_started_at", start) }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, store.Run{PRID: prs[0].ID, Round: 1, Role: store.RoleSimplify, Kind: store.RunInitial,
		TargetSHA: "h1", State: store.RunWorking, Identity: "i", ReviewerLogin: "l", PromptText: "p",
		CreatedAt: start.Add(time.Minute), SubmittedAt: new(start.Add(time.Minute))}); err != nil {
		t.Fatal(err)
	}
	r, err := statusGather(ctx, d, statusOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	labels := append(slices.Clone(r.Rounds.PRs), "talkable#11940 (delta check)", "talkable#9")
	got := statusDashProgress(ctx, d, labels)
	if len(got) != len(labels) {
		t.Fatalf("progress for %d of %d labels", len(got), len(labels))
	}
	want := []string{"started 0s", "claude-simplify simplify started 1m0s working"}
	for i, label := range labels {
		g := describeProgress(got[i], start)
		switch label {
		case "talkable#9":
			if g != nil {
				t.Errorf("%s: %q, want none", label, g)
			}
		default:
			if !slices.Equal(g, want) {
				t.Errorf("%s: %q, want %q", label, g, want)
			}
		}
	}
	if fmt.Sprint(r.Rounds.PRs) != "[talkable#11940]" {
		t.Errorf("rounds = %v", r.Rounds.PRs)
	}
}

// progressRun is a run of a round started at start: created at start+created,
// submitted and ended at start plus those (nil: not yet).
func progressRun(role, kind, state string, start time.Time, created time.Duration, submitted, ended *time.Duration) store.Run {
	at := func(d *time.Duration) *time.Time {
		if d == nil {
			return nil
		}
		return new(start.Add(*d))
	}
	return store.Run{Round: 2, Role: role, Kind: kind, State: state, CreatedAt: start.Add(created), SubmittedAt: at(submitted), EndedAt: at(ended)}
}

// The judge's own pass, its runs of kind own_pass, is an entry of its own
// right before the judge's: its start, work, end and failure never merge into
// the judge's main run, created up front and pending while the own pass
// works, nor the other way round; a model fallback's continuation of the own
// pass (kind own_pass too) belongs to the own pass.
func TestRoleProgressKeepsOwnPassApartFromJudge(t *testing.T) {
	start := roundsNow.Add(-30 * time.Minute)
	claude := progressRun(store.RoleClaude, store.RunRereview, store.RunWorking, start, time.Minute, new(time.Minute), nil)
	judge := progressRun(store.RoleJudge, store.RunRereview, store.RunPending, start, time.Minute, nil, nil)
	own := progressRun(store.RoleJudge, store.RunOwnPass, store.RunWorking, start, time.Minute, new(time.Minute), nil)

	got := describeProgress(&tui.RoundProgress{StartedAt: start, Roles: roleProgress(nil, []store.Run{claude, judge, own})}, start)
	want := []string{
		"started 0s",
		"claude-review claude started 1m0s working",
		"codex-judge judge (judge) (own pass) started 1m0s working",
		"codex-judge judge (judge) not started",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the own pass working with the reviewers:\n%q\nwant\n%q", got, want)
	}

	// The own pass moved to another model and ended; the judge works.
	claude.State, claude.EndedAt = store.RunEnded, new(start.Add(12*time.Minute))
	own.State, own.EndedAt = store.RunAbandoned, new(start.Add(5*time.Minute))
	fallback := progressRun(store.RoleJudge, store.RunOwnPass, store.RunEnded, start, 5*time.Minute, new(6*time.Minute), new(15*time.Minute))
	judge.State, judge.SubmittedAt = store.RunWorking, new(start.Add(16*time.Minute))
	got = describeProgress(&tui.RoundProgress{StartedAt: start, Roles: roleProgress(nil, []store.Run{claude, judge, own, fallback})}, start)
	want = []string{
		"started 0s",
		"claude-review claude started 1m0s ended 12m0s",
		"codex-judge judge (judge) (own pass) started 1m0s ended 15m0s",
		"codex-judge judge (judge) started 16m0s working",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the judge working after its own pass:\n%q\nwant\n%q", got, want)
	}

	// The own pass failed and the judge's main run ended.
	fallback.State = store.RunFailed
	judge.State, judge.EndedAt = store.RunEnded, new(start.Add(25*time.Minute))
	got = describeProgress(&tui.RoundProgress{StartedAt: start, Roles: roleProgress(nil, []store.Run{claude, judge, own, fallback})}, start)
	want = []string{
		"started 0s",
		"claude-review claude started 1m0s ended 12m0s",
		"codex-judge judge (judge) (own pass) started 1m0s ended 15m0s failed",
		"codex-judge judge (judge) started 16m0s ended 25m0s",
	}
	if !slices.Equal(got, want) {
		t.Errorf("a failed own pass beside an ended judge:\n%q\nwant\n%q", got, want)
	}
}

// A role the round named without a run is listed as not started, and the
// judge whose own pass alone has runs is one of them: its own pass's entry
// does not stand for its main run.
func TestRoundProgressListsTheJudgeNotStartedBesideItsOwnPass(t *testing.T) {
	start := roundsNow.Add(-10 * time.Minute)
	g := &tui.RoundProgress{StartedAt: start, Roles: roleProgress(nil, []store.Run{
		progressRun(store.RoleClaude, store.RunInitial, store.RunWorking, start, time.Minute, new(time.Minute), nil),
		progressRun(store.RoleJudge, store.RunOwnPass, store.RunWorking, start, time.Minute, new(time.Minute), nil),
	})}
	w := &tui.RoundWhy{At: start.Add(time.Second), Roles: []string{store.RoleJudge, store.RoleClaude, store.RoleCodexReview}}
	listNamedRoles(nil, g, w)
	want := []string{
		"started 0s",
		"claude-review claude started 1m0s working",
		"codex-review codex not started",
		"codex-judge judge (judge) (own pass) started 1m0s working",
		"codex-judge judge (judge) not started",
	}
	if got := describeProgress(g, start); !slices.Equal(got, want) {
		t.Errorf("Progress:\n%q\nwant\n%q", got, want)
	}
}
