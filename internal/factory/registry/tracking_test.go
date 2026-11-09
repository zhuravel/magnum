package registry_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/store"
)

// The tracking table comes with the registry's migrations, on the tests'
// template as on a file an older binary left (version 1 and its tasks):
// none of those tasks has a row until the daemon reads its issue.
func TestTheTrackingMigrationAppliesOnTheTemplateAndAnOlderFile(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	var n int
	if err := r.DBForTest().QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'tracking'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("the template's tracking tables: %d, %v; want one", n, err)
	}
	path := filepath.Join(t.TempDir(), "factory.db")
	writeVersion1(t, path)
	old, err := registry.Open(path, registry.Options{})
	if err != nil {
		t.Fatalf("Open of a version 1 file: %v", err)
	}
	defer old.Close()
	if v, err := old.SchemaVersion(t.Context()); err != nil || v != registry.LatestSchemaVersion() {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, registry.LatestSchemaVersion())
	}
	if all, err := old.Trackings(t.Context()); err != nil || len(all) != 0 {
		t.Errorf("Trackings of an older file = %v, %v; want none", all, err)
	}
}

// A task's tracking row keeps the last read of its issue that worked (the
// key it is of, the status's name, id and category, since when and whether
// the changelog was searched for it, the assignee)
// and the last try, which a failure moves with its cause while the facts
// stay; a write replaces the whole row, one row per task, and a row of a
// task that does not exist is refused.
func TestATasksTrackingRowKeepsItsLastReadAndItsLastTry(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12380")
	other := createTask(t, r, "PR-12381")
	if _, err := r.Tracking(ctx, task.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Tracking before a read: %v, want ErrNotFound", err)
	}
	read := registry.Tracking{TaskID: task.ID, JiraKey: "PR-12380", Status: "In Review", StatusID: "10002", Category: "indeterminate",
		Since: t0.Add(-48 * time.Hour), Searched: true, Assigned: true, AssigneeYou: true, ReadAt: t0, TriedAt: t0}
	if err := r.SetTracking(ctx, read); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Tracking(ctx, task.ID); err != nil || !reflect.DeepEqual(got, read) {
		t.Fatalf("Tracking = %+v, %v\nwant %+v", got, err, read)
	}
	failed := read
	failed.TriedAt, failed.LastError = t0.Add(10*time.Minute), "timeout"
	if err := r.SetTracking(ctx, failed); err != nil {
		t.Fatal(err)
	}
	unread := registry.Tracking{TaskID: other.ID, JiraKey: "PR-12381", TriedAt: t0, LastError: "not_found"}
	if err := r.SetTracking(ctx, unread); err != nil {
		t.Fatal(err)
	}
	all, err := r.Trackings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int64]registry.Tracking{task.ID: failed, other.ID: unread}; !reflect.DeepEqual(all, want) {
		t.Errorf("Trackings = %+v\nwant %+v", all, want)
	}
	if err := r.SetTracking(ctx, registry.Tracking{TaskID: 999, JiraKey: "PR-1", TriedAt: t0}); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("a row of no task: %v, want ErrNotFound", err)
	}
	if err := r.SetTracking(ctx, registry.Tracking{TaskID: task.ID, JiraKey: "PR-12380"}); err == nil {
		t.Error("a row without its try's time was written")
	}
}

// A tracking row keeps who moved the issue into its status (you, someone
// else, or unknown: never a name or an id), a column of its own that an
// older file gets with its migration, unknown on its rows; any other word
// is refused.
func TestATrackingRowKeepsWhoMovedTheIssue(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12382")
	for _, by := range []string{"you", "other", ""} {
		row := registry.Tracking{TaskID: task.ID, JiraKey: "PR-12382", Status: "In Review", MovedBy: by, ReadAt: t0, TriedAt: t0}
		if err := r.SetTracking(ctx, row); err != nil {
			t.Fatalf("moved by %q: %v", by, err)
		}
		if got, err := r.Tracking(ctx, task.ID); err != nil || got.MovedBy != by {
			t.Errorf("moved by %q: read back %+v, %v", by, got, err)
		}
	}
	if err := r.SetTracking(ctx, registry.Tracking{TaskID: task.ID, JiraKey: "PR-12382", MovedBy: "Bob Example", TriedAt: t0}); err == nil {
		t.Error("a name was written as who moved the issue")
	}
	path := filepath.Join(t.TempDir(), "factory.db")
	writeVersion1(t, path)
	old, err := registry.Open(path, registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var col int
	if err := old.DBForTest().QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('tracking') WHERE name = 'moved_by'").Scan(&col); err != nil || col != 1 {
		t.Errorf("an older file's tracking has moved_by: %d, %v", col, err)
	}
}

// writeVersion7 writes the factory registry a version 7 binary left at
// path: every migration up to 0007, one published task, and its tracking
// row, which migration 0008 must keep.
func writeVersion7(t *testing.T, path string) {
	t.Helper()
	ctx := t.Context()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 7; v++ {
		if _, err := tx.ExecContext(ctx, registry.MigrationSQLForTest(v)); err != nil {
			t.Fatalf("migration %d: %v", v, err)
		}
	}
	ts, since := store.FormatTime(t0), store.FormatTime(t0.Add(-48*time.Hour))
	for _, q := range []string{
		"PRAGMA user_version = 7",
		`INSERT INTO tasks (id, jira_key, kind, repo, title, state, pr_number, created_at, updated_at)
			VALUES (1, 'PR-12383', 'bug', 'example/app', 'First', 'published', 4530, '` + ts + `', '` + ts + `')`,
		`INSERT INTO tracking (task_id, jira_key, jira_status, jira_status_id, jira_category, jira_since, since_searched, assigned,
			assignee_you, read_at, tried_at, last_error)
			VALUES (1, 'PR-12383', 'In Review', '10002', 'indeterminate', '` + since + `', 1, 1, 1, '` + ts + `', '` + ts + `', '')`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			t.Fatalf("version 7 file: %s: %v", q, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// Migration 0008 keeps the tracking rows a version 7 registry holds, every
// fact of them, and adds who moved the issue as unknown.
func TestTheMovedByMigrationKeepsAVersion7FilesTrackingRow(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "factory.db")
	writeVersion7(t, path)
	r, err := registry.Open(path, registry.Options{})
	if err != nil {
		t.Fatalf("Open of a version 7 file: %v", err)
	}
	defer r.Close()
	if v, err := r.SchemaVersion(t.Context()); err != nil || v != registry.LatestSchemaVersion() || v < 8 {
		t.Fatalf("SchemaVersion = %d, %v; want the latest (%d), 8 or later", v, err, registry.LatestSchemaVersion())
	}
	want := registry.Tracking{TaskID: 1, JiraKey: "PR-12383", Status: "In Review", StatusID: "10002", Category: "indeterminate",
		Since: t0.Add(-48 * time.Hour), Searched: true, Assigned: true, AssigneeYou: true, ReadAt: t0, TriedAt: t0}
	if got, err := r.Tracking(t.Context(), 1); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the migrated row: %+v, %v\nwant %+v (moved by unknown)", got, err, want)
	}
}
