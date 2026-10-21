package engine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// rateLimited is GitHub refusing a GraphQL read for the rate limit, as it
// refused talkable#12054's ("RATE_LIMIT: API rate limit already exceeded").
var rateLimited = &github.APIError{Op: "review threads talkable/talkable#2",
	Errors: []github.GraphQLError{{Type: "RATE_LIMIT", Message: "API rate limit already exceeded for user ID 1."}}}

// rateLimitedThreads makes the fake GitHub refuse PR #2's review threads
// for the rate limit, as it refused talkable#12054's on 2026-10-21, and the
// meter learn from the refusal that zhuravel's budget is spent until until
// (spec 101's mark, set here directly); off undoes both.
func rateLimitedThreads(h *harness, until time.Time) (off func()) {
	h.gh.mu.Lock()
	h.gh.threadsErr = rateLimited
	h.gh.onThreads = func() { h.lowBudget("zhuravel", 0, until) }
	h.gh.mu.Unlock()
	return func() {
		h.gh.mu.Lock()
		h.gh.threadsErr, h.gh.onThreads = nil, nil
		h.gh.mu.Unlock()
	}
}

// colleaguesThread is the thread a colleague left on talkable#12054, on
// PR #2 an hour ago.
func colleaguesThread(h *harness) {
	h.gh.mu.Lock()
	h.gh.threads = map[int][]github.Thread{2: {
		extThread(4335404192, "lib/sandbox.rb", 41, h.clock.Now().Add(-time.Hour), "MINOR: `Dir.children` raises EACCES on the sandbox root", "rev-ann"),
	}}
	h.gh.mu.Unlock()
}

// On 2026-10-21 at 11:19Z `magnum fix` on talkable#12054, a review with
// findings, read the other reviewers' threads at the job's start; GitHub
// refused the read for the rate limit, and the fixer answered magnum's
// findings alone while a colleague's MINOR thread waited for an answer. The
// read now comes before the start: refused for the rate limit, with the
// meter then saying the budget is low, it holds the fix as the floor does
// (no fixer, no pin taken off), and the tick that finds the budget
// recovered starts it with the thread in the external file, read once. With
// the floor off the job starts as before, on magnum's findings alone, and
// says so.
func TestAFixWhoseOtherReviewersReadIsRateLimitedIsHeldUntilTheBudgetRecovers(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		h, git := fixHarness(t)
		pinnedFixablePR(h)
		pr := h.pr(2)
		colleaguesThread(h)
		fixer(h, git, "", func(run store.Run) string {
			return fixReport(run.ID, "nothing", "2 threads (0 fixed, 0 already fixed, 2 not a bug, 0 won't fix)", "none")
		})
		until := h.clock.Now().Add(30 * time.Minute)
		off := rateLimitedThreads(h, until)
		id := h.fix(FixPayload{})
		h.handle()
		want := "held; the fix of talkable/talkable#2 starts once the GitHub budget of zhuravel recovers if the review on a1a1a1a still stands " +
			"(github budget: zhuravel at 0 points until " + until.Local().Format("15:04") + ")"
		if res := h.wantDone(id); res != want {
			t.Fatalf("magnum fix whose read hit the rate limit = %q, want %q", res, want)
		}
		if n := len(h.fixRuns(pr.ID)); n != 0 {
			t.Fatalf("fix runs = %d: the fixer started without the other reviewers' items", n)
		}
		if ev, ok := h.fixEvent("fix.start"); ok {
			t.Fatalf("fix.start recorded: %s", ev.Message)
		}
		if ev, ok := h.fixEvent("fix.held"); !ok || !strings.Contains(string(ev.Data), heldBudget) {
			t.Errorf("fix.held = %+v, %v", ev, ok)
		}
		raw, ok := kvValue(h, KVPRFixHeld(pr.ID))
		if !ok || HeldFixStarts(raw) != "when the GitHub budget recovers" {
			t.Fatalf("held fix = %q, %v", raw, ok)
		}
		if cur, sl := h.pr(2), h.slot("review1"); !cur.Pinned || !sl.Pinned {
			t.Errorf("the held fix took the pin off: PR %v slot %v", cur.Pinned, sl.Pinned)
		}

		off()
		reads := h.gh.count("issue_comments:talkable/talkable#2")
		h.advance(31 * time.Minute) // past the reset
		h.tick()
		runs := h.fixRuns(pr.ID)
		if len(runs) != 1 || runs[0].TargetSHA != fixHead {
			t.Fatalf("fix runs after the recovery = %+v, want cycle 1 on %s", runs, fixHead)
		}
		x := readExternalOf(t, runs[0])
		if len(x.Threads) != 1 || x.Threads[0].CommentID != 4335404192 || x.Threads[0].Answered {
			t.Errorf("external threads = %+v, want the colleague's thread unanswered", x.Threads)
		}
		external := filepath.Join(filepath.Dir(deref(runs[0].ReportPath)), externalFile)
		if !slices.ContainsFunc(h.ag.all(), func(c string) bool {
			return strings.HasPrefix(c, "fix_prompt:claude-fix:"+runs[0].ID+"|") && strings.HasSuffix(c, "|"+external)
		}) {
			t.Errorf("the fix prompt does not name %s: %v", external, h.ag.all())
		}
		if n := h.gh.count("issue_comments:talkable/talkable#2") - reads; n != 1 {
			t.Errorf("the held fix's start read the conversation %d times, want once: the job writes what it read before the start", n)
		}
		if ev, ok := h.fixEvent("fix.dropped"); ok {
			t.Errorf("fix.dropped recorded: %s", ev.Message)
		}
	})
	t.Run("floor off", func(t *testing.T) {
		h, git := fixHarness(t, func(h *harness) { h.cfg.GitHub.BudgetFloor = 0 })
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
		colleaguesThread(h)
		fixer(h, git, "", func(run store.Run) string {
			return fixReport(run.ID, "nothing", "2 threads (0 fixed, 0 already fixed, 2 not a bug, 0 won't fix)", "none")
		})
		h.gh.mu.Lock()
		h.gh.threadsErr = rateLimited
		h.gh.mu.Unlock()
		id := h.fix(FixPayload{})
		h.handle()
		if res := h.wantDone(id); !strings.HasPrefix(res, "fixing talkable/talkable#2: claude-fix fixes the 2 findings") {
			t.Fatalf("magnum fix with the floor off = %q, want the fix started", res)
		}
		if n := len(h.fixRuns(pr.ID)); n != 1 {
			t.Fatalf("fix runs = %d, want the fixer's", n)
		}
		if ev, ok := h.fixEvent("fix.external"); !ok || ev.Level != "warn" || !strings.Contains(ev.Message, "claude-fix answers magnum's findings alone") ||
			!strings.Contains(ev.Message, "rate limit") {
			t.Errorf("fix.external = %+v, %v", ev, ok)
		}
	})
}

// A job with findings reads the other reviewers' items before its start
// too, but they are not its whole work, as they are for a review that found
// nothing: when their file cannot be written beside the report, the fixer
// is prompted on magnum's findings alone, and a warning says so.
func TestAFixWithFindingsGoesOnWhenTheOtherReviewersFileCannotBeWritten(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	colleaguesThread(h)
	// A directory where the file goes: neither written nor removed.
	blocked := filepath.Join(h.layout.ReviewDir("talkable", "talkable", 2, fixHead), externalFile)
	if err := os.MkdirAll(filepath.Join(blocked, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
	})
	h.fix(FixPayload{})
	h.handle()
	if n := h.ag.count("submit:"); n != 1 {
		t.Errorf("the fixer was prompted %d times, want once, on magnum's findings", n)
	}
	if ev, ok := h.fixEvent("fix.external"); !ok || ev.Level != "warn" || !strings.Contains(ev.Message, "claude-fix answers magnum's findings alone") {
		t.Errorf("fix.external = %+v, %v", ev, ok)
	}
	if ev, ok := h.fixEvent("fix.failed"); ok {
		t.Errorf("fix.failed recorded: %s", ev.Message)
	}
	if runs := h.fixRuns(pr.ID); len(runs) != 1 || runs[0].State == store.RunAbandoned {
		t.Errorf("fix runs = %+v, want the fixer's", runs)
	}
}

// On talkable#12054 at 11:19Z the job went on without the other reviewers'
// items, and its only trace was a fix.external warning: the loop would have
// ended "clean after 1 fix cycle", an all-clear for items nobody read. A
// cycle whose fixer did not get them (a read that failed for a cause other
// than the rate limit, or GitHub cutting a list short) records it on the
// loop's row, and the loop's end says so, with what reads them again: in
// fix.replied, fix.done and their data, and in the toast of the loop's end.
func TestALoopWhoseOthersWereNotReadSaysSoAtItsEnd(t *testing.T) {
	const unread = "the other reviewers' threads and comments were not read (HTTP 502): `magnum fix talkable/talkable#2` reads them again"
	badGateway := &github.APIError{Op: "review threads talkable/talkable#2", Status: 502, Message: "Bad Gateway"}
	t.Run("clean after the re-review", func(t *testing.T) {
		h, git := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
		h.gh.mu.Lock()
		h.gh.threadsErr = badGateway
		h.gh.mu.Unlock()
		pushingFixer(h, git, fixPushed)
		h.fix(FixPayload{})
		h.handle()
		if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing || st.OthersUnread != "not read (HTTP 502)" {
			t.Fatalf("fix state after the push = %+v, %v; want the unread items on the loop's row", st, ok)
		}
		if ev, ok := h.fixEvent("fix.replied"); !ok || !strings.Contains(ev.Message, unread) {
			t.Errorf("fix.replied = %+v, %v; want %q", ev, ok, unread)
		}
		reviewWith(h, findings(map[string]int{}, "COMMENTED"))
		h.advance(time.Minute)
		reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
		if _, ok := h.fixStateOf(2); ok {
			t.Fatal("the loop goes on after a clean re-review")
		}
		ev, ok := h.fixEvent("fix.done")
		if !ok || !strings.Contains(ev.Message, unread) || !strings.Contains(string(ev.Data), `"others_unread":"not read (HTTP 502)"`) {
			t.Errorf("fix.done = %+v, %v; want %q", ev, ok, unread)
		}
		h.flush()
		if got := toastsWith(h, "PR #2: clean for magnum's findings after 1 fix cycle"); len(got) != 1 || !strings.Contains(got[0], unread) {
			t.Errorf("toasts = %v, want the loop's end saying the other reviewers were not read", h.nh.shown())
		}
		if n := len(h.fixRuns(pr.ID)); n != 1 {
			t.Errorf("fix runs = %d, want 1", n)
		}
	})
	t.Run("replied only", func(t *testing.T) {
		h, git := fixHarness(t)
		fixablePR(h, "zhuravel", map[string]int{"P2": 2})
		h.gh.mu.Lock()
		h.gh.threadsErr = badGateway
		h.gh.mu.Unlock()
		fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
		h.fix(FixPayload{})
		h.handle()
		if ev, ok := h.fixEvent("fix.replied"); !ok || !strings.Contains(ev.Message, unread) {
			t.Errorf("fix.replied = %+v, %v; want %q", ev, ok, unread)
		}
		h.flush()
		if got := toastsWith(h, "PR #2: claude-fix pushed nothing"); len(got) != 1 || !strings.Contains(got[0], unread) {
			t.Errorf("toasts = %v, want the loop's end saying the other reviewers were not read", h.nh.shown())
		}
	})
	t.Run("cut short", func(t *testing.T) {
		h, git := fixHarness(t)
		fixablePR(h, "zhuravel", map[string]int{"P2": 2})
		colleaguesThread(h)
		h.gh.mu.Lock()
		h.gh.threadsCut = map[int]bool{2: true}
		h.gh.mu.Unlock()
		fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
		h.fix(FixPayload{})
		h.handle()
		const cut = "the other reviewers' threads and comments were not all read (over 1000 review threads): `magnum fix talkable/talkable#2` reads them again"
		if ev, ok := h.fixEvent("fix.replied"); !ok || !strings.Contains(ev.Message, cut) {
			t.Errorf("fix.replied = %+v, %v; want %q", ev, ok, cut)
		}
	})
	t.Run("needs you", func(t *testing.T) {
		h, git := fixHarness(t)
		fixablePR(h, "zhuravel", map[string]int{"P2": 2})
		h.gh.mu.Lock()
		h.gh.threadsErr = badGateway
		h.gh.mu.Unlock()
		fixer(h, git, "", func(run store.Run) string {
			return fixReport(run.ID, "nothing", fixReplied, "- 2101 P1 app/models/offer.rb:42: wants a schema change")
		})
		h.fix(FixPayload{})
		h.handle()
		pr := h.wantState(2, store.PRNeedsAttention)
		msg := deref(pr.LastError)
		first, _, _ := strings.Cut(msg, "\n")
		why, alone := attention.Explain(attention.KindFix, msg, "talkable#2"), attention.Explain(attention.KindFix, first, "talkable#2")
		if why.Cause != alone.Cause || why.Fix != alone.Fix || !slices.Equal(why.NeedsYou, alone.NeedsYou) || !strings.Contains(why.Fix, "magnum fix talkable#2") {
			t.Errorf("attention = %+v, want the reason of its first line %+v: the unread line must change neither cause nor remedy", why, alone)
		}
		if !slices.Contains(why.Tail, unread) {
			t.Errorf("the attention's detail = %q, want %q", why.Tail, unread)
		}
	})
	t.Run("read by the next cycle", func(t *testing.T) {
		h, git := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
		h.gh.mu.Lock()
		h.gh.threadsErr = badGateway
		h.gh.mu.Unlock()
		fixerBy(h, git, func(run store.Run) (string, string) {
			if run.TargetSHA == fixHead { // cycle 1 pushes; cycle 2 only replies
				return fixPushed, fixReport(run.ID, "b2b2b2b Fix the finding", fixReplied, "none")
			}
			return "", fixReport(run.ID, "nothing", fixReplied, "none")
		})
		h.fix(FixPayload{})
		h.handle()
		h.gh.mu.Lock()
		h.gh.threadsErr = nil
		h.gh.mu.Unlock()
		reviewWith(h, findings(map[string]int{"P2": 1}, "COMMENTED"))
		h.advance(time.Minute)
		reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
		if n := len(h.fixRuns(pr.ID)); n != 2 {
			t.Fatalf("fix runs = %d, want the second cycle's", n)
		}
		if ev, ok := h.fixEvent("fix.replied"); !ok || strings.Contains(ev.Message, "other reviewers") || strings.Contains(string(ev.Data), "others_unread") {
			t.Errorf("fix.replied of the cycle that read them = %+v, %v; want no word of unread items", ev, ok)
		}
	})
	t.Run("all read", func(t *testing.T) {
		h, git := fixHarness(t)
		fixablePR(h, "zhuravel", map[string]int{"P2": 2})
		colleaguesThread(h)
		fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
		h.fix(FixPayload{})
		h.handle()
		if ev, ok := h.fixEvent("fix.replied"); !ok || strings.Contains(ev.Message, "other reviewers") {
			t.Errorf("fix.replied = %+v, %v; want no word of unread items", ev, ok)
		}
	})
}

// A loop's next cycle reads the other reviewers' items before it starts
// too: a read GitHub refuses for the rate limit holds the cycle in phase
// reviewing, as a budget below the floor does, with fix.waiting, and the
// tick that finds the budget recovered starts it.
func TestANextFixCycleWhoseReadIsRateLimitedIsHeld(t *testing.T) {
	h, git := fixHarness(t)
	pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"), fixPushed2)
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.mu.Unlock()
	before := len(h.rd.all())
	reqPollNoSettle(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	h.awaitRound(before) // the re-review runs
	off := rateLimitedThreads(h, h.clock.Now().Add(30*time.Minute))
	close(h.rd.gate)
	h.settle()
	h.rd.mu.Lock()
	h.rd.gate = nil
	h.rd.mu.Unlock()

	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs = %d, want 1: the next cycle started without the other reviewers' items", n)
	}
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing || st.Cycle != 1 || !st.Held {
		t.Fatalf("fix state = %+v, %v; want the loop held before its next cycle", st, ok)
	}
	if ev, ok := h.fixEvent("fix.waiting"); !ok || !strings.Contains(ev.Message, "github budget: zhuravel at 0 points") {
		t.Errorf("fix.waiting = %+v, %v", ev, ok)
	}
	off()
	h.tick()
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs = %d while the budget is low, want 1", n)
	}

	h.advance(31 * time.Minute)
	h.tick()
	runs := h.fixRuns(pr.ID)
	if len(runs) != 2 || runs[1].TargetSHA != fixPushed {
		t.Fatalf("fix runs after the recovery = %+v, want cycle 2 on %s", runs, fixPushed)
	}
	if st, ok := h.fixStateOf(2); !ok || st.Cycle != 2 {
		t.Errorf("fix state after the recovery = %+v, %v", st, ok)
	}
}
