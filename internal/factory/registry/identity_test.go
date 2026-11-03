package registry_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/store"
)

func TestTransitionTaskWritesASpan(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	clk.Add(time.Minute)
	err := r.TransitionTask(ctx, task.ID, []string{registry.TaskQueued}, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", "investigate")
	})
	if err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	err = r.TransitionTask(ctx, task.ID, []string{registry.TaskActive}, registry.TaskNeedsYou, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "you")
		u.Set("waits_what", "answer 3 questions")
		u.Because("3 questions")
	})
	if err != nil {
		t.Fatal(err)
	}

	// A refused compare-and-set writes no span.
	clk.Add(time.Minute)
	err = r.TransitionTask(ctx, task.ID, []string{registry.TaskActive}, registry.TaskWaiting, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "ci")
		u.Because("this must not be recorded")
	})
	if !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("transition from the wrong state = %v, want ErrConflict", err)
	}
	if err := r.TransitionTask(ctx, task.ID+100, nil, registry.TaskActive, nil); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("transition of a missing task = %v, want ErrNotFound", err)
	}

	spans, err := r.Spans(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 3 {
		t.Fatalf("Spans = %+v, want 3 (creation and two moves)", spans)
	}
	for i := range spans {
		spans[i].ID = 0 // the row ids are the registry's
	}
	want := []registry.Span{
		{TaskID: task.ID, From: "", To: registry.TaskQueued, At: t0},
		{TaskID: task.ID, From: registry.TaskQueued, To: registry.TaskActive, Stage: "investigate", At: t0.Add(time.Minute)},
		{TaskID: task.ID, From: registry.TaskActive, To: registry.TaskNeedsYou, Stage: "investigate", WaitsWho: "you",
			WaitsWhat: "answer 3 questions", OnYou: true, Reason: "3 questions", At: t0.Add(2 * time.Minute)},
	}
	for i := range want {
		if spans[i] != want[i] {
			t.Errorf("span %d\n got %+v\nwant %+v", i, spans[i], want[i])
		}
	}

	// A state-keeping update writes a span when it moves what a span records
	// (the stage here), and none when it only sets bookkeeping like the head.
	clk.Add(time.Minute)
	if err := r.TransitionTask(ctx, task.ID, []string{registry.TaskNeedsYou}, "", func(u *registry.TaskUpdate) { u.Set("head_sha", "abc123") }); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Spans(ctx, task.ID); len(got) != 3 {
		t.Errorf("setting the head wrote a span: %d spans", len(got))
	}
	if err := r.TransitionTask(ctx, task.ID, []string{registry.TaskNeedsYou}, "", func(u *registry.TaskUpdate) { u.Set("stage", "coding") }); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Spans(ctx, task.ID)
	if len(got) != 4 || got[3].From != registry.TaskNeedsYou || got[3].To != registry.TaskNeedsYou || got[3].Stage != "coding" || !got[3].OnYou {
		t.Errorf("spans after a stage move = %+v", got)
	}
	if none, err := r.Spans(ctx, task.ID+100); err != nil || len(none) != 0 {
		t.Errorf("Spans of a missing task = %+v, %v; want none", none, err)
	}
}

func TestAPublishedTaskHoldsItsKey(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12345")
	err := r.TransitionTask(ctx, task.ID, nil, registry.TaskPublished, func(u *registry.TaskUpdate) { u.Set("pr_number", 77) })
	if err != nil {
		t.Fatalf("transition to published: %v", err)
	}

	_, err = r.CreateTask(ctx, registry.Task{JiraKey: "PR-12345", Kind: "bug"})
	var active registry.ErrActiveKey
	if !errors.As(err, &active) || active.TaskID != task.ID {
		t.Fatalf("CreateTask while the PR is open = %v, want an ErrActiveKey for task %d", err, task.ID)
	}
	held, err := r.TaskByKey(ctx, "PR-12345")
	if err != nil || held.ID != task.ID || held.State != registry.TaskPublished || held.PRNumber != 77 {
		t.Errorf("TaskByKey = %+v, %v; want the published task", held, err)
	}
	for _, key := range []string{"PR-12399", ""} {
		if _, err := r.TaskByKey(ctx, key); !errors.Is(err, registry.ErrNotFound) {
			t.Errorf("TaskByKey(%q) = %v, want ErrNotFound", key, err)
		}
	}

	// Done means merged: the key is free again.
	if err := r.TransitionTask(ctx, task.ID, []string{registry.TaskPublished}, registry.TaskDone, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TaskByKey(ctx, "PR-12345"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("TaskByKey after done = %v, want ErrNotFound", err)
	}
	if _, err := r.CreateTask(ctx, registry.Task{JiraKey: "PR-12345", Kind: "bug"}); err != nil {
		t.Errorf("CreateTask after done: %v", err)
	}
}

func TestTheSchemaHoldsAPublishedKeyToo(t *testing.T) {
	t.Parallel()
	db := registrytest.Open(t).DBForTest()
	const insert = "INSERT INTO tasks (jira_key, state, created_at, updated_at) VALUES ('PR-12345', ?, 'x', 'x')"
	if _, err := db.ExecContext(t.Context(), insert, "published"); err != nil {
		t.Fatalf("a published task: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), insert, "queued"); err == nil {
		t.Error("the schema holds two unfinished tasks for a key that has a published one")
	}
}

func TestATaskCarriesCodexNeverAndItsQuestionRevision(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	if task.CodexNever || task.QuestionRev != 0 {
		t.Fatalf("a new task = %+v, want codex allowed and no question revision", task)
	}
	if err := r.TransitionTask(ctx, task.ID, nil, "", func(u *registry.TaskUpdate) { u.Set("codex_never", true) }); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceQuestions(ctx, task.ID, 4, askTwo()); err != nil {
		t.Fatal(err)
	}
	got, _ := r.TaskByID(ctx, task.ID)
	if !got.CodexNever || got.QuestionRev != 4 {
		t.Errorf("task = %+v, want CodexNever and QuestionRev 4", got)
	}
	err := r.TransitionTask(ctx, task.ID, nil, "", func(u *registry.TaskUpdate) { u.Set("question_rev", 9) })
	if err == nil {
		t.Error("TransitionTask set the question revision, which ReplaceQuestions keeps")
	}
}

// writeVersion1 builds the database an older binary left: migration 1 only,
// and a row in every table that points at a task.
func writeVersion1(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("version 1 file: %s: %v", q, err)
		}
	}
	exec(registry.MigrationSQLForTest(1))
	exec("PRAGMA user_version = 1")
	ts := store.FormatTime(t0)
	exec(`INSERT INTO tasks (id, jira_key, kind, repo, title, state, stage, waits_who, waits_what, attempt, created_at, updated_at)
		VALUES (1, 'PR-12301', 'bug', 'example/app', 'First', 'needs_you', 'investigate', 'you', 'answer a question', 1, ?, ?)`, ts, ts)
	exec(`INSERT INTO tasks (id, prompt, kind, repo, title, state, created_at, updated_at)
		VALUES (2, 'a free prompt', 'chore', 'example/app', 'Second', 'stopped', ?, ?)`, ts, ts)
	exec("INSERT INTO attempts (id, task_id, stage, n, state, started_at) VALUES ('t1-investigate-1', 1, 'investigate', 1, 'ended', ?)", ts)
	exec("INSERT INTO attempts (id, task_id, stage, n, state, started_at) VALUES ('t2-coding-1', 2, 'coding', 1, 'abandoned', ?)", ts)
	opts := `[{"label":"Seven days","means":"Fri 7 Nov"},{"label":"One day","means":"Thu 13 Nov"}]`
	exec(`INSERT INTO questions (task_id, revision, idx, text, options_json, recommended, answer, state, asked_at, answered_at)
		VALUES (1, 1, 0, 'When?', ?, 0, 'Seven days', 'answered', ?, ?)`, opts, ts, ts)
	exec("INSERT INTO questions (task_id, revision, idx, text, state, asked_at) VALUES (1, 1, 1, 'Opt out?', 'voided', ?)", ts)
	exec("INSERT INTO questions (task_id, revision, idx, text, state, asked_at) VALUES (1, 2, 0, 'Opt out?', 'open', ?)", ts)
	exec("INSERT INTO ledger (task_id, attempt_id, role, run_id, hash, text, delivered, sent_at) VALUES (1, 't1-investigate-1', 'claude-reproducer', 'run-1', 'h1', 'Read ticket.md', 'yes', ?)", ts)
	exec("INSERT INTO decisions (task_id, kind, choice, at) VALUES (1, 'ready', 'push', ?)", ts)
	exec("INSERT INTO effects (key, kind, task_id, state, result, begun_at, ended_at) VALUES ('t1:push', 'push', 1, 'ok', 'abc123', ?, ?)", ts, ts)
	exec("INSERT INTO events (at, task_id, kind, phase, text) VALUES (?, 1, 'push', 'ok', '')", ts)
	exec("INSERT INTO events (at, kind, phase) VALUES (?, 'daemon', 'info')", ts)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTheRebuildKeepsEveryRowOfAVersion1File(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "factory.db")
	writeVersion1(t, path)
	r, err := registry.Open(path, registry.Options{})
	if err != nil {
		t.Fatalf("Open of a version 1 file: %v", err)
	}
	defer r.Close()
	ctx := t.Context()
	db := r.DBForTest()

	if v, err := r.SchemaVersion(ctx); err != nil || v != registry.LatestSchemaVersion() {
		t.Errorf("SchemaVersion = %d, %v; want %d", v, err, registry.LatestSchemaVersion())
	}
	tasks, err := r.Tasks(ctx)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("Tasks = %+v, %v; want both rows", tasks, err)
	}
	first, second := tasks[0], tasks[1]
	if first.ID != 1 || first.JiraKey != "PR-12301" || first.State != registry.TaskNeedsYou || first.Stage != "investigate" ||
		first.WaitsWho != "you" || first.Attempt != 1 || !first.Created.Equal(t0) {
		t.Errorf("first task = %+v", first)
	}
	if second.ID != 2 || second.Prompt != "a free prompt" || second.State != registry.TaskStopped || second.JiraKey != "" {
		t.Errorf("second task = %+v", second)
	}
	if first.QuestionRev != 2 || second.QuestionRev != 0 || first.CodexNever {
		t.Errorf("new columns: question revisions %d and %d, codex_never %v; want 2, 0, false", first.QuestionRev, second.QuestionRev, first.CodexNever)
	}

	// Each child row still names its task.
	for table, want := range map[string]int{"attempts": 2, "questions": 3, "ledger": 1, "decisions": 1, "effects": 1, "events": 2} {
		var n, orphans int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != want {
			t.Errorf("%s holds %d rows, %v; want %d", table, n, err, want)
		}
		q := "SELECT count(*) FROM " + table + " WHERE task_id IS NOT NULL AND task_id NOT IN (SELECT id FROM tasks)"
		if err := db.QueryRowContext(ctx, q).Scan(&orphans); err != nil || orphans != 0 {
			t.Errorf("%s has %d rows that name no task, %v", table, orphans, err)
		}
	}
	if a, err := r.AttemptByID(ctx, "t2-coding-1"); err != nil || a.TaskID != 2 {
		t.Errorf("attempt of the second task = %+v, %v", a, err)
	}
	if qs, err := r.Questions(ctx, 1); err != nil || len(qs) != 3 || qs[0].Answer != "Seven days" {
		t.Errorf("Questions(1) = %+v, %v", qs, err)
	}
	if e, err := r.EffectByKey(ctx, "t1:push"); err != nil || e.TaskID != 1 || e.Result != "abc123" {
		t.Errorf("effect = %+v, %v", e, err)
	}

	// The foreign keys still point at tasks, nothing dangles, and they are enforced.
	if n := foreignKeyViolations(t, db); n != 0 {
		t.Errorf("PRAGMA foreign_key_check reports %d violations after the rebuild", n)
	}
	for _, child := range []string{"attempts", "questions", "ledger", "decisions", "effects", "events"} {
		if parents := parentTables(t, db, child); !slices.Contains(parents, "tasks") {
			t.Errorf("%s refers to %v after the rebuild, want tasks", child, parents)
		}
	}
	var fk int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, %v after the rebuild; want 1", fk, err)
	}
	if _, err := r.BeginAttempt(ctx, 999, "coding"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("BeginAttempt for a missing task = %v, want ErrNotFound", err)
	}

	// The key rule was rebuilt too.
	if _, err := r.CreateTask(ctx, registry.Task{JiraKey: "PR-12301"}); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("a second task for the key of an old one = %v, want ErrConflict", err)
	}
	if err := r.TransitionTask(ctx, 1, nil, registry.TaskPublished, nil); err != nil {
		t.Errorf("an old task cannot be published: %v", err)
	}

	// The records that need a first row for an old task have one.
	spans, err := r.Spans(ctx, 1)
	if err != nil || len(spans) != 2 || spans[0].To != registry.TaskNeedsYou || !spans[0].OnYou || spans[0].From != "" {
		t.Errorf("Spans(1) = %+v, %v; want the migrated first span, then the publication", spans, err)
	}
	if hist, err := r.AnswerHistory(ctx, qs0ID(t, r)); err != nil || len(hist) != 1 || hist[0].Answer != "Seven days" {
		t.Errorf("AnswerHistory of an old answer = %+v, %v", hist, err)
	}
	if q := mustQuestions(t, r, 1)[0]; !q.TookRecommendation {
		t.Errorf("old answer %q matched the recommendation but TookRecommendation is false", q.Answer)
	}
	if q := mustQuestions(t, r, 1)[1]; q.VoidedBy != 2 {
		t.Errorf("old voided question VoidedBy = %d, want 2", q.VoidedBy)
	}
}

func qs0ID(t *testing.T, r *registry.Registry) int64 {
	t.Helper()
	return mustQuestions(t, r, 1)[0].ID
}

// foreignKeyViolations is the number of rows PRAGMA foreign_key_check reports.
func foreignKeyViolations(t *testing.T, db *sql.DB) int {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// parentTables lists the tables that child's foreign keys refer to.
func parentTables(t *testing.T, db *sql.DB, child string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT "table" FROM pragma_foreign_key_list(?)`, child)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var parents []string
	for rows.Next() {
		var parent string
		if err := rows.Scan(&parent); err != nil {
			t.Fatal(err)
		}
		parents = append(parents, parent)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return parents
}

func TestOpenLeavesTheFileAloneWhenBeforeMigrateRefuses(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "factory.db")
	writeVersion1(t, path)
	refusal := errors.New("a factory daemon runs the older schema")
	var from, to int
	_, err := registry.Open(path, registry.Options{BeforeMigrate: func(f, n int) error { from, to = f, n; return refusal }})
	if !errors.Is(err, refusal) {
		t.Fatalf("Open = %v, want the refusal of BeforeMigrate", err)
	}
	if from != 1 || to != registry.LatestSchemaVersion() {
		t.Errorf("BeforeMigrate(%d, %d), want (1, %d)", from, to, registry.LatestSchemaVersion())
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var v, spansTable int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 1 {
		t.Errorf("user_version = %d, %v after a refused migration; want 1", v, err)
	}
	if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('task_spans', 'tasks_new')").Scan(&spansTable); err != nil || spansTable != 0 {
		t.Errorf("a refused migration left %d new tables, %v", spansTable, err)
	}
	var def string
	if err := raw.QueryRow("SELECT sql FROM sqlite_master WHERE name = 'tasks'").Scan(&def); err != nil || strings.Contains(def, "published") {
		t.Errorf("tasks was rebuilt by a refused migration: %v", err)
	}

	// The same file opens when nobody objects, and a current file does not ask.
	asked := false
	ok, err := registry.Open(path, registry.Options{BeforeMigrate: func(int, int) error { return nil }})
	if err != nil {
		t.Fatalf("Open after the refusal: %v", err)
	}
	ok.Close()
	again, err := registry.Open(path, registry.Options{BeforeMigrate: func(int, int) error { asked = true; return refusal }})
	if err != nil || asked {
		t.Errorf("Open of a current file: err %v, BeforeMigrate asked %v; want neither", err, asked)
	}
	if again != nil {
		again.Close()
	}
}
