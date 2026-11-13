package factory

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
	"github.com/zhuravel/magnum/internal/tui"
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
	st := storetest.OpenFile(t, l.DB())
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

// The local review is one more round under magnum's limit
// (max_concurrent_reviews): it yields only while magnum's rounds in flight
// and due fill that limit, so a busy day with free rounds does not hold it.
func TestTheLocalReviewTakesAFreeRoundUnderMagnumsLimit(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_shop", Owner: "example", Name: "shop", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	pr := func(n int, state string) store.PR {
		up, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "PR_" + strconv.Itoa(n), Number: n,
			URL: "https://github.com/example/shop/pull/" + strconv.Itoa(n), HeadSHA: strings.Repeat("ab", 20),
			GHState: store.GHOpen, InitialState: state, Identity: "zhuravel"})
		if err != nil {
			t.Fatal(err)
		}
		return up.PR
	}
	cfg := config.Defaults()
	cfg.Daemon.MaxConcurrentReviews = 3
	m := NewMagnum(l, cfg)
	pr(1, store.PRReviewing)
	pr(2, store.PRReviewing)
	if busy, why, err := m.Busy(ctx); busy || err != nil {
		t.Fatalf("2 rounds in flight under a limit of 3: Busy = %v, %q, %v; want not busy", busy, why, err)
	}
	due := pr(3, store.PRQueued)
	if busy, why, err := m.Busy(ctx); !busy || err != nil || !strings.Contains(why, "limit of 3") {
		t.Fatalf("2 in flight and 1 due under a limit of 3: Busy = %v, %q, %v; want busy at the limit", busy, why, err)
	}
	if err := st.TransitionPR(ctx, due.ID, []string{store.PRQueued}, store.PRReviewing, nil); err != nil {
		t.Fatal(err)
	}
	if busy, why, err := m.Busy(ctx); !busy || err != nil || !strings.Contains(why, "3 review rounds") {
		t.Fatalf("3 rounds in flight under a limit of 3: Busy = %v, %q, %v; want busy, naming them", busy, why, err)
	}
}

// At a limit of one round, magnum is busy while a review round of its runs
// (a PR claimed, reviewed or verified) or is due (a queued PR whose next try
// has come); a queued PR whose next try is later, magnum's daemon not
// running, or no registry at all is not busy, and nothing creates the
// registry. Why says it plainly, never a PR's text.
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
	cfg.Daemon.MaxConcurrentReviews = 1
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

// magnumPR is PR #n of example/shop in magnum's registry, open at head and
// opened by zhuravel, as edit shapes it.
func magnumPR(t *testing.T, st *store.Store, n int, head string, edit func(*store.GitHubPR)) store.PR {
	t.Helper()
	ctx := t.Context()
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_shop", Owner: "example", Name: "shop", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	g := store.GitHubPR{RepoID: repo.ID, NodeID: "PR_" + strconv.Itoa(n), Number: n, URL: "https://github.com/example/shop/pull/" + strconv.Itoa(n),
		HeadSHA: head, GHState: store.GHOpen, InitialState: store.PRQueued, Identity: "zhuravel", AuthorLogin: new("zhuravel")}
	if edit != nil {
		edit(&g)
	}
	up, err := st.UpsertPRFromGitHub(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	return up.PR
}

// setKV stores value (JSON for a struct) under key in magnum's registry.
func setKV(t *testing.T, st *store.Store, key string, value any) {
	t.Helper()
	s, ok := value.(string)
	if !ok {
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		s = string(b)
	}
	if err := st.SetKV(t.Context(), key, s); err != nil {
		t.Fatal(err)
	}
}

// openWatchConfig is the defaults with a watch of example's repositories
// under autofix = "open".
func openWatchConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Watches = []config.Watch{{Owner: "example", Include: []string{"*"}, Identity: "zhuravel", Autofix: config.AutofixOpen}}
	return cfg
}

// The factory reads magnum's registry for its PRs and never migrates it:
// one a migration behind this build (magnum's daemon still runs the build
// before) or one ahead of it (a newer magnum) is an error, and the file is
// byte for byte as it was.
func TestPRStateRefusesAMagnumRegistryOfAnotherSchema(t *testing.T) {
	for name, delta := range map[string]int{"behind": -1, "ahead": 1} {
		t.Run(name, func(t *testing.T) {
			l, st := magnumLayout(t)
			v := store.LatestSchemaVersion() + delta
			if _, err := st.DB().Exec("PRAGMA user_version = " + strconv.Itoa(v)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				t.Fatal(err)
			}
			st.Close()
			before, err := os.ReadFile(l.DB())
			if err != nil {
				t.Fatal(err)
			}
			views, err := NewMagnum(l, openWatchConfig()).PRStates(t.Context(), []PRRef{{Repo: "example/shop", Number: 1}})
			if err == nil {
				t.Fatalf("PRStates on a registry at schema %d = %+v, want an error", v, views)
			}
			after, err := os.ReadFile(l.DB())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("PRStates changed magnum's registry")
			}
			if got := userVersion(t, l.DB()); got != v {
				t.Fatalf("user_version %d after PRStates, want %d", got, v)
			}
		})
	}
}

// PRStates reads, per PR, magnum's row (its GitHub state, head, review and
// author), the open verdict, the fix loop and the stalemate, and the
// attention a PR needs in magnum's words; a PR magnum does not have, in a
// repository it has or not, is not found, and whether a watch covers it
// says if it ever will be.
func TestPRStatesReadsTheRowTheVerdictTheLoopAndTheStalemate(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	now := time.Now()
	head := strings.Repeat("ab", 20)
	fixing := magnumPR(t, st, 7, head, func(g *store.GitHubPR) {
		g.ReviewGate = &store.ReviewGate{Decision: "APPROVED"}
	})
	magnumPR(t, st, 8, head, func(g *store.GitHubPR) {
		g.GHState, g.MergedAt, g.ClosedAt = store.GHMerged, new(now.Add(-time.Hour)), new(now.Add(-time.Hour))
	})
	attn := magnumPR(t, st, 9, head, nil)
	if err := st.TransitionPR(ctx, attn.ID, nil, store.PRNeedsAttention, func(u *store.PRUpdate) {
		u.Set("last_error", "judge reported a blocker: the slot's database is gone")
	}); err != nil {
		t.Fatal(err)
	}
	setKV(t, st, engine.KVPRAttention(attn.ID), "blocked")
	setKV(t, st, store.KVDaemonLastTick, store.FormatTime(now))
	verdict := engine.OpenVerdict{Verdict: engine.OpenFixing, Why: "fixing, cycle 2 of 5", Head: head, At: now.Add(time.Minute)}
	setKV(t, st, engine.KVPROpen(fixing.ID), verdict)
	setKV(t, st, engine.KVPRFix(fixing.ID), engine.FixState{Cycle: 2, Cap: 5, Phase: "fixing", Head: head})
	setKV(t, st, engine.KVPRStalemate(fixing.ID), engine.Stalemate{Threads: []engine.StalemateThread{{ID: "T1", URL: "https://github.com/example/shop/pull/7#r1"}}})
	setKV(t, st, engine.KVPRFixHeld(attn.ID), map[string]any{"at": now, "head": head, "by": "autofix", "why": "github_budget"})

	refs := []PRRef{{"example/shop", 7}, {"example/shop", 8}, {"example/shop", 9}, {"example/shop", 10}, {"other/site", 1}}
	views, err := NewMagnum(l, openWatchConfig()).PRStates(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != len(refs) {
		t.Fatalf("%d views for %d refs: %+v", len(views), len(refs), views)
	}
	v := views[refs[0]]
	switch {
	case !v.Found || !v.DaemonUp || !v.Watched || v.Ref != refs[0]:
		t.Errorf("#7: found %v, daemon up %v, watched %v, ref %+v", v.Found, v.DaemonUp, v.Watched, v.Ref)
	case v.GHState != store.GHOpen || v.Head != head || v.AuthorLogin != "zhuravel" || v.Draft || !v.Approved:
		t.Errorf("#7's row: %+v", v)
	case !v.HasOpen || v.Open.Verdict != engine.OpenFixing || v.Open.Why != verdict.Why || v.Stale:
		t.Errorf("#7's verdict %+v (has %v, stale %v), want %+v, fresh", v.Open, v.HasOpen, v.Stale, verdict)
	case !v.Fixing || v.Fix.Cycle != 2 || v.Fix.Cap != 5:
		t.Errorf("#7's loop: fixing %v, %+v", v.Fixing, v.Fix)
	case !v.Stalemate:
		t.Error("#7's stalemate not read")
	case v.FixHeld != "":
		t.Errorf("#7 holds no fix, yet reads %q", v.FixHeld)
	}
	if v := views[refs[1]]; !v.Found || v.GHState != store.GHMerged || v.MergedAt == nil || v.HasOpen || v.Fixing {
		t.Errorf("#8, merged: %+v", v)
	}
	if v := views[refs[2]]; v.State != store.PRNeedsAttention || v.Attention != "blocked" || !strings.Contains(v.AttentionMsg, "the slot's database is gone") {
		t.Errorf("#9, needs attention: state %q, attention %q, %q", v.State, v.Attention, v.AttentionMsg)
	} else if v.FixHeld != "when the GitHub budget recovers" {
		t.Errorf("#9's held fix reads %q, want when it starts", v.FixHeld)
	}
	if v := views[refs[3]]; v.Found || !v.Watched || !v.DaemonUp {
		t.Errorf("#10, not in magnum's registry: %+v", v)
	}
	if v := views[refs[4]]; v.Found || v.Watched {
		t.Errorf("other/site#1, in no watch: %+v", v)
	}
}

// With magnum's daemon down (its pidfile names no live process) every
// view says so, and without its registry nothing is found and nothing is
// created.
func TestPRStatesWithoutMagnumsDaemonSaysSo(t *testing.T) {
	l, st := magnumLayout(t)
	head := strings.Repeat("cd", 20)
	pr := magnumPR(t, st, 7, head, nil)
	setKV(t, st, store.KVDaemonLastTick, store.FormatTime(time.Now()))
	setKV(t, st, engine.KVPROpen(pr.ID), engine.OpenVerdict{Verdict: engine.OpenIdle, Why: "nothing open", Head: head, At: time.Now()})
	if err := os.Remove(l.Pid()); err != nil {
		t.Fatal(err)
	}
	ref := PRRef{"example/shop", 7}
	views, err := NewMagnum(l, openWatchConfig()).PRStates(t.Context(), []PRRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if v := views[ref]; v.DaemonUp || !v.Found || !v.Stale {
		t.Errorf("magnum's daemon down: %+v, want found, the daemon down and its verdict stale", v)
	}

	none := paths.Layout{Home: t.TempDir()}
	if err := os.MkdirAll(none.State(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteFileAtomic(none.Pid(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	views, err = NewMagnum(none, openWatchConfig()).PRStates(t.Context(), []PRRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if v := views[ref]; v.Found || !v.DaemonUp {
		t.Errorf("no registry: %+v, want not found", v)
	}
	if fsx.Exists(none.DB()) {
		t.Fatal("PRStates created magnum's registry")
	}
}

// A verdict decided at an older head, before the PR's row last changed, or
// with magnum's last tick long ago no longer tells the PR's state: it is
// stale, which the screen shows as magnum looking, never as idle.
func TestAnOpenRowOfAnOlderHeadIsStale(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	now := time.Now()
	head, older := strings.Repeat("ef", 20), strings.Repeat("01", 20)
	pr := magnumPR(t, st, 7, head, nil)
	ref := PRRef{"example/shop", 7}
	m := NewMagnum(l, openWatchConfig())
	read := func() PRState {
		t.Helper()
		views, err := m.PRStates(ctx, []PRRef{ref})
		if err != nil {
			t.Fatal(err)
		}
		return views[ref]
	}
	setKV(t, st, store.KVDaemonLastTick, store.FormatTime(now))
	setKV(t, st, engine.KVPROpen(pr.ID), engine.OpenVerdict{Verdict: engine.OpenIdle, Why: "nothing open", Head: head, At: now.Add(time.Minute)})
	if v := read(); !v.HasOpen || v.Stale {
		t.Fatalf("a verdict at the PR's head after its row changed: %+v, want fresh", v)
	}
	setKV(t, st, engine.KVPROpen(pr.ID), engine.OpenVerdict{Verdict: engine.OpenIdle, Why: "nothing open", Head: older, At: now.Add(time.Minute)})
	if v := read(); !v.HasOpen || !v.Stale {
		t.Errorf("a verdict of an older head: %+v, want stale", v)
	}
	setKV(t, st, engine.KVPROpen(pr.ID), engine.OpenVerdict{Verdict: engine.OpenIdle, Why: "nothing open", Head: head, At: pr.UpdatedAt.Add(-time.Minute)})
	if v := read(); !v.Stale {
		t.Errorf("a verdict decided before the PR's row changed: %+v, want stale", v)
	}
	setKV(t, st, engine.KVPROpen(pr.ID), engine.OpenVerdict{Verdict: engine.OpenIdle, Why: "nothing open", Head: head, At: now.Add(time.Minute)})
	setKV(t, st, store.KVDaemonLastTick, store.FormatTime(now.Add(-time.Hour)))
	if v := read(); !v.Stale {
		t.Errorf("a verdict with magnum's last tick an hour ago: %+v, want stale", v)
	}
	setKV(t, st, engine.KVPROpen(pr.ID), "not a verdict")
	setKV(t, st, store.KVDaemonLastTick, store.FormatTime(now))
	if v := read(); v.HasOpen {
		t.Errorf("a verdict that does not parse: %+v, want none", v)
	}
}

// fixStart records a fix.start event of magnum's on PR #n of example/shop:
// cycle of the loop, started by by.
func fixStart(t *testing.T, st *store.Store, n, cycle int, by string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"cycle": cycle, "by": by, "role": "claude-fix"})
	if err != nil {
		t.Fatal(err)
	}
	subject := "pr:example/shop#" + strconv.Itoa(n)
	if _, err := st.AppendEvent(t.Context(), store.Event{Subject: &subject, Kind: "fix.start", Message: "claude-fix fixes", Data: data}); err != nil {
		t.Fatal(err)
	}
}

// FixLoopsOf counts, per PR, the fix loops magnum started (a fix.start of
// cycle 1; a later cycle is the same loop), by hand (magnum fix, f on the
// board) apart from autofix's, over the whole history magnum keeps; a PR
// with none counts zero, and magnum's registry is only read.
func TestFixLoopsCountsTheLoopsOfAPRByWhoStartedThem(t *testing.T) {
	l, st := magnumLayout(t)
	magnumPR(t, st, 7, strings.Repeat("ab", 20), nil)
	magnumPR(t, st, 8, strings.Repeat("ab", 20), nil)
	fixStart(t, st, 7, 1, "magnum fix")
	fixStart(t, st, 7, 2, "magnum fix")
	for range 250 { // a busy PR: more events than one page of the board's reads
		subject := "pr:example/shop#7"
		if _, err := st.AppendEvent(t.Context(), store.Event{Subject: &subject, Kind: "round.start", Message: "a round"}); err != nil {
			t.Fatal(err)
		}
	}
	fixStart(t, st, 7, 1, engine.AutofixBy)
	fixStart(t, st, 7, 2, engine.AutofixBy)
	fixStart(t, st, 7, 1, engine.AutofixBy)
	fixStart(t, st, 8, 1, "magnum fix")
	seven, eight, nine, other := PRRef{"example/shop", 7}, PRRef{"example/shop", 8}, PRRef{"example/shop", 9}, PRRef{"other/site", 1}
	got, err := FixLoopsOf(t.Context(), l, []PRRef{seven, eight, nine, other})
	if err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[PRRef]FixLoops{seven: {ByHand: 1, ByAutofix: 2}, eight: {ByHand: 1}, nine: {}, other: {}} {
		if got[ref] != want {
			t.Errorf("%s#%d: %+v, want %+v", ref.Repo, ref.Number, got[ref], want)
		}
	}
}

// FixLoopsOf counts the fixers' pushes on a PR too, and the commits they
// pushed (an event that names no count is one commit): the fix-loop commits
// after the publish that `magnum factory stats` shows.
func TestFixLoopsCountTheFixersPushesAndCommits(t *testing.T) {
	l, st := magnumLayout(t)
	magnumPR(t, st, 7, strings.Repeat("ab", 20), nil)
	fixStart(t, st, 7, 1, engine.AutofixBy)
	fixEvent(t, st, 7, "fix.pushed", map[string]any{"cycle": 1, "commits": 2})
	fixStart(t, st, 7, 2, engine.AutofixBy)
	fixEvent(t, st, 7, "fix.pushed", map[string]any{"cycle": 2, "commits": 1})
	fixEvent(t, st, 7, "fix.pushed", map[string]any{"cycle": 2})
	fixEvent(t, st, 7, "fix.replied", map[string]any{"cycle": 2, "commits": 9})
	seven := PRRef{"example/shop", 7}
	got, err := FixLoopsOf(t.Context(), l, []PRRef{seven})
	if err != nil {
		t.Fatal(err)
	}
	if want := (FixLoops{ByAutofix: 1, Pushes: 3, Commits: 4}); got[seven] != want {
		t.Errorf("%+v, want %+v", got[seven], want)
	}
}

// fixEvent records a fix event of magnum's of kind on PR #n of
// example/shop, with data.
func fixEvent(t *testing.T, st *store.Store, n int, kind string, data map[string]any) {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	subject := "pr:example/shop#" + strconv.Itoa(n)
	if _, err := st.AppendEvent(t.Context(), store.Event{Subject: &subject, Kind: kind, Message: kind + " of the fix loop", Data: b}); err != nil {
		t.Fatal(err)
	}
}

// FixRecords folds, per PR, magnum's fix events into counts: the loops
// started, the newest start's cycle, the fixers' pushes, the threads they
// replied in, and whether the newest loop's last word left the PR to you.
// The same Magnum reads only the events written since its last call; a PR
// or a repository magnum does not have is the zero record, and magnum's
// registry is only read.
func TestFixRecordsFoldTheFixEventsOfAPR(t *testing.T) {
	l, st := magnumLayout(t)
	magnumPR(t, st, 7, strings.Repeat("ab", 20), nil)
	magnumPR(t, st, 8, strings.Repeat("ab", 20), nil)
	fixStart(t, st, 7, 1, "magnum fix")
	fixEvent(t, st, 7, "fix.pushed", map[string]any{"cycle": 1, "commits": 2})
	fixEvent(t, st, 7, "fix.replied", map[string]any{"cycle": 1, "threads": 3})
	fixEvent(t, st, 7, "round.start", map[string]any{"threads": 40})
	fixStart(t, st, 7, 2, "magnum fix")
	fixEvent(t, st, 7, "fix.pushed", map[string]any{"cycle": 2, "commits": 1})
	fixEvent(t, st, 7, "fix.replied", map[string]any{"cycle": 2}) // a reply without a count of its threads
	fixEvent(t, st, 7, "fix.needs_you", map[string]any{"cycle": 2, "needs_you": 1})
	fixEvent(t, st, 8, "fix.replied", map[string]any{"threads": 9})
	m := NewMagnum(l, openWatchConfig())
	seven, eight, nine, other := PRRef{"example/shop", 7}, PRRef{"example/shop", 8}, PRRef{"example/shop", 9}, PRRef{"other/site", 1}
	before := registryBytes(t, l)
	got, err := m.FixRecords(t.Context(), []PRRef{seven, eight, nine, other})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, registryBytes(t, l)) {
		t.Error("FixRecords changed magnum's registry")
	}
	for ref, want := range map[PRRef]FixRecord{seven: {Loops: 1, Cycle: 2, Pushes: 2, Threads: 3, LeftToYou: true}, eight: {Threads: 9},
		nine: {}, other: {}} {
		if got[ref] != want {
			t.Errorf("%s#%d: %+v, want %+v", ref.Repo, ref.Number, got[ref], want)
		}
	}

	fixStart(t, st, 7, 1, engine.AutofixBy)
	fixEvent(t, st, 7, "fix.replied", map[string]any{"cycle": 1, "threads": 2})
	fixEvent(t, st, 7, "fix.done", map[string]any{"cycle": 1})
	got, err = m.FixRecords(t.Context(), []PRRef{seven})
	if err != nil {
		t.Fatal(err)
	}
	if want := (FixRecord{Loops: 2, Cycle: 1, Pushes: 2, Threads: 5}); got[seven] != want {
		t.Errorf("after a second loop: %+v, want %+v", got[seven], want)
	}
	if again, err := NewMagnum(l, openWatchConfig()).FixRecords(t.Context(), []PRRef{seven}); err != nil || again[seven] != got[seven] {
		t.Errorf("a fresh Magnum reads %+v (%v), want the same %+v", again[seven], err, got[seven])
	}
}

// selfConfig is openWatchConfig with zhuravel as the watch's identity, so
// zhuravel's PRs are your own (config.SelfMatch).
func selfConfig() *config.Config {
	cfg := openWatchConfig()
	cfg.Identities = []config.Identity{{Name: "zhuravel", Kind: "gh", Login: "zhuravel"}}
	return cfg
}

// registryBytes are magnum's registry and its write-ahead log, byte for
// byte: a write by another connection while the fixture's stays open lands
// in the log and is never checkpointed into the database file.
func registryBytes(t *testing.T, l paths.Layout) []byte {
	t.Helper()
	db, err := os.ReadFile(l.DB())
	if err != nil {
		t.Fatal(err)
	}
	wal, err := os.ReadFile(l.DB() + "-wal")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return append(db, wal...)
}

// The factory's handle on magnum's registry is read only (PRAGMA
// query_only): a write through it fails and leaves the registry as it was,
// so no read of the factory's (Busy, PRStates, OwnPRs, FixLoopsOf) can
// change magnum's registry, even by mistake. Before, the handle took
// writes.
func TestTheFactorysHandleOnMagnumsRegistryRefusesWrites(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	ro, err := (&magnumRegistry{l: l}).open()
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, _, err := ro.GetKV(ctx, "probe.write"); err != nil {
		t.Fatalf("a read through the factory's handle: %v", err)
	}
	if err := ro.SetKV(ctx, "probe.write", "1"); err == nil {
		t.Error("a write through the factory's handle on magnum's registry succeeded")
	}
	if _, ok, err := st.GetKV(ctx, "probe.write"); err != nil || ok {
		t.Errorf("magnum's registry has the probe: %v, %v", ok, err)
	}
}

// OwnPRs lists your own open PRs from magnum's registry, each read as
// PRStates reads a PR (magnum's id, title and page, the verdict, the fix
// loop, fix off), and changes nothing in the registry: its file and its
// write-ahead log are byte for byte as they were.
func TestOwnPRsReadsYourOpenPRsAndWritesNothing(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	now := time.Now()
	head := strings.Repeat("ab", 20)
	pr := magnumPR(t, st, 7, head, func(g *store.GitHubPR) { g.Title = new("Coupon cache warmup") })
	setKV(t, st, store.KVDaemonLastTick, store.FormatTime(now))
	setKV(t, st, engine.KVPROpen(pr.ID), engine.OpenVerdict{Verdict: engine.OpenOff, Why: "fix off", Head: head, At: now.Add(time.Minute), Off: engine.OpenOffFixOff})
	setKV(t, st, engine.KVPRFixOff(pr.ID), "1")
	if _, err := st.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	before := registryBytes(t, l)
	prs, err := NewMagnum(l, selfConfig()).OwnPRs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("OwnPRs = %+v, want #7", prs)
	}
	v := prs[0]
	switch {
	case v.Ref != (PRRef{"example/shop", 7}) || v.ID != pr.ID || v.Title != "Coupon cache warmup" || v.URL != "https://github.com/example/shop/pull/7":
		t.Errorf("#7: ref %+v, id %d, title %q, url %q", v.Ref, v.ID, v.Title, v.URL)
	case !v.Found || !v.DaemonUp || !v.Watched || !v.OpenWatch:
		t.Errorf("#7: found %v, daemon up %v, watched %v, open watch %v", v.Found, v.DaemonUp, v.Watched, v.OpenWatch)
	case !v.HasOpen || v.Stale || v.Open.Off != engine.OpenOffFixOff || !v.FixOff:
		t.Errorf("#7's verdict %+v (has %v, stale %v), fix off %v", v.Open, v.HasOpen, v.Stale, v.FixOff)
	}
	if !bytes.Equal(before, registryBytes(t, l)) {
		t.Fatal("OwnPRs changed magnum's registry")
	}
}

// A PR of yours in a repository no [[watch]] covers is not listed: magnum
// no longer polls it, so its row stays open after a merge on GitHub and
// says nothing true of it. Before, YOUR PRS showed it as "magnum: review
// of #N" for good.
func TestYourPRsOfARepositoryNoWatchCoversAreNotListed(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	head := strings.Repeat("cd", 20)
	magnumPR(t, st, 1, head, nil)
	other, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_site", Owner: "other", Name: "site", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: other.ID, NodeID: "PR_site_2", Number: 2, URL: "https://github.com/other/site/pull/2",
		HeadSHA: head, GHState: store.GHOpen, InitialState: store.PRQueued, Identity: "zhuravel", AuthorLogin: new("zhuravel")}); err != nil {
		t.Fatal(err)
	}
	prs, err := NewMagnum(l, selfConfig()).OwnPRs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []PRRef
	for _, v := range prs {
		got = append(got, v.Ref)
	}
	if want := []PRRef{{"example/shop", 1}}; !slices.Equal(got, want) {
		t.Errorf("OwnPRs lists %v, want %v: other/site has no watch", got, want)
	}
}

// A draft, a PR of someone else, a merged or closed PR and a PR of yours
// that GitHub no longer has open are not your open PRs; without magnum's
// registry nothing is listed, and nothing is created.
func TestDraftsAndOtherAuthorsPRsAreNotListed(t *testing.T) {
	l, st := magnumLayout(t)
	head := strings.Repeat("cd", 20)
	magnumPR(t, st, 1, head, nil)
	magnumPR(t, st, 2, head, func(g *store.GitHubPR) { g.IsDraft = true })
	magnumPR(t, st, 3, head, func(g *store.GitHubPR) { g.AuthorLogin = new("alice") })
	magnumPR(t, st, 4, head, func(g *store.GitHubPR) {
		g.GHState, g.MergedAt, g.ClosedAt = store.GHMerged, new(time.Now()), new(time.Now())
	})
	magnumPR(t, st, 5, head, func(g *store.GitHubPR) { g.GHState, g.ClosedAt = store.GHClosed, new(time.Now()) })
	magnumPR(t, st, 6, head, func(g *store.GitHubPR) { g.AuthorLogin = new("Zhuravel") })
	prs, err := NewMagnum(l, selfConfig()).OwnPRs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, v := range prs {
		got = append(got, v.Ref.Number)
	}
	if !slices.Equal(got, []int{1, 6}) {
		t.Errorf("OwnPRs lists %v, want your open non-draft PRs #1 and #6", got)
	}

	none := paths.Layout{Home: t.TempDir()}
	if prs, err := NewMagnum(none, selfConfig()).OwnPRs(t.Context()); err != nil || len(prs) != 0 {
		t.Errorf("no registry: OwnPRs = %+v, %v", prs, err)
	}
	if fsx.Exists(none.DB()) {
		t.Fatal("OwnPRs created magnum's registry")
	}
}

// postedReview records a judge run of magnum's on pr that posted review
// reviewID at head with result, a judge result file's JSON.
func postedReview(t *testing.T, st *store.Store, pr store.PR, reviewID int64, head, result string) {
	t.Helper()
	ctx := t.Context()
	run, err := st.CreateRun(ctx, store.Run{ID: "run-" + strconv.FormatInt(reviewID, 10), PRID: pr.ID, Round: 1, Role: store.RoleJudge,
		Kind: store.RunInitial, TargetSHA: head, Identity: "zhuravel", ReviewerLogin: "zhuravel", State: store.RunPending, PromptText: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionRun(ctx, run.ID, nil, store.RunVerified, func(u *store.RunUpdate) {
		u.Set("outcome", "posted")
		u.Set("review_id", reviewID)
		u.Set("result_json", result)
	}); err != nil {
		t.Fatal(err)
	}
}

// PRStates and OwnPRs read, in the same opening of magnum's registry, what
// the screen's columns and words say of a PR beside its verdict: the
// head's CI by the repository's required checks (a required check that
// did not run at the head is never green), magnum's last posted review
// with its findings, GitHub's review gate, the reviewers asked and not
// yet answered with when, the branches, the head's last move, the PR's
// last activity and its age, and the watch's autofix mode. Before, a PR's
// state stopped at an Approved bool.
func TestPRStatesReadsCIReviewsAndReviewers(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	head := strings.Repeat("ab", 20)
	asked, active := now.Add(-17*time.Hour), now.Add(-3*time.Hour)
	pr := magnumPR(t, st, 7, head, func(g *store.GitHubPR) {
		g.HeadRef, g.BaseRef = new("zhuravel-coupon-cache"), new("master")
		g.CI = &store.CIStatus{SHA: head, Total: 2, Passed: 2, Complete: true, ByWorkflowFile: true, Checks: []store.CheckResult{
			{Name: "rspec", Workflow: "ci", State: store.CheckPassed, At: now}, {Name: "lint", Workflow: "ci", State: store.CheckPassed, At: now}}}
		g.ReviewGate = &store.ReviewGate{Decision: "REVIEW_REQUIRED", Complete: true,
			Opinions: []store.LatestReview{{Login: "rev-bob", State: "CHANGES_REQUESTED", CommitSHA: head}}}
		g.RequestedReviewers = []string{"rev-ann"}
		g.ReviewRequests = []store.ReviewRequest{{At: asked.Add(-time.Hour), By: "zhuravel", To: "rev-bob"}, {At: asked, By: "zhuravel", To: "rev-ann"}}
		g.ActivityAt = &active
	})
	if err := st.SetGitHubRequiredChecks(ctx, "example/shop", store.GitHubRequiredChecks{Branch: "master", Checks: []string{"rspec", "Completion"},
		Known: true, FetchedAt: now, CheckedAt: now}); err != nil {
		t.Fatal(err)
	}
	postedReview(t, st, pr, 901, head, `{"event":"COMMENTED","verdict":"blocking","findings":{"P1":1,"P2":3}}`)
	setKV(t, st, store.KVDaemonLastTick, store.FormatTime(now))

	ref := PRRef{"example/shop", 7}
	views, err := NewMagnum(l, openWatchConfig()).PRStates(ctx, []PRRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	own, err := NewMagnum(l, selfConfig()).OwnPRs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 {
		t.Fatalf("OwnPRs = %+v, want #7", own)
	}
	for name, v := range map[string]PRState{"PRStates": views[ref], "OwnPRs": own[0]} {
		switch {
		case v.CI == nil || v.CI.State != "passed" || len(v.CI.Required) != 2 || v.CI.Required[0].State != "passed" || v.CI.Required[1].State != "missing":
			t.Errorf("%s: CI %+v, want passed with rspec passed and Completion missing", name, v.CI)
		case v.Review == nil || v.Review.SHA != head || v.Review.Counts != [4]int{0, 1, 3, 0} || v.Review.Verdict != store.VerdictBlocking:
			t.Errorf("%s: magnum's last review %+v, want P1 P2×3 blocking at the head", name, v.Review)
		case v.Findings == nil || v.Findings.Counts != [4]int{0, 1, 3, 0} || v.Findings.SHA != head:
			t.Errorf("%s: findings %+v", name, v.Findings)
		case v.Gate == nil || v.Gate.Decision != "REVIEW_REQUIRED" || len(v.Gate.Opinions) != 1 || v.Gate.Opinions[0].Login != "rev-bob":
			t.Errorf("%s: review gate %+v", name, v.Gate)
		case !slices.Equal(v.Asked, []ReviewAsk{{Who: "rev-ann", At: asked}}):
			t.Errorf("%s: asked %+v, want rev-ann since %v (rev-bob answered)", name, v.Asked, asked)
		case v.HeadRef != "zhuravel-coupon-cache" || v.BaseRef != "master":
			t.Errorf("%s: branches %q → %q", name, v.HeadRef, v.BaseRef)
		case !v.ActivityAt.Equal(active) || v.CreatedAt.IsZero() || v.HeadChangedAt.IsZero():
			t.Errorf("%s: activity %v, created %v, head moved %v", name, v.ActivityAt, v.CreatedAt, v.HeadChangedAt)
		}
	}
	if v := views[ref]; v.Autofix != config.AutofixOpen {
		t.Errorf("autofix %q, want the watch's %q", v.Autofix, config.AutofixOpen)
	}
}

// PRStates and OwnPRs link the head's failed jobs to their pages and say
// why GitHub blocks the merge, from the checks' read and magnum's row (its
// review gate), for the card's CI line. Before, a failed job had a name and
// no link, and GitHub's block went unread.
func TestPRStatesLinkTheFailedJobsAndSayWhyGitHubBlocksTheMerge(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	head := strings.Repeat("cd", 20)
	job := "https://github.com/example/shop/actions/runs/7001/job/9001"
	magnumPR(t, st, 8, head, func(g *store.GitHubPR) {
		g.CI = &store.CIStatus{SHA: head, Total: 1, Failed: 1, Complete: true, ByWorkflowFile: true, Merge: "BLOCKED",
			Checks: []store.CheckResult{{Name: "rspec", Workflow: "ci", State: store.CheckFailed, At: now, URL: job}}}
		g.ReviewGate = &store.ReviewGate{Decision: "REVIEW_REQUIRED", Complete: true}
	})
	if err := st.SetGitHubRequiredChecks(ctx, "example/shop", store.GitHubRequiredChecks{Branch: "master", Checks: []string{"rspec"},
		Known: true, FetchedAt: now, CheckedAt: now}); err != nil {
		t.Fatal(err)
	}
	ref := PRRef{"example/shop", 8}
	views, err := NewMagnum(l, openWatchConfig()).PRStates(ctx, []PRRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	ci := views[ref].CI
	if ci == nil || !slices.Equal(ci.Links, []tui.CheckLink{{Name: "ci / rspec", URL: job}}) {
		t.Fatalf("CI %+v, want ci / rspec linked to its job", ci)
	}
	if want := "GitHub blocks the merge: rspec failed, a required approval is missing"; ci.Blocked != want {
		t.Errorf("CI says %q, want %q", ci.Blocked, want)
	}
}

// PRStates and OwnPRs say when magnum's daemon runs an older build than the
// one on disk (engine.SkewNote: its binary changed since the daemon
// started), from the build the daemon recorded (engine.KVDaemonBuild); one
// that runs what is on disk, and a daemon that is down, say nothing. Before,
// nothing under internal/factory read the daemon's build, so a daemon
// without the look left every verdict to come for good, unexplained.
func TestPRStatesSayMagnumsDaemonRunsAnOlderBuild(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	magnumPR(t, st, 7, strings.Repeat("ab", 20), nil)
	bin := l.Home + "/magnum"
	if err := os.WriteFile(bin, []byte("new build"), 0o755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 6, 22, 29, 0, 0, time.UTC)
	ref := PRRef{"example/shop", 7}
	read := func() PRState {
		t.Helper()
		views, err := NewMagnum(l, openWatchConfig()).PRStates(ctx, []PRRef{ref})
		if err != nil {
			t.Fatal(err)
		}
		own, err := NewMagnum(l, selfConfig()).OwnPRs(ctx)
		if err != nil || len(own) != 1 || (own[0].OldBuild == nil) != (views[ref].OldBuild == nil) {
			t.Fatalf("OwnPRs = %+v, %v: want the same build as PRStates", own, err)
		}
		return views[ref]
	}
	setKV(t, st, engine.KVDaemonBuild, engine.Build{Version: "dev", Revision: strings.Repeat("c8fcd4c6", 5), Path: bin,
		ModTime: fi.ModTime().Add(-time.Hour), StartedAt: started})
	if v := read(); v.OldBuild == nil || v.OldBuild.Label() != "dev (c8fcd4c)" || !v.OldBuild.StartedAt.Equal(started) {
		t.Errorf("a daemon whose binary changed since it started: old build %+v", v.OldBuild)
	}
	setKV(t, st, engine.KVDaemonBuild, engine.Build{Version: "dev", Path: bin, ModTime: fi.ModTime(), StartedAt: started})
	if v := read(); v.OldBuild != nil {
		t.Errorf("a daemon that runs the build on disk: old build %+v", v.OldBuild)
	}
}

// PRStates and OwnPRs say what magnum's open of the PR does, as the board
// tells it for its o: whether it restores the PR in a slot (the judge's
// session parked: it claims a slot and holds the PR, so the screen asks
// y/N) or focuses a live pane, and how long a restore holds it ([daemon]
// open_hold); and the replies magnum's judge has not read yet.
func TestPRStatesSayWhatMagnumsOpenDoes(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	head := strings.Repeat("ab", 20)
	now := time.Now().UTC().Truncate(time.Second)
	parked := magnumPR(t, st, 7, head, nil)
	live := magnumPR(t, st, 8, head, func(g *store.GitHubPR) {
		g.Replies = []store.Reply{{At: now.Add(-time.Hour), By: "rev-ann", Thread: true}, {At: now.Add(-time.Minute), By: "alice"}}
	})
	for pr, state := range map[int64]string{parked.ID: store.SessionParked, live.ID: store.SessionLive} {
		if _, err := st.CreateSession(ctx, store.Session{PRID: pr, Role: store.RoleJudge, State: state, AgentName: new("mg-judge")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.TransitionPR(ctx, live.ID, nil, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("reviewed_sha", head)
		u.Set("reviewed_at", store.FormatTime(now.Add(-2*time.Hour)))
	}); err != nil {
		t.Fatal(err)
	}
	cfg := openWatchConfig()
	cfg.Daemon.OpenHold = config.Duration{Duration: 30 * time.Minute}
	refs := []PRRef{{"example/shop", 7}, {"example/shop", 8}}
	views, err := NewMagnum(l, cfg).PRStates(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	if v := views[refs[0]]; !v.OpenRestores || v.OpenHold != 30*time.Minute {
		t.Errorf("a PR whose judge is parked: restores %v, hold %v; want a restore held 30m", v.OpenRestores, v.OpenHold)
	}
	v := views[refs[1]]
	if v.OpenRestores {
		t.Error("a PR whose judge's pane is live restores: its open focuses the pane")
	}
	if len(v.Replies) != 2 || v.Replies[0].By != "rev-ann" || !v.Replies[0].Thread || v.Replies[1].By != "alice" {
		t.Errorf("replies %+v, want rev-ann's in magnum's thread and alice's, both after magnum's review", v.Replies)
	}
}

// roundRun records a run of magnum's on pr in round with role at at on
// head; a reviewID above zero makes it the run that posted that review,
// verified 2 minutes after it was created.
func roundRun(t *testing.T, st *store.Store, pr store.PR, round int, role, head string, reviewID int64, at time.Time) store.Run {
	t.Helper()
	r := store.Run{PRID: pr.ID, Round: round, Role: role, Kind: store.RunInitial, State: store.RunVerified, TargetSHA: head,
		Identity: "zhuravel", ReviewerLogin: "zhuravel", PromptText: "p", CreatedAt: at}
	if reviewID > 0 {
		r.Outcome, r.ReviewID, r.VerifiedAt = new("posted"), new(reviewID), new(at.Add(2*time.Minute))
	}
	run, err := st.CreateRun(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// FirstPostedRound reads magnum's first round on a PR that posted a review
// and began at or after a time (the publish), with the findings it posted
// (magnum's id of each, its path, its line as the start and the end, its
// priority and its title) and its result's counts, and writes nothing: a round that began before
// the time counts not, and a PR, a repository or a registry magnum does not
// have has no round, and nothing is created.
func TestFirstPostedRoundReadsMagnumsFirstPostedRoundSinceThePublish(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	pr := magnumPR(t, st, 7, strings.Repeat("ab", 20), nil)
	published := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	before := roundRun(t, st, pr, 1, store.RoleJudge, "aaa111", 500, published.Add(-time.Minute))
	if err := st.RecordFindings(ctx, before.ID, pr.ID, 1, []store.Finding{{FindingID: "F1", Title: "Before", Severity: "P1", Path: "app/a.rb",
		Line: 1, Verdict: store.FindingPosted}}); err != nil {
		t.Fatal(err)
	}
	roundRun(t, st, pr, 2, "claude-review", "bbb222", 0, published.Add(time.Hour))
	judge := roundRun(t, st, pr, 2, store.RoleJudge, "bbb222", 501, published.Add(time.Hour+5*time.Minute))
	if err := st.UpdateRun(ctx, judge.ID, func(u *store.RunUpdate) { u.Set("result_json", `{"findings":{"P0":0,"P1":0,"P2":1,"P3":1}}`) }); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFindings(ctx, judge.ID, pr.ID, 2, []store.Finding{
		{FindingID: "F1", Title: "The refund misses the cap", Severity: "P2", Path: "app/models/credit.rb", Line: 42, Verdict: store.FindingPosted},
		{FindingID: "F2", Title: "A body note", Severity: "P3", Verdict: store.FindingPosted},
	}); err != nil {
		t.Fatal(err)
	}
	m := NewMagnum(l, openWatchConfig())
	ref := PRRef{"example/shop", 7}
	was := registryBytes(t, l)
	got, ok, err := m.FirstPostedRound(ctx, ref, published)
	if err != nil || !ok {
		t.Fatalf("FirstPostedRound = %v, %v", ok, err)
	}
	if !bytes.Equal(was, registryBytes(t, l)) {
		t.Error("FirstPostedRound changed magnum's registry")
	}
	if got.RunID != judge.ID || got.Round != 2 || got.Head != "bbb222" || got.ReviewID != 501 || !got.Start.Equal(published.Add(time.Hour)) ||
		!got.Posted.Equal(published.Add(time.Hour+7*time.Minute)) || !got.Counted || got.Counts != [4]int{0, 0, 1, 1} {
		t.Errorf("the round %+v, want the judge's run of round 2, posted as it was verified, with its result's counts", got)
	}
	if len(got.Findings) != 2 {
		t.Fatalf("its findings %+v, want 2", got.Findings)
	}
	f, body := got.Findings[0], got.Findings[1]
	if f.ID == 0 || f.Path != "app/models/credit.rb" || f.StartLine != 42 || f.EndLine != 42 || f.Priority != "P2" || f.Title != "The refund misses the cap" {
		t.Errorf("the placed finding %+v", f)
	}
	if body.Path != "" || body.StartLine != 0 || body.EndLine != 0 || body.Priority != "P3" {
		t.Errorf("the body finding %+v", body)
	}
	if early, ok, err := m.FirstPostedRound(ctx, ref, published.Add(-time.Hour)); err != nil || !ok || early.Round != 1 {
		t.Errorf("since before round 1: %+v, %v, %v", early, ok, err)
	}
	for _, r := range []PRRef{{"example/shop", 8}, {"other/site", 7}} {
		if _, ok, err := m.FirstPostedRound(ctx, r, published); err != nil || ok {
			t.Errorf("%s#%d magnum lacks: %v, %v; want no round", r.Repo, r.Number, ok, err)
		}
	}
	none := paths.Layout{Home: t.TempDir()}
	if _, ok, err := NewMagnum(none, nil).FirstPostedRound(ctx, ref, published); err != nil || ok {
		t.Errorf("no registry: %v, %v; want no round", ok, err)
	}
	if fsx.Exists(none.DB()) {
		t.Error("FirstPostedRound created magnum's registry")
	}
}

// The factory reads your overrule of Codex's reading where `magnum resume
// --tool codex` writes it, magnum's registry, and writes nothing: the
// mark's time when it is set, zero when it is not (the review daemon
// deleted it once Codex reported again), when it is no time, or when there
// is no registry, which it never creates.
func TestTheFactoryReadsYourCodexOverruleInMagnumsRegistry(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	m := NewMagnum(l, config.Defaults())
	if at, err := m.CodexOverrule(ctx); err != nil || !at.IsZero() {
		t.Fatalf("no mark: %v, %v; want zero", at, err)
	}
	resumed := time.Date(2026, 11, 13, 11, 3, 0, 0, time.UTC)
	setKV(t, st, engine.KVUsageCodexOverruledAt, store.FormatTime(resumed))
	if at, err := m.CodexOverrule(ctx); err != nil || !at.Equal(resumed) {
		t.Fatalf("the mark: %v, %v; want %v", at, err, resumed)
	}
	setKV(t, st, engine.KVUsageCodexOverruledAt, "soon")
	if at, err := m.CodexOverrule(ctx); err != nil || !at.IsZero() {
		t.Fatalf("a mark that is no time: %v, %v; want zero", at, err)
	}
	none := paths.Layout{Home: t.TempDir()}
	if at, err := NewMagnum(none, nil).CodexOverrule(ctx); err != nil || !at.IsZero() {
		t.Fatalf("no registry: %v, %v; want zero", at, err)
	}
	if fsx.Exists(none.DB()) {
		t.Error("CodexOverrule created magnum's registry")
	}
}
