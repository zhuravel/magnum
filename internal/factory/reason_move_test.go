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
func offersF(move string) bool {
	return slices.ContainsFunc(strings.FieldsFunc(move, func(r rune) bool { return r == ' ' || r == ',' }),
		func(w string) bool { return w == "f" })
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
		"a required check that failed":                                      {verdictNeeds("required check build failed at a1b2c3d", "ci"), mvFailedCI, mvFailedCIShort},
		"a check of the CI that failed":                                     {verdictNeeds("CI failed at a1b2c3d", "ci"), mvFailedCI, mvFailedCIShort},
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
		"a finding the fixer skipped":           {verdictNeeds("1 item needs you: 1 skipped finding", "finding"), mvFixes, mvFixesShort},
		"magnum's findings that wait for a fix": {verdictNeeds("magnum's reply waits for your answer", "findings"), mvFixes, mvFixesShort},
		"the fix loops of the day used up":      {verdictNeeds("3 loops today: f starts another", "loop"), mvFixes, mvFixesShort},
		"the fix loop refused to start":         {verdictNeeds("nothing to fix: the fixer's file lists nothing", "loop"), mvOpens, mvOpensShort},
		"magnum's review missing":               {verdictNeeds("no review of magnum's stands at a1b2c3d: `magnum review` reviews it", "magnum"), mvReview, mvReviewShort},
		"magnum's review late":                  {verdictNeeds("magnum has not reviewed a1b2c3d since 09:00", "magnum_late"), mvReview, mvReviewShort},
		"a magnum need in words nothing knows":  {verdictNeeds("something about magnum", "magnum"), mvOpens, mvOpensShort},
		"replies of magnum's among the items":   {verdictNeeds("2 items need you: 2 replies of magnum's", "magnum", "magnum"), mvThreads, mvThreadsShort},
		"a conflict with the base":              {verdictNeeds("1 item needs you: 1 conflict with the base", "conflict"), mvOpens, mvOpensShort},
		"a review body":                         {verdictNeeds("1 item needs you: 1 review body of a person", "review"), mvOpens, mvOpensShort},
		"a thread and a skipped finding":        {verdictNeeds("2 items need you", "thread", "finding"), mvOpens, mvOpensShort},
		"a reason of a kind nothing knows":      {verdictNeeds("something new", "new_kind"), mvOpens, mvOpensShort},
		"a verdict that names no kind":          {verdictNeeds("the fix row of #4 cannot be read: `magnum abort` clears it"), mvOpens, mvOpensShort},
		"no verdict at all":                     {func(v *PRState) { v.HasOpen = false }, mvOpens, mvOpensShort},
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

// A published task's row says the same move as your PR's in the same
// state: the function is one, applied where either row is drawn. A task's
// row said "see the task card" for each of these.
func TestATasksPRAndYoursSayTheSameMoveForTheSameReason(t *testing.T) {
	for name, c := range map[string]struct {
		edit        func(*PRState)
		long, short string
	}{
		"a failed check":    {verdictNeeds("required check build failed at a1b2c3d", "ci"), mvFailedCI, mvFailedCIShort},
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
	for name, c := range map[string]struct{ src, want string }{
		"a required check that did not run": {ci, `"%s` + didNotRunWords + `%s: %s"`},
		"a required check that failed":      {verdict, `"%s` + failedWords + `%s"`},
		"magnum's review missing":           {verdict, `"` + noReviewWords},
		"magnum's reply that waits":         {verdict, `"magnum's ` + replyWaitsWords},
		"magnum's replies among the items":  {verdict, `"reply of magnum's"`},
		"the day's loops used up":           {verdict, `"%s today: ` + loopsUsedWords + `"`},
	} {
		if !strings.Contains(c.src, c.want) {
			t.Errorf("%s: the engine's code no longer holds %s, which reasonMove matches", name, c.want)
		}
	}
}
