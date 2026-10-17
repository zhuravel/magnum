package attention

import (
	"os"
	"strings"
	"testing"
)

// The stored error of a PR whose dependency step failed three times ends a
// 200-line Ruby build log whose first line is "installing 1 tool": the
// explanation names the stage, the step, the tool the PR pins, the line that
// says why (configure: error: …) and the package to install.
func TestExplainsADependencyFailureFromItsBuildLog(t *testing.T) {
	msg, err := os.ReadFile("testdata/ruby_build.txt")
	if err != nil {
		t.Fatal(err)
	}
	r := Explain("", string(msg), "talkable#9992")
	if r.Kind != KindFailed || r.Stage != StageDependencies || r.Attempts != 3 || r.Head != "f34abc5" {
		t.Fatalf("reason %+v", r)
	}
	if r.Step != "bundle check >/dev/null || bundle install --jobs 4" {
		t.Errorf("step %q", r.Step)
	}
	if !strings.Contains(r.Cause, "mise could not install ruby@3.4.11") || !strings.Contains(r.Cause, "jemalloc requested but not found") {
		t.Errorf("cause %q", r.Cause)
	}
	if r.Summary != "dependencies (bundle install) failed 3× on f34abc5: mise could not install ruby@3.4.11, which the PR pins: configure: error: jemalloc requested but not found" {
		t.Errorf("summary %q", r.Summary)
	}
	if !strings.Contains(r.Fix, "brew install jemalloc") || !strings.Contains(r.Fix, "magnum review talkable#9992") || !strings.Contains(r.Fix, "magnum ignore talkable#9992") {
		t.Errorf("fix %q", r.Fix)
	}
	if len(r.Summary) > 4*SummaryMax || strings.ContainsAny(r.Summary, "\n\r") {
		t.Errorf("summary is not one short line: %q", r.Summary)
	}
	if len(r.Tail) == 0 || len(r.Tail) > TailLines {
		t.Errorf("tail %q", r.Tail)
	}
	for _, l := range r.Tail {
		if strings.Contains(l, "--verbose") || strings.Contains(l, "Version:") || strings.Contains(l, "external command failed") ||
			strings.Contains(l, "███") || strings.HasPrefix(l, "mise ruby@") {
			t.Errorf("noise in the tail: %q", l)
		}
	}
}

func TestExplainsTheOtherShapes(t *testing.T) {
	for _, tc := range []struct {
		name, kind, msg            string
		stage, cause, summary, fix string
	}{
		{"ssh key", "failed",
			"3 attempts on ee75a0d: checkout in review5: gitx: fetch PR 11985: git -C /Users/me/Projects/talkable fetch --no-tags origin +refs/pull/11985/head:refs/magnum/pr/11985 exited 128: git@github.com: Permission denied (publickey).\r\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.",
			StageFetch, "GitHub refused the SSH key", "fetch failed 3× on ee75a0d: GitHub refused the SSH key", "HTTPS through gh"},
		{"per-PR worktree", "",
			"3 attempts on 5427900: per-PR worktree: gitx: worktree add: git -C /x worktree add --detach /y abc exited 128: fatal: '/y' already exists",
			StageWorktree, "fatal: '/y' already exists", "worktree failed 3× on 5427900: fatal: '/y' already exists", "magnum review talkable#1"},
		{"judge blocked", "blocked",
			"judge blocked: Ruby is 2.6.10; repository AGENTS.md requires stopping and asking the user to activate mise when Ruby is below 4.0.",
			StageJudge, "Ruby is 2.6.10; repository AGENTS.md", "judge blocked: Ruby is 2.6.10", "magnum open talkable#1"},
		{"blocked inferred", "",
			"blocked: agents: prompt mg-1-codex-judge: agent blocked: herdr agent.prompt: agent_blocked: agent is blocked and requires interactive input",
			StageJudge, "agents: prompt mg-1-codex-judge", "judge blocked: ", "magnum open talkable#1"},
		{"identity", "identity_error",
			"judge identity check failed: gh api user --jq .login exited 1 with error connecting to api.github.com; no GitHub writes were made.",
			StageIdentity, "gh api user --jq .login exited 1", "identity check failed: gh api user", "magnum identities check"},
		{"leak", "identity_leak",
			`review 5402115199 carrying the run's marker was posted as "zhuravel", not "zhuravel[bot]"`,
			StageIdentity, "posted as \"zhuravel\"", "identity leak: review 5402115199", "magnum resume"},
		{"no review", "needs_attention",
			"no review carrying the run's marker after the nudge",
			StageReview, "no review carrying", "no review carrying the run's marker", "magnum open talkable#1"},
		{"plain error", "failed",
			"3 attempts on 1234567: checkout in review1: slots: something odd happened",
			StageCheckout, "something odd happened", "checkout failed 3× on 1234567: something odd happened", "magnum ignore talkable#1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Explain(tc.kind, tc.msg, "talkable#1")
			if r.Stage != tc.stage || !strings.Contains(r.Cause, tc.cause) || !strings.HasPrefix(r.Summary, tc.summary) || !strings.Contains(r.Fix, tc.fix) {
				t.Fatalf("reason\n stage   %q\n cause   %q\n summary %q\n fix     %q", r.Stage, r.Cause, r.Summary, r.Fix)
			}
		})
	}
}

// Terminal escapes in an error (colors, a window-title OSC) never reach a
// screen through the explanation.
func TestExplainDropsTerminalEscapes(t *testing.T) {
	r := Explain("", "boom \x1b[31mred\x1b]0;pwned\a\x1b[0m\r\nnext", "talkable#1")
	if r.Summary != "boom red" || strings.ContainsAny(r.Summary+r.Cause+strings.Join(r.Tail, ""), "\x1b\a") {
		t.Fatalf("reason %+v", r)
	}
}

// The summary and the cause stay one line of bounded length whatever the
// error holds.
func TestExplainKeepsTheSummaryOneShortLine(t *testing.T) {
	r := Explain("failed", "3 attempts on abcdef1: checkout in review1: x exited 1: "+strings.Repeat("error: very long ", 100), "talkable#1")
	if n := len([]rune(r.Summary)); n > SummaryMax || strings.Contains(r.Summary, "\n") {
		t.Fatalf("summary %d runes: %q", n, r.Summary)
	}
	if n := len([]rune(r.Cause)); n > SummaryMax {
		t.Fatalf("cause %d runes", n)
	}
}

// A fix job's attention names the fixer (its report's file name says which
// role), the report path its message carries and the two remedies, `magnum
// fix` and `magnum abort`; its summary keeps the job's own sentence. A
// message without a report names the fixer alone, and a row stored without
// its kind is still read as the fix job's.
func TestExplainNamesTheFixerItsReportAndTheRemedies(t *testing.T) {
	const report = "/state/reviews/talkable/talkable/2/a1a1a1a1a1a1/claude-fix.md"
	for _, tc := range []struct {
		name, kind, msg         string
		summary, cause, fixWith string
	}{
		{"needs you", KindFix, "fix: 2 findings need your call: " + report,
			"fix: 2 findings need your call: " + report, "2 findings need your call", "read claude-fix's report " + report},
		{"failed run", KindFix, "fix run failed: the fixer's push failed: " + report,
			"fix run failed: the fixer's push failed: " + report, "the fixer's push failed", "read claude-fix's report " + report},
		{"no report", KindFix, "fix guard failed: slot review1 holds c3c3c3c, not the reviewed head a1a1a1a",
			"fix guard failed: slot review1 holds c3c3c3c", "slot review1 holds c3c3c3c", "`magnum open talkable#2` shows the fixer's pane"},
		{"kind inferred", "", "fix run failed: claude-fix run r-1 is failed: agent blocked",
			"fix run failed: claude-fix run r-1 is failed", "agent blocked", "`magnum fix talkable#2`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Explain(tc.kind, tc.msg, "talkable#2")
			if r.Kind != KindFix || r.Stage != StageFix || !strings.HasPrefix(r.Summary, tc.summary) || !strings.Contains(r.Cause, tc.cause) {
				t.Errorf("reason kind %q stage %q\n summary %q\n cause   %q", r.Kind, r.Stage, r.Summary, r.Cause)
			}
			for _, want := range []string{tc.fixWith, "`magnum fix talkable#2`", "`magnum abort talkable#2`"} {
				if !strings.Contains(r.Fix, want) {
					t.Errorf("fix %q lacks %q", r.Fix, want)
				}
			}
			if strings.Contains(r.Fix, "judge") {
				t.Errorf("fix %q names the judge", r.Fix)
			}
		})
	}
}

// A fix reason too long for the toast's one line keeps the counts, which come
// first, and drops the report's path, which the next step names in full,
// instead of cutting the path in the middle.
func TestAFixReasonTooLongForTheSummaryDropsTheReportPathNotTheCounts(t *testing.T) {
	const report = "/Users/alice/.local/share/magnum/reviews/talkable/talkable/11483/75f3675c950b/claude-fix.md"
	msg := "5 fix cycles (14 on this PR), 3 findings left: 1 new, 0 missed earlier, 2 fallout of the fixes: " + report
	r := Explain(KindFix, msg, "talkable#11483")
	if want := "fix: 5 fix cycles (14 on this PR), 3 findings left: 1 new, 0 missed earlier, 2 fallout of the fixes"; r.Summary != want {
		t.Errorf("summary = %q, want %q", r.Summary, want)
	}
	if r.Cause != strings.TrimPrefix(r.Summary, "fix: ") {
		t.Errorf("cause = %q, want the summary's sentence", r.Cause)
	}
	if !strings.Contains(r.Fix, report) {
		t.Errorf("fix = %q, want it to name the report %s", r.Fix, report)
	}
	short := "2 fix cycles, 2 findings left: " + report
	if r := Explain(KindFix, short, "talkable#2"); r.Summary != "fix: "+short {
		t.Errorf("summary of a line that fits = %q, want it whole", r.Summary)
	}
}

// On talkable#11932 a launch that never came up held the judge's agent name
// for hours: every start failed with herdr's agent_name_taken (or, with the
// launch in another pane, waited as busy), and the board said "setup". The
// reason names the dead launch, the role, its pane and the agent, and says
// the next retry closes it, for a PR in needs_attention and for one that
// still retries alike; a chain without the signature reads as before.
func TestExplainsADeadLaunchThatHoldsTheName(t *testing.T) {
	want := "a dead launch holds the codex-judge's pane w9J:p2 (agent mg-11932-codex-judge-5d01cf): closed on the next retry"
	for _, tc := range []struct {
		name, kind, msg, summary string
	}{
		{"name taken, three attempts", KindFailed,
			"3 attempts on 5d01cfa: start codex-judge: agents: start mg-11932-codex-judge-5d01cf in w9J:p2: herdr agent.start: agent_name_taken: agent name mg-11932-codex-judge-5d01cf is already in use",
			"agents failed 3× on 5d01cfa: " + want},
		{"name taken, retrying", "",
			"start codex-judge: agents: start mg-11932-codex-judge-5d01cf in w9J:p2: herdr agent.start: agent_name_taken: agent name mg-11932-codex-judge-5d01cf is already in use",
			"agents failed: " + want},
		{"listed in another pane, retrying", "",
			"start codex-judge: agents: start mg-11932-codex-judge-5d01cf: herdr still lists in pane w9J:p2 the agent of session 535, which magnum closed; the agent never came up and has no conversation: agent busy",
			"agents failed: " + want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Explain(tc.kind, tc.msg, "talkable#11932")
			if r.Kind != KindFailed || r.Stage != StageAgents || r.Cause != want || r.Summary != tc.summary {
				t.Fatalf("reason %+v", r)
			}
			if !strings.Contains(r.Fix, "next retry closes the pane") || !strings.Contains(r.Fix, "`magnum review talkable#11932`") ||
				!strings.Contains(r.Fix, "`magnum open talkable#11932`") {
				t.Fatalf("fix %q", r.Fix)
			}
		})
	}
	other := Explain(KindFailed, "3 attempts on 5d01cfa: start codex-judge: agents: start mg-11932-codex-judge-5d01cf in w9J:p2: herdr agent.start: timeout: no agent", "talkable#11932")
	if strings.Contains(other.Summary, "dead launch") {
		t.Fatalf("a start that timed out reads as a dead launch: %+v", other)
	}
}
