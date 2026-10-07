package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func notesContent(text string, files ...string) NotesContent {
	c := NotesContent{Notes: []byte(text)}
	for i := 0; i+1 < len(files); i += 2 {
		c.Files = append(c.Files, NotesBlob{Path: files[i], Body: []byte(files[i+1])})
	}
	return c
}

func countRows(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every state of the notes is a version; an unchanged state is not stored
// again, and a body (the notes text, a harness file) is stored once per
// content even when a later version comes back to it.
func TestNotesVersionsAreRecordedAndDeduplicated(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 11920, PRReviewing)

	a := notesContent("# Notes\n\n- run bin/rspec\n", "run.sh", "bin/rspec \"$@\"\n")
	v1, added, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromImport, Content: a, Dedupe: true})
	if err != nil || !added {
		t.Fatalf("first version: %v added %v", err, added)
	}
	clk.Add(time.Minute)
	if again, added, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromJudge, Content: a, Dedupe: true}); err != nil || added || again.ID != v1.ID {
		t.Fatalf("the same state again: %+v added %v err %v, want version %d kept", again, added, err, v1.ID)
	}
	b := notesContent("# Notes\n\n- run bin/rspec\n- lint with bin/rubocop\n", "run.sh", "bin/rspec \"$@\"\n", "lint.sh", "bin/rubocop\n")
	v2, added, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromJudge, PRID: pr.ID, RunID: "r-1", Content: b, Dedupe: true})
	if err != nil || !added {
		t.Fatalf("second version: %v added %v", err, added)
	}
	if v2.PRNumber != 11920 || v2.RunID != "r-1" || v2.Source != NotesFromJudge || len(v2.Files) != 2 || v2.Bytes != int64(len(b.Notes)) {
		t.Errorf("second version = %+v", v2)
	}
	// Back to the first text: a new version, but no new body.
	v3, added, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromHuman, Content: a, Dedupe: true})
	if err != nil || !added {
		t.Fatalf("third version: %v added %v", err, added)
	}
	var bodies int
	if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM notes_versions WHERE body IS NOT NULL").Scan(&bodies); err != nil {
		t.Fatal(err)
	}
	if bodies != 2 || countRows(t, st, "notes_versions") != 3 {
		t.Errorf("%d bodies in %d versions, want 2 in 3", bodies, countRows(t, st, "notes_versions"))
	}
	if n := countRows(t, st, "harness_blobs"); n != 2 { // run.sh and lint.sh, once each
		t.Errorf("harness_blobs = %d rows, want 2", n)
	}
	for _, v := range []NotesVersion{v1, v3} {
		got, err := st.NotesVersionContent(ctx, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Notes) != string(a.Notes) || len(got.Files) != 1 || string(got.Files[0].Body) != "bin/rspec \"$@\"\n" {
			t.Errorf("version %d content = %q %+v", v.ID, got.Notes, got.Files)
		}
	}
	hist, err := st.NotesHistory(ctx, repo.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 || hist[0].ID != v3.ID || hist[2].ID != v1.ID {
		t.Errorf("history = %+v, want v3, v2, v1", hist)
	}
	if latest, err := st.LatestNotesVersion(ctx, repo.ID); err != nil || latest.ID != v3.ID {
		t.Errorf("latest = %+v, %v", latest, err)
	}
	if _, err := st.LatestNotesVersion(ctx, repo.ID+1); !errors.Is(err, ErrNotFound) {
		t.Errorf("latest of a repository without notes: %v", err)
	}
}

// A curation's proposed state is kept as a version of source curation, but
// it joins the history only through the version recorded when the proposal
// is applied; rejected, expired and invalid proposals stay too.
func TestNotesProposalsAreKeptWhateverBecomesOfThem(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	base, _, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromImport, Content: notesContent("old\n", "probe_spec.rb", "x")})
	if err != nil {
		t.Fatal(err)
	}
	proposed := notesContent("new\n", "run.sh", "bin/rspec\n")
	mk := func() NotesProposal {
		p, err := st.CreateNotesProposal(ctx, NotesProposalInput{RepoID: repo.ID, Kind: ProposalCuration, Trigger: "over_limit",
			BaseVersionID: base.ID, Proposed: &proposed, Changes: []byte(`{"files":[{"name":"probe_spec.rb","action":"deleted","reason":"one PR's probe"}]}`),
			State: ProposalPending, Model: "sonnet", PromptSHA: "abc", Scratch: "/tmp/x"})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	applied, rejected, expired := mk(), mk(), mk()
	if applied.VersionID == nil || applied.State != ProposalPending || applied.DecidedAt != nil || applied.Model != "sonnet" {
		t.Fatalf("proposal = %+v", applied)
	}
	if hist, _ := st.NotesHistory(ctx, repo.ID, 0); len(hist) != 1 {
		t.Fatalf("a pending proposal joined the history: %+v", hist)
	}
	if n, _ := st.CountNotesProposals(ctx, ProposalPending); n != 3 {
		t.Errorf("pending = %d, want 3", n)
	}
	clk.Add(time.Hour)
	p, err := st.DecideNotesProposal(ctx, applied.ID, []string{ProposalPending}, ProposalApplied, "", time.Time{},
		&NotesVersionInput{Source: NotesFromCuration, Content: proposed})
	if err != nil {
		t.Fatal(err)
	}
	if p.State != ProposalApplied || p.AppliedVersionID == nil || p.DecidedAt == nil {
		t.Fatalf("applied = %+v", p)
	}
	if _, err := st.DecideNotesProposal(ctx, rejected.ID, []string{ProposalPending}, ProposalRejected, "keep the probes list short", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DecideNotesProposal(ctx, expired.ID, []string{ProposalPending}, ProposalExpired, "older than 7 days", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DecideNotesProposal(ctx, expired.ID, []string{ProposalPending}, ProposalApplied, "", time.Time{}, nil); !errors.Is(err, ErrConflict) {
		t.Errorf("deciding an expired proposal again: %v, want a conflict", err)
	}
	invalid, err := st.CreateNotesProposal(ctx, NotesProposalInput{RepoID: repo.ID, Kind: ProposalCuration, BaseVersionID: base.ID,
		State: ProposalInvalid, Reason: "a harness file is not named in the notes"})
	if err != nil || invalid.VersionID != nil || invalid.DecidedAt == nil {
		t.Fatalf("invalid proposal = %+v, %v", invalid, err)
	}

	all, err := st.NotesProposals(ctx, NotesProposalFilter{RepoID: repo.ID})
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]int{}
	for _, p := range all {
		states[p.State]++
	}
	if len(all) != 4 || states[ProposalApplied] != 1 || states[ProposalRejected] != 1 || states[ProposalExpired] != 1 || states[ProposalInvalid] != 1 {
		t.Errorf("proposals = %v", states)
	}
	var r NotesProposal
	if i := slices.IndexFunc(all, func(p NotesProposal) bool { return p.ID == rejected.ID }); i >= 0 {
		r = all[i]
	}
	if r.Reason != "keep the probes list short" || !strings.Contains(string(r.Changes), "one PR's probe") {
		t.Errorf("rejected proposal = %+v", r)
	}
	hist, err := st.NotesHistory(ctx, repo.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[0].ID != *p.AppliedVersionID || hist[0].Source != NotesFromCuration || hist[0].ProposalID == nil || *hist[0].ProposalID != applied.ID {
		t.Errorf("history after the apply = %+v", hist)
	}
}

// A restore names the version it proposes, which may be an applied
// curation's (of source curation too): that version stays in the history
// and is still the latest, whatever becomes of the restore, so the notes it
// holds are not recorded again. Only a curation's own proposed state is
// left out of the history.
func TestARestoreNeverHidesTheVersionItProposes(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	base, _, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromImport, Content: notesContent("old\n")})
	if err != nil {
		t.Fatal(err)
	}
	proposed := notesContent("new\n", "run.sh", "bin/rspec\n")
	cur, err := st.CreateNotesProposal(ctx, NotesProposalInput{RepoID: repo.ID, Kind: ProposalCuration, BaseVersionID: base.ID,
		Proposed: &proposed, State: ProposalPending})
	if err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Hour)
	applied, err := st.DecideNotesProposal(ctx, cur.ID, []string{ProposalPending}, ProposalApplied, "", time.Time{},
		&NotesVersionInput{Source: NotesFromCuration, Content: proposed, Dedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	av := *applied.AppliedVersionID
	// A restore of the applied version left waiting (a run that never
	// answered), and one rejected.
	for _, state := range []string{ProposalPending, ProposalRejected} {
		if _, err := st.CreateNotesProposal(ctx, NotesProposalInput{RepoID: repo.ID, Kind: ProposalRestore, Trigger: "restore",
			BaseVersionID: base.ID, VersionID: av, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := st.NotesHistory(ctx, repo.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[0].ID != av || hist[1].ID != base.ID {
		t.Fatalf("history = %+v, want the applied curation %d and the import %d", hist, av, base.ID)
	}
	if latest, err := st.LatestNotesVersion(ctx, repo.ID); err != nil || latest.ID != av {
		t.Fatalf("latest = %+v, %v, want the applied curation %d", latest, err, av)
	}
	if v, added, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromImport, Content: proposed, Dedupe: true}); err != nil || added || v.ID != av {
		t.Fatalf("recording the applied state again = %+v added %v err %v, want version %d kept", v, added, err, av)
	}
	if slices.ContainsFunc(hist, func(v NotesVersion) bool { return v.ID == *cur.VersionID }) {
		t.Errorf("the curation's proposed state %d joined the history", *cur.VersionID)
	}
}

// The usage counts: rounds a harness file existed for, the uses recorded
// since, and the unused candidates.
func TestNotesUsageCountsRoundsAndUses(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	for i := range NotesUnusedRounds {
		files := []string{"probe_spec.rb", "run.sh"}
		if i >= NotesUnusedRounds-2 {
			files = append(files, "late.sh")
		}
		if err := st.RecordNotesRound(ctx, repo.ID, files, clk.Now()); err != nil {
			t.Fatal(err)
		}
		clk.Add(time.Hour)
	}
	if err := st.RecordNotesUsage(ctx, repo.ID, "r-judge", []string{"run.sh"}, clk.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordNotesUsage(ctx, repo.ID, "r-judge", []string{"run.sh"}, clk.Now()); err != nil { // once per run
		t.Fatal(err)
	}
	uses, err := st.NotesFileUses(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]NotesFileUse{}
	for _, u := range uses {
		got[u.File] = u
	}
	if u := got["probe_spec.rb"]; u.Rounds != NotesUnusedRounds || u.Uses != 0 || !u.Unused() {
		t.Errorf("probe_spec.rb = %+v, want an unused candidate", u)
	}
	if u := got["run.sh"]; u.Uses != 1 || u.Unused() || u.LastUsed == nil {
		t.Errorf("run.sh = %+v", u)
	}
	if u := got["late.sh"]; u.Rounds != 2 || u.Unused() {
		t.Errorf("late.sh = %+v: too young to be a candidate", u)
	}
	// A file that is gone is forgotten; one that comes back starts over.
	if err := st.RecordNotesRound(ctx, repo.ID, []string{"run.sh"}, clk.Now()); err != nil {
		t.Fatal(err)
	}
	if uses, _ := st.NotesFileUses(ctx, repo.ID); len(uses) != 1 || uses[0].File != "run.sh" {
		t.Errorf("after the probes went: %+v", uses)
	}
}

// Retention never touches the notes tables, the findings, the misses or the
// judge's stored results.
func TestPruneLeavesNotesFindingsAndMissesAlone(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewing)
	if _, _, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromImport, Content: notesContent("n\n", "a.sh", "a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateNotesProposal(ctx, NotesProposalInput{RepoID: repo.ID, Kind: ProposalCuration, State: ProposalInvalid, Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordNotesRound(ctx, repo.ID, []string{"a.sh"}, clk.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordNotesUsage(ctx, repo.ID, "r-1", []string{"a.sh"}, clk.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, Run{ID: "r-1", PRID: pr.ID, Round: 1, Role: "codex-judge", Kind: RunInitial, TargetSHA: "h", State: RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFindings(ctx, "r-1", pr.ID, 1, []Finding{{FindingID: "F1", Verdict: FindingPosted}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, "UPDATE runs SET result_json = '{}' WHERE id = 'r-1'"); err != nil {
		t.Fatal(err)
	}
	m, err := st.UpsertMiss(ctx, Miss{PRID: pr.ID, SourceURL: "https://github.com/talkable/talkable/pull/7#discussion_r1", SourceKind: MissSourceThread,
		Reviewer: "rev-ann", ReviewedSHA: "h", Class: MissMiss, Raised: MissRaisedNone, Scope: MissScopeRepo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateNotesProposal(ctx, NotesProposalInput{RepoID: repo.ID, Kind: ProposalCuration, State: ProposalPending,
		Misses: []ProposalMiss{{MissID: m.ID, Outcome: MissSkipped, Reason: "covered"}}}); err != nil {
		t.Fatal(err)
	}
	tables := []string{"notes_versions", "harness_blobs", "notes_version_files", "notes_proposals", "notes_files", "notes_usage", "findings", "misses",
		"notes_proposal_misses"}
	before := map[string]int{}
	for _, tb := range tables {
		before[tb] = countRows(t, st, tb)
	}
	clk.Add(10 * 365 * 24 * time.Hour)
	if _, err := st.Prune(ctx, time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, tb := range tables {
		if n := countRows(t, st, tb); n != before[tb] || n == 0 {
			t.Errorf("%s: %d rows after the prune, %d before", tb, n, before[tb])
		}
	}
	var result string
	if err := st.DB().QueryRowContext(ctx, "SELECT result_json FROM runs WHERE id = 'r-1'").Scan(&result); err != nil || result != "{}" {
		t.Errorf("runs.result_json after the prune = %q, %v", result, err)
	}
}
