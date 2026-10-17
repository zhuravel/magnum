package engine

// The gate of a skill proposal (DECISIONS "The gate replays the learned
// cases"), the self-improving loop's fourth stage. A proposal the editor
// committed waits in gating; at a tick with nothing holding it the daemon
// replays eval cases on it in its own goroutine, one at a time: the learned
// cases of the inputs it noted (its targets, those no gate made anything of
// yet first, then the newest: gateTargets) and the newest other cases of
// both corpora, up to [learn] gate_cases (learn.SelectCases), the judge
// alone when only the judge skill and the judge prompts changed
// (learn.Mode). Each case's baseline is the newest stored replay with the
// prompts and skill of the proposal's base commit (eval.Baselines), else a
// replay of a detached worktree at that commit first (gateBaseName, removed
// after the gate: never the checkout, whose branch and uncommitted edits
// are the operator's); judge-only, both judges read the reviewer reports of
// the case's newest full replay whose reviewers read the candidate's
// prompts, so the baseline is that replay when the base's judge made it,
// else the base's judge alone on its reports (eval.JudgeBaselines). A
// target the baseline covers already (learn.AtBaseline: its miss found, its
// declined finding no longer reported) is no target: no candidate replays
// it, the next target past the cap takes its place, and the decision
// dismisses its input. The candidate replays with the proposal's worktree,
// and a case it lost is replayed once more. learn.Judge decides: passed (it
// lands, learn_land.go), held (the operator decides) or failed (its
// worktree and branch go, its inputs stay new but for one two failed gates
// did not improve, which is dismissed); a revert (learn_revert.go) is held
// whatever it decides. Every replay is stored as `magnum eval
// run` stores one, under runs of the gate's own (a baseline, a baseline
// judge, a candidate and a retry run, each with its agent tag), so a later
// gate and `magnum eval` read them. The replays run in-process through Deps.EvalCase
// (evalrun.RunCase, which the daemon's wiring sets: the engine cannot
// import evalrun, Amendment A1). The Codex soft cap, a usage limit, a
// shutdown, a drain or a pause (checked before each replay), and a replay
// that ended without a score where the verdict needs one leave the
// proposal gating for a later tick; the third gate in a row that stops on
// a replay without a score holds it (gateStopLimit), and the sixth stop
// short of a verdict holds it too (gateAttemptLimit), a drain or a pause
// not counting (waitsHeld).

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
	// gateAttemptLimit is how many gates of a proposal may stop short for
	// any reason but a shutdown or the daemon's own hold (the Codex budget,
	// a usage limit, a replay without a score, an error; not a drain or a
	// pause, which say nothing about the proposal) before the next such
	// stop holds it for the operator: a gate that never fits under the
	// budget would keep the loop waiting for good.
	gateAttemptLimit = 6
	// gateMaxAge is how long a proposal may stay gating, from when the
	// editor made it, before it is held for the operator whether or not a
	// gate could start (the editor waits behind it).
	gateMaxAge = 72 * time.Hour
	// gateStopWait bounds the wait of a rejection for the gate it stops.
	gateStopWait = 20 * time.Second
	// kvLearnGateStops counts the gates of a proposal that stopped short
	// since a gate last decided (gateStops as JSON).
	kvLearnGateStops = "learn.gate_stops"
	// gateReasonRunes clips the rule stored as a gated proposal's reason.
	gateReasonRunes = 2000
	// gateBaseName is the gate's detached worktree of the proposal's base
	// commit under Layout.LearnWorktrees, whose prompts and skill the
	// baseline replays (one gate runs at a time); no editor run is named
	// so, so the orphan cleanup leaves it to the gate, which removes it
	// when it ends, and to the next start (dropStaleGateBase).
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

// gateStops is kvLearnGateStops: Attempts gates of Proposal stopped short
// (Last says why the last did), Stops of them on a replay without a score.
type gateStops struct {
	Proposal int64  `json:"proposal"`
	Stops    int    `json:"stops"`
	Attempts int    `json:"attempts,omitempty"`
	Last     string `json:"last,omitempty"`
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
// (gateBudget); one gating for gateMaxAge is held for the operator instead,
// whatever holds its gate. A gating proposal a crashed or restarted daemon
// left behind is picked up the same way. A dry run and `magnum daemon
// --once` never start one.
func (e *Engine) maybeGate(ctx context.Context) {
	if e.d.DryRun || e.once || e.gateUnavailable() != "" || e.learnBusy() != "" {
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
	if e.now().Sub(p.CreatedAt) >= gateMaxAge {
		e.holdOldGate(ctx, p)
		return
	}
	if e.gateWaits() || e.gateHold(ctx) != "" {
		return
	}
	if level, snap := e.budgetLevel(); level >= usage.Soft {
		e.gateBudget(ctx, p, snap)
		return
	}
	e.startGate(ctx, p)
}

// holdOldGate holds proposal p, gating for gateMaxAge without a verdict,
// for the operator, naming why its last gate stopped (kvLearnGateStops) or
// that none could start; the result of its last gate, if any, stays.
func (e *Engine) holdOldGate(ctx context.Context, p store.LearnProposal) {
	why := "no gate of it could start (the Codex budget, a drain or a pause held every one)"
	if s, ok := e.gateStopsOf(ctx, p.ID); ok && s.Last != "" {
		why = fmt.Sprintf("%s stopped short, the last: %s", textx.Count(s.Attempts, "gate", "gates"), s.Last)
	}
	res := GateResult{State: learn.GateHeld, Mode: learn.Mode(p.Files), Rule: fmt.Sprintf("gating for %s without a verdict: %s",
		textx.Count(int(gateMaxAge/(24*time.Hour)), "day", "days"), why)}
	if err := e.gateDecide(context.WithoutCancel(ctx), p, res); err != nil {
		e.warnUnlessStopped(ctx, err, "skill gate: hold a proposal gating too long", "proposal", p.ID)
	}
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
	done := make(chan struct{})
	e.gateCancel, e.gateStarted, e.gateProposal, e.gateDone = cancel, e.now(), p.ID, done
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
			close(done)
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

// stopGateOf cancels the gate of proposal id if it runs and returns what is
// closed once it ended (nil when none runs for it).
func (e *Engine) stopGateOf(id int64) <-chan struct{} {
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	if e.gateCancel == nil || e.gateProposal != id {
		return nil
	}
	e.gateCancel()
	return e.gateDone
}

// gateReplay is one replay of the gate: its score, or why it has none:
// skip (the case is left out: its PR is flagged), missing (it ended
// without a score: herdr away, a GitHub read, a judge's timeout, a judge
// that read other reviewer reports than the gate chose) or stop (the gate
// must stop now: something holds it, gateHold, the budget, a usage limit,
// a login, a shutdown); held says the stop is the daemon's own hold
// (gateHold: a drain, a stop or a pause), which says nothing about the
// proposal and so does not count toward gateAttemptLimit (waitsHeld); the
// eval run it is stored in and, judge-only, the full replay whose reviewer
// reports its judge read.
type gateReplay struct {
	score               *eval.Score
	run, reports        string
	skip, missing, stop string
	held                bool
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
	for i, gc := range cases {
		row, stop, err := g.caseRow(gc)
		if stop {
			return err
		}
		if row.TargetGate() == store.LearnTargetBaseline { // no target: the next one past the cap takes its place
			learn.PromoteTarget(cases, i)
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
// learn.gate_waiting event and the error runGate returns. Every such stop
// but a shutdown's (or a rejection's) and the daemon's own hold's
// (waitsHeld) counts (countGateAttempt), and the gateAttemptLimit-th holds
// p for the operator instead, naming why.
func (g *gateWork) waits(why string) error {
	return g.stops(why, true)
}

// waitsHeld ends the gate short as waits does, for a stop that is the
// daemon's own hold (gateHold: a drain for a restart, a stop or a pause of
// the daemon or of a CLI): it neither counts toward gateAttemptLimit nor
// resets the count of the earlier stops, since a restart or a pause says
// nothing about the proposal and the operator's own restarts (the improve
// loop lands several commits a day) would hold it; gateMaxAge still bounds
// the proposal.
func (g *gateWork) waitsHeld(why string) error {
	return g.stops(why, false)
}

// stops is waits and waitsHeld: counted says whether the stop counts.
func (g *gateWork) stops(why string, counted bool) error {
	if counted && g.ctx.Err() == nil {
		if n := g.e.countGateAttempt(g.octx, g.p.ID, why); n >= gateAttemptLimit {
			g.res.State, g.res.Rule = learn.GateHeld, fmt.Sprintf("%d gates in a row stopped short without a verdict, the last: %s", n, why)
			g.res.Mode = cmp.Or(g.res.Mode, learn.Mode(g.p.Files))
			return g.e.gateDecide(g.octx, g.p, g.res)
		}
	}
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
		return gateReplay{stop: why, held: true}
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
		out.stop = "the gate was stopped (a shutdown or a rejection)"
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
	case rp.stop != "" && rp.held:
		return true, g.waitsHeld(rp.stop)
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
// replay, unless the baseline covers a target already (learn.AtBaseline:
// it is no target, and its input retires); stop when the gate ends there,
// with what runGate returns.
func (g *gateWork) caseRow(gc learn.GateCase) (GateCase, bool, error) {
	row := GateCase{GateRow: learn.GateRow{Case: gc.Case.Name, Target: gc.Target, Skip: gc.Skip}}
	if row.Skip != "" {
		return row, false, nil
	}
	if g.ctx.Err() != nil {
		return row, true, g.waits("the gate was stopped (a shutdown or a rejection)")
	}
	if stop, err := g.baseline(gc, &row); stop {
		return row, true, err
	}
	if why := learn.AtBaseline(row.Case, row.Before); gc.Target && row.Skip == "" && why != "" {
		row.Skip = why
		return row, false, nil
	}
	if b, ok := g.storedCandidate(gc.Case, row.ReportsRun, ""); ok && row.Skip == "" { // a gate that stopped short made it
		row.After, row.CandidateRun = trimScore(b.Case.Score), b.Run
		return row, false, nil
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

// storedCandidate is the oldest stored replay of case c with the
// candidate's inputs other than run skip's, which an earlier gate of the
// proposal that stopped short made (or one of another with the same texts:
// the inputs hash the content): eval.Baselines over clean runs, judge-only
// eval.JudgeBaselines on the reviewer reports of run reports. ok is false
// when there is none.
func (g *gateWork) storedCandidate(c eval.Case, reports, skip string) (eval.Baseline, bool) {
	bs := eval.Baselines(g.runs, g.candIn, c)
	if g.judgeOnly {
		bs = eval.JudgeBaselines(g.runs, g.candIn, c, reports)
	}
	for _, b := range slices.Backward(bs) { // newest first: the oldest is the first replay
		if b.Run != skip {
			return b, true
		}
	}
	return eval.Baseline{}, false
}

// retryLost replays a case the candidate lost once more before the loss
// counts (a stored second replay of an earlier gate's counts as one); stop
// when the gate ends there, with what runGate returns.
func (g *gateWork) retryLost(cases []learn.GateCase) (bool, error) {
	for i := range g.res.Cases {
		row := &g.res.Cases[i]
		if len(row.Lost()) == 0 {
			continue
		}
		c := cases[slices.IndexFunc(cases, func(gc learn.GateCase) bool { return gc.Case.Name == row.Case })].Case
		if b, ok := g.storedCandidate(c, row.ReportsRun, row.CandidateRun); ok { // a gate that stopped short made it
			row.Retried, row.After, row.CandidateRun = true, trimScore(b.Case.Score), b.Run
			continue
		}
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
	return e.countGateStops(ctx, id, func(s *gateStops) int { s.Stops++; return s.Stops })
}

// countGateAttempt counts one more gate of proposal id that stopped short,
// why, and returns how many did since a gate last decided.
func (e *Engine) countGateAttempt(ctx context.Context, id int64, why string) int {
	return e.countGateStops(ctx, id, func(s *gateStops) int { s.Attempts, s.Last = s.Attempts+1, oneLine(why, 300); return s.Attempts })
}

// countGateStops applies count to proposal id's kvLearnGateStops (a mark of
// another proposal's starts over) and stores it.
func (e *Engine) countGateStops(ctx context.Context, id int64, count func(*gateStops) int) int {
	s, ok := e.gateStopsOf(ctx, id)
	if !ok {
		s = gateStops{Proposal: id}
	}
	n := count(&s)
	if b, err := json.Marshal(s); err == nil {
		e.setKV(ctx, kvLearnGateStops, string(b))
	}
	return n
}

// gateStopsOf reads kvLearnGateStops when it counts proposal id's gates.
func (e *Engine) gateStopsOf(ctx context.Context, id int64) (gateStops, bool) {
	var s gateStops
	if v, ok := e.getKV(ctx, kvLearnGateStops); !ok || json.Unmarshal([]byte(v), &s) != nil || s.Proposal != id {
		return gateStops{}, false
	}
	return s, true
}

// gateDecide stores the gate's result on p and moves it from gating to the
// verdict's state, with what it made of each target it replayed or found
// covered at the baseline (store.GateLearnProposal, which retires the
// inputs the baseline covers and dismisses one two failed gates did not
// improve, and the learned corpus follows): a failed one loses its worktree
// and branch (its other inputs stay new), a held one waits for the operator
// with a toast, a passed one for the landing. An error when the registry
// did not take it: p stays gating, and the gate that stopped short waits
// out gateRetryAfter.
func (e *Engine) gateDecide(ctx context.Context, p store.LearnProposal, res GateResult) error {
	if res.Cases == nil {
		res.Cases = []GateCase{}
	}
	b, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("skill gate: encode the result of proposal %d: %w", p.ID, err)
	}
	targets, err := e.gateTargetOutcomes(ctx, p, res.Cases)
	if err != nil {
		return fmt.Errorf("skill gate: the inputs of proposal %d: %w", p.ID, err)
	}
	got, err := e.st.GateLearnProposal(ctx, p.ID, res.State, textx.Clip(res.Rule, gateReasonRunes), e.now(), b, targets)
	if err != nil {
		e.log.Warn("skill gate: decide the proposal", "proposal", p.ID, "state", res.State, "err", err)
		return fmt.Errorf("skill gate: decide proposal %d: %w", p.ID, err)
	}
	if res.State == store.LearnFailed || slices.ContainsFunc(targets, func(t store.LearnTarget) bool { return t.Gate == store.LearnTargetBaseline }) {
		if err := e.syncLearnedCorpus(ctx); err != nil { // an input it dismissed leaves the corpus
			e.log.Warn("skill gate: rewrite the learned corpus after a decision", "proposal", p.ID, "err", err)
		}
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

// gateTargetOutcomes is what the gate made of the learned case of each
// input p noted, from its rows (learn.GateRow.TargetGate): covered at the
// baseline, improved or not; a target it did not compare is left out.
func (e *Engine) gateTargetOutcomes(ctx context.Context, p store.LearnProposal, rows []GateCase) ([]store.LearnTarget, error) {
	if !slices.ContainsFunc(rows, func(c GateCase) bool { return c.TargetGate() != "" }) {
		return nil, nil
	}
	ins, err := e.st.LearnProposalInputs(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	var out []store.LearnTarget
	for _, c := range rows {
		gate := c.TargetGate()
		i := slices.IndexFunc(ins, func(in store.LearnInput) bool {
			return in.Outcome == store.MissNoted && learn.TargetName(in.Kind, in.InputID) == c.Case
		})
		if gate != "" && i >= 0 {
			out = append(out, store.LearnTarget{Kind: ins[i].Kind, InputID: ins[i].InputID, Gate: gate})
		}
	}
	return out, nil
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
// (learn.TargetName): first those no gate made anything of yet
// (store.LearnInput.Gated), then the others, each the misses first and the
// newest first, so the oldest inputs, an earlier gate's included, do not
// take every place of every gate (learn.SelectCases replays them in this
// order, up to [learn] gate_cases).
func (e *Engine) gateTargets(ctx context.Context, p store.LearnProposal) ([]string, error) {
	ins, err := e.st.LearnProposalInputs(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	ins = slices.DeleteFunc(ins, func(in store.LearnInput) bool { return in.Outcome != store.MissNoted })
	slices.SortStableFunc(ins, func(a, b store.LearnInput) int {
		return cmp.Or(cmp.Compare(min(a.Gated, 1), min(b.Gated, 1)), cmp.Compare(gateKindRank(a.Kind), gateKindRank(b.Kind)),
			cmp.Compare(b.InputID, a.InputID))
	})
	out := make([]string, 0, len(ins))
	for _, in := range ins {
		out = append(out, learn.TargetName(in.Kind, in.InputID))
	}
	return out, nil
}

// gateKindRank orders the targets' kinds: the misses first.
func gateKindRank(kind string) int {
	if kind == store.LearnInputMiss {
		return 0
	}
	return 1
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
	if err := git.WorktreeDiscard(ctx, e.d.Checkout, e.d.Layout.LearnWorktrees(), dir); err != nil {
		// A directory git does not know as a worktree: it is the gate's own.
		if rerr := os.RemoveAll(dir); rerr != nil {
			return "", errors.Join(err, rerr)
		}
		if err := git.WorktreeDiscard(ctx, e.d.Checkout, e.d.Layout.LearnWorktrees(), dir); err != nil {
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
	if err := e.d.LearnGit.WorktreeDiscard(ctx, e.d.Checkout, e.d.Layout.LearnWorktrees(), e.gateBaseDir()); err != nil {
		e.log.Warn("skill gate: remove the worktree of the base", "err", err)
	}
}

// dropStaleGateBase removes the gate's worktree of the base at startup,
// when a daemon that crashed or was killed mid-gate left it (no gate runs
// yet); a dry run and a daemon without a checkout touch nothing.
func (e *Engine) dropStaleGateBase(ctx context.Context) {
	if e.d.DryRun || e.d.LearnGit == nil || e.d.Checkout == "" || !e.d.Layout.Valid() {
		return
	}
	if _, err := os.Lstat(e.gateBaseDir()); err == nil {
		e.log.Info("skill gate: removing the worktree of the base a stopped daemon left", "worktree", e.gateBaseDir())
		e.dropGateBase(ctx)
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
// again from p's branch (the stale registrations of the loop's worktrees
// forgotten first, gitx.WorktreeDiscard).
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
	if err := git.WorktreeDiscard(ctx, e.d.Checkout, e.d.Layout.LearnWorktrees(), wt); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(wt), 0o700); err != nil {
		return err
	}
	return git.WorktreeAdd(ctx, e.d.Checkout, wt, p.Branch, false, "")
}
