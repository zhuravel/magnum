package engine

import (
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
