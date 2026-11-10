package engine

// A run that should produce a required check of an own PR's head and that
// started no job (approved by the operator on 2026-11-09, after the live
// case of that day: run 42673098082 of ci.yml, event pull_request, ended
// "failure" after 50 s with no job, "This run likely failed because of a
// workflow file issue", while the PR changed one .rst file and the same
// workflow passed on other PRs that day: a GitHub startup failure, which a
// person re-ran by hand). GitHub's rollup of the head holds nothing of such
// a run (its check suite has no check run), so rule 11 of the look said
// that the required check did not run and needed you. Now, at its "did not
// run" verdict, the look reads the head's runs once (openReadRuns: every
// workflow's, github.HeadRuns, as startCI's identity, recorded per head in
// kvPRCIRerun, never a read per tick; a run that has not ended or a failed
// read is read again settle_max later). Where each .github/workflows
// file's newest run for a pull_request or a push ended with no job started
// (noJobStarted: startup_failure, or failure or cancelled with no job in
// its latest attempt), no run of the head has not ended (queuedWhy), and
// the PR changes no workflow or action file (changesActions), it re-runs
// that run once for
// the head (openRerunCI: POST actions/runs/{id}/rerun, recorded before the
// call, so a restart or a second look never re-runs twice), and waits for
// its new attempt (the same run id, a higher run_attempt) as for a started
// run: the poll reads it (seeRerun, from awaitCIRuns). A second start that
// fails at the head, GitHub's refusal of the re-run and magnum's own need
// you; a new head starts fresh.

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kvPRCIRerun holds the look's read of the runs at the PR's head and its
// re-run of the one that started no job (ciRerun as JSON), the last head's.
func kvPRCIRerun(prID int64) string { return fmt.Sprintf("pr.%d.ci_rerun", prID) }

// ciRerun is what the look read of the runs at Head (at ReadAt) and did
// about them: Run is the run that started no job (nil: none); Queued a run
// of the head that had not ended at the read (its id, QueuedStatus its
// status then), and Unread a read that failed (UnreadWhy: GitHub would not
// mint the token of the read, actionsNotGranted's words), either of which
// the look reads again settle_max after ReadAt; At when the look asked GitHub to
// re-run Run (written before the call, so a re-run whose answer was lost
// still counts), Failed why GitHub refused (its status, never its message;
// a 403 adds cannotStartWorkflows' words) and Refused why magnum did not
// (At zero); Seen is the re-run's new attempt as the poll last read it
// (nil: none seen).
type ciRerun struct {
	Head         string     `json:"head"`
	ReadAt       time.Time  `json:"read_at"`
	Queued       int64      `json:"queued,omitempty"`
	QueuedStatus string     `json:"queued_status,omitempty"`
	Unread       bool       `json:"unread,omitempty"`
	UnreadWhy    string     `json:"unread_why,omitempty"`
	Run          *ciNoJob   `json:"run,omitempty"`
	At           time.Time  `json:"at,omitzero"`
	Failed       string     `json:"failed,omitempty"`
	Refused      string     `json:"refused,omitempty"`
	Seen         *ciAttempt `json:"seen,omitempty"`
}

// ciNoJob is a run that started no job: its id, its workflow file, the
// attempt that started none, and that attempt's conclusion.
type ciNoJob struct {
	ID         int64  `json:"id"`
	Workflow   string `json:"workflow"`
	Attempt    int    `json:"attempt"`
	Conclusion string `json:"conclusion"`
}

// ciAttempt is a newer attempt of a re-run run: its number, GitHub's status
// and, once it ended, its conclusion, and NoJob: it ended with no job
// started.
type ciAttempt struct {
	Attempt    int    `json:"attempt"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	NoJob      bool   `json:"no_job,omitempty"`
}

// running reports whether a is an attempt that has not ended, ended
// whether it ended. Both are false for nil, none seen.
func (a *ciAttempt) running() bool { return a != nil && a.Status != "completed" }
func (a *ciAttempt) ended() bool   { return a != nil && a.Status == "completed" }

// asked reports whether the look asked GitHub to re-run the head's run and
// GitHub took it.
func (r ciRerun) asked() bool { return !r.At.IsZero() && r.Failed == "" && r.Run != nil }

// decided reports whether the look re-ran the head's run or refused to:
// the record that a look and a restart never redo.
func (r ciRerun) decided() bool { return !r.At.IsZero() || r.Refused != "" }

// readDue reports whether the look reads the runs of head (again) at now:
// none read at the head, or settleMax passed since a read that found a run
// that had not ended or that failed.
func (r ciRerun) readDue(head string, now time.Time, settleMax time.Duration) bool {
	return r.Head != head || (r.Queued != 0 || r.Unread) && !now.Before(r.ReadAt.Add(settleMax))
}

// named is the run in the verdict's words: "run 42673098082 of ci.yml".
func (r *ciNoJob) named() string { return fmt.Sprintf("run %d of %s", r.ID, r.Workflow) }

// headRunLister is the engine's GitHub client's list of every workflow's
// runs at a commit (*github.Client's HeadRuns), runJobLister its list of a
// run's jobs (RunJobs) and runRerunner its re-run of a run (RerunRun).
type (
	headRunLister interface {
		HeadRuns(ctx context.Context, owner, repo, sha string) ([]github.WorkflowRun, error)
	}
	runJobLister interface {
		RunJobs(ctx context.Context, owner, repo string, runID int64) ([]github.WorkflowJob, error)
	}
	runRerunner interface {
		RerunRun(ctx context.Context, owner, repo string, runID int64) error
	}
)

var (
	_ headRunLister = (*github.Client)(nil)
	_ runJobLister  = (*github.Client)(nil)
	_ runRerunner   = (*github.Client)(nil)
)

// ciRerunAt is the look's record of pr's head (kvPRCIRerun; zero: none, an
// older head's, or one that cannot be read: openReadRuns and openRerunCI
// read it again before they act).
func (e *Engine) ciRerunAt(ctx context.Context, pr store.PR) ciRerun {
	r, ok, err := readKVJSON[ciRerun](ctx, e, kvPRCIRerun(pr.ID), "the look's re-run")
	if err != nil || !ok || r.Head != pr.HeadSHA {
		return ciRerun{}
	}
	return r
}

// storedRerun is the record of pr's head as openReadRuns and openRerunCI
// read it before they act: ok is false when it cannot be read (said), so
// they do nothing.
func (e *Engine) storedRerun(ctx context.Context, pr store.PR) (ciRerun, bool) {
	r, found, err := readKVJSON[ciRerun](ctx, e, kvPRCIRerun(pr.ID), "the look's re-run")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "autofix open: the record of the head's runs", "pr", pr.ID)
		return ciRerun{}, false
	}
	if !found || r.Head != pr.HeadSHA {
		return ciRerun{}, true
	}
	return r, true
}

// noJobStarted reports whether run, which ended, started no job: GitHub's
// startup_failure, or a failure or a cancel whose latest attempt has no job
// (read through gh as owner/name, only for those). Any other conclusion
// (success, action_required, skipped, ...) is no start that failed.
func noJobStarted(ctx context.Context, gh GitHub, owner, name string, run github.WorkflowRun) (bool, error) {
	switch run.Conclusion {
	case "startup_failure":
		return true, nil
	case "failure", "cancelled":
		l, ok := gh.(runJobLister)
		if !ok {
			return false, errors.New("no GitHub client that lists a run's jobs")
		}
		jobs, err := l.RunJobs(ctx, owner, name, run.ID)
		return err == nil && len(jobs) == 0, err
	}
	return false, nil
}

// noJobRunAt reads the runs at commit sha of owner/name through gh, newest
// first, and returns the newest one that started no job (noJobStarted)
// among the newest run of each .github/workflows file for each of a
// pull_request and a push (an older run that a newer one of its workflow
// and event followed is no longer the head's), and the id and the status
// of the newest run of a .github/workflows file, of any event, that has
// not ended (queued: none is then returned, as a re-run beside it could
// cancel it or be cancelled by it, the workflow's concurrency group being
// its workflow and ref).
func noJobRunAt(ctx context.Context, gh GitHub, owner, name, sha string) (found *ciNoJob, queued int64, status string, err error) {
	l, ok := gh.(headRunLister)
	if !ok {
		return nil, 0, "", errors.New("no GitHub client that lists a commit's runs")
	}
	runs, err := l.HeadRuns(ctx, owner, name, sha)
	if err != nil {
		return nil, 0, "", err
	}
	seen := map[[2]string]bool{}
	for _, run := range runs { // newest first
		if run.Workflow == "" {
			continue // GitHub's own dynamic runs
		}
		if !run.Done() && queued == 0 {
			queued, status = run.ID, run.Status
		}
		key := [2]string{run.Workflow, run.Event}
		if run.Event != "pull_request" && run.Event != "push" || seen[key] {
			continue
		}
		seen[key] = true
		if found != nil || queued != 0 || !run.Done() { // nothing is re-run beside a queued run: no jobs to read
			continue
		}
		noJob, jobsErr := noJobStarted(ctx, gh, owner, name, run)
		if jobsErr != nil {
			return nil, 0, "", jobsErr
		}
		if noJob {
			found = &ciNoJob{ID: run.ID, Workflow: run.Workflow, Attempt: run.Attempt, Conclusion: run.Conclusion}
		}
	}
	if queued != 0 {
		found = nil
	}
	return found, queued, status, nil
}

// openReadRuns is the look's one read of the runs at pr's head (rule 11
// asked: v.readRuns), in the look's batch acts (startCI's identity,
// ciDispatchIdentity), and its record (kvPRCIRerun): the run that started
// no job (noJobRunAt), a run that has not ended, or a read that failed
// (Unread, said once per distinct error: fix.open_ci_runs_error; UnreadWhy
// when GitHub would not mint the batch's token). A head the look re-ran or
// refused already is not read again. ok is false when it did nothing (the record could not
// be read or written).
func (e *Engine) openReadRuns(ctx context.Context, acts *ciActions, repo store.Repo, pr store.PR) (ciRerun, bool) {
	if stored, ok := e.storedRerun(ctx, pr); !ok || stored.decided() {
		return stored, ok && stored.decided()
	}
	full, who := repo.FullName(), acts.identity
	r := ciRerun{Head: pr.HeadSHA, ReadAt: e.now()}
	gh, err := acts.client(ctx)
	if err == nil {
		r.Run, r.Queued, r.QueuedStatus, err = noJobRunAt(ctx, gh, repo.Owner, repo.Name, pr.HeadSHA)
	}
	if err != nil {
		r = ciRerun{Head: pr.HeadSHA, ReadAt: r.ReadAt, Unread: true}
		if actionsRefused(err) { // ci_actions.go: the verdict says what to grant
			r.UnreadWhy = actionsNotGranted(who)
		}
		if e.changed("open_ci_runs:"+full, err.Error()) {
			e.event(ctx, "warn", prSubject(repo, pr.Number), "fix.open_ci_runs_error",
				fmt.Sprintf("the runs at %s, read as %s: %v", textx.ShortSHA(pr.HeadSHA), who, err), map[string]any{"head": pr.HeadSHA, "identity": who})
		}
	} else {
		e.changed("open_ci_runs:"+full, "")
	}
	if err := writeKVJSON(ctx, e, kvPRCIRerun(pr.ID), "the look's re-run", r); err != nil {
		e.warnUnlessStopped(ctx, err, "autofix open: record the read of the head's runs", "pr", pr.ID)
		return ciRerun{}, false
	}
	return r, true
}

// changesActions reports whether p, a path the PR changes, is a file GitHub
// Actions runs as the PR's head has it: a workflow or a local action
// (changesWorkflow), or an action's metadata anywhere (action.yml,
// action.yaml, in any case).
func changesActions(p string) bool {
	base := strings.ToLower(path.Base(p))
	return changesWorkflow(p) || base == "action.yml" || base == "action.yaml"
}

// openRerunCI re-runs r.Run, the run of pr's head that started no job (rule
// 11 asked: v.rerun), as startCI's identity, once for the head: the record
// is written before the call (kvPRCIRerun's At), and a head the record says
// was re-run or refused is not asked again. It refuses, once per head, a PR
// that changes its own workflows or actions (changesActions: the run would
// run the PR's code with the repository's secrets) or whose files GitHub
// did not list whole. ok is false when it did nothing and the next look
// asks again (the head's files are not listed yet, or the record could not
// be read or written). Events: fix.open_ci_rerun, fix.open_ci_rerun_failed
// (GitHub's status, never its message) and fix.open_ci_rerun_refused, whose
// data names the head, the run, its workflow, attempt and conclusion, and
// the identity.
func (e *Engine) openRerunCI(ctx context.Context, acts *ciActions, repo store.Repo, pr store.PR, r ciRerun, what string) (ciRerun, bool) {
	if stored, ok := e.storedRerun(ctx, pr); !ok || stored.decided() {
		return stored, ok && stored.decided()
	}
	files, listed, err := e.st.PRFilesOf(ctx, pr.ID)
	switch {
	case err != nil:
		e.warnUnlessStopped(ctx, err, "the PR's files before re-running its CI", "pr", pr.ID)
		return ciRerun{}, false
	case !listed || files.HeadSHA != pr.HeadSHA:
		return ciRerun{}, false // the poll lists them with its next Details read
	case slices.ContainsFunc(files.Paths, changesActions):
		r.Refused = "the PR changes its own workflows or actions"
	case files.Truncated:
		r.Refused = "GitHub did not list every file of the PR"
	}
	subject, run := prSubject(repo, pr.Number), r.Run.named()
	identity := acts.identity
	lead := fmt.Sprintf("%s did not run at %s, as %s started no job%s", what, textx.ShortSHA(pr.HeadSHA), run, runEnd(r.Run.Conclusion))
	data := map[string]any{"head": pr.HeadSHA, "run": r.Run.ID, "workflow": r.Run.Workflow, "attempt": r.Run.Attempt,
		"conclusion": r.Run.Conclusion, "identity": identity}
	write := func() bool {
		if err := writeKVJSON(ctx, e, kvPRCIRerun(pr.ID), "the look's re-run", r); err != nil {
			e.warnUnlessStopped(ctx, err, "autofix open: record the re-run of the head's run", "pr", pr.ID)
			return false
		}
		return true
	}
	if r.Refused != "" {
		if !write() {
			return ciRerun{}, false
		}
		e.event(ctx, "info", subject, "fix.open_ci_rerun_refused", fmt.Sprintf("%s; magnum does not re-run it: %s", lead, r.Refused), data)
		return r, true
	}
	r.At = e.now()
	if !write() {
		return ciRerun{}, false
	}
	gh, err := acts.client(ctx) // after the record: a mint is no re-run
	if err == nil {
		if rr, ok := gh.(runRerunner); !ok {
			err = fmt.Errorf("%w of %s", errNoGitHub, identity)
		} else {
			err = rr.RerunRun(ctx, repo.Owner, repo.Name, r.Run.ID)
		}
	}
	if err != nil {
		r.Failed = e.actionsFailed(identity, err)
	}
	write()
	if r.Failed != "" {
		data["failed"] = r.Failed
		e.event(ctx, "warn", subject, "fix.open_ci_rerun_failed", fmt.Sprintf("%s; magnum's re-run of it as %s failed: %s", lead, identity, r.Failed), data)
		return r, true
	}
	e.event(ctx, "info", subject, "fix.open_ci_rerun", fmt.Sprintf("%s; magnum re-ran it as %s", lead, identity), data)
	return r, true
}

// openCIRerun is the look's side of the verdict v of f (lookOpen): the read
// of the head's runs it asked for (v.readRuns, openReadRuns), then the
// re-run (v.rerun, openRerunCI), each followed by the verdict decided again
// on what it did, which it returns with its work.
func (e *Engine) openCIRerun(ctx context.Context, acts *ciActions, repo store.Repo, pr store.PR, f *openFacts, v OpenVerdict, work []openItem) (OpenVerdict, []openItem) {
	if v.readRuns {
		if r, ok := e.openReadRuns(ctx, acts, repo, pr); ok {
			f.ciRerun = r
			v, work, _ = decideOpen(*f)
		}
	}
	if v.rerun {
		if r, ok := e.openRerunCI(ctx, acts, repo, pr, f.ciRerun, missingWhat(f.ci.missing)); ok {
			f.ciRerun = r
			v, work, _ = decideOpen(*f)
		}
	}
	return v, work
}

// rerunWaits reports whether the poll reads the head's runs for r, the
// look's re-run (awaitCIRuns) at now: GitHub took it, and it is less than
// settleMax old or its new attempt had not ended at the last read.
func (r ciRerun) rerunWaits(now time.Time, settleMax time.Duration) bool {
	return r.asked() && (now.Before(r.At.Add(settleMax)) || r.Seen.running())
}

// seeRerun is the poll's read of the new attempt of r's run (awaitCIRuns):
// the head's runs as identity, the one of r.Run's id whose attempt is newer
// than the one that started no job, and once it ended in a failure or a
// cancel, its jobs. It records what it saw (ciRerun.Seen) and says a second
// start that failed once (fix.open_ci_rerun_twice). Nothing is read while
// identity's GitHub budget is low or once the attempt ended; a failed read
// is said once per distinct error (poll.ci_runs_error) and leaves the last
// read standing.
func (e *Engine) seeRerun(ctx context.Context, acts *ciActions, owner, name string, pr store.PR, r ciRerun) {
	identity := acts.identity
	if _, low := e.githubLow(identity); low || r.Seen.ended() {
		return
	}
	full, short := owner+"/"+name, textx.ShortSHA(r.Head)
	fail := func(err error) {
		if e.changed("ci_head_runs:"+full, err.Error()) {
			e.event(ctx, "warn", "repo:"+full, "poll.ci_runs_error", fmt.Sprintf("the runs at %s, after magnum re-ran %s: %v", short, r.Run.named(), err), nil)
		}
	}
	gh, err := acts.client(ctx)
	if errors.Is(err, errNoGitHub) {
		return
	}
	if err != nil {
		fail(err)
		return
	}
	l, ok := gh.(headRunLister)
	if !ok {
		return
	}
	runs, err := l.HeadRuns(ctx, owner, name, r.Head)
	if err != nil {
		fail(err)
		return
	}
	i := slices.IndexFunc(runs, func(run github.WorkflowRun) bool { return run.ID == r.Run.ID && run.Attempt > r.Run.Attempt })
	if i < 0 {
		e.changed("ci_head_runs:"+full, "")
		return
	}
	run := runs[i]
	seen := &ciAttempt{Attempt: run.Attempt, Status: run.Status, Conclusion: run.Conclusion}
	if run.Done() {
		if seen.NoJob, err = noJobStarted(ctx, gh, owner, name, run); err != nil {
			fail(err)
			return
		}
	}
	e.changed("ci_head_runs:"+full, "")
	if r.Seen != nil && *r.Seen == *seen {
		return
	}
	r.Seen = seen
	if err := writeKVJSON(ctx, e, kvPRCIRerun(pr.ID), "the look's re-run", r); err != nil {
		e.warnUnlessStopped(ctx, err, "record the re-run's attempt", "pr", pr.ID)
		return
	}
	if seen.NoJob {
		e.event(ctx, "warn", prSubject(store.Repo{Owner: owner, Name: name}, pr.Number), "fix.open_ci_rerun_twice",
			fmt.Sprintf("%s failed to start twice at %s%s: the PR needs you", r.Run.named(), short, runEnd(seen.Conclusion)),
			map[string]any{"head": r.Head, "run": r.Run.ID, "workflow": r.Run.Workflow, "attempt": seen.Attempt, "conclusion": seen.Conclusion,
				"identity": identity})
	}
}

// openRerunVerdict is rule 11's verdict by the look's read of the head's
// runs and its re-run of the one that started no job (f.ciRerun), past the
// waits for the head's checks and before ci_dispatch's start: ok false
// leaves the verdict to the rest of rule 11 (no run that started no job,
// or a read that failed). Where the head's runs are due a read it asks for
// it (v.readRuns) unless the GitHub budget is low, and waits for the budget
// where nothing was read and no ci_dispatch would start the workflow
// instead; it needs you, with what to grant, while GitHub would not mint
// the token that reads them (UnreadWhy; a start would fail on the same
// mint); it waits while a run of the head has not ended (queuedWhy), asks
// for the re-run (v.rerun) when nothing holds it (`magnum pause`, the
// budget), waits for the re-run's attempt until settle_max after it, and
// needs you when magnum or GitHub refused the re-run, when its attempt
// started no job either, ended without the check, or never came.
func openRerunVerdict(f openFacts, v *OpenVerdict, waits func(string, time.Time) OpenVerdict, needsYou func(string) OpenVerdict, what string) (OpenVerdict, bool) {
	r, low := f.ciRerun, f.low || f.ciLow
	due := r.readDue(f.pr.HeadSHA, f.now, f.settle.max)
	if due {
		v.readRuns = !low
	}
	if r.Head == f.pr.HeadSHA && r.Unread && r.UnreadWhy != "" { // ci_actions.go: GitHub will not mint the token that reads them
		return needsYou(fmt.Sprintf("magnum cannot read the head's runs (%s); run its workflow on the PR's branch, or push", r.UnreadWhy)), true
	}
	if due {
		if r.Head != f.pr.HeadSHA || r.Unread {
			if low && f.ciWorkflow == "" {
				return waits(what+" did not run; magnum reads the head's runs once the GitHub budget allows", time.Time{}), true
			}
			return OpenVerdict{}, false
		}
	}
	if r.Queued != 0 {
		return waits(queuedWhy(r.Queued, r.QueuedStatus), r.ReadAt.Add(f.settle.max)), true
	}
	if r.Run == nil {
		return OpenVerdict{}, false
	}
	const onGitHub = "; re-run it on GitHub"
	run := r.Run.named()
	switch {
	case r.Refused != "":
		return needsYou(fmt.Sprintf("magnum does not re-run %s (%s)%s", run, r.Refused, onGitHub)), true
	case r.Failed != "":
		return needsYou(fmt.Sprintf("magnum's re-run of %s failed (%s)%s", run, r.Failed, onGitHub)), true
	case r.asked() && r.Seen.ended() && r.Seen.NoJob:
		return needsYou(fmt.Sprintf("%s failed to start twice%s%s, or push", run, runEnd(r.Seen.Conclusion), onGitHub)), true
	case r.asked() && f.now.Before(r.At.Add(f.settle.max)):
		return waits(rerunWhy(f, what), r.At.Add(f.settle.max)), true
	case r.asked() && r.Seen.ended():
		return needsYou(fmt.Sprintf("magnum re-ran %s at %s and it ended%s without it%s", run, waitClock(r.At, f.now), runEnd(r.Seen.Conclusion), onGitHub)), true
	case r.asked():
		return needsYou(fmt.Sprintf("magnum re-ran %s at %s and no new attempt came%s", run, waitClock(r.At, f.now), onGitHub)), true
	case f.pause != "":
		return waits(fmt.Sprintf("%s did not run; magnum re-runs %s once the pause ends", what, run), f.pauseUntil), true
	case low:
		return waits(fmt.Sprintf("%s did not run; magnum re-runs %s once the GitHub budget allows", what, run), time.Time{}), true
	}
	v.rerun = true
	return needsYou("magnum re-runs " + run), true
}

// queuedWhy is the wait beside run id of the head, which had not ended at
// the read, by status, its status then: one GitHub has not given a runner
// yet (requested, queued, pending) "run 42673098082 has started no job
// yet", and any other (in_progress, waiting for an environment) "run
// 42673098082 has not ended".
func queuedWhy(id int64, status string) string {
	switch status {
	case "requested", "queued", "pending":
		return fmt.Sprintf("run %d has started no job yet", id)
	}
	return fmt.Sprintf("run %d has not ended", id)
}

// rerunWhy is the wait for the look's re-run of f: "magnum re-ran run
// 42673098082 of ci.yml at 20:47, as required check build did not run".
func rerunWhy(f openFacts, what string) string {
	return fmt.Sprintf("magnum re-ran %s at %s, as %s did not run", f.ciRerun.Run.named(), waitClock(f.ciRerun.At, f.now), what)
}
