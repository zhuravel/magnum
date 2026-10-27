package attention

import (
	"encoding/json"
	"os"
	"slices"
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

// A park whose cause is on magnum's side (a setup or round that failed, the
// agent API overloaded, the judge blocked or stopped without a review,
// magnum's own identity) is told apart from one that asks the PR's author
// (the fix job's), by the kind alone: the status comment says the first is
// magnum's fault, never the second.
func TestOnlyTheFixJobsParkIsNotOnMagnumsSide(t *testing.T) {
	for _, k := range Kinds() {
		if got, want := OnMagnumsSide(k), k != KindFix; got != want {
			t.Errorf("OnMagnumsSide(%q) = %v, want %v", k, got, want)
		}
	}
	if OnMagnumsSide("") || OnMagnumsSide("muted") {
		t.Error("an unknown kind is on magnum's side")
	}
	if k := Explain("", "7 attempts on abc1234def: fetch: exit 128", "").Kind; !OnMagnumsSide(k) {
		t.Errorf("an older row's inferred kind %q is not on magnum's side", k)
	}
}

// A fix job whose fixer left findings it could not close says what to do
// about each, where (talkable-integration#867: one finding fixed in a thread
// a colleague started, where the fixer did not write then, and one P2 of
// magnum's it declined; "2 findings need your call" told the operator
// neither): the message is a sentence per finding with its thread's link
// and the report last, the summary counts the calls, the next step is the
// calls one by one (Steps, which `magnum status` prints one per line,
// joined in Fix), and NeedsYou carries each call's thread, link and
// sentence. A fix in another reviewer's thread asks for the reply alone
// (that reviewer resolves it). Findings named in the older words follow the
// calls, and the loop's end after a re-review that did not post keeps its
// own words before them.
func TestTheFixRemedySaysWhatToDoPerThread(t *testing.T) {
	const (
		report   = "/state/reviews/talkable/talkable/2/a1a1a1a1a1a1/claude-fix.md"
		other    = "https://github.com/talkable/talkable/pull/2#discussion_r4302616576"
		magnums  = "https://github.com/talkable/talkable/pull/2#discussion_r4302866759"
		fixedIn  = "reply fixed in b2b2b2b in another reviewer's thread: " + other
		declined = "decide magnum's P2 on src/integration.js:1706: the fixer declined it; reply won't fix at " + magnums +
			", or `magnum fix talkable/talkable#2` sends it back"
	)
	calls := []FixCall{
		FixedCall("4302616576", other, "b2b2b2b2b2b2b2b2b2b2", false),
		DeclinedCall("4302866759", magnums, "P2", "src/integration.js:1706", "talkable/talkable#2", true),
	}
	if calls[0].Action != fixedIn || calls[1].Action != declined {
		t.Fatalf("calls = %+v", calls)
	}
	if got, want := FixedCall("2101", magnums, "b2b2b2b", true).Action,
		"reply fixed in b2b2b2b in magnum's thread and resolve it: "+magnums; got != want {
		t.Errorf("a fixed finding whose reply failed in magnum's thread = %q, want %q", got, want)
	}
	if got := DeclinedCall("4302616576", other, "P1", "app/a.rb:3", "talkable/talkable#2", false).Action; !strings.HasPrefix(got, "decide another reviewer's P1 on app/a.rb:3: ") {
		t.Errorf("a declined finding in another reviewer's thread = %q", got)
	}
	msg := "fix: " + FixCallsText(calls, "", report)
	if want := "fix: " + fixedIn + "; " + declined + "; details: " + report; msg != want {
		t.Fatalf("message = %q, want %q", msg, want)
	}

	r := Explain(KindFix, msg, "talkable#2")
	if r.Kind != KindFix || r.Stage != StageFix || r.Summary != "fix: reply in 1 thread, decide 1 finding" {
		t.Errorf("reason kind %q stage %q summary %q", r.Kind, r.Stage, r.Summary)
	}
	steps := []string{fixedIn, declined, "details: " + report}
	if !slices.Equal(r.Steps, steps) || r.Fix != strings.Join(steps, "; ") {
		t.Errorf("steps %q\nfix %q", r.Steps, r.Fix)
	}
	if !slices.Equal(r.NeedsYou, calls) {
		t.Errorf("needs you = %+v, want %+v", r.NeedsYou, calls)
	}
	if r := Explain("", msg, "talkable#2"); r.Kind != KindFix || len(r.NeedsYou) != 2 {
		t.Errorf("a row stored without its kind: kind %q, needs you %+v", r.Kind, r.NeedsYou)
	}

	more := "1 more finding needs your call (2101 P1 app/models/offer.rb:42)"
	r = Explain(KindFix, "fix: "+FixCallsText(calls[1:], more, report), "talkable#2")
	if r.Summary != "fix: decide 1 finding, 1 more needs your call" || !slices.Equal(r.Steps, []string{declined, more, "details: " + report}) {
		t.Errorf("with older words: summary %q, steps %q", r.Summary, r.Steps)
	}

	ended := "fix loop ended: the re-review did not post (error); " + FixCallsText(calls[:1], "", report)
	r = Explain(KindFix, ended, "talkable#2")
	if r.Summary != "fix loop ended: the re-review did not post (error); reply in 1 thread" || len(r.NeedsYou) != 1 ||
		!slices.Equal(r.Steps, []string{fixedIn, "details: " + report}) {
		t.Errorf("after a re-review that did not post: summary %q, steps %q, needs you %+v", r.Summary, r.Steps, r.NeedsYou)
	}

	if r := Explain(KindFix, "fix: 2 findings need your call: "+report, "talkable#2"); len(r.NeedsYou) != 0 || len(r.Steps) != 0 {
		t.Errorf("the older words name calls: %+v", r)
	}
}

// On talkable#12112 the fixer answered magnum's P1 thread but listed the fix
// under Needs you, and the loop's end asked to "reply fixed in e05fbde in
// magnum's thread and resolve it". A thread of magnum's that shows its
// answer and is still open now asks only for the resolution: the call reads
// back from the stored message beside the others, the card counts it as a
// resolution, and its link is the thread's.
func TestAResolveCallReadsBackForTheCard(t *testing.T) {
	const (
		report  = "/state/reviews/talkable/talkable/2/a1a1a1a1a1a1/claude-fix.md"
		magnums = "https://github.com/talkable/talkable/pull/2#discussion_r4302866759"
		other   = "https://github.com/talkable/talkable/pull/2#discussion_r4302616576"
	)
	resolve := ResolveCall("4302866759", magnums)
	if want := "resolve magnum's thread, already answered: " + magnums; resolve.Action != want || resolve.Thread != "4302866759" || resolve.URL != magnums {
		t.Fatalf("ResolveCall = %+v, want the action %q", resolve, want)
	}
	calls := []FixCall{FixedCall("4302616576", other, "e05fbde", false), resolve,
		DeclinedCall("4302616576", other, "P2", "app/a.rb:3", "talkable/talkable#2", false)}
	r := Explain(KindFix, "fix: "+FixCallsText(calls, "", report), "talkable#2")
	if !slices.Equal(r.NeedsYou, calls) {
		t.Errorf("needs you = %+v\nwant        %+v", r.NeedsYou, calls)
	}
	if r.Summary != "fix: reply in 1 thread, resolve 1 thread, decide 1 finding" {
		t.Errorf("summary %q", r.Summary)
	}
	if want := []string{calls[0].Action, resolve.Action, calls[2].Action, "details: " + report}; !slices.Equal(r.Steps, want) {
		t.Errorf("steps %q\nwant  %q", r.Steps, want)
	}
	if r := Explain(KindFix, "fix: "+FixCallsText([]FixCall{resolve}, "", report), "talkable#2"); r.Summary != "fix: resolve 1 thread" || len(r.NeedsYou) != 1 {
		t.Errorf("a resolution alone: summary %q, needs you %+v", r.Summary, r.NeedsYou)
	}
}

// A conflict with the base that the fixer left to the author (it needs a
// product decision, so the fixer aborted its merge) is a call without a
// thread: the PR's page, where the author merges the base, and the paths
// that need the decision. It reads back for the card as the other calls do,
// and the summary counts it first.
func TestAConflictCallReadsBackForTheCard(t *testing.T) {
	const (
		report = "/state/reviews/talkable/talkable/2/a1a1a1a1a1a1/claude-fix.md"
		page   = "https://github.com/talkable/talkable/pull/2"
		other  = "https://github.com/talkable/talkable/pull/2#discussion_r4302616576"
	)
	conflict := ConflictCall([]string{"app/models/offer.rb", "db/schema.rb"}, page)
	if want := "merge the base by hand and decide the conflict in app/models/offer.rb, db/schema.rb: " + page; conflict.Action != want ||
		conflict.Thread != "" || conflict.URL != page {
		t.Fatalf("ConflictCall = %+v, want the action %q", conflict, want)
	}
	calls := []FixCall{conflict, DeclinedCall("4302616576", other, "P2", "app/a.rb:3", "talkable/talkable#2", false)}
	r := Explain(KindFix, "fix: "+FixCallsText(calls, "", report), "talkable#2")
	if !slices.Equal(r.NeedsYou, calls) {
		t.Errorf("needs you = %+v\nwant        %+v", r.NeedsYou, calls)
	}
	if r.Summary != "fix: merge the base by hand, decide 1 finding" {
		t.Errorf("summary %q", r.Summary)
	}
	if want := []string{conflict.Action, calls[1].Action, "details: " + report}; !slices.Equal(r.Steps, want) {
		t.Errorf("steps %q\nwant  %q", r.Steps, want)
	}
	if b, err := json.Marshal(conflict); err != nil || strings.Contains(string(b), `"thread"`) {
		t.Errorf("the conflict call's JSON = %s, %v; want no thread", b, err)
	}
}

// A message stored before the fixer replied in other reviewers' threads
// (spec 82's words: "in the thread a human started and resolve it", "a
// human's") still reads as its calls, so a PR that needed attention then
// keeps its card and `magnum status` lines after the upgrade.
func TestAFixCallInTheOlderWordsStillReads(t *testing.T) {
	const (
		report = "/state/reviews/talkable/talkable/2/a1a1a1a1a1a1/claude-fix.md"
		human  = "https://github.com/talkable/talkable/pull/2#discussion_r4302616576"
		fixed  = "reply fixed in b2b2b2b in the thread a human started and resolve it: " + human
		decide = "decide a human's P1 on app/a.rb:3: the fixer declined it; reply won't fix at " + human + ", or `magnum fix talkable/talkable#2` sends it back"
	)
	r := Explain(KindFix, "fix: "+fixed+"; "+decide+"; details: "+report, "talkable#2")
	want := []FixCall{{Thread: "4302616576", URL: human, Action: fixed}, {Thread: "4302616576", URL: human, Action: decide}}
	if !slices.Equal(r.NeedsYou, want) || r.Summary != "fix: reply in 1 thread, decide 1 finding" {
		t.Errorf("needs you %+v, summary %q", r.NeedsYou, r.Summary)
	}
}
