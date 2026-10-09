package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestMigrationV22ToV23Declines reopens a registry the previous binary
// wrote (user_version 22): the declines table and its indexes appear empty,
// and the schema's CHECKs refuse an unknown outcome, verdict, scope or
// state.
func TestMigrationV22ToV23Declines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var stmts []string
	for _, m := range migrations[:22] {
		stmts = append(stmts, m.sql)
	}
	stmts = append(stmts, "PRAGMA user_version = 22",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, title, head_sha, head_changed_at, state, identity, created_at, updated_at, gh_state)
		 VALUES (1, 1, 'PR_1', 1, 'u', 't', 'h1', '`+ts+`', 'reviewed', 'app', '`+ts+`', '`+ts+`', 'MERGED')`)
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v22 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v22 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 23 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	for _, obj := range []struct{ kind, name string }{{"table", "declines"}, {"index", "declines_pr"},
		{"index", "declines_state"}, {"index", "declines_skill"}} {
		var n int
		if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?", obj.kind, obj.name).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s %s: %d, %v", obj.kind, obj.name, n, err)
		}
	}
	if n, err := st.CountDeclines(ctx, DeclineFilter{}); err != nil || n != 0 {
		t.Fatalf("declines after the migration = %d, %v", n, err)
	}
	insert := func(url, outcome, verdict, scope, state string) string {
		return `INSERT INTO declines (pr_id, source_url, comment_id, reviewed_sha, outcome, verdict, scope, state, created_at, updated_at)
			VALUES (1, '` + url + `', 1, 'h1', '` + outcome + `', ` + verdict + `, ` + scope + `, '` + state + `', '` + ts + `', '` + ts + `')`
	}
	for name, q := range map[string]string{
		"outcome": insert("u1", "ignored", "NULL", "NULL", "new"),
		"verdict": insert("u2", "declined", "'bogus'", "NULL", "new"),
		"scope":   insert("u3", "declined", "'wrong'", "'global'", "new"),
		"state":   insert("u4", "declined", "'wrong'", "'general'", "open"),
	} {
		if _, err := st.DB().ExecContext(ctx, q); err == nil {
			t.Errorf("%s: the CHECK accepted a bad value", name)
		}
	}
	if _, err := st.DB().ExecContext(ctx, insert("u5", "declined", "'over_ranked'", "'repo'", "new")); err != nil {
		t.Errorf("a good decline: %v", err)
	}
}

// decline is a decline of pr's root comment id, answered with outcome.
func decline(pr PR, id int64, outcome string) Decline {
	return Decline{PRID: pr.ID, SourceURL: "https://github.com/talkable/talkable/pull/" + itoa(int64(pr.Number)) + "#discussion_r" + itoa(id),
		CommentID: id, Path: "app/a.rb", Line: 12, Title: "Nil receiver", Priority: "P2", ReviewedSHA: "sha-7", Outcome: outcome}
}

func TestUpsertDeclineKeepsIDStateAndCreatedAt(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	run := mustPostedRun(t, st, pr.ID, 1, 501)

	in := decline(pr, 1, DeclineFixed)
	in.ReviewID, in.RunID = 501, run.ID
	first, err := st.UpsertDecline(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	want := in
	want.ID, want.Match, want.State, want.CreatedAt, want.UpdatedAt = first.ID, []string{}, MissNew, t0, t0
	if first.ID == 0 || !reflect.DeepEqual(first, want) {
		t.Fatalf("inserted decline = %+v\nwant %+v", first, want)
	}
	var nulls bool
	err = st.DB().QueryRowContext(ctx, `SELECT finding_row_id IS NULL AND skill_sha IS NULL AND verdict IS NULL
		AND lesson IS NULL AND scope IS NULL FROM declines WHERE id = ?`, first.ID).Scan(&nulls)
	if err != nil || !nulls {
		t.Fatalf("empty optional columns are NULL = %v, %v", nulls, err)
	}

	// Someone dismissed it; the next retro reads the thread again and the
	// classifier answers otherwise.
	clk.Add(time.Hour)
	if _, err := st.DB().ExecContext(ctx, "UPDATE declines SET state = ? WHERE id = ?", MissDismissed, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFindings(ctx, run.ID, pr.ID, 1, []Finding{{FindingID: "F1", Path: "app/a.rb", Line: 12, Verdict: FindingPosted}}); err != nil {
		t.Fatal(err)
	}
	fs, err := st.FindingsByPR(ctx, pr.ID)
	if err != nil || len(fs) != 1 {
		t.Fatalf("findings = %+v, %v", fs, err)
	}
	next := in
	next.Outcome, next.Verdict, next.Lesson, next.Scope = DeclineDeclined, VerdictWrong, "When a receiver is checked upstream, do not report it.", MissScopeGeneral
	next.Match, next.SkillSHA, next.FindingRowID = []string{"nil", "receiver"}, "0123456789ab", &fs[0].ID
	next.State = MissUsed // an existing decline keeps its state whatever the caller says
	second, err := st.UpsertDecline(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	want = next
	want.ID, want.State, want.CreatedAt, want.UpdatedAt = first.ID, MissDismissed, t0, t0.Add(time.Hour)
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("updated decline = %+v\nwant %+v", second, want)
	}
	if n, err := st.CountDeclines(ctx, DeclineFilter{}); err != nil || n != 1 {
		t.Fatalf("declines after the upsert = %d, %v; want 1", n, err)
	}

	// Recording the run's findings again replaces their rows: the decline
	// stays, without the link to a row that is gone.
	if err := st.RecordFindings(ctx, run.ID, pr.ID, 1, []Finding{{FindingID: "F1", Path: "app/a.rb", Line: 12, Verdict: FindingPosted}}); err != nil {
		t.Fatalf("recording the findings again under a decline: %v", err)
	}
	got, err := st.Declines(ctx, DeclineFilter{})
	if err != nil || len(got) != 1 || got[0].FindingRowID != nil || got[0].Outcome != DeclineDeclined {
		t.Fatalf("declines after the findings were recorded again = %+v, %v", got, err)
	}
}

func TestUpsertDeclineRefusesBadDeclines(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	good := decline(pr, 1, DeclineDeclined)
	good.Verdict = VerdictRight
	cases := []struct {
		name   string
		mutate func(*Decline)
		want   string // a word the error must carry
	}{
		{"no pr", func(d *Decline) { d.PRID = 0 }, "required"},
		{"no source url", func(d *Decline) { d.SourceURL = "" }, "required"},
		{"no comment id", func(d *Decline) { d.CommentID = 0 }, "required"},
		{"no reviewed sha", func(d *Decline) { d.ReviewedSHA = "" }, "required"},
		{"no outcome", func(d *Decline) { d.Outcome = "" }, "outcome"},
		{"bad outcome", func(d *Decline) { d.Outcome = "ignored" }, "outcome"},
		{"bad verdict", func(d *Decline) { d.Verdict = "bogus" }, "verdict"},
		{"bad scope", func(d *Decline) { d.Scope = "global" }, "scope"},
		{"bad state", func(d *Decline) { d.State = "open" }, "state"},
		{"unknown pr", func(d *Decline) { d.PRID = 999 }, "FOREIGN KEY"},
	}
	for _, c := range cases {
		d := good
		c.mutate(&d)
		if _, err := st.UpsertDecline(ctx, d); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want one mentioning %q", c.name, err, c.want)
		}
	}
	if n, err := st.CountDeclines(ctx, DeclineFilter{}); err != nil || n != 0 {
		t.Fatalf("declines after refused upserts = %d, %v", n, err)
	}
}

func TestDeclinesFilterByVerdictScopeAndState(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr7 := mustPR(t, st, repo.ID, 7, PRReviewed)
	pr8 := mustPR(t, st, repo.ID, 8, PRReviewed)
	add := func(pr PR, id int64, outcome, verdict, scope, skill string, d time.Duration) Decline {
		t.Helper()
		clk.Set(t0.Add(d))
		in := decline(pr, id, outcome)
		in.Verdict, in.Scope, in.SkillSHA = verdict, scope, skill
		got, err := st.UpsertDecline(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	a := add(pr7, 1, DeclineDeclined, VerdictWrong, MissScopeGeneral, "aaaaaaaaaaaa", 0)
	add(pr7, 2, DeclineFixed, "", "", "aaaaaaaaaaaa", time.Minute)
	c := add(pr8, 3, DeclineArgued, VerdictMissingContext, MissScopeRepo, "bbbbbbbbbbbb", 2*time.Minute)
	add(pr8, 4, DeclineDeclined, VerdictOverRanked, MissScopeGeneral, "bbbbbbbbbbbb", 3*time.Minute)
	add(pr8, 5, DeclineArgued, VerdictRight, "", "", 3*time.Minute) // same instant as 4: the later id comes first
	if _, err := st.DB().ExecContext(ctx, "UPDATE declines SET state = ? WHERE id = ?", MissUsed, c.ID); err != nil {
		t.Fatal(err)
	}

	names := func(ds []Decline) string {
		var out []string
		for _, d := range ds {
			out = append(out, itoa(d.CommentID))
		}
		return strings.Join(out, "")
	}
	cases := []struct {
		name string
		f    DeclineFilter
		want string
	}{
		{"everything, newest first", DeclineFilter{}, "54321"},
		{"one outcome", DeclineFilter{Outcomes: []string{DeclineArgued}}, "53"},
		{"two outcomes", DeclineFilter{Outcomes: []string{DeclineDeclined, DeclineArgued}}, "5431"},
		{"one verdict", DeclineFilter{Verdicts: []string{VerdictWrong}}, "1"},
		{"two verdicts", DeclineFilter{Verdicts: []string{VerdictWrong, VerdictOverRanked}}, "41"},
		{"one scope", DeclineFilter{Scopes: []string{MissScopeGeneral}}, "41"},
		{"one state", DeclineFilter{States: []string{MissNew}}, "5421"},
		{"another state", DeclineFilter{States: []string{MissUsed}}, "3"},
		{"verdict, scope and state", DeclineFilter{Verdicts: []string{VerdictMissingContext}, Scopes: []string{MissScopeRepo}, States: []string{MissNew}}, ""},
		{"general wrong or over-ranked, new", DeclineFilter{Verdicts: []string{VerdictWrong, VerdictOverRanked}, Scopes: []string{MissScopeGeneral}, States: []string{MissNew}}, "41"},
		{"one PR", DeclineFilter{PRID: pr7.ID}, "21"},
		{"one repository", DeclineFilter{RepoID: repo.ID}, "54321"},
		{"a repository nothing belongs to", DeclineFilter{RepoID: 999}, ""},
		{"one skill", DeclineFilter{SkillSHA: "bbbbbbbbbbbb"}, "43"},
	}
	for _, tc := range cases {
		got, err := st.Declines(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if names(got) != tc.want {
			t.Errorf("%s: declines = %q, want %q", tc.name, names(got), tc.want)
		}
		if n, err := st.CountDeclines(ctx, tc.f); err != nil || n != len(tc.want) {
			t.Errorf("%s: CountDeclines = %d, %v; want %d", tc.name, n, err, len(tc.want))
		}
	}

	all, _ := st.Declines(ctx, DeclineFilter{})
	for _, d := range all {
		wantNumber := 8
		if d.PRID == pr7.ID {
			wantNumber = 7
		}
		if d.Repo != "talkable/talkable" || d.Number != wantNumber {
			t.Errorf("decline %d: repo %q number %d, want talkable/talkable #%d", d.ID, d.Repo, d.Number, wantNumber)
		}
	}
	// The row reads the same through Declines as through UpsertDecline, apart from the PR's name.
	got := all[len(all)-1]
	got.Repo, got.Number = "", 0
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("decline 1 through Declines = %+v\nthrough UpsertDecline %+v", got, a)
	}
}

func TestSetDeclineStateIsCompareAndSet(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	var ids []int64
	for id := int64(1); id <= 3; id++ {
		d, err := st.UpsertDecline(ctx, decline(pr, id, DeclineDeclined))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.ID)
	}
	if _, err := st.DB().ExecContext(ctx, "UPDATE declines SET state = ? WHERE id = ?", MissUsed, ids[1]); err != nil {
		t.Fatal(err)
	}

	clk.Add(time.Hour)
	n, err := st.SetDeclineState(ctx, ids[:2], []string{MissNew}, MissUsed)
	if err != nil || n != 1 {
		t.Fatalf("SetDeclineState(new -> used) = %d, %v; want 1 (the used one is left alone)", n, err)
	}
	states := map[int64]string{}
	ds, _ := st.Declines(ctx, DeclineFilter{})
	for _, d := range ds {
		states[d.ID] = d.State
		if d.ID == ids[0] && !d.UpdatedAt.Equal(t0.Add(time.Hour)) {
			t.Errorf("the moved decline's updated_at = %v", d.UpdatedAt)
		}
		if d.ID == ids[1] && !d.UpdatedAt.Equal(t0) {
			t.Errorf("the decline left alone was touched: updated_at %v", d.UpdatedAt)
		}
	}
	if want := map[int64]string{ids[0]: MissUsed, ids[1]: MissUsed, ids[2]: MissNew}; !reflect.DeepEqual(states, want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	// Two from-states, and no id at all.
	if n, err := st.SetDeclineState(ctx, ids, []string{MissNew, MissUsed}, MissDismissed); err != nil || n != 3 {
		t.Fatalf("SetDeclineState(new|used -> dismissed) = %d, %v; want 3", n, err)
	}
	if n, err := st.SetDeclineState(ctx, nil, []string{MissNew}, MissUsed); err != nil || n != 0 {
		t.Fatalf("SetDeclineState(no ids) = %d, %v", n, err)
	}
	for name, call := range map[string]func() (int, error){
		"bad to":   func() (int, error) { return st.SetDeclineState(ctx, ids, []string{MissNew}, "open") },
		"bad from": func() (int, error) { return st.SetDeclineState(ctx, ids, []string{"open"}, MissUsed) },
		"no from":  func() (int, error) { return st.SetDeclineState(ctx, ids, nil, MissUsed) },
	} {
		if _, err := call(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
