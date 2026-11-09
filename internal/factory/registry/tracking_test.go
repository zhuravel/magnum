package registry_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
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
// key it is of, the status's name and category, since when, the assignee)
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
	read := registry.Tracking{TaskID: task.ID, JiraKey: "PR-12380", Status: "In Review", Category: "indeterminate",
		Since: t0.Add(-48 * time.Hour), Assigned: true, AssigneeYou: true, ReadAt: t0, TriedAt: t0}
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
