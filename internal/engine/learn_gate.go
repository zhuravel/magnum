package engine

// The gate of a skill proposal (DECISIONS "The gate replays the learned
// cases"), the self-improving loop's fourth stage. A proposal the editor
// committed waits in gating; at a tick with nothing holding it the daemon
// replays eval cases on it in its own goroutine, one at a time: the learned
// cases of the inputs it noted (its targets, those no gate made anything of
// yet first, then the newest: gateTargets), up to [learn] gate_guards
// guards on top (cases whose newest replay made before the proposal, a gate's
// replays of its candidate left out, found a defect, the least recently
// replayed first: learn.GuardsFrom; judge-only,
// those with a full replay of the candidate's reviewers) and the newest other cases of
// both corpora, up to [learn] gate_cases (learn.SelectCases), the judge
// alone when only the judge skill and the judge prompts changed
// (learn.Mode). A guard's baseline is that newest replay while the
// candidate finds what it found (guardBaseline), and the base's score once
// the candidate lost it twice (guardsAtBase); a miss target the candidate
// found is replayed once more before the win counts (confirmWins). Each
// other case's baseline is the newest stored replay with the
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
// short of a verdict holds it too (gateAttemptLimit), a drain, a pause, a
// held kind and a replay a CLI's outage ended (outageOf: a launch failure
// shared across PRs) not counting (waitsHeld).
//
// A local case, a round of the factory's local review (eval.KindLocal), takes
// only a guard place (learn.Guards.Local), when the daemon reads the local
// cases' corpus (Deps.EvalLocalCorpus) and the candidate changes what a
// local round reads (its local inputs, Deps.EvalLocalInputs, are not the
// base's: withLocal). It replays with every role, also in a judge-only gate,
// in candidate runs of its own (learn.GateRunLocal, GateRunLocalSecond), and
// is matched on its round's inputs. A loss counts only once two replays of
// the base made after the one that chose it found what both candidate
// replays lost (localBase): one local replay is a coin flip. The local
// guards replay last, and only when the gate does not fail without them
// (localGuards): a guard can only add a loss. A local replay that ends
// config_error (a role gone, no identity), a refusal or a flag is never a
// loss nor a stop; one that lost a reviewer's report or whose worktree
// could not be prepared is no score, as for a posted case: the row is left
// out on the candidate's first replay, and the gate stops on its second or
// the base's.

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

	"github.com/zhuravel/magnum/internal/agents"
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
	// for good. A replay a CLI's outage ended (outageOf) is no such stop.
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
// eval runs its two scores come from (a guard's stored baseline's run, or
// the base's once the candidate lost it twice), the run of a won miss
// target's second replay and, judge-only, the full replay whose reviewer
// reports both judges read.
type GateCase struct {
	learn.GateRow
	BaselineRun  string `json:"baseline_run,omitempty"`
	CandidateRun string `json:"candidate_run,omitempty"` // a second replay's when Retried
	SecondRun    string `json:"second_run,omitempty"`    // a won miss target's second replay (learn.GateRow.Confirm)
	ReportsRun   string `json:"reports_run,omitempty"`
}

// GateResult is what a proposal's gate decided (learn_proposals.gate_json):
// the verdict and its rule, the mode, the inputs hashes the candidate and
// the baseline replayed with (eval.Run.Inputs), every case, the eval runs
// the gate stored and why the corpus could not be read, if it could not;
// Local says why the gate took no local case or left some out (withLocal:
// the local cases' corpus cannot be read, the candidate changes nothing a
// local round reads, a case it cannot take), "" when it says nothing.
type GateResult struct {
	State          string     `json:"state"`
	Rule           string     `json:"rule"`
	Mode           string     `json:"mode"`
	Inputs         string     `json:"inputs,omitempty"`
	BaselineInputs string     `json:"baseline_inputs,omitempty"`
	Cases          []GateCase `json:"cases"`
	Runs           []string   `json:"runs,omitempty"`
	CorpusError    string     `json:"corpus_error,omitempty"`
	Local          string     `json:"local,omitempty"`
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
// pause of a reviewing CLI, the hold of a reviewing CLI's kind for a round
// (kindHoldGate: its span runs, or a round probes it; once the span is
// over and no round probes it, the gate replays as any round may), or the
// Codex soft cap (gateBudget tells).
func (e *Engine) gateHold(ctx context.Context) string {
	if why := cmp.Or(e.holdReason(ctx), e.userPause(ctx)); why != "" {
		return why
	}
	roles := e.cfg.RolesFor(nil)
	var kinds []string
	for _, r := range roles {
		if k := r.AgentKind(); k != "" && !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	if why := e.kindPauseReason(ctx, kinds); why != "" {
		return why
	}
	if kind, why := e.kindHoldGate(ctx, roles, 0); why != "" {
		return why + "; " + resumeToolHint(kind)
	}
	return ""
}

// gateKindHeld is the hold of the first agent kind the gate's roles use
// that a shared launch failure holds, whatever its span (kindHoldOf):
// "codex held (shared launch failure) since 18:22"; "" = none.
func (e *Engine) gateKindHeld(ctx context.Context) string {
	for _, k := range agentRoleKinds(e.cfg.RolesFor(nil)) {
		if h, ok := e.kindHoldOf(ctx, k); ok {
			return fmt.Sprintf("%s held (shared launch failure) since %s", k, h.Since.Local().Format("15:04"))
		}
	}
	return ""
}

// sharedLaunchLine reports whether line, the pane line a replay of the
// case of the PR pr ended with, is a launch failure shared across PRs: one
// agents.shared_failure named within sharedFailureQuiet, or one another
// PR's agent ended with within sharedFailureWindow (the replay's own PR
// makes the second, as noteSharedFailures counts PRs; a line only that PR
// ended with is its own).
func (e *Engine) sharedLaunchLine(ctx context.Context, line, pr string) bool {
	if line == "" {
		return false
	}
	now := e.now()
	if e.sharedFailuresSaid(ctx, now.Add(-sharedFailureQuiet))[line] {
		return true
	}
	pr = strings.ToLower(pr)
	return slices.ContainsFunc(e.launchLines(ctx, now.Add(-sharedFailureWindow)), func(l launchLine) bool {
		return l.line == line && l.pr != pr
	})
}

// outageOf is why the replay cr of case c (label: a candidate, a baseline,
// ...), which ended without a score, why, is a CLI's outage and not the
// replay's own failure: a kind the gate replays is held (gateKindHeld: a
// shared launch failure held it, maybe while the replay ran), or cr ended
// on a launch failure shared across PRs (sharedLaunchLine). Such a stop is
// a delay (gateReplay.held), never a stop toward gateStopLimit or
// gateAttemptLimit: on 2026-11-05 a Codex outage ended every replay of
// skill proposal 17's gates, and three of them held it for the operator,
// unscored. "" = neither.
func (g *gateWork) outageOf(c eval.Case, cr eval.CaseRun, label, why string) string {
	if held := g.e.gateKindHeld(g.ctx); held != "" {
		return fmt.Sprintf("the %s replay of %s ended %s while %s: %s", label, c.Name, cr.Outcome, held, why)
	}
	if line := agents.PaneErrorOf(cr.Error); g.e.sharedLaunchLine(g.ctx, line, c.PR) {
		return fmt.Sprintf("the %s replay of %s ended %s on a launch failure shared across PRs: %s", label, c.Name, cr.Outcome, line)
	}
	return ""
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
	why := "no gate of it could start or finish (the Codex budget, a drain, a pause, a held kind or a launch failure shared across PRs held every one)"
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
// a login, a shutdown, a CLI's outage); held says the stop is the daemon's
// own hold (gateHold: a drain, a stop, a pause or a held kind) or a CLI's
// outage (outageOf), which say nothing about the proposal and so count
// toward neither gateStopLimit nor gateAttemptLimit (waitsHeld); the
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
// and a candidate replay of each (a second one of a lost case; the local
// guards last, only when the gate does not fail without them:
// localGuards), the verdict and the proposal's next state. It returns an
// error when the gate stopped short and p stays gating: a drain or a pause
// before a replay (gateHold), a replay at the soft cap or a usage limit, a
// replay without a score that the verdict needs (noScore), a shutdown, an
// unreadable registry, inputs or runs, a verdict the registry did not take.
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
	reviewers := "" // judge-only, a guard needs a full replay whose reviewers read the candidate's prompts
	if g.judgeOnly {
		reviewers = g.candRev
	}
	corpus, local := g.withLocal(corpus)
	g.guards = learn.GuardsFrom(g.runs, e.cfg.Learn.GateGuards, g.candIn, p.CreatedAt, reviewers)
	g.guards.Local = local
	cases := learn.SelectCases(corpus, targets, e.gateWatched, flagged, e.cfg.Learn.GateCases, g.guards)
	guards := len(slices.DeleteFunc(slices.Clone(cases), func(gc learn.GateCase) bool { return !gc.Guard }))
	locals := len(slices.DeleteFunc(slices.Clone(cases), func(gc learn.GateCase) bool { return !gc.Guard || !gc.Case.Local() }))
	started := fmt.Sprintf("the gate of skill proposal %d started: %s, %s (%s)", p.ID, textx.Count(len(cases), "case", "cases"), strings.Join(targets, ", "), mode)
	if locals > 0 {
		started += fmt.Sprintf(", %s replayed with every role", textx.Count(locals, "local guard", "local guards"))
	}
	e.event(ctx, "info", learnSubject, "learn.gate_start", started,
		map[string]any{"proposal": p.ID, "cases": len(cases), "guards": guards, "local_guards": locals, "mode": mode})

	defer g.finishRuns()
	var held []heldGuard // the local guards, replayed last (localGuards)
	var others []int     // the other rows
	for i, gc := range cases {
		if gc.Guard && gc.Case.Local() && gc.Skip == "" { // its place in the rows is kept, saying why it has no score yet
			held = append(held, heldGuard{row: len(g.res.Cases), gc: gc})
			g.res.Cases = append(g.res.Cases, GateCase{GateRow: learn.GateRow{Case: gc.Case.Name, Guard: true, Local: true, Skip: skipStoppedBefore}})
			continue
		}
		row, stop, err := g.caseRow(gc)
		if stop {
			return err
		}
		if row.TargetGate() == store.LearnTargetBaseline { // no target: the next one past the cap takes its place
			learn.PromoteTarget(cases, i)
		}
		others = append(others, len(g.res.Cases))
		g.res.Cases = append(g.res.Cases, row)
	}
	if stop, err := g.retryLost(cases, others); stop {
		return err
	}
	if stop, err := g.guardsAtBase(cases, others); stop {
		return err
	}
	if stop, err := g.confirmWins(cases); stop {
		return err
	}
	if stop, err := g.localGuards(cases, held); stop {
		return err
	}
	return g.decide()
}

// heldGuard is a local guard runGate replays last (localGuards): its row
// of the result and its case.
type heldGuard struct {
	row int
	gc  learn.GateCase
}

// skipFailsWithout is why a local guard of a gate its other cases fail is
// not replayed (localGuards).
const skipFailsWithout = "not replayed: the gate fails on its other cases, and a guard can only add a loss"

// skipStoppedBefore is why a local guard's row has no score when the gate
// stopped before the local guards were done (before localGuards, or at an
// earlier guard's or this one's replay), as a stop that holds the proposal
// stores the rows; the guard's own row (caseRow) or skipFailsWithout
// replaces it when the gate gets there.
const skipStoppedBefore = "not scored: the gate stopped before its local guards were done"

// localGuards replays the local guards held, last: each one's row
// (caseRow), the candidate's second replay of one it lost (retryLost) and
// the base's (guardsAtBase, localBase). A guard can only add a loss
// (learn.Judge), and a local one costs up to four replays with every role
// of 30 to 47 minutes each, so in a gate the other cases fail already
// (their targets did not improve, a win its second replay did not find
// again, a regression) none is replayed: its row is skipped
// (skipFailsWithout). A revert, which waits for the operator whatever the
// gate finds, replays them, since the operator reads its guards. Stop when
// the gate ends there, with what runGate returns.
func (g *gateWork) localGuards(cases []learn.GateCase, held []heldGuard) (bool, error) {
	if len(held) == 0 {
		return false, nil
	}
	if learn.Judge(g.rows()).State == learn.GateFailed && g.p.Reverts == 0 {
		for _, h := range held {
			g.res.Cases[h.row].Skip = skipFailsWithout
		}
		return false, nil
	}
	rows := make([]int, 0, len(held))
	for _, h := range held {
		row, stop, err := g.caseRow(h.gc)
		if stop {
			return true, err
		}
		g.res.Cases[h.row] = row
		rows = append(rows, h.row)
	}
	if stop, err := g.retryLost(cases, rows); stop {
		return true, err
	}
	return g.guardsAtBase(cases, rows)
}

// rows are the gate's rows so far.
func (g *gateWork) rows() []learn.GateRow {
	rows := make([]learn.GateRow, len(g.res.Cases))
	for i, c := range g.res.Cases {
		rows[i] = c.GateRow
	}
	return rows
}

// gateWork is one gate's state (runGate): the proposal, what its base and
// its candidate replay with, the stored runs as the gate last read them,
// its guard places (learn.GuardsFrom, over the runs as it first read them),
// the eval runs it made, one per kind of replay, and its result so far.
type gateWork struct {
	e         *Engine
	ctx, octx context.Context
	p         store.LearnProposal
	res       GateResult
	runs      []eval.Run
	guards    learn.Guards

	corpusPath, baseDir              string
	baseIn, baseRev, candIn, candRev string
	judgeOnly                        bool
	// baseLocal and candLocal are what a local round reads with the base
	// and with the candidate (Deps.EvalLocalInputs), which a local case's
	// replays are matched on.
	baseLocal, candLocal string

	base, baseJudge, cand, retry *gateRun
	// local and localSecond are a local guard's candidate replays, with
	// every role (learn.GateRunLocal, GateRunLocalSecond); baseSecond the
	// base's second replay of one the candidate lost twice
	// (learn.GateRunBaseSecond: a run holds one replay of a case).
	local, localSecond, baseSecond *gateRun
}

// judgeOnlyFor reports whether case c replays with the judge alone: in a
// judge-only gate, but never a local case (a local round runs every role).
func (g *gateWork) judgeOnlyFor(c eval.Case) bool { return g.judgeOnly && !c.Local() }

// candInFor and baseInFor are the inputs c's replays with the candidate and
// with the base are matched on (eval.CaseInputs): a local case's round's.
func (g *gateWork) candInFor(c eval.Case) string {
	if c.Local() {
		return g.candLocal
	}
	return g.candIn
}

func (g *gateWork) baseInFor(c eval.Case) string {
	if c.Local() {
		return g.baseLocal
	}
	return g.baseIn
}

// candSlot is the run and the label of case c's candidate replay (second:
// its second one): a local case's go to runs of their own.
func (g *gateWork) candSlot(c eval.Case, second bool) (**gateRun, string) {
	switch {
	case c.Local() && second:
		return &g.localSecond, learn.GateRunLocalSecond
	case c.Local():
		return &g.local, learn.GateRunLocal
	case second:
		return &g.retry, learn.GateRunSecond
	}
	return &g.cand, learn.GateRunCandidate
}

// withLocal is corpus with the local cases of the local cases' corpus
// (Deps.EvalLocalCorpus) and whether the gate takes them, as guards only
// (learn.Guards.Local): when the daemon reads them, the corpus can be read
// and the candidate changes what a local round reads (Deps.EvalLocalInputs:
// with the same local inputs as the base's, a round would read what it
// read at the base). A case whose name corpus has, one that is no local
// case, and one evalrun.Refused names (Deps.EvalRefused: a task Codex must
// not see again, an earlier replay refused) are left out. GateResult.Local
// says what it left out and why; nothing of this stops the gate, since the
// operator edits that file by hand and a typo must not hold every proposal.
func (g *gateWork) withLocal(corpus *eval.Corpus) (*eval.Corpus, bool) {
	e := g.e
	if e.d.EvalLocalCorpus == nil || e.d.EvalLocalInputs == nil {
		return corpus, false
	}
	lc, err := e.d.EvalLocalCorpus()
	if err != nil {
		g.res.Local = "the local cases' corpus cannot be read, so no local case is replayed: " + oneLine(err.Error(), retroWhyRunes)
		return corpus, false
	}
	if lc == nil || len(lc.Cases) == 0 {
		return corpus, false
	}
	if g.baseLocal, err = e.d.EvalLocalInputs(g.baseDir); err == nil {
		g.candLocal, err = e.d.EvalLocalInputs(g.p.Scratch)
	}
	switch {
	case err != nil:
		g.res.Local = "what a local round reads cannot be hashed, so no local case is replayed: " + oneLine(err.Error(), retroWhyRunes)
		return corpus, false
	case g.candLocal == g.baseLocal:
		g.res.Local = "the candidate changes nothing a local round reads (its local inputs are the base's), so no local case is replayed"
		return corpus, false
	}
	out := &eval.Corpus{}
	if corpus != nil {
		out.Cases = slices.Clone(corpus.Cases)
	}
	var left []string
	for _, c := range lc.Cases {
		why := ""
		switch {
		case !c.Local():
			why = "no local case"
		case slices.ContainsFunc(out.Cases, func(o eval.Case) bool { return o.Name == c.Name }):
			why = "a name the corpus has"
		case e.d.EvalRefused != nil:
			why = e.d.EvalRefused(g.ctx, c)
		}
		if why != "" {
			left = append(left, c.Name+" ("+oneLine(why, 300)+")")
			continue
		}
		out.Cases = append(out.Cases, c)
	}
	if len(left) > 0 {
		g.res.Local = "local cases left out: " + strings.Join(left, "; ")
	}
	return out, true
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
// the daemon or of a CLI, a held kind) or a CLI's outage (outageOf): it
// neither counts toward gateAttemptLimit (nor gateStopLimit) nor
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
	r := &gateRun{dir: rdir, tag: eval.AgentTag(id), run: eval.Run{ID: id, Label: learn.GateRunLabel(g.p.ID, label),
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
	for _, r := range []*gateRun{g.base, g.baseJudge, g.cand, g.retry, g.local, g.localSecond, g.baseSecond} {
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
	case c.Local() && cr.Outcome == pipeline.OutcomeRefused: // flagged by the replay (evalrun.RunCase)
		out.skip = learn.SkipFlagged
		g.localRefused(c, why)
	case cr.Score == nil:
		if stop := g.outageOf(c, cr, label, why); stop != "" { // a delay, whether or not the verdict needs the score
			out.stop, out.held = stop, true
			break
		}
		if c.Local() && cr.Outcome == eval.OutcomeConfigError {
			// A frozen role the config no longer declares, no gh identity: no
			// replay scores the round until the config changes, so never a
			// loss, never a stop. A lost reviewer's report (missing_role) or a
			// worktree that could not be prepared (environment) can pass on a
			// later try, so they are no score, as for a posted case.
			out.skip = fmt.Sprintf("the %s replay ended %s, which scores no round until the config changes: %s", label, cr.Outcome, why)
			break
		}
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

// caseRow is case gc's row: its baseline (baseline; a guard's stored one,
// guardBaseline), then the candidate's replay, unless the baseline covers a
// target already (learn.AtBaseline: it is no target, and its input
// retires); stop when the gate ends there, with what runGate returns.
func (g *gateWork) caseRow(gc learn.GateCase) (GateCase, bool, error) {
	row := GateCase{GateRow: learn.GateRow{Case: gc.Case.Name, Target: gc.Target, Guard: gc.Guard, Skip: gc.Skip, Local: gc.Case.Local()}}
	if row.Skip != "" {
		return row, false, nil
	}
	if g.ctx.Err() != nil {
		return row, true, g.waits("the gate was stopped (a shutdown or a rejection)")
	}
	if gc.Guard {
		g.guardBaseline(gc, &row)
	} else if stop, err := g.baseline(gc, &row); stop {
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
		slot, label := g.candSlot(gc.Case, false)
		rp := g.replay(slot, label, g.p.Scratch, g.judgeOnlyFor(gc.Case), g.candIn, g.candRev, gc.Case, row.ReportsRun)
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
	judgeOnly, baseIn := g.judgeOnlyFor(gc.Case), g.baseInFor(gc.Case)
	fr, fc, full := eval.FullReplayFor(g.runs, gc.Case.Name, gc.Case.Head, &eval.Run{ReviewerInputs: g.candRev})
	switch {
	case judgeOnly && full && fc.ReportDir != "":
		row.ReportsRun = fr.ID
		if bs := eval.JudgeBaselines(g.runs, g.baseIn, gc.Case, fr.ID); len(bs) > 0 {
			row.Before, row.BaselineRun = trimScore(bs[0].Case.Score), bs[0].Run
			return false, nil
		}
		rp := g.replay(&g.baseJudge, learn.GateRunBaseJudge, g.baseDir, true, g.baseIn, g.baseRev, gc.Case, fr.ID)
		if stop, err := g.halt(rp, gc.Target); stop {
			return true, err
		}
		row.Before, row.BaselineRun, row.Skip = rp.score, rp.run, cmp.Or(rp.skip, rp.missing)
		return false, nil
	case judgeOnly && len(eval.Baselines(g.runs, g.baseIn, gc.Case)) > 0:
		row.Skip = noFullReplay
		return false, nil
	}
	if bs := eval.Baselines(g.runs, baseIn, gc.Case); len(bs) > 0 {
		row.Before, row.BaselineRun = trimScore(bs[0].Case.Score), bs[0].Run
		return false, nil
	}
	rp := g.replay(&g.base, learn.GateRunBase, g.baseDir, false, g.baseIn, g.baseRev, gc.Case, "")
	if stop, err := g.halt(rp, gc.Target); stop {
		return true, err
	}
	row.Before, row.BaselineRun, row.Skip = rp.score, rp.run, cmp.Or(rp.skip, rp.missing)
	if judgeOnly && row.Skip == "" { // its reviewers' reports are the candidate's judge's
		if fr, fc, ok := eval.FullReplayFor(g.runs, gc.Case.Name, gc.Case.Head, &eval.Run{ReviewerInputs: g.candRev}); ok && fc.ReportDir != "" &&
			fr.ID == rp.run {
			row.ReportsRun = fr.ID
		} else {
			row.Skip = noFullReplay
		}
	}
	return false, nil
}

// guardBaseline sets guard row's baseline lazily (DECISIONS "The gate
// guards what master finds"): the replay learn.GuardsFrom chose the guard
// by (Guards.Replay: the newest made before the proposal by the operator's
// runs or a gate's base, no gate's replay of its candidate, so every gate of
// the proposal takes the same one), of whatever inputs,
// which the candidate must find again (Stored; the note
// names the replay and its inputs). The base's score is taken only once
// the candidate lost one of its defects twice (guardsAtBase). Judge-only,
// the candidate's judge reads the reviewer reports of the case's newest
// full replay whose reviewers read the candidate's prompts (learn.GuardsFrom
// took no case without one); a guard without one is left out
// (noFullReplay): no full replay is made for a guard.
func (g *gateWork) guardBaseline(gc learn.GateCase, row *GateCase) {
	ref, ok := g.guards.Replay(gc.Case)
	if !ok { // learn.GuardsFrom chose it by this replay
		row.Skip = "no stored replay to guard"
		return
	}
	if g.judgeOnlyFor(gc.Case) {
		fr, fc, full := eval.FullReplayFor(g.runs, gc.Case.Name, gc.Case.Head, &eval.Run{ReviewerInputs: g.candRev})
		if !full || fc.ReportDir == "" {
			row.Skip = noFullReplay
			return
		}
		row.ReportsRun = fr.ID
	}
	row.Before, row.BaselineRun, row.Stored = trimScore(ref.Case.Score), ref.Run, true
	row.Note = fmt.Sprintf("a guard: its baseline is the newest stored replay, run %s with inputs %s, not the base's", ref.Run,
		cmp.Or(ref.Inputs, "not recorded"))
}

// storedCandidate is the oldest stored replay of case c with the
// candidate's inputs other than run skip's, which an earlier gate of the
// proposal that stopped short made (or one of another with the same texts:
// the inputs hash the content): eval.Baselines over clean runs, judge-only
// eval.JudgeBaselines on the reviewer reports of run reports. ok is false
// when there is none.
func (g *gateWork) storedCandidate(c eval.Case, reports, skip string) (eval.Baseline, bool) {
	bs := eval.Baselines(g.runs, g.candInFor(c), c)
	if g.judgeOnlyFor(c) {
		bs = eval.JudgeBaselines(g.runs, g.candIn, c, reports)
	}
	for _, b := range slices.Backward(bs) { // newest first: the oldest is the first replay
		if b.Run != skip {
			return b, true
		}
	}
	return eval.Baseline{}, false
}

// caseNamed is the case of the gate named name.
func caseNamed(cases []learn.GateCase, name string) eval.Case {
	return cases[slices.IndexFunc(cases, func(gc learn.GateCase) bool { return gc.Case.Name == name })].Case
}

// second is the candidate's second replay of row's case c: a stored one an
// earlier gate that stopped short made, else a replay now, whose score the
// verdict needs (a replay without one stops the gate); skip when it was
// left out (its PR is flagged).
func (g *gateWork) second(c eval.Case, row *GateCase) (score *eval.Score, run, skip string, stop bool, err error) {
	if b, ok := g.storedCandidate(c, row.ReportsRun, row.CandidateRun); ok {
		return trimScore(b.Case.Score), b.Run, "", false, nil
	}
	slot, label := g.candSlot(c, true)
	rp := g.replay(slot, label, g.p.Scratch, g.judgeOnlyFor(c), g.candIn, g.candRev, c, row.ReportsRun)
	if stop, err := g.halt(rp, true); stop {
		return nil, "", "", true, err
	}
	return rp.score, rp.run, rp.skip, false, nil
}

// retryLost replays a case of rows (indexes of the gate's rows) the
// candidate lost once more before the loss counts (second), keeping the
// first replay's score (learn.GateRow.First: a loss counts for a defect
// both lost); stop when the gate ends there, with what runGate returns.
func (g *gateWork) retryLost(cases []learn.GateCase, rows []int) (bool, error) {
	for _, i := range rows {
		row := &g.res.Cases[i]
		if len(row.Lost()) == 0 {
			continue
		}
		score, run, skip, stop, err := g.second(caseNamed(cases, row.Case), row)
		switch {
		case stop:
			return true, err
		case skip != "":
			row.Skip = skip
		default:
			row.First = row.After
			row.Retried, row.After, row.CandidateRun = true, score, run
		}
	}
	return false, nil
}

// guardsAtBase decides each guard the candidate lost a defect of on both
// replays against its stored baseline (learn.GateRow.Stored) on the base's
// score: a stored replay of the base made no earlier than that baseline
// (storedBase: the baseline itself when the base made it, or one an earlier
// gate of the proposal that stopped short made), else a replay of the base
// now, so an older replay of the base never stands in for a newer one that
// found more. A defect the base finds and the candidate lost is a
// regression (learn.Judge); a guard the base lost too is skipped
// (learn.GateRow.AtBase: not the candidate's loss) with a
// learn.guard_lost_at_base warning and a toast (lostAtBase). A local guard
// takes two replays of the base instead (localBase). Stop when the gate
// ends there, with what runGate returns. rows are the indexes of the
// gate's rows it looks at.
func (g *gateWork) guardsAtBase(cases []learn.GateCase, rows []int) (bool, error) {
	for _, i := range rows {
		row := &g.res.Cases[i]
		if !row.Guard || !row.Stored || !row.Retried || row.Skip != "" || len(row.Lost()) == 0 {
			continue
		}
		c, ref := caseNamed(cases, row.Case), row.BaselineRun
		if c.Local() {
			if stop, err := g.localBase(c, row); stop {
				return true, err
			}
			continue
		}
		base, run := g.storedBase(c, row.ReportsRun)
		if base == nil {
			var rp gateReplay
			if g.judgeOnly {
				rp = g.replay(&g.baseJudge, learn.GateRunBaseJudge, g.baseDir, true, g.baseIn, g.baseRev, c, row.ReportsRun)
			} else {
				rp = g.replay(&g.base, learn.GateRunBase, g.baseDir, false, g.baseIn, g.baseRev, c, "")
			}
			if stop, err := g.halt(rp, true); stop { // without its score a loss could be the candidate's
				return true, err
			}
			if rp.skip != "" {
				row.Skip = rp.skip
				continue
			}
			base, run = rp.score, rp.run
		}
		row.BaselineRun = run
		if !row.AtBase(base) {
			row.Note = fmt.Sprintf("a guard the candidate lost twice: run %s found what it lost, and so does the base's replay, run %s", ref, run)
			continue
		}
		row.Note = fmt.Sprintf("a guard the candidate lost twice: run %s found what it lost, and the base's replay, run %s, lost it too", ref, run)
		g.lostAtBase(row.Case, ref, g.baseIn)
	}
	return false, nil
}

// localBase decides local guard row, of case c, which the candidate lost a
// defect of on both replays, on two replays of the base: a local replay is
// a coin flip (task-1-round-1 scored 1/2, then 0/2, at the same inputs), and
// the one that chose the guard found its defect by being chosen, so it
// never stands for the base, even when the base made it. The base's
// replays are the stored ones at the base's local inputs made after that
// one (storedBases: an earlier gate of the proposal that stopped short made
// them), newest first, else replays of the base now, the second
// (learn.GateRunBaseSecond) only when the first found a defect the
// candidate lost. A defect counts as found at the base only when both
// found it (learn.BothFound), so a candidate no worse than the base fails
// on a defect found half the time in 1 gate in 16, not in 1 in 8. A base
// replay that ends config_error leaves the row out (replay); one without a
// score for another reason, a lost reviewer's report included, stops the
// gate (a loss could be the candidate's). Stop when the gate ends there,
// with what runGate returns.
func (g *gateWork) localBase(c eval.Case, row *GateCase) (bool, error) {
	ref, lost := row.BaselineRun, row.Lost()
	var scores []*eval.Score
	var runs []string
	for _, b := range g.storedBases(c) {
		if len(scores) == 2 {
			break
		}
		scores, runs = append(scores, trimScore(b.Case.Score)), append(runs, b.Run)
	}
	slots := []struct {
		slot  **gateRun
		label string
	}{{&g.base, learn.GateRunBase}, {&g.baseSecond, learn.GateRunBaseSecond}}
	for len(scores) < 2 {
		if len(scores) == 1 && !slices.ContainsFunc(lost, func(id string) bool { return foundDefect(scores[0], id) }) {
			break // the base lost it too: one replay says so
		}
		s := slots[len(scores)] // a run holds one replay of a case
		rp := g.replay(s.slot, s.label, g.baseDir, false, g.baseIn, g.baseRev, c, "")
		if stop, err := g.halt(rp, true); stop {
			return true, err
		}
		if rp.skip != "" {
			row.Skip = rp.skip
			return false, nil
		}
		scores, runs = append(scores, rp.score), append(runs, rp.run)
	}
	base := scores[0]
	if len(scores) == 2 {
		base = learn.BothFound(scores[0], scores[1])
	}
	row.BaselineRun = runs[len(runs)-1]
	if !row.AtBase(base) {
		row.Note = fmt.Sprintf("a local guard the candidate lost twice: run %s found what it lost, and so did both of the base's replays, runs %s",
			ref, strings.Join(runs, " and "))
		return false, nil
	}
	said := "the base's replay, run " + runs[0] + ", did not"
	if len(runs) == 2 {
		said = "the base's replays, runs " + strings.Join(runs, " and ") + ", did not both find it"
	}
	row.Note = fmt.Sprintf("a local guard the candidate lost twice: run %s found what it lost, and %s", ref, said)
	g.lostAtBase(row.Case, ref, g.baseInFor(c))
	return false, nil
}

// foundDefect reports whether s found defect id.
func foundDefect(s *eval.Score, id string) bool {
	return s != nil && slices.ContainsFunc(s.Defects, func(d eval.DefectResult) bool { return d.ID == id && d.Found })
}

// storedBases are the stored replays of local guard case c with the base's
// local inputs (eval.Baselines) made after the replay that chose the guard
// (learn.Guards.Replay), newest first.
func (g *gateWork) storedBases(c eval.Case) []eval.Baseline {
	ref, _ := g.guards.Replay(c)
	var out []eval.Baseline
	for _, b := range eval.Baselines(g.runs, g.baseInFor(c), c) {
		if b.At.After(ref.At) { // never ref itself
			out = append(out, b)
		}
	}
	slices.SortStableFunc(out, func(a, b eval.Baseline) int { return b.At.Compare(a.At) })
	return out
}

// localRefused says that Codex refused the gate's replay of local case c:
// evalrun flagged it under its synthetic PR, so no replay of it runs again,
// but the factory never reads that flag (it marks a task never for Codex
// only when Codex refuses its own run), so the operator is told, once per
// case: a learn.local_refused warning and a toast.
func (g *gateWork) localRefused(c eval.Case, why string) {
	e := g.e
	switch first, err := e.st.ShouldSend(g.octx, "learn:local-refused:"+c.Name, 30*24*time.Hour); {
	case err != nil:
		e.warnUnlessStopped(g.ctx, err, "skill gate: the refused local replay", "case", c.Name)
		return
	case !first:
		return
	}
	e.event(g.octx, "warn", learnSubject, "learn.local_refused", fmt.Sprintf("local case %s (%s): a replay of the skill gate of proposal %d was "+
		"refused, so the case is flagged and never replayed again; the factory does not know of it: %s", c.Name, c.PR, g.p.ID, why),
		map[string]any{"proposal": g.p.ID, "case": c.Name, "pr": c.PR})
	title := fmt.Sprintf("a replay of local case %s was refused", c.Name)
	e.info(notify.Item{Key: "learn-local-refused:" + c.Name, Title: title, Line: title,
		Body: "The skill gate's replay of a round of the factory's local review was refused. The case is flagged and never " +
			"replayed again, but the factory does not know: if its task's Codex roles work on it again, mark the task " +
			"never for Codex."})
}

// storedBase is the base's stored score of guard case c, which a guard
// lost twice takes (guardsAtBase): the newest stored replay with the base's
// inputs (eval.Baselines; judge-only, the base's judge on the reviewer
// reports of run reports: eval.JudgeBaselines) that is the guard's stored
// baseline (learn.Guards.Replay) or was made after it, and its run; nil
// when there is none.
func (g *gateWork) storedBase(c eval.Case, reports string) (*eval.Score, string) {
	ref, _ := g.guards.Replay(c)
	bs := eval.Baselines(g.runs, g.baseIn, c)
	if g.judgeOnly {
		bs = eval.JudgeBaselines(g.runs, g.baseIn, c, reports)
	}
	var best *eval.Baseline
	for i := range bs { // newest run first: of two dated alike, the newer run's
		if b := &bs[i]; (b.Run == ref.Run || b.At.After(ref.At)) && (best == nil || b.At.After(best.At)) {
			best = b
		}
	}
	if best == nil {
		return nil, ""
	}
	return trimScore(best.Case.Score), best.Run
}

// lostAtBase says that guard case name, which run ref found, is lost at the
// proposal's base too: the review at master no longer finds what it did,
// whatever the proposal changes (a model, a notes or a code change, a hand
// edit of a prompt). One warn event learn.guard_lost_at_base (the case, the
// run, the inputs the base's replays ran with: baseIn, a local case's
// round's own) and one toast per proposal and case, the registry reserving
// the pair (a gate resumed after a stop decides the guard again); neither
// names more than the case.
func (g *gateWork) lostAtBase(name, ref, baseIn string) {
	e := g.e
	switch first, err := e.st.ShouldSend(g.octx, fmt.Sprintf("learn:guard-lost:%d:%s", g.p.ID, name), gateMaxAge); {
	case err != nil:
		e.warnUnlessStopped(g.ctx, err, "skill gate: the guard lost at the base", "proposal", g.p.ID, "case", name)
		return
	case !first:
		return
	}
	e.event(g.octx, "warn", learnSubject, "learn.guard_lost_at_base", fmt.Sprintf("guard case %s: the candidate of skill proposal %d "+
		"lost twice what run %s found, and the base lost it too (inputs %s): not the proposal's loss, but the review at master no longer "+
		"finds it", name, g.p.ID, ref, baseIn), map[string]any{"proposal": g.p.ID, "case": name, "run": ref, "inputs": baseIn})
	title := fmt.Sprintf("eval case %s is lost at master", name)
	e.info(notify.Item{Key: "learn-guard-lost:" + name + ":" + baseIn, Title: title, Line: title,
		Body: fmt.Sprintf("Run %s found it; the gate of skill proposal %d replayed it at master, which lost it too. "+
			"`magnum eval run --case %s` measures it again.", ref, g.p.ID, name)})
}

// confirmWins replays a miss target the candidate found more of than the
// baseline once more before the win counts (learn.GateRow.WonOnce, Confirm;
// second): one replay can find a defect by chance. A proposal that regressed
// already fails whatever it improved, so no second replay confirms its wins,
// which then count neither way (learn.GateRow.TargetGate: nothing showed the
// candidate does not find them, so they never count toward a dismissal).
// Stop when the gate ends there, with what runGate returns.
func (g *gateWork) confirmWins(cases []learn.GateCase) (bool, error) {
	if len(learn.Regressions(g.rows())) > 0 {
		return false, nil
	}
	for i := range g.res.Cases {
		row := &g.res.Cases[i]
		if !row.WonOnce() {
			continue
		}
		score, run, skip, stop, err := g.second(caseNamed(cases, row.Case), row)
		switch {
		case stop:
			return true, err
		case skip != "":
			row.Skip = skip
			continue
		}
		row.Confirm(score)
		row.SecondRun = run
		row.Note = fmt.Sprintf("its second replay, run %s, found it too", run)
		if !row.Confirmed {
			row.Note = fmt.Sprintf("found by its first replay only: its second, run %s, did not find it", run)
		}
	}
	return false, nil
}

// decide judges the rows (learn.Judge; a revert is held whatever it
// decides) and moves p to the verdict's state (gateDecide).
func (g *gateWork) decide() error {
	res, p := &g.res, g.p
	v := learn.Judge(g.rows())
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
