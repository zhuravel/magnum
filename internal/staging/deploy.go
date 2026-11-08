package staging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/execx"
)

// A staging deploy, on the operator's confirmed word: Plan shows what it
// would do on each env, Deploy starts one effect per env, and Step advances
// every live effect one step a tick. An effect is a journal beside the
// mirror (<mirror_dir>/<owner>/<repo>.deploys/<id>.json), the source of
// truth: each step writes begin, does its work and writes ok or fail, and a
// restart resumes at the recorded step, whose guard runs again before it
// acts. The steps:
//
//  1. claim: the env's flock (<env>.lock in the deploys dir), held across
//     processes for the effect's life: one effect per repository and env
//     runs at a time; the others wait, queued.
//  2. plan: the tip and the PR's head again. A head that moved fails it; a
//     head the tip holds already is done, "already there".
//  3. merge: `git merge-tree --write-tree <tip> <head>`. When the tip moved
//     since the operator's plan and the merge adds migrations, or changes
//     .github/workflows paths, the plan did not show, it stops: needs you.
//     Clean, `git commit-tree` makes [tip, head] with the bot as author and
//     committer (CommitIdentity); a conflict goes to the Resolver (at most 2
//     tries), whose merge is fetched into the mirror.
//  4. verify: the merge is a commit whose parents are exactly [tip, head]
//     and whose tree is a fresh merge-tree's of them, or differs from it
//     only in its conflicted paths; the head is still the plan's; a tip that
//     moved on merges again (at most 2 times, push's included).
//  5. push: PushEnv, then verify's checks again from what the remote and
//     the mirror say right then (never the journal alone), the allowlist
//     again, and the plain push `git push origin
//     <sha>:refs/heads/staging-<env>`, never forced. A rejection because the
//     tip moved merges again; a branch that already holds the merge is not
//     pushed again.
//  6. watch: ci.yml's run on the merge; a failure fails the effect.
//  7. confirm: a deployment of the env whose history holds the merge ends
//     it done; none ConfirmAfterCI after CI passed ends it done, "pushed,
//     not confirmed".
//
// A failure that comes back maxFails times in a row at a step ends the
// effect (failed before the push, "pushed, not confirmed" after it), and a
// resolver's job or ci.yml that runs too long ends its wait: no effect holds
// its env for ever. A cancel stops an effect at once and it never pushes
// after; a cancel after the push stops only the watching: a deploy never
// reverts a push.
//
// The resolver fetch runs upload-pack on the resolver's repository with its
// own options (uploadPack) and protocol v2: git's local transport drops the
// fetch's -c options for that child, and v2 runs no v0-only server path
// such as core.alternateRefsCommand.

// EffectID names a deploy effect: <env>-<pr>-<time>-<random>, lower-case
// letters, digits and dashes.
type EffectID string

// The steps of an effect, in order (Effect.Step).
const (
	StepClaim   = "claim"
	StepPlan    = "plan"
	StepMerge   = "merge"
	StepVerify  = "verify"
	StepPush    = "push"
	StepWatch   = "watch"
	StepConfirm = "confirm"
)

// The phases of a step, as Deps.Event gets them.
const (
	PhaseBegin = "begin"
	PhaseOK    = "ok"
	PhaseFail  = "fail"
)

// The states of an effect (Effect.State). Queued and running are live; the
// others end it.
const (
	// EffectQueued: another effect runs on the env; this one waits.
	EffectQueued = "queued"
	// EffectRunning: it holds the env and advances a step at each Step.
	EffectRunning = "running"
	// EffectNeedsYou: it stopped before anything was pushed, for the
	// operator: the tip moved and the merge now brings migrations or
	// workflow changes the plan did not show. A new plan and Deploy of the
	// same PR and head resumes it.
	EffectNeedsYou = "needs you"
	// EffectFailed: it ended on a failure, Why says which; before the push
	// step nothing was pushed.
	EffectFailed = "failed"
	// EffectCancelled: the operator cancelled it; a push before the cancel
	// stands.
	EffectCancelled = "cancelled"
	// EffectDone: pushed, CI passed, and a deployment holds the merge, or
	// Why starts with NotConfirmed.
	EffectDone = "done"
)

// NotConfirmed starts the Why of an effect done without a deployment that
// holds its merge: none ConfirmAfterCI after ci.yml passed, or the
// deployments are not readable.
const NotConfirmed = "pushed, not confirmed"

// Who asks for a deploy (Deploy's by), recorded in the journal and every
// Event; never inferred.
const (
	ByCLI     = "cli"
	ByLiaison = "liaison"
	ByFactory = "factory"
)

// The states of a resolver's job (ResolveResult.State).
const (
	ResolveRunning = "running"
	ResolveDone    = "done"
	ResolveFailed  = "failed"
)

// The bounds of a deploy.
const (
	// maxRemerges is how many times an effect merges again after the tip
	// moved, verify's and push's together; one more move fails it.
	maxRemerges = 2
	// maxResolves is how many resolver jobs an effect starts at most.
	maxResolves = 2
	// lockWait is how long a step waits for the mirror's flock (a Refresh's
	// fetch) before it tries again at the next Step.
	lockWait = 2 * time.Minute
	// keepEnded is how long an ended effect's journal is kept.
	keepEnded = 30 * 24 * time.Hour
	// maxListed is how many paths a Why names; the rest are counted.
	maxListed = 10
	// maxIdent bounds the commit identity's name and email, in bytes.
	maxIdent = 200
	// maxWhy bounds a Why taken from another component (the resolver).
	maxWhy = 200
	// maxFails is how many times in a row a step may fail (a fetch, a git
	// command, a read of GitHub or of the resolver, a push) before the
	// effect ends.
	maxFails = 3
	// resolveWait is how long a resolver's job may run before the effect
	// fails, the job cancelled.
	resolveWait = 2 * time.Hour
	// watchFor is how long after the push watch waits for ci.yml to end
	// before it goes on to confirm.
	watchFor = 2 * time.Hour
	// maxStored is how many conflicted paths a journal keeps; maxPlanPaths
	// how many migrations a plan may show (the journal keeps them all).
	maxStored    = 100
	maxPlanPaths = 1000
	// envNameMax bounds an env's name so its effect ids stay ids (idRe).
	envNameMax = 32
)

// DeployPlan is what a deploy of PR would do on each env, for the operator
// to confirm before Deploy.
type DeployPlan struct {
	PR   PR
	Envs []EnvPlan
	At   time.Time
}

// EnvPlan is a deploy's plan on one env: the branch's tip, whether the PR's
// head merges into it cleanly (else the conflicted paths), the migrations
// the merge adds (db/migrate files it adds against the tip), and whether it
// changes any .github/workflows path. Why says why the env has no plan (no
// such branch, the merge could not be read, the head is on the branch
// already: "already there"); Deploy refuses such an env. The paths are as
// git lists them, not made safe for a terminal.
type EnvPlan struct {
	Env        Env
	Tip        string
	Clean      bool
	Conflicts  []string
	Migrations []string
	Workflows  bool
	Why        string
}

// Effect is a deploy effect as the screens see it: its step and state, why,
// the merge's sha once made, ci.yml's run, and Try, the merge's try (1, and
// one more after each move of the tip).
type Effect struct {
	ID               EffectID
	Repo             string
	Env              Env
	PR               PR
	By               string
	Step, State, Why string
	Merge, RunURL    string
	Try              int
	Started, Updated time.Time
}

// Resolver resolves a deploy's merge conflict in a repository of its own.
// Result on a job it does not know answers failed.
type Resolver interface {
	// Start starts a job and returns at once.
	Start(ctx context.Context, r ResolveRequest) (job string, err error)
	// Result is the job's state: running, done (MergeSHA in Dir) or failed
	// (Why).
	Result(ctx context.Context, job string) (ResolveResult, error)
	// Cancel stops the job: the deploy stopped, or merges again.
	Cancel(ctx context.Context, job string) error
}

// ResolveRequest asks for the merge of Head into Target (staging-<env>) at
// TargetTip, as [TargetTip, Head]: Ref is owner/repo#N, Effect the effect's
// id, Try 1 or 2.
type ResolveRequest struct {
	Repo, Ref, Target, TargetTip, Head, Effect string
	Try                                        int
}

// ResolveResult is a job's state. Done: MergeSHA is the merge, a commit in
// the local repository at Dir (an absolute path, its git dir or its work
// tree), which Deploy fetches into the mirror; the resolver keeps the
// commit there until the effect is past verify or cancelled. Why also
// carries what the resolver checked.
type ResolveResult struct{ State, MergeSHA, Why, Dir string }

// PushEnv is called right before each push of mergeSHA to target
// (staging-<env>): it refuses by its own rules (a merge's .github/workflows
// paths) or returns the push's env, the bot's credentials.
type PushEnv func(ctx context.Context, repo, mergeSHA, target string) (map[string]string, error)

// errNotDeployable is what Plan and Deploy answer when the Git cannot
// deploy (no deployGit).
var errNotDeployable = errors.New("staging: this git cannot deploy")

// checkPR refuses a PR a deploy cannot name exactly: owner/name, a number
// and a full head sha.
func checkPR(pr PR) error {
	owner, name, ok := strings.Cut(pr.Repo, "/")
	switch {
	case !ok || !validPart(owner) || !validPart(name):
		return fmt.Errorf("staging: %q is not owner/name", pr.Repo)
	case pr.Number <= 0:
		return fmt.Errorf("staging: PR number %d", pr.Number)
	case !isOID(pr.Head):
		return fmt.Errorf("staging: PR #%d's head %q is no full sha", pr.Number, pr.Head)
	}
	return nil
}

// deployPR is pr as a deploy keeps it: no commit list.
func deployPR(pr PR) PR {
	pr.Commits = nil
	return pr
}

// deployDeps is the repository's mirror dir and the deploy's git.
func (c *Client) deployDeps(repo string) (string, deployGit, error) {
	g, ok := c.d.Git.(deployGit)
	if !ok {
		return "", nil, errNotDeployable
	}
	if c.d.GitHub == nil {
		return "", nil, errors.New("staging: no GitHub configured")
	}
	dir, ok := c.d.Mirror(repo)
	if !ok {
		if dir != "" {
			return "", nil, errors.New("staging " + repo + ": " + WhyVolumeNotMounted)
		}
		return "", nil, errors.New("staging " + repo + ": no staging mirror is configured")
	}
	return dir, g, nil
}

// gitEnv is the fetch's env of repo (GitEnv), or why there is none.
func (c *Client) gitEnv(repo string) (map[string]string, error) {
	var env map[string]string
	if c.d.GitEnv != nil {
		env = c.d.GitEnv(repo)
	}
	if env == nil {
		return nil, errors.New("no git env for the fetch: the poll identity's credentials are not configured")
	}
	return env, nil
}

// underMirror runs fn under the mirror's flock, waiting for it at most
// lockWait. Only git runs under it: never a call to GitHub, the resolver,
// PushEnv or CommitIdentity, which would hold up every Refresh.
func underMirror(ctx context.Context, dir string, fn func() error) error {
	lctx, cancel := context.WithTimeout(ctx, lockWait)
	unlock, err := lockMirror(lctx, dir, nil)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("the mirror's lock: %w", err)
	}
	defer unlock()
	return fn()
}

// envNameRe is a staging env's name as an effect id carries it.
var envNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,` + strconv.Itoa(envNameMax-1) + `}$`)

// allowed checks env against the allowlist: one of Stagings(repo) by name
// and branch, the name a slug of at most envNameMax lower-case letters,
// digits and dashes (not led by a dash: an effect id carries it), the
// branch staging-<name>, and never master, main, the remote's default
// branch (base, the mirror's HEAD) or production* (the name or the branch,
// case ignored).
func (c *Client) allowed(repo string, env Env, base string) error {
	refused := func(why string) error {
		return fmt.Errorf("staging: %s (%s) is refused: %s", oneLine(env.Name), oneLine(env.Branch), why)
	}
	switch b := strings.ToLower(env.Branch); {
	case !envNameRe.MatchString(env.Name):
		return refused(fmt.Sprintf("the name is not a slug of at most %d lower-case letters, digits and dashes", envNameMax))
	case env.Branch != "staging-"+env.Name || !stagingBranchRe.MatchString(env.Branch):
		return refused("the branch is not staging-<name>")
	case b == "master" || b == "main" || base == "" || strings.EqualFold(env.Branch, base):
		return refused("the remote's default branch, master and main are never staging")
	case strings.HasPrefix(strings.ToLower(env.Name), "production") || strings.HasPrefix(b, "production"):
		return refused("production is never staging")
	case c.d.Stagings == nil:
		return refused("no staging environments are configured")
	case !slices.Contains(c.d.Stagings(repo), env):
		return refused("not one of the repository's [[factory.repo.staging]] entries")
	}
	return nil
}

// tipsOf fetches the branches and, for number > 0, the PR's pull ref into
// the mirror, and returns the branches' tips (a branch the remote lacks is
// absent) and the PR's head. The caller holds the mirror's flock.
func (c *Client) tipsOf(ctx context.Context, dir string, env map[string]string, branches []string, number int) (map[string]string, string, error) {
	specs := map[string]string{}
	for _, b := range branches {
		specs["refs/heads/"+b] = "+refs/heads/" + b + ":refs/heads/" + b
	}
	if number > 0 {
		specs[pullRef(number)] = "+" + pullRef(number) + ":" + pullRef(number)
	}
	for range len(branches) + 1 {
		if len(specs) == 0 {
			break
		}
		err := c.d.Git.Fetch(ctx, dir, env, slices.Sorted(maps.Values(specs)))
		miss, isMiss := errors.AsType[*MissingRefError](err)
		switch {
		case isMiss && miss.Ref == pullRef(number):
			return nil, "", fmt.Errorf("PR #%d has no pull ref on the remote (%s)", number, miss.Ref)
		case isMiss && specs[miss.Ref] != "":
			delete(specs, miss.Ref) // a branch the remote lacks: its tip is absent
			continue
		case err != nil:
			return nil, "", fmt.Errorf("the fetch failed: %s", gitReason(err))
		}
		break
	}
	names := slices.Sorted(maps.Keys(specs))
	if len(names) == 0 {
		return map[string]string{}, "", nil
	}
	refs, err := c.d.Git.Refs(ctx, dir, names...)
	if err != nil {
		return nil, "", fmt.Errorf("the mirror's refs could not be read: %s", gitReason(err))
	}
	tips := map[string]string{}
	for _, b := range branches {
		if sha := refs["refs/heads/"+b]; isOID(sha) {
			tips[b] = sha
		}
	}
	head := ""
	if number > 0 {
		head = refs[pullRef(number)]
	}
	return tips, head, nil
}

// mergeRead is what a merge of head into tip would be: merge-tree's tree,
// its conflicted paths, the migrations it adds and the workflow paths it
// changes.
type mergeRead struct {
	tree       string
	conflicts  []string
	migrations []string
	workflows  []string
}

// isMigration reports whether path is a migration: a file under a
// db/migrate directory (the root's, or an engine's).
func isMigration(path string) bool {
	return strings.HasPrefix(path, "db/migrate/") || strings.Contains(path, "/db/migrate/")
}

// isWorkflow reports whether path is under .github/workflows.
func isWorkflow(path string) bool { return strings.HasPrefix(path, ".github/workflows/") }

// readMerge merges head into tip in the mirror (merge-tree) and reads what
// the merge changes against tip. The caller holds the mirror's flock.
func readMerge(ctx context.Context, g deployGit, dir string, env map[string]string, tip, head string) (mergeRead, error) {
	tree, conflicts, err := g.mergeTree(ctx, dir, env, tip, head)
	if err != nil {
		return mergeRead{}, fmt.Errorf("merge-tree: %s", gitReason(err))
	}
	changes, err := g.treeDiff(ctx, dir, env, tip, tree)
	if err != nil {
		return mergeRead{}, fmt.Errorf("the merge's changes: %s", gitReason(err))
	}
	m := mergeRead{tree: tree, conflicts: conflicts}
	for _, ch := range changes {
		if ch.Status == 'A' && isMigration(ch.Path) {
			m.migrations = append(m.migrations, ch.Path)
		}
		if isWorkflow(ch.Path) {
			m.workflows = append(m.workflows, ch.Path)
		}
	}
	return m, nil
}

// alreadyThere is the Why of a head the branch holds already.
func alreadyThere(number int, branch string) string {
	return fmt.Sprintf("already there: %s holds PR #%d's head", branch, number)
}

// Plan reads, never writes a journal or pushes: for each env, the current
// tip, whether the PR's head merges cleanly into it (merge-tree in the
// mirror), the migrations the merge adds and whether it changes any
// .github/workflows path. It fetches the env branches and the PR's pull ref
// into the mirror, under its flock. Every env must pass the allowlist, and
// the remote's pull ref must be the PR's head; an env whose branch is absent,
// whose merge cannot be read or whose tip holds the head already has Why.
// The caller shows the plan to the operator before Deploy.
func (c *Client) Plan(ctx context.Context, pr PR, envs []Env) (DeployPlan, error) {
	if err := checkPR(pr); err != nil {
		return DeployPlan{}, err
	}
	if len(envs) == 0 {
		return DeployPlan{}, errors.New("staging: a plan needs an env")
	}
	dir, g, err := c.deployDeps(pr.Repo)
	if err != nil {
		return DeployPlan{}, err
	}
	env, err := c.gitEnv(pr.Repo)
	if err != nil {
		return DeployPlan{}, fmt.Errorf("staging %s: %w", pr.Repo, err)
	}
	plan := DeployPlan{PR: deployPR(pr), At: c.d.Now()}
	err = underMirror(ctx, dir, func() error {
		base, err := c.d.Git.Base(ctx, dir, pr.Repo)
		if err != nil {
			return fmt.Errorf("the mirror: %w", err)
		}
		branches := make([]string, 0, len(envs))
		for _, e := range envs {
			if err := c.allowed(pr.Repo, e, base); err != nil {
				return err
			}
			branches = append(branches, e.Branch)
		}
		tips, head, err := c.tipsOf(ctx, dir, env, branches, pr.Number)
		if err != nil {
			return err
		}
		if head != pr.Head {
			return fmt.Errorf("PR #%d's head on the remote is %s, not %s: plan with its head", pr.Number, short(head), short(pr.Head))
		}
		for _, e := range envs {
			plan.Envs = append(plan.Envs, planEnv(ctx, g, dir, env, e, tips[e.Branch], pr))
		}
		return nil
	})
	if err != nil {
		return DeployPlan{}, fmt.Errorf("staging %s: plan: %w", pr.Repo, err)
	}
	return plan, nil
}

// planEnv is the plan of pr on env at tip.
func planEnv(ctx context.Context, g deployGit, dir string, env map[string]string, e Env, tip string, pr PR) EnvPlan {
	ep := EnvPlan{Env: e, Tip: tip}
	if tip == "" {
		ep.Why = "the remote has no branch " + e.Branch
		return ep
	}
	there, err := g.isAncestor(ctx, dir, pr.Head, tip)
	switch {
	case err != nil:
		ep.Why = "the branch's history could not be read: " + gitReason(err)
		return ep
	case there:
		ep.Why = alreadyThere(pr.Number, e.Branch)
		return ep
	}
	m, err := readMerge(ctx, g, dir, env, tip, pr.Head)
	if err != nil {
		ep.Why = "the merge could not be read: " + err.Error()
		return ep
	}
	ep.Clean, ep.Conflicts, ep.Migrations, ep.Workflows = len(m.conflicts) == 0, m.conflicts, m.migrations, len(m.workflows) > 0
	return ep
}

// Deploy starts, on the operator's confirmed plan, one effect per env, or
// resumes the one of the same PR and head that needs you there; by is who
// asked (ByCLI, ByLiaison, ByFactory). It starts all or none: every env is
// checked first (on the allowlist, with a plan: no Why, a tip, at most
// maxPlanPaths migrations; no live effect of the same PR at the same head),
// and every journal is written, under the deploys dir's flock, before any
// claim; a write that fails takes the others back. Each effect is written
// queued and claims its env when no other effect holds or waits for it; it
// returns at once, after the claim: Step does the rest.
func (c *Client) Deploy(ctx context.Context, plan DeployPlan, by string) ([]EffectID, error) {
	pr := deployPR(plan.PR)
	if err := checkPR(pr); err != nil {
		return nil, err
	}
	switch {
	case by != ByCLI && by != ByLiaison && by != ByFactory:
		return nil, fmt.Errorf("staging: a deploy asked by %q: by is cli, liaison or factory", by)
	case len(plan.Envs) == 0:
		return nil, errors.New("staging: the plan has no env")
	}
	dir, _, err := c.deployDeps(pr.Repo)
	if err != nil {
		return nil, err
	}
	var base string
	if err := underMirror(ctx, dir, func() error {
		base, err = c.d.Git.Base(ctx, dir, pr.Repo)
		return err
	}); err != nil {
		return nil, fmt.Errorf("staging %s: the mirror: %w", pr.Repo, err)
	}
	root, err := openDeploys(dir, true)
	if err != nil {
		return nil, fmt.Errorf("staging %s: the deploys' directory: %w", pr.Repo, err)
	}
	defer root.Close()
	ids, err := c.writeEffects(ctx, root, plan, pr, by, base)
	if err != nil {
		return nil, err
	}
	for _, ep := range plan.Envs {
		if _, _, err := c.claim(ctx, pr.Repo, dir, root, ep.Env.Name); err != nil {
			c.d.Log.Warn("staging: a deploy's env not claimed", "repo", pr.Repo, "env", ep.Env.Name, "err", errLine(err))
		}
	}
	return ids, nil
}

// writeEffects checks the plan's envs and writes their journals, queued,
// under the deploys dir's flock: all or none.
func (c *Client) writeEffects(ctx context.Context, root *os.Root, plan DeployPlan, pr PR, by, base string) ([]EffectID, error) {
	f, _, err := flockIn(ctx, root, dirLockName, true)
	if err != nil {
		return nil, err
	}
	defer unflock(f)
	all := readJournals(root)
	seen := map[string]bool{}
	resume := map[string]*journal{}
	for _, ep := range plan.Envs {
		e := ep.Env
		switch {
		case seen[e.Name]:
			return nil, fmt.Errorf("staging: the plan names %s twice", e.Name)
		case ep.Why != "":
			return nil, fmt.Errorf("staging: %s has no plan: %s", oneLine(e.Name), oneLine(ep.Why))
		case !isOID(ep.Tip):
			return nil, fmt.Errorf("staging: the plan for %s names no tip", oneLine(e.Name))
		case len(ep.Migrations) > maxPlanPaths:
			return nil, fmt.Errorf("staging: the plan for %s shows %d migrations, over %d", e.Name, len(ep.Migrations), maxPlanPaths)
		}
		seen[e.Name] = true
		if err := c.allowed(pr.Repo, e, base); err != nil {
			return nil, err
		}
		for _, j := range all {
			if j.Env.Name != e.Name || j.PR.Number != pr.Number || j.PR.Head != pr.Head || !strings.EqualFold(j.Repo, pr.Repo) {
				continue
			}
			if j.live() {
				return nil, fmt.Errorf("staging: PR #%d at %s is already deploying to %s (%s, %s)", pr.Number, short(pr.Head), e.Name, j.ID, j.State)
			}
			if j.State == EffectNeedsYou {
				resume[e.Name] = j
			}
		}
	}
	now := c.d.Now()
	type written struct{ j, old *journal }
	var done []written
	undo := func() {
		for _, w := range slices.Backward(done) {
			if w.old != nil {
				_ = c.writeLocked(ctx, root, w.old)
			} else {
				_ = root.Remove(journalName(w.j.ID))
				_ = root.Remove(effectLockName(w.j.ID))
			}
		}
	}
	for _, ep := range plan.Envs {
		j := &journal{V: journalVersion, ID: newID(ep.Env.Name, pr.Number, now), Repo: pr.Repo, Env: ep.Env, PR: pr, Started: now}
		old := resume[ep.Env.Name]
		if old != nil {
			// A fresh start on the same id: the operator's new word.
			kept := *old
			j = &journal{V: journalVersion, ID: old.ID, Repo: old.Repo, Env: old.Env, PR: pr, Started: old.Started}
			old = &kept
		}
		if !validID(j.ID) {
			undo()
			return nil, fmt.Errorf("staging: the effect id %q is no id", j.ID)
		}
		j.By, j.Step, j.State, j.Why = by, StepClaim, EffectQueued, "waiting for the env"
		j.PlanTip, j.PlanMigrations, j.PlanWorkflows, j.PlanAt = ep.Tip, slices.Clone(ep.Migrations), ep.Workflows, plan.At
		j.Updated = now
		err := c.writeLocked(ctx, root, j)
		if err == nil && c.hook != nil {
			err = c.hook(j.ID, "deploy", "written")
		}
		if err != nil {
			if old == nil {
				done = append(done, written{j: j})
			}
			undo()
			return nil, fmt.Errorf("staging %s: the journal of %s: %w", pr.Repo, j.ID, err)
		}
		done = append(done, written{j: j, old: old})
	}
	ids := make([]EffectID, len(done))
	for i, w := range done {
		ids[i] = w.j.ID
	}
	return ids, nil
}

// writeLocked writes j under its effect flock.
func (c *Client) writeLocked(ctx context.Context, root *os.Root, j *journal) error {
	f, _, err := flockIn(ctx, root, effectLockName(j.ID), true)
	if err != nil {
		return err
	}
	defer unflock(f)
	return writeJournal(root, j)
}

// claim takes env's flock for this client when no one holds it, and claims
// the env's next live effect: the oldest running one (its runner stopped),
// else the oldest queued; it reads the journals under the deploys dir's
// flock. It reports the effect it holds the env for. A flock this client
// holds for an effect no longer running goes first.
func (c *Client) claim(ctx context.Context, repo, dir string, root *os.Root, env string) (EffectID, bool, error) {
	df, _, err := flockIn(ctx, root, dirLockName, true)
	if err != nil {
		return "", false, err
	}
	defer unflock(df)
	all := readJournals(root)
	if id, held := c.heldFor(dir, env); held {
		if slices.ContainsFunc(all, func(j *journal) bool { return j.ID == id && j.State == EffectRunning }) {
			return id, true, nil
		}
		c.release(dir, env)
	}
	var next *journal
	for _, j := range all {
		if j.Env.Name != env || !j.live() || !strings.EqualFold(j.Repo, repo) {
			continue
		}
		if next == nil || j.State == EffectRunning && next.State != EffectRunning {
			next = j
		}
	}
	if next == nil {
		return "", false, nil
	}
	f, ok, err := flockIn(ctx, root, envLockName(env), false)
	if err != nil || !ok {
		return "", false, err
	}
	held := false
	defer func() {
		if !held {
			unflock(f)
		}
	}()
	ef, _, err := flockIn(ctx, root, effectLockName(next.ID), true)
	if err != nil {
		return "", false, err
	}
	defer unflock(ef)
	j, err := readJournal(root, next.ID)
	if err != nil || !j.live() {
		return "", false, err
	}
	r := &stepRun{c: c, repo: repo, dir: dir, root: root, j: j}
	r.redeliver() // a crash may have cut them
	if j.State == EffectQueued {
		if err := r.begin(StepClaim, false, "claiming "+env); err != nil {
			return "", false, err
		}
		j.State = EffectRunning
		if err := r.ok(StepPlan, "claimed "+env); err != nil {
			return "", false, err
		}
	} else {
		c.d.Log.Info("staging: resuming a deploy whose runner stopped", "repo", repo, "effect", j.ID, "step", j.Step)
	}
	c.mu.Lock()
	c.envLocks[envKey(dir, env)] = &envLock{f: f, id: j.ID}
	c.mu.Unlock()
	held = true
	return j.ID, true, nil
}

// probeRepo is the repository Client.root names to Deps.Mirror to learn
// mirror_dir: MirrorDir puts every repository's mirror at
// <mirror_dir>/<owner>/<name>.git.
const probeRepo = "magnum/probe"

// root is mirror_dir, from Deps.Mirror; "" when there is none or its
// volume is not mounted.
func (c *Client) root() string {
	if c.d.Mirror == nil {
		return ""
	}
	dir, ok := c.d.Mirror(probeRepo)
	if !ok || dir == "" {
		return ""
	}
	return filepath.Dir(filepath.Dir(dir))
}

// deployRepos lists the repositories that have a deploys dir under
// mirror_dir, from their journals: each journal's repository must name, by
// Deps.Mirror, the deploys dir it was found in (a journal elsewhere is passed
// over). A fresh client finds every effect so, whoever started it.
func (c *Client) deployRepos() []string {
	root := c.root()
	if root == "" {
		return nil
	}
	seen := map[string]string{}
	for _, dd := range deployDirs(root) {
		r, err := os.OpenRoot(dd)
		if err != nil {
			continue
		}
		for _, j := range readJournals(r) {
			if dir, ok := c.d.Mirror(j.Repo); ok && deploysDir(dir) == dd {
				seen[key(j.Repo)] = j.Repo
			}
		}
		r.Close()
	}
	return slices.Sorted(maps.Values(seen))
}

// Step advances every live effect under mirror_dir one step: per env, the
// effect this client holds the env for, or the next one it can claim (an
// env another process holds is left to it). It finds the effects itself
// (deployRepos), so a deploy one daemon started resumes in the other with
// no wiring. It never blocks on an agent or on CI, only on git in the
// mirror (a fetch, a merge, a push); a Step while another runs on the same
// client returns at once. A daemon calls it each tick.
func (c *Client) Step(ctx context.Context) error {
	if !c.stepping.TryLock() {
		return nil
	}
	defer c.stepping.Unlock()
	var errs []error
	for _, repo := range c.deployRepos() {
		if err := c.stepRepo(ctx, repo); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stepRepo advances repo's effects, an env at a time, and drops the
// journals of effects that ended over keepEnded ago.
func (c *Client) stepRepo(ctx context.Context, repo string) error {
	dir, ok := c.d.Mirror(repo)
	if !ok {
		return nil
	}
	root, err := openDeploys(dir, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("staging %s: the deploys' directory: %w", repo, err)
	}
	defer root.Close()
	now := c.d.Now()
	envs := map[string]bool{}
	for _, j := range readJournals(root) {
		switch {
		case !strings.EqualFold(j.Repo, repo):
		case j.live():
			envs[j.Env.Name] = true
		case now.Sub(j.Updated) > keepEnded:
			_ = root.Remove(journalName(j.ID))
			_ = root.Remove(effectLockName(j.ID))
		}
	}
	c.mu.Lock()
	for k := range c.envLocks {
		if d, env, _ := strings.Cut(k, "\x00"); d == deploysDir(dir) {
			envs[env] = true
		}
	}
	c.mu.Unlock()
	var errs []error
	for _, env := range slices.Sorted(maps.Keys(envs)) {
		if err := c.stepEnv(ctx, repo, dir, root, env); err != nil {
			errs = append(errs, fmt.Errorf("staging %s %s: %w", repo, env, err))
		}
	}
	return errors.Join(errs...)
}

// stepEnv advances env's effect one step: the one this client holds the
// env for, else the next it claims; the flock goes once it ended.
func (c *Client) stepEnv(ctx context.Context, repo, dir string, root *os.Root, env string) error {
	id, ok, err := c.claim(ctx, repo, dir, root, env)
	if err != nil || !ok {
		return err
	}
	live, err := c.advance(ctx, repo, dir, root, id)
	if !live {
		c.release(dir, env)
	}
	return err
}

// advance runs effect id's step once, under its flock (each step takes the
// mirror's around its git alone); it reports whether the effect is still
// live. An effect whose env or PR is not its id's fails at once.
func (c *Client) advance(ctx context.Context, repo, dir string, root *os.Root, id EffectID) (bool, error) {
	f, _, err := flockIn(ctx, root, effectLockName(id), true)
	if err != nil {
		return true, err
	}
	defer unflock(f)
	j, err := readJournal(root, id)
	if err != nil {
		return false, err
	}
	if j.State != EffectRunning {
		return j.live(), nil
	}
	_, g, err := c.deployDeps(repo)
	if err != nil {
		return true, err
	}
	r := &stepRun{c: c, repo: repo, dir: dir, root: root, g: g, j: j}
	r.redeliver()
	if env, n, ok := idOf(j.ID); !ok || env != j.Env.Name || n != j.PR.Number {
		err = r.end(ctx, EffectFailed, fmt.Sprintf("the journal's env %s or PR #%d is not its id's: nothing was pushed", oneLine(j.Env.Name), j.PR.Number))
	} else {
		err = r.run(ctx)
	}
	if err != nil && j.live() && !errors.Is(err, errHook) && ctx.Err() == nil {
		// The mirror's lock, or a write: the step fails this time.
		err = errors.Join(err, r.retry(ctx, j.Step, "the step could not run: "+errLine(err)))
	}
	if !j.live() && j.Held && !errors.Is(err, errHook) {
		r.dropRefs(ctx)
	}
	return j.live(), err
}

// Effects lists repo's effects, the newest first, from their journals:
// those of every process that deploys from the same mirror_dir.
func (c *Client) Effects(repo string) []Effect {
	dir, ok := c.d.Mirror(repo)
	if !ok {
		return nil
	}
	root, err := openDeploys(dir, false)
	if err != nil {
		return nil
	}
	defer root.Close()
	var out []Effect
	for _, j := range slices.Backward(readJournals(root)) {
		if strings.EqualFold(j.Repo, repo) {
			out = append(out, j.effect())
		}
	}
	return out
}

// Cancel stops effect id at once: a queued or running effect ends
// cancelled, its resolver job is cancelled, and it never pushes after (a
// step in flight finishes first, under the effect's flock). A cancel after
// the push stops only the watching: the push stands, a deploy never
// reverts. An effect that already ended stays as it is. It finds the effect
// under mirror_dir, whoever started it.
func (c *Client) Cancel(ctx context.Context, id EffectID) error {
	if !validID(id) {
		return fmt.Errorf("staging: %q is no effect id", id)
	}
	for _, repo := range c.deployRepos() {
		dir, ok := c.d.Mirror(repo)
		if !ok {
			continue
		}
		root, err := openDeploys(dir, false)
		if err != nil {
			continue
		}
		found, err := c.cancelIn(ctx, repo, dir, root, id)
		root.Close()
		if found {
			return err
		}
	}
	return fmt.Errorf("staging: no deploy effect %s", id)
}

// cancelIn cancels id when its journal is in root.
func (c *Client) cancelIn(ctx context.Context, repo, dir string, root *os.Root, id EffectID) (bool, error) {
	if _, err := root.Lstat(journalName(id)); err != nil {
		return false, nil
	}
	f, _, err := flockIn(ctx, root, effectLockName(id), true)
	if err != nil {
		return true, err
	}
	defer unflock(f)
	j, err := readJournal(root, id)
	if err != nil {
		return true, err
	}
	if !j.live() {
		return true, nil
	}
	why := "cancelled: nothing was pushed"
	if !j.PushedAt.IsZero() {
		why = "cancelled after the push: the watching stops, the push stands (a deploy never reverts)"
	}
	_, g, _ := c.deployDeps(repo)
	r := &stepRun{c: c, repo: repo, dir: dir, root: root, g: g, j: j}
	r.redeliver()
	err = r.end(ctx, EffectCancelled, why)
	if err == nil && j.Held {
		r.dropRefs(ctx) // when the mirror's lock is busy, the refs wait for the next end of a step
	}
	if held, ok := c.heldFor(dir, j.Env.Name); ok && held == id {
		c.release(dir, j.Env.Name)
	}
	return true, err
}

// without is the paths of a that b lacks.
func without(a, b []string) []string {
	var out []string
	for _, p := range a {
		if !slices.Contains(b, p) {
			out = append(out, p)
		}
	}
	return out
}

// safePath is a repository path as a Why, an event or a log may carry it:
// quoted, with its control characters escaped, when it holds one or is not
// UTF-8.
func safePath(p string) string {
	if !utf8.ValidString(p) || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || !unicode.IsPrint(r) && r != ' ' }) {
		return strconv.Quote(p)
	}
	return p
}

// listed names at most maxListed paths, made safe, and counts the rest.
func listed(paths []string) string {
	out := make([]string, 0, min(len(paths), maxListed))
	for _, p := range paths[:min(len(paths), maxListed)] {
		out = append(out, safePath(p))
	}
	s := strings.Join(out, ", ")
	if len(paths) > maxListed {
		s += fmt.Sprintf(" and %d more", len(paths)-maxListed)
	}
	return s
}

// minutes is d in whole minutes, for a Why.
func minutes(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return strconv.Itoa(int(d.Hours())) + " hours"
	}
	return strconv.Itoa(int(d.Minutes())) + " minutes"
}

// shorts is shas cut for a Why.
func shorts(shas []string) string {
	out := make([]string, len(shas))
	for i, s := range shas {
		out[i] = short(s)
	}
	return "[" + strings.Join(out, ", ") + "]"
}

// oneLine is s's first line without control characters, cut to maxWhy
// bytes: another component's words in a Why.
func oneLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > maxWhy {
		s = strings.ToValidUTF8(s[:maxWhy], "") + "…"
	}
	return s
}

// gitReason is why a git command failed, for a Why: the lines of its
// stderr (already redacted) that say it (fatal:, error:, the remote's
// remote: lines, a ref's rejection), never the command line with its
// options; another error's first line otherwise.
func gitReason(err error) string {
	ee, ok := errors.AsType[*execx.ExitError](err)
	if !ok {
		return errLine(err)
	}
	var lines []string
	for line := range strings.Lines(ee.Stderr) {
		line = strings.TrimSpace(line)
		switch {
		case line == "", strings.HasPrefix(line, "hint:"), strings.HasPrefix(line, "To "),
			strings.HasPrefix(line, "error: failed to push some refs"):
			continue
		}
		lines = append(lines, oneLine(line))
	}
	if len(lines) == 0 {
		return fmt.Sprintf("git exited %d", ee.Code)
	}
	return oneLine(strings.Join(lines, "; "))
}
