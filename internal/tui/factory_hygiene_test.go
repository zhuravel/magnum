package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The help glosses only what the screen draws: no staging cells, QA,
// CI-fix tries, snooze, "say one line", learning view or rating, which no
// data fills and no key offers yet; the stages it glosses are the built-in
// kinds' (judge, reproduction, publish), not the spec's later ones (tests,
// merge master, cleanup). Before, it listed all of spec 9.E's words.
func TestTheHelpListsOnlyWhatExists(t *testing.T) {
	h := testFactory(t, 140, 50)
	help := ansi.Strip(strings.Join(h.m.helpContent(140), "\n"))
	words := map[string]bool{}
	for _, l := range strings.Split(help, "\n") {
		if w, _, ok := strings.Cut(strings.TrimSpace(l), "  "); ok {
			words[w] = true
		}
	}
	for _, gone := range []string{"void, bart, bastion", "outdated", "latest", "merging", "deploying", "CI 12/41", "QA 4/9",
		"QA ✓ / QA ✗", "bart: no QA", "bastion waits for void QA", "code failed", "runner died", "try 1 of 2", "out of fix tries",
		"ready to merge", "access needed", "snooze", "say one line", "you're in it", "no output 9m", "your PR, written by hand",
		"lesson", "rule", "skill edit", "test case", "replay", "used in", "repeats seen after the rule", "rule to approve",
		"rate + / −", "codex limit: waits till 16:00", "runbook", "start", "tests", "merge master", "merge", "cleanup", "pushed"} {
		if words[gone] {
			t.Errorf("the help glosses %q, which the screen never draws", gone)
		}
	}
	for _, kept := range []string{"investigate", "judge", "critic", "questions", "reproduction", "coding", "local review",
		"pr body", "ready", "publish", "step in", "memory full: new tasks wait", "you pushed 11:02", "push + PR"} {
		if !words[kept] {
			t.Errorf("the help lacks %q", kept)
		}
	}
}
