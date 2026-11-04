package factory

import (
	"database/sql"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// magnumLayout is a layout whose magnum daemon runs (its pidfile names
// this test's process) on a registry at the newest schema.
func magnumLayout(t *testing.T) (paths.Layout, *store.Store) {
	t.Helper()
	storetest.Parallel(t)
	l := paths.Layout{Home: t.TempDir()}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, l.DB())
	if err := fsx.WriteFileAtomic(l.Pid(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return l, st
}

// userVersion is the registry's schema version, read without migrating.
func userVersion(t *testing.T, path string) int {
	t.Helper()
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The factory never migrates magnum's registry: one a version behind this
// build (magnum's daemon still runs the older build) is an error that says
// to restart magnum's daemon first, and its user_version stays as it was.
func TestMagnumIsNeverMigratedByTheFactory(t *testing.T) {
	l, st := magnumLayout(t)
	behind := store.LatestSchemaVersion() - 1
	if _, err := st.DB().Exec("PRAGMA user_version = " + strconv.Itoa(behind)); err != nil {
		t.Fatal(err)
	}
	busy, why, err := NewMagnum(l, config.Defaults()).Busy(t.Context())
	if err == nil || !strings.Contains(err.Error(), "restart magnum's daemon first") {
		t.Fatalf("Busy = %v, %q, %v; want an error that says to restart magnum's daemon first", busy, why, err)
	}
	if v := userVersion(t, l.DB()); v != behind {
		t.Fatalf("user_version %d after Busy, want %d: the factory migrated magnum's registry", v, behind)
	}
}

// magnum is busy while a review round of its runs (a PR claimed, reviewed
// or verified) or is due (a queued PR whose next try has come); a queued PR
// whose next try is later, magnum's daemon not running, or no registry at
// all is not busy, and nothing creates the registry. Why says it plainly,
// never a PR's text.
func TestMagnumIsBusyWhileARoundRunsOrIsDue(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_shop", Owner: "example", Name: "shop", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	pr := func(n int, state string) store.PR {
		up, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "PR_" + strconv.Itoa(n), Number: n,
			URL: "https://github.com/example/shop/pull/" + strconv.Itoa(n), HeadSHA: strings.Repeat("ab", 20), Title: new("Secret title"),
			GHState: store.GHOpen, InitialState: state, Identity: "zhuravel"})
		if err != nil {
			t.Fatal(err)
		}
		return up.PR
	}
	cfg := config.Defaults()
	m := NewMagnum(l, cfg)
	if busy, why, err := m.Busy(ctx); busy || err != nil {
		t.Fatalf("no PR: Busy = %v, %q, %v", busy, why, err)
	}
	queued := pr(1, store.PRQueued)
	if err := st.TransitionPR(ctx, queued.ID, []string{store.PRQueued}, store.PRQueued, func(u *store.PRUpdate) {
		u.Set("next_attempt_at", time.Now().Add(time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	if busy, why, err := m.Busy(ctx); busy || err != nil {
		t.Fatalf("a PR queued for later: Busy = %v, %q, %v", busy, why, err)
	}
	due := pr(2, store.PRQueued)
	if busy, why, err := m.Busy(ctx); !busy || err != nil || !strings.Contains(why, "due") {
		t.Fatalf("a PR due: Busy = %v, %q, %v", busy, why, err)
	}

	// Inside magnum's quiet hours its gates hold every queued round nobody
	// forced, so none is due; a forced one is.
	now := time.Now().Local()
	cfg.Daemon.QuietHours = now.Add(-time.Hour).Format("15:04") + "-" + now.Add(time.Hour).Format("15:04")
	if busy, why, err := m.Busy(ctx); busy || err != nil {
		t.Fatalf("a PR queued inside quiet hours: Busy = %v, %q, %v; want not busy", busy, why, err)
	}
	if err := st.TransitionPR(ctx, due.ID, []string{store.PRQueued}, store.PRQueued, func(u *store.PRUpdate) { u.Set("forced", true) }); err != nil {
		t.Fatal(err)
	}
	if busy, why, err := m.Busy(ctx); !busy || err != nil || !strings.Contains(why, "due") {
		t.Fatalf("a forced PR inside quiet hours: Busy = %v, %q, %v; want due", busy, why, err)
	}
	cfg.Daemon.QuietHours = ""

	// A due round that magnum's other gates hold (the GitHub floor, a slot
	// pinned, a paused agent) and that never starts holds the local review
	// only so long, then the local review goes on and says why.
	clock := time.Now()
	m.(*magnumRegistry).now = func() time.Time { return clock }
	if busy, _, _ := m.Busy(ctx); !busy {
		t.Fatal("a PR due: not busy")
	}
	clock = clock.Add(dueHold + time.Minute)
	busy, why, err := m.Busy(ctx)
	if busy || err != nil || !strings.Contains(why, "has not started for") {
		t.Fatalf("a PR due for longer than %s: Busy = %v, %q, %v; want not busy, saying so", dueHold, busy, why, err)
	}
	m.(*magnumRegistry).now = time.Now
	pr(3, store.PRReviewing)
	busy, why, err = m.Busy(ctx)
	if !busy || err != nil || !strings.Contains(why, "runs") || strings.Contains(why, "Secret") {
		t.Fatalf("a round in flight: Busy = %v, %q, %v", busy, why, err)
	}
	if err := os.Remove(l.Pid()); err != nil {
		t.Fatal(err)
	}
	if busy, why, err := m.Busy(ctx); busy || err != nil {
		t.Fatalf("no magnum daemon: Busy = %v, %q, %v", busy, why, err)
	}

	none := paths.Layout{Home: t.TempDir()}
	if err := os.MkdirAll(none.State(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteFileAtomic(none.Pid(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if busy, why, err := NewMagnum(none, cfg).Busy(ctx); busy || err != nil {
		t.Fatalf("no registry: Busy = %v, %q, %v", busy, why, err)
	}
	if fsx.Exists(none.DB()) {
		t.Fatal("Busy created magnum's registry")
	}
}
