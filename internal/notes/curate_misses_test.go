package notes

import (
	"slices"
	"strings"
	"testing"
)

// missChanges is goodChanges accounting for misses 41 (noted in Tests) and
// 42 (skipped).
func missChanges() Changes {
	ch := goodChanges()
	ch.Misses = []MissChange{
		{ID: 41, Action: MissNoted, Section: "Tests"},
		{ID: 42, Action: MissSkipped, Reason: "a naming preference, no defect a review could catch"},
	}
	return ch
}

// A proposal given misses accounts for every one of them, each once: noted
// with the section of the proposal that covers it, or skipped with a
// one-line reason; anything else is a problem the nudge names.
func TestValidateAccountsForEveryMissGiven(t *testing.T) {
	check := Check{Base: baseState(), PRNumbers: []int{11920}, Misses: []int64{41, 42}}
	if problems := Validate(proposal(t, baseState(), goodNotes, goodFiles(), missChanges()), check); len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	cases := []struct {
		name  string
		edit  func(ch *Changes)
		wants string
	}{
		{"a miss skipped without accounting", func(ch *Changes) { ch.Misses = ch.Misses[:1] },
			"miss 42 of misses.json is not in changes.json's misses (noted with its section, or skipped with a reason)"},
		{"no misses at all", func(ch *Changes) { ch.Misses = nil }, "miss 41 of misses.json is not in changes.json's misses"},
		{"a miss it was not given", func(ch *Changes) {
			ch.Misses = append(ch.Misses, MissChange{ID: 43, Action: MissSkipped, Reason: "x"})
		}, "miss 43 in changes.json was not in misses.json"},
		{"a miss twice", func(ch *Changes) { ch.Misses = append(ch.Misses, ch.Misses[0]) }, "miss 41 is in changes.json's misses twice"},
		{"noted without a section", func(ch *Changes) { ch.Misses[0].Section = "" }, "miss 41 in changes.json is noted without the section that covers it"},
		{"noted in a section the proposal lacks", func(ch *Changes) { ch.Misses[0].Section = "Pitfalls" },
			`miss 41 in changes.json is noted in "Pitfalls", which is not a section of proposal.md`},
		{"skipped without a reason", func(ch *Changes) { ch.Misses[1].Reason = " " }, "miss 42 in changes.json is skipped without a reason"},
		{"another action", func(ch *Changes) { ch.Misses[1].Action = "dropped" }, `miss 42 in changes.json: action "dropped" is not one of noted, skipped`},
		{"a reason on two lines", func(ch *Changes) { ch.Misses[1].Reason = "one\ntwo" }, "miss 42 in changes.json: the reason is not one line"},
		{"a pull request in a reason", func(ch *Changes) { ch.Misses[1].Reason = "fixed in PR 4521" }, "a reason in changes.json names a pull request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := missChanges()
			tc.edit(&ch)
			problems := Validate(proposal(t, baseState(), goodNotes, goodFiles(), ch), check)
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.wants) }) {
				t.Errorf("problems %q lack %q", problems, tc.wants)
			}
		})
	}
	// Without misses given, changes.json may leave the key out.
	if problems := Validate(proposal(t, baseState(), goodNotes, goodFiles(), goodChanges()), Check{Base: baseState()}); len(problems) != 0 {
		t.Fatalf("no misses given: %v", problems)
	}
}

// A curation whose every miss is skipped may leave the notes as they are:
// the proposal still waits for the operator, who confirms the skips. A
// proposal that notes a miss must change something.
func TestValidateTakesAnUnchangedProposalThatSkipsEveryMiss(t *testing.T) {
	base := State{Exists: true, Notes: []byte("# Notes for talkable/talkable\n\n## Tests\nRun `run_spec.sh <spec>`.\n"),
		Files: []Blob{{Path: "run_spec.sh", Body: []byte("bin/rspec \"$@\"\n")}}}
	base.Files[0].SHA256 = TextSHA(base.Files[0].Body)
	ch := Changes{
		Sections: []Change{{Name: "Tests", Action: ActionKept, Reason: "every review runs specs"}},
		Files:    []Change{{Name: "run_spec.sh", Action: ActionKept, Reason: "runs any spec"}},
		Misses:   []MissChange{{ID: 41, Action: MissSkipped, Reason: "the Tests section already says to run the specs"}},
	}
	files := map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n"}
	check := Check{Base: base, Misses: []int64{41}}
	if problems := Validate(proposal(t, base, string(base.Notes), files, ch), check); len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	ch.Misses[0] = MissChange{ID: 41, Action: MissNoted, Section: "Tests"}
	problems := Validate(proposal(t, base, string(base.Notes), files, ch), check)
	if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "the proposal changes nothing") }) {
		t.Errorf("a noted miss without a change: %q", problems)
	}
}
