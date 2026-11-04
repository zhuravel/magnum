package factory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// endDir is a task directory for CheckEnd and the stage's start: files
// written with writeAt are newer than since unless the test says otherwise.
type endDir struct {
	t     *testing.T
	dir   string
	since time.Time
}

func newEndDir(t *testing.T) *endDir {
	t.Helper()
	return &endDir{t: t, dir: t.TempDir(), since: t0}
}

// write writes name with text a minute after the stage's start.
func (d *endDir) write(name, text string) { d.writeAt(name, text, d.since.Add(time.Minute)) }

// writeAt writes name with text and gives it the modification time at.
func (d *endDir) writeAt(name, text string, at time.Time) {
	d.t.Helper()
	path := filepath.Join(d.dir, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		d.t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		d.t.Fatal(err)
	}
}

func (d *endDir) check(fact string, roles ...string) (bool, string) {
	return CheckEnd(fact, d.dir, d.since, roles...)
}

// A report as the judge's prompt asks for it: the decisions first, then
// the cause, the proof, the plan, the helpers, the doubts and the files.
const goodReport = `# PR-12345

## Architecture decisions
none

## Cause
Copying a campaign copies its activities before their reward rules (app/models/campaign.rb:88).

## Proof
Reproduced: yes. ` + "`bin/rspec spec/models/campaign_copy_spec.rb:12`" + ` fails with 0 points, want 25.

## Plan
Copy the reward rules first, then point each activity at its copy.

## Helpers
- app/models/reward_rule.rb:40 ` + "`RewardRule#duplicate`" + ` copies one rule with its tiers

## Doubts
none

## Files
- app/models/campaign.rb
- spec/models/campaign_copy_spec.rb
`

const goodAcceptance = `{"checks": [{"id": "A1", "point": "copied activities keep their points", "given": "a campaign with a 25-point activity", "then": "its copy awards 25 points", "how": "spec"}]}`

// reportWith is goodReport with section heading replaced by its text
// (the heading line included), or left out when text is "".
func reportWith(heading, text string) string {
	lines := strings.Split(goodReport, "\n")
	var out []string
	skip := false
	for _, l := range lines {
		if strings.HasPrefix(l, "## ") {
			skip = l == "## "+heading
			if skip && text != "" {
				out = append(out, strings.TrimRight(text, "\n"))
			}
		}
		if !skip {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestAReportWithoutArchitectureDecisionsDoesNotPass(t *testing.T) {
	for _, tc := range []struct {
		name, report, want string
	}{
		{"no decisions section", reportWith("Architecture decisions", ""), "Architecture decisions"},
		{"decisions after another section", strings.Replace(reportWith("Architecture decisions", ""), "## Files",
			"## Architecture decisions\nnone\n\n## Files", 1), "Architecture decisions"},
		{"empty decisions", reportWith("Architecture decisions", "## Architecture decisions\n\n"), "Architecture decisions"},
		{"no files", reportWith("Files", ""), "Files"},
		{"files that name none", reportWith("Files", "## Files\nnone\n"), "Files"},
		{"no helpers", reportWith("Helpers", ""), "Helpers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newEndDir(t)
			d.write("report.md", tc.report)
			d.write("acceptance.json", goodAcceptance)
			ok, why := d.check("report")
			if ok || !strings.HasPrefix(why, "report.md: ") || !strings.Contains(why, tc.want) {
				t.Fatalf("CheckEnd(report) = %v, %q; want not done, a why about report.md's %s", ok, why, tc.want)
			}
		})
	}
	d := newEndDir(t)
	d.write("report.md", reportWith("Architecture decisions",
		"## Architecture decisions\n- Store the copy's origin on the rule (a column), not in a side table.\n"))
	d.write("acceptance.json", goodAcceptance)
	if ok, why := d.check("report"); !ok {
		t.Fatalf("a report with a decision: CheckEnd(report) = %v, %q, want done", ok, why)
	}
}

func TestAReportWithoutAcceptanceDoesNotPass(t *testing.T) {
	for _, tc := range []struct {
		name, acceptance string // "" = no file
	}{
		{"no file", ""},
		{"no checks", `{"checks": []}`},
		{"not json", `{"checks": [`},
		{"a check without what must hold", `{"checks": [{"id": "A1", "point": "copies keep their points"}]}`},
		{"a check without an id", `{"checks": [{"then": "its copy awards 25 points"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newEndDir(t)
			d.write("report.md", goodReport)
			if tc.acceptance != "" {
				d.write("acceptance.json", tc.acceptance)
			}
			if ok, why := d.check("report"); ok || !strings.HasPrefix(why, "acceptance.json: ") {
				t.Fatalf("CheckEnd(report) = %v, %q; want not done, a why about acceptance.json", ok, why)
			}
		})
	}
	d := newEndDir(t)
	d.write("report.md", goodReport)
	d.write("acceptance.json", goodAcceptance)
	if ok, why := d.check("report"); !ok {
		t.Fatalf("CheckEnd(report) = %v, %q, want done", ok, why)
	}
}

// questions.json is optional, but one that is there must hold questions
// the operator can answer.
func TestAMalformedQuestionsFileFailsTheReport(t *testing.T) {
	for _, tc := range []struct{ name, questions string }{
		{"not json", `[{"text": "Which base?"`},
		{"not a list", `{"text": "Which base?"}`},
		{"a question without text", `[{"options": [{"label": "master"}, {"label": "release"}]}]`},
		{"one option", `[{"text": "Which base?", "options": [{"label": "master"}]}]`},
		{"a recommendation out of range", `[{"text": "Which base?", "options": [{"label": "master"}, {"label": "release"}], "recommended": 2}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newEndDir(t)
			d.write("report.md", goodReport)
			d.write("acceptance.json", goodAcceptance)
			d.write("questions.json", tc.questions)
			if ok, why := d.check("report"); ok || !strings.HasPrefix(why, "questions.json: ") {
				t.Fatalf("CheckEnd(report) = %v, %q; want not done, a why about questions.json", ok, why)
			}
		})
	}
	for _, questions := range []string{"", "[]",
		`[{"text": "Which base?", "context": "the ticket names release-7", "example": "a PR into release-7", "options": [{"label": "master", "means": "the usual base"}, {"label": "release-7", "means": "the ticket's"}], "recommended": null, "why": "no recommendation", "raised_by": "fx-judge"}]`} {
		d := newEndDir(t)
		d.write("report.md", goodReport)
		d.write("acceptance.json", goodAcceptance)
		if questions != "" {
			d.write("questions.json", questions)
		}
		if ok, why := d.check("report"); !ok {
			t.Fatalf("questions.json %q: CheckEnd(report) = %v, %q, want done", questions, ok, why)
		}
	}
}

// The reproduction question of a bug that did not reproduce: the four ways
// on, one of them recommended.
const reproQuestion = `[{"text": "The bug did not reproduce. How do we go on?", "context": "a failing spec and rails runner both pass",
 "example": "a copied campaign keeps its 25 points here", "options": [
 {"label": "give a hint", "means": "you say where it breaks"}, {"label": "take the other cause", "means": "the cause agent's"},
 {"label": "try Docker or a staging", "means": "a closer environment"}, {"label": "drop", "means": "stop the task"}],
 "recommended": 0, "why": "the ticket names no account", "raised_by": "fx-judge"}]`

func TestABugThatDidNotReproduceMustAskWithOptions(t *testing.T) {
	notReproduced := reportWith("Proof", "## Proof\nReproduced: no. The spec and `rails runner` both give 25 points.\n")
	for _, tc := range []struct {
		name, questions string
	}{
		{"no question", ""},
		{"another question", `[{"text": "Which base?", "options": [{"label": "master"}, {"label": "release"}], "recommended": 0}]`},
		{"three of the four ways", strings.Replace(reproQuestion, `{"label": "drop", "means": "stop the task"}`, `{"label": "wait", "means": "later"}`, 1)},
		{"no recommendation", strings.Replace(reproQuestion, `"recommended": 0`, `"recommended": null`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newEndDir(t)
			d.write("report.md", notReproduced)
			d.write("acceptance.json", goodAcceptance)
			if tc.questions != "" {
				d.write("questions.json", tc.questions)
			}
			if ok, why := d.check("report"); ok || !strings.HasPrefix(why, "questions.json: ") || !strings.Contains(why, "reproduc") {
				t.Fatalf("CheckEnd(report) = %v, %q; want not done, a why asking for the reproduction question", ok, why)
			}
		})
	}
	d := newEndDir(t)
	d.write("report.md", notReproduced)
	d.write("acceptance.json", goodAcceptance)
	d.write("questions.json", reproQuestion)
	if ok, why := d.check("report"); !ok {
		t.Fatalf("with the reproduction question: CheckEnd(report) = %v, %q, want done", ok, why)
	}
	reproduced := newEndDir(t)
	reproduced.write("report.md", goodReport)
	reproduced.write("acceptance.json", goodAcceptance)
	if ok, why := reproduced.check("report"); !ok {
		t.Fatalf("a bug that reproduced asks nothing: CheckEnd(report) = %v, %q, want done", ok, why)
	}
}

func TestCheckEndRefusesAFileOlderThanTheStage(t *testing.T) {
	before := t0.Add(-time.Minute)
	d := newEndDir(t)
	d.writeAt("findings-fx-cause.md", "The cause is in app/models/campaign.rb:88.\n", before)
	if ok, why := d.check("findings", "fx-cause"); ok || !strings.HasPrefix(why, "findings-fx-cause.md: ") || !strings.Contains(why, "older") {
		t.Fatalf("CheckEnd(findings) = %v, %q; want not done, findings-fx-cause.md older than the stage", ok, why)
	}
	for _, old := range []string{"report.md", "acceptance.json", "questions.json"} {
		r := newEndDir(t)
		r.write("report.md", goodReport)
		r.write("acceptance.json", goodAcceptance)
		r.write("questions.json", "[]")
		r.writeAt(old, map[string]string{"report.md": goodReport, "acceptance.json": goodAcceptance, "questions.json": "[]"}[old], before)
		if ok, why := r.check("report"); ok || !strings.HasPrefix(why, old+": ") || !strings.Contains(why, "older") {
			t.Fatalf("an old %s: CheckEnd(report) = %v, %q; want not done, %s older than the stage", old, ok, why, old)
		}
	}
	d.write("findings-fx-cause.md", "The cause is in app/models/campaign.rb:88.\n")
	if ok, why := d.check("findings", "fx-cause"); !ok {
		t.Fatalf("a fresh file: CheckEnd(findings) = %v, %q, want done", ok, why)
	}
}

// The findings end fact holds when every role of the stage wrote its own
// file, with something in it.
func TestCheckEndNeedsEveryRolesFindings(t *testing.T) {
	d := newEndDir(t)
	d.write("findings-fx-repro.md", "bin/rspec spec/models/campaign_copy_spec.rb:12 fails: 0 points, want 25\n")
	d.write("findings-fx-history.md", "  \n")
	if ok, why := d.check("findings", "fx-repro", "fx-cause"); ok || !strings.HasPrefix(why, "findings-fx-cause.md: ") {
		t.Fatalf("CheckEnd(findings) = %v, %q; want not done for the missing findings-fx-cause.md", ok, why)
	}
	if ok, why := d.check("findings", "fx-history"); ok || !strings.HasPrefix(why, "findings-fx-history.md: ") || !strings.Contains(why, "empty") {
		t.Fatalf("CheckEnd(findings) = %v, %q; want not done for an empty findings-fx-history.md", ok, why)
	}
	if ok, why := d.check("findings"); ok {
		t.Fatalf("CheckEnd(findings) without roles = %v, %q; want not done", ok, why)
	}
	if ok, why := d.check("findings", "fx-repro"); !ok {
		t.Fatalf("CheckEnd(findings, fx-repro) = %v, %q, want done", ok, why)
	}
}

// The critic writes critic.md and its questions in a file of its own,
// which the gate takes with the judge's in one batch.
func TestTheCriticsEndFactIsItsFileAndItsQuestions(t *testing.T) {
	d := newEndDir(t)
	if ok, why := d.check("critic", "fx-critic"); ok || !strings.HasPrefix(why, "critic.md: ") {
		t.Fatalf("CheckEnd(critic) = %v, %q; want not done without critic.md", ok, why)
	}
	d.write("critic.md", "The plan does not say what happens to rules a copy shares.\n")
	d.write("questions-fx-critic.json", `[{"text": "Share or copy?"`)
	if ok, why := d.check("critic", "fx-critic"); ok || !strings.HasPrefix(why, "questions-fx-critic.json: ") {
		t.Fatalf("CheckEnd(critic) = %v, %q; want not done for its malformed questions", ok, why)
	}
	d.write("questions-fx-critic.json", `[{"text": "Share or copy?", "options": [{"label": "share"}, {"label": "copy"}], "recommended": 1}]`)
	if ok, why := d.check("critic", "fx-critic"); !ok {
		t.Fatalf("CheckEnd(critic) = %v, %q, want done", ok, why)
	}
}

// A plan is wide when it touches 3 or more files, the database or
// security: only then does the plan's critic run.
func TestAWidePlanTouchesThreeFilesTheDatabaseOrSecurity(t *testing.T) {
	for _, tc := range []struct {
		files []string
		wide  bool
	}{
		{[]string{"app/models/campaign.rb", "spec/models/campaign_spec.rb"}, false},
		{[]string{"app/models/campaign.rb", "spec/models/campaign_spec.rb", "app/views/campaigns/show.haml"}, true},
		{[]string{"db/migrate/20261104090000_add_origin_to_rules.rb"}, true},
		{[]string{"db/schema.rb"}, true},
		{[]string{"app/controllers/sessions_controller.rb"}, true},
		{[]string{"app/policies/campaign_policy.rb"}, true},
		{[]string{"lib/auth/token_check.rb"}, true},
		{[]string{"config/initializers/devise.rb"}, true},
		{[]string{"app/models/sessionless.rb", "app/models/campaign.rb"}, false},
		{[]string{"app/models/dbx.rb"}, false},
	} {
		if wide, why := wideChange(tc.files); wide != tc.wide || why == "" {
			t.Errorf("wideChange(%v) = %v, %q; want %v and a reason", tc.files, wide, why, tc.wide)
		}
	}
}
