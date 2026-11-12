package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/usage"
)

// fakeReviewer is the local reviewer: it records each round's case and
// answers with the next of answers (the last one repeats); block, when set,
// holds a round until it closes or the round's context ends.
type fakeReviewer struct {
	mu      sync.Mutex
	cases   []evalrun.LocalCase
	answers []func(c evalrun.LocalCase) evalrun.LocalResult
	block   chan struct{}
	// errAt is the error the round of call n (from 1) answers instead of
	// a result.
	errAt map[int]error
}

func (f *fakeReviewer) Round(ctx context.Context, c evalrun.LocalCase) (evalrun.LocalResult, error) {
	f.mu.Lock()
	f.cases = append(f.cases, c)
	i := min(len(f.cases), len(f.answers)) - 1
	answer, block, err := f.answers[i], f.block, f.errAt[len(f.cases)]
	f.mu.Unlock()
	if err != nil {
		return evalrun.LocalResult{}, err
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return evalrun.LocalResult{}, ctx.Err()
		}
	}
	r := answer(c)
	r.Round = c.Round
	return r, nil
}

func (f *fakeReviewer) calls() []evalrun.LocalCase {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cases)
}

// fakeMagnum is magnum's daemon: busy says why it is busy ("" = it is not);
// prs are the PRs its registry has (a ref it lacks is not found, its
// daemon up), prErr fails PRStates, and reads counts the PRStates calls;
// own are your own open PRs OwnPRs lists, ownErr fails it, and ownReads
// counts its calls; fix are the PRs' fix records (a ref it lacks has the
// zero record), and fixErr fails FixRecords; posted are the PRs' rounds
// that posted a review, in the order they began, which FirstPostedRound
// reads (postedReads counts its calls, postedErr fails it, and postedGate,
// when set, holds it until the test closes it).
type fakeMagnum struct {
	mu          sync.Mutex
	busy        string
	prs         map[PRRef]PRState
	prErr       error
	reads       int
	own         []PRState
	ownErr      error
	ownReads    int
	fix         map[PRRef]FixRecord
	fixErr      error
	posted      map[PRRef][]PostedRound
	postedReads int
	postedErr   error
	postedGate  chan struct{}
}

func (f *fakeMagnum) FirstPostedRound(ctx context.Context, ref PRRef, since time.Time) (PostedRound, bool, error) {
	f.mu.Lock()
	f.postedReads++
	gate := f.postedGate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return PostedRound{}, false, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postedErr != nil {
		return PostedRound{}, false, f.postedErr
	}
	for _, r := range f.posted[ref] {
		if !r.Start.Before(since) {
			r.Findings = slices.Clone(r.Findings)
			return r, true, nil
		}
	}
	return PostedRound{}, false, nil
}

// post makes magnum's registry hold round r of ref, after its others.
func (f *fakeMagnum) post(ref PRRef, r PostedRound) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posted == nil {
		f.posted = map[PRRef][]PostedRound{}
	}
	f.posted[ref] = append(f.posted[ref], r)
}

// setPostedErr makes FirstPostedRound fail with err (nil: it reads).
func (f *fakeMagnum) setPostedErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postedErr = err
}

// setPostedGate holds FirstPostedRound until gate closes (nil: no hold).
func (f *fakeMagnum) setPostedGate(gate chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postedGate = gate
}

// postedCalls is how many times FirstPostedRound was asked.
func (f *fakeMagnum) postedCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postedReads
}

func (f *fakeMagnum) FixRecords(_ context.Context, refs []PRRef) (map[PRRef]FixRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fixErr != nil {
		return nil, f.fixErr
	}
	out := make(map[PRRef]FixRecord, len(refs))
	for _, r := range refs {
		out[r] = f.fix[r]
	}
	return out, nil
}

// setFix makes magnum's registry record rec of ref's fix loops.
func (f *fakeMagnum) setFix(ref PRRef, rec FixRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fix == nil {
		f.fix = map[PRRef]FixRecord{}
	}
	f.fix[ref] = rec
}

// setFixErr makes FixRecords fail with err (nil: it reads).
func (f *fakeMagnum) setFixErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fixErr = err
}

func (f *fakeMagnum) OwnPRs(context.Context) ([]PRState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ownReads++
	if f.ownErr != nil {
		return nil, f.ownErr
	}
	return slices.Clone(f.own), nil
}

func (f *fakeMagnum) Busy(context.Context) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy != "", f.busy, nil
}

func (f *fakeMagnum) PRStates(_ context.Context, refs []PRRef) (map[PRRef]PRState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.prErr != nil {
		return nil, f.prErr
	}
	out := make(map[PRRef]PRState, len(refs))
	for _, r := range refs {
		v, ok := f.prs[r]
		if !ok {
			v = PRState{DaemonUp: true}
		}
		v.Ref = r
		out[r] = v
	}
	return out, nil
}

// setPR makes magnum's registry say v of ref (nil: it lacks the PR).
func (f *fakeMagnum) setPR(ref PRRef, v *PRState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prs == nil {
		f.prs = map[PRRef]PRState{}
	}
	if v == nil {
		delete(f.prs, ref)
		return
	}
	f.prs[ref] = *v
}

// setPRErr makes PRStates fail with err (nil: it reads again).
func (f *fakeMagnum) setPRErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prErr = err
}

func (f *fakeMagnum) prReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakeMagnum) set(why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy = why
}

// reviewRig is the coding rig with the local review wired: the fake
// reviewer and magnum, a watch of the repository, the review candidate,
// and a Codex gauge that reads used percent.
type reviewRig struct {
	*codingRig
	rv     *fakeReviewer
	magnum *fakeMagnum
	used   float64
}

func newReviewRig(t *testing.T, answers ...func(evalrun.LocalCase) evalrun.LocalResult) *reviewRig {
	t.Helper()
	r := &reviewRig{codingRig: newCodingRig(t), rv: &fakeReviewer{answers: answers}, magnum: &fakeMagnum{}}
	r.cfg.Watches = []config.Watch{{Owner: "example", Include: []string{"*"}, Identity: "zhuravel"}}
	dir, err := WriteReviewCandidate(r.layout)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []*Deps{&r.deps, &r.e.d} {
		d.Review, d.Magnum, d.ReviewCandidate = r.rv, r.magnum, dir
		d.Usage = func(context.Context, time.Time) (usage.Snapshot, error) {
			return usage.Snapshot{Window: usage.Window{UsedPercent: r.used}}, nil
		}
	}
	return r
}

// restart is the coding rig's restart: the review's deps reach the new
// engine through r.deps, where the rig put them.
func (r *reviewRig) restart() {
	r.t.Helper()
	r.codingRig.restart()
}

// reviewTask is a bug task at its local review, its writer's fix
// committed on its branch and proven: the head the round reviews.
func (r *reviewRig) reviewTask(key string) (registry.Task, string) {
	r.t.Helper()
	task := r.codingTask(key, "bug")
	head := r.fix(task)
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", "local_review")
		u.Set("head_sha", head)
	}); err != nil {
		r.t.Fatal(err)
	}
	return r.reload(task.ID), head
}

// run ticks and waits for the work off the loop, n times or until done
// holds.
func (r *reviewRig) run(n int, done func() bool) {
	r.t.Helper()
	for range n {
		if done != nil && done() {
			return
		}
		r.tick()
		r.e.waitBackground()
	}
}

// startBlockedRound runs the ticks that start a round the reviewer holds
// (block): the claim's, then the round's, without waiting for the round.
func (r *reviewRig) startBlockedRound() {
	r.t.Helper()
	r.tick()
	r.e.waitBackground()
	r.tick()
	deadline := time.Now().Add(10 * time.Second)
	for len(r.rv.calls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(r.rv.calls()) != 1 {
		r.t.Fatalf("%d rounds started, want 1", len(r.rv.calls()))
	}
}

// cycles are the task's review cycles.
func (r *reviewRig) cycles(id int64) []registry.Cycle {
	r.t.Helper()
	cs, err := r.reg.Cycles(r.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return cs
}

// findings are the task's findings, by fingerprint.
func (r *reviewRig) findings(id int64) map[string]registry.Finding {
	r.t.Helper()
	fs, err := r.reg.Findings(r.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[string]registry.Finding{}
	for _, f := range fs {
		out[f.Fingerprint] = f
	}
	return out
}

// writerFixes is the writer's fix turn: a commit (none when files is nil),
// proof.json, the reply to review-<n>.json (each finding fixed, but those
// declined with the risk given, "" for none), then its turn's end.
func (r *reviewRig) writerFixes(task registry.Task, n int, files map[string]string, declined map[string]string) {
	r.t.Helper()
	r.clock.Add(time.Minute)
	if files != nil {
		r.commit(task.JiraKey+" fixes the review", files)
	}
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	var rf struct {
		Findings []struct {
			ID string `json:"id"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewFile(n))), &rf); err != nil {
		r.t.Fatal(err)
	}
	var replies []map[string]string
	for _, f := range rf.Findings {
		reply := map[string]string{"id": f.ID, "action": "fixed"}
		if risk, ok := declined[f.ID]; ok {
			reply["action"], reply["risk"] = "declined", risk
		}
		replies = append(replies, reply)
	}
	b, _ := json.Marshal(map[string]any{"replies": replies})
	r.taskFile(task.ID, reviewReplyFile(n), string(b))
	r.ended(task, "fx-writer")
}

// result is a round's answer: a dry run of findings (verdict from them).
func result(fs ...evalrun.LocalFinding) func(evalrun.LocalCase) evalrun.LocalResult {
	return func(evalrun.LocalCase) evalrun.LocalResult {
		r := evalrun.LocalResult{Outcome: pipeline.OutcomeDryRun, Verdict: "clean", Findings: fs}
		for _, f := range fs {
			switch f.Severity {
			case "P0", "P1", "P2", "P3":
				r.Counts[f.Severity[1]-'0']++
			}
		}
		switch {
		case r.Counts[0]+r.Counts[1] > 0:
			r.Verdict = "blocking"
		case r.Counts[2]+r.Counts[3] > 0:
			r.Verdict = "non_blocking"
		}
		return r
	}
}

// rounds are n rounds that each answer answer.
func rounds(n int, answer func(evalrun.LocalCase) evalrun.LocalResult) []func(evalrun.LocalCase) evalrun.LocalResult {
	out := make([]func(evalrun.LocalCase) evalrun.LocalResult, n)
	for i := range out {
		out[i] = answer
	}
	return out
}

// outcome is a round that ended as outcome, its result clean.
func outcome(o string) func(evalrun.LocalCase) evalrun.LocalResult {
	return func(c evalrun.LocalCase) evalrun.LocalResult {
		r := result()(c)
		r.Outcome = o
		return r
	}
}

var (
	refundFinding = evalrun.LocalFinding{Severity: "P1", Title: "Points skip the refunded activities", Path: "app/credit.rb", Line: 3,
		Body: "A refund leaves its points counted."}
	unitFinding = evalrun.LocalFinding{Severity: "P2", Title: "Points count cancelled activities", Path: "app/credit.rb", Line: 14}
	logFinding  = evalrun.LocalFinding{Severity: "P1", Title: "The log names the wrong method", Path: "app/notes.rb", Line: 31}
	nitFinding  = evalrun.LocalFinding{Severity: "P3", Title: "A comment says cents (optional, no reply needed)", Path: "app/credit.rb", Line: 1}
)

// fixedCredit is a commit that changes app/credit.rb once more (round n's
// fix), its spec still passing.
func fixedCredit(n int) map[string]string {
	return map[string]string{"app/credit.rb": strings.Replace(creditRB, "    0\n", "    10\n", 1) + fmt.Sprintf("# round %d\n", n)}
}

// The loop stops as soon as a round blocks nothing: round 1 finds a P1, the
// writer fixes it, round 2 is clean. Two cycles, the stage passes with exit
// clean, and the writer got the blocking finding as review-1.json.
func TestReviewLoopStopsWhenNothingBlocks(t *testing.T) {
	r := newReviewRig(t, result(refundFinding), result())
	task, head := r.reviewTask("PR-12361")
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 1 || !strings.Contains(ps[0].Text, reviewFile(1)) {
		t.Fatalf("the writer's prompts %+v, want its fix turn naming %s", ps, reviewFile(1))
	}
	if got := r.readTask(task.ID, reviewFile(1)); !strings.Contains(got, refundFinding.Title) || strings.Contains(got, nitFinding.Title) {
		t.Fatalf("%s:\n%s", reviewFile(1), got)
	}
	r.writerFixes(task, 1, fixedCredit(1), nil)
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	got := r.reload(task.ID)
	cs := r.cycles(task.ID)
	if got.Stage != "pr_body" || len(cs) != 2 || cs[0].Exit != "" || cs[1].Exit != registry.ExitClean || cs[0].HeadBefore != head ||
		cs[0].HeadAfter == head || cs[1].HeadBefore != cs[0].HeadAfter {
		t.Fatalf("task %s at %s waits %q; cycles %+v", got.State, got.Stage, got.WaitsWhat, cs)
	}
	if calls := r.rv.calls(); len(calls) != 2 || calls[1].HeadSHA != cs[0].HeadAfter || calls[0].Round != 1 || calls[1].Round != 2 {
		t.Fatalf("rounds %+v", calls)
	}
}

// At most cycles (3) rounds run: three rounds that each block on a new
// finding end the loop at its cap, and the task needs you with the count.
func TestReviewLoopStopsAtThreeRounds(t *testing.T) {
	r := newReviewRig(t, result(refundFinding), result(unitFinding), result(logFinding))
	task, _ := r.reviewTask("PR-12362")
	for n := 1; n <= 2; n++ {
		r.run(6, func() bool {
			return fileIn(r.layout.FactoryTaskDir(task.ID), reviewFile(n)) && len(r.rt.promptsTo("fx-writer")) >= n
		})
		r.writerFixes(task, n, fixedCredit(n), nil)
	}
	r.run(10, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	cs := r.cycles(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWhat != "1 blocking finding after 3 review rounds (blocking 1 → 1 → 1, P1 1 → 0 → 1; verdict blocking): P1 "+logFinding.Title+" (app/notes.rb)" || len(cs) != 3 || cs[2].Exit != registry.ExitCap {
		t.Fatalf("task %s waits %q; cycles %+v", got.State, got.WaitsWhat, cs)
	}
	if n := len(r.rv.calls()); n != 3 {
		t.Fatalf("%d rounds, want 3", n)
	}
}

// Round 1 runs the watch's review roles, later rounds the same roles
// without later_without (claude-simplify).
func TestLaterRoundsRunWithoutSimplify(t *testing.T) {
	r := newReviewRig(t, result(refundFinding), result())
	task, _ := r.reviewTask("PR-12363")
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	r.writerFixes(task, 1, fixedCredit(1), nil)
	r.run(8, func() bool { return len(r.rv.calls()) == 2 })
	calls := r.rv.calls()
	if len(calls) != 2 {
		t.Fatalf("%d rounds", len(calls))
	}
	want1 := []string{"codex-judge", "claude-review", "codex-review", "claude-simplify"}
	if !slices.Equal(calls[0].Roles, want1) || !slices.Equal(calls[1].Roles, want1[:3]) {
		t.Fatalf("round 1 roles %v, round 2 %v", calls[0].Roles, calls[1].Roles)
	}
}

// The writer must commit its fix before the next round: a fix turn that
// ends with the branch where the round reviewed it gets a fix turn saying
// so, and no round runs until a commit lands.
func TestReviewLoopNeedsACommitBeforeTheNextRound(t *testing.T) {
	r := newReviewRig(t, result(refundFinding), result())
	task, head := r.reviewTask("PR-12364")
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	r.writerFixes(task, 1, nil, nil)
	r.run(6, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 })
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, fixLead) || !strings.Contains(ps[1].Text, "commit") {
		t.Fatalf("the writer's prompts %+v, want a fix turn asking for its commit", ps)
	}
	if n := len(r.rv.calls()); n != 1 {
		t.Fatalf("%d rounds before the writer committed, want 1", n)
	}
	if got := r.git(r.copyDir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("head %s", got)
	}
	r.clock.Add(time.Minute)
	r.commit(task.JiraKey+" fixes the review", fixedCredit(1))
	r.ended(task, "fx-writer")
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 2 {
		t.Fatalf("task at %s after %d rounds", got.Stage, len(r.rv.calls()))
	}
}

// After each fix commit the factory's checks run again at the new head,
// through the coding stage's own path: its lint, before the next round,
// and a lint offense on the fix gives the writer a fix turn, not a round.
func TestReviewLoopRunsThePrecheckAfterEachFix(t *testing.T) {
	r := newReviewRig(t, result(refundFinding), result())
	task, head := r.reviewTask("PR-12365")
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	offenses := 0
	r.lint = func(files []string) string {
		if offenses > 0 {
			return ""
		}
		offenses++
		return "app/credit.rb:3:5: C: Style: the fix's line"
	}
	r.writerFixes(task, 1, map[string]string{"app/credit.rb": strings.Replace(creditRB, "    0\n", "    10 \n", 1)}, nil)
	r.run(6, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 })
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, fixLead) || !strings.Contains(ps[1].Text, "lint") {
		t.Fatalf("the writer's prompts %+v, want a fix turn for the lint on its fix", ps)
	}
	if n := len(r.rv.calls()); n != 1 {
		t.Fatalf("%d rounds before the fix passed its checks, want 1", n)
	}
	r.clock.Add(time.Minute)
	fix := r.commit(task.JiraKey+" lint", fixedCredit(1))
	r.ended(task, "fx-writer")
	r.run(8, func() bool { return len(r.rv.calls()) == 2 })
	lints := r.ran("bin/lint")
	if len(lints) == 0 || lints[len(lints)-1].Head != fix || lints[len(lints)-1].Head == head {
		t.Fatalf("lint runs %+v, want the last at the fix %s", lints, fix)
	}
	if calls := r.rv.calls(); len(calls) != 2 || calls[1].HeadSHA != fix {
		t.Fatalf("rounds %+v, want round 2 at %s", calls, fix)
	}
}

// A finding that comes back twice after its fix goes to the doubts: once
// only such findings block, the loop ends on an oscillation and the stage
// passes with the finding among the doubts.
func TestAFindingThatReturnsTwiceAfterItsFixBecomesADoubt(t *testing.T) {
	r := newReviewRig(t, result(refundFinding))
	task, _ := r.reviewTask("PR-12366")
	for n := 1; n <= 2; n++ {
		r.run(6, func() bool {
			return fileIn(r.layout.FactoryTaskDir(task.ID), reviewFile(n)) && len(r.rt.promptsTo("fx-writer")) >= n
		})
		r.writerFixes(task, n, fixedCredit(n), nil)
	}
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
	cs := r.cycles(task.ID)
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(cs) != 3 || cs[2].Exit != registry.ExitOscillation {
		t.Fatalf("task %s at %s waits %q; cycles %+v", got.State, got.Stage, got.WaitsWhat, cs)
	}
	f := r.findings(task.ID)[Fingerprint(refundFinding)]
	if f.Reopened != 2 {
		t.Fatalf("finding %+v, want it reopened twice", f)
	}
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Exit != registry.ExitOscillation || len(sum.Doubts) != 1 || sum.Doubts[0].Title != refundFinding.Title {
		t.Fatalf("summary %+v, want the finding among the doubts", sum)
	}
}

// A finding the judge marks a product question joins the task's question
// batch: it is asked as a question of the local review, the task waits for
// your answer, and the writer gets no fix turn before it.
func TestAQuestionFindingJoinsTheQuestionBatch(t *testing.T) {
	q := evalrun.LocalFinding{Severity: "P2", Title: "Should a refund take back the points it gave?", Path: "app/credit.rb", Line: 12, Question: true,
		Body: "The ticket says nothing of refunds."}
	r := newReviewRig(t, result(q))
	task, _ := r.reviewTask("PR-12367")
	r.run(5, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWhat != "1 question before local review goes on" {
		t.Fatalf("task %s waits %q", got.State, got.WaitsWhat)
	}
	qs, err := r.reg.Questions(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].Text != q.Title || qs[0].RaisedBy != "local_review" || qs[0].State != registry.QuestionOpen || len(qs[0].Options) < 2 {
		t.Fatalf("questions %+v", qs)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 0 {
		t.Fatalf("the writer got %d prompts before your answer, want none", len(ps))
	}
}

// The local judge gets the operator's answers to the factory's questions
// beside the ticket: localCase reads the task's answers.md into the case,
// the only statement of intent a person made. A task the operator answered
// nothing has no answers.md, and its case carries none.
func TestTheLocalRoundGetsTheOperatorsAnswersFromTheTasksAnswersFile(t *testing.T) {
	const answers = "# Your answers\n\nThe operator's answers to the questions of this task, oldest first.\n\n## Should a refund take back the points it gave?\n\nAsked by fx-judge (revision 1, question 1). The answer is the recommended option.\n\nkeep them\n"
	for name, tc := range map[string]struct{ file, want string }{
		"answered":   {answers, answers},
		"unanswered": {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReviewRig(t, result())
			task, _ := r.reviewTask("PR-12368")
			if tc.file != "" {
				r.taskFile(task.ID, answersFile, tc.file)
			}
			r.run(4, func() bool { return len(r.rv.calls()) > 0 })
			calls := r.rv.calls()
			if len(calls) == 0 {
				t.Fatal("no round ran")
			}
			if calls[0].AnswersMD != tc.want {
				t.Errorf("AnswersMD = %q, want %q", calls[0].AnswersMD, tc.want)
			}
		})
	}
}

// The local review reads the title the PR will have: the task's title laid
// out by the repository's title_format, as the ready card and the publish lay
// it out. Where a repository requires "[KEY] Title", a description that read
// "KEY Title" would have the judge raise a title finding the writer cannot fix.
func TestTheLocalRoundGetsTheTitleThePRWillHave(t *testing.T) {
	for name, tc := range map[string]struct{ format, want string }{
		"default":  {"", "PR-12370 title"},
		"brackets": {"[{key}] {title}", "[PR-12370] title"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReviewRig(t, result())
			r.cfg.Factory.Repos[0].TitleFormat = tc.format
			task, _ := r.reviewTask("PR-12370")
			r.run(4, func() bool { return len(r.rv.calls()) > 0 })
			calls := r.rv.calls()
			if len(calls) == 0 {
				t.Fatal("no round ran")
			}
			if calls[0].Title != tc.want || calls[0].Title != prTitle(task, tc.format) {
				t.Errorf("the round's title = %q, want %q (the PR's)", calls[0].Title, tc.want)
			}
		})
	}
}

// End to end, from the task's folder to the judge's ticket file: the round
// is handed the task's ticket.md and the answers.md the factory wrote
// (answersMarkdown, here from two answered questions), and the file the
// round writes holds the ticket, the fixed heading and the operator's
// questions and answers, without answers.md's own title or the line that
// tells the factory's agents to weigh the answers against their prompt, the
// question headings one level below the fixed heading.
func TestTheLocalRoundsTicketFileJoinsTheTasksTicketAndAnswers(t *testing.T) {
	r := newReviewRig(t, result())
	task, _ := r.reviewTask("PR-12369")
	r.taskFile(task.ID, ticketFile, "# PR-12369\n\n## Summary\n\nCredit points count the activities.\n\n\n")
	r.taskFile(task.ID, answersFile, answersMarkdown([]registry.Question{
		{Text: "Should a refund take back the points it gave?", RaisedBy: "fx-judge", Revision: 1, Index: 0, State: registry.QuestionAnswered,
			Options: []registry.Option{{Label: "keep them"}, {Label: "take them back"}}, Recommended: 0, TookRecommendation: true, Answer: "keep them"},
		{Text: "Do guest orders count?", RaisedBy: "local_review", Revision: 2, Index: 0, State: registry.QuestionAnswered, Recommended: -1,
			Answer: "only paid ones"},
	}))
	r.run(4, func() bool { return len(r.rv.calls()) > 0 })
	calls := r.rv.calls()
	if len(calls) == 0 {
		t.Fatal("no round ran")
	}
	want := "# PR-12369\n\n## Summary\n\nCredit points count the activities.\n\n" +
		"## The operator's answers to the factory's questions\n\n" +
		"### Should a refund take back the points it gave?\n\nAsked by fx-judge (revision 1, question 1). The answer is the recommended option.\n\nkeep them\n\n" +
		"### Do guest orders count?\n\nAsked by local_review (revision 2, question 1). The answer is the operator's own words; no option was recommended.\n\nonly paid ones\n"
	if got := evalrun.LocalTicket(calls[0].TicketMD, calls[0].AnswersMD); got != want {
		t.Errorf("the ticket file = %q, want %q", got, want)
	}
}

// A product question you answered "change it" that the next round raises
// again was not changed: it is a live finding the writer gets to fix, never
// a leftover that lets the loop end clean. One answered "keep it" is a
// leftover: you decided it.
func TestAnAnsweredChangeItThatWasNotChangedStaysLive(t *testing.T) {
	q := evalrun.LocalFinding{Severity: "P2", Title: "Should a refund take back the points it gave?", Path: "app/credit.rb", Line: 12, Question: true,
		Body: "The ticket says nothing of refunds."}
	r := newReviewRig(t, result(q), result(q, refundFinding), result()) // round 2: another finding blocks beside it
	task, _ := r.reviewTask("PR-12369")
	r.run(5, func() bool { return !isLive(r.reload(task.ID)) })
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "change it"}})
	r.run(6, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	r.writerFixes(task, 1, fixedCredit(1), nil) // a commit that changes something else: the question's code stays
	r.run(10, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 || r.reload(task.ID).Stage != "local_review" })
	got := r.reload(task.ID)
	if got.Stage != "local_review" || len(r.rt.promptsTo("fx-writer")) != 2 {
		t.Fatalf("task %s at %s waits %q after %d rounds, want the writer's fix turn for the question it did not change", got.State,
			got.Stage, got.WaitsWhat, len(r.rv.calls()))
	}
	if review := r.readTask(task.ID, reviewFile(2)); !strings.Contains(review, q.Title) {
		t.Fatalf("%s:\n%s\nwant the question you answered change it among the findings to fix", reviewFile(2), review)
	}
}

// A decline counts only with the risk of the suggested change: a finding
// the writer declined without one stays open and blocks the next round
// again; one declined with a risk is rebutted, blocks no more, and ends
// among the leftovers.
func TestADeclineWithoutARiskStaysOpen(t *testing.T) {
	r := newReviewRig(t, append(rounds(2, result(refundFinding, unitFinding)), result())...)
	task, _ := r.reviewTask("PR-12368")
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	var rf ReviewFile
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewFile(1))), &rf); err != nil || len(rf.Findings) != 2 {
		t.Fatalf("%s: %+v, %v", reviewFile(1), rf, err)
	}
	ids := map[string]string{}
	for _, f := range rf.Findings {
		ids[f.Title] = f.ID
	}
	r.writerFixes(task, 1, fixedCredit(1), map[string]string{ids[refundFinding.Title]: "", ids[unitFinding.Title]: "counting them changes the monthly statements"})
	r.run(8, func() bool { return fileIn(r.layout.FactoryTaskDir(task.ID), reviewFile(2)) })
	fs := r.findings(task.ID)
	if f := fs[Fingerprint(refundFinding)]; f.Outcome != registry.FindingOpen {
		t.Errorf("the finding declined without a risk is %s, want open", f.Outcome)
	}
	if f := fs[Fingerprint(unitFinding)]; f.Outcome != registry.FindingRebutted {
		t.Errorf("the finding declined with a risk is %s, want rebutted", f.Outcome)
	}
	var rf2 ReviewFile
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewFile(2))), &rf2); err != nil {
		t.Fatal(err)
	}
	if len(rf2.Findings) != 1 || rf2.Findings[0].Title != refundFinding.Title {
		t.Fatalf("round 2 hands the writer %+v, want only the finding declined without a risk", rf2.Findings)
	}
	r.writerFixes(task, 2, fixedCredit(2), nil)
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Exit != registry.ExitClean || len(sum.Declined) != 1 || sum.Declined[0].Title != unitFinding.Title ||
		sum.Declined[0].Risk != "counting them changes the monthly statements" {
		t.Fatalf("summary %+v, want the finding declined with its risk among the declined", sum)
	}
}

// Review Focus 1: a round that ended needs_attention is review
// unavailable, never clean: the task needs you saying so, the cycle ends
// unavailable, the writer gets nothing and nothing is pushed or begun.
func TestReviewUnavailableNeedsYouAndPushesNothing(t *testing.T) {
	r := newReviewRig(t, outcome(pipeline.OutcomeNeedsAttention))
	task, _ := r.reviewTask("PR-12369")
	r.run(5, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWhat != "review unavailable (needs_attention)" || got.Stage != "local_review" {
		t.Fatalf("task %s at %s waits %q", got.State, got.Stage, got.WaitsWhat)
	}
	if cs := r.cycles(task.ID); len(cs) != 1 || cs[0].Exit != registry.ExitUnavailable {
		t.Fatalf("cycles %+v", cs)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 0 {
		t.Fatalf("the writer got %d prompts, want none", len(ps))
	}
	if effs, err := r.reg.OpenEffects(r.ctx); err != nil || len(effs) != 0 {
		t.Fatalf("effects %+v, %v", effs, err)
	}
	if fileIn(r.layout.FactoryTaskDir(task.ID), reviewSummaryFile) {
		t.Fatal("an unavailable review left a summary for the ready card")
	}
}

// A local round yields to magnum's real rounds: while magnum is busy no
// round starts and the task says why; once it is not, the round runs.
func TestLocalRoundWaitsWhileMagnumRunsARealRound(t *testing.T) {
	r := newReviewRig(t, result())
	r.magnum.set("a review round of magnum's runs")
	task, _ := r.reviewTask("PR-12370")
	r.run(4, nil)
	if got := r.reload(task.ID); len(r.rv.calls()) != 0 || got.WaitsWho != "magnum" || !strings.Contains(got.WaitsWhat, "runs") {
		t.Fatalf("%d rounds; task waits %q: %q", len(r.rv.calls()), got.WaitsWho, got.WaitsWhat)
	}
	r.magnum.set("")
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); len(r.rv.calls()) != 1 || got.Stage != "pr_body" {
		t.Fatalf("%d rounds; task at %s", len(r.rv.calls()), got.Stage)
	}
}

// A local round with Codex roles yields to the Codex soft cap: at or past
// [usage] codex_soft no round starts, and the task says Codex's use.
func TestLocalRoundWaitsAtTheCodexSoftCap(t *testing.T) {
	r := newReviewRig(t, result())
	r.cfg.Usage.CodexSoft = 80
	r.used = 85
	task, _ := r.reviewTask("PR-12371")
	r.run(4, nil)
	if got := r.reload(task.ID); len(r.rv.calls()) != 0 || got.WaitsWho != "usage" || !strings.Contains(got.WaitsWhat, "codex at 85%") {
		t.Fatalf("%d rounds; task waits %q: %q", len(r.rv.calls()), got.WaitsWho, got.WaitsWhat)
	}
	r.used = 40
	r.run(6, func() bool { return len(r.rv.calls()) == 1 })
	if len(r.rv.calls()) != 1 {
		t.Fatal("no round below the soft cap")
	}
}

// A local round runs while no writer turn does: while an agent of the task
// is at work in its copy, no round starts.
func TestLocalRoundWaitsWhileTheWriterWorks(t *testing.T) {
	r := newReviewRig(t, result())
	task, _ := r.reviewTask("PR-12372")
	r.rt.setAtWork("fx-writer")
	r.run(4, nil)
	if got := r.reload(task.ID); len(r.rv.calls()) != 0 || got.WaitsWho != waitAgent || !strings.Contains(got.WaitsWhat, "fx-writer") {
		t.Fatalf("%d rounds; task waits %q: %q", len(r.rv.calls()), got.WaitsWho, got.WaitsWhat)
	}
	r.rt.setAtWork("")
	r.run(6, func() bool { return len(r.rv.calls()) == 1 })
	if len(r.rv.calls()) != 1 {
		t.Fatal("no round once the writer stopped")
	}
}

// Every round is a cycle with its findings: the heads before and after, the
// review skill's hash and its exit, and each finding by its fingerprint,
// its priority and whether it blocks; a blocking one the writer fixed is
// fixed, a P3 stays open for the ready card.
func TestEveryRoundIsACycleWithItsFindings(t *testing.T) {
	r := newReviewRig(t, result(refundFinding, nitFinding), result(nitFinding))
	task, head := r.reviewTask("PR-12373")
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	r.writerFixes(task, 1, fixedCredit(1), nil)
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	cs := r.cycles(task.ID)
	if len(cs) != 2 || cs[0].HeadBefore != head || cs[0].HeadAfter == "" || cs[0].SkillHash == "" || cs[0].Ended.IsZero() || cs[1].Exit != registry.ExitClean {
		t.Fatalf("cycles %+v", cs)
	}
	fs := r.findings(task.ID)
	refund, nit := fs[Fingerprint(refundFinding)], fs[Fingerprint(nitFinding)]
	if refund.Priority != "P1" || !refund.Blocking || refund.Outcome != registry.FindingFixed || refund.FirstCycle != 1 || refund.FixCommit != cs[0].HeadAfter {
		t.Errorf("the P1 %+v", refund)
	}
	if nit.Priority != "P3" || nit.Blocking || nit.Outcome != registry.FindingOpen {
		t.Errorf("the P3 %+v", nit)
	}
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Exit != registry.ExitClean || len(sum.Leftovers) != 1 || sum.Leftovers[0].Title != nitFinding.Title {
		t.Fatalf("summary %+v, want the P3 among the leftovers", sum)
	}
}

// A local round's findings keep the roles that raised them, as the judge's
// provenance names them, in the registry (sources_json): a later round adds
// its own to a finding it sees again.
func TestALocalRoundsFindingsKeepTheirSources(t *testing.T) {
	refund, nit := refundFinding, nitFinding
	refund.Sources, nit.Sources = []string{"claude-review", "judge"}, []string{"codex-review"}
	again := refundFinding
	again.Sources = []string{"codex-review"}
	r := newReviewRig(t, result(refund, nit), result(again))
	task, _ := r.reviewTask("PR-12391")
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	fs := r.findings(task.ID)
	if got := fs[Fingerprint(refundFinding)].Sources; !slices.Equal(got, []string{"claude-review", "judge"}) {
		t.Errorf("the P1's sources %q after round 1", got)
	}
	if got := fs[Fingerprint(nitFinding)].Sources; !slices.Equal(got, []string{"codex-review"}) {
		t.Errorf("the P3's sources %q after round 1", got)
	}
	r.writerFixes(task, 1, fixedCredit(1), nil)
	r.run(8, func() bool {
		return len(r.rv.calls()) == 2 && len(r.cycles(task.ID)) == 2 && !r.cycles(task.ID)[1].Ended.IsZero()
	})
	if got := r.findings(task.ID)[Fingerprint(refundFinding)].Sources; !slices.Equal(got, []string{"claude-review", "judge", "codex-review"}) {
		t.Errorf("the P1's sources %q after round 2, want round 2's added", got)
	}
}

// [factory.local_review] left_blocks = true makes a P3 defect that a round
// leaves open block the loop's exit as a P2 one does: a round whose only
// finding is a P3 does not end the loop clean, the writer gets the finding
// (review-1.json), the finding counts as blocking in the registry, and the
// loop ends clean once a round leaves none.
func TestLeftBlocksMakesALeftP3BlockTheExitOfTheLoop(t *testing.T) {
	r := newReviewRig(t, result(nitFinding), result())
	r.cfg.Factory.LocalReview.LeftBlocks = true
	task, _ := r.reviewTask("PR-12371")
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 1 || !strings.Contains(ps[0].Text, reviewFile(1)) {
		t.Fatalf("the writer's prompts %+v, want its fix turn naming %s", ps, reviewFile(1))
	}
	if got := r.readTask(task.ID, reviewFile(1)); !strings.Contains(got, nitTitle) || strings.Contains(got, "no reply needed") {
		t.Fatalf("%s:\n%s, want the P3 under its title, without the suffix that says no reply is needed", reviewFile(1), got)
	}
	if got := r.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("the task is at %s after a round whose only finding is a P3, want the loop still going", got.Stage)
	}
	if nit := r.findings(task.ID)[Fingerprint(nitFinding)]; !nit.Blocking {
		t.Errorf("the P3 %+v, want it to count as blocking", nit)
	}
	r.writerFixes(task, 1, fixedCredit(1), nil)
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	if got := r.reload(task.ID); got.Stage != "pr_body" || sum.Exit != registry.ExitClean || len(sum.Leftovers) != 0 {
		t.Fatalf("task at %s, summary %+v, want a clean exit with nothing left after the P3 was fixed", got.Stage, sum)
	}
}

// With left_blocks a P3 simplification still blocks nothing (the setting
// is about defects), and a P3 the writer declined with its risk is kept
// for the ready card as a P2 declined is.
func TestLeftBlocksLeavesASimplificationAndADeclinedP3Alone(t *testing.T) {
	simplify := nitFinding
	simplify.Title, simplify.Simplification = "Inline the helper", true
	r := newReviewRig(t, result(simplify), result())
	r.cfg.Factory.LocalReview.LeftBlocks = true
	task, _ := r.reviewTask("PR-12372")
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 0 || sum.Exit != registry.ExitClean || len(sum.Leftovers) != 1 || !sum.Leftovers[0].Simplification {
		t.Fatalf("writer prompts %+v, summary %+v, want a clean exit with the simplification left and no turn for the writer", ps, sum)
	}

	r = newReviewRig(t, rounds(2, result(nitFinding))...)
	r.cfg.Factory.LocalReview.LeftBlocks = true
	task, _ = r.reviewTask("PR-12373")
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	r.writerFixes(task, 1, fixedCredit(1), map[string]string{"F1": "the comment is the spec's own words"})
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Exit != registry.ExitClean || len(sum.Declined) != 1 || sum.Declined[0].Title != nitTitle || len(sum.Leftovers) != 0 {
		t.Fatalf("summary %+v, want the P3 among the declined, with its risk", sum)
	}
}

// nitTitle is nitFinding's title as the writer gets it under left_blocks:
// without the "(optional, no reply needed)" the judge adds to a P3, which
// would let the writer skip a finding it must answer.
const nitTitle = "A comment says cents"

// left_blocks is about defects: a P3 the judge marked a product question
// stays a leftover as without the setting. The loop ends clean, the question
// is among the leftovers and no questions file is written for you.
func TestLeftBlocksLeavesAP3ProductQuestionAmongTheLeftovers(t *testing.T) {
	question := nitFinding
	question.Title, question.Question = "Should archived campaigns copy too", true
	r := newReviewRig(t, result(question), result())
	r.cfg.Factory.LocalReview.LeftBlocks = true
	task, _ := r.reviewTask("PR-12375")
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	if got := r.reload(task.ID); got.Stage != "pr_body" || sum.Exit != registry.ExitClean || len(sum.Leftovers) != 1 || !sum.Leftovers[0].Question {
		t.Fatalf("task %s at %s, summary %+v, want a clean exit with the question left", got.State, got.Stage, sum)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 0 || fileIn(r.layout.FactoryTaskDir(task.ID), roleQuestionsFile("local_review")) {
		t.Errorf("writer prompts %+v, questions file %v, want neither", ps, fileIn(r.layout.FactoryTaskDir(task.ID), roleQuestionsFile("local_review")))
	}
	if q := r.findings(task.ID)[Fingerprint(question)]; q.Blocking {
		t.Errorf("the P3 question %+v, want it not to block", q)
	}
}

// At the cap a round that blocks only through left_blocks (a P3 defect, no
// P0 to P2, no question, no doubt) ends the loop clean instead of parking
// the task on you: the writer had the earlier rounds to fix the P3s, and
// the last round's stay for the card and its w. An event says so.
func TestLeftBlocksEndsTheLoopCleanAtTheCapWhenOnlyP3sBlock(t *testing.T) {
	nit := func(title string, line int) evalrun.LocalFinding {
		f := nitFinding
		f.Title, f.Line = title+" (optional, no reply needed)", line
		return f
	}
	r := newReviewRig(t, result(nit("First nit", 1)), result(nit("Second nit", 12)), result(nit("Third nit", 25)))
	r.cfg.Factory.LocalReview.LeftBlocks = true
	task, _ := r.reviewTask("PR-12376")
	for n := 1; n <= 2; n++ {
		r.run(6, func() bool {
			return fileIn(r.layout.FactoryTaskDir(task.ID), reviewFile(n)) && len(r.rt.promptsTo("fx-writer")) >= n
		})
		r.writerFixes(task, n, fixedCredit(n), nil)
	}
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
	got := r.reload(task.ID)
	cs := r.cycles(task.ID)
	if got.State != registry.TaskActive || got.Stage != "pr_body" || len(cs) != 3 || cs[2].Exit != registry.ExitClean {
		t.Fatalf("task %s at %s waits %q; cycles %+v, want the loop ended clean at its third round", got.State, got.Stage, got.WaitsWhat, cs)
	}
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Exit != registry.ExitClean || len(sum.Leftovers) != 1 || sum.Leftovers[0].Title != "Third nit" || len(knownDefects(sum)) != 1 {
		t.Fatalf("summary %+v, want the third round's P3 left, as a known defect", sum)
	}
	if !slices.ContainsFunc(r.eventTexts(task.ID, "review"), func(s string) bool { return strings.Contains(s, "left_blocks") && strings.Contains(s, "1 P3") }) {
		t.Errorf("review events %q, want one that says left_blocks left 1 P3 for the card at the cap", r.eventTexts(task.ID, "review"))
	}

	// A blocking finding at the cap still parks the task, left_blocks or not.
	r = newReviewRig(t, result(refundFinding), result(unitFinding), result(nit("Third nit", 25), logFinding))
	r.cfg.Factory.LocalReview.LeftBlocks = true
	task, _ = r.reviewTask("PR-12377")
	for n := 1; n <= 2; n++ {
		r.run(6, func() bool {
			return fileIn(r.layout.FactoryTaskDir(task.ID), reviewFile(n)) && len(r.rt.promptsTo("fx-writer")) >= n
		})
		r.writerFixes(task, n, fixedCredit(n), nil)
	}
	r.run(10, func() bool { return !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "blocking finding") {
		t.Fatalf("task %s waits %q, want the cap's stop for a round with a P1 in it", got.State, got.WaitsWhat)
	}
}

// A Codex refusal in a local round makes the task Codex-never for good:
// the round runs again with magnum's judge_fallback in place of the Codex
// judge and without Codex's own review, and no later round runs a Codex
// role; the refused round is never run on Codex again.
func TestACodexRefusalInALocalRoundRunsLaterRoundsWithTheFallbackJudge(t *testing.T) {
	refused := func(c evalrun.LocalCase) evalrun.LocalResult {
		return evalrun.LocalResult{Outcome: pipeline.OutcomeRefused, Refused: config.KindCodex, Round: c.Round}
	}
	r := newReviewRig(t, refused, result(refundFinding), result())
	task, _ := r.reviewTask("PR-12374")
	r.run(6, func() bool { return len(r.rv.calls()) >= 2 && len(r.rt.promptsTo("fx-writer")) > 0 })
	if got := r.reload(task.ID); !got.CodexNever {
		t.Fatal("the task is not Codex-never after a Codex refusal")
	}
	r.writerFixes(task, 1, fixedCredit(1), nil)
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	calls := r.rv.calls()
	if len(calls) != 3 {
		t.Fatalf("%d rounds, want the refused one, its run again and round 2", len(calls))
	}
	for _, c := range calls[1:] {
		if !slices.Contains(c.Roles, "claude-judge") {
			t.Errorf("round %d roles %v, want the fallback judge", c.Round, c.Roles)
		}
		for _, role := range c.Roles {
			if rl, ok := r.cfg.RoleByNameOrAlias(nil, role); ok && rl.AgentKind() == config.KindCodex {
				t.Errorf("round %d after the refusal runs the Codex role %s", c.Round, role)
			}
		}
	}
	if calls[1].Round != 1 || calls[2].Round != 2 {
		t.Fatalf("rounds %d, %d; want round 1 again, then round 2", calls[1].Round, calls[2].Round)
	}
}

// A round that hangs ends at its cap and runs once more; a second hang is
// review unavailable (timeout), and the task needs you.
func TestALocalRoundThatHangsRunsOnceMoreThenNeedsYou(t *testing.T) {
	r := newReviewRig(t, result())
	r.rv.block = make(chan struct{}) // never closed: each round hangs
	r.e.roundCap = 20 * time.Millisecond
	task, _ := r.reviewTask("PR-12375")
	r.run(8, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWhat != "review unavailable (timeout): over its cap of 20ms" || len(r.rv.calls()) != 2 {
		t.Fatalf("task %s waits %q after %d rounds", got.State, got.WaitsWhat, len(r.rv.calls()))
	}
	if cs := r.cycles(task.ID); len(cs) != 1 || cs[0].Exit != registry.ExitUnavailable {
		t.Fatalf("cycles %+v", cs)
	}
}

// A round that went without a reviewer's report (its pane was lost
// mid-round) is not read as clean on that alone: it runs once more, and
// the round that has every report decides.
func TestALocalRoundThatLostAReviewerRunsOnceMoreBeforeItCountsAsClean(t *testing.T) {
	lost := func(c evalrun.LocalCase) evalrun.LocalResult {
		res := result()(c)
		res.Missing = []string{"claude-review"}
		return res
	}
	r := newReviewRig(t, lost, result())
	task, _ := r.reviewTask("PR-12376")
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 2 {
		t.Fatalf("task at %s after %d rounds, want pr_body after 2", got.Stage, len(r.rv.calls()))
	}
	if cs := r.cycles(task.ID); len(cs) != 1 || cs[0].Exit != registry.ExitClean {
		t.Fatalf("cycles %+v, want the one round's cycle, clean", cs)
	}
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil || len(sum.Missing) != 0 {
		t.Fatalf("summary %+v (%v): the round run again heard every reviewer, so none is missing", sum, err)
	}
}

// A drain mid-round loses nothing: the round's cycle stays open, nothing
// is read as its result and the task does not need you; after the restart
// the same round runs again in that cycle (its number, so its leftover
// workspace is closed by its label) and decides.
func TestADrainMidRoundRunsTheRoundAgainAfterTheRestart(t *testing.T) {
	r := newReviewRig(t, result())
	r.rv.block = make(chan struct{})
	task, _ := r.reviewTask("PR-12377")
	r.startBlockedRound()
	r.e.stopBackground() // the drain cancels the round
	r.rv.mu.Lock()
	r.rv.block = nil
	r.rv.mu.Unlock()
	if got := r.reload(task.ID); !isLive(got) {
		t.Fatalf("task %s waits %q after the drain", got.State, got.WaitsWhat)
	}
	if cs := r.cycles(task.ID); len(cs) != 1 || !cs[0].Ended.IsZero() {
		t.Fatalf("cycles %+v, want one still open", cs)
	}
	r.restart()
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" })
	calls := r.rv.calls()
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(calls) != 2 || calls[1].Round != 1 {
		t.Fatalf("task at %s after rounds %+v", got.Stage, calls)
	}
	if cs := r.cycles(task.ID); len(cs) != 1 || cs[0].Exit != registry.ExitClean {
		t.Fatalf("cycles %+v, want the one cycle, clean", cs)
	}
}

// A restart after a round ended but before the loop read it reads the
// result the round left: the round does not run again.
func TestARestartAfterARoundReadsItsResult(t *testing.T) {
	r := newReviewRig(t, result())
	task, _ := r.reviewTask("PR-12378")
	r.run(3, func() bool { return len(r.rv.calls()) == 1 })
	r.e.waitBackground()
	r.restart()
	r.run(4, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 1 {
		t.Fatalf("task at %s after %d rounds, want pr_body after 1", got.Stage, len(r.rv.calls()))
	}
}

// gitIn runs git in dir from a round's goroutine (no t.Fatal there).
func (r *reviewRig) gitIn(dir string, args ...string) {
	if _, err := r.real.Run(context.Background(), execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...)}); err != nil {
		r.t.Errorf("git %v: %v", args, err)
	}
}

// A round's roles work in the task's copy, and what they leave there never
// reaches the writer: a round that leaves the copy off its branch or the
// branch at another head than it reviewed asks you to restore the copy to
// the reviewed head or keep the change, never a bare retry, naming what it
// left; before your answer the factory moves no branch or HEAD and changes
// none of it, nor a tracked change or a new file left beside it (it saves
// them first). Restore puts the change and the new file back and the copy
// back on its branch at the reviewed head, and the round's result is read.
// Keep switches a copy off its branch back to it, carrying the change, and
// the round's result is read; a branch at a new head is the task's head, and
// its checks run again there. Your own put-back and retry let the task go
// on as before.
func TestARoundThatLeavesTheCopyOffItsHeadAsksToRestoreOrKeepIt(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		moved      bool // the branch moved, not only the copy off it
		leave      func(r *reviewRig, dir string)
		undo       func(r *reviewRig, branch, head string)
	}{
		{"a detached HEAD", "the copy on a detached HEAD", false,
			func(r *reviewRig, dir string) { r.gitIn(dir, "switch", "--quiet", "--detach") },
			func(r *reviewRig, branch, _ string) { r.git(r.copyDir, "switch", "--quiet", branch) }},
		{"a moved head", "not at", true,
			func(r *reviewRig, dir string) {
				r.gitIn(dir, "commit", "--quiet", "--allow-empty", "-m", "a reviewer's commit")
			},
			func(r *reviewRig, _, head string) { r.git(r.copyDir, "reset", "--quiet", "--keep", head) }},
		// A tracked change and a new file on a moved head go with it.
		{"a moved head with a tracked change and a new file", "1 tracked change in the copy (README)", true,
			func(r *reviewRig, dir string) {
				r.gitIn(dir, "commit", "--quiet", "--allow-empty", "-m", "a reviewer's commit")
				r.put(dir, "README", "a reviewer's mutant\n")
				r.put(dir, "spec/probe_spec.rb", "a reviewer's probe\n")
			},
			func(r *reviewRig, _, head string) {
				r.git(r.copyDir, "checkout", "--", "README")
				if err := os.Remove(filepath.Join(r.copyDir, "spec", "probe_spec.rb")); err != nil {
					r.t.Fatal(err)
				}
				r.git(r.copyDir, "reset", "--quiet", "--keep", head)
			}},
	} {
		for _, answer := range []string{optionRestoreCopy, optionKeepChange, "your own put-back and retry"} {
			t.Run(tc.name+"/"+answer, func(t *testing.T) {
				var rig *reviewRig
				rig = newReviewRig(t, func(c evalrun.LocalCase) evalrun.LocalResult {
					tc.leave(rig, c.Checkout)
					return result()(c)
				})
				r := rig
				task, head := r.reviewTask("PR-12380")
				r.run(5, func() bool { return !isLive(r.reload(task.ID)) })
				got := r.reload(task.ID)
				qs := r.newestQuestions(task.ID)
				if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "round 1 of the local review left") ||
					!strings.Contains(got.WaitsWhat, tc.want) || got.Stage != "local_review" || strings.Contains(got.WaitsWhat, "retry") ||
					len(qs) != 1 || !slices.Equal(optionLabels(qs[0]), []string{optionRestoreCopy, optionKeepChange}) {
					t.Fatalf("task %s at %s waits %q with %+v; want the restore-or-keep question, naming %q", got.State, got.Stage,
						got.WaitsWhat, qs, tc.want)
				}
				if cs := r.cycles(task.ID); len(cs) != 1 || !cs[0].Ended.IsZero() {
					t.Fatalf("cycles %+v, want the round's still open", cs)
				}
				if ps := r.rt.promptsTo("fx-writer"); len(ps) != 0 {
					t.Fatalf("the writer got %d prompts over the round's leftovers", len(ps))
				}
				moved := r.git(r.copyDir, "rev-parse", "HEAD")
				if want := fmt.Sprintf("%s is reset from %s to %s; its 1 commit after %s leaves the branch (git's reflog keeps it; "+
					"it is not in the saved diff)", task.Branch, short(moved), short(head), short(head)); tc.moved &&
					!strings.Contains(qs[0].Options[0].Means, want) {
					t.Fatalf("Restore says %q; want %q", qs[0].Options[0].Means, want)
				}
				if tc.moved == (moved == head) || (tc.name == "a moved head with a tracked change and a new file" &&
					(r.read("README") != "a reviewer's mutant\n" || !r.inCopy("spec/probe_spec.rb"))) {
					t.Fatal("the factory changed what the round left before your answer")
				}
				switch answer {
				case optionRestoreCopy, optionKeepChange:
					r.answerLeft(task.ID, answer)
				default:
					tc.undo(r, task.Branch, head)
					r.letGoOn(task.ID)
				}
				r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" })
				got = r.reload(task.ID)
				branch := r.gitOut(r.copyDir, "symbolic-ref", "--short", "HEAD")
				now := r.git(r.copyDir, "rev-parse", "HEAD")
				switch {
				case answer == optionKeepChange && tc.moved: // the new head is the task's: its checks run again
					if got.Stage == "local_review" || got.Stage == "pr_body" || branch != task.Branch || now != moved {
						t.Fatalf("task at %s on %s at %s; want the checks again at the moved head %s", got.Stage, branch, now, moved)
					}
				default:
					if got.Stage != "pr_body" || len(r.rv.calls()) != 1 || branch != task.Branch || now != head {
						t.Fatalf("task at %s waits %q after %d rounds, copy on %q at %s; want pr_body on the one round's result, "+
							"on %s at %s", got.Stage, got.WaitsWhat, len(r.rv.calls()), branch, now, task.Branch, head)
					}
				}
				if tc.name == "a moved head with a tracked change and a new file" {
					kept := answer == optionKeepChange
					if (r.read("README") == "a reviewer's mutant\n") != kept || r.inCopy("spec/probe_spec.rb") != kept {
						t.Fatalf("README %q, probe %v after %q", r.read("README"), r.inCopy("spec/probe_spec.rb"), answer)
					}
					if answer == optionRestoreCopy && (!strings.Contains(r.readTask(task.ID, "review-1-left.diff"), "+a reviewer's mutant") ||
						r.readTask(task.ID, "review-1-left/spec/probe_spec.rb") != "a reviewer's probe\n") {
						t.Fatal("the restore did not save the change and the new file first")
					}
				}
				if answer == optionRestoreCopy && tc.moved && !slices.ContainsFunc(r.events(task.ID, "review"), func(e registry.Event) bool {
					return strings.Contains(e.Text, short(moved))
				}) {
					t.Fatalf("no review event names the head %s the restore moved the branch back from", short(moved))
				}
			})
		}
	}
}

// A round that went without a reviewer's report runs once more; a second
// round without it is review unavailable, never clean (Review Focus 1),
// and an error's run again leaves the lost report its own one more round.
func TestALocalRoundTwiceWithoutAReviewerIsUnavailable(t *testing.T) {
	lost := func(c evalrun.LocalCase) evalrun.LocalResult {
		res := result()(c)
		res.Missing = []string{"claude-review"}
		return res
	}
	for name, answers := range map[string][]func(evalrun.LocalCase) evalrun.LocalResult{
		"twice lost":                {lost, lost},
		"an error, then lost twice": {outcome(pipeline.OutcomeError), lost, lost},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReviewRig(t, answers...)
			task, _ := r.reviewTask("PR-12381")
			r.run(10, func() bool { return !isLive(r.reload(task.ID)) })
			got := r.reload(task.ID)
			if got.State != registry.TaskNeedsYou || got.WaitsWhat != "review unavailable (no report from claude-review)" ||
				len(r.rv.calls()) != len(answers) {
				t.Fatalf("task %s waits %q after %d rounds", got.State, got.WaitsWhat, len(r.rv.calls()))
			}
			if cs := r.cycles(task.ID); len(cs) != 1 || cs[0].Exit != registry.ExitUnavailable {
				t.Fatalf("cycles %+v", cs)
			}
		})
	}
}

// The loop's summary names every reviewer a round of it went without, so
// the ready card and the PR body can say which review never ran.
func TestASummaryNamesTheReviewersARoundWentWithout(t *testing.T) {
	blockedLost := func(c evalrun.LocalCase) evalrun.LocalResult {
		res := result(refundFinding)(c)
		res.Missing = []string{"codex-review"}
		return res
	}
	r := newReviewRig(t, blockedLost, result())
	task, _ := r.reviewTask("PR-12382")
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	r.writerFixes(task, 1, fixedCredit(1), nil)
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Exit != registry.ExitClean || !slices.Equal(sum.Missing, []string{"codex-review"}) {
		t.Fatalf("summary %+v, want codex-review among the reviewers a round went without", sum)
	}
}

// What the loop needs you for is never lost: when the daemon stops between
// ending the loop's cycle (unavailable, or at the cap) and moving the task
// to you, the next start finds the cycle ended with no word to you since,
// and the task needs you with the same reason; no fresh loop starts. Your
// "go on" after it starts one.
func TestTheLoopsWaitOnYouSurvivesACrashBeforeIt(t *testing.T) {
	r := newReviewRig(t, outcome(pipeline.OutcomeNeedsAttention), result())
	r.e.beforeStop = func() error { return errors.New("the daemon stops here") }
	task, _ := r.reviewTask("PR-12383")
	r.run(4, func() bool { return len(r.cycles(task.ID)) == 1 && !r.cycles(task.ID)[0].Ended.IsZero() })
	if got := r.reload(task.ID); !isLive(got) {
		t.Fatalf("the crash did not happen before the move to you: task %s waits %q", got.State, got.WaitsWhat)
	}
	r.restart()
	r.run(4, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWhat != "review unavailable (needs_attention)" || len(r.rv.calls()) != 1 {
		t.Fatalf("task %s waits %q after %d rounds; want you, after the one round", got.State, got.WaitsWhat, len(r.rv.calls()))
	}
	r.letGoOn(task.ID)
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 2 {
		t.Fatalf("after your go on: task at %s after %d rounds; want a fresh loop", got.Stage, len(r.rv.calls()))
	}
}

// The round's own error reaches you, redacted and clipped, with the
// outcome; a configuration fault (no gh identity to run as) is not run
// again and names what to configure.
func TestTheRoundsErrorReachesYou(t *testing.T) {
	failing := func(c evalrun.LocalCase) evalrun.LocalResult {
		res := outcome(pipeline.OutcomeError)(c)
		res.Error = "the round's app could not start: herdr answered nothing"
		return res
	}
	r := newReviewRig(t, failing)
	task, _ := r.reviewTask("PR-12384")
	r.run(8, func() bool { return !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou ||
		got.WaitsWhat != "review unavailable (error): the round's app could not start: herdr answered nothing" {
		t.Fatalf("task %s waits %q", got.State, got.WaitsWhat)
	}

	r = newReviewRig(t, result())
	r.rv.errAt = map[int]error{1: &evalrun.ConfigError{Key: "[[identity]] kind = \"gh\"", Why: "a local round runs as a gh identity, and the config has none"}}
	task, _ = r.reviewTask("PR-12385")
	r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "[[identity]] kind = \"gh\"") || len(r.rv.calls()) != 1 {
		t.Fatalf("task %s waits %q after %d rounds; want you at once, naming the key", got.State, got.WaitsWhat, len(r.rv.calls()))
	}
}

// A finding the judge posted at a blocking priority while its counts say
// nothing blocks is not lost when the loop ends clean: the summary keeps it
// among the leftovers.
func TestACleanRoundKeepsAPostedFindingItsCountsLeaveOut(t *testing.T) {
	r := newReviewRig(t, func(c evalrun.LocalCase) evalrun.LocalResult {
		return evalrun.LocalResult{Outcome: pipeline.OutcomeDryRun, Verdict: "clean", Findings: []evalrun.LocalFinding{refundFinding}}
	})
	task, _ := r.reviewTask("PR-12386")
	r.run(5, func() bool { return r.reload(task.ID).Stage != "local_review" })
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Exit != registry.ExitClean || len(sum.Leftovers) != 1 || sum.Leftovers[0].Title != refundFinding.Title {
		t.Fatalf("summary %+v, want the posted P1 among the leftovers", sum)
	}
}

// A local round runs with the copy's own environment, the one the
// factory's agents get in it (its WT_BRANCH and env), never one of a
// synthetic PR's worktree: the reviewers' specs use the copy's databases.
func TestALocalRoundRunsInTheCopysEnvironment(t *testing.T) {
	r := newReviewRig(t, result())
	r.cp.Env = map[string]string{"WT_BRANCH": "factory1", "REDIS_DB_OFFSET": "16", "PORT": "3001"}
	r.copies.claimed = r.cp
	task, _ := r.reviewTask("PR-12387")
	r.run(5, func() bool { return len(r.rv.calls()) == 1 })
	calls := r.rv.calls()
	if len(calls) != 1 || !maps.Equal(calls[0].Env, r.cp.Env) {
		t.Fatalf("the round's env %v, want the copy's %v", calls[0].Env, r.cp.Env)
	}
	_ = task
}

// fileIn reports whether dir holds name.
func fileIn(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return !errors.Is(err, os.ErrNotExist)
}

// reviewSession is magnum's review session as the local review sees it
// (Deps.ReviewHerdrUp, ReviewReveal): down, when set, is its error; each
// open is recorded at the rig's clock.
type reviewSession struct {
	mu     sync.Mutex
	down   error
	opened []time.Time
	pings  int
}

func (s *reviewSession) set(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = err
}

func (s *reviewSession) opens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.opened)
}

// asked is how many times the session was pinged.
func (s *reviewSession) asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pings
}

// wire puts the session in the rig's deps, the engine's too.
func (s *reviewSession) wire(r *reviewRig) {
	for _, d := range []*Deps{&r.deps, &r.e.d} {
		d.ReviewHerdrUp = func(context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.pings++
			return s.down
		}
		d.ReviewReveal = func(context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.opened = append(s.opened, r.clock.Now())
			return nil
		}
	}
}

// The local review opens its panes in magnum's review session ([herdr]
// review_session): while that does not answer, no round starts and the
// task waits on it, never ending its cycle unavailable; while no server
// runs there the factory opens it, at most twice an hour. A round that
// ended while the session went away runs again once it answers.
func TestALocalReviewWaitsWhileTheReviewSessionDoesNotAnswer(t *testing.T) {
	noServer := fmt.Errorf("herdr ping: %w: %w", herdr.ErrUnavailable, syscall.ENOENT)
	r := newReviewRig(t, result())
	r.cfg.Herdr.ReviewSession = "reviews"
	s := &reviewSession{down: noServer}
	s.wire(r)
	task, _ := r.reviewTask("PR-12390")
	r.run(4, nil)
	got := r.reload(task.ID)
	if len(r.rv.calls()) != 0 || got.State != registry.TaskActive || got.WaitsWho != HoldHerdr || !strings.Contains(got.WaitsWhat, "reviews") {
		t.Fatalf("%d rounds; task %s waits %q: %q", len(r.rv.calls()), got.State, got.WaitsWho, got.WaitsWhat)
	}
	if n := s.opens(); n != 1 {
		t.Fatalf("the review session was opened %d times in a minute, want once", n)
	}
	r.clock.Add(herdrRevealGap)
	r.run(2, nil)
	if n := s.opens(); n != 2 {
		t.Fatalf("the review session was opened %d times after %s, want twice", n, herdrRevealGap)
	}
	s.set(fmt.Errorf("herdr ping: %w", herdr.ErrTimeout)) // a server that runs: its client is open already
	r.clock.Add(herdrRevealGap)
	r.run(2, nil)
	if n := s.opens(); n != 2 || len(r.rv.calls()) != 0 {
		t.Fatalf("%d opens and %d rounds while the session runs but does not answer", n, len(r.rv.calls()))
	}
	s.set(nil)
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); len(r.rv.calls()) != 1 || got.Stage != "pr_body" {
		t.Fatalf("%d rounds; task at %s once the session answered", len(r.rv.calls()), got.Stage)
	}

	// The session goes away under a round, which ends needing attention: it
	// waits and runs again, rather than ending its cycle unavailable.
	r = newReviewRig(t, func(c evalrun.LocalCase) evalrun.LocalResult {
		s.set(noServer)
		return outcome(pipeline.OutcomeNeedsAttention)(c)
	}, result())
	s = &reviewSession{}
	s.wire(r)
	task, _ = r.reviewTask("PR-12391")
	r.run(6, func() bool { return len(r.rv.calls()) == 1 && r.reload(task.ID).WaitsWho == HoldHerdr })
	if got := r.reload(task.ID); len(r.rv.calls()) != 1 || got.State != registry.TaskActive || got.WaitsWho != HoldHerdr {
		t.Fatalf("%d rounds; task %s waits %q: %q", len(r.rv.calls()), got.State, got.WaitsWho, got.WaitsWhat)
	}
	s.set(nil)
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); len(r.rv.calls()) != 2 || got.Stage != "pr_body" {
		t.Fatalf("%d rounds; task %s at %s waits %q", len(r.rv.calls()), got.State, got.Stage, got.WaitsWhat)
	}
	if cs := r.cycles(task.ID); len(cs) != 1 || cs[0].Exit == registry.ExitUnavailable {
		t.Fatalf("cycles %+v, want one that did not end unavailable", cs)
	}
}

// magnum's review session is asked once a tick, however many tasks wait on
// it at their local review: a server that hangs costs a tick one ping's
// timeout, never one for each task.
func TestTheReviewSessionIsAskedOnceATick(t *testing.T) {
	r := newReviewRig(t, result())
	r.cfg.Herdr.ReviewSession = "reviews"
	s := &reviewSession{down: fmt.Errorf("herdr ping: %w", herdr.ErrTimeout)}
	s.wire(r)
	var tasks []registry.Task
	for _, key := range []string{"PR-12392", "PR-12393", "PR-12394"} {
		task, _ := r.reviewTask(key)
		tasks = append(tasks, task)
	}
	waiting := func() bool {
		return !slices.ContainsFunc(tasks, func(task registry.Task) bool { return r.reload(task.ID).WaitsWho != HoldHerdr })
	}
	r.run(6, waiting)
	if !waiting() {
		t.Fatal("the tasks do not all wait on magnum's review session")
	}
	before := s.asked()
	r.run(1, nil)
	if n := s.asked() - before; n != 1 {
		t.Fatalf("one tick asked the review session %d times for %d tasks, want once", n, len(tasks))
	}
}
