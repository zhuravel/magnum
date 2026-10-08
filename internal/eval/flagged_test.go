package eval

import (
	"strings"
	"testing"
	"time"
)

// A refused case is recorded once, with its run, head and reason, and a
// later record of it does not replace the first; no file is no case.
func TestFlaggedCasesAreKeptUnderTheRunsRoot(t *testing.T) {
	root := t.TempDir()
	got, err := LoadFlagged(root)
	if err != nil || len(got) != 0 {
		t.Fatalf("LoadFlagged without a file = %v, %v", got, err)
	}
	at := time.Date(2026, 10, 7, 13, 40, 0, 0, time.UTC)
	first := Flagged{Case: "oauth-session", PR: "talkable/talkable#11990", Head: "abc1234", Run: "20261007-134000",
		Reason: "Codex refused the review: content flagged as a cybersecurity risk (codex-judge, run r-1)", At: at}
	if err := MarkFlagged(root, first); err != nil {
		t.Fatal(err)
	}
	if err := MarkFlagged(root, Flagged{Case: "oauth-session", Run: "later"}); err != nil {
		t.Fatal(err)
	}
	got, err = LoadFlagged(root)
	if err != nil || len(got) != 1 || got["oauth-session"] != first {
		t.Fatalf("LoadFlagged = %+v, %v", got, err)
	}
	r := got["oauth-session"].Refusal()
	for _, want := range []string{"flagged in run 20261007-134000", "cybersecurity risk", "never replayed", FlaggedFile} {
		if !strings.Contains(r, want) {
			t.Errorf("Refusal() = %q, want %q", r, want)
		}
	}
}

// A flagged case stays flagged under another name: FlaggedCase matches by
// the case's name and by its PR (owner/repo#N, any letter case), never
// another PR.
func TestAFlaggedCaseMatchesByNameOrPR(t *testing.T) {
	flagged := map[string]Flagged{"oauth-session": {Case: "oauth-session", PR: "talkable/talkable#11990", Run: "r1"}}
	for _, tc := range []struct {
		c    Case
		want bool
	}{
		{Case{Name: "oauth-session", PR: "talkable/talkable#1"}, true},
		{Case{Name: "another-name", PR: "talkable/talkable#11990"}, true},
		{Case{Name: "another-name", PR: "Talkable/Talkable#11990"}, true},
		{Case{Name: "another-name", PR: "talkable/talkable#11999"}, false},
		{Case{Name: "another-name", PR: "example/talkable#11990"}, false},
	} {
		if f, ok := FlaggedCase(flagged, tc.c); ok != tc.want || (ok && f.Run != "r1") {
			t.Errorf("FlaggedCase(%s %s) = %+v, %v; want %v", tc.c.Name, tc.c.PR, f, ok, tc.want)
		}
	}
}
