package engine

// The gate of a skill proposal (DECISIONS "The gate replays the learned
// cases"), the self-improving loop's fourth stage. A proposal the editor
// committed waits in gating; at a tick with nothing holding it the daemon
// replays eval cases on it in its own goroutine, one at a time: the learned
// cases of the inputs it noted (its targets) and the newest other cases of
// both corpora, up to [learn] gate_cases (learn.SelectCases), the judge
// alone when only the judge skill and the judge prompts changed
// (learn.Mode). Each case's baseline is the newest stored replay with
// master's prompts and skill (eval.Baselines), else a replay of the
// checkout first; the candidate replays with the proposal's worktree, and a
// case it lost is replayed once more. learn.Judge decides: passed (it lands,
// learn_land.go), held (the operator decides) or failed (its worktree and
// branch go, its inputs stay new); a revert (learn_revert.go) is held
// whatever it decides. Every replay is stored as `magnum eval
// run` stores one, under runs of the gate's own (a baseline, a candidate
// and a retry run, each with its agent tag), so a later gate and `magnum
// eval` read them. The replays run in-process through Deps.EvalCase
// (evalrun.RunCase, which the daemon's wiring sets: the engine cannot
// import evalrun, Amendment A1). The Codex soft cap, a usage limit, a
// shutdown or a pause leaves the proposal gating for a later tick.

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
	// an error before any case) waits before it starts again.
	gateRetryAfter = 30 * time.Minute
	// gateReasonRunes clips the rule stored as a gated proposal's reason.
	gateReasonRunes = 2000
)

// EvalGateOptions are one replay of the gate (evalrun.Options' part the
// gate sets): the directory whose prompts and judge skill it reads (the
// checkout for a baseline, the proposal's worktree for the candidate),
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

// GateMark is the gate in flight (KVLearnGating).
type GateMark struct {
	Proposal int64     `json:"proposal"`
	Started  time.Time `json:"started"`
}

// GateCase is one case of a gate's result: the row learn.Judge read and
// the eval runs its two scores come from.
type GateCase struct {
	learn.GateRow
	BaselineRun  string `json:"baseline_run,omitempty"`
	CandidateRun string `json:"candidate_run,omitempty"` // a second replay's when Retried
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

// gateRun is an eval run the gate stores, made on its first replay.
type gateRun struct {
	run  eval.Run
	dir  string
	tag  string
	opts EvalGateOptions
}

// runGate is the gate of proposal p: its worktree (reused, else added
// again from its branch), the cases, a baseline and a candidate replay of
// each (a second one of a lost case), the verdict and the proposal's next
// state. It returns an error when the gate stopped short and p stays
// gating: a replay at the soft cap or a usage limit, a shutdown, an
// unreadable registry, inputs or runs.
func (e *Engine) runGate(ctx context.Context, p store.LearnProposal) error {
	octx := context.WithoutCancel(ctx)
	waits := func(why string) error {
		e.event(octx, "info", learnSubject, "learn.gate_waiting", fmt.Sprintf("the gate of skill proposal %d stopped and waits: %s", p.ID, why),
			map[string]any{"proposal": p.ID, "reason": why})
		return errors.New(why)
	}
	if why := e.gateUnavailable(); why != "" {
		return errors.New(why)
	}
	if err := e.ensureCandidate(ctx, p, false); errors.Is(err, errCandidateGone) {
		e.gateDecide(octx, p, GateResult{State: learn.GateFailed, Rule: oneLine(err.Error(), retroWhyRunes), Mode: learn.Mode(p.Files)})
		return nil
	} else if err != nil {
		return waits("the worktree: " + oneLine(err.Error(), retroWhyRunes))
	}
	mode := learn.Mode(p.Files)
	res := GateResult{Mode: mode, Cases: []GateCase{}}
	targets, err := e.gateTargets(ctx, p)
	if err != nil {
		return waits("the proposal's inputs: " + oneLine(err.Error(), retroWhyRunes))
	}
	corpus, corpusPath, err := e.d.EvalCorpus()
	if err != nil {
		res.CorpusError, corpus = oneLine(err.Error(), retroWhyRunes), nil
	}
	cases := learn.SelectCases(corpus, targets, e.gateWatched, e.gateFlagged(ctx), e.cfg.Learn.GateCases)
	baseIn, baseRev, err := e.d.EvalInputs(e.d.Checkout)
	if err != nil {
		return waits("master's inputs: " + oneLine(err.Error(), retroWhyRunes))
	}
	candIn, candRev, err := e.d.EvalInputs(p.Scratch)
	if err != nil {
		return waits("the candidate's inputs: " + oneLine(err.Error(), retroWhyRunes))
	}
	res.Inputs, res.BaselineInputs = candIn, baseIn
	runs, err := e.d.EvalRuns()
	if err != nil {
		return waits("the eval runs: " + oneLine(err.Error(), retroWhyRunes))
	}
	e.event(ctx, "info", learnSubject, "learn.gate_start", fmt.Sprintf("the gate of skill proposal %d started: %s, %s (%s)", p.ID,
		textx.Count(len(cases), "case", "cases"), strings.Join(targets, ", "), mode), map[string]any{"proposal": p.ID, "cases": len(cases), "mode": mode})

	commit, dirty := e.gateCommit(ctx)
	var base, cand, retry *gateRun
	mkRun := func(slot **gateRun, label, dir string, judgeOnly bool, in, rev string) (*gateRun, error) {
		if *slot != nil {
			return *slot, nil
		}
		id, rdir, err := eval.NewRunDir(e.d.Layout.Eval(), e.now())
		if err != nil {
			return nil, err
		}
		r := &gateRun{dir: rdir, tag: eval.AgentTag(id), run: eval.Run{ID: id, Label: fmt.Sprintf("learn gate of proposal %d: %s", p.ID, label),
			Commit: commit, Dirty: dirty, Corpus: corpusPath, Started: e.now(), Cases: []eval.CaseRun{}, Inputs: in, ReviewerInputs: rev,
			Candidate: dir}}
		r.opts = EvalGateOptions{Candidate: dir, JudgeOnly: judgeOnly, RunDir: rdir, AgentTag: r.tag}
		*slot = r
		res.Runs = append(res.Runs, id)
		return r, nil
	}
	defer func() { // every run the gate made is finished, whatever stopped it
		for _, r := range []*gateRun{base, cand, retry} {
			if r != nil {
				r.run.Finished = e.now()
				if err := eval.SaveRun(r.dir, r.run); err != nil {
					e.log.Warn("skill gate: save an eval run", "run", r.run.ID, "err", err)
				}
			}
		}
	}()
	// replay replays c into the run in slot: its score, or why it has none
	// (skip), or why the gate must stop (stop).
	replay := func(slot **gateRun, label, dir string, judgeOnly bool, in, rev string, c eval.Case) (score *eval.Score, run, skip, stop string) {
		r, err := mkRun(slot, label, dir, judgeOnly, in, rev)
		if err != nil {
			return nil, "", "", "the eval run's directory: " + oneLine(err.Error(), retroWhyRunes)
		}
		cr := e.d.EvalCase(ctx, c, r.opts)
		cr.Finished = e.now()
		r.run.Cases = append(r.run.Cases, cr)
		if err := eval.SaveRun(r.dir, r.run); err != nil {
			e.log.Warn("skill gate: save an eval run", "run", r.run.ID, "err", err)
		}
		if rs, err := e.d.EvalRuns(); err == nil {
			runs = rs
		}
		why := cmp.Or(oneLine(cr.Error, retroWhyRunes), "no result to score")
		switch {
		case ctx.Err() != nil:
			return nil, r.run.ID, "", "the daemon is stopping"
		case cr.Outcome == eval.OutcomeBudget, cr.Outcome == pipeline.OutcomeUsageLimit, cr.Outcome == pipeline.OutcomeLoginRequired:
			return nil, r.run.ID, "", fmt.Sprintf("case %s: %s (%s)", c.Name, cr.Outcome, why)
		case cr.Outcome == eval.OutcomeFlagged:
			return nil, r.run.ID, learn.SkipFlagged, ""
		case cr.Score == nil:
			return nil, r.run.ID, fmt.Sprintf("the %s replay ended %s: %s", label, cr.Outcome, why), ""
		}
		return trimScore(cr.Score), r.run.ID, "", ""
	}

	for _, gc := range cases {
		row := GateCase{GateRow: learn.GateRow{Case: gc.Case.Name, Target: gc.Target, Skip: gc.Skip}}
		if row.Skip != "" {
			res.Cases = append(res.Cases, row)
			continue
		}
		if ctx.Err() != nil {
			return waits("the daemon is stopping")
		}
		if bs := eval.Baselines(runs, baseIn, gc.Case); len(bs) > 0 {
			row.Before, row.BaselineRun = trimScore(bs[0].Case.Score), bs[0].Run
		} else {
			score, run, skip, stop := replay(&base, "baseline", e.d.Checkout, false, baseIn, baseRev, gc.Case)
			if stop != "" {
				return waits(stop)
			}
			row.Before, row.BaselineRun, row.Skip = score, run, skip
		}
		if row.Skip == "" && mode == learn.GateJudgeOnly {
			if _, fc, ok := eval.FullReplayFor(runs, gc.Case.Name, gc.Case.Head, &eval.Run{ReviewerInputs: candRev}); !ok || fc.ReportDir == "" {
				row.Skip = "no full replay whose reviewers read the candidate's prompts, for the judge alone"
			}
		}
		if row.Skip == "" {
			score, run, skip, stop := replay(&cand, "candidate", p.Scratch, mode == learn.GateJudgeOnly, candIn, candRev, gc.Case)
			if stop != "" {
				return waits(stop)
			}
			row.After, row.CandidateRun, row.Skip = score, run, skip
		}
		res.Cases = append(res.Cases, row)
	}
	// A case the candidate lost is replayed once more before it counts.
	for i := range res.Cases {
		row := &res.Cases[i]
		if len(row.Lost()) == 0 {
			continue
		}
		c := cases[slices.IndexFunc(cases, func(gc learn.GateCase) bool { return gc.Case.Name == row.Case })].Case
		score, run, _, stop := replay(&retry, "second", p.Scratch, mode == learn.GateJudgeOnly, candIn, candRev, c)
		if stop != "" {
			return waits(stop)
		}
		row.Retried = true
		if score != nil {
			row.After, row.CandidateRun = score, run
		}
	}
	rows := make([]learn.GateRow, len(res.Cases))
	for i, c := range res.Cases {
		rows[i] = c.GateRow
	}
	v := learn.Judge(rows)
	res.State, res.Rule = v.State, v.Rule
	if res.CorpusError != "" && v.State == learn.GateHeld {
		res.Rule += "; the corpus: " + res.CorpusError
	}
	if p.Reverts != 0 { // a revert lands only on the operator's y (learn_revert.go)
		res.State, res.Rule = learn.GateHeld, fmt.Sprintf("a revert of skill version %d waits for the operator whatever the gate found "+
			"(it would have %s it: %s)", p.Reverts, v.State, res.Rule)
	}
	e.gateDecide(octx, p, res)
	return nil
}

// gateDecide stores the gate's result on p and moves it from gating to the
// verdict's state: a failed one loses its worktree and branch (its inputs
// stay new), a held one waits for the operator with a toast, a passed one
// for the landing.
func (e *Engine) gateDecide(ctx context.Context, p store.LearnProposal, res GateResult) {
	if res.Cases == nil {
		res.Cases = []GateCase{}
	}
	b, err := json.Marshal(res)
	if err != nil {
		e.log.Warn("skill gate: encode the result", "proposal", p.ID, "err", err)
		return
	}
	got, err := e.st.DecideLearnProposal(ctx, p.ID, []string{store.LearnGating}, res.State, textx.Clip(res.Rule, gateReasonRunes), e.now(), b)
	if err != nil {
		e.log.Warn("skill gate: decide the proposal", "proposal", p.ID, "state", res.State, "err", err)
		return
	}
	e.event(ctx, "info", learnSubject, "learn.gate_done", fmt.Sprintf("the gate %s skill proposal %d: %s", res.State, p.ID, oneLine(res.Rule, 300)),
		map[string]any{"proposal": p.ID, "state": res.State, "rule": oneLine(res.Rule, 300), "cases": len(res.Cases), "mode": res.Mode})
	switch res.State {
	case store.LearnFailed:
		e.dropCandidate(ctx, p.Scratch, p.Branch)
	case store.LearnHeld:
		e.noteHeld(ctx, got, res.Rule, true)
	}
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
// an earlier replay of it was refused (eval.FlaggedFile, which cannot be
// read refuses every PR), or the registry holds a Codex flag for it (one
// it cannot read counts).
func (e *Engine) gateFlagged(ctx context.Context) func(pr string) bool {
	flagged, ferr := eval.LoadFlagged(e.d.Layout.Eval())
	return func(pr string) bool {
		if ferr != nil {
			return true
		}
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
	}
}

// gateCommit is what the gate's eval runs record of magnum's code, as
// `magnum eval run` does: the checkout's short HEAD and whether it has
// uncommitted changes.
func (e *Engine) gateCommit(ctx context.Context) (string, bool) {
	sha, err := e.d.LearnGit.RevParse(ctx, e.d.Checkout, "HEAD")
	if err != nil {
		return "", false
	}
	st, err := e.d.LearnGit.StatusPaths(ctx, e.d.Checkout)
	return learnShort(sha), err == nil && len(st) > 0
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
// landing may have moved it (rebased, when moved says so).
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
