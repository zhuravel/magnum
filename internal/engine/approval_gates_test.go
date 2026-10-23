package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// judgeApprovesHarness is a harness whose App judge approves a clean review
// (no_findings_event APPROVE), every head listing app/models/order.rb unless
// its spec lists its own files.
func judgeApprovesHarness(t *testing.T, mods ...func(*harness)) *harness {
	t.Helper()
	approves := func(h *harness) {
		h.cfg.IdentityByName("talkable-app").NoFindingsEvent = "APPROVE"
		h.gh.defaultFiles = []string{"app/models/order.rb"}
	}
	return newHarness(t, append([]func(*harness){approves}, mods...)...)
}

// asGhIdentity makes the talkable watch post as the gh identity zhuravel,
// whose no_findings_event is APPROVE.
func asGhIdentity(h *harness) { h.cfg.Watches[0].Identity = "zhuravel" }

// roundOf opens PR #2 as spec, after the first sync's baseline, and runs
// its first round, forced (magnum review) when force; it returns the
// round's input.
func roundOf(t *testing.T, h *harness, spec prSpec, force bool) pipeline.RoundInput {
	t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, spec)
	h.tick()
	if force {
		h.reviewAgain(2)
	} else {
		h.advance(5 * time.Minute)
		h.tick()
	}
	ins := h.rd.all()
	if len(ins) == 0 {
		t.Fatalf("no round of #2 ran (state %s)", h.pr(2).State)
	}
	return ins[len(ins)-1]
}

// A round whose judge would approve a clean review is held, at dispatch,
// to the gates of the approval as the operator that need no review (spec
// 09): a PR that edits .github/workflows/ci.yml or a local action, one
// that changes .claude/ or an AGENTS.md anywhere, a draft magnum reviews on
// request, one whose file list GitHub cut: its input says why in the judge
// prompt's fixed words (never a path), and one round.approval_refused
// event per head says it too, with nothing more for a second round of the
// head. A gh posting identity is held the same way. A clean PR that passes
// every gate carries no refusal, and every gated round asks the CI gate
// when its judge's prompt is written.
func TestTheJudgesApprovalIsHeldToTheGatesDecidedAtDispatch(t *testing.T) {
	const (
		workflows   = "the PR changes its own workflows or actions"
		agentConfig = "the PR changes the review agents' instructions or hooks"
	)
	for _, tc := range []struct {
		name  string
		spec  prSpec
		gh    bool
		force bool
		want  string
	}{
		{name: "the CI workflow", spec: prSpec{files: []string{"app/models/order.rb", ".github/workflows/ci.yml"}}, want: workflows},
		{name: "a local action", spec: prSpec{files: []string{".github/actions/setup/action.yml"}}, want: workflows},
		{name: "the CI workflow as a gh identity", spec: prSpec{files: []string{".github/workflows/ci.yml"}}, gh: true, want: workflows},
		{name: ".claude/", spec: prSpec{files: []string{".claude/settings.json"}}, want: agentConfig},
		{name: "an AGENTS.md", spec: prSpec{files: []string{"app/AGENTS.md"}}, want: agentConfig},
		{name: "a draft", spec: prSpec{draft: true}, force: true, want: "the PR is a draft"},
		{name: "a cut file list", spec: prSpec{filesTruncated: true}, want: "magnum cannot tell which files the PR changes"},
		{name: "a clean PR"},
		{name: "a clean PR as a gh identity", gh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mods []func(*harness)
			if tc.gh {
				mods = append(mods, asGhIdentity)
			}
			h := judgeApprovesHarness(t, mods...)
			spec := tc.spec
			spec.n, spec.head = 2, "b1"
			in := roundOf(t, h, spec, tc.force)
			if in.NoApproval != tc.want || in.CIGate == nil {
				t.Fatalf("round input: no approval %q (CI gate %v), want %q and the CI gate", in.NoApproval, in.CIGate != nil, tc.want)
			}
			refused := approvalEvents(t, h, 2, "round.approval_refused")
			if tc.want == "" {
				if len(refused) != 0 {
					t.Fatalf("round.approval_refused events %+v for a PR every gate lets through", refused)
				}
				return
			}
			if len(refused) != 1 || refused[0].Message != "the judge does not approve talkable/talkable#2: "+tc.want+"; a clean verdict posts as a comment" {
				t.Fatalf("round.approval_refused events: %+v", refused)
			}
			if d := eventData(t, refused[0]); d["reason"] != tc.want || d["head_sha"] != "b1" || d["ci"] != nil {
				t.Fatalf("event data %s", refused[0].Data)
			}
			start := approvalEvents(t, h, 2, "engine.round_start")
			if d := eventData(t, start[len(start)-1]); d["no_approval"] != true {
				t.Fatalf("round start %s: want no_approval", start[len(start)-1].Data)
			}
			h.advance(time.Hour)
			h.reviewAgain(2)
			if again := h.rd.all(); again[len(again)-1].NoApproval != tc.want || len(approvalEvents(t, h, 2, "round.approval_refused")) != 1 {
				t.Fatalf("the second round of b1: no approval %q, %d events: want the same refusal, said once",
					again[len(again)-1].NoApproval, len(approvalEvents(t, h, 2, "round.approval_refused")))
			}
		})
	}
}

// Rounds whose judge comments anyway are held to no gate: a repository
// whose judge comments on a clean review, a dry run, the operator's own
// PR (self-authored). Their input carries no refusal and no CI gate, and
// no round.approval_refused event is written.
func TestARoundThatCannotApproveAnywayIsHeldToNoGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*harness)
		spec prSpec
	}{
		{"a commenting identity", func(h *harness) { h.cfg.IdentityByName("talkable-app").NoFindingsEvent = "COMMENT" }, prSpec{}},
		{"the operator's own PR", func(*harness) {}, prSpec{author: "zhuravel"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := judgeApprovesHarness(t, tc.mod, func(h *harness) { h.cfg.Watches[0].IncludeOwn = new(true) })
			spec := tc.spec
			spec.n, spec.head, spec.files = 2, "b1", []string{".github/workflows/ci.yml"}
			in := roundOf(t, h, spec, false)
			if in.NoApproval != "" || in.CIGate != nil || len(approvalEvents(t, h, 2, "round.approval_refused")) != 0 {
				t.Fatalf("no approval %q, CI gate %v, events %+v: want none", in.NoApproval, in.CIGate != nil, approvalEvents(t, h, 2, "round.approval_refused"))
			}
		})
	}
	h := judgeApprovesHarness(t, func(h *harness) { h.gh.defaultFiles = []string{".github/workflows/ci.yml"} })
	pr := h.reviewedPR(2, "b1")
	job := &roundJob{pr: pr, repo: store.Repo{Owner: "talkable", Name: "talkable"}, kind: pipeline.KindInitial}
	rs := &roundSetup{kind: pipeline.KindInitial, target: "b1"}
	h.e.planApprovalGates(h.ctx, job, rs, true)
	if job.noApproval != "" || job.ciGated {
		t.Fatalf("a dry run: no approval %q, CI gate %v", job.noApproval, job.ciGated)
	}
}

const judgeWorkflowsWords = "the PR changes its own workflows or actions"

// A continue decides the dispatch gates again, as every other round does:
// they are functions of the registry (the PR's draft and muted marks, its
// watch, its files), so no record of the paused round's answer is kept. The
// same PR gives the same answer, and a PR that changed meanwhile the
// current one; the CI gate is asked again at its prompt as before.
func TestAContinueDecidesTheDispatchGatesAgain(t *testing.T) {
	h := judgeApprovesHarness(t, func(h *harness) { h.gh.defaultFiles = []string{".github/workflows/ci.yml"} })
	pr := h.reviewedPR(2, "b1")
	repo := store.Repo{Owner: "talkable", Name: "talkable"}
	plan := func(pr store.PR) *roundJob {
		job := &roundJob{pr: pr, repo: repo, kind: kindContinue}
		h.e.planApprovalGates(h.ctx, job, &roundSetup{kind: kindContinue, target: "b1"}, false)
		return job
	}
	if job := plan(pr); job.noApproval != judgeWorkflowsWords || !job.ciGated {
		t.Fatalf("the continue of a workflow-changing PR: no approval %q, CI gate %v: want the refusal and the CI gate", job.noApproval, job.ciGated)
	}
	pr.IsDraft = true // it turned into a draft while the round was paused
	if job := plan(pr); job.noApproval != "the PR is a draft" {
		t.Fatalf("the continue of a PR that became a draft: no approval %q", job.noApproval)
	}
	pr.IsDraft, pr.Muted = false, true
	if job := plan(pr); job.noApproval != "the PR is muted" {
		t.Fatalf("the continue of a PR that was muted: no approval %q", job.noApproval)
	}
	if v, ok := kvValue(h, fmt.Sprintf("pr.%d.no_approval_round", pr.ID)); ok {
		t.Fatalf("a record of the paused round's answer is kept again: %q", v)
	}
}

// A round that began as a continue and became a fresh one (its judge's
// session was gone) says why it cannot approve, like any fresh round: the
// event is written once per head and reason, so a continue that stays a
// continue repeats nothing.
func TestAContinueThatBecameAFreshRoundSaysWhyItCannotApprove(t *testing.T) {
	h := judgeApprovesHarness(t, func(h *harness) { h.gh.defaultFiles = []string{".github/workflows/ci.yml"} })
	pr := h.reviewedPR(2, "b1")
	repo := store.Repo{Owner: "talkable", Name: "talkable"}
	// The first round's event stands for b1; a new head has none yet.
	if evs := approvalEvents(t, h, 2, "round.approval_refused"); len(evs) != 1 {
		t.Fatalf("round.approval_refused events after the first round: %+v", evs)
	}
	job := &roundJob{pr: pr, repo: repo, kind: kindContinue}
	h.e.planApprovalGates(h.ctx, job, &roundSetup{kind: kindContinue, target: "b1"}, false) // a continue that stays one
	if evs := approvalEvents(t, h, 2, "round.approval_refused"); len(evs) != 1 {
		t.Fatalf("a continue of the same head repeated the event: %+v", evs)
	}
	job = &roundJob{pr: pr, repo: repo, kind: kindContinue}
	h.e.planApprovalGates(h.ctx, job, &roundSetup{kind: pipeline.KindRecovery, target: "b2"}, false) // a continue that became a fresh round
	if job.noApproval != judgeWorkflowsWords {
		t.Fatalf("no approval %q", job.noApproval)
	}
	evs := approvalEvents(t, h, 2, "round.approval_refused")
	if len(evs) != 2 || eventData(t, evs[1])["head_sha"] != "b2" || eventData(t, evs[1])["reason"] != judgeWorkflowsWords {
		t.Fatalf("round.approval_refused events: %+v, want a second one for b2", evs)
	}
}

// A round stopped during its judge's turn continues after the restart with
// the refusal its dispatch gave, without a record of it: the same PR gives
// the same answer, and the head's event is not written twice.
func TestAPausedRoundsContinueKeepsItsRefusalWithoutARecord(t *testing.T) {
	h := judgeApprovesHarness(t, func(h *harness) { h.gh.defaultFiles = []string{".github/workflows/ci.yml"} })
	h.reviewedPR(2, "b1")
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		round, err := nextRound(h, in.PR.ID)
		if err == nil {
			err = sentJudge(h, in, round)
		}
		if err == nil {
			err = shutDown(h, in.PR.ID)
		}
		if err != nil {
			return failRound(t, err)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeStopped, Round: round, Error: "pipeline: round cancelled: context canceled"}, nil
	}
	pollPR(h, 40*time.Minute, 2, "b2")
	h.advance(5 * time.Minute)
	h.tick()
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: in.Round, ReviewID: 202, Event: "COMMENTED", ReviewCommit: "b2"}, nil
	}
	h.e = New(h.d)
	h.startup()
	h.wantState(2, store.PRPaused)
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	ins := h.rd.all()
	in := ins[len(ins)-1]
	if in.Kind != pipeline.KindContinue || in.NoApproval != judgeWorkflowsWords || in.CIGate == nil {
		t.Fatalf("the continue: kind %s, no approval %q, CI gate %v", in.Kind, in.NoApproval, in.CIGate != nil)
	}
	var heads []any
	for _, ev := range approvalEvents(t, h, 2, "round.approval_refused") {
		heads = append(heads, eventData(t, ev)["head_sha"])
	}
	if !slices.Equal(heads, []any{"b1", "b2"}) {
		t.Fatalf("round.approval_refused events for heads %v, want one each for b1 and b2", heads)
	}
}

// The CI gate, asked when the judge's prompt is written: red CI refuses
// (one round.approval_refused event per head with the CI state red, the
// judge prompt's words), red CI whose every failing check fails the same
// way on the base branch's tip lets the APPROVE through without
// review.auto_approve_ci_base ("the approval goes ahead" is the approval
// as the operator's to say), and so do checks that still run, checks that
// passed and a registry that holds a newer head's checks than the round's
// target, each with the CI state it decided by.
func TestTheJudgesCIGateRefusesOnlyRedCIUnlikeTheBase(t *testing.T) {
	failed, passed := github.CheckFailed, github.CheckPassed
	audit := github.Check{Name: "Dependency Audit", State: failed, Workflow: "Audit", WorkflowFile: "audit.yml", At: ciBefore}
	rspec := github.Check{Name: "rspec", State: failed, Workflow: "CI", WorkflowFile: "ci.yml", At: ciBefore}
	green := github.Check{Name: "rspec", State: passed, Workflow: "CI", WorkflowFile: "ci.yml", At: ciBefore}
	running := github.Check{Name: "rspec", State: github.CheckPending, Workflow: "CI", WorkflowFile: "ci.yml"}
	for _, tc := range []struct {
		name, ci string
		checks   []github.Check
		target   string
		refusal  string
		state    string
	}{
		{"red unlike the base", "FAILURE", []github.Check{rspec, audit}, "b1", ciFailsJudge, pipeline.CIRed},
		{"red like the base", "FAILURE", []github.Check{audit, green}, "b1", "", pipeline.CIRedLikeBase},
		{"running", "PENDING", []github.Check{running}, "b1", "", pipeline.CIRunning},
		{"green", "SUCCESS", []github.Check{green}, "b1", "", pipeline.CIGreen},
		{"a newer head's checks", "FAILURE", []github.Check{rspec}, "b0", "", pipeline.CIUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := judgeApprovesHarness(t, withBaseChecks(audit, green))
			in := roundOf(t, h, prSpec{n: 2, head: "b1", ci: tc.ci, checks: tc.checks}, false)
			for range 2 {
				g := in.CIGate(h.ctx, h.pr(2), tc.target)
				if g.Refusal != tc.refusal || g.State != tc.state {
					t.Fatalf("CI gate = %+v, want refusal %q, state %s", g, tc.refusal, tc.state)
				}
			}
			refused := approvalEvents(t, h, 2, "round.approval_refused")
			if tc.refusal == "" {
				if len(refused) != 0 {
					t.Fatalf("round.approval_refused events %+v", refused)
				}
			} else if len(refused) != 1 || eventData(t, refused[0])["ci"] != pipeline.CIRed || eventData(t, refused[0])["reason"] != ciFailsJudge {
				t.Fatalf("round.approval_refused events %+v: want one, CI red", refused)
			}
			if evs := h.events(ciBaseEvent); len(evs) != 0 {
				t.Fatalf("%s events %+v: the judge's gate records none", ciBaseEvent, evs)
			}
		})
	}
}

// The draft and muted gates come from one function for the approval as the
// operator (the card's words) and for the judge's approval (the prompt's
// fixed words): neither side switches on the other's text, so rewording one
// skips no gate.
func TestAPRsStateRefusalGivesTheCardsWordsAndThePromptsWords(t *testing.T) {
	h := judgeApprovesHarness(t)
	pr := h.reviewedPR(2, "b1")
	for _, tc := range []struct {
		name         string
		draft, muted bool
		why, short   string
	}{
		{"a draft", true, false, "it is a draft", "the PR is a draft"},
		{"a muted PR", false, true, "it is muted", "the PR is muted"},
		{"a muted draft", true, true, "it is a draft", "the PR is a draft"},
		{"neither", false, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pr
			p.IsDraft, p.Muted = tc.draft, tc.muted
			if why, short := prStateRefusal(p); why != tc.why || short != tc.short {
				t.Fatalf("prStateRefusal = %q, %q; want %q, %q", why, short, tc.why, tc.short)
			}
			job := &roundJob{pr: p, repo: store.Repo{Owner: "talkable", Name: "talkable"}, kind: pipeline.KindInitial}
			if got := h.e.approvalGatesDue(h.ctx, job, &roundSetup{kind: pipeline.KindInitial, target: "b1"}, false); got != tc.short {
				t.Fatalf("the judge's gate = %q, want %q", got, tc.short)
			}
			if got := autoApproveRefusal(autoFacts{PR: p, Login: "zhuravel"}); tc.why != "" && got != tc.why {
				t.Fatalf("the approval as the operator refuses with %q, want %q", got, tc.why)
			}
		})
	}
}

// A rollup the registry never read (a NULL ci_state: the poll's CI read
// failed, or the PR was never fetched in full) is not green: both gates say
// unknown, which still lets the approval through, as a green rollup does,
// and the judge's round.judge event can tell "never read" from "passed". A
// rollup of no checks (read, and empty) is green.
func TestARollupNeverReadIsUnknownAndStillLetsTheApprovalThrough(t *testing.T) {
	green := github.Check{Name: "rspec", State: github.CheckPassed, Workflow: "CI", WorkflowFile: "ci.yml", At: ciBefore}
	for _, tc := range []struct {
		name      string
		ci        string
		checks    []github.Check
		neverRead bool
		state     string
	}{
		{"never read", "SUCCESS", []github.Check{green}, true, pipeline.CIUnknown},
		{"read and green", "SUCCESS", []github.Check{green}, false, pipeline.CIGreen},
		{"read and empty", "", nil, false, pipeline.CIGreen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := judgeApprovesHarness(t)
			in := roundOf(t, h, prSpec{n: 2, head: "b1", ci: tc.ci, checks: tc.checks}, false)
			if tc.neverRead {
				if _, err := h.st.DB().ExecContext(h.ctx, "UPDATE prs SET ci_state = NULL WHERE id = ?", h.pr(2).ID); err != nil {
					t.Fatal(err)
				}
			}
			pr := h.pr(2)
			if (pr.CIState == nil) != tc.neverRead {
				t.Fatalf("test setup: ci_state %v", pr.CIState)
			}
			if a := h.e.ciGateOf(h.ctx, store.RepoPR{PR: pr, Repo: "talkable/talkable"}); a.why != "" || a.waits || a.state != tc.state {
				t.Fatalf("ciGateOf = %+v, want %s and no refusal", a, tc.state)
			}
			if g := in.CIGate(h.ctx, pr, "b1"); g.Refusal != "" || g.State != tc.state {
				t.Fatalf("judge's CI gate = %+v, want %s and no refusal", g, tc.state)
			}
		})
	}
}

// The judge's CI gate never waits behind auto-approval, whose writes hold
// autoMu up to gh's timeout: with autoMu held it decides, a read of the
// base tip's checks included (the base-checks cache and the GitHub budget
// have locks of their own), and the race detector sees no unguarded state
// shared with the pass.
func TestTheJudgesCIGateNeverWaitsBehindAnApprovalWrite(t *testing.T) {
	audit := github.Check{Name: "Dependency Audit", State: github.CheckFailed, Workflow: "Audit", WorkflowFile: "audit.yml", At: ciBefore}
	h := judgeApprovesHarness(t, withBaseChecks(audit))
	in := roundOf(t, h, prSpec{n: 2, head: "b1", ci: "FAILURE", checks: []github.Check{audit}}, false)
	pr := h.pr(2)
	h.e.autoMu.Lock()
	done := make(chan pipeline.CIGateResult, 1)
	go func() { done <- in.CIGate(h.ctx, pr, "b1") }()
	select {
	case g := <-done:
		h.e.autoMu.Unlock()
		if g.State != pipeline.CIRedLikeBase || g.Refusal != "" {
			t.Fatalf("CI gate = %+v, want the base tip's failure let through", g)
		}
	case <-time.After(10 * time.Second):
		h.e.autoMu.Unlock()
		t.Fatal("the judge's CI gate waited for autoMu")
	}
	// The pass's budget reads and base tip reads (each of a tip not read
	// yet) beside the judge's gate, on goroutines of their own.
	c := store.RepoPR{PR: pr, Repo: "talkable/talkable"}
	var wg sync.WaitGroup
	for g := range 3 {
		wg.Go(func() {
			for i := range 20 {
				h.e.autoReadsPass(h.ctx)
				h.e.baseTipChecks(h.ctx, c, fmt.Sprintf("%d%039d", g, i))
				in.CIGate(h.ctx, pr, "b1")
			}
		})
	}
	wg.Wait()
}

// The gate's answer recorded nowhere but its own event: the CI gate and
// the approval as the operator decide by one function (ciGateOf), and only
// the approval as the operator records review.auto_approve_ci_base when it
// goes ahead on checks the base tip fails too.
func TestTheCIGateDecisionIsSplitFromItsRecord(t *testing.T) {
	audit := github.Check{Name: "Dependency Audit", State: github.CheckFailed, Workflow: "Audit", WorkflowFile: "audit.yml", At: ciBefore}
	h, _ := newAutoHarness(t, withBaseChecks(audit))
	reviewedWithChecks(h, "FAILURE", audit)
	pr := h.pr(2)
	c := store.RepoPR{PR: pr, Repo: "talkable/talkable"}
	if a := h.e.ciGateOf(h.ctx, c); a.why != "" || a.state != pipeline.CIRedLikeBase || a.failing != 1 || a.base != fakeBaseOid {
		t.Fatalf("ciGateOf = %+v", a)
	}
	if evs := h.events(ciBaseEvent); len(evs) != 1 || !strings.Contains(evs[0].Message, "the approval goes ahead") {
		t.Fatalf("%s events %+v: want the approval as the operator's one", ciBaseEvent, evs)
	}
}

// cleanAsThePipeline makes every round post a clean review as the pipeline
// does: an approval, or, when the approval's gates refused it
// (RoundInput.NoApproval), a comment that says so in its result
// (RoundResult.NoApproval).
func cleanAsThePipeline(h *harness) {
	h.rd.mu.Lock()
	defer h.rd.mu.Unlock()
	h.rd.judgeLogin = "talkable[bot]" // posted records the judge's run
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		res, err := h.rd.posted(h.ctx, in, len(h.rd.all()))
		if err != nil {
			return res, err
		}
		res.Event, res.Findings, res.NoApproval = "APPROVED", map[string]int{}, ""
		if in.NoApproval != "" {
			res.Event, res.NoApproval = "COMMENTED", in.NoApproval
		}
		result, err := json.Marshal(map[string]any{"event": res.Event, "verdict": store.VerdictClean, "findings": res.Findings})
		if err == nil {
			err = h.st.UpdateRun(h.ctx, res.JudgeRunID, func(u *store.RunUpdate) {
				u.Set("outcome", pipeline.OutcomePosted)
				u.Set("review_event", res.Event)
				u.Set("result_json", string(result))
			})
		}
		if err != nil {
			return failRound(h.t, err)
		}
		return res, nil
	}
}

// A PR that changes its own workflows, reviewed clean where the App's
// judge approves a clean review, gets a comment instead (the approval's
// gates), yet still needs the operator: magnum found nothing to fix and a
// person must approve by hand, which is what the needs-you list is for. It
// is listed with the gates' reason, its toast says "approve by hand: the PR
// changes its own workflows or actions", and a clean PR beside it is
// listed as magnum approved it. A later review the gates let through drops
// the record.
func TestAWorkflowChangingPRReviewedCleanNeedsYouWithTheGatesReason(t *testing.T) {
	const why = "the PR changes its own workflows or actions"
	h := judgeApprovesHarness(t, noEveryReview)
	cleanAsThePipeline(h)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", gate: gateRequired, files: []string{".github/workflows/ci.yml"}},
		prSpec{n: 3, head: "c1", gate: gateRequired})
	h.tick()
	for i := 0; i < 20 && (h.pr(2).State != store.PRReviewed || h.pr(3).State != store.PRReviewed); i++ {
		h.advance(5 * time.Minute)
		h.tick() // one round at a time (one slot)
	}
	if pr := h.wantState(2, store.PRReviewed); deref(pr.LastReviewEvent) != "COMMENTED" {
		t.Fatalf("#2 posted %q, want the comment the gates asked for", deref(pr.LastReviewEvent))
	}
	h.flush() // the tick that saw the last review queued its toast
	h.flush()
	list, err := h.st.NeedsMePRs(h.ctx, h.cfg.CommentsWhenClean, h.cfg.SelfMatch())
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for _, n := range list {
		got[n.PR.Number] = n.NeedsMe + "|" + n.Refused
	}
	if got[2] != store.NeedsMeApprove+"|"+why || got[3] != store.NeedsMeApprove+"|" {
		t.Fatalf("needs you = %v: want #2 with the gates' reason and #3 as approved", got)
	}
	for _, want := range []string{
		"magnum: talkable#2 needs your approval | https://github.com/talkable/talkable/pull/2\nmagnum found nothing to fix in b1; approve by hand: " + why + ".",
		"magnum: talkable#3 needs your approval | https://github.com/talkable/talkable/pull/3\nmagnum approved c1;",
	} {
		if len(toastsWith(h, want)) != 1 {
			t.Errorf("toasts lack %q: %q", want, h.nh.shown())
		}
	}
	pr := h.pr(2)
	if v, _ := kvValue(h, store.KVPRNoApprovalReview(pr.ID)); v != (store.NoApprovalReview{ReviewID: deref(pr.LastReviewID), Reason: why}).Value() {
		t.Fatalf("%s = %q", store.KVPRNoApprovalReview(pr.ID), v)
	}
	h.e.settleNoApprovalReview(h.ctx, pr, pipeline.RoundResult{ReviewID: deref(pr.LastReviewID) + 1})
	if v, ok := kvValue(h, store.KVPRNoApprovalReview(pr.ID)); ok {
		t.Fatalf("a later review the gates let through left the record %q", v)
	}
}
