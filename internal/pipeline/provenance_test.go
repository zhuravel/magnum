package pipeline

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum"
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

// Every provenance entry carries a short title, and a pre-existing problem
// the judge proved next to the PR's changes is marked nearby: `magnum debt`
// lists them. A title is one line (control characters and runs of blanks
// folded to one space) of at most 120 runes; nearby holds only for true (or
// "true"), and never on a posted finding, which is the PR's own.
func TestParseProvenanceReadsTitleAndNearby(t *testing.T) {
	long := strings.Repeat("word ", 40)
	r, ok := parseResult([]byte(`{"status":"posted","provenance":[
	 {"id":"F1","title":"  Export skips\nthe \u001b[31msite\t check ","severity":"P2","path":"lib/export.rb","line":31,"sources":["claude-review"],"verdict":"rejected","reason_code":"pre_existing","nearby":true},
	 {"id":"F2","title":"` + long + `","severity":"P1","sources":["judge"],"verdict":"rejected","reason_code":"pre_existing","nearby":"true"},
	 {"id":"F3","title":7,"severity":"P2","sources":["judge"],"verdict":"rejected","reason_code":"pre_existing","nearby":"yes"},
	 {"id":"F4","title":"Total skips tax","severity":"P2","sources":["judge"],"verdict":"posted","nearby":true},
	 {"id":"F5","severity":"P3","sources":["judge"],"verdict":"rejected","reason_code":"speculative","nearby":false}
	]}`))
	if !ok {
		t.Fatal("parse failed")
	}
	type titled struct {
		Title  string
		Nearby bool
	}
	var got []titled
	for _, f := range r.Provenance {
		got = append(got, titled{f.Title, f.Nearby})
	}
	want := []titled{
		{"Export skips the [31msite check", true},
		{strings.TrimSpace(strings.Repeat("word ", 24)) + "…", true}, // 119 runes and the ellipsis
		{"", false},
		{"Total skips tax", false},
		{"", false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("titles and nearby = %+v\nwant %+v", got, want)
	}
}

// The skill's example result file names a title on every provenance entry
// and marks its proven pre-existing P2 nearby, and the parser reads them.
func TestTheSkillsExampleResultCarriesTitlesAndANearbyFinding(t *testing.T) {
	_, sec8, ok := strings.Cut(string(magnum.Skill), "## 8. Write the result")
	if !ok {
		t.Fatal("SKILL.md has no section 8")
	}
	_, block, ok := strings.Cut(sec8, "```json\n")
	if !ok {
		t.Fatal("section 8 has no JSON example")
	}
	block, _, _ = strings.Cut(block, "```")
	r, ok := parseResult([]byte(block))
	if !ok || len(r.Provenance) == 0 {
		t.Fatalf("the example does not parse: %+v", r)
	}
	nearby := 0
	for _, f := range r.Provenance {
		if f.Title == "" {
			t.Errorf("entry %s has no title", f.ID)
		}
		if f.Nearby {
			nearby++
			if f.Verdict != store.FindingRejected || f.ReasonCode != "pre_existing" || (f.Severity != "P1" && f.Severity != "P2") {
				t.Errorf("nearby entry %+v is not a rejected pre-existing P1 or P2", f)
			}
		}
	}
	if nearby != 1 {
		t.Errorf("%d nearby entries, want 1", nearby)
	}
}

// A posted round stores the judge's provenance on its judge run, titles and
// the nearby mark included.
func TestPostedRoundRecordsTitlesAndNearbyFindings(t *testing.T) {
	e := newEnv(t)
	p := e.judgePosts(601, "COMMENTED", "COMMENT")
	p.extra = map[string]any{"provenance": []any{
		map[string]any{"id": "F1", "title": "Total skips tax", "severity": "P2", "path": "app/a.rb", "line": 3, "sources": []string{"judge"}, "verdict": "posted"},
		map[string]any{"id": "F2", "title": "Export skips site check", "severity": "P2", "path": "lib/export.rb", "line": 31,
			"sources": []string{"claude-review"}, "verdict": "rejected", "reason_code": "pre_existing", "nearby": true},
	}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	got, err := e.st.FindingsSince(e.ctx, t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Title != "Total skips tax" || got[0].Nearby ||
		got[1].Title != "Export skips site check" || !got[1].Nearby || got[1].ReasonCode != "pre_existing" {
		t.Fatalf("findings = %+v", got)
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
