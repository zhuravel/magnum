package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// overloadLine is the judge's pane line of the 2026-10-20 outage.
const overloadLine = "stream disconnected before completion: error sending request"

// overloadedOn is the result of a round whose judge's pane showed its
// provider overloaded on the agent kind kind, as the pipeline returns it.
func overloadedOn(in pipeline.RoundInput, kind string) pipeline.RoundResult {
	return pipeline.RoundResult{Outcome: pipeline.OutcomeOverloaded, Round: max(in.Round, 1), Error: "judge pane: " + overloadLine,
		Pause: &pipeline.Pause{Kind: string(agents.HealthOverloaded), Tool: kind, Detail: overloadLine, Proven: true}}
}

// overloadsOn is a round script whose every round overloads on kind.
func overloadsOn(kind string) func(pipeline.RoundInput) (pipeline.RoundResult, error) {
	return func(in pipeline.RoundInput) (pipeline.RoundResult, error) { return overloadedOn(in, kind), nil }
}

// addPoolSlots adds free slots to the pool's first one, up to n.
func addPoolSlots(h *harness, n int) {
	h.t.Helper()
	pool := h.cfg.Pools[0]
	for i := 2; i <= n; i++ {
		if _, err := h.st.CreateSlot(h.ctx, store.Slot{Name: pool.Slot(i), RepoFullName: pool.Repo, Kind: store.SlotKindPool,
			Path: pool.Path(i), MainClone: pool.MainClone, State: store.SlotFree}); err != nil {
			h.t.Fatal(err)
		}
	}
}

// prEventAt records an event of kind under PR n at at.
func prEventAt(h *harness, n int, kind string, data map[string]any, at time.Time) {
	h.t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		h.t.Fatal(err)
	}
	subject := prSubject(store.Repo{Owner: "talkable", Name: "talkable"}, n)
	if _, err := h.st.AppendEvent(h.ctx, store.Event{At: at, Level: "info", Subject: &subject, Kind: kind, Message: kind + " in a test", Data: raw}); err != nil {
		h.t.Fatal(err)
	}
}

// overloadEvents are the round.overload_shared events, oldest first.
func overloadEvents(h *harness) []store.Event { return h.events("round.overload_shared") }

// runDue moves the clock to the earliest time one of the PRs ns is due (a
// queued PR's eligibility, a paused one's retry), ticks, and returns the
// PRs whose rounds ran.
func runDue(h *harness, ns []int) []int {
	h.t.Helper()
	var due time.Time
	for _, n := range ns {
		pr := h.pr(n)
		var at *time.Time
		switch pr.State {
		case store.PRQueued:
			at = pr.NextEligibleAt
		case store.PRPaused:
			at = pr.NextAttemptAt
		}
		if at != nil && (due.IsZero() || at.Before(due)) {
			due = *at
		}
	}
	if due.IsZero() {
		h.t.Fatal("no PR is due")
	}
	if d := due.Sub(h.clock.Now()); d > 0 {
		h.advance(d)
	}
	before := len(h.rd.all())
	h.tick()
	var ran []int
	for _, in := range h.rd.all()[before:] {
		ran = append(ran, in.PR.Number)
	}
	return ran
}

// On 2026-10-20 from 13:01 to 13:27Z a provider outage ended 21 rounds as
// overloaded and parked six PRs: each overload charged an attempt, retried
// after 1 and 2 minutes, and the third parked. Three PRs whose codex rounds
// overload 20 seconds apart share the failure: only the first overload,
// which no other PR shared yet, is charged; every other one charges
// nothing and retries after a wait that doubles from a minute and stops at
// retryMax, with one round.overload_shared event naming how many PRs share
// it and never the pane line. None parks, and once the provider answers
// each posts and forgets its count.
func TestAnOverloadSharedAcrossPRsChargesNoAttempt(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Pools[0].Max = 3 })
	addPoolSlots(h, 3)
	var outage atomic.Bool
	outage.Store(true)
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if outage.Load() {
			return overloadedOn(in, agents.KindCodex), nil
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: max(in.Round, 1), ReviewID: 9, Event: "COMMENTED"}, nil
	}
	prs := []int{2, 3, 4}
	t0 := h.clock.Now()
	specs := []prSpec{{n: 1, head: "base1", updated: t0}}
	h.open(specs...)
	h.startup()
	h.tick() // the first sync: #1 is a baseline
	for _, n := range prs {
		specs = append(specs, prSpec{n: n, head: fmt.Sprintf("h%d", n), updated: h.clock.Now()})
		h.open(specs...)
		h.tick() // queued, eligible after the quiet period
		h.advance(20 * time.Second)
	}

	const failures = 8
	waits := map[int][]time.Duration{}
	for step := 0; len(waits[2]) < failures || len(waits[3]) < failures || len(waits[4]) < failures; step++ {
		if step > 3*failures {
			t.Fatalf("rounds never came due: waits %v", waits)
		}
		for _, n := range runDue(h, prs) {
			pr := h.pr(n)
			if pr.State != store.PRPaused || pr.NextAttemptAt == nil {
				t.Fatalf("after overload %d, #%d is %s (attempts %d, last_error %q), want paused for a retry",
					len(waits[n])+1, n, pr.State, pr.Attempts, deref(pr.LastError))
			}
			waits[n] = append(waits[n], pr.NextAttemptAt.Sub(h.clock.Now()))
		}
	}
	doubling := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		retryMax, retryMax, retryMax}
	want := map[int][]time.Duration{
		2: append([]time.Duration{backoff(1)}, doubling[:failures-1]...), // the first overload, which nobody shared yet, is charged
		3: doubling[:failures],
		4: doubling[:failures],
	}
	for _, n := range prs {
		if got := waits[n][:failures]; !slices.Equal(got, want[n]) {
			t.Errorf("#%d waits %v, want %v", n, got, want[n])
		}
	}
	if a2, a3, a4 := h.pr(2).Attempts, h.pr(3).Attempts, h.pr(4).Attempts; a2 != 1 || a3 != 0 || a4 != 0 {
		t.Fatalf("attempts #2=%d #3=%d #4=%d, want 1 0 0: a shared overload charges nothing", a2, a3, a4)
	}
	if v, ok := h.e.getKV(h.ctx, kvPROverloadShared(h.pr(3).ID)); !ok || v != fmt.Sprint(failures) {
		t.Fatalf("#3's count of shared overloads = %q (%v), want %d", v, ok, failures)
	}
	if last := deref(h.pr(3).LastError); !strings.Contains(last, "codex is overloaded across 3 PRs") || !strings.Contains(last, "no attempt charged") {
		t.Fatalf("#3's wait %q does not say the provider is overloaded across the PRs", last)
	}
	evs := overloadEvents(h)
	if len(evs) != 3*failures-1 {
		t.Fatalf("%d round.overload_shared events, want one per uncharged overload (%d)", len(evs), 3*failures-1)
	}
	if first := evs[0].Message; !strings.Contains(first, "2 PRs") {
		t.Errorf("the first shared overload %q, want it to name the 2 PRs that share it", first)
	}
	for _, ev := range evs {
		if ev.Level != "info" || strings.Contains(ev.Message, "stream disconnected") {
			t.Fatalf("event %+v: want info, without the pane line", ev)
		}
	}
	if last := evs[len(evs)-1].Message; !strings.Contains(last, "3 PRs") || !strings.Contains(last, "codex") || !strings.Contains(last, "in 30m") {
		t.Errorf("the last shared overload %q, want the kind, the 3 PRs and the capped wait", last)
	}

	outage.Store(false)
	for step := 0; ; step++ {
		if step > 6 {
			t.Fatal("the PRs never posted once the provider answered")
		}
		runDue(h, prs)
		if h.pr(2).State == store.PRReviewed && h.pr(3).State == store.PRReviewed && h.pr(4).State == store.PRReviewed {
			break
		}
	}
	for _, n := range prs {
		if v, ok := h.e.getKV(h.ctx, kvPROverloadShared(h.pr(n).ID)); ok {
			t.Errorf("#%d keeps its count of shared overloads %q after it posted", n, v)
		}
	}
}

// A PR whose rounds overload with no other PR's sharing it is charged as
// before: 1 minute, then 2, and the third overload parks it.
func TestALoneOverloadStillParksOnTheThirdAttempt(t *testing.T) {
	h := newHarness(t)
	h.rd.script = overloadsOn(agents.KindCodex)
	h.reviewedPRExpect(2, "b1", store.PRPaused)
	if pr := h.pr(2); pr.Attempts != 1 || !pr.NextAttemptAt.Equal(h.clock.Now().Add(time.Minute)) {
		t.Fatalf("first lone overload: attempts %d, retry at %v", pr.Attempts, pr.NextAttemptAt)
	}
	h.e.setKV(h.ctx, kvPROverloadShared(h.pr(2).ID), "4") // as if shared overloads came before
	h.advance(time.Minute)
	h.tick()
	if pr := h.wantState(2, store.PRPaused); pr.Attempts != 2 || !pr.NextAttemptAt.Equal(h.clock.Now().Add(2*time.Minute)) {
		t.Fatalf("second lone overload: attempts %d, retry at %v", pr.Attempts, pr.NextAttemptAt)
	}
	if v, ok := h.e.getKV(h.ctx, kvPROverloadShared(h.pr(2).ID)); ok {
		t.Fatalf("a lone overload kept the count of shared overloads in a row (%q)", v)
	}
	h.advance(2 * time.Minute)
	h.tick()
	if pr := h.wantState(2, store.PRNeedsAttention); pr.Attempts != 3 {
		t.Fatalf("third lone overload: attempts %d", pr.Attempts)
	}
	if evs := overloadEvents(h); len(evs) != 0 {
		t.Fatalf("a lone overload is no shared one: %+v", evs)
	}
}

// Another PR shares a codex overload when, within the last hour
// (overloadShareWindow), a round of theirs on codex ended overloaded or an
// agent of theirs on codex ended with the same pane line
// (noteSharedFailures' lines). Older, of another kind or with another line,
// it does not: the overload is lone and charged.
func TestAnOverloadIsSharedOnlyWithinTheWindowAndOnItsKind(t *testing.T) {
	overloadOf3 := func(kind string) func(h *harness, at time.Time) {
		return func(h *harness, at time.Time) { // what the engine records when #3's round ends overloaded on kind
			prEventAt(h, 3, "engine.round_result", map[string]any{"outcome": pipeline.OutcomeOverloaded, "tool": kind}, at)
		}
	}
	launchOf3 := func(role, line string) func(h *harness, at time.Time) {
		return func(h *harness, at time.Time) {
			prEventAt(h, 3, agents.EventLaunchCleared, map[string]any{"role": role, "agent": "mg-" + role, "pane": "p1", "last_line": line}, at)
		}
	}
	cases := []struct {
		name    string
		other   func(h *harness, at time.Time)
		ago     time.Duration
		charged bool
	}{
		{"a codex overload 59 minutes before", overloadOf3(agents.KindCodex), 59 * time.Minute, false},
		{"a codex overload 61 minutes before", overloadOf3(agents.KindCodex), 61 * time.Minute, true},
		{"a claude overload a minute before", overloadOf3(agents.KindClaude), time.Minute, true},
		{"a codex launch that ended on the same pane line", launchOf3("codex-judge", overloadLine), time.Minute, false},
		{"a claude launch that ended on the same pane line", launchOf3("claude-review", overloadLine), time.Minute, true},
		{"a codex launch that ended on another line", launchOf3("codex-judge", "Error: unexpected argument"), time.Minute, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.rd.script = overloadsOn(agents.KindCodex)
			h.open(prSpec{n: 1, head: "base1"})
			h.startup()
			h.tick()
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
			h.tick() // #2 queued
			h.advance(5 * time.Minute)
			c.other(h, h.clock.Now().Add(-c.ago))
			h.tick() // #2's round overloads
			pr := h.wantState(2, store.PRPaused)
			if c.charged {
				if pr.Attempts != 1 || len(overloadEvents(h)) != 0 {
					t.Fatalf("lone: attempts %d, events %+v; want one attempt charged and no shared event", pr.Attempts, overloadEvents(h))
				}
				return
			}
			evs := overloadEvents(h)
			if pr.Attempts != 0 || !pr.NextAttemptAt.Equal(h.clock.Now().Add(time.Minute)) || len(evs) != 1 || !strings.Contains(evs[0].Message, "2 PRs") {
				t.Fatalf("shared: attempts %d, retry at %v, events %+v; want no attempt charged, a retry in a minute and one event naming 2 PRs",
					pr.Attempts, pr.NextAttemptAt, evs)
			}
		})
	}
}

// A PR that parked on its own overloads stays parked when other PRs'
// overloads become shared with it: the change charges no attempt to the
// rounds it settles, and starts no round of a parked PR.
func TestASharedOverloadNeverRetriesAParkedPR(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Pools[0].Max = 2 })
	addPoolSlots(h, 2)
	h.rd.script = overloadsOn(agents.KindCodex)
	h.reviewedPRExpect(2, "b1", store.PRPaused)
	h.advance(time.Minute)
	h.tick()
	h.advance(2 * time.Minute)
	h.tick()
	h.wantState(2, store.PRNeedsAttention)
	parked := len(h.rd.all())

	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick() // #3 queued
	// #3 overloads 5, 6, 8 and 12 minutes after #2's last overload.
	for i := range 4 {
		if pr := h.pr(3); pr.State != store.PRQueued && pr.State != store.PRPaused {
			t.Fatalf("after %d overloads shared with the parked #2's, #3 is %s (attempts %d), want it waiting for a retry", i, pr.State, pr.Attempts)
		}
		runDue(h, []int{3})
	}
	if pr := h.wantState(3, store.PRPaused); pr.Attempts != 0 {
		t.Fatalf("#3's overloads, shared with the parked #2's, charged %d attempts", pr.Attempts)
	}
	if evs := overloadEvents(h); len(evs) != 4 || !strings.Contains(evs[0].Message, "2 PRs") {
		t.Fatalf("events %+v, want #3's 4 overloads shared with #2", evs)
	}
	for range 6 {
		h.advance(10 * time.Minute)
		h.tick()
	}
	pr := h.wantState(2, store.PRNeedsAttention)
	if pr.NextAttemptAt != nil || pr.Attempts != 3 {
		t.Fatalf("the parked #2 was scheduled again: attempts %d, retry at %v", pr.Attempts, pr.NextAttemptAt)
	}
	for _, in := range h.rd.all()[parked:] {
		if in.PR.Number == 2 {
			t.Fatalf("a round of the parked #2 ran: %+v", in)
		}
	}
}

// awaitRounds waits until n rounds in all reached the pipeline.
func awaitRounds(h *harness, n int) {
	h.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); len(h.rd.all()) < n; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			h.t.Fatalf("%d rounds reached the pipeline, want %d", len(h.rd.all()), n)
		}
	}
}

// awaitOneSettled waits until exactly one of the PRs ns has left its
// round (claiming, reviewing).
func awaitOneSettled(h *harness, ns ...int) {
	h.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		settled := 0
		for _, n := range ns {
			pr, err := h.lookupPR(n)
			if err != nil {
				h.t.Fatal(err)
			}
			if pr.State != store.PRClaiming && pr.State != store.PRReviewing {
				settled++
			}
		}
		if settled == 1 {
			return
		}
		if settled > 1 || time.Now().After(deadline) {
			h.t.Fatalf("%d of the PRs %v left their rounds, want 1", settled, ns)
		}
	}
}

// Two PRs whose codex rounds overload in the same tick and last as long
// retry in lockstep. Once their wait reaches the 30-minute cap, the first
// of them to settle finds the other's last overload a cap and a round's
// length old, which a window of retryMax would miss, charging it; within
// overloadShareWindow, twice the cap, both stay uncharged, cycle after
// cycle, and keep retrying together.
func TestPRsOverloadingInLockstepAtTheCapStayUncharged(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Pools[0].Max = 2 })
	addPoolSlots(h, 2)
	h.rd.script = overloadsOn(agents.KindCodex)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick() // both queued at once
	h.advance(5 * time.Minute)
	// A third PR's codex round overloaded a moment before: the outage is
	// under way, so neither first overload is lone.
	prEventAt(h, 4, "engine.round_result", map[string]any{"outcome": pipeline.OutcomeOverloaded, "tool": agents.KindCodex}, h.clock.Now())
	const roundLength = 2 * time.Minute
	gate := make(chan struct{})
	h.rd.gate = gate
	for cycle := 1; cycle <= 8; cycle++ {
		before := len(h.rd.all())
		if err := h.e.Tick(h.ctx); err != nil {
			t.Fatal(err)
		}
		awaitRounds(h, before+2) // both rounds started in the same tick and wait at the gate
		h.advance(roundLength)
		gate <- struct{}{} // one round ends first,
		awaitOneSettled(h, 2, 3)
		gate <- struct{}{} // the other in the same second
		h.settle()
		var due []time.Time
		for _, n := range []int{2, 3} {
			pr := h.wantState(n, store.PRPaused)
			if pr.Attempts != 0 || pr.NextAttemptAt == nil {
				t.Fatalf("cycle %d: #%d charged %d attempts (retry at %v, last_error %q)", cycle, n, pr.Attempts, pr.NextAttemptAt, deref(pr.LastError))
			}
			due = append(due, *pr.NextAttemptAt)
		}
		if want := h.clock.Now().Add(backoff(cycle)); !due[0].Equal(want) || !due[1].Equal(want) {
			t.Fatalf("cycle %d: retries at %v and %v, want both at %v", cycle, due[0], due[1], want)
		}
		h.advance(due[0].Sub(h.clock.Now()))
	}
}
