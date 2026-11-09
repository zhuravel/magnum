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
// record a day after its last event, never the record of one its journal
// says still runs.
func TestAStagingDeploysEventsGoIntoTheRegistryOnceInSeqOrder(t *testing.T) {
	f := &fakeStaging{}
	h := deployHarness(t, f)
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
	f.mu.Lock()
	f.effects = map[string][]staging.Effect{"talkable/talkable": {live}}
	f.mu.Unlock()

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

// Seq is part of an event's key: a step that runs again (a retry, a
// resumed effect) writes the same step and phase at a later Seq, and both
// land, the record keeping the later. A repeat of the newest Seq's event
// rewrites a record its first write lost (the event landed, the record's
// write after it failed), but never turns a record back to an earlier
// event of the same Seq.
func TestAStagingDeploysEventsOfEachJournalWriteLand(t *testing.T) {
	h := deployHarness(t, &fakeStaging{})
	h.e.stagingEvent(testEffect(4, staging.StepPush, staging.EffectRunning), staging.StepPush, staging.PhaseBegin, "push of 6b6b6b6")
	h.e.stagingEvent(testEffect(6, staging.StepPush, staging.EffectRunning), staging.StepPush, staging.PhaseBegin, "push of 7c7c7c7")
	if evs := h.events("staging.deploy"); len(evs) != 2 {
		t.Fatalf("%d events of two pushes' begins at Seq 4 and 6, want 2: %+v", len(evs), evs)
	}
	if r := h.stagingDeployRecords(2); len(r) != 1 || r[0].Seq != 6 || r[0].Text != "push of 7c7c7c7" {
		t.Fatalf("records %+v", r)
	}

	h.e.stagingEvent(testEffect(7, staging.StepPush, staging.EffectRunning), staging.StepPush, staging.PhaseOK, "pushed 7c7c7c7")
	h.e.stagingEvent(testEffect(7, staging.StepWatch, staging.EffectRunning), staging.StepWatch, staging.PhaseBegin, "watching ci.yml")
	h.e.stagingEvent(testEffect(7, staging.StepPush, staging.EffectRunning), staging.StepPush, staging.PhaseOK, "pushed 7c7c7c7") // a repeat
	if r := h.stagingDeployRecords(2)[0]; r.Seq != 7 || r.Step != staging.StepWatch {
		t.Fatalf("a repeat turned the record back: %+v", r)
	}
	h.e.delKV(h.ctx, KVStagingDeploy("void-2-20261005t100000-abcd")) // the record's write lost
	h.e.stagingEvent(testEffect(7, staging.StepWatch, staging.EffectRunning), staging.StepWatch, staging.PhaseBegin, "watching ci.yml")
	if r := h.stagingDeployRecords(2); len(r) != 1 || r[0].Seq != 7 || r[0].Step != staging.StepWatch || r[0].Text != "watching ci.yml" {
		t.Fatalf("the repeat did not rewrite the lost record: %+v", r)
	}
	if n := len(h.events("staging.deploy")); n != 4 {
		t.Fatalf("%d events, want 4: a repeat writes none", n)
	}
}

// The library delivers an event once it is written in the journal, and
// never again when the registry refused it (busy past stagingEventWait):
// the tick mends each live record from its effect's journal
// (Staging.Effects: files under mirror_dir, no git). One behind a newer
// journal write takes the journal's state, so a lost end does not read as
// live for ever; one level with its journal keeps its event's words; and a
// live one no journal holds is dropped a day after its last event, as an
// ended one is.
func TestALiveStagingDeploysRecordCatchesUpWithItsJournal(t *testing.T) {
	f := &fakeStaging{}
	h := deployHarness(t, f)
	h.e.stagingEvent(testEffect(8, staging.StepWatch, staging.EffectRunning), staging.StepWatch, staging.PhaseBegin, "watching ci.yml")
	bart := testEffect(3, staging.StepPush, staging.EffectRunning)
	bart.ID, bart.Env = "bart-2-20261005t100000-abcd", envBart
	h.e.stagingEvent(bart, staging.StepPush, staging.PhaseBegin, "push of 6b6b6b6")
	gone := testEffect(2, staging.StepClaim, staging.EffectQueued)
	gone.ID, gone.Env = "bastion-2-20261005t100000-abcd", envBastion
	h.e.stagingEvent(gone, staging.StepClaim, staging.PhaseBegin, "")

	end := testEffect(9, staging.StepWatch, staging.EffectFailed)
	end.Why, end.Updated = "ci.yml failed on the merge", h.clock.Now()
	f.mu.Lock()
	f.effects = map[string][]staging.Effect{"talkable/talkable": {end, bart}} // bastion's journal is gone
	f.mu.Unlock()
	h.tick()
	recs := map[string]StagingDeployRecord{}
	for _, r := range h.stagingDeployRecords(2) {
		recs[r.Env] = r
	}
	if r := recs["void"]; r.Seq != 9 || r.State != staging.EffectFailed || r.Live() || r.Why != "ci.yml failed on the merge" ||
		r.Step != staging.StepWatch || r.Text != "" {
		t.Fatalf("void's record did not catch up with its journal: %+v", r)
	}
	if r := recs["bart"]; r.Seq != 3 || r.Text != "push of 6b6b6b6" || !r.Live() {
		t.Fatalf("bart's record, level with its journal, changed: %+v", r)
	}
	if r, ok := recs["bastion"]; !ok || !r.Live() {
		t.Fatalf("bastion's record went before its day: %+v", recs)
	}

	h.advance(stagingDeployKeep + time.Hour)
	h.tick()
	for _, r := range h.stagingDeployRecords(2) {
		if r.Env == "bastion" || r.Env == "void" {
			t.Errorf("%s's record stayed a day after its last event: %+v", r.Env, r)
		}
	}
	if r := h.stagingDeployRecords(2); len(r) != 1 || r[0].Env != "bart" {
		t.Fatalf("records %+v, want bart's alone: its journal still runs", r)
	}
}
