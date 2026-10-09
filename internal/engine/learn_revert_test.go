package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

// landVersion commits text as the skill on the fixture's master, as a
// landing does, and records it as a version the daemon restarted on at
// restarted.
func (f *editFixture) landVersion(text string, restarted time.Time) store.LearnVersion {
	f.h.t.Helper()
	writeFile(f.h.t, f.checkout, learn.SkillFile, text)
	f.gitOut(f.checkout, "commit", "--quiet", "-am", "A landed skill version.")
	sha := f.gitOut(f.checkout, "rev-parse", "master")
	skill, prompts := learnHashes(f.checkout)
	v, err := f.h.st.CreateLearnVersion(f.h.ctx, store.LearnVersion{LandedSHA: sha, SkillSHA: skill, PromptsSHA: prompts, LandedAt: restarted.Add(-time.Minute)})
	if err != nil {
		f.h.t.Fatal(err)
	}
	if err := f.h.st.MarkLearnRestarted(f.h.ctx, v.ID, restarted); err != nil {
		f.h.t.Fatal(err)
	}
	v.RestartedAt = &restarted
	return v
}

// noCases gives the fixture's daemon an eval with an empty corpus: a gate
// replays nothing and decides on no case.
func (f *editFixture) noCases() {
	f.h.e.d.EvalCase = func(context.Context, eval.Case, EvalGateOptions) eval.CaseRun {
		f.h.t.Error("a gate without cases replayed one")
		return eval.CaseRun{}
	}
	f.h.e.d.EvalInputs = func(string) (string, string, error) { return "in", "rev", nil }
	f.h.e.d.EvalRuns = func() ([]eval.Run, error) { return nil, nil }
	f.h.e.d.EvalCorpus = func() (*eval.Corpus, string, error) { return &eval.Corpus{}, "", nil }
}

// postedWith records a posted review of pr at at with n posted findings,
// declined of which the author declined or argued, by a round that ran the
// skill copy skill.
func postedWith(h *harness, pr store.PR, at time.Time, n, declined int, skill string) {
	h.t.Helper()
	run, err := h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: 1, Role: store.RoleJudge, Kind: store.RunInitial, State: store.RunVerified,
		TargetSHA: "head-" + at.Format("0102"), Identity: "zhuravel", ReviewerLogin: "zhuravel", PromptText: "Read /state/skill/" + skill + "/SKILL.md.",
		Outcome: new("posted"), ReviewID: new(at.Unix()), CreatedAt: at})
	if err != nil {
		h.t.Fatal(err)
	}
	var fs []store.Finding
	for i := range n {
		fs = append(fs, store.Finding{FindingID: "F" + strconv.Itoa(i), Severity: "P2", Verdict: store.FindingPosted})
	}
	if err := h.st.RecordFindings(h.ctx, run.ID, pr.ID, 1, fs); err != nil {
		h.t.Fatal(err)
	}
	for i := range declined {
		outcome := store.DeclineDeclined
		if i%2 == 1 {
			outcome = store.DeclineArgued
		}
		if _, err := h.st.UpsertDecline(h.ctx, store.Decline{PRID: pr.ID, SourceURL: pr.URL + "#r" + at.Format("0102") + strconv.Itoa(i),
			CommentID: at.Unix() + int64(i), RunID: run.ID, ReviewedSHA: run.TargetSHA, SkillSHA: skill, Outcome: outcome}); err != nil {
			h.t.Fatal(err)
		}
	}
}

// A revert restores the files a version changed as they were before it, on
// a branch of its own from master, with the design log's entry that names
// the version and why, through the format gate, and waits for the gate.
func TestARevertRestoresTheFilesOfTheVersionBefore(t *testing.T) {
	f := newEditFixture(t, goodTurn)
	v := f.landVersion(editSkillText+editAddedRule, f.h.clock.Now().Add(-48*time.Hour))
	master := f.gitOut(f.checkout, "rev-parse", "master")
	p, err := f.h.e.runRevert(f.h.ctx, v, store.LearnTriggerRequest, "the operator asked for it")
	if err != nil {
		t.Fatalf("runRevert: %v", err)
	}
	if p.State != store.LearnGating || p.Reverts != v.ID || p.Trigger != store.LearnTriggerRequest || p.BaseSHA != master ||
		!strings.HasPrefix(p.Branch, "learn/") || p.CandidateSHA == "" {
		t.Fatalf("proposal = %+v", p)
	}
	if want := []string{learn.DecisionsFile, editGolden, learn.SkillFile}; !slices.Equal(p.Files, want) {
		t.Fatalf("files = %q, want %q", p.Files, want)
	}
	if got := f.gitOut(f.checkout, "show", p.CandidateSHA+":"+learn.SkillFile); got+"\n" != editSkillText {
		t.Fatalf("the reverted skill = %q", got)
	}
	log := strings.Join(strings.Fields(f.gitOut(f.checkout, "show", p.CandidateSHA+":"+learn.DecisionsFile)), " ")
	if !strings.Contains(log, "**Reverts skill version "+strconv.FormatInt(v.ID, 10)+": the operator asked for it** (") {
		t.Fatalf("the design log:\n%s", log)
	}
	if msg := f.gitOut(f.checkout, "log", "-1", "--format=%B", p.CandidateSHA); !strings.Contains(msg, "No new dependencies.") ||
		!strings.HasPrefix(msg, "Reverts skill version") {
		t.Fatalf("the commit message = %q", msg)
	}
	if got := f.gitOut(f.checkout, "rev-parse", "master"); got != master {
		t.Fatal("the revert moved master")
	}
	calls := f.gt.all()
	if len(calls) != len(formatGate) || !strings.HasSuffix(calls[0], ": "+strings.Join(formatGate[0], " ")) || !strings.Contains(calls[0], "-update") {
		t.Fatalf("format gate = %q", calls)
	}
	b, err := os.ReadFile(filepath.Join(p.RunDir, learn.ChangesFile))
	if ch, cerr := learn.ParseChanges(b); err != nil || cerr != nil || !strings.HasPrefix(ch.Decision, "Reverts skill version") {
		t.Fatalf("the run's changes.json = %s, %v %v", b, err, cerr)
	}
}

// A revert's design-log entry, which carries the operator's reason, is in
// the tree the format gate checks: a private word in it makes the revert
// invalid, and nothing is committed.
func TestARevertsEntryPassesTheFormatGate(t *testing.T) {
	f := newEditFixture(t, goodTurn)
	f.gt.private = "Zorblax"
	v := f.landVersion(editSkillText+editAddedRule, f.h.clock.Now().Add(-48*time.Hour))
	p, err := f.h.e.runRevert(f.h.ctx, v, store.LearnTriggerRequest, "Zorblax asked for it")
	if err != nil {
		t.Fatalf("runRevert: %v", err)
	}
	if p.State != store.LearnInvalid || !strings.Contains(p.Reason, "make check-private") || p.CandidateSHA != "" || f.branchExists(p.Branch) {
		t.Fatalf("proposal = %+v", p)
	}
}

// A version whose reviews had their findings declined or argued 10 points
// more often than the version before (here the skill before the first
// version, over a window as long) gets one revert proposal, which the gate
// holds for the operator, and a toast; a later scan proposes nothing more
// for that version, even once the operator rejected it.
func TestARegressionMakesOneHeldRevertProposal(t *testing.T) {
	f := newEditFixture(t, goodTurn)
	f.noCases()
	f.h.cfg.Learn.RegressionMinFindings = 4
	now := f.h.clock.Now()
	v := f.landVersion(editSkillText+editAddedRule, now.Add(-48*time.Hour))
	pr := corpusPR(f.h, "talkable/talkable", 30)
	postedWith(f.h, pr, now.Add(-72*time.Hour), 4, 0, "older")    // the skill before: 0 of 4 declined
	postedWith(f.h, pr, now.Add(-24*time.Hour), 4, 2, v.SkillSHA) // version 1: 2 of 4

	f.scan()
	ps := f.proposals()
	if len(ps) != 1 || ps[0].Trigger != store.LearnTriggerRegression || ps[0].Reverts != v.ID || ps[0].State != store.LearnGating {
		t.Fatalf("proposals after the scan = %+v", ps)
	}
	if got, _ := f.h.e.getKV(f.h.ctx, KVLearnRegression); got != strconv.FormatInt(v.ID, 10) {
		t.Fatalf("regression mark = %q", got)
	}
	log := strings.Join(strings.Fields(f.gitOut(f.checkout, "show", ps[0].CandidateSHA+":"+learn.DecisionsFile)), " ")
	if !strings.Contains(log, "declined or argued 50% of its 4 posted findings, against 0% of 4 for the skill before it") {
		t.Fatalf("the revert's entry does not say why:\n%s", log)
	}
	f.h.e.maybeGate(f.h.ctx)
	f.h.settle()
	p, err := f.h.st.LearnProposalByID(f.h.ctx, ps[0].ID)
	if err != nil || p.State != store.LearnHeld || !strings.Contains(p.Reason, "revert") {
		t.Fatalf("after the gate = %+v, %v", p, err)
	}
	toasts := f.toasts()
	if !strings.Contains(toasts, "skill version "+strconv.FormatInt(v.ID, 10)+" regressed") ||
		!strings.Contains(toasts, "skill proposal "+strconv.FormatInt(p.ID, 10)+" waits for review") {
		t.Fatalf("toasts:\n%s", toasts)
	}

	f.scan()
	if _, err := f.h.e.requestLearnReject(f.h.ctx, LearnDecisionPayload{ID: p.ID, Reason: "noise"}); err != nil {
		t.Fatal(err)
	}
	f.scan()
	if ps := f.proposals(); len(ps) != 1 {
		t.Fatalf("a second revert for version %d: %+v", v.ID, ps)
	}
}

// The operator's reason for a revert goes into the public design log and
// the commit's subject, so a reason that names a person of the registry's
// pull requests or a watched repository's owner or name is refused before
// any revert starts, as one naming a pull request is; a plain one starts
// the revert.
func TestARevertsReasonNamingAPersonOrARepositoryIsRefused(t *testing.T) {
	f := newEditFixture(t, goodTurn)
	v := f.landVersion(editSkillText+editAddedRule, f.h.clock.Now().Add(-48*time.Hour))
	for reason, want := range map[string]string{
		"Alice saw noise in the reviews": "a login", "noise on Talkable pull requests": "a repository", "see #12": "a pull request",
	} {
		if msg, err := f.h.e.requestLearnRevert(f.h.ctx, LearnRevertPayload{Version: v.ID, Reason: reason}); err == nil ||
			!strings.Contains(err.Error(), "public design log") || !strings.Contains(err.Error(), want) {
			t.Errorf("reason %q: %q, %v (want a refusal naming %s)", reason, msg, err, want)
		}
	}
	f.h.settle()
	if ps := f.proposals(); len(ps) != 0 {
		t.Fatalf("proposals after the refusals = %+v", ps)
	}
	if _, err := f.h.e.requestLearnRevert(f.h.ctx, LearnRevertPayload{Version: v.ID, Reason: "authors declined twice as much"}); err != nil {
		t.Fatal(err)
	}
	f.h.settle()
	if ps := f.proposals(); len(ps) != 1 || ps[0].Reverts != v.ID || ps[0].State != store.LearnGating {
		t.Fatalf("proposals = %+v", ps)
	}
}

// regressed sets up a version whose reviews had 2 of their 4 posted
// findings declined or argued, against none of 4 for the skill before it.
func (f *editFixture) regressed() store.LearnVersion {
	f.h.t.Helper()
	f.h.cfg.Learn.RegressionMinFindings = 4
	now := f.h.clock.Now()
	v := f.landVersion(editSkillText+editAddedRule, now.Add(-48*time.Hour))
	pr := corpusPR(f.h, "talkable/talkable", 30)
	postedWith(f.h, pr, now.Add(-72*time.Hour), 4, 0, "older")
	postedWith(f.h, pr, now.Add(-24*time.Hour), 4, 2, v.SkillSHA)
	return v
}

// A version a waiting proposal reverts already (the operator's `magnum
// learn --revert`) gets no second revert from the scan, which marks it as
// proposed and says nothing.
func TestTheScanDoesNotRevertAVersionAProposalRevertsAlready(t *testing.T) {
	f := newEditFixture(t, goodTurn)
	f.noCases()
	v := f.regressed()
	manual, err := f.h.st.CreateLearnProposal(f.h.ctx, store.LearnProposalInput{Trigger: store.LearnTriggerRequest, BaseSHA: f.master,
		Branch: "learn/20261005-091500", CandidateSHA: strings.Repeat("4", 40), State: store.LearnHeld, Reverts: v.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.scan()
	if ps := f.proposals(); len(ps) != 1 || ps[0].ID != manual.ID {
		t.Fatalf("proposals after the scan = %+v", ps)
	}
	if got, _ := f.h.e.getKV(f.h.ctx, KVLearnRegression); got != strconv.FormatInt(v.ID, 10) {
		t.Fatalf("regression mark = %q", got)
	}
	if toasts := f.toasts(); strings.Contains(toasts, "regressed") {
		t.Fatalf("toasts:\n%s", toasts)
	}
}

// mergeBaseHook runs before ahead of each MergeBase, the first git call of
// revertPlan, which maybeRegression makes after its checks.
type mergeBaseHook struct {
	LearnGit
	before func()
}

func (g mergeBaseHook) MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	g.before()
	return g.LearnGit.MergeBase(ctx, dir, a, b)
}

// A regression's revert that does not start (a drain began between the
// scan's checks and the start) leaves the version unmarked, so the next
// scan proposes it; the toast comes with the revert that started.
func TestARegressionsRevertThatDidNotStartIsProposedAtTheNextScan(t *testing.T) {
	f := newEditFixture(t, goodTurn)
	f.noCases()
	v := f.regressed()
	drained := false
	f.h.e.d.LearnGit = mergeBaseHook{LearnGit: f.git, before: func() {
		if !drained {
			drained = true
			f.h.e.setKV(f.h.ctx, KVDaemonDraining, "1")
		}
	}}
	f.scan()
	if ps := f.proposals(); len(ps) != 0 {
		t.Fatalf("proposals during a drain = %+v", ps)
	}
	if got, _ := f.h.e.getKV(f.h.ctx, KVLearnRegression); got != "" {
		t.Fatalf("regression mark = %q, want none", got)
	}
	if toasts := f.toasts(); strings.Contains(toasts, "regressed") {
		t.Fatalf("toasts before the revert started:\n%s", toasts)
	}
	f.h.e.delKV(f.h.ctx, KVDaemonDraining)
	f.scan()
	if ps := f.proposals(); len(ps) != 1 || ps[0].Reverts != v.ID || ps[0].Trigger != store.LearnTriggerRegression {
		t.Fatalf("proposals after the drain = %+v", ps)
	}
	if got, _ := f.h.e.getKV(f.h.ctx, KVLearnRegression); got != strconv.FormatInt(v.ID, 10) {
		t.Fatalf("regression mark = %q", got)
	}
	if toasts := f.toasts(); !strings.Contains(toasts, "skill version "+strconv.FormatInt(v.ID, 10)+" regressed") {
		t.Fatalf("toasts:\n%s", toasts)
	}
}

// A revert never lands at a tick: the gate holds it whatever it found (a
// regression too, keeping its worktree for the review), and a passed one
// waits for the operator's y.
func TestARevertProposalIsNeverLandedAlone(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	f.h.e.supervised = true
	if _, err := f.h.st.DecideLearnProposal(f.h.ctx, f.p.ID, []string{store.LearnGating}, store.LearnFailed, "", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	v, err := f.h.st.CreateLearnVersion(f.h.ctx, store.LearnVersion{LandedSHA: learnMaster, SkillSHA: "s", PromptsSHA: "p"})
	if err != nil {
		t.Fatal(err)
	}
	revert := func(run, sha, state string) store.LearnProposal {
		t.Helper()
		wt := filepath.Join(f.h.layout.LearnWorktrees(), run)
		candidateFiles(t, wt)
		f.git.set(wt, "HEAD", sha)
		f.git.set(f.checkout, "refs/heads/learn/"+run, sha)
		f.git.mu.Lock()
		f.git.parent[sha] = learnMaster
		f.git.worktree[wt] = [2]string{f.checkout, "learn/" + run}
		f.git.mu.Unlock()
		f.ev.mu.Lock()
		f.ev.inputs[wt] = [2]string{"in-revert", "rev-revert"}
		f.ev.mu.Unlock()
		p, err := f.h.st.CreateLearnProposal(f.h.ctx, store.LearnProposalInput{Trigger: store.LearnTriggerRegression, BaseSHA: learnMaster,
			Branch: "learn/" + run, CandidateSHA: sha, Files: gateFullFiles, State: state, Scratch: wt, Reverts: v.ID,
			RunDir: filepath.Join(f.h.layout.LearnSkill(), run)})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	held := revert("20261005-091500", strings.Repeat("4", 40), store.LearnGating)
	f.ev.corpus = &eval.Corpus{Cases: []eval.Case{f.other}}
	f.baseline("eval-base", "", map[string]*eval.Score{f.other.Name: found([]string{"scope"})})
	f.ev.script(held.Scratch, f.other.Name, found(nil, "scope")) // lost, and lost again
	f.h.e.maybeGate(f.h.ctx)
	f.h.settle()
	p, err := f.h.st.LearnProposalByID(f.h.ctx, held.ID)
	if err != nil || p.State != store.LearnHeld || !strings.Contains(p.Reason, "revert") || !strings.Contains(p.Reason, "lost scope") {
		t.Fatalf("the gated revert = %+v, %v", p, err)
	}
	if _, err := os.Stat(held.Scratch); err != nil || len(f.git.called("worktree discard "+held.Scratch)) != 0 {
		t.Fatalf("the held revert lost its worktree: %v", err)
	}

	passed := revert("20261005-093000", strings.Repeat("5", 40), store.LearnPassed)
	f.h.e.maybeLearnLand(f.h.ctx)
	if got, err := f.h.st.LearnProposalByID(f.h.ctx, passed.ID); err != nil || got.State != store.LearnPassed || len(f.git.called("merge")) != 0 {
		t.Fatalf("a passed revert at a tick = %+v, %v (merges %q)", got, err, f.git.called("merge"))
	}
}
