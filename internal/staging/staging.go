// Package staging tells, for each open PR and each staging environment of
// its repository, whether the PR is live there (latest), pushed but not yet
// confirmed by a deployment (pushed), on staging at an older commit
// (outdated), absent (none), or unknown.
//
// It reads a treeless bare mirror of the repository (`git clone --bare
// --filter=tree:0`), GitHub's deployments of each environment and ci.yml's
// runs. A staging deploy is a two-parent merge, [staging tip, PR head],
// pushed to staging-<env>; Jenkins deploys a tip once its ci.yml passes and
// then records a deployment of the environment whose ref is that tip. So a
// PR is on staging at commit X when X is the newest PR commit that is an
// ancestor of a second parent of the branch's first-parent chain (within a
// window), and live when the newest successful deployment's history holds
// the merge that brought it in. Ancestry decides, never sha equality.
//
// The package only reads: it never pushes (that is the deploy's, later),
// and it runs git only in its own mirror directory. Refresh fetches and
// reads GitHub at most once a minute per repository across processes (a
// flock and a state file beside the mirror); Status is a pure lookup over
// the last Refresh.
package staging

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// Env is a staging environment: its name, which is also the GitHub
// deployment environment, and its branch, staging-<name>.
type Env struct{ Name, Branch string }

// State is where a PR stands on one environment.
type State string

// The states.
const (
	// StateLatest: the PR's head is on the branch, and the newest successful
	// deployment of the environment holds the merge that brought it in.
	StateLatest State = "latest"
	// StatePushed: the head is on the branch, but no deployment confirms it
	// yet; Detail says why.
	StatePushed State = "pushed"
	// StateOutdated: an older commit of the PR is on the branch, the head is
	// not; Behind counts the commits after it.
	StateOutdated State = "outdated"
	// StateNone: no commit of the PR is on the branch within the window.
	StateNone State = "none"
	// StateUnknown: the mirror is absent, a fetch failed, or there was no
	// Refresh; Why says which.
	StateUnknown State = "unknown"
)

// Detail is why a pushed PR is not confirmed yet.
type Detail string

// The details of StatePushed.
const (
	// DetailCIRunning: ci.yml runs on the merge (or has not started yet).
	DetailCIRunning Detail = "ci_running"
	// DetailCIFailedOurs: ci.yml failed, and a failing job names a path the
	// PR changes, or a job failed that passed on the branch's tip before the
	// merge, the merge alone in between ("new against" it: a flaky job reads
	// the same).
	DetailCIFailedOurs Detail = "ci_failed_ours"
	// DetailCIFailedNotOurs: ci.yml failed, but neither: no failing job names
	// a path the PR changes, and the same jobs failed on the tip before, or
	// the run covers several merges, or it cannot be told (Why says so).
	DetailCIFailedNotOurs Detail = "ci_failed_not_ours"
	// DetailWaitingJenkins: ci.yml passed, and the deploy has not recorded a
	// deployment yet, less than ConfirmWait after the push.
	DetailWaitingJenkins Detail = "waiting_jenkins"
	// DetailNotConfirmed: ci.yml passed but no deployment holds the merge
	// ConfirmWait after the push, or the deployments are not readable.
	DetailNotConfirmed Detail = "not_confirmed"
)

// The fixed numbers of the rules.
const (
	// Window and WindowAge bound the first-parent walk from a branch's tip:
	// the last Window first-parent commits, or WindowAge, whichever ends
	// first. A PR merged before it is none.
	Window    = 400
	WindowAge = 90 * 24 * time.Hour
	// ConfirmWait is how long after the push a passed ci.yml waits for a
	// deployment before the PR is "not confirmed".
	ConfirmWait = 45 * time.Minute
	// RefreshEvery is how often Refresh fetches and reads GitHub for a
	// repository at most, across every process that shares the mirror.
	RefreshEvery = time.Minute
	// StaleAfter is how old the last good Refresh may be before Status says
	// unknown, "stale": Refreshes that keep failing or being cut never leave
	// an old answer standing.
	StaleAfter = 10 * time.Minute
	// Workflow is the workflow file whose run on a merge says its CI.
	Workflow = "ci.yml"
)

// WhyNotReadable is EnvStatus.Why of a pushed PR whose environment's
// deployments GitHub refuses (a 403 or 404): not readable is never "none".
const WhyNotReadable = "deployments not readable (Deployments: read on the poll App)"

// WhyVolumeNotMounted is EnvStatus.Why while the mirror's volume (a
// /Volumes/<name> disk) is not mounted.
const WhyVolumeNotMounted = "the mirror's volume is not mounted"

// OpenPR is an open PR a Refresh fetches: its number and its base branch
// ("" = the remote's default branch). A stacked PR's base is the branch of
// the PR under it.
type OpenPR struct {
	Number int
	Base   string
}

// PR is the PR Status answers for.
type PR struct {
	Repo   string // owner/name
	Number int
	Head   string
	Base   string // the base ref, for example master ("" = the remote's default branch)
	// Commits are the PR's commits, oldest first. When nil, Status takes
	// the ones the last Refresh derived in the mirror, rev-list
	// Base..refs/pull/N/head, for a PR it had open on that Base.
	Commits []string
}

// EnvStatus is a PR's state on one environment.
type EnvStatus struct {
	Env         Env
	State       State
	Detail      Detail
	Tip         string    // staging-<env>'s tip
	DeployedTip string    // the newest successful deployment's ref, "" if not readable
	DeployedAt  time.Time // when that deployment was created
	InTip       string    // the PR commit found in the tip: the head, or for outdated the older one
	Behind      int       // outdated: the PR's commits after InTip that the tip lacks
	PushedAt    time.Time // the merge commit that brought InTip in: its committer time
	RunURL      string    // ci.yml's run on that merge commit (pushed, any detail), "" if none
	Why         string    // unknown, not_confirmed because deployments are not readable, or what could not be told
}

// Commit is a commit as the mirror lists it.
type Commit struct {
	SHA     string
	Parents []string
	At      time.Time // committer time (FirstParent only)
}

// Git is the git the package runs, always on the mirror directory dir,
// named to git (never found by git's search, which could walk up into a
// repository around it). NewGit is the real one.
type Git interface {
	// Base returns the branch the mirror's HEAD names: the remote's default
	// branch. dir must be a bare repository of its own (HEAD, objects/ and
	// refs/, and git calls it one) whose origin is repo's and treeless. Only
	// a definite answer that it is none (absent, laid out otherwise, git's
	// "not a git repository", not bare), with ctx not done, wraps
	// ErrNoMirror; anything else is a plain error, and the dir stays as it is.
	Base(ctx context.Context, dir, repo string) (string, error)
	// Clone makes dir, which must be absent, a treeless bare mirror of repo
	// (owner/name), its HEAD the remote's default branch.
	Clone(ctx context.Context, dir, repo string, env map[string]string) error
	// Fetch fetches refspecs (forced, explicit) from the mirror's origin
	// with the tree:0 filter. A refspec whose source the remote lacks fails
	// with a *MissingRefError.
	Fetch(ctx context.Context, dir string, env map[string]string, refspecs []string) error
	// Refs lists the mirror's refs under the prefixes (such as
	// "refs/heads/"), by full name.
	Refs(ctx context.Context, dir string, prefixes ...string) (map[string]string, error)
	// FirstParent lists at most limit commits of rev's first-parent chain,
	// newest first, with their parents and committer times.
	FirstParent(ctx context.Context, dir, rev string, limit int) ([]Commit, error)
	// Graph lists the commits reachable from include and from none of
	// exclude, with their parents, every commit before its parents (no
	// times). One process, whatever the number of revisions.
	Graph(ctx context.Context, dir string, include, exclude []string) ([]Commit, error)
	// ChangedPaths lists the paths head changes since its merge base with
	// base (`git diff --name-only base...head`); in a treeless mirror it
	// fetches the two trees it needs.
	ChangedPaths(ctx context.Context, dir string, env map[string]string, base, head string) ([]string, error)
}

// ErrNoMirror is what Git.Base's error wraps when dir is no mirror: Refresh
// moves such a dir aside and clones the mirror again.
var ErrNoMirror = errors.New("no mirror")

// MissingRefError is Git.Fetch's error when the remote has no Ref.
type MissingRefError struct{ Ref string }

func (e *MissingRefError) Error() string { return "the remote has no " + e.Ref }

// ErrNotReadable is what GitHub.DeploymentsOf's error wraps on a 403 or a
// 404: the token may not read deployments, which never means "none".
var ErrNotReadable = errors.New("not readable")

// Deployment is a GitHub deployment of an environment.
type Deployment struct {
	ID        int64
	SHA       string // the commit GitHub resolved the deployment's ref to
	Ref       string // the ref as created (the deploy records the tip's sha)
	CreatedAt time.Time
	Success   bool // it has a success status
}

// Deployments is one read of an environment's deployments.
type Deployments struct {
	// ETag is the answer's ETag, to send as If-None-Match next time.
	ETag string
	// NotModified: GitHub answered 304 to the etag sent; List is empty and
	// the last list stands.
	NotModified bool
	// List is the deployments, newest first (a page is enough: the newest
	// successful one is what counts).
	List []Deployment
}

// Run is a workflow run on a commit.
type Run struct {
	ID         int64
	URL        string
	Status     string // queued, in_progress, completed, ...
	Conclusion string // once completed: success, failure, cancelled, timed_out, skipped, ...
	CreatedAt  time.Time
	// Failed are the failing jobs of a completed run that did not succeed;
	// nil when they were not read.
	Failed []Job
}

// Job is a failing job of a run.
type Job struct {
	Name string
	// Paths are the repository paths its failure names (its check run's
	// annotations); empty = it names none, or they could not be read, so
	// whether the failure is a PR's cannot be told from it.
	Paths []string
}

// GitHub is what the package reads from GitHub as the poll identity.
type GitHub interface {
	// DeploymentsOf reads the environment's deployments, newest first, each
	// with whether it has a success status, sending etag as If-None-Match
	// ("" = none). A 403 or 404 is an error that wraps ErrNotReadable.
	DeploymentsOf(ctx context.Context, repo, environment, etag string) (Deployments, error)
	// RunsOf lists the runs of the workflow file (Workflow) on the commit
	// sha, newest first; the newest, when it failed, carries its failing
	// jobs.
	RunsOf(ctx context.Context, repo, workflow, sha string) ([]Run, error)
}

// Deps is what a Client runs on.
type Deps struct {
	Git    Git
	GitHub GitHub
	// Mirror is the repository's mirror dir; ok false = not configured or
	// its volume is absent (dir, when not "", says which: MirrorDir).
	Mirror func(repo string) (dir string, ok bool)
	// GitEnv is the environment of the fetch (the poll identity's git env:
	// its GH_CONFIG_DIR, the credential helper's login); gh is git's only
	// credential helper, so the operator's own credentials never answer
	// implicitly. Nil, or nil for a repository, and nothing is fetched.
	GitEnv func(repo string) map[string]string
	Now    func() time.Time
	Log    *slog.Logger
}

// Client answers staging status from what Refresh read. It is safe for
// concurrent use.
type Client struct {
	d Deps

	mu      sync.Mutex
	data    map[string]*state    // by repository, lower case
	cloning map[string]time.Time // the mirrors this client clones, by dir: since when
	// bg runs the background clones, on life (tests wait for them); Close
	// ends life.
	bg   sync.WaitGroup
	life context.Context
	end  context.CancelFunc
}

// New returns a Client on d.
func New(d Deps) *Client {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	life, end := context.WithCancel(context.Background())
	return &Client{d: d, data: map[string]*state{}, cloning: map[string]time.Time{}, life: life, end: end}
}

// Close stops the client's background clones and waits for them: their
// context ends, so execx ends each git's process group (SIGTERM, then
// SIGKILL), and the half clone is removed. A closed client starts no clone.
// The daemons call it when they stop.
func (c *Client) Close() {
	c.end()
	c.bg.Wait()
}

// key is the repository's key in Client.data.
func key(repo string) string { return strings.ToLower(repo) }

// get returns the repository's last data, nil before any Refresh.
func (c *Client) get(repo string) *state {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data[key(repo)]
}

// set makes st the repository's data.
func (c *Client) set(repo string, st *state) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key(repo)] = st
}

// short is a sha cut to 12 characters, for a Why.
func short(sha string) string { return sha[:min(12, len(sha))] }

// errLine is err's first line, redacted and cut to 200 bytes, for a Why or
// a log line.
func errLine(err error) string {
	s, _, _ := strings.Cut(execx.Redact(err.Error()), "\n")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
