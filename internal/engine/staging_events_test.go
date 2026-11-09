package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/staging"
)

// testEffect is PR #2's deploy to void as Step left it at seq.
func testEffect(seq int, step, state string) staging.Effect {
	return staging.Effect{ID: "void-2-20261005t100000-abcd", Repo: "talkable/talkable", Env: envVoid,
		PR: staging.PR{Repo: "talkable/talkable", Number: 2, Head: fixHead}, By: staging.ByCLI, Step: step, State: state, Try: 1, Seq: seq}
}

// stagingDeployRecords are PR #n's deploy records as the board reads them.
func (h *harness) stagingDeployRecords(n int) []StagingDeployRecord {
	h.t.Helper()
	values, err := h.st.KVValues(h.ctx, KVStagingDeploys)
	if err != nil {
		h.t.Fatal(err)
	}
	return StagingDeploysOf(values, "talkable/talkable", n)
}

// Each step's event of a deploy (staging's Deps.Event: at least once, from
// its outbox) goes into magnum's registry once, on the PR's subject, keyed
// by (effect, Seq, step, phase): a repeat writes nothing. The effect's
// record, which the board and `magnum staging` read, keeps the event of
// its newest Seq: a late event of an older write is in the registry but
// leaves the record. A fail is a warning. The tick drops an ended effect's
// record a day after its last event, never a live one's.
func TestAStagingDeploysEventsGoIntoTheRegistryOnceInSeqOrder(t *testing.T) {
	h := deployHarness(t, &fakeStaging{})
	h.e.stagingEvent(testEffect(4, staging.StepPush, staging.EffectRunning), staging.StepPush, staging.PhaseBegin, "push of 6b6b6b6 to staging-void")
	h.e.stagingEvent(testEffect(4, staging.StepPush, staging.EffectRunning), staging.StepPush, staging.PhaseBegin, "push of 6b6b6b6 to staging-void")
	h.e.stagingEvent(testEffect(3, staging.StepVerify, staging.EffectRunning), staging.StepVerify, staging.PhaseOK, "the merge is [tip, head]")
	evs := h.events("staging.deploy")
	if len(evs) != 2 || !strings.Contains(evs[0].Message, "void-2-20261005t100000-abcd") || !strings.Contains(evs[0].Message, "push begin: push of 6b6b6b6") {
		t.Fatalf("events %+v", evs)
	}
	recs := h.stagingDeployRecords(2)
	if len(recs) != 1 {
		t.Fatalf("records %+v", recs)
	}
	if r := recs[0]; r.Effect != "void-2-20261005t100000-abcd" || r.Env != "void" || r.Step != staging.StepPush || r.Phase != staging.PhaseBegin ||
		r.Seq != 4 || r.State != staging.EffectRunning || r.Head != fixHead || !r.Live() || r.Text != "push of 6b6b6b6 to staging-void" {
		t.Fatalf("record %+v", r)
	}
	if got := h.stagingDeployRecords(3); len(got) != 0 {
		t.Fatalf("#3's records %+v", got)
	}

	end := testEffect(9, staging.StepConfirm, staging.EffectFailed)
	end.Why = "ci.yml failed on the merge"
	h.e.stagingEvent(end, staging.StepWatch, staging.PhaseFail, "ci.yml failed")
	if r := h.stagingDeployRecords(2)[0]; r.State != staging.EffectFailed || r.Live() || r.Why != "ci.yml failed on the merge" {
		t.Fatalf("the ended record %+v", r)
	}
	if evs := h.events("staging.deploy"); evs[len(evs)-1].Level != "warn" {
		t.Errorf("a fail is %s", evs[len(evs)-1].Level)
	}
	live := testEffect(2, staging.StepClaim, staging.EffectQueued)
	live.ID, live.Env = "bart-2-20261005t100000-abcd", envBart
	h.e.stagingEvent(live, staging.StepClaim, staging.PhaseBegin, "")

	h.advance(23 * time.Hour)
	h.tick()
	if got := len(h.stagingDeployRecords(2)); got != 2 {
		t.Fatalf("%d records a day after, want both", got)
	}
	h.advance(2 * time.Hour)
	h.tick()
	recs = h.stagingDeployRecords(2)
	if len(recs) != 1 || recs[0].Env != "bart" {
		t.Fatalf("records after the ended one's day: %+v", recs)
	}
	if !slices.ContainsFunc(recs, StagingDeployRecord.Live) {
		t.Fatal("the live record went")
	}
}
