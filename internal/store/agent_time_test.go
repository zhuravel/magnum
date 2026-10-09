package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// agentRun creates a run of the given state with its timestamps relative to
// t0 (nil leaves the column empty).
func agentRun(t *testing.T, st *Store, prID int64, round int, role, state string, created time.Duration, submitted, working, ended *time.Duration) Run {
	t.Helper()
	at := func(d *time.Duration) *time.Time {
		if d == nil {
			return nil
		}
		return new(t0.Add(*d))
	}
	r, err := st.CreateRun(context.Background(), Run{PRID: prID, Round: round, Role: role, Kind: RunInitial, State: state,
		TargetSHA: "h1", Identity: "talkable-app", ReviewerLogin: "talkable[bot]", PromptText: "p",
		CreatedAt: t0.Add(created), SubmittedAt: at(submitted), WorkingSeenAt: at(working), EndedAt: at(ended)})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return r
}

func TestAgentTimeSumsRunDurationsAndCountsDistinctRounds(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	other := mustPR(t, st, repo.ID, 8, PRReviewed)
	since := t0.Add(-24 * time.Hour)
	now := t0.Add(2 * time.Hour)

	// Round 1: two roles; the first from its submission, the second (never
	// submitted) from its creation.
	agentRun(t, st, pr.ID, 1, RoleClaude, RunVerified, time.Minute, new(2*time.Minute), nil, new(12*time.Minute)) // 10m
	agentRun(t, st, pr.ID, 1, RoleCodexReview, RunVerified, time.Minute, nil, nil, new(6*time.Minute))            // 5m
	// Round 2: the judge is still going, to now.
	agentRun(t, st, pr.ID, 2, RoleJudge, RunWorking, time.Hour, new(time.Hour+time.Minute), new(time.Hour+30*time.Minute), nil) // 59m
	// Before the window: neither its time nor its round counts.
	agentRun(t, st, pr.ID, 0, RoleJudge, RunVerified, -48*time.Hour, nil, nil, new(-47*time.Hour))
	// Another PR.
	agentRun(t, st, other.ID, 1, RoleClaude, RunVerified, 0, nil, nil, new(30*time.Minute)) // 30m

	got, err := st.AgentTimeSince(ctx, since, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []PRAgentTime{
		{PRID: pr.ID, Repo: "talkable/talkable", Number: 7, Time: 10*time.Minute + 5*time.Minute + 59*time.Minute, Rounds: 2},
		{PRID: other.ID, Repo: "talkable/talkable", Number: 8, Time: 30 * time.Minute, Rounds: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentTimeSince = %+v, want %+v", got, want)
	}

	one, err := st.AgentTimeSince(ctx, since, now, other.ID)
	if err != nil || !reflect.DeepEqual(one, want[1:]) {
		t.Fatalf("AgentTimeSince(other) = %+v, %v; want %+v", one, err, want[1:])
	}
	none, err := st.AgentTimeSince(ctx, t0.Add(24*time.Hour), now)
	if err != nil || len(none) != 0 {
		t.Fatalf("a window after every run = %+v, %v; want nothing", none, err)
	}
}

func TestAgentTimeIsSortedByTimeThenPR(t *testing.T) {
	st, _ := newStore(t)
	repo := mustRepo(t, st)
	a := mustPR(t, st, repo.ID, 1, PRReviewed)
	b := mustPR(t, st, repo.ID, 2, PRReviewed)
	c := mustPR(t, st, repo.ID, 3, PRReviewed)
	agentRun(t, st, a.ID, 1, RoleClaude, RunVerified, 0, nil, nil, new(10*time.Minute))
	agentRun(t, st, b.ID, 1, RoleClaude, RunVerified, 0, nil, nil, new(40*time.Minute))
	agentRun(t, st, c.ID, 1, RoleClaude, RunVerified, 0, nil, nil, new(10*time.Minute))

	got, err := st.AgentTimeSince(context.Background(), t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var numbers []int
	for _, g := range got {
		numbers = append(numbers, g.Number)
	}
	if want := []int{2, 1, 3}; !reflect.DeepEqual(numbers, want) {
		t.Fatalf("order = %v, want %v (most time first, ties by PR)", numbers, want)
	}
}

func TestAgentTimeNeverCountsANegativeOrUnendedFinishedRun(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	now := t0.Add(10 * time.Hour)

	// Ended before it started (clocks differ between the daemon and the agent).
	agentRun(t, st, pr.ID, 1, RoleClaude, RunVerified, 0, new(10*time.Minute), nil, new(5*time.Minute))
	// A failed run that never got an end counts to the last time it was seen
	// working, not to now.
	agentRun(t, st, pr.ID, 1, RoleCodexReview, RunFailed, 0, new(time.Minute), new(21*time.Minute), nil) // 20m
	// A failed run never seen working has no time.
	agentRun(t, st, pr.ID, 2, RoleClaude, RunAbandoned, 0, nil, nil, nil)
	// Waiting for its prompt to go: counted to now.
	agentRun(t, st, pr.ID, 3, RoleJudge, RunPending, 9*time.Hour, nil, nil, nil) // 1h

	got, err := st.AgentTimeSince(ctx, t0.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Time != 20*time.Minute+time.Hour || got[0].Rounds != 3 {
		t.Fatalf("AgentTimeSince = %+v, want 1h20m over 3 rounds", got)
	}
}
