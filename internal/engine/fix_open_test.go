package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// openHarness is autofixHarness with autofix = "open": the operator's PR
// #2, opened at fixHead, is reviewed by its next tick with what next gives.
// mods run before the engine is built.
func openHarness(t *testing.T, next func(in pipeline.RoundInput) (map[string]int, string), mods ...func(*harness)) (*harness, *fixGit) {
	t.Helper()
	return autofixHarness(t, "zhuravel", next, append([]func(*harness){func(h *harness) { h.cfg.Pipeline.Autofix = config.AutofixOpen }}, mods...)...)
}

// pastSettle moves the clock past the settle's cap and the hands-off time
// after the operator's own push.
func (h *harness) pastSettle() { h.advance(31 * time.Minute) }

// colleagueThread is rev-ann's unanswered thread on PR #2.
func (h *harness) colleagueThread(id int64) github.Thread {
	return extThread(id, "lib/sandbox.rb", 185, h.clock.Now().Add(-time.Hour), "MINOR: `run` drops stderr", "rev-ann")
}

// setThreads makes PR #2's review threads ts.
func (h *harness) setThreads(ts ...github.Thread) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	h.gh.threads = map[int][]github.Thread{2: ts}
}

// touch makes GitHub report PR #2 at head as updated now (a comment moved
// its updatedAt), and polls it.
func (h *harness) touch(head string) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: head, author: "zhuravel"})
	h.tick()
}

// openOf is PR #n's verdict (ok false: no row).
func (h *harness) openOf(n int) (OpenVerdict, bool) {
	h.t.Helper()
	raw, ok := kvValue(h, KVPROpen(h.pr(n).ID))
	if !ok {
		return OpenVerdict{}, false
	}
	v, ok := ParseOpenVerdict(raw)
	if !ok {
		h.t.Fatalf("PR #%d's verdict does not parse: %s", n, raw)
	}
	return v, true
}

// ledgerOf is PR #n's seen ledger.
func (h *harness) ledgerOf(n int) openLedger {
	h.t.Helper()
	var l openLedger
	if raw, ok := kvValue(h, kvPRFixSeen(h.pr(n).ID)); ok {
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			h.t.Fatal(err)
		}
	}
	return l
}

// wantOpen fails unless PR #2's verdict is verdict with why in its words.
func (h *harness) wantOpen(verdict, why string) OpenVerdict {
	h.t.Helper()
	v, ok := h.openOf(2)
	if !ok || v.Verdict != verdict || !strings.Contains(v.Why, why) {
		h.t.Fatalf("verdict = %+v, %v; want %s %q", v, ok, verdict, why)
	}
	return v
}

// markReviewed records PR #n as reviewed by magnum at head, its review
// clean (run run-review-<n>, review 100+n), without a round: the pool's one
// slot need not serve every PR of a test.
func (h *harness) markReviewed(n int, head string) {
	h.t.Helper()
	pr := h.pr(n)
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{pr.State}, store.PRReviewed, func(u *store.PRUpdate) { u.Set("reviewed_sha", head) }); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.DB().ExecContext(h.ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login,
  state, outcome, review_id, review_url, prompt_text, result_json, created_at)
  VALUES (?, ?, 1, 'codex-judge', 'initial', ?, ?, 'talkable[bot]', 'verified', 'posted', ?, ?, 'p', '{"event":"COMMENTED","findings":{}}', ?)`,
		fmt.Sprintf("run-review-%d", n), pr.ID, head, pr.Identity, 100+n, fmt.Sprintf("https://github.com/talkable/talkable/pull/%d#pullrequestreview-%d", n, 100+n),
		store.FormatTime(h.clock.Now())); err != nil {
		h.t.Fatal(err)
	}
}

// headOf is a head of 40 characters for PR #n.
func headOf(n int) string { return strings.Repeat(fmt.Sprintf("%x", n%16), 40) }

// openOwnPRs is a harness with autofix = "open" whose own PRs ns are open
// and reviewed clean (markReviewed), each at headOf(n).
func openOwnPRs(t *testing.T, ns ...int) (*harness, *fixGit) {
	t.Helper()
	h, git := fixHarness(t, func(h *harness) { h.cfg.Pipeline.Autofix = config.AutofixOpen })
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	specs := []prSpec{{n: 1, head: "base1"}}
	for _, n := range ns {
		specs = append(specs, prSpec{n: n, head: headOf(n), author: "zhuravel"})
	}
	h.open(specs...)
	h.tick() // queued
	for _, n := range ns {
		h.markReviewed(n, headOf(n))
	}
	return h, git
}

// answeredBy is th with the author's fix-marked reply, a minute later.
func answeredBy(th github.Thread) github.Thread {
	last := th.Comments[len(th.Comments)-1]
	th.Comments = append(slices.Clone(th.Comments), github.ThreadComment{ID: last.ID + 1, AuthorLogin: "zhuravel", AuthorType: "User",
		AuthorAssociation: "MEMBER", Body: "Fixed in b2b2b2b.\n\n" + postreview.FixReplyMarker, URL: last.URL + "-reply",
		CreatedAt: last.CreatedAt.Add(time.Minute)})
	return th
}

// A clean review of magnum's at the head, a colleague's thread that nobody
// answered, the reviewers settled: one tick starts a fix loop by itself,
// with by = autofix, where "own" started nothing after a clean review. The
// look hands its read of GitHub to the job, which reads it no second time.
func TestAutofixOpenStartsForAColleaguesThreadAfterACleanReview(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(4322458360))
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
	})
	h.tick() // reviewed
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Fatalf("fix runs = %d before the settle", n)
	}
	h.pastSettle()
	reads := h.gh.count("threads:talkable/talkable#2")
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		t.Fatalf("fix runs = %d, want the loop autofix started", n)
	}
	// The look's read, which the start hands to the job, and the settle's
	// (nameUnmarkedReplies): the job reads GitHub no second time.
	if n := h.gh.count("threads:talkable/talkable#2") - reads; n != 2 {
		t.Errorf("threads read %d times in the start's tick, want twice: the job took the look's read", n)
	}
	if by := h.fixStartBy(); by != autofixBy {
		t.Errorf("the loop was started by %q, want autofix", by)
	}
	if ev, ok := h.fixEvent("autofix.open"); !ok || !strings.Contains(ev.Message, "autofix starts a fix loop on 1 thread of a person") {
		t.Errorf("autofix.open = %q, %v", ev.Message, ok)
	}
	if l := h.ledgerOf(2); len(l.Starts) != 1 {
		t.Errorf("ledger starts = %v, want the one start", l.Starts)
	}
}

// Under "open" the round's end starts nothing, so that every start waits
// for the reviewers to settle; the first look after the settle starts the
// loop on magnum's findings.
func TestTheRoundEndStartsNothingUnderOpen(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{"P2": 2}, "COMMENTED"))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.tick() // reviewed with findings
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Fatalf("fix runs = %d at the round's end, want none", n)
	}
	h.tick() // the look after the round
	h.wantOpen(OpenWaits, "hands off")
	h.pastSettle()
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 || h.fixStartBy() != autofixBy {
		t.Fatalf("fix runs = %d by %q, want autofix's after the settle", n, h.fixStartBy())
	}
}

// A loop worked on magnum's review once: after it ends, the review is in
// the ledger, and later looks start nothing more for it.
func TestAutofixOpenStartsOnceForOneMagnumReview(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{"P2": 2}, "COMMENTED"))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.tick()
	h.pastSettle()
	h.tick() // the loop runs and ends: the fixer replied to every finding
	for range 3 {
		h.advance(40 * time.Minute)
		h.tick()
	}
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		t.Errorf("fix runs = %d, want one loop for the one review", n)
	}
	if l := h.ledgerOf(2); !slices.Contains(l.Reviews, "review:run-loop-1") {
		t.Errorf("ledger = %+v, want the review worked on", l)
	}
}

// A conflict with the PR's own base (a stacked PR's feature branch) is work:
// the look checks the base the PR merges into, not the default branch.
func TestAutofixOpenStartsOnAConflictWithThePRsOwnBase(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	git.conflict("app/models/offer.rb")
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", "0 threads", "none") })
	h.tick() // reviewed
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", base: "feature"})
	h.tick() // the stacked PR's base, as GitHub reports it
	if pr := h.pr(2); deref(pr.BaseRef) != "feature" {
		t.Fatalf("base = %q, want feature", deref(pr.BaseRef))
	}
	h.pastSettle()
	h.tick()
	fetches, _ := git.calls()
	if len(fetches) == 0 || !slices.ContainsFunc(fetches, func(f string) bool { return strings.HasSuffix(f, ":feature") }) ||
		slices.ContainsFunc(fetches, func(f string) bool { return strings.HasSuffix(f, ":master") }) {
		t.Errorf("fetches = %v, want the PR's own base only", fetches)
	}
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		t.Fatalf("fix runs = %d, want the loop for the conflict", n)
	}
}

// The look reads GitHub only when the PR changed, or after the safety
// interval: two ticks over an unchanged PR read once, a new updatedAt reads
// again, and 30 minutes later it reads again.
func TestAutofixOpenReadsGitHubOnlyWhenThePRChanged(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.tick()
	h.pastSettle()
	reads := func() int { return h.gh.count("threads:talkable/talkable#2") }
	before := reads()
	h.tick()
	h.tick()
	if n := reads() - before; n != 1 {
		t.Fatalf("threads read %d times over two ticks of an unchanged PR, want once", n)
	}
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
	h.advance(time.Minute)
	h.touch(fixHead)
	if n := reads() - before; n != 2 {
		t.Errorf("threads read %d times after the PR changed, want twice", n)
	}
	h.advance(31 * time.Minute)
	h.tick()
	if n := reads() - before; n != 3 {
		t.Errorf("threads read %d times after 30 minutes, want three times", n)
	}
}

// Below the GitHub floor the look reads nothing and starts nothing: the
// verdict waits for the budget.
func TestAutofixOpenHoldsBelowTheGitHubFloor(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(4322458360))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", "0 threads", "none") })
	h.tick()
	h.pastSettle()
	h.lowBudget("zhuravel", 10, h.clock.Now().Add(time.Hour))
	before := h.gh.count("threads:talkable/talkable#2")
	h.tick()
	if n := h.gh.count("threads:talkable/talkable#2") - before; n != 0 {
		t.Errorf("threads read %d times below the floor", n)
	}
	h.wantOpen(OpenWaits, "GitHub budget")
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Errorf("fix runs = %d below the floor", n)
	}
}

// Drafts, forks and PRs taken out with F get an off verdict and no loop;
// another author's PR gets no row at all.
func TestAutofixOpenLeavesDraftsForksFixOffAndOthersPRsAlone(t *testing.T) {
	for _, tc := range []struct {
		name, author, off string
		edit              func(h *harness, pr store.PR)
	}{
		{name: "draft", author: "zhuravel", off: OpenOffDraft, edit: func(h *harness, pr store.PR) {
			if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("is_draft", true) }); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "fork", author: "zhuravel", off: OpenOffFork, edit: func(h *harness, pr store.PR) {
			if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("is_cross_repo", true) }); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "fix off", author: "zhuravel", off: OpenOffFixOff, edit: func(h *harness, pr store.PR) {
			if err := h.e.setFixOff(h.ctx, pr.ID, FixOff{Since: h.clock.Now(), By: fixOffBy}); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "another author's", author: "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := autofixHarness(t, tc.author, findings(map[string]int{"P2": 1}, "COMMENTED"),
				func(h *harness) { h.cfg.Pipeline.Autofix = config.AutofixOpen })
			fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
			h.tick()
			h.pastSettle()
			if tc.edit != nil {
				tc.edit(h, h.pr(2))
			}
			if err := h.e.autofixOpen(h.ctx); err != nil {
				t.Fatal(err)
			}
			h.settle()
			if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
				t.Errorf("fix runs = %d, want none", n)
			}
			v, ok := h.openOf(2)
			switch {
			case tc.off == "" && ok:
				t.Errorf("another author's PR has a verdict: %+v", v)
			case tc.off != "" && (!ok || v.Verdict != OpenOff || v.Off != tc.off):
				t.Errorf("verdict = %+v, %v; want off (%s)", v, ok, tc.off)
			}
		})
	}
}

// A loop started by hand (f) is tracked like autofix's: its fixer's file
// lists every item, quiet and handed ones included, and the keys it had
// end up handed.
func TestAManualFixHandsEveryItemToTheFixer(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"), func(h *harness) {
		h.cfg.Pipeline.QuietMarkers = []string{"<!-- review-receipt -->"}
	})
	th := h.colleagueThread(4322458360)
	h.setThreads(th)
	h.gh.mu.Lock()
	h.gh.prComments = map[int][]github.PRComment{2: {{ID: 701, URL: prPage + "#issuecomment-701", Body: "Done.\n<!-- review-receipt -->",
		CreatedAt: h.clock.Now(), AuthorLogin: "review-tool", AuthorType: "Bot"}}}
	h.gh.mu.Unlock()
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", "0 threads", "none") })
	h.tick() // reviewed
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	runs := h.fixRuns(h.pr(2).ID)
	if len(runs) != 1 {
		t.Fatalf("fix runs = %+v", runs)
	}
	if x := readExternalOf(t, runs[0]); len(x.Threads) != 1 || len(x.Conversation) != 1 {
		t.Errorf("external file = %+v, want the thread and the quiet receipt", x)
	}
	h.tick() // the look sees the loop ended
	l := h.ledgerOf(2)
	for _, key := range []string{"thread:4322458360@4322458360", "remark:701"} {
		if _, ok := l.Handed[key]; !ok {
			t.Errorf("handed = %v, want %s, which the fixer had", l.Handed, key)
		}
	}
}

// The head the fixer pushed is not the operator's push: no hands-off wait
// follows it.
func TestTheFixersPushIsNotYourPush(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{"P2": 1}, "COMMENTED"))
	fixer(h, git, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "b2b2b2b Fix the total (review comment 4100000001)", fixReplied, "none")
	})
	h.tick()
	h.pastSettle()
	h.tick() // the loop starts; the fixer pushes, and the loop forces its re-review
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing {
		t.Fatalf("loop = %+v, %v; want it waiting for the re-review", st, ok)
	}
	h.tick() // the look sees the row in phase reviewing
	if l := h.ledgerOf(2); !slices.Contains(l.FixerHeads, fixPushed) {
		t.Errorf("fixer heads = %v, want %s", l.FixerHeads, fixPushed)
	}
}

// Over idle ticks the verdict row is written once: an unchanged verdict on
// an unchanged PR costs no write.
func TestTheOpenRowIsRewrittenOnlyWhenSomethingChanged(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.tick()
	h.pastSettle()
	h.tick()
	first := h.wantOpen(OpenIdle, "nothing open")
	for range 3 {
		h.advance(time.Minute)
		h.tick()
	}
	if v := h.wantOpen(OpenIdle, "nothing open"); !v.At.Equal(first.At) {
		t.Errorf("the verdict was rewritten at %s, want it kept from %s", v.At, first.At)
	}
}

// The verdict says since when it has said what it says (Since): a change
// of the PR's row rewrites it at a new time and keeps when that began, and
// another verdict begins anew. A reader tells from it how long a PR has
// needed you, not how long ago the look last decided.
func TestTheVerdictSaysSinceWhenItHasSaidTheSame(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.tick()
	h.pastSettle()
	h.tick()
	idle := h.wantOpen(OpenIdle, "nothing open")
	if !idle.Since.Equal(idle.At) {
		t.Fatalf("a new idle verdict says since %s, want its own time %s", idle.Since, idle.At)
	}
	h.advance(time.Minute)
	h.touch(fixHead)
	if v := h.wantOpen(OpenIdle, "nothing open"); !v.At.After(idle.At) || !v.Since.Equal(idle.At) {
		t.Errorf("idle again after a change of the PR: at %s since %s, want a new time since %s", v.At, v.Since, idle.At)
	}

	h.advance(time.Minute)
	pr := h.pr(2)
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{pr.State}, store.PRNeedsAttention, func(u *store.PRUpdate) {
		u.Set("last_error", "the round failed")
	}); err != nil {
		t.Fatal(err)
	}
	h.e.setKV(h.ctx, KVPRAttention(pr.ID), "failed")
	h.tick()
	needs := h.wantOpen(OpenNeedsYou, "needs your attention")
	if !needs.Since.Equal(needs.At) || !needs.Since.After(idle.At) {
		t.Errorf("a verdict that needs you after idle says since %s (at %s), want its own time", needs.Since, needs.At)
	}
	h.advance(time.Minute)
	h.touch(fixHead)
	if v := h.wantOpen(OpenNeedsYou, "needs your attention"); !v.At.After(needs.At) || !v.Since.Equal(needs.At) {
		t.Errorf("still needs you after a change of the PR: at %s since %s, want since %s", v.At, v.Since, needs.At)
	}
}

// A cycle whose fixer got no external file (both reads failed) hands no
// other reviewer's key: after the loop, a bot's thread the look read
// before the start is still work, never seen.
func TestItemsTheFixerNeverGotAreNotHanded(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{"P2": 1}, "COMMENTED"))
	h.setThreads(extThread(4322458500, "app/b.rb", 3, h.clock.Now().Add(-time.Hour), "P1: unsafe call", "review-bot"))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.tick() // reviewed
	h.tick() // the look reads the bot's thread; hands off still
	h.gh.mu.Lock()
	h.gh.threadsErr = errors.New("HTTP 502")
	h.gh.mu.Unlock()
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	if !runsWithoutFile(h) {
		t.Fatal("the fixer got an external file")
	}
	h.gh.mu.Lock()
	h.gh.threadsErr = nil
	h.gh.mu.Unlock()
	h.tick() // the look sees the loop ended
	l := h.ledgerOf(2)
	if _, ok := l.Handed["thread:4322458500@4322458500"]; ok {
		t.Errorf("handed = %v: the bot's thread the fixer never got is handed", l.Handed)
	}
	if !slices.Contains(l.Reviews, "review:run-loop-1") {
		t.Errorf("reviews = %v, want the review the loop worked on", l.Reviews)
	}
	if v, ok := h.openOf(2); ok && v.Verdict == OpenIdle {
		t.Errorf("verdict = %+v: the bot's thread is still work", v)
	}
}

// runsWithoutFile reports whether PR #2's fix runs all lack the external
// file beside their report.
func runsWithoutFile(h *harness) bool {
	for _, r := range h.fixRuns(h.pr(2).ID) {
		if _, ok := readExternalFile(filepath.Dir(deref(r.ReportPath))); ok {
			return false
		}
	}
	return true
}

// An item that arrived after the look's read but is in the fixer's file is
// handed: the keys come from the file the fixer got.
func TestTheHandedKeysComeFromTheFixersFile(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{"P2": 1}, "COMMENTED"))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.tick() // reviewed
	h.tick() // the look reads: nothing of other reviewers
	h.setThreads(h.colleagueThread(4322458700))
	id := h.fix(FixPayload{}) // its job reads the PR again
	h.handle()
	h.wantDone(id)
	h.tick() // the look sees the loop ended
	if _, ok := h.ledgerOf(2).Handed["thread:4322458700@4322458700"]; !ok {
		t.Errorf("handed = %v, want the thread in the fixer's file", h.ledgerOf(2).Handed)
	}
}

// A dry-run daemon reads and decides, but writes no row and starts nothing.
func TestAutofixOpenInADryRunWritesNoRowAndStartsNothing(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(4322458360))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", "0 threads", "none") })
	h.tick()
	h.pastSettle()
	verdict, _ := kvValue(h, KVPROpen(h.pr(2).ID))
	ledger, _ := kvValue(h, kvPRFixSeen(h.pr(2).ID))
	h.e.d.DryRun = true
	h.tick()
	if v, _ := kvValue(h, KVPROpen(h.pr(2).ID)); v != verdict {
		t.Errorf("a dry run wrote the verdict %s over %s", v, verdict)
	}
	if l, _ := kvValue(h, kvPRFixSeen(h.pr(2).ID)); l != ledger {
		t.Errorf("a dry run wrote the ledger %s over %s", l, ledger)
	}
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Errorf("fix runs = %d in a dry run", n)
	}
}

// A PR that needs attention, of any kind, starts no loop: the verdict needs
// you.
func TestAutofixOpenStartsNothingWhileThePRNeedsYou(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{"P2": 1}, "COMMENTED"))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.tick()
	h.pastSettle()
	pr := h.pr(2)
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRReviewed}, store.PRNeedsAttention, func(u *store.PRUpdate) {
		u.Set("last_error", "the round failed")
	}); err != nil {
		t.Fatal(err)
	}
	h.e.setKV(h.ctx, KVPRAttention(pr.ID), "failed")
	h.tick()
	if n := len(h.fixRuns(pr.ID)); n != 0 {
		t.Errorf("fix runs = %d while the PR needs you", n)
	}
	h.wantOpen(OpenNeedsYou, "needs your attention (failed)")
}

// One fetch of a base serves every PR of the clone in the tick.
func TestOneBaseFetchServesEveryPROfAClone(t *testing.T) {
	h, git := openOwnPRs(t, 2, 3, 4)
	h.tick() // the looks read and check the base
	h.pastSettle()
	before, _ := git.calls()
	h.tick() // every PR's check is due again
	after, _ := git.calls()
	if n := len(after) - len(before); n != 1 {
		t.Errorf("%d base fetches in the tick (%v), want one for the clone and its base", n, after[len(before):])
	}
	for n := 2; n <= 4; n++ {
		if l := h.ledgerOf(n); l.Conflict.Head != headOf(n) || l.Conflict.Unchecked {
			t.Errorf("PR #%d's conflict check = %+v", n, l.Conflict)
		}
	}
}

// At most four PRs read GitHub in one tick; the fifth says unknown and
// reads on the next tick.
func TestAtMostFourPRsReadGitHubInATick(t *testing.T) {
	h, _ := openOwnPRs(t, 2, 3, 4, 5, 6)
	h.tick()
	var unknown []int
	for n := 2; n <= 6; n++ {
		if v, ok := h.openOf(n); ok && v.Verdict == OpenUnknown {
			unknown = append(unknown, n)
		}
	}
	if !slices.Equal(unknown, []int{6}) {
		t.Errorf("unknown after the tick: %v, want the fifth PR, #6", unknown)
	}
	h.tick()
	for n := 2; n <= 6; n++ {
		if v, ok := h.openOf(n); !ok || v.Verdict == OpenUnknown {
			t.Errorf("PR #%d: %+v, %v after the next tick", n, v, ok)
		}
	}
}

// A watch switched back from "open" leaves no stale verdict: the look
// deletes the row of an own open PR whose watch is no longer "open".
func TestAnOpenRowOfAWatchThatLeftOpenIsDeleted(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.tick()
	h.pastSettle()
	h.tick()
	h.wantOpen(OpenIdle, "nothing open")
	h.cfg.Pipeline.Autofix = config.AutofixOwn
	h.tick()
	if v, ok := h.openOf(2); ok {
		t.Errorf("the verdict outlived autofix open: %+v", v)
	}
}

// When prepareFix refuses what the look decided to start as nothing to
// fix (the colleague's thread was answered after the look's read, which
// nothing has made due again), the PR needs you with the refusal's
// sentence: never unknown, which would read as looking for good. The look
// does not try again until the PR changes.
func TestANothingToFixRefusalNeedsYou(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(4322458360))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", "0 threads", "none") })
	h.tick() // reviewed
	h.tick() // the look reads the thread; hands off still
	h.setThreads(answeredBy(h.colleagueThread(4322458360)))
	h.advance(26 * time.Minute) // past the hands-off time, before the safety read
	h.tick()
	h.wantOpen(OpenNeedsYou, "nothing to fix")
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Errorf("fix runs = %d", n)
	}
	reads := h.gh.count("threads:talkable/talkable#2")
	h.advance(time.Minute)
	h.tick()
	if n := h.gh.count("threads:talkable/talkable#2") - reads; n != 0 {
		t.Errorf("the refused start was tried again (%d reads)", n)
	}
	h.wantOpen(OpenNeedsYou, "nothing to fix")
}

// A poll of GitHub that failed leaves the PR's row as it was: the look
// never starts a loop on it, and the verdict is unknown until a poll
// answers again. Below the GitHub floor it waits for the budget instead.
func TestAutofixOpenStartsNothingWhileThePollIsBehind(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(4322458360))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", "0 threads", "none") })
	h.tick() // reviewed
	h.pastSettle()
	h.gh.mu.Lock()
	h.gh.radarErr = errors.New("HTTP 502")
	h.gh.mu.Unlock()
	h.advance(2 * time.Minute)
	if err := h.e.Tick(h.ctx); err == nil {
		t.Fatal("the tick's poll did not fail")
	}
	h.settle()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Errorf("fix runs = %d while the poll fails", n)
	}
	h.wantOpen(OpenUnknown, "the poll of GitHub is behind")
	h.gh.mu.Lock()
	h.gh.radarErr = nil
	h.gh.mu.Unlock()
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		t.Errorf("fix runs = %d once the poll answered, want the loop", n)
	}
}

// A start the fix job refuses after its own read of GitHub (here the read
// fails) is not tried at every tick: the verdict waits with the refusal, and
// the look asks again 5 minutes later.
func TestARefusedStartIsTriedAgainAfterFiveMinutes(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(4322458360))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", "0 threads", "none") })
	h.tick() // reviewed
	h.tick() // the look reads the thread; hands off still
	h.advance(26 * time.Minute)
	h.gh.mu.Lock()
	h.gh.threadsErr = errors.New("HTTP 502")
	h.gh.mu.Unlock()
	h.tick() // prepareFix reads again, and fails
	h.wantOpen(OpenWaits, "could not be read")
	reads := h.gh.count("threads:talkable/talkable#2")
	h.advance(time.Minute)
	h.tick()
	if n := h.gh.count("threads:talkable/talkable#2") - reads; n != 0 {
		t.Errorf("the refused start was tried again a minute later (%d reads)", n)
	}
	h.wantOpen(OpenWaits, "could not be read")
	h.gh.mu.Lock()
	h.gh.threadsErr = nil
	h.gh.mu.Unlock()
	h.advance(5 * time.Minute)
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		t.Errorf("fix runs = %d five minutes later, want the loop", n)
	}
}

// A start whose own look at the base failed (its fetch failed once) is no
// disagreement between the look and the fix job: the conflict the look
// found is unknown to the job, not gone. The verdict waits with why, and
// the look asks again 5 minutes later, when the loop starts for the
// conflict; it never needs you until someone touches the PR.
func TestAConflictStartWhoseBaseFetchFailedOnceIsAskedAgain(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	git.conflict("app/models/offer.rb")
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", "0 threads", "none") })
	h.tick() // reviewed
	h.tick() // the look checks the base and finds the conflict; hands off still
	if l := h.ledgerOf(2); len(l.Conflict.Paths) == 0 {
		t.Fatalf("conflict = %+v, want the look's check to find it", l.Conflict)
	}
	h.advance(26 * time.Minute) // past the hands-off time, before the look's next base check
	git.mu.Lock()
	git.fetchErr = errors.New("gitx: fetch master: exit 128: fatal: unable to access the remote")
	git.mu.Unlock()
	h.tick() // the job fetches the base itself, and fails
	h.wantOpen(OpenWaits, "its merge with master could not be checked")
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Fatalf("fix runs = %d after the failed fetch", n)
	}
	git.mu.Lock()
	git.fetchErr = nil
	git.mu.Unlock()
	h.advance(5 * time.Minute)
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		t.Errorf("fix runs = %d five minutes later, want the loop for the conflict", n)
	}
}

// The look reads the loops that ended oldest first, so the ledger's
// bounded lists keep the newest: after more fix.pushed events than it
// keeps heads, and more fix.start events than it keeps reviews, the
// fixer's newest head (the PR's head after its last push) and the newest
// review a loop worked on are in the ledger, the oldest are not.
func TestTheLookKeepsTheNewestFixerHeadsAndReviews(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.tick() // reviewed
	h.tick() // the look's first look: its ledger
	h.advance(time.Minute)
	pr := h.pr(2)
	subject := "pr:talkable/talkable#2"
	for i := range openReviewsMax + 1 {
		h.e.event(h.ctx, "info", subject, "fix.start", "claude-fix starts", map[string]any{"review_run": fmt.Sprintf("run-old-%02d", i)})
	}
	for i := range openFixerHeadsMax + 2 {
		h.e.event(h.ctx, "info", subject, "fix.pushed", "claude-fix pushed", map[string]any{"head": strings.Repeat(fmt.Sprintf("%x", i), 40)})
	}
	h.e.noteLoopRan(pr.ID) // the look reads the PR's events again
	h.tick()
	l := h.ledgerOf(2)
	newest, oldest := strings.Repeat(fmt.Sprintf("%x", openFixerHeadsMax+1), 40), strings.Repeat("0", 40)
	if !slices.Contains(l.FixerHeads, newest) || slices.Contains(l.FixerHeads, oldest) {
		t.Errorf("fixer heads = %v, want the newest %d with %s, without %s", l.FixerHeads, openFixerHeadsMax, newest[:7], oldest[:7])
	}
	if last := fmt.Sprintf("review:run-old-%02d", openReviewsMax); !slices.Contains(l.Reviews, last) || slices.Contains(l.Reviews, "review:run-old-00") {
		t.Errorf("reviews = %v, want the newest %d with %s", l.Reviews, openReviewsMax, last)
	}
}

// The look tracks a PR's loops from its first look at the PR: a loop that
// ended before (an f while autofix was "never") hands nothing. The thread
// its fixer left unanswered is then work for a loop of autofix's own, not
// a leftover that needs you on the first tick after the switch.
func TestALoopThatEndedBeforeTheLooksFirstLookHandsNothing(t *testing.T) {
	h, git := autofixHarness(t, "zhuravel", findings(map[string]int{}, "COMMENTED"), func(h *harness) { h.cfg.Pipeline.Autofix = config.AutofixNever })
	h.setThreads(h.colleagueThread(4322458360))
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
	})
	h.tick() // reviewed
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id) // the fixer had the thread, and left it unanswered
	h.advance(time.Minute)
	h.cfg.Pipeline.Autofix = config.AutofixOpen
	h.tick() // the look's first look
	h.tick()
	if l := h.ledgerOf(2); len(l.Handed) > 0 || len(l.Reviews) > 0 {
		t.Errorf("ledger = %+v: a loop before the first look handed its keys", l)
	}
	h.wantState(2, store.PRReviewed)
	h.pastSettle()
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 2 || h.fixStartBy() != autofixBy {
		t.Errorf("fix runs = %d, the last by %q; want autofix's loop for the thread", n, h.fixStartBy())
	}
}

// simplifiedReReview runs, under "open", a loop on a review with a P2
// whose fixer pushes fixPushed, and whose re-review posts only a
// simplification on the fixer's change; a later cycle's fixer answers and
// pushes nothing. mods run before the engine is built.
func simplifiedReReview(t *testing.T, mods ...func(*harness)) *harness {
	t.Helper()
	h, git := openHarness(t, func(in pipeline.RoundInput) (map[string]int, string) {
		if in.TargetSHA == fixPushed {
			return map[string]int{}, "COMMENTED"
		}
		return map[string]int{"P2": 1}, "COMMENTED"
	}, mods...)
	h.rd.mu.Lock()
	inner := h.rd.script
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		res, err := inner(in)
		if err != nil || in.TargetSHA != fixPushed {
			return res, err
		}
		if _, err := h.st.DB().ExecContext(h.ctx, `UPDATE runs SET result_json = ? WHERE pr_id = ? AND target_sha = ? AND outcome = 'posted'`,
			`{"event":"COMMENTED","findings":{},"candidates":{"claude-simplify":{"suggested":1}}}`, in.PR.ID, fixPushed); err != nil {
			return failRound(h.t, err)
		}
		return res, nil
	}
	h.rd.mu.Unlock()
	var mu sync.Mutex
	runs := 0
	fixerBy(h, git, func(run store.Run) (string, string) {
		mu.Lock()
		defer mu.Unlock()
		if runs++; runs == 1 {
			return fixPushed, fixReport(run.ID, "b2b2b2b Fix the total (review comment 4100000001)", fixReplied, "none")
		}
		return "", fixReport(run.ID, "nothing", fixReplied, "none")
	})
	h.tick() // reviewed: a P2
	h.pastSettle()
	h.tick() // the look starts the loop; the fixer pushes, and the loop forces its re-review
	for range 4 {
		h.tick() // the re-review: a simplification
		h.advance(10 * time.Minute)
	}
	return h
}

// Under "open" a loop goes on while its re-review leaves optional work
// (here only a simplification on the fixer's own change), as rule A1 makes
// it work for a start: the next cycle of the same loop fixes it, where an
// end as done would have the look start a new loop on that re-review,
// spending one of autofix_max_loops and resetting the cycle count.
func TestAnOpenLoopGoesOnForASimplificationItsReReviewLeft(t *testing.T) {
	h := simplifiedReReview(t)
	var cycles []float64
	for _, ev := range h.fixEvents("fix.start") {
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		c, _ := data["cycle"].(float64)
		cycles = append(cycles, c)
	}
	if !slices.Equal(cycles, []float64{1, 2}) {
		t.Errorf("fix.start cycles = %v, want the loop's two cycles", cycles)
	}
	if n := len(h.fixEvents("autofix.open")); n != 1 {
		t.Errorf("autofix started %d loops, want the one", n)
	}
}

// At its cap, a loop under "open" whose re-review left only optional work
// needs you, and says what is left in those words.
func TestAnOpenLoopAtItsCapSaysTheOptionalWorkLeft(t *testing.T) {
	h := simplifiedReReview(t, func(h *harness) { h.cfg.Pipeline.FixCycles = 1 })
	ev, ok := h.fixEvent("fix.capped")
	if !ok || !strings.Contains(ev.Message, "1 fix cycle, 1 optional finding left") {
		t.Errorf("fix.capped = %q, %v; want the optional finding left", ev.Message, ok)
	}
	if n := len(h.fixEvents("autofix.open")); n != 1 {
		t.Errorf("autofix started %d loops, want the one", n)
	}
}

// Under "own" a loop still ends as done on a re-review with only optional
// work: the rule of "open" leaves the other values as they were.
func TestAnOwnLoopStillEndsOnASimplificationOnlyReReview(t *testing.T) {
	h := simplifiedReReview(t, func(h *harness) { h.cfg.Pipeline.Autofix = config.AutofixOwn })
	if _, ok := h.fixEvent("fix.done"); !ok {
		t.Error("no fix.done: the loop went on for the simplification")
	}
	if n := len(h.fixEvents("fix.start")); n != 1 {
		t.Errorf("fix.start events = %d, want the one cycle", n)
	}
}

// One failed poll of GitHub (an HTTP 502 in GitHub's bad hour) is not a
// poll behind: the tick before answered, so the PR's row is a tick old at
// most, though ticks start a poll interval plus a tick's length apart. The
// verdict stays as it was; a second failed poll in a row makes it unknown.
func TestOneFailedPollIsNotAPollBehind(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.tick()
	h.pastSettle()
	h.tick()
	h.wantOpen(OpenIdle, "nothing open")
	h.gh.mu.Lock()
	h.gh.radarErr = errors.New("HTTP 502")
	h.gh.mu.Unlock()
	failedTick := func() {
		h.t.Helper()
		if err := h.e.Tick(h.ctx); err == nil {
			h.t.Fatal("the tick's poll did not fail")
		}
		h.settle()
	}
	h.advance(62 * time.Second) // a 20-second tick, the 30-second wait, 12 seconds into this one
	failedTick()
	h.wantOpen(OpenIdle, "nothing open")
	h.advance(50 * time.Second)
	failedTick()
	h.wantOpen(OpenUnknown, "the poll of GitHub is behind")
}

// A loop whose push and end the look never saw in phase reviewing (it ran
// before the look tracked the PR, as around a restart) still makes its
// pushed head the fixer's: the look reads it from the loop's fix.pushed
// event, so no hands-off wait follows it, and a loop starts on the new
// work once the reviewers settle.
func TestAPushTheLookNeverSawIsStillTheFixers(t *testing.T) {
	h, git := autofixHarness(t, "zhuravel", func(in pipeline.RoundInput) (map[string]int, string) {
		if in.TargetSHA == fixPushed {
			return map[string]int{}, "COMMENTED"
		}
		return map[string]int{"P2": 1}, "COMMENTED"
	}, func(h *harness) { h.cfg.Pipeline.Autofix = config.AutofixNever })
	h.tick() // reviewed: a P2
	pushingFixer(h, git, fixPushed)
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"}) // the re-review is clean: the loop ends
	if _, ok := h.fixStateOf(2); ok {
		t.Fatal("the loop goes on after a clean re-review")
	}
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", "0 threads", "none") })
	h.setThreads(h.colleagueThread(4322458360))
	h.cfg.Pipeline.Autofix = config.AutofixOpen
	h.advance(time.Minute)
	h.tick() // the look's first look
	if l := h.ledgerOf(2); !slices.Contains(l.FixerHeads, fixPushed) {
		t.Errorf("fixer heads = %v, want %s from the loop's fix.pushed", l.FixerHeads, fixPushed)
	}
	h.advance(5 * time.Minute) // past the settle, within hands_off_after_push of the fixer's push
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 2 || h.fixStartBy() != autofixBy {
		v, _ := h.openOf(2)
		t.Errorf("fix runs = %d, the last by %q (verdict %s: %s); want autofix's loop with no hands-off wait", n, h.fixStartBy(), v.Verdict, v.Why)
	}
}

// A mute stops every automatic round, autofix = "open"'s loops included:
// the look leaves a muted own PR alone (off, muted) and starts no loop on
// a colleague's thread there. Before, it started one, and each push of its
// fixer forced a re-review of a PR the operator had switched off.
func TestAutofixOpenLeavesAMutedPRAlone(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(4322458360))
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
	})
	h.tick() // reviewed
	if err := h.st.UpdatePR(h.ctx, h.pr(2).ID, func(u *store.PRUpdate) { u.Set("muted", true) }); err != nil {
		t.Fatal(err)
	}
	h.pastSettle()
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Errorf("fix runs = %d: autofix started a loop on a muted PR", n)
	}
	if v := h.wantOpen(OpenOff, "muted"); v.Off != OpenOffMuted {
		t.Errorf("off = %q, want %q", v.Off, OpenOffMuted)
	}
}

// The look decides on the PR rows it listed at the start of its step, and
// a loop's end on another goroutine can move a PR to needs attention after
// that: the start reads the PR again and waits for the next look when it
// changed, so no autofix loop starts over the attention. Before, the stale
// row started one, whose end later replaced the attention's message.
func TestTheLookStartsNothingOnAPRThatChangedDuringIt(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(4322458360))
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
	})
	h.tick() // reviewed
	h.pastSettle()
	var once sync.Once
	h.gh.mu.Lock()
	h.gh.onThreads = func() { // during the look's own read of GitHub, after it listed the PRs
		once.Do(func() {
			pr := h.pr(2)
			if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRReviewed}, store.PRNeedsAttention, func(u *store.PRUpdate) {
				u.Set("last_error", "fix: fix run failed: the push was refused")
			}); err != nil {
				t.Errorf("transition: %v", err)
			}
			h.e.setKV(h.ctx, KVPRAttention(pr.ID), attention.KindFix)
		})
	}
	h.gh.mu.Unlock()
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Errorf("fix runs = %d: autofix started a loop on a PR that needs attention", n)
	}
	if pr := h.pr(2); pr.State != store.PRNeedsAttention || deref(pr.LastError) != "fix: fix run failed: the push was refused" {
		t.Errorf("state %s, last_error %q: want the attention as it was raised", pr.State, deref(pr.LastError))
	}
	h.gh.mu.Lock()
	h.gh.onThreads = nil
	h.gh.mu.Unlock()
	h.advance(time.Minute)
	h.tick()
	h.wantOpen(OpenNeedsYou, "needs your attention")
}

// On a head older than settle_max the look still waits for the reviewers
// to go quiet: a colleague's thread on a head two hours old starts no loop
// while the colleague may still be at work, and starts one once
// settle_quiet passed. Before, the cap counted from the head's move had
// passed long ago, and the first comment of several started a loop at once.
func TestAColleaguesThreadOnAnOldHeadWaitsForQuiet(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
	})
	h.tick() // reviewed
	h.advance(2 * time.Hour)
	h.tick()
	h.wantOpen(OpenIdle, "nothing open")
	h.setThreads(h.colleagueThread(4322458360))
	now := h.clock.Now()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", updated: now, activity: now})
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Fatalf("fix runs = %d a tick after the colleague's comment, want none before settle_quiet", n)
	}
	h.wantOpen(OpenWaits, "settling")
	h.advance(4 * time.Minute)
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		v, _ := h.openOf(2)
		t.Errorf("fix runs = %d once quiet (verdict %s: %s), want the loop", n, v.Verdict, v.Why)
	}
}

// A failed fetch of the base leaves the head's merge unknown, never idle,
// and the look checks again 5 minutes later, as a failed read is tried
// again, not 30 minutes later. Before, the failed check counted as no
// conflict: an approved PR read idle, merge yours, while its base had moved
// under it.
func TestAFailedBaseCheckIsNeverIdleAndIsTriedAgainSoon(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.tick() // reviewed
	git.mu.Lock()
	git.fetchErr = errors.New("gitx: fetch master: exit 128: fatal: unable to access the remote")
	git.mu.Unlock()
	h.pastSettle()
	h.tick()
	h.wantOpen(OpenUnknown, "the merge with master could not be checked")
	git.mu.Lock()
	git.fetchErr = nil
	git.mu.Unlock()
	h.advance(6 * time.Minute)
	h.tick()
	h.wantOpen(OpenIdle, "nothing open")
}

// colleagueFixer is the fake fixer that answers the colleague's thread of
// colleagueThread and pushes nothing.
func colleagueFixer(h *harness, git *fixGit) {
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
	})
}

// `magnum pause` holds the look's starts as it held "own"'s, whose loops
// began at the end of rounds the pause held: a PR with work waits and says
// so, spends none of its daily starts and needs nobody. The first look
// after `magnum resume` starts the loop, with nothing new on GitHub.
func TestAPausedLookStartsNothingUntilThePauseEnds(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(4322458360))
	colleagueFixer(h, git)
	h.tick() // reviewed
	h.e.setKV(h.ctx, store.KVDaemonPaused, "1")
	h.pastSettle()
	h.tick()
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Fatalf("fix runs = %d under the pause, want none", n)
	}
	if v := h.wantOpen(OpenWaits, "daemon paused: 1 thread of a person waits for `magnum resume`"); len(v.Needs) != 0 {
		t.Errorf("needs = %+v under the pause, want none", v.Needs)
	}
	if l := h.ledgerOf(2); len(l.Starts) != 0 {
		t.Errorf("starts = %v under the pause, want none spent", l.Starts)
	}
	h.wantState(2, store.PRReviewed)
	h.e.delKV(h.ctx, store.KVDaemonPaused)
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 || h.fixStartBy() != autofixBy {
		t.Fatalf("fix runs = %d by %q after the pause, want autofix's loop", n, h.fixStartBy())
	}
}

// [daemon] quiet_hours hold the look's starts until they end, as they hold
// a full round: the verdict names the window's end, and the first look
// after it starts the loop.
func TestQuietHoursHoldTheLooksStartsUntilTheirEnd(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(4322458360))
	colleagueFixer(h, git)
	h.tick() // reviewed
	h.pastSettle()
	now := h.clock.Now()
	end := now.Add(2 * time.Hour).Truncate(time.Minute)
	h.cfg.Daemon.QuietHours = now.Add(-time.Hour).Format("15:04") + "-" + end.Format("15:04")
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Fatalf("fix runs = %d inside quiet hours, want none", n)
	}
	if v := h.wantOpen(OpenWaits, "quiet hours ("+h.cfg.Daemon.QuietHours+"): 1 thread of a person waits until "+end.Format("15:04")); !v.Until.Equal(end) {
		t.Errorf("until = %s, want the window's end %s", v.Until, end)
	}
	if l := h.ledgerOf(2); len(l.Starts) != 0 {
		t.Errorf("starts = %v inside quiet hours, want none spent", l.Starts)
	}
	h.clock.Set(end)
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 || h.fixStartBy() != autofixBy {
		t.Fatalf("fix runs = %d by %q after quiet hours, want autofix's loop", n, h.fixStartBy())
	}
}

// A loop you ask for runs under the pause: `magnum pause` holds what
// starts by itself, never `f`.
func TestFStartsALoopUnderThePause(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(4322458360))
	colleagueFixer(h, git)
	h.tick() // reviewed
	h.e.setKV(h.ctx, store.KVDaemonPaused, "1")
	h.pastSettle()
	id := h.fix(FixPayload{})
	h.tick()
	h.wantDone(id)
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 || h.fixStartBy() != fixBy {
		t.Fatalf("fix runs = %d by %q under the pause, want the loop f asked for", n, h.fixStartBy())
	}
}

// A colleague's approval of a PR whose magnum review is clean starts no
// loop: before, its body ("No blocking items") was a person's remark, the
// loop it started answered nothing, as the skill says, and the PR then
// needed you for the approval. The PR is idle, its approval said.
func TestAnApprovalOnACleanReviewStartsNoLoop(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	colleagueFixer(h, git)
	h.tick() // reviewed clean
	h.gh.mu.Lock()
	if h.gh.allReviews == nil {
		h.gh.allReviews = map[int][]github.Review{}
	}
	h.gh.allReviews[2] = append(h.gh.allReviews[2], github.Review{DatabaseID: 9100, State: "APPROVED", Body: "Reviewed a1a1a1a. No blocking items.",
		URL: prPage + "#pullrequestreview-9100", SubmittedAt: h.clock.Now(), AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"})
	h.gh.mu.Unlock()
	h.pastSettle()
	h.tick()
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Fatalf("fix runs = %d for an approval, want none", n)
	}
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
}

// A loop the look started that ended unclaimed because a round took the
// free slot first started no fixer: it spends none of the PR's daily
// starts. With autofix_max_loops = 1 the next look starts the loop again,
// where before the PR needed you for "1 loop today".
func TestAStartThatFoundNoSlotSpendsNoStart(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"), func(h *harness) { h.cfg.Pipeline.AutofixMaxLoops = 1 })
	h.setThreads(h.colleagueThread(4322458360))
	colleagueFixer(h, git)
	h.tick() // reviewed in review1
	handBack(h)
	h.sl.reserveErr = fmt.Errorf("slots: reserve for PR #2 in talkable/talkable: %w", slots.ErrNoFreeSlot) // a round takes it first
	h.pastSettle()
	h.tick()
	if ev, ok := h.fixEvent("fix.ended"); !ok || !strings.Contains(ev.Message, "no free talkable/talkable slot") {
		t.Fatalf("fix.ended = %q, %v; want the loop ended for want of a slot", ev.Message, ok)
	}
	h.sl.reserveErr = nil
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		v, _ := h.openOf(2)
		t.Fatalf("fix runs = %d (verdict %s %q), want the loop started again: the start that found no slot is not counted", n, v.Verdict, v.Why)
	}
	if l := h.ledgerOf(2); len(l.Starts) != 1 {
		t.Errorf("starts = %v, want the one loop that ran", l.Starts)
	}
}

// A later cycle of a loop the look started that finds no slot (the PR's
// idle slot went to another PR while the cycle waited, and a round took
// the free one first) ends unclaimed, but its loop ran a fixer in cycle 1:
// the loop's start stays one of the PR's daily starts.
func TestALaterCycleThatFoundNoSlotKeepsItsLoopsStart(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(4322458360))
	colleagueFixer(h, git)
	h.tick() // reviewed
	h.pastSettle()
	h.tick() // the look starts a loop, whose fixer answers the thread
	l := h.ledgerOf(2)
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 || len(l.Starts) != 1 {
		t.Fatalf("fix runs %d, starts %v; want the loop the look started", n, l.Starts)
	}
	pr := h.pr(2)
	repo, err := h.st.RepoByID(h.ctx, pr.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	job := &fixJob{repo: repo, watch: h.cfg.Watches[0], pr: pr, state: FixState{Cycle: 2, Cap: 3, By: autofixBy, StartedAt: l.Starts[0]}}
	h.e.endFixUnclaimed(h.ctx, job, noFreeFixSlot{repo: "talkable/talkable", label: "talkable/talkable#2"}.Error(), true)
	h.e.noteLoopRan(pr.ID)
	if err := h.e.autofixOpen(h.ctx); err != nil {
		t.Fatal(err)
	}
	if l := h.ledgerOf(2); len(l.Starts) != 1 {
		t.Errorf("starts = %v after cycle 2 found no slot, want the loop's start kept", l.Starts)
	}
}

// Two PRs without a slot and one free slot: the look starts one loop in
// the tick, and the other PR waits for a slot, saying so, without a loop
// that would lose the race for it ("fix not started") and without spending
// a start. The first loop's claim is still on its way while the look goes
// on (a goroutine of its own), so the pool still lists the slot free.
func TestTheLookStartsNoMoreSlotLoopsInATickThanSlotsAreFree(t *testing.T) {
	h, git := openOwnPRs(t, 2, 3)
	colleagueFixer(h, git)
	h.gh.mu.Lock()
	h.gh.threads = map[int][]github.Thread{2: {h.colleagueThread(4322458360)}, 3: {h.colleagueThread(4322458370)}}
	h.gh.mu.Unlock()
	h.pastSettle()
	gate := make(chan struct{})
	h.sl.mu.Lock()
	h.sl.reserveGate = gate
	h.sl.mu.Unlock()
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	waits := 0
	for n := 2; n <= 3; n++ {
		if v, _ := h.openOf(n); v.Verdict == OpenWaits && strings.Contains(v.Why, "waits for a free talkable/talkable slot") {
			waits++
		}
	}
	close(gate)
	h.settle()
	if waits != 1 {
		t.Errorf("%d PRs wait for a slot while the first claim is on its way, want one", waits)
	}
	started, waiting := 0, 0
	for n := 2; n <= 3; n++ {
		v, _ := h.openOf(n)
		switch {
		case len(h.fixRuns(h.pr(n).ID)) == 1:
			started++
		case v.Verdict == OpenWaits && strings.Contains(v.Why, "waits for a free talkable/talkable slot"):
			waiting++
			if l := h.ledgerOf(n); len(l.Starts) != 0 {
				t.Errorf("PR #%d waits for a slot with starts %v, want none spent", n, l.Starts)
			}
		default:
			t.Errorf("PR #%d: verdict %s %q", n, v.Verdict, v.Why)
		}
	}
	if started != 1 || waiting != 1 {
		t.Errorf("started %d, waiting %d; want one loop and one PR waiting for a slot", started, waiting)
	}
	if ev, ok := h.fixEventOf(3, "fix.ended"); ok {
		t.Errorf("fix.ended on #3 = %q, want no loop that lost the slot", ev.Message)
	}
	if ev, ok := h.fixEventOf(2, "fix.ended"); ok && strings.Contains(ev.Message, "no free") {
		t.Errorf("fix.ended on #2 = %q, want no loop that lost the slot", ev.Message)
	}
}

// The look's check against this tick's earlier starts runs only once one
// of them took a slot: a PR without a slot in a full pool meets the fix
// job's own refusals first, as before. Under a drain the verdict says the
// drain, and nothing is evicted or provisioned for a loop that cannot start
// before the restart.
func TestAFullPoolLeavesTheFixJobsRefusalsFirst(t *testing.T) {
	h, git := openOwnPRs(t, 2, 3)
	colleagueFixer(h, git)
	h.gh.mu.Lock()
	h.gh.threads = map[int][]github.Thread{2: {h.colleagueThread(4322458360)}}
	h.gh.mu.Unlock()
	sl := h.slot("review1") // #3 holds the pool's one slot, evictable
	if err := h.st.TransitionSlot(h.ctx, sl.ID, nil, store.SlotHeld, func(u *store.SlotUpdate) {
		u.Set("pr_id", h.pr(3).ID)
		u.Set("last_used_at", store.FormatTime(h.clock.Now().AddDate(0, 0, -3)))
	}); err != nil {
		t.Fatal(err)
	}
	h.e.setKV(h.ctx, KVDaemonDraining, "1")
	h.pastSettle()
	h.tick()
	if v, _ := h.openOf(2); v.Verdict != OpenWaits || !strings.Contains(v.Why, "the daemon is draining") {
		t.Errorf("verdict = %s %q, want a wait for the drain", v.Verdict, v.Why)
	}
	for _, sub := range []string{"slot:review1", "pool:talkable/talkable"} {
		for _, ev := range subjectEvents(t, h, sub) {
			if ev.Kind == "slot.evict" || ev.Kind == "pool.provision" {
				t.Errorf("%s: %s %q under the drain, want none", sub, ev.Kind, ev.Message)
			}
		}
	}
}

// fixEventOf is PR #n's newest event of kind (ok false: none).
func (h *harness) fixEventOf(n int, kind string) (store.Event, bool) {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#"+itoa(int64(n)), 0)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, ev := range slices.Backward(evs) {
		if ev.Kind == kind {
			return ev, true
		}
	}
	return store.Event{}, false
}
