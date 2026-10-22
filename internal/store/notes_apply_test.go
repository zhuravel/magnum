package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestMigrationV26ToV27AddsApplyingAndBackfillsDeciders builds a registry the
// v26 binary would have written (user_version 26, foreign keys on) with a
// proposal in every state, one linked to a miss, and reopens it with Open:
// notes_proposals takes the applying state and the decider, keeps every row
// and its ids, backfills the decider (the operator's applies and
// rejections, magnum's expiries, invalid and superseded proposals, none for
// a pending one), and its miss link survives the rebuild.
func TestMigrationV26ToV27AddsApplyingAndBackfillsDeciders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var setup []string
	for _, m := range migrations[:26] {
		setup = append(setup, m.sql)
	}
	setup = append(setup, "PRAGMA user_version = 26",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, simplify_done, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewed', 'talkable-app', 0, '`+ts+`', '`+ts+`')`,
		`INSERT INTO notes_versions (id, repo_id, at, source, bytes, sha256) VALUES (3, 1, '`+ts+`', 'import', 0, 'e3b0')`,
		`INSERT INTO notes_proposals (id, repo_id, kind, trigger_reason, base_version_id, version_id, state, created_at, decided_at) VALUES
		 (1, 1, 'curation', 'misses', 3, 3, 'applied', '`+ts+`', '`+ts+`'),
		 (2, 1, 'curation', 'misses', 3, 3, 'rejected', '`+ts+`', '`+ts+`'),
		 (3, 1, 'curation', 'misses', 3, 3, 'expired', '`+ts+`', '`+ts+`'),
		 (4, 1, 'curation', 'misses', 3, NULL, 'invalid', '`+ts+`', '`+ts+`'),
		 (5, 1, 'curation', 'misses', 3, 3, 'superseded', '`+ts+`', '`+ts+`'),
		 (6, 1, 'curation', 'misses', 3, 3, 'pending', '`+ts+`', NULL)`,
		`INSERT INTO misses (id, pr_id, source_url, source_kind, reviewer, reviewed_sha, class, raised, scope, state, created_at, updated_at)
		 VALUES (5, 1, 'https://example.com/r/1', 'review', 'rev-ann', 'h1', 'miss', 'none', 'repo', 'new', '`+ts+`', '`+ts+`')`,
		`INSERT INTO notes_proposal_misses (proposal_id, miss_id, outcome, reason) VALUES (6, 5, 'skipped', 'one PR only')`,
	)
	for _, q := range setup {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v26 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v26 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 27 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	want := map[int64]string{1: DecidedByOperator, 2: DecidedByOperator, 3: DecidedByMagnum, 4: DecidedByMagnum, 5: DecidedByMagnum, 6: ""}
	ps, err := st.NotesProposals(ctx, NotesProposalFilter{RepoID: 1})
	if err != nil || len(ps) != len(want) {
		t.Fatalf("proposals after 0027 = %+v, %v", ps, err)
	}
	for _, p := range ps {
		if p.DecidedBy != want[p.ID] {
			t.Errorf("proposal %d (%s) decided by %q, want %q", p.ID, p.State, p.DecidedBy, want[p.ID])
		}
	}
	links, err := st.ProposalMisses(ctx, 6)
	if err != nil || len(links) != 1 || links[0].Reason != "one PR only" {
		t.Fatalf("proposal misses after 0027 = %+v, %v", links, err)
	}
	if _, err := st.ClaimNotesProposal(ctx, 6, NotesClaim{By: DecidedByCritic, Target: "fp"}); err != nil {
		t.Fatalf("claim after 0027 = %v", err)
	}
	for _, q := range []string{"UPDATE notes_proposals SET state = 'bogus' WHERE id = 6", "UPDATE notes_proposals SET decided_by = 'bob' WHERE id = 6"} {
		if _, err := st.DB().ExecContext(ctx, q); err == nil {
			t.Errorf("%s was accepted: the CHECK is gone", q)
		}
	}
}

// A claim moves a pending proposal to applying with its decider, the notes
// it found (recorded once, deduplicated) and its target, compare-and-set: a
// second claim, a rejection or an expiry of a claimed proposal conflict. Its
// release puts it back to pending with the claim cleared; its record moves
// it to applied, keeping the decider.
func TestAClaimIsCompareAndSetAndReleasable(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	base, _, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromImport, Content: NotesContent{Notes: []byte("# Notes\n")}})
	if err != nil {
		t.Fatal(err)
	}
	proposed := NotesContent{Notes: []byte("# Notes\n- more\n")}
	p, err := st.CreateNotesProposal(ctx, NotesProposalInput{RepoID: repo.ID, Kind: ProposalCuration, BaseVersionID: base.ID, Proposed: &proposed, State: ProposalPending})
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.ClaimNotesProposal(ctx, p.ID, NotesClaim{By: DecidedByCritic, Prior: NotesContent{Notes: []byte("# Notes\n")}, Target: "fp-target"})
	if err != nil || c.State != ProposalApplying || c.DecidedBy != DecidedByCritic || c.TargetSHA256 != "fp-target" || c.PriorVersionID == nil ||
		*c.PriorVersionID != base.ID {
		t.Fatalf("claim = %+v, %v (the prior notes are the base version, not recorded again)", c, err)
	}
	if _, err := st.ClaimNotesProposal(ctx, p.ID, NotesClaim{By: DecidedByOperator, Target: "fp-target"}); !errors.Is(err, ErrConflict) {
		t.Errorf("a second claim = %v", err)
	}
	for _, to := range []string{ProposalRejected, ProposalExpired} {
		if _, err := st.DecideNotesProposal(ctx, p.ID, []string{ProposalPending}, to, DecidedByOperator, "", time.Time{}, nil); !errors.Is(err, ErrConflict) {
			t.Errorf("%s of a claimed proposal = %v", to, err)
		}
	}
	r, err := st.ReleaseNotesClaim(ctx, p.ID)
	if err != nil || r.State != ProposalPending || r.DecidedBy != "" || r.PriorVersionID != nil || r.TargetSHA256 != "" {
		t.Fatalf("release = %+v, %v", r, err)
	}
	if _, err := st.ReleaseNotesClaim(ctx, p.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("a release of a pending proposal = %v", err)
	}
	if _, err := st.ClaimNotesProposal(ctx, p.ID, NotesClaim{By: DecidedByOperator, Prior: NotesContent{Notes: []byte("# Notes\n")}, Target: "fp"}); err != nil {
		t.Fatal(err)
	}
	a, err := st.DecideNotesProposal(ctx, p.ID, []string{ProposalApplying}, ProposalApplied, DecidedByOperator, "", time.Time{},
		&NotesVersionInput{Source: NotesFromCuration, Content: proposed, Dedupe: true})
	if err != nil || a.State != ProposalApplied || a.DecidedBy != DecidedByOperator || a.AppliedVersionID == nil || a.DecidedAt == nil {
		t.Fatalf("applied = %+v, %v", a, err)
	}
	if _, err := st.DecideNotesProposal(ctx, p.ID, []string{ProposalPending}, ProposalRejected, "someone", "", time.Time{}, nil); err == nil {
		t.Error("an unknown decider was accepted")
	}
}
