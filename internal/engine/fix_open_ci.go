package engine

// A required check that did not run at an own PR's head (DECISIONS "A
// required check that did not run at the head is never green", "The look
// starts the CI workflow a head's required check did not run in"): a
// repository whose CI workflow runs only when a PR opens gives every later
// push, the fixer's included, no run of the check GitHub requires, and the
// operator started it by hand. Rule 11 of the look waits for such a check
// while a check of the head runs (a job that needs others is created only
// when they end) and until settle_max after the push, and then needs you,
// or, where the repository's [[repo]] ci_dispatch names the workflow that
// produces its required checks, starts that workflow on the PR's branch
// once for the head (openStartCI) as [[repo]] ci_dispatch_as, else the
// watch's poll identity, the daemon's own client of that identity (never
// the fixer's pane; a fixer App's over a token with Actions write minted
// for the daemon alone, ci_actions.go), and waits for its run while the
// head's checks run, while the run is queued or runs (the poll reads it,
// awaitCIRuns: GitHub's rollup of the head leaves it out) and settle_max
// after the start. GitHub runs the workflow as the branch has it and
// refuses one without a workflow_dispatch trigger there; a PR that changes
// its own workflows or actions is never started (its CI would run its own
// code with the repository's secrets). The start is recorded before the
// call (kvPRCIStart), so a head is started at most once, a crash between
// the two included. Before it needs you or starts the workflow, rule 11
// reads the head's runs once and re-runs a run that started no job
// (ci_rerun.go).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kvPRCIStart holds the look's start of the PR's CI workflow (ciStart as
// JSON), the last head's.
func kvPRCIStart(prID int64) string { return fmt.Sprintf("pr.%d.ci_dispatch", prID) }

// ciStart is the look's start of a PR's CI workflow at Head: the workflow
// file, when it was asked (At: written before the call, so a start whose
// end was lost still counts), why it failed (Failed: GitHub's answer as a
// status, "GitHub answered HTTP 422", never its message; a 403 adds
// cannotStartWorkflows' words) or why magnum did not start it (Refused, At
// zero), and its run as the poll last read it (Run: nil, none seen).
type ciStart struct {
	Head     string    `json:"head"`
	Workflow string    `json:"workflow"`
	At       time.Time `json:"at,omitzero"`
	Failed   string    `json:"failed,omitempty"`
	Refused  string    `json:"refused,omitempty"`
	Run      *ciRun    `json:"run,omitempty"`
}

// ciRun is the newest run of a start's workflow that a dispatch started at
// the head since the start (dispatchedRun): GitHub's status of it (queued,
// in_progress, completed, ...) and, once it ended, its conclusion.
type ciRun struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
}

// running reports whether r is a run that has not ended (queued, waiting
// or in progress); ended whether it ended. Both are false for nil, no run.
func (r *ciRun) running() bool { return r != nil && r.Status != "completed" }
func (r *ciRun) ended() bool   { return r != nil && r.Status == "completed" }

// ciRunSkew is how long before the look's start GitHub may have created
// its run, GitHub's clock against the daemon's: a run created earlier is
// an older one of the head (someone's start by hand), not the look's.
const ciRunSkew = time.Minute

// workflowDispatcher is the engine's GitHub client's start of a workflow
// run (*github.Client's DispatchWorkflow); dispatchedRunLister its list of
// the runs a dispatch started (DispatchedRuns).
type (
	workflowDispatcher interface {
		DispatchWorkflow(ctx context.Context, owner, repo, workflow, ref string) error
	}
	dispatchedRunLister interface {
		DispatchedRuns(ctx context.Context, owner, repo, workflow, sha string) ([]github.WorkflowRun, error)
	}
)

var (
	_ workflowDispatcher  = (*github.Client)(nil)
	_ dispatchedRunLister = (*github.Client)(nil)
)

// ciDispatchIdentity is the identity that starts the CI workflow of
// repository full under w, and reads its runs: [[repo]] ci_dispatch_as,
// else the watch's poll identity, else its identity.
func (e *Engine) ciDispatchIdentity(full string, w config.Watch) string {
	return cmp.Or(e.cfg.CIDispatchAs(full), w.PollIdentity, w.Identity)
}

// openCIStartOf is what the look knows of starting pr's CI workflow, of
// repository full, for f.ci (rule 11's): [[repo]] ci_dispatch where a
// required check did not run at the head ("" otherwise), and its start at
// the head (zero: none).
func (e *Engine) openCIStartOf(ctx context.Context, full string, pr store.PR, ci openCI) (string, ciStart) {
	if len(ci.missing) == 0 {
		return "", ciStart{}
	}
	workflow := e.cfg.CIDispatch(full)
	if workflow == "" {
		return "", ciStart{}
	}
	return workflow, e.ciStartAt(ctx, pr)
}

// ciStartAt is the look's start of pr's CI workflow at its head
// (kvPRCIStart; zero: none, or an older head's).
func (e *Engine) ciStartAt(ctx context.Context, pr store.PR) ciStart {
	var s ciStart
	if raw, ok := e.getKV(ctx, kvPRCIStart(pr.ID)); ok && json.Unmarshal([]byte(raw), &s) == nil && s.Head == pr.HeadSHA {
		return s
	}
	return ciStart{}
}

// awaitCIRuns is the poll's read of the runs of the look's starts of the
// CI workflow of repository full under w at now, and the numbers of the
// radar's PRs whose checks the poll reads for them, whatever the rollup
// says (fetchDetails). A start waits where GitHub took it at the head the
// radar still shows, the head's stored checks miss a required check
// (openCI), and it is less than settle_max old or its run was queued or
// running at the last read. For such a PR the poll reads the head's checks
// and the workflow's dispatched runs at the head (dispatchedRun), and keeps
// the newest one that GitHub created since the start in the start's record
// (ciStart.Run). GitHub's rollup of the head leaves that run out (its
// workflow_dispatch check suite), so a run that is queued or runs shows
// only there, and the Completion check the run creates at its end in an
// older suite of the head is created passed, which moves neither the
// rollup's state nor its failed or pending counts. So the look waits while
// the run does, past settle_max (GitHub's limits end a run), and the poll
// that sees the run end reads the checks it left. One read of each a poll
// for such a PR at most, none once the run's check shows, the run ended
// and, with no run, the wait ended; it starts nothing. The look's re-run of
// a run that started no job (ci_rerun.go) waits the same way in every
// repository, ci_dispatch or not: while GitHub took it, the head's stored
// checks miss a required check, and it is less than settle_max old or its
// new attempt was queued or running at the last read, the poll reads the
// head's checks and its runs (seeRerun).
func (e *Engine) awaitCIRuns(ctx context.Context, w config.Watch, full string, prs []github.PRRadar, byNode map[string]store.PR, now time.Time) map[int]bool {
	owner, name, _ := strings.Cut(full, "/")
	identity, dispatch, settleMax := e.ciDispatchIdentity(full, w), e.cfg.CIDispatch(full) != "", e.openSettle().max
	acts := e.ciActions(identity) // ci_actions.go: one token for the poll's reads of the repository
	defer acts.end()
	var awaited map[int]bool
	for _, p := range prs {
		pr, ok := byNode[p.NodeID]
		if !ok || pr.HeadSHA != p.HeadRefOid { // a moved head's checks are read anyway; the start was the old head's
			continue
		}
		r := e.ciRerunAt(ctx, pr) // ci_rerun.go: the look's re-run waits as a start does
		rerun := r.rerunWaits(now, settleMax)
		var s ciStart
		if dispatch {
			s = e.ciStartAt(ctx, pr)
		}
		start := !s.At.IsZero() && s.Failed == "" && (now.Before(s.At.Add(settleMax)) || s.Run.running())
		if !rerun && !start || len(e.openCI(ctx, full, pr).missing) == 0 {
			continue
		}
		if awaited == nil {
			awaited = map[int]bool{}
		}
		awaited[p.Number] = true
		if rerun {
			e.seeRerun(ctx, acts, owner, name, pr, r)
		}
		if !start {
			continue
		}
		if run, ok := e.dispatchedRun(ctx, acts, owner, name, s); ok && (s.Run == nil || *s.Run != run) {
			s.Run = &run
			e.writeCIStart(ctx, pr, s)
		}
	}
	return awaited
}

// dispatchedRun is the newest run of start s's workflow at its head that
// GitHub created since the start (less ciRunSkew), as identity reads the
// runs a dispatch started (DispatchedRuns); ok is false for none, and for
// runs that were not read: identity's GitHub budget is low, it has no
// client that lists them, or the call failed (said once per distinct
// error, poll.ci_runs_error), which leaves the last read standing.
func (e *Engine) dispatchedRun(ctx context.Context, acts *ciActions, owner, name string, s ciStart) (ciRun, bool) {
	if _, low := e.githubLow(acts.identity); low {
		return ciRun{}, false
	}
	gh, err := acts.client(ctx)
	if errors.Is(err, errNoGitHub) {
		return ciRun{}, false
	}
	full := owner + "/" + name
	var runs []github.WorkflowRun
	if err == nil {
		l, ok := gh.(dispatchedRunLister)
		if !ok {
			return ciRun{}, false
		}
		runs, err = l.DispatchedRuns(ctx, owner, name, s.Workflow, s.Head)
	}
	if err != nil {
		if e.changed("ci_runs:"+full, err.Error()) {
			e.event(ctx, "warn", "repo:"+full, "poll.ci_runs_error",
				fmt.Sprintf("the runs of %s that magnum started at %s: %v", s.Workflow, textx.ShortSHA(s.Head), err), nil)
		}
		return ciRun{}, false
	}
	e.changed("ci_runs:"+full, "")
	for _, r := range runs { // newest first
		if !r.CreatedAt.Before(s.At.Add(-ciRunSkew)) {
			return ciRun{Status: r.Status, Conclusion: r.Conclusion}, true
		}
	}
	return ciRun{}, false
}

// openMissingCI is rule 11's verdict v of f's head, at which the required
// checks f.ci.missing names have no run (their words: missingWhat): while a
// check of the head has not finished (f.ci.running: a job that needs others
// is created only once they end, however long they run) and until
// settle_max after the head moved, it waits for them to start (CI at the
// head). After that they did not run, and it needs you with how to start
// them, unless the look starts the workflow ci_dispatch names
// (f.ciWorkflow): it asks for the start (v.dispatch) when nothing holds it,
// waits for the end of `magnum pause` and for the GitHub budget (the poll
// identity's, f.low, and ci_dispatch_as's, f.ciLow), and waits
// for the run of a start it made (f.ciStart) while the head's checks run,
// while the run is queued or runs (ciStart.Run) and until settle_max after
// the start. A start that failed, one magnum refused, one whose run ended
// without the checks (in words of the run's conclusion, runEnd) and one
// whose run never came need you. Before either, where no start was made
// at the head, the look's read of the head's runs and its re-run of the
// one that started no job decide (openRerunVerdict, ci_rerun.go), and the
// re-run's attempt is waited for as a start's run is.
func openMissingCI(f openFacts, v OpenVerdict) OpenVerdict {
	short, what := textx.ShortSHA(f.pr.HeadSHA), missingWhat(f.ci.missing)
	waits := func(why string, until time.Time) OpenVerdict {
		if !until.After(f.now) {
			until = time.Time{} // unknown: until the head's checks end
		}
		v.Verdict, v.Why, v.Until = OpenWaits, "CI at "+short+": "+why, until
		return v
	}
	needsYou := func(how string) OpenVerdict {
		v.Needs = append(v.Needs, OpenNeed{Kind: "ci_missing", URL: f.pr.URL})
		v.Verdict, v.Why = OpenNeedsYou, fmt.Sprintf("%s did not run at %s: %s", what, short, how)
		return v
	}
	wf, s := f.ciWorkflow, f.ciStart
	started := !s.At.IsZero() && s.Failed == ""
	startedWhy := func() string {
		return fmt.Sprintf("magnum started %s at %s, as %s did not run", wf, waitClock(s.At, f.now), what)
	}
	due := f.pr.HeadChangedAt.Add(f.settle.max)
	switch {
	case (f.ci.running || s.Run.running()) && started:
		return waits(startedWhy(), s.At.Add(f.settle.max))
	case (f.ci.running || f.ciRerun.Seen.running()) && f.ciRerun.asked(): // ci_rerun.go
		return waits(rerunWhy(f, what), f.ciRerun.At.Add(f.settle.max))
	case f.ci.running || f.now.Before(due):
		return waits(what+" has not started", due)
	}
	if s.At.IsZero() && s.Refused == "" { // ci_rerun.go: a run of the head that started no job, before any start
		if rv, ok := openRerunVerdict(f, &v, waits, needsYou, what); ok {
			return rv
		}
	}
	const byHand = "; run it on the PR's branch"
	switch {
	case wf == "":
		return needsYou("run its workflow on the PR's branch, or push")
	case s.Refused != "":
		return needsYou(fmt.Sprintf("magnum does not start %s (%s)%s", wf, s.Refused, byHand))
	case s.Failed != "":
		return needsYou(fmt.Sprintf("magnum's start of %s failed (%s)%s", wf, s.Failed, byHand))
	case started && f.now.Before(s.At.Add(f.settle.max)):
		return waits(startedWhy(), s.At.Add(f.settle.max))
	case started && s.Run.ended():
		return needsYou(fmt.Sprintf("magnum started %s at %s and its run ended%s without it%s", wf, waitClock(s.At, f.now), runEnd(s.Run.Conclusion), byHand))
	case started:
		return needsYou(fmt.Sprintf("magnum started %s at %s and no run came%s", wf, waitClock(s.At, f.now), byHand))
	case f.pause != "":
		return waits(fmt.Sprintf("%s did not run; magnum starts %s once the pause ends", what, wf), f.pauseUntil)
	case f.low || f.ciLow:
		return waits(fmt.Sprintf("%s did not run; magnum starts %s once the GitHub budget allows", what, wf), time.Time{})
	}
	v.dispatch = true
	return needsYou("magnum starts " + wf)
}

// conclusionRe is a run's conclusion as GitHub writes one ("timed_out").
var conclusionRe = regexp.MustCompile(`^[a-z_]{1,32}$`)

// runEnd is how a run of the CI workflow ended, by its conclusion, in the
// verdict's words (" (timed out)"); "" for none, or one that is not
// GitHub's plain word.
func runEnd(conclusion string) string {
	if !conclusionRe.MatchString(conclusion) {
		return ""
	}
	return " (" + strings.ReplaceAll(conclusion, "_", " ") + ")"
}

// missingWhat names missing, required entries with no run on a head, in
// the verdict's words: the first ("required check Completion", "required
// workflow CI" for a workflow entry), and how many more ("and 1 more"). An
// entry is the repository's configuration or GitHub's rulesets, never PR
// text; one that is not plain text (shownCheckName) is "a required check".
func missingWhat(missing []string) string {
	what := "a required check"
	if workflow, ok := strings.CutPrefix(missing[0], store.RequiredWorkflowPrefix); ok && shownCheckName(workflow) {
		what = "required workflow " + workflow
	} else if !ok && shownCheckName(missing[0]) {
		what = "required check " + missing[0]
	}
	if n := len(missing) - 1; n > 0 {
		what += fmt.Sprintf(" and %d more", n)
	}
	return what
}

// openStartCI starts workflow, [[repo]] ci_dispatch, on the branch of pr,
// an own PR of repo under w whose required check did not run at its head
// (rule 11 asked: v.dispatch), and returns the start as the verdict reads
// it; ok is false when it did nothing and the next look asks again (the
// head's file list is not read yet, or the record could not be written).
// The start itself, its refusals, its record and its identity are
// startCI's (ci_dispatch.go), with the look's events:
// fix.open_ci_dispatch, fix.open_ci_dispatch_failed or
// fix.open_ci_dispatch_refused.
func (e *Engine) openStartCI(ctx context.Context, acts *ciActions, repo store.Repo, pr store.PR, workflow, what string) (ciStart, bool) {
	lead := what + " did not run at " + textx.ShortSHA(pr.HeadSHA)
	return e.startCI(ctx, acts, repo, pr, workflow, ciWords{
		kind:    "fix.open_ci_dispatch",
		refused: func(why string) string { return fmt.Sprintf("%s; magnum does not start %s: %s", lead, workflow, why) },
		failed: func(identity, why string) string {
			return fmt.Sprintf("%s; magnum's start of %s as %s failed: %s", lead, workflow, identity, why)
		},
		started: func(identity string) string {
			return fmt.Sprintf("%s; magnum started %s on the PR's branch as %s", lead, workflow, identity)
		},
	})
}

// cannotStartWorkflows says why GitHub refused identity a workflow start
// (HTTP 403, not its rate limit), in magnum's words: the identity cannot
// start workflows, what one of its kind needs, and the key that names an
// identity that can.
func (e *Engine) cannotStartWorkflows(identity string) string {
	need := "an App needs Actions: write, a gh login push access and the repo scope"
	if id := e.cfg.IdentityByName(identity); id != nil {
		need = "a gh login needs push access and the repo scope"
		if id.Kind == "app" {
			need = "an App needs Actions: write"
		}
	}
	return identity + " cannot start workflows, " + need + "; set [[repo]] ci_dispatch_as to an identity that can"
}

// writeCIStart records s as pr's start of its CI workflow (kvPRCIStart),
// and reports whether it did.
func (e *Engine) writeCIStart(ctx context.Context, pr store.PR, s ciStart) bool {
	b, err := json.Marshal(s)
	if err == nil {
		err = e.st.SetKV(ctx, kvPRCIStart(pr.ID), string(b))
	}
	if err != nil {
		e.warnUnlessStopped(ctx, err, "autofix open: record the start of the PR's CI", "pr", pr.ID)
		return false
	}
	return true
}
