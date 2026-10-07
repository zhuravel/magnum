package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// ownInput is a round whose judge does its own pass while the reviewers
// work ([pipeline] judge_own_pass = "parallel").
func (e *env) ownInput(kind string) RoundInput {
	in := e.input(kind)
	in.OwnPass = true
	return in
}

// writeOwn is the judge's own pass: it writes judge-own.md (the run's
// report path) and ends its turn.
func writeOwn() behavior {
	return writeReport("## Own pass\n- [P2] app/models/order.rb:42 retries twice\n")
}

// afterRuns waits until each role's run of kind on head reaches state, then
// behaves as next (another role's turn ends meanwhile in its goroutine).
func afterRuns(e *env, head, state string, roles []agents.Role, next behavior) behavior {
	return func(f *fakeAgents, run store.Run, text string) error {
		for _, r := range roles {
			e.waitRun(r, head, state)
		}
		return next(f, run, text)
	}
}

// judgeSees records, when the judge's candidates prompt arrives, the state of
// every other run of the round, then behaves as next.
type judgeSees struct {
	mu     sync.Mutex
	states map[string]string // "<role>/<kind>" -> state at the candidates prompt
}

func (j *judgeSees) then(e *env, next behavior) behavior {
	return func(f *fakeAgents, run store.Run, text string) error {
		j.mu.Lock()
		j.states = map[string]string{}
		for _, r := range e.runs() {
			if r.ID != run.ID {
				j.states[r.Role+"/"+r.Kind] = r.State
			}
		}
		j.mu.Unlock()
		return next(f, run, text)
	}
}

func (j *judgeSees) want(t *testing.T, states map[string]string) {
	t.Helper()
	j.mu.Lock()
	defer j.mu.Unlock()
	for k, want := range states {
		if got := j.states[k]; got != want {
			t.Errorf("at the candidates prompt %s was %q, want %q (all: %v)", k, got, want, j.states)
		}
	}
}

// The judge is prompted for its own pass together with the reviewers (a
// reviewer still works when it ends), and for the candidates once every
// reviewer and the own pass ended: two prompts, the second naming its own
// pass, the review carrying the candidates run's marker, which the own-pass
// prompt quoted already.
func TestJudgeOwnPassRunsWithTheReviewers(t *testing.T) {
	e := newEnv(t)
	// claude-review ends only after the judge's own pass ended: the judge
	// worked during the reviewers' stage.
	e.ag.behaviors[agents.RoleClaude] = []behavior{afterRuns(e, target, store.RunVerified, []agents.Role{agents.RoleJudge}, writeReport("## P2 claude\n"))}
	var sees judgeSees
	e.ag.behaviors[agents.RoleJudge] = []behavior{writeOwn(), sees.then(e, e.judgePosts(801, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t))}

	res, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.ReviewID != 801 {
		t.Fatalf("result = %+v", res)
	}
	sees.want(t, map[string]string{"claude-review/initial": store.RunVerified, "codex-review/initial": store.RunVerified,
		"codex-judge/own_pass": store.RunVerified})

	dir := e.reportDir()
	own := e.runOf(agents.RoleJudge, store.RunOwnPass)
	judgeRun := e.runOf(agents.RoleJudge, store.RunInitial)
	wantRun(t, own, store.RunVerified, ReportOK)
	if store.Deref(own.ReportPath) != filepath.Join(dir, agents.OwnFindingsFile) || own.Round != judgeRun.Round || own.TargetSHA != target {
		t.Errorf("own-pass run = %+v", own)
	}
	if res.JudgeRunID != judgeRun.ID || res.OwnPass == nil || res.OwnPass.Status != ReportOK || res.OwnPass.RunID != own.ID {
		t.Errorf("JudgeRunID %q (want %s), OwnPass %+v", res.JudgeRunID, judgeRun.ID, res.OwnPass)
	}
	submits := e.ag.submitsFor(agents.RoleJudge)
	if len(submits) != 2 {
		t.Fatalf("judge submits = %d, want the own pass and the candidates", len(submits))
	}
	if submits[0].Run.ID != own.ID || submits[1].Run.ID != judgeRun.ID {
		t.Errorf("judge prompts went to runs %s, %s; want %s then %s", submits[0].Run.ID, submits[1].Run.ID, own.ID, judgeRun.ID)
	}
	mustContain(t, "own-pass prompt", submits[0].Text, "mode: initial", "phase: own_pass", "run_id: "+judgeRun.ID,
		"own_findings: "+filepath.Join(dir, agents.OwnFindingsFile), "head_sha: "+target, "post nothing")
	for _, leak := range []string{"reports:", "result_file", "post_review", "claude-review.md"} {
		if strings.Contains(submits[0].Text, leak) {
			t.Errorf("the own-pass prompt carries %q:\n%s", leak, submits[0].Text)
		}
	}
	mustContain(t, "candidates prompt", submits[1].Text, "mode: initial", "phase: candidates", "run_id: "+judgeRun.ID,
		"own_findings: "+filepath.Join(dir, agents.OwnFindingsFile), "Your own pass is in "+filepath.Join(dir, agents.OwnFindingsFile),
		"claude-review: "+filepath.Join(dir, "claude-review.md"), "result_file: "+filepath.Join(dir, "codex-judge.json"))
	if len(e.eventsOf("round.own_pass")) < 2 {
		t.Errorf("round.own_pass events = %v, want its prompt and its end", e.eventsOf("round.own_pass"))
	}
}

// Whichever ends last starts the candidates phase: here the own pass, which
// ends only after both reviewers.
func TestCandidatesWaitForTheOwnPass(t *testing.T) {
	e := newEnv(t)
	var sees judgeSees
	e.ag.behaviors[agents.RoleJudge] = []behavior{
		afterRuns(e, target, store.RunVerified, []agents.Role{agents.RoleClaude, agents.RoleCodexReview}, writeOwn()),
		sees.then(e, e.judgePosts(802, "COMMENTED", "COMMENT").behavior(t)),
	}
	res, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	sees.want(t, map[string]string{"claude-review/initial": store.RunVerified, "codex-review/initial": store.RunVerified,
		"codex-judge/own_pass": store.RunVerified})
	if n := len(e.ag.submitsFor(agents.RoleJudge)); n != 2 {
		t.Errorf("judge submits = %d, want 2", n)
	}
}

// A round whose judge is alone prompts it once, as before: no own pass for
// a round of the judge alone (every reviewer triaged out, or `--role`
// naming only the judge), a delta check, a re-review of the same head, or a
// continued turn.
func TestLoneJudgeGetsOnePrompt(t *testing.T) {
	cases := map[string]func(e *env) RoundInput{
		"judge alone": func(e *env) RoundInput {
			in := e.ownInput(KindInitial)
			in.Roles = []config.Role{e.judgeRole()}
			return in
		},
		"delta check": func(e *env) RoundInput {
			in := e.ownInput(KindRereview)
			in.Previous = &PreviousReview{ID: 77, Event: "COMMENTED", SHA: head2}
			in.Roles, in.DeltaCheck = []config.Role{e.judgeRole()}, &DeltaCheck{Lines: 4}
			return in
		},
		"same head": func(e *env) RoundInput {
			in := e.ownInput(KindRereview)
			in.Previous = &PreviousReview{ID: 77, Event: "COMMENTED", SHA: target}
			in.Roles, in.SameHead = []config.Role{e.judgeRole()}, true
			return in
		},
		"every reviewer logged out": func(e *env) RoundInput {
			e.ag.preflight["claude"] = agents.ErrLoginRequired
			in := e.ownInput(KindInitial)
			claude, _ := e.cfg.RoleByNameOrAlias(nil, string(agents.RoleClaude))
			in.Roles = []config.Role{e.judgeRole(), claude}
			return in
		},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(803, "COMMENTED", "COMMENT").behavior(t)}
			res, err := e.r.RunRound(e.ctx, input(e))
			if err != nil || res.Outcome != OutcomePosted {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			submits := e.ag.submitsFor(agents.RoleJudge)
			if len(submits) != 1 || strings.Contains(submits[0].Text, "phase:") || strings.Contains(submits[0].Text, "own_findings") {
				t.Fatalf("judge submits = %d:\n%v", len(submits), submits)
			}
			if runs := e.runsOf(agents.RoleJudge, store.RunOwnPass); len(runs) != 0 || res.OwnPass != nil {
				t.Errorf("own-pass runs = %+v, OwnPass %+v", runs, res.OwnPass)
			}
		})
	}
}

// judge_own_pass = "after" (RoundInput.OwnPass false) keeps today's flow:
// the reviewers, then one judge prompt that names no phase.
func TestJudgeOwnPassAfterKeepsOneJudgePrompt(t *testing.T) {
	e := newEnv(t)
	var sees judgeSees
	e.ag.behaviors[agents.RoleJudge] = []behavior{sees.then(e, e.judgePosts(804, "COMMENTED", "COMMENT").behavior(t))}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted || res.OwnPass != nil {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	sees.want(t, map[string]string{"claude-review/initial": store.RunVerified, "codex-review/initial": store.RunVerified})
	submits := e.ag.submitsFor(agents.RoleJudge)
	if len(submits) != 1 || strings.Contains(submits[0].Text, "phase:") || strings.Contains(submits[0].Text, "Your own pass") {
		t.Fatalf("judge submits = %d", len(submits))
	}
	if runs := e.runsOf(agents.RoleJudge, store.RunOwnPass); len(runs) != 0 {
		t.Errorf("own-pass runs = %+v", runs)
	}
}

// An own pass that leaves no file is no failure: the candidates prompt says
// so and asks for the pass then.
func TestMissingOwnFindingsAsksForThePassInTheCandidatesPhase(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{endSilently(), e.judgePosts(805, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	wantRun(t, e.runOf(agents.RoleJudge, store.RunOwnPass), store.RunFailed, ReportMissing)
	submits := e.ag.submitsFor(agents.RoleJudge)
	if len(submits) != 2 {
		t.Fatalf("judge submits = %d", len(submits))
	}
	mustContain(t, "candidates prompt", submits[1].Text, "phase: candidates",
		"Your own pass left no "+filepath.Join(e.reportDir(), agents.OwnFindingsFile)+": run your own full pass now")
}

// A re-review's own pass carries the re-review context (previous review,
// threads) and the candidates phase the usual re-review prompt.
func TestOwnPassOfARereview(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{writeOwn(), e.judgePosts(806, "COMMENTED", "COMMENT").behavior(t)}
	in := e.ownInput(KindRereview)
	in.Previous = &PreviousReview{ID: 3012345678, Event: "CHANGES_REQUESTED", SHA: head2}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	submits := e.ag.submitsFor(agents.RoleJudge)
	if len(submits) != 2 || e.runOf(agents.RoleJudge, store.RunOwnPass).ID != submits[0].Run.ID {
		t.Fatalf("judge submits = %d", len(submits))
	}
	mustContain(t, "own-pass prompt", submits[0].Text, "mode: rereview", "phase: own_pass", "previous_head_sha: "+head2,
		"Decide the reply contract")
	mustContain(t, "candidates prompt", submits[1].Text, "mode: rereview", "phase: candidates", "previous_head_sha: "+head2)
}

// A judge that started fresh only because its prompt cache was cold is
// prompted at its rereview effort, and its own pass reads the commits since
// the earlier reviews (cold full re-reviews ran the judge at xhigh, 43k
// output tokens each, against high when they resumed it); a judge whose
// session was lost re-reads the whole PR at its full effort.
func TestAColdJudgesOwnPassReviewsTheNewCommitsAtTheRereviewEffort(t *testing.T) {
	for _, tc := range []struct {
		name string
		cold bool
	}{{"cold cache", true}, {"lost session", false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.ag.behaviors[agents.RoleJudge] = []behavior{writeOwn(), e.judgePosts(807, "COMMENTED", "COMMENT").behavior(t)}
			in := e.ownInput(KindRecovery)
			in.ColdJudge = tc.cold
			in.Previous = &PreviousReview{ID: 3012345678, Event: "CHANGES_REQUESTED", SHA: prevSHA}
			res, err := e.r.RunRound(e.ctx, in)
			if err != nil || res.Outcome != OutcomePosted {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			own := e.ag.submitsFor(agents.RoleJudge)[0].Text
			commits := "git log --oneline " + prevSHA + ".." + target
			effort := "Work at high reasoning effort"
			if tc.cold {
				mustContain(t, "cold own-pass prompt", own, "mode: recovery", commits, effort)
				return
			}
			if strings.Contains(own, commits) || strings.Contains(own, effort) {
				t.Errorf("a lost session's own pass is scoped to the new commits or at the rereview effort:\n%s", own)
			}
		})
	}
}

// goneJudge loses the judge's session before the round, as when its start
// adopted an agent that was quitting (a live case: the own pass got
// agent_not_found 0.5 s after the start, and the round failed 9 minutes
// later, after claude-review, with the candidates prompt refused). restart
// is RoundInput.RestartJudge: it counts its calls and gives the judge a new
// live session when live is set.
func goneJudge(t *testing.T, e *env, live bool, err error) (in RoundInput, restarts *int) {
	t.Helper()
	if terr := e.st.TransitionSession(e.ctx, e.sess[agents.RoleJudge].ID, nil, store.SessionLost, nil); terr != nil {
		t.Fatal(terr)
	}
	in = e.ownInput(KindRecovery)
	in.Previous = &PreviousReview{ID: 3012345678, Event: "CHANGES_REQUESTED", SHA: prevSHA}
	restarts = new(int)
	in.RestartJudge = func(ctx context.Context) error {
		*restarts++
		if live {
			e.addSession(e.r.Config.JudgeFor(nil))
		}
		return err
	}
	return in, restarts
}

// The own pass refused because the judge's session is gone starts the judge
// once more and sends the pass again in a new own_pass run; the round goes
// on as usual.
func TestAnOwnPassRefusedWithNoSessionStartsTheJudgeOnceMore(t *testing.T) {
	e := newEnv(t)
	// The refused prompt takes the first behavior.
	e.ag.behaviors[agents.RoleJudge] = []behavior{hang(), writeOwn(), e.judgePosts(808, "COMMENTED", "COMMENT").behavior(t)}
	in, restarts := goneJudge(t, e, true, nil)
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 808 {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if *restarts != 1 {
		t.Fatalf("restarts = %d, want 1", *restarts)
	}
	var own []store.Run
	for _, r := range e.runs() {
		if r.Role == string(agents.RoleJudge) && r.Kind == store.RunOwnPass {
			own = append(own, r)
		}
	}
	if len(own) != 2 || own[0].State != store.RunAbandoned || own[1].State != store.RunVerified {
		t.Fatalf("own-pass runs = %+v, want the refused one abandoned and the second verified", own)
	}
	if res.OwnPass == nil || res.OwnPass.Status != ReportOK || res.OwnPass.RunID != own[1].ID {
		t.Fatalf("own pass = %+v", res.OwnPass)
	}
	submits := e.ag.submitsFor(agents.RoleJudge)
	if len(submits) != 3 || submits[0].Text != submits[1].Text || !strings.Contains(submits[2].Text, "phase: candidates") {
		t.Fatalf("judge prompts = %d: want the own pass twice, then the candidates", len(submits))
	}
	if len(e.eventsOf("round.judge_restarted")) != 1 {
		t.Errorf("round.judge_restarted events = %v", e.eventsOf("round.judge_restarted"))
	}
}

// A second refusal, or a start that fails, ends the round at once with the
// refusal, the reviewer still at work interrupted and its run abandoned,
// instead of after the reviewers.
func TestAnOwnPassRefusedAgainEndsTheRoundAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		live bool
		err  error
		want string
	}{
		{"refused again", false, nil, "no live agent session"},
		{"the start fails", false, errors.New("agent_pane_busy"), "agent_pane_busy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			// claude-review works until the round cancels its turn: its waits
			// would otherwise move the shared clock through its whole timeout
			// while the own pass is refused, and it would end timed out.
			e.ag.hangs = map[agents.Role]bool{agents.RoleClaude: true}
			in, restarts := goneJudge(t, e, tc.live, tc.err)
			start := e.clock.Now()
			res, err := e.r.RunRound(e.ctx, in)
			if err == nil || res.Outcome != OutcomeError || !strings.Contains(res.Error, "also after a restart of the judge") ||
				!strings.Contains(res.Error, tc.want) {
				t.Fatalf("RunRound = %+v, %v; want the refusal at once", res, err)
			}
			if *restarts != 1 {
				t.Fatalf("restarts = %d, want 1", *restarts)
			}
			if waited := e.clock.Now().Sub(start); waited >= e.r.Config.JudgeFor(nil).Timeout.Duration {
				t.Fatalf("the round waited %s, as long as the reviewers' timeout", waited)
			}
			if claude := e.runOf(agents.RoleClaude, KindRecovery); claude.State != store.RunAbandoned {
				t.Fatalf("claude-review run = %s, want it interrupted and abandoned", claude.State)
			}
			for _, s := range e.ag.submitsFor(agents.RoleJudge) {
				if strings.Contains(s.Text, "phase: candidates") {
					t.Fatalf("the judge got the candidates prompt after its session was gone for good")
				}
			}
		})
	}
}

// A push while the judge does its own pass cuts it short like a reviewer:
// its turn is interrupted (ctrl+c twice) and its run abandoned, and the
// restarted round prompts it for its own pass on the new head, naming the
// head it moved from.
func TestPushDuringOwnPassRestartsItOnTheNewHead(t *testing.T) {
	e := newEnv(t)
	in := e.ownInput(KindInitial)
	sw := e.withRestarts(&in, 2)
	post := e.judgePosts(807, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{
		afterRuns(e, target, store.RunVerified, []agents.Role{agents.RoleClaude, agents.RoleCodexReview}, pushThen(e, head2, hang())),
		writeOwn(),
		post.behavior(t),
	}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.TargetSHA != head2 || res.Restarts != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got := sw.asked(); !slices.Equal(got, []string{head2}) {
		t.Errorf("switches = %v", got)
	}
	judgeAgent := agents.AgentName("talkable/talkable", 11920, agents.RoleJudge)
	var presses int
	for _, s := range e.keys.sends {
		if s == "agent:"+judgeAgent+":ctrl+c" {
			presses++
		}
	}
	if presses != 2 {
		t.Errorf("judge interrupts = %d (%v), want ctrl+c twice", presses, e.keys.sends)
	}
	owns := e.runsOf(agents.RoleJudge, store.RunOwnPass)
	if len(owns) != 2 {
		t.Fatalf("own-pass runs = %d, want the cut one and the restarted one", len(owns))
	}
	if owns[0].TargetSHA != target || owns[0].State != store.RunAbandoned || store.Deref(owns[0].Outcome) != ReportHeadMoved {
		t.Errorf("cut own pass = %s / %s / %q", textx.ShortSHA(owns[0].TargetSHA), owns[0].State, store.Deref(owns[0].Outcome))
	}
	wantRun(t, owns[1], store.RunVerified, ReportOK)
	if owns[1].TargetSHA != head2 || store.Deref(owns[1].ReportPath) != filepath.Join(e.dirOf(head2), agents.OwnFindingsFile) {
		t.Errorf("restarted own pass = %+v", owns[1])
	}
	submits := e.ag.submitsFor(agents.RoleJudge)
	if len(submits) != 3 {
		t.Fatalf("judge submits = %d, want own pass, restarted own pass, candidates", len(submits))
	}
	mustContain(t, "restarted own-pass prompt", submits[1].Text, "phase: own_pass", "head_sha: "+head2,
		"from `"+target+"` to `"+head2+"`", "own_findings: "+filepath.Join(e.dirOf(head2), agents.OwnFindingsFile))
	mustContain(t, "candidates prompt", submits[2].Text, "phase: candidates", "head_sha: "+head2)
	restarted := e.eventsOf("round.restarted")
	if len(restarted) != 1 {
		t.Fatalf("round.restarted events = %d", len(restarted))
	}
	mustContain(t, "round.restarted", restarted[0].Message, "cut short: codex-judge (own pass)")
}

// A usage limit that ends the own pass does not end the round: the
// reviewers finish, the candidates prompt goes out, and the judge's turn
// there pauses the round on the limit, so the paused round continues the
// judge's candidates turn (judgePrompted) with every report in its context.
func TestUsageLimitDuringOwnPassPausesAtTheCandidatesTurn(t *testing.T) {
	e := newEnv(t)
	limited := func(f *fakeAgents, run store.Run, text string) error {
		f.mu.Lock()
		f.reads[agents.RoleJudge] = "■ You've hit your usage limit. Try again at 3:45 PM.\n"
		f.mu.Unlock()
		return f.end(run.ID)
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{limited, endSilently()}
	res, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeUsageLimit || res.Pause == nil || res.Pause.Tool != "codex" {
		t.Fatalf("result = %+v (pause %+v)", res, res.Pause)
	}
	wantRun(t, e.runOf(agents.RoleJudge, store.RunOwnPass), store.RunFailed, string(agents.HealthUsageLimit))
	wantRun(t, e.runOf(agents.RoleJudge, store.RunInitial), store.RunFailed, OutcomeUsageLimit)
	for _, role := range []agents.Role{agents.RoleClaude, agents.RoleCodexReview} {
		if r := e.runOf(role, store.RunInitial); r.State != store.RunVerified {
			t.Errorf("%s = %s, want its report kept", role, r.State)
		}
	}
	if n := len(e.ag.submitsFor(agents.RoleJudge)); n != 2 {
		t.Errorf("judge submits = %d, want the own pass and the candidates", n)
	}
}

// Cancelling the round (magnum abort, a shutdown) during the own pass stops
// it there: no candidates prompt, the candidates run abandoned unprompted,
// and the own pass's run left in flight for the engine to settle (as a
// reviewer's).
func TestCancelDuringOwnPassStopsTheRound(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	e.ag.hangs = map[agents.Role]bool{agents.RoleJudge: true} // its own pass works until the round is cancelled
	e.ag.behaviors[agents.RoleClaude] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		e.waitRun(agents.RoleJudge, target, store.RunWorking)
		cancel()
		return nil
	}}
	res, err := e.r.RunRound(ctx, e.ownInput(KindInitial))
	if err == nil || res.Outcome != OutcomeStopped {
		t.Fatalf("RunRound = %+v, %v; want stopped", res, err)
	}
	if n := len(e.ag.submitsFor(agents.RoleJudge)); n != 1 {
		t.Errorf("judge submits = %d, want only the own pass", n)
	}
	wantRun(t, e.runOf(agents.RoleJudge, store.RunInitial), store.RunAbandoned, "")
	if own := e.runOf(agents.RoleJudge, store.RunOwnPass); own.State != store.RunWorking {
		t.Errorf("own pass = %s, want it left working for the engine", own.State)
	}
}

// A model limit that ends the own pass continues it on the kind's fallback
// model, in a run that is part of the own pass (kind own_pass), so crash
// recovery never takes it for the judge's candidates turn.
func TestOwnPassModelLimitContinuesAsOwnPass(t *testing.T) {
	e := newEnv(t)
	k := e.cfg.Kinds["codex"]
	k.SwitchModel, k.FallbackModels = "/model {model}", []string{"gpt-5"}
	e.cfg.Kinds["codex"] = k
	e.ag.behaviors[agents.RoleJudge] = []behavior{hitsLimit(fableLimitPane), writeOwn(), e.judgePosts(808, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	owns := e.runsOf(agents.RoleJudge, store.RunOwnPass)
	if len(owns) != 2 || len(e.runsOf(agents.RoleJudge, store.RunContinue)) != 0 {
		t.Fatalf("own-pass runs = %d, continue runs = %d; want the own pass and its continuation", len(owns), len(e.runsOf(agents.RoleJudge, store.RunContinue)))
	}
	wantRun(t, owns[0], store.RunFailed, string(agents.HealthModelLimit))
	wantRun(t, owns[1], store.RunVerified, ReportOK)
	submits := e.ag.submitsFor(agents.RoleJudge)
	if len(submits) != 3 {
		t.Fatalf("judge submits = %d", len(submits))
	}
	mustContain(t, "continuation", submits[1].Text, "gpt-5", filepath.Join(e.reportDir(), agents.OwnFindingsFile))
	mustContain(t, "candidates prompt", submits[2].Text, "phase: candidates", "Your own pass is in")
}

// The checkout check covers the judge's own pass: an edit it leaves is
// caught once it ended, named, and the checkout restored before the
// candidates prompt.
func TestCheckoutCheckCoversTheOwnPass(t *testing.T) {
	e := newEnv(t)
	var sees judgeSees
	dirty := func(f *fakeAgents, run store.Run, text string) error {
		e.git.mu.Lock()
		e.git.status = gitx.Status{Tracked: 1}
		e.git.mu.Unlock()
		return writeOwn()(f, run, text)
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{
		afterRuns(e, target, store.RunVerified, []agents.Role{agents.RoleClaude, agents.RoleCodexReview}, dirty),
		sees.then(e, e.judgePosts(809, "COMMENTED", "COMMENT").behavior(t)),
	}
	e.restoreRules(false)
	res, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	dirtyEvents := e.eventsOf("round.checkout_dirty")
	if len(dirtyEvents) != 1 || !strings.Contains(dirtyEvents[0].Message, "codex-judge") {
		t.Fatalf("round.checkout_dirty = %+v, want one naming the judge", dirtyEvents)
	}
	if n := len(e.exec.CallsWithPrefix("git", "-C", slotPath, "reset", "--hard", "--quiet")); n != 1 {
		t.Errorf("tree restores = %d, want 1", n)
	}
	sees.want(t, map[string]string{"codex-judge/own_pass": store.RunVerified})
	if _, err := os.Stat(filepath.Join(e.reportDir(), agents.OwnFindingsFile)); err != nil {
		t.Errorf("judge-own.md: %v", err)
	}
}

// judgeRole is the round's built-in judge as configured.
func (e *env) judgeRole() config.Role {
	e.t.Helper()
	r, ok := e.cfg.RoleByNameOrAlias(nil, string(agents.RoleJudge))
	if !ok {
		e.t.Fatal("no judge role")
	}
	return r
}
