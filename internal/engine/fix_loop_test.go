package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// fixPushed2 is the head the fixer pushes in its second cycle.
const fixPushed2 = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"

// reviewWith makes h's rounds post a review whose findings and event
// next(in) gives, recording its judge run as a real round does (its result
// file, the review's id and URL), so the fix job finds that review on the
// head it reviewed.
func reviewWith(h *harness, next func(in pipeline.RoundInput) (map[string]int, string)) {
	var mu sync.Mutex
	n := 0
	h.rd.mu.Lock()
	defer h.rd.mu.Unlock()
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		findings, event := next(in)
		runs, err := h.st.RunsByPR(h.ctx, in.PR.ID)
		if err != nil {
			return failRound(h.t, err)
		}
		round := 1
		for _, r := range runs {
			round = max(round, r.Round+1)
		}
		id := int64(300 + k)
		url := fmt.Sprintf("https://github.com/talkable/talkable/pull/%d#pullrequestreview-%d", in.PR.Number, id)
		result, err := json.Marshal(map[string]any{"event": event, "findings": findings})
		if err != nil {
			return failRound(h.t, err)
		}
		if _, err := h.st.DB().ExecContext(h.ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login,
  state, outcome, review_id, review_url, prompt_text, result_json, created_at)
  VALUES (?, ?, ?, 'codex-judge', ?, ?, ?, 'talkable[bot]', 'verified', 'posted', ?, ?, 'p', ?, ?)`,
			fmt.Sprintf("run-loop-%d", k), in.PR.ID, round, in.Kind, in.TargetSHA, in.PR.Identity, id, url, string(result),
			store.FormatTime(h.clock.Now())); err != nil {
			return failRound(h.t, err)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: round, ReviewID: id, ReviewURL: url, Event: event,
			ReviewCommit: in.TargetSHA, TargetSHA: in.TargetSHA, Findings: findings}, nil
	}
}

// pushingFixer makes h's fake fixer push heads[i] in its i-th run (the last
// one again past the list), report that commit and need nobody.
func pushingFixer(h *harness, git *fixGit, heads ...string) {
	var mu sync.Mutex
	n := 0
	fixerBy(h, git, func(run store.Run) (string, string) {
		mu.Lock()
		head := heads[min(n, len(heads)-1)]
		n++
		mu.Unlock()
		return head, fixReport(run.ID, head[:7]+" Fix the finding", fixReplied, "none")
	})
}

// loopToReReview starts a loop on the operator's reviewed PR #2 whose fixer
// pushes fixPushed in its first cycle (and the rest of heads after), and
// leaves the loop waiting for the forced re-review of fixPushed, with
// GitHub showing that head; the re-reviews post what next gives.
func loopToReReview(t *testing.T, h *harness, git *fixGit, next func(in pipeline.RoundInput) (map[string]int, string), heads ...string) store.PR {
	t.Helper()
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	pushingFixer(h, git, append([]string{fixPushed}, heads...)...)
	h.fix(FixPayload{})
	h.handle()
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing || st.Cycle != 1 {
		t.Fatalf("fix state after the first push = %+v, %v", st, ok)
	}
	reviewWith(h, next)
	h.advance(time.Minute) // the re-review's run is newer than the review the fixer fixed
	return pr
}

// newTeeLogger is the engine's logger for a test that reads what it logs:
// to the test's log and to logs.
func newTeeLogger(t *testing.T, logs *syncBuffer) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.MultiWriter(testWriter{t}, logs), &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// findings is a round plan that posts findings with event every time.
func findings(f map[string]int, event string) func(pipeline.RoundInput) (map[string]int, string) {
	return func(pipeline.RoundInput) (map[string]int, string) { return f, event }
}

// A re-review of the fixer's push that finds nothing ends the loop as done:
// its row goes, fix.done is recorded, an info toast says how many cycles it
// took, and nobody is asked.
func TestACleanReReviewEndsTheLoop(t *testing.T) {
	h, git := fixHarness(t)
	pr := loopToReReview(t, h, git, findings(map[string]int{}, "COMMENTED"))
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on after a clean re-review")
	}
	h.wantState(2, store.PRReviewed)
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Errorf("fix runs = %d, want 1", n)
	}
	ev, ok := h.fixEvent("fix.done")
	if !ok || !strings.Contains(ev.Message, "1 fix cycle") {
		t.Errorf("fix.done = %+v, %v", ev, ok)
	}
	h.flush()
	if got := toastsWith(h, "PR #2: clean after 1 fix cycle"); len(got) != 1 {
		t.Errorf("toasts = %v", h.nh.shown())
	}
	h.wantToasts("needs attention", 0)
}

// A re-review that requests changes with no new findings is not clean: the
// earlier findings it keeps open remain. Below the cap the next cycle runs
// (here its fixer answers and pushes nothing, which ends the loop with its
// info toast); at the cap the loop ends as needs attention, saying the
// earlier findings are still open.
func TestAChangesRequestedReReviewWithNoNewFindingsIsNotClean(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cycles int
	}{{name: "below the cap", cycles: 3}, {name: "at the cap", cycles: 1}} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t, func(h *harness) { h.cfg.Pipeline.FixCycles = tc.cycles })
			pr := loopToReReview(t, h, git, findings(map[string]int{}, "CHANGES_REQUESTED"))
			fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
			reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})

			if _, ok := h.fixEvent("fix.done"); ok {
				t.Error("the loop ended as clean")
			}
			if _, ok := h.fixStateOf(2); ok {
				t.Error("the loop goes on")
			}
			runs := h.fixRuns(pr.ID)
			h.flush()
			if got := toastsWith(h, "clean after"); len(got) != 0 {
				t.Errorf("toasted %v", got)
			}
			if tc.cycles > 1 {
				if len(runs) != 2 {
					t.Fatalf("fix runs = %d, want the second cycle", len(runs))
				}
				if ev, _ := h.fixEvent("fix.start"); !strings.Contains(ev.Message, "fixes the open earlier findings of the review on b2b2b2b") {
					t.Errorf("fix.start = %q", ev.Message)
				}
				if got := toastsWith(h, "PR #2: claude-fix pushed nothing, replied to 3 findings"); len(got) != 1 {
					t.Errorf("toasts = %v", h.nh.shown())
				}
				return
			}
			if len(runs) != 1 {
				t.Fatalf("fix runs = %d, want 1 at the cap", len(runs))
			}
			cur := h.wantState(2, store.PRNeedsAttention)
			if want := "1 fix cycle, earlier findings still open: " + deref(runs[0].ReportPath); deref(cur.LastError) != want {
				t.Errorf("last_error = %q, want %q", deref(cur.LastError), want)
			}
		})
	}
}

// Findings in the re-review of the fixer's push start the next cycle of the
// same loop: the fixer runs again on the re-reviewed head with the
// re-review's URL, the loop keeps who started it, from which head and when,
// and its second push waits for its own forced re-review in cycle 2.
func TestFindingsAfterAFixStartTheNextCycle(t *testing.T) {
	h, git := fixHarness(t)
	pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"), fixPushed2)
	first, _ := h.fixStateOf(2)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})

	runs := h.fixRuns(pr.ID)
	if len(runs) != 2 {
		t.Fatalf("fix runs = %d, want 2", len(runs))
	}
	url := "https://github.com/talkable/talkable/pull/2#pullrequestreview-301"
	if !slices.ContainsFunc(h.ag.all(), func(c string) bool {
		return strings.HasPrefix(c, "fix_prompt:claude-fix:"+runs[1].ID+"|feature|"+url+"|")
	}) {
		t.Errorf("the second run's prompt does not fix the re-review %s: %v", url, h.ag.all())
	}
	if runs[1].TargetSHA != fixPushed {
		t.Errorf("the second run fixes %s, want the re-reviewed %s", runs[1].TargetSHA, fixPushed)
	}
	st, ok := h.fixStateOf(2)
	if !ok || st.Cycle != 2 || st.Cap != 3 || st.Phase != FixPhaseReviewing || st.Head != fixPushed2 || st.By != "magnum fix" ||
		st.Since != fixHead || !st.StartedAt.Equal(first.StartedAt) || st.Role != "claude-fix" || st.Report != deref(runs[1].ReportPath) {
		t.Fatalf("fix state = %+v, %v", st, ok)
	}
	if cur := h.wantState(2, store.PRRereviewPending); !cur.Forced || cur.HeadSHA != fixPushed2 {
		t.Errorf("the second re-review: forced %v, head %s", cur.Forced, cur.HeadSHA)
	}
	h.wantToasts("needs attention", 0)
}

// The loop stops at fix_cycles: findings in the re-review of the last
// cycle's push end it as needs attention, the fix job's kind, naming the
// cycles, the findings left and the last report, and start no more fixers.
func TestTheCapEndsTheLoopAsNeedsAttention(t *testing.T) {
	h, git := fixHarness(t, func(h *harness) { h.cfg.Pipeline.FixCycles = 2 })
	pr := loopToReReview(t, h, git, findings(map[string]int{"P1": 1, "P2": 1}, "CHANGES_REQUESTED"), fixPushed2)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"}) // re-review 1: cycle 2 runs and pushes
	if st, ok := h.fixStateOf(2); !ok || st.Cycle != 2 || st.Cap != 2 || st.Phase != FixPhaseReviewing {
		t.Fatalf("fix state after cycle 2 = %+v, %v", st, ok)
	}
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed2, author: "zhuravel"}) // re-review 2: the cap

	runs := h.fixRuns(pr.ID)
	if len(runs) != 2 {
		t.Fatalf("fix runs = %d, want 2", len(runs))
	}
	report := deref(runs[1].ReportPath)
	cur := h.wantState(2, store.PRNeedsAttention)
	if want := "2 fix cycles, 2 findings left: " + report; deref(cur.LastError) != want {
		t.Errorf("last_error = %q, want %q", deref(cur.LastError), want)
	}
	if kind, _ := h.e.getKV(h.ctx, KVPRAttention(pr.ID)); kind != attention.KindFix {
		t.Errorf("attention kind = %q", kind)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on past its cap")
	}
	h.wantReportInData("fix.capped", report)
	h.wantToasts("needs attention", 1)
}

// Findings the fixer left to the author when it pushed end the loop once
// the push is re-reviewed, whatever that round found: needs attention names
// how many, each by its comment id, priority and place, and the report, but
// never the fixer's reasons, which stay in the report.
func TestNeedsYouEndsTheLoopAfterItsReReview(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P1": 1, "P2": 2})
	fixer(h, git, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "b2b2b2b Guard the nil owner", fixReplied,
			"- 2101 P1 app/models/offer.rb:42: wants a schema change\n- 2102 P2 app/lib/x.rb:7: contested")
	})
	h.fix(FixPayload{})
	h.handle()
	reviewWith(h, findings(map[string]int{"P2": 1}, "COMMENTED"))
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})

	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Errorf("fix runs = %d, want 1: a second cycle ran over the author's findings", n)
	}
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	cur := h.wantState(2, store.PRNeedsAttention)
	want := "fix: 2 findings need your call (2101 P1 app/models/offer.rb:42, 2102 P2 app/lib/x.rb:7): " + report
	if deref(cur.LastError) != want {
		t.Errorf("last_error = %q, want %q", deref(cur.LastError), want)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
	h.flush()
	h.wantToasts("needs attention", 1)
	for _, reason := range []string{"schema change", "contested"} {
		if strings.Contains(deref(cur.LastError), reason) {
			t.Errorf("last_error carries the fixer's words %q", reason)
		}
		for _, toast := range h.nh.shown() {
			if strings.Contains(toast, reason) {
				t.Errorf("toast %q carries the fixer's words %q", toast, reason)
			}
		}
		evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			if strings.Contains(ev.Message, reason) || strings.Contains(string(ev.Data), reason) {
				t.Errorf("event %s carries the fixer's words %q: %s %s", ev.Kind, reason, ev.Message, ev.Data)
			}
		}
	}
}

// A re-review that does not post ends the loop: it says so as the fix
// job's attention (a PR the round sent back in line keeps its retry and gets
// the toast alone), and a PR the round itself left needing attention keeps
// the round's account of why.
func TestAReReviewThatDidNotPostEndsTheLoop(t *testing.T) {
	for _, tc := range []struct {
		outcome   string
		state     string
		lastError string
	}{
		{outcome: pipeline.OutcomeTimeout, state: store.PRRereviewPending, lastError: "timeout: judge timed out"},
		{outcome: pipeline.OutcomeNeedsAttention, state: store.PRNeedsAttention, lastError: "judge timed out"},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			h, git := fixHarness(t)
			loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"))
			h.rd.mu.Lock()
			h.rd.script = func(pipeline.RoundInput) (pipeline.RoundResult, error) {
				return pipeline.RoundResult{Outcome: tc.outcome, Error: "judge timed out"}, nil
			}
			h.rd.mu.Unlock()
			reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})

			if _, ok := h.fixStateOf(2); ok {
				t.Error("the loop goes on")
			}
			cur := h.wantState(2, tc.state)
			if deref(cur.LastError) != tc.lastError {
				t.Errorf("last_error = %q, want %q", deref(cur.LastError), tc.lastError)
			}
			ev, ok := h.fixEvent("fix.failed")
			if want := "fix loop ended: the re-review did not post (" + tc.outcome + ")"; !ok || ev.Message != want {
				t.Errorf("fix.failed = %q, %v; want %q", ev.Message, ok, want)
			}
			h.wantToasts("needs attention", 1)
		})
	}
}

// A daemon that stops while the loop's re-review runs leaves the loop for
// the round's recovery: the re-review was cut short, it did not fail.
func TestAShutdownDuringTheReReviewKeepsTheLoop(t *testing.T) {
	h, git := fixHarness(t)
	pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"))
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.mu.Unlock()
	before := len(h.rd.all())
	reqPollNoSettle(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	h.awaitRound(before)
	h.e.mu.Lock()
	h.e.rounds[pr.ID].cancel() // what shutdown does to every reservation
	h.e.mu.Unlock()
	h.settle()

	st, ok := h.fixStateOf(2)
	if !ok || st.Phase != FixPhaseReviewing || st.Cycle != 1 {
		t.Errorf("fix state = %+v, %v; want the loop waiting for its re-review", st, ok)
	}
	if _, ok := h.fixEvent("fix.failed"); ok {
		t.Error("the shutdown failed the loop")
	}
	h.wantToasts("needs attention", 0)
}

// An abort of the loop's running re-review ends the loop quietly: its row
// goes with the abort, nobody is asked, and no fixer starts.
func TestAnAbortedReReviewEndsTheLoopQuietly(t *testing.T) {
	h, git := fixHarness(t)
	pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"))
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.mu.Unlock()
	before := len(h.rd.all())
	reqPollNoSettle(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	h.awaitRound(before)
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	h.wantDone(id)

	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop's row outlived the abort")
	}
	if _, ok := h.fixEvent("fix.stopped"); !ok {
		t.Error("no fix.stopped event")
	}
	if _, ok := h.fixEvent("fix.failed"); ok {
		t.Error("the abort failed the loop")
	}
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Errorf("fix runs = %d, want 1", n)
	}
	h.wantToasts("needs attention", 0)
}

// autofixHarness is fixHarness with [pipeline] autofix = "own" and the
// operator's PR #2 opened at fixHead by author, synced and queued: its next
// tick reviews it with what next gives. mods run before the engine is built.
func autofixHarness(t *testing.T, author string, next func(in pipeline.RoundInput) (map[string]int, string), mods ...func(*harness)) (*harness, *fixGit) {
	t.Helper()
	h, git := fixHarness(t, append([]func(*harness){func(h *harness) { h.cfg.Pipeline.Autofix = config.AutofixOwn }}, mods...)...)
	reviewWith(h, next)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: author})
	h.tick() // #2 queued
	h.advance(5 * time.Minute)
	return h, git
}

// fixStartBy is who started PR #2's latest loop, as its fix.start event
// says ("" = no loop started).
func (h *harness) fixStartBy() string {
	h.t.Helper()
	ev, ok := h.fixEvent("fix.start")
	if !ok {
		return ""
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		h.t.Fatal(err)
	}
	by, _ := data["by"].(string)
	return by
}

// With autofix = "own", a review that posts findings on the operator's PR
// starts the fix loop by itself, as `magnum fix` would.
func TestAutofixOwnStartsALoopAfterFindings(t *testing.T) {
	h, git := autofixHarness(t, "zhuravel", findings(map[string]int{"P2": 2}, "COMMENTED"))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.tick() // reviewed: the fixer runs and answers everything
	pr := h.pr(2)
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs = %d, want 1", n)
	}
	if by := h.fixStartBy(); by != "autofix" {
		t.Errorf("the loop was started by %q, want autofix", by)
	}
	h.flush()
	if got := toastsWith(h, "PR #2: claude-fix pushed nothing, replied to 3 findings"); len(got) != 1 {
		t.Errorf("toasts = %v", h.nh.shown())
	}
}

// Autofix fixes the head the review saw: a push the poller recorded while
// the round ran makes it start nothing, which it logs and never toasts.
func TestAutofixSkipsAHeadThatMovedSinceTheReview(t *testing.T) {
	logs := &syncBuffer{}
	h, git := autofixHarness(t, "zhuravel", findings(map[string]int{"P2": 1}, "COMMENTED"), func(h *harness) {
		h.d.Logger = newTeeLogger(h.t, logs)
	})
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.rd.mu.Lock()
	inner := h.rd.script
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		// The author pushes while the judge works: the poller records it.
		if _, err := h.st.UpsertPRFromGitHub(h.ctx, store.GitHubPR{RepoID: in.PR.RepoID, NodeID: in.PR.NodeID, Number: in.PR.Number,
			URL: in.PR.URL, HeadSHA: fixPushed}); err != nil {
			return failRound(h.t, err)
		}
		return inner(in)
	}
	h.rd.mu.Unlock()
	h.tick()
	pr := h.pr(2)
	if n := len(h.fixRuns(pr.ID)); n != 0 {
		t.Errorf("fix runs = %d, want none", n)
	}
	if h.ag.count("submit:claude-fix:") != 0 {
		t.Error("the fixer was prompted")
	}
	if !strings.Contains(logs.String(), "nothing to fix: no review on b2b2b2b") {
		t.Errorf("the refusal is not logged:\n%s", logs.String())
	}
	h.flush()
	for _, toast := range h.nh.shown() {
		if strings.Contains(toast, "fix") || strings.Contains(toast, "needs attention") {
			t.Errorf("toasted %q", toast)
		}
	}
}

// Autofix fixes only the operator's own PRs: the fixer pushes and replies
// as them.
func TestAutofixIgnoresAnotherAuthorsPR(t *testing.T) {
	h, git := autofixHarness(t, "alice", findings(map[string]int{"P1": 1}, "CHANGES_REQUESTED"))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Errorf("fix runs = %d, want none", n)
	}
	h.wantState(2, store.PRReviewed)
}

// A PR switched out of autofix (`magnum fix --off`) is left to the
// operator, however many findings its review has.
func TestAutofixLeavesAFixOffPRAlone(t *testing.T) {
	h, git := autofixHarness(t, "zhuravel", findings(map[string]int{"P1": 1}, "CHANGES_REQUESTED"))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	pr := h.pr(2)
	if err := h.e.setFixOff(h.ctx, pr.ID, FixOff{Since: h.clock.Now(), By: "magnum fix --off"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if n := len(h.fixRuns(pr.ID)); n != 0 {
		t.Errorf("fix runs = %d, want none", n)
	}
	h.wantState(2, store.PRReviewed)
}

// Autofix starts nothing after a review that approves, one that found
// nothing, or on a watch whose autofix is "never" (the default).
func TestAutofixLeavesAnApprovedOrCleanReviewAlone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		findings map[string]int
		event    string
		mod      func(h *harness)
	}{
		{name: "approved", findings: map[string]int{"P3": 1}, event: "APPROVED"},
		{name: "clean", findings: map[string]int{}, event: "COMMENTED"},
		{name: "never", findings: map[string]int{"P1": 1}, event: "CHANGES_REQUESTED",
			mod: func(h *harness) { h.cfg.Pipeline.Autofix = config.AutofixNever }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mods []func(*harness)
			if tc.mod != nil {
				mods = append(mods, tc.mod)
			}
			h, git := autofixHarness(t, "zhuravel", findings(tc.findings, tc.event), mods...)
			fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
			h.tick()
			if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
				t.Errorf("fix runs = %d, want none", n)
			}
		})
	}
}

// A watch that asks for autofix but has no fix role starts nothing and says
// so in the log once per PR, not at every review.
func TestAutofixWithoutAFixRoleLogsOncePerPR(t *testing.T) {
	logs := &syncBuffer{}
	h, _ := autofixHarness(t, "zhuravel", findings(map[string]int{"P2": 1}, "COMMENTED"), func(h *harness) {
		h.cfg.Watches[0].FixRole = new("")
		h.d.Logger = newTeeLogger(h.t, logs)
	})
	h.tick() // reviewed
	h.advance(time.Hour)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"}) // a push
	h.advance(time.Hour)
	h.tick() // re-reviewed
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want 2", n)
	}
	if n := strings.Count(logs.String(), "autofix: no fix role for talkable/talkable"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, logs.String())
	}
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Errorf("fix runs = %d, want none", n)
	}
}

// A loop's row that cannot be read is never taken for no loop: autofix
// starts nothing over it.
func TestAnUnreadableFixRowStartsNoAutofix(t *testing.T) {
	h, git := autofixHarness(t, "zhuravel", findings(map[string]int{"P2": 1}, "COMMENTED"))
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.rd.mu.Lock()
	inner := h.rd.script
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if err := h.st.SetKV(h.ctx, KVPRFix(in.PR.ID), "not json"); err != nil {
			return failRound(h.t, err)
		}
		return inner(in)
	}
	h.rd.mu.Unlock()
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Errorf("fix runs = %d, want none", n)
	}
	if raw, _ := h.e.getKV(h.ctx, KVPRFix(h.pr(2).ID)); raw != "not json" {
		t.Errorf("the unreadable row became %q", raw)
	}
}
