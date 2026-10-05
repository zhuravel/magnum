package engine

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/store"
)

// A timing wait is built from the rule's code, not its words: a reason
// worded in any way still becomes the wait its rule names.
func TestThrottleWaitIsBuiltFromTheRule(t *testing.T) {
	h := newHarness(t)
	pr := pushedAfter(t, h, 40*time.Minute)
	w := *h.cfg.WatchFor("talkable/talkable")
	until := h.clock.Now().Add(time.Hour)
	for rule, want := range map[eligibility.Rule]string{
		eligibility.RuleRequested:     WaitRequested,
		eligibility.RuleQuiet:         WaitQuiet,
		eligibility.RuleBurst:         WaitBurst,
		eligibility.RuleInterval:      WaitInterval,
		eligibility.RuleDraftInterval: WaitDraftInterval,
		eligibility.RuleCap:           WaitCap,
		eligibility.RuleSmallDelta:    WaitDelta,
		"":                            WaitOther,
	} {
		td := eligibility.ThrottleDecision{Rule: rule, Reason: "a rule worded anew", NextEligibleAt: until}
		got := h.e.throttleWait(h.ctx, pr, w, eligibility.PRFacts{}, td, h.clock.Now())
		if got.Reason != want || !got.Until.Equal(until) {
			t.Errorf("rule %q: wait %+v, want %s", rule, got, want)
		}
	}
}

// A dispatch gate's wait is built from the gate's code, not from its words:
// any sentence becomes the wait its code names, a kind pause takes its end
// from the pause recorded now (none recorded: the sentence alone), and a
// gate an older daemon wrote without a code reads as another reason.
func TestWaitIsBuiltFromTheGateCode(t *testing.T) {
	h := newHarness(t)
	pushedAfter(t, h, 40*time.Minute)
	h.advance(10 * time.Minute)
	pr := h.pr(2)
	until := h.clock.Now().Add(time.Hour)
	h.e.setKV(h.ctx, KVToolPausedUntil("codex"), store.FormatTime(until))
	h.e.setKV(h.ctx, KVToolPausedReason("codex"), "usage_limit")
	for _, tc := range []struct {
		g      gate
		reason string
		until  time.Time
	}{
		{gate{reason: WaitIdentity, text: "the posting login lost its token"}, WaitIdentity, time.Time{}},
		{gate{reason: WaitBudget, text: "most of the Codex week is spent"}, WaitBudget, time.Time{}},
		{gate{reason: WaitCapacity, text: "enough rounds run already"}, WaitCapacity, time.Time{}},
		{gate{reason: WaitSlot, text: "the checkout is taken"}, WaitSlot, time.Time{}},
		{gate{reason: WaitOther, text: "a human types in the panes"}, WaitOther, time.Time{}},
		{gate{reason: WaitKind, kind: "codex", text: "the Codex agents rest"}, WaitKind, until},
		{gate{reason: WaitKind, kind: "claude", text: "the Claude agents rest"}, WaitOther, time.Time{}},
	} {
		h.e.noteGate(h.ctx, pr.ID, tc.g)
		if v, _ := h.e.getKV(h.ctx, kvPRGate(pr.ID)); v != tc.g.text {
			t.Errorf("%+v: the gate's sentence for magnum status is %q", tc.g, v)
		}
		w := h.e.waitFor(h.ctx, pr, nil, h.clock.Now())
		if w.Reason != tc.reason || w.Detail != tc.g.text || !w.Until.Equal(tc.until) {
			t.Errorf("%+v: wait %+v, want %s until %v", tc.g, w, tc.reason, tc.until)
		}
	}
	h.e.delKV(h.ctx, kvPRGateReason(pr.ID))
	h.e.setKV(h.ctx, kvPRGate(pr.ID), "slot review1 is pinned (magnum unpin)")
	if w := h.e.waitFor(h.ctx, pr, nil, h.clock.Now()); w.Reason != WaitOther || w.Detail != "slot review1 is pinned (magnum unpin)" {
		t.Errorf("a gate without its code: wait %+v", w)
	}
}
