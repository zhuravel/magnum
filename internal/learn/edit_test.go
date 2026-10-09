package learn

import (
	"os"
	"path/filepath"
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
// rest, wrapped at 104 runes with two-space continuation lines.
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
}

// The loop's entries go to a last section of their own, Learned rules,
// which the first one creates at the end of the log; the operators' own
// entries go to the sections before it, so an operator's entry and a
// candidate's never touch the same lines and a landing's rebase does not
// conflict over them.
func TestAppendLearnedKeepsTheLoopsEntriesInTheirOwnSection(t *testing.T) {
	first := DecisionEntry("The judge checks the lock.", "2026-10-09", []string{"m1"})
	second := RevertEntry("Reverts skill version 1: it regressed.", "2026-10-10")
	doc := AppendLearned([]byte("# Decisions\n\n## Operations\n\n- **Old.** It says why.\n"), first)
	want := "# Decisions\n\n## Operations\n\n- **Old.** It says why.\n\n" + LearnedHeading + "\n\n" + learnedIntro + first
	if string(doc) != want {
		t.Fatalf("first entry:\n%s\nwant:\n%s", doc, want)
	}
	for line := range strings.SplitSeq(learnedIntro, "\n") {
		if n := len([]rune(line)); n > DecisionWidth {
			t.Errorf("intro line of %d runes: %q", n, line)
		}
	}
	doc = AppendLearned(doc, second)
	if string(doc) != want+second {
		t.Fatalf("second entry:\n%s", doc)
	}

	// An entry goes to the end of the section, before a section someone
	// added after it, and a log without a final line break gets one.
	later := string(doc) + "\n## Later\n\n- **Someone's.** Why.\n"
	got := string(AppendLearned([]byte(later), first))
	if want := want + second + first + "\n## Later\n\n- **Someone's.** Why.\n"; got != want {
		t.Fatalf("before a later section:\n%s\nwant:\n%s", got, want)
	}
	if got := string(AppendLearned([]byte("# Decisions\n- old"), first)); got != "# Decisions\n- old\n\n"+LearnedHeading+"\n\n"+learnedIntro+first {
		t.Fatalf("no final line break:\n%s", got)
	}
	if strings.Count(got, LearnedHeading) != 1 {
		t.Fatalf("the heading twice:\n%s", got)
	}
}

// The design log carries its Learned rules section, with the introduction
// the loop would write, before the loop's first entry: the first entry
// goes into it after a blank line, as into a section it creates, and an
// operator's entry committed above the section meanwhile stays where it
// is.
func TestAppendLearnedFindsTheSectionBeforeItsFirstEntry(t *testing.T) {
	entry := DecisionEntry("The judge checks the lock.", "2026-10-09", []string{"m1"})
	bare := "# Decisions\n\n## Operations\n\n- **Old.** It says why.\n"
	want := string(AppendLearned([]byte(bare), entry))
	empty := bare + "\n" + LearnedHeading + "\n\n" + strings.TrimSuffix(learnedIntro, "\n")
	if got := string(AppendLearned([]byte(empty), entry)); got != want {
		t.Fatalf("the first entry into the empty section:\n%s\nwant:\n%s", got, want)
	}
	mine := "- **Mine.** It says why.\n"
	above := func(doc string) string { return strings.Replace(doc, "\n"+LearnedHeading, mine+"\n"+LearnedHeading, 1) }
	if got := string(AppendLearned([]byte(above(empty)), entry)); got != above(want) {
		t.Fatalf("with an operator's entry above the section:\n%s\nwant:\n%s", got, above(want))
	}
	second := RevertEntry("Reverts skill version 1: it regressed.", "2026-10-10")
	if got := string(AppendLearned(AppendLearned([]byte(empty), entry), second)); got != want+second {
		t.Fatalf("the second entry:\n%s", got)
	}
}

// The repository's design log ends with the Learned rules section, opened
// with the introduction the loop writes, so no operator's entry appended
// at the end of the file before the loop's first landing can conflict with
// the candidate that would have created it.
func TestTheDesignLogEndsWithTheLearnedRulesSection(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(DecisionsFile)))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	locs := learnedHeadingRe.FindAllStringIndex(doc, -1)
	if len(locs) != 1 {
		t.Fatalf("%s has %d %q headings, want 1", DecisionsFile, len(locs), LearnedHeading)
	}
	section := doc[locs[0][1]:]
	if strings.Contains(section, "\n## ") {
		t.Fatalf("%s has a section after %q", DecisionsFile, LearnedHeading)
	}
	if !strings.HasPrefix(section, "\n\n"+strings.TrimSuffix(learnedIntro, "\n")) {
		t.Fatalf("%q does not open with the loop's introduction:\n%s", LearnedHeading, textOf(section, 400))
	}
}

// textOf is the first n bytes of s.
func textOf(s string, n int) string { return s[:min(n, len(s))] }
