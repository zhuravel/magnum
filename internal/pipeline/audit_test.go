package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
)

// An audit pass (the engine's audit_every) gives its judge the areas no
// round probed, whose files changed since, and an earlier probe to run
// again: magnum writes them to audit.json in the report directory, since
// their paths and behaviours come from the PR, and the recovery prompt
// names the file and says to probe each through its real entry point with
// a control, never to post an unknown area as a finding, then to re-review
// the delta; the reviewers re-review the delta as with a cold judge. A
// recovery that is no audit pass names none.
func TestAnAuditPassNamesItsAreasInAFileNeverInThePrompt(t *testing.T) {
	areas := []AuditArea{
		{EvidenceEntry: EvidenceEntry{Path: "lib/cleanup.rb", Behaviour: "deletes the expired rows", State: EvidenceUnknown}, Why: "irreversible write"},
		{EvidenceEntry: EvidenceEntry{Path: "app/a.rb", Behaviour: "the restore keeps the tier", State: EvidenceProbed,
			Command: "bin/rspec spec/a_spec.rb", Control: true, Result: "passed"}, Head: "b1", Round: 2, Why: "rotation"},
	}
	for _, audit := range []bool{true, false} {
		e := newEnv(t)
		e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(611, "COMMENTED", "COMMENT").behavior(t)}
		in := e.input(KindRecovery)
		in.ColdJudge = true
		in.Previous = &PreviousReview{ID: 3012345678, Event: "COMMENTED", SHA: prevSHA}
		if audit {
			in.Audit = areas
		}
		res, err := e.r.RunRound(e.ctx, in)
		if err != nil || res.Outcome != OutcomePosted {
			t.Fatalf("RunRound = %+v, %v", res, err)
		}
		path := filepath.Join(res.ReportDir, AuditFile)
		judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
		if !audit {
			if strings.Contains(judge, "audit pass") {
				t.Fatalf("a recovery without areas carries the audit clause:\n%s", judge)
			}
			continue
		}
		mustContain(t, "judge prompt", judge, "This is an audit pass: "+path+" lists 2 areas", "real entry point", "with a control",
			"never a finding", "git log --oneline "+prevSHA+".."+target)
		for _, pr := range []string{"lib/cleanup.rb", "expired rows", "app/a.rb"} {
			if strings.Contains(judge, pr) {
				t.Errorf("the judge's prompt carries %q, which the PR wrote:\n%s", pr, judge)
			}
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var file struct {
			HeadSHA string      `json:"head_sha"`
			Areas   []AuditArea `json:"areas"`
		}
		if err := json.Unmarshal(b, &file); err != nil || file.HeadSHA != target || !reflect.DeepEqual(file.Areas, areas) {
			t.Fatalf("%s = %+v, %v\n%s", AuditFile, file, err, b)
		}
		claude := e.ag.submitsFor(agents.RoleClaude)[0].Text
		mustContain(t, "claude-review prompt", claude, prevSHA)
	}
}
