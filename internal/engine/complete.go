package engine

// Completions (DECISIONS "A cut shell command is killed, and a review that
// missed a reviewer is completed"): from 2026-10-25 to 2026-10-28 six clean
// reviews went without codex-review, whose pane a cut `codex review` kept
// busy, and auto-approval refused each for the reviewer it did not hear
// (missingReports), so an approval waited for the operator or the next
// push. Now, when auto-approval refuses a clean review of a repository it
// approves for that alone (publicUnheard), and every report the review went
// without is missing for a cause that passes (passingCauses: a busy pane, a
// timeout, a lost session, ...), magnum completes the review on its head:
// the PR waits in rereview_pending as for a CI follow-up (ci_red.go), held
// only by a snooze, quiet hours and the kinds' pauses, and its round is a
// same-head re-review that runs the judge in its session and, besides it,
// only the roles the review went without (pipeline.RoundInput.Complete),
// with no triage, no reruns and no restarts. It counts against neither the
// daily cap nor the full pass's re-reviews (a round of the reviewed head),
// and it posts a new review, which auto-approval's gates decide on as on
// any other. A refusal, a logged-out CLI and a usage limit need the
// operator or time, which the pauses wait for: no completion. A review that
// held its approval for a full pass gets the full pass (the gate refuses it
// first, approval_hold.go). Each head gets maxCompletions; after them
// pr.complete_gave_up says so once, and the refusal stands. A push or the
// close of the PR drops a completion that waits (supersedeCompletion,
// dropCompletion), and the status comment says, while one waits or runs,
// that magnum runs the missing reviewer again (approvalLine).

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kvPRComplete holds the PR's completions of its head (completion as
// JSON).
func kvPRComplete(prID int64) string { return fmt.Sprintf("pr.%d.complete", prID) }

// maxCompletions is how many completions one head gets.
const maxCompletions = 2

// passingCauses are the report statuses a completion runs a role again
// for: what passes by itself (a busy pane, a timeout, a lost or missing
// session, a failed or stalled run, an overload, a cancel, a person at the
// panes, a role that ended without its report). Any other (a refusal, a
// logged-out CLI, a usage limit) needs the operator or time.
var passingCauses = []string{pipeline.ReportBusy, pipeline.ReportTimeout, pipeline.ReportLost, pipeline.ReportNoSession,
	pipeline.ReportFailed, string(agents.HealthStalled), string(agents.HealthOverloaded), pipeline.ReportCancelled, pipeline.ReportHumanActive, pipeline.ReportMissing}

// completion is what kvPRComplete records of the PR's head: the clean
// review a completion waits to complete (ReviewID, of round Round; 0 =
// none waits), the round whose reviewers' reports that review rests on
// (Base), the roles it runs and why their reports were missing, which
// completion of the head it is (Attempt), when it was queued and when its
// round last started (zero: not yet), the completion rounds of the head
// (Rounds: a later completion rests on Base too, and a continue of a
// paused one goes on as one), and whether magnum gave up on the head.
type completion struct {
	Head     string    `json:"head"`
	ReviewID int64     `json:"review_id,omitempty"`
	Round    int       `json:"round,omitempty"`
	Base     int       `json:"base,omitempty"`
	Roles    []string  `json:"roles,omitempty"`
	Causes   []string  `json:"causes,omitempty"`
	Attempt  int       `json:"attempt,omitempty"`
	Queued   time.Time `json:"queued,omitzero"`
	Started  time.Time `json:"started,omitzero"`
	Rounds   []int     `json:"rounds,omitempty"`
	GaveUp   bool      `json:"gave_up,omitempty"`
}

// dueFor reports whether c waits, or runs, for pr: its review is pr's
// latest on the head magnum reviewed, which is pr's head, and GitHub has
// pr open.
func (c completion) dueFor(pr store.PR) bool {
	return c.ReviewID != 0 && !c.GaveUp && c.Head != "" && c.Head == pr.HeadSHA && c.Head == deref(pr.ReviewedSHA) &&
		c.ReviewID == deref(pr.LastReviewID) && pr.GHState == store.GHOpen && !postMerge(pr)
}

// completionOf is PR prID's record (the zero completion when it has none
// or it cannot be read).
func (e *Engine) completionOf(ctx context.Context, prID int64) completion {
	var c completion
	if v, ok := e.getKV(ctx, kvPRComplete(prID)); ok {
		_ = json.Unmarshal([]byte(v), &c)
	}
	return c
}

// saveCompletion records c as PR prID's.
func (e *Engine) saveCompletion(ctx context.Context, prID int64, c completion) {
	if b, err := json.Marshal(c); err == nil {
		e.setKV(ctx, kvPRComplete(prID), string(b))
	}
}

// considerCompletion queues the completion of sum, the clean review c's
// round posted on c's head, which auto-approval refuses for the reports it
// went without (missingReports, publicUnheard): the PR is reviewed at the
// head sum covers, not forced, its watch would review it on its own, every
// report the review went without is missing for a passing cause and names
// a role of the watch, the round whose reports the review rests on
// reviewed this head (completionBase), and fewer than maxCompletions ran on
// it; after that many, pr.complete_gave_up once. A completion of sum queued
// already is put back in line when the PR left it before its round
// started; one whose round started and left the PR reviewed again (a
// failed round the operator took out of needs_attention) was an attempt.
// Run at every auto-approval pass (the round's end, each tick): what it
// queued once it never queues again.
func (e *Engine) considerCompletion(ctx context.Context, c store.RepoPR, sum store.ReviewSummary) {
	pr := c.PR
	w := e.cfg.WatchFor(c.Repo)
	if w == nil || e.d.Layout.Scratch != "" || pr.State != store.PRReviewed || pr.Forced || pr.DetailsAt == nil || sum.SHA != pr.HeadSHA {
		return
	}
	rec := e.completionOf(ctx, pr.ID)
	if rec.Head != pr.HeadSHA {
		rec = completion{Head: pr.HeadSHA}
	}
	if rec.GaveUp {
		return
	}
	now := e.now()
	if rec.ReviewID == sum.ReviewID && rec.Started.IsZero() {
		if rec.dueFor(pr) {
			e.queueCompletion(ctx, c, *w, rec, now) // it left the line before it started: back in it
		}
		return
	}
	run, err := e.st.RunByID(ctx, sum.RunID)
	if err != nil {
		return // missingReports warned
	}
	m, ok := pipeline.ReadRoundMissingReports(ctx, e.st, pr.ID, run.Round)
	if !ok {
		return
	}
	var roles, causes []string
	for _, r := range m.Unheard() {
		if !slices.Contains(passingCauses, r.Status) {
			return // the operator's or time's: the pauses wait for it
		}
		if !slices.Contains(roles, r.Role) {
			roles, causes = append(roles, r.Role), append(causes, r.Status)
		}
	}
	if len(roles) == 0 {
		return
	}
	candidates := e.rolesForPR(ctx, w, pr.ID)
	for _, name := range roles {
		if !slices.ContainsFunc(candidates, func(r config.Role) bool { return !r.Judge && r.Name == name }) {
			return // a role the watch no longer runs
		}
	}
	base, ok := e.completionBase(ctx, pr, rec, run.Round, m)
	if !ok {
		return
	}
	ran := rec.Attempt
	if rec.ReviewID != 0 && rec.Started.IsZero() {
		ran-- // the one that waited never started: a later review took its place
	}
	if ran >= maxCompletions {
		rec.GaveUp, rec.ReviewID, rec.Queued = true, 0, time.Time{}
		e.saveCompletion(ctx, pr.ID, rec)
		e.event(ctx, "warn", prSubject(repoOf(c.Repo), pr.Number), "pr.complete_gave_up",
			fmt.Sprintf("round %d went without %s on %s too; no more completions of the head after %d: the refusal stands",
				run.Round, strings.Join(roles, ", "), textx.ShortSHA(pr.HeadSHA), maxCompletions),
			map[string]any{"round": run.Round, "head": pr.HeadSHA, "roles": roles, "causes": causes, "attempts": ran})
		return
	}
	if dec := e.classify(ctx, *w, pr, now); !dec.Eligible {
		return
	}
	rec.ReviewID, rec.Round, rec.Base, rec.Roles, rec.Causes = sum.ReviewID, run.Round, base, roles, causes
	rec.Attempt, rec.Queued, rec.Started = ran+1, now.UTC(), time.Time{}
	e.saveCompletion(ctx, pr.ID, rec) // first: the throttle reads it
	if !e.queueCompletion(ctx, c, *w, rec, now) {
		return
	}
	what := make([]string, len(roles))
	for i := range roles {
		what[i] = roles[i] + " (" + causes[i] + ")"
	}
	e.event(ctx, "info", prSubject(repoOf(c.Repo), pr.Number), "pr.complete_queued",
		fmt.Sprintf("round %d's clean review of %s went without %s; completion %d of %d runs %s again with the judge",
			run.Round, textx.ShortSHA(pr.HeadSHA), strings.Join(what, ", "), rec.Attempt, maxCompletions, textx.Plural(len(roles), "it", "them")),
		map[string]any{"round": run.Round, "head": pr.HeadSHA, "roles": roles, "causes": causes, "attempt": rec.Attempt})
}

// queueCompletion moves c's PR, reviewed, to rereview_pending for its
// completion rec (queue); false when it could not, with pr.complete_failed.
func (e *Engine) queueCompletion(ctx context.Context, c store.RepoPR, w config.Watch, rec completion, now time.Time) bool {
	err := e.queue(ctx, c.PR, w, []string{store.PRReviewed}, false, now, fmt.Sprintf("completion of round %d", rec.Round))
	if err != nil {
		e.event(ctx, "warn", prSubject(repoOf(c.Repo), c.PR.Number), "pr.complete_failed",
			fmt.Sprintf("the completion of round %d could not be queued: %v; the next pass of auto-approval tries again", rec.Round, err),
			map[string]any{"round": rec.Round, "head": rec.Head, "attempt": rec.Attempt, "stage": "queue"})
		return false
	}
	return true
}

// completionBase is the round whose reviewers' reports the review of round
// (m: its record) rests on: round itself, the base of the completion that
// round was (rec.Rounds), or, for a round of the judge alone, the round it
// carried the missing reports from (or that round's base). ok is false
// when the carried reports come from more than one round, or the base
// round has no run on pr's head (a delta check's carried reports are of an
// older head: a completion of this one would not complete it).
func (e *Engine) completionBase(ctx context.Context, pr store.PR, rec completion, round int, m pipeline.MissingReports) (int, bool) {
	base := round
	switch {
	case slices.Contains(rec.Rounds, round) && rec.Base > 0:
		base = rec.Base
	case len(m.Missing) == 0 && len(m.Carried) > 0:
		base = m.Carried[0].Round
		for _, r := range m.Carried {
			if r.Round != base {
				return 0, false
			}
		}
		if slices.Contains(rec.Rounds, base) && rec.Base > 0 {
			base = rec.Base
		}
	}
	if base <= 0 {
		return 0, false
	}
	runs, err := e.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		return 0, false
	}
	return base, slices.ContainsFunc(runs, func(r store.Run) bool { return r.Round == base && r.TargetSHA == pr.HeadSHA })
}

// completionDue is the completion a round dispatched for pr runs: a
// same-head re-review (sameHead) nobody forced or requested (those run as
// asked) while one is due (completion.dueFor); nil otherwise.
func (e *Engine) completionDue(ctx context.Context, pr store.PR, sameHead, requested bool) *completion {
	if !sameHead || requested || pr.Forced {
		return nil
	}
	rec := e.completionOf(ctx, pr.ID)
	if !rec.dueFor(pr) {
		return nil
	}
	return &rec
}

// completionWait is when pr's completion, due and nobody's forced round,
// was queued (eligibility.PRFacts.CompleteAt); ok is false when none is
// due.
func (e *Engine) completionWait(ctx context.Context, pr store.PR) (time.Time, bool) {
	if pr.Forced {
		return time.Time{}, false
	}
	rec := e.completionOf(ctx, pr.ID)
	if !rec.dueFor(pr) {
		return time.Time{}, false
	}
	if rec.Queued.IsZero() {
		return rec.Started, true
	}
	return rec.Queued, true
}

// continuedCompletion is the completion a continue of pr's paused round
// goes on with: the record of target whose completion rounds hold round;
// nil otherwise.
func (e *Engine) continuedCompletion(ctx context.Context, pr store.PR, round int, target string) *completion {
	rec := e.completionOf(ctx, pr.ID)
	if rec.Head != target || !slices.Contains(rec.Rounds, round) {
		return nil
	}
	return &rec
}

// planCompletion settles the completion the round rs sets up for job runs
// (rs.complete): the one dispatch found due, while the checkout found the
// reviewed head (a newer head's round reviews it in full, and counts), or,
// for a continue, the paused round's when it was one (continuedCompletion).
func (e *Engine) planCompletion(ctx context.Context, job *roundJob, rs *roundSetup) {
	switch {
	case job.kind == kindContinue:
		job.complete = e.continuedCompletion(ctx, job.pr, job.round, rs.target)
	case !rs.sameHead:
		job.complete = nil
	}
	rs.complete = job.complete
}

// reruns reports whether rs's round runs the roles of a completion: one of
// the reviewed head, not the continue of a paused one (its reviewers ran).
func (rs *roundSetup) reruns() bool { return rs.complete != nil && rs.sameHead }

// noteCompletionStart records that the PR's due completion c (nil = the
// round is none) started its round at at: it waits no more.
func (e *Engine) noteCompletionStart(ctx context.Context, prID int64, c *completion, at time.Time) {
	if c == nil {
		return
	}
	rec := e.completionOf(ctx, prID)
	if rec.ReviewID == 0 {
		return
	}
	rec.Queued, rec.Started = time.Time{}, at.UTC()
	e.saveCompletion(ctx, prID, rec)
}

// settleCompletion records, at the end of a completion's round (in.Complete)
// on target, its round among the head's completion rounds, and, when it
// ended without a review or replies (not stopped by a shutdown, nor
// paused), pr.complete_failed: the PR retries it as a failed round.
func (e *Engine) settleCompletion(ctx context.Context, job *roundJob, in pipeline.RoundInput, res pipeline.RoundResult, outcome string, cancelled bool) {
	if in.Complete == nil {
		return
	}
	rec := e.completionOf(ctx, job.pr.ID)
	if rec.Head == in.TargetSHA && res.Round > 0 && !slices.Contains(rec.Rounds, res.Round) {
		rec.Rounds = append(rec.Rounds, res.Round)
		e.saveCompletion(ctx, job.pr.ID, rec)
	}
	switch outcome {
	case pipeline.OutcomePosted, pipeline.OutcomeReplied, pipeline.OutcomeDryRun,
		pipeline.OutcomeUsageLimit, pipeline.OutcomeLoginRequired, pipeline.OutcomeOverloaded:
		return
	}
	if cancelled {
		return
	}
	e.event(ctx, "warn", prSubject(job.repo, job.pr.Number), "pr.complete_failed",
		fmt.Sprintf("the completion of round %d (round %d) ended %s; it retries as a failed round does", in.Complete.Round, res.Round, outcome),
		map[string]any{"round": in.Complete.Round, "head": in.TargetSHA, "attempt": rec.Attempt, "outcome": outcome, "stage": "round"})
}

// supersedeCompletion deletes the record of an older head when pr's head
// moved: the push's round reviews the new head in full, and a completion
// that waited is dropped.
func (e *Engine) supersedeCompletion(ctx context.Context, pr store.PR) {
	rec := e.completionOf(ctx, pr.ID)
	if rec.Head == "" || rec.Head == pr.HeadSHA {
		return
	}
	e.delKV(ctx, kvPRComplete(pr.ID))
	if rec.ReviewID != 0 && !rec.GaveUp {
		e.log.Info("completion dropped: a push", "pr", pr.ID, "head", textx.ShortSHA(pr.HeadSHA), "completed", textx.ShortSHA(rec.Head))
	}
}

// dropCompletion deletes PR prID's record as GitHub closed it: a
// completion that waited is dropped.
func (e *Engine) dropCompletion(ctx context.Context, prID int64) {
	if _, ok := e.getKV(ctx, kvPRComplete(prID)); ok {
		e.delKV(ctx, kvPRComplete(prID))
	}
}

// completionRoles is the roles a completion runs: the judge of toRun and
// the roles of roles (the watch's) it names (pipeline.Completion.Roles),
// whatever their runs say.
func completionRoles(roles, toRun []config.Role, names []string) []config.Role {
	out := judgeAlone(toRun)
	for _, r := range roles {
		if !r.Judge && slices.Contains(names, r.Name) {
			out = append(out, r)
		}
	}
	return out
}

// completionInput is the pipeline's completion of rs's round: its review's
// round and base, the roles it runs, and what the base round's reviewers
// re-reviewed from (the head reviewed before it, when it was a re-review;
// "" = the whole PR) with whether those commits were force-pushed or merged
// a branch in, from the base round's runs on the round's head (a role it
// runs first, else any).
func (e *Engine) completionInput(ctx context.Context, job *roundJob, rs roundSetup, base string) *pipeline.Completion {
	c := rs.complete
	out := &pipeline.Completion{Round: c.Round, Base: c.Base, Roles: slices.Clone(c.Roles)}
	if rs.kind == kindContinue {
		return out
	}
	runs, err := e.st.RunsByPR(ctx, job.pr.ID)
	if err != nil {
		return out
	}
	var pick *store.Run
	for i := range runs {
		r := &runs[i]
		if r.Round != c.Base || r.TargetSHA != rs.target {
			continue
		}
		if pick == nil || slices.Contains(c.Roles, r.Role) && !slices.Contains(c.Roles, pick.Role) {
			pick = r
		}
	}
	if pick == nil || pick.Kind != store.RunRereview || deref(pick.PrevReviewedSHA) == "" || deref(pick.PrevReviewedSHA) == rs.target {
		return out
	}
	out.Since = deref(pick.PrevReviewedSHA)
	_, out.ForcePushed = e.headContext(ctx, job.slot.Path, base, out.Since, rs.target)
	out.BaseMerged = !out.ForcePushed && e.baseMerged(ctx, job, out.Since, rs.target)
	return out
}

// completionWords is the round's start event's name of a completion.
func completionWords(c *completion) string {
	return fmt.Sprintf("completion of round %d", c.Round)
}
