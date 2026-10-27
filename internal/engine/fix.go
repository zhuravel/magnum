package engine

// The fix job: a fixer agent fixes magnum's own findings on the operator's
// PR, in the PR's slot and as the operator, and the PR then gets a forced
// re-review of what it pushed. `magnum fix <ref>` asks for it (ReqFix). The
// job holds the PR with a reservation of its own (roundHandle.fix) for its
// whole run, so no round, open or eviction touches the slot meanwhile, and
// keeps where the loop is in the PR's KVPRFix row (fix_state.go): while the
// fixer works (phase fixing) dispatch holds the PR's rounds (fixHolds,
// WaitFixing), and while the row exists `magnum review` and a second loop
// are refused. The job's steps are begin/ok/fail steps on the PR's subject
// (package steps): claim (only for a PR whose slot the pool took back: a
// slot at the reviewed head, as `magnum open` claims one), guard (the slot
// is the PR's, nobody else is in it, it holds the reviewed head; then the
// base is fetched and a conflict with it becomes the fixer's to merge
// first, checkFixBase), start (the
// fixer's session in the PR's workspace, a run of kind fix, its prompt) and
// wait (the run's end; the time-up line on timeout); the settle
// (fix_settle.go) reads the report, puts the slot back in order and ends the
// cycle. A daemon that stops while the fixer works leaves the row in phase
// fixing, and the next start ends that loop (recoverFixes).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// ReqFix starts a fix loop on a PR, or switches the fix job off or on for
// it (FixPayload).
const ReqFix = "fix"

// FixPayload is a `magnum fix` request: start a fix loop on the PR (DryRun:
// answer what would happen and do nothing), or switch the fix job off for it
// (Off: autofix leaves it alone, KVPRFixOff) or on again (On). Neither Off
// nor On starts a loop.
type FixPayload struct {
	PRTarget
	DryRun bool `json:"dry_run,omitempty"`
	Off    bool `json:"off,omitempty"`
	On     bool `json:"on,omitempty"`
}

const (
	// fixBy is who starts a loop asked for by hand (FixState.By) and the
	// re-review it forces.
	fixBy = "magnum fix"
	// fixOffBy is who switches the fix job off (FixOff.By).
	fixOffBy = "magnum fix --off"
	// fixPoll is how often the job looks at its run while the fixer works.
	fixPoll = pipeline.DefaultPollInterval
)

// fixTimeUpText is the last call to a fixer whose time ran out: its budget,
// its report file and the marker the report starts with.
const fixTimeUpText = "Time is up: this fix had %s. Stop every background task you started with TaskStop and start nothing new. " +
	"Write the report to %s now (its first line: `%s`) with what you pushed and replied so far, put every finding you did not finish under `## Needs you`, and end your turn."

// errFixTimeout is a fix run that did not end within its time.
var errFixTimeout = errors.New("fix run timed out")

// errFixLoopGone refuses a next cycle whose loop is no longer the one waiting
// for its re-review (a stop ended it, or another cycle took it on): nobody
// asked for that cycle any more, so the hook lets it go quietly.
var errFixLoopGone = errors.New("the fix loop is no longer waiting for its re-review")

// errFixDraining refuses a fix cycle while the daemon drains for a restart,
// which would end its fixer ("fix run lost in a daemon restart"); a loop
// waiting for its next cycle keeps its row for the restart's recovery, and
// `magnum fix` never sees it: requestFix holds the request (holdFix).
var errFixDraining = errors.New("the daemon is draining; try after the restart")

// kindPausedError is prepareFix's refusal while the fix role's agent kind
// is paused (a usage limit, a login): its text is the refusal a `magnum fix`
// answers, and a held fix waits for the pause of kind to end instead of
// being dropped (startHeldFix, heldPause).
type kindPausedError struct {
	kind, text string
}

func (e *kindPausedError) Error() string { return e.text }

// fixJob is one cycle of a fix loop: the PR, its watch and slot, the fix
// role, the review the fixer fixes and the loop's row.
type fixJob struct {
	repo   store.Repo
	watch  config.Watch
	pool   *config.Pool
	pr     store.PR
	slot   store.Slot
	role   config.Role
	review store.ReviewSummary // the review the run fixes: its run, head, URL and findings
	head   string              // the head the fixer starts from: the review's, or a later one that kept its verdict (reviewStood)
	round  int                 // the review's round, the fix run's
	login  string              // the login that posted the review, whose threads the fixer answers
	notes  []string            // what the request's answer adds ("this watch posts as you: ...")
	state  FixState            // the loop's row as the job writes it
	run    store.Run           // the fixer's run, once fixStart created it
	// started is when fixStart created the run: a reply of the fixer's on
	// GitHub since then is this run's (dropFixerAnswered).
	started time.Time
	// others is what the other reviewers wrote, as prepareFix read it before
	// the start (nil when the read failed, or the budget held the job):
	// writeExternal writes these without reading GitHub again. alone: the
	// review found nothing and left nothing open, and the job, a new loop,
	// answers their unanswered items alone (spec 96).
	others *externalRead
	alone  bool
	// budget is set when prepareFix found the GitHub budget of the identity
	// the job reads as below the floor (github_floor.go), or GitHub refused
	// its read of the other reviewers' items for the rate limit and the
	// meter then said so: it took no pin off, and the job is held (startFix
	// refuses with githubBudgetError). othersDeferred: the other reviewers'
	// items are read when the held fix starts.
	budget         *githubLowBudget
	othersDeferred bool
	// touched: the fixer may have changed the slot (its prompt was sent), so
	// the settle and a stop put the slot back in order.
	touched bool
	// schema is what the job's claim did to the slot's databases ("loaded
	// the schema of …", or why it could not; "" for none, and for a slot the
	// PR had): fixClaim sets it, the fix.start event says it.
	schema string
	// conflict is the PR's base whose tip conflicts with the head the fixer
	// starts from (baseConflict), which the fixer merges first: prepareFix
	// looks for a job on a review that found nothing, whose work it may be,
	// and the guard looks again for every job, in the slot the fixer works
	// in (checkFixBase). Zero for a clean merge, or when nobody could check.
	// forMerge: the merge is all the job starts for (a review that found
	// nothing, and no other reviewer's item waits).
	conflict fixConflict
	forMerge bool
}

// fixConflict is a merge of the PR's base into its head that conflicts:
// the base branch, the tip of it magnum fetched and the conflicting paths,
// sorted (zero: none).
type fixConflict struct {
	base, sha string
	paths     []string
}

// words say what the fixer does about c: "merges master at c0c0c0c (2
// conflicting files)".
func (c fixConflict) words() string {
	return fmt.Sprintf("merges %s at %s (%s)", c.base, textx.ShortSHA(c.sha), textx.Count(len(c.paths), "conflicting file", "conflicting files"))
}

func (j *fixJob) label() string   { return fmt.Sprintf("%s#%d", j.repo.FullName(), j.pr.Number) }
func (j *fixJob) subject() string { return prSubject(j.repo, j.pr.Number) }

// asRound is the job as a roundJob, for the helpers rounds share with it
// (paneEnv, needsAttention).
func (j *fixJob) asRound() *roundJob {
	return &roundJob{pr: j.pr, repo: j.repo, watch: j.watch, pool: j.pool, slot: j.slot, hasSlot: j.slot.ID != 0}
}

// fixes is what the job does where, in words, after its role's name:
// "fixes the 2 findings of the review on b2b2b2b in review1", counting the
// new ones and the earlier P0 to P2 it keeps open
// (ReviewSummary.OpenProblems), with ", 1 of them open from earlier
// reviews," when it keeps some; "fixes the open earlier findings of the
// review on b2b2b2b in review1" for a next cycle's review that requests
// changes with neither (fixRemains). A review on an older commit than the
// head the fixer starts from adds "(the head c3c3c3c kept its verdict)". A
// job on a review that found nothing (others) "answers the other
// reviewers' 2 unanswered threads and comments in review1 (magnum's review
// of b2b2b2b found nothing)". A job whose PR has no slot yet works "in a
// slot it claims" (fixClaim). A job whose head conflicts with its base
// starts with "merges master at c0c0c0c (2 conflicting files), then ", and
// one that starts for the merge alone (forMerge) "merges master at c0c0c0c
// (2 conflicting files) in review1 (magnum's review of b2b2b2b found
// nothing)".
func (j *fixJob) fixes() string {
	s := j.work()
	if j.conflict.sha != "" && !j.forMerge {
		s = j.conflict.words() + ", then " + s
	}
	return s
}

// work is fixes() without the merge a job with findings or other
// reviewers' items starts with.
func (j *fixJob) work() string {
	sha, kept := textx.ShortSHA(j.review.SHA), ""
	if j.head != "" && j.head != j.review.SHA {
		kept = fmt.Sprintf("the head %s kept its verdict", textx.ShortSHA(j.head))
		sha += " (" + kept + ")"
	}
	where := j.slot.Name
	if j.slot.ID == 0 {
		where = "a slot it claims"
	}
	note := "magnum's review of " + textx.ShortSHA(j.review.SHA) + " found nothing"
	if kept != "" {
		note += "; " + kept
	}
	open := j.review.OpenProblems()
	n := j.review.Findings() + open
	switch {
	case j.forMerge && j.conflict.sha != "":
		return fmt.Sprintf("%s in %s (%s)", j.conflict.words(), where, note)
	case j.alone && j.othersDeferred:
		return fmt.Sprintf("answers what the other reviewers left unanswered, read once the GitHub budget recovers, in %s (magnum's review of %s found nothing)",
			where, textx.ShortSHA(j.review.SHA))
	case j.alone && j.others != nil:
		threads, conversation := j.others.items.unanswered()
		return fmt.Sprintf("answers the other reviewers' %s in %s (%s)",
			textx.Count(threads+conversation, "unanswered thread or comment", "unanswered threads and comments"), where, note)
	case n == 0:
		return fmt.Sprintf("fixes the open earlier findings of the review on %s in %s", sha, where)
	case open == 0:
		return fmt.Sprintf("fixes the %s of the review on %s in %s", textx.Count(n, "finding", "findings"), sha, where)
	}
	return fmt.Sprintf("fixes the %s of the review on %s, %d of them open from earlier reviews, in %s",
		textx.Count(n, "finding", "findings"), sha, open, where)
}

// fixPin is what prepareFix does about a pinned PR or slot.
type fixPin int

const (
	// fixPinRefuses: autofix and a loop's next cycle, which nobody asked for
	// just now, refuse a pinned slot, as a pin holds every automatic round.
	fixPinRefuses fixPin = iota
	// fixPinUnpins: `magnum fix` takes the pin off as `magnum review` does
	// (unpinFor); a hold a guard persisted, or one its guard finds once the
	// pin went, still refuses.
	fixPinUnpins
	// fixPinWouldUnpin: a dry run of `magnum fix` says it would.
	fixPinWouldUnpin
)

// fixRunningSentence is the refusal while a loop runs on the PR.
func fixRunningSentence(st FixState) string {
	return fmt.Sprintf("fixing, cycle %d of %d; `magnum abort` stops it", st.Cycle, st.Cap)
}

// requestFix handles `magnum fix`: --off and --on set and clear the PR's
// opt-out of autofix; otherwise a fix loop starts (startFix) unless one of
// prepareFix's refusals applies, and the answer says what runs, with the
// notes that concern it. While the daemon drains for a restart the request
// is held for it instead (holdFix). A dry run (the request's or the
// daemon's) answers what would start. A whiteboard job holding the PR is
// refused with whiteboardHeld once every other refusal passed: it yields to
// the request (answerYielding). That refusal carries the other reviewers'
// items prepareFix read, and the request's pass after the yield gets them
// back as carried: it runs every refusal again but reads GitHub no second
// time at the same head.
func (e *Engine) requestFix(ctx context.Context, p FixPayload, carried *externalRead) (string, error) {
	repo, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		return "", err
	}
	subject := prSubject(repo, pr.Number)
	switch {
	case p.Off && p.On:
		return "", errors.New("magnum fix: --off and --on together")
	case p.Off:
		if p.DryRun {
			return fmt.Sprintf("dry run: would switch fix off for PR #%d", pr.Number), nil
		}
		if err := e.setFixOff(ctx, pr.ID, FixOff{Since: e.now().UTC(), By: fixOffBy}); err != nil {
			return "", err
		}
		e.event(ctx, "info", subject, "fix.off", "fix switched off by "+fixOffBy+": autofix leaves the PR alone", map[string]any{"by": fixOffBy})
		return fmt.Sprintf("fix is off for PR #%d: autofix leaves it to you; `magnum fix --on` lifts it", pr.Number), nil
	case p.On:
		if p.DryRun {
			return fmt.Sprintf("dry run: would switch autofix on again for PR #%d", pr.Number), nil
		}
		if err := e.clearFixOff(ctx, pr.ID); err != nil {
			return "", err
		}
		e.event(ctx, "info", subject, "fix.on", "fix switched on again by magnum fix --on", nil)
		return fmt.Sprintf("autofix is on again for PR #%d", pr.Number), nil
	}
	_, off, err := e.fixOff(ctx, pr.ID)
	if err != nil {
		return "", err
	}
	var job *fixJob
	dry := p.DryRun || e.d.DryRun
	switch {
	case e.draining(ctx): // held for the restart (fix_held.go), which would end a fixer started now
		return e.holdFix(ctx, pr, dry, fixBy, fixHold{})
	case dry:
		job, err = e.prepareFix(ctx, pr, FixState{}, fixPinWouldUnpin, carried)
		if err == nil && job.budget != nil { // github_floor.go: it would be held
			return e.holdFix(ctx, pr, true, fixBy, fixHold{low: job.budget, job: job})
		}
	case e.whiteboardHolds(pr.ID): // the job yields to the request once every other refusal passed (answerYielding)
		pre, err := e.prepareFix(ctx, pr, FixState{}, fixPinWouldUnpin, carried)
		if err != nil {
			return "", err
		}
		var carry any
		if pre.others != nil {
			carry = pre.others
		}
		return "", heldByWhiteboard(repo, pr, carry)
	default:
		job, err = e.startFix(ctx, pr, fixBy, FixState{}, carried)
	}
	var budget *githubBudgetError
	switch {
	case errors.Is(err, errFixDraining): // a drain began since draining was asked
		return e.holdFix(ctx, pr, false, fixBy, fixHold{})
	case errors.As(err, &budget): // github_floor.go: held until the budget recovers
		return e.holdFix(ctx, pr, false, fixBy, fixHold{low: &budget.low, job: budget.job})
	case err != nil:
		return "", err
	}
	if dry && e.whiteboardHolds(pr.ID) {
		job.notes = append(job.notes, "would stop the whiteboard writer first")
	}
	what := fmt.Sprintf("%s %s, cycle %d of %d", job.role.Name, job.fixes(), max(job.state.Cycle, 1), job.state.Cap)
	res := fmt.Sprintf("fixing %s: %s; `magnum abort %s` stops it", job.label(), what, job.label())
	if dry {
		e.rec.Record(ctx, subject, "fix", what)
		res = fmt.Sprintf("dry run: would start fixing %s: %s, then a forced re-review of what it pushes", job.label(), what)
	}
	notes := job.notes
	if off {
		notes = append(notes, "fix is off for this PR: this requested loop runs anyway")
	}
	for _, n := range notes {
		res += "; " + n
	}
	return res, nil
}

// prepareFix decides whether a fix cycle may start on pr and gathers what it
// needs; the error is the refusal, a sentence for the request's answer. prev
// is the loop a next cycle continues (zero for a new loop, which a loop
// already running refuses). pin says what a pinned PR or slot does: it
// refuses, or, for `magnum fix`, once every other refusal passed, its pin
// goes as for `magnum review` (the job's notes say so for the answer). A
// drain for a restart is startFix's refusal (errFixDraining), not this one:
// holdFix asks it under a drain, so a request it would refuse after the
// restart is refused at once, not held. A paused agent kind of the fix
// role refuses with kindPausedError, which a held fix waits out. Once every
// other refusal passed, and before the pin goes, it reads the other
// reviewers' items for every job (othersBeforeStart, spec 102), which the
// job's external file gets: a review that found nothing is refused when
// none waits for an answer or the read fails (othersWaiting), unless the
// head conflicts with the PR's base (baseConflict in the PR's slot, else
// in its clone; fetched first but for a request only asked about), which
// the job then merges first and alone (forMerge). carried is
// the other reviewers' items the same request read before (nil = none),
// used instead of reading GitHub again at the same head. Below the GitHub
// floor of the identity the job reads as (github_floor.go), or when GitHub
// refuses that read for the rate limit and the meter then says the budget
// is below it, it takes no pin off: the job carries the low budget
// (fixJob.budget) for startFix to hold it, and the other reviewers' items
// are read when the held fix starts (othersDeferred). A PR whose slot the
// pool took back gets a job without a slot, which claims one when it runs
// (fixClaim); a pool without a free slot refuses it before the pin goes
// (fixSlotFree).
func (e *Engine) prepareFix(ctx context.Context, pr store.PR, prev FixState, pin fixPin, carried *externalRead) (*fixJob, error) {
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return nil, err
	}
	full := repo.FullName()
	label := fmt.Sprintf("%s#%d", full, pr.Number)
	w := e.cfg.WatchFor(full)
	if w == nil {
		return nil, fmt.Errorf("%s is not watched (no [[watch]] covers it)", full)
	}
	// github_floor.go: below the floor of the identity the job reads as, the
	// job is prepared without reading GitHub or taking a pin off, and held.
	low, isLow := e.githubLow(w.PollIdentity)
	role, ok := e.cfg.FixRoleFor(w)
	if !ok {
		return nil, fmt.Errorf("no fix role for %s", full)
	}
	fixer := e.cfg.IdentityByName(role.Identity)
	switch {
	case fixer == nil:
		return nil, fmt.Errorf("no identity %s for the fix role", role.Identity)
	case fixer.Kind != "gh": // a poll identity the role took (an explicit App is a config error)
		return nil, fmt.Errorf("the fix role's identity %s is an App; name a gh identity", fixer.Name)
	}
	cur, running, err := e.fixState(ctx, pr.ID)
	switch {
	case err != nil:
		return nil, err // unreadable: never a second loop over it
	case running && prev.Cycle == 0:
		return nil, errors.New(fixRunningSentence(cur))
	case prev.Cycle > 0 && !(running && sameWaitingLoop(cur, prev)):
		return nil, fmt.Errorf("%s: %w", label, errFixLoopGone)
	}
	switch author := deref(pr.AuthorLogin); {
	case author == "":
		return nil, fmt.Errorf("PR #%d's author is not known yet (no details from GitHub); the fixer pushes and replies as you", pr.Number)
	case !e.ownPR(pr):
		return nil, fmt.Errorf("PR #%d is %s's, not yours; the fixer pushes and replies as you", pr.Number, author)
	}
	if pr.IsCrossRepo {
		return nil, errors.New("the head lives in a fork magnum cannot push to")
	}
	if pr.GHState != store.GHOpen {
		return nil, fmt.Errorf("%s is not open (GitHub %s): nothing to fix", label, pr.GHState)
	}
	if slices.Contains(store.InFlightStates, pr.State) || pr.State == store.PRPaused || e.fixHeld(pr.ID, pin) {
		return nil, fmt.Errorf("%s: a round is already in progress (%s)", label, pr.State)
	}
	sums, err := e.st.LastReviewSummaries(ctx, []int64{pr.ID})
	if err != nil {
		return nil, err
	}
	head := textx.ShortSHA(pr.HeadSHA)
	sum, ok := sums[pr.ID]
	stood := false
	if ok {
		if stood, err = e.reviewStood(ctx, pr, sum); err != nil {
			return nil, err
		}
	}
	on := textx.ShortSHA(sum.SHA)
	switch {
	case !ok || deref(pr.ReviewedSHA) != pr.HeadSHA || (sum.SHA != pr.HeadSHA && !stood):
		return nil, fmt.Errorf("nothing to fix: no review on %s", head)
	case sum.URL == "":
		return nil, fmt.Errorf("%s: the review on %s has no URL to fix from", label, on)
	case deref(pr.HeadRef) == "":
		return nil, fmt.Errorf("%s: its branch is not known yet (no details from GitHub)", label)
	}
	src := e.d.Identities[pr.Identity]
	if src == nil || src.Login() == "" {
		return nil, fmt.Errorf("identity %q is not configured", pr.Identity)
	}
	reviewRun, err := e.st.RunByID(ctx, sum.RunID)
	if err != nil {
		return nil, fmt.Errorf("%s: the review's run %s: %w", label, sum.RunID, err)
	}
	slot, has, err := e.slotOf(ctx, pr.ID)
	if err != nil {
		return nil, err
	}
	if err := fixSlotRefusal(label, pr, slot, has, pin, stood, on); err != nil {
		return nil, err
	}
	now := e.now()
	if s, ok := e.snoozeOf(ctx, pr.ID, now); ok {
		return nil, fmt.Errorf("%s is snoozed until %s; `magnum snooze %s --off` lifts it", label, waitClock(s.Until, now), label)
	}
	if until := e.cfg.Daemon.CooldownUntil(pr.HumanActiveAt); now.Before(until) {
		return nil, fmt.Errorf("%s: someone typed into its panes; run `magnum fix %s` again after %s (human_cooldown)", label, label, waitClock(until, now))
	}
	if kind, why := e.pausedKind(ctx, []string{role.AgentKind()}); why != "" {
		return nil, &kindPausedError{kind: kind, text: fmt.Sprintf("%s: %s", label, why)}
	}
	// The other reviewers' items, read before the start (spec 102): a
	// review that found nothing and left nothing open leaves the fixer only
	// what waits for an answer there (spec 96; a next cycle's changes
	// request may keep earlier findings open with neither, fixRemains). A
	// read GitHub refuses for the rate limit holds the job as the floor does.
	alone := prev.Cycle == 0 && sum.Findings()+sum.OpenProblems() == 0
	var others *externalRead
	// A conflict with the base is work too (spec 5.6): othersWaiting asks
	// for it only when nothing else would start the job, and a request only
	// asked about (fixPinWouldUnpin) looks at the base as last fetched.
	var conflict fixConflict
	merges := func() (bool, error) {
		dir, clone := slot.Path, slot.MainClone
		if !has {
			dir = e.cloneOf(repo)
			clone = dir
		}
		var err error
		if conflict, err = e.baseConflict(ctx, repo, pr, dir, clone, pr.HeadSHA, pin != fixPinWouldUnpin); err != nil {
			err = fmt.Errorf("its merge with %s could not be checked: %w", cmp.Or(deref(pr.BaseRef), repo.DefaultBranch, "its base"), err)
		}
		return conflict.sha != "", err
	}
	if !isLow {
		var held *githubLowBudget
		probe := &fixJob{repo: repo, watch: *w, pr: pr, login: src.Login()}
		if others, held, err = e.othersBeforeStart(ctx, probe, on, alone, carried, merges); err != nil { // fix_external.go
			return nil, err
		}
		if held != nil {
			low, isLow = *held, true
		}
	}
	if err := e.fixSlotFree(ctx, repo, pr, label, has, isLow, pin); err != nil {
		return nil, err
	}
	var notes []string
	switch {
	case pin == fixPinRefuses || !isPinned(pr, slot, has):
	case pin == fixPinWouldUnpin:
		what, origin := e.pinnedWhat(ctx, repo, pr, slot, has)
		notes = append(notes, "would unpin "+what+origin+" to fix it")
	case isLow: // the pin goes when the held fix starts
		what, origin := e.pinnedWhat(ctx, repo, pr, slot, has)
		notes = append(notes, "unpins "+what+origin+" once it starts")
	default:
		note, held, err := e.unpinFor(ctx, repo, pr, slot, has, fixBy, "fix") // review_unpin.go
		switch {
		case err != nil:
			return nil, fmt.Errorf("%s: %w", label, err)
		case held != "":
			return nil, fmt.Errorf("%s: %s, but %s", label, note, held)
		}
		pr.Pinned, slot.Pinned = false, false
		notes = append(notes, note)
	}
	job := &fixJob{repo: repo, watch: *w, pool: e.cfg.PoolFor(full), pr: pr, slot: slot, role: role, review: sum, head: pr.HeadSHA,
		round: reviewRun.Round, login: src.Login(), notes: notes, state: prev, others: others, alone: alone, conflict: conflict}
	if conflict.sha != "" { // asked only when nothing else waits: the merge is the job's whole work
		job.forMerge = true
	}
	if isLow { // the other reviewers' items are read when the held fix starts
		job.budget, job.othersDeferred = &low, true
	}
	if job.state.Cap == 0 {
		job.state.Cap = e.cfg.FixCyclesFor(w)
	}
	if github.SameAccount(src.Login(), fixer.Login) {
		job.notes = append(job.notes, "this watch posts as you: the re-review will not read the fixer's replies as yours")
	}
	return job, nil
}

// fixSlotRefusal is prepareFix's refusal of pr's slot (has false: it has
// none), nil when the fixer may work in it, or, for a PR the pool took its
// slot from, in one the job claims at the reviewed head (fixClaim; prepareFix
// refuses a slotless PR whose review does not stand on its head before):
// a slot broken, lost or in another state, a slot, or a PR without one,
// pinned where pin refuses, a slot held by a guard, or one whose checkout is
// not the PR's head that kept the verdict of the review on on (stood).
func fixSlotRefusal(label string, pr store.PR, slot store.Slot, has bool, pin fixPin, stood bool, on string) error {
	switch {
	case !has && pr.Pinned && pin == fixPinRefuses:
		return fmt.Errorf("%s is pinned (magnum unpin)", label)
	case !has:
		return nil
	case slot.State == store.SlotBroken || slot.State == store.SlotLost:
		return fmt.Errorf("%s: %s", label, unusableSlotSentence(slot))
	case slot.State != store.SlotClaimed && slot.State != store.SlotHeld:
		return fmt.Errorf("%s: slot %s is %s", label, slot.Name, slot.State)
	case slot.Pinned && pin == fixPinRefuses:
		return fmt.Errorf("%s: slot %s is pinned (magnum unpin)", label, slot.Name)
	case slot.HoldReason != nil:
		return fmt.Errorf("%s: %s", label, slotHeldReason(slot))
	case stood && deref(slot.CheckedOutSHA) != pr.HeadSHA: // a push that only merged the base moves no checkout
		return fmt.Errorf("%s: slot %s holds %s, not its head %s, which kept the verdict of the review on %s; `magnum review %s` reviews it first",
			label, slot.Name, textx.ShortSHA(deref(slot.CheckedOutSHA)), textx.ShortSHA(pr.HeadSHA), on, label)
	}
	return nil
}

// fixSlotFree refuses a fix on pr while it has no slot (has false) and its
// pool has none free, as `magnum open` is refused: a slot is provisioned or
// evicted for it meanwhile (needSlot), and the answer names the retry. A PR
// with a slot, a repository without a pool (a per-PR worktree is made for
// it), a job held for the GitHub budget (held: the held fix asks again when
// it starts) and a job only asked about (fixPinWouldUnpin: a dry run, a
// hold) pass.
func (e *Engine) fixSlotFree(ctx context.Context, repo store.Repo, pr store.PR, label string, has, held bool, pin fixPin) error {
	pool := e.cfg.PoolFor(repo.FullName())
	if has || held || pin == fixPinWouldUnpin || pool == nil {
		return nil
	}
	free, err := e.st.FreeSlots(ctx, pool.Repo, pr.ID)
	switch {
	case err != nil:
		return err
	case len(free) > 0:
		return nil
	}
	e.needSlot(ctx, *pool, prSubject(repo, pr.Number))
	return noFreeFixSlot{repo: pool.Repo, label: label}
}

// noFreeFixSlot is the refusal of a fix whose PR has no slot while the pool
// has none free: the retry it names finds the one freed meanwhile. It is
// slots.ErrNoFreeSlot (errors.Is), so a claim that lost its free slot ends
// the loop quietly with it (runFix, endFixUnclaimed).
type noFreeFixSlot struct{ repo, label string }

func (e noFreeFixSlot) Error() string {
	return fmt.Sprintf("no free %s slot right now; run `magnum fix %s` again shortly", e.repo, e.label)
}

func (noFreeFixSlot) Unwrap() error { return slots.ErrNoFreeSlot }

// reviewStood reports whether the PR's head kept the verdict of sum, its
// last posted review, made on an older commit: the head is the reviewed one
// (a push that only merged the base settled it, settlePush) and every round
// of the judge since the review ended replied, posting no review because
// its verdict and event stayed the review's. A round counts by its last
// judge run, so a continued or nudged turn counts by how it ended; the
// judge's own pass, which neither ends a round nor posts, and the other
// roles' runs do not count.
func (e *Engine) reviewStood(ctx context.Context, pr store.PR, sum store.ReviewSummary) (bool, error) {
	if sum.SHA == pr.HeadSHA || deref(pr.ReviewedSHA) != pr.HeadSHA {
		return false, nil
	}
	runs, err := e.st.RunsAfter(ctx, sum.RunID)
	if err != nil {
		return false, err
	}
	last := map[int]store.Run{}
	for _, r := range runs {
		if e.isJudge(r.Role) && r.Kind != store.RunOwnPass {
			last[r.Round] = r
		}
	}
	for _, r := range last {
		if deref(r.Outcome) != pipeline.OutcomeReplied {
			return false, nil
		}
	}
	return true, nil
}

// startFix starts a fix cycle on pr under prepareFix's refusals, a
// drain's (errFixDraining) and the GitHub floor's (githubBudgetError:
// below it, or after a read of the other reviewers' items GitHub refused
// for the rate limit, prepareFix takes no pin off, and the hold keeps the
// job it prepared): a new loop
// (prev zero, started by by: "magnum fix", "autofix") at cycle 1 of the
// watch's fix_cycles, or the next cycle of the loop prev waits in. A new
// loop `magnum fix` asks for unpins a pinned PR (fixPinUnpins); autofix and
// a next cycle refuse the pin (fixPinRefuses). The job reserves the PR
// (roundHandle.fix), writes the loop's row in phase fixing and runs on a
// goroutine of its own under the reservation (runFix), whose context
// derives from ctx: pass the daemon's, never a round's (its reservation's
// end cancels it). carried is prepareFix's (nil but for a `magnum fix`
// answered after a whiteboard job yielded to it). It returns a copy of the
// job as it started, for the caller's answer (the job's own goes on
// changing).
func (e *Engine) startFix(ctx context.Context, pr store.PR, by string, prev FixState, carried *externalRead) (*fixJob, error) {
	pin := fixPinRefuses
	if by == fixBy && prev.Cycle == 0 {
		pin = fixPinUnpins
	}
	if e.draining(ctx) {
		return nil, errFixDraining
	}
	job, err := e.prepareFix(ctx, pr, prev, pin, carried)
	if err != nil {
		return nil, err
	}
	if job.budget != nil { // github_floor.go: prepared without reads; held until the budget recovers
		return nil, &githubBudgetError{low: *job.budget, job: job}
	}
	if e.d.Agents == nil || e.d.Slots == nil {
		return nil, errors.New("the daemon has no agents or slots manager")
	}
	st := job.state
	if prev.Cycle == 0 {
		st = FixState{Cycle: 1, Cap: job.state.Cap, Since: job.head, By: by, StartedAt: e.now().UTC()}
	} else {
		st.Cycle++
	}
	st.Phase, st.Head, st.Report, st.Role, st.NeedsYou, st.ForcedAt, st.Held = FixPhaseFixing, job.head, "", job.role.Name, nil, time.Time{}, false
	st.Answered, st.Context, st.OthersUnread = nil, "", "" // the last cycle's calls go with its NeedsYou
	job.state = st
	rctx, cancel := context.WithCancel(ctx)
	if !e.reserve(pr.ID, &roundHandle{cancel: cancel, fix: true}) {
		cancel()
		return nil, fmt.Errorf("%s: %s; run `magnum fix %s` again shortly", job.label(), e.heldReason(pr.ID), job.label())
	}
	if err := e.claimFixRow(ctx, job, prev, st); err != nil {
		e.unreserve(pr.ID)
		return nil, err
	}
	started := *job
	e.roundWG.Go(func() {
		defer e.unreserve(pr.ID)
		defer e.workEnded(rctx, pr.ID) // the idle park's clock (park.go), before the reservation drops
		e.safely(rctx, "fix", job.subject(), func() { e.runFix(rctx, job) },
			func(ctx context.Context, msg string) { e.fixPanicked(ctx, job, msg) })
	})
	return &started, nil
}

// claimFixRow writes the cycle's row (st, phase fixing) once the PR is
// reserved, compare-and-set against what a writer without the reservation
// may have done since prepareFix looked: `magnum abort` on a reviewed PR
// ends a loop waiting for its re-review without one (endFixLoop). A new loop
// needs the PR still without a row; a next cycle needs the row still to be
// prev's loop waiting at prev's cycle, and replaces that very row
// (Store.SetKVIf), or it is refused with errFixLoopGone. From the
// reservation on, a stop goes to the job instead.
func (e *Engine) claimFixRow(ctx context.Context, job *fixJob, prev, st FixState) error {
	cur, raw, ok, err := e.fixRow(ctx, job.pr.ID)
	switch {
	case err != nil:
		return err
	case prev.Cycle == 0 && ok:
		return errors.New(fixRunningSentence(cur))
	case prev.Cycle == 0:
		return e.setFixState(ctx, job.pr.ID, st)
	case !ok || !sameWaitingLoop(cur, prev):
		return fmt.Errorf("%s: %w", job.label(), errFixLoopGone)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	swapped, err := e.st.SetKVIf(ctx, KVPRFix(job.pr.ID), raw, string(b))
	switch {
	case err != nil:
		return err
	case !swapped:
		return fmt.Errorf("%s: %w", job.label(), errFixLoopGone)
	}
	return nil
}

// sameWaitingLoop reports whether cur is still the loop prev was read as,
// waiting for its re-review at the same cycle.
func sameWaitingLoop(cur, prev FixState) bool {
	return cur.Phase == FixPhaseReviewing && cur.Cycle == prev.Cycle && cur.StartedAt.Equal(prev.StartedAt)
}

// runFix runs one fix cycle under its reservation: claim (a job whose PR
// has no slot), guard, start and wait, then the settle (fix_settle.go).
// `magnum abort` cancels ctx and leaves its orders on the reservation: the
// fixer is stopped and the loop ends quietly (stopFix). A daemon shutdown
// cancels ctx with no order: the loop's row stays in phase fixing, which
// the next start ends (recoverFixes).
func (e *Engine) runFix(ctx context.Context, job *fixJob) {
	ctx = github.WithOp(ctx, opFix) // github_cost.go
	subject := job.subject()
	if err := steps.ResetSubject(ctx, e.st, subject); err != nil {
		e.log.Warn("fix: new step generation", "subject", subject, "err", err)
	}
	var err error
	failed := ""
	if job.slot.ID == 0 { // the pool took the PR's slot back
		failed, err = "fix claim failed: ", steps.Step(ctx, e.st, subject, "fix.claim", func(ctx context.Context) error { return e.fixClaim(ctx, job) })
	}
	if err == nil {
		if err = steps.Step(ctx, e.st, subject, "fix.guard", func(ctx context.Context) error { return e.fixGuard(ctx, job) }); err != nil {
			failed = "fix guard failed: "
		} else if err = steps.Step(ctx, e.st, subject, "fix.start", func(ctx context.Context) error { return e.fixStart(ctx, job) }); err != nil {
			failed = "fix run failed: "
		} else if err = steps.Step(ctx, e.st, subject, "fix.wait", func(ctx context.Context) error { return e.fixWait(ctx, job) }); err != nil {
			failed = "fix run failed: "
		}
	}
	fctx := context.WithoutCancel(ctx)
	if orders := e.closeFixHandle(job.pr.ID); len(orders) > 0 {
		e.stopFix(fctx, job, orders)
		return
	}
	if ctx.Err() != nil {
		e.log.Info("fix interrupted by a shutdown; the next start ends its loop", "subject", subject)
		return
	}
	switch {
	case errors.Is(err, slots.ErrNoFreeSlot): // the claim lost its free slot (fixClaim): nothing failed
		e.endFixUnclaimed(fctx, job, err.Error())
	case err != nil:
		msg := failed + err.Error()
		if job.touched {
			msg += e.putFixSlotBack(fctx, job)
		}
		e.failFix(fctx, job, msg, "")
	default:
		e.settleFix(fctx, job)
	}
}

// endFixUnclaimed ends the loop of a job whose claim found no free slot
// after all (a round took the one the request saw free) quietly, as a stop
// does: nothing failed and why names the retry, so the loop's row goes, a
// fix.ended event and an info toast say why, and nobody is called.
func (e *Engine) endFixUnclaimed(ctx context.Context, job *fixJob, why string) {
	e.endFixState(ctx, job)
	e.event(ctx, "info", job.subject(), "fix.ended", "the fix loop ends: "+why, job.eventData())
	title := fmt.Sprintf("magnum: fix not started: %s#%d", job.repo.Name, job.pr.Number)
	e.info(notify.Item{Key: fmt.Sprintf("fix-unclaimed:%d:%s:%d", job.pr.ID, store.FormatTime(job.state.StartedAt), job.state.Cycle), Title: title,
		Body: job.pr.URL + "\n" + why, Line: strings.TrimPrefix(title, "magnum: ") + ": " + why + " " + job.pr.URL, Kind: kindFixEnded})
}

// putFixSlotBack stops a fixer whose run failed, timed out or panicked and
// puts the slot back at the reviewed head (spec 3.6: the fixer's local
// commits would hold the slot for head drift at the next round's guard). It
// returns what to add to the failure's message ("" when the slot is back).
func (e *Engine) putFixSlotBack(ctx context.Context, job *fixJob) string {
	return e.stopFixerAt(ctx, job, job.head)
}

// stopFixerAt stops the job's fixer and puts its slot back at sha: the fix
// role's working agents are interrupted and waited for until quiet (a fixer
// left at work would go on pushing and replying as the operator after magnum
// ended its loop), then the slot is resynced. It returns what to add to the
// failure's message ("" when the slot is back, or the PR has no slot).
func (e *Engine) stopFixerAt(ctx context.Context, job *fixJob, sha string) string {
	if job.role.Name != "" && e.interruptSessions(ctx, job.pr, &job.watch, job.role.Name) > 0 && !e.waitAgentsQuiet(ctx, job.pr.ID) {
		e.log.Info("the fixer still works after the interrupt", "subject", job.subject())
	}
	if job.slot.ID == 0 || sha == "" {
		return ""
	}
	if err := e.resyncFixSlot(ctx, job, sha); err != nil {
		return fmt.Sprintf("; slot %s not put back: %v", job.slot.Name, err)
	}
	return ""
}

// closeFixHandle ends the job's part that an abort can stop (the
// reservation's stop.ended, as afterRound marks a round's) and returns the
// abort and ignore orders that arrived before: from now on an abort is told
// to try again once the settle let go of the PR.
func (e *Engine) closeFixHandle(prID int64) []stopOrder {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.rounds[prID]
	if h == nil {
		return nil
	}
	h.stop.ended = true
	return slices.Clone(h.stop.reqs)
}

// fixClaim gives a job whose PR has no slot (the pool took it back after
// the review) one at the reviewed head, as `magnum open` claims one, without
// its pin or open hold (claimAtReviewedHead); fixGuard then takes it as the
// job's, and the fix.start event says what the claim did to the slot's
// databases (fixJob.schema): a schema that could not be loaded leaves the
// fixer's specs on the last PR's. The slot moves to held, as a round leaves
// one: the job's reservation keeps eviction off it while the fixer works,
// and the pool can take it back once the loop let go of the PR. A claim that
// failed, or that `magnum abort` (an ignore) cut short, hands back the pool
// slot it reserved, released as after a failed open: kept, it would carry the
// last PR's checkout record, which a later open takes as the PR's own. One a
// shutdown cut short leaves it held, never claimed for good. A pool slot
// another round took since prepareFix found it free fails as prepareFix
// refuses (noFreeFixSlot), and runFix ends the loop quietly with it.
func (e *Engine) fixClaim(ctx context.Context, job *fixJob) error {
	slot, note, err := e.claimAtReviewedHead(ctx, job.pr, job.repo)
	fctx := context.WithoutCancel(ctx)
	switch {
	case errors.Is(err, slots.ErrNoFreeSlot) && job.pool != nil:
		return noFreeFixSlot{repo: job.pool.Repo, label: job.label()}
	case slot.ID == 0:
		return err
	case err != nil && job.pool != nil && (ctx.Err() == nil || e.stopOrdered(job.pr.ID)):
		reason, what := "fix failed", "a failed claim of magnum fix"
		if ctx.Err() != nil {
			reason, what = "fix stopped", "a claim of magnum fix that a stop cut short"
		}
		if rerr := e.releaseSlot(fctx, slot, *job.pool, reason); rerr != nil {
			e.event(fctx, "warn", "slot:"+slot.Name, "slot.release_failed", "release after "+what+": "+rerr.Error(), nil)
		} else {
			e.event(fctx, "info", "slot:"+slot.Name, "slot.released", slot.Name+" released after "+what, nil)
		}
		return err
	}
	if terr := e.st.TransitionSlot(fctx, slot.ID, []string{store.SlotClaimed}, store.SlotHeld, nil); terr != nil && !errors.Is(terr, store.ErrConflict) {
		e.log.Warn("fix: hold the claimed slot", "subject", job.subject(), "slot", slot.Name, "err", terr)
	}
	job.schema = strings.TrimPrefix(note, "; ")
	return err
}

// fixGuard checks that the fixer may work in the job's slot: the slot is
// still the PR's (for a job that claimed one, fixClaim, the slot the PR now
// has becomes the job's), no round is in flight, the head is still the
// reviewed one and the slot holds it, nobody typed into the PR's panes
// within human_cooldown, and Slots.Guard finds no person in the slot (their
// agent or process, their changes) and no HEAD drift. From then on the slot
// is the fixer's: its last_used_at moves, so a person who types into the
// PR's panes during the fix is told apart from the fixer, and checkFixBase
// looks there whether the head conflicts with the PR's base.
func (e *Engine) fixGuard(ctx context.Context, job *fixJob) error {
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		return err
	}
	job.pr = pr
	slot, has, err := e.slotOf(ctx, pr.ID)
	switch {
	case err != nil:
		return err
	case !has && job.slot.ID == 0:
		return errors.New("the PR has no slot")
	case !has || (job.slot.ID != 0 && slot.ID != job.slot.ID):
		return fmt.Errorf("slot %s is no longer the PR's", job.slot.Name)
	case slot.State != store.SlotClaimed && slot.State != store.SlotHeld:
		return fmt.Errorf("slot %s is %s", slot.Name, slot.State)
	}
	job.slot = slot
	head := job.head
	switch {
	case slices.Contains(store.InFlightStates, pr.State) || pr.State == store.PRPaused:
		return fmt.Errorf("a round is in progress (%s)", pr.State)
	case pr.HeadSHA != head:
		return fmt.Errorf("the head moved to %s since the review on %s", textx.ShortSHA(pr.HeadSHA), textx.ShortSHA(job.review.SHA))
	case deref(pr.ReviewedSHA) != head:
		return fmt.Errorf("the reviewed head is %s, not %s", textx.ShortSHA(deref(pr.ReviewedSHA)), textx.ShortSHA(head))
	case deref(slot.CheckedOutSHA) != head:
		return fmt.Errorf("slot %s holds %s, not the reviewed head %s", slot.Name, textx.ShortSHA(deref(slot.CheckedOutSHA)), textx.ShortSHA(head))
	}
	if until := e.cfg.Daemon.CooldownUntil(pr.HumanActiveAt); e.now().Before(until) {
		return fmt.Errorf("someone typed into the PR's panes, which are theirs until %s (human_cooldown)", until.Local().Format("15:04"))
	}
	if err := e.d.Slots.Guard(ctx, slot); err != nil {
		return err
	}
	if err := e.st.UpdateSlotFields(ctx, slot.ID, func(u *store.SlotUpdate) { u.Set("last_used_at", e.now()) }); err != nil {
		return err
	}
	return e.checkFixBase(ctx, job)
}

// checkFixBase looks, in the job's slot, whether the head the fixer starts
// from conflicts with the PR's base, the base fetched first (baseConflict,
// spec 5.6): a conflict becomes the job's, which the prompt names and the
// fixer merges before the findings, and a clean merge clears what
// prepareFix found. A look that fails never blocks the fix: a
// fix.conflict_unchecked warning says why, and the fixer starts without the
// merge step, or, for a job that starts for the merge alone (forMerge),
// with the tip prepareFix fetched moments before. Such a job whose conflict
// is gone has nothing left to do, and the guard fails it. A shutdown or a
// stop during the fetch ends the guard with its cause.
func (e *Engine) checkFixBase(ctx context.Context, job *fixJob) error {
	if e.d.Git == nil {
		return nil
	}
	c, err := e.baseConflict(ctx, job.repo, job.pr, job.slot.Path, job.slot.MainClone, job.head, true)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		then := job.role.Name + " starts without the merge step"
		if job.conflict.sha != "" {
			then = fmt.Sprintf("%s merges the tip %s that the request fetched", job.role.Name, textx.ShortSHA(job.conflict.sha))
		}
		base := cmp.Or(deref(job.pr.BaseRef), job.repo.DefaultBranch)
		e.event(ctx, "warn", job.subject(), "fix.conflict_unchecked", fmt.Sprintf("could not check %s for a conflict with its base %s: %s; %s",
			job.label(), base, oneLine(err.Error(), 300), then), map[string]any{"role": job.role.Name, "base": base, "cycle": job.state.Cycle})
		return nil
	case c.sha == "" && job.forMerge:
		return fmt.Errorf("nothing to fix: the head %s merges %s cleanly now, and the review on %s found nothing",
			textx.ShortSHA(job.head), job.conflict.base, textx.ShortSHA(job.review.SHA))
	}
	job.conflict = c
	return nil
}

// baseConflict says whether merging the tip of pr's base branch into head
// conflicts (fixConflict with the paths; zero for a clean merge): with fetch,
// the base is fetched into mainClone first, as a slot's checkout fetches
// it, then its tip (origin/<base>) is merged with head by `git merge-tree`
// in dir, which writes no ref, index or work tree (Git.MergeConflicts).
// GitHub's mergeable is not read: it answers UNKNOWN until its own
// computation ends. The error says why it could not tell: no git or
// checkout, a base not known yet or one whose name a shell would read
// (the prompt names it), a failed fetch or a revision git does not know.
func (e *Engine) baseConflict(ctx context.Context, repo store.Repo, pr store.PR, dir, mainClone, head string, fetch bool) (fixConflict, error) {
	base := cmp.Or(deref(pr.BaseRef), repo.DefaultBranch)
	switch {
	case e.d.Git == nil:
		return fixConflict{}, errors.New("no git to look with")
	case dir == "":
		return fixConflict{}, errors.New("no checkout of the repository to look in")
	case base == "":
		return fixConflict{}, errors.New("its base branch is not known yet")
	case !gitx.ShellSafeRef(base):
		return fixConflict{}, errors.New("the base branch's name has characters magnum does not pass to a shell")
	}
	if fetch {
		if err := e.d.Git.FetchBranch(ctx, cmp.Or(mainClone, dir), base); err != nil {
			return fixConflict{}, err
		}
	}
	tip, err := e.d.Git.RevParse(ctx, dir, "refs/remotes/origin/"+base)
	if err != nil {
		return fixConflict{}, err
	}
	paths, err := e.d.Git.MergeConflicts(ctx, dir, head, tip)
	if err != nil || len(paths) == 0 {
		return fixConflict{}, err
	}
	return fixConflict{base: base, sha: tip, paths: paths}, nil
}

// cloneOf is a checkout of repo that shares the objects of its slots, for a
// look at a PR without a slot: its pool's main clone, else the clone magnum
// found for it (repos.clone_path); "" for none.
func (e *Engine) cloneOf(repo store.Repo) string {
	if pool := e.cfg.PoolFor(repo.FullName()); pool != nil && pool.MainClone != "" {
		return pool.MainClone
	}
	return deref(repo.ClonePath)
}

// fixStart starts or resumes the fixer in the PR's workspace (its pane gets
// the fix role's identity's env, agents.Manager.paneEnv), compacts a resumed
// fixer whose context is nearly full, or starts it fresh (fixCompact, told
// whether the fixer was parked before, so that it waits for the figure),
// creates its run of kind fix in the review's round, writes the other
// reviewers' open items beside the run's report (writeExternal, which the
// prompt names when it could) and sends it the fix prompt, which says so
// when the review found nothing (NoFindings: a job of the others alone,
// whose run is abandoned and fails when that file could not be written).
// A run that Submit refused before sending is abandoned; one whose prompt
// may have arrived is waited for.
func (e *Engine) fixStart(ctx context.Context, job *fixJob) error {
	pr := job.pr
	src := e.d.Identities[pr.Identity]
	if src == nil {
		return fmt.Errorf("identity %q is not configured", pr.Identity)
	}
	if err := e.d.Agents.Preflight(ctx, job.role.AgentKind()); err != nil && isLogin(err) {
		return err
	}
	env, err := e.paneEnv(ctx, job.asRound(), src, job.head)
	if err != nil {
		return err
	}
	// Only the fixer's pane: a pane laid out for a round role nobody starts
	// leaves a "starting" session row that `magnum open` would try to focus.
	// The round's roles stay parked, or get their panes from the next
	// round's prepare, which reuses this workspace.
	layout := []config.Role{job.role}
	ws, err := e.d.Agents.EnsureWorkspace(ctx, pr, job.slot.Path, env, fmt.Sprintf("%s#%d", job.repo.Name, pr.Number), layout)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if ws.Panes[agents.Role(job.role.Name)] == "" {
		if ws, err = e.d.Agents.EnsurePane(ctx, pr, ws, job.slot.Path, env, job.role); err != nil {
			return fmt.Errorf("%s pane: %w", job.role.Name, err)
		}
	}
	// Read before ensureAgent resumes a parked fixer, whose pane draws its
	// status line only some time after the resume (fixCompact waits for it).
	live, err := e.st.LiveSessionByPRRole(ctx, pr.ID, job.role.Name)
	wasLive := err == nil && sessionUp(live, job.role)
	fresh, err := e.ensureAgent(ctx, pr, job.role, ws, false, effortFull)
	if err != nil {
		return fmt.Errorf("start %s: %w", job.role.Name, err)
	}
	if !fresh { // a resumed conversation: compacted, or started fresh, when its context is nearly full
		if err := e.fixCompact(ctx, job, ws, !wasLive); err != nil {
			return err
		}
	}
	job.started = e.now()
	run, err := e.d.Agents.NewRun(ctx, pr, agents.Role(job.role.Name), store.RunFix, job.round)
	if err != nil {
		return err
	}
	job.run = run
	job.state.Report = deref(run.ReportPath)
	if err := e.setFixState(ctx, pr.ID, job.state); err != nil {
		e.abandonFixRun(ctx, run, err.Error())
		return err
	}
	external, unread := "", ""
	if job.state.Report != "" {
		external, unread = e.writeExternal(ctx, job, filepath.Dir(job.state.Report)) // fix_external.go
	}
	if unread != "" { // the loop's end says so (othersUnread)
		job.state.OthersUnread = unread
		if err := e.setFixState(ctx, pr.ID, job.state); err != nil {
			e.log.Warn("fix: record the other reviewers' items as unread", "subject", job.subject(), "err", err)
		}
	}
	if external == "" && job.alone && job.conflict.sha == "" { // the other reviewers' items are the job's whole work
		err := errors.New("the review found nothing, and the file of the other reviewers' threads and comments, all the fixer would answer, could not be written beside the report")
		e.abandonFixRun(ctx, run, err.Error())
		return err
	}
	// The fixer reads the repository notes, never edits them: a copy
	// beside the report, in parts (agents.FreezeNotes), as a round's roles.
	var notesParts agents.NotesParts
	if job.state.Report != "" {
		var err error
		notesParts, err = agents.FreezeNotes(NotesPath(e.d.Layout, job.repo.Owner, job.repo.Name), filepath.Dir(job.state.Report))
		if err != nil {
			e.log.Warn("fix: freeze the repository notes", "subject", job.subject(), "err", err)
		}
	}
	text, err := e.d.Agents.FixPrompt(pr, agents.Role(job.role.Name), run, agents.FixPromptInput{
		HeadRef:         deref(pr.HeadRef),
		ReviewURL:       job.review.URL,
		ReviewFile:      filepath.Join(e.d.Layout.ReviewDir(job.repo.Owner, job.repo.Name, pr.Number, job.review.SHA), agents.PostReviewFile),
		ExternalFile:    external,
		ReviewerLogin:   job.login,
		DBLockCommand:   agents.DBLockLine(e.d.Layout.Binary(), job.slot.Path, job.role.Name),
		FixReplyCommand: agents.FixReplyLine(e.d.Layout.Binary()),
		NoFindings:      job.alone,
		NotesParts:      notesParts,
		ConflictBase:    job.conflict.base,
		ConflictSHA:     job.conflict.sha,
		ConflictPaths:   job.conflict.paths,
	})
	if err != nil {
		e.abandonFixRun(ctx, run, err.Error())
		return err
	}
	job.touched = true
	if serr := e.d.Agents.Submit(ctx, run, text); serr != nil {
		cur, err := e.st.RunByID(context.WithoutCancel(ctx), run.ID)
		switch {
		case err != nil:
			return errors.Join(serr, err)
		case cur.State == store.RunPending: // refused before sending
			job.touched = false
			e.abandonFixRun(ctx, run, serr.Error())
			return serr
		case cur.State == store.RunFailed || cur.State == store.RunAbandoned || ctx.Err() != nil:
			return serr
		}
		// Submitted (stalled, timed out, acked as blocked): the text may be
		// in the agent, so the run is observed, never sent again.
		e.log.Warn("fix: prompt not acknowledged as working; waiting for the run", "subject", job.subject(), "run", run.ID, "err", serr)
	}
	msg := fmt.Sprintf("%s %s (run %s, cycle %d of %d)", job.role.Name, job.fixes(), run.ID, job.state.Cycle, job.state.Cap)
	data := map[string]any{"run": run.ID, "role": job.role.Name, "slot": job.slot.Name, "report": job.state.Report,
		"cycle": job.state.Cycle, "cap": job.state.Cap, "by": job.state.By}
	if job.schema != "" { // fixClaim: what the claim did to the slot's databases
		msg += "; " + job.schema
		data["schema"] = job.schema
	}
	if c := job.conflict; c.sha != "" { // checkFixBase: the fixer merges the base first, as fixes says
		data["conflict_base"], data["conflict_sha"], data["conflict_files"] = c.base, c.sha, len(c.paths)
	}
	e.event(ctx, "info", job.subject(), "fix.start", msg, data)
	return nil
}

// fixWait waits for the fixer's run to end, as the pipeline waits for a
// reviewer's: a run ended (the agent idle on two ticks) is the fixer's
// report to read; a run failed or abandoned, or a session lost, fails the
// job. A run past the role's timeout gets the time-up line within the run
// and TimeUpGrace more; one that still runs is interrupted and fails.
func (e *Engine) fixWait(ctx context.Context, job *fixJob) error {
	timeout := job.role.Timeout.Duration
	err := e.waitFixRun(ctx, job.run.ID, timeout)
	if !errors.Is(err, errFixTimeout) {
		return err
	}
	budget := humanDuration(timeout)
	report := deref(job.run.ReportPath)
	if terr := e.d.Agents.Tell(ctx, job.run, fmt.Sprintf(fixTimeUpText, budget, report, agents.ReportMarker(job.run.ID))); terr != nil {
		return fmt.Errorf("%s ran out of its %s and could not be asked for its report: %w", job.role.Name, budget, terr)
	}
	e.event(ctx, "warn", job.subject(), "fix.time_up", fmt.Sprintf("%s ran out of its %s: asked it to write its report now, waiting %s more (run %s)",
		job.role.Name, budget, humanDuration(pipeline.TimeUpGrace), job.run.ID), map[string]any{"run": job.run.ID, "report": report})
	if err := e.waitFixRun(ctx, job.run.ID, pipeline.TimeUpGrace); !errors.Is(err, errFixTimeout) {
		return err
	}
	return fmt.Errorf("%s ran out of its %s and wrote no report in %s more", job.role.Name, budget, humanDuration(pipeline.TimeUpGrace))
}

// waitFixRun polls run runID every fixPoll until it ends (nil), fails or
// loses its session (an error), timeout passes (errFixTimeout; 0 = none) or
// ctx ends.
func (e *Engine) waitFixRun(ctx context.Context, runID string, timeout time.Duration) error {
	return e.waitAgentRun(ctx, runID, timeout, nil)
}

// waitAgentRun is waitFixRun with each, when set, run on every poll of a
// run still at work: its error ends the wait (the whiteboard job's look at
// the PR, whiteboard.go).
func (e *Engine) waitAgentRun(ctx context.Context, runID string, timeout time.Duration, each func(ctx context.Context) error) error {
	deadline := e.now().Add(timeout)
	for {
		run, err := e.st.RunByID(ctx, runID)
		if err != nil {
			return err
		}
		switch run.State {
		case store.RunEnded, store.RunVerified:
			return nil
		case store.RunFailed, store.RunAbandoned:
			return fmt.Errorf("%s run %s is %s: %s", run.Role, run.ID, run.State, deref(run.Error))
		}
		if run.SessionID != nil {
			if s, err := e.st.SessionByID(ctx, *run.SessionID); err == nil && s.State != store.SessionLive && s.State != store.SessionStarting {
				return fmt.Errorf("%s session %d is %s", run.Role, s.ID, s.State)
			}
		}
		if each != nil {
			if err := each(ctx); err != nil {
				return err
			}
		}
		if timeout > 0 && !e.now().Before(deadline) {
			return errFixTimeout
		}
		if err := e.d.Sleep(ctx, fixPoll); err != nil {
			return err
		}
	}
}

// abandonFixRun marks the fixer's run abandoned with why (a run already
// final keeps its state) and reports whether it did.
func (e *Engine) abandonFixRun(ctx context.Context, run store.Run, why string) bool {
	active := []string{store.RunPending, store.RunSubmitted, store.RunWorking, store.RunEnded}
	err := e.st.TransitionRun(context.WithoutCancel(ctx), run.ID, active, store.RunAbandoned, func(u *store.RunUpdate) {
		u.Set("error", oneLine(why, 300))
		u.Set("ended_at", e.now())
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		e.log.Warn("fix: abandon run", "run", run.ID, "err", err)
	}
	return err == nil
}

// fixHolds reports whether the PR's fix loop holds its next round: the
// fixer works on it (phase fixing), or its row cannot be read (no round
// starts over a loop that may run). The loop's own re-review (phase
// reviewing) passes.
func (e *Engine) fixHolds(ctx context.Context, pr store.PR) bool {
	st, ok, err := e.fixState(ctx, pr.ID)
	if err != nil {
		if e.logOnce(fmt.Sprintf("fix-state:%d", pr.ID), err.Error(), e.now()) {
			e.log.Warn("dispatch holds a PR whose fix loop cannot be read", "pr", pr.ID, "err", err)
		}
		return true
	}
	return ok && st.Phase == FixPhaseFixing
}

// fixWaitOf is the wait of a PR whose fix loop holds it (fixHolds; ok
// false when none does).
func (e *Engine) fixWaitOf(ctx context.Context, prID int64) (Wait, bool) {
	st, ok, err := e.fixState(ctx, prID)
	switch {
	case err != nil:
		return Wait{Reason: WaitFixing, Detail: "its fix loop, whose row cannot be read (`magnum abort` clears it)"}, true
	case !ok || st.Phase != FixPhaseFixing:
		return Wait{}, false
	}
	return Wait{Reason: WaitFixing, Count: st.Cycle, Max: st.Cap, Detail: fmt.Sprintf("the fixer (fix cycle %d of %d)", st.Cycle, st.Cap)}, true
}

// fixPanicked settles a fix job that panicked as a failed one; the abort
// and ignore orders it held hear why (stopsPanicked). A fixer that may have
// touched the slot is stopped and the slot put back first, as after a failed
// run (putFixSlotBack).
func (e *Engine) fixPanicked(ctx context.Context, job *fixJob, msg string) {
	if orders := e.closeFixHandle(job.pr.ID); len(orders) > 0 {
		e.stopsPanicked(ctx, orders, msg)
	}
	if job.run.ID != "" {
		e.abandonFixRun(ctx, job.run, "the fix job panicked")
	}
	why := "fix run failed: panic: " + msg
	if job.touched {
		why += e.putFixSlotBack(ctx, job)
	}
	e.failFix(ctx, job, why, "")
}

// recoverFixes ends the fix loops a stopped daemon left with the fixer at
// work (phase fixing): no fixer run survives a restart, so its run fails
// and the loop ends as needs attention. herdr keeps the fixer's pane across
// a crash or `daemon-restart --now`, so a fixer still at work is
// interrupted until quiet and the slot is put back at the loop's head first
// (stopFixerAt), before magnum tells the operator the run was lost. A loop
// in phase reviewing goes on (recoverReviewingLoop): one whose re-review is
// still to run waits for it (the round's own recovery re-reviews), one whose
// re-review posted gets the decision the stop kept the hook from making.
func (e *Engine) recoverFixes(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	e.mu.Lock()
	clear(e.drainHeld) // this start decides every loop a drain held
	e.mu.Unlock()
	keys, err := e.st.KVKeys(ctx, "pr.*.fix")
	if err != nil {
		e.log.Warn("recovery: fix loops", "err", err)
		return
	}
	const why = "fix run lost in a daemon restart"
	for _, key := range keys {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.fix", &id); err != nil || KVPRFix(id) != key {
			continue
		}
		st, raw, ok, err := e.fixRow(ctx, id)
		if err != nil {
			e.log.Warn("recovery: a fix loop that cannot be read stays for magnum abort", "key", key, "err", err)
			continue
		}
		if !ok || (st.Phase != FixPhaseFixing && st.Phase != FixPhaseReviewing) {
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		if err != nil {
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		job := e.loopJob(ctx, repo, pr, st)
		if st.Phase == FixPhaseReviewing {
			e.recoverReviewingLoop(ctx, job, raw, "while the daemon was stopped") // fix_loop.go
			continue
		}
		if job.role.Name == "" {
			job.role.Name = st.Role // the role the loop ran, though the config no longer names it
		}
		msg := why + e.stopFixerAt(ctx, job, cmp.Or(st.Head, deref(pr.ReviewedSHA)))
		if runs, err := e.st.RunsByPR(ctx, pr.ID); err == nil {
			for _, r := range runs {
				if r.Kind == store.RunFix {
					e.finishFixRun(ctx, r, store.RunFailed, "lost", why)
				}
			}
		}
		e.failFix(ctx, job, msg, st.Report)
	}
}
