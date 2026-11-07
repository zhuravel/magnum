package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// prsSeedOwn seeds your own open PR #20 (zhuravel's), your draft #21, your
// PR #22 merged an hour ago and alice's open #23, in talkable/talkable;
// edit shapes each by number before it is stored.
func prsSeedOwn(t *testing.T, f *inspFixture) *store.Store {
	t.Helper()
	st := f.store()
	at := func(d time.Duration) string { return store.FormatTime(time.Now().Add(-d)) }
	seed := func(n int, author, state string, set func(*store.PRUpdate)) {
		inspSeedPR(t, st, "talkable/talkable", n, state, func(u *store.PRUpdate) {
			u.Set("author_login", author)
			u.Set("gh_updated_at", at(time.Duration(n)*time.Minute))
			if set != nil {
				set(u)
			}
		})
	}
	seed(20, "zhuravel", store.PRReviewed, nil)
	seed(21, "zhuravel", store.PRReviewed, func(u *store.PRUpdate) { u.Set("is_draft", true) })
	seed(22, "zhuravel", store.PRClosed, func(u *store.PRUpdate) {
		u.Set("gh_state", store.GHMerged)
		u.Set("merged_at", at(time.Hour))
		u.Set("closed_at", at(time.Hour))
	})
	seed(23, "alice", store.PRReviewed, nil)
	return st
}

// numbers are the PR numbers of rows, in order.
func numbers(rows []tui.PRBoardRow) []int {
	out := make([]int, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Number)
	}
	slices.Sort(out)
	return out
}

// With [board] own_prs = "factory" the board leaves your own open PRs out
// of its open PRs (the factory screen lists them); a draft of yours, which
// the factory screen does not list, a closed one and other people's stay,
// and the board's footer learns how many it left out. By default (own_prs
// = "board") the board lists them as today.
func TestTheBoardHidesOwnPRsWhenTheFactoryShowsThem(t *testing.T) {
	f := newInspFixture(t)
	st := prsSeedOwn(t, f)
	defer st.Close()
	ctx := context.Background()
	a, err := inspOpenApp(f.Ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cfg := a.Config
	o := prsOptions{View: tui.ViewAll, Sort: tui.SortUpdated, Desc: true}
	rows, err := prsRows(ctx, st, cfg, o, prsSelfLogins(cfg), f.Ctx.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if got := numbers(rows); !slices.Equal(got, []int{20, 21, 22, 23}) {
		t.Fatalf("own_prs = board lists %v, want every PR", got)
	}

	cfg.Board.OwnPRs = "factory"
	rows, err = prsRows(ctx, st, cfg, o, prsSelfLogins(cfg), f.Ctx.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if got := numbers(rows); !slices.Equal(got, []int{21, 22, 23}) {
		t.Errorf("own_prs = factory lists %v, want your open #20 left out", got)
	}
	var hidden atomic.Int64
	src := ownPRsSource(cfg, prsSource(st, cfg, store.BoardFilter{ClosedSince: time.Now().Add(-24 * time.Hour)}, prsSelfLogins(cfg), f.Ctx.Layout), &hidden)
	if _, err := src(ctx); err != nil {
		t.Fatal(err)
	}
	if hidden.Load() != 1 {
		t.Errorf("the board's footer counts %d of your PRs on the factory screen, want 1", hidden.Load())
	}
}

// With [board] own_prs = "factory" the board keeps your open PR of a
// repository no [[watch]] covers: the factory screen lists only the PRs
// magnum watches, so it would show nowhere, and the footer counts only the
// PRs it left to the factory screen. Before, the board left it out too.
func TestTheBoardKeepsYourPRsOfARepositoryNoWatchCovers(t *testing.T) {
	f := newInspFixture(t)
	st := prsSeedOwn(t, f)
	defer st.Close()
	ctx := context.Background()
	inspSeedPR(t, st, "example/shop", 30, store.PRReviewed, func(u *store.PRUpdate) { u.Set("author_login", "zhuravel") })
	a, err := inspOpenApp(f.Ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cfg := a.Config
	cfg.Board.OwnPRs = "factory"
	var hidden atomic.Int64
	rows, err := ownPRsSource(cfg, prsSource(st, cfg, store.BoardFilter{}, prsSelfLogins(cfg), f.Ctx.Layout), &hidden)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := numbers(rows); !slices.Equal(got, []int{21, 22, 23, 30}) {
		t.Errorf("own_prs = factory lists %v, want example/shop#30 kept beside your draft, your merged PR and alice's", got)
	}
	if hidden.Load() != 1 {
		t.Errorf("the board's footer counts %d of your PRs on the factory screen, want 1 (#20)", hidden.Load())
	}
}

// Your own open PRs on the board carry their autofix = "open" verdict
// (pr.<id>.open): fresh, or stale ("looking") when magnum decided it at
// another head, before the PR's row changed or long ago; a PR of someone
// else's carries none.
func TestTheBoardMarksOwnPRsWithTheVerdict(t *testing.T) {
	f := newInspFixture(t)
	st := prsSeedOwn(t, f)
	defer st.Close()
	ctx := context.Background()
	_, own := inspSeedPR(t, st, "talkable/talkable", 24, store.PRReviewed, func(u *store.PRUpdate) { u.Set("author_login", "zhuravel") })
	pr20, err := st.PRByRepoNumber(ctx, own.RepoID, 20)
	if err != nil {
		t.Fatal(err)
	}
	pr23, err := st.PRByRepoNumber(ctx, own.RepoID, 23)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	put := func(id int64, v engine.OpenVerdict) {
		b, _ := json.Marshal(v)
		if err := st.SetKV(ctx, engine.KVPROpen(id), string(b)); err != nil {
			t.Fatal(err)
		}
	}
	put(pr20.ID, engine.OpenVerdict{Verdict: engine.OpenNeedsYou, Why: "1 item needs you: 1 thread of a person", Head: pr20.HeadSHA, At: later})
	put(own.ID, engine.OpenVerdict{Verdict: engine.OpenIdle, Why: "nothing open at 0123456", Head: "0123456789", At: later})
	put(pr23.ID, engine.OpenVerdict{Verdict: engine.OpenIdle, Why: "nothing open", Head: pr23.HeadSHA, At: later})
	if err := st.SetKV(ctx, store.KVDaemonLastTick, store.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	a, err := inspOpenApp(f.Ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	rows, err := prsRows(ctx, st, a.Config, prsOptions{View: tui.ViewAll, Sort: tui.SortUpdated, Desc: true}, prsSelfLogins(a.Config), f.Ctx.Layout)
	if err != nil {
		t.Fatal(err)
	}
	by := map[int]tui.PRBoardRow{}
	for _, r := range rows {
		by[r.Number] = r
	}
	want := tui.PROpen{Verdict: engine.OpenNeedsYou, Why: "1 item needs you: 1 thread of a person", Head: pr20.HeadSHA}
	if o := by[20].Open; o == nil || *o != want {
		t.Errorf("#20's verdict %+v, want %+v", o, want)
	}
	if o := by[24].Open; o == nil || !o.Stale || o.Verdict != engine.OpenIdle {
		t.Errorf("#24's verdict of another head %+v, want it stale", o)
	}
	if o := by[23].Open; o != nil {
		t.Errorf("alice's #23 carries a verdict %+v", o)
	}
	if o := by[21].Open; o != nil {
		t.Errorf("your draft #21 carries a verdict %+v, though it has none", o)
	}
}

// magnum prs --json gains each PR's verdict as "open" (verdict, why, head,
// stale), null for a PR without one; the existing keys stay.
func TestPrsJSONCarriesTheVerdict(t *testing.T) {
	f := newInspFixture(t)
	st := prsSeedOwn(t, f)
	ctx := context.Background()
	repo, err := st.RepoByFullName(ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	pr20, err := st.PRByRepoNumber(ctx, repo.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(engine.OpenVerdict{Verdict: engine.OpenWaits, Why: "settling until 21:13", Head: pr20.HeadSHA, At: time.Now().Add(time.Minute)})
	if err := st.SetKV(ctx, engine.KVPROpen(pr20.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, store.KVDaemonLastTick, store.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if code := f.run("tui", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatalf("%v\n%s", err, f.Out.String())
	}
	seen := 0
	for _, r := range rows {
		open, ok := r["open"]
		if !ok {
			t.Errorf("%v: no open key", r["ref"])
		}
		if _, ok := r["fix_off"]; !ok {
			t.Errorf("%v: an existing key is gone", r["ref"])
		}
		switch r["number"] {
		case float64(20):
			seen++
			want := map[string]any{"verdict": "waits", "why": "settling until 21:13", "head": pr20.HeadSHA, "stale": false}
			got, _ := open.(map[string]any)
			if len(got) != len(want) {
				t.Errorf("#20's open = %v, want %v", open, want)
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("#20's open.%s = %v, want %v", k, got[k], v)
				}
			}
		default:
			if open != nil {
				t.Errorf("%v: open = %v, want null", r["ref"], open)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("no row #20 in %s", f.Out.String())
	}
}

// own_prs = "factory" in the user's config reaches the printed rows: your
// open #20 is left out.
func TestThePrintedBoardHidesOwnPRsUnderOwnPRsFactory(t *testing.T) {
	f := newInspFixture(t)
	prsSeedOwn(t, f).Close()
	cfgFile := filepath.Join(f.Home, "config.toml")
	body, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgFile, append(body, []byte("\n[board]\nown_prs = \"factory\"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run("tui"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	out := f.Out.String()
	if strings.Contains(out, "talkable#20 ") || !strings.Contains(out, "talkable#23") || !strings.Contains(out, "talkable#21") {
		t.Errorf("own_prs = factory printed:\n%s", out)
	}
}
