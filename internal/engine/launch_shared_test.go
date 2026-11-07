package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// bootstrapLine is the pane line of the 2026-11-05 Codex bootstrap failure.
const bootstrapLine = "Error: account/read failed during TUI bootstrap: account/read failed: failed to load workspace requirements: invalid data (code -32603)"

// launchDied is a round whose judge's prompt herdr refused, also after a
// restart of the judge, because its launch died with line in its pane, as
// the pipeline ends it.
func launchDied(in pipeline.RoundInput, line string) pipeline.RoundResult {
	return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Round: max(in.Round, 1), TargetSHA: in.TargetSHA,
		Error: "pipeline: judge prompt refused, also after a restart of the judge: agents: prompt mg-codex-judge: " +
			"herdr agent.prompt: agent_not_found: agent target mg-codex-judge not found; the pane shows: " + line}
}

// sharedLaunchHarness opens #2, queued, and #3, whose author the watch
// skips: a PR whose agents' lines count, which never runs a round here.
func sharedLaunchHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // the first sync: #1 is a baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1", author: "dependabot"})
	h.tick()
	h.wantState(2, store.PRQueued)
	return h
}

// On 2026-11-05 every codex-judge launch died at the TUI bootstrap with one
// pane line, which no PR caused, and two PRs spent three attempts each,
// four times over, between the operator's forced reviews. A round whose
// error carries a pane line another PR's agent ended with within 30
// minutes charges no attempt and does not count against the daily cap; it
// retries after a wait that doubles from a minute, its last error says the
// launch fails the same way on the PRs, and one round.launch_shared info
// event per round says so without the line. A line no other PR shares is
// charged as before.
func TestARoundThatDiesOnALaunchLineAnotherPRSharesChargesNoAttempt(t *testing.T) {
	h := sharedLaunchHarness(t)
	refusedPrompt(h, 3, "codex-judge", bootstrapLine)
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) { return launchDied(in, bootstrapLine), nil }

	h.advance(5 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 1)
	pr := h.wantState(2, store.PRQueued)
	if pr.Attempts != 0 || pr.RoundsToday != 0 {
		t.Fatalf("after a shared launch failure: attempts %d, rounds_today %d, want 0 and 0", pr.Attempts, pr.RoundsToday)
	}
	if pr.NextAttemptAt == nil || pr.NextAttemptAt.Sub(h.clock.Now()) != time.Minute {
		t.Fatalf("next attempt %v, want in a minute", pr.NextAttemptAt)
	}
	if last := deref(pr.LastError); !strings.Contains(last, "fails the same way on 2 PRs") || !strings.Contains(last, "no attempt charged") {
		t.Fatalf("last_error %q does not say the launch fails the same way on the PRs", last)
	}

	h.advance(time.Minute)
	h.tick()
	reqWantRounds(t, h, 2)
	pr = h.wantState(2, store.PRQueued)
	if pr.Attempts != 0 || pr.NextAttemptAt == nil || pr.NextAttemptAt.Sub(h.clock.Now()) != 2*time.Minute {
		t.Fatalf("the second shared failure: attempts %d, next attempt %v, want 0 and in 2 minutes", pr.Attempts, pr.NextAttemptAt)
	}
	evs := h.events(eventRoundLaunchShared)
	if len(evs) != 2 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, "2 PRs") || strings.Contains(evs[0].Message, "TUI bootstrap") {
		t.Fatalf("round.launch_shared events %+v, want two info events naming the PRs, not the line", evs)
	}

	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return launchDied(in, "Error: a line of this PR's own"), nil
	}
	h.advance(2 * time.Minute)
	h.tick()
	reqWantRounds(t, h, 3)
	if pr := h.wantState(2, store.PRQueued); pr.Attempts != 1 {
		t.Fatalf("a line no other PR shares: attempts %d, want 1", pr.Attempts)
	}
}
