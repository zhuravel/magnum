package factory

// The local magnum review loop (spec 6.5): before any PR exists, magnum's
// full review runs on the task's change in its copy, as a blind dry run of
// the repository's watch's review roles (evalrun.RunLocal, through
// Deps.Review), round after round, at most [factory.local_review] cycles of
// them. A round runs off the loop at the lowest priority: it waits while
// magnum has a real round in flight or due, while Codex is past its soft
// cap, while an agent or you are at work in the copy and for a heavy slot.
// The factory reads its result itself (Blocks): a P0 to P2, new or still
// open, or a blocking verdict blocks; a round that did not finish is review
// unavailable, and the task needs you, never a clean pass. A blocking round
// hands its findings to the writer (review-<n>.json) for one fix turn of
// the coding stage's roles, whose end fact is the coding stage's own (the
// factory's proof and precheck at the new head, provenHead) and the
// review's (the writer's reply, review-reply-<n>.json, and a commit since
// the round unless it declined every finding with its risk); then the next
// round runs. A finding is followed by its fingerprint: one that comes back
// twice after its fix is a doubt, and when only doubts block, the loop ends
// on the oscillation and the stage passes; one the writer declined with
// its risk blocks no more; a product question joins the task's question
// batch. Every round is a cycle of the registry with its findings, and
// what a round left lives in the task's directory, so a restart at any
// point runs a round again only when it left no result.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/usage"
)

// ReviewFixPrompt is the prompt of the writer's fix turn in the local
// review: the coding stage's roles get it in place of their own prompt.
const ReviewFixPrompt = "factory-review-fix.md"

// localReviewer is the LocalReviewer the factory daemon wires:
// evalrun.RunLocal over d, whose config is the review candidate's
// (WriteReviewCandidate, config.WithCandidate).
type localReviewer struct{ d evalrun.Deps }

// NewLocalReviewer is the local reviewer that runs each round with
// evalrun.RunLocal over d.
func NewLocalReviewer(d evalrun.Deps) LocalReviewer { return localReviewer{d: d} }

// Round runs local round c (evalrun.RunLocal).
func (r localReviewer) Round(ctx context.Context, c evalrun.LocalCase) (evalrun.LocalResult, error) {
	return evalrun.RunLocal(ctx, r.d, c)
}

// The files of the local review in the task's directory: a round's result
// as the round read it (local-review-<n>.json, n the cycle's number; a
// round set aside to run again keeps its file as
// local-review-<n>-<why>-<k>.json), the findings it hands the writer
// (review-<n>.json), the writer's reply (review-reply-<n>.json), the risks
// of the findings the writer declined (review-declined.json) and the
// loop's end for the ready card and the PR body (review-summary.json).
const (
	reviewSummaryFile  = "review-summary.json"
	reviewDeclinedFile = "review-declined.json"
)

func reviewRoundFile(n int) string { return fmt.Sprintf("local-review-%d.json", n) }
func reviewFile(n int) string      { return fmt.Sprintf("review-%d.json", n) }
func reviewReplyFile(n int) string { return fmt.Sprintf("review-reply-%d.json", n) }

// reviewFileRe is a round's review-<n>.json.
var reviewFileRe = regexp.MustCompile(`^review-(\d+)\.json$`)

// newestReview is the number of the newest review-<n>.json in dir, 0 for
// none.
func newestReview(dir string) int {
	names, _ := filepath.Glob(filepath.Join(dir, "review-*.json"))
	n := 0
	for _, p := range names {
		if m := reviewFileRe.FindStringSubmatch(filepath.Base(p)); m != nil {
			if k, err := strconv.Atoi(m[1]); err == nil {
				n = max(n, k)
			}
		}
	}
	return n
}

// The waits of a local round, besides the agents' and the checks': magnum
// has a round in flight or due, and the task's round runs.
const (
	waitMagnum = "magnum"
	waitReview = "review"
)

// heavyReview is a local round's use of a heavy slot.
const heavyReview = "local review"

// localRoundCap ends a round that runs this long (a hang): a round takes
// 39 minutes at the median, its roles' own timeouts end within the hour.
const localRoundCap = 3 * time.Hour

// ReviewItem is one finding of the local review as the factory hands it on:
// its id in the round's file (F1, F2, ...), its fingerprint, severity,
// title, path, line and body, whether the judge marked it a product
// question, and, for one the writer declined, the risk it named.
type ReviewItem struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	Severity    string `json:"severity,omitempty"`
	Title       string `json:"title"`
	Path        string `json:"path,omitempty"`
	Line        int    `json:"line,omitempty"`
	Body        string `json:"body,omitempty"`
	Question    bool   `json:"question,omitempty"`
	Risk        string `json:"risk,omitempty"`
}

// ReviewFile is review-<n>.json, what a blocking round hands the writer:
// the cycle's number, the head it reviewed, how many attempts the review
// stage had when it was written (the fix turn is the first attempt after
// them) and the findings to fix, the questions among them.
type ReviewFile struct {
	Round          int          `json:"round"`
	Head           string       `json:"head"`
	AttemptsBefore int          `json:"attempts_before"`
	Findings       []ReviewItem `json:"findings"`
}

// ReviewReply is review-reply-<n>.json, the writer's: per finding, fixed,
// or declined with the risk of its suggested change.
type ReviewReply struct {
	Replies []struct {
		ID     string `json:"id"`
		Action string `json:"action"`
		Risk   string `json:"risk"`
	} `json:"replies"`
}

// The actions of a reply.
const (
	replyFixed    = "fixed"
	replyDeclined = "declined"
)

// declinedWithRisk are the ids of rep's findings declined with a risk, and
// that risk.
func (rep ReviewReply) declinedWithRisk() map[string]string {
	out := map[string]string{}
	for _, r := range rep.Replies {
		if r.Action == replyDeclined && strings.TrimSpace(r.Risk) != "" {
			out[r.ID] = strings.TrimSpace(r.Risk)
		}
	}
	return out
}

// ReviewSummary is review-summary.json: how the local review ended (exit
// clean or oscillation), after how many rounds, at which head, and what it
// left that does not block, for the ready card and the PR body's "Known":
// the leftovers (P3s and simplifications of its last round), the findings
// the writer declined with their risks, and the doubts (findings that came
// back twice after their fix).
type ReviewSummary struct {
	Exit      string       `json:"exit"`
	Rounds    int          `json:"rounds"`
	Head      string       `json:"head"`
	Leftovers []ReviewItem `json:"leftovers"`
	Declined  []ReviewItem `json:"declined"`
	Doubts    []ReviewItem `json:"doubts"`
}

// localRound is a round that runs off the loop: its cycle (id and number),
// the loop's round number, done closed when it ended, cancel to end it,
// stopped when the factory ended it (the task stopped being live: nothing
// of it is read), and what it ended with (written before done closes): a
// result the round's goroutine wrote to the task's directory, or drained,
// the daemon stopping it, or err, its result that could not be written.
type localRound struct {
	cycle    int64
	n, round int
	done     chan struct{}
	cancel   context.CancelFunc
	stopped  atomic.Bool
	drained  bool
	err      error
}

func (r *localRound) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// roundOf is the round of task id off the loop, nil for none.
func (e *Engine) roundOf(id int64) *localRound {
	e.reviewMu.Lock()
	defer e.reviewMu.Unlock()
	return e.reviews[id]
}

// forgetRound drops r, the round of task id.
func (e *Engine) forgetRound(id int64, r *localRound) {
	e.reviewMu.Lock()
	defer e.reviewMu.Unlock()
	if e.reviews[id] == r {
		delete(e.reviews, id)
	}
}

// sweepRounds ends the rounds of the tasks that are not live (a stop, a
// NEEDS YOU, gone): nothing of them is read, the round's cycle stays open
// and runs again once the task is back at work.
func (e *Engine) sweepRounds(tasks []registry.Task) {
	live := map[int64]bool{}
	for _, t := range tasks {
		if isLive(t) {
			live[t.ID] = true
		}
	}
	e.reviewMu.Lock()
	defer e.reviewMu.Unlock()
	for id, r := range e.reviews {
		if live[id] {
			continue
		}
		if !r.ended() {
			r.stopped.Store(true)
			r.cancel()
			continue
		}
		delete(e.reviews, id)
	}
}

// runReview runs one pass of review stage st of task t (the engine's runner
// for config.FactoryStageReview); done reports that the loop ended clean or
// on an oscillation. The newest cycle decides: one that ended the stage
// passes it again (its move did not land); an open one goes on (goOnCycle);
// else the next round of the loop starts, once it may (startRound). A loop
// is the cycles after the newest that ended one with you (cap, unavailable,
// operator): your "go on" after it starts a loop afresh. A build without the
// local reviewer, agents or git leaves the task where it is.
func (e *Engine) runReview(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
	if e.d.Review == nil || e.d.Runtime == nil || e.d.Git == nil {
		return false, nil
	}
	cycles, err := e.reg.Cycles(ctx, t.ID)
	if err != nil {
		return false, err
	}
	var last *registry.Cycle
	if len(cycles) > 0 {
		last = &cycles[len(cycles)-1]
	}
	if last != nil && !last.Ended.IsZero() && (last.Exit == registry.ExitClean || last.Exit == registry.ExitOscillation) {
		return true, nil
	}
	loop := loopCycles(cycles)
	c, err := e.claimCopy(ctx, t)
	if err != nil {
		return false, err
	}
	if last != nil && last.Ended.IsZero() {
		return e.goOnCycle(ctx, t, st, c, *last, len(loop))
	}
	return false, e.startRound(ctx, t, c, nil, len(loop)+1)
}

// loopCycles are the cycles of the current loop: those after the newest
// whose exit ended a loop with you.
func loopCycles(cs []registry.Cycle) []registry.Cycle {
	for i, c := range slices.Backward(cs) {
		switch c.Exit {
		case registry.ExitCap, registry.ExitUnavailable, registry.ExitOperator:
			return cs[i+1:]
		}
	}
	return cs
}

// startRound starts round `round` of the loop in copy c, in cycle cy (a
// round that runs again: a restart, a hang, a refusal) or in a new one,
// once it may: not while the daemon drains, and after the waits of the
// lowest priority (reviewWait) and a heavy slot. Its roles are the watch's
// (reviewRoles), its case the change at the task's proven head
// (localCase); the round runs off the loop (launchRound).
func (e *Engine) startRound(ctx context.Context, t registry.Task, c Copy, cy *registry.Cycle, round int) error {
	if e.draining.Load() {
		return nil
	}
	if r := e.roundOf(t.ID); r != nil {
		if !r.ended() {
			return nil
		}
		e.forgetRound(t.ID, r) // an ended round of another cycle: read already
	}
	w := e.cfg.WatchFor(t.Repo)
	if w == nil {
		return e.stageFailed(ctx, t, needsYou{"no [[watch]] covers " + t.Repo + ": the local review runs its review roles"})
	}
	roles, err := e.reviewRoles(t, w, round)
	if err != nil {
		return e.stageFailed(ctx, t, err)
	}
	who, what, err := e.reviewWait(ctx, t, roles)
	if err != nil {
		return err
	}
	if who != "" {
		e.waitOn(ctx, t, who, what)
		return heldError{who: who, what: what}
	}
	repo, err := e.factoryRepo(t)
	if err != nil {
		return err
	}
	bh, err := e.branchHead(ctx, t, c, repo)
	switch {
	case err != nil:
		return err
	case bh.Problem != "":
		return e.stageFailed(ctx, t, needsYou{"the local review cannot start: " + bh.Problem})
	case t.HeadSHA != "" && bh.Head != t.HeadSHA:
		return e.stageFailed(ctx, t, needsYou{fmt.Sprintf("the branch moved to %s since the factory's checks passed at %s: "+
			"the local review reviews a checked head only", short(bh.Head), short(t.HeadSHA))})
	}
	lc := e.localCase(t, *w, c, bh, repo, roles, round)
	release, ok := e.heavy(ctx, t, heavyReview)
	if !ok {
		return nil // the task waits "heavy: …"
	}
	if cy == nil {
		begun, err := e.reg.BeginCycle(ctx, t.ID, bh.Head, skillHash(e.d.ReviewCandidate))
		if err != nil {
			release()
			return err
		}
		cy = &begun
	}
	e.launchRound(ctx, t, *cy, lc, release)
	return nil
}

// reviewWait is what a local round waits for before it starts, "" for
// nothing: a hold of the machine (mayLaunch), an agent or you at work in
// the task's copy, the factory's checks of the head, magnum's real rounds
// (Magnum.Busy: in flight or due; its registry that cannot be read is a
// wait too, with why), and, when the round runs a Codex role, Codex at or
// past [usage] codex_soft. who and what are the task's wait.
func (e *Engine) reviewWait(ctx context.Context, t registry.Task, roles []string) (who, what string, err error) {
	if who, what, ok := e.mayLaunch(); !ok {
		return who, what, nil
	}
	role, you, err := e.d.Runtime.AtWork(ctx, t)
	switch {
	case err != nil:
		return "", "", err
	case you:
		return waitAgent, "you're in a pane of " + cmp.Or(t.Copy, "the copy") + "; the local review waits until you leave it", nil
	case role != "":
		return waitAgent, "the " + role + " agent works in " + cmp.Or(t.Copy, "the copy") + "; the local review waits for its turn to end", nil
	}
	if e.checkRuns(t.ID) {
		return waitPrecheck, cmp.Or(e.checkNote(t.ID), waitPrecheck+": runs"), nil
	}
	if e.d.Magnum != nil {
		busy, why, err := e.d.Magnum.Busy(ctx)
		switch {
		case err != nil:
			return waitMagnum, textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 300), nil
		case busy:
			return waitMagnum, why + "; the local review yields to it", nil
		}
	}
	if e.runsCodex(roles) {
		if what := e.codexSoft(ctx); what != "" {
			return waitUsage, what, nil
		}
	}
	return "", "", nil
}

// runsCodex reports whether a round of roles runs a Codex role.
func (e *Engine) runsCodex(roles []string) bool {
	return slices.ContainsFunc(roles, func(name string) bool {
		r, ok := e.cfg.RoleByNameOrAlias(nil, name)
		return ok && r.AgentKind() == config.KindCodex
	})
}

// codexSoft says that Codex is at or past [usage] codex_soft, "" when it is
// not, the cap is off or no gauge reads (the round's own health checks stop
// at a limit then).
func (e *Engine) codexSoft(ctx context.Context) string {
	u := e.cfg.Usage
	if u.CodexSoft <= 0 && u.CodexHard <= 0 {
		return ""
	}
	read := e.d.Usage
	if read == nil {
		home := u.CodexHome
		read = func(ctx context.Context, now time.Time) (usage.Snapshot, error) { return usage.Codex(ctx, home, now) }
	}
	snap, err := read(ctx, e.now())
	if err != nil || usage.Decide(snap, u.CodexSoft, u.CodexHard) == usage.OK {
		return ""
	}
	return fmt.Sprintf("codex at %.0f%%, at or past [usage] codex_soft %.0f%%: the local review yields to magnum's reviews", snap.Used(), u.CodexSoft)
}

// reviewRoles are the roles round `round` of task t's local review runs:
// the watch's review roles, without [factory.local_review] later_without
// after round 1. On a task Codex never works on again, no Codex role runs,
// and magnum's judge_fallback takes the Codex judge's place; a judge that
// cannot be replaced so is a needsYou.
func (e *Engine) reviewRoles(t registry.Task, w *config.Watch, round int) ([]string, error) {
	var out []string
	for _, r := range e.cfg.RolesFor(w) {
		if round > 1 && slices.ContainsFunc(e.cfg.Factory.LocalReview.LaterWithout, r.Matches) {
			continue
		}
		if t.CodexNever && r.AgentKind() == config.KindCodex {
			if !r.Judge {
				continue // Codex never sees the task again
			}
			fb, ok := e.cfg.JudgeFallbackFor(w)
			if !ok || !fb.IsAgent() || fb.AgentKind() == config.KindCodex {
				return nil, needsYou{"Codex never works on " + TaskKey(t) + " again and the review's judge " + r.Name +
					" runs on Codex: set [pipeline] judge_fallback (or the watch's) to a judge of another kind"}
			}
			r = fb
		}
		if !slices.Contains(out, r.Name) {
			out = append(out, r.Name)
		}
	}
	return out, nil
}

// localCase is round `round` of task t's change in copy c: the branch's
// head and its base (bh), the title with the Jira key, the plan as the body
// (report.md), the ticket (ticket.md, else the operator's request), the
// watch and the round's roles.
func (e *Engine) localCase(t registry.Task, w config.Watch, c Copy, bh branchHead, repo config.FactoryRepo, roles []string, round int) evalrun.LocalCase {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return ""
		}
		return string(b)
	}
	title := strings.TrimSpace(t.Title)
	if t.JiraKey != "" && !strings.HasPrefix(title, t.JiraKey) {
		title = strings.TrimSpace(t.JiraKey + " " + title)
	}
	return evalrun.LocalCase{Checkout: c.Path, BaseSHA: bh.Base, HeadSHA: bh.Head, Branch: t.Branch, Title: title, Body: read(reportFile),
		TicketMD: cmp.Or(read(ticketFile), read("prompt.md")), Watch: w, Roles: roles, Round: round, TaskID: t.ID, Repo: t.Repo, Base: baseOf(repo)}
}

// launchRound runs local round lc of cycle cy off the loop, holding the
// heavy slot (release frees it), on the engine's context and within
// roundCap, while the task waits saying the round runs. Its result goes to
// the task's directory as local-review-<n>.json, however it ended: a round
// over its cap as a timeout, one that could not run as an error, a panic
// too. A drain, or the factory stopping it (sweepRounds), writes nothing:
// the round runs again in its cycle.
func (e *Engine) launchRound(ctx context.Context, t registry.Task, cy registry.Cycle, lc evalrun.LocalCase, release func()) {
	rctx, cancel := context.WithTimeout(e.bgCtx, e.roundCap)
	r := &localRound{cycle: cy.ID, n: cy.N, round: lc.Round, done: make(chan struct{}), cancel: cancel}
	e.reviewMu.Lock()
	e.reviews[t.ID] = r
	e.reviewMu.Unlock()
	e.waitOn(ctx, t, waitReview, fmt.Sprintf("round %d of the local review runs", lc.Round))
	e.event(ctx, t.ID, "review", registry.PhaseBegin, fmt.Sprintf("round %d (cycle %d) at %s: %s", lc.Round, cy.N, short(lc.HeadSHA),
		strings.Join(lc.Roles, ", ")))
	path := filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), reviewRoundFile(cy.N))
	e.bg.Go(func() {
		defer close(r.done)
		defer cancel()
		defer release()
		var res evalrun.LocalResult
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					e.log.Error("panic in a local review round; the daemon goes on", "task", t.ID, "panic", fmt.Sprint(p),
						"stack", execx.Redact(string(debug.Stack())))
					res, err = evalrun.LocalResult{}, fmt.Errorf("the round panicked: %v", p)
				}
			}()
			res, err = e.d.Review.Round(rctx, lc)
		}()
		bg := context.Background()
		switch {
		case e.bgCtx.Err() != nil || r.stopped.Load():
			r.drained = true
			e.event(bg, t.ID, "review", registry.PhaseFail, fmt.Sprintf("round %d (cycle %d) stopped before its end: it runs again", lc.Round, cy.N))
			return
		case rctx.Err() != nil && (err != nil || res.Outcome == "" || res.Outcome == pipeline.OutcomeStopped):
			res = evalrun.LocalResult{Outcome: pipeline.OutcomeTimeout, Error: "over its cap of " + e.roundCap.String()}
		case err != nil:
			res = evalrun.LocalResult{Outcome: pipeline.OutcomeError, Error: err.Error()}
		}
		res.Round = lc.Round
		b, merr := json.MarshalIndent(res, "", "  ")
		if merr == nil {
			merr = fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
		}
		if merr != nil {
			r.err = fmt.Errorf("record round %d of the local review: %w", lc.Round, merr)
			e.event(bg, t.ID, "review", registry.PhaseFail, r.err.Error())
			return
		}
		e.event(bg, t.ID, "review", registry.PhaseOK, fmt.Sprintf("round %d (cycle %d) ended %s", lc.Round, cy.N, res.Outcome))
	})
}

// goOnCycle goes on with open cycle cy, round `round` of the loop: its
// round while it runs (the task's wait says so), then its result as the
// round left it (local-review-<n>.json; none, after a restart or a round
// set aside, runs the round again in the cycle), read once (decide) and
// then its fix (fixPhase).
func (e *Engine) goOnCycle(ctx context.Context, t registry.Task, st config.FactoryStage, c Copy, cy registry.Cycle, round int) (bool, error) {
	if r := e.roundOf(t.ID); r != nil {
		if r.cycle == cy.ID && !r.ended() {
			return false, nil
		}
		e.forgetRound(t.ID, r)
		if r.cycle == cy.ID && r.err != nil {
			return false, r.err // its result was lost: the round runs again on the next pass
		}
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	res, ok, err := readRoundResult(dir, cy.N)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, e.startRound(ctx, t, c, &cy, round)
	}
	if t.WaitsWho == waitReview {
		e.waitOn(ctx, t, "", "")
		t.WaitsWho, t.WaitsWhat = "", ""
	}
	if rf, ok, err := readReviewFile(dir, cy.N); err != nil {
		return false, err
	} else if ok {
		return e.fixPhase(ctx, t, st, cy, rf)
	}
	return e.decide(ctx, t, st, cy, round, res)
}

// readRoundResult reads the result round n left in dir; ok is false when
// there is none. One that does not parse is set aside, so the round runs
// again.
func readRoundResult(dir string, n int) (evalrun.LocalResult, bool, error) {
	path := filepath.Join(dir, reviewRoundFile(n))
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return evalrun.LocalResult{}, false, nil
	}
	if err != nil {
		return evalrun.LocalResult{}, false, err
	}
	var res evalrun.LocalResult
	if err := json.Unmarshal(b, &res); err != nil {
		if err := setAside(dir, n, "unread"); err != nil {
			return res, false, err
		}
		return res, false, nil
	}
	return res, true, nil
}

// readReviewFile reads review-<n>.json in dir; ok is false when there is
// none.
func readReviewFile(dir string, n int) (ReviewFile, bool, error) {
	var rf ReviewFile
	b, err := os.ReadFile(filepath.Join(dir, reviewFile(n)))
	if errors.Is(err, fs.ErrNotExist) {
		return rf, false, nil
	}
	if err != nil {
		return rf, false, err
	}
	if err := json.Unmarshal(b, &rf); err != nil {
		return rf, false, fmt.Errorf("%s: %w", reviewFile(n), err)
	}
	return rf, true, nil
}

// setAside keeps round n's result in dir under another name
// (local-review-<n>-<why>-<k>.json), so the round runs again in its cycle.
func setAside(dir string, n int, why string) error {
	for k := 1; ; k++ {
		to := filepath.Join(dir, fmt.Sprintf("local-review-%d-%s-%d.json", n, why, k))
		if fsx.Exists(to) {
			continue
		}
		return os.Rename(filepath.Join(dir, reviewRoundFile(n)), to)
	}
}

// roundTries counts round n's results set aside to run it again (a round
// that did not finish, or went without a reviewer).
func roundTries(dir string, n int) int {
	names, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("local-review-%d-retry-*.json", n)))
	return len(names)
}

// decide reads cycle cy's round, round `round` of the loop (res):
//   - a Codex refusal marks the task Codex-never for good, and the round
//     runs again without Codex (reviewRoles), never on it;
//   - a round that did not finish (Blocks: unavailable) runs once more when
//     it ended in an error or a timeout, then ends its cycle unavailable,
//     and the task needs you with why: never read as clean;
//   - a clean round that went without a reviewer's report runs once more;
//   - else its findings are recorded (seeFindings) and what blocks decides:
//     nothing (or only findings the writer declined with their risk) ends
//     the loop clean, only doubts on an oscillation (the stage passes for
//     both, with the summary for the ready card), anything else at the cap
//     needs you, and before it the writer gets the findings (fixPhase), the
//     product questions among them asked first.
func (e *Engine) decide(ctx context.Context, t registry.Task, st config.FactoryStage, cy registry.Cycle, round int, res evalrun.LocalResult) (bool, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	again := func(why, said string) (bool, error) {
		if err := setAside(dir, cy.N, why); err != nil {
			return false, err
		}
		e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("round %d (cycle %d) %s: it runs once more", round, cy.N, said))
		return false, nil
	}
	if res.Outcome == pipeline.OutcomeRefused && res.Refused == config.KindCodex {
		if err := e.markCodexNever(ctx, t, fmt.Sprintf("Codex refused round %d of the local review", round)); err != nil {
			return false, err
		}
		return again("refused", "was refused by Codex; Codex never works on "+TaskKey(t)+" again")
	}
	blocks, unavailable, why := Blocks(res)
	tries := roundTries(dir, cy.N)
	switch {
	case unavailable && tries == 0 && (res.Outcome == pipeline.OutcomeError || res.Outcome == pipeline.OutcomeTimeout):
		return again("retry", "ended "+res.Outcome)
	case unavailable:
		if err := e.endCycle(ctx, cy, cy.HeadBefore, registry.ExitUnavailable); err != nil {
			return false, err
		}
		e.toOperator(ctx, t, why, "the local review did not finish")
		return false, nil
	case !blocks && len(res.Missing) > 0 && tries == 0:
		return again("retry", "went without "+strings.Join(res.Missing, ", ")+"'s report")
	}
	seen, err := e.seeFindings(ctx, cy, res)
	if err != nil {
		return false, err
	}
	answered, err := e.answeredQuestions(ctx, t)
	if err != nil {
		return false, err
	}
	declined := readDeclined(dir)
	var live, questions, doubts, rebutted, leftovers []ReviewItem
	for _, s := range seen {
		item := s.item
		switch {
		case s.f.Simplification || !blockingSeverity(s.f.Severity):
			leftovers = append(leftovers, item)
		case s.reg.Outcome == registry.FindingRebutted || s.reg.Outcome == registry.FindingWaived:
			rebutted = append(rebutted, item)
		case s.f.Question && answered[questionKey(st.Name, item.Title)]:
			leftovers = append(leftovers, item) // you decided it
		case s.f.Question:
			questions = append(questions, item)
		case s.reg.Reopened >= 2:
			doubts = append(doubts, item)
		default:
			live = append(live, item)
		}
	}
	if blocks && len(live)+len(questions)+len(doubts)+len(rebutted) == 0 {
		// The judge's verdict blocks with no P0 to P2 among the findings:
		// the writer gets what it flagged.
		for _, s := range seen {
			if !s.f.Simplification {
				live = append(live, s.item)
			}
		}
		if len(live) == 0 {
			live = append(live, ReviewItem{ID: "F1", Fingerprint: "verdict", Title: "the review's verdict is blocking", Body: why})
		}
		leftovers = slices.DeleteFunc(leftovers, func(i ReviewItem) bool {
			return slices.ContainsFunc(live, func(l ReviewItem) bool { return l.ID == i.ID })
		})
	}
	cycles := max(1, e.cfg.Factory.LocalReview.Cycles)
	// Every finding the writer declined with its risk, whichever round
	// raised it: the ready card and the PR body's "Known" name them.
	all := slices.SortedFunc(maps.Values(declined), func(a, b ReviewItem) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
	sum := ReviewSummary{Rounds: round, Head: cy.HeadBefore, Leftovers: leftovers, Declined: all, Doubts: doubts}
	switch {
	case !blocks || len(live)+len(questions)+len(doubts) == 0:
		sum.Exit = registry.ExitClean
		return e.endLoop(ctx, t, cy, sum)
	case len(live)+len(questions) == 0:
		sum.Exit = registry.ExitOscillation
		return e.endLoop(ctx, t, cy, sum)
	case round >= cycles:
		if err := e.endCycle(ctx, cy, cy.HeadBefore, registry.ExitCap); err != nil {
			return false, err
		}
		e.toOperator(ctx, t, fmt.Sprintf("%s after %s", plural(len(live)+len(questions)+len(doubts), "blocking finding"),
			plural(round, "review round")), "the local review's cap")
		return false, nil
	}
	n, err := e.attemptsOf(ctx, t, st)
	if err != nil {
		return false, err
	}
	rf := ReviewFile{Round: cy.N, Head: cy.HeadBefore, AttemptsBefore: n, Findings: append(live, questions...)}
	if len(questions) > 0 {
		if err := e.writeReviewQuestions(dir, st, questions, round); err != nil {
			return false, err
		}
	}
	if err := writeJSON(filepath.Join(dir, reviewFile(cy.N)), rf); err != nil {
		return false, err
	}
	e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("round %d (cycle %d) blocks: %s for the writer (%s)%s", round, cy.N,
		plural(len(live), "finding"), reviewFile(cy.N), map[bool]string{true: fmt.Sprintf(", %s for you first", plural(len(questions), "question")), false: ""}[len(questions) > 0]))
	return e.fixPhase(ctx, t, st, cy, rf)
}

// seenFinding is a finding of a round, as the round has it (f), as the
// factory hands it on (item) and as the registry holds it once seen (reg).
type seenFinding struct {
	f    evalrun.LocalFinding
	item ReviewItem
	reg  registry.Finding
}

// seeFindings records every finding of cycle cy's round in the registry
// (SeeFinding, by its fingerprint: seen again in one cycle, nothing
// changes), with its class (a defect, a simplification or a question) and
// whether it blocks.
func (e *Engine) seeFindings(ctx context.Context, cy registry.Cycle, res evalrun.LocalResult) ([]seenFinding, error) {
	out := make([]seenFinding, 0, len(res.Findings))
	for i, f := range res.Findings {
		fp := Fingerprint(f)
		class := "defect"
		switch {
		case f.Simplification:
			class = "simplification"
		case f.Question:
			class = "question"
		}
		reg, err := e.reg.SeeFinding(ctx, cy.ID, registry.Finding{Fingerprint: fp, Path: f.Path, StartLine: f.Line, EndLine: f.Line,
			TitleHash: titleHash(f.Title), Priority: f.Severity, Class: class, Verdict: "posted", Blocking: blockingSeverity(f.Severity) && !f.Simplification})
		if err != nil {
			return nil, err
		}
		out = append(out, seenFinding{f: f, reg: reg, item: ReviewItem{ID: fmt.Sprintf("F%d", i+1), Fingerprint: fp, Severity: f.Severity,
			Title: f.Title, Path: f.Path, Line: f.Line, Body: f.Body, Question: f.Question}})
	}
	return out, nil
}

// answeredQuestions are the keys (questionKey) of task t's questions you
// answered.
func (e *Engine) answeredQuestions(ctx context.Context, t registry.Task) (map[string]bool, error) {
	qs, err := e.reg.Questions(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, q := range qs {
		if q.State == registry.QuestionAnswered {
			out[questionKey(q.RaisedBy, q.Text)] = true
		}
	}
	return out, nil
}

// writeReviewQuestions writes the product questions of round `round` as
// the review stage's questions file (questions-<stage>.json, raised by the
// stage), which the task's question batch reads: each the finding's title,
// its place and body as the context, and the two ways on, none
// recommended.
func (e *Engine) writeReviewQuestions(dir string, st config.FactoryStage, qs []ReviewItem, round int) error {
	out := make([]QuestionFile, 0, len(qs))
	for _, q := range qs {
		where := q.Path
		if q.Line > 0 {
			where += ":" + strconv.Itoa(q.Line)
		}
		out = append(out, QuestionFile{Text: q.Title, Context: textx.Clip(strings.TrimSpace(strings.Join([]string{where, q.Body}, ": ")), 600),
			Options: []registry.Option{{Label: "change it", Means: "the writer changes the code as the finding asks"},
				{Label: "keep it", Means: "the code stays as it is, and the finding is no defect"}},
			Why:      "no recommendation: it is the product's call, which the ticket leaves open",
			Evidence: fmt.Sprintf("round %d of the local review", round)})
	}
	return writeJSON(filepath.Join(dir, roleQuestionsFile(st.Name)), out)
}

// attemptsOf counts task t's attempts of stage st.
func (e *Engine) attemptsOf(ctx context.Context, t registry.Task, st config.FactoryStage) (int, error) {
	as, err := e.reg.Attempts(ctx, t.ID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range as {
		if a.Stage == st.Name {
			n = max(n, a.N)
		}
	}
	return n, nil
}

// fixPhase is cycle cy's fix: your answers to its product questions first
// (the task's question batch), then one fix turn of the coding stage's
// roles in review stage st with ReviewFixPrompt (runTurn, its review's end
// fact: reviewTurn), until an attempt after the round's passed; then the
// cycle ends at the head the checks passed (closeFix), and the next round
// runs on the next pass.
func (e *Engine) fixPhase(ctx context.Context, t registry.Task, st config.FactoryStage, cy registry.Cycle, rf ReviewFile) (bool, error) {
	if slices.ContainsFunc(rf.Findings, func(i ReviewItem) bool { return i.Question }) {
		if waits, err := e.awaitAnswers(ctx, t); err != nil || waits {
			return false, err
		}
	}
	done, err := e.fixDone(ctx, t, st, rf)
	if err != nil {
		return false, err
	}
	if !done {
		roles := e.writerRoles(t)
		if len(roles) == 0 {
			return false, e.stageFailed(ctx, t, needsYou{"no stage of the " + t.Kind + " kind writes the change: the local review has no writer to fix it"})
		}
		var live []string
		for _, i := range rf.Findings {
			if !i.Question {
				live = append(live, i.ID)
			}
		}
		turn := config.FactoryStage{Name: st.Name, Type: config.FactoryStageAgents, Roles: roles, End: config.FactoryEndProof, Prompt: ReviewFixPrompt}
		if done, err = e.runTurn(ctx, t, turn, agentsTurn{review: &reviewTurn{n: cy.N, reviewed: cy.HeadBefore, live: live}}); err != nil || !done {
			return false, err
		}
	}
	return false, e.closeFix(ctx, t, cy, rf)
}

// fixDone reports whether a fix turn of round rf passed: an attempt of the
// review stage after the round's is verified.
func (e *Engine) fixDone(ctx context.Context, t registry.Task, st config.FactoryStage, rf ReviewFile) (bool, error) {
	as, err := e.reg.Attempts(ctx, t.ID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(as, func(a registry.Attempt) bool {
		return a.Stage == st.Name && a.N > rf.AttemptsBefore && a.State == registry.AttemptVerified
	}), nil
}

// writerRoles are the roles of the first agents stage of task t's kind that
// ends on the proof: the writer.
func (e *Engine) writerRoles(t registry.Task) []string {
	for _, st := range e.cfg.Factory.Kinds[t.Kind].Stages {
		if st.Type == config.FactoryStageAgents && st.End == config.FactoryEndProof {
			return st.Roles
		}
	}
	return nil
}

// closeFix ends cycle cy after its fix passed: each finding the writer
// declined with its risk is rebutted (the risk kept for the ready card),
// one declined without a risk stays open, every other one is fixed by the
// head the factory's checks passed at, which the cycle ends at (it is the
// task's head too). A cycle ended already is fine: a restart came between.
func (e *Engine) closeFix(ctx context.Context, t registry.Task, cy registry.Cycle, rf ReviewFile) error {
	cur, err := e.reg.TaskByID(ctx, t.ID)
	if err != nil {
		return err
	}
	head := cur.HeadSHA
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	var rep ReviewReply
	if b, err := os.ReadFile(filepath.Join(dir, reviewReplyFile(cy.N))); err == nil {
		_ = json.Unmarshal(b, &rep) // the end fact read it
	}
	risks := rep.declinedWithRisk()
	declinedNoRisk := map[string]bool{}
	for _, r := range rep.Replies {
		if r.Action == replyDeclined && risks[r.ID] == "" {
			declinedNoRisk[r.ID] = true
		}
	}
	findings, err := e.reg.Findings(ctx, t.ID)
	if err != nil {
		return err
	}
	byFP := map[string]registry.Finding{}
	for _, f := range findings {
		byFP[f.Fingerprint] = f
	}
	declined := readDeclined(dir)
	fixed, kept := 0, 0
	for _, i := range rf.Findings {
		f, ok := byFP[i.Fingerprint]
		switch {
		case !ok:
		case risks[i.ID] != "":
			if err := e.reg.SettleFinding(ctx, f.ID, registry.FindingRebutted, ""); err != nil {
				return err
			}
			i.Risk = risks[i.ID]
			declined[i.Fingerprint] = i
			kept++
		case declinedNoRisk[i.ID]:
			kept++ // a decline without a risk counts as open
		default:
			if err := e.reg.SettleFinding(ctx, f.ID, registry.FindingFixed, head); err != nil {
				return err
			}
			fixed++
		}
	}
	if len(declined) > 0 {
		if err := writeJSON(filepath.Join(dir, reviewDeclinedFile), declined); err != nil {
			return err
		}
	}
	if err := e.endCycle(ctx, cy, head, ""); err != nil {
		return err
	}
	e.event(ctx, t.ID, "review", registry.PhaseOK, fmt.Sprintf("cycle %d's fix at %s: %d fixed, %d declined", cy.N, short(head), fixed, kept))
	return nil
}

// readDeclined is review-declined.json in dir: the findings the writer
// declined with the risk it named, by fingerprint.
func readDeclined(dir string) map[string]ReviewItem {
	out := map[string]ReviewItem{}
	if b, err := os.ReadFile(filepath.Join(dir, reviewDeclinedFile)); err == nil {
		_ = json.Unmarshal(b, &out) // an unreadable one names no risk
	}
	return out
}

// endCycle ends cycle cy at head with exit; one ended already (a restart
// came between) is fine.
func (e *Engine) endCycle(ctx context.Context, cy registry.Cycle, head, exit string) error {
	delta := ""
	if cy.HeadBefore != "" && head != "" && head != cy.HeadBefore {
		delta = short(cy.HeadBefore) + ".." + short(head)
	}
	if err := e.reg.EndCycle(ctx, cy.ID, head, delta, exit, 0); err != nil && !errors.Is(err, registry.ErrConflict) {
		return err
	}
	return nil
}

// endLoop ends the loop on cycle cy with sum's exit (clean or an
// oscillation): the summary for the ready card and the PR body first, then
// the cycle; the stage passes.
func (e *Engine) endLoop(ctx context.Context, t registry.Task, cy registry.Cycle, sum ReviewSummary) (bool, error) {
	if err := writeJSON(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), reviewSummaryFile), sum); err != nil {
		return false, err
	}
	if err := e.endCycle(ctx, cy, cy.HeadBefore, sum.Exit); err != nil {
		return false, err
	}
	e.event(ctx, t.ID, "review", registry.PhaseOK, fmt.Sprintf("the local review ended %s after %s: %d left, %d declined, %d doubts",
		sum.Exit, plural(sum.Rounds, "round"), len(sum.Leftovers), len(sum.Declined), len(sum.Doubts)))
	return true, nil
}

// markCodexNever marks task t never for Codex again (a refusal of a local
// round, why), with a codex_never event; a task marked already is left.
func (e *Engine) markCodexNever(ctx context.Context, t registry.Task, why string) error {
	if t.CodexNever {
		return nil
	}
	if err := e.reg.TransitionTask(ctx, t.ID, nil, "", func(u *registry.TaskUpdate) {
		u.Set("codex_never", true)
		u.Because(why)
	}); err != nil {
		return err
	}
	e.event(ctx, t.ID, "codex_never", registry.PhaseInfo, why+"; Codex never works on "+TaskKey(t)+" again")
	return nil
}

// writeJSON writes v to path as indented JSON, atomically and private.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// reviewTurn is the review's part of a fix turn's end fact (agentsTurn): the
// cycle's number n, the head the round reviewed, and the ids of the
// findings that need a fix (not the questions).
type reviewTurn struct {
	n        int
	reviewed string
	live     []string
}

// replyProblem is what keeps the writer's reply from being read: missing,
// older than since, not JSON, or a reply with another action than fixed or
// declined.
func (rt *reviewTurn) replyProblem(dir string, since time.Time) *endProblem {
	name := reviewReplyFile(rt.n)
	text, p := readFresh(dir, name, since)
	if p != nil {
		return p
	}
	var rep ReviewReply
	if err := json.Unmarshal([]byte(text), &rep); err != nil {
		return &endProblem{file: name, why: "not valid JSON (" + err.Error() + `): write {"replies": [{"id": "F1", "action": "fixed"}]}`}
	}
	for _, r := range rep.Replies {
		if r.Action != replyFixed && r.Action != replyDeclined {
			return &endProblem{file: name, why: fmt.Sprintf("the reply to %s says %q: write fixed, or declined with its risk", r.ID, r.Action)}
		}
	}
	return nil
}

// commitProblem is the fix turn's change problem when the branch has no
// commit since the round reviewed it (head is still that head) while a
// finding needs a fix: one the writer neither fixed in a commit nor
// declined with its risk.
func (rt *reviewTurn) commitProblem(dir, head string) *endProblem {
	if head == "" || head != rt.reviewed {
		return nil
	}
	var rep ReviewReply
	if b, err := os.ReadFile(filepath.Join(dir, reviewReplyFile(rt.n))); err == nil {
		_ = json.Unmarshal(b, &rep) // replyProblem read it
	}
	risks := rep.declinedWithRisk()
	if !slices.ContainsFunc(rt.live, func(id string) bool { return risks[id] == "" }) {
		return nil // every finding declined with its risk: nothing to commit
	}
	return &endProblem{file: "the branch", change: true, why: fmt.Sprintf("the branch has no commit since the local review reviewed %s: "+
		"commit your fixes, or decline each finding with its risk", short(rt.reviewed))}
}
