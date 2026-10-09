package engine

// The gate of a skill proposal (DECISIONS "The gate replays the learned
// cases"), the self-improving loop's fourth stage. A proposal the editor
// committed waits in gating; at a tick with nothing holding it the daemon
// replays eval cases on it in its own goroutine, one at a time: the learned
// cases of the inputs it noted (its targets) and the newest other cases of
// both corpora, up to [learn] gate_cases (learn.SelectCases), the judge
// alone when only the judge skill and the judge prompts changed
// (learn.Mode). Each case's baseline is the newest stored replay with the
// prompts and skill of the proposal's base commit (eval.Baselines), else a
// replay of a detached worktree at that commit first (gateBaseName, removed
// after the gate: never the checkout, whose branch and uncommitted edits
// are the operator's); judge-only, both judges read the reviewer reports of
// the case's newest full replay whose reviewers read the candidate's
// prompts, so the baseline is that replay when the base's judge made it,
// else the base's judge alone on its reports (eval.JudgeBaselines). The
// candidate replays with the proposal's worktree, and a case it lost is
// replayed once more. learn.Judge decides: passed (it lands,
// learn_land.go), held (the operator decides) or failed (its worktree and
// branch go, its inputs stay new); a revert (learn_revert.go) is held
// whatever it decides. Every replay is stored as `magnum eval
// run` stores one, under runs of the gate's own (a baseline, a baseline
// judge, a candidate and a retry run, each with its agent tag), so a later
// gate and `magnum eval` read them. The replays run in-process through Deps.EvalCase
// (evalrun.RunCase, which the daemon's wiring sets: the engine cannot
// import evalrun, Amendment A1). The Codex soft cap, a usage limit, a
// shutdown, a drain or a pause (checked before each replay), and a replay
// that ended without a score where the verdict needs one leave the
// proposal gating for a later tick; the third gate in a row that stops on
// a replay without a score holds it (gateStopLimit).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/usage"
)

const (
	// gateRetryAfter is how long a gate that stopped short (a usage limit,
	// a drain or a pause, a replay without a score, an error before any
	// case, a verdict the registry did not take) waits before it starts
	// again.
	gateRetryAfter = 30 * time.Minute
	// gateStopLimit is how many gates of a proposal may stop on a replay
	// without a score before the next such stop holds it for the operator:
	// a replay that never scores (a pull request GitHub no longer serves)
	// would keep the oldest gating proposal, and the loop behind it, waiting
	// for good.
	gateStopLimit = 3
	// kvLearnGateStops counts the gates of a proposal that stopped on a
	// replay without a score since a gate last decided (gateStops as JSON).
	kvLearnGateStops = "learn.gate_stops"
	// gateReasonRunes clips the rule stored as a gated proposal's reason.
	gateReasonRunes = 2000
	// gateBaseName is the gate's detached worktree of the proposal's base
	// commit under Layout.LearnWorktrees, whose prompts and skill the
	// baseline replays (one gate runs at a time); no editor run is named
	// so, so the orphan cleanup leaves it to the gate.
	gateBaseName = "gate-base"
)

// EvalGateOptions are one replay of the gate (evalrun.Options' part the
// gate sets): the directory whose prompts and judge skill it reads (the
// gate's worktree of the base for a baseline, the proposal's worktree for
// the candidate),
// whether the judge alone reviews (on the reviewers' reports of the
// case's newest full replay whose reviewers read what this one's would),
// and the run it belongs to, its directory under Layout.Eval and its
// agent tag (eval.AgentTag), both its own.
type EvalGateOptions struct {
	Candidate string
	JudgeOnly bool
	RunDir    string
	AgentTag  string
}

// gateStops is kvLearnGateStops: Stops gates of Proposal stopped on a
// replay without a score.
type gateStops struct {
	Proposal int64 `json:"proposal"`
	Stops    int   `json:"stops"`
}

// GateMark is the gate in flight (KVLearnGating).
type GateMark struct {
	Proposal int64     `json:"proposal"`
	Started  time.Time `json:"started"`
}

// GateCase is one case of a gate's result: the row learn.Judge read, the
// eval runs its two scores come from and, judge-only, the full replay
// whose reviewer reports both judges read.
type GateCase struct {
	learn.GateRow
	BaselineRun  string `json:"baseline_run,omitempty"`
	CandidateRun string `json:"candidate_run,omitempty"` // a second replay's when Retried
	ReportsRun   string `json:"reports_run,omitempty"`
}

// GateResult is what a proposal's gate decided (learn_proposals.gate_json):
// the verdict and its rule, the mode, the inputs hashes the candidate and
// the baseline replayed with (eval.Run.Inputs), every case, the eval runs
// the gate stored and why the corpus could not be read, if it could not.
type GateResult struct {
	State          string     `json:"state"`
	Rule           string     `json:"rule"`
	Mode           string     `json:"mode"`
	Inputs         string     `json:"inputs,omitempty"`
	BaselineInputs string     `json:"baseline_inputs,omitempty"`
	Cases          []GateCase `json:"cases"`
	Runs           []string   `json:"runs,omitempty"`
	CorpusError    string     `json:"corpus_error,omitempty"`
}

// errCandidateGone is a proposal whose branch is gone or moved: nothing
// can be replayed or landed of it.
var errCandidateGone = errors.New("the candidate is gone")

// gateBusy reports whether a gate is in flight.
func (e *Engine) gateBusy() bool {
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	return e.gateCancel != nil
}

// gateWaits reports whether a gate that stopped short (or panicked) still
// waits out gateRetryAfter.
func (e *Engine) gateWaits() bool {
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	return e.now().Before(e.gateRetry)
}

// gateUnavailable is why no gate can run ("" = one can): no eval seams
// (a dry run, a binary without the daemon's wiring) or no checkout.
func (e *Engine) gateUnavailable() string {
	switch {
	case e.d.EvalCase == nil || e.d.EvalInputs == nil || e.d.EvalRuns == nil || e.d.EvalCorpus == nil:
		return "no eval replays in this daemon"
	case e.d.Checkout == "" || e.d.LearnGit == nil:
		return "no checkout of magnum"
	}
	return ""
}

// gateHold is why the gate waits now ("" = it may start): a drain, an
// infrastructure pause, `magnum pause` (the gate replays review rounds), a
// pause of a reviewing CLI, or the Codex soft cap (gateBudget tells).
func (e *Engine) gateHold(ctx context.Context) string {
	if why := cmp.Or(e.holdReason(ctx), e.userPause(ctx)); why != "" {
		return why
	}
	var kinds []string
	for _, r := range e.cfg.RolesFor(nil) {
		if k := r.AgentKind(); k != "" && !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	return e.kindPauseReason(ctx, kinds)
}

// maybeGate starts the gate of the oldest gating proposal (every tick),
// unless a learn job runs (a gate, an edit, the retro or a notes curation:
// learnBusy), a stopped gate waits out gateRetryAfter,
// something holds it (gateHold) or the Codex budget is at the soft cap
// (gateBudget). A gating proposal a crashed or restarted daemon left
// behind is picked up the same way. A dry run and `magnum daemon --once`
// never start one.
func (e *Engine) maybeGate(ctx context.Context) {
	if e.d.DryRun || e.once || e.gateUnavailable() != "" || e.learnBusy() != "" || e.gateWaits() {
		return
	}
	if e.gateHold(ctx) != "" {
		return
	}
	gating, err := e.st.LearnProposals(ctx, []string{store.LearnGating})
	if err != nil || len(gating) == 0 {
		if err != nil {
			e.warnUnlessStopped(ctx, err, "skill gate: list the gating proposals")
		}
		return
	}
	p := gating[len(gating)-1] // the oldest
	if level, snap := e.budgetLevel(); level >= usage.Soft {
		e.gateBudget(ctx, p, snap)
		return
	}
	e.startGate(ctx, p)
}

// gateBudget says, once per budget window, that proposal p waits for the
// Codex budget: a learn.gate_waiting event and a toast. The registry
// reserves the window's key, so a restarted daemon stays quiet.
func (e *Engine) gateBudget(ctx context.Context, p store.LearnProposal, snap usage.Snapshot) {
	w := bindingWindow(snap)
	window := strconv.FormatInt(w.ResetsAt.Unix(), 10)
	span := time.Duration(w.WindowMinutes) * time.Minute
	if span <= 0 {
		span = budgetToastWindow
	}
	why := fmt.Sprintf("Codex budget %.0f%% used, at or past [usage] codex_soft %g%%: real reviews come first", snap.Used(), e.cfg.Usage.CodexSoft)
	switch first, err := e.st.ShouldSend(ctx, "learn:gate-waiting:"+window, span); {
	case err != nil:
		e.warnUnlessStopped(ctx, err, "skill gate: the budget wait")
		return
	case !first:
		return
	}
	e.event(ctx, "info", learnSubject, "learn.gate_waiting", fmt.Sprintf("the gate of skill proposal %d waits: %s", p.ID, why),
		map[string]any{"proposal": p.ID, "reason": "budget"})
	title := fmt.Sprintf("skill proposal %d waits for the Codex budget", p.ID)
	e.info(notify.Item{Key: "learn-gate-budget:" + window, Title: title, Line: title, Window: span,
		Body: why + "; the gate replays its cases once the budget is below the cap."})
}

// startGate runs the gate of p in its own goroutine on a child of ctx
// (the daemon's: shutdown cancels it), unless the daemon is stopping or a
// learn job runs; it is KVLearnGating while it runs.
func (e *Engine) startGate(ctx context.Context, p store.LearnProposal) bool {
	if e.stopping(ctx) != "" {
		return false
	}
	e.learnMu.Lock()
	defer e.learnMu.Unlock()
	if e.learnBusy() != "" {
		return false
	}
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	gctx, cancel := context.WithCancel(ctx)
	e.gateCancel, e.gateStarted, e.gateProposal = cancel, e.now(), p.ID
	if b, err := json.Marshal(GateMark{Proposal: p.ID, Started: e.gateStarted}); err == nil {
		e.setKV(ctx, KVLearnGating, string(b))
	}
	e.gateWG.Go(func() {
		defer func() {
			e.delKV(context.WithoutCancel(ctx), KVLearnGating)
			e.gateMu.Lock()
			e.gateCancel = nil
			e.gateMu.Unlock()
			cancel()
		}()
		stopped := false
		if e.safely(gctx, "skill gate", learnSubject, func() { stopped = e.runGate(gctx, p) != nil }, nil) || stopped {
			e.gateMu.Lock()
			e.gateRetry = e.now().Add(gateRetryAfter)
			e.gateMu.Unlock()
		}
	})
	return true
}

// stopGate cancels a running gate (shutdown); its proposal stays gating.
func (e *Engine) stopGate() {
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	if e.gateCancel != nil {
		e.gateCancel()
	}
}

// gateReplay is one replay of the gate: its score, or why it has none:
// skip (the case is left out: its PR is flagged), missing (it ended
// without a score: herdr away, a GitHub read, a judge's timeout, a judge
// that read other reviewer reports than the gate chose) or stop (the gate
// must stop now: something holds it, gateHold, the budget, a usage limit,
// a login, a shutdown); the eval run it is stored in and, judge-only, the
// full replay whose reviewer reports its judge read.
type gateReplay struct {
	score               *eval.Score
	run, reports        string
	skip, missing, stop string
}

// gateRun is an eval run the gate stores, made on its first replay.
type gateRun struct {
	run  eval.Run
	dir  string
	tag  string
	opts EvalGateOptions
}

// runGate is the gate of proposal p: its worktree (reused, else added
// again from its branch), a worktree of its base, the cases, a baseline
// and a candidate replay of each (a second one of a lost case), the
// verdict and the proposal's next state. It returns an error when the gate
// stopped short and p stays gating: a drain or a pause before a replay
// (gateHold), a replay at the soft cap or a usage limit, a replay without a
// score that the verdict needs (noScore), a shutdown, an unreadable
// registry, inputs or runs, a verdict the registry did not take.
func (e *Engine) runGate(ctx context.Context, p store.LearnProposal) error {
	g := &gateWork{e: e, ctx: ctx, octx: context.WithoutCancel(ctx), p: p}
	if why := e.gateUnavailable(); why != "" {
		return errors.New(why)
	}
	if err := e.ensureCandidate(ctx, p, false); errors.Is(err, errCandidateGone) {
		return e.gateDecide(g.octx, p, GateResult{State: learn.GateFailed, Rule: oneLine(err.Error(), retroWhyRunes), Mode: learn.Mode(p.Files)})
	} else if err != nil {
		return g.waits("the worktree: " + oneLine(err.Error(), retroWhyRunes))
	}
	baseDir, err := e.addGateBase(ctx, p.BaseSHA)
	defer e.dropGateBase(g.octx) // whatever ends the gate
	if err != nil {
		return g.waits("the worktree of its base: " + oneLine(err.Error(), retroWhyRunes))
	}
	mode := learn.Mode(p.Files)
	g.baseDir, g.judgeOnly, g.res = baseDir, mode == learn.GateJudgeOnly, GateResult{Mode: mode, Cases: []GateCase{}}
	targets, err := e.gateTargets(ctx, p)
	if err != nil {
		return g.waits("the proposal's inputs: " + oneLine(err.Error(), retroWhyRunes))
	}
	// A corpus or a flag list that cannot be read is no verdict (its targets
	// would read as not in the corpus, or every PR as flagged): the gate
	// stops as on a replay without a score.
	corpus, corpusPath, err := e.d.EvalCorpus()
	if err != nil {
		g.res.CorpusError = oneLine(err.Error(), retroWhyRunes)
		return g.noScore("the corpus cannot be read: " + g.res.CorpusError)
	}
	flagged, err := e.gateFlagged(ctx)
	if err != nil {
		return g.noScore("the flagged replays cannot be read: " + oneLine(err.Error(), retroWhyRunes))
	}
	g.corpusPath = corpusPath
	cases := learn.SelectCases(corpus, targets, e.gateWatched, flagged, e.cfg.Learn.GateCases)
	if g.baseIn, g.baseRev, err = e.d.EvalInputs(baseDir); err != nil {
		return g.waits("the base's inputs: " + oneLine(err.Error(), retroWhyRunes))
	}
	if g.candIn, g.candRev, err = e.d.EvalInputs(p.Scratch); err != nil {
		return g.waits("the candidate's inputs: " + oneLine(err.Error(), retroWhyRunes))
	}
	g.res.Inputs, g.res.BaselineInputs = g.candIn, g.baseIn
	if g.runs, err = e.d.EvalRuns(); err != nil {
		return g.waits("the eval runs: " + oneLine(err.Error(), retroWhyRunes))
	}
	e.event(ctx, "info", learnSubject, "learn.gate_start", fmt.Sprintf("the gate of skill proposal %d started: %s, %s (%s)", p.ID,
		textx.Count(len(cases), "case", "cases"), strings.Join(targets, ", "), mode), map[string]any{"proposal": p.ID, "cases": len(cases), "mode": mode})

	defer g.finishRuns()
	for _, gc := range cases {
		row, stop, err := g.caseRow(gc)
		if stop {
			return err
		}
		g.res.Cases = append(g.res.Cases, row)
	}
	if stop, err := g.retryLost(cases); stop {
		return err
	}
	return g.decide()
}

// gateWork is one gate's state (runGate): the proposal, what its base and
// its candidate replay with, the stored runs as the gate last read them,
// the eval runs it made, one per kind of replay, and its result so far.
type gateWork struct {
	e         *Engine
	ctx, octx context.Context
	p         store.LearnProposal
	res       GateResult
	runs      []eval.Run

	corpusPath, baseDir              string
	baseIn, baseRev, candIn, candRev string
	judgeOnly                        bool

	base, baseJudge, cand, retry *gateRun
}

// waits ends the gate short with why, p staying gating: a
// learn.gate_waiting event and the error runGate returns.
func (g *gateWork) waits(why string) error {
	g.e.event(g.octx, "info", learnSubject, "learn.gate_waiting", fmt.Sprintf("the gate of skill proposal %d stopped and waits: %s", g.p.ID, why),
		map[string]any{"proposal": g.p.ID, "reason": why})
	return errors.New(why)
}

// mkRun is the run in slot, made on its first replay: its directory under
// Layout.Eval, its agent tag, and the inputs and the tree (treeState of
// dir) it replays.
func (g *gateWork) mkRun(slot **gateRun, label, dir string, judgeOnly bool, in, rev string) (*gateRun, error) {
	if *slot != nil {
		return *slot, nil
	}
	e := g.e
	commit, dirty := e.treeState(g.ctx, dir)
	id, rdir, err := eval.NewRunDir(e.d.Layout.Eval(), e.now())
	if err != nil {
		return nil, err
	}
	r := &gateRun{dir: rdir, tag: eval.AgentTag(id), run: eval.Run{ID: id, Label: fmt.Sprintf("learn gate of proposal %d: %s", g.p.ID, label),
		Commit: commit, Dirty: dirty, Corpus: g.corpusPath, Started: e.now(), Cases: []eval.CaseRun{}, Inputs: in, ReviewerInputs: rev,
		Candidate: dir}}
	r.opts = EvalGateOptions{Candidate: dir, JudgeOnly: judgeOnly, RunDir: rdir, AgentTag: r.tag}
	*slot = r
	g.res.Runs = append(g.res.Runs, id)
	return r, nil
}

// finishRuns finishes and saves every run the gate made, whatever stopped
// it.
func (g *gateWork) finishRuns() {
	for _, r := range []*gateRun{g.base, g.baseJudge, g.cand, g.retry} {
		if r != nil {
			r.run.Finished = g.e.now()
			if err := eval.SaveRun(r.dir, r.run); err != nil {
				g.e.log.Warn("skill gate: save an eval run", "run", r.run.ID, "err", err)
			}
		}
	}
}

// replay replays c into the run in slot (judge-only: on the reviewer
// reports of the run want names, "" = any; a judge that read others has no
// score), after checking what holds the gate (gateHold: a drain or a pause
// since it started).
func (g *gateWork) replay(slot **gateRun, label, dir string, judgeOnly bool, in, rev string, c eval.Case, want string) gateReplay {
	e, ctx := g.e, g.ctx
	if why := e.gateHold(ctx); why != "" {
		return gateReplay{stop: why}
	}
	r, err := g.mkRun(slot, label, dir, judgeOnly, in, rev)
	if err != nil {
		return gateReplay{stop: "the eval run's directory: " + oneLine(err.Error(), retroWhyRunes)}
	}
	cr := e.d.EvalCase(ctx, c, r.opts)
	cr.Finished = e.now()
	r.run.Cases = append(r.run.Cases, cr)
	if err := eval.SaveRun(r.dir, r.run); err != nil {
		e.log.Warn("skill gate: save an eval run", "run", r.run.ID, "err", err)
	}
	if rs, err := e.d.EvalRuns(); err == nil {
		g.runs = rs
	}
	out := gateReplay{run: r.run.ID, reports: cr.ReportsRun}
	why := cmp.Or(oneLine(cr.Error, retroWhyRunes), "no result to score")
	switch {
	case ctx.Err() != nil:
		out.stop = "the daemon is stopping"
	case cr.Outcome == eval.OutcomeBudget, cr.Outcome == pipeline.OutcomeUsageLimit, cr.Outcome == pipeline.OutcomeLoginRequired:
		out.stop = fmt.Sprintf("case %s: %s (%s)", c.Name, cr.Outcome, why)
	case cr.Outcome == eval.OutcomeFlagged:
		out.skip = learn.SkipFlagged
	case cr.Score == nil:
		out.missing = fmt.Sprintf("the %s replay of %s ended %s: %s", label, c.Name, cr.Outcome, why)
	case judgeOnly && want != "" && cr.ReportsRun != want: // a full replay newer than the gate's choice
		out.missing = fmt.Sprintf("the %s replay of %s read the reviewer reports of run %s, not those of run %s", label, c.Name,
			cmp.Or(cr.ReportsRun, "none"), want)
	default:
		out.score = trimScore(cr.Score)
	}
	return out
}

// noScore stops the gate on a replay that ended without a score where the
// verdict needs one (a target's or its baseline's, a lost case's second):
// p stays gating for a later try, and only the gateStopLimit-th gate of p
// that stopped so holds it, saying why.
func (g *gateWork) noScore(why string) error {
	n := g.e.countGateStop(g.octx, g.p.ID)
	if n < gateStopLimit {
		return g.waits(why)
	}
	g.res.State, g.res.Rule = learn.GateHeld, fmt.Sprintf("%d gates in a row stopped on a replay without a score, the last: %s", n, why)
	return g.e.gateDecide(g.octx, g.p, g.res)
}

// halt is whether replay rp ends the gate, and with what: a stop, or one
// without a score where the verdict needs one (need).
func (g *gateWork) halt(rp gateReplay, need bool) (bool, error) {
	switch {
	case rp.stop != "":
		return true, g.waits(rp.stop)
	case rp.missing != "" && need:
		return true, g.noScore(rp.missing)
	}
	return false, nil
}

// noFullReplay is why a judge-only gate leaves a case out.
const noFullReplay = "no full replay whose reviewers read the candidate's prompts, for the judge alone"

// caseRow is case gc's row: its baseline (baseline), then the candidate's
// replay; stop when the gate ends there, with what runGate returns.
func (g *gateWork) caseRow(gc learn.GateCase) (GateCase, bool, error) {
	row := GateCase{GateRow: learn.GateRow{Case: gc.Case.Name, Target: gc.Target, Skip: gc.Skip}}
	if row.Skip != "" {
		return row, false, nil
	}
	if g.ctx.Err() != nil {
		return row, true, g.waits("the daemon is stopping")
	}
	if stop, err := g.baseline(gc, &row); stop {
		return row, true, err
	}
	if row.Skip == "" {
		rp := g.replay(&g.cand, "candidate", g.p.Scratch, g.judgeOnly, g.candIn, g.candRev, gc.Case, row.ReportsRun)
		if stop, err := g.halt(rp, gc.Target); stop {
			return row, true, err
		}
		row.After, row.CandidateRun, row.Skip = rp.score, rp.run, cmp.Or(rp.skip, rp.missing)
	}
	return row, false, nil
}

// baseline sets row's baseline: a stored replay with the base's inputs,
// else a replay of the base. Judge-only, both judges read the reviewer
// reports of the case's newest full replay whose reviewers read the
// candidate's prompts: the baseline is that replay itself when the base's
// judge made it, else the base's judge alone on its reports (stored, or
// replayed now); without one the base's full replay is the baseline and
// makes it, and a stored baseline that recorded no reviewer inputs is no
// full replay (the case is left out). Stop when the gate ends there.
func (g *gateWork) baseline(gc learn.GateCase, row *GateCase) (bool, error) {
	fr, fc, full := eval.FullReplayFor(g.runs, gc.Case.Name, gc.Case.Head, &eval.Run{ReviewerInputs: g.candRev})
	switch {
	case g.judgeOnly && full && fc.ReportDir != "":
		row.ReportsRun = fr.ID
		if bs := eval.JudgeBaselines(g.runs, g.baseIn, gc.Case, fr.ID); len(bs) > 0 {
			row.Before, row.BaselineRun = trimScore(bs[0].Case.Score), bs[0].Run
			return false, nil
		}
		rp := g.replay(&g.baseJudge, "baseline judge", g.baseDir, true, g.baseIn, g.baseRev, gc.Case, fr.ID)
		if stop, err := g.halt(rp, gc.Target); stop {
			return true, err
		}
		row.Before, row.BaselineRun, row.Skip = rp.score, rp.run, cmp.Or(rp.skip, rp.missing)
		return false, nil
	case g.judgeOnly && len(eval.Baselines(g.runs, g.baseIn, gc.Case)) > 0:
		row.Skip = noFullReplay
		return false, nil
	}
	if bs := eval.Baselines(g.runs, g.baseIn, gc.Case); len(bs) > 0 {
		row.Before, row.BaselineRun = trimScore(bs[0].Case.Score), bs[0].Run
		return false, nil
	}
	rp := g.replay(&g.base, "baseline", g.baseDir, false, g.baseIn, g.baseRev, gc.Case, "")
	if stop, err := g.halt(rp, gc.Target); stop {
		return true, err
	}
	row.Before, row.BaselineRun, row.Skip = rp.score, rp.run, cmp.Or(rp.skip, rp.missing)
	if g.judgeOnly && row.Skip == "" { // its reviewers' reports are the candidate's judge's
		if fr, fc, ok := eval.FullReplayFor(g.runs, gc.Case.Name, gc.Case.Head, &eval.Run{ReviewerInputs: g.candRev}); ok && fc.ReportDir != "" &&
			fr.ID == rp.run {
			row.ReportsRun = fr.ID
		} else {
			row.Skip = noFullReplay
		}
	}
	return false, nil
}

// retryLost replays a case the candidate lost once more before the loss
// counts; stop when the gate ends there, with what runGate returns.
func (g *gateWork) retryLost(cases []learn.GateCase) (bool, error) {
	for i := range g.res.Cases {
		row := &g.res.Cases[i]
		if len(row.Lost()) == 0 {
			continue
		}
		c := cases[slices.IndexFunc(cases, func(gc learn.GateCase) bool { return gc.Case.Name == row.Case })].Case
		rp := g.replay(&g.retry, "second", g.p.Scratch, g.judgeOnly, g.candIn, g.candRev, c, row.ReportsRun)
		if stop, err := g.halt(rp, true); stop { // without its score the loss would count
			return true, err
		}
		if rp.skip != "" {
			row.Skip = rp.skip
			continue
		}
		row.Retried, row.After, row.CandidateRun = true, rp.score, rp.run
	}
	return false, nil
}

// decide judges the rows (learn.Judge; a revert is held whatever it
// decides) and moves p to the verdict's state (gateDecide).
func (g *gateWork) decide() error {
	res, p := &g.res, g.p
	rows := make([]learn.GateRow, len(res.Cases))
	for i, c := range res.Cases {
		rows[i] = c.GateRow
	}
	v := learn.Judge(rows)
	res.State, res.Rule = v.State, v.Rule
	if p.Reverts != 0 { // a revert lands only on the operator's y (learn_revert.go)
		res.State, res.Rule = learn.GateHeld, fmt.Sprintf("a revert of skill version %d waits for the operator whatever the gate found "+
			"(it would have %s it: %s)", p.Reverts, v.State, res.Rule)
	}
	return g.e.gateDecide(g.octx, p, *res)
}

// countGateStop counts one more gate of proposal id that stopped on a
// replay without a score (kvLearnGateStops) and returns how many did since
// a gate last decided.
func (e *Engine) countGateStop(ctx context.Context, id int64) int {
	var s gateStops
	if v, ok := e.getKV(ctx, kvLearnGateStops); !ok || json.Unmarshal([]byte(v), &s) != nil || s.Proposal != id {
		s = gateStops{Proposal: id}
	}
	s.Stops++
	if b, err := json.Marshal(s); err == nil {
		e.setKV(ctx, kvLearnGateStops, string(b))
	}
	return s.Stops
}

// gateDecide stores the gate's result on p and moves it from gating to the
// verdict's state: a failed one loses its worktree and branch (its inputs
// stay new), a held one waits for the operator with a toast, a passed one
// for the landing. An error when the registry did not take it: p stays
// gating, and the gate that stopped short waits out gateRetryAfter.
func (e *Engine) gateDecide(ctx context.Context, p store.LearnProposal, res GateResult) error {
	if res.Cases == nil {
		res.Cases = []GateCase{}
	}
	b, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("skill gate: encode the result of proposal %d: %w", p.ID, err)
	}
	got, err := e.st.DecideLearnProposal(ctx, p.ID, []string{store.LearnGating}, res.State, textx.Clip(res.Rule, gateReasonRunes), e.now(), b)
	if err != nil {
		e.log.Warn("skill gate: decide the proposal", "proposal", p.ID, "state", res.State, "err", err)
		return fmt.Errorf("skill gate: decide proposal %d: %w", p.ID, err)
	}
	e.delKV(ctx, kvLearnGateStops)
	e.event(ctx, "info", learnSubject, "learn.gate_done", fmt.Sprintf("the gate %s skill proposal %d: %s", res.State, p.ID, oneLine(res.Rule, 300)),
		map[string]any{"proposal": p.ID, "state": res.State, "rule": oneLine(res.Rule, 300), "cases": len(res.Cases), "mode": res.Mode})
	switch res.State {
	case store.LearnFailed:
		e.dropCandidate(ctx, p.Scratch, p.Branch)
	case store.LearnHeld:
		e.noteHeld(ctx, got, res.Rule, true)
	}
	return nil
}

// noteHeld tells that proposal p waits for the operator, why: a
// learn.held event and, with toast, a toast (learn-proposal:<id>) that
// names the review (the operator who asked for a landing reads the answer
// instead).
func (e *Engine) noteHeld(ctx context.Context, p store.LearnProposal, why string, toast bool) {
	e.event(ctx, "info", learnSubject, "learn.held", fmt.Sprintf("skill proposal %d is held for review: %s", p.ID, oneLine(why, 300)),
		map[string]any{"proposal": p.ID, "reason": oneLine(why, 300)})
	if !toast {
		return
	}
	title := fmt.Sprintf("skill proposal %d waits for review", p.ID)
	e.info(notify.Item{Key: "learn-proposal:" + strconv.FormatInt(p.ID, 10), Title: title, Line: title,
		Body: oneLine(why, 200) + ". `magnum learn --review` lands or rejects it."})
}

// trimScore is s without the findings that matched no defect (their text
// is the judge's review of a PR; the eval run keeps them).
func trimScore(s *eval.Score) *eval.Score {
	if s == nil {
		return nil
	}
	out := *s
	out.Unmatched = nil
	return &out
}

// gateTargets are the learned cases of the inputs p noted
// (learn.TargetName), misses first.
func (e *Engine) gateTargets(ctx context.Context, p store.LearnProposal) ([]string, error) {
	ins, err := e.st.LearnProposalInputs(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, kind := range []string{store.LearnInputMiss, store.LearnInputDecline} {
		for _, in := range ins {
			if in.Kind == kind && in.Outcome == store.MissNoted {
				out = append(out, learn.TargetName(in.Kind, in.InputID))
			}
		}
	}
	return out, nil
}

// gateWatched reports whether a watch covers pr (owner/repo#N): a replay
// takes the watch's roles and identity.
func (e *Engine) gateWatched(pr string) bool {
	repo, _, _ := strings.Cut(pr, "#")
	return e.cfg.WatchFor(repo) != nil
}

// gateFlagged reports whether pr (owner/repo#N) must never be replayed:
// an earlier replay of it was refused (eval.FlaggedFile; an error when it
// cannot be read, and the gate replays nothing), or the registry holds a
// Codex flag for it (one it cannot read counts).
func (e *Engine) gateFlagged(ctx context.Context) (func(pr string) bool, error) {
	flagged, err := eval.LoadFlagged(e.d.Layout.Eval())
	if err != nil {
		return nil, err
	}
	return func(pr string) bool {
		for _, f := range flagged {
			if strings.EqualFold(strings.TrimSpace(f.PR), pr) {
				return true
			}
		}
		repoName, num, _ := strings.Cut(pr, "#")
		n, err := strconv.Atoi(num)
		if err != nil {
			return true
		}
		repo, err := e.st.RepoByFullName(ctx, repoName)
		if errors.Is(err, store.ErrNotFound) {
			return false
		}
		if err != nil {
			return true
		}
		row, err := e.st.PRByRepoNumber(ctx, repo.ID, n)
		if errors.Is(err, store.ErrNotFound) {
			return false
		}
		if err != nil {
			return true
		}
		_, ok := e.codexFlag(ctx, row.ID)
		return ok
	}, nil
}

// treeState is what a gate's eval run records of the tree whose prompts
// and skill it replays (the worktree of the base, the proposal's), as
// `magnum eval run` records its checkout: the short HEAD and whether it has
// uncommitted changes (eval.Baselines reuses no dirty run), which a status
// that cannot be read counts as.
func (e *Engine) treeState(ctx context.Context, dir string) (string, bool) {
	sha, err := e.d.LearnGit.RevParse(ctx, dir, "HEAD")
	if err != nil {
		return "", true
	}
	st, err := e.d.LearnGit.StatusPaths(ctx, dir)
	return learnShort(sha), err != nil || len(st) > 0
}

// gateBaseDir is the gate's worktree of the base (gateBaseName).
func (e *Engine) gateBaseDir() string {
	return filepath.Join(e.d.Layout.LearnWorktrees(), gateBaseName)
}

// addGateBase adds the gate's worktree of the base at sha, detached, in
// place of whatever a gate a crash cut short left there, and returns it.
func (e *Engine) addGateBase(ctx context.Context, sha string) (string, error) {
	git, dir := e.d.LearnGit, e.gateBaseDir()
	if err := git.WorktreeDiscard(ctx, e.d.Checkout, dir); err != nil {
		// A directory git does not know as a worktree: it is the gate's own.
		if rerr := os.RemoveAll(dir); rerr != nil {
			return "", errors.Join(err, rerr)
		}
		if err := git.WorktreeDiscard(ctx, e.d.Checkout, dir); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	if err := git.WorktreeAdd(ctx, e.d.Checkout, dir, sha, true, ""); err != nil {
		return "", err
	}
	head, err := git.RevParse(ctx, dir, "HEAD")
	if err != nil {
		return "", err
	}
	if head != sha {
		return "", fmt.Errorf("it is at %s, not at %s", learnShort(head), learnShort(sha))
	}
	return dir, nil
}

// dropGateBase removes the gate's worktree of the base; a failure is
// logged (the next gate replaces it).
func (e *Engine) dropGateBase(ctx context.Context) {
	if err := e.d.LearnGit.WorktreeDiscard(ctx, e.d.Checkout, e.gateBaseDir()); err != nil {
		e.log.Warn("skill gate: remove the worktree of the base", "err", err)
	}
}

// ownWorktree reports whether wt is a worktree directory of the loop's
// (a direct child of Layout.LearnWorktrees).
func (e *Engine) ownWorktree(wt string) bool {
	rel, err := filepath.Rel(e.d.Layout.LearnWorktrees(), wt)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !strings.Contains(rel, string(filepath.Separator))
}

// ensureCandidate makes sure p's worktree is there at its branch's commit:
// one a restart left behind is reused as it is; a missing one is added
// again from p's branch (a stale registration pruned first).
// errCandidateGone when the branch is gone, the worktree holds another
// commit than the branch, or the branch moved from p's commit: only the
// landing may have moved it, rebasing it onto master, and with moved such
// a head is accepted (rebasedCandidate), nothing else.
func (e *Engine) ensureCandidate(ctx context.Context, p store.LearnProposal, moved bool) error {
	git, wt := e.d.LearnGit, p.Scratch
	if !e.ownWorktree(wt) || p.CandidateSHA == "" || !strings.HasPrefix(p.Branch, learnBranchPrefix) {
		return fmt.Errorf("%w: proposal %d has no worktree of its own or no commit", errCandidateGone, p.ID)
	}
	sha, err := git.RevParse(ctx, e.d.Checkout, "refs/heads/"+p.Branch)
	switch {
	case errors.Is(err, gitx.ErrNoSuchRef):
		return fmt.Errorf("%w: its branch %s is gone", errCandidateGone, p.Branch)
	case err != nil:
		return err
	case sha != p.CandidateSHA && !moved:
		return fmt.Errorf("%w: its branch %s moved to %s from %s", errCandidateGone, p.Branch, learnShort(sha), learnShort(p.CandidateSHA))
	case sha != p.CandidateSHA:
		if err := e.rebasedCandidate(ctx, p, sha); err != nil {
			return err
		}
	}
	if _, err := os.Stat(wt); err == nil {
		head, err := git.RevParse(ctx, wt, "HEAD")
		if err != nil {
			return err
		}
		if head != sha {
			return fmt.Errorf("%w: its worktree is at %s, not at its branch's commit %s", errCandidateGone, learnShort(head), learnShort(sha))
		}
		return nil
	}
	if err := git.WorktreeDiscard(ctx, e.d.Checkout, wt); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(wt), 0o700); err != nil {
		return err
	}
	return git.WorktreeAdd(ctx, e.d.Checkout, wt, p.Branch, false, "")
}
