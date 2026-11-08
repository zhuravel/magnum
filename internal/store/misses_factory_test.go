package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/sqlitetest"
)

// Migration 0031 adds where a miss came from and where it went wrong to
// misses: the rows stored before have no origin, no stage and were not
// caught by the posted round.
func TestMigrationV30ToV31AddsTheMissesOriginStageAndPostedCatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&"+sqlitetest.FastFile)
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var stmts []string
	for _, m := range migrations[:30] {
		stmts = append(stmts, m.SQL)
	}
	stmts = append(stmts, "PRAGMA user_version = 30",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewed', 'talkable-app', '`+ts+`', '`+ts+`')`,
		`INSERT INTO misses (id, pr_id, source_url, source_kind, reviewer, path, line, reviewed_sha, class, raised, created_at, updated_at)
		 VALUES (1, 1, 'https://github.com/talkable/talkable/pull/7#discussion_r1', 'thread', 'rev-ann', 'a.rb', 4, 'h1', 'miss', 'none', '`+ts+`', '`+ts+`')`)
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v30 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v30 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 31 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	var origin, stage sql.NullString
	var caught int
	if err := st.db.QueryRowContext(ctx, "SELECT origin, stage, posted_caught FROM misses WHERE id = 1").Scan(&origin, &stage, &caught); err != nil {
		t.Fatal(err)
	}
	if origin.Valid || stage.Valid || caught != 0 {
		t.Errorf("a miss stored before = origin %v, stage %v, posted_caught %d", origin, stage, caught)
	}
	old, err := st.Misses(ctx, MissFilter{})
	if err != nil || len(old) != 1 || old[0].Origin != "" || old[0].Stage != "" || old[0].PostedCaught {
		t.Fatalf("misses stored before = %+v, %v", old, err)
	}
}

// UpsertMiss keeps a factory-origin miss's origin, stage and posted catch;
// a stage and a posted catch belong to a factory-origin miss only, the
// stage is one of plan, code and review, and an unclassified write (a retro
// whose classifier failed) keeps the classifier's stage as it keeps its
// class, while the origin and the catch are Build's and always take the new
// value.
func TestUpsertMissStoresTheFactoryOriginAndStage(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	pr := mustPR(t, st, mustRepo(t, st).ID, 7, PRReviewed)
	base := Miss{PRID: pr.ID, SourceURL: "https://github.com/talkable/talkable/pull/7#discussion_r1", SourceKind: MissSourceThread,
		Reviewer: "rev-ann", Path: "a.rb", Line: 4, ReviewedSHA: "h1", Origin: MissOriginFactory}

	m := base
	m.Class, m.Severity, m.Title, m.Scope, m.Stage, m.PostedCaught = MissMiss, "P2", "Scope lost", MissScopeGeneral, MissStagePlan, true
	got, err := st.UpsertMiss(ctx, m)
	if err != nil || got.Origin != MissOriginFactory || got.Stage != MissStagePlan || !got.PostedCaught {
		t.Fatalf("a factory-origin miss = %+v, %v", got, err)
	}
	b, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(b), `"origin":"factory"`) || !strings.Contains(string(b), `"stage":"plan"`) ||
		!strings.Contains(string(b), `"posted_caught":true`) {
		t.Fatalf("JSON = %s, %v", b, err)
	}
	unclassified := base // the classifier failed on the next retro
	if got, err = st.UpsertMiss(ctx, unclassified); err != nil || got.Class != MissMiss || got.Stage != MissStagePlan ||
		got.Origin != MissOriginFactory || got.PostedCaught {
		t.Fatalf("after an unclassified write = %+v, %v", got, err)
	}
	plain := base
	plain.Origin, plain.Class = "", MissNotIssue
	if got, err = st.UpsertMiss(ctx, plain); err != nil || got.Origin != "" || got.Stage != "" || got.PostedCaught {
		t.Fatalf("classified again without an origin = %+v, %v", got, err)
	}
	b, _ = json.Marshal(got)
	if strings.Contains(string(b), "origin") || strings.Contains(string(b), "stage") || strings.Contains(string(b), "posted_caught") {
		t.Fatalf("a plain miss's JSON names the factory: %s", b)
	}

	for name, bad := range map[string]Miss{
		"an unknown origin":                 {Origin: "elsewhere", Class: MissMiss},
		"an unknown stage":                  {Origin: MissOriginFactory, Class: MissNotIssue, Stage: "deploy"},
		"a stage without the origin":        {Class: MissNotIssue, Stage: MissStageReview},
		"a posted catch without the origin": {Class: MissNotIssue, PostedCaught: true},
	} {
		m := base
		m.SourceURL += "-" + strings.ReplaceAll(name, " ", "-")
		m.Origin, m.Class, m.Stage, m.PostedCaught = bad.Origin, bad.Class, bad.Stage, bad.PostedCaught
		if _, err := st.UpsertMiss(ctx, m); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
	for _, stage := range []string{MissStagePlan, MissStageCode, MissStageReview} {
		m := base
		m.SourceURL += "-" + stage
		m.Class, m.Stage = MissStyle, stage
		if got, err := st.UpsertMiss(ctx, m); err != nil || got.Stage != stage {
			t.Errorf("stage %s: %+v, %v", stage, got, err)
		}
	}
}

// A comment the posted round caught is recorded only as a factory-origin
// miss: every reader of the misses leaves it out (the editor, the notes,
// the corpus, the counts), unless it asks for it (the listing).
func TestMissesLeaveOutWhatThePostedRoundCaughtUnlessAsked(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	pr := mustPR(t, st, mustRepo(t, st).ID, 7, PRReviewed)
	for i, caught := range []bool{false, true} {
		m := Miss{PRID: pr.ID, SourceURL: pr.URL + "#r" + string(rune('1'+i)), SourceKind: MissSourceThread, Reviewer: "rev-ann",
			ReviewedSHA: "h1", Class: MissMiss, Severity: "P2", Title: "Scope lost", Scope: MissScopeRepo, Origin: MissOriginFactory,
			PostedCaught: caught}
		if _, err := st.UpsertMiss(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	f := MissFilter{Classes: []string{MissMiss}, States: []string{MissNew}, Scopes: []string{MissScopeRepo}}
	if ms, err := st.Misses(ctx, f); err != nil || len(ms) != 1 || ms[0].PostedCaught {
		t.Fatalf("Misses = %+v, %v; want only the miss both rounds missed", ms, err)
	}
	if n, err := st.CountMisses(ctx, f); err != nil || n != 1 {
		t.Fatalf("CountMisses = %d, %v; want 1", n, err)
	}
	if ms, err := st.Misses(ctx, MissFilter{}); err != nil || len(ms) != 1 {
		t.Fatalf("Misses of every class = %+v, %v; want 1", ms, err)
	}
	f.WithPostedCaught = true
	if ms, err := st.Misses(ctx, f); err != nil || len(ms) != 2 {
		t.Fatalf("Misses with the posted catches = %+v, %v; want 2", ms, err)
	}
	if n, err := st.CountMisses(ctx, f); err != nil || n != 2 {
		t.Fatalf("CountMisses with the posted catches = %d, %v; want 2", n, err)
	}
}

// A version's misses are the review skill's: a comment its posted round
// caught is none of them.
func TestVersionMeasureLeavesOutWhatThePostedRoundCaught(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	pr := mustPR(t, st, mustRepo(t, st).ID, 7, PRReviewed)
	r1 := t0
	v1 := measureVersion(t, st, 1, "aaaaaaaaaaaa", &r1)
	in := postedRun(t, st, pr.ID, 0, "h1", v1.SkillSHA, true, "P1")
	for i, caught := range []bool{false, true} {
		if _, err := st.UpsertMiss(ctx, Miss{PRID: pr.ID, SourceURL: pr.URL + "#m" + string(rune('1'+i)), SourceKind: MissSourceThread,
			Reviewer: "rev-ann", ReviewedSHA: in.TargetSHA, Class: MissMiss, Severity: "P1", Scope: MissScopeGeneral,
			Origin: MissOriginFactory, PostedCaught: caught}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.VersionMeasure(ctx, v1, nil)
	if err != nil || got.MissesGeneral != 1 {
		t.Fatalf("VersionMeasure = %+v, %v; want one general miss", got, err)
	}
}

// Origins selects the misses of an origin: the factory's (posted catches
// left out unless asked, as always).
func TestMissFilterSelectsTheFactorysMisses(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	pr := mustPR(t, st, mustRepo(t, st).ID, 7, PRReviewed)
	for i, m := range []Miss{{}, {Origin: MissOriginFactory}, {Origin: MissOriginFactory, PostedCaught: true}} {
		m.PRID, m.SourceURL, m.SourceKind, m.Reviewer, m.ReviewedSHA = pr.ID, pr.URL+"#o"+string(rune('1'+i)), MissSourceThread, "rev-ann", "h1"
		if _, err := st.UpsertMiss(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		f    MissFilter
		want int
	}{
		{MissFilter{Origins: []string{MissOriginFactory}}, 1},
		{MissFilter{Origins: []string{MissOriginFactory}, WithPostedCaught: true}, 2},
		{MissFilter{}, 2},
	} {
		if n, err := st.CountMisses(ctx, tc.f); err != nil || n != tc.want {
			t.Errorf("CountMisses(%+v) = %d, %v; want %d", tc.f, n, err, tc.want)
		}
	}
}
