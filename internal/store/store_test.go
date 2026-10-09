package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// clock is a settable test clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Set(t time.Time)     { c.mu.Lock(); c.now = t; c.mu.Unlock() }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// migratedImage is a database migrated once per test binary; newStore writes
// a copy of it, which Open finds current. storetest does the same for the
// other packages (this one cannot import it).
var migratedImage = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "magnum-store-test-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "magnum.db")
	st, err := Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := st.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		st.Close()
		return nil, err
	}
	if err := st.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
})

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "magnum.db")
	image, err := migratedImage()
	if err != nil {
		t.Fatalf("migrate the template: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	c := &clock{now: t0}
	st.Clock = c.Now
	return st, c
}

func mustRepo(t *testing.T, st *Store) Repo {
	t.Helper()
	r, err := st.UpsertRepo(context.Background(), Repo{
		NodeID: "R_1", Owner: "talkable", Name: "talkable", WatchOwner: "talkable",
		DefaultBranch: "master", Mode: RepoModePool,
	})
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	return r
}

func mustPR(t *testing.T, st *Store, repoID int64, number int, state string) PR {
	t.Helper()
	res, err := st.UpsertPRFromGitHub(context.Background(), GitHubPR{
		RepoID: repoID, NodeID: "PR_" + strconv.Itoa(number), Number: number,
		URL: "https://github.com/talkable/talkable/pull/" + strconv.Itoa(number), HeadSHA: "sha-" + strconv.Itoa(number),
		Title: new("PR " + strconv.Itoa(number)), GHState: GHOpen,
		InitialState: state, Identity: "talkable-app",
	})
	if err != nil {
		t.Fatalf("UpsertPRFromGitHub: %v", err)
	}
	return res.PR
}

func mustSlot(t *testing.T, st *Store, name, state string) Slot {
	t.Helper()
	s, err := st.CreateSlot(context.Background(), Slot{
		Name: name, RepoFullName: "talkable/talkable", Kind: SlotKindPool,
		Path: "/tmp/talkable." + name, MainClone: "/tmp/talkable", State: state,
		PlaceholderBranch: new(name), DBSlug: new(name),
	})
	if err != nil {
		t.Fatalf("CreateSlot: %v", err)
	}
	return s
}

func TestOpenAppliesMigrationsOnceAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "magnum.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	v, err := st.SchemaVersion(ctx)
	if err != nil || v != LatestSchemaVersion() || v < 1 {
		t.Fatalf("SchemaVersion = %d, %v; latest %d", v, err, LatestSchemaVersion())
	}
	if _, err := st.UpsertRepo(ctx, Repo{NodeID: "R_1", Owner: "o", Name: "n", WatchOwner: "o", Mode: RepoModePerPR}); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open: migrations must not run again (they would fail on CREATE TABLE).
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer st2.Close()
	v2, _ := st2.SchemaVersion(ctx)
	if v2 != v {
		t.Fatalf("version changed on reopen: %d -> %d", v, v2)
	}
	if _, err := st2.RepoByFullName(ctx, "o/n"); err != nil {
		t.Fatalf("data lost on reopen: %v", err)
	}
}

func TestOpenPragmas(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	var mode string
	if err := st.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	var fk, busy, sync int
	st.DB().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk)
	st.DB().QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy)
	st.DB().QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync)
	if fk != 1 || busy != 5000 || sync != 1 {
		t.Fatalf("foreign_keys=%d busy_timeout=%d synchronous=%d", fk, busy, sync)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a schema newer than this binary knows")
	}
}

// TestBeforeMigrateRefusalLeavesTheFileAlone: BeforeMigrate sees the file's
// version and this binary's; its error fails OpenWith before any migration
// runs, and a current schema never calls it.
func TestBeforeMigrateRefusalLeavesTheFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	var from, to int
	refuse := func(f, t int) error { from, to = f, t; return errors.New("daemon running") }
	if _, err := OpenWith(path, Options{BeforeMigrate: refuse}); err == nil || !strings.Contains(err.Error(), "daemon running") {
		t.Fatalf("OpenWith = %v, want the refusal", err)
	}
	if from != 0 || to != LatestSchemaVersion() {
		t.Fatalf("BeforeMigrate(%d, %d), want (0, %d)", from, to, LatestSchemaVersion())
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var v, tables int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if v != 0 || tables != 0 {
		t.Fatalf("refused migration left user_version %d and %d tables", v, tables)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	called := false
	st, err = OpenWith(path, Options{BeforeMigrate: func(int, int) error { called = true; return errors.New("no") }})
	if err != nil || called {
		t.Fatalf("current schema: err %v, BeforeMigrate called %v", err, called)
	}
	st.Close()
}

// TestOpenFilesArePrivate: the database and its WAL and shared-memory files
// hold the same data, so all three are 0600, on a fresh database and on a
// pre-existing one that was group/world readable.
func TestOpenFilesArePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "magnum.db")
	check := func(when string) {
		t.Helper()
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatalf("%s: %v", when, err)
			}
			if perm := fi.Mode().Perm(); perm != 0o600 {
				t.Errorf("%s: %s is %o, want 600", when, filepath.Base(p), perm)
			}
		}
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	check("fresh database")
	st.Close()

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	check("re-opened database that was 0644")
}

// TestTxRollsBackOnPanic: a panic inside a transaction body that the caller
// recovers must not strand the store's only connection.
func TestTxRollsBackOnPanic(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic was swallowed")
			}
		}()
		_ = st.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO kv (key, value, updated_at) VALUES ('k', 'v', 'now')"); err != nil {
				t.Fatal(err)
			}
			panic("boom")
		})
	}()

	// The connection is free again (with the old code this call waits forever)
	// and the half-done insert was rolled back.
	short, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, ok, err := st.GetKV(short, "k"); err != nil || ok {
		t.Fatalf("after the panic: GetKV = %v, %v; want a free connection and no row", ok, err)
	}
	if err := st.SetKV(short, "k", "v"); err != nil {
		t.Fatalf("write after the panic: %v", err)
	}
}

func TestTimeFormatSortsChronologically(t *testing.T) {
	a := FormatTime(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC))
	b := FormatTime(time.Date(2026, 1, 1, 10, 0, 0, 500_000_000, time.UTC))
	c := FormatTime(time.Date(2026, 1, 1, 11, 0, 0, 0, time.FixedZone("x", 3600))) // == 10:00 UTC
	if !(a < b) {
		t.Fatalf("%s should sort before %s", a, b)
	}
	if c != a {
		t.Fatalf("non-UTC input not normalized: %s vs %s", c, a)
	}
	got, err := ParseTime(b)
	if err != nil || !got.Equal(time.Date(2026, 1, 1, 10, 0, 0, 500_000_000, time.UTC)) {
		t.Fatalf("ParseTime(%s) = %v, %v", b, got, err)
	}
	if _, err := ParseTime("2026-01-01T10:00:00Z"); err != nil {
		t.Fatalf("plain RFC3339 should parse: %v", err)
	}
}

func TestUpsertRepoKeepsFirstSyncedAt(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	r := mustRepo(t, st)
	if r.FirstSyncedAt != nil || !r.LastSeenAt.Equal(t0) {
		t.Fatalf("unexpected repo: %+v", r)
	}
	clk.Add(time.Minute)
	first := clk.Now()
	r.FirstSyncedAt = &first
	r.ClonePath = new("/Users/x/Projects/talkable")
	r2, err := st.UpsertRepo(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if r2.ID != r.ID || r2.FirstSyncedAt == nil || !r2.FirstSyncedAt.Equal(first) || Deref(r2.ClonePath) == "" {
		t.Fatalf("upsert did not update: %+v", r2)
	}
	clk.Add(time.Minute)
	later := clk.Now()
	r2.FirstSyncedAt = &later
	r3, _ := st.UpsertRepo(ctx, r2)
	if !r3.FirstSyncedAt.Equal(first) {
		t.Fatalf("first_synced_at overwritten: %v", r3.FirstSyncedAt)
	}
	if !r3.LastSeenAt.Equal(later) {
		t.Fatalf("last_seen_at not bumped: %v", r3.LastSeenAt)
	}
	byName, err := st.RepoByFullName(ctx, "Talkable/TALKABLE")
	if err != nil || byName.ID != r.ID {
		t.Fatalf("RepoByFullName case-insensitive: %+v %v", byName, err)
	}
	if _, err := st.RepoByFullName(ctx, "nope/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing repo err = %v", err)
	}
}

func TestUpsertPRFromGitHubChangeDetection(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	in := GitHubPR{
		RepoID: repo.ID, NodeID: "PR_1", Number: 1, URL: "u1", HeadSHA: "aaa",
		Title: new("first"), AuthorLogin: new("alice"), Labels: []string{"x"}, GHState: GHOpen,
		InitialState: PRBaseline, Identity: "zhuravel",
	}
	res, err := st.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.New || res.HeadChanged || res.PR.State != PRBaseline || res.PR.HeadSHA != "aaa" || !res.PR.HeadChangedAt.Equal(t0) {
		t.Fatalf("insert result: %+v", res)
	}
	if len(res.PR.Labels) != 1 || res.PR.Labels[0] != "x" || Deref(res.PR.Title) != "first" {
		t.Fatalf("insert fields: %+v", res.PR)
	}

	// Same data: nothing changes, updated_at stays.
	clk.Add(time.Minute)
	res2, err := st.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res2.New || res2.HeadChanged || res2.Changed || !res2.PR.UpdatedAt.Equal(t0) {
		t.Fatalf("no-op upsert reported change: %+v", res2)
	}

	// Radar-only refresh (no title) keeps the title; draft flip is a change, head is not.
	radar := GitHubPR{RepoID: repo.ID, NodeID: "PR_1", Number: 1, URL: "u1", HeadSHA: "aaa", IsDraft: true, InitialState: PRQueued, Identity: "other"}
	res3, err := st.UpsertPRFromGitHub(ctx, radar)
	if err != nil {
		t.Fatal(err)
	}
	if res3.New || res3.HeadChanged || !res3.Changed || !res3.PR.IsDraft || Deref(res3.PR.Title) != "first" || len(res3.PR.Labels) != 1 {
		t.Fatalf("radar upsert: %+v", res3)
	}
	if res3.PR.State != PRBaseline || res3.PR.Identity != "other" {
		t.Fatalf("upsert must keep the state and let an unreviewed PR follow the watch identity: %+v", res3.PR)
	}

	// New head.
	clk.Add(time.Minute)
	in.HeadSHA = "bbb"
	res4, err := st.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !res4.HeadChanged || !res4.Changed || res4.PR.HeadSHA != "bbb" || !res4.PR.HeadChangedAt.Equal(clk.Now()) {
		t.Fatalf("head change: %+v", res4)
	}
	byNum, err := st.PRByRepoNumber(ctx, repo.ID, 1)
	if err != nil || byNum.ID != res.PR.ID || byNum.HeadSHA != "bbb" {
		t.Fatalf("PRByRepoNumber: %+v %v", byNum, err)
	}
	if _, err := st.PRByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PRByID missing: %v", err)
	}
	if _, err := st.UpsertPRFromGitHub(ctx, GitHubPR{RepoID: repo.ID, NodeID: "PR_2", Number: 2, URL: "u", HeadSHA: "c"}); err == nil {
		t.Fatal("insert without InitialState/Identity must fail")
	}
}

func TestUpsertPRFromGitHubReviewRequested(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	base := GitHubPR{
		RepoID: repo.ID, NodeID: "PR_1", Number: 1, URL: "u1", HeadSHA: "aaa", GHState: GHOpen,
		InitialState: PRQueued, Identity: "zhuravel",
	}

	// nil on insert: the column default (false).
	res, err := st.UpsertPRFromGitHub(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if res.PR.ReviewRequested {
		t.Fatalf("nil ReviewRequested inserted as true: %+v", res.PR)
	}

	// false -> true is a change; true is persisted and round-trips through reads.
	clk.Add(time.Minute)
	in := base
	in.ReviewRequested = new(true)
	res, err = st.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.New || res.HeadChanged || !res.Changed || !res.PR.ReviewRequested {
		t.Fatalf("review_requested false->true: %+v", res)
	}
	got, err := st.PRByID(ctx, res.PR.ID)
	if err != nil || !got.ReviewRequested {
		t.Fatalf("PRByID after upsert: %+v %v", got, err)
	}

	// Same value again is a no-op.
	clk.Add(time.Minute)
	updatedAt := got.UpdatedAt
	res, err = st.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || !res.PR.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("no-op review_requested upsert reported change: %+v", res)
	}

	// nil keeps the stored value.
	res, err = st.UpsertPRFromGitHub(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || !res.PR.ReviewRequested {
		t.Fatalf("nil must keep review_requested=true: %+v", res)
	}

	// true -> false clears it.
	clk.Add(time.Minute)
	in.ReviewRequested = new(false)
	res, err = st.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.PR.ReviewRequested {
		t.Fatalf("review_requested true->false: %+v", res)
	}

	// New PR inserted with true.
	n := base
	n.NodeID, n.Number, n.URL, n.ReviewRequested = "PR_2", 2, "u2", new(true)
	res, err = st.UpsertPRFromGitHub(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	if !res.New || !res.PR.ReviewRequested {
		t.Fatalf("insert with ReviewRequested=true: %+v", res)
	}

	// The automation-side whitelist accepts the column.
	setPR(t, st, res.PR.ID, func(u *PRUpdate) { u.Set("review_requested", false) })
	got, err = st.PRByID(ctx, res.PR.ID)
	if err != nil || got.ReviewRequested {
		t.Fatalf("UpdatePR review_requested=false: %+v %v", got, err)
	}
}

func TestTransitionPRCompareAndSet(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 10, PRQueued)

	clk.Add(time.Second)
	err := st.TransitionPR(ctx, pr.ID, []string{PRQueued, PRRereviewPending}, PRClaiming, func(u *PRUpdate) {
		u.Set("last_error", "boom")
		u.Set("next_attempt_at", clk.Now().Add(time.Minute))
		u.Inc("attempts", 1)
		u.Copy("prev_state", "state")
	})
	if err != nil {
		t.Fatalf("TransitionPR: %v", err)
	}
	got, _ := st.PRByID(ctx, pr.ID)
	if got.State != PRClaiming || Deref(got.LastError) != "boom" || got.Attempts != 1 || Deref(got.PrevState) != PRQueued {
		t.Fatalf("after transition: %+v", got)
	}
	if got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(clk.Now().Add(time.Minute)) || !got.UpdatedAt.Equal(clk.Now()) {
		t.Fatalf("timestamps: %+v", got)
	}

	// Second identical transition loses the race.
	err = st.TransitionPR(ctx, pr.ID, []string{PRQueued}, PRClaiming, nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	if err := st.TransitionPR(ctx, 4242, []string{PRQueued}, PRClaiming, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// Unknown columns and direct state writes are rejected.
	if err := st.UpdatePR(ctx, pr.ID, func(u *PRUpdate) { u.Set("no_such_col", 1) }); err == nil {
		t.Fatal("unknown column accepted")
	}
	if err := st.UpdatePR(ctx, pr.ID, func(u *PRUpdate) { u.Set("state", PRQueued) }); err == nil {
		t.Fatal("state written through Set")
	}
	// Clearing a nullable column.
	if err := st.UpdatePR(ctx, pr.ID, func(u *PRUpdate) { u.Set("last_error", nil); u.Set("pinned", true) }); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PRByID(ctx, pr.ID)
	if got.LastError != nil || !got.Pinned || got.State != PRClaiming {
		t.Fatalf("UpdatePR: %+v", got)
	}
	// Invalid target state hits the CHECK constraint.
	if err := st.TransitionPR(ctx, pr.ID, nil, "bogus", nil); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("bogus state: %v", err)
	}
}

// TestTransitionGuardedByWhere: Update.Where adds a column condition to the
// same UPDATE as the state compare-and-set; a failed condition is ErrConflict
// and changes nothing.
func TestTransitionGuardedByWhere(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 10, PRQueued) // head_sha "sha-10"

	// The head moved on since the caller decided: nothing is written.
	err := st.TransitionPR(ctx, pr.ID, []string{PRQueued}, PRClaiming, func(u *PRUpdate) {
		u.Where("head_sha", "sha-old")
		u.Set("last_error", "must not be written")
	})
	if !errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
		t.Fatalf("stale head_sha: %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "head_sha") {
		t.Fatalf("the error should name the guarded column: %v", err)
	}
	got, _ := st.PRByID(ctx, pr.ID)
	if got.State != PRQueued || got.LastError != nil || !got.UpdatedAt.Equal(pr.UpdatedAt) {
		t.Fatalf("row changed despite the failed guard: %+v", got)
	}

	// The state still has to match too, whatever the guard says.
	err = st.TransitionPR(ctx, pr.ID, []string{PRReviewing}, PRClaiming, func(u *PRUpdate) { u.Where("head_sha", "sha-10") })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong state with a matching guard: %v, want ErrConflict", err)
	}

	// Matching head and state: the transition goes through.
	err = st.TransitionPR(ctx, pr.ID, []string{PRQueued}, PRClaiming, func(u *PRUpdate) {
		u.Where("head_sha", "sha-10")
		u.Set("last_error", "ok")
	})
	if err != nil {
		t.Fatalf("matching head_sha: %v", err)
	}
	got, _ = st.PRByID(ctx, pr.ID)
	if got.State != PRClaiming || Deref(got.LastError) != "ok" {
		t.Fatalf("after the guarded transition: %+v", got)
	}

	// Several conditions combine with AND; nil means IS NULL; a missing row
	// is still ErrNotFound; works on updates without a state change.
	err = st.UpdatePR(ctx, pr.ID, func(u *PRUpdate) {
		u.Where("head_sha", "sha-10")
		u.Where("skip_reason", nil)
		u.Set("pinned", true)
	})
	if err != nil {
		t.Fatalf("head_sha and skip_reason IS NULL: %v", err)
	}
	err = st.UpdatePR(ctx, pr.ID, func(u *PRUpdate) { u.Where("last_error", nil); u.Set("pinned", false) })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("last_error is set, IS NULL guard: %v, want ErrConflict", err)
	}
	if got, _ := st.PRByID(ctx, pr.ID); !got.Pinned {
		t.Fatal("failed guard still wrote")
	}
	if err := st.TransitionPR(ctx, 4242, []string{PRQueued}, PRClaiming, func(u *PRUpdate) { u.Where("head_sha", "x") }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing row: %v, want ErrNotFound", err)
	}

	// Column names are checked against the table, never interpolated blindly.
	for _, col := range []string{"no_such_col", "head_sha = 'x' OR 1", "id; DROP TABLE prs"} {
		err := st.UpdatePR(ctx, pr.ID, func(u *PRUpdate) { u.Where(col, "x") })
		if err == nil || errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			t.Fatalf("Where(%q) = %v, want a column error", col, err)
		}
	}
	if err := st.UpdatePR(ctx, pr.ID, func(u *PRUpdate) { u.Where("head_sha", struct{}{}) }); err == nil {
		t.Fatal("unsupported value type accepted")
	}

	// Slots, sessions and runs share the builder.
	sl := mustSlot(t, st, "review1", SlotFree)
	if err := st.TransitionSlot(ctx, sl.ID, []string{SlotFree}, SlotHeld, func(u *SlotUpdate) { u.Where("name", "other") }); !errors.Is(err, ErrConflict) {
		t.Fatalf("slot guard: %v, want ErrConflict", err)
	}
	if err := st.TransitionSlot(ctx, sl.ID, []string{SlotFree}, SlotHeld, func(u *SlotUpdate) { u.Where("name", "review1") }); err != nil {
		t.Fatalf("slot guard: %v", err)
	}
}

func TestClaimSlotIsExclusive(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr1 := mustPR(t, st, repo.ID, 1, PRQueued)
	pr2 := mustPR(t, st, repo.ID, 2, PRQueued)
	s1 := mustSlot(t, st, "review1", SlotFree)
	s2 := mustSlot(t, st, "review2", SlotFree)

	a, err := st.ClaimSlot(ctx, pr1.ID, s1.ID, "talkable_development__review1")
	if err != nil {
		t.Fatalf("ClaimSlot: %v", err)
	}
	if a.SlotID != s1.ID || a.PRID != pr1.ID || a.Path != s1.Path || Deref(a.HeadSHA) != pr1.HeadSHA || len(a.DBNames) != 1 {
		t.Fatalf("assignment: %+v", a)
	}
	gotSlot, _ := st.SlotByName(ctx, "review1")
	gotPR, _ := st.PRByID(ctx, pr1.ID)
	if gotSlot.State != SlotClaimed || Deref(gotSlot.PRID) != pr1.ID || gotSlot.LastUsedAt == nil || gotPR.State != PRClaiming {
		t.Fatalf("after claim slot=%+v pr=%+v", gotSlot, gotPR)
	}

	// Same slot, other PR: conflict, and pr2 is untouched.
	if _, err := st.ClaimSlot(ctx, pr2.ID, s1.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("second claim of slot: %v", err)
	}
	if p, _ := st.PRByID(ctx, pr2.ID); p.State != PRQueued {
		t.Fatalf("pr2 changed by failed claim: %s", p.State)
	}
	// Same PR, other slot: conflict, and slot 2 stays free.
	if _, err := st.ClaimSlot(ctx, pr1.ID, s2.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("second claim for PR: %v", err)
	}
	if s, _ := st.SlotByName(ctx, "review2"); s.State != SlotFree || s.PRID != nil {
		t.Fatalf("slot2 changed by failed claim: %+v", s)
	}
	open, err := st.OpenAssignmentBySlot(ctx, s1.ID)
	if err != nil || open.ID != a.ID {
		t.Fatalf("OpenAssignmentBySlot: %+v %v", open, err)
	}

	// Release: closes the assignment, clears pr_id, frees the slot; pr2 can now claim it.
	if err := st.ReleaseSlot(ctx, s1.ID, []string{SlotClaimed, SlotBusy, SlotHeld, SlotReleasing}, SlotFree, "released"); err != nil {
		t.Fatalf("ReleaseSlot: %v", err)
	}
	if _, err := openAssignmentOf(ctx, st, pr1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("assignment still open: %v", err)
	}
	if _, err := st.ClaimSlot(ctx, pr2.ID, s1.ID); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	hist, err := st.AssignmentsByPR(ctx, pr1.ID)
	if err != nil || len(hist) != 1 || hist[0].EndedAt == nil || Deref(hist[0].EndReason) != "released" {
		t.Fatalf("history: %+v %v", hist, err)
	}
}

func TestClaimSlotConcurrentSingleWinner(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	slot := mustSlot(t, st, "review1", SlotFree)
	var prs []PR
	for i := 1; i <= 8; i++ {
		prs = append(prs, mustPR(t, st, repo.ID, i, PRQueued))
	}
	var wg sync.WaitGroup
	errs := make([]error, len(prs))
	for i, p := range prs {
		wg.Go(func() { _, errs[i] = st.ClaimSlot(ctx, p.ID, slot.ID) })
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrConflict):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d", wins)
	}
}

func TestTransitionSlotAndUpdateFields(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	s := mustSlot(t, st, "review1", SlotProvisioning)
	if err := st.TransitionSlot(ctx, s.ID, []string{SlotFree}, SlotRemoving, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict: %v", err)
	}
	if err := st.TransitionSlot(ctx, s.ID, []string{SlotProvisioning}, SlotFree, func(u *SlotUpdate) { u.Set("checked_out_sha", "abc") }); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSlotFields(ctx, s.ID, func(u *SlotUpdate) { u.Set("pinned", true); u.Set("hold_reason", "manual") }); err != nil {
		t.Fatal(err)
	}
	got, _ := st.SlotByID(ctx, s.ID)
	if got.State != SlotFree || Deref(got.CheckedOutSHA) != "abc" || !got.Pinned || Deref(got.HoldReason) != "manual" {
		t.Fatalf("slot: %+v", got)
	}
	all, err := st.ListSlots(ctx, SlotFilter{RepoFullName: "talkable/talkable"})
	if err != nil || len(all) != 1 {
		t.Fatalf("ListSlots: %v %v", all, err)
	}
	none, _ := st.ListSlots(ctx, SlotFilter{States: []string{SlotBroken}})
	if len(none) != 0 {
		t.Fatalf("state filter: %v", none)
	}
}

func setPR(t *testing.T, st *Store, id int64, f func(u *PRUpdate)) {
	t.Helper()
	if err := st.UpdatePR(context.Background(), id, f); err != nil {
		t.Fatal(err)
	}
}

func setSlot(t *testing.T, st *Store, id int64, f func(u *SlotUpdate)) {
	t.Helper()
	if err := st.UpdateSlotFields(context.Background(), id, f); err != nil {
		t.Fatal(err)
	}
}

func setSession(t *testing.T, st *Store, id int64, f func(u *SessionUpdate)) {
	t.Helper()
	if err := st.UpdateSession(context.Background(), id, f); err != nil {
		t.Fatal(err)
	}
}

func TestCandidatesOrderingAndGates(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	now := t0.Add(10 * time.Hour)

	old := mustPR(t, st, repo.ID, 1, PRQueued)
	setPR(t, st, old.ID, func(u *PRUpdate) { u.Set("gh_updated_at", t0.Add(1*time.Hour)) })
	newer := mustPR(t, st, repo.ID, 2, PRQueued)
	setPR(t, st, newer.ID, func(u *PRUpdate) { u.Set("gh_updated_at", t0.Add(2*time.Hour)) })
	forced := mustPR(t, st, repo.ID, 3, PRQueued)
	setPR(t, st, forced.ID, func(u *PRUpdate) { u.Set("gh_updated_at", t0.Add(9*time.Hour)); u.Set("forced", true) })

	// rereview_pending, all gates passed.
	due := mustPR(t, st, repo.ID, 4, PRRereviewPending)
	setPR(t, st, due.ID, func(u *PRUpdate) {
		u.Set("gh_updated_at", t0.Add(3*time.Hour))
		u.Set("pending_since", now.Add(-10*time.Minute))
		u.Set("last_round_started_at", now.Add(-time.Hour))
		u.Set("next_eligible_at", now)
	})
	// next_eligible_at in the future.
	notYet := mustPR(t, st, repo.ID, 5, PRRereviewPending)
	setPR(t, st, notYet.ID, func(u *PRUpdate) { u.Set("next_eligible_at", now.Add(time.Minute)) })
	// still inside the push quiet period.
	quiet := mustPR(t, st, repo.ID, 6, PRRereviewPending)
	setPR(t, st, quiet.ID, func(u *PRUpdate) { u.Set("pending_since", now.Add(-time.Minute)) })
	// draft reviewed 1h ago (draft interval is 2h).
	draft := mustPR(t, st, repo.ID, 7, PRRereviewPending)
	setPR(t, st, draft.ID, func(u *PRUpdate) { u.Set("is_draft", true); u.Set("last_round_started_at", now.Add(-time.Hour)) })
	// daily cap reached today; a cap from yesterday does not count.
	capped := mustPR(t, st, repo.ID, 8, PRRereviewPending)
	setPR(t, st, capped.ID, func(u *PRUpdate) { u.Set("rounds_today", 6); u.Set("rounds_day", DayKey(now)) })
	yesterday := mustPR(t, st, repo.ID, 9, PRRereviewPending)
	setPR(t, st, yesterday.ID, func(u *PRUpdate) {
		u.Set("rounds_today", 6)
		u.Set("rounds_day", DayKey(now.Add(-24*time.Hour)))
		u.Set("gh_updated_at", t0.Add(4*time.Hour))
	})
	// muted, and muted+forced; backoff pending.
	muted := mustPR(t, st, repo.ID, 10, PRQueued)
	setPR(t, st, muted.ID, func(u *PRUpdate) { u.Set("muted", true) })
	mutedForced := mustPR(t, st, repo.ID, 11, PRQueued)
	setPR(t, st, mutedForced.ID, func(u *PRUpdate) {
		u.Set("muted", true)
		u.Set("forced", true)
		u.Set("gh_updated_at", t0.Add(8*time.Hour))
	})
	backoff := mustPR(t, st, repo.ID, 12, PRQueued)
	setPR(t, st, backoff.ID, func(u *PRUpdate) { u.Set("next_attempt_at", now.Add(time.Minute)) })
	// forced rereview bypasses throttles.
	forcedRe := mustPR(t, st, repo.ID, 13, PRRereviewPending)
	setPR(t, st, forcedRe.ID, func(u *PRUpdate) {
		u.Set("forced", true)
		u.Set("next_eligible_at", now.Add(time.Hour))
		u.Set("gh_updated_at", t0.Add(7*time.Hour))
	})
	// other states never qualify.
	mustPR(t, st, repo.ID, 14, PRReviewed)
	mustPR(t, st, repo.ID, 15, PRBaseline)
	// review_requested sorts after forced but before plain PRs regardless of
	// age; between two requested PRs the older activity wins.
	requestedNew := mustPR(t, st, repo.ID, 16, PRQueued)
	setPR(t, st, requestedNew.ID, func(u *PRUpdate) {
		u.Set("gh_updated_at", t0.Add(6*time.Hour))
		u.Set("review_requested", true)
	})
	requestedOld := mustPR(t, st, repo.ID, 17, PRQueued)
	setPR(t, st, requestedOld.ID, func(u *PRUpdate) {
		u.Set("gh_updated_at", t0.Add(5*time.Hour))
		u.Set("review_requested", true)
	})
	// forced + review_requested: forced still outranks the requested ones, and
	// among forced PRs review_requested comes first.
	forcedRequested := mustPR(t, st, repo.ID, 18, PRQueued)
	setPR(t, st, forcedRequested.ID, func(u *PRUpdate) {
		u.Set("gh_updated_at", t0.Add(9*time.Hour+30*time.Minute))
		u.Set("forced", true)
		u.Set("review_requested", true)
	})
	clk.Set(now)

	got, err := st.Candidates(ctx, CandidateParams{
		Now: now, QuietPeriod: 5 * time.Minute, MinInterval: 30 * time.Minute,
		DraftMinInterval: 2 * time.Hour, MaxRoundsPerDay: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, p := range got {
		nums = append(nums, p.Number)
	}
	want := []int{18, 13, 11, 3, 17, 16, 1, 2, 4, 9}
	if len(nums) != len(want) {
		t.Fatalf("candidates = %v, want %v", nums, want)
	}
	for i := range want {
		if nums[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", nums, want)
		}
	}
}

func TestFreeAndEvictableSlots(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 1, PRQueued)

	a := mustSlot(t, st, "review1", SlotFree)
	b := mustSlot(t, st, "review2", SlotFree)
	c := mustSlot(t, st, "review3", SlotFree)
	pinned := mustSlot(t, st, "review4", SlotFree)
	setSlot(t, st, pinned.ID, func(u *SlotUpdate) { u.Set("pinned", true) })
	held := mustSlot(t, st, "review5", SlotFree)
	setSlot(t, st, held.ID, func(u *SlotUpdate) { u.Set("hold_reason", "human") })
	setSlot(t, st, a.ID, func(u *SlotUpdate) { u.Set("last_used_at", t0.Add(-1*time.Hour)) })
	setSlot(t, st, b.ID, func(u *SlotUpdate) { u.Set("last_used_at", t0.Add(-3*time.Hour)) })
	setSlot(t, st, c.ID, func(u *SlotUpdate) { u.Set("last_used_at", t0.Add(-2*time.Hour)) })

	names := func(ss []Slot) []string {
		var out []string
		for _, s := range ss {
			out = append(out, s.Name)
		}
		return out
	}
	free, err := st.FreeSlots(ctx, "talkable/talkable", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(free); len(got) != 3 || got[0] != "review2" || got[1] != "review3" || got[2] != "review1" {
		t.Fatalf("LRU order = %v", got)
	}
	// A slot that held this PR before comes first.
	if _, err := st.OpenAssignment(ctx, Assignment{PRID: pr.ID, SlotID: a.ID, Path: a.Path}); err != nil {
		t.Fatal(err)
	}
	asg, _ := openAssignmentOf(ctx, st, pr.ID)
	closeAssignment(t, st, asg.ID, "test")
	free, _ = st.FreeSlots(ctx, "talkable/talkable", pr.ID)
	if got := names(free); got[0] != "review1" {
		t.Fatalf("preferred slot not first: %v", got)
	}

	// Evictable: held slot idle beyond min_warm, no active runs, no busy sessions.
	if _, err := st.ClaimSlot(ctx, pr.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionSlot(ctx, b.ID, []string{SlotClaimed}, SlotHeld, nil); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Hour)
	ev, err := st.EvictableSlots(ctx, "talkable/talkable", clk.Now(), 30*time.Minute)
	if err != nil || len(ev) != 1 || ev[0].ID != b.ID {
		t.Fatalf("EvictableSlots = %v, %v", names(ev), err)
	}
	// An active run blocks eviction.
	run, err := st.CreateRun(ctx, Run{PRID: pr.ID, Round: 1, Role: RoleJudge, Kind: RunInitial, TargetSHA: pr.HeadSHA,
		Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: RunPending, PromptText: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if ev, _ := st.EvictableSlots(ctx, "talkable/talkable", clk.Now(), 30*time.Minute); len(ev) != 0 {
		t.Fatalf("slot with active run evictable: %v", names(ev))
	}
	if err := st.TransitionRun(ctx, run.ID, []string{RunPending}, RunAbandoned, nil); err != nil {
		t.Fatal(err)
	}
	// A recently prompted live session blocks eviction.
	sess, err := st.CreateSession(ctx, Session{PRID: pr.ID, Role: RoleClaude, State: SessionLive})
	if err != nil {
		t.Fatal(err)
	}
	setSession(t, st, sess.ID, func(u *SessionUpdate) { u.Set("last_prompt_at", clk.Now().Add(-5*time.Minute)) })
	if ev, _ := st.EvictableSlots(ctx, "talkable/talkable", clk.Now(), 30*time.Minute); len(ev) != 0 {
		t.Fatalf("slot with warm session evictable: %v", names(ev))
	}
	clk.Add(time.Hour)
	if ev, _ := st.EvictableSlots(ctx, "talkable/talkable", clk.Now(), 30*time.Minute); len(ev) != 1 {
		t.Fatalf("idle slot not evictable: %v", names(ev))
	}
}

func TestClosedPastGrace(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	due := mustPR(t, st, repo.ID, 1, PRClosed)
	setPR(t, st, due.ID, func(u *PRUpdate) { u.Set("release_after", t0.Add(-time.Minute)) })
	notYet := mustPR(t, st, repo.ID, 2, PRClosed)
	setPR(t, st, notYet.ID, func(u *PRUpdate) { u.Set("release_after", t0.Add(time.Minute)) })
	busy := mustPR(t, st, repo.ID, 3, PRClosed)
	setPR(t, st, busy.ID, func(u *PRUpdate) { u.Set("release_after", t0.Add(-time.Minute)) })
	if _, err := st.CreateRun(ctx, Run{PRID: busy.ID, Round: 1, Role: RoleJudge, Kind: RunInitial, TargetSHA: "x",
		Identity: "i", ReviewerLogin: "l", State: RunWorking, PromptText: "p"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ClosedPastGrace(ctx, t0)
	if err != nil || len(got) != 1 || got[0].ID != due.ID {
		t.Fatalf("ClosedPastGrace = %+v, %v", got, err)
	}
}

func TestSessionsAndRuns(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 1, PRReviewing)

	s1, err := st.CreateSession(ctx, Session{PRID: pr.ID, Role: RoleJudge, AgentName: new("mg-talkable-1-judge"), State: SessionStarting,
		Env: map[string]string{"WT_BRANCH": "review1"}})
	if err != nil {
		t.Fatal(err)
	}
	if s1.Generation != 1 || s1.Env["WT_BRANCH"] != "review1" || !s1.StartedAt.Equal(t0) {
		t.Fatalf("session: %+v", s1)
	}
	if _, err := st.CreateSession(ctx, Session{PRID: pr.ID, Role: RoleJudge, State: SessionLive}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second live judge: %v", err)
	}
	live, err := st.LiveSessionByPRRole(ctx, pr.ID, RoleJudge)
	if err != nil || live.ID != s1.ID {
		t.Fatalf("LiveSessionByPRRole: %+v %v", live, err)
	}
	if err := st.TransitionSession(ctx, s1.ID, []string{SessionStarting, SessionLive}, SessionParked, func(u *SessionUpdate) {
		u.Set("session_id", "uuid-1")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LiveSessionByPRRole(ctx, pr.ID, RoleJudge); !errors.Is(err, ErrNotFound) {
		t.Fatalf("parked session still live: %v", err)
	}
	s2, err := st.CreateSession(ctx, Session{PRID: pr.ID, Role: RoleJudge, State: SessionLive, ResumedFrom: new("uuid-1")})
	if err != nil || s2.Generation != 2 {
		t.Fatalf("resumed session: %+v %v", s2, err)
	}
	ls, err := st.LiveSessions(ctx)
	if err != nil || len(ls) != 1 || ls[0].ID != s2.ID {
		t.Fatalf("LiveSessions: %+v %v", ls, err)
	}

	r1, err := st.CreateRun(ctx, Run{PRID: pr.ID, Round: 1, Role: RoleJudge, SessionID: &s2.ID, Kind: RunInitial,
		TargetSHA: pr.HeadSHA, Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: RunPending, PromptText: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if r1.ID == "" || !r1.CreatedAt.Equal(t0) {
		t.Fatalf("run: %+v", r1)
	}
	r2, err := st.CreateRun(ctx, Run{PRID: pr.ID, Round: 1, Role: RoleClaude, Kind: RunInitial,
		TargetSHA: pr.HeadSHA, Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: RunPending, PromptText: "go"})
	if err != nil || r2.ID == r1.ID {
		t.Fatalf("second run id: %q vs %q, %v", r2.ID, r1.ID, err)
	}
	clk.Add(time.Minute)
	if err := st.TransitionRun(ctx, r1.ID, []string{RunPending}, RunSubmitted, func(u *RunUpdate) { u.Set("submitted_at", clk.Now()) }); err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionRun(ctx, r1.ID, []string{RunPending}, RunSubmitted, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("run CAS: %v", err)
	}
	if err := st.TransitionRun(ctx, r2.ID, nil, RunVerified, func(u *RunUpdate) { u.Set("review_id", int64(77)) }); err != nil {
		t.Fatal(err)
	}
	active, err := st.ActiveRuns(ctx)
	if err != nil || len(active) != 1 || active[0].ID != r1.ID || active[0].SubmittedAt == nil {
		t.Fatalf("ActiveRuns: %+v %v", active, err)
	}
	got, err := st.RunByID(ctx, r2.ID)
	if err != nil || Deref(got.ReviewID) != 77 || got.State != RunVerified {
		t.Fatalf("RunByID: %+v %v", got, err)
	}
}

func TestSlotDatabases(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	slot := mustSlot(t, st, "review1", SlotFree)
	if _, err := st.UpsertSlotDatabase(ctx, SlotDatabase{SlotID: &slot.ID, DBName: "talkable_development__review1", Slug: "review1", SizeMB: new(12.5)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertSlotDatabase(ctx, SlotDatabase{DBName: "talkable_test__orphan", Slug: "orphan"}); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	d, err := st.UpsertSlotDatabase(ctx, SlotDatabase{SlotID: &slot.ID, DBName: "talkable_development__review1", Slug: "review1", SizeMB: new(13.0)})
	if err != nil || !d.FirstSeenAt.Equal(t0) || !d.LastSeenAt.Equal(clk.Now()) || Deref(d.SizeMB) != 13.0 {
		t.Fatalf("re-upsert: %+v %v", d, err)
	}
	if err := st.MarkSlotDatabaseDropped(ctx, "talkable_test__orphan", "cleanup"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkSlotDatabaseDropped(ctx, "nope", "cleanup"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drop missing: %v", err)
	}
	live, _ := st.ListSlotDatabases(ctx, false)
	all, _ := st.ListSlotDatabases(ctx, true)
	if len(live) != 1 || len(all) != 2 {
		t.Fatalf("live=%d all=%d", len(live), len(all))
	}
	// Seen again after a drop: it exists again.
	d2, _ := st.UpsertSlotDatabase(ctx, SlotDatabase{DBName: "talkable_test__orphan", Slug: "orphan"})
	if d2.DroppedAt != nil {
		t.Fatalf("re-seen db still dropped: %+v", d2)
	}
}

func TestRequestsQueue(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	if q, err := st.PendingRequests(ctx, 0); err != nil || len(q) != 0 {
		t.Fatalf("empty queue: %v %v", q, err)
	}
	id1, err := st.EnqueueRequest(ctx, "review", map[string]any{"pr": 1})
	if err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Second)
	id2, _ := st.EnqueueRequest(ctx, "kick", nil)
	q, err := st.PendingRequests(ctx, 1)
	if err != nil || len(q) != 1 || q[0].ID != id1 || q[0].Kind != "review" || string(q[0].Payload) != `{"pr":1}` {
		t.Fatalf("next: %+v %v", q, err)
	}
	if err := st.CompleteRequest(ctx, id1, RequestDone, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteRequest(ctx, id1, RequestFailed, "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("double complete: %v", err)
	}
	if q, _ = st.PendingRequests(ctx, 0); len(q) != 1 || q[0].ID != id2 || string(q[0].Payload) != "{}" {
		t.Fatalf("second: %+v", q)
	}
	got, _ := st.RequestByID(ctx, id1)
	if got.State != RequestDone || Deref(got.Result) != "ok" || got.HandledAt == nil {
		t.Fatalf("RequestByID: %+v", got)
	}
}

func TestEventsKVNotifications(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	for i, m := range []string{"one", "two", "three"} {
		if _, err := st.AppendEvent(ctx, Event{Level: "info", Subject: new("pr:1"), Kind: "poll", Message: m, Data: json.RawMessage(`{"i":` + strconv.Itoa(i) + `}`)}); err != nil {
			t.Fatal(err)
		}
		clk.Add(time.Second)
	}
	if _, err := st.AppendEvent(ctx, Event{Level: "info", Subject: new("pr:2"), Kind: "poll", Message: "other"}); err != nil {
		t.Fatal(err)
	}
	evs, err := st.EventsBySubject(ctx, "pr:1", 2)
	if err != nil || len(evs) != 2 || evs[0].Message != "two" || evs[1].Message != "three" || string(evs[1].Data) != `{"i":2}` {
		t.Fatalf("EventsBySubject: %+v %v", evs, err)
	}
	if _, err := st.AppendEvent(ctx, Event{Kind: "x"}); err == nil {
		t.Fatal("event without message accepted")
	}

	if _, ok, err := st.GetKV(ctx, "daemon.paused_until"); ok || err != nil {
		t.Fatalf("missing kv: %v %v", ok, err)
	}
	for _, v := range []string{"a", "b"} {
		if err := st.SetKV(ctx, "daemon.paused_until", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok, _ := st.GetKV(ctx, "daemon.paused_until"); !ok || v != "b" {
		t.Fatalf("kv = %q %v", v, ok)
	}
	if err := st.DeleteKV(ctx, "daemon.paused_until"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetKV(ctx, "daemon.paused_until"); ok {
		t.Fatal("kv not deleted")
	}

	send, err := st.ShouldSend(ctx, "attention:pr:1", 10*time.Minute)
	if err != nil || !send {
		t.Fatalf("first send: %v %v", send, err)
	}
	clk.Add(time.Minute)
	if send, _ := st.ShouldSend(ctx, "attention:pr:1", 10*time.Minute); send {
		t.Fatal("deduped notification sent")
	}
	clk.Add(10 * time.Minute)
	if send, _ := st.ShouldSend(ctx, "attention:pr:1", 10*time.Minute); !send {
		t.Fatal("notification after window suppressed")
	}
}

func TestListPRsFilter(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	mustPR(t, st, repo.ID, 1, PRQueued)
	mustPR(t, st, repo.ID, 2, PRReviewed)
	mustPR(t, st, repo.ID, 3, PRQueued)
	got, err := st.ListPRs(ctx, PRFilter{RepoID: repo.ID, States: []string{PRQueued}})
	if err != nil || len(got) != 2 {
		t.Fatalf("ListPRs: %v %v", got, err)
	}
	all, _ := st.ListPRs(ctx, PRFilter{})
	nums := []int{}
	for _, p := range all {
		nums = append(nums, p.Number)
	}
	if !sort.IntsAreSorted(nums) || len(nums) != 3 {
		t.Fatalf("all = %v", nums)
	}
}

// Two handles on one file stand in for the daemon and the CLI: concurrent
// claims across them must serialize (BEGIN IMMEDIATE + busy_timeout) with
// exactly one winner and no SQLITE_BUSY errors.
func TestClaimSlotAcrossHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	repo := mustRepo(t, a)
	slot := mustSlot(t, a, "review1", SlotFree)
	var prs []PR
	for i := 1; i <= 10; i++ {
		prs = append(prs, mustPR(t, a, repo.ID, i, PRQueued))
	}
	var wg sync.WaitGroup
	errs := make([]error, len(prs))
	for i, p := range prs {
		h := a
		if i%2 == 1 {
			h = b
		}
		wg.Go(func() { _, errs[i] = h.ClaimSlot(ctx, p.ID, slot.ID) })
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d", wins)
	}
}

// A PR follows its watch's identity only until a review is posted or a
// forced round pinned one with `magnum review --as`.
func TestUpsertPRIdentityFollowsTheWatchUntilReviewed(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	base := GitHubPR{RepoID: repo.ID, NodeID: "PR_9", Number: 9, URL: "u9", HeadSHA: "aaa", InitialState: PRBaseline, Identity: "zhuravel"}
	if _, err := st.UpsertPRFromGitHub(ctx, base); err != nil {
		t.Fatal(err)
	}
	base.Identity = "zhuravel-app"
	res, err := st.UpsertPRFromGitHub(ctx, base)
	if err != nil || res.PR.Identity != "zhuravel-app" || !res.Changed {
		t.Fatalf("unreviewed PR must follow the watch identity: %+v %v", res.PR, err)
	}
	if err := st.UpdatePR(ctx, res.PR.ID, func(u *PRUpdate) { u.Set("forced", true); u.Set("identity", "me") }); err != nil {
		t.Fatal(err)
	}
	if res, err = st.UpsertPRFromGitHub(ctx, base); err != nil || res.PR.Identity != "me" {
		t.Fatalf("a forced PR keeps the identity magnum review --as chose: %+v %v", res.PR, err)
	}
	if err := st.UpdatePR(ctx, res.PR.ID, func(u *PRUpdate) { u.Set("forced", false); u.Set("reviewed_sha", "aaa") }); err != nil {
		t.Fatal(err)
	}
	if res, err = st.UpsertPRFromGitHub(ctx, base); err != nil || res.PR.Identity != "me" {
		t.Fatalf("a reviewed PR keeps the login that owns its review thread: %+v %v", res.PR, err)
	}
}

func TestKVScreenWidths(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	key := KVScreenWidths("board")
	if key != "tui.board.widths" {
		t.Fatalf("KVScreenWidths(board) = %q", key)
	}
	if got := KVScreenWidths("dashboard"); got != "tui.dashboard.widths" {
		t.Fatalf("KVScreenWidths(dashboard) = %q", got)
	}
	if _, ok, err := st.GetKV(ctx, key); ok || err != nil {
		t.Fatalf("widths before any drag: ok=%v err=%v", ok, err)
	}
	want := map[string]int{"title": 42, "author": 14}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, key, string(raw)); err != nil {
		t.Fatal(err)
	}
	v, ok, err := st.GetKV(ctx, key)
	if err != nil || !ok {
		t.Fatalf("GetKV = %q %v %v", v, ok, err)
	}
	var got map[string]int
	if err := json.Unmarshal([]byte(v), &got); err != nil || len(got) != 2 || got["title"] != 42 || got["author"] != 14 {
		t.Fatalf("round trip = %v (%v)", got, err)
	}
	if _, ok, _ := st.GetKV(ctx, KVScreenWidths("dashboard")); ok {
		t.Fatal("board widths leaked into the dashboard key")
	}
	if err := st.DeleteKV(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetKV(ctx, key); ok {
		t.Fatal("widths not deleted")
	}
}
