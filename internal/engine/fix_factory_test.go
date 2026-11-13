package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/fixwork"
	"github.com/zhuravel/magnum/internal/herdr"
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

// The end of the factory's re-review is written once, by whichever end
// takes the PR's fix row first (C6): a stop that ended the fix first
// leaves the round's own end nothing to write (the round names its end
// while it still holds the PR, so none the tick serves comes between,
// TestTheFactorysRoundNamesItsEndBeforeItLetsGoOfThePR); a round's end
// that came first leaves a later stop
// nothing to end; and the operator's review that takes the place of a fix
// kept without a forced round ends it once, by "magnum review", so no
// deleter of the row is silent.
func TestTheEndOfTheFactorysReReviewIsWrittenOnce(t *testing.T) {
	storetest.Parallel(t)
	ref := fixwork.FixRef{Work: 7, Cycle: 1, Head: "b2"}
	// job is the round of PR #2 that took the fix, as dispatch takes it.
	job := func(h *harness) *roundJob {
		h.t.Helper()
		if r := h.reReviewOf(ref); r.State != store.RequestDone {
			h.t.Fatalf("the factory's re-review: %s %q", r.State, deref(r.Result))
		}
		repo, pr, err := h.e.resolve(h.ctx, PRTarget{Ref: "2"})
		if err != nil {
			h.t.Fatal(err)
		}
		j := &roundJob{pr: pr, repo: repo}
		if j.fixRef, j.fixRefRaw = h.e.fixRefOf(h.ctx, pr.ID); j.fixRef == nil {
			h.t.Fatal("no fix kept")
		}
		return j
	}
	ended := pipeline.RoundResult{Outcome: pipeline.OutcomeError, Round: 2, TargetSHA: "b2"}
	t.Run("a stop, then the round's end", func(t *testing.T) {
		h := leaseHarness(t, &fakeLease{})
		j := job(h)
		h.e.endFixRef(h.ctx, j.repo, j.pr, pipeline.OutcomeStopped, "magnum abort")
		h.e.fixRefAfterRound(h.ctx, j, ended)
		if outcome, by := factoryRoundOf(t, h, ref); outcome != pipeline.OutcomeStopped || by != "magnum abort" {
			t.Errorf("fix.factory_round = %s by %q, want the stop's alone", outcome, by)
		}
	})
	t.Run("the round's end, then a stop", func(t *testing.T) {
		h := leaseHarness(t, &fakeLease{})
		j := job(h)
		h.e.fixRefAfterRound(h.ctx, j, ended)
		h.e.endFixRef(h.ctx, j.repo, j.pr, pipeline.OutcomeStopped, "magnum abort")
		if outcome, _ := factoryRoundOf(t, h, ref); outcome != pipeline.OutcomeError {
			t.Errorf("fix.factory_round = %s, want the round's alone", outcome)
		}
	})
	t.Run("your review takes the place of a fix kept without its round", func(t *testing.T) {
		h := leaseHarness(t, &fakeLease{})
		j := job(h)
		if err := h.st.UpdatePR(h.ctx, j.pr.ID, func(u *store.PRUpdate) { u.Set("forced", false) }); err != nil {
			t.Fatal(err)
		}
		review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
		h.handle()
		if r := h.request(review); r.State != store.RequestDone {
			t.Fatalf("magnum review: %s %q", r.State, deref(r.Result))
		}
		h.e.fixRefAfterRound(h.ctx, j, ended)
		if outcome, by := factoryRoundOf(t, h, ref); outcome != pipeline.OutcomeStopped || by != "magnum review" {
			t.Errorf("fix.factory_round = %s by %q, want stopped by your review", outcome, by)
		}
	})
}

// logHook is a logger that calls on with each record before its handler
// writes it.
type logHook struct {
	slog.Handler
	on func(slog.Record)
}

func (l logHook) Handle(ctx context.Context, r slog.Record) error {
	l.on(r)
	return l.Handler.Handle(ctx, r)
}

func (l logHook) WithAttrs(as []slog.Attr) slog.Handler {
	return logHook{Handler: l.Handler.WithAttrs(as), on: l.on}
}

func (l logHook) WithGroup(name string) slog.Handler {
	return logHook{Handler: l.Handler.WithGroup(name), on: l.on}
}

// The round of the factory's fix names its end while it still holds the PR
// (the engine owner's ruling, C6's window): a `magnum review` the tick
// serves meanwhile is refused as a round in progress, and a stop is told to
// ask again shortly, so neither can end the fix as "stopped" before the
// round says how it ended. Here the fix.factory_round event is written while
// the PR's reservation is still the round's.
func TestTheFactorysRoundNamesItsEndBeforeItLetsGoOfThePR(t *testing.T) {
	var prID atomic.Int64
	var seen, held atomic.Bool
	h := newHarness(t, func(h *harness) {
		h.d.FixLeaseOf = (&fakeLease{}).reader(h.clock.Now)
		h.d.Logger = slog.New(logHook{Handler: slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}),
			on: func(r slog.Record) {
				if strings.Contains(r.Message, "ended posted: the re-review of the factory's push of b2") {
					seen.Store(true)
					held.Store(h.e.roundActive(prID.Load()))
				}
			}})
	})
	prID.Store(h.reviewedPR(2, "b1").ID)
	factoryOwns(h)
	pollPR(h, time.Hour, 2, "b2")
	h.advance(6 * time.Minute)
	ref := fixwork.FixRef{Work: 7, Cycle: 1, Head: "b2"}
	if r := h.reReviewOf(ref); r.State != store.RequestDone {
		t.Fatalf("the factory's re-review: %s %q", r.State, deref(r.Result))
	}
	h.tick()
	if outcome, _ := factoryRoundOf(t, h, ref); outcome != pipeline.OutcomePosted {
		t.Fatalf("fix.factory_round = %s, want posted", outcome)
	}
	if !seen.Load() || !held.Load() {
		t.Errorf("fix.factory_round written (%v) while the round held the PR (%v): want it written before the round let go of the PR",
			seen.Load(), held.Load())
	}
}

// A clean re-review of the factory's push that holds its approval
// (approve_after_full_pass reached) posts a comment and forces the full
// pass from inside its own round (settleApprovalHold). That forced round
// carries no fix, and must not end the fix the posting round holds (C4):
// the factory hears the one end of its re-review, posted, with the review,
// and the full pass that follows carries no fix of the factory's.
func TestAHeldCleanFactoryReReviewReportsItsPostedEnd(t *testing.T) {
	h := holdHarness(t, []string{store.VerdictBlocking, store.VerdictBlocking, store.VerdictBlocking, store.VerdictClean})
	h.rd.mu.Lock()
	planned := h.rd.script
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		res, err := planned(in)
		if err == nil {
			res.ReviewURL = fmt.Sprintf("https://github.com/talkable/talkable/pull/2#pullrequestreview-%d", res.ReviewID)
		}
		return res, err
	}
	h.rd.mu.Unlock()
	h.reviewedPR(2, "b1")
	pushAndRereview(t, h, "b2")
	pushAndRereview(t, h, "b3")
	factoryOwns(h)
	h.advance(30 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b4"})
	h.tick()
	ref := fixwork.FixRef{Work: 7, Cycle: 3, Head: "b4"}
	if r := h.reReviewOf(ref); r.State != store.RequestDone {
		t.Fatalf("the factory's re-review: %s %q", r.State, deref(r.Result))
	}
	h.advance(5 * time.Minute)
	h.tick()
	ins := h.rd.all()
	wantHeld(t, "the factory's re-review", ins[len(ins)-1])
	if pr := h.wantState(2, store.PRRereviewPending); !pr.Forced || deref(pr.LastReviewEvent) != "COMMENTED" {
		t.Fatalf("after the held re-review: forced %v, event %q; want its comment and the full pass forced", pr.Forced, deref(pr.LastReviewEvent))
	}
	var data struct {
		FixRef    fixwork.FixRef `json:"fix_ref"`
		Outcome   string         `json:"outcome"`
		ReviewURL string         `json:"review_url"`
		By        string         `json:"by"`
	}
	ended := func() {
		t.Helper()
		evs := h.fixEvents("fix.factory_round")
		if len(evs) != 1 {
			t.Fatalf("fix.factory_round events = %+v, want the re-review's one end", evs)
		}
		if err := json.Unmarshal(evs[0].Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.FixRef != ref || data.Outcome != pipeline.OutcomePosted || !strings.Contains(data.ReviewURL, "#pullrequestreview-") || data.By != "" {
			t.Errorf("fix.factory_round = %s, want the fix posted with its review", evs[0].Data)
		}
	}
	ended()
	if r, raw := h.e.fixRefOf(h.ctx, h.pr(2).ID); r != nil {
		t.Errorf("the PR keeps the fix after its round: %s", raw)
	}
	h.advance(time.Minute)
	h.tick() // the full pass before the approval
	if ins := h.rd.all(); len(ins) != 5 || !ins[4].FullPass {
		t.Fatalf("rounds = %d, want the full pass after the held re-review", len(ins))
	}
	ended()
}

// closePR2 merges PR #2 on GitHub and lets the poll confirm it (twice, a
// confirmGap apart): the PR moves to closed.
func closePR2(h *harness) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"}) // #2 left the OPEN list
	h.gh.mu.Lock()
	h.gh.closed[2] = "MERGED"
	h.gh.mu.Unlock()
	h.tick()
	h.advance(confirmGap)
	h.tick()
	h.wantState(2, store.PRClosed)
}

// A PR that merges while the factory's re-review of its fix waits (here
// paused mid-turn, whose continue a closed PR never gets) ends that fix
// with the close: fix.factory_round names it, closed, and the row goes, so
// the factory's loop stops waiting and the operator's post-merge review is
// not refused as one that "waits in line" (C5). A fix row a closed PR still
// holds refuses nothing, and `magnum ignore` of a closed PR ends it too.
func TestAClosedPRsFactoryReReviewEndsWithTheClose(t *testing.T) {
	ref := fixwork.FixRef{Work: 7, Cycle: 1, Head: "b2"}
	h := pausedFactoryRound(t, ref)
	pr := h.pr(2)
	closePR2(h)
	if outcome, by := factoryRoundOf(t, h, ref); outcome != pipeline.OutcomeClosed || by != "the PR's merge on GitHub" {
		t.Errorf("fix.factory_round = %s by %q, want closed by the merge", outcome, by)
	}
	if r, raw := h.e.fixRefOf(h.ctx, pr.ID); r != nil {
		t.Errorf("the closed PR keeps the fix: %s", raw)
	}

	// A row the close left (a build before this one): it refuses no review,
	// and an ignore of the closed PR ends it.
	h.e.setKV(h.ctx, kvPRFixRef(pr.ID), rowJSON(fixRefRow{FixRef: ref, At: h.clock.Now().UTC()}))
	if why := h.e.factoryReviewRefusal(h.ctx, h.pr(2), "talkable/talkable#2"); why != "" {
		t.Errorf("the refusal of a closed PR's review: %q", why)
	}
	id := h.enqueue(ReqIgnore, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("magnum ignore: %s %q", r.State, deref(r.Result))
	}
	evs := h.fixEvents("fix.factory_round")
	if len(evs) != 2 || !strings.Contains(evs[len(evs)-1].Message, "ended closed: magnum ignore") {
		t.Errorf("fix.factory_round events = %+v, want the ignore's, closed", evs)
	}
	if r, raw := h.e.fixRefOf(h.ctx, pr.ID); r != nil {
		t.Errorf("the closed PR keeps the fix after the ignore: %s", raw)
	}
	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(review); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "post-merge") {
		t.Errorf("the post-merge review: %s %q", r.State, deref(r.Result))
	}
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

// The factory alerts the operator itself, so its raise and lift of a PR's
// attention say nothing on magnum's side: no toast of any title, urgent or
// batched, and no reveal of a pane, even with reveal_on_attention on.
func TestTheFactorysAttentionNeverToastsNorReveals(t *testing.T) {
	var focused, revealed atomic.Int32
	h := newHarness(t, func(h *harness) {
		h.cfg.Terminal.RevealOnAttention = true
		h.d.Focus = func(context.Context, string) error { focused.Add(1); return nil }
		h.d.Reveal = func(context.Context) error { revealed.Add(1); return nil }
	})
	pr := h.reviewedPR(2, "b1")
	factoryOwns(h)
	role, ok := h.cfg.FixRoleFor(h.cfg.WatchFor("talkable/talkable"))
	if !ok {
		t.Fatal("no fix role")
	}
	h.liveSession(pr.ID, role.Name, "claude", h.clock.Now(), herdr.StatusIdle) // a pane a reveal would focus
	toasts, batched := len(h.nh.titles("")), h.e.batch.Pending()
	for _, raise := range []bool{true, false, true} {
		id := h.enqueue(fixwork.ReqFixAttention, fixwork.AttentionPayload{Repo: "talkable/talkable", Number: 2, Raise: raise, Cause: "needs_you",
			Why: "1 call the fixer left needs you: https://github.com/talkable/talkable/pull/2#discussion_r4100"})
		h.handle()
		if r := h.request(id); r.State != store.RequestDone {
			t.Fatalf("raise %v: %s %q", raise, r.State, deref(r.Result))
		}
	}
	h.wantState(2, store.PRNeedsAttention)
	if got := h.nh.titles(""); len(got) != toasts {
		t.Errorf("toasts = %v, want none new", got[toasts:])
	}
	if n := h.e.batch.Pending(); n != batched {
		t.Errorf("batched toasts = %d, want %d", n, batched)
	}
	if f, r := focused.Load(), revealed.Load(); f != 0 || r != 0 {
		t.Errorf("focused %d and revealed %d times, want neither", f, r)
	}
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
