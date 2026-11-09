package factoryread

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func localTask(id string) string {
	return `INSERT INTO tasks (id, jira_key, repo, title, branch, state, created_at, updated_at) VALUES (` + id +
		`, 'PR-12361', 'example/example', 'Credit points count the activities', 'zhuravel-PR-12361-credit-points', 'done',
		'2026-11-05T08:00:00Z', '2026-11-05T08:00:00Z')`
}

func startedCycle(task, n, head, at string) string {
	return `INSERT INTO review_cycles (task_id, n, head_before, started_at) VALUES (` + task + `, ` + n + `, '` + head + `', '` + at + `')`
}

func event(task, kind, phase, text string) string {
	return `INSERT INTO events (at, task_id, kind, phase, text) VALUES ('2026-11-05T09:00:00Z', ` + task + `, '` + kind + `', '` + phase + `', '` +
		text + `')`
}

func answered(task, rev, idx, at string) string {
	return `INSERT INTO questions (task_id, revision, idx, text, answer, state, asked_at, answered_at) VALUES (` + task + `, ` + rev + `, ` + idx +
		`, 'Round the points down?', 'yes', 'answered', '2026-11-05T08:30:00Z', '` + at + `')`
}

// The replay of a local round reads, from the factory's registry, the task's repository, branch,
// title and Jira key, each cycle of its local review with the head it reviewed and its start, the
// loop's round and the roles its newest begin event names (a round that ran again in its cycle names
// its last roles), and when each answered question was answered; an open question has no time.
func TestLocalReviewReadsATasksRoundsTheirRolesAndItsAnswerTimes(t *testing.T) {
	path := fixture(t, localTask("1"), localTask("2"),
		startedCycle("1", "1", opened, "2026-11-05T09:00:00Z"), startedCycle("1", "2", approved, "2026-11-05T11:00:00Z"),
		startedCycle("2", "1", cycleOne, "2026-11-05T09:30:00Z"),
		event("1", "review", "begin", "round 1 (cycle 1) at aaaaaaaaaaaa: claude-review, codex-review, codex-judge"),
		event("1", "review", "begin", "round 1 (cycle 1) at aaaaaaaaaaaa: claude-review, claude-judge"),
		event("1", "review", "ok", "round 1 (cycle 1) ended dry_run"),
		event("1", "review", "begin", "round 2 (cycle 2) at bbbbbbbbbbbb: claude-review, claude-judge"),
		event("2", "review", "begin", "round 1 (cycle 1) at cccccccccccc: other-role"),
		answered("1", "1", "0", "2026-11-05T08:45:00Z"), answered("1", "2", "0", "2026-11-05T10:15:00Z"),
		`INSERT INTO questions (task_id, revision, idx, text, state, asked_at) VALUES (1, 2, 1, 'Open?', 'open', '2026-11-05T10:00:00Z')`)
	got, err := LocalReview(context.Background(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 1 || got.Repo != "example/example" || got.Branch != "zhuravel-PR-12361-credit-points" || got.Title != "Credit points count the activities" ||
		got.JiraKey != "PR-12361" {
		t.Fatalf("task %+v", got)
	}
	want := []LocalRound{
		{Cycle: 1, Round: 1, Head: opened, Started: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC), Roles: []string{"claude-review", "claude-judge"}},
		{Cycle: 2, Round: 2, Head: approved, Started: time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC), Roles: []string{"claude-review", "claude-judge"}},
	}
	if len(got.Rounds) != len(want) {
		t.Fatalf("rounds %+v, want %+v", got.Rounds, want)
	}
	for i, r := range got.Rounds {
		w := want[i]
		if r.Cycle != w.Cycle || r.Round != w.Round || r.Head != w.Head || !r.Started.Equal(w.Started) || !slices.Equal(r.Roles, w.Roles) {
			t.Errorf("round %d: %+v, want %+v", i, r, w)
		}
	}
	at := []time.Time{time.Date(2026, 11, 5, 8, 45, 0, 0, time.UTC), time.Date(2026, 11, 5, 10, 15, 0, 0, time.UTC)}
	if !slices.EqualFunc(got.Answered, at, time.Time.Equal) {
		t.Errorf("answered at %v, want %v", got.Answered, at)
	}
	if n := got.AnsweredAfter(got.Rounds[0].Started); n != 1 {
		t.Errorf("answered after round 1's start: %d, want 1", n)
	}
}

// A cycle no begin event names (its event in another shape, or for another head) has round 0 and no
// roles: the replay refuses to freeze it rather than guess them.
func TestLocalReviewLeavesACycleWithoutItsBeginEventUnnamed(t *testing.T) {
	path := fixture(t, localTask("1"), startedCycle("1", "1", opened, "2026-11-05T09:00:00Z"),
		event("1", "review", "begin", "round 1 (cycle 1) at bbbbbbbbbbbb: claude-review"),
		event("1", "review", "info", "round 1 (cycle 1) at aaaaaaaaaaaa: claude-review"),
		event("1", "review", "begin", "the review of cycle 1 began"))
	got, err := LocalReview(context.Background(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rounds) != 1 || got.Rounds[0].Round != 0 || got.Rounds[0].Roles != nil {
		t.Fatalf("rounds %+v, want one cycle without its round or roles", got.Rounds)
	}
}

// A task the registry does not hold is ErrNoTask; a registry that is not there is ErrNoRegistry, and
// the read creates nothing; the registry and its WAL keep every byte.
func TestLocalReviewSaysWhatIsMissingAndWritesNothing(t *testing.T) {
	path := fixture(t, localTask("1"), startedCycle("1", "1", opened, "2026-11-05T09:00:00Z"))
	if _, err := LocalReview(context.Background(), path, 9); !errors.Is(err, ErrNoTask) {
		t.Errorf("task 9: %v, want ErrNoTask", err)
	}
	dir := t.TempDir()
	if _, err := LocalReview(context.Background(), filepath.Join(dir, "factory.db"), 1); !errors.Is(err, ErrNoRegistry) {
		t.Errorf("no registry: %v, want ErrNoRegistry", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("the read left %v", ents)
	}
	db, _ := os.ReadFile(path)
	wal, _ := os.ReadFile(path + "-wal")
	if len(wal) == 0 {
		t.Fatal("the fixture has no WAL: the test proves nothing")
	}
	if _, err := LocalReview(context.Background(), path, 1); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, db) {
		t.Error("the read changed the registry")
	}
	if b, _ := os.ReadFile(path + "-wal"); !bytes.Equal(b, wal) {
		t.Error("the read changed the WAL")
	}
}
