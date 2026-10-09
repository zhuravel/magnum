package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

// landSkill is the candidate's skill in its worktree.
const landSkill = "# Skill\n\n## 4. Findings\nProve five facts.\nWhen two writers share a row, check the lock.\n"

// candidateFiles writes the candidate's skill and prompts into wt.
func candidateFiles(t *testing.T, wt string) {
	t.Helper()
	writeFile(t, wt, learn.SkillFile, landSkill)
	writeFile(t, wt, "prompts/judge-initial.md", "Review {{.URL}}.\n")
	writeFile(t, wt, "prompts/claude-review.md", "Review the change.\n")
}

// newLandFixture is a gate fixture whose proposal the gate passed, with the
// candidate's skill and prompts in its worktree, on a daemon launchd
// supervises.
func newLandFixture(t *testing.T, mods ...func(*harness)) *gateFixture {
	t.Helper()
	f := newGateFixture(t, gateFullFiles, mods...)
	f.h.e.supervised = true
	candidateFiles(t, f.wt)
	f.decide(store.LearnPassed)
	return f
}

// decide moves the fixture's proposal from gating to state, as its gate
// would.
func (f *gateFixture) decide(state string) {
	f.h.t.Helper()
	p, err := f.h.st.DecideLearnProposal(f.h.ctx, f.p.ID, []string{store.LearnGating}, state, f.target.Name+" found (0 → 1 of 1)",
		time.Time{}, []byte(`{"state":"`+state+`"}`))
	if err != nil {
		f.h.t.Fatal(err)
	}
	f.p = p
}

// addPassed adds another passed proposal of run, its worktree on its own
// commit on learnMaster.
func (f *gateFixture) addPassed(run, sha string) store.LearnProposal {
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
		Branch: "learn/" + run, CandidateSHA: sha, Files: gateFullFiles, State: store.LearnPassed, Scratch: wt,
		RunDir: filepath.Join(f.h.layout.LearnSkill(), run)})
	if err != nil {
		f.h.t.Fatal(err)
	}
	return p
}

func (f *gateFixture) versions() []store.LearnVersion {
	f.h.t.Helper()
	vs, err := f.h.st.LearnVersions(f.h.ctx)
	if err != nil {
		f.h.t.Fatal(err)
	}
	return vs
}

func (f *gateFixture) restartMark() (LearnRestartMark, bool) {
	f.h.t.Helper()
	return f.h.e.learnRestart(f.h.ctx)
}

func TestLandingFastForwardsMasterAndRecordsAVersion(t *testing.T) {
	f := newLandFixture(t)
	f.h.tick()

	p := f.proposal()
	if p.State != store.LearnLanded || p.LandedSHA != learnCandidate || p.LandedAt == nil || p.DecidedAt == nil {
		t.Fatalf("proposal = %+v, want landed at %s", p, learnCandidate)
	}
	if got := f.git.called("merge"); !slices.Equal(got, []string{"merge --ff-only learn/" + gateRunID}) {
		t.Fatalf("merges = %q", got)
	}
	if got, _ := f.git.RevParse(f.h.ctx, f.checkout, "refs/heads/master"); got != learnCandidate {
		t.Fatalf("master = %s", got)
	}
	if got := slices.Concat(f.git.called("worktree"), f.git.called("branch")); !slices.Equal(got,
		[]string{"worktree discard " + f.wt, "branch delete learn/" + gateRunID}) {
		t.Fatalf("after the landing: %q, want the worktree and the branch gone", got)
	}
	if got := f.git.called("rebase"); len(got) != 0 {
		t.Fatalf("a branch on master was rebased: %q", got)
	}
	sum := sha256.Sum256([]byte(landSkill))
	vs := f.versions()
	if len(vs) != 1 || vs[0].ProposalID != p.ID || vs[0].LandedSHA != learnCandidate || vs[0].SkillSHA != hex.EncodeToString(sum[:])[:12] ||
		len(vs[0].PromptsSHA) != 12 || vs[0].RestartedAt != nil {
		t.Fatalf("versions = %+v", vs)
	}
	ins, err := f.h.st.LearnProposalInputs(f.h.ctx, p.ID)
	if err != nil || len(ins) != 1 || ins[0].State != store.MissUsed {
		t.Fatalf("inputs = %+v, %v: the noted miss is used", ins, err)
	}
	if n := f.h.e.landedToday(f.h.ctx); n != 1 {
		t.Fatalf("landed today = %d", n)
	}
	if m, ok := f.restartMark(); !ok || m.Version != vs[0].ID || m.Proposal != p.ID || m.Manual {
		t.Fatalf("restart mark = %+v, %v", m, ok)
	}
	if evs := f.h.events("learn.landed"); len(evs) != 1 || len(f.h.events("learn.land_begin")) != 1 {
		t.Fatalf("landed events = %+v", evs)
	}
	if toasts := f.toasts(); !strings.Contains(toasts, fmt.Sprintf("skill version %d landed", vs[0].ID)) || strings.Contains(toasts, "restart magnum") {
		t.Fatalf("toasts = %q", toasts)
	}
}

// A landing a crash cut short after the fast-forward: the next one finds
// the branch on master and records it without merging again, whatever the
// checkout has checked out since.
func TestALandingCutShortAfterTheMergeIsRecorded(t *testing.T) {
	f := newLandFixture(t)
	f.git.set(f.checkout, "refs/heads/master", learnCandidate)
	f.git.branch[f.checkout] = "feature"
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnLanded || p.LandedSHA != learnCandidate || len(f.git.called("merge")) != 0 ||
		len(f.git.called("rebase")) != 0 || len(f.versions()) != 1 {
		t.Fatalf("proposal = %s landed %s, git %q", p.State, p.LandedSHA, f.git.calls)
	}
}

// A landing the operator asked for, cut short after the fast-forward,
// leaves the proposal held with its commit on master: the operator's n
// then records the landing instead of rejecting it and dropping the
// branch, and so does the expiry of a proposal nobody decided.
func TestAProposalMasterHoldsIsRecordedAsLandedNotRejected(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	candidateFiles(t, f.wt)
	f.decide(store.LearnHeld)
	f.git.set(f.checkout, "refs/heads/master", learnCandidate)
	_, err := f.h.e.requestLearnReject(f.h.ctx, LearnDecisionPayload{ID: f.p.ID, Reason: "noise"})
	if err == nil || !strings.Contains(err.Error(), "on master already") {
		t.Fatalf("requestLearnReject = %v, want a refusal naming the landing", err)
	}
	p := f.proposal()
	if p.State != store.LearnLanded || p.LandedSHA != learnCandidate || len(f.versions()) != 1 {
		t.Fatalf("proposal = %s %q landed %s, versions %+v", p.State, p.Reason, p.LandedSHA, f.versions())
	}
	if ins, err := f.h.st.LearnProposalInputs(f.h.ctx, p.ID); err != nil || len(ins) != 1 || ins[0].State != store.MissUsed {
		t.Fatalf("inputs = %+v, %v, want the noted miss used", ins, err)
	}

	f2 := newLandFixture(t)
	f2.git.set(f2.checkout, "refs/heads/master", learnCandidate)
	f2.h.advance(ProposalTTL + time.Hour)
	f2.h.e.expireLearnProposals(f2.h.ctx)
	if p := f2.proposal(); p.State != store.LearnLanded || len(f2.h.events("learn.expired")) != 0 {
		t.Fatalf("an expiring proposal on master = %s %q", p.State, p.Reason)
	}
}

// The rebase and the fast-forward run on a context a shutdown does not
// cancel (with a timeout of their own), so a stop never cuts the landing
// between them.
func TestTheLandingsRebaseAndMergeOutliveAStop(t *testing.T) {
	f := newLandFixture(t)
	f.moveMaster(strings.Repeat("7", 40))
	ctx, cancel := context.WithCancel(f.h.ctx)
	defer cancel()
	var rebaseErr, mergeErr error
	f.git.onRebase = func(c context.Context) {
		cancel() // the daemon stops during the rebase
		rebaseErr = c.Err()
		if _, ok := c.Deadline(); !ok {
			rebaseErr = errors.New("no deadline")
		}
	}
	f.git.onMerge = func(c context.Context) { mergeErr = c.Err() }
	if _, _, err := f.h.e.landProposal(ctx, f.p, false); err != nil {
		t.Fatalf("landProposal: %v", err)
	}
	if rebaseErr != nil || mergeErr != nil {
		t.Fatalf("the rebase's context: %v, the merge's: %v", rebaseErr, mergeErr)
	}
	if p := f.proposal(); p.State != store.LearnLanded || len(f.git.called("merge")) != 1 {
		t.Fatalf("proposal = %s %q", p.State, p.Reason)
	}
}

// A start marks the newest version restarted even when its landing wrote
// no restart mark (a crash right after the landing's transaction): the
// daemon that starts reads the landed prompts and skill.
func TestAStartMarksAVersionWhoseRestartMarkIsMissing(t *testing.T) {
	f := newLandFixture(t)
	f.h.tick()
	f.h.e.delKV(f.h.ctx, KVLearnRestart)
	f.h.advance(time.Minute)
	f.h.startup()
	if vs := f.versions(); len(vs) != 1 || vs[0].RestartedAt == nil || !vs[0].RestartedAt.Equal(f.h.clock.Now()) {
		t.Fatalf("versions = %+v, want the newest restarted now", vs)
	}
}

func TestLandingHoldsOnADirtySkillFile(t *testing.T) {
	f := newLandFixture(t)
	f.git.dirty[f.checkout] = []string{"README.md", learn.SkillFile}
	f.h.tick()

	p := f.proposal()
	if p.State != store.LearnHeld || !strings.Contains(p.Reason, learn.SkillFile+" has uncommitted changes") || strings.Contains(p.Reason, "README") {
		t.Fatalf("proposal = %s %q, want held naming the skill", p.State, p.Reason)
	}
	if got := f.git.called("merge"); len(got) != 0 {
		t.Fatalf("merged over the operator's edit: %q", got)
	}
	if got := f.git.called("worktree"); len(got) != 0 {
		t.Fatalf("a held proposal lost its worktree: %q", got)
	}
	if evs := f.h.events("learn.held"); len(evs) != 1 || !strings.Contains(f.toasts(), fmt.Sprintf("skill proposal %d waits for review", p.ID)) {
		t.Fatalf("held events = %+v", evs)
	}
	if len(f.versions()) != 0 {
		t.Fatal("a held proposal made a version")
	}
	// Each check before it holds too: master not checked out, an operation
	// in progress.
	for _, c := range []struct {
		set  func(f *gateFixture)
		want string
	}{
		{func(f *gateFixture) { f.git.branch[f.checkout] = "feature" }, "the checkout is on feature, not master"},
		{func(f *gateFixture) { f.git.op[f.checkout] = "rebase" }, "a rebase is in progress in the checkout"},
	} {
		f := newLandFixture(t)
		c.set(f)
		f.h.tick()
		if p := f.proposal(); p.State != store.LearnHeld || !strings.Contains(p.Reason, c.want) || len(f.git.called("merge")) != 0 {
			t.Fatalf("proposal = %s %q, want held: %s", p.State, p.Reason, c.want)
		}
	}
}

// moveMaster commits sha on the checkout's master, as the operator would.
func (f *gateFixture) moveMaster(sha string) {
	f.git.mu.Lock()
	defer f.git.mu.Unlock()
	f.git.parent[sha] = f.git.refs[f.checkout+"|refs/heads/"+learnBase]
	f.git.refs[f.checkout+"|refs/heads/"+learnBase], f.git.refs[f.checkout+"|HEAD"] = sha, sha
}

func TestLandingRebasesOnceAndHoldsOnAConflict(t *testing.T) {
	moved := strings.Repeat("7", 40)
	f := newLandFixture(t)
	f.moveMaster(moved)
	f.h.tick()

	p := f.proposal()
	// Only the candidate's commit is replayed: the commits after its base.
	if got := f.git.called("rebase"); !slices.Equal(got, []string{"rebase " + f.wt + " " + moved + " " + learnMaster}) {
		t.Fatalf("rebases = %q, want one of the candidate onto the moved master", got)
	}
	rebased, _ := f.git.RevParse(f.h.ctx, f.checkout, "refs/heads/master")
	if p.State != store.LearnLanded || rebased == learnCandidate || rebased == moved || p.LandedSHA != rebased {
		t.Fatalf("proposal = %s landed %s, master %s", p.State, p.LandedSHA, rebased)
	}

	// A landing held after its rebase (a dirty file) lands later without a
	// second rebase: the branch is on master now.
	f3 := newLandFixture(t)
	f3.moveMaster(moved)
	f3.git.dirty[f3.checkout] = []string{learn.SkillFile}
	f3.h.tick()
	if p := f3.proposal(); p.State != store.LearnHeld || len(f3.git.called("rebase")) != 1 {
		t.Fatalf("proposal = %s %q after %q", p.State, p.Reason, f3.git.called("rebase"))
	}
	f3.git.dirty[f3.checkout] = nil
	if got, err := f3.h.e.requestLearnLand(f3.h.ctx, LearnDecisionPayload{ID: f3.p.ID}); err != nil || !strings.Contains(got, "landed") {
		t.Fatalf("requestLearnLand = %q, %v", got, err)
	}
	if p := f3.proposal(); p.State != store.LearnLanded || len(f3.git.called("rebase")) != 1 {
		t.Fatalf("proposal = %s, rebases %q", p.State, f3.git.called("rebase"))
	}

	f2 := newLandFixture(t)
	f2.moveMaster(moved)
	f2.git.rebaseErr = fmt.Errorf("gitx: rebase: %w", gitx.ErrRebaseConflict)
	f2.h.tick()
	p2 := f2.proposal()
	if p2.State != store.LearnHeld || !strings.Contains(p2.Reason, "conflicts") || len(f2.git.called("merge")) != 0 || len(f2.git.called("rebase")) != 1 {
		t.Fatalf("proposal = %s %q, merges %q", p2.State, p2.Reason, f2.git.called("merge"))
	}
}

// A master the operator reset below the proposal's base (a commit of his
// dropped) holds the landing: the gate measured the candidate on that
// base, and a fast-forward would bring the dropped commit back.
func TestALandingHoldsWhenMasterNoLongerHoldsItsBase(t *testing.T) {
	f := newLandFixture(t)
	older := strings.Repeat("a", 40)
	f.git.mu.Lock()
	f.git.parent[learnMaster] = older
	f.git.mu.Unlock()
	f.git.set(f.checkout, "refs/heads/master", older)
	f.git.set(f.checkout, "HEAD", older)
	f.h.tick()
	p := f.proposal()
	if p.State != store.LearnHeld || !strings.Contains(p.Reason, "no longer holds") || !strings.Contains(p.Reason, learnShort(learnMaster)) {
		t.Fatalf("proposal = %s %q, want held: master lost its base", p.State, p.Reason)
	}
	if got := slices.Concat(f.git.called("rebase"), f.git.called("merge")); len(got) != 0 {
		t.Fatalf("git = %q, want no rebase and no merge", got)
	}
}

// The branch of a proposal may have moved only by an earlier landing's
// rebase: a commit made in its worktree on top of the candidate, or an
// amended candidate, would land ungated and holds the proposal; the
// candidate rebased onto master (one commit on master, its patch id) lands.
func TestALandingRefusesACommitMadeInTheWorktree(t *testing.T) {
	for _, c := range []struct {
		name   string
		sha    string
		parent string
	}{
		{"on top of the candidate", strings.Repeat("6", 40), learnCandidate},
		{"an amended candidate", strings.Repeat("8", 40), learnMaster},
	} {
		f := newLandFixture(t)
		f.git.mu.Lock()
		f.git.parent[c.sha] = c.parent
		f.git.mu.Unlock()
		f.git.set(f.wt, "HEAD", c.sha)
		f.git.set(f.checkout, "refs/heads/learn/"+gateRunID, c.sha)
		f.h.tick()
		if p := f.proposal(); p.State != store.LearnHeld || !strings.Contains(p.Reason, "moved") || len(f.git.called("merge")) != 0 {
			t.Fatalf("%s: proposal = %s %q, merges %q", c.name, p.State, p.Reason, f.git.called("merge"))
		}
	}

	// The candidate as an earlier landing rebased it onto master.
	moved, rebased := strings.Repeat("7", 40), strings.Repeat("9", 40)
	f := newLandFixture(t)
	f.moveMaster(moved)
	f.git.mu.Lock()
	f.git.parent[rebased], f.git.patch[rebased] = moved, f.git.patchOf(learnCandidate)
	f.git.mu.Unlock()
	f.git.set(f.wt, "HEAD", rebased)
	f.git.set(f.checkout, "refs/heads/learn/"+gateRunID, rebased)
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnLanded || p.LandedSHA != rebased || len(f.git.called("rebase")) != 0 {
		t.Fatalf("the rebased candidate: proposal = %s %q landed %s, rebases %q", p.State, p.Reason, p.LandedSHA, f.git.called("rebase"))
	}
}

func TestOneLandingPerDay(t *testing.T) {
	f := newLandFixture(t)
	second := f.addPassed("20261005-093000", strings.Repeat("5", 40))
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnLanded {
		t.Fatalf("the oldest = %s", p.State)
	}
	// The daemon restarts on the version; the second waits for tomorrow.
	f.h.startup()
	f.h.tick()
	if p, _ := f.h.st.LearnProposalByID(f.h.ctx, second.ID); p.State != store.LearnPassed || len(f.versions()) != 1 {
		t.Fatalf("the second = %s on the same day, versions %d", p.State, len(f.versions()))
	}
	f.h.advance(24 * time.Hour)
	f.h.tick()
	if p, _ := f.h.st.LearnProposalByID(f.h.ctx, second.ID); p.State != store.LearnLanded || len(f.versions()) != 2 {
		t.Fatalf("the second = %s the next day", p.State)
	}
	// It landed on the first: its branch was rebased onto the new master.
	if got := f.git.called("rebase"); len(got) != 1 || !strings.HasSuffix(got[0], " "+learnCandidate+" "+learnMaster) {
		t.Fatalf("rebases = %q", got)
	}

	// A version the daemon has not restarted on holds the next landing,
	// whatever land_per_day allows.
	f2 := newLandFixture(t, func(h *harness) { h.cfg.Learn.LandPerDay = 3 })
	f2.h.e.supervised = false // the restart is the operator's: the daemon keeps ticking
	third := f2.addPassed("20261005-093000", strings.Repeat("5", 40))
	f2.h.tick()
	f2.h.tick()
	if p, _ := f2.h.st.LearnProposalByID(f2.h.ctx, third.ID); p.State != store.LearnPassed {
		t.Fatalf("a second landing before the restart: %s", p.State)
	}
	f2.h.startup()
	f2.h.tick()
	if p, _ := f2.h.st.LearnProposalByID(f2.h.ctx, third.ID); p.State != store.LearnLanded {
		t.Fatalf("the second after the restart, land_per_day 3: %s", p.State)
	}
}

func TestALandedVersionAsksForAnIdleRestart(t *testing.T) {
	f := newLandFixture(t)
	f.h.tick() // lands
	// Background work in flight holds the restart.
	f.h.e.gateMu.Lock()
	f.h.e.gateCancel = func() {}
	f.h.e.gateMu.Unlock()
	if err := f.h.e.Tick(f.h.ctx); err != nil {
		t.Fatalf("a restart with a gate in flight: %v", err)
	}
	f.h.e.gateMu.Lock()
	f.h.e.gateCancel = nil
	f.h.e.gateMu.Unlock()
	if err := f.h.e.Tick(f.h.ctx); !errors.Is(err, ErrRestartForLearn) {
		t.Fatalf("tick = %v, want ErrRestartForLearn", err)
	}
	if _, ok := f.restartMark(); !ok {
		t.Fatal("the exit cleared the mark: the next daemon could not mark the version")
	}

	// [learn] restart manual, or no launchd to start it again: the daemon
	// keeps running, and a toast and the mark ask the operator.
	for _, c := range []struct {
		name string
		mod  func(*harness)
		sup  bool
	}{
		{"manual", func(h *harness) { h.cfg.Learn.Restart = config.LearnRestartManual }, true},
		{"unsupervised", func(*harness) {}, false},
	} {
		f := newLandFixture(t, c.mod)
		f.h.e.supervised = c.sup
		f.h.tick()
		if err := f.h.e.Tick(f.h.ctx); err != nil {
			t.Fatalf("%s: tick = %v, want the daemon to keep running", c.name, err)
		}
		m, ok := f.restartMark()
		if !ok || !m.Manual {
			t.Fatalf("%s: mark = %+v, %v", c.name, m, ok)
		}
		if toasts := f.toasts(); !strings.Contains(toasts, fmt.Sprintf("restart magnum for skill version %d", m.Version)) {
			t.Fatalf("%s: toasts = %q", c.name, toasts)
		}
	}
}

func TestTheNewDaemonMarksTheVersionRestarted(t *testing.T) {
	f := newLandFixture(t)
	f.h.tick()
	f.h.advance(5 * time.Minute)
	restarted := f.h.clock.Now()
	f.h.startup()

	vs := f.versions()
	if len(vs) != 1 || vs[0].RestartedAt == nil || !vs[0].RestartedAt.Equal(restarted) {
		t.Fatalf("versions = %+v, want restarted at %s", vs, restarted)
	}
	if _, ok := f.restartMark(); ok {
		t.Fatal("the mark outlived the restart")
	}
	if evs := f.h.events("learn.restart"); len(evs) != 1 || !strings.Contains(evs[0].Message, fmt.Sprintf("skill version %d", vs[0].ID)) {
		t.Fatalf("restart events = %+v", evs)
	}
	// A later start changes nothing.
	f.h.advance(time.Hour)
	f.h.startup()
	if vs := f.versions(); !vs[0].RestartedAt.Equal(restarted) || len(f.h.events("learn.restart")) != 1 {
		t.Fatalf("a second start moved the window: %+v", vs)
	}
}

func TestLearnLandRequestLandsAHeldProposal(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	f.h.e.supervised = true
	candidateFiles(t, f.wt)
	f.decide(store.LearnHeld)
	// The day's landing is spent: the operator's answer lands it anyway.
	day, _ := json.Marshal(LandDay{Day: store.DayKey(f.h.clock.Now()), Landed: 1})
	f.h.e.setKV(f.h.ctx, KVLearnLandDay, string(day))
	id := f.h.enqueue(ReqLearnLand, LearnDecisionPayload{ID: f.p.ID})
	f.h.tick()

	r := f.h.request(id)
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), fmt.Sprintf("landed proposal %d on master", f.p.ID)) ||
		!strings.Contains(deref(r.Result), "restarts on it once nothing is in flight") {
		t.Fatalf("request = %s %q", r.State, deref(r.Result))
	}
	if p := f.proposal(); p.State != store.LearnLanded || len(f.versions()) != 1 {
		t.Fatalf("proposal = %s", p.State)
	}

	// A held proposal whose landing cannot go on stays held, and the
	// answer says why (no toast: the operator reads the answer).
	f2 := newGateFixture(t, gateFullFiles)
	candidateFiles(t, f2.wt)
	f2.decide(store.LearnHeld)
	f2.git.dirty[f2.checkout] = []string{learn.SkillFile}
	id = f2.h.enqueue(ReqLearnLand, LearnDecisionPayload{ID: f2.p.ID})
	f2.h.tick()
	r = f2.h.request(id)
	if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "stays held") || !strings.Contains(deref(r.Result), learn.SkillFile) {
		t.Fatalf("request = %s %q", r.State, deref(r.Result))
	}
	if p := f2.proposal(); p.State != store.LearnHeld || strings.Contains(f2.toasts(), "waits for review") {
		t.Fatalf("proposal = %s, toasts %q", p.State, f2.toasts())
	}
	// Only a held proposal waits for the operator.
	f3 := newLandFixture(t)
	if _, err := f3.h.e.requestLearnLand(f3.h.ctx, LearnDecisionPayload{ID: f3.p.ID}); err == nil || !strings.Contains(err.Error(), "only a held proposal") {
		t.Fatalf("landing a passed proposal on request: %v", err)
	}
	if _, err := os.Stat(f3.wt); err != nil {
		t.Fatalf("a refused request touched the worktree: %v", err)
	}
}
