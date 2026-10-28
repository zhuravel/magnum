package engine

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// missedBusy is a clean round whose judge went without codex-review's
// report because its shell pane was busy (talkable#12092), and
// aliceCompleting the status comment's line while magnum runs that
// reviewer again.
var (
	missedBusy = autoRound{findings: map[string]int{}, verdict: store.VerdictClean, line: "No problems found. LGTM :shipit:",
		missing: []pipeline.MissingReport{{Role: "codex-review", Status: pipeline.ReportBusy}}}
	aliceCompleting = "Not approved as alice yet: one of Magnum's reviewers did not finish on this head, and Magnum runs it again. " +
		"Magnum approves only a complete review."
)

// firstRound opens PR #2 (bobRevs) at head after a first sync and runs its
// first round, whose end queues what auto-approval decides on.
func (h *harness) firstRound(head string) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, bobRevs(prSpec{head: head}))
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 1 {
		h.t.Fatalf("rounds = %d, want the first review", n)
	}
}

// completeQueued is PR #2's pr.complete_queued events' data.
func completeQueued(t *testing.T, h *harness) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range approvalEvents(t, h, 2, "pr.complete_queued") {
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// The live case (talkable#12092 and five more from 2026-10-25 to
// 2026-10-28): codex-review found its pane busy after a restart, the round
// went on without it and posted a clean review, and auto-approval refused
// that review for the reviewer it did not hear. The review's end queues a
// completion of the same head: only codex-review and the judge start, the
// judge in its session at its re-review effort, with no triage and no
// restarts, and its clean review, which heard codex-review, is approved.
func TestACleanReviewThatMissedABusyReviewerIsCompleted(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, missedBusy, cleanRound)
	h.firstRound("b1")
	h.wantRefused(2, "codex-review (busy)")
	h.wantState(2, store.PRRereviewPending)
	queued := completeQueued(t, h)
	if len(queued) != 1 {
		t.Fatalf("pr.complete_queued events = %+v, want one", queued)
	}
	if q := queued[0]; q["round"] != float64(1) || q["head"] != "b1" || q["attempt"] != float64(1) ||
		!slices.Equal(anyStrings(q["roles"]), []string{"codex-review"}) || !slices.Equal(anyStrings(q["causes"]), []string{pipeline.ReportBusy}) {
		t.Fatalf("pr.complete_queued data = %+v", q)
	}

	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("rounds = %d, want the completion", len(ins))
	}
	in := ins[1]
	if c := in.Complete; c == nil || c.Round != 1 || c.Base != 1 || !slices.Equal(c.Roles, []string{"codex-review"}) {
		t.Fatalf("the completion's input: %+v", in.Complete)
	}
	if in.Kind != pipeline.KindRereview || !in.SameHead || in.TargetSHA != "b1" || in.MaxRestarts != 0 || in.FullPass || in.HoldApproval {
		t.Fatalf("input: kind %s same head %v target %s restarts %d full pass %v hold %v",
			in.Kind, in.SameHead, in.TargetSHA, in.MaxRestarts, in.FullPass, in.HoldApproval)
	}
	if got := h.workspaceRoles(in.PR.ID); !slices.Equal(got, []string{"codex-judge", "codex-review"}) {
		t.Fatalf("workspace roles = %v, want the judge and codex-review", got)
	}
	starts := approvalEvents(t, h, 2, "engine.round_start")
	var data map[string]any
	if len(starts) != 2 || json.Unmarshal(starts[1].Data, &data) != nil || data["completes"] != float64(1) ||
		starts[1].Message != "completion of round 1 in review1 at b1: codex-judge, codex-review" {
		t.Fatalf("round starts: %+v (data %+v)", starts, data)
	}
	h.wantState(2, store.PRReviewed)
	h.flush()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want the approval of b1 after the completion", got)
	}
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want no third", n)
	}
}

// A reviewer that refused (Codex's cyber warning), is logged out or hit its
// usage limit needs the operator or time, which the pauses already wait
// for: its clean review gets no completion, and the refusal stands.
func TestCompletionIsNotForARefusedOrLoggedOutReviewer(t *testing.T) {
	for _, status := range []string{"refused", "login_required", "usage_limit"} {
		t.Run(status, func(t *testing.T) {
			h, plan := newAutoHarness(t, approvesAsAlice)
			r := missedBusy
			r.missing = []pipeline.MissingReport{{Role: "codex-review", Status: status}}
			plan.set(2, r, cleanRound)
			h.firstRound("b1")
			h.flush()
			h.flush()
			h.wantRefused(2, "codex-review ("+status+")")
			h.wantState(2, store.PRReviewed)
			if q := completeQueued(t, h); len(q) != 0 {
				t.Fatalf("pr.complete_queued events: %+v", q)
			}
			if n := len(h.rd.all()); n != 1 {
				t.Fatalf("rounds = %d, want the first alone", n)
			}
			if _, ok := kvValue(h, kvPRComplete(h.pr(2).ID)); ok {
				t.Fatal("a completion is on record")
			}
		})
	}
}

// codex-review stays busy: the first review and its two completions all
// go without it. After the second completion magnum gives up on the head
// with one pr.complete_gave_up, starts no third, and auto-approval's
// refusal stands.
func TestCompletionGivesUpAfterTwoAttemptsOnAHead(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, missedBusy)
	h.firstRound("b1")
	h.tick() // completion 1
	h.tick() // completion 2
	for range 3 {
		h.flush()
	}
	if n := len(h.rd.all()); n != 3 {
		t.Fatalf("rounds = %d, want the review and two completions", n)
	}
	queued := completeQueued(t, h)
	if len(queued) != 2 || queued[0]["attempt"] != float64(1) || queued[1]["attempt"] != float64(2) || queued[1]["round"] != float64(2) {
		t.Fatalf("pr.complete_queued events = %+v, want attempts 1 and 2", queued)
	}
	if evs := approvalEvents(t, h, 2, "pr.complete_gave_up"); len(evs) != 1 {
		t.Fatalf("pr.complete_gave_up events = %+v, want one", evs)
	}
	h.wantState(2, store.PRReviewed)
	if got := h.autoRefused(2); !strings.Contains(got, "codex-review (busy)") {
		t.Fatalf("held back for %q, want the reviewer it did not hear", got)
	}
	if r, _ := ParseAutoApproveRefused(mustKV(t, h, KVPRAutoApproveRefused(h.pr(2).ID))); r.RunID != "run-3" {
		t.Fatalf("the refusal names %s, want the second completion's review (run-3)", r.RunID)
	}
	if got := h.created(); len(got) != 0 {
		t.Fatalf("posted = %q", got)
	}
}

// A push while the completion waits drops it: the next round is the
// re-review of the new head, in full, and nothing completes the old one.
func TestAPushDropsTheCompletion(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, missedBusy, cleanRound)
	h.firstRound("b1")
	h.wantState(2, store.PRRereviewPending)
	h.moveTo(prSpec{head: "b2"})
	h.tick()
	if rec := h.e.completionOf(h.ctx, h.pr(2).ID); rec.Head != "b2" || rec.ReviewID != 0 {
		t.Fatalf("record after the push of b2: %+v, want b2's with no completion waiting", rec)
	}
	h.reviewTo(2, "b2")
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("rounds = %d, want the re-review of b2", len(ins))
	}
	if in := ins[1]; in.TargetSHA != "b2" || in.Complete != nil || in.SameHead {
		t.Fatalf("second round: target %s completion %+v same head %v, want the re-review of b2", in.TargetSHA, in.Complete, in.SameHead)
	}
}

// A clean re-review that held its approval for a full pass
// (approve_after_full_pass) and went without codex-review gets the full
// pass it asked for, every role fresh on the whole PR, not a completion.
func TestAHeldReviewGetsItsFullPassNotACompletion(t *testing.T) {
	autoApproves := func(h *harness) {
		h.cfg.Pipeline.ApproveAfterFullPass = 1
		h.cfg.Watches[0].AutoApprove = []string{"talkable"}
		h.cfg.Watches[0].AutoApproveAs = "zhuravel"
	}
	h := holdHarness(t, []string{store.VerdictBlocking, store.VerdictBlocking, store.VerdictClean}, autoApproves)
	posted := h.rd.script
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		res, err := posted(in)
		if err != nil || !in.HoldApproval {
			return res, err
		}
		reports := []agents.Report{{Role: "claude-review", Path: "claude-review.md", Status: pipeline.ReportOK},
			{Role: "codex-review", Status: pipeline.ReportBusy, Missing: true}}
		if _, err := pipeline.RecordMissingReports(h.ctx, h.st, in.PR.ID, res.Round, in.TargetSHA, reports); err != nil {
			return failRound(h.t, err)
		}
		return res, nil
	}
	h.reviewedPR(2, "b1")
	pushAndRereview(t, h, "b2")
	wantHeld(t, "re-review 2", pushHeld(t, h, 2, "b3"))
	h.flush()
	if q := completeQueued(t, h); len(q) != 0 {
		t.Fatalf("pr.complete_queued events: %+v", q)
	}
	if v, ok := kvValue(h, kvPRComplete(h.pr(2).ID)); ok {
		t.Fatalf("a completion is on record: %s", v)
	}
	ins := h.rd.all()
	if len(ins) != 4 {
		t.Fatalf("rounds = %d, want the full pass after the held review", len(ins))
	}
	if in := ins[3]; !in.FullPass || in.Complete != nil {
		t.Fatalf("the round after the held review: full pass %v completion %+v", in.FullPass, in.Complete)
	}
}

// A completion is magnum's own repair of a round that counted already: it
// counts against neither the daily cap (a PR at its cap is completed all
// the same) nor the re-reviews a scheduled full pass counts, so the next
// push's re-review is no full pass.
func TestCompletionCountsNeitherTodayNorTowardTheFullPass(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice, func(h *harness) {
		h.cfg.Daemon.MaxRoundsPerPRPerDay = 1
		h.cfg.Pipeline.FullPassEvery = 2 // the next re-review would be one, were the completion counted
		h.cfg.Pipeline.FullPassLines, h.cfg.Pipeline.FullPassFiles, h.cfg.Pipeline.FullPassAfter = 0, 0, config.Duration{}
		h.cfg.Pipeline.ApproveAfterFullPass = 0
	})
	pipelineRunKinds(h)
	plan.set(2, missedBusy, cleanRound, cleanRound)
	h.firstRound("b1")
	if pr := h.pr(2); pr.RoundsToday != 1 {
		t.Fatalf("rounds_today = %d after the first review, want 1", pr.RoundsToday)
	}
	h.tick()
	if n := len(h.rd.all()); n != 2 || h.rd.all()[1].Complete == nil {
		t.Fatalf("rounds = %d, want the completion at the daily cap", n)
	}
	if pr := h.wantState(2, store.PRReviewed); pr.RoundsToday != 1 {
		t.Fatalf("rounds_today = %d after the completion, want 1", pr.RoundsToday)
	}
	if n, _ := h.e.rereviewsSinceFullPass(h.ctx, h.pr(2).ID); n != 0 {
		t.Fatalf("re-reviews since the full pass = %d, want the completion not counted", n)
	}
	h.advance(24 * time.Hour) // a new day: the cap lets the push's re-review through
	h.push(2, "b2")
	h.reviewTo(2, "b2")
	if in := h.rd.all()[2]; in.FullPass || in.Complete != nil {
		t.Fatalf("the re-review of b2: full pass %v completion %+v, want an ordinary one", in.FullPass, in.Complete)
	}
}

// pipelineRunKinds has the auto harness's rounds record their judge runs
// as the pipeline does: of the round's kind, with the head reviewed before
// them (the full pass's count reads them).
func pipelineRunKinds(h *harness) {
	script := h.rd.script
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		res, err := script(in)
		if err != nil {
			return res, err
		}
		kind := map[string]string{pipeline.KindRereview: store.RunRereview, pipeline.KindRecovery: store.RunRecovery}[in.Kind]
		if kind == "" {
			kind = store.RunInitial
		}
		if _, err := h.st.DB().ExecContext(h.ctx, `UPDATE runs SET kind = ?, prev_reviewed_sha = ? WHERE pr_id = ? AND round = ?`,
			kind, in.PR.ReviewedSHA, in.PR.ID, res.Round); err != nil {
			return failRound(h.t, err)
		}
		return res, nil
	}
}

// A completion completes a re-review that did not hold its approval for a
// full pass (approve_after_full_pass), and that re-review counts toward the
// next hold now: the completion holds none itself, and its clean review,
// which heard every reviewer, is approved.
func TestACompletionNeverHoldsItsApproval(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice, func(h *harness) {
		h.cfg.Pipeline.ApproveAfterFullPass = 1
		h.cfg.Pipeline.FullPassEvery, h.cfg.Pipeline.FullPassLines, h.cfg.Pipeline.FullPassFiles = 0, 0, 0
		h.cfg.Pipeline.FullPassAfter = config.Duration{}
	})
	pipelineRunKinds(h)
	plan.set(2, blockingRound, missedBusy, cleanRound)
	h.firstRound("b1")
	h.wantState(2, store.PRReviewed)
	h.moveTo(prSpec{head: "b2"})
	h.reviewTo(2, "b2")
	ins := h.rd.all()
	if len(ins) != 3 {
		t.Fatalf("rounds = %d, want the re-review of b2 and its completion", len(ins))
	}
	if in := ins[1]; in.HoldApproval || in.Complete != nil {
		t.Fatalf("the re-review of b2: hold %v completion %+v", in.HoldApproval, in.Complete)
	}
	if in := ins[2]; in.Complete == nil || in.HoldApproval {
		t.Fatalf("the completion: %+v, hold %v: want one that may approve", in.Complete, in.HoldApproval)
	}
	h.flush()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b2:") {
		t.Fatalf("posted = %q, want the approval of b2", got)
	}
}

// While the completion is queued or runs, the status comment's line says
// that magnum runs the missing reviewer again, in fixed words that name no
// role, no status and nobody with @, in the third person; once magnum
// gives up on the head, the line is the one of a review that did not hear
// every reviewer again.
func TestTheStatusLineSaysMagnumRunsTheReviewerAgainUntilItGivesUp(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, missedBusy)
	h.firstRound("b1")
	h.wantState(2, store.PRRereviewPending)
	h.advance(statusEvery)
	h.e.noteStatusComments(h.ctx)
	h.wantApprovalLine(aliceCompleting)
	body := h.statusComment(2).body
	for _, bad := range []string{"codex-review", "busy", "@alice", " you ", "You "} {
		if strings.Contains(body, bad) {
			t.Fatalf("the status comment says %q:\n%s", bad, body)
		}
	}
	h.tick() // completion 1
	h.tick() // completion 2: magnum gives up
	h.flush()
	h.flush()
	if evs := approvalEvents(t, h, 2, "pr.complete_gave_up"); len(evs) != 1 {
		t.Fatalf("pr.complete_gave_up events = %+v, want one", evs)
	}
	h.wantApprovalLine(aliceUnheard)
}

// completionRounds has the auto harness's completion rounds end as end says
// (its outcome; "" = as the plan posts), each with the judge run a
// pipeline round leaves, of the round the engine gave it.
func completionRounds(h *harness, end string) {
	script := h.rd.script
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if in.Complete == nil || end == "" {
			return script(in)
		}
		round := in.Round
		if round == 0 {
			round = len(h.rd.all())
		}
		if _, err := h.st.DB().ExecContext(h.ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login,
  state, outcome, prompt_text, created_at) VALUES (?, ?, ?, 'codex-judge', 'rereview', ?, ?, 'talkable[bot]', 'abandoned', ?, 'p', ?)`,
			"run-c"+strconv.Itoa(len(h.rd.all())), in.PR.ID, round, in.TargetSHA, in.PR.Identity, end, store.FormatTime(h.clock.Now())); err != nil {
			return failRound(h.t, err)
		}
		return pipeline.RoundResult{Outcome: end, Round: round, TargetSHA: in.TargetSHA, Error: end}, nil
	}
}

// The reviewer's Important finding: a completion whose round failed (its
// judge timed out) was retried as a failed round, three times, all as
// attempt 1. Each round a completion starts is one of the head's two, also
// across a restart of the daemon: the second that fails ends them with
// pr.complete_gave_up once, the PR back at its review, and the refusal
// stands.
func TestAFailedCompletionsRetriesNeverPassTheHeadsTwo(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, missedBusy)
	completionRounds(h, pipeline.OutcomeTimeout)
	h.firstRound("b1")
	h.tick() // completion round 1 times out
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want the review and the completion", n)
	}
	h.e = New(h.d) // a restart: the count is the registry's
	h.startup()
	for range 6 {
		h.advance(5 * time.Minute)
		h.tick()
	}
	if n := len(h.rd.all()); n != 3 {
		t.Fatalf("rounds = %d, want the review and two completion rounds", n)
	}
	if evs := approvalEvents(t, h, 2, "pr.complete_gave_up"); len(evs) != 1 {
		t.Fatalf("pr.complete_gave_up events = %+v, want one", evs)
	}
	if q := completeQueued(t, h); len(q) != 1 {
		t.Fatalf("pr.complete_queued events = %+v, want the first alone", q)
	}
	h.wantState(2, store.PRReviewed)
	if got := h.autoRefused(2); !strings.Contains(got, "codex-review (busy)") {
		t.Fatalf("held back for %q, want the reviewer it did not hear", got)
	}
}

// The reviewer's I1: `magnum abort` (the board's take-back) of a completion
// that waits took it back, and the same tick's auto-approval pass queued it
// again. An abort sticks for the head: no completion runs on it, and a new
// head gets its own.
func TestAnAbortOfAWaitingCompletionSticksForItsHead(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, missedBusy, missedBusy, cleanRound)
	h.firstRound("b1")
	h.wantState(2, store.PRRereviewPending)
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "took back") {
		t.Fatalf("abort: %s %q", r.State, deref(r.Result))
	}
	for range 3 {
		h.flush()
	}
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d after the abort, want none more", n)
	}
	if q := completeQueued(t, h); len(q) != 1 {
		t.Fatalf("pr.complete_queued events = %+v, want the one the abort took back", q)
	}
	h.wantState(2, store.PRReviewed)

	h.moveTo(prSpec{head: "b2"})
	h.reviewTo(2, "b2")
	if q := completeQueued(t, h); len(q) != 2 || q[1]["head"] != "b2" {
		t.Fatalf("pr.complete_queued events = %+v, want b2's completion", q)
	}
}

// An abort of a completion that runs sticks too: no second completion
// follows at once, and no later pass queues one.
func TestAnAbortOfARunningCompletionSticksForItsHead(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, missedBusy)
	h.firstRound("b1")
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{}) // the completion waits in the pipeline
	h.rd.mu.Unlock()
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(1)
	h.rd.mu.Lock()
	h.rd.gate = nil // a round the abort lets through would run
	h.rd.mu.Unlock()
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("abort: %s %q", r.State, deref(r.Result))
	}
	for range 3 {
		h.flush()
	}
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d after the abort, want the review and the aborted completion", n)
	}
	if q := completeQueued(t, h); len(q) != 1 {
		t.Fatalf("pr.complete_queued events = %+v, want the first alone", q)
	}
	h.wantState(2, store.PRReviewed)
}

// The reviewer's M1: a head another gate refuses for good (its CI
// workflows, the agents' configuration, more files than GitHub lists) gets
// no completion, and the status comment says that gate's words, not that
// Magnum runs the reviewer again.
func TestNoCompletionForAHeadAnotherGateRefusesForGood(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, missedBusy, cleanRound)
	h.reviewedFor(prSpec{head: "b1", files: aliceWorkflowFiles})
	if q := completeQueued(t, h); len(q) != 0 {
		t.Fatalf("pr.complete_queued events: %+v", q)
	}
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want the review alone", n)
	}
	h.wantApprovalLine(aliceWorkflows)
}

// The reviewer's M2: a completion whose late reviewer misses again posted
// a review that added nothing, twice per head. Now its round prompts no
// judge and posts nothing (pipeline.OutcomeUnheard): the attempt counts,
// the PR is back at its review, and the status comment keeps its line.
func TestACompletionWhoseLateReviewerMissesAgainPostsNoReview(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, missedBusy)
	completionRounds(h, pipeline.OutcomeUnheard)
	h.firstRound("b1")
	review := deref(h.pr(2).LastReviewID)
	h.tick() // completion 1: codex-review misses again
	h.wantState(2, store.PRRereviewPending)
	if q := completeQueued(t, h); len(q) != 2 || q[1]["attempt"] != float64(2) {
		t.Fatalf("pr.complete_queued events = %+v, want attempt 2 queued", q)
	}
	h.advance(statusEvery)
	h.e.noteStatusComments(h.ctx)
	h.wantApprovalLine(aliceCompleting)
	h.tick() // completion 2 misses too: magnum gives up
	h.flush()
	h.flush()
	if evs := approvalEvents(t, h, 2, "pr.complete_gave_up"); len(evs) != 1 {
		t.Fatalf("pr.complete_gave_up events = %+v, want one", evs)
	}
	if n := len(h.rd.all()); n != 3 {
		t.Fatalf("rounds = %d, want the review and two completions", n)
	}
	if pr := h.wantState(2, store.PRReviewed); deref(pr.LastReviewID) != review {
		t.Fatalf("last review %d, want round 1's %d: a completion that heard nobody posts nothing", deref(pr.LastReviewID), review)
	}
	h.wantApprovalLine(aliceUnheard)
}

// The reviewer's M4: a completion is no new review of the author's code, so
// the re-review interval of the next push still runs from the round it
// completes.
func TestACompletionKeepsTheReReviewIntervalOfItsRound(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, missedBusy, cleanRound)
	h.firstRound("b1")
	start := deref(h.pr(2).LastRoundStartedAt)
	h.advance(10 * time.Minute)
	h.tick() // the completion
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want the completion", n)
	}
	if pr := h.wantState(2, store.PRReviewed); !deref(pr.LastRoundStartedAt).Equal(start) {
		t.Fatalf("last_round_started_at = %v, want the completed round's start %v", deref(pr.LastRoundStartedAt), start)
	}
}

// The reviewer's M6 and Codex's minor: a head keeps its completions across
// the close and reopen of its PR, and across a push that went back to it
// before its round: a head that had both gets none more.
func TestAHeadKeepsItsCompletionsThroughACloseAndAPushBack(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, missedBusy)
	h.firstRound("b1")
	h.tick() // completion 1
	h.tick() // completion 2: magnum gives up
	h.flush()
	if n := len(h.rd.all()); n != 3 {
		t.Fatalf("rounds = %d, want the review and two completions", n)
	}
	h.open(prSpec{n: 1, head: "base1"})
	h.gh.mu.Lock()
	h.gh.closed[2] = "CLOSED"
	h.gh.mu.Unlock()
	for range 3 {
		h.advance(30 * time.Second)
		h.tick()
	}
	h.wantState(2, store.PRClosed)
	h.gh.mu.Lock()
	delete(h.gh.closed, 2)
	h.gh.mu.Unlock()
	h.moveTo(prSpec{head: "b1"}) // reopened
	h.tick()
	h.flush()
	h.flush()
	if n := len(h.rd.all()); n != 3 {
		t.Fatalf("rounds = %d after the reopen, want none more", n)
	}
	h.moveTo(prSpec{head: "b2"})
	h.tick() // b2 waits for its quiet period
	h.moveTo(prSpec{head: "b1"})
	h.tick()
	h.flush()
	h.flush()
	if n := len(h.rd.all()); n != 3 {
		t.Fatalf("rounds = %d after the push back to b1, want none more", n)
	}
	if q := completeQueued(t, h); len(q) != 2 {
		t.Fatalf("pr.complete_queued events = %+v, want the head's two", q)
	}
}

// The reviewer's M7: the completion's round was on record only once it
// settled, so a crash in between made the next completion rest on that
// round alone, and its judge lost the reports that stand. The round is on
// record from its start, and the next completion rests on the first base.
func TestACompletionsRoundIsOnRecordFromItsStart(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, missedBusy, missedBusy, cleanRound)
	script := h.rd.script
	var recorded []int
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if in.Complete != nil && in.Complete.Round == 1 {
			recorded = h.e.completionOf(h.ctx, in.PR.ID).Rounds // what a crash now leaves
		}
		return script(in)
	}
	h.firstRound("b1")
	h.tick() // completion 1
	if !slices.Contains(recorded, 2) {
		t.Fatalf("the record held rounds %v while completion round 2 ran, want it", recorded)
	}
	h.tick() // completion 2
	ins := h.rd.all()
	if len(ins) != 3 || ins[2].Complete == nil || ins[2].Complete.Base != 1 || ins[2].Complete.Round != 2 {
		t.Fatalf("rounds = %d, the second completion %+v: want it to complete round 2 on round 1's reports", len(ins), ins[len(ins)-1].Complete)
	}
}
