package factory

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// The words of each move, as the operator reads them: the long form, then
// the short form that fits the YOUR MOVE column of a narrow screen.
const (
	mvRerunCI       = "b: re-run CI on GitHub, or push"
	mvRerunCIShort  = "b re-run CI"
	mvFailedCI      = "b opens its CI"
	mvFailedCIShort = "b CI"
	mvCIFix         = "c fixes its CI, b opens it"
	mvCIFixShort    = "c CI fix"
	mvReview        = "r reviews it"
	mvReviewShort   = "r review"
	mvThreads       = "b opens the threads"
	mvThreadsShort  = "b threads"
	mvReport        = "e opens the report, b the PR"
	mvReportShort   = "e report · b PR"
	mvFixes         = "f fixes, b opens it"
	mvFixesShort    = "f fix · b PR"
	mvOpens         = "b opens it"
	mvOpensShort    = "b PR"
)

// verdictNeeds is a fresh verdict that needs you for why, with needs of the
// kinds given (their links are not the move's business).
func verdictNeeds(why string, kinds ...string) func(*PRState) {
	return func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenNeedsYou, why)
		for _, k := range kinds {
			v.Open.Needs = append(v.Open.Needs, engine.OpenNeed{Kind: k, URL: "https://github.com/example/talkable/pull/1"})
		}
	}
}

// offersF reports whether move offers the fixer: f as a key of its own.
func offersF(move string) bool { return offersKey(move, "f") }

// offersKey reports whether move names key as a key of its own.
func offersKey(move, key string) bool {
	return slices.ContainsFunc(strings.FieldsFunc(move, func(r rune) bool { return r == ' ' || r == ',' }),
		func(w string) bool { return w == key })
}

// Your move on a PR that needs you follows the reason magnum's look gave,
// not one fixed pair of keys: re-run a check that did not run, open the
// CI of one that failed, open the threads magnum stopped arguing in, read
// the fixer's report, review a PR magnum has not reviewed, fix open
// findings, else open the PR. f is offered only where the fixer has work
// (open findings it can take): it reads no CI, so it is no move for a
// failed check, a check that did not run, a stalemate or a reason nothing
// knows. Before, every one of them said "b or f", and talkable-docs#447 (a
// clean review, a required check that never started) sent the operator to a
// fixer with nothing to fix.
func TestYourMoveFollowsTheReasonThePRNeedsYou(t *testing.T) {
	const didNotRun = "build did not run at a1b2c3d: run its workflow on the PR's branch, or push"
	for name, c := range map[string]struct {
		edit        func(*PRState)
		long, short string
	}{
		"a required check that did not run (kind ci_missing)":               {verdictNeeds(didNotRun, "ci_missing"), mvRerunCI, mvRerunCIShort},
		"a required check that did not run (kind ci, as before ci_missing)": {verdictNeeds(didNotRun, "ci"), mvRerunCI, mvRerunCIShort},
		"a required check that failed":                                      {verdictNeeds("required check build failed at a1b2c3d", "ci"), mvCIFix, mvCIFixShort},
		"a check of the CI that failed":                                     {verdictNeeds("CI failed at a1b2c3d", "ci"), mvCIFix, mvCIFixShort},
		"a failed check, how it failed":                                     {verdictNeeds("required check build failed at a1b2c3d (timed out)", "ci"), mvCIFix, mvCIFixShort},
		"a failed check whose CI fix found a flaky test":                    {verdictNeeds("required check build failed at a1b2c3d: the CI fix found a flaky test, not the PR's code", "ci"), mvCIFix, mvCIFixShort},
		"a failed check whose CI fix was stopped":                           {verdictNeeds("required check build failed at a1b2c3d: the CI fix at a1b2c3d was stopped; it is yours", "ci"), mvCIFix, mvCIFixShort},
		"a failed check red on the base too":                                {verdictNeeds("required check build failed at a1b2c3d, and on master at c0c0c0c too: magnum fixes only a check that passes on the base", "ci"), mvFailedCI, mvFailedCIShort},
		"a failed check the base never ran":                                 {verdictNeeds("required check build failed at a1b2c3d; master's last 5 commits never ran it: magnum fixes only a check that passes on the base", "ci"), mvFailedCI, mvFailedCIShort},
		"a failed check whose CI fix found a missing secret":                {verdictNeeds("required check build failed at a1b2c3d: the CI fix found a missing secret", "ci"), mvFailedCI, mvFailedCIShort},
		"a failed check whose CI fix found an infrastructure failure":       {verdictNeeds("required check build failed at a1b2c3d: the CI fix found an infrastructure failure", "ci"), mvFailedCI, mvFailedCIShort},
		"a failed check whose CI fix found a CI config change":              {verdictNeeds("required check build failed at a1b2c3d: the CI fix found a CI config change it may not make", "ci"), mvFailedCI, mvFailedCIShort},
		"a failed check whose CI fix found a failure on the base":           {verdictNeeds("required check build failed at a1b2c3d: the CI fix found a failure on the base", "ci"), mvFailedCI, mvFailedCIShort},
		"a failed check after 3 CI fixes":                                   {verdictNeeds("required check build failed at a1b2c3d after 3 CI fixes: the CI is yours", "ci"), mvFailedCI, mvFailedCIShort},
		"a failed check whose run lists no failed job":                      {verdictNeeds("required check build failed at a1b2c3d: its run lists no failed job magnum can read; it is yours", "ci"), mvFailedCI, mvFailedCIShort},
		"a failed check magnum cannot read why":                             {verdictNeeds("required check build failed at a1b2c3d: magnum cannot read why (the log is gone)", "ci"), mvFailedCI, mvFailedCIShort},
		"a failed check of no GitHub Actions job":                           {verdictNeeds("required check build failed at a1b2c3d: no GitHub Actions job, so magnum cannot read why", "ci"), mvFailedCI, mvFailedCIShort},
		"a failed check of a PR that changes its own CI config":             {verdictNeeds("required check build failed at a1b2c3d: the PR changes its own CI config (a workflow, an action, a script of .github/bin), so its CI is yours", "ci"), mvFailedCI, mvFailedCIShort},
		"a failed check whose files GitHub did not list":                    {verdictNeeds("required check build failed at a1b2c3d: GitHub did not list every file of the PR, so magnum cannot tell whether it changes its CI", "ci"), mvFailedCI, mvFailedCIShort},
		"a failed check whose record of CI fixes cannot be read":            {verdictNeeds("required check build failed at a1b2c3d: the record of its CI fixes cannot be read", "ci"), mvFailedCI, mvFailedCIShort},
		"a ci need in words nothing knows (never f)":                        {verdictNeeds("build is red", "ci"), mvOpens, mvOpensShort},
		"a person's thread":                                                 {verdictNeeds("1 item needs you: 1 thread of a person", "thread"), mvThreads, mvThreadsShort},
		"magnum's reply that waits for you":                                 {verdictNeeds("magnum's reply waits for your answer: https://example.com/t", "magnum"), mvThreads, mvThreadsShort},
		"magnum stopped arguing (verdict)":                                  {verdictNeeds("magnum stopped arguing in 2 threads", "thread", "thread"), mvThreads, mvThreadsShort},
		"magnum stopped arguing (row alone)":                                {func(v *PRState) { v.Stalemate = true }, mvThreads, mvThreadsShort},
		"the fixer left you items":                                          {fixerAttention(fixerNeeds...), mvReport, mvReportShort},
		"the fixer's report, its verdict too": {func(v *PRState) {
			fixerAttention(fixerNeeds...)(v)
			verdictNeeds("needs your attention (fix)", "thread")(v)
		}, mvReport, mvReportShort},
		"a fix attention of a cap, with its report": {func(v *PRState) {
			v.State, v.Attention, v.AttentionMsg, v.FixReport = store.PRNeedsAttention, "fix", "fix: 2 fix cycles, 2 findings left", fixerReportPath
		}, mvReport, mvReportShort},
		"an attention of another kind": {func(v *PRState) {
			v.State, v.Attention, v.AttentionMsg = store.PRNeedsAttention, "blocked", "judge blocked: the slot's database is gone"
		}, mvOpens, mvOpensShort},
		"a finding the fixer skipped":                   {verdictNeeds("1 item needs you: 1 skipped finding", "finding"), mvFixes, mvFixesShort},
		"magnum's findings that wait for a fix":         {verdictNeeds("magnum's reply waits for your answer", "findings"), mvFixes, mvFixesShort},
		"the fix loops of the day used up":              {verdictNeeds("3 loops today: f starts another", "loop"), mvFixes, mvFixesShort},
		"the day's loops used up, a CI fix is the work": {verdictNeeds("3 loops today: f starts another, c a CI fix", "loop"), mvCIFix, mvCIFixShort},
		"the fix loop refused to start":                 {verdictNeeds("nothing to fix: the fixer's file lists nothing", "loop"), mvOpens, mvOpensShort},
		"magnum's review missing":                       {verdictNeeds("no review of magnum's stands at a1b2c3d: `magnum review` reviews it", "magnum"), mvReview, mvReviewShort},
		"magnum's review late":                          {verdictNeeds("magnum has not reviewed a1b2c3d since 09:00", "magnum_late"), mvReview, mvReviewShort},
		"a magnum need in words nothing knows":          {verdictNeeds("something about magnum", "magnum"), mvOpens, mvOpensShort},
		"replies of magnum's among the items":           {verdictNeeds("2 items need you: 2 replies of magnum's", "magnum", "magnum"), mvThreads, mvThreadsShort},
		"a conflict with the base":                      {verdictNeeds("1 item needs you: 1 conflict with the base", "conflict"), mvOpens, mvOpensShort},
		"a review body":                                 {verdictNeeds("1 item needs you: 1 review body of a person", "review"), mvOpens, mvOpensShort},
		"a thread and a skipped finding":                {verdictNeeds("2 items need you", "thread", "finding"), mvOpens, mvOpensShort},
		"a reason of a kind nothing knows":              {verdictNeeds("something new", "new_kind"), mvOpens, mvOpensShort},
		"a verdict that names no kind":                  {verdictNeeds("the fix row of #4 cannot be read: `magnum abort` clears it"), mvOpens, mvOpensShort},
		"no verdict at all":                             {func(v *PRState) { v.HasOpen = false }, mvOpens, mvOpensShort},
		"a stale verdict's kinds are not read": {func(v *PRState) {
			verdictNeeds(didNotRun, "ci")(v)
			v.Stale = true
		}, mvOpens, mvOpensShort},
		"a verdict that does not need you": {func(v *PRState) {
			v.HasOpen, v.Open = true, verdict(engine.OpenWaits, didNotRun)
			v.Open.Needs = []engine.OpenNeed{{Kind: "ci"}}
		}, mvOpens, mvOpensShort},
	} {
		t.Run(name, func(t *testing.T) {
			var st PRState
			c.edit(&st)
			got := reasonMove(st)
			if got.long != c.long || got.short != c.short {
				t.Errorf("move %q / %q, want %q / %q", got.long, got.short, c.long, c.short)
			}
			if fixer := c.long == mvFixes; offersF(got.long) != fixer || offersF(got.short) != fixer {
				t.Errorf("move %q / %q: f offered %v, want it only for findings the fixer can take", got.long, got.short, offersF(got.long))
			}
			if ciFix := c.long == mvCIFix; offersKey(got.long, "c") != ciFix || offersKey(got.short, "c") != ciFix {
				t.Errorf("move %q / %q: c offered %v, want it only for a failed check the CI fix may take", got.long, got.short, offersKey(got.long, "c"))
			}
		})
	}
}

// A clean review of magnum's and a required check that did not run at the
// head (talkable-docs#447: the CI run failed before any job started) need
// you to run the check again, which f does not: your PR's row and a
// published task's row both say "b re-run CI" and offer no f, whichever
// kind the verdict's need has.
func TestAPRWhoseRequiredCheckDidNotRunSaysToRerunCI(t *testing.T) {
	const didNotRun = "build did not run at a1b2c3d: run its workflow on the PR's branch, or push"
	for _, kind := range []string{"ci_missing", "ci"} {
		t.Run(kind, func(t *testing.T) {
			r := newScreenRig(t)
			r.withMagnum()
			r.publishedTask("PR-12340", 4540)
			edit := func(v *PRState) {
				reviewedAt(v, [4]int{}, store.VerdictClean)
				v.CI = &tui.CIInfo{State: "passed", Total: 41, Passed: 41, Required: []tui.CheckState{{Name: "build", Label: "build", State: "missing"}}}
				verdictNeeds(didNotRun, kind)(v)
			}
			r.pr(4540, edit)
			r.own(81, 4541, edit)
			d := r.data(time.Time{})
			for _, key := range []string{"PR-12340", "#4541"} {
				row := rowOf(t, d, key)
				if row.Group != tui.GroupNeedsYou || row.Move != mvRerunCI || row.MoveShort != mvRerunCIShort {
					t.Errorf("%s: group %d, move %q / %q, want NEEDS YOU and %q / %q", key, row.Group, row.Move, row.MoveShort, mvRerunCI, mvRerunCIShort)
				}
				if offersF(row.Move) || offersF(row.MoveShort) {
					t.Errorf("%s: move %q / %q offers f for a check that did not run", key, row.Move, row.MoveShort)
				}
			}
		})
	}
}

// A required check that failed at the head of a PR whose review leaves
// nothing, and that the CI fix has not refused, says "c fixes its CI, b
// opens it": c is magnum's CI fix (the daemon's plan first, then y/N), on
// your PR's row and a published task's alike. One the CI fix refused (red on
// the base too) keeps "b opens its CI": c would be refused with the same
// words.
func TestAPRWhoseRequiredCheckFailedSaysToFixItWithC(t *testing.T) {
	for name, c := range map[string]struct {
		why         string
		long, short string
	}{
		"a plain failure":     {"required check build failed at a1b2c3d", mvCIFix, mvCIFixShort},
		"red on the base too": {"required check build failed at a1b2c3d, and on master at c0c0c0c too: magnum fixes only a check that passes on the base", mvFailedCI, mvFailedCIShort},
	} {
		t.Run(name, func(t *testing.T) {
			r := newScreenRig(t)
			r.withMagnum()
			r.publishedTask("PR-12340", 4540)
			edit := func(v *PRState) {
				reviewedAt(v, [4]int{}, store.VerdictClean)
				v.CI = &tui.CIInfo{State: "failed", Total: 41, Passed: 40, Failed: 1, Required: []tui.CheckState{{Name: "build", Label: "build", State: "failed"}}}
				verdictNeeds(c.why, "ci")(v)
			}
			r.pr(4540, edit)
			r.own(81, 4541, edit)
			d := r.data(time.Time{})
			for _, key := range []string{"PR-12340", "#4541"} {
				if row := rowOf(t, d, key); row.Group != tui.GroupNeedsYou || row.Move != c.long || row.MoveShort != c.short {
					t.Errorf("%s: group %d, move %q / %q, want NEEDS YOU and %q / %q", key, row.Group, row.Move, row.MoveShort, c.long, c.short)
				}
			}
		})
	}
}

// A published task's row says the same move as your PR's in the same
// state: the function is one, applied where either row is drawn. A task's
// row said "see the task card" for each of these.
func TestATasksPRAndYoursSayTheSameMoveForTheSameReason(t *testing.T) {
	for name, c := range map[string]struct {
		edit        func(*PRState)
		long, short string
	}{
		"a failed check": {verdictNeeds("required check build failed at a1b2c3d", "ci"), mvCIFix, mvCIFixShort},
		"a failed check the CI fix refused": {verdictNeeds("required check build failed at a1b2c3d, and on master at c0c0c0c too: magnum fixes only a check that passes on the base", "ci"),
			mvFailedCI, mvFailedCIShort},
		"the loops used up, a CI fix is the work": {verdictNeeds("3 loops today: f starts another, c a CI fix", "loop"), mvCIFix, mvCIFixShort},
		"a thread":          {verdictNeeds("1 item needs you: 1 thread of a person", "thread"), mvThreads, mvThreadsShort},
		"a stalemate":       {func(v *PRState) { v.OpenWatch, v.ReviewedSHA, v.Stalemate = false, prHead, true }, mvThreads, mvThreadsShort},
		"the fixer's items": {fixerAttention(fixerNeeds...), mvReport, mvReportShort},
		"a missing review":  {verdictNeeds("no review of magnum's stands at a1b2c3d", "magnum"), mvReview, mvReviewShort},
	} {
		t.Run(name, func(t *testing.T) {
			r := newScreenRig(t)
			r.withMagnum()
			r.publishedTask("PR-12340", 4540)
			r.pr(4540, c.edit)
			r.own(81, 4541, c.edit)
			d := r.data(time.Time{})
			for _, key := range []string{"PR-12340", "#4541"} {
				if row := rowOf(t, d, key); row.Group != tui.GroupNeedsYou || row.Move != c.long || row.MoveShort != c.short {
					t.Errorf("%s: group %d, move %q / %q, want NEEDS YOU and %q / %q", key, row.Group, row.Move, row.MoveShort, c.long, c.short)
				}
			}
		})
	}
}

// The words of the engine's verdicts that the move reads are in the engine's
// code, in the very forms reasonMove matches. The factory's tests cannot run
// the engine's look (its facts, ledger and GitHub reads are the engine's own
// test harness, and an exported entry would need the engine edited), so this
// reads the engine's source: a reword there fails here, where the move would
// otherwise fall back to the PR's page without a word.
func TestTheEnginesWordsThatTheMoveReadsAreStillTheEngines(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "engine", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	ci, verdict := read("fix_open_ci.go"), read("fix_open_verdict.go")
	cifix := read("ci_fix.go")
	for name, c := range map[string]struct{ src, want string }{
		"a required check that did not run": {ci, `"%s` + didNotRunWords + `%s: %s"`},
		"a required check that failed":      {verdict, `"%s` + failedWords + `%s"`},
		"magnum's review missing":           {verdict, `"` + noReviewWords},
		"magnum's reply that waits":         {verdict, `"magnum's ` + replyWaitsWords},
		"magnum's replies among the items":  {verdict, `"reply of magnum's"`},
		"the day's loops used up":           {verdict, `"%s today: ` + loopsUsedWords + `"`},
		"the day's loops used up, a CI fix": {verdict, `capped += "` + loopCIFixWords + `"`},
	} {
		if !strings.Contains(c.src, c.want) {
			t.Errorf("%s: the engine's code no longer holds %s, which reasonMove matches", name, c.want)
		}
	}
	// The reasons the CI fix refuses a failed check: each phrase reasonMove
	// reads is in ci_fix.go, as the verdict says it.
	for _, w := range ciRefusalWords {
		if !strings.Contains(cifix, w) {
			t.Errorf("the engine's ci_fix.go no longer holds %q, which says a CI fix is refused", w)
		}
	}
	for _, class := range ciCannotClassWords {
		if !strings.Contains(cifix, `"`+class+`"`) {
			t.Errorf("the engine's ci_fix.go no longer holds the CI fix's class %q, which says a CI fix is refused", class)
		}
	}
	if !strings.Contains(cifix, `"`+ciFixFoundWords+`"`) {
		t.Errorf("the engine's ci_fix.go no longer says %q before the class a CI fix found", ciFixFoundWords)
	}
}
