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
// the fixer's pane), and waits for its run
// while the head's checks run and settle_max after the start. GitHub runs
// the workflow as the branch has it and refuses one without a
// workflow_dispatch trigger there; a PR that changes its own workflows or
// actions is never started (its CI would run its own code with the
// repository's secrets). The start is recorded before the call
// (kvPRCIStart), so a head is started at most once, a crash between the two
// included.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
// zero).
type ciStart struct {
	Head     string    `json:"head"`
	Workflow string    `json:"workflow"`
	At       time.Time `json:"at,omitzero"`
	Failed   string    `json:"failed,omitempty"`
	Refused  string    `json:"refused,omitempty"`
}

// workflowDispatcher is the engine's GitHub client's start of a workflow
// run (*github.Client's DispatchWorkflow).
type workflowDispatcher interface {
	DispatchWorkflow(ctx context.Context, owner, repo, workflow, ref string) error
}

var _ workflowDispatcher = (*github.Client)(nil)

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
	var s ciStart
	if raw, ok := e.getKV(ctx, kvPRCIStart(pr.ID)); ok && json.Unmarshal([]byte(raw), &s) == nil && s.Head == pr.HeadSHA {
		return workflow, s
	}
	return workflow, ciStart{}
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
// for the run of a start it made (f.ciStart) while the head's checks run
// and until settle_max after the start. A start that failed, one magnum
// refused, and one whose run never came need you.
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
		v.Needs = append(v.Needs, OpenNeed{Kind: "ci", URL: f.pr.URL})
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
	case f.ci.running && started:
		return waits(startedWhy(), s.At.Add(f.settle.max))
	case f.ci.running || f.now.Before(due):
		return waits(what+" has not started", due)
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
// It refuses, once per head, a PR that changes its own workflows or
// actions or whose files GitHub did not list whole, and one whose branch
// it does not know. Else it records the start (kvPRCIStart) before it
// asks GitHub as [[repo]] ci_dispatch_as, else the watch's poll identity
// (the identity the look reads the PR and its budget with), else its
// identity, and records how it ended, with an event naming that identity:
// fix.open_ci_dispatch, fix.open_ci_dispatch_failed (the status only: the
// message of GitHub's refusal can name the branch, the PR's text; a 403
// says that the identity cannot start workflows, cannotStartWorkflows) or
// fix.open_ci_dispatch_refused.
func (e *Engine) openStartCI(ctx context.Context, repo store.Repo, w config.Watch, pr store.PR, workflow, what string) (ciStart, bool) {
	s := ciStart{Head: pr.HeadSHA, Workflow: workflow}
	files, listed, err := e.st.PRFilesOf(ctx, pr.ID)
	switch {
	case err != nil:
		e.warnUnlessStopped(ctx, err, "autofix open: the PR's files before starting its CI", "pr", pr.ID)
		return ciStart{}, false
	case !listed || files.HeadSHA != pr.HeadSHA:
		return ciStart{}, false // the poll lists them with its next Details read
	case slices.ContainsFunc(files.Paths, changesWorkflow):
		s.Refused = "the PR changes its own workflows or actions"
	case files.Truncated:
		s.Refused = "GitHub did not list every file of the PR"
	case deref(pr.HeadRef) == "":
		s.Refused = "the PR's branch is not known"
	}
	subject, short := prSubject(repo, pr.Number), textx.ShortSHA(pr.HeadSHA)
	identity := cmp.Or(e.cfg.CIDispatchAs(repo.FullName()), w.PollIdentity, w.Identity)
	data := map[string]any{"head": pr.HeadSHA, "workflow": workflow, "identity": identity}
	if s.Refused != "" {
		if !e.writeCIStart(ctx, pr, s) {
			return ciStart{}, false
		}
		e.event(ctx, "info", subject, "fix.open_ci_dispatch_refused",
			fmt.Sprintf("%s did not run at %s; magnum does not start %s: %s", what, short, workflow, s.Refused), data)
		return s, true
	}
	s.At = e.now()
	if !e.writeCIStart(ctx, pr, s) {
		return ciStart{}, false
	}
	gh := e.gh(identity)
	d, ok := gh.(workflowDispatcher)
	if gh == nil || !ok {
		s.Failed = "no GitHub client of " + identity
	} else if err := d.DispatchWorkflow(ctx, repo.Owner, repo.Name, workflow, deref(pr.HeadRef)); err != nil {
		s.Failed = "the call failed"
		var api *github.APIError
		if errors.As(err, &api) && api.Status != 0 {
			s.Failed = fmt.Sprintf("GitHub answered HTTP %d", api.Status)
		}
		if errors.Is(err, github.ErrForbidden) {
			s.Failed += ": " + e.cannotStartWorkflows(identity)
		}
	}
	e.writeCIStart(ctx, pr, s)
	if s.Failed != "" {
		data["failed"] = s.Failed
		e.event(ctx, "warn", subject, "fix.open_ci_dispatch_failed",
			fmt.Sprintf("%s did not run at %s; magnum's start of %s as %s failed: %s", what, short, workflow, identity, s.Failed), data)
		return s, true
	}
	e.event(ctx, "info", subject, "fix.open_ci_dispatch",
		fmt.Sprintf("%s did not run at %s; magnum started %s on the PR's branch as %s", what, short, workflow, identity), data)
	return s, true
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
