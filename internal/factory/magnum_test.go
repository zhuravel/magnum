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
	}
	if v := views[refs[1]]; !v.Found || v.GHState != store.GHMerged || v.MergedAt == nil || v.HasOpen || v.Fixing {
		t.Errorf("#8, merged: %+v", v)
	}
	if v := views[refs[2]]; v.State != store.PRNeedsAttention || v.Attention != "blocked" || !strings.Contains(v.AttentionMsg, "the slot's database is gone") {
		t.Errorf("#9, needs attention: state %q, attention %q, %q", v.State, v.Attention, v.AttentionMsg)
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

// selfConfig is openWatchConfig with zhuravel as the watch's identity, so
// zhuravel's PRs are your own (config.SelfMatch).
func selfConfig() *config.Config {
	cfg := openWatchConfig()
	cfg.Identities = []config.Identity{{Name: "zhuravel", Kind: "gh", Login: "zhuravel"}}
	return cfg
}

// OwnPRs lists your own open PRs from magnum's registry, each read as
// PRStates reads a PR (magnum's id, title and page, the verdict, the fix
// loop, fix off), and changes nothing in the registry: its file is byte
// for byte as it was.
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
	before, err := os.ReadFile(l.DB())
	if err != nil {
		t.Fatal(err)
	}
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
	after, err := os.ReadFile(l.DB())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("OwnPRs changed magnum's registry")
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
