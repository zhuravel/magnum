package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// starRecord is PR prID's star record (store.KVPRStar) as stored; false
// when the PR is not starred.
func (h *actHarness) starRecord(prID int64) (prStar, bool) {
	h.t.Helper()
	v, ok := h.kv(store.KVPRStar(prID))
	if !ok {
		return prStar{}, false
	}
	var s prStar
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		h.t.Fatalf("star record %q: %v", v, err)
	}
	return s, true
}

// `magnum star <ref>` stars a PR in the registry, at once and without the
// daemon: starring again keeps the first record, --off unstars it (twice is
// no error), --json prints the state, and a PR the registry does not know is
// refused with nothing written. It sets a state and never toggles.
func TestStarCommandSetsAndClearsAndRefusesAnUnknownPR(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	run := func(want int, args ...string) string {
		t.Helper()
		h.out.Reset()
		h.errb.Reset()
		if code := h.cmd("star", args...); code != want {
			t.Fatalf("star %v: exit %d, want %d: %s%s", args, code, want, h.out.String(), h.errb.String())
		}
		return h.out.String() + h.errb.String()
	}

	actContains(t, run(0, "talkable#5"), "starred talkable#5")
	first, ok := h.starRecord(pr.ID)
	if !ok || !first.At.Equal(h.now) || first.By != "magnum star" {
		t.Fatalf("record %+v (%v), want at %v by magnum star", first, ok, h.now)
	}
	h.now = h.now.Add(time.Hour)
	actContains(t, run(0, "talkable#5"), "talkable#5 is starred (since "+inspClock(h.now, first.At)+", by magnum star)")
	if again, _ := h.starRecord(pr.ID); again != first {
		t.Errorf("starring again rewrote the record: %+v, was %+v", again, first)
	}

	var out struct {
		Ref       string    `json:"ref"`
		Starred   bool      `json:"starred"`
		StarredAt time.Time `json:"starred_at"`
		StarredBy string    `json:"starred_by"`
		Changed   bool      `json:"changed"`
	}
	if err := json.Unmarshal([]byte(run(0, "talkable#5", "--json")), &out); err != nil {
		t.Fatal(err)
	}
	if out.Ref != "talkable/talkable#5" || !out.Starred || !out.StarredAt.Equal(first.At) || out.StarredBy != "magnum star" || out.Changed {
		t.Errorf("--json %+v", out)
	}

	actContains(t, run(0, "talkable#5", "--off"), "unstarred talkable#5")
	if _, ok := h.starRecord(pr.ID); ok {
		t.Fatal("--off left the record")
	}
	actContains(t, run(0, "--off", "talkable#5"), "talkable#5 is not starred")
	raw := run(0, "--off", "talkable#5", "--json")
	if !strings.Contains(raw, `"starred": false`) || strings.Contains(raw, "starred_at") {
		t.Errorf("--off --json: %s", raw)
	}

	msg := run(1, "talkable#99")
	actContains(t, msg, "talkable/talkable#99 is not in the registry yet", "then run `magnum star talkable#99` again")
	if keys, err := h.st.KVKeys(h.ctx, "pr.*.star"); err != nil || len(keys) != 0 {
		t.Errorf("an unknown PR wrote %v (%v)", keys, err)
	}
	for _, args := range [][]string{{}, {"talkable#5", "talkable#6"}} {
		run(2, args...)
	}
	if len(h.requests()) != 0 {
		t.Errorf("the star queued daemon requests: %+v", h.requests())
	}
}

// A star lives in the registry: the board's * writes it as "the board", every
// load of the board reads it with its time, starring again (from the CLI)
// keeps the first time, and it outlives a merge and a reopen, so a reopened
// PR returns starred; only an unstar ends it.
func TestStarIsKeptAcrossReloads(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.seedPR("talkable/talkable", 6, store.PRReviewed)
	acts := newScreenActions(h.c)
	defer acts.Close()
	ctx := context.Background()
	res, err := acts.Star(ctx, "talkable#5", true)
	if err != nil || !strings.Contains(res.Text, "starred talkable#5") {
		t.Fatalf("the board's star: %+v, %v", res, err)
	}
	at := h.now
	src := prsSource(h.st, h.c.Config, store.BoardFilter{IncludeClosed: true}, nil, h.c.Layout)
	starredOf := func() map[int]time.Time {
		t.Helper()
		rows, err := src(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int]time.Time{}
		for _, r := range rows {
			if r.Starred {
				out[r.Number] = r.StarredAt
			}
		}
		return out
	}
	for range 2 {
		if got := starredOf(); len(got) != 1 || !got[5].Equal(at) {
			t.Fatalf("a load reads the stars %v, want #5 at %v", got, at)
		}
	}
	if rec, _ := h.starRecord(pr.ID); rec.By != "the board" {
		t.Errorf("the board's star is by %q", rec.By)
	}

	h.now = h.now.Add(time.Hour)
	if code := h.cmd("star", "talkable#5"); code != 0 {
		t.Fatalf("star again: %s", h.errb.String())
	}
	h.setPR(pr.ID, store.PRClosed, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) })
	if got := starredOf(); !got[5].Equal(at) {
		t.Errorf("merged: stars %v, want #5 still at %v", got, at)
	}
	h.setPR(pr.ID, store.PRRereviewPending, func(u *store.PRUpdate) { u.Set("gh_state", store.GHOpen) })
	if got := starredOf(); !got[5].Equal(at) {
		t.Errorf("reopened: stars %v, want #5 at %v", got, at)
	}

	if _, err := acts.Star(ctx, "talkable#5", false); err != nil {
		t.Fatal(err)
	}
	if got := starredOf(); len(got) != 0 {
		t.Errorf("after the unstar: %v", got)
	}
}

// The live board's --limit no longer caps the registry query, which orders
// by the last update: the board reads every PR, with its star, and caps
// after its own sort (tui.PRBoardOptions.Limit), as the printed rows do.
func TestTheBoardSourceReadsPastTheLimit(t *testing.T) {
	f := newInspFixture(t)
	prsSeed(t, f)
	st := f.store()
	_, old := inspSeedPR(t, st, "talkable/talkable", 11000, store.PRBaseline, func(u *store.PRUpdate) {
		u.Set("gh_updated_at", store.FormatTime(time.Now().Add(-21*24*time.Hour)))
	})
	if err := st.SetKV(context.Background(), store.KVPRStar(old.ID), `{"at":"2026-10-16T09:00:00Z","by":"magnum star"}`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	onScreen(t)
	var opts tui.PRBoardOptions
	var rows []tui.PRBoardRow
	prev := tuiPRBoard
	tuiPRBoard = func(ctx context.Context, src tui.PRBoardSource, _ tui.DashboardActions, o tui.PRBoardOptions) error {
		opts = o
		var err error
		rows, err = src.Rows(ctx)
		return err
	}
	t.Cleanup(func() { tuiPRBoard = prev })

	if code := f.run("tui", "--limit", "1"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if opts.Limit != 1 {
		t.Errorf("the board's limit is %d, want 1", opts.Limit)
	}
	i := slices.IndexFunc(rows, func(r tui.PRBoardRow) bool { return r.Number == 11000 })
	if len(rows) != 3 || i < 0 || !rows[i].Starred || !rows[i].StarredAt.Equal(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("the board read %v (the starred #11000 at %d)", prsRefs(rows), i)
	}

	if code := f.run("tui", "--limit", "1", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var printed []prsJSONRow
	if err := json.Unmarshal(f.Out.Bytes(), &printed); err != nil {
		t.Fatal(err)
	}
	if len(printed) != 1 || printed[0].Number != 11000 || !printed[0].Starred {
		t.Errorf("--limit 1 --json printed %v, want the starred #11000", prsJSONRefs(printed))
	}
}
