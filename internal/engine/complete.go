package engine

// Completions (DECISIONS "A review that missed a reviewer is completed"):
// from 2026-10-25 to 2026-10-28 six clean reviews went without codex-review,
// whose pane a cut `codex review` kept busy, and auto-approval refused each
// for the reviewer it did not hear (missingReports), so an approval waited
// for the operator or the next push. Now, when auto-approval refuses a clean
// review of a repository it approves for that alone (publicUnheard: no
// other gate refuses the head for good), and every report the review went
// without is missing for a cause that passes (passingCauses: a busy pane, a
// timeout, a lost session, ...), magnum completes the review on its head:
// the PR waits in rereview_pending as for a CI follow-up (ci_red.go), held
// by what holds any automatic round but the push quiet period, the
// re-review interval and the daily cap, and its round is a same-head
// re-review that runs the judge in its session and, besides it, only the
// roles the review went without (pipeline.RoundInput.Complete), with no
// triage, no reruns and no restarts. It counts against neither the daily
// cap, nor the full pass's re-reviews (a round of the reviewed head), nor
// the re-review interval (its start is given back), and it posts a new
// review, which auto-approval's gates decide on as on any other, unless
// every late role missed again (pipeline.OutcomeUnheard: no review). A
// refusal, a logged-out CLI and a usage limit need the operator or time,
// which the pauses wait for: no completion. A review that held its approval
// for a full pass gets the full pass (the gate refuses it first,
// approval_hold.go). Each head gets maxCompletions rounds, counted as they
// start, retries of a failed one included, and kept across a restart, a
// close and a push that went back to the head; after them
// pr.complete_gave_up says so once, and the refusal stands. A stop of the
// daemon during the last one, before its judge was prompted, resumes that
// round on the reports it verified (interrupted), never a third. A push drops a
// completion that waits (supersedeCompletion), and so does the close of the
// PR (dropCompletion) and the operator's abort or ignore, which stops the
// head's completions for good (stopCompletion). While one waits or runs, the
// status comment says that magnum runs the missing reviewer again
// (approvalLine).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
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

// maxCompletions is how many completion rounds one head gets.
const maxCompletions = 2

// pastHeads is how many earlier heads a PR's record keeps the spent
// completions of (completion.Past).
const pastHeads = 5

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
// completion of the head was queued last (Attempt), when it was queued and
// when its round last started (zero: not yet), the completion rounds the
// head started (Rounds, each recorded as it starts: their number is the
// head's spent completions, and a continue of a paused one goes on as one),
// whether magnum gave up on the head and whether the operator stopped its
// completions (an abort or an ignore). Past keeps the spent completions of
// the PR's earlier heads, so a push back to one of them gets no more.
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
	Stopped  bool      `json:"stopped,omitempty"`
	// Anchor is the start of the round the queued completion completes,
	// as the throttle had it when it was queued (last_round_started_at):
	// what the completion's rounds give back (giveBackStart).
	Anchor *time.Time   `json:"anchor,omitempty"`
	Past   []completion `json:"past,omitempty"`
	// resume: the completion resumes its head's last round, which a stop
	// cut short (interrupted); never stored.
	resume bool
}

// forHead is c as the record of head: c itself when it is head's, else
// head's spent or stopped completions from Past (or none), with c's own
// moved to Past when it spent a round, gave up or was stopped (a head that
// spent nothing pushes no spent one out). A completion that waited on the
// old head waits no more.
func (c completion) forHead(head string) completion {
	if c.Head == head {
		return c
	}
	out := completion{Head: head}
	past := c.Past
	if c.Head != "" && (len(c.Rounds) > 0 || c.GaveUp || c.Stopped) {
		past = append(slices.Clone(past), completion{Head: c.Head, Base: c.Base, Attempt: c.Attempt, Rounds: c.Rounds, GaveUp: c.GaveUp,
			Stopped: c.Stopped})
	}
	for _, p := range past {
		if p.Head == head {
			out.Base, out.Attempt, out.Rounds, out.GaveUp, out.Stopped = p.Base, p.Attempt, p.Rounds, p.GaveUp, p.Stopped
			continue
		}
		out.Past = append(out.Past, p)
	}
	out.Past = out.Past[max(0, len(out.Past)-pastHeads):]
	return out
}

// rerun names the roles c's round runs again: its Roles, none when it
// resumes the head's last round, which ran them.
func (c *completion) rerun() []string {
	if c.resume {
		return nil
	}
	return c.Roles
}

// interrupted reports whether c waits for pr with no round left: a stop
// of the daemon cut its last round short before the judge was prompted
// (the round's end would have spent or given it up), so the next round of
// the head resumes that one (Completion.Resume) instead of being an
// ordinary same-head review that drops its late reports.
func (c completion) interrupted(pr store.PR) bool {
	return c.waitsFor(pr) && len(c.Rounds) >= maxCompletions
}

// waitsFor reports whether c waits, or runs, for pr: its review is pr's
// latest on the head magnum reviewed, which is pr's head, GitHub has pr
// open, magnum did not give up on the head and the operator did not stop
// it.
func (c completion) waitsFor(pr store.PR) bool {
	return c.ReviewID != 0 && !c.GaveUp && !c.Stopped && c.Head != "" && c.Head == pr.HeadSHA && c.Head == deref(pr.ReviewedSHA) &&
		c.ReviewID == deref(pr.LastReviewID) && pr.GHState == store.GHOpen && !postMerge(pr)
}

// dueFor reports whether c's next round may start for pr: it waits
// (waitsFor) and the head has started fewer than maxCompletions.
func (c completion) dueFor(pr store.PR) bool {
	return c.waitsFor(pr) && len(c.Rounds) < maxCompletions
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
// went without (missingReports, publicUnheard): the PR, read again, is
// reviewed at the head sum covers, not forced, its watch would review it on
// its own, every report the review went without is missing for a passing
// cause and names a role of the watch, the round whose reports the review
// rests on reviewed this head (completionBase), and the head started fewer
// than maxCompletions; after that many, pr.complete_gave_up once. A
// completion of sum queued already is put back in line when the PR left it
// before its round started; one whose round started and left the PR
// reviewed (a round that heard no late reviewer, a failed one the operator
// took out of needs_attention) spent its attempt. Nothing is queued on a
// head the operator stopped the completions of. Run at every auto-approval
// pass (the round's end, each tick): what it queued once it never queues
// again.
func (e *Engine) considerCompletion(ctx context.Context, c store.RepoPR, sum store.ReviewSummary) {
	w := e.cfg.WatchFor(c.Repo)
	if w == nil || e.d.Layout.Scratch != "" {
		return
	}
	pr, err := e.st.PRByID(ctx, c.PR.ID) // the PR now, not the pass's snapshot
	if err != nil {
		return
	}
	c.PR = pr
	if pr.State != store.PRReviewed || pr.Forced || pr.DetailsAt == nil || sum.SHA != pr.HeadSHA || sum.ReviewID != deref(pr.LastReviewID) {
		return
	}
	rec := e.completionOf(ctx, pr.ID).forHead(pr.HeadSHA)
	if rec.GaveUp || rec.Stopped {
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
		if !slices.Contains(roles, r.Role) {
			roles, causes = append(roles, r.Role), append(causes, r.Status)
		}
	}
	again := rec.ReviewID == sum.ReviewID && len(rec.Causes) == len(rec.Roles) && len(rec.Roles) > 0
	if again {
		roles, causes = rec.Roles, rec.Causes // what the last completion of sum found (settleCompletion), not the review's
	}
	if len(roles) == 0 {
		return
	}
	if i := slices.IndexFunc(causes, func(s string) bool { return !slices.Contains(passingCauses, s) }); i >= 0 {
		if again { // its completion found what needs the operator or time: no other is spent on a stale cause
			e.giveUpCompletion(ctx, repoOf(c.Repo), pr, rec, fmt.Sprintf("the completion of round %d found %s (%s), which passes not by itself",
				rec.Round, roles[i], causes[i]), map[string]any{"round": rec.Round, "roles": roles, "causes": causes})
		}
		return // the operator's or time's: the pauses wait for it
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
	if ran := len(rec.Rounds); ran >= maxCompletions {
		e.giveUpCompletion(ctx, repoOf(c.Repo), pr, rec, fmt.Sprintf("round %d went without %s on %s too", run.Round, strings.Join(roles, ", "),
			textx.ShortSHA(pr.HeadSHA)), map[string]any{"round": run.Round, "roles": roles, "causes": causes})
		return
	}
	if dec := e.classify(ctx, *w, pr, now); !dec.Eligible {
		return
	}
	if !again || rec.Anchor == nil {
		rec.Anchor = pr.LastRoundStartedAt // the start the completion's rounds give back
	}
	rec.ReviewID, rec.Round, rec.Base, rec.Roles, rec.Causes = sum.ReviewID, run.Round, base, roles, causes
	rec.Attempt, rec.Queued, rec.Started = len(rec.Rounds)+1, now.UTC(), time.Time{}
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

// giveUpCompletion records that the head of rec, pr's, gets no more
// completions, with one pr.complete_gave_up (why, and data besides the
// head and the rounds the head started).
func (e *Engine) giveUpCompletion(ctx context.Context, repo store.Repo, pr store.PR, rec completion, why string, data map[string]any) {
	rec.GaveUp, rec.ReviewID, rec.Queued = true, 0, time.Time{}
	e.saveCompletion(ctx, pr.ID, rec)
	data["head"], data["attempts"] = rec.Head, len(rec.Rounds)
	e.event(ctx, "warn", prSubject(repo, pr.Number), "pr.complete_gave_up",
		fmt.Sprintf("%s; no more completions of the head after %d: the refusal stands", why, maxCompletions), data)
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
// (m: its record) rests on: round itself, or, for a round of the judge
// alone, the round it carried the missing reports from; when that round
// is one of the head's completion rounds (rec.Rounds), the base of those
// completions instead (rec.Base: a completion's round rests on the reports
// that stand from it, and the pipeline reads each role's latest run of the
// rounds from the base on), while any other round that ran its reviewers
// is the base of its own completion. ok is false when the carried reports come from more
// than one round, or the base round has no run on pr's head: a delta check
// carries reports of an older head, which a completion of this one would
// not complete (a limit of the design).
func (e *Engine) completionBase(ctx context.Context, pr store.PR, rec completion, round int, m pipeline.MissingReports) (int, bool) {
	base := round
	if len(m.Missing) == 0 && len(m.Carried) > 0 {
		base = m.Carried[0].Round
		for _, r := range m.Carried {
			if r.Round != base {
				return 0, false
			}
		}
	}
	if rec.Base > 0 && rec.Base < base && slices.Contains(rec.Rounds, base) {
		base = rec.Base
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
// asked) while one is due (completion.dueFor), or one whose last round a
// stop cut short, which it resumes (interrupted, rerun: no role runs
// again); nil otherwise.
func (e *Engine) completionDue(ctx context.Context, pr store.PR, sameHead, requested bool) *completion {
	if !sameHead || requested || pr.Forced {
		return nil
	}
	rec := e.completionOf(ctx, pr.ID)
	switch {
	case rec.dueFor(pr):
	case rec.interrupted(pr):
		rec.resume = true
	default:
		return nil
	}
	return &rec
}

// completionWait is when pr's completion, due (or interrupted) and
// nobody's forced round, was queued, or its last round started
// (eligibility.PRFacts.CompleteAt); ok is false when none is due.
func (e *Engine) completionWait(ctx context.Context, pr store.PR) (time.Time, bool) {
	if pr.Forced {
		return time.Time{}, false
	}
	rec := e.completionOf(ctx, pr.ID)
	if !rec.dueFor(pr) && !rec.interrupted(pr) {
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

// startCompletion numbers the completion round rs's round is (rs.round:
// the PR's next round, which the pipeline takes as given) and records it,
// as it starts, among the head's completion rounds, which no crash, retry
// or restart takes back: each is one of the head's maxCompletions. A
// completion that resumes its head's last round runs in that round again,
// which spends nothing. A round that is none (or a continue) is left
// alone.
func (e *Engine) startCompletion(ctx context.Context, prID int64, rs *roundSetup, at time.Time) {
	if !rs.reruns() || rs.kind == kindContinue {
		return
	}
	rec := e.completionOf(ctx, prID).forHead(rs.target)
	if rs.complete.resume && len(rec.Rounds) > 0 {
		rs.round = slices.Max(rec.Rounds) // the round a stop cut short
	} else {
		runs, err := e.st.RunsByPR(ctx, prID)
		if err != nil {
			e.warnUnlessStopped(ctx, err, "completion: the PR's rounds", "pr", prID)
			return
		}
		rs.round = 1
		for _, r := range runs {
			rs.round = max(rs.round, r.Round+1)
		}
	}
	if !slices.Contains(rec.Rounds, rs.round) {
		rec.Rounds = append(rec.Rounds, rs.round)
	}
	rec.Queued, rec.Started = time.Time{}, at.UTC()
	e.saveCompletion(ctx, prID, rec)
}

// settleCompletion writes, at the end of a completion's round
// (in.Complete), pr.complete_unheard when it heard none of its late roles
// again (pipeline.OutcomeUnheard: no review), with what each of them ended
// on as the record's causes, which decide whether the next completion of
// the review runs (considerCompletion), and pr.complete_failed when it
// ended without a review for another reason (not stopped by a shutdown,
// nor paused for a continue).
func (e *Engine) settleCompletion(ctx context.Context, job *roundJob, in pipeline.RoundInput, res pipeline.RoundResult, outcome string, cancelled bool) {
	if in.Complete == nil || cancelled {
		return
	}
	rec := e.completionOf(ctx, job.pr.ID)
	subject := prSubject(job.repo, job.pr.Number)
	data := map[string]any{"round": in.Complete.Round, "head": in.TargetSHA, "attempt": len(rec.Rounds), "outcome": outcome}
	switch outcome {
	case pipeline.OutcomePosted, pipeline.OutcomeReplied, pipeline.OutcomeDryRun,
		pipeline.OutcomeUsageLimit, pipeline.OutcomeLoginRequired, pipeline.OutcomeOverloaded:
	case pipeline.OutcomeUnheard:
		if rec.Head == in.TargetSHA && len(rec.Roles) > 0 {
			causes := make([]string, len(rec.Roles))
			for i, role := range rec.Roles {
				causes[i] = cmp.Or(res.Reports[agents.Role(role)].Status, pipeline.ReportMissing)
			}
			rec.Causes, data["causes"] = causes, causes
			e.saveCompletion(ctx, job.pr.ID, rec)
		}
		e.event(ctx, "info", subject, "pr.complete_unheard",
			fmt.Sprintf("the completion of round %d (round %d) heard none of %s again; no review posted",
				in.Complete.Round, res.Round, strings.Join(in.Complete.Roles, ", ")), data)
	default:
		data["stage"] = "round"
		e.event(ctx, "warn", subject, "pr.complete_failed",
			fmt.Sprintf("the completion of round %d ended %s in round %d (completion %d of %d of the head)",
				in.Complete.Round, outcome, res.Round, len(rec.Rounds), maxCompletions), data)
	}
}

// endSpentCompletion settles the failed round of a completion of round
// completed (0 = the round was none) when it was the head's last
// (maxCompletions started): a retry would be one more, so the PR goes back
// to reviewed at the review it completes, with the re-review interval
// given back and pr.complete_gave_up; false when the round was none, the
// head has rounds left (the PR retries it as any failed round) or moved
// (its round reviews the new head).
func (e *Engine) endSpentCompletion(ctx context.Context, job *roundJob, pr store.PR, completed int, from []string, outcome string) bool {
	if completed == 0 {
		return false
	}
	rec := e.completionOf(ctx, pr.ID)
	if rec.Head == "" || rec.Head != pr.HeadSHA || rec.Head != deref(pr.ReviewedSHA) || len(rec.Rounds) < maxCompletions {
		return false
	}
	err := e.st.TransitionPR(ctx, pr.ID, from, store.PRReviewed, func(u *store.PRUpdate) {
		u.Where("head_sha", rec.Head)
		u.Where("reviewed_sha", rec.Head)
		giveBackStart(u, pr, rec)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
		u.Set("next_eligible_at", nil)
		u.Set("last_error", nil)
	})
	if err != nil {
		if !errors.Is(err, store.ErrConflict) {
			e.log.Warn("completion: the PR back to reviewed", "pr", pr.ID, "err", err)
		}
		return false
	}
	e.giveUpCompletion(ctx, job.repo, pr, rec, fmt.Sprintf("the completion of round %d ended %s in its last round", completed, cmp.Or(outcome, "without a review")),
		map[string]any{"round": completed, "outcome": outcome})
	return true
}

// giveBackStart has u give back the start of the completion round rec
// last started (rec.Started) to the throttle: last_round_started_at back to
// the start of the round the completion completes (rec.Anchor), also after
// a retried round or the continue of a paused one. A completion is no
// review of new code, so the re-review interval of the next push runs from
// the round it completes. It leaves the start alone when another round
// recorded one since.
func giveBackStart(u *store.PRUpdate, pr store.PR, rec completion) {
	if !rec.Started.IsZero() && pr.LastRoundStartedAt != nil && pr.LastRoundStartedAt.Equal(rec.Started) {
		u.Set("last_round_started_at", rec.Anchor) // nil stores NULL
	}
}

// onUnheard settles a completion's round that heard none of its late
// roles again (pipeline.OutcomeUnheard): nothing posted, so the PR goes back
// to reviewed at the review it completes (or waits for the re-review of a
// head that moved meanwhile, settleHead), with the re-review interval given
// back; its round is spent (startCompletion), and the next auto-approval
// pass queues the next completion or gives up.
func (e *Engine) onUnheard(ctx context.Context, job *roundJob, pr store.PR, in pipeline.RoundInput, from []string) {
	now := e.now()
	rec := e.completionOf(ctx, pr.ID)
	m := e.settleHead(ctx, job, pr, in.TargetSHA, now)
	set := func(u *store.PRUpdate) {
		if m.to == store.PRReviewed {
			u.Where("head_sha", m.reviewed)
			if m.trivial != nil {
				u.Set("reviewed_sha", m.reviewed)
			}
			u.Set("next_eligible_at", nil)
		} else {
			u.Set("next_eligible_at", m.next)
			if pr.PendingSince == nil {
				u.Set("pending_since", pr.HeadChangedAt)
			}
		}
		giveBackStart(u, pr, rec)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
		u.Set("last_error", nil)
	}
	err := e.st.TransitionPR(ctx, pr.ID, from, m.to, set)
	if errors.Is(err, store.ErrConflict) && m.to == store.PRReviewed {
		// The head moved after the read: the PR waits for its re-review.
		if cur, rerr := e.st.PRByID(ctx, pr.ID); rerr == nil && slices.Contains(from, cur.State) && cur.HeadSHA != m.reviewed {
			pr, m.to, m.next, m.trivial = cur, store.PRRereviewPending, e.rereviewAt(ctx, cur, job.watch, in.TargetSHA, now), nil
			err = e.st.TransitionPR(ctx, pr.ID, from, m.to, set)
		}
	}
	if err != nil {
		e.log.Info("completion heard no one: the PR moved on", "pr", pr.ID, "err", err)
	}
}

// supersedeCompletion moves the PR's record to its new head when the head
// moved: the push's round reviews the new head in full, a completion that
// waited is dropped, and the old head's spent completions stay in Past.
func (e *Engine) supersedeCompletion(ctx context.Context, pr store.PR) {
	rec := e.completionOf(ctx, pr.ID)
	if rec.Head == "" || rec.Head == pr.HeadSHA {
		return
	}
	if rec.ReviewID != 0 && !rec.GaveUp && !rec.Stopped {
		e.log.Info("completion dropped: a push", "pr", pr.ID, "head", textx.ShortSHA(pr.HeadSHA), "completed", textx.ShortSHA(rec.Head))
	}
	e.saveCompletion(ctx, pr.ID, rec.forHead(pr.HeadSHA))
}

// dropCompletion drops the completion that waits on PR prID's head as
// GitHub closed the PR; the head's spent completions stay, so a reopen
// gets no more than the head had left.
func (e *Engine) dropCompletion(ctx context.Context, prID int64) {
	rec := e.completionOf(ctx, prID)
	if rec.ReviewID == 0 {
		return
	}
	rec.ReviewID, rec.Queued = 0, time.Time{}
	e.saveCompletion(ctx, prID, rec)
}

// stopCompletion stops, at the operator's abort or ignore of pr, the
// completions of its head when one waits or runs (waitsFor): it is
// dropped and no later auto-approval pass queues one there, nor after a
// push back to the head; a new head gets its own. The stop of any other
// round leaves them alone. The callers stop it before they move the PR out
// of line, so no auto-approval pass in between queues it again. It returns
// what it did for the stop's result ("" = no completion waited or ran).
func (e *Engine) stopCompletion(ctx context.Context, repo store.Repo, pr store.PR) string {
	if cur, err := e.st.PRByID(ctx, pr.ID); err == nil {
		pr = cur
	}
	rec := e.completionOf(ctx, pr.ID)
	if !rec.waitsFor(pr) {
		return "" // the stop is of another round: the head's completions stay as they are
	}
	rec.Stopped, rec.ReviewID, rec.Queued = true, 0, time.Time{}
	e.saveCompletion(ctx, pr.ID, rec)
	note := "completions of " + textx.ShortSHA(pr.HeadSHA) + " stopped"
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.complete_stopped",
		fmt.Sprintf("the completion of round %d stopped: no completion runs on %s, a new head gets its own", rec.Round, textx.ShortSHA(pr.HeadSHA)),
		map[string]any{"round": rec.Round, "head": pr.HeadSHA, "attempts": len(rec.Rounds)})
	return note
}

// completionInput is the pipeline's completion of rs's round: its review's
// round and base, the roles it runs, and what the base round's reviewers
// re-reviewed from (the head reviewed before it, when it was a re-review;
// "" = the whole PR) with whether those commits were force-pushed or merged
// a branch in, from the base round's runs on the round's head (a role it
// runs first, else any).
func (e *Engine) completionInput(ctx context.Context, job *roundJob, rs roundSetup, base string) *pipeline.Completion {
	c := rs.complete
	out := &pipeline.Completion{Round: c.Round, Base: c.Base, Roles: slices.Clone(c.Roles), Resume: c.resume}
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
