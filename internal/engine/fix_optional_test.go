package engine

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// optionalThread is magnum's thread on PR #2 whose P3's Fix ends "This is
// the author's call.".
const optionalThread = 4302866759

// optionalEntry is the fixer's "## Optional calls" entry for it.
const optionalEntry = "4302866759 P3 app/models/offer.rb:42: option A keeps the cap, option B lifts it"

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
			writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{optionalThread}, nil)
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
			writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{optionalThread}, nil)
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
// list the call again. Another open P3 beside it is still work.
func TestALoopEndsAsDoneWhenItsOptionalCallIsAllThatIsLeft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		open   int
		cycles int
	}{
		{name: "the call alone", open: 1, cycles: 1},
		{name: "another P3 beside it", open: 2, cycles: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t, func(h *harness) { h.cfg.Pipeline.Autofix = config.AutofixOpen })
			pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1, "P3": 1})
			fixer(h, git, fixPushed, func(run store.Run) string {
				writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{optionalThread}, nil)
				return withOptional(fixReport(run.ID, "b2b2b2b Fix the total (review comment 4100000001)",
					"1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none"), optionalEntry)
			})
			h.fix(FixPayload{})
			h.handle()
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
		writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{optionalThread}, nil)
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
