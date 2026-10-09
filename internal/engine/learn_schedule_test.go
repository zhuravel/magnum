package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// scheduledEditFixture is an edit fixture whose [learn] editor is on.
func scheduledEditFixture(t *testing.T) *editFixture {
	t.Helper()
	f := newEditFixture(t, goodTurn)
	f.h.cfg.Learn.Editor = true
	return f
}

// evidence marks new general evidence, as a retro that stored some does.
func (f *editFixture) evidence() {
	f.h.t.Helper()
	f.h.e.setKV(f.h.ctx, KVLearnEvidence, store.FormatTime(f.h.clock.Now()))
}

// scan runs the loop's scan learnCheckEvery after the last one and waits
// for what it started.
func (f *editFixture) scan() {
	f.h.t.Helper()
	f.h.advance(learnCheckEvery)
	f.h.e.maybeLearnScan(f.h.ctx)
	f.h.settle()
}

func (f *editFixture) proposals() []store.LearnProposal {
	f.h.t.Helper()
	ps, err := f.h.st.LearnProposals(f.h.ctx, nil)
	if err != nil {
		f.h.t.Fatal(err)
	}
	return ps
}

// The scheduled editor runs at a scan after a retro stored new general
// evidence (KVLearnEvidence), at most once a local day: it consumes the
// mark and the day when it starts, so a second mark the same day waits for
// the next day, and nothing runs without a mark or with [learn] editor off.
func TestTheEditorRunsOnceADayAfterNewGeneralEvidence(t *testing.T) {
	f := scheduledEditFixture(t)
	f.scan()
	if ps := f.proposals(); len(ps) != 0 {
		t.Fatalf("an edit without new evidence: %+v", ps)
	}
	f.evidence()
	f.h.cfg.Learn.Editor = false
	f.scan()
	if ps := f.proposals(); len(ps) != 0 {
		t.Fatalf("an edit with [learn] editor off: %+v", ps)
	}
	f.h.cfg.Learn.Editor = true
	f.scan()
	ps := f.proposals()
	if len(ps) != 1 || ps[0].State != store.LearnGating || ps[0].Trigger != store.LearnTriggerEvidence {
		t.Fatalf("proposals = %+v", ps)
	}
	if day, _ := f.h.e.getKV(f.h.ctx, KVLearnEditDay); day != store.DayKey(f.h.clock.Now()) {
		t.Fatalf("edit day = %q", day)
	}
	if v, ok := f.h.e.getKV(f.h.ctx, KVLearnEvidence); ok && v != "" {
		t.Fatalf("the evidence mark stayed: %q", v)
	}
	if calls := f.gt.all(); len(calls) == 0 || calls[0] != f.checkout+": go version" {
		t.Fatalf("the editor did not check go first: %q", calls)
	}

	// The gate failed it; a retro the same day stores more evidence.
	if _, err := f.h.st.DecideLearnProposal(f.h.ctx, ps[0].ID, []string{store.LearnGating}, store.LearnFailed, "no target improved", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	f.evidence()
	f.scan()
	if ps := f.proposals(); len(ps) != 1 {
		t.Fatalf("a second edit the same day: %+v", ps)
	}
	f.h.advance(24 * time.Hour)
	f.scan()
	if ps := f.proposals(); len(ps) != 2 || ps[0].State != store.LearnGating {
		t.Fatalf("the next day's edit = %+v", ps)
	}
}

// judgingPR puts pr in its judge stage: reviewing, its round started now
// and its judge prompted.
func judgingPR(t *testing.T, h *harness, pr store.PR) {
	t.Helper()
	if err := h.st.TransitionPR(h.ctx, pr.ID, nil, store.PRReviewing, func(u *store.PRUpdate) {
		u.Set("last_round_started_at", h.clock.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.judgeRun(pr, store.RunInitial, "base1", store.RunWorking, "", ""); err != nil {
		t.Fatal(err)
	}
}

// The editor waits while a round of any repository is in its judge stage
// (the judge's skill copy is about to be read), under `magnum pause`, an
// infrastructure pause or a pause of its CLI, while a proposal waits for
// the gate, the landing or the operator, and while go does not run in the
// checkout; its mark waits with it.
func TestTheEditorWaitsForAJudgeStageAndAUserPause(t *testing.T) {
	f := scheduledEditFixture(t)
	f.evidence()
	pr := corpusPR(f.h, "example/tools", 3)
	judgingPR(t, f.h, pr)
	f.scan()
	if ps := f.proposals(); len(ps) != 0 {
		t.Fatalf("an edit during a judge stage: %+v", ps)
	}
	if err := f.h.st.TransitionPR(f.h.ctx, pr.ID, nil, store.PRReviewed, nil); err != nil {
		t.Fatal(err)
	}

	f.h.e.setKV(f.h.ctx, store.KVDaemonPaused, "1")
	f.scan()
	if ps := f.proposals(); len(ps) != 0 {
		t.Fatalf("an edit under `magnum pause`: %+v", ps)
	}
	f.h.e.delKV(f.h.ctx, store.KVDaemonPaused)

	f.h.e.setToolPause(f.h.ctx, "claude", "usage_limit", "limit reached", f.h.clock.Now().Add(time.Hour))
	f.scan()
	if ps := f.proposals(); len(ps) != 0 {
		t.Fatalf("an edit while its CLI is paused: %+v", ps)
	}
	f.h.advance(time.Hour)

	held, err := f.h.st.CreateLearnProposal(f.h.ctx, store.LearnProposalInput{Trigger: store.LearnTriggerRequest, BaseSHA: f.master,
		Branch: "learn/old", State: store.LearnHeld})
	if err != nil {
		t.Fatal(err)
	}
	f.scan()
	if ps := f.proposals(); len(ps) != 1 {
		t.Fatalf("an edit while proposal %d is held: %+v", held.ID, ps)
	}
	if _, err := f.h.st.DecideLearnProposal(f.h.ctx, held.ID, []string{store.LearnHeld}, store.LearnRejected, "", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}

	f.gt.mu.Lock()
	f.gt.fail = map[string]string{"go version": "mise: go not found"}
	f.gt.mu.Unlock()
	f.scan()
	if ps := f.proposals(); len(ps) != 1 {
		t.Fatalf("an edit without go: %+v", ps)
	}
	if v, _ := f.h.e.getKV(f.h.ctx, KVLearnEvidence); v == "" {
		t.Fatal("the evidence mark went while the editor waited")
	}
	f.gt.mu.Lock()
	f.gt.fail = nil
	f.gt.mu.Unlock()
	f.scan()
	if ps := f.proposals(); len(ps) != 2 || ps[0].State != store.LearnGating {
		t.Fatalf("the edit once nothing holds it = %+v", ps)
	}
}

// fakeLearnJob marks a learn job running, as its start would; the returned
// func ends it.
func fakeLearnJob(h *harness, job string) func() {
	h.t.Helper()
	now := h.clock.Now()
	switch job {
	case "retro":
		h.e.retroMu.Lock()
		h.e.retroCancel, h.e.retroStarted = func() {}, now
		h.e.retroMu.Unlock()
		return func() {
			h.e.retroMu.Lock()
			h.e.retroCancel = nil
			h.e.retroMu.Unlock()
		}
	case "curation":
		h.e.curateMu.Lock()
		h.e.curateCancel, h.e.curateStarted, h.e.curateRepo = func() {}, now, "example/tools"
		h.e.curateMu.Unlock()
		return func() {
			h.e.curateMu.Lock()
			h.e.curateCancel = nil
			h.e.curateMu.Unlock()
		}
	}
	h.t.Fatalf("no learn job %q", job)
	return nil
}

// Learn jobs run one at a time: a running notes curation holds the day's
// retro, which starts once the curation ended, and a `magnum retro` is
// refused naming the curation; a running retro holds a skill edit and the
// gate of a gating proposal.
func TestLearnJobsRunOneAtATime(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc), func(h *harness) {
		h.cfg.Learn.Enabled = true
		h.cfg.Learn.DailyAt = "07:00"
	})
	retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	end := fakeLearnJob(h, "curation")
	if got := h.e.learnBusy(); got != "the notes curation of example/tools" {
		t.Fatalf("learnBusy = %q", got)
	}
	h.e.maybeRetro(h.ctx)
	h.settle()
	if h.e.retroBusy() {
		t.Fatal("the retro started while a curation ran")
	}
	if _, ok := h.e.getKV(h.ctx, KVRetroDay); ok {
		t.Fatal("the day's retro ran during a curation")
	}
	if req := h.requestRetro(RetroPayload{}); req.State != store.RequestFailed || !strings.Contains(deref(req.Result), "the notes curation of example/tools") {
		t.Fatalf("a retro request during a curation = %s %q", req.State, deref(req.Result))
	}
	end()
	h.e.maybeRetro(h.ctx)
	h.settle()
	if day, _ := h.e.getKV(h.ctx, KVRetroDay); day != store.DayKey(h.clock.Now()) {
		t.Fatalf("the retro did not run once the curation ended: day %q", day)
	}

	end = fakeLearnJob(h, "retro")
	defer end()
	if got := h.e.learnBusy(); got != "the retro" {
		t.Fatalf("learnBusy = %q", got)
	}
	if _, err := h.e.requestEdit(h.ctx, LearnProposePayload{}); err == nil || !strings.Contains(err.Error(), "the retro") {
		t.Fatalf("an edit request during the retro = %v", err)
	}
	if started, _ := h.e.startEdit(h.ctx, store.LearnTriggerRequest); started {
		t.Fatal("an edit started during the retro")
	}
}

// A retro that stores a new general miss of P0 to P2 (or a new general
// finding declined as wrong or over-ranked) marks new evidence for the
// editor; one that only stores again what was already there does not.
func TestARetroThatStoresNewGeneralEvidenceWakesTheEditor(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc))
	retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	h.requestRetro(RetroPayload{})
	if v, _ := h.e.getKV(h.ctx, KVLearnEvidence); v == "" {
		t.Fatal("a retro with a new general miss left no evidence mark")
	}
	evs, err := h.st.EventsOfKindsSince(h.ctx, time.Time{}, "retro.done")
	if err != nil || len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"learn_inputs":1`) {
		t.Fatalf("retro.done = %+v, %v", evs, err)
	}
	h.e.delKV(h.ctx, KVLearnEvidence)
	h.requestRetro(RetroPayload{Again: true})
	if v, ok := h.e.getKV(h.ctx, KVLearnEvidence); ok && v != "" {
		t.Fatalf("a retro that found nothing new marked evidence: %q", v)
	}
}
