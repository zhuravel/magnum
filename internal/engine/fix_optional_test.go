package engine

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// optionalThread is magnum's thread on PR #2 whose P3's Fix ends "This is
// the author's call.".
const optionalThread = 4302866759

// optionalEntry is the fixer's "## Optional calls" entry for it.
const optionalEntry = "4302866759 P3 app/models/offer.rb:42: option A keeps the cap, option B lifts it"

// optionalP3 is magnum's comment of that P3: its Fix names the options
// and ends with the author's call.
const optionalP3 = "**[P3] The cap stays at 10 (optional, no reply needed)**\n\nA second coupon is refused.\n\n" +
	"**Fix**: option A keeps the cap, option B lifts it. This is the author's call."

// writeOptionalReview writes, beside the fix report at report, the files
// of the review whose P3 at app/models/offer.rb:42 has body (its threads
// file names optionalThread, magnum's).
func writeOptionalReview(t *testing.T, report, body string) {
	t.Helper()
	writeFixReviewFiles(t, report, []int64{optionalThread}, []fixReviewComment{{Path: "app/models/offer.rb", Line: 42, Body: body}})
}

// withOptional is a fix report with an "## Optional calls" section of
// entries after its Needs you.
func withOptional(report string, entries ...string) string {
	body := "none"
	if len(entries) > 0 {
		body = "- " + strings.Join(entries, "\n- ")
	}
	return report + "## Optional calls\n" + body + "\n"
}

// A P3 whose Fix ends "This is the author's call." held an own PR for the
// operator (2026-11-07: an optional P3 kept it in Needs you for a day, and
// R47's CI start waited behind it). The fixer now leaves it as it is, with
// no reply, under "## Optional calls": the settle raises no attention for
// it, the loop ends without the operator, and the PR keeps the call as the
// operator's (its thread and link) for the look and the merge's words, with
// a fix.optional event. A P0 to P2 there, or an entry magnum cannot read,
// still holds as Needs you does: only a P3 is optional.
func TestAnOptionalCallHoldsNothing(t *testing.T) {
	t.Run("a P3", func(t *testing.T) {
		h, git := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{"P3": 1})
		fixer(h, git, "", func(run store.Run) string {
			writeOptionalReview(h.t, deref(run.ReportPath), optionalP3)
			return withOptional(fixReport(run.ID, "nothing", noReplies, "none"), optionalEntry)
		})
		h.fix(FixPayload{})
		h.handle()
		if cur := h.wantState(2, store.PRReviewed); cur.LastError != nil {
			t.Errorf("last_error = %q, want none", deref(cur.LastError))
		}
		if _, ok := h.fixStateOf(2); ok {
			t.Error("the loop goes on")
		}
		if ev, ok := h.fixEvent("fix.needs_you"); ok {
			t.Errorf("fix.needs_you = %q, want none", ev.Message)
		}
		link := prPage + "#discussion_r4302866759"
		if ev, ok := h.fixEvent("fix.optional"); !ok || !strings.Contains(ev.Message, "1 optional call") || !strings.Contains(ev.Message, link) ||
			strings.Contains(ev.Message, "option A") {
			t.Errorf("fix.optional = %q, %v; want the call's link, never the fixer's words", ev.Message, ok)
		}
		if calls := h.e.optionalCalls(h.ctx, pr.ID); !slices.Equal(calls, []optionalCall{{Thread: optionalThread, URL: link, At: h.clock.Now().UTC()}}) {
			t.Errorf("the PR's optional calls = %+v, want the call made at the settle", calls)
		}
		h.flush()
		h.wantToasts("needs attention", 0)
	})
	t.Run("an earlier review's P3, as GitHub shows its thread", func(t *testing.T) {
		h, git := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
		th := magnumsThread(optionalThread, "app/models/offer.rb", 42, h.clock.Now().Add(-time.Hour), false)
		th.Comments[0].Body = optionalP3
		h.setThreads(th)
		fixer(h, git, "", func(run store.Run) string {
			writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{optionalThread}, nil)
			return withOptional(fixReport(run.ID, "nothing", noReplies, "none"), optionalEntry)
		})
		h.fix(FixPayload{})
		h.handle()
		if cur := h.wantState(2, store.PRReviewed); cur.LastError != nil {
			t.Errorf("last_error = %q, want none", deref(cur.LastError))
		}
		if calls := h.e.optionalCalls(h.ctx, pr.ID); len(calls) != 1 {
			t.Errorf("the PR's optional calls = %+v, want the call", calls)
		}
	})
	// The entry is the fixer's word: magnum's own comment says whether the
	// finding is a P3 whose Fix ends with the author's call. A P1, or a P3
	// with no such Fix, listed there all the same holds as Needs you does,
	// and so does one magnum cannot find.
	for name, body := range map[string]string{
		"a P1 listed as a P3":           strings.Replace(optionalP3, "**[P3]", "**[P1]", 1),
		"a P3 that is no author's call": strings.Replace(optionalP3, " This is the author's call.", "", 1),
		"no comment of magnum's":        "",
	} {
		t.Run(name+" holds", func(t *testing.T) {
			h, git := fixHarness(t)
			pr := fixablePR(h, "zhuravel", map[string]int{"P1": 1})
			fixer(h, git, "", func(run store.Run) string {
				if body == "" {
					writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{optionalThread}, nil)
				} else {
					writeOptionalReview(h.t, deref(run.ReportPath), body)
				}
				return withOptional(fixReport(run.ID, "nothing", noReplies, "none"), optionalEntry)
			})
			h.fix(FixPayload{})
			h.handle()
			cur := h.wantState(2, store.PRNeedsAttention)
			if msg := deref(cur.LastError); !strings.HasPrefix(msg, "fix: 1 finding needs your call: ") {
				t.Errorf("last_error = %q, want the entry to need you", msg)
			}
			if calls := h.e.optionalCalls(h.ctx, pr.ID); len(calls) != 0 {
				t.Errorf("the PR's optional calls = %+v, want none", calls)
			}
		})
	}
	t.Run("a P2 holds", func(t *testing.T) {
		h, git := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
		fixer(h, git, "", func(run store.Run) string {
			writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{optionalThread}, nil)
			return withOptional(fixReport(run.ID, "nothing", noReplies, "none"), strings.Replace(optionalEntry, " P3 ", " P2 ", 1), "my file.rb: spaces")
		})
		h.fix(FixPayload{})
		h.handle()
		cur := h.wantState(2, store.PRNeedsAttention)
		if msg := deref(cur.LastError); !strings.HasPrefix(msg, "fix: 2 findings need your call: ") {
			t.Errorf("last_error = %q, want the P2 and the unread entry to need you", msg)
		}
		if calls := h.e.optionalCalls(h.ctx, pr.ID); len(calls) != 0 {
			t.Errorf("the PR's optional calls = %+v, want none", calls)
		}
	})
}

// An optional call raises no attention, but it is still the operator's
// call: `magnum fix --decide` takes its thread (the PR's record of the
// optional calls dates it, as an attention dates the others), posts the
// choice there and starts the loop, whose fixer gets it as a decision.
func TestFixDecideTakesAnOptionalCall(t *testing.T) {
	h, git := fixHarness(t)
	h.gh.clock = h.clock.Now
	pr := fixablePR(h, "zhuravel", map[string]int{"P3": 1})
	h.setThreads(magnumsThread(optionalThread, "app/models/offer.rb", 42, h.clock.Now().Add(-time.Hour), false))
	var mu sync.Mutex
	runs := 0
	fixerBy(h, git, func(run store.Run) (string, string) {
		mu.Lock()
		runs++
		first := runs == 1
		mu.Unlock()
		if first {
			writeOptionalReview(h.t, deref(run.ReportPath), optionalP3)
			return "", withOptional(fixReport(run.ID, "nothing", noReplies, "none"), optionalEntry)
		}
		return "", fixReport(run.ID, "nothing", noReplies, "none")
	})
	h.fix(FixPayload{})
	h.handle()
	h.wantState(2, store.PRReviewed)
	h.advance(10 * time.Minute)

	id := h.fix(FixPayload{Decide: []FixDecision{{Thread: optionalThread, Text: "Take option A."}}})
	h.handle()
	if res := h.wantDone(id); !strings.Contains(res, "replied in 1 thread as zhuravel") {
		t.Errorf("answer = %q, want your reply posted and the loop started", res)
	}
	all := h.fixRuns(pr.ID)
	if len(all) != 2 {
		t.Fatalf("fix runs = %d, want the loop --decide started", len(all))
	}
	if x := readExternalOf(t, all[1]); len(x.Decisions) != 1 || x.Decisions[0].CommentID != optionalThread {
		t.Errorf("decisions = %+v, want your choice on the optional call", x.Decisions)
	}
}

// Under autofix = "open" a loop's re-review that leaves only optional work
// goes on to the next cycle (rule A1), but an optional call is the
// operator's, not the fixer's: a re-review whose one open P3 is the call
// the fixer just left ends the loop as done, with no cycle that would only
// list the call again, whatever you asked in its thread before the call.
// Another open P3 beside it is still work, and so is the call once your
// reply after it takes it.
func TestALoopEndsAsDoneWhenItsOptionalCallIsAllThatIsLeft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		open   int
		reply  bool          // your reply in the call's thread
		at     time.Duration // when, from the settle that takes the call
		cycles int
	}{
		{name: "the call alone", open: 1, cycles: 1},
		{name: "a question of yours before the call", open: 1, reply: true, at: -time.Minute, cycles: 1},
		{name: "your reply after the call", open: 1, reply: true, at: 30 * time.Second, cycles: 2},
		{name: "another P3 beside it", open: 2, cycles: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t, func(h *harness) { h.cfg.Pipeline.Autofix = config.AutofixOpen })
			pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1, "P3": 1})
			start := h.clock.Now()
			th := magnumsThread(optionalThread, "app/models/offer.rb", 42, start.Add(-time.Hour), false)
			if tc.reply {
				th = magnumsThread(optionalThread, "app/models/offer.rb", 42, start.Add(-time.Hour), false, "which one keeps the cap?")
				th.Comments[1].CreatedAt = start.Add(tc.at)
			}
			h.setThreads(th)
			fixer(h, git, fixPushed, func(run store.Run) string {
				writeOptionalReview(h.t, deref(run.ReportPath), optionalP3)
				return withOptional(fixReport(run.ID, "b2b2b2b Fix the total (review comment 4100000001)",
					"1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none"), optionalEntry)
			})
			h.fix(FixPayload{})
			h.handle()
			if calls := h.e.optionalCalls(h.ctx, pr.ID); len(calls) != 1 || !calls[0].At.Equal(start.UTC()) {
				t.Fatalf("the PR's optional calls = %+v, want the call taken at %v", calls, start)
			}
			reviewWithOpen(h, findings(map[string]int{}, "COMMENTED"), map[string]int{"P3": tc.open})
			h.advance(time.Minute)
			reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
			if n := len(h.fixRuns(pr.ID)); n != tc.cycles {
				t.Errorf("fix runs = %d, want %d", n, tc.cycles)
			}
			if tc.cycles == 1 {
				if _, ok := h.fixStateOf(2); ok {
					t.Error("the loop goes on over the operator's optional call")
				}
				if _, ok := h.fixEvent("fix.done"); !ok {
					t.Error("no fix.done")
				}
			}
		})
	}
}

// Under autofix = "open" the look started a loop on a review whose one
// finding was a P3 author's call, and after it the finding the fixer left
// without a word needed the operator. Now the fixer lists it as an optional
// call: the look starts no other loop on it, the PR goes idle, and its
// verdict names the call with its link, so the operator sees it at the
// merge.
func TestTheLookLeavesAnOptionalCallToTheMerge(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{"P3": 1}, "COMMENTED"))
	th := magnumsThread(optionalThread, "app/models/offer.rb", 42, h.clock.Now().Add(-time.Hour), false)
	th.Comments[0].ReviewID = 301 // the review the first round posts
	h.setThreads(th)
	fixer(h, git, "", func(run store.Run) string {
		writeOptionalReview(h.t, deref(run.ReportPath), optionalP3)
		return withOptional(fixReport(run.ID, "nothing", noReplies, "none"), optionalEntry)
	})
	h.tick() // reviewed
	h.pastSettle()
	h.tick() // the look starts the loop, whose fixer leaves the optional call
	pr := h.wantState(2, store.PRReviewed)
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs = %d, want the look's loop", n)
	}
	for range 3 {
		h.pastSettle()
		h.tick()
	}
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Errorf("fix runs = %d, want no loop on the operator's call", n)
	}
	v := h.wantOpen(OpenIdle, "1 optional call: "+prPage+"#discussion_r4302866759")
	if len(v.Optional) != 1 {
		t.Errorf("verdict's optional calls = %q", v.Optional)
	}
}

// The operator's optional calls are named at the merge while they wait. A
// reply of yours in a call's thread after the call takes it (it may be your
// decision), but one from before the call does not (a question that
// magnum's reply round answered before the fixer left the call). A later
// review that keeps no earlier P3 open no longer counts a call of an
// earlier review's (you took it with a push): it leaves the record, while
// one you answered stays there for its decision.
func TestAnOptionalCallWaitsUntilYouTakeIt(t *testing.T) {
	h, _ := fixHarness(t)
	const prID = 7
	callAt := itemsAt.Add(10 * time.Minute)
	calls := []optionalCall{{Thread: 5000, URL: prPage + "#discussion_r5000", At: callAt}, {Thread: 5001, URL: prPage + "#discussion_r5001", At: callAt}}
	h.e.setOptionalCalls(h.ctx, prID, calls)
	mine := magnumThreadsOf([]github.Thread{
		magnumThreadWith(5000, itemsAt, [2]string{"zhuravel", "which would you pick?"}, [2]string{"talkable", "option A"}),
		magnumThreadWith(5001, callAt, [2]string{"zhuravel", "take option B"}),
	}, []string{"talkable[bot]"}, "zhuravel")
	open, threads := h.e.openOptional(h.ctx, prID, mine, cleanSum())
	if !slices.Equal(open, calls[:1]) || !threads[5000] || threads[5001] {
		t.Errorf("open calls = %+v (%v), want 5000 alone: your reply before it takes nothing", open, threads)
	}
	if got := openOptionalIn(calls, mine); got != 1 {
		t.Errorf("a loop's count of the calls left open = %d, want 1", got)
	}
	later := cleanSum()
	later.RunID, later.ReviewID, later.OpenCounts = "r2", 102, &[4]int{0, 0, 0, 0}
	if open, _ := h.e.openOptional(h.ctx, prID, mine, later); len(open) != 0 {
		t.Errorf("open calls after a review that keeps no P3 open = %+v, want none", open)
	}
	if got := h.e.optionalCalls(h.ctx, prID); !slices.Equal(got, calls[1:]) {
		t.Errorf("the record = %+v, want the call you answered alone", got)
	}
}

// earlierP3Entry is the "## Needs you" entry a fixer wrote for magnum's P3
// author's call (optionalThread) before optional calls existed: not fixed,
// the author's call.
const earlierP3Entry = "- 4302866759 P3 app/models/offer.rb:42: not fixed. This is the author's call: option A keeps the cap, option B lifts it"

// earlierP3Call is the call that entry made: decide magnum's P3, which the
// fixer declined.
const earlierP3Call = "decide magnum's P3 on app/models/offer.rb:42: the fixer declined it; reply won't fix at " +
	prPage + "#discussion_r4302866759, or `magnum fix talkable/talkable#2` sends it back"

// heldByAnEarlierP3 is the live own PR of 2026-11-08 under autofix =
// "open": the review posted magnum's P3 author's call (optionalThread,
// beside others, more threads), the look's loop started on it, and its
// fixer, from before optional calls, left needs under "## Needs you"
// (earlierP3Entry among them), so the PR needs attention with the call to
// decide the P3. Each later run's fixer writes next's report.
func heldByAnEarlierP3(t *testing.T, needs string, next func(h *harness, run store.Run) string, others ...github.Thread) (*harness, store.PR) {
	t.Helper()
	h, git := openHarness(t, findings(map[string]int{"P3": 1}, "COMMENTED"))
	h.gh.clock = h.clock.Now
	th := magnumsThread(optionalThread, "app/models/offer.rb", 42, h.clock.Now().Add(-time.Hour), false)
	th.Comments[0].Body, th.Comments[0].ReviewID = optionalP3, 301 // the review the first round posts
	h.setThreads(append([]github.Thread{th}, others...)...)
	var mu sync.Mutex
	runs := 0
	fixerBy(h, git, func(run store.Run) (string, string) {
		mu.Lock()
		runs++
		first := runs == 1
		mu.Unlock()
		writeOptionalReview(h.t, deref(run.ReportPath), optionalP3)
		if first {
			return "", fixReport(run.ID, "nothing", noReplies, needs)
		}
		return "", next(h, run)
	})
	h.tick() // reviewed
	h.pastSettle()
	h.tick() // the look starts the loop, whose fixer leaves the P3 to you
	pr := h.wantState(2, store.PRNeedsAttention)
	if msg := deref(pr.LastError); !strings.Contains(msg, earlierP3Call) {
		t.Fatalf("last_error = %q, want the call to decide magnum's P3", msg)
	}
	h.advance(10 * time.Minute)
	return h, pr
}

// A PR held under needs_attention by an earlier fix run's call to decide
// magnum's P3 author's call stayed there (2026-11-08, live) after a manual
// `magnum fix` whose fixer listed that P3 as an optional call and left
// nothing else: the run recorded the call, which holds nothing, but the old
// attention stood until `magnum fix --done`. Now a run with no Needs you
// whose optional calls cover every call of the earlier attention (the same
// threads) lifts it as its own result: the PR is back to reviewed, a
// fix.optional_lifted event says so, and the look names the call at the
// merge. An attention with a call the run did not cover stays as it is, and
// a decision of yours in the P3's thread is no optional call: it goes back
// to you unless the fixer builds it.
func TestAnOptionalCallLiftsTheEarlierAttentionOfItsP3(t *testing.T) {
	optionalOnly := func(_ *harness, run store.Run) string {
		return withOptional(fixReport(run.ID, "nothing", noReplies, "none"), optionalEntry)
	}
	t.Run("every call covered", func(t *testing.T) {
		h, pr := heldByAnEarlierP3(t, earlierP3Entry, optionalOnly)
		h.fix(FixPayload{})
		h.handle()
		cur := h.wantState(2, store.PRReviewed)
		if cur.LastError != nil {
			t.Errorf("last_error = %q after the lift", deref(cur.LastError))
		}
		for _, key := range []string{KVPRAttention(pr.ID), kvPRFixCalls(pr.ID)} {
			if v, ok := h.e.getKV(h.ctx, key); ok {
				t.Errorf("%s = %q outlived the lift", key, v)
			}
		}
		link := prPage + "#discussion_r4302866759"
		if ev, ok := h.fixEvent("fix.optional_lifted"); !ok || !strings.Contains(ev.Message, "1 call of the attention as an optional call") ||
			strings.Contains(ev.Message, "option A") {
			t.Errorf("fix.optional_lifted = %q, %v; want the lift, never the fixer's words", ev.Message, ok)
		}
		if ev, ok := h.fixEvent("fix.decisions_built"); ok {
			t.Errorf("fix.decisions_built = %q: no decision was built", ev.Message)
		}
		if calls := h.e.optionalCalls(h.ctx, pr.ID); len(calls) != 1 || calls[0].Thread != optionalThread {
			t.Errorf("the PR's optional calls = %+v, want the P3's", calls)
		}
		for range 3 {
			h.pastSettle()
			h.tick()
		}
		if n := len(h.fixRuns(pr.ID)); n != 2 {
			t.Errorf("fix runs = %d, want no loop on the operator's call", n)
		}
		h.wantOpen(OpenIdle, "1 optional call: "+link)
	})
	t.Run("a call not covered stays", func(t *testing.T) {
		h, _ := heldByAnEarlierP3(t, earlierP3Entry+"\n"+leftDecision, optionalOnly, extThread(decideThread, "lib/sandbox.rb", 185,
			time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC), "MINOR: `run` drops stderr", "rev-ann"))
		before := deref(h.pr(2).LastError)
		if !strings.Contains(before, decideCall) {
			t.Fatalf("last_error = %q, want rev-ann's call beside the P3's", before)
		}
		h.fix(FixPayload{})
		h.handle()
		if msg := deref(h.wantState(2, store.PRNeedsAttention).LastError); msg != before {
			t.Errorf("last_error = %q, want the attention as it was", msg)
		}
		if ev, ok := h.fixEvent("fix.optional_lifted"); ok {
			t.Errorf("fix.optional_lifted = %q over a call the run did not cover", ev.Message)
		}
	})
	// A run that could not read the threads wrote no external file, so it
	// could not tell whether you decided in the P3's thread: no lift.
	t.Run("no lift without the threads read", func(t *testing.T) {
		h, _ := heldByAnEarlierP3(t, earlierP3Entry, optionalOnly)
		before := deref(h.pr(2).LastError)
		h.gh.mu.Lock()
		h.gh.threadsErr = errors.New("HTTP 502")
		h.gh.mu.Unlock()
		h.fix(FixPayload{})
		h.handle()
		if msg := deref(h.wantState(2, store.PRNeedsAttention).LastError); msg != before {
			t.Errorf("last_error = %q, want the attention as it was", msg)
		}
		if n := len(h.fixRuns(h.pr(2).ID)); n != 2 {
			t.Fatalf("fix runs = %d, want the manual run", n)
		}
		if ev, ok := h.fixEvent("fix.optional_lifted"); ok {
			t.Errorf("fix.optional_lifted = %q without the threads read", ev.Message)
		}
	})
	t.Run("your decision is no optional call", func(t *testing.T) {
		h, _ := heldByAnEarlierP3(t, earlierP3Entry, optionalOnly)
		h.gh.mu.Lock()
		h.gh.threads[2][0] = authorSays(h.gh.threads[2][0], 4302866999, "Take option A.", h.clock.Now())
		h.gh.mu.Unlock()
		h.fix(FixPayload{})
		h.handle()
		cur := h.wantState(2, store.PRNeedsAttention)
		if msg := deref(cur.LastError); !strings.Contains(msg, "decide magnum's finding on app/models/offer.rb:42: ") {
			t.Errorf("last_error = %q, want your decision back with you", msg)
		}
		if ev, ok := h.fixEvent("fix.decisions"); !ok || !strings.Contains(ev.Message, "1 decision goes back to you") {
			t.Errorf("fix.decisions = %q, %v", ev.Message, ok)
		}
		if ev, ok := h.fixEvent("fix.optional_lifted"); ok {
			t.Errorf("fix.optional_lifted = %q over your decision", ev.Message)
		}
	})
}
