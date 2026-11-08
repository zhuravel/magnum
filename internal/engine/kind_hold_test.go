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

// heldHarness is sharedLaunchHarness with the bootstrap line shared: #3's
// codex-judge prompt was refused on it, and #2's first round dies on it, so
// the next health check holds codex. Its rounds, while codex launches die,
// end on the line when a codex judge runs, and post otherwise.
func heldHarness(t *testing.T, mods ...func(*harness)) *harness {
	t.Helper()
	h := sharedLaunchHarness(t, mods...)
	refusedPrompt(h, 3, "codex-judge", bootstrapLine)
	h.rd.script = codexDies(h, bootstrapLine)
	h.advance(5 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	return h
}

// codexDies is a round script whose rounds die on line while their judge
// is codex-judge (launchesDie), and post with any other judge.
func codexDies(h *harness, line string) func(pipeline.RoundInput) (pipeline.RoundResult, error) {
	die := launchesDie(h, line)
	return func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if slices.Contains(roleNames(in.Roles), "codex-judge") {
			return die(in)
		}
		return posted(in.TargetSHA, 0), nil
	}
}

// codexLaunches is a round script whose codex-judge takes its prompt (a
// submitted run, as the probe needs) and posts.
func codexLaunches(h *harness) func(pipeline.RoundInput) (pipeline.RoundResult, error) {
	return func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if slices.Contains(roleNames(in.Roles), "codex-judge") {
			now := h.clock.Now()
			if _, err := h.st.CreateRun(h.ctx, store.Run{PRID: in.PR.ID, Round: max(in.Round, 1), Role: "codex-judge", Kind: store.RunInitial,
				TargetSHA: in.TargetSHA, Identity: "talkable-app", ReviewerLogin: magnumApp, State: store.RunSubmitted, PromptText: "go",
				CreatedAt: now, SubmittedAt: &now}); err != nil {
				return failRound(h.t, err)
			}
		}
		return posted(in.TargetSHA, 0), nil
	}
}

// heldUntil is codex's hold's end (fails the test without one).
func heldUntil(h *harness) time.Time {
	h.t.Helper()
	hold, ok := h.e.kindHoldOf(h.ctx, agents.KindCodex)
	if !ok {
		h.t.Fatal("codex is not held")
	}
	return hold.Until
}

// On 2026-11-05 codex-judge launches died for two hours and every round
// that needed one died with them, while claude-judge, the judge_fallback,
// launched fine. A launch failure two or more PRs' codex roles share holds
// codex for 15 minutes (kind.held, and the shared failure says so instead
// of "nothing is paused"): a round that needs a codex agent runs the
// watch's judge_fallback in the judge's place and without the codex agent
// roles (codex-review, a shell command, still runs), and posts.
func TestASharedLaunchFailureHoldsTheKindAndRunsTheFallbackJudge(t *testing.T) {
	h := heldHarness(t)
	h.advance(time.Minute) // #2's retry
	h.tick()
	until := heldUntil(h)
	if want := h.clock.Now().Add(kindHoldSpan); !until.Equal(want) {
		t.Fatalf("codex held until %v, want %v", until, want)
	}
	if evs := h.events(EventKindHeld); len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "codex held for 15m") {
		t.Fatalf("kind.held events %+v", evs)
	}
	if evs := h.events(EventSharedFailure); len(evs) != 1 || !strings.Contains(evs[0].Message, "codex is held until") ||
		strings.Contains(evs[0].Message, "nothing is paused") {
		t.Fatalf("the shared failure %+v does not say codex is held", evs)
	}
	reqWantRounds(t, h, 2)
	names := roleNames(h.rd.all()[1].Roles)
	if !slices.Contains(names, "claude-judge") || slices.Contains(names, "codex-judge") || !slices.Contains(names, "codex-review") ||
		!slices.Contains(names, "claude-review") {
		t.Fatalf("the held round's roles = %v, want claude-judge in codex-judge's place and the rest", names)
	}
	h.wantState(2, store.PRReviewed)
}

// The hold ends at its first good probe. Once its 15 minutes are over, the
// next round that needs codex runs it as before, the probe, while any other
// still runs the fallback: a probe whose codex launch dies on a pane line
// holds codex 15 minutes more (kind.hold_renewed) and charges the PR
// nothing, and one whose codex agent takes its prompt lifts the hold
// (kind.hold_lifted, with a toast), so the next rounds run codex again.
func TestAKindHoldRenewsOnAFailedProbeAndEndsAtItsFirstGoodOne(t *testing.T) {
	h := heldHarness(t)
	h.advance(time.Minute)
	h.tick() // held: #2 posts with the fallback
	h.wantState(2, store.PRReviewed)
	until := heldUntil(h)

	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2"})
	h.advance(until.Sub(h.clock.Now()) + 30*time.Minute) // past the hold and the re-review interval
	h.tick()
	reqWantRounds(t, h, 3)
	if names := roleNames(h.rd.all()[2].Roles); !slices.Contains(names, "codex-judge") {
		t.Fatalf("the probe's roles = %v, want codex-judge", names)
	}
	if evs := h.events(EventKindHoldProbe); len(evs) != 1 || !strings.Contains(evs[0].Message, "talkable#2") {
		t.Fatalf("kind.hold_probe events %+v", evs)
	}
	if got := heldUntil(h); !got.Equal(h.clock.Now().Add(kindHoldSpan)) {
		t.Fatalf("after a failed probe codex is held until %v, want 15 minutes from now", got)
	}
	if evs := h.events(EventKindHoldRenewed); len(evs) != 1 || evs[0].Level != "warn" {
		t.Fatalf("kind.hold_renewed events %+v", evs)
	}
	if pr := h.wantState(2, store.PRRereviewPending); pr.Attempts != 0 {
		t.Fatalf("the failed probe charged %d attempts", pr.Attempts)
	}

	h.advance(time.Minute) // #2's retry, inside the renewed hold: the fallback again
	h.tick()
	reqWantRounds(t, h, 4)
	if names := roleNames(h.rd.all()[3].Roles); !slices.Contains(names, "claude-judge") {
		t.Fatalf("the round inside the renewed hold runs %v, want claude-judge", names)
	}

	h.rd.script = codexLaunches(h)
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b3"})
	h.advance(heldUntil(h).Sub(h.clock.Now()) + 30*time.Minute)
	h.tick()
	reqWantRounds(t, h, 5)
	if _, held := h.e.kindHoldOf(h.ctx, agents.KindCodex); held {
		t.Fatal("a good probe left codex held")
	}
	if evs := h.events(EventKindHoldLifted); len(evs) != 1 || !strings.Contains(evs[0].Message, "first good probe") {
		t.Fatalf("kind.hold_lifted events %+v", evs)
	}
	h.wantToasts("codex launches again", 1)
	h.wantState(2, store.PRReviewed)
}

// A watch without a judge_fallback has no judge to run in codex's place:
// its round waits out the hold, as a paused kind's does, and the toast
// says so instead of naming a fallback judge. A plain `magnum resume`
// leaves the hold, and its answer names it with the command that lifts
// it, as it names a paused kind; `magnum resume --tool codex` lifts it.
func TestAKindHoldWithoutAFallbackWaitsUntilResumed(t *testing.T) {
	h := heldHarness(t, func(h *harness) { h.cfg.Pipeline.JudgeFallback = "" })
	h.advance(time.Minute)
	h.tick()
	until := heldUntil(h)
	reqWantRounds(t, h, 1)
	pr := h.wantState(2, store.PRQueued)
	raw, _ := h.e.getKV(h.ctx, KVPRWait(pr.ID))
	if w, ok := ParseWait(raw); !ok || w.Reason != WaitKind || w.Subject != agents.KindCodex || !strings.Contains(w.Detail, "codex held") {
		t.Fatalf("wait %q, want codex's hold", raw)
	}
	if got := toastsWith(h, "codex is held"); len(got) != 1 || strings.Contains(got[0], "fallback judge") ||
		!strings.Contains(got[0], "rounds that need it run their judge_fallback, or wait, until a probe launches it") {
		t.Fatalf("the hold's toasts %q, want one that says rounds run their judge_fallback or wait", got)
	}

	res, err := h.e.requestPause(h.ctx, PausePayload{}, false)
	want := "resumed; codex is still held (shared launch failure) until " + until.Local().Format("15:04") + ": `magnum resume --tool codex` lifts it"
	if err != nil || res != want {
		t.Fatalf("a plain resume = %q, %v\nwant %q", res, err, want)
	}
	heldUntil(h)

	h.rd.script = codexLaunches(h)
	resume := h.enqueue(ReqResume, PausePayload{Tool: agents.KindCodex})
	h.tick()
	if r := h.request(resume); r.State != store.RequestDone {
		t.Fatalf("resume: %+v %q", r, deref(r.Result))
	}
	if _, held := h.e.kindHoldOf(h.ctx, agents.KindCodex); held {
		t.Fatal("magnum resume --tool codex left the hold")
	}
	reqWantRounds(t, h, 2)
	h.wantState(2, store.PRReviewed)
}

// A probe whose launch dies on a line other than the hold's is its PR's own
// failure (a slot or a config of its own), not the kind's: its round is
// charged as any lone failure, and the hold stays as it was, its line and
// its end, so the next round that needs the kind probes it again. Renewing
// the hold on such a line held the kind for every PR and, the line now the
// hold's, charged the probing PR nothing, round after round.
func TestAProbeThatDiesOnALineOfItsOwnIsChargedAndLeavesTheHold(t *testing.T) {
	h := heldHarness(t)
	h.advance(time.Minute)
	h.tick() // held: #2 posts with the fallback
	h.wantState(2, store.PRReviewed)
	until := heldUntil(h)
	shared := len(h.events(eventRoundLaunchShared))

	const own = "Error: a line of this PR's own"
	h.rd.script = codexDies(h, own)
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2"})
	h.advance(until.Sub(h.clock.Now()) + 30*time.Minute) // past the hold and the re-review interval
	h.tick()
	reqWantRounds(t, h, 3)
	if names := roleNames(h.rd.all()[2].Roles); !slices.Contains(names, "codex-judge") {
		t.Fatalf("the probe's roles = %v, want codex-judge", names)
	}
	hold, held := h.e.kindHoldOf(h.ctx, agents.KindCodex)
	if !held || hold.Line != bootstrapLine || !hold.Until.Equal(until) || hold.Probes != 0 || hold.ProbePR != 0 {
		t.Fatalf("after a probe that died on a line of its own the hold is %+v (held %v), want it as it was, no probe running", hold, held)
	}
	if evs := h.events(EventKindHoldRenewed); len(evs) != 0 {
		t.Fatalf("a probe that died on a line of its own renewed the hold: %+v", evs)
	}
	pr := h.pr(2)
	if pr.Attempts != 1 || strings.Contains(deref(pr.LastError), "fails the same way") || len(h.events(eventRoundLaunchShared)) != shared {
		t.Fatalf("the probe's own failure: attempts %d, last_error %q, want one attempt charged", pr.Attempts, deref(pr.LastError))
	}

	h.advance(time.Hour) // its retry
	h.tick()
	reqWantRounds(t, h, 4)
	if names := roleNames(h.rd.all()[3].Roles); !slices.Contains(names, "codex-judge") {
		t.Fatalf("the round after a probe that died on a line of its own runs %v, want codex-judge, the next probe", names)
	}
	if evs := h.events(EventKindHoldProbe); len(evs) != 2 {
		t.Fatalf("kind.hold_probe events %+v, want two probes", evs)
	}
	if pr := h.pr(2); pr.Attempts != 2 {
		t.Fatalf("the second probe on the PR's own line: attempts %d, want 2", pr.Attempts)
	}
}

// A health check settles a running probe only when it lifts the hold: a
// probe whose first codex launch died on the hold's line and whose judge's
// restart then took its prompt is a good probe, though a health check came
// between the two.
func TestAProbeWhoseKindLaunchesOnItsRestartLiftsTheHold(t *testing.T) {
	h := heldHarness(t)
	h.advance(time.Minute)
	h.tick() // held: #2 posts with the fallback
	h.wantState(2, store.PRReviewed)
	until := heldUntil(h)

	refused, release := make(chan struct{}), make(chan struct{})
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if !slices.Contains(roleNames(in.Roles), "codex-judge") {
			return posted(in.TargetSHA, 0), nil
		}
		if err := refusedRun(h, in.PR.ID, max(in.Round, 1), "codex-judge", bootstrapLine); err != nil {
			return failRound(h.t, err)
		}
		close(refused)
		select {
		case <-release:
		case <-h.ctx.Done():
			return failRound(h.t, h.ctx.Err())
		}
		return codexLaunches(h)(in)
	}
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b2"})
	h.advance(until.Sub(h.clock.Now()) + 30*time.Minute) // past the hold and the re-review interval
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refused:
	case <-time.After(time.Minute):
		t.Fatal("the probe's round never prompted codex-judge")
	}
	h.advance(30 * time.Second)
	h.e.checkKindHolds(h.ctx) // a health check while the probe's judge restarts
	if hold, held := h.e.kindHoldOf(h.ctx, agents.KindCodex); !held || hold.ProbePR == 0 || hold.Probes != 0 {
		t.Errorf("a health check during the probe settled it: %+v (held %v)", hold, held)
	}
	close(release)
	h.settle()
	reqWantRounds(t, h, 3)
	if hold, held := h.e.kindHoldOf(h.ctx, agents.KindCodex); held {
		t.Fatalf("the probe's judge took its prompt after its restart, and codex stays held: %+v", hold)
	}
	if evs := h.events(EventKindHoldRenewed); len(evs) != 0 {
		t.Fatalf("kind.hold_renewed events %+v, want none", evs)
	}
	h.wantState(2, store.PRReviewed)
}
