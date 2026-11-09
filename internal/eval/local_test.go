package eval

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// localCorpus is a corpus with one local case: a round of the factory's local review, named by its
// synthetic PR number (900000 + task × 10 + round), with a defect it must raise as a question.
const localCorpus = `
[[case]]
name = "task-1-round-1"
kind = "local"
pr = "example/example#900011"
head = "` + sha + `"
why = "the local round took the plan's choice for intent"

[[case.defect]]
id = "plan-choice"
title = "The plan's choice of what a user sees is the operator's to make"
severity = "P2"
question = true
match = ["deliberate", "intended"]
`

// A local case is read with its kind, its synthetic PR number split like a posted case's, and its
// question defect; a case without a kind is a posted one, as every case was before.
func TestACorpusReadsALocalCaseBesideAPostedOne(t *testing.T) {
	c, err := parseCorpus("", []byte(validCorpus+localCorpus))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cases) != 3 {
		t.Fatalf("cases = %d, want 3", len(c.Cases))
	}
	for _, cs := range c.Cases[:2] {
		if cs.Kind != KindPosted || cs.Local() {
			t.Errorf("case %s: kind %q, want the posted kind", cs.Name, cs.Kind)
		}
	}
	lc := c.Cases[2]
	if lc.Kind != KindLocal || !lc.Local() || lc.Owner != "example" || lc.Repo != "example" || lc.Number != 900011 || lc.Head != sha {
		t.Fatalf("local case %+v", lc)
	}
	if d := lc.Defects[0]; !d.Question || d.Severity != "P2" {
		t.Fatalf("question defect %+v", d)
	}
}

// A local case names its round by the synthetic number, takes its base from its frozen inputs and is
// the only kind whose defect may ask for a question: a posted round asks none, so such a defect could
// never be found. An absent question and a question in the review's body are refused too, and so is an
// unknown kind.
func TestALocalCaseAndAQuestionDefectBreakTheirOwnRules(t *testing.T) {
	tests := []struct {
		name, src string
		want      []string
	}{
		{"unknown kind", strings.Replace(localCorpus, `kind = "local"`, `kind = "posted"`, 1), []string{`case "task-1-round-1"`, `kind must be "local"`}},
		{"a real PR number", strings.Replace(localCorpus, "#900011", "#12212", 1), []string{"synthetic number", "eval add"}},
		{"a base of its own", strings.Replace(localCorpus, `why =`, "base = \"master\"\nwhy =", 1), []string{"base", "frozen"}},
		{"a question on a posted case", strings.Replace(localCorpus, `kind = "local"`+"\n", "", 1), []string{`defect "plan-choice"`, "question = true needs a local case"}},
		{"an absent question", strings.Replace(localCorpus, "question = true", "question = true\nexpect = \"absent\"", 1), []string{"question = true", "absent"}},
		{"a question in the body", strings.Replace(localCorpus, "question = true", "question = true\nbody = true", 1), []string{"question = true", "body = true"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCorpus("", []byte(tt.src))
			if err == nil {
				t.Fatal("invalid corpus was accepted")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

// A corpus written before cases had a kind loads as it did, every case posted, and a written corpus
// leaves the posted kind out: the learned corpus the daemon rewrites stays the same text.
func TestACorpusWithoutKindsLoadsAndWritesAsBefore(t *testing.T) {
	c, err := parseCorpus("", []byte(validCorpus))
	if err != nil {
		t.Fatal(err)
	}
	for _, cs := range c.Cases {
		if cs.Kind != "" || cs.Local() {
			t.Errorf("case %s has kind %q", cs.Name, cs.Kind)
		}
		for _, d := range cs.Defects {
			if d.Question {
				t.Errorf("defect %s/%s asks for a question", cs.Name, d.ID)
			}
		}
	}
	path := filepath.Join(t.TempDir(), "corpus.toml")
	if err := WriteCorpus(path, *c); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "kind") || strings.Contains(string(b), "question") {
		t.Fatalf("a corpus of posted cases spells out the new fields:\n%s", b)
	}
	if _, err := LoadCorpus(path); err != nil {
		t.Fatal(err)
	}
}

// questionCase is the local case of localCorpus, parsed.
func questionCase(t *testing.T) Case {
	t.Helper()
	c, err := parseCorpus("", []byte(localCorpus))
	if err != nil {
		t.Fatal(err)
	}
	return c.Cases[0]
}

// A question defect is found only by a question for the operator at its severity or stronger: a P3
// question goes to the task's leftovers and a finding that is no question to the writer, so neither
// satisfies a P2 question, and neither counts as noise, since each is on the defect.
func TestAQuestionDefectIsFoundOnlyByAQuestionAtItsSeverity(t *testing.T) {
	cs := questionCase(t)
	tests := []struct {
		name  string
		f     Finding
		found bool
	}{
		{"a P2 question", Finding{Body: "Is the deliberate rounding the operator's call?", Severity: "P2", Question: true}, true},
		{"a P1 question", Finding{Body: "The plan calls it intended", Severity: "P1", Question: true}, true},
		{"a P3 question", Finding{Body: "Is the deliberate rounding wanted?", Severity: "P3", Question: true}, false},
		{"a question without a severity", Finding{Body: "deliberate?", Question: true}, false},
		{"a P1 that is no question", Finding{Body: "The deliberate rounding is wrong", Severity: "P1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := ScoreCase(cs, Result{Findings: []Finding{tt.f}})
			d := s.Defects[0]
			if d.Found != tt.found || d.SeverityOK != tt.found || s.Found != map[bool]int{true: 1}[tt.found] || s.Total != 1 {
				t.Fatalf("score %+v, want found %v", s, tt.found)
			}
			if !d.Question || d.Want != "P2" {
				t.Errorf("defect result %+v, want it to say it wants a P2 question", d)
			}
			if s.Noise != 0 {
				t.Errorf("noise %d: a finding on the defect is never noise", s.Noise)
			}
		})
	}
	s := ScoreCase(cs, Result{Findings: []Finding{{Path: "app/a.rb", Line: 3, Body: "unrelated", Severity: "P2", Question: true}}})
	if s.Found != 0 || s.Noise != 1 {
		t.Fatalf("a question on something else: %+v, want it missed and the question noise", s)
	}
}

// A question defect without a severity is found by any question; the defect without question = true
// takes a question like any other finding.
func TestAQuestionDefectWithoutASeverityTakesAnyQuestion(t *testing.T) {
	cs := questionCase(t)
	cs.Defects[0].Severity = ""
	if s := ScoreCase(cs, Result{Findings: []Finding{{Body: "deliberate?", Severity: "P3", Question: true}}}); s.Found != 1 {
		t.Fatalf("score %+v, want the P3 question found", s)
	}
	cs.Defects[0].Question, cs.Defects[0].Severity = false, "P2"
	s := ScoreCase(cs, Result{Findings: []Finding{{Body: "deliberate?", Severity: "P1", Question: true}}})
	if s.Found != 1 || s.Defects[0].Question {
		t.Fatalf("score %+v, want a plain defect found by a question", s)
	}
}

// The markdown report says a question defect wants a question.
func TestTheReportSaysAQuestionDefectWantsAQuestion(t *testing.T) {
	cs := questionCase(t)
	run := Run{ID: "20261109-120000", Cases: []CaseRun{{Case: cs.Name, Kind: KindLocal, PR: cs.PR, Head: cs.Head, Outcome: "dry_run"}}}
	missed := ScoreCase(cs, Result{})
	run.Cases[0].Score = &missed
	var b strings.Builder
	WriteMarkdown(&b, run)
	if !strings.Contains(b.String(), "missed `plan-choice` The plan's choice of what a user sees is the operator's to make (wanted a P2 question)") {
		t.Fatalf("report:\n%s", b.String())
	}
	found := ScoreCase(cs, Result{Findings: []Finding{{Body: "intended?", Severity: "P1", Question: true}}})
	run.Cases[0].Score = &found
	b.Reset()
	WriteMarkdown(&b, run)
	if !strings.Contains(b.String(), "found `plan-choice` The plan's choice of what a user sees is the operator's to make (a P1 question, wanted a P2 question)") {
		t.Fatalf("report:\n%s", b.String())
	}
}

// A local case's run keeps the findings it was scored on (the round's, as the factory acts on them),
// so `eval score` re-scores it against the corpus's current defects from those, without the result
// file, which holds no question marks a posted parse would read. A local run whose case the corpus now
// has as a posted one keeps its score and says why.
func TestRescoreScoresALocalCaseFromItsRecordedFindings(t *testing.T) {
	cs := questionCase(t)
	findings := []Finding{{Body: "Is the rounding the operator's to decide?", Severity: "P2", Question: true}}
	old := ScoreCase(cs, Result{Findings: findings})
	if old.Found != 0 {
		t.Fatalf("fixture: the defect's words should miss the finding: %+v", old)
	}
	run := Run{ID: "r1", Cases: []CaseRun{{Case: cs.Name, Kind: KindLocal, PR: cs.PR, Head: cs.Head, Outcome: "dry_run",
		ResultFile: "/abs/codex-judge.json", Score: &old, Findings: findings}}}
	fixed := cs
	fixed.Defects = []Defect{cs.Defects[0]}
	fixed.Defects[0].Match = []string{"rounding"}
	read := func(string) ([]byte, error) { t.Fatal("a local case's result file was read"); return nil, nil }
	got := Rescore(run, &Corpus{Cases: []Case{fixed}}, read)
	if s := got.Cases[0].Score; s == nil || s.Found != 1 || !s.Defects[0].Question {
		t.Fatalf("rescored %+v, want the question found", got.Cases[0].Score)
	}
	posted := fixed
	posted.Kind, posted.Defects[0].Question = KindPosted, false
	got = Rescore(run, &Corpus{Cases: []Case{posted}}, read)
	if got.Cases[0].Score != &old || !strings.Contains(got.Cases[0].Error, "another kind") {
		t.Fatalf("case %+v, want the old score kept and why", got.Cases[0])
	}
}

// The example corpus shows a local case: a round of the factory's local review named by its
// synthetic PR, whose defect a P2 question for the operator finds and a P3 one does not.
func TestTheExampleCorpusHasALocalCaseThatWantsAQuestion(t *testing.T) {
	c, err := LoadCorpus(filepath.Join("..", "..", "eval.toml.example"))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(c.Cases, func(k Case) bool { return k.Local() })
	if i < 0 {
		t.Fatal("the example corpus has no local case")
	}
	k := c.Cases[i]
	if k.Owner != "example" || k.Number < LocalNumberBase || len(k.Defects) != 1 || !k.Defects[0].Question || k.Defects[0].Severity != "P2" {
		t.Fatalf("case %+v: want one P2 question defect on a synthetic PR of an example repository", k)
	}
	for _, tc := range []struct {
		severity string
		found    bool
	}{{"P3", false}, {"P2", true}} {
		f := Finding{Path: "app/models/credit.rb", Line: 7, Severity: tc.severity, Question: true,
			Body: "Should a refund take back the points it gave? The plan calls keeping them deliberate."}
		if s := ScoreCase(k, Result{Findings: []Finding{f}}); s.Found != map[bool]int{true: 1}[tc.found] || s.Noise != 0 {
			t.Errorf("a %s question: %+v, want found %v and no noise", tc.severity, s, tc.found)
		}
	}
}
