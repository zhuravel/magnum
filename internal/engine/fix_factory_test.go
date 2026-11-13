package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/fixwork"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// kindsBeforeT2 are the request kinds the daemon handled before the
// interlock (internal/engine/requests.go at d0f76148): any other kind falls
// to its default, which answers it as unknown.
var kindsBeforeT2 = []string{
	ReqKick, ReqReview, ReqPin, ReqUnpin, ReqMute, ReqUnmute, ReqSnooze, ReqCodexFlag, ReqFix, ReqWhiteboard, ReqApprove,
	ReqRequestChanges, ReqUnapprove, ReqAbort, ReqIgnore, ReqPause, ReqResume, ReqStagingDeploy, ReqStagingCancel,
	ReqRestartSessions, ReqIdentityVerdict, ReqRetro, ReqNotesCurate, ReqLearnPropose, ReqLearnReject, ReqLearnLand,
	ReqLearnRevert, ReqRelease, ReqCleanup, ReqOpen, ReqProvision, ReqRepair, ReqAdopt,
}

// reviewPayloadBeforeT2 is ReviewPayload as the build before the interlock
// decoded it, without FixRef and LocalRound.
type reviewPayloadBeforeT2 struct {
	PRTarget
	Again    bool     `json:"again,omitempty"`
	Fresh    bool     `json:"fresh,omitempty"`
	Full     bool     `json:"full,omitempty"`
	Simplify bool     `json:"simplify,omitempty"`
	Roles    []string `json:"roles,omitempty"`
	As       string   `json:"as,omitempty"`
	DryRun   bool     `json:"dry_run,omitempty"`
	Replies  bool     `json:"replies,omitempty"`
}

// The kinds the interlock adds are new to a build from before it, whose
// default answers a kind it does not know as unknown, as this build's does:
// the factory reads that answer and starts nothing on a magnum that cannot
// follow it. A re-review that names the factory's fix fails on such a
// build too, naming the field it predates, instead of running as a plain
// forced round.
func TestAnOlderBuildAnswersTheNewKindsAsUnknown(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	for _, kind := range []string{ReqFixHandover, fixwork.ReqFixAttention} {
		if slices.Contains(kindsBeforeT2, kind) {
			t.Errorf("%s was a kind before the interlock", kind)
		}
		id := h.enqueue(kind, map[string]any{"repo": "talkable/talkable", "number": 2})
		h.handle()
		if r := h.request(id); strings.Contains(deref(r.Result), "unknown request kind") {
			t.Errorf("this build answers %s as unknown: %q", kind, deref(r.Result))
		}
	}
	id := h.enqueue("fix_attention_v2", map[string]any{"repo": "talkable/talkable", "number": 2})
	h.handle()
	if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != `unknown request kind "fix_attention_v2"` {
		t.Errorf("a kind this build does not know: %s %q", r.State, deref(r.Result))
	}

	payload, err := json.Marshal(ReviewPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2},
		FixRef: &fixwork.FixRef{Work: 7, Cycle: 1, Head: "c3c3c3c"}, LocalRound: &fixwork.LocalRound{SHA: "c3c3c3c"}})
	if err != nil {
		t.Fatal(err)
	}
	var old reviewPayloadBeforeT2
	if err := decode(store.Request{ID: 9, Kind: ReqReview, Payload: payload}, &old); err == nil || !strings.Contains(err.Error(), "this daemon predates") {
		t.Errorf("the build before decodes a fix's re-review: %v", err)
	}
	// The factory's abort names its fix: a build before it would stop any round.
	payload, err = json.Marshal(TargetPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}, FixRef: &fixwork.FixRef{Work: 7, Cycle: 1, Head: "c3c3c3c"}})
	if err != nil {
		t.Fatal(err)
	}
	var oldTarget struct {
		PRTarget
		Slot   string `json:"slot,omitempty"`
		Force  bool   `json:"force,omitempty"`
		Reason string `json:"reason,omitempty"`
	}
	if err := decode(store.Request{ID: 10, Kind: ReqAbort, Payload: payload}, &oldTarget); err == nil || !strings.Contains(err.Error(), "this daemon predates") {
		t.Errorf("the build before decodes the factory's abort: %v", err)
	}
}

// The factory's re-review of its fixer's push (a review request with
// FixRef) goes on a PR the factory fixes even while its lease says the
// fixer works, which refuses a plain `magnum review`; the PR keeps the fix
// and its local round until the round that takes it ends, which names them
// (fix.factory_round). A PR the factory does not fix refuses it.
func TestTheFactorysReReviewKeepsItsFixRefToTheRoundsEnd(t *testing.T) {
	lease := &fakeLease{}
	lease.set(FixLease{Fixing: true, Phase: FixPhaseFixing, Head: "b2", Cycle: 1, Cap: 3}, nil)
	h := leaseHarness(t, lease)
	pr := h.pr(2)
	rounds := len(h.rd.all())
	ref := &fixwork.FixRef{Work: 7, Cycle: 1, Head: "b2"}
	local := &fixwork.LocalRound{SHA: "b2", PromptHash: "p1", SkillHash: "s1", File: "/factory/work/7/local-1.json"}
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, FixRef: ref, LocalRound: local})
	h.handle()
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("the factory's re-review: %s %q", r.State, deref(r.Result))
	}
	if r, _ := h.e.fixRefOf(h.ctx, pr.ID); r == nil || r.FixRef != *ref || *r.LocalRound != *local {
		t.Fatalf("the PR keeps %+v, want the fix and its local round", r)
	}
	h.tick()
	if got := len(h.rd.all()); got != rounds+1 {
		t.Fatalf("rounds = %d, want %d: the factory's re-review runs", got, rounds+1)
	}
	ev, ok := h.fixEvent("fix.factory_round")
	if !ok || !strings.Contains(ev.Message, "the re-review of the factory's push of b2 (cycle 1)") {
		t.Fatalf("fix.factory_round = %+v, %v", ev, ok)
	}
	var data struct {
		FixRef     fixwork.FixRef     `json:"fix_ref"`
		LocalRound fixwork.LocalRound `json:"local_round"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil || data.FixRef != *ref || data.LocalRound != *local {
		t.Errorf("fix.factory_round data = %s (%v)", ev.Data, err)
	}
	if r, raw := h.e.fixRefOf(h.ctx, pr.ID); r != nil || raw != "" {
		t.Errorf("the PR keeps the fix after its round: %s", raw)
	}

	h.e.delKV(h.ctx, fixwork.KVPRFixOwner(pr.ID))
	id = h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, FixRef: ref})
	h.handle()
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "the factory does not fix this PR") {
		t.Errorf("a fix's re-review of a PR magnum fixes: %s %q", r.State, deref(r.Result))
	}
}

// A re-review of the factory's push whose judge turn pauses on a usage
// limit or a lost login is not over: the turn continues once the pause
// lifts, as the same round. The PR keeps the factory's fix meanwhile, no
// fix.factory_round says the round ended, and the continued round, which
// posts the review, takes the fix and names it in the one fix.factory_round.
func TestAPausedReReviewKeepsTheFactorysFixForItsContinue(t *testing.T) {
	storetest.Parallel(t)
	for _, outcome := range []string{pipeline.OutcomeUsageLimit, pipeline.OutcomeLoginRequired} {
		t.Run(outcome, func(t *testing.T) {
			h := leaseHarness(t, &fakeLease{})
			pr := h.pr(2)
			h.rd.mu.Lock()
			h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) { return h.rd.posted(h.ctx, in, 0) }
			h.rd.mu.Unlock()
			pausedReReview(h, outcome, true)
			ref := fixwork.FixRef{Work: 7, Cycle: 1, Head: "b2"}
			if r := h.reReviewOf(ref); r.State != store.RequestDone {
				t.Fatalf("the factory's re-review: %s %q", r.State, deref(r.Result))
			}
			h.tick()
			h.wantState(2, store.PRPaused)
			if ev, ok := h.fixEvent("fix.factory_round"); ok {
				t.Errorf("fix.factory_round for a turn that continues: %q", ev.Message)
			}
			if kept, raw := h.e.fixRefOf(h.ctx, pr.ID); kept == nil || kept.FixRef != ref {
				t.Fatalf("the factory's fix = %q, want %+v kept for the continue", raw, ref)
			}

			h.advance(2 * time.Hour) // the pause lifts: the turn continues and posts
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
			h.tick()
			if ins := h.rd.all(); ins[len(ins)-1].Kind != pipeline.KindContinue {
				t.Fatalf("the round after the pause is a %s round, want the continue", ins[len(ins)-1].Kind)
			}
			evs := h.fixEvents("fix.factory_round")
			if len(evs) != 1 {
				t.Fatalf("fix.factory_round events = %d, want the continue's alone", len(evs))
			}
			var data struct {
				FixRef  fixwork.FixRef `json:"fix_ref"`
				Outcome string         `json:"outcome"`
			}
			if err := json.Unmarshal(evs[0].Data, &data); err != nil || data.FixRef != ref || data.Outcome != pipeline.OutcomePosted {
				t.Errorf("fix.factory_round data = %s (%v), want the fix, posted", evs[0].Data, err)
			}
			if r, raw := h.e.fixRefOf(h.ctx, pr.ID); r != nil {
				t.Errorf("the PR keeps the fix after the continue: %s", raw)
			}
		})
	}
}

// pausedFactoryRound leaves PR #2's re-review of the factory's fix ref
// paused mid-turn, as a usage limit or a restart during the judge's turn
// leaves it: paused, forced, the fix kept and no round holding the PR.
func pausedFactoryRound(t *testing.T, ref fixwork.FixRef) *harness {
	t.Helper()
	h := leaseHarness(t, &fakeLease{})
	h.rd.mu.Lock()
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) { return h.rd.posted(h.ctx, in, 0) }
	h.rd.mu.Unlock()
	pausedReReview(h, pipeline.OutcomeUsageLimit, true)
	if r := h.reReviewOf(ref); r.State != store.RequestDone {
		t.Fatalf("the factory's re-review: %s %q", r.State, deref(r.Result))
	}
	h.tick()
	if pr := h.wantState(2, store.PRPaused); !pr.Forced || h.e.roundActive(pr.ID) {
		t.Fatalf("the paused round: forced %v, held %v", pr.Forced, h.e.roundActive(pr.ID))
	}
	return h
}

// The factory's abort of its own round paused mid-turn stops it, as `magnum
// abort` stops a paused round: its turn would otherwise continue later,
// past the hold of the factory's next fixer, and post on an old head. An
// abort naming another fix leaves it paused.
func TestTheFactorysAbortStopsItsOwnRoundPausedMidTurn(t *testing.T) {
	ref := fixwork.FixRef{Work: 7, Cycle: 1, Head: "b2"}
	h := pausedFactoryRound(t, ref)
	pr := h.pr(2)
	abort := func(fix fixwork.FixRef) store.Request {
		t.Helper()
		id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}, FixRef: &fix})
		h.handle()
		return h.request(id)
	}
	if r := abort(fixwork.FixRef{Work: 7, Cycle: 2, Head: "b3"}); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "nothing to abort") {
		t.Errorf("an abort of another fix: %s %q", r.State, deref(r.Result))
	}
	h.wantState(2, store.PRPaused)
	if r := abort(ref); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "aborted talkable/talkable#2") {
		t.Fatalf("the factory's abort of its paused round: %s %q", r.State, deref(r.Result))
	}
	if cur := h.pr(2); cur.State == store.PRPaused || cur.Forced {
		t.Errorf("PR #2 = %s, forced %v; want the paused round stopped", cur.State, cur.Forced)
	}
	if r, raw := h.e.fixRefOf(h.ctx, pr.ID); r != nil {
		t.Errorf("the PR keeps the fix: %s", raw)
	}
}

// The factory's abort reads which round holds the PR under the same lock
// its stop order goes in with: whatever holds the PR by then and is not a
// round of its fix (a whiteboard writer that took the PR as the factory's
// round ended) gets no order and runs on, and only a round of that fix is
// stopped, its order marked as the factory's.
func TestTheFactorysAbortChoosesItsRoundUnderTheStopsLock(t *testing.T) {
	h := leaseHarness(t, &fakeLease{})
	repo, pr, err := h.e.resolve(h.ctx, PRTarget{Ref: "2"})
	if err != nil {
		t.Fatal(err)
	}
	ref := fixwork.FixRef{Work: 7, Cycle: 1, Head: "b2"}
	var cancels atomic.Int32
	hold := func(rh *roundHandle) *roundHandle {
		t.Helper()
		rh.cancel = func() { cancels.Add(1) }
		if !h.e.reserve(pr.ID, rh) {
			t.Fatal("the PR could not be reserved")
		}
		return rh
	}
	wb := hold(&roundHandle{whiteboard: true})
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}, FixRef: &ref})
	if !h.e.stopHeld(h.ctx, id, repo, pr, false, &ref) {
		t.Fatal("stopHeld reports no holder")
	}
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "is not the factory's re-review of b2 (cycle 1): it runs on") {
		t.Errorf("the factory's abort of a whiteboard writer: %s %q", r.State, deref(r.Result))
	}
	if n := cancels.Load(); n != 0 || len(wb.stop.reqs) != 0 {
		t.Errorf("the whiteboard writer got the factory's stop: %d cancels, orders %+v", n, wb.stop.reqs)
	}
	h.e.unreserve(pr.ID) // which cancels the writer's context
	cancels.Store(0)
	round := hold(&roundHandle{fixRef: &ref})
	id = h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}, FixRef: &ref})
	if !h.e.stopHeld(h.ctx, id, repo, pr, false, &ref) || cancels.Load() != 1 {
		t.Fatal("the round of the factory's fix was not stopped")
	}
	if len(round.stop.reqs) != 1 || !round.stop.reqs[0].fix {
		t.Errorf("orders = %+v, want the factory's", round.stop.reqs)
	}
	h.e.unreserve(pr.ID)
}

// factoryRoundOf is the outcome and the stop's author the one
// fix.factory_round event of PR #2 names for fix; it fails the test unless
// exactly one names it.
func factoryRoundOf(t *testing.T, h *harness, fix fixwork.FixRef) (outcome, by string) {
	t.Helper()
	evs := h.fixEvents("fix.factory_round")
	if len(evs) != 1 {
		t.Fatalf("fix.factory_round events = %d, want 1", len(evs))
	}
	var data struct {
		FixRef  fixwork.FixRef `json:"fix_ref"`
		Outcome string         `json:"outcome"`
		By      string         `json:"by"`
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || data.FixRef != fix {
		t.Fatalf("fix.factory_round data = %s (%v), want the fix %+v", evs[0].Data, err, fix)
	}
	return data.Outcome, data.By
}

// Your `magnum abort` or `magnum ignore` of the factory's re-review, in line
// or in flight, ends that round: the PR's fix goes, and fix.factory_round
// names it with the stop, so the factory's loop stops waiting for it. So
// does the factory's own abort.
func TestAStopOfTheFactorysReReviewNamesItsFix(t *testing.T) {
	storetest.Parallel(t)
	ref := fixwork.FixRef{Work: 7, Cycle: 1, Head: "b2"}
	for _, tc := range []struct {
		name            string
		running, ignore bool
		fix             *fixwork.FixRef
		by              string
	}{
		{"your abort of it in line", false, false, nil, "magnum abort"},
		{"your ignore of it in line", false, true, nil, "magnum ignore"},
		{"your abort of it in flight", true, false, nil, "magnum abort"},
		{"your ignore of it in flight", true, true, nil, "magnum ignore"},
		{"the factory's abort of it in line", false, false, &ref, "the factory's abort"},
		{"the factory's abort of it in flight", true, false, &ref, "the factory's abort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := leaseHarness(t, &fakeLease{})
			if r := h.reReviewOf(ref); r.State != store.RequestDone {
				t.Fatalf("the factory's re-review: %s %q", r.State, deref(r.Result))
			}
			if tc.running {
				h.gatedTick()
			}
			kind := ReqAbort
			if tc.ignore {
				kind = ReqIgnore
			}
			id := h.enqueue(kind, TargetPayload{PRTarget: PRTarget{Ref: "2"}, FixRef: tc.fix})
			h.handle()
			if r := h.request(id); r.State != store.RequestDone {
				t.Fatalf("%s: %s %q", kind, r.State, deref(r.Result))
			}
			if outcome, by := factoryRoundOf(t, h, ref); outcome != pipeline.OutcomeStopped || by != tc.by {
				t.Errorf("fix.factory_round = %s by %q, want stopped by %q", outcome, by, tc.by)
			}
			if r, raw := h.e.fixRefOf(h.ctx, h.pr(2).ID); r != nil {
				t.Errorf("the PR keeps the fix: %s", raw)
			}
		})
	}
}

// The factory raises the attention of a reviewed PR it fixes, silently (no
// toast), with its why as the PR's message and kind fix, and lifts it while
// it is still that one; it touches no PR magnum fixes.
func TestTheFactoryRaisesAndLiftsAPRsAttentionSilently(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	factoryOwns(h)
	ask := func(raise bool) store.Request {
		t.Helper()
		id := h.enqueue(fixwork.ReqFixAttention, fixwork.AttentionPayload{Repo: "talkable/talkable", Number: 2, Raise: raise, Cause: "needs_you",
			Why: "1 call the fixer left needs you: https://github.com/talkable/talkable/pull/2#discussion_r4100"})
		h.handle()
		return h.request(id)
	}
	const msg = "fix: 1 call the fixer left needs you: https://github.com/talkable/talkable/pull/2#discussion_r4100"
	if r := ask(true); r.State != store.RequestDone {
		t.Fatalf("raise: %s %q", r.State, deref(r.Result))
	}
	pr := h.wantState(2, store.PRNeedsAttention)
	if deref(pr.LastError) != msg {
		t.Errorf("last_error = %q, want %q", deref(pr.LastError), msg)
	}
	if kind, _ := kvValue(h, KVPRAttention(pr.ID)); kind != "fix" {
		t.Errorf("attention kind = %q, want fix", kind)
	}
	h.wantToasts("needs attention", 0)
	if r := ask(false); r.State != store.RequestDone {
		t.Fatalf("lift: %s %q", r.State, deref(r.Result))
	}
	h.wantState(2, store.PRReviewed)
	if ev, ok := h.fixEvent("fix.factory_attention"); !ok || !strings.Contains(ev.Message, "lifted") {
		t.Errorf("fix.factory_attention = %+v, %v", ev, ok)
	}

	h.e.delKV(h.ctx, fixwork.KVPRFixOwner(pr.ID))
	if r := ask(true); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "the factory does not fix this PR") {
		t.Errorf("raise on a PR magnum fixes: %s %q", r.State, deref(r.Result))
	}
	h.wantState(2, store.PRReviewed)
}

// gatedTick runs a tick whose round, once dispatched, stays in the pipeline
// until its context ends, and waits for it to get there.
func (h *harness) gatedTick() {
	h.t.Helper()
	before := len(h.rd.all())
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.mu.Unlock()
	if err := h.e.Tick(h.ctx); err != nil {
		h.t.Fatalf("tick: %v", err)
	}
	h.awaitRound(before)
}

// reReviewOf asks for the factory's re-review of PR #2's push for fix and
// returns the request.
func (h *harness) reReviewOf(fix fixwork.FixRef) store.Request {
	h.t.Helper()
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, FixRef: &fix})
	h.handle()
	return h.request(id)
}

// The factory's abort names its fix (TargetPayload.FixRef) and stops only
// the round that re-reviews it: a round in flight for another fix, or for
// no fix at all, runs on, and the request says why; a forced re-review of
// its fix still in line is taken back. The operator's abort, naming no fix,
// stops any round as before.
func TestTheFactorysAbortStopsOnlyItsOwnRound(t *testing.T) {
	storetest.Parallel(t)
	ref := fixwork.FixRef{Work: 7, Cycle: 1, Head: "b2"}
	other := fixwork.FixRef{Work: 7, Cycle: 2, Head: "b3"}
	abort := func(h *harness, fix *fixwork.FixRef) store.Request {
		h.t.Helper()
		id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}, FixRef: fix})
		h.tick()
		return h.request(id)
	}
	t.Run("its own round", func(t *testing.T) {
		h := leaseHarness(t, &fakeLease{})
		if r := h.reReviewOf(ref); r.State != store.RequestDone {
			t.Fatalf("the factory's re-review: %s %q", r.State, deref(r.Result))
		}
		h.gatedTick()
		pr := h.pr(2)
		id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}, FixRef: &other})
		h.e.handleRequests(h.ctx)
		if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "is not the factory's re-review of b3 (cycle 2): it runs on") {
			t.Errorf("an abort of another fix: %s %q", r.State, deref(r.Result))
		}
		if !h.e.roundActive(pr.ID) {
			t.Fatal("the round stopped for another fix's abort")
		}
		if r := abort(h, &ref); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "aborted talkable/talkable#2") {
			t.Errorf("the factory's abort of its round: %s %q", r.State, deref(r.Result))
		}
		if h.e.roundActive(pr.ID) || !h.hasEvent("pr:talkable/talkable#2", "pr.aborted") {
			t.Error("the factory's round was not aborted")
		}
	})
	t.Run("another round", func(t *testing.T) {
		h := leaseHarness(t, &fakeLease{})
		h.gatedTick() // the automatic re-review of b2
		pr := h.pr(2)
		id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}, FixRef: &ref})
		h.e.handleRequests(h.ctx)
		if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "is not the factory's re-review of b2 (cycle 1): it runs on") {
			t.Errorf("the factory's abort of a round not its own: %s %q", r.State, deref(r.Result))
		}
		if !h.e.roundActive(pr.ID) {
			t.Fatal("the round stopped for the factory's abort")
		}
		if r := abort(h, nil); r.State != store.RequestDone { // the operator's abort
			t.Errorf("magnum abort: %s %q", r.State, deref(r.Result))
		}
		if h.e.roundActive(pr.ID) {
			t.Error("magnum abort did not stop the round")
		}
	})
	t.Run("in line", func(t *testing.T) {
		h := leaseHarness(t, &fakeLease{})
		h.reReviewOf(ref)
		id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}, FixRef: &other})
		h.handle()
		if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "nothing to abort") {
			t.Errorf("an abort of another fix in line: %s %q", r.State, deref(r.Result))
		}
		if !h.pr(2).Forced {
			t.Fatal("the factory's re-review was taken back for another fix's abort")
		}
		id = h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}, FixRef: &ref})
		h.handle()
		if r := h.request(id); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "took back the forced review") {
			t.Errorf("the factory's abort of its re-review in line: %s %q", r.State, deref(r.Result))
		}
		if pr := h.pr(2); pr.Forced {
			t.Error("the factory's re-review stays forced")
		}
		if r, raw := h.e.fixRefOf(h.ctx, h.pr(2).ID); r != nil {
			t.Errorf("the PR keeps the fix: %s", raw)
		}
	})
}
