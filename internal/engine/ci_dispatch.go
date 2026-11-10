package engine

// Starting a PR's CI workflow (DECISIONS "The look starts the CI workflow a
// head's required check did not run in", R47, extended on 2026-11-08 to
// every push magnum makes). A repository whose CI workflow runs only when a
// PR opens gives every later push no run of the checks GitHub requires.
// The look starts ci_dispatch's workflow where a required check did not run
// at an own PR's head after settle_max (openStartCI, fix_open_ci.go); after
// a push magnum made itself (the daemon's merge of the PR's parent, a
// fixer's push the settle confirmed: notePush) it starts it ciPushGrace
// later (ciAfterPushes), once for the head, never while a run of the
// workflow on the PR's branch is in progress (a start cancels it: the
// workflow's concurrency group is the workflow and the branch), and not at
// all for a head a run of the workflow ran at already (a repository whose
// CI does run on pushes). Both start it as startCI does: R47's refusals (a
// PR that changes its own workflows or actions, a file list GitHub cut
// short, an unknown branch), its record before the call (kvPRCIStart, so a
// head is started at most once, a crash between the two included) and its
// identity ([[repo]] ci_dispatch_as, else the watch's poll identity, the
// daemon's own client, never the fixer's pane).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// ciPushGrace is how long after a push of its own to a PR magnum waits
// before it starts the PR's CI workflow: a run the push started on a
// repository whose CI runs on pushes shows by then.
const ciPushGrace = 2 * time.Minute

// kvPRPushed holds the last head magnum pushed to the PR (ciPush as JSON)
// until ciAfterPushes decided on its CI.
func kvPRPushed(prID int64) string { return fmt.Sprintf("pr.%d.pushed", prID) }

// ciPush is a head magnum pushed to a PR, and when.
type ciPush struct {
	Head string    `json:"head"`
	At   time.Time `json:"at"`
}

// recentRunLister is the engine's GitHub client's list of a workflow's
// newest runs of every branch (*github.Client's RecentRuns).
type recentRunLister interface {
	RecentRuns(ctx context.Context, owner, repo, workflow string) ([]github.WorkflowRun, error)
}

var _ recentRunLister = (*github.Client)(nil)

// ciWords are what startCI's events say: their kind (kind, kind+"_failed",
// kind+"_refused") and their messages, a refusal's (why: startCI's
// reason), a failed start's (as identity, with GitHub's status) and a
// start's (as identity).
type ciWords struct {
	kind    string
	refused func(why string) string
	failed  func(identity, why string) string
	started func(identity string) string
}

// notePush records head as a push magnum made to the PR just now: a merge
// of its parent, a fixer's push. ciAfterPushes starts its CI.
func (e *Engine) notePush(ctx context.Context, prID int64, head string) {
	if err := writeKVJSON(ctx, e, kvPRPushed(prID), "magnum's push", ciPush{Head: head, At: e.now().UTC()}); err != nil {
		e.warnUnlessStopped(ctx, err, "record magnum's push for its CI", "pr", prID)
	}
}

// ciAfterPushes is the tick's start of the CI workflow of each head magnum
// pushed (notePush) ciPushGrace ago: ciAfterPush.
func (e *Engine) ciAfterPushes(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	keys, err := e.st.KVKeys(ctx, "pr.*.pushed")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "magnum's pushes")
		return
	}
	now := e.now()
	for _, key := range keys {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.pushed", &id); err != nil || kvPRPushed(id) != key {
			continue
		}
		e.ciAfterPush(ctx, id, now)
	}
}

// ciAfterPush decides on the CI of the head magnum pushed to PR id: past
// the grace, a PR that is open and still at that head, of a repository with
// ci_dispatch, whose head has no start yet (the look's, or an earlier
// one's), gets one (startCI), unless a run of the workflow ran at the head
// already; while a run of the workflow on its branch is in progress, while
// the identity's GitHub budget is low, or while the runs cannot be read, it
// waits. Anything else drops the record: a head that moved on is no longer
// magnum's push.
func (e *Engine) ciAfterPush(ctx context.Context, id int64, now time.Time) {
	key := kvPRPushed(id)
	raw, ok := e.getKV(ctx, key)
	if !ok {
		return
	}
	done := func() {
		if _, err := e.st.DeleteKVIf(ctx, key, raw); err != nil { // a newer push rewrote it: that one waits
			e.warnUnlessStopped(ctx, err, "drop magnum's push", "pr", id)
		}
	}
	p, ok, err := readKVJSON[ciPush](ctx, e, key, "magnum's push")
	if err != nil || !ok {
		done()
		return
	}
	if now.Before(p.At.Add(ciPushGrace)) {
		return
	}
	pr, err := e.st.PRByID(ctx, id)
	if err != nil {
		done()
		return
	}
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		done()
		return
	}
	full := repo.FullName()
	workflow, w := e.cfg.CIDispatch(full), e.cfg.WatchFor(full)
	if workflow == "" || w == nil || pr.GHState != store.GHOpen || pr.HeadSHA != p.Head {
		done()
		return
	}
	if s := e.ciStartAt(ctx, pr); !s.At.IsZero() || s.Refused != "" { // the head was started (or refused) already
		done()
		return
	}
	identity := e.ciDispatchIdentity(full, *w)
	if _, low := e.githubLow(identity); low {
		return
	}
	acts := e.ciActions(identity) // ci_actions.go: the read and the start share one token
	defer acts.end()
	gh, err := acts.client(ctx)
	switch {
	case errors.Is(err, errNoGitHub):
		done()
		return
	}
	var runs []github.WorkflowRun
	if err == nil && gh != nil {
		l, ok := gh.(recentRunLister)
		if !ok {
			done()
			return
		}
		runs, err = l.RecentRuns(ctx, repo.Owner, repo.Name, workflow)
	}
	if err != nil {
		if e.changed("ci_recent_runs:"+full, err.Error()) {
			e.event(ctx, "warn", "repo:"+full, "poll.ci_runs_error", fmt.Sprintf("the newest runs of %s, before starting it after magnum's push: %v", workflow, err), nil)
		}
		return
	}
	e.changed("ci_recent_runs:"+full, "")
	subject, short := prSubject(repo, pr.Number), textx.ShortSHA(p.Head)
	if slices.ContainsFunc(runs, func(r github.WorkflowRun) bool { return r.HeadSHA == p.Head }) {
		e.event(ctx, "info", subject, "fix.push_ci_ran", fmt.Sprintf("magnum pushed %s, and %s ran at it already: magnum starts nothing", short, workflow),
			map[string]any{"head": p.Head, "workflow": workflow})
		done()
		return
	}
	branch := deref(pr.HeadRef)
	if branch != "" && slices.ContainsFunc(runs, func(r github.WorkflowRun) bool { return r.HeadBranch == branch && !r.Done() }) {
		return // a start would cancel it: the next tick looks again
	}
	lead := "magnum pushed " + short
	if _, ok := e.startCI(ctx, acts, repo, pr, workflow, ciWords{
		kind:    "fix.push_ci_dispatch",
		refused: func(why string) string { return fmt.Sprintf("%s; it does not start %s: %s", lead, workflow, why) },
		failed: func(identity, why string) string {
			return fmt.Sprintf("%s; its start of %s as %s failed: %s", lead, workflow, identity, why)
		},
		started: func(identity string) string {
			return fmt.Sprintf("%s; started %s on the PR's branch as %s", lead, workflow, identity)
		},
	}); ok {
		done()
	}
}

// startCI starts workflow, [[repo]] ci_dispatch, on the branch of pr, a
// PR of repo under w, and returns the start; ok is false when it did
// nothing and its caller asks again (the head's file list is not read yet,
// or the record could not be written). It refuses, once per head, a PR
// that changes its own workflows or actions or whose files GitHub did not
// list whole, and one whose branch it does not know. Else it records the
// start (kvPRCIStart) before it asks GitHub as [[repo]] ci_dispatch_as,
// else the watch's poll identity, else its identity, and records how it
// ended, with an event of words naming that identity: words.kind, its
// _failed (the status only: the message of GitHub's refusal can name the
// branch, the PR's text; a 403 says that the identity cannot start
// workflows, cannotStartWorkflows) or its _refused.
func (e *Engine) startCI(ctx context.Context, acts *ciActions, repo store.Repo, pr store.PR, workflow string, words ciWords) (ciStart, bool) {
	s := ciStart{Head: pr.HeadSHA, Workflow: workflow}
	files, listed, err := e.st.PRFilesOf(ctx, pr.ID)
	switch {
	case err != nil:
		e.warnUnlessStopped(ctx, err, "the PR's files before starting its CI", "pr", pr.ID)
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
	subject := prSubject(repo, pr.Number)
	identity := acts.identity
	data := map[string]any{"head": pr.HeadSHA, "workflow": workflow, "identity": identity}
	if s.Refused != "" {
		if !e.writeCIStart(ctx, pr, s) {
			return ciStart{}, false
		}
		e.event(ctx, "info", subject, words.kind+"_refused", words.refused(s.Refused), data)
		return s, true
	}
	s.At = e.now()
	if !e.writeCIStart(ctx, pr, s) {
		return ciStart{}, false
	}
	gh, err := acts.client(ctx) // after the record: a mint is no start
	if err == nil {
		if d, ok := gh.(workflowDispatcher); !ok {
			err = fmt.Errorf("%w of %s", errNoGitHub, identity)
		} else {
			err = d.DispatchWorkflow(ctx, repo.Owner, repo.Name, workflow, deref(pr.HeadRef))
		}
	}
	if err != nil {
		s.Failed = e.actionsFailed(identity, err)
	}
	e.writeCIStart(ctx, pr, s)
	if s.Failed != "" {
		data["failed"] = s.Failed
		e.event(ctx, "warn", subject, words.kind+"_failed", words.failed(identity, s.Failed), data)
		return s, true
	}
	e.event(ctx, "info", subject, words.kind, words.started(identity), data)
	return s, true
}
