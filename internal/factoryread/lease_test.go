package factoryread

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/sqlitetest"
	"github.com/zhuravel/magnum/internal/store"
)

// workRow is a pr_work row of PR number of repo in phase, at cycle, with
// the lease's holder until until ("" and the zero time: none) and the
// times of the flip, the import and the first work ("" for none).
func workRow(repo string, number int, phase string, cycle int, holder string, until time.Time, flipped, imported, worked string) string {
	q := func(s string) string {
		if s == "" {
			return "NULL"
		}
		return "'" + s + "'"
	}
	u := ""
	if !until.IsZero() {
		u = store.FormatTime(until)
	}
	return `INSERT INTO pr_work (repo, number, phase, phase_at, head, cycle, cap, lease_holder, lease_until, flipped_at, imported_at, worked_at,
		created_at, updated_at) VALUES ('` + repo + `', ` + strconv.Itoa(number) + `, '` + phase + `', '2026-11-13T09:00:00Z', 'c3c3c3c', ` +
		strconv.Itoa(cycle) + `, 3, '` + holder + `', ` + q(u) + `, ` + q(flipped) + `, ` + q(imported) + `, ` + q(worked) +
		`, '2026-11-13T08:00:00Z', '2026-11-13T09:00:00Z')`
}

// The factory fixes no PR it has no row of: its lease is the zero Lease and
// no error, as for a registry that does not exist (no factory ever ran on
// this Mac), so magnum reviews it as before.
func TestTheLeaseOfAPRWithNoRowIsZero(t *testing.T) {
	path := fixture(t, workRow("talkable/talkable", 900011, "fixing", 1, "factory 100", time.Now().Add(time.Hour), "", "", ""))
	for _, pr := range []struct {
		repo   string
		number int
	}{{"talkable/talkable", 900012}, {"talkable/other", 900011}} {
		got, err := LeaseOf(context.Background(), path, pr.repo, pr.number)
		if err != nil || got != (Lease{}) {
			t.Errorf("LeaseOf(%s#%d) = %+v, %v; want the zero Lease and no error", pr.repo, pr.number, got, err)
		}
	}
	missing := filepath.Join(t.TempDir(), "factory", "factory.db")
	if got, err := LeaseOf(context.Background(), missing, "talkable/talkable", 900011); err != nil || got != (Lease{}) {
		t.Errorf("LeaseOf on no registry = %+v, %v; want the zero Lease and no error", got, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("LeaseOf created the registry: %v", err)
	}
	if jobs, err := FixJobs(context.Background(), missing); err != nil || len(jobs) != 0 {
		t.Errorf("FixJobs on no registry = %+v, %v; want none", jobs, err)
	}
}

// A factory older than the PR work table (its registry at schema 8) fixes
// nothing: the lease of any PR is the zero Lease and no error, it has no
// fix loop at work, and the read leaves its file as it was.
func TestAnOlderFactoryRegistryGivesAZeroLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factory", "factory.db")
	ms, err := store.LoadMigrations(os.DirFS(filepath.Join("..", "factory", "registry")), "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) < 9 || ms[8].Name != "0009_pr_work.sql" {
		t.Fatalf("migration 9 is %v, want 0009_pr_work.sql", ms[min(8, len(ms)-1)].Name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path+"?"+sqlitetest.FastFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ApplyMigrations(context.Background(), db, ms[:8], nil); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := LeaseOf(context.Background(), path, "talkable/talkable", 900011)
	if err != nil || got != (Lease{}) {
		t.Fatalf("LeaseOf on a registry at schema 8 = %+v, %v; want the zero Lease and no error", got, err)
	}
	if jobs, err := FixJobs(context.Background(), path); err != nil || len(jobs) != 0 {
		t.Fatalf("FixJobs on a registry at schema 8 = %+v, %v; want none", jobs, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the read changed the older factory's registry")
	}
}

// The lease says whether the factory fixes the PR now: its loop is in a
// phase that holds an agent or a push under a lease that runs. A lease that
// ran out (a factory that stopped while it worked) and a loop whose push
// magnum re-reviews hold nothing; the phase, the head, the cycle and
// whether the factory worked the PR since its import read as the row has
// them.
func TestTheLeaseSaysWhetherTheFactoryFixesThePRNow(t *testing.T) {
	now := time.Now().UTC()
	path := fixture(t,
		workRow("talkable/talkable", 1, "fixing", 2, "factory 100", now.Add(10*time.Minute), "2026-11-13T07:00:00Z", "2026-11-13T07:00:01Z",
			"2026-11-13T07:30:00Z"),
		workRow("talkable/talkable", 2, "fixing", 1, "factory 100", now.Add(-time.Minute), "2026-11-13T07:00:00Z", "2026-11-13T07:00:01Z", ""),
		workRow("talkable/talkable", 3, "reviewing", 1, "factory 100", now.Add(10*time.Minute), "", "", ""),
		workRow("talkable/talkable", 4, "idle", 0, "", time.Time{}, "2026-11-13T07:00:00Z", "2026-11-13T07:00:01Z", ""))
	for number, want := range map[int]struct {
		fixing, worked bool
		phase          string
		cycle          int
	}{1: {true, true, "fixing", 2}, 2: {false, false, "fixing", 1}, 3: {false, false, "reviewing", 1}, 4: {false, false, "idle", 0}} {
		got, err := LeaseOf(context.Background(), path, "Talkable/Talkable", number)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID == 0 || got.Fixing != want.fixing || got.Worked != want.worked || got.Phase != want.phase || got.Cycle != want.cycle ||
			got.Cap != 3 || (want.phase != "idle" && got.Head != "c3c3c3c") {
			t.Errorf("LeaseOf(#%d) = %+v, want fixing %v, worked %v, phase %s, cycle %d", number, got, want.fixing, want.worked, want.phase, want.cycle)
		}
	}
	got, _ := LeaseOf(context.Background(), path, "talkable/talkable", 1)
	if got.Holder != "factory 100" || got.FlippedAt.IsZero() || got.ImportedAt.IsZero() || got.WorkedAt.IsZero() || got.Until.IsZero() {
		t.Errorf("LeaseOf(#1) = %+v, want its holder, its end and the times of the flip, the import and the first work", got)
	}
	jobs, err := FixJobs(context.Background(), path)
	if err != nil || len(jobs) != 2 || jobs[0].Number != 1 || jobs[1].Number != 2 || jobs[0].Since.IsZero() {
		t.Fatalf("FixJobs = %+v, %v; want the two loops in phase fixing, by row (the reviewing one holds no agent)", jobs, err)
	}
}

// Each phase of a PR's fix work, under a lease that runs: the factory fixes
// the PR now (Lease.Fixing), and its loop is a fix job install waits for
// (FixJobs), only in the phases that hold an agent or a push, fixing,
// local_review and pushing. A loop that is queued, waits for magnum's
// re-review or waits for you holds neither.
func TestTheLeaseAndTheFixJobsCountOnlyAPhaseWithAnAgentOrAPush(t *testing.T) {
	until := time.Now().Add(10 * time.Minute)
	phases := []struct {
		phase string
		busy  bool
	}{{"idle", false}, {"queued", false}, {"fixing", true}, {"local_review", true}, {"pushing", true}, {"reviewing", false}, {"held", false}}
	var rows []string
	for i, p := range phases {
		cycle := 1
		if p.phase == "idle" {
			cycle = 0
		}
		rows = append(rows, workRow("talkable/talkable", i+1, p.phase, cycle, "factory 100", until, "", "", ""))
	}
	path := fixture(t, rows...)
	jobs, err := FixJobs(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range phases {
		l, err := LeaseOf(context.Background(), path, "talkable/talkable", i+1)
		if err != nil {
			t.Fatal(err)
		}
		if l.Phase != p.phase || l.Fixing != p.busy {
			t.Errorf("phase %s: the lease reads %s, fixing %v; want fixing %v", p.phase, l.Phase, l.Fixing, p.busy)
		}
		listed := slices.ContainsFunc(jobs, func(j FixJob) bool { return j.Number == i+1 && j.Phase == p.phase })
		if listed != p.busy {
			t.Errorf("phase %s: FixJobs lists it %v, want %v", p.phase, listed, p.busy)
		}
	}
}

// A file that is no registry is an error, never a zero Lease: the caller
// cannot tell, and decides.
func TestALeaseThatCannotBeReadIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factory.db")
	if err := os.WriteFile(path, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LeaseOf(context.Background(), path, "talkable/talkable", 1); err == nil {
		t.Fatalf("LeaseOf on a file that is no registry = %+v, want an error", got)
	}
	if _, err := FixJobs(context.Background(), path); err == nil {
		t.Fatal("FixJobs on a file that is no registry, want an error")
	}
}
