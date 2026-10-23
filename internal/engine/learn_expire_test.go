package engine

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/learn"
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

// addWaiting adds a proposal of run in state (held or passed) on its own
// commit sha on learnMaster, its worktree and branch there, the noted miss
// its input, as the gate left it.
func (f *gateFixture) addWaiting(run, sha, state string) store.LearnProposal {
	f.h.t.Helper()
	wt := filepath.Join(f.h.layout.LearnWorktrees(), run)
	candidateFiles(f.h.t, wt)
	f.git.set(wt, "HEAD", sha)
	f.git.set(f.checkout, "refs/heads/learn/"+run, sha)
	f.git.mu.Lock()
	f.git.parent[sha] = learnMaster
	f.git.worktree[wt] = [2]string{f.checkout, "learn/" + run}
	f.git.mu.Unlock()
	p, err := f.h.st.CreateLearnProposal(f.h.ctx, store.LearnProposalInput{Trigger: store.LearnTriggerEvidence, BaseSHA: learnMaster,
		Branch: "learn/" + run, CandidateSHA: sha, Files: gateFullFiles, State: state, Scratch: wt, At: f.h.clock.Now(),
		RunDir: filepath.Join(f.h.layout.LearnSkill(), run),
		Inputs: []store.LearnInput{{Kind: store.LearnInputMiss, InputID: f.miss.ID, Outcome: store.MissNoted, Where: "SKILL.md"}}})
	if err != nil {
		f.h.t.Fatal(err)
	}
	return p
}

func (f *gateFixture) stateOf(p store.LearnProposal) store.LearnProposal {
	f.h.t.Helper()
	got, err := f.h.st.LearnProposalByID(f.h.ctx, p.ID)
	if err != nil {
		f.h.t.Fatal(err)
	}
	return got
}

// A held or passed proposal whose commit no longer applies to master (the
// operator moved the lines it edits) holds no decision for anyone: it
// expires at the scan, however young, with the files that conflict in its
// reason and its learn.expired event, its worktree and branch gone, its
// inputs still new (nothing dismissed). The check runs in the proposal's
// own worktree, never in the checkout; one that still applies, and a
// gating one (the gate decides it), stay as they are.
func TestAProposalMasterMovedUnderExpiresAtTheScan(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	held := f.addWaiting("20261001-070000", strings.Repeat("6", 40), store.LearnHeld)
	passed := f.addWaiting("20261002-070000", strings.Repeat("7", 40), store.LearnPassed)
	fresh := f.addWaiting("20261003-070000", strings.Repeat("8", 40), store.LearnHeld)
	f.git.conflicts[held.CandidateSHA] = []string{learn.SkillFile, "prompts/judge-shared.md"}
	f.git.conflicts[passed.CandidateSHA] = []string{"prompts/judge-initial.md"}
	f.git.conflicts[f.p.CandidateSHA] = []string{learn.SkillFile} // gating: the gate's to decide

	f.h.e.maybeLearnScan(f.h.ctx)

	for _, c := range []struct {
		p    store.LearnProposal
		want string
	}{{held, store.LearnExpired}, {passed, store.LearnExpired}, {fresh, store.LearnHeld}, {f.p, store.LearnGating}} {
		got := f.stateOf(c.p)
		if got.State != c.want {
			t.Errorf("proposal %d (%s) = %s; want %s", c.p.ID, c.p.State, got.State, c.want)
		}
		_, statErr := os.Stat(c.p.Scratch)
		if gone := os.IsNotExist(statErr); gone != (c.want == store.LearnExpired) {
			t.Errorf("proposal %d's worktree gone = %v", c.p.ID, gone)
		}
		if deleted := len(f.git.called("branch delete "+c.p.Branch)) > 0; deleted != (c.want == store.LearnExpired) {
			t.Errorf("proposal %d's branch deleted = %v", c.p.ID, deleted)
		}
	}
	got := f.stateOf(held)
	if got.DecidedAt == nil || got.Reason != "master moved: "+learn.SkillFile+", prompts/judge-shared.md" {
		t.Fatalf("the expired proposal = %+v; want it decided with the conflicting files as its reason", got)
	}
	evs, err := f.h.st.EventsOfKindsSince(f.h.ctx, time.Time{}, "learn.expired")
	if err != nil || len(evs) != 2 {
		t.Fatalf("learn.expired events = %+v, %v", evs, err)
	}
	msgs := evs[0].Message + "\n" + evs[1].Message
	for _, want := range []string{"master moved: " + learn.SkillFile + ", prompts/judge-shared.md", "master moved: prompts/judge-initial.md"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("the events say %q; want %q", msgs, want)
		}
	}
	if strings.Contains(msgs, "7 days") {
		t.Errorf("a proposal of this hour expired for a week's wait: %q", msgs)
	}
	if ms, err := f.h.st.Misses(f.h.ctx, store.MissFilter{}); err != nil || len(ms) != 1 || ms[0].State != store.MissNew {
		t.Fatalf("the input after the expiry = %+v, %v; want it new, not dismissed", ms, err)
	}
	// Each check ran in the worktree of the proposal it tried, against
	// master, and none in the checkout or for the gating proposal.
	ran := f.git.called("replay")
	for _, c := range ran {
		if strings.HasPrefix(c, "replay "+f.checkout+" ") || strings.HasSuffix(c, " "+f.p.CandidateSHA) {
			t.Errorf("the check ran for the wrong target: %q", c)
		}
	}
	want := []string{"replay " + held.Scratch + " " + learnMaster + " " + held.CandidateSHA,
		"replay " + passed.Scratch + " " + learnMaster + " " + passed.CandidateSHA,
		"replay " + fresh.Scratch + " " + learnMaster + " " + fresh.CandidateSHA}
	slices.Sort(ran)
	slices.Sort(want)
	if !slices.Equal(ran, want) {
		t.Fatalf("checks = %q; want %q", ran, want)
	}
}

// A proposal master holds already (a landing cut short after the
// fast-forward) is recorded as landed, as the week's expiry does, whatever
// a check says of it.
func TestAProposalMasterHoldsAlreadyIsLandedNotExpiredAsMoved(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	p := f.addWaiting("20261001-070000", strings.Repeat("6", 40), store.LearnHeld)
	f.git.set(f.checkout, "refs/heads/"+learnBase, p.CandidateSHA) // master is the candidate's commit
	f.git.conflicts[p.CandidateSHA] = []string{learn.SkillFile}

	f.h.e.maybeLearnScan(f.h.ctx)

	if got := f.stateOf(p); got.State != store.LearnLanded || got.LandedSHA != p.CandidateSHA {
		t.Fatalf("the proposal on master = %+v; want it landed", got)
	}
}

// A check that cannot be made (git refuses, the branch or the worktree is
// gone) tells nothing about the proposal: it waits, as it did.
func TestAProposalWhoseCheckCannotBeMadeStaysWaiting(t *testing.T) {
	for name, spoil := range map[string]func(t *testing.T, f *gateFixture, p store.LearnProposal){
		"git refuses": func(_ *testing.T, f *gateFixture, _ store.LearnProposal) {
			f.git.replayErr = errors.New("merge-tree: unknown option")
		},
		"no branch": func(_ *testing.T, f *gateFixture, p store.LearnProposal) {
			delete(f.git.refs, f.checkout+"|refs/heads/"+p.Branch)
		},
		"no worktree": func(t *testing.T, _ *gateFixture, p store.LearnProposal) {
			t.Helper()
			if err := os.RemoveAll(p.Scratch); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newGateFixture(t, gateFullFiles)
			p := f.addWaiting("20261001-070000", strings.Repeat("6", 40), store.LearnHeld)
			f.git.conflicts[p.CandidateSHA] = []string{learn.SkillFile}
			spoil(t, f, p)

			f.h.e.maybeLearnScan(f.h.ctx)

			if got := f.stateOf(p); got.State != store.LearnHeld {
				t.Fatalf("the proposal = %s; want held", got.State)
			}
			if evs, err := f.h.st.EventsOfKindsSince(f.h.ctx, time.Time{}, "learn.expired"); err != nil || len(evs) != 0 {
				t.Fatalf("learn.expired events = %+v, %v", evs, err)
			}
			if got := f.git.called("branch delete"); len(got) != 0 {
				t.Fatalf("a branch was deleted: %q", got)
			}
		})
	}
}

// The 2026-10-20 case end to end, on a real repository: a held proposal
// blocks the scheduled editor until the operator's own commit moves the
// lines it edits, then it expires at the scan and the editor starts in that
// same scan on the master that moved, with new evidence waiting. A commit of
// the operator's elsewhere leaves the proposal held, and the editor waiting.
func TestAHeldProposalMasterMovedUnderExpiresAndTheEditorStartsTheSameDay(t *testing.T) {
	f := scheduledEditFixture(t)
	p := f.edit()
	if _, err := f.h.st.DecideLearnProposal(f.h.ctx, p.ID, []string{store.LearnGating}, store.LearnHeld, "left to you", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	f.evidence()
	operator := func(rel, text, msg string) {
		t.Helper()
		writeFile(t, f.checkout, rel, text)
		f.gitOut(f.checkout, "add", "--", rel)
		f.gitOut(f.checkout, "commit", "--quiet", "-m", msg)
	}

	operator("docs/other.md", "unrelated\n", "the operator edits a doc")
	f.scan()
	if ps := f.proposals(); len(ps) != 1 || ps[0].State != store.LearnHeld {
		t.Fatalf("after a commit elsewhere: %+v; want the one proposal, held", ps)
	}
	if _, err := os.Stat(p.Scratch); err != nil || !f.branchExists(p.Branch) {
		t.Fatalf("the held proposal lost its worktree (%v) or its branch", err)
	}

	operator(learn.SkillFile, editSkillText+"An operator's rule.\n", "the operator edits the skill")
	f.scan()
	ps := f.proposals()
	if len(ps) != 2 || ps[0].State != store.LearnGating || ps[0].BaseSHA != f.gitOut(f.checkout, "rev-parse", "master") || ps[1].ID != p.ID {
		t.Fatalf("after the skill moved: %+v; want the new edit gating on the new master beside the old proposal", ps)
	}
	if ps[1].State != store.LearnExpired || ps[1].Reason != "master moved: "+learn.SkillFile {
		t.Fatalf("the old proposal = %s %q; want expired, master moved: %s", ps[1].State, ps[1].Reason, learn.SkillFile)
	}
	if _, err := os.Stat(p.Scratch); !os.IsNotExist(err) || f.branchExists(p.Branch) {
		t.Fatalf("the expired proposal kept its worktree (%v) or its branch", err)
	}
	if ms, err := f.h.st.Misses(f.h.ctx, store.MissFilter{}); err != nil || len(ms) != 1 || ms[0].State != store.MissNew {
		t.Fatalf("the miss after the expiry = %+v, %v; want new", ms, err)
	}
}
