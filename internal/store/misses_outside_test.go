package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// Migration 0029 adds misses.comment_sha and misses.outside_reason to a
// registry the previous binary wrote (user_version 28): its misses stay,
// with both NULL, and the ones the retro stores after it keep both.
func TestMigrationV28ToV29AddsTheCommentsCommitAndWhyItIsOutside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var stmts []string
	for _, m := range migrations[:28] {
		stmts = append(stmts, m.sql)
	}
	miss := func(id, class string) string {
		return `INSERT INTO misses (id, pr_id, source_url, source_kind, reviewer, path, line, reviewed_sha, class, raised, created_at, updated_at)
		 VALUES (` + id + `, 1, 'https://github.com/talkable/talkable/pull/7#discussion_r` + id + `', 'thread', 'rev-ann', 'a.rb', 4, 'h1', '` +
			class + `', 'none', '` + ts + `', '` + ts + `')`
	}
	stmts = append(stmts, "PRAGMA user_version = 28",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewed', 'talkable-app', '`+ts+`', '`+ts+`')`,
		miss("1", "outside"), miss("2", "miss"))
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v28 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v28 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 29 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	var nulls int
	if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM misses WHERE comment_sha IS NULL AND outside_reason IS NULL").Scan(&nulls); err != nil || nulls != 2 {
		t.Fatalf("rows stored before with both NULL = %d, %v; want 2", nulls, err)
	}
	old, err := st.Misses(ctx, MissFilter{})
	if err != nil || len(old) != 2 {
		t.Fatalf("misses stored before = %+v, %v", old, err)
	}
	for _, m := range old {
		if m.CommentSHA != "" || m.OutsideReason != "" || m.ReviewedSHA != "h1" {
			t.Errorf("miss %d stored before = %+v", m.ID, m)
		}
	}
	got, err := st.UpsertMiss(ctx, Miss{PRID: 1, SourceURL: "https://github.com/talkable/talkable/pull/7#discussion_r3", SourceKind: MissSourceThread,
		Reviewer: "rev-ann", Path: "a.rb", Line: 4, ReviewedSHA: "h1", CommentSHA: "h2", Class: MissOutside, OutsideReason: MissOutsideFileChanged})
	if err != nil || got.CommentSHA != "h2" || got.OutsideReason != MissOutsideFileChanged {
		t.Fatalf("a miss stored after the migration = %+v, %v", got, err)
	}
}

// UpsertMiss keeps the commit a comment was made on and why it is outside;
// the reason belongs to class outside only, and an unclassified write (a
// retro whose classifier failed) keeps the classifier's reason as it keeps
// its class, while a new class takes the reason away.
func TestUpsertMissStoresTheCommentsCommitAndWhyItIsOutside(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	pr := mustPR(t, st, mustRepo(t, st).ID, 7, PRReviewed)
	base := Miss{PRID: pr.ID, SourceURL: "https://github.com/talkable/talkable/pull/7#discussion_r1", SourceKind: MissSourceThread,
		Reviewer: "rev-ann", Path: "a.rb", Line: 4, ReviewedSHA: "h1", CommentSHA: "h2"}

	outside := base
	outside.Class, outside.OutsideReason = MissOutside, MissOutsideClassifier
	got, err := st.UpsertMiss(ctx, outside)
	if err != nil || got.CommentSHA != "h2" || got.OutsideReason != MissOutsideClassifier {
		t.Fatalf("an outside miss = %+v, %v", got, err)
	}
	b, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(b), `"comment_sha":"h2"`) || !strings.Contains(string(b), `"outside_reason":"classifier"`) {
		t.Fatalf("JSON = %s, %v", b, err)
	}
	if got, err = st.UpsertMiss(ctx, base); err != nil || got.Class != MissOutside || got.OutsideReason != MissOutsideClassifier {
		t.Fatalf("after an unclassified write = %+v, %v", got, err)
	}
	miss := base
	miss.Class, miss.Severity, miss.Title, miss.Scope = MissMiss, "P2", "Scope lost", MissScopeGeneral
	if got, err = st.UpsertMiss(ctx, miss); err != nil || got.Class != MissMiss || got.OutsideReason != "" || got.CommentSHA != "h2" {
		t.Fatalf("classified a miss = %+v, %v", got, err)
	}
	b, _ = json.Marshal(got)
	if strings.Contains(string(b), "outside_reason") {
		t.Fatalf("a miss's JSON names an outside reason: %s", b)
	}

	for name, m := range map[string]Miss{
		"an unknown reason":           {Class: MissOutside, OutsideReason: "too_old"},
		"a reason on another class":   {Class: MissNotIssue, OutsideReason: MissOutsideFileChanged},
		"a reason on an unclassified": {OutsideReason: MissOutsideBeforeReview},
	} {
		bad := base
		bad.SourceURL += "-" + strings.ReplaceAll(name, " ", "-")
		bad.Class, bad.OutsideReason = m.Class, m.OutsideReason
		if _, err := st.UpsertMiss(ctx, bad); err == nil || !strings.Contains(err.Error(), "outside_reason") {
			t.Errorf("%s: error = %v, want one about outside_reason", name, err)
		}
	}
	for _, why := range []string{MissOutsideBeforeReview, MissOutsideFileChanged, MissOutsideNotDescendant, MissOutsideNoCompare,
		MissOutsideBodyUnreviewed, MissOutsideClassifier} {
		m := base
		m.SourceURL += "-" + why
		m.Class, m.OutsideReason = MissOutside, why
		if got, err := st.UpsertMiss(ctx, m); err != nil || got.OutsideReason != why {
			t.Errorf("reason %s: %+v, %v", why, got, err)
		}
	}
}
