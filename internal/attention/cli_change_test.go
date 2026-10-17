package attention

import (
	"strings"
	"testing"
	"time"
)

// On 2026-10-16 codex-cli 0.163.0 died at its TUI bootstrap in every judge
// pane, and each parked PR's line named only its own failure. The engine
// ends a launch failure's stored error with the note CLIChange writes when
// the role's CLI changed shortly before; the explanation keeps the note at
// the end of its summary and cause, whatever their length, and takes it
// from a launch failure only: a command's output that ends with the same
// words (a dependency step the PR runs) is no launch of an agent CLI.
func TestALaunchFailureNamesARecentCLIChange(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 40, 0, 0, time.Local)
	at := time.Date(2026, 10, 16, 11, 24, 0, 0, time.Local)
	note := CLIChange("codex", at, now)
	if note != "; the codex CLI changed at 11:24" {
		t.Fatalf("note %q", note)
	}
	if got := CLIChange("codex", at, at.Add(13*time.Hour)); got != "; the codex CLI changed at Oct 16 11:24" {
		t.Fatalf("a change of an earlier day: %q", got)
	}

	dead := "3 attempts on abc1234def: start codex-judge: agents: start mg-talkable-2-codex-judge in p-judge: herdr agent.start: agent_name_taken: agent name mg-talkable-2-codex-judge is already in use"
	r := Explain("failed", dead+note, "talkable#2")
	line := "a dead launch holds the codex-judge's pane p-judge (agent mg-talkable-2-codex-judge): closed on the next retry"
	if !strings.HasPrefix(r.Summary, "agents failed 3× on abc1234: a dead launch holds the codex-judge's pane") ||
		!strings.HasSuffix(r.Summary, "…; the codex CLI changed at 11:24") || len([]rune(r.Summary)) != SummaryMax {
		t.Errorf("summary %q", r.Summary)
	}
	if r.Cause != line+"; the codex CLI changed at 11:24" {
		t.Errorf("cause %q", r.Cause)
	}
	if plain := Explain("failed", dead, "talkable#2"); plain.Summary != "agents failed 3× on abc1234: "+line || plain.Fix != r.Fix {
		t.Errorf("without the note: %+v", plain)
	}

	long := "3 attempts on abc1234def: start codex-judge: agents: prompt mg-talkable-2-codex-judge: agent_not_found; the pane shows: Error: " +
		strings.Repeat("account/read failed during TUI bootstrap ", 8)
	r = Explain("failed", long+"; the codex CLI changed at Oct 16 11:24", "talkable#2")
	for _, s := range []string{r.Summary, r.Cause} {
		if !strings.HasSuffix(s, "; the codex CLI changed at Oct 16 11:24") || len([]rune(s)) > SummaryMax {
			t.Errorf("a long line keeps the note within %d runes: %q", SummaryMax, s)
		}
	}

	if role, ok := LaunchRole(strings.TrimPrefix(dead, "3 attempts on abc1234def: ")); role != "codex-judge" || !ok {
		t.Errorf("the role that did not start: %q %v", role, ok)
	}
	if _, ok := LaunchRole("deps talkable: bin/setup: start x: y"); ok {
		t.Error("a dependency step's error read as a start")
	}

	spoof := "3 attempts on abc1234def: deps talkable: bin/setup: mise -C /x exec -- bin/setup exited 1: installing\nError: no gem; the codex CLI changed at 11:24"
	if r := Explain("failed", spoof, "talkable#2"); !strings.HasSuffix(r.Cause, "Error: no gem; the codex CLI changed at 11:24") ||
		strings.Count(r.Summary, "CLI changed") != 1 {
		t.Errorf("a command's output is read as it is: %+v", r)
	}
}
