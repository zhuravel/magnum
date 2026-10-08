package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// flagToast is the toast every refused round sends.
const flagToast = "Codex flagged · never reviewed again"

// refusedRound is a round whose judge's own pass Codex refused.
func refusedRound(in pipeline.RoundInput) (pipeline.RoundResult, error) {
	ref := pipeline.Refusal{Role: "codex-judge", Kind: "codex", RunID: "r-own-1",
		Detail: "■ This content was flagged for possible cybersecurity risk."}
	return pipeline.RoundResult{Outcome: pipeline.OutcomeRefused, Round: 1, TargetSHA: in.TargetSHA, Refusal: &ref, Error: ref.Sentence()}, nil
}

// flagOf is PR #n's Codex flag (ok false: none).
func (h *harness) flagOf(n int) (CodexFlag, bool) {
	h.t.Helper()
	v, ok, err := h.st.GetKV(h.ctx, store.KVPRCodexFlag(h.pr(n).ID))
	if err != nil {
		h.t.Fatal(err)
	}
	if !ok {
		return CodexFlag{}, false
	}
	return ParseCodexFlag(v), true
}

// refusedPR is PR #2 at b1 after a round Codex refused.
func (h *harness) refusedPR() store.PR {
	h.t.Helper()
	h.queuedPR(2, "b1")
	h.rd.script = refusedRound
	h.advance(5 * time.Minute)
	h.tick()
	h.rd.script = nil
	return h.wantState(2, store.PRIneligible)
}

// The live case (talkable#11990): Codex refused the round. The PR is
// flagged for good (the refused head, role, run and time), its last error
// says who was refused in which run, one toast goes out, and nothing about
// it runs again by itself: not the next push, not a review request on
// GitHub, not a forced `magnum review`, which refuses with the reason.
func TestARefusedRoundFlagsThePRForGood(t *testing.T) {
	h := newHarness(t)
	pr := h.refusedPR()
	want := "Codex refused the review: content flagged as a cybersecurity risk (codex-judge, run r-own-1)"
	if deref(pr.LastError) != want || !strings.Contains(deref(pr.SkipReason), "Codex flagged") || pr.Forced {
		t.Fatalf("PR = last_error %q, skip_reason %q, forced %v", deref(pr.LastError), deref(pr.SkipReason), pr.Forced)
	}
	f, ok := h.flagOf(2)
	if !ok || f.Kind != "codex" || f.Role != "codex-judge" || f.Run != "r-own-1" || f.Head != "b1" || f.At.IsZero() ||
		!strings.Contains(f.Detail, "cybersecurity") {
		t.Fatalf("flag = %+v, %v", f, ok)
	}
	if f.Short() != flagToast {
		t.Errorf("Short() = %q", f.Short())
	}
	h.wantToasts(flagToast, 1)
	if !h.hasEvent("pr:talkable/talkable#2", "pr.codex_flagged") {
		t.Error("no pr.codex_flagged event")
	}

	// A push.
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2"})
	h.advance(30 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	if pr := h.wantState(2, store.PRIneligible); !strings.Contains(deref(pr.SkipReason), "Codex flagged") {
		t.Errorf("skip_reason after the push = %q", deref(pr.SkipReason))
	}
	// A review request on GitHub.
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
	h.advance(30 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	h.wantState(2, store.PRIneligible)
	// magnum review.
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}})
	h.tick()
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(store.Deref(r.Result), "Codex flagged") ||
		!strings.Contains(store.Deref(r.Result), "magnum codex-flag clear") {
		t.Fatalf("magnum review = %s: %s", r.State, store.Deref(r.Result))
	}
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	h.wantToasts(flagToast, 1)
}

// What waits for a round when the flag is set (a forced review queued
// before, a paused turn) never starts: dispatch takes it out of line.
func TestDispatchHoldsAFlaggedPRWhateverWaits(t *testing.T) {
	h := newHarness(t)
	h.queuedPR(2, "b1")
	pr := h.pr(2)
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRQueued}, store.PRQueued, func(u *store.PRUpdate) {
		u.Set("forced", true)
	}); err != nil {
		t.Fatal(err)
	}
	h.rawFlag(2)
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 0)
	if pr := h.wantState(2, store.PRIneligible); pr.Forced {
		t.Error("the forced mark stayed")
	}

	h2 := newHarness(t)
	h2.queuedPR(2, "b1")
	if err := h2.st.TransitionPR(h2.ctx, h2.pr(2).ID, []string{store.PRQueued}, store.PRPaused, nil); err != nil {
		t.Fatal(err)
	}
	h2.rawFlag(2)
	h2.advance(10 * time.Minute)
	h2.tick()
	reqWantRounds(t, h2, 0)
	h2.wantState(2, store.PRIneligible)
}

// A flag record magnum cannot parse is a flag still: the PR waiting for its
// round is taken out of line with the flag's reason, and no round starts,
// not even a forced one.
func TestAnUnreadableFlagHoldsThePROutOfDispatch(t *testing.T) {
	h := newHarness(t)
	h.queuedPR(2, "b1")
	if err := h.st.SetKV(h.ctx, store.KVPRCodexFlag(h.pr(2).ID), "{not json"); err != nil {
		t.Fatal(err)
	}
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 0)
	if pr := h.wantState(2, store.PRIneligible); !strings.Contains(deref(pr.SkipReason), "Codex flagged") {
		t.Fatalf("skip_reason = %q", deref(pr.SkipReason))
	}
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}})
	h.tick()
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(store.Deref(r.Result), "the flag could not be read") {
		t.Fatalf("magnum review = %s: %s", r.State, store.Deref(r.Result))
	}
	reqWantRounds(t, h, 0)
}

// failFlagWrites makes the registry refuse every Codex flag write (a
// trigger), until the returned func lifts it.
func (h *harness) failFlagWrites() func() {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(h.ctx, `CREATE TRIGGER fail_flag BEFORE INSERT ON kv WHEN NEW.key GLOB 'pr.*.codex_flag'
BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		h.t.Fatal(err)
	}
	return func() {
		if _, err := h.st.DB().ExecContext(h.ctx, "DROP TRIGGER fail_flag"); err != nil {
			h.t.Fatal(err)
		}
	}
}

// A refused round whose flag write fails keeps the flag: the PR stays out
// of rounds (a push, a forced review), and the write is tried again every
// tick until it takes.
func TestAFailedFlagWriteIsRetriedAndHoldsThePR(t *testing.T) {
	h := newHarness(t)
	lift := h.failFlagWrites()
	h.refusedPR()
	if _, ok := h.flagOf(2); ok {
		t.Fatal("the flag was written through the failing registry")
	}
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2"})
	h.advance(30 * time.Minute)
	h.tick()
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}})
	h.tick()
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(store.Deref(r.Result), "Codex flagged") {
		t.Fatalf("magnum review = %s: %s", r.State, store.Deref(r.Result))
	}
	reqWantRounds(t, h, 1)
	h.wantState(2, store.PRIneligible)

	lift()
	h.tick()
	if f, ok := h.flagOf(2); !ok || f.Role != "codex-judge" || f.Run != "r-own-1" || f.Head != "b1" {
		t.Fatalf("flag after the retry = %+v, %v", f, ok)
	}
	if _, ok := h.e.unwrittenFlag(h.pr(2).ID); ok {
		t.Fatal("the written flag is still kept in memory")
	}
}

// liveOf counts PR #n's live sessions.
func (h *harness) liveOf(n int) int {
	h.t.Helper()
	live, err := h.st.LiveSessions(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	id := h.pr(n).ID
	return len(slices.DeleteFunc(live, func(s store.Session) bool { return s.PRID != id }))
}

// A flagged PR is never reviewed again, yet it kept its pool slot and its
// live agents (talkable#11990 held review17 and three agents for a day).
// The tick after the flag parks its agents and releases its slot, once.
func TestAFlaggedPRGivesBackItsAgentsAndSlot(t *testing.T) {
	h := newHarness(t)
	h.refusedPR()
	pr := h.pr(2)
	if h.liveOf(2) == 0 || h.slot("review1").State != store.SlotHeld {
		t.Fatalf("after the refusal: %d live sessions, slot %s", h.liveOf(2), h.slot("review1").State)
	}
	h.advance(time.Minute)
	h.tick()
	if h.liveOf(2) != 0 || h.parks(pr.ID) != 1 {
		t.Fatalf("live sessions %d, parks %d: want the flagged PR's agents parked", h.liveOf(2), h.parks(pr.ID))
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree || !slices.Contains(h.sl.all(), "release:review1:codex-flagged") {
		t.Fatalf("slot %s, calls %v: want review1 released", sl.State, h.sl.all())
	}
	if !h.hasEvent("pr:talkable/talkable#2", "pr.codex_flag_released") {
		t.Error("no pr.codex_flag_released event")
	}
	h.advance(time.Minute)
	h.tick()
	releases := slices.DeleteFunc(h.sl.all(), func(c string) bool { return c != "release:review1:codex-flagged" })
	if h.parks(pr.ID) != 1 || len(releases) != 1 {
		t.Fatalf("parked or released again: %v %v", h.ag.all(), h.sl.all())
	}
}

// A flagged PR the operator opened (`magnum open` pins the PR and its slot)
// to review it by hand keeps its agents and its slot.
func TestAFlaggedPRMagnumOpenPinnedKeepsItsAgentsAndSlot(t *testing.T) {
	h := newHarness(t)
	pr := h.openedPR()
	parks := h.parks(pr.ID)
	h.rawFlag(2)
	h.advance(time.Hour)
	h.tick()
	if h.parks(pr.ID) != parks || h.liveOf(2) == 0 {
		t.Fatalf("parks %d (was %d), live %d: a pinned PR's agents were parked", h.parks(pr.ID), parks, h.liveOf(2))
	}
	if sl := h.slot("review1"); sl.State == store.SlotFree || slices.Contains(h.sl.all(), "release:review1:codex-flagged") {
		t.Fatalf("slot %s, calls %v: a pinned slot was released", sl.State, h.sl.all())
	}
}

// rawFlag writes PR #n's flag record and nothing else: the dispatch guard
// is what keeps the round off.
func (h *harness) rawFlag(n int) {
	h.t.Helper()
	v, err := CodexFlag{Kind: "codex", Role: "codex-judge", Run: "r-1", At: h.clock.Now()}.marshal()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, store.KVPRCodexFlag(h.pr(n).ID), v); err != nil {
		h.t.Fatal(err)
	}
}

// setFlag flags PR #n as `magnum codex-flag set` does.
func (h *harness) setFlag(n int, reason string) string {
	h.t.Helper()
	id := h.enqueue(ReqCodexFlag, CodexFlagPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: n}, Reason: reason})
	h.handle()
	r := h.request(id)
	if r.State != store.RequestDone {
		h.t.Fatalf("codex-flag set = %s: %s", r.State, store.Deref(r.Result))
	}
	return store.Deref(r.Result)
}

// `magnum codex-flag set` flags a PR found before (its reason kept, by
// whom), and `clear` lifts the flag: the PR is judged by the watch again,
// so an unreviewed head is queued and reviewed.
func TestCodexFlagSetAndClear(t *testing.T) {
	h := newHarness(t)
	h.queuedPR(2, "b1")
	res := h.setFlag(2, "Codex warned about cyber abuse on 10-06")
	if !strings.Contains(res, "flagged talkable/talkable#2") {
		t.Errorf("answer = %q", res)
	}
	f, ok := h.flagOf(2)
	if !ok || f.Kind != "codex" || f.Detail != "Codex warned about cyber abuse on 10-06" || f.By != "magnum codex-flag" || f.Head != "b1" {
		t.Fatalf("flag = %+v, %v", f, ok)
	}
	h.wantState(2, store.PRIneligible)
	h.wantToasts(flagToast, 0) // the operator set it: no toast

	id := h.enqueue(ReqCodexFlag, CodexFlagPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}, Clear: true})
	h.handle()
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(store.Deref(r.Result), "cleared") {
		t.Fatalf("clear = %s: %s", r.State, store.Deref(r.Result))
	}
	if _, ok := h.flagOf(2); ok {
		t.Fatal("the flag stayed")
	}
	h.wantState(2, store.PRQueued)
	h.advance(10 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	h.wantState(2, store.PRReviewed)

	// Clearing a PR with no flag says so.
	id = h.enqueue(ReqCodexFlag, CodexFlagPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}, Clear: true})
	h.handle()
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(store.Deref(r.Result), "not flagged") {
		t.Fatalf("clear again = %s: %s", r.State, store.Deref(r.Result))
	}
}
