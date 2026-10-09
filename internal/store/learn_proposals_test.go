package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMigrationV23ToV24LearnProposals reopens a registry the previous
// binary wrote (user_version 23): the proposal, input and version tables
// and their indexes appear empty, and the schema's CHECKs refuse an
// unknown trigger, state, input kind or outcome.
func TestMigrationV23ToV24LearnProposals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var stmts []string
	for _, m := range migrations[:23] {
		stmts = append(stmts, m.sql)
	}
	stmts = append(stmts, "PRAGMA user_version = 23")
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v23 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v23 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 24 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	for _, obj := range []struct{ kind, name string }{{"table", "learn_proposals"}, {"table", "learn_proposal_inputs"},
		{"table", "learn_versions"}, {"index", "learn_proposals_state"}, {"index", "learn_proposal_inputs_input"}} {
		var n int
		if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?", obj.kind, obj.name).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s %s: %d, %v", obj.kind, obj.name, n, err)
		}
	}
	if n, err := st.CountLearnProposals(ctx, LearnGating); err != nil || n != 0 {
		t.Fatalf("proposals after the migration = %d, %v", n, err)
	}
	ts := FormatTime(t0)
	proposal := func(trigger, state string) string {
		return `INSERT INTO learn_proposals (trigger_reason, base_sha, branch, state, created_at) VALUES ('` + trigger + `', 'b', 'learn/x', '` +
			state + `', '` + ts + `')`
	}
	if _, err := st.DB().ExecContext(ctx, proposal(LearnTriggerRequest, LearnGating)); err != nil {
		t.Fatalf("a good proposal: %v", err)
	}
	input := func(kind, outcome string) string {
		return `INSERT INTO learn_proposal_inputs (proposal_id, kind, input_id, outcome) VALUES (1, '` + kind + `', 9, '` + outcome + `')`
	}
	for name, q := range map[string]string{
		"trigger": proposal("whim", LearnGating),
		"state":   proposal(LearnTriggerEvidence, "pending"),
		"kind":    input("comment", MissNoted),
		"outcome": input(LearnInputMiss, "maybe"),
		"gate":    `INSERT INTO learn_proposal_inputs (proposal_id, kind, input_id, outcome, gate) VALUES (1, 'miss', 10, 'noted', 'better')`,
	} {
		if _, err := st.DB().ExecContext(ctx, q); err == nil {
			t.Errorf("%s: the CHECK accepted a bad value", name)
		}
	}
}

// generalMiss stores a general miss on pr (the editor's kind of input).
func generalMiss(t *testing.T, st *Store, pr PR, n int) Miss {
	t.Helper()
	m, err := st.UpsertMiss(context.Background(), Miss{PRID: pr.ID, SourceURL: "https://github.com/talkable/talkable/pull/7#discussion_r" + strconv.Itoa(n),
		SourceKind: MissSourceThread, Reviewer: "rev-ann", ReviewedSHA: "abc", Class: MissMiss, Severity: "P1", Path: "app/a.rb", Line: n,
		Title: "Missed a race", Lesson: "When two writers share a row, check the lock.", Scope: MissScopeGeneral})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// generalDecline stores a general decline on pr, declined as wrong.
func generalDecline(t *testing.T, st *Store, pr PR, id int64) Decline {
	t.Helper()
	in := decline(pr, id, DeclineDeclined)
	in.Verdict, in.Scope, in.Lesson = VerdictWrong, MissScopeGeneral, "When a receiver is checked upstream, do not report it."
	d, err := st.UpsertDecline(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func declineByID(t *testing.T, st *Store, id int64) Decline {
	t.Helper()
	ds, err := st.Declines(context.Background(), DeclineFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("no decline %d", id)
	return Decline{}
}

// proposeLearn stores a gating proposal with inputs.
func proposeLearn(t *testing.T, st *Store, inputs ...LearnInput) LearnProposal {
	t.Helper()
	p, err := st.CreateLearnProposal(context.Background(), LearnProposalInput{Trigger: LearnTriggerEvidence, BaseSHA: "base1",
		Branch: "learn/20261009-070000", CandidateSHA: "cand1", Files: []string{"skills/magnum-review/SKILL.md"}, State: LearnGating,
		Model: "sonnet", PromptSHA: "p1", Scratch: "/s", RunDir: "/r", Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCreateLearnProposalLinksItsInputs(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	m := generalMiss(t, st, pr, 40)
	d := generalDecline(t, st, pr, 1)

	p := proposeLearn(t, st, LearnInput{Kind: LearnInputDecline, InputID: d.ID, Outcome: MissSkipped, Reason: "the rule exists"},
		LearnInput{Kind: LearnInputMiss, InputID: m.ID, Outcome: MissNoted, Where: "SKILL.md section 4"})
	want := LearnProposal{ID: p.ID, Trigger: LearnTriggerEvidence, BaseSHA: "base1", Branch: "learn/20261009-070000", CandidateSHA: "cand1",
		Files: []string{"skills/magnum-review/SKILL.md"}, State: LearnGating, Model: "sonnet", PromptSHA256: "p1", Scratch: "/s", RunDir: "/r",
		CreatedAt: t0}
	if p.ID == 0 || !reflect.DeepEqual(p, want) {
		t.Fatalf("proposal = %+v\nwant %+v", p, want)
	}
	if got, err := st.LearnProposalByID(ctx, p.ID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("by id = %+v, %v", got, err)
	}
	ins, err := st.LearnProposalInputs(ctx, p.ID)
	wantIns := []LearnInput{
		{Kind: LearnInputDecline, InputID: d.ID, Outcome: MissSkipped, Reason: "the rule exists", State: MissNew},
		{Kind: LearnInputMiss, InputID: m.ID, Outcome: MissNoted, Where: "SKILL.md section 4", State: MissNew},
	}
	if err != nil || !reflect.DeepEqual(ins, wantIns) {
		t.Fatalf("inputs = %+v, %v\nwant %+v", ins, err, wantIns)
	}
	if ps, err := st.LearnProposals(ctx, []string{LearnGating, LearnHeld}); err != nil || len(ps) != 1 || ps[0].ID != p.ID {
		t.Fatalf("waiting proposals = %+v, %v", ps, err)
	}
	if n, err := st.CountLearnProposals(ctx, LearnGating); err != nil || n != 1 {
		t.Fatalf("gating = %d, %v", n, err)
	}

	// An invalid proposal is decided at once and keeps magnum's problems;
	// the files and inputs default to empty lists.
	inv, err := st.CreateLearnProposal(ctx, LearnProposalInput{Trigger: LearnTriggerRequest, BaseSHA: "base1", Branch: "learn/x",
		State: LearnInvalid, Reason: "SKILL.md is over its cap", At: t0.Add(time.Minute)})
	if err != nil || inv.DecidedAt == nil || !inv.DecidedAt.Equal(t0.Add(time.Minute)) || inv.Reason != "SKILL.md is over its cap" ||
		inv.Files == nil || len(inv.Files) != 0 {
		t.Fatalf("invalid proposal = %+v, %v", inv, err)
	}
	if all, _ := st.LearnProposals(ctx, nil); len(all) != 2 || all[0].ID != inv.ID {
		t.Fatalf("every proposal, newest first = %+v", all)
	}

	// A bad input or a bad state stores nothing.
	for name, in := range map[string]LearnProposalInput{
		"outcome": {Trigger: LearnTriggerEvidence, BaseSHA: "b", Branch: "learn/y", State: LearnGating,
			Inputs: []LearnInput{{Kind: LearnInputMiss, InputID: m.ID, Outcome: "maybe"}}},
		"kind": {Trigger: LearnTriggerEvidence, BaseSHA: "b", Branch: "learn/y", State: LearnGating,
			Inputs: []LearnInput{{Kind: "comment", InputID: m.ID, Outcome: MissNoted}}},
		"twice": {Trigger: LearnTriggerEvidence, BaseSHA: "b", Branch: "learn/y", State: LearnGating,
			Inputs: []LearnInput{{Kind: LearnInputMiss, InputID: m.ID, Outcome: MissNoted}, {Kind: LearnInputMiss, InputID: m.ID, Outcome: MissSkipped}}},
		"state":   {Trigger: LearnTriggerEvidence, BaseSHA: "b", Branch: "learn/y", State: "pending"},
		"trigger": {Trigger: "whim", BaseSHA: "b", Branch: "learn/y", State: LearnGating},
		"no base": {Trigger: LearnTriggerEvidence, Branch: "learn/y", State: LearnGating},
	} {
		if _, err := st.CreateLearnProposal(ctx, in); err == nil {
			t.Errorf("%s: a bad proposal was stored", name)
		}
	}
	if all, _ := st.LearnProposals(ctx, nil); len(all) != 2 {
		t.Fatalf("refused proposals left rows: %+v", all)
	}
}

func TestDecideLearnProposalIsCompareAndSet(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	p := proposeLearn(t, st)

	clk.Add(time.Hour)
	gate := []byte(`{"rule":"targets could not be replayed"}`)
	held, err := st.DecideLearnProposal(ctx, p.ID, []string{LearnGating}, LearnHeld, "", time.Time{}, gate)
	if err != nil || held.State != LearnHeld || string(held.Gate) != string(gate) || held.DecidedAt != nil {
		t.Fatalf("held = %+v, %v (a held proposal still waits: no decided_at)", held, err)
	}
	if _, err := st.DecideLearnProposal(ctx, p.ID, []string{LearnGating}, LearnPassed, "", time.Time{}, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("a decision from a state it is not in = %v, want ErrConflict", err)
	}
	if _, err := st.DecideLearnProposal(ctx, p.ID, []string{LearnHeld}, "pending", "", time.Time{}, nil); err == nil {
		t.Fatal("an unknown state was accepted")
	}
	if _, err := st.DecideLearnProposal(ctx, 999, []string{LearnHeld}, LearnRejected, "", time.Time{}, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown proposal = %v, want ErrNotFound", err)
	}
	at := t0.Add(2 * time.Hour)
	rej, err := st.DecideLearnProposal(ctx, p.ID, []string{LearnHeld}, LearnRejected, "too broad", at, nil)
	if err != nil || rej.State != LearnRejected || rej.Reason != "too broad" || rej.DecidedAt == nil || !rej.DecidedAt.Equal(at) ||
		string(rej.Gate) != string(gate) || rej.LandedSHA != "" || rej.LandedAt != nil {
		t.Fatalf("rejected = %+v, %v (the gate stays, nothing landed)", rej, err)
	}
	var g map[string]string
	if err := json.Unmarshal(rej.Gate, &g); err != nil || g["rule"] == "" {
		t.Fatalf("gate = %s, %v", rej.Gate, err)
	}
}

// A proposal records when it last began to wait for the landing or the
// operator (passed or held), which the week before it expires counts from:
// made so, or moved there by the gate or by a landing that held it.
func TestALearnProposalRecordsWhenItBeganToWait(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	p := proposeLearn(t, st)
	if p.WaitingSince != nil {
		t.Fatalf("a gating proposal waits since %v", p.WaitingSince)
	}
	clk.Add(6 * 24 * time.Hour)
	passed, err := st.DecideLearnProposal(ctx, p.ID, []string{LearnGating}, LearnPassed, "", time.Time{}, nil)
	if err != nil || passed.WaitingSince == nil || !passed.WaitingSince.Equal(clk.Now()) {
		t.Fatalf("passed = %+v, %v; want waiting since %v", passed, err, clk.Now())
	}
	at := clk.Now().Add(3 * time.Hour)
	held, err := st.DecideLearnProposal(ctx, p.ID, []string{LearnPassed}, LearnHeld, "the checkout is dirty", at, nil)
	if err != nil || held.WaitingSince == nil || !held.WaitingSince.Equal(at) {
		t.Fatalf("held by the landing = %+v, %v; want waiting since %v", held, err, at)
	}
	expired, err := st.DecideLearnProposal(ctx, p.ID, []string{LearnHeld}, LearnExpired, "", at.Add(time.Hour), nil)
	if err != nil || expired.WaitingSince == nil || !expired.WaitingSince.Equal(at) {
		t.Fatalf("expired = %+v, %v; the time it began to wait stays", expired, err)
	}
	made, err := st.CreateLearnProposal(ctx, LearnProposalInput{Trigger: LearnTriggerRequest, BaseSHA: "b", Branch: "learn/x", State: LearnHeld,
		At: t0})
	if err != nil || made.WaitingSince == nil || !made.WaitingSince.Equal(t0) {
		t.Fatalf("made held = %+v, %v", made, err)
	}
}

func TestALandedProposalMarksItsNotedInputsUsed(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	noted, skipped := generalMiss(t, st, pr, 40), generalMiss(t, st, pr, 41)
	declined := generalDecline(t, st, pr, 1)
	p := proposeLearn(t, st, LearnInput{Kind: LearnInputMiss, InputID: noted.ID, Outcome: MissNoted, Where: "section 4"},
		LearnInput{Kind: LearnInputMiss, InputID: skipped.ID, Outcome: MissSkipped, Reason: "one repository's"},
		LearnInput{Kind: LearnInputDecline, InputID: declined.ID, Outcome: MissNoted, Where: "section 5"})
	// A gate result moves nothing.
	if _, err := st.DecideLearnProposal(ctx, p.ID, []string{LearnGating}, LearnPassed, "", time.Time{}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if missByID(t, st, noted.ID).State != MissNew || declineByID(t, st, declined.ID).State != MissNew {
		t.Fatal("a passed proposal moved its inputs")
	}
	landed, _, err := st.LandLearnProposal(ctx, p.ID, []string{LearnPassed}, LearnVersion{LandedSHA: "abc123", SkillSHA: "s", PromptsSHA: "p"})
	if err != nil || landed.State != LearnLanded || landed.LandedSHA != "abc123" {
		t.Fatalf("landed = %+v, %v", landed, err)
	}
	if s := missByID(t, st, noted.ID).State; s != MissUsed {
		t.Errorf("the noted miss = %s, want used", s)
	}
	if s := declineByID(t, st, declined.ID).State; s != MissUsed {
		t.Errorf("the noted decline = %s, want used", s)
	}
	if s := missByID(t, st, skipped.ID).State; s != MissNew {
		t.Errorf("the skipped miss = %s, want new (a later edit may use it)", s)
	}
	ins, err := st.LearnProposalInputs(ctx, p.ID)
	if err != nil || len(ins) != 3 || ins[0].State != MissUsed {
		t.Fatalf("inputs after the landing = %+v, %v", ins, err)
	}
}

func TestAnInputRejectedTwiceIsDismissed(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	m := generalMiss(t, st, pr, 40)
	d := generalDecline(t, st, pr, 1)
	reject := func(reason string, inputs ...LearnInput) LearnProposal {
		t.Helper()
		p := proposeLearn(t, st, inputs...)
		clk.Add(time.Minute)
		if _, err := st.DecideLearnProposal(ctx, p.ID, []string{LearnGating}, LearnRejected, reason, time.Time{}, nil); err != nil {
			t.Fatal(err)
		}
		return p
	}
	reject("too specific", LearnInput{Kind: LearnInputMiss, InputID: m.ID, Outcome: MissNoted, Where: "section 4"},
		LearnInput{Kind: LearnInputDecline, InputID: d.ID, Outcome: MissSkipped, Reason: "covered"})
	if missByID(t, st, m.ID).State != MissNew || declineByID(t, st, d.ID).State != MissNew {
		t.Fatal("one rejection dismissed an input")
	}
	// An expiry never counts as a rejection.
	exp := proposeLearn(t, st, LearnInput{Kind: LearnInputMiss, InputID: m.ID, Outcome: MissNoted, Where: "section 4"})
	if _, err := st.DecideLearnProposal(ctx, exp.ID, []string{LearnGating}, LearnExpired, "not reviewed within 7 days", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	if missByID(t, st, m.ID).State != MissNew {
		t.Fatal("an expiry dismissed an input")
	}
	reject("", LearnInput{Kind: LearnInputMiss, InputID: m.ID, Outcome: MissSkipped, Reason: "covered"})
	if mm := missByID(t, st, m.ID); mm.State != MissDismissed || mm.Dismissed != LearnDismissedRejections {
		t.Fatalf("a miss in two rejected proposals = %s %q, want dismissed saying why", mm.State, mm.Dismissed)
	}
	if s := declineByID(t, st, d.ID).State; s != MissNew {
		t.Fatalf("a decline in one rejected proposal = %s, want new", s)
	}
	reasons, err := st.LearnInputRejections(ctx, LearnInputMiss, m.ID)
	if err != nil || !reflect.DeepEqual(reasons, []string{"too specific"}) {
		t.Fatalf("the miss's rejections = %q, %v (an empty reason is left out)", reasons, err)
	}
	if reasons, err := st.LearnInputRejections(ctx, LearnInputDecline, d.ID); err != nil || !reflect.DeepEqual(reasons, []string{"too specific"}) {
		t.Fatalf("the decline's rejections = %q, %v", reasons, err)
	}
	if reasons, err := st.LearnInputRejections(ctx, LearnInputMiss, d.ID+m.ID+1); err != nil || len(reasons) != 0 {
		t.Fatalf("an input no proposal had = %q, %v", reasons, err)
	}
}

func TestLearnVersionsNewestFirst(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	p := proposeLearn(t, st)
	first, err := st.CreateLearnVersion(ctx, LearnVersion{ProposalID: p.ID, LandedSHA: "aaa", SkillSHA: "s1", PromptsSHA: "q1", LandedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateLearnVersion(ctx, LearnVersion{LandedSHA: "bbb", SkillSHA: "s2", PromptsSHA: "q2", LandedAt: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateLearnVersion(ctx, LearnVersion{SkillSHA: "s3", PromptsSHA: "q3"}); err == nil {
		t.Fatal("a version without a landed commit was stored")
	}
	if err := st.MarkLearnRestarted(ctx, first.ID, t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The first restart starts the window: a later one changes nothing.
	if err := st.MarkLearnRestarted(ctx, first.ID, t0.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkLearnRestarted(ctx, 999, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown version = %v, want ErrNotFound", err)
	}
	vs, err := st.LearnVersions(ctx)
	if err != nil || len(vs) != 2 || vs[0].ID != second.ID || vs[1].ID != first.ID {
		t.Fatalf("versions = %+v, %v; want newest first", vs, err)
	}
	if vs[1].ProposalID != p.ID || vs[1].RestartedAt == nil || !vs[1].RestartedAt.Equal(t0.Add(10*time.Minute)) || vs[1].LandedSHA != "aaa" ||
		vs[0].ProposalID != 0 || vs[0].RestartedAt != nil || !strings.HasPrefix(vs[0].SkillSHA, "s2") {
		t.Fatalf("versions = %+v", vs)
	}
}

// gateLearn moves proposal p from gating to state as its gate would, with
// what the gate made of its targets.
func gateLearn(t *testing.T, st *Store, p LearnProposal, state, rule string, targets ...LearnTarget) LearnProposal {
	t.Helper()
	got, err := st.GateLearnProposal(context.Background(), p.ID, state, rule, time.Time{}, []byte(`{"state":"`+state+`"}`), targets)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A target its baseline already covers (a miss the baseline finds, a
// decline it no longer reports) is no target: the gate's decision
// dismisses the input, whatever the verdict, and its link row and
// `magnum misses` say why.
func TestAGateRetiresAnInputItsBaselineAlreadyCovers(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	m, other := generalMiss(t, st, pr, 40), generalMiss(t, st, pr, 41)
	d := generalDecline(t, st, pr, 1)
	p := proposeLearn(t, st, LearnInput{Kind: LearnInputMiss, InputID: m.ID, Outcome: MissNoted, Where: "section 4"},
		LearnInput{Kind: LearnInputMiss, InputID: other.ID, Outcome: MissNoted, Where: "section 4"},
		LearnInput{Kind: LearnInputDecline, InputID: d.ID, Outcome: MissNoted, Where: "section 5"})
	got := gateLearn(t, st, p, LearnHeld, "no target could be replayed", LearnTarget{Kind: LearnInputMiss, InputID: m.ID, Gate: LearnTargetBaseline},
		LearnTarget{Kind: LearnInputDecline, InputID: d.ID, Gate: LearnTargetBaseline})
	if got.State != LearnHeld || got.Reason != "no target could be replayed" || got.WaitingSince == nil {
		t.Fatalf("proposal = %+v", got)
	}
	if mm := missByID(t, st, m.ID); mm.State != MissDismissed || mm.Dismissed != LearnDismissedFound || mm.DismissedBy == nil || *mm.DismissedBy != p.ID {
		t.Fatalf("the miss found at the baseline = %s %q by %v, want dismissed by proposal %d", mm.State, mm.Dismissed, mm.DismissedBy, p.ID)
	}
	if dd := declineByID(t, st, d.ID); dd.State != MissDismissed || dd.Dismissed != LearnDismissedNotReported || dd.DismissedBy == nil {
		t.Fatalf("the decline not reported at the baseline = %+v", dd)
	}
	if om := missByID(t, st, other.ID); om.State != MissNew || om.Dismissed != "" || om.DismissedBy != nil {
		t.Fatalf("the other miss = %+v, want new", om)
	}
	ins, err := st.LearnProposalInputs(ctx, p.ID)
	if err != nil || len(ins) != 3 {
		t.Fatalf("inputs = %+v, %v", ins, err)
	}
	for _, in := range ins {
		want := LearnInput{Kind: in.Kind, InputID: in.InputID, Outcome: MissNoted, Where: in.Where, State: MissNew}
		switch {
		case in.Kind == LearnInputMiss && in.InputID == m.ID:
			want.Gate, want.Gated, want.Dismissed, want.State = LearnTargetBaseline, 1, LearnDismissedFound, MissDismissed
		case in.Kind == LearnInputDecline && in.InputID == d.ID:
			want.Gate, want.Gated, want.Dismissed, want.State = LearnTargetBaseline, 1, LearnDismissedNotReported, MissDismissed
		}
		if !reflect.DeepEqual(in, want) {
			t.Errorf("input %s %d = %+v, want %+v", in.Kind, in.InputID, in, want)
		}
	}
	// Only the gate decides from gating with targets, and only to a gate's
	// state: anything else is refused and changes nothing.
	q := proposeLearn(t, st, LearnInput{Kind: LearnInputMiss, InputID: other.ID, Outcome: MissNoted, Where: "section 4"})
	for _, to := range []string{LearnRejected, LearnLanded, LearnGating} {
		if _, err := st.GateLearnProposal(ctx, q.ID, to, "", time.Time{}, nil, nil); err == nil {
			t.Errorf("a gate's decision to %s was taken", to)
		}
	}
	if _, err := st.GateLearnProposal(ctx, q.ID, LearnFailed, "", time.Time{}, nil,
		[]LearnTarget{{Kind: LearnInputMiss, InputID: other.ID, Gate: "better"}}); err == nil {
		t.Error("an unknown target outcome was taken")
	}
	if cur, _ := st.LearnProposalByID(ctx, q.ID); cur.State != LearnGating {
		t.Fatalf("a refused decision moved the proposal to %s", cur.State)
	}
}

// An input whose case two failed gates compared and did not improve is
// dismissed: the editor would note it again every run, and every gate of it
// would fail the same way. A failed gate in which it improved (another case
// regressed), a held one and a gate that never compared it do not count.
func TestAnInputTwoFailedGatesDidNotImproveIsDismissed(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	m, better, never := generalMiss(t, st, pr, 40), generalMiss(t, st, pr, 41), generalMiss(t, st, pr, 42)
	noted := func(ids ...int64) []LearnInput {
		var out []LearnInput
		for _, id := range ids {
			out = append(out, LearnInput{Kind: LearnInputMiss, InputID: id, Outcome: MissNoted, Where: "section 4"})
		}
		return out
	}
	gate := func(state string, targets ...LearnTarget) LearnProposal {
		t.Helper()
		clk.Add(time.Minute)
		return gateLearn(t, st, proposeLearn(t, st, noted(m.ID, better.ID, never.ID)...), state, "no target improved", targets...)
	}
	target := func(id int64, outcome string) LearnTarget {
		return LearnTarget{Kind: LearnInputMiss, InputID: id, Gate: outcome}
	}
	gate(LearnFailed, target(m.ID, LearnTargetNotImproved), target(better.ID, LearnTargetImproved))
	gate(LearnHeld, target(m.ID, LearnTargetNotImproved))
	if mm := missByID(t, st, m.ID); mm.State != MissNew {
		t.Fatalf("after one failed gate and a held one: %s, want new", mm.State)
	}
	last := gate(LearnFailed, target(m.ID, LearnTargetNotImproved), target(better.ID, LearnTargetImproved))
	if mm := missByID(t, st, m.ID); mm.State != MissDismissed || mm.Dismissed != LearnDismissedFailures || mm.DismissedBy == nil || *mm.DismissedBy != last.ID {
		t.Fatalf("after two failed gates = %s %q by %v, want dismissed by %d", mm.State, mm.Dismissed, mm.DismissedBy, last.ID)
	}
	for _, id := range []int64{better.ID, never.ID} {
		if s := missByID(t, st, id).State; s != MissNew {
			t.Errorf("miss %d = %s, want new", id, s)
		}
	}
	if LearnDismissFailures != 2 || !strings.Contains(LearnDismissedFailures, "2") {
		t.Fatalf("LearnDismissFailures = %d, said %q", LearnDismissFailures, LearnDismissedFailures)
	}
	ins, err := st.LearnProposalInputs(ctx, last.ID)
	if err != nil {
		t.Fatal(err)
	}
	gated := map[int64]int{}
	for _, in := range ins {
		gated[in.InputID] = in.Gated
	}
	if gated[m.ID] != 3 || gated[better.ID] != 2 || gated[never.ID] != 0 {
		t.Fatalf("gates that compared each input = %v", gated)
	}
}

// The editor reads the failed gates of the proposals that noted an input,
// oldest first, with the gate's rule.
func TestLearnInputHistoryListsTheFailedGatesThatNotedIt(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	m := generalMiss(t, st, pr, 40)
	in := LearnInput{Kind: LearnInputMiss, InputID: m.ID, Outcome: MissNoted, Where: "section 4"}
	first := gateLearn(t, st, proposeLearn(t, st, in), LearnFailed, "no target improved (miss-1 as at the baseline)")
	clk.Add(time.Minute)
	gateLearn(t, st, proposeLearn(t, st, in), LearnHeld, "no target could be replayed")
	clk.Add(time.Minute)
	skipped := in
	skipped.Outcome, skipped.Where, skipped.Reason = MissSkipped, "", "covered"
	gateLearn(t, st, proposeLearn(t, st, skipped), LearnFailed, "regressed: coupon-scope lost scope")
	clk.Add(time.Minute)
	second := gateLearn(t, st, proposeLearn(t, st, in), LearnFailed, "regressed: coupon-scope lost scope")
	got, err := st.LearnInputHistory(ctx, LearnInputMiss, m.ID)
	want := []LearnInputGate{{Proposal: first.ID, State: LearnFailed, Rule: "no target improved (miss-1 as at the baseline)"},
		{Proposal: second.ID, State: LearnFailed, Rule: "regressed: coupon-scope lost scope"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %+v, %v\nwant %+v", got, err, want)
	}
	if got, err := st.LearnInputHistory(ctx, LearnInputDecline, m.ID); err != nil || len(got) != 0 {
		t.Fatalf("another kind's history = %+v, %v", got, err)
	}
}
