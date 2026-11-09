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
// its last roles), and each answered question with when it was answered; an open question is not
// read.
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
	if len(got.Questions) != len(at) {
		t.Fatalf("questions %+v, want the two answered ones", got.Questions)
	}
	for i, q := range got.Questions {
		if q.Revision != i+1 || q.Index != 0 || q.Text != "Round the points down?" || q.Answer != "yes" || q.State != "answered" || !q.Answered.Equal(at[i]) {
			t.Errorf("question %d: %+v, want revision %d's answered at %v", i, q, i+1, at[i])
		}
	}
	if qs, left := got.AnswersAt(got.Rounds[0].Started); len(qs) != 1 || qs[0].Revision != 1 || left != 1 {
		t.Errorf("answers at round 1's start: %+v, %d left out; want revision 1's, and 1 left out", qs, left)
	}
}

// answeredWith is an answered question with its options, recommendation and every answer it had
// (question_answers), the first one at its answered time: the factory's rows.
func answeredWith(id, task, rev, idx, text, options, recommended string, took bool, answers ...[2]string) []string {
	took01 := "0"
	if took {
		took01 = "1"
	}
	stmts := []string{`INSERT INTO questions (id, task_id, revision, idx, text, options_json, recommended, answer, state, raised_by, asked_at,
		answered_at, took_recommendation) VALUES (` + id + `, ` + task + `, ` + rev + `, ` + idx + `, '` + text + `', '` + options + `', ` + recommended +
		`, '` + answers[len(answers)-1][0] + `', 'answered', 'fx-judge', '2026-11-05T08:00:00Z', '` + answers[0][1] + `', ` + took01 + `)`}
	for _, a := range answers {
		stmts = append(stmts, `INSERT INTO question_answers (question_id, answer, at) VALUES (`+id+`, '`+a[0]+`', '`+a[1]+`')`)
	}
	return stmts
}

// A task's answers as they stood at a time (a round's start) are the questions answered before it,
// each with its newest answer before it, and whether that answer took the recommendation: an answer
// given at that time or later never counts (it is left out, and counted), a revised answer counts as
// it was before the revision, and a question asked again is there with each copy answered before the
// time (answers.md shows the newest). A row from before the registry kept every answer counts with
// its answer at its answered time.
func TestLocalReviewReadsATasksAnswersAsTheyStoodAtATime(t *testing.T) {
	start := "2026-11-05T09:00:00Z"
	upDown := `[{"label":"Up","means":"round up"},{"label":"Down","means":"round down"}]`
	stmts := []string{localTask("1"), localTask("2"), startedCycle("1", "1", opened, start), answered("2", "1", "0", "2026-11-05T08:10:00Z"),
		`INSERT INTO questions (task_id, revision, idx, text, state, asked_at) VALUES (1, 3, 1, 'Open?', 'open', '2026-11-05T08:00:00Z')`,
		`INSERT INTO questions (task_id, revision, idx, text, answer, state, asked_at, answered_at) VALUES (1, 1, 0, 'Count refunds?',
			'only paid ones', 'answered', '2026-11-05T08:00:00Z', '2026-11-05T08:20:00Z')`}
	stmts = append(stmts, answeredWith("11", "1", "1", "1", "Round the points down?", upDown, "0", false,
		[2]string{"Up", "2026-11-05T08:40:00Z"}, [2]string{"Down", "2026-11-05T09:30:00Z"})...)
	stmts = append(stmts, answeredWith("12", "1", "1", "2", "Do guest orders count?", "[]", "-1", false, [2]string{"no", start})...)
	stmts = append(stmts, answeredWith("13", "1", "1", "3", "Which branch ships it?", "[]", "-1", false, [2]string{"main", "2026-11-05T08:50:00Z"})...)
	stmts = append(stmts, answeredWith("14", "1", "2", "0", "which branch ships it", "[]", "-1", false, [2]string{"develop", "2026-11-05T08:55:00Z"})...)
	stmts = append(stmts, answeredWith("15", "1", "3", "0", "Which branch ships it?", "[]", "-1", false, [2]string{"release", "2026-11-05T10:00:00Z"})...)
	got, err := LocalReview(context.Background(), fixture(t, stmts...), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Questions) != 6 {
		t.Fatalf("questions %+v, want task 1's six answered ones", got.Questions)
	}
	if q := got.Questions[1]; q.Answer != "Down" || q.Recommended != 0 || len(q.Options) != 2 || q.Options[1].Label != "Down" || q.RaisedBy != "fx-judge" ||
		len(q.History) != 2 || q.History[0].Answer != "Up" {
		t.Errorf("a revised question %+v, want its row, its options and both its answers", q)
	}
	qs, left := got.AnswersAt(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
	type stood struct {
		rev, idx    int
		answer      string
		recommended bool
	}
	want := []stood{{1, 0, "only paid ones", false}, {1, 1, "Up", true}, {1, 3, "main", false}, {2, 0, "develop", false}}
	var have []stood
	for _, q := range qs {
		if q.State != "answered" {
			t.Errorf("question %+v is not answered", q)
		}
		have = append(have, stood{q.Revision, q.Index, q.Answer, q.TookRecommendation})
	}
	if !slices.Equal(have, want) {
		t.Errorf("answers at the start %+v, want %+v", have, want)
	}
	if left != 3 {
		t.Errorf("%d answers left out, want 3: one at the start, a revision and an answer after it", left)
	}
	if qs, left := got.AnswersAt(time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC)); len(qs) != 6 || left != 0 || qs[1].Answer != "Down" || qs[1].TookRecommendation {
		t.Errorf("answers now: %+v, %d left out; want every answer as it stands", qs, left)
	}
}

// A begin event that names its round's judge after the roles (since the second opinion: "; judge
// <role> (<kind>)", and the judge it takes the place of) gives the roles alone.
func TestLocalReviewReadsTheRolesOfABeginEventThatNamesItsJudge(t *testing.T) {
	path := fixture(t, localTask("1"), startedCycle("1", "1", opened, "2026-11-05T09:00:00Z"),
		startedCycle("1", "2", opened, "2026-11-05T10:00:00Z"),
		event("1", "review", "begin", "round 1 (cycle 1) at aaaaaaaaaaaa: codex-judge, claude-review, codex-review; judge codex-judge (codex)"),
		event("1", "review", "begin", "round 2 (cycle 2) at aaaaaaaaaaaa: claude-judge, claude-review, codex-review; judge claude-judge (claude), "+
			"a second opinion in place of codex-judge (codex)"))
	got, err := LocalReview(context.Background(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rounds) != 2 || got.Rounds[0].Round != 1 || !slices.Equal(got.Rounds[0].Roles, []string{"codex-judge", "claude-review", "codex-review"}) ||
		got.Rounds[1].Round != 2 || !slices.Equal(got.Rounds[1].Roles, []string{"claude-judge", "claude-review", "codex-review"}) {
		t.Fatalf("rounds %+v, want each round's roles without its judge's words", got.Rounds)
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

// A task's Codex mark is read with its rounds (LocalTask.CodexNever, frozen with a local case), and
// TaskCodex reads it alone with the task's published PR, for the replay's check: none until the
// factory marks it and publishes it; ErrNoTask and ErrNoRegistry as LocalReview says them.
func TestTaskCodexReadsTheFactorysCodexMarkAndThePublishedPR(t *testing.T) {
	path := fixture(t, localTask("1"), localTask("2"), startedCycle("1", "1", opened, "2026-11-05T09:00:00Z"))
	ctx := context.Background()
	if never, pr, err := TaskCodex(ctx, path, 1); err != nil || never || pr != "" {
		t.Fatalf("unmarked: %v, %q, %v", never, pr, err)
	}
	if lt, err := LocalReview(ctx, path, 1); err != nil || lt.CodexNever {
		t.Fatalf("unmarked task %+v, %v", lt, err)
	}
	path = fixture(t, localTask("1"), localTask("2"), startedCycle("1", "1", opened, "2026-11-05T09:00:00Z"),
		`UPDATE tasks SET codex_never = 1 WHERE id = 1`,
		`INSERT INTO pr_links (task_id, repo, number, opened_at) VALUES (1, 'example/example', 7, '2026-11-05T12:00:00Z')`)
	if never, pr, err := TaskCodex(ctx, path, 1); err != nil || !never || pr != "example/example#7" {
		t.Fatalf("marked and published: %v, %q, %v", never, pr, err)
	}
	if lt, err := LocalReview(ctx, path, 1); err != nil || !lt.CodexNever {
		t.Fatalf("marked task %+v, %v", lt, err)
	}
	if never, pr, err := TaskCodex(ctx, path, 2); err != nil || never || pr != "" {
		t.Fatalf("the other task: %v, %q, %v", never, pr, err)
	}
	if _, _, err := TaskCodex(ctx, path, 9); !errors.Is(err, ErrNoTask) {
		t.Errorf("task 9: %v, want ErrNoTask", err)
	}
	if _, _, err := TaskCodex(ctx, filepath.Join(t.TempDir(), "factory.db"), 1); !errors.Is(err, ErrNoRegistry) {
		t.Errorf("no registry: %v, want ErrNoRegistry", err)
	}
}
