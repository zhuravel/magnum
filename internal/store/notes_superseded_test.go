package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMigrationV16ToV17AcceptsSupersededProposals builds a registry the v16
// binary would have written (user_version 16, foreign keys on) with notes
// proposals, one of them linked to a miss, and reopens it with Open:
// notes_proposals takes the superseded state, keeps every row and its ids,
// notes_proposal_misses keeps its links and still references the proposals,
// and the indexes come back.
func TestMigrationV16ToV17AcceptsSupersededProposals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var setup []string
	for _, m := range migrations[:16] {
		setup = append(setup, m.sql)
	}
	setup = append(setup, "PRAGMA user_version = 16",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, simplify_done, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewed', 'talkable-app', 0, '`+ts+`', '`+ts+`')`,
		`INSERT INTO notes_versions (id, repo_id, at, source, bytes, sha256) VALUES (3, 1, '`+ts+`', 'import', 0, 'e3b0')`,
		`INSERT INTO notes_proposals (id, repo_id, kind, trigger_reason, base_version_id, version_id, changes_json, state, created_at)
		 VALUES (4, 1, 'curation', 'over_limit', 3, 3, '{"sections":[]}', 'pending', '`+ts+`'),
		        (9, 1, 'curation', 'misses', 3, NULL, NULL, 'invalid', '`+ts+`')`,
		`INSERT INTO misses (id, pr_id, source_url, source_kind, reviewer, reviewed_sha, class, raised, scope, state, created_at, updated_at)
		 VALUES (5, 1, 'https://example.com/r/1', 'review', 'rev-ann', 'h1', 'miss', 'none', 'repo', 'new', '`+ts+`', '`+ts+`')`,
		`INSERT INTO notes_proposal_misses (proposal_id, miss_id, outcome, reason) VALUES (4, 5, 'skipped', 'one PR only')`,
	)
	for _, q := range setup {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v16 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v16 database: %v", err)
	}
	defer st.Close()
	st.Clock = func() time.Time { return t0.Add(time.Hour) }
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 17 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	ps, err := st.NotesProposals(ctx, NotesProposalFilter{RepoID: 1})
	if err != nil || len(ps) != 2 || ps[0].ID != 9 || ps[0].State != ProposalInvalid || ps[1].ID != 4 || ps[1].Trigger != "over_limit" ||
		string(ps[1].Changes) != `{"sections":[]}` {
		t.Fatalf("proposals after 0017 = %+v, %v", ps, err)
	}
	links, err := st.ProposalMisses(ctx, 4)
	if err != nil || len(links) != 1 || links[0].MissID != 5 || links[0].Reason != "one PR only" {
		t.Fatalf("proposal misses after 0017 = %+v, %v", links, err)
	}

	// A stale proposal is superseded; its miss stays new for the curation
	// that follows it up.
	p, err := st.DecideNotesProposal(ctx, 4, []string{ProposalPending}, ProposalSuperseded, "the notes changed since it was made", time.Time{}, nil)
	if err != nil || p.State != ProposalSuperseded || p.DecidedAt == nil {
		t.Fatalf("supersede = %+v, %v", p, err)
	}
	links, err = st.ProposalMisses(ctx, 4)
	if err != nil || len(links) != 1 || links[0].Miss.State != MissNew {
		t.Fatalf("the miss after a supersede = %+v, %v", links, err)
	}
	if _, err := st.DB().ExecContext(ctx, "UPDATE notes_proposals SET state = 'bogus' WHERE id = 9"); err == nil {
		t.Fatal("an unknown proposal state was accepted: the CHECK is gone")
	}
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO notes_proposal_misses (proposal_id, miss_id, outcome) VALUES (77, 5, 'noted')`); err == nil {
		t.Fatal("a link to a missing proposal was accepted: the foreign key is gone")
	}
	for _, tbl := range []string{"notes_proposals", "notes_proposal_misses"} {
		var ddl string
		if err := st.DB().QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", tbl).Scan(&ddl); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(ddl, "_new") {
			t.Fatalf("%s DDL:\n%s", tbl, ddl)
		}
	}
	for _, idx := range []string{"notes_proposals_repo", "notes_proposals_state", "notes_proposal_misses_miss"} {
		var n int
		if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", idx).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s after 0017: %d, %v", idx, n, err)
		}
	}
}
