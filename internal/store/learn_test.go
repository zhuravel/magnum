package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestMigrationV9ToV10Learn reopens a registry the previous binary wrote
// (user_version 9): the retro tables and their indexes appear empty, the
// PRs stay as they were, and the schema's CHECKs refuse an unknown status or
// class.
func TestMigrationV9ToV10Learn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var stmts []string
	for _, m := range migrations[:9] {
		stmts = append(stmts, m.sql)
	}
	pr := func(id int64, ghState string) string {
		return `INSERT INTO prs (id, repo_id, node_id, number, url, title, head_sha, head_changed_at, state, identity, created_at, updated_at,
		 gh_state, merged_at)
		 VALUES (` + itoa(id) + `, 1, 'PR_` + itoa(id) + `', ` + itoa(id) + `, 'u', 'title ` + itoa(id) + `', 'h1', '` + ts + `', 'reviewed', 'app', '` + ts + `', '` + ts + `',
		 '` + ghState + `', '` + ts + `')`
	}
	stmts = append(stmts, "PRAGMA user_version = 9",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		pr(1, "OPEN"), pr(2, "MERGED"))
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v9 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v9 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 10 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	for _, obj := range []struct{ kind, name string }{{"table", "retro_prs"}, {"table", "misses"},
		{"index", "misses_pr"}, {"index", "misses_class_state"}} {
		var n int
		if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?", obj.kind, obj.name).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s %s: %d, %v", obj.kind, obj.name, n, err)
		}
	}
	for _, table := range []string{"retro_prs", "misses"} {
		var n int
		if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s rows after the migration: %d, %v", table, n, err)
		}
	}
	for id, wantState := range map[int64]string{1: GHOpen, 2: GHMerged} {
		got, err := st.PRByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if Deref(got.Title) != "title "+itoa(id) || got.GHState != wantState || got.State != PRReviewed || got.HeadSHA != "h1" {
			t.Errorf("pr %d after the migration = %+v", id, got)
		}
	}

	// The CHECKs: a bad status, class, raised value, source or scope is refused whatever Go says.
	bad := map[string]string{
		"retro status": `INSERT INTO retro_prs (pr_id, retro_at, day, status) VALUES (1, '` + ts + `', '2026-10-03', 'bogus')`,
		"miss class": `INSERT INTO misses (pr_id, source_url, source_kind, reviewer, reviewed_sha, class, raised, created_at, updated_at)
			VALUES (1, 'u1', 'thread', 'bob-rev', 'h1', 'bogus', 'none', '` + ts + `', '` + ts + `')`,
		"miss raised": `INSERT INTO misses (pr_id, source_url, source_kind, reviewer, reviewed_sha, class, raised, created_at, updated_at)
			VALUES (1, 'u2', 'thread', 'bob-rev', 'h1', 'miss', 'bogus', '` + ts + `', '` + ts + `')`,
		"miss source": `INSERT INTO misses (pr_id, source_url, source_kind, reviewer, reviewed_sha, class, raised, created_at, updated_at)
			VALUES (1, 'u3', 'bogus', 'bob-rev', 'h1', 'miss', 'none', '` + ts + `', '` + ts + `')`,
		"miss scope": `INSERT INTO misses (pr_id, source_url, source_kind, reviewer, reviewed_sha, class, raised, scope, created_at, updated_at)
			VALUES (1, 'u4', 'thread', 'bob-rev', 'h1', 'miss', 'none', 'bogus', '` + ts + `', '` + ts + `')`,
	}
	for name, q := range bad {
		if _, err := st.DB().ExecContext(ctx, q); err == nil {
			t.Errorf("%s: the CHECK accepted a bad value", name)
		}
	}
	ok := `INSERT INTO retro_prs (pr_id, retro_at, day, status) VALUES (1, '` + ts + `', '2026-10-03', 'nothing')`
	if _, err := st.DB().ExecContext(ctx, ok); err != nil {
		t.Errorf("a good retro row: %v", err)
	}
	var attempts int
	if err := st.DB().QueryRowContext(ctx, "SELECT attempts FROM retro_prs WHERE pr_id = 1").Scan(&attempts); err != nil || attempts != 0 {
		t.Errorf("retro_prs.attempts of a new row = %d, %v; want 0", attempts, err)
	}
}

// mustClosePR makes PR id a closed or merged one: state is GHClosed or GHMerged.
func mustClosePR(t *testing.T, st *Store, id int64, state string, closed, merged *time.Time) {
	t.Helper()
	err := st.UpdatePR(context.Background(), id, func(u *PRUpdate) {
		u.Set("gh_state", state)
		u.Set("closed_at", closed)
		u.Set("merged_at", merged)
	})
	if err != nil {
		t.Fatalf("close PR %d: %v", id, err)
	}
}

// mustPostedRun creates a run of pr that posted review reviewID.
func mustPostedRun(t *testing.T, st *Store, prID int64, round int, reviewID int64) Run {
	t.Helper()
	r, err := st.CreateRun(context.Background(), Run{PRID: prID, Round: round, Role: RoleJudge, Kind: RunInitial, State: RunVerified,
		TargetSHA: "h1", Identity: "talkable-app", ReviewerLogin: "talkable[bot]", PromptText: "p", CreatedAt: t0,
		ReviewID: Ptr(reviewID)})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return r
}

func at(t time.Time) *time.Time { return &t }

func TestRecordRetroPRInsertsThenReplaces(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)

	if _, err := st.RetroPRByID(ctx, pr.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RetroPRByID before any retro = %v, want ErrNotFound", err)
	}
	// An empty time is now, an empty day is that moment's day, an empty error is NULL.
	if err := st.RecordRetroPR(ctx, RetroPR{PRID: pr.ID, Status: RetroNothing}); err != nil {
		t.Fatal(err)
	}
	got, err := st.RetroPRByID(ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := RetroPR{PRID: pr.ID, RetroAt: t0, Day: DayKey(t0), Status: RetroNothing}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("retro = %+v, want %+v", got, want)
	}
	var null bool
	if err := st.DB().QueryRowContext(ctx, "SELECT error IS NULL FROM retro_prs WHERE pr_id = ?", pr.ID).Scan(&null); err != nil || !null {
		t.Fatalf("error column NULL = %v, %v", null, err)
	}

	// Recording again replaces the row.
	clk.Add(24 * time.Hour)
	again := RetroPR{PRID: pr.ID, RetroAt: t0.Add(36 * time.Hour), Day: "2026-10-04", Status: RetroFailed, Candidates: 3, Error: "classifier timed out"}
	if err := st.RecordRetroPR(ctx, again); err != nil {
		t.Fatal(err)
	}
	again.Attempts = 1 // a failure counts an attempt (TestRecordRetroPRCountsFailedAttempts)
	if got, err := st.RetroPRByID(ctx, pr.ID); err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("replaced retro = %+v, %v\nwant %+v", got, err, again)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM retro_prs").Scan(&n); err != nil || n != 1 {
		t.Fatalf("retro rows = %d, %v; want the one PR's single row", n, err)
	}
	// Replacing with no error clears the old one.
	if err := st.RecordRetroPR(ctx, RetroPR{PRID: pr.ID, Status: RetroClassified, Candidates: 2}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.RetroPRByID(ctx, pr.ID); got.Error != "" || got.Status != RetroClassified || got.Candidates != 2 || !got.RetroAt.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("retro after a clean re-run = %+v", got)
	}
}

func TestRecordRetroPRRefusesBadRecords(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	for _, status := range []string{RetroNothing, RetroClassified, RetroUnclassified, RetroFailed} {
		if err := st.RecordRetroPR(ctx, RetroPR{PRID: pr.ID, Status: status}); err != nil {
			t.Errorf("status %s: %v", status, err)
		}
	}
	for name, r := range map[string]RetroPR{
		"no status":  {PRID: pr.ID},
		"bad status": {PRID: pr.ID, Status: "done"},
		"no pr":      {Status: RetroNothing},
		"unknown pr": {PRID: 999, Status: RetroNothing},
	} {
		if err := st.RecordRetroPR(ctx, r); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	err := st.RecordRetroPR(ctx, RetroPR{PRID: pr.ID, Status: "done"})
	if err == nil || !strings.Contains(err.Error(), `status "done"`) {
		t.Fatalf("bad status error = %v, want it to name the status", err)
	}
}

func TestUpsertMissKeepsIDStateAndCreatedAt(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	in := Miss{PRID: pr.ID, SourceURL: "https://github.com/talkable/talkable/pull/7#discussion_r1", SourceKind: MissSourceThread,
		Reviewer: "bob-rev", Path: "app/a.rb", Line: 12, ReviewedSHA: "sha-7"}

	first, err := st.UpsertMiss(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	want := Miss{ID: first.ID, PRID: pr.ID, SourceURL: in.SourceURL, SourceKind: MissSourceThread, Reviewer: "bob-rev",
		Path: "app/a.rb", Line: 12, ReviewedSHA: "sha-7", Class: MissUnclassified, Raised: MissRaisedNone,
		Lines: []int{}, Match: []string{}, State: MissNew, CreatedAt: t0, UpdatedAt: t0}
	if first.ID == 0 || !reflect.DeepEqual(first, want) {
		t.Fatalf("inserted miss = %+v\nwant %+v", first, want)
	}
	var nulls bool
	err = st.DB().QueryRowContext(ctx, `SELECT severity IS NULL AND finding_ref IS NULL AND reason_code IS NULL AND title IS NULL
		AND lesson IS NULL AND scope IS NULL FROM misses WHERE id = ?`, first.ID).Scan(&nulls)
	if err != nil || !nulls {
		t.Fatalf("empty optional columns are NULL = %v, %v", nulls, err)
	}

	// Someone dismissed it; the next retro finds the same comment and classifies it.
	clk.Add(time.Hour)
	if _, err := st.DB().ExecContext(ctx, "UPDATE misses SET state = ? WHERE id = ?", MissDismissed, first.ID); err != nil {
		t.Fatal(err)
	}
	next := in
	next.Class, next.Severity, next.Raised, next.FindingRef, next.ReasonCode = MissMiss, "P2", MissRaisedRejected, "F2", "speculative"
	next.Title, next.Lesson, next.Scope = "Nil receiver", "Check the receiver before calling it", MissScopeRepo
	next.Lines, next.Match = []int{12, 13}, []string{"nil", "receiver"}
	next.State = MissUsed // an existing miss keeps its state whatever the caller says
	second, err := st.UpsertMiss(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	want = next
	want.ID, want.State, want.CreatedAt, want.UpdatedAt = first.ID, MissDismissed, t0, t0.Add(time.Hour)
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("updated miss = %+v\nwant %+v", second, want)
	}
	if n, err := st.CountMisses(ctx, MissFilter{}); err != nil || n != 1 {
		t.Fatalf("misses after the upsert = %d, %v; want 1", n, err)
	}

	// Every other column takes the caller's value, an emptied one included,
	// except what comes with the class: an unclassified write keeps it
	// (TestUpsertMissNeverDowngradesAClassification).
	clk.Add(time.Hour)
	third, err := st.UpsertMiss(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	want = Miss{ID: first.ID, PRID: pr.ID, SourceURL: in.SourceURL, SourceKind: MissSourceThread, Reviewer: "bob-rev",
		Path: "app/a.rb", Line: 12, ReviewedSHA: "sha-7", Class: second.Class, Severity: second.Severity, Raised: MissRaisedNone,
		Title: second.Title, Lesson: second.Lesson, Scope: second.Scope, Lines: second.Lines, Match: second.Match,
		State: MissDismissed, CreatedAt: t0, UpdatedAt: t0.Add(2 * time.Hour)}
	if !reflect.DeepEqual(third, want) {
		t.Fatalf("miss upserted with empty fields = %+v\nwant %+v", third, want)
	}
}

func TestUpsertMissRefusesBadMisses(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	good := Miss{PRID: pr.ID, SourceURL: "https://github.com/talkable/talkable/pull/7#discussion_r1", SourceKind: MissSourceReview,
		Reviewer: "bob-rev", ReviewedSHA: "sha-7"}
	cases := []struct {
		name   string
		mutate func(*Miss)
		want   string // a word the error must carry
	}{
		{"no pr", func(m *Miss) { m.PRID = 0 }, "required"},
		{"no source url", func(m *Miss) { m.SourceURL = "" }, "required"},
		{"no source kind", func(m *Miss) { m.SourceKind = "" }, "required"},
		{"no reviewer", func(m *Miss) { m.Reviewer = "" }, "required"},
		{"no reviewed sha", func(m *Miss) { m.ReviewedSHA = "" }, "required"},
		{"bad source kind", func(m *Miss) { m.SourceKind = "comment" }, "source_kind"},
		{"bad class", func(m *Miss) { m.Class = "bug" }, "class"},
		{"bad raised", func(m *Miss) { m.Raised = "accepted" }, "raised"},
		{"bad state", func(m *Miss) { m.State = "open" }, "state"},
		{"bad scope", func(m *Miss) { m.Scope = "global" }, "scope"},
		{"unknown pr", func(m *Miss) { m.PRID = 999 }, "FOREIGN KEY"},
	}
	for _, c := range cases {
		m := good
		c.mutate(&m)
		if _, err := st.UpsertMiss(ctx, m); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want one mentioning %q", c.name, err, c.want)
		}
	}
	if n, err := st.CountMisses(ctx, MissFilter{}); err != nil || n != 0 {
		t.Fatalf("misses after refused upserts = %d, %v", n, err)
	}
}

func TestMissesFilterAndOrder(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr7 := mustPR(t, st, repo.ID, 7, PRReviewed)
	pr8 := mustPR(t, st, repo.ID, 8, PRReviewed)
	add := func(pr PR, name, class string, d time.Duration) Miss {
		t.Helper()
		clk.Set(t0.Add(d))
		m, err := st.UpsertMiss(ctx, Miss{PRID: pr.ID, SourceURL: "https://github.com/talkable/talkable/pull/" + itoa(int64(pr.Number)) + "#" + name,
			SourceKind: MissSourceThread, Reviewer: "bob-rev", ReviewedSHA: "sha", Class: class})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a := add(pr7, "a", MissMiss, 0)
	add(pr7, "b", MissStyle, time.Minute)
	c := add(pr8, "c", MissMiss, 2*time.Minute)
	add(pr8, "d", MissUnclassified, 3*time.Minute)
	add(pr8, "e", MissNotIssue, 3*time.Minute) // same instant as d: the later id comes first
	if _, err := st.DB().ExecContext(ctx, "UPDATE misses SET state = ? WHERE id = ?", MissUsed, c.ID); err != nil {
		t.Fatal(err)
	}

	names := func(ms []Miss) string {
		var out []string
		for _, m := range ms {
			out = append(out, m.SourceURL[strings.LastIndex(m.SourceURL, "#")+1:])
		}
		return strings.Join(out, "")
	}
	cases := []struct {
		name string
		f    MissFilter
		want string
	}{
		{"everything, newest first", MissFilter{}, "edcba"},
		{"one class", MissFilter{Classes: []string{MissMiss}}, "ca"},
		{"two classes", MissFilter{Classes: []string{MissMiss, MissStyle}}, "cba"},
		{"one state", MissFilter{States: []string{MissNew}}, "edba"},
		{"another state", MissFilter{States: []string{MissUsed}}, "c"},
		{"one PR", MissFilter{PRID: pr7.ID}, "ba"},
		{"class and state", MissFilter{Classes: []string{MissMiss}, States: []string{MissNew}}, "a"},
		{"class, state and PR", MissFilter{Classes: []string{MissMiss}, States: []string{MissUsed}, PRID: pr7.ID}, ""},
		{"a class nothing has", MissFilter{Classes: []string{MissOutside}}, ""},
		{"a PR nothing belongs to", MissFilter{PRID: 999}, ""},
	}
	for _, tc := range cases {
		got, err := st.Misses(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if names(got) != tc.want {
			t.Errorf("%s: misses = %q, want %q", tc.name, names(got), tc.want)
		}
		if n, err := st.CountMisses(ctx, tc.f); err != nil || n != len(tc.want) {
			t.Errorf("%s: CountMisses = %d, %v; want %d", tc.name, n, err, len(tc.want))
		}
	}

	all, _ := st.Misses(ctx, MissFilter{})
	for _, m := range all {
		wantNumber := 8
		if m.PRID == pr7.ID {
			wantNumber = 7
		}
		if m.Repo != "talkable/talkable" || m.Number != wantNumber {
			t.Errorf("miss %d: repo %q number %d, want talkable/talkable #%d", m.ID, m.Repo, m.Number, wantNumber)
		}
	}
	// The row itself reads the same through Misses as through UpsertMiss, apart from the PR's name.
	got := all[len(all)-1]
	got.Repo, got.Number = "", 0
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("miss a through Misses = %+v\nthrough UpsertMiss %+v", got, a)
	}
}

func TestRetroDueSelectsClosedReviewedPRsNewestFirst(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	since := t0.Add(-48 * time.Hour)
	nums := func(prs []PR) []int {
		var out []int
		for _, p := range prs {
			out = append(out, p.Number)
		}
		return out
	}
	due := func(q RetroQuery) []int {
		t.Helper()
		prs, err := st.RetroDue(ctx, q)
		if err != nil {
			t.Fatalf("RetroDue(%+v): %v", q, err)
		}
		return nums(prs)
	}
	newPR := func(number int) PR { return mustPR(t, st, repo.ID, number, PRReviewed) }

	open7 := newPR(7) // still open: not due
	mustPostedRun(t, st, open7.ID, 1, 1)

	old8 := newPR(8) // closed ten days ago: outside Since
	mustClosePR(t, st, old8.ID, GHClosed, at(t0.Add(-240*time.Hour)), nil)
	mustPostedRun(t, st, old8.ID, 1, 2)

	silent9 := newPR(9) // merged, but no run posted a review
	mustClosePR(t, st, silent9.ID, GHMerged, nil, at(t0.Add(-time.Hour)))
	mustRunAt(t, st, silent9.ID, 1, RoleJudge, 0)

	done10 := newPR(10) // merged, already looked at
	mustClosePR(t, st, done10.ID, GHMerged, nil, at(t0.Add(-2*time.Hour)))
	mustPostedRun(t, st, done10.ID, 1, 3)
	if err := st.RecordRetroPR(ctx, RetroPR{PRID: done10.ID, Status: RetroNothing}); err != nil {
		t.Fatal(err)
	}

	merged11 := newPR(11) // merged, closed_at unset: merged_at counts; several posted runs still list it once
	mustClosePR(t, st, merged11.ID, GHMerged, nil, at(t0.Add(-3*time.Hour)))
	mustPostedRun(t, st, merged11.ID, 1, 4)
	mustPostedRun(t, st, merged11.ID, 2, 5)
	mustRunAt(t, st, merged11.ID, 3, RoleJudge, 0)

	closed12 := newPR(12) // closed an hour ago
	mustClosePR(t, st, closed12.ID, GHClosed, at(t0.Add(-time.Hour)), nil)
	mustPostedRun(t, st, closed12.ID, 1, 6)

	merged13 := newPR(13) // merged long ago but closed_at is later: closed_at wins
	mustClosePR(t, st, merged13.ID, GHMerged, at(t0.Add(-30*time.Minute)), at(t0.Add(-5*time.Hour)))
	mustPostedRun(t, st, merged13.ID, 1, 7)

	closed14 := newPR(14) // closed at the same instant as 12: the later id comes first
	mustClosePR(t, st, closed14.ID, GHClosed, at(t0.Add(-time.Hour)), nil)
	mustPostedRun(t, st, closed14.ID, 1, 8)

	if got, want := due(RetroQuery{Since: since}), []int{13, 14, 12, 11}; !reflect.DeepEqual(got, want) {
		t.Errorf("due since %s = %v, want %v", since.Format(time.RFC3339), got, want)
	}
	if got, want := due(RetroQuery{Since: since, Again: true}), []int{13, 14, 12, 10, 11}; !reflect.DeepEqual(got, want) {
		t.Errorf("due again = %v, want %v", got, want)
	}
	if got, want := due(RetroQuery{Since: t0.Add(-90 * time.Minute)}), []int{13, 14, 12}; !reflect.DeepEqual(got, want) {
		t.Errorf("due since 90 minutes ago = %v, want %v", got, want)
	}
	if got := due(RetroQuery{Since: t0}); len(got) != 0 {
		t.Errorf("due since now = %v, want none", got)
	}
	// PRIDs names the PRs and ignores Since; a PR still has to be closed, reviewed and (unless Again) not looked at.
	ids := []int64{old8.ID, open7.ID, silent9.ID, done10.ID, merged11.ID}
	if got, want := due(RetroQuery{Since: t0, PRIDs: ids}), []int{11, 8}; !reflect.DeepEqual(got, want) {
		t.Errorf("due by id = %v, want %v", got, want)
	}
	if got, want := due(RetroQuery{PRIDs: ids, Again: true}), []int{10, 11, 8}; !reflect.DeepEqual(got, want) {
		t.Errorf("due by id again = %v, want %v", got, want)
	}
	// A due PR comes back whole.
	prs, _ := st.RetroDue(ctx, RetroQuery{Since: since})
	if len(prs) == 0 || prs[0].ID != merged13.ID || prs[0].GHState != GHMerged || Deref(prs[0].Title) != "PR 13" || prs[0].ClosedAt == nil || !prs[0].ClosedAt.Equal(t0.Add(-30*time.Minute)) {
		t.Fatalf("first due PR = %+v", prs)
	}
}

// TestRecordRetroPRCountsFailedAttempts: each failed retro of a PR counts
// an attempt; any other outcome starts the count again.
func TestRecordRetroPRCountsFailedAttempts(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	pr := mustPR(t, st, mustRepo(t, st).ID, 7, PRReviewed)
	for i, tc := range []struct {
		status string
		want   int
	}{{RetroFailed, 1}, {RetroFailed, 2}, {RetroClassified, 0}, {RetroFailed, 1}} {
		if err := st.RecordRetroPR(ctx, RetroPR{PRID: pr.ID, Status: tc.status, Error: "boom"}); err != nil {
			t.Fatal(err)
		}
		got, err := st.RetroPRByID(ctx, pr.ID)
		if err != nil || got.Attempts != tc.want {
			t.Fatalf("step %d (%s): attempts = %d, %v; want %d", i+1, tc.status, got.Attempts, err, tc.want)
		}
	}
}

// TestRetroDueRetriesFailedPRsUpToThreeAttempts: a PR whose retro failed
// stays due until RetroMaxAttempts failures; one that finished otherwise
// does not; Again takes every PR whatever its record.
func TestRetroDueRetriesFailedPRsUpToThreeAttempts(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	record := func(pr PR, status string, times int) {
		for range times {
			if err := st.RecordRetroPR(ctx, RetroPR{PRID: pr.ID, Status: status}); err != nil {
				t.Fatal(err)
			}
		}
	}
	var prs []PR
	for i, n := range []int{21, 22, 23, 24} {
		pr := mustPR(t, st, repo.ID, n, PRReviewed)
		mustClosePR(t, st, pr.ID, GHMerged, nil, at(t0.Add(-time.Duration(i+1)*time.Hour)))
		mustPostedRun(t, st, pr.ID, 1, int64(100+n))
		prs = append(prs, pr)
	}
	record(prs[0], RetroFailed, 1)
	record(prs[1], RetroFailed, RetroMaxAttempts-1)
	record(prs[2], RetroFailed, RetroMaxAttempts)
	record(prs[3], RetroClassified, 1)
	got, err := st.RetroDue(ctx, RetroQuery{Since: t0.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, p := range got {
		nums = append(nums, p.Number)
	}
	if want := []int{21, 22}; !reflect.DeepEqual(nums, want) {
		t.Fatalf("due = %v, want %v (failed below %d attempts)", nums, want, RetroMaxAttempts)
	}
	if all, _ := st.RetroDue(ctx, RetroQuery{Since: t0.Add(-24 * time.Hour), Again: true}); len(all) != 4 {
		t.Fatalf("due again = %d PRs, want 4", len(all))
	}
}

// TestUpsertMissNeverDowngradesAClassification: an unclassified write (a
// retro whose classifier failed) keeps what an earlier retro classified;
// any other class replaces it.
func TestUpsertMissNeverDowngradesAClassification(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	pr := mustPR(t, st, mustRepo(t, st).ID, 7, PRReviewed)
	base := Miss{PRID: pr.ID, SourceURL: "https://github.com/talkable/talkable/pull/7#discussion_r1", SourceKind: MissSourceThread,
		Reviewer: "rev-ann", Path: "a.rb", Line: 4, ReviewedSHA: "h1"}
	miss := base
	miss.Class, miss.Severity, miss.Title, miss.Lesson, miss.Scope = MissMiss, "P2", "Scope lost", "When X, check Y.", MissScopeGeneral
	miss.Lines, miss.Match = []int{3, 4}, []string{"scope"}
	if _, err := st.UpsertMiss(ctx, miss); err != nil {
		t.Fatal(err)
	}
	unclassified := base
	unclassified.Lines = []int{4, 4}
	got, err := st.UpsertMiss(ctx, unclassified)
	if err != nil {
		t.Fatal(err)
	}
	if got.Class != MissMiss || got.Severity != "P2" || got.Title != "Scope lost" || got.Lesson == "" || got.Scope != MissScopeGeneral ||
		!reflect.DeepEqual(got.Lines, []int{3, 4}) || !reflect.DeepEqual(got.Match, []string{"scope"}) {
		t.Fatalf("after an unclassified write = %+v", got)
	}
	style := base
	style.Class = MissStyle
	if got, err = st.UpsertMiss(ctx, style); err != nil || got.Class != MissStyle || got.Title != "" || got.Severity != "" {
		t.Fatalf("a new class replaces the old one: %+v, %v", got, err)
	}
}
