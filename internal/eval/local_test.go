package eval

import (
	"os"
	"path/filepath"
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
