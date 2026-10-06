package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestMigrationV21ToV22AddsTheReplies builds a registry the v21 binary would
// have written (user_version 21) with an open PR whose Details were fetched
// and a merged one, and reopens it with Open: prs.replies_json and
// prs.replies_read_at exist and are empty, and the open PR's details_at is
// cleared so the next poll reads its remarks, while the merged one keeps it.
func TestMigrationV21ToV22AddsTheReplies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var setup []string
	for _, m := range migrations[:21] {
		setup = append(setup, m.sql)
	}
	setup = append(setup, "PRAGMA user_version = 21",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, gh_state, gh_updated_at, merged_at, closed_at, details_at, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewed', 'talkable-app', 'OPEN', '`+ts+`', NULL, NULL, '`+ts+`', '`+ts+`', '`+ts+`'),
		        (2, 1, 'PR_2', 8, 'u8', 'h2', '`+ts+`', 'closed', 'talkable-app', 'MERGED', '`+ts+`', '`+ts+`', '`+ts+`', '`+ts+`', '`+ts+`', '`+ts+`')`,
	)
	for _, q := range setup {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v21 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v21 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 22 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	open, _ := st.PRByID(ctx, 1)
	if open.Replies != nil || open.RepliesReadAt != nil || open.DetailsAt != nil {
		t.Fatalf("open PR: replies %+v, read at %v, details_at %v; want none until the next poll", open.Replies, open.RepliesReadAt, open.DetailsAt)
	}
	merged, _ := st.PRByID(ctx, 2)
	if merged.Replies != nil || merged.DetailsAt == nil {
		t.Fatalf("merged PR: replies %+v, details_at %v; want none and its details_at kept", merged.Replies, merged.DetailsAt)
	}
}

// The poller writes the replies the Details read without counting them as a
// GitHub change (they move no eligibility); an upsert without them keeps the
// stored ones, and new ones replace them.
func TestUpsertKeepsTheReplies(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	first := []Reply{{At: t0.Add(time.Minute), By: "alice", Thread: true}}
	in := GitHubPR{RepoID: repo.ID, NodeID: "PR_7", Number: 7, URL: "u7", HeadSHA: "h1", GHState: GHOpen,
		GHUpdatedAt: Ptr(t0), InitialState: PRBaseline, Identity: "talkable-app", Replies: first}
	res, err := st.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(res.PR.Replies, first, Reply.Equal) {
		t.Fatalf("inserted replies = %+v", res.PR.Replies)
	}
	in.Replies = nil
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || res.Changed || !slices.EqualFunc(res.PR.Replies, first, Reply.Equal) {
		t.Fatalf("no replies read: changed %v, replies %+v, %v", res.Changed, res.PR.Replies, err)
	}
	more := append(slices.Clone(first), Reply{At: t0.Add(2 * time.Minute), By: "alice"})
	in.Replies = more
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || res.Changed || !slices.EqualFunc(res.PR.Replies, more, Reply.Equal) {
		t.Fatalf("a new reply: changed %v, replies %+v, %v", res.Changed, res.PR.Replies, err)
	}
	in.Replies = []Reply{}
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || res.Changed || len(res.PR.Replies) != 0 || res.PR.Replies == nil {
		t.Fatalf("none left: changed %v, replies %#v, %v", res.Changed, res.PR.Replies, err)
	}
}

// A PR's pending replies are those after magnum's judge last read the
// threads (replies_read_at), else after its last review; a PR magnum never
// reviewed has none. The board counts them too.
func TestPendingRepliesAreThoseAfterTheJudgeLastReadTheThreads(t *testing.T) {
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	replies := []Reply{{At: at(1), By: "alice"}, {At: at(5), By: "alice", Thread: true}, {At: at(9), By: "coder[bot]", Thread: true}}
	for name, c := range map[string]struct {
		reviewed       string
		reviewedAt     *time.Time
		readAt         *time.Time
		want           int
		wantLatestMins int
	}{
		"never reviewed":                {reviewed: "", want: 0},
		"after the review":              {reviewed: "h1", reviewedAt: Ptr(at(2)), want: 2, wantLatestMins: 9},
		"after the judge read them":     {reviewed: "h1", reviewedAt: Ptr(at(2)), readAt: Ptr(at(6)), want: 1, wantLatestMins: 9},
		"read after the last one":       {reviewed: "h1", reviewedAt: Ptr(at(2)), readAt: Ptr(at(9)), want: 0},
		"a review without its time yet": {reviewed: "h1", want: 3, wantLatestMins: 9},
	} {
		t.Run(name, func(t *testing.T) {
			got := PendingReplies(replies, c.reviewed, c.reviewedAt, c.readAt)
			if len(got) != c.want {
				t.Fatalf("pending = %+v, want %d", got, c.want)
			}
			if c.want > 0 && !got[len(got)-1].At.Equal(at(c.wantLatestMins)) {
				t.Fatalf("latest pending = %v", got[len(got)-1].At)
			}
		})
	}

	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	res, err := st.UpsertPRFromGitHub(ctx, GitHubPR{RepoID: repo.ID, NodeID: "PR_7", Number: 7, URL: "u7", HeadSHA: "h1", GHState: GHOpen,
		GHUpdatedAt: Ptr(t0), InitialState: PRReviewed, Identity: "talkable-app", Replies: replies})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdatePR(ctx, res.PR.ID, func(u *PRUpdate) {
		u.Set("reviewed_sha", "h1")
		u.Set("reviewed_at", at(2))
		u.Set("replies_read_at", at(6))
	}); err != nil {
		t.Fatal(err)
	}
	pr, _ := st.PRByID(ctx, res.PR.ID)
	if n := len(pr.PendingReplies()); n != 1 || pr.RepliesReadAt == nil || !pr.RepliesReadAt.Equal(at(6)) {
		t.Fatalf("PR pending = %d, read at %v", n, pr.RepliesReadAt)
	}
	rows, err := st.Board(ctx, BoardFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("board: %d rows, %v", len(rows), err)
	}
	if rows[0].PendingReplies != 1 {
		t.Fatalf("board pending replies = %d, want 1", rows[0].PendingReplies)
	}
}
