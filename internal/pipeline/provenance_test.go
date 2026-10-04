package pipeline

import (
	"reflect"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

func TestParseProvenance(t *testing.T) {
	r, ok := parseResult([]byte(`{"status":"posted","provenance":[
	 {"id":"F1","severity":"p2","path":"app/models/order.rb","line":42,"sources":["claude-review","Judge","claude-review"],"verdict":"posted","reason_code":"duplicate"},
	 {"id":"F2","severity":"P3","path":"app/x.rb","line":"7","sources":"codex-review","verdict":"Rejected","reason_code":"Not reproducible"},
	 {"id":"F2","severity":"P1","sources":["judge"],"verdict":"posted","line":-3},
	 {"id":4,"sources":[],"verdict":"dropped","reason_code":"style-only"},
	 {"id":"F5","verdict":"maybe"},
	 "F6",
	 null
	]}`))
	if !ok {
		t.Fatal("parse failed")
	}
	want := []findingRecord{
		{ID: "F1", Severity: "P2", Path: "app/models/order.rb", Line: 42, Sources: []string{"claude-review", "judge"}, Verdict: store.FindingPosted},
		{ID: "F2", Severity: "P3", Path: "app/x.rb", Line: 7, Sources: []string{"codex-review"}, Verdict: store.FindingRejected, ReasonCode: "not_reproducible"},
		{ID: "#3", Severity: "P1", Sources: []string{"judge"}, Verdict: store.FindingPosted},
		{ID: "4", Sources: []string{}, Verdict: store.FindingRejected, ReasonCode: "style_only"},
	}
	if !reflect.DeepEqual(r.Provenance, want) {
		t.Fatalf("provenance = %+v\nwant %+v", r.Provenance, want)
	}
	// Result files written before provenance, or with a malformed one.
	for _, raw := range []string{`{"status":"posted","findings":{"P2":1}}`, `{"status":"posted","provenance":{"F1":"posted"}}`} {
		if r, ok := parseResult([]byte(raw)); !ok || r.Provenance != nil {
			t.Errorf("%s: provenance = %+v", raw, r.Provenance)
		}
	}
}

// A posted round stores the judge's provenance on its judge run; recording
// is replaced, not appended, and a result without provenance stores none.
func TestPostedRoundRecordsFindings(t *testing.T) {
	e := newEnv(t)
	p := e.judgePosts(601, "CHANGES_REQUESTED", "REQUEST_CHANGES")
	p.extra = map[string]any{"provenance": []any{
		map[string]any{"id": "F1", "severity": "P1", "path": "app/a.rb", "line": 3, "sources": []string{"claude-review"}, "verdict": "posted"},
		map[string]any{"id": "F2", "severity": "P2", "path": "app/b.rb", "line": 9, "sources": []string{"codex-review", "judge"}, "verdict": "posted"},
		map[string]any{"id": "F3", "severity": "P3", "sources": []string{"claude-review"}, "verdict": "rejected", "reason_code": "speculative"},
	}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	judge := e.runOf(agents.RoleJudge, KindInitial)
	got, err := e.st.FindingsSince(e.ctx, t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("findings = %+v", got)
	}
	for _, f := range got {
		if f.RunID != judge.ID || f.PRID != e.pr.ID || f.Round != 1 || f.Repo != "talkable/talkable" {
			t.Errorf("finding %s = %+v", f.FindingID, f)
		}
	}
	if f := got[2]; f.FindingID != "F3" || f.Verdict != store.FindingRejected || f.ReasonCode != "speculative" || f.Path != "" {
		t.Errorf("rejected finding = %+v", f)
	}
	if f := got[1]; !reflect.DeepEqual(f.Sources, []string{"codex-review", "judge"}) || f.Line != 9 {
		t.Errorf("shared finding = %+v", f)
	}
}

func TestFindingsAreRecordedOnlyForAPostedRoundWithProvenance(t *testing.T) {
	// An older result file: no provenance, no rows.
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if got, _ := e.st.FindingsSince(e.ctx, t0.Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("findings without provenance = %+v", got)
	}

	// A dry run plans a review: nothing is recorded.
	e = newEnv(t)
	p := e.judgePosts(601, "COMMENTED", "COMMENT")
	p.gh, p.status = nil, statusDryRun
	p.extra = map[string]any{"provenance": []any{map[string]any{"id": "F1", "sources": []string{"judge"}, "verdict": "posted"}}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	in := e.input(KindInitial)
	in.DryRun = true
	if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomeDryRun {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if got, _ := e.st.FindingsSince(e.ctx, t0.Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("findings of a dry run = %+v", got)
	}
}
