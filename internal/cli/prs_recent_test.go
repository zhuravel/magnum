package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// prsSeedRecent seeds prsSeed's three PRs (#3 merged long ago) and three
// PRs GitHub closed: #11990 merged an hour ago before magnum reviewed its
// last push (it closed while rereview_pending), #11991 closed unmerged two
// hours ago, #11992 merged three days ago (outside the 24h window).
func prsSeedRecent(t *testing.T, f *inspFixture) {
	t.Helper()
	prsSeed(t, f)
	st := f.store()
	at := func(d time.Duration) string { return store.FormatTime(time.Now().Add(-d)) }
	closed := func(n int, gh, prev string, when time.Duration, reviewed string) {
		inspSeedPR(t, st, "talkable/talkable", n, store.PRClosed, func(u *store.PRUpdate) {
			u.Set("gh_updated_at", at(when))
			u.Set("gh_state", gh)
			u.Set("prev_state", prev)
			u.Set("closed_at", at(when))
			if gh == store.GHMerged {
				u.Set("merged_at", at(when))
			}
			if reviewed != "" {
				u.Set("reviewed_sha", reviewed)
				u.Set("last_review_event", "COMMENTED")
				u.Set("reviewed_at", at(when+time.Hour))
			}
		})
	}
	closed(11990, store.GHMerged, store.PRRereviewPending, time.Hour, "0123456789abcdef")
	closed(11991, store.GHClosed, store.PRQueued, 2*time.Hour, "")
	closed(11992, store.GHMerged, store.PRReviewed, 72*time.Hour, "abcdef0123456789abcdef0123456789abcdef01")
	st.Close()
}

// `magnum prs` without a terminal lists the PRs GitHub merged or closed
// within [board] recent_closed after the open ones, newest closed first;
// the state column flags the one merged before magnum reviewed it.
func TestPRsListsRecentlyClosedAfterTheOpenOnes(t *testing.T) {
	f := newInspFixture(t)
	prsSeedRecent(t, f)

	if code := f.run("prs"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	out := f.Out.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var refs []string
	for _, l := range lines[1:] {
		refs = append(refs, strings.Fields(l)[0])
	}
	if want := []string{"talkable#11931", "talkable#11920", "talkable#11990", "talkable#11991"}; !slices.Equal(refs, want) {
		t.Fatalf("rows %v, want %v:\n%s", refs, want, out)
	}
	if !strings.Contains(lines[3], "closed,merged,unreviewed") {
		t.Errorf("the merged unreviewed row's state:\n%s", lines[3])
	}
	if !strings.Contains(lines[4], "closed,closed") || strings.Contains(lines[4], "unreviewed") {
		t.Errorf("the closed row's state:\n%s", lines[4])
	}

	if code := f.run("prs", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var rows []prsJSONRow
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatalf("json: %v\n%s", err, f.Out.String())
	}
	if len(rows) != 4 {
		t.Fatalf("json rows %v", prsJSONRefs(rows))
	}
	for _, r := range rows[:2] {
		if r.Recent || r.MergedUnreviewed || !r.ClosedAt.IsZero() {
			t.Errorf("open row %s: %+v", r.Ref, r)
		}
	}
	if r := rows[2]; !r.Recent || !r.MergedUnreviewed || r.ClosedAt.IsZero() || time.Since(r.ClosedAt) > 2*time.Hour {
		t.Errorf("merged unreviewed row: recent %v flag %v closed %v", r.Recent, r.MergedUnreviewed, r.ClosedAt)
	}
	if r := rows[3]; !r.Recent || r.MergedUnreviewed {
		t.Errorf("closed row: recent %v flag %v", r.Recent, r.MergedUnreviewed)
	}

	// --all keeps listing every closed PR; the ones outside the window are
	// not in the section.
	if code := f.run("prs", "--all", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	got := prsJSONRefs(rows)
	if len(got) != 6 || !slices.Equal(got[4:], []string{"talkable/talkable#11990", "talkable/talkable#11991"}) {
		t.Fatalf("--all rows %v, want every PR with the two recent ones last", got)
	}
	for _, r := range rows[:4] {
		if r.Recent {
			t.Errorf("--all: %s is in the section", r.Ref)
		}
	}
}

// recent_closed = "0" turns the section off: only open PRs are listed.
func TestPRsRecentClosedZeroListsOnlyOpenPRs(t *testing.T) {
	f := newInspFixture(t)
	cfg := filepath.Join(f.Home, "config.toml")
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(body, "\n[board]\nrecent_closed = \"0\"\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	prsSeedRecent(t, f)
	if code := f.run("prs", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var rows []prsJSONRow
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if got := prsJSONRefs(rows); !slices.Equal(got, []string{"talkable/talkable#11931", "talkable/talkable#11920"}) {
		t.Fatalf("rows %v, want only the open PRs", got)
	}
}

// The live board's source moves the window with the clock: every load
// asks for the PRs closed within recent_closed of now, so a PR leaves the
// section once it is older.
func TestPRsSourceMovesTheWindowWithTheClock(t *testing.T) {
	f := newInspFixture(t)
	prsSeedRecent(t, f)
	a, err := inspOpenApp(f.Ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	d := newStatusDeps(a, "test")
	o := prsOptions{View: tui.ViewAll, Sort: tui.SortUpdated, Desc: true}
	src := prsSource(d.Store, d.Config, o.filter(), nil, f.Ctx.Layout)
	recent := func() []string {
		t.Helper()
		rows, err := src(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			if r.Recent {
				out = append(out, r.Ref)
			}
		}
		slices.Sort(out)
		return out
	}
	if got := recent(); !slices.Equal(got, []string{"talkable/talkable#11990", "talkable/talkable#11991"}) {
		t.Fatalf("now: %v", got)
	}
	prev := inspNow
	inspNow = func() time.Time { return time.Now().Add(90 * time.Minute) } // #11991 closed 3h30m ago, #11990 2h30m ago
	t.Cleanup(func() { inspNow = prev })
	if got := recent(); !slices.Equal(got, []string{"talkable/talkable#11990", "talkable/talkable#11991"}) {
		t.Fatalf("+90m: %v", got)
	}
	inspNow = func() time.Time { return time.Now().Add(22*time.Hour + 30*time.Minute) } // #11991 is 24h30m old
	if got := recent(); !slices.Equal(got, []string{"talkable/talkable#11990"}) {
		t.Fatalf("+22h30m: %v", got)
	}
	if bo := prsBoardOptions(d.Config, o); bo.RecentClosed != 24*time.Hour {
		t.Errorf("board options RecentClosed = %v, want the configured 24h", bo.RecentClosed)
	}
}

// The state column flags a PR merged before magnum reviewed its last push.
func TestPRsStateCellFlagsMergedUnreviewed(t *testing.T) {
	for _, c := range []struct {
		row  tui.PRBoardRow
		want string
	}{
		{tui.PRBoardRow{State: "closed", GHState: "MERGED", MergedUnreviewed: true}, "closed,merged,unreviewed"},
		{tui.PRBoardRow{State: "released", GHState: "MERGED", MergedUnreviewed: true, Muted: true}, "released,merged,unreviewed,muted"},
		{tui.PRBoardRow{State: "closed", GHState: "MERGED"}, "closed,merged"},
		{tui.PRBoardRow{State: "closed", GHState: "CLOSED"}, "closed,closed"},
	} {
		if got := prsStateCell(c.row, prsNow); got != c.want {
			t.Errorf("%+v: %q, want %q", c.row, got, c.want)
		}
	}
}
