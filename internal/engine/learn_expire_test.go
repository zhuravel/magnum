package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// A held or passed proposal nobody decided within ProposalTTL expires at
// the loop's scan: learn.expired, its worktree and branch go, and its
// inputs stay as they are (new, for a later edit). A younger one and a
// gating one (the gate decides it) are left alone.
func TestAHeldOrPassedProposalExpiresAfterAWeek(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	old := f.h.clock.Now().Add(-ProposalTTL - time.Hour)
	add := func(run, state string, at time.Time) store.LearnProposal {
		t.Helper()
		wt := filepath.Join(f.h.layout.LearnWorktrees(), run)
		candidateFiles(t, wt)
		p, err := f.h.st.CreateLearnProposal(f.h.ctx, store.LearnProposalInput{Trigger: store.LearnTriggerEvidence, BaseSHA: learnMaster,
			Branch: "learn/" + run, CandidateSHA: strings.Repeat("6", 40), Files: gateFullFiles, State: state, Scratch: wt, At: at,
			RunDir: filepath.Join(f.h.layout.LearnSkill(), run),
			Inputs: []store.LearnInput{{Kind: store.LearnInputMiss, InputID: f.miss.ID, Outcome: store.MissNoted, Where: "SKILL.md"}}})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	held := add("20260920-070000", store.LearnHeld, old)
	passed := add("20260921-070000", store.LearnPassed, old)
	young := add("20261001-070000", store.LearnHeld, f.h.clock.Now().Add(-ProposalTTL+time.Hour))
	gating := add("20260922-070000", store.LearnGating, old)

	f.h.e.maybeLearnScan(f.h.ctx)
	for _, c := range []struct {
		p    store.LearnProposal
		want string
	}{{held, store.LearnExpired}, {passed, store.LearnExpired}, {young, store.LearnHeld}, {gating, store.LearnGating}} {
		got, err := f.h.st.LearnProposalByID(f.h.ctx, c.p.ID)
		if err != nil || got.State != c.want {
			t.Errorf("proposal %d (%s) = %s, %v; want %s", c.p.ID, c.p.State, got.State, err, c.want)
		}
		_, statErr := os.Stat(c.p.Scratch)
		if gone := os.IsNotExist(statErr); gone != (c.want == store.LearnExpired) {
			t.Errorf("proposal %d's worktree gone = %v", c.p.ID, gone)
		}
		if deleted := len(f.git.called("branch delete "+c.p.Branch)) > 0; deleted != (c.want == store.LearnExpired) {
			t.Errorf("proposal %d's branch deleted = %v", c.p.ID, deleted)
		}
	}
	if got, err := f.h.st.LearnProposalByID(f.h.ctx, held.ID); err != nil || got.DecidedAt == nil || !strings.Contains(got.Reason, "7 days") {
		t.Fatalf("the expired proposal = %+v, %v", got, err)
	}
	if ms, err := f.h.st.Misses(f.h.ctx, store.MissFilter{}); err != nil || len(ms) != 1 || ms[0].State != store.MissNew {
		t.Fatalf("the input after the expiry = %+v, %v", ms, err)
	}
	evs, err := f.h.st.EventsOfKindsSince(f.h.ctx, time.Time{}, "learn.expired")
	if err != nil || len(evs) != 2 {
		t.Fatalf("learn.expired events = %+v, %v", evs, err)
	}
}

// The week before a proposal expires counts from when it began to wait for
// the landing or the operator, not from its making: one whose gate waited
// six days and more (the Codex cap, a pause) before it held it keeps its
// worktree for a week after the hold.
func TestAProposalExpiresAWeekAfterItBeganToWait(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	if _, err := f.h.st.DecideLearnProposal(f.h.ctx, f.p.ID, []string{store.LearnGating}, store.LearnFailed, "", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	run := "20260925-070000"
	wt := filepath.Join(f.h.layout.LearnWorktrees(), run)
	candidateFiles(t, wt)
	p, err := f.h.st.CreateLearnProposal(f.h.ctx, store.LearnProposalInput{Trigger: store.LearnTriggerEvidence, BaseSHA: learnMaster,
		Branch: "learn/" + run, CandidateSHA: strings.Repeat("6", 40), Files: gateFullFiles, State: store.LearnGating, Scratch: wt,
		At: f.h.clock.Now().Add(-ProposalTTL - 24*time.Hour), RunDir: filepath.Join(f.h.layout.LearnSkill(), run)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.st.DecideLearnProposal(f.h.ctx, p.ID, []string{store.LearnGating}, store.LearnHeld, "targets could not be replayed",
		f.h.clock.Now().Add(-time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	f.h.e.maybeLearnScan(f.h.ctx)
	if got, err := f.h.st.LearnProposalByID(f.h.ctx, p.ID); err != nil || got.State != store.LearnHeld {
		t.Fatalf("an hour after the hold = %+v, %v; want held", got, err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("the held proposal lost its worktree: %v", err)
	}
	f.h.advance(ProposalTTL)
	f.h.e.maybeLearnScan(f.h.ctx)
	if got, err := f.h.st.LearnProposalByID(f.h.ctx, p.ID); err != nil || got.State != store.LearnExpired {
		t.Fatalf("a week after the hold = %+v, %v; want expired", got, err)
	}
}
