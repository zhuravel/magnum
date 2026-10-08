package pipeline

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
)

// A round of the judge alone (a delta check, a same-head re-review) builds
// on the round before it: its record carries what that round's judge went
// without, named with that round, so auto-approval refuses its review too;
// the record of each round stays its own (auto-approval reads the record of
// the round whose review it approves). A round whose reviewers run carries
// nothing. A record written before there was one per round is read as its
// round's.
func TestAJudgeAloneRoundCarriesWhatTheRoundBeforeWentWithout(t *testing.T) {
	missed := []MissingReport{{Role: string(agents.RoleCodexReview), Status: "login_required"}}
	for _, tc := range []struct {
		name    string
		edit    func(e *env, in *RoundInput)
		carried []MissingReport
	}{
		{"a delta check", func(e *env, in *RoundInput) {
			in.Roles = []config.Role{e.judgeRole()}
			in.Previous = &PreviousReview{ID: 601, Event: "COMMENTED", SHA: prevSHA, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
			in.DeltaCheck = &DeltaCheck{Lines: 1, Files: []DeltaFile{{Path: "app/models/coupon.rb", Status: "modified"}}}
		}, []MissingReport{{Role: string(agents.RoleCodexReview), Status: "login_required", Round: 1}}},
		{"a same-head re-review", func(e *env, in *RoundInput) {
			in.Roles, in.SameHead = []config.Role{e.judgeRole()}, true
			in.Previous = &PreviousReview{ID: 601, Event: "COMMENTED", SHA: target, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
		}, []MissingReport{{Role: string(agents.RoleCodexReview), Status: "login_required", Round: 1}}},
		{"a re-review with its reviewers", func(e *env, in *RoundInput) {
			in.Previous = &PreviousReview{ID: 601, Event: "COMMENTED", SHA: prevSHA, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			old, _ := json.Marshal(MissingReports{Round: 1, Head: prevSHA, Missing: missed})
			if err := e.st.SetKV(e.ctx, KVMissingReports(e.pr.ID), string(old)); err != nil {
				t.Fatal(err)
			}
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "COMMENTED", "COMMENT").behavior(t)}
			in := e.input(KindRereview)
			in.Round = 2
			tc.edit(e, &in)
			if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomePosted {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			rec, ok := ReadRoundMissingReports(e.ctx, e.st, e.pr.ID, 2)
			if !ok || rec.Round != 2 || rec.Head != target || len(rec.Missing) != 0 || !slices.Equal(rec.Carried, tc.carried) {
				t.Fatalf("round 2's record = %+v, %v; want carried %+v", rec, ok, tc.carried)
			}
			if tc.carried != nil {
				if got := rec.String(); got != "codex-review (login_required in round 1)" {
					t.Errorf("round 2's record names %q", got)
				}
			} else if got := rec.String(); got != "" {
				t.Errorf("round 2's record names %q", got)
			}
			first, ok := ReadRoundMissingReports(e.ctx, e.st, e.pr.ID, 1)
			if !ok || first.Head != prevSHA || first.String() != "codex-review (login_required)" {
				t.Fatalf("round 1's record = %+v, %v", first, ok)
			}
			if latest, ok := ReadMissingReports(e.ctx, e.st, e.pr.ID); !ok || latest.Round != 2 {
				t.Fatalf("latest record = %+v, %v", latest, ok)
			}
		})
	}
}

// The PR's record keeps one entry per round, the latest missingRecords: a
// round recorded again (a continue's judge) replaces its entry, and a
// round of the judge alone carries from the latest round before it, its
// carried reports included, with the round that went without them.
func TestThePRsRecordKeepsItsLatestRounds(t *testing.T) {
	e := newEnv(t)
	record := func(round int, reports ...agents.Report) MissingReports {
		t.Helper()
		m, err := RecordMissingReports(e.ctx, e.st, e.pr.ID, round, target, reports)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	heard := agents.Report{Role: string(agents.RoleClaude), Path: "claude-review.md", Status: ReportOK}
	record(1, heard, agents.Report{Role: string(agents.RoleCodexReview), Status: "timeout", Missing: true})
	if m := record(1, heard, agents.Report{Role: string(agents.RoleCodexReview), Status: "login_required", Missing: true}); m.String() != "codex-review (login_required)" {
		t.Fatalf("round 1 recorded again = %q", m.String())
	}
	if m := record(2); m.String() != "codex-review (login_required in round 1)" {
		t.Fatalf("round 2 = %q", m.String())
	}
	if m := record(3); m.String() != "codex-review (login_required in round 1)" {
		t.Fatalf("round 3, after round 2 of the judge alone = %q", m.String())
	}
	for round := 4; round <= missingRecords+2; round++ {
		record(round, heard)
	}
	for round, want := range map[int]bool{1: false, 2: false, 3: true, missingRecords + 2: true} {
		if _, ok := ReadRoundMissingReports(e.ctx, e.st, e.pr.ID, round); ok != want {
			t.Errorf("round %d recorded %v, want %v", round, ok, want)
		}
	}
}
