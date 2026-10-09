package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// judgeRun records a judge run of round 1 the way the pipeline leaves it:
// prompted (submitted_at) and finished in state with outcome and error.
// Round scripts call it, so it returns its error.
func (h *harness) judgeRun(pr store.PR, kind, target, state, outcome, errMsg string) (store.Run, error) {
	run, err := h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: 1, Role: store.RoleJudge, Kind: kind,
		TargetSHA: target, Identity: pr.Identity, ReviewerLogin: "talkable[bot]", State: store.RunPending, PromptText: "judge"})
	if err != nil {
		return store.Run{}, err
	}
	now := h.clock.Now()
	if err := h.st.TransitionRun(h.ctx, run.ID, nil, state, func(u *store.RunUpdate) {
		u.Set("submitted_at", now)
		if outcome != "" {
			u.Set("outcome", outcome)
		}
		if errMsg != "" {
			u.Set("error", errMsg)
		}
	}); err != nil {
		return store.Run{}, err
	}
	return run, nil
}

// A judge turn that pauses twice (and was nudged in between) keeps quoting
// the original run's marker: the pipeline finishes every paused run as
// failed with an error, which must not break the lineage.
func TestSecondPauseKeepsTheOriginalMarker(t *testing.T) {
	h := newHarness(t)
	var marker string
	var continued []pipeline.RoundInput
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if in.PR.Number != 2 {
			return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: 1, ReviewID: 7, Event: "COMMENTED"}, nil
		}
		pause := func(kind string) (pipeline.RoundResult, error) {
			if _, err := h.judgeRun(in.PR, kind, in.TargetSHA, store.RunFailed, pipeline.OutcomeUsageLimit, "judge pane: usage limit"); err != nil {
				return failRound(t, err)
			}
			return pipeline.RoundResult{Outcome: pipeline.OutcomeUsageLimit, Round: 1, JudgeRunID: marker,
				Pause: &pipeline.Pause{Kind: string(agents.HealthUsageLimit), Tool: agents.KindCodex, Until: h.clock.Now().Add(time.Hour)}}, nil
		}
		if in.Kind != pipeline.KindContinue {
			run, err := h.judgeRun(in.PR, store.RunInitial, in.TargetSHA, store.RunFailed, pipeline.OutcomeUsageLimit, "judge pane: usage limit")
			if err != nil {
				return failRound(t, err)
			}
			marker = run.ID
			return pause(store.RunNudge) // the nudge of the same turn paused too
		}
		continued = append(continued, in)
		if len(continued) == 1 {
			return pause(store.RunContinue)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: in.Round, ReviewID: 9, Event: "COMMENTED"}, nil
	}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRPaused)

	h.advance(time.Hour)
	h.tick() // first continue: pauses again
	h.wantState(2, store.PRPaused)
	h.advance(time.Hour)
	h.tick() // second continue: posts
	h.wantState(2, store.PRReviewed)

	if len(continued) != 2 {
		t.Fatalf("continue rounds: %d", len(continued))
	}
	for i, in := range continued {
		if in.ContinueRunID != marker || in.Round != 1 || in.TargetSHA != "b1" {
			t.Fatalf("continue %d: marker %q round %d target %q, want marker %q round 1 at b1", i+1, in.ContinueRunID, in.Round, in.TargetSHA, marker)
		}
	}
}

// Crash recovery decides from the run rows: a reviewing PR whose judge run
// belongs to an earlier, finished round (the crash hit between "PR to
// reviewing" and the new round's runs) goes back in line; a PR claimed to
// continue a paused turn pauses again instead of losing the turn.
func TestCrashRecoveryResumesOnlyAnUnfinishedJudgeTurn(t *testing.T) {
	h := newHarness(t)
	p2 := h.reviewedPR(2, "b1")
	if _, err := h.judgeRun(p2, store.RunInitial, "b1", store.RunVerified, pipeline.OutcomePosted, ""); err != nil {
		t.Fatal(err)
	}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"}, prSpec{n: 3, head: "c1"})
	h.advance(time.Minute)
	h.tick()
	p3 := h.pr(3)
	if _, err := h.judgeRun(p3, store.RunInitial, "c1", store.RunFailed, pipeline.OutcomeUsageLimit, "judge pane: usage limit"); err != nil {
		t.Fatal(err)
	}

	// The crash: #2's new round had moved it to reviewing, #3 was being
	// claimed to continue its paused turn.
	h.advance(time.Minute)
	if err := h.st.TransitionPR(h.ctx, p2.ID, nil, store.PRReviewing, func(u *store.PRUpdate) {
		u.Set("last_round_started_at", h.clock.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionPR(h.ctx, p3.ID, nil, store.PRClaiming, nil); err != nil {
		t.Fatal(err)
	}
	e2 := New(h.d)
	e2.recoverRows(h.ctx)
	h.wantState(2, store.PRRereviewPending)
	h.wantState(3, store.PRPaused)
}

// The dry-run marker belongs to its forced request: a marker left without
// one is not inherited by an automatic round (which posts).
func TestStaleDryRunMarkerIsNotInherited(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.e.setKV(h.ctx, store.KVPRDryRun(pr.ID), store.PRReviewed) // left behind; the PR is not forced
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(40 * time.Minute)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].DryRun {
		t.Fatalf("rounds: %+v", ins)
	}
	if p := h.wantState(2, store.PRReviewed); deref(p.ReviewedSHA) != "b2" {
		t.Fatalf("reviewed_sha %q", deref(p.ReviewedSHA))
	}
	if _, ok := h.e.getKV(h.ctx, store.KVPRDryRun(pr.ID)); ok {
		t.Fatal("stale dry-run marker kept")
	}
}

// A dry-run request replayed after a crash (or asked twice) keeps the state
// the PR had before the first, so the PR does not come back queued and get a
// real round.
func TestReplayedDryRunRequestKeepsTheOriginalState(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	for range 2 {
		if _, err := h.e.requestReview(h.ctx, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, DryRun: true}); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := h.e.getKV(h.ctx, store.KVPRDryRun(pr.ID)); v != store.PRReviewed {
		t.Fatalf("marker = %q, want reviewed", v)
	}
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if !in.DryRun {
			t.Error("a dry-run request posted")
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun, Event: "COMMENTED"}, nil
	}
	h.tick()
	h.wantState(2, store.PRReviewed)
	h.advance(time.Hour)
	h.tick()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds: %d (a real round followed the dry run)", n)
	}
}

// A dry run never posts: when its marker cannot be read the round does not
// start (the setup fails without using the retry budget). With the kv
// table gone the PR's Codex flag cannot be read either, which holds it out
// of dispatch before its setup (DECISIONS "A Codex flag fails closed"), so
// the setup's own check is asked directly.
func TestDryRunMarkerReadFailureFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	if _, err := h.e.requestReview(h.ctx, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB().ExecContext(h.ctx, "DROP TABLE kv"); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("a round ran without its dry-run mode: %d rounds", n)
	}
	pr := h.wantState(2, store.PRRereviewPending)
	if pr.Attempts != 0 {
		t.Fatalf("attempts %d", pr.Attempts)
	}
	if _, err := h.e.dryRunRound(h.ctx, pr); err == nil || !strings.Contains(err.Error(), "dry-run marker") {
		t.Fatalf("dryRunRound = %v, want the marker's read error", err)
	}
	if _, _, serr := h.e.prepare(h.ctx, &roundJob{pr: pr}); serr == nil || !serr.noCharge || !strings.Contains(serr.Error(), "dry-run marker") {
		t.Fatalf("prepare = %v, want an uncharged setup failure on the marker", serr)
	}
}

// A dry run asked at a reviewed PR returns it to reviewed only while its
// head is still the reviewed commit; a push before the dry round leaves it
// waiting for a real re-review.
func TestDryRunAfterAPushDoesNotParkTheNewHeadAsReviewed(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if in.DryRun {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun, Event: "COMMENTED"}, nil
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: 2, ReviewID: 9, Event: "COMMENTED"}, nil
	}
	// Someone types in the PR's panes: dispatch waits (human_cooldown 15m).
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("human_active_at", h.clock.Now()) }); err != nil {
		t.Fatal(err)
	}
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, DryRun: true})
	h.tick() // marker "reviewed"; the round waits
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick() // the push
	h.advance(15 * time.Minute)
	h.tick() // the dry round, at b2
	ins := h.rd.all()
	if len(ins) != 2 || !ins[1].DryRun || ins[1].TargetSHA != "b2" {
		t.Fatalf("rounds: %+v", ins)
	}
	p := h.wantState(2, store.PRRereviewPending)
	if deref(p.ReviewedSHA) != "b1" || p.Forced {
		t.Fatalf("after the dry run: reviewed_sha %q forced %v", deref(p.ReviewedSHA), p.Forced)
	}
}

// requeueMovedHead: a reviewed PR whose head moved between finish's read and
// its transition goes to rereview_pending; a PR at its reviewed head stays.
func TestRequeueMovedHeadAfterTheReviewTransition(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	w := *h.cfg.WatchFor("talkable/talkable")
	h.e.requeueMovedHead(h.ctx, w, pr.ID, h.clock.Now())
	h.wantState(2, store.PRReviewed)

	// What the poller wrote while the PR was still reviewing.
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) {
		u.Set("head_sha", "b2")
		u.Set("pending_since", h.clock.Now())
	}); err != nil {
		t.Fatal(err)
	}
	h.e.requeueMovedHead(h.ctx, w, pr.ID, h.clock.Now())
	if p := h.wantState(2, store.PRRereviewPending); p.NextEligibleAt == nil {
		t.Fatal("no next_eligible_at")
	}
}

// --once runs the heavy worker alongside the observation loop: a round that
// completes only once it is observed finishes while a slow reconcile runs.
func TestRunOnceObservesRoundsWhileHeavyWorkRuns(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)

	h.inv.gate = make(chan struct{}) // the startup reconcile of the --once run waits for the round
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		start := h.ag.count("observe")
		deadline := time.Now().Add(3 * time.Second) // shorter than the scan's fallback
		for h.ag.count("observe") < start+2 {
			if time.Now().After(deadline) {
				return pipeline.RoundResult{Outcome: pipeline.OutcomeTimeout, Error: "never observed"}, nil
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(h.inv.gate)
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: 1, ReviewID: 5, Event: "COMMENTED"}, nil
	}
	if err := h.e.Run(h.ctx, Options{Once: true, NoSignals: true}); err != nil {
		t.Fatal(err)
	}
	h.wantState(2, store.PRReviewed)
}

// A filter relaxed in the config (read at startup) brings an ineligible PR
// back in line on the next start, without waiting for a change on GitHub.
func TestStartupReclassifiesIneligiblePRs(t *testing.T) {
	h := newHarness(t)
	w := &h.cfg.Watches[0]
	w.SkipAuthors = append(w.SkipAuthors, "alice")
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.wantState(2, store.PRIneligible)

	w.SkipAuthors = slices.DeleteFunc(w.SkipAuthors, func(a string) bool { return a == "alice" })
	h.e = New(h.d) // the daemon restarts with the new config
	h.startup()
	h.wantState(2, store.PRQueued)
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)
}

// A `magnum open` restore holds its PR like a round but is not a review: it
// takes no max_concurrent_reviews capacity and the tab bar does not count it.
func TestOpenRestoresAreNotReviewRounds(t *testing.T) {
	h := newHarness(t)
	if !h.e.reserve(42, &roundHandle{cancel: func() {}, open: true}) {
		t.Fatal("reserve an open")
	}
	defer h.e.unreserve(42)
	if h.e.reserve(42, &roundHandle{cancel: func() {}}) || !h.e.roundActive(42) {
		t.Fatal("a round reserved a PR an open holds")
	}
	if h.e.reserveEviction(42) {
		t.Fatal("an eviction reserved a PR an open holds")
	}
	if n := h.e.reviewRounds(); n != 0 {
		t.Fatalf("review rounds = %d", n)
	}
	if bar := h.e.tabBar(h.ctx); strings.Contains(bar, "reviewing") {
		t.Fatalf("tab bar = %q", bar)
	}
}
