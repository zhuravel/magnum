package learn

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

const editSkill = "# Skill\n\n## 4. Findings\nProve five facts.\nReachability decides the priority.\n"

// goodEdit is a valid answer for goodCheck: two inputs, the skill edited.
func goodEdit() (EditCheck, Changes) {
	c := EditCheck{
		Inputs: EditInputs{
			Misses:   []EditMiss{{ID: "m12", Severity: "P1", Title: "Missed a race", Lesson: "When two writers share a row, check the lock."}},
			Declines: []EditDecline{{ID: "d3", Priority: "P2", Verdict: store.VerdictWrong, Title: "Nil receiver"}},
			SkillCap: 200, Files: EditAllowedGlobs,
		},
		Changed: []string{SkillFile},
		Skill:   []byte(editSkill + "When two writers share a row, check the lock.\n"),
		Pins:    []string{"Prove five facts.", "Reachability decides the priority."},
		Diff: "--- a/skills/magnum-review/SKILL.md\n+++ b/skills/magnum-review/SKILL.md\n@@ -5,0 +6 @@\n" +
			"+When two writers share a row, check the lock.\n",
		Leaks:  notes.Check{Branches: []string{"fix/coupon-expiry"}},
		People: []string{"alice", "rev-ann"}, Own: []string{"zhuravel"}, Repos: []string{"talkable", "talkable"},
	}
	ch := Changes{
		Files: []FileChange{{Name: SkillFile, Action: FileEdited, Reason: "a rule for shared rows"},
			{Name: "prompts/judge-initial.md", Action: FileUnchanged, Reason: "the skill covers it"}},
		Inputs: []InputChange{{ID: "m12", Action: InputNoted, Where: "SKILL.md section 4"},
			{ID: "d3", Action: InputSkipped, Reason: "the receiver rule already covers it"}},
		Decision: "The judge checks the lock of a row two writers share. A reviewer caught a lost update magnum missed.",
	}
	return c, ch
}

// hasProblem fails unless one of problems holds want.
func hasProblem(t *testing.T, problems []string, want string) {
	t.Helper()
	if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) }) {
		t.Errorf("problems %q lack %q", problems, want)
	}
}

func TestValidateEditAcceptsAGoodAnswer(t *testing.T) {
	c, ch := goodEdit()
	if problems := ValidateEdit(c, ch); len(problems) != 0 {
		t.Fatalf("a good answer: %q", problems)
	}
}

func TestValidateEditRequiresEveryInputOnce(t *testing.T) {
	c, ch := goodEdit()
	ch.Inputs = []InputChange{
		{ID: "m12", Action: InputNoted, Where: "SKILL.md section 4"},
		{ID: "m12", Action: InputSkipped, Reason: "again"},
		{ID: "m99", Action: InputNoted, Where: "x"},
	}
	problems := ValidateEdit(c, ch)
	hasProblem(t, problems, "input m12 is in changes.json's inputs twice")
	hasProblem(t, problems, `input "m99" in changes.json was not in inputs.json`)
	hasProblem(t, problems, "input d3 of inputs.json is not in changes.json's inputs")

	_, ch = goodEdit()
	ch.Inputs = []InputChange{{ID: "m12", Action: InputNoted}, {ID: "d3", Action: InputSkipped},
		{ID: "d3x", Action: "maybe"}}
	problems = ValidateEdit(c, ch)
	hasProblem(t, problems, "input m12 in changes.json is noted without where")
	hasProblem(t, problems, "input d3 in changes.json is skipped without a reason")

	_, ch = goodEdit()
	ch.Inputs[1].Reason = strings.Repeat("long ", 70)
	hasProblem(t, ValidateEdit(c, ch), "longer than 300 characters")
	ch.Inputs[1].Reason = "two\nlines"
	hasProblem(t, ValidateEdit(c, ch), "not one line")
	ch.Inputs[1].Action = "maybe"
	hasProblem(t, ValidateEdit(c, ch), `action "maybe" is not one of noted, skipped`)
}

func TestValidateEditRefusesAFileOutsideTheAllowedSet(t *testing.T) {
	for _, p := range []string{"internal/agents/review_format_test.go", "prompts/retro.md", "prompts/notes-curate.md",
		"config.defaults.toml", "prompts/judge-initial.md.tmp", "skills/magnum-review/notes/"} {
		if AllowedEditFile(p) {
			t.Errorf("%s is allowed", p)
		}
		c, ch := goodEdit()
		c.Changed = append(c.Changed, p)
		ch.Files = append(ch.Files, FileChange{Name: p, Action: FileEdited, Reason: "needed"})
		hasProblem(t, ValidateEdit(c, ch), p+" changed, but only "+strings.Join(EditAllowedGlobs, ", ")+" may")
	}
	for _, p := range []string{SkillFile, "prompts/judge-initial.md", "prompts/judge-own-pass.md", "prompts/claude-review.md"} {
		if !AllowedEditFile(p) {
			t.Errorf("%s is not allowed", p)
		}
	}
	// An allowed file that changed must be listed edited, and one listed
	// edited must have changed.
	c, ch := goodEdit()
	c.Changed = append(c.Changed, "prompts/judge-rereview.md")
	ch.Files[1].Action = FileEdited
	problems := ValidateEdit(c, ch)
	hasProblem(t, problems, "prompts/judge-rereview.md changed but is not in changes.json's files")
	hasProblem(t, problems, "file prompts/judge-initial.md is edited in changes.json but did not change")
	c.Changed = nil
	hasProblem(t, ValidateEdit(c, ch), "no allowed file changed")
}

func TestValidateEditRefusesASkillOverTheCap(t *testing.T) {
	c, ch := goodEdit()
	c.Inputs.SkillCap = len(c.Skill) - 1
	hasProblem(t, ValidateEdit(c, ch), "skills/magnum-review/SKILL.md is "+strconv.Itoa(len(c.Skill))+" bytes, over its cap of "+
		strconv.Itoa(len(c.Skill)-1))
	c.Inputs.SkillCap = len(c.Skill)
	if problems := ValidateEdit(c, ch); len(problems) != 0 {
		t.Fatalf("a skill at its cap: %q", problems)
	}
}

func TestValidateEditRefusesALostPin(t *testing.T) {
	c, ch := goodEdit()
	c.Skill = []byte(strings.Replace(string(c.Skill), "Reachability decides the priority.", "Reach decides.", 1))
	hasProblem(t, ValidateEdit(c, ch), `SKILL.md lost a pinned sentence: "Reachability decides the priority."`)
}

func TestValidateEditRefusesAPRNumberInTheDecision(t *testing.T) {
	for _, d := range []string{
		"The judge checks the lock, as #1234 showed.",
		"The judge checks the lock, as PR 1234 showed.",
		"See https://github.com/talkable/talkable/pull/12 for why.",
		"alice asked for a lock check.",
		"In talkable the lock is checked.",
		"",
		strings.Repeat("x", DecisionMaxRunes+1),
	} {
		c, ch := goodEdit()
		ch.Decision = d
		if problems := ValidateEdit(c, ch); len(problems) == 0 {
			t.Errorf("decision %.40q passed", d)
		}
	}
	c, ch := goodEdit()
	ch.Decision = "Fix the race on fix/coupon-expiry."
	hasProblem(t, ValidateEdit(c, ch), "the decision in changes.json names a pull request's branch")
	// The diff's added lines go through the same leak rules; a removed line
	// does not.
	_, ch = goodEdit()
	c.Diff += "+Run it from /Users/alice/src.\n-See PR 12.\n"
	problems := ValidateEdit(c, ch)
	hasProblem(t, problems, "the change names a home directory path")
	if slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "pull request or issue number") }) {
		t.Errorf("a removed line was checked: %q", problems)
	}
	// The added lines may not name an input's people or repository, nor
	// hold a link, either: the skill is public.
	for line, want := range map[string]string{
		"+Ask alice before a lock.\n":                "the change names a person's login",
		"+In talkable, check the lock.\n":            "the change names a repository or its owner",
		"+See https://example.com/locks for more.\n": "the change holds a link",
	} {
		c, ch := goodEdit()
		c.Diff += line
		hasProblem(t, ValidateEdit(c, ch), want)
	}
}

// A prompt's template actions are its {{...}} tokens, in order.
func TestTemplateActionsListsTheActionsInOrder(t *testing.T) {
	got := TemplateActions("Review {{.URL}} at `{{.HeadSHA}}`.\n{{- if .ForcePushed}} History was rewritten.{{end}}\n{{range .Reports}}\n- {{.Label}}\n{{end}}")
	want := []string{"{{.URL}}", "{{.HeadSHA}}", "{{- if .ForcePushed}}", "{{end}}", "{{range .Reports}}", "{{.Label}}", "{{end}}"}
	if !slices.Equal(got, want) {
		t.Fatalf("TemplateActions = %q", got)
	}
}

func TestPinsRoundTrip(t *testing.T) {
	pins := []string{"Reachability decides the priority.", "Prove five facts."}
	b := FormatPins(pins, 30182)
	got, limit, err := ParsePins(b)
	if err != nil || limit != 30182 || !slices.Equal(got, []string{"Prove five facts.", "Reachability decides the priority."}) {
		t.Fatalf("ParsePins = %q, %d, %v", got, limit, err)
	}
	if _, _, err := ParsePins([]byte("Prove five facts.\n")); err == nil {
		t.Fatal("a pins file without a cap parsed")
	}
}

func TestParseInputID(t *testing.T) {
	for _, tc := range []struct {
		in   string
		kind string
		id   int64
		ok   bool
	}{{"m12", store.LearnInputMiss, 12, true}, {"d3", store.LearnInputDecline, 3, true}, {"m0", "", 0, false},
		{"m012", "", 0, false}, {"x1", "", 0, false}, {"m", "", 0, false}, {"d-1", "", 0, false}} {
		kind, id, ok := ParseInputID(tc.in)
		if kind != tc.kind || id != tc.id || ok != tc.ok {
			t.Errorf("ParseInputID(%q) = %q, %d, %v", tc.in, kind, id, ok)
		}
	}
	if MissInputID(12) != "m12" || DeclineInputID(3) != "d3" {
		t.Error("input ids")
	}
}

// The editor's decision becomes a design-log entry like every other: its
// first sentence in bold as the headline, the day and the inputs, then the
// rest, wrapped at 104 runes with two-space continuation lines, appended
// after the log's last line.
func TestDecisionEntryWrapsTheHeadlineAndTheRest(t *testing.T) {
	if h, r := Headline("The judge checks e.g. the lock.  It matters."); h != "The judge checks e.g." || r != "the lock. It matters." {
		t.Errorf("Headline = %q, %q (a stop followed by a space ends it)", h, r)
	}
	if h, r := Headline("One sentence without a stop"); h != "One sentence without a stop" || r != "" {
		t.Errorf("Headline = %q, %q", h, r)
	}
	long := "The judge checks the lock of a row two writers share. " + strings.Repeat("Because a lost update corrupts totals. ", 6)
	entry := DecisionEntry(long, "2026-10-09", []string{"m12", "d3"})
	lines := strings.Split(strings.TrimSuffix(entry, "\n"), "\n")
	if !strings.HasPrefix(lines[0], "- **The judge checks the lock of a row two writers share** (2026-10-09, the skill editor's") || len(lines) < 3 {
		t.Fatalf("entry = %q", entry)
	}
	for _, l := range lines {
		if n := len([]rune(l)); n > DecisionWidth || (l != lines[0] && !strings.HasPrefix(l, "  ")) {
			t.Errorf("line of %d runes: %q", n, l)
		}
	}
	if got := strings.Join(strings.Fields(entry), " "); !strings.Contains(got, "proposal for m12, d3). Because a lost update") {
		t.Errorf("entry text = %q", got)
	}
	doc := AppendDecision([]byte("# Decisions\n- old"), entry)
	if string(doc) != "# Decisions\n- old\n"+entry {
		t.Errorf("appended = %q", doc)
	}
}
