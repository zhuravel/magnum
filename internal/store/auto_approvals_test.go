package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Migration 0020 adds the automatic approvals and the PRs the operator
// stopped them on to a registry the previous binary wrote (user_version 19),
// whose PRs stay as they were.
func TestMigrationV19ToV20AddsAutoApprovals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var stmts []string
	for _, m := range migrations[:19] {
		stmts = append(stmts, m.sql)
	}
	stmts = append(stmts, "PRAGMA user_version = 19",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, created_at, updated_at, gh_state)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewed', 'talkable-app', '`+ts+`', '`+ts+`', 'OPEN')`)
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v19 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v19 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 20 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	if pr, err := st.PRByID(ctx, 1); err != nil || pr.State != PRReviewed || pr.HeadSHA != "h1" {
		t.Fatalf("PR after the migration = %+v, %v", pr, err)
	}
	if _, ok, err := st.LiveAutoApproval(ctx, 1); ok || err != nil {
		t.Fatalf("a migrated PR has a live approval (%v)", err)
	}
	if _, err := st.InsertAutoApproval(ctx, AutoApproval{PRID: 1, RunID: "r1", HeadSHA: "h1", Identity: "zhuravel", Login: "zhuravel"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAutoApproveHold(ctx, AutoApproveHold{PRID: 1, Held: true, Reason: "you reviewed it by hand", At: t0}); err != nil {
		t.Fatal(err)
	}
}

// A PR has at most one live automatic approval (posting, standing or
// dismissing): a second post is a conflict until the first failed or was
// withdrawn. Every transition is compare-and-set on the state.
func TestOnlyOneAutoApprovalOfAPRIsLive(t *testing.T) {
	st, c := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	a, err := st.InsertAutoApproval(ctx, AutoApproval{PRID: pr.ID, RunID: "r1", SourceReviewID: 300, SourceURL: "u300",
		HeadSHA: pr.HeadSHA, Identity: "zhuravel", Login: "zhuravel"})
	if err != nil || a.ID == 0 || a.State != AutoPosting || a.Attempts != 1 || !a.CreatedAt.Equal(t0) {
		t.Fatalf("insert = %+v, %v", a, err)
	}
	if _, err := st.InsertAutoApproval(ctx, AutoApproval{PRID: pr.ID, RunID: "r2", HeadSHA: pr.HeadSHA, Identity: "zhuravel", Login: "zhuravel"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second live approval = %v, want ErrConflict", err)
	}
	c.now = t0.Add(time.Minute)
	if err := st.TransitionAutoApproval(ctx, a.ID, []string{AutoPosting}, AutoStanding, func(u *AutoApprovalUpdate) {
		u.Set("review_id", int64(901))
		u.Set("review_url", "u901")
		u.Set("posted_at", c.now)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionAutoApproval(ctx, a.ID, []string{AutoPosting}, AutoFailed, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale transition = %v, want ErrConflict", err)
	}
	live, ok, err := st.LiveAutoApproval(ctx, pr.ID)
	if err != nil || !ok || live.ID != a.ID || live.State != AutoStanding || live.ReviewID != 901 || live.PostedAt == nil || !live.PostedAt.Equal(c.now) {
		t.Fatalf("live = %+v, %v, %v", live, ok, err)
	}
	if got, ok, err := st.AutoApprovalOfRun(ctx, pr.ID, "r1"); err != nil || !ok || got.ID != a.ID {
		t.Fatalf("of run r1 = %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := st.AutoApprovalOfRun(ctx, pr.ID, "r2"); ok {
		t.Fatal("run r2 has an approval")
	}
	if err := st.TransitionAutoApproval(ctx, a.ID, []string{AutoStanding}, AutoDismissed, func(u *AutoApprovalUpdate) {
		u.Set("ended_by", AutoEndedMagnum)
		u.Set("ended_at", c.now)
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.LiveAutoApproval(ctx, pr.ID); ok {
		t.Fatal("a dismissed approval is live")
	}
	b, err := st.InsertAutoApproval(ctx, AutoApproval{PRID: pr.ID, RunID: "r2", HeadSHA: pr.HeadSHA, Identity: "zhuravel", Login: "zhuravel"})
	if err != nil {
		t.Fatalf("after the dismissal: %v", err)
	}
	latest, err := st.LatestAutoApprovals(ctx, []int64{pr.ID, 999})
	if err != nil || len(latest) != 1 || latest[pr.ID].ID != b.ID {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
}

// The standing approvals are those of open PRs; the day's count is every
// approval posted since then, withdrawn or not.
func TestStandingAutoApprovalsAndTheDaysCount(t *testing.T) {
	st, c := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	post := func(n int, at time.Time, end bool) PR {
		pr := mustPR(t, st, repo.ID, n, PRReviewed)
		a, err := st.InsertAutoApproval(ctx, AutoApproval{PRID: pr.ID, RunID: "r", HeadSHA: pr.HeadSHA, Identity: "zhuravel", Login: "zhuravel"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.TransitionAutoApproval(ctx, a.ID, []string{AutoPosting}, AutoStanding, func(u *AutoApprovalUpdate) {
			u.Set("review_id", int64(900+n))
			u.Set("posted_at", at)
		}); err != nil {
			t.Fatal(err)
		}
		if end {
			if err := st.TransitionAutoApproval(ctx, a.ID, []string{AutoStanding}, AutoDismissed, nil); err != nil {
				t.Fatal(err)
			}
		}
		return pr
	}
	post(1, t0.Add(-26*time.Hour), false) // yesterday, standing
	post(2, t0.Add(-time.Hour), true)     // today, withdrawn
	closed := post(3, t0.Add(-2*time.Hour), false)
	post(4, t0, false)
	if err := st.UpdatePR(ctx, closed.ID, func(u *PRUpdate) { u.Set("gh_state", GHMerged) }); err != nil {
		t.Fatal(err)
	}
	c.now = t0
	got, err := st.StandingAutoApprovals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, a := range got {
		nums = append(nums, a.PR.Number)
		if a.Repo != "talkable/talkable" {
			t.Errorf("repo = %q", a.Repo)
		}
	}
	if len(nums) != 2 || nums[0] != 1 || nums[1] != 4 {
		t.Fatalf("standing on open PRs = %v, want [1 4]", nums)
	}
	n, err := st.CountAutoApprovedSince(ctx, t0.Add(-12*time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("since this morning = %d, %v, want 3", n, err)
	}
}

// A hold is one row per PR: the operator's last word wins.
func TestAutoApproveHoldIsThePRsLastWord(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	if _, ok, err := st.AutoApproveHold(ctx, pr.ID); ok || err != nil {
		t.Fatalf("a hold before any: %v", err)
	}
	if err := st.SetAutoApproveHold(ctx, AutoApproveHold{PRID: pr.ID, Held: true, Reason: "dismissed by you", At: t0}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAutoApproveHold(ctx, AutoApproveHold{PRID: pr.ID, Held: false, Reason: "resumed", At: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h, ok, err := st.AutoApproveHold(ctx, pr.ID)
	if err != nil || !ok || h.Held || h.Reason != "resumed" || !h.At.Equal(t0.Add(time.Hour)) {
		t.Fatalf("hold = %+v, %v, %v", h, ok, err)
	}
	all, err := st.AutoApproveHolds(ctx, []int64{pr.ID, 999})
	if err != nil || len(all) != 1 || all[pr.ID].Reason != "resumed" {
		t.Fatalf("holds = %+v, %v", all, err)
	}
}

// The candidates for an automatic approval are the open reviewed PRs whose
// review covers the head, not drafts, not muted and without a live one.
func TestAutoApproveCandidatesAreReviewedOpenPRs(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	reviewed := func(n int, set func(u *PRUpdate)) PR {
		pr := mustPR(t, st, repo.ID, n, PRReviewed)
		if err := st.UpdatePR(ctx, pr.ID, func(u *PRUpdate) {
			u.Set("reviewed_sha", pr.HeadSHA)
			if set != nil {
				set(u)
			}
		}); err != nil {
			t.Fatal(err)
		}
		return pr
	}
	reviewed(1, nil)
	reviewed(2, func(u *PRUpdate) { u.Set("is_draft", true) })
	reviewed(3, func(u *PRUpdate) { u.Set("muted", true) })
	reviewed(4, func(u *PRUpdate) { u.Set("gh_state", GHMerged) })
	older := reviewed(5, nil)
	if err := st.UpdatePR(ctx, older.ID, func(u *PRUpdate) { u.Set("reviewed_sha", "older") }); err != nil {
		t.Fatal(err)
	}
	approved := reviewed(6, nil)
	mustPR(t, st, repo.ID, 7, PRQueued)
	if _, err := st.InsertAutoApproval(ctx, AutoApproval{PRID: approved.ID, RunID: "r", HeadSHA: approved.HeadSHA, Identity: "zhuravel", Login: "zhuravel"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.AutoApproveCandidates(ctx)
	if err != nil || len(got) != 1 || got[0].PR.Number != 1 || got[0].Repo != "talkable/talkable" {
		t.Fatalf("candidates = %+v, %v", got, err)
	}
}

// A review summary names the review the round posted.
func TestReviewSummaryCarriesTheReviewID(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login, state, outcome, review_id, review_url, prompt_text, result_json, created_at)
		VALUES ('r1', ?, 1, 'codex-judge', 'initial', 'sha-7', 'talkable-app', 'talkable[bot]', 'verified', 'posted', 345, 'u345', 'p', ?, ?)`,
		pr.ID, `{"event":"APPROVE","verdict":"clean","findings":{}}`, FormatTime(t0)); err != nil {
		t.Fatal(err)
	}
	sums, err := st.LastReviewSummaries(ctx, []int64{pr.ID})
	if err != nil || sums[pr.ID].ReviewID != 345 || sums[pr.ID].RunID != "r1" || sums[pr.ID].URL != "u345" {
		t.Fatalf("summary = %+v, %v", sums[pr.ID], err)
	}
}
