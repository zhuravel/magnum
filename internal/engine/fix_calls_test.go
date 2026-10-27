package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// fixReviewComment is a comment of the review file beside a fix report.
type fixReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

// writeFixReviewFiles writes, beside the fix report at report, the round's
// threads file with magnum's threads (their first comments' ids) and the
// review file with the review's own comments.
func writeFixReviewFiles(t *testing.T, report string, threads []int64, comments []fixReviewComment) {
	t.Helper()
	dir := filepath.Dir(report)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Error(err)
		return
	}
	own := []agents.ReviewThread{}
	for _, id := range threads {
		own = append(own, agents.ReviewThread{ID: "PRRT_x", CommentID: id, Finding: "**[P2] a finding**", Replies: []agents.ThreadReply{}})
	}
	if comments == nil {
		comments = []fixReviewComment{}
	}
	for name, v := range map[string]any{
		pipeline.ThreadsFile:  own,
		agents.PostReviewFile: map[string]any{"event": "COMMENT", "body": "**Re-review**", "comments": comments},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Error(err)
		}
	}
}

// On talkable-integration#867 the fixer pushed the fix of a finding whose
// thread a colleague started, where it did not write then, and declined a
// P2 of magnum's whose proof needed a committed TLS key; the loop then said
// "2 findings need your call (…)" and the operator had to read the report
// to learn what the call was. Each finding the fixer says it fixed ("fixed
// in <sha>.") or did not ("not fixed.") is now a sentence with the action
// and the thread's link, the report last: in the toast at the push, the
// fix.needs_you events, the PR's last_error and pr.needs_attention, and the
// attention reason the card and `magnum status --json` show. A fix in
// another reviewer's thread that the fixer did not answer asks for the
// reply alone: that reviewer resolves the thread. The fixer's reasons stay
// in the report.
func TestANeedsYouLoopSaysWhatToDoPerThread(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	fixer(h, git, fixPushed, func(run store.Run) string {
		writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{4296732634, 4302866759}, nil)
		return fixReport(run.ID, "b2b2b2b Keep the phone digits (review thread 4302616576)", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)",
			"- 4302616576 P2 src/integration.js:135: fixed in b2b2b2b. A human started this thread, so I did not reply in it or resolve it.\n"+
				"- 4302866759 P2 src/integration.js:1706: not fixed. The spec needs a committed TLS private key.")
	})
	h.fix(FixPayload{})
	h.handle()
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	const (
		other   = "https://github.com/talkable/talkable/pull/2#discussion_r4302616576"
		magnums = "https://github.com/talkable/talkable/pull/2#discussion_r4302866759"
		fixed   = "reply fixed in b2b2b2b in another reviewer's thread: " + other
		decline = "decide magnum's P2 on src/integration.js:1706: the fixer declined it; reply won't fix at " + magnums +
			", or `magnum fix talkable/talkable#2` sends it back"
	)
	want := "fix: " + fixed + "; " + decline + "; details: " + report

	if ev, ok := h.fixEvent("fix.needs_you"); !ok || ev.Message != want+"; the re-review of b2b2b2b runs first" {
		t.Errorf("fix.needs_you at the push = %q, want %q", ev.Message, want+"; the re-review of b2b2b2b runs first")
	}
	if got := toastsWith(h, "talkable#2 needs attention"); len(got) != 1 || !strings.Contains(got[0], fixed) || !strings.Contains(got[0], decline) {
		t.Errorf("toasts at the push = %q, want one with both calls", h.nh.shown())
	}

	reviewWith(h, findings(map[string]int{"P2": 1}, "COMMENTED"))
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})

	cur := h.wantState(2, store.PRNeedsAttention)
	if deref(cur.LastError) != want {
		t.Errorf("last_error = %q\nwant         %q", deref(cur.LastError), want)
	}
	if ev, ok := h.fixEvent("fix.needs_you"); !ok || ev.Message != want {
		t.Errorf("fix.needs_you at the loop's end = %q", ev.Message)
	}
	if ev, ok := h.fixEvent("pr.needs_attention"); !ok || ev.Message != attention.KindFix+": "+want {
		t.Errorf("pr.needs_attention = %q", ev.Message)
	}
	why := attention.Explain(attention.KindFix, deref(cur.LastError), "talkable#2")
	calls := []attention.FixCall{{Thread: "4302616576", URL: other, Action: fixed}, {Thread: "4302866759", URL: magnums, Action: decline}}
	if !slices.Equal(why.NeedsYou, calls) || !slices.Equal(why.Steps, []string{fixed, decline, "details: " + report}) {
		t.Errorf("the attention reason: needs you %+v, steps %q", why.NeedsYou, why.Steps)
	}
	h.flush()
	h.wantToasts("needs attention", 1)

	evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, words := range []string{"A human started", "TLS private key"} {
		if strings.Contains(deref(cur.LastError), words) {
			t.Errorf("last_error carries the fixer's words %q", words)
		}
		for _, toast := range h.nh.shown() {
			if strings.Contains(toast, words) {
				t.Errorf("toast %q carries the fixer's words %q", toast, words)
			}
		}
		for _, ev := range evs {
			if strings.Contains(ev.Message, words) || strings.Contains(string(ev.Data), words) {
				t.Errorf("event %s carries the fixer's words %q", ev.Kind, words)
			}
		}
	}
}

// An entry magnum cannot read as fixed or not fixed keeps today's words for
// that finding: one without those words, or about the PR description (no
// thread to answer in), is named by its id, priority and place after the
// calls, one with anything else there is only counted, and a fixed one whose
// thread magnum cannot place (no threads file beside the report) is named
// alike rather than called another reviewer's. A report magnum can read no call of
// keeps today's message whole: named after a re-review, counted at the
// settle.
func TestAnUnreadableNeedsYouBulletKeepsTheOldWords(t *testing.T) {
	const prURL, ref = "https://github.com/talkable/talkable/pull/2", "talkable/talkable#2"
	report := filepath.Join(t.TempDir(), "claude-fix.md")
	writeFixReviewFiles(t, report, []int64{2102}, nil)
	got := needsYouText([]string{
		"2101 P1 app/models/offer.rb:42: wants a schema change",
		"2102 P2 app/lib/x.rb:7: not fixed. contested",
		"see the thread",
		"5485002407 description: not fixed. the Deploy section names two migrations",
	}, nil, report, prURL, ref, true)
	want := "decide magnum's P2 on app/lib/x.rb:7: the fixer declined it; reply won't fix at " + prURL + "#discussion_r2102, or `magnum fix " + ref +
		"` sends it back; 3 more findings need your call (2101 P1 app/models/offer.rb:42, 5485002407 description, 1 entry magnum could not read); details: " + report
	if got != want {
		t.Errorf("needsYouText =\n %q\nwant\n %q", got, want)
	}

	old := []string{"2101 P1 app/models/offer.rb:42: wants a schema change", "2102 P2 app/lib/x.rb:7: contested"}
	if got, want := needsYouText(old, nil, report, prURL, ref, true), "2 findings need your call (2101 P1 app/models/offer.rb:42, 2102 P2 app/lib/x.rb:7): "+report; got != want {
		t.Errorf("no call, named = %q, want %q", got, want)
	}
	if got, want := needsYouText(old, nil, report, prURL, ref, false), "2 findings need your call: "+report; got != want {
		t.Errorf("no call, counted = %q, want %q", got, want)
	}

	bare := filepath.Join(t.TempDir(), "claude-fix.md")
	if got, want := needsYouText([]string{"2103 P2 app/c.rb:9: fixed in c3c3c3c. replied nowhere"}, nil, bare, prURL, ref, true),
		"1 finding needs your call (2103 P2 app/c.rb:9): "+bare; got != want {
		t.Errorf("a thread magnum cannot place = %q, want %q", got, want)
	}
}

// The threads file beside the fixer's report lists the threads magnum had
// when the round that posted the fixed review began, so that review's own
// comments are not in it: an entry whose place and priority match a comment
// of the review file is magnum's thread too (a failed reply there is not
// taken for another reviewer's thread), and one at the same place with
// another priority is not.
func TestTheFixedReviewsOwnCommentsAreMagnumsThreads(t *testing.T) {
	const prURL, ref = "https://github.com/talkable/talkable/pull/2", "talkable/talkable#2"
	report := filepath.Join(t.TempDir(), "claude-fix.md")
	writeFixReviewFiles(t, report, []int64{2101}, []fixReviewComment{{Path: "app/lib/x.rb", Line: 7, Body: "**[P2] wrong total**\n\nThe total counts discounted items."}})
	got := needsYouText([]string{
		"2102 P2 app/lib/x.rb:7: fixed in b2b2b2b. The reply failed.",
		"2103 P1 app/lib/x.rb:7: not fixed. Out of scope.",
	}, nil, report, prURL, ref, true)
	want := "reply fixed in b2b2b2b in magnum's thread and resolve it: " + prURL + "#discussion_r2102; " +
		"decide another reviewer's P1 on app/lib/x.rb:7: the fixer declined it; reply won't fix at " + prURL + "#discussion_r2103, or `magnum fix " + ref +
		"` sends it back; details: " + report
	if got != want {
		t.Errorf("needsYouText =\n %q\nwant\n %q", got, want)
	}
}

// magnumsThread is a thread of magnum's review on PR #2 (its first comment
// by the App's login, "talkable") with the given replies, each by the
// author (zhuravel), and resolved when said.
func magnumsThread(id int64, path string, line int, at time.Time, resolved bool, replies ...string) github.Thread {
	authors := []string{"talkable"}
	for range replies {
		authors = append(authors, "zhuravel")
	}
	t := extThread(id, path, line, at, "**[P1] the owner can be nil**", authors...)
	for i, body := range replies {
		t.Comments[i+1].Body = body
	}
	t.Resolved = resolved
	return t
}

// On talkable#12112 the fixer answered magnum's P1 thread and listed the fix
// under Needs you all the same, and magnum asked the operator to "reply
// fixed in e05fbde in magnum's thread and resolve it". A thread of magnum's
// that GitHub shows open with the answer the call asks for ("Fixed in" for
// a fix) now asks only for the resolution, at the settle and at the loop's
// end alike (each reads the thread once from GitHub when no re-review read
// it after the fixer's report), and the card reads that call back.
func TestAnAnsweredOpenThreadOfMagnumsReviewAsksOnlyToResolveIt(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P1": 1})
	h.gh.mu.Lock()
	h.gh.threads = map[int][]github.Thread{2: {magnumsThread(4302866759, "app/models/offer.rb", 42, h.clock.Now().Add(-time.Hour), false,
		"Fixed in e05fbde: the owner is checked before the total.")}}
	h.gh.mu.Unlock()
	fixer(h, git, fixPushed, func(run store.Run) string {
		writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{4302866759}, nil)
		return fixReport(run.ID, "e05fbde Check the owner\nb2b2b2b Cover the nil owner", fixReplied,
			"- 4302866759 P1 app/models/offer.rb:42: fixed in e05fbde. Resolving the thread failed.")
	})
	h.fix(FixPayload{})
	h.handle()
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	const url = prPage + "#discussion_r4302866759"
	resolve := "resolve magnum's thread, already answered: " + url
	want := "fix: " + resolve + "; details: " + report
	if ev, ok := h.fixEvent("fix.needs_you"); !ok || ev.Message != want+"; the re-review of b2b2b2b runs first" {
		t.Errorf("fix.needs_you at the settle = %q, want %q", ev.Message, want+"; the re-review of b2b2b2b runs first")
	}
	if ev, ok := h.fixEvent("fix.calls_settled"); !ok || ev.Message != "1 finding left to you is answered in magnum's thread and asks only for the resolution" {
		t.Errorf("fix.calls_settled = %+v, %v", ev, ok)
	}

	reviewWith(h, findings(map[string]int{}, "COMMENTED"))
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	cur := h.wantState(2, store.PRNeedsAttention)
	if deref(cur.LastError) != want {
		t.Errorf("last_error at the loop's end = %q\nwant %q", deref(cur.LastError), want)
	}
	why := attention.Explain(attention.KindFix, deref(cur.LastError), "talkable#2")
	if calls := []attention.FixCall{{Thread: "4302866759", URL: url, Action: resolve}}; !slices.Equal(why.NeedsYou, calls) || why.Summary != "fix: resolve 1 thread" {
		t.Errorf("the card: needs you %+v, summary %q", why.NeedsYou, why.Summary)
	}
}

// A thread of magnum's that the operator resolved by hand, with no reply,
// asks nothing any more: its call goes, for a finding the fixer declined as
// for one it fixed, while a thread of magnum's still open without an answer
// keeps its call. Once no call is left, the push's re-review decides the
// loop alone.
func TestAThreadTheOperatorResolvedByHandDropsItsCall(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P1": 1, "P2": 2})
	at := h.clock.Now().Add(-time.Hour)
	h.gh.mu.Lock()
	h.gh.threads = map[int][]github.Thread{2: {
		magnumsThread(2101, "app/models/offer.rb", 42, at, true),
		magnumsThread(2102, "app/lib/x.rb", 7, at, true),
		magnumsThread(2103, "app/lib/y.rb", 9, at, false),
	}}
	h.gh.mu.Unlock()
	fixer(h, git, fixPushed, func(run store.Run) string {
		writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{2101, 2102, 2103}, nil)
		return fixReport(run.ID, "b2b2b2b Guard the nil owner", fixReplied,
			"- 2101 P1 app/models/offer.rb:42: not fixed. Which of the two owners wins?\n"+
				"- 2102 P2 app/lib/x.rb:7: fixed in b2b2b2b. The reply failed.\n"+
				"- 2103 P2 app/lib/y.rb:9: fixed in b2b2b2b. The reply failed.")
	})
	h.fix(FixPayload{})
	h.handle()
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	want := "fix: reply fixed in b2b2b2b in magnum's thread and resolve it: " + prPage + "#discussion_r2103; details: " + report
	if ev, ok := h.fixEvent("fix.needs_you"); !ok || ev.Message != want+"; the re-review of b2b2b2b runs first" {
		t.Errorf("fix.needs_you = %q, want %q", ev.Message, want+"; the re-review of b2b2b2b runs first")
	}
	if st, ok := h.fixStateOf(2); !ok || !slices.Equal(st.NeedsYou, []string{"2103 P2 app/lib/y.rb:9: fixed in b2b2b2b. The reply failed."}) {
		t.Errorf("the loop keeps %q for its end, want the open thread's entry alone", st.NeedsYou)
	}
	if ev, ok := h.fixEvent("fix.calls_settled"); !ok || ev.Message != "2 findings left to you are resolved in magnum's threads and ask nothing" {
		t.Errorf("fix.calls_settled = %+v, %v", ev, ok)
	}

	t.Run("every call settled", func(t *testing.T) {
		h, git := fixHarness(t)
		fixablePR(h, "zhuravel", map[string]int{"P1": 1})
		h.gh.mu.Lock()
		h.gh.threads = map[int][]github.Thread{2: {magnumsThread(2101, "app/models/offer.rb", 42, h.clock.Now().Add(-time.Hour), true)}}
		h.gh.mu.Unlock()
		fixer(h, git, fixPushed, func(run store.Run) string {
			writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{2101}, nil)
			return fixReport(run.ID, "b2b2b2b Guard the nil owner", fixReplied, "- 2101 P1 app/models/offer.rb:42: not fixed. Which owner wins?")
		})
		h.fix(FixPayload{})
		h.handle()
		if _, ok := h.fixEvent("fix.needs_you"); ok {
			t.Error("a fix.needs_you event for a call the operator settled")
		}
		h.flush()
		h.wantToasts("needs attention", 0)
		reviewWith(h, findings(map[string]int{}, "COMMENTED"))
		h.advance(time.Minute)
		reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
		h.wantState(2, store.PRReviewed)
		if _, ok := h.fixEvent("fix.done"); !ok {
			t.Error("no fix.done after a clean re-review with no call left")
		}
	})
}

// magnum asks for a reply in another reviewer's thread and never for its
// resolution (that reviewer resolves it): a fix there that the author
// answered by hand without the fixer's marker keeps the reply-only call,
// whatever the thread shows.
func TestAnOtherReviewersThreadStillAsksOnlyForAReply(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	other := extThread(4322458400, "app/b.rb", 9, h.clock.Now().Add(-time.Hour), "Rename this", "bob-rev", "zhuravel")
	other.Comments[1].Body = "Fixed in b2b2b2b: renamed."
	h.gh.mu.Lock()
	h.gh.threads = map[int][]github.Thread{2: {other}}
	h.gh.mu.Unlock()
	fixer(h, git, fixPushed, func(run store.Run) string {
		writeFixReviewFiles(h.t, deref(run.ReportPath), nil, nil)
		return fixReport(run.ID, "b2b2b2b Rename the helper", fixReplied, "- 4322458400 app/b.rb:9: fixed in b2b2b2b. The author answered there already.")
	})
	h.fix(FixPayload{})
	h.handle()
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	want := "fix: reply fixed in b2b2b2b in another reviewer's thread: " + prPage + "#discussion_r4322458400; details: " + report
	if ev, ok := h.fixEvent("fix.needs_you"); !ok || ev.Message != want+"; the re-review of b2b2b2b runs first" {
		t.Errorf("fix.needs_you = %q, want %q", ev.Message, want+"; the re-review of b2b2b2b runs first")
	}
	if _, ok := h.fixEvent("fix.calls_settled"); ok {
		t.Error("another reviewer's thread settled a call")
	}
}
