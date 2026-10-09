package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// keyHerdr is fakeHerdr with key sending: a second ctrl+c to an agent
// leaves it idle, as an interrupted agent would be.
type keyHerdr struct {
	*fakeHerdr
	kmu  sync.Mutex
	keys []string
	sent map[string]int
}

func (k *keyHerdr) AgentSendKeys(_ context.Context, target string, keys ...string) error {
	k.kmu.Lock()
	defer k.kmu.Unlock()
	k.keys = append(k.keys, "agent:"+target+":"+strings.Join(keys, "+"))
	if k.sent == nil {
		k.sent = map[string]int{}
	}
	k.sent[target]++
	if k.sent[target] >= 2 {
		k.fakeHerdr.mu.Lock()
		for i := range k.fakeHerdr.agents {
			if k.fakeHerdr.agents[i].Name == target {
				k.fakeHerdr.agents[i].AgentStatus = herdr.StatusIdle
			}
		}
		k.fakeHerdr.mu.Unlock()
	}
	return nil
}

func (k *keyHerdr) PaneSendKeys(_ context.Context, pane string, keys ...string) error {
	k.kmu.Lock()
	defer k.kmu.Unlock()
	k.keys = append(k.keys, "pane:"+pane+":"+strings.Join(keys, "+"))
	return nil
}

func (k *keyHerdr) all() []string {
	k.kmu.Lock()
	defer k.kmu.Unlock()
	return slices.Clone(k.keys)
}

// withKeys gives the harness a herdr that sends keys.
func withKeys(kh **keyHerdr) func(*harness) {
	return func(h *harness) {
		*kh = &keyHerdr{fakeHerdr: h.hd}
		h.d.Herdr = *kh
	}
}

// runningRound queues PR #n at head and starts its round, which then runs
// (blocked in the pipeline) until its context ends. With reviewed, #n was
// reviewed at "r<n>" first and head is a new push.
func (h *harness) runningRound(n int, head string, reviewed bool) store.PR {
	h.t.Helper()
	if reviewed {
		h.reviewedPR(n, fmt.Sprintf("r%d", n))
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: n, head: head})
		h.tick() // new head: rereview_pending
	} else {
		h.open(prSpec{n: 1, head: "base1"})
		h.startup()
		h.tick() // first sync: #1 baseline
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: n, head: head})
		h.tick() // #n queued
	}
	h.advance(time.Hour)
	before := len(h.rd.all())
	h.rd.mu.Lock()
	h.rd.gate = make(chan struct{})
	h.rd.mu.Unlock()
	if err := h.e.Tick(h.ctx); err != nil {
		h.t.Fatalf("tick: %v", err)
	}
	for i := 0; len(h.rd.all()) == before; i++ {
		if i > 500 {
			h.t.Fatal("the round never reached the pipeline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !h.e.roundActive(h.pr(n).ID) {
		h.t.Fatal("no round running")
	}
	return h.wantState(n, store.PRReviewing)
}

// working marks the PR's judge agent working in herdr.
func (h *harness) working(n int) {
	h.hd.mu.Lock()
	h.hd.agents = append(h.hd.agents, herdr.AgentInfo{PaneID: "p-judge", Name: fmt.Sprintf("mg-t-%d-codex-judge", n),
		Agent: "codex", AgentStatus: herdr.StatusWorking})
	h.hd.mu.Unlock()
}

func TestAbortStopsTheRunningRound(t *testing.T) {
	var kh *keyHerdr
	h := newHarness(t, withKeys(&kh))
	pr := h.runningRound(7, "h7", false)
	h.working(7)
	run, err := h.judgeRun(pr, "initial", "h7", store.RunSubmitted, "", "")
	if err != nil {
		t.Fatal(err)
	}

	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "7"}})
	h.tick()

	r := h.request(id)
	res := deref(r.Result)
	if r.State != store.RequestDone {
		t.Fatalf("abort: %s %q", r.State, res)
	}
	for _, want := range []string{"aborted talkable/talkable#7", "round stopped", "1 agent interrupted", "1 run abandoned",
		"sessions parked", "PR baseline", "slot review1 released"} {
		if !strings.Contains(res, want) {
			t.Errorf("result %q lacks %q", res, want)
		}
	}
	if got, want := kh.all(), []string{"agent:mg-t-7-codex-judge:ctrl+c", "agent:mg-t-7-codex-judge:ctrl+c"}; !slices.Equal(got, want) {
		t.Errorf("keys %v, want %v", got, want)
	}
	cur := h.wantState(7, store.PRBaseline)
	if cur.Forced || cur.Muted {
		t.Errorf("aborted PR: forced=%v muted=%v", cur.Forced, cur.Muted)
	}
	got, err := h.st.RunByID(h.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.RunAbandoned || deref(got.Error) != abortedBy {
		t.Errorf("run %s %q, want abandoned %q", got.State, deref(got.Error), abortedBy)
	}
	if h.ag.count(fmt.Sprintf("park:%d", pr.ID)) == 0 {
		t.Error("sessions not parked")
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Errorf("slot not handed back: %s pr=%v", sl.State, sl.PRID)
	}
	if h.e.roundActive(pr.ID) {
		t.Error("still reserved")
	}
	if !h.hasEvent("pr:talkable/talkable#7", "pr.aborted") {
		t.Error("no pr.aborted event")
	}

	// Nothing brings the head back: the next push does.
	h.advance(time.Hour)
	h.tick()
	h.wantState(7, store.PRBaseline)
}

func TestAbortOfAReReviewKeepsTheReview(t *testing.T) {
	h := newHarness(t)
	h.runningRound(2, "h2", true)
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "talkable#2"}})
	h.tick()
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("abort: %s %q", r.State, deref(r.Result))
	}
	cur := h.wantState(2, store.PRReviewed)
	if deref(cur.ReviewedSHA) != "r2" {
		t.Errorf("reviewed_sha %q, want r2", deref(cur.ReviewedSHA))
	}
	rounds := len(h.rd.all())
	h.advance(2 * time.Hour)
	h.tick()
	h.wantState(2, store.PRReviewed)
	if len(h.rd.all()) != rounds {
		t.Error("an aborted re-review ran again without a new push")
	}
}

func TestAbortWithoutARunningRoundFails(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestFailed || deref(r.Result) != "no review of talkable/talkable#2 is running or queued (state reviewed)" {
		t.Fatalf("abort: %s %q", r.State, deref(r.Result))
	}
	if h.ag.count("park:") != 0 {
		t.Error("parked a PR without a round")
	}
}

func TestAbortKeepsAPinnedSlot(t *testing.T) {
	h := newHarness(t)
	pr := h.runningRound(7, "h7", false)
	pin := h.enqueue(ReqPin, TargetPayload{PRTarget: PRTarget{Ref: "7"}})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	if r := h.request(pin); r.State != store.RequestDone {
		t.Fatalf("pin: %s %q", r.State, deref(r.Result))
	}
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "7"}})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "slot review1 kept: pinned") {
		t.Fatalf("abort: %s %q", r.State, deref(r.Result))
	}
	if sl := h.slot("review1"); sl.State != store.SlotHeld || sl.PRID == nil || *sl.PRID != pr.ID {
		t.Errorf("pinned slot: %s pr=%v", sl.State, sl.PRID)
	}
}

func TestIgnoreMutesAndNeverQueues(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	id := h.enqueue(ReqIgnore, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestDone {
		t.Fatalf("ignore: %s %q", r.State, deref(r.Result))
	}
	for _, want := range []string{"ignored talkable/talkable#2", "sessions parked", "PR ineligible", "slot review1 released", "magnum unmute"} {
		if !strings.Contains(deref(r.Result), want) {
			t.Errorf("result %q lacks %q", deref(r.Result), want)
		}
	}
	if strings.Contains(deref(r.Result), "round stopped") {
		t.Errorf("no round ran: %q", deref(r.Result))
	}
	want := func(when string) {
		t.Helper()
		cur := h.wantState(2, store.PRIneligible)
		if !cur.Muted || deref(cur.SkipReason) != skipIgnored {
			t.Fatalf("%s: muted=%v skip_reason %q", when, cur.Muted, deref(cur.SkipReason))
		}
	}
	want("after ignore")
	rounds := len(h.rd.all())

	// A push, the push quiet period, a restart: still ignored, never queued.
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	want("after a push")
	h.advance(2 * time.Hour)
	h.tick()
	h.startup()
	h.tick()
	want("after a restart")
	if len(h.rd.all()) != rounds {
		t.Fatal("an ignored PR was reviewed")
	}
	if h.slot("review1").PRID != nil {
		t.Fatal("an ignored PR claimed a slot")
	}

	// unmute clears the mark and resumes reviews of the new head.
	u := h.enqueue(ReqUnmute, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	if r := h.request(u); r.State != store.RequestDone {
		t.Fatalf("unmute: %s %q", r.State, deref(r.Result))
	}
	cur := h.pr(pr.Number)
	if cur.Muted || deref(cur.SkipReason) == skipIgnored || deref(cur.ReviewedSHA) != "b2" || len(h.rd.all()) != rounds+1 {
		t.Fatalf("after unmute: state %s reviewed %q muted=%v skip_reason %q", cur.State, deref(cur.ReviewedSHA), cur.Muted, deref(cur.SkipReason))
	}
}

func TestIgnoreStopsARunningRound(t *testing.T) {
	var kh *keyHerdr
	h := newHarness(t, withKeys(&kh))
	h.runningRound(7, "h7", false)
	h.working(7)
	id := h.enqueue(ReqIgnore, TargetPayload{PRTarget: PRTarget{Ref: "7"}})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "round stopped") {
		t.Fatalf("ignore: %s %q", r.State, deref(r.Result))
	}
	cur := h.wantState(7, store.PRIneligible)
	if !cur.Muted || deref(cur.SkipReason) != skipIgnored {
		t.Fatalf("muted=%v skip_reason %q", cur.Muted, deref(cur.SkipReason))
	}
	if len(kh.all()) != 2 {
		t.Errorf("keys %v", kh.all())
	}
}

func TestIgnoreKeepsAPerPRWorktree(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	// Move the PR to a per-PR worktree: the pool slot goes back first.
	rel := h.enqueue(ReqRelease, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	if r := h.request(rel); r.State != store.RequestDone {
		t.Fatalf("release: %q", deref(r.Result))
	}
	wt, err := h.st.CreateSlot(h.ctx, store.Slot{Name: "talkable-pr-2", RepoFullName: "talkable/talkable", Kind: store.SlotKindPerPR,
		Path: h.layout.Home + "/wt-2", MainClone: h.layout.Home + "/talkable", State: store.SlotHeld, PRID: &pr.ID})
	if err != nil {
		t.Fatal(err)
	}
	id := h.enqueue(ReqIgnore, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "worktree talkable-pr-2 kept") {
		t.Fatalf("ignore: %s %q", r.State, deref(r.Result))
	}
	if sl := h.slot(wt.Name); sl.State != store.SlotHeld || sl.PRID == nil {
		t.Errorf("per-PR worktree: %s pr=%v", sl.State, sl.PRID)
	}
}

func TestIgnoreOfAClosedPRMutesIt(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	if err := h.st.TransitionPR(h.ctx, pr.ID, nil, store.PRClosed, nil); err != nil {
		t.Fatal(err)
	}
	id := h.enqueue(ReqIgnore, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.e.handleRequests(h.ctx) // no poll: it would see the PR open again
	h.settle()
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "muted only") {
		t.Fatalf("ignore: %s %q", r.State, deref(r.Result))
	}
	if h.ag.count("park:") != 0 || h.slot("review1").PRID == nil {
		t.Error("ignore touched a closed PR's sessions or slot (cleanup owns them)")
	}
	cur := h.pr(2)
	if cur.State != store.PRClosed || !cur.Muted || deref(cur.SkipReason) != skipIgnored {
		t.Fatalf("closed PR: state %s muted=%v skip_reason %q", cur.State, cur.Muted, deref(cur.SkipReason))
	}
}

func (h *harness) hasEvent(subject, kind string) bool {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, subject, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	return slices.ContainsFunc(evs, func(e store.Event) bool { return e.Kind == kind })
}
