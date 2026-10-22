package notes

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// PrepareCritic copies what the critic reads into its run directory: the
// notes now and the proposal, each with its harness (the archive among it),
// the curator's changes, the review data and the rejections.
func TestPrepareCriticCopiesWhatTheCriticReads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".critic", "talkable", "talkable", "20261022-120000")
	live := State{Exists: true, Notes: []byte("# Notes\n- run bin/rspec\n"), Files: []Blob{{Path: "qa.sh", Body: []byte("open admin\n")}}}
	proposed := State{Exists: true, Notes: []byte("# Notes\n"), Files: []Blob{{Path: "qa.sh", Body: []byte("open admin\n")}}}.
		WithArchive([]byte("# Archive\n- run bin/rspec\n"))
	run, err := PrepareCritic(dir, live, proposed, nil, []byte(`{"repo":"talkable/talkable"}`), []byte("[]"))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		run.Current(): "# Notes\n- run bin/rspec\n", filepath.Join(run.CurrentHarness(), "qa.sh"): "open admin\n",
		run.Proposed(): "# Notes\n", filepath.Join(run.ProposedHarness(), ArchiveName): "# Archive\n- run bin/rspec\n",
		run.Changes(): "{}", run.Review(): `{"repo":"talkable/talkable"}`, run.Rejections(): "[]",
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	if _, err := os.Stat(run.Decision()); !os.IsNotExist(err) {
		t.Errorf("decision.json exists before the critic wrote it: %v", err)
	}
	if _, err := PrepareCritic(dir, live, proposed, nil, nil, nil); err == nil {
		t.Error("a second critique reused the run directory")
	}
}

// ReadDecision reads one JSON value with known fields only and words its
// problems for a nudge; a file the critic did not write is missing.
func TestReadDecisionRefusesUnknownFieldsAndTrailingValues(t *testing.T) {
	run := CriticRun{Dir: t.TempDir()}
	if _, missing, problems := ReadDecision(run); !missing || len(problems) != 1 || !strings.Contains(problems[0], "was not written") {
		t.Fatalf("no file: missing %v, %v", missing, problems)
	}
	for body, want := range map[string]string{
		`{"decision":"apply","target":"fp","verdict":"yes"}`: "unknown field",
		`{"decision":"apply","target":"fp"} {}`:              "more than one JSON value",
		`not json`:                                           "is not valid",
	} {
		if err := os.WriteFile(run.Decision(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, problems := ReadDecision(run); len(problems) != 1 || !strings.Contains(problems[0], want) {
			t.Errorf("%s: problems %v, want %q", body, problems, want)
		}
	}
	if err := os.WriteFile(run.Decision(), []byte(`{"decision":"reject","target":"fp","reason":"keep the QA steps","losses":[],"checked":["the diff"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d, missing, problems := ReadDecision(run)
	if missing || len(problems) > 0 || d.Decision != DecisionReject || d.Target != "fp" || !slices.Equal(d.Checked, []string{"the diff"}) {
		t.Fatalf("decision = %+v, missing %v, %v", d, missing, problems)
	}
}

// ValidateDecision: apply is valid only for a proposal that can be applied
// (no conflicting merge, nothing lost) and recurate only for one that
// cannot; every decision names the target the prompt gave; reject and
// recurate need a one-line reason within the limit that leaks nothing; the
// lists hold one-line items with no secret or home path.
func TestValidateDecisionBindsTheTargetAndTheDecisionToTheProposal(t *testing.T) {
	leaks := func(text string) []string {
		if strings.Contains(text, "#12") {
			return []string{"names a pull request"}
		}
		return nil
	}
	ok := DecisionCheck{Target: "fp", Leaks: leaks}
	cases := []struct {
		name string
		d    Decision
		c    DecisionCheck
		want string // "" = valid
	}{
		{"apply", Decision{Decision: DecisionApply, Target: "fp"}, ok, ""},
		{"apply of another target", Decision{Decision: DecisionApply, Target: "other"}, ok, `"target" must be`},
		{"reject without a target", Decision{Decision: DecisionReject, Reason: "keep it"}, ok, `"target" must be`},
		{"apply of a conflicting merge", Decision{Decision: DecisionApply, Target: "fp"}, DecisionCheck{Target: "fp", Conflict: true}, "merge conflicts"},
		{"apply of a lossy proposal", Decision{Decision: DecisionApply, Target: "fp"}, DecisionCheck{Target: "fp", Lossy: true}, "would lose lines"},
		{"recurate of an applicable one", Decision{Decision: DecisionRecurate, Target: "fp", Reason: "start over"}, ok, "recurate is valid only"},
		{"recurate of a lossy one", Decision{Decision: DecisionRecurate, Target: "fp", Reason: "keep the answers"}, DecisionCheck{Target: "fp", Lossy: true}, ""},
		{"reject without a reason", Decision{Decision: DecisionReject, Target: "fp"}, ok, `needs a "reason"`},
		{"two lines", Decision{Decision: DecisionReject, Target: "fp", Reason: "a\nb"}, ok, "one line"},
		{"too long", Decision{Decision: DecisionReject, Target: "fp", Reason: strings.Repeat("x", MaxReasonRunes+1)}, ok, "over 300"},
		{"a leak", Decision{Decision: DecisionReject, Target: "fp", Reason: "as in #12"}, ok, `"reason" names a pull request`},
		{"an unknown word", Decision{Decision: "merge", Target: "fp"}, ok, "must be apply, reject or recurate"},
		{"a secret in losses", Decision{Decision: DecisionApply, Target: "fp", Losses: []string{"token ghp_" + strings.Repeat("a", 36)}}, ok, "secret"},
		{"a home path in checked", Decision{Decision: DecisionApply, Target: "fp", Checked: []string{"/Users/alice/notes.md"}}, ok, "home directory"},
		{"too many items", Decision{Decision: DecisionApply, Target: "fp", Checked: make([]string, MaxDecisionItems+1)}, ok, "more than"},
	}
	for _, tc := range cases {
		got := ValidateDecision(tc.d, tc.c)
		switch {
		case tc.want == "" && len(got) > 0:
			t.Errorf("%s: problems %v, want none", tc.name, got)
		case tc.want != "" && !slices.ContainsFunc(got, func(p string) bool { return strings.Contains(p, tc.want) }):
			t.Errorf("%s: problems %v, want one with %q", tc.name, got, tc.want)
		}
	}
}

// ReasonLeaks refuses what the curation's reasons may not hold, and a probe
// file name.
func TestReasonLeaksNamesAProbeFile(t *testing.T) {
	if got := ReasonLeaks("keep campaign_snapshot_probes_spec.rb out", Check{}); !slices.Contains(got, "names a probe file") {
		t.Errorf("probe file: %v", got)
	}
	if got := ReasonLeaks("keep the QA steps for the admin", Check{}); len(got) > 0 {
		t.Errorf("a clean reason: %v", got)
	}
}
