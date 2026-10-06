package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

const stalemateHead = "abcdef0123456789abcdef0123456789abcdef01" // inspSeedPR's head

// prsSeedReplies seeds #11960 (reviewed on its head, two replies an hour
// ago that its judge has not re-decided, one thread magnum stopped arguing
// in), #11961 (reviewed, no reply) and #11962 (merged, a stalemate left
// behind), and returns the first one's id.
func prsSeedReplies(t *testing.T, f *inspFixture) {
	t.Helper()
	st := f.store()
	defer st.Close()
	ctx := context.Background()
	at := func(d time.Duration) time.Time { return time.Now().Add(-d) }
	replies, err := json.Marshal([]store.Reply{{At: at(time.Hour), By: "alice", Thread: true}, {At: at(30 * time.Minute), By: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	reviewed := func(u *store.PRUpdate) {
		u.Set("gh_updated_at", store.FormatTime(at(time.Hour)))
		u.Set("reviewed_sha", stalemateHead)
		u.Set("last_review_event", "CHANGES_REQUESTED")
		u.Set("reviewed_at", store.FormatTime(at(2*time.Hour)))
		u.Set("last_review_login", "talkable")
	}
	_, a := inspSeedPR(t, st, "talkable/talkable", 11960, store.PRReviewed, func(u *store.PRUpdate) {
		reviewed(u)
		u.Set("replies_json", string(replies))
	})
	_, b := inspSeedPR(t, st, "talkable/talkable", 11961, store.PRReviewed, reviewed)
	_, c := inspSeedPR(t, st, "talkable/talkable", 11962, store.PRReleased, func(u *store.PRUpdate) {
		reviewed(u)
		u.Set("gh_state", store.GHMerged)
	})
	stale := func(id int64, threads ...engine.StalemateThread) {
		v, err := json.Marshal(engine.Stalemate{Threads: threads})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetKV(ctx, engine.KVPRStalemate(id), string(v)); err != nil {
			t.Fatal(err)
		}
	}
	stale(a.ID, engine.StalemateThread{ID: "PRRT_1", URL: "https://github.com/talkable/talkable/pull/11960#discussion_r11"},
		engine.StalemateThread{ID: "PRRT_2"}) // a thread without a URL is named by its id
	stale(c.ID, engine.StalemateThread{ID: "PRRT_3", URL: "https://github.com/talkable/talkable/pull/11962#discussion_r33"})
	if err := st.SetKV(ctx, engine.KVPRStalemate(b.ID), "not json"); err != nil { // an unreadable value flags nothing
		t.Fatal(err)
	}
}

// The board's rows carry the replies waiting for the judge and the threads
// magnum stopped arguing in, read from the registry; a PR GitHub merged has
// no stalemate to decide.
func TestPRsSourceCarriesTheRepliesAndTheStalemate(t *testing.T) {
	f := newInspFixture(t)
	prsSeedReplies(t, f)
	st := f.store()
	defer st.Close()
	rows, err := prsSource(st, nil, store.BoardFilter{}, nil, f.Ctx.Layout)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]tui.PRBoardRow{}
	for _, r := range rows {
		got[r.Number] = r
	}
	if r := got[11960]; r.Replies != 2 || !slices.Equal(r.Stalemate, []string{"https://github.com/talkable/talkable/pull/11960#discussion_r11", "PRRT_2"}) {
		t.Errorf("#11960: replies %d, stalemate %v", r.Replies, r.Stalemate)
	}
	if r := got[11961]; r.Replies != 0 || len(r.Stalemate) != 0 {
		t.Errorf("#11961: replies %d, stalemate %v", r.Replies, r.Stalemate)
	}
	if r, ok := got[11962]; ok && len(r.Stalemate) != 0 {
		t.Errorf("a merged PR is flagged: %v", r.Stalemate)
	}
}

// `prs --json` gains pending_replies and stalemate_threads (a list: [] when
// there is none), and the table says them in words.
func TestPRsJSONAndTableSayTheRepliesAndTheStalemate(t *testing.T) {
	f := newInspFixture(t)
	prsSeedReplies(t, f)
	if code := f.run("prs", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatalf("%v\n%s", err, f.Out.String())
	}
	by := map[string]map[string]any{}
	for _, r := range rows {
		by[r["ref"].(string)] = r
	}
	a, b := by["talkable/talkable#11960"], by["talkable/talkable#11961"]
	if a == nil || b == nil {
		t.Fatalf("rows %v", by)
	}
	if a["pending_replies"] != 2.0 || !slices.Equal(anyStrings(a["stalemate_threads"]), []string{"https://github.com/talkable/talkable/pull/11960#discussion_r11", "PRRT_2"}) {
		t.Errorf("#11960: pending_replies %v, stalemate_threads %v", a["pending_replies"], a["stalemate_threads"])
	}
	if v, ok := b["pending_replies"]; !ok || v != 0.0 {
		t.Errorf("#11961: pending_replies %v (present %v)", v, ok)
	}
	if v, ok := b["stalemate_threads"].([]any); !ok || len(v) != 0 {
		t.Errorf("#11961: stalemate_threads %#v, want []", b["stalemate_threads"])
	}

	f.Out.Reset()
	if code := f.run("prs"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var withReplies, without string
	for _, l := range strings.Split(f.Out.String(), "\n") {
		switch {
		case strings.HasPrefix(l, "talkable#11960"):
			withReplies = l
		case strings.HasPrefix(l, "talkable#11961"):
			without = l
		}
	}
	for _, want := range []string{"changes_requested 2h by talkable, 2 replies", "reviewed,stalemate"} {
		if !strings.Contains(withReplies, want) {
			t.Errorf("#11960 row lacks %q:\n%s", want, withReplies)
		}
	}
	if strings.Contains(without, "repl") || strings.Contains(without, "stalemate") {
		t.Errorf("#11961 row mentions replies or a stalemate:\n%s", without)
	}
}

// The table's LAST REVIEW cell: the replies follow the review, or stand
// alone when the PR shows none.
func TestPRsTableLastReviewCellCountsTheReplies(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	li := &tui.ReviewInfo{Login: "talkable", Event: "APPROVED", SubmittedAt: now.Add(-3 * time.Hour)}
	for _, tc := range []struct {
		r    tui.PRBoardRow
		want string
	}{
		{tui.PRBoardRow{LastReview: li}, "approved 3h by talkable"},
		{tui.PRBoardRow{LastReview: li, Replies: 1}, "approved 3h by talkable, 1 reply"},
		{tui.PRBoardRow{LastReview: li, Replies: 3}, "approved 3h by talkable, 3 replies"},
		{tui.PRBoardRow{Replies: 2}, "2 replies"},
		{tui.PRBoardRow{}, "-"},
	} {
		if got := prsLastReviewCellOf(tc.r, now); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.r, got, tc.want)
		}
	}
}
