package staging

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A staging deploy, on the operator's confirmed word: Plan shows what it
// would do on each env, Deploy starts one effect per env, and Step advances
// every live effect one step a tick. An effect is a journal beside the
// mirror (<mirror_dir>/<owner>/<repo>.deploys/<id>.json), the source of
// truth: each step writes begin, does its work and writes ok or fail, and a
// restart resumes at the recorded step, whose guard runs again before it
// acts. The steps:
//
//  1. claim: the env's flock (<id>'s deploys dir, <env>.lock), held across
//     processes for the effect's life: one effect per repository and env
//     runs at a time; the others wait, queued.
//  2. plan: the tip and the PR's head again. A head that moved fails it.
//  3. merge: `git merge-tree --write-tree <tip> <head>`. When the tip moved
//     since the operator's plan and the merge adds migrations, or changes
//     .github/workflows paths, the plan did not show, it stops: needs you.
//     Clean, `git commit-tree` makes [tip, head] with the bot as author and
//     committer (CommitIdentity); a conflict goes to the Resolver (at most 2
//     tries), whose merge is fetched into the mirror.
//  4. verify: the merge's parents are exactly [tip, head], its tree is
//     merge-tree's or differs from it only in the conflicted paths, the
//     head is still the plan's; a moved tip merges again (at most 2 times,
//     push's included).
//  5. push: PushEnv, then the plain push `git push origin
//     <sha>:refs/heads/staging-<env>`, never forced. A rejection because the
//     tip moved merges again; a branch that already holds the merge is not
//     pushed again.
//  6. watch: ci.yml's run on the merge; a failure fails the effect.
//  7. confirm: a deployment of the env whose history holds the merge ends
//     it done; none ConfirmAfterCI after CI passed ends it done, "pushed,
//     not confirmed".
//
// A cancel stops an effect at once and it never pushes after; a cancel after
// the push stops only the watching: a deploy never reverts a push.

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
	// maxPushTries is how many pushes may fail for another reason than a
	// moved tip before the effect fails.
	maxPushTries = 3
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
// such branch, the merge could not be read); Deploy refuses such an env.
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

// meet records that a caller named repo: Step advances its effects.
func (c *Client) meet(repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.repos[key(repo)]; !ok {
		c.repos[key(repo)] = repo
	}
}

// met lists the repositories this client met, sorted.
func (c *Client) met() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(maps.Values(c.repos))
}

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
// lockWait.
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

// allowed checks env against the allowlist: one of Stagings(repo) by name
// and branch, the branch staging-<name> (^staging-[a-z0-9-]+$), and never
// master, main, the remote's default branch (base, the mirror's HEAD) or
// production* (the name or the branch, case ignored).
func (c *Client) allowed(repo string, env Env, base string) error {
	refused := func(why string) error {
		return fmt.Errorf("staging: %s (%s) is refused: %s", env.Name, env.Branch, why)
	}
	switch b := strings.ToLower(env.Branch); {
	case env.Branch != "staging-"+env.Name || !stagingBranchRe.MatchString(env.Branch):
		return refused("the branch is not staging-<name> of lower-case letters, digits and dashes")
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
			return nil, "", fmt.Errorf("the fetch failed: %s", errLine(err))
		}
		break
	}
	names := slices.Sorted(maps.Keys(specs))
	if len(names) == 0 {
		return map[string]string{}, "", nil
	}
	refs, err := c.d.Git.Refs(ctx, dir, names...)
	if err != nil {
		return nil, "", fmt.Errorf("the mirror's refs could not be read: %s", errLine(err))
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
		return mergeRead{}, fmt.Errorf("merge-tree: %s", errLine(err))
	}
	changes, err := g.treeDiff(ctx, dir, env, tip, tree)
	if err != nil {
		return mergeRead{}, fmt.Errorf("the merge's changes: %s", errLine(err))
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

// Plan reads, never writes a journal or pushes: for each env, the current
// tip, whether the PR's head merges cleanly into it (merge-tree in the
// mirror), the migrations the merge adds and whether it changes any
// .github/workflows path. It fetches the env branches and the PR's pull ref
// into the mirror, under its flock. Every env must pass the allowlist, and
// the remote's pull ref must be the PR's head; an env whose branch is absent
// or whose merge cannot be read has Why. The caller shows the plan to the
// operator before Deploy.
func (c *Client) Plan(ctx context.Context, pr PR, envs []Env) (DeployPlan, error) {
	if err := checkPR(pr); err != nil {
		return DeployPlan{}, err
	}
	c.meet(pr.Repo)
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
			ep := EnvPlan{Env: e, Tip: tips[e.Branch]}
			if ep.Tip == "" {
				ep.Why = "the remote has no branch " + e.Branch
				plan.Envs = append(plan.Envs, ep)
				continue
			}
			m, err := readMerge(ctx, g, dir, env, ep.Tip, pr.Head)
			if err != nil {
				ep.Why = "the merge could not be read: " + err.Error()
			} else {
				ep.Clean, ep.Conflicts, ep.Migrations, ep.Workflows = len(m.conflicts) == 0, m.conflicts, m.migrations, len(m.workflows) > 0
			}
			plan.Envs = append(plan.Envs, ep)
		}
		return nil
	})
	if err != nil {
		return DeployPlan{}, fmt.Errorf("staging %s: plan: %w", pr.Repo, err)
	}
	return plan, nil
}

// Deploy starts, on the operator's confirmed plan, one effect per env, or
// resumes the one of the same PR and head that needs you there; by is who
// asked (ByCLI, ByLiaison, ByFactory). It checks every env first and starts
// none when one is refused: not on the allowlist, without a plan (Why, no
// tip), or with a live effect of the same PR at the same head. Each effect
// is written queued, and claims its env when no other effect holds or waits
// for it; it returns at once, after the claim: Step does the rest.
func (c *Client) Deploy(ctx context.Context, plan DeployPlan, by string) ([]EffectID, error) {
	pr := deployPR(plan.PR)
	if err := checkPR(pr); err != nil {
		return nil, err
	}
	c.meet(pr.Repo)
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
	all := readJournals(root)
	seen := map[string]bool{}
	resume := map[string]*journal{}
	for _, ep := range plan.Envs {
		e := ep.Env
		switch {
		case seen[e.Name]:
			return nil, fmt.Errorf("staging: the plan names %s twice", e.Name)
		case ep.Why != "":
			return nil, fmt.Errorf("staging: %s has no plan: %s", e.Name, ep.Why)
		case !isOID(ep.Tip):
			return nil, fmt.Errorf("staging: the plan for %s names no tip", e.Name)
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
	ids := make([]EffectID, 0, len(plan.Envs))
	for _, ep := range plan.Envs {
		j := &journal{V: journalVersion, ID: newID(ep.Env.Name, pr.Number, now), Repo: pr.Repo, Env: ep.Env, PR: pr, Started: now}
		if old := resume[ep.Env.Name]; old != nil {
			j, err = c.resumeNeedsYou(ctx, root, old.ID)
			if err != nil {
				return ids, err
			}
		}
		j.By, j.Step, j.Phase, j.State, j.Why = by, StepClaim, "", EffectQueued, "waiting for the env"
		j.PlanTip, j.PlanMigrations, j.PlanWorkflows, j.PlanAt = ep.Tip, slices.Clone(ep.Migrations), ep.Workflows, plan.At
		j.PR.HeadRef = pr.HeadRef
		j.Tip, j.Remerges, j.PushTries, j.MergeTree, j.Conflicts, j.Merge = "", 0, 0, "", nil, ""
		j.Updated = now
		if err := c.writeLocked(ctx, root, j); err != nil {
			return ids, fmt.Errorf("staging %s: the journal of %s: %w", pr.Repo, j.ID, err)
		}
		ids = append(ids, j.ID)
	}
	for _, ep := range plan.Envs {
		if _, held := c.heldFor(dir, ep.Env.Name); held {
			continue // this client runs another effect there: the new one waits
		}
		if _, _, err := c.claim(ctx, pr.Repo, dir, root, ep.Env.Name, readJournals(root)); err != nil {
			c.d.Log.Warn("staging: a deploy's env not claimed", "repo", pr.Repo, "env", ep.Env.Name, "err", errLine(err))
		}
	}
	return ids, nil
}

// resumeNeedsYou reads the needs-you effect id for Deploy to start again,
// under its flock; one that left needs you meanwhile is refused.
func (c *Client) resumeNeedsYou(ctx context.Context, root *os.Root, id EffectID) (*journal, error) {
	f, _, err := flockIn(ctx, root, effectLockName(id), true)
	if err != nil {
		return nil, err
	}
	defer unflock(f)
	j, err := readJournal(root, id)
	if err != nil {
		return nil, err
	}
	if j.State != EffectNeedsYou {
		return nil, fmt.Errorf("staging: %s is %s now, no longer needs you", id, j.State)
	}
	return j, nil
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
// else the oldest queued. It reports the effect it holds the flock for.
func (c *Client) claim(ctx context.Context, repo, dir string, root *os.Root, env string, all []*journal) (EffectID, bool, error) {
	var next *journal
	for _, j := range all {
		if j.Env.Name != env || !j.live() {
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

// Step advances every live effect of the repositories this client met one
// step: per env, the effect this client holds the env for, or the next one
// it can claim (an env another process holds is left to it). It never
// blocks on an agent or on CI, only on git in the mirror (a fetch, a merge,
// a push); a Step while another runs on the same client returns at once. A
// daemon calls it each tick; after a restart, an effect resumes once the
// daemon names its repository to the client (a Refresh, Effects).
func (c *Client) Step(ctx context.Context) error {
	if !c.stepping.TryLock() {
		return nil
	}
	defer c.stepping.Unlock()
	var errs []error
	for _, repo := range c.met() {
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
	all := readJournals(root)
	now := c.d.Now()
	envs := map[string]bool{}
	for _, j := range all {
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
		if err := c.stepEnv(ctx, repo, dir, root, env, all); err != nil {
			errs = append(errs, fmt.Errorf("staging %s %s: %w", repo, env, err))
		}
	}
	return errors.Join(errs...)
}

// stepEnv advances env's effect one step: the one this client holds the
// env for (the flock goes once it ended), else the next it claims.
func (c *Client) stepEnv(ctx context.Context, repo, dir string, root *os.Root, env string, all []*journal) error {
	id, held := c.heldFor(dir, env)
	if held && !slices.ContainsFunc(all, func(j *journal) bool { return j.ID == id && j.live() }) {
		c.release(dir, env) // it ended (a Cancel): the env is free
		held = false
	}
	if !held {
		var ok bool
		var err error
		if id, ok, err = c.claim(ctx, repo, dir, root, env, all); err != nil || !ok {
			return err
		}
	}
	live, err := c.advance(ctx, repo, dir, root, id)
	if !live {
		c.release(dir, env)
	}
	return err
}

// advance runs effect id's step once, under its flock and the mirror's;
// it reports whether the effect is still live.
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
	if !j.live() {
		return false, nil
	}
	_, g, err := c.deployDeps(repo)
	if err != nil {
		return true, err
	}
	r := &stepRun{c: c, repo: repo, dir: dir, root: root, g: g, j: j}
	err = underMirror(ctx, dir, func() error {
		switch j.Step {
		case StepClaim, StepPlan:
			return r.plan(ctx)
		case StepMerge:
			return r.merge(ctx)
		case StepVerify:
			return r.verify(ctx)
		case StepPush:
			return r.push(ctx)
		case StepWatch:
			return r.watch(ctx)
		case StepConfirm:
			return r.confirm(ctx)
		}
		return r.end(ctx, EffectFailed, "the journal names an unknown step "+strconv.Quote(j.Step))
	})
	if err != nil && j.live() && !errors.Is(err, errHook) && ctx.Err() == nil {
		// The mirror's lock, or a write: the step stays, and runs again.
		r.note(j.Step, "the step could not run: "+errLine(err))
	}
	return j.live(), err
}

// Effects lists repo's effects, the newest first, from their journals:
// those of every process that deploys from the same mirror_dir.
func (c *Client) Effects(repo string) []Effect {
	c.meet(repo)
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
// reverts. An effect that already ended stays as it is. The effect must be
// one of a repository this client met.
func (c *Client) Cancel(ctx context.Context, id EffectID) error {
	if !validID(id) {
		return fmt.Errorf("staging: %q is no effect id", id)
	}
	for _, repo := range c.met() {
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
	if j.Resolved && g != nil {
		// Its resolved ref goes under the mirror's lock; when the lock is
		// busy, the cancel stands and the ref waits for the mirror's gc.
		err = underMirror(ctx, dir, func() error { return r.end(ctx, EffectCancelled, why) })
	}
	if j.live() {
		r.g = nil
		err = r.end(ctx, EffectCancelled, why)
	}
	if held, ok := c.heldFor(dir, j.Env.Name); ok && held == id {
		c.release(dir, j.Env.Name)
	}
	return true, err
}

// errHook is what a test's hook returns to stop a Step as a killed process
// would: nothing after it is written.
var errHook = errors.New("stopped by the test's hook")

// stepRun is one run of an effect's step: its repository, mirror, deploys dir and journal.
type stepRun struct {
	c    *Client
	repo string
	dir  string
	root *os.Root
	g    deployGit
	j    *journal
}

// at runs the test's hook at a point of step.
func (r *stepRun) at(step, point string) error {
	if r.c.hook == nil {
		return nil
	}
	return r.c.hook(r.j.ID, step, point)
}

// save writes the journal.
func (r *stepRun) save() error {
	r.j.Updated = r.c.d.Now()
	return writeJournal(r.root, r.j)
}

// event records step's phase.
func (r *stepRun) event(step, phase, text string) {
	r.c.d.Log.Debug("staging: deploy", "repo", r.repo, "effect", r.j.ID, "by", r.j.By, "step", step, "phase", phase, "text", text)
	if r.c.d.Event != nil {
		r.c.d.Event(r.j.effect(), step, phase, text)
	}
}

// begin writes step's begin, then runs the test's hook. A polling step (a
// resolver's job, watch, confirm) writes it once; any other writes it each
// time it runs, so a begin found at the start is a step a stopped process
// left: it runs again ("resumed"), its guard first. A step that failed for
// a passing reason (phase fail, Why) runs again without a new begin.
func (r *stepRun) begin(step string, polling bool, text string) error {
	switch {
	case r.j.Step == step && r.j.Phase == PhaseFail:
	case polling && r.j.Step == step && r.j.Phase == PhaseBegin:
	default:
		if r.j.Step == step && r.j.Phase == PhaseBegin {
			text = "resumed: " + text
		}
		r.j.Step, r.j.Phase = step, PhaseBegin
		if err := r.save(); err != nil {
			return err
		}
		r.event(step, PhaseBegin, text)
	}
	return r.at(step, "begin")
}

// ok writes the step's ok and moves the effect to next.
func (r *stepRun) ok(next, text string) error {
	step := r.j.Step
	r.j.Step, r.j.Phase, r.j.Why = next, "", ""
	if err := r.save(); err != nil {
		return err
	}
	r.event(step, PhaseOK, text)
	return nil
}

// note records a passing failure of step: phase fail and why, and the
// event, once per why; the step runs again at the next Step.
func (r *stepRun) note(step, why string) {
	if r.j.Why == why && r.j.Phase == PhaseFail {
		return
	}
	r.j.Phase, r.j.Why = PhaseFail, why
	if err := r.save(); err != nil {
		r.c.d.Log.Warn("staging: a deploy's journal not written", "effect", r.j.ID, "err", errLine(err))
		return
	}
	r.event(step, PhaseFail, why)
}

// retry is note, for a step that returns: the step runs again at the next
// Step.
func (r *stepRun) retry(step, why string) error {
	r.note(step, why)
	return nil
}

// wait is note for a step that waits (CI, Jenkins, the resolver): no fail.
func (r *stepRun) wait(why string) error {
	if r.j.Why == why {
		return nil
	}
	r.j.Why = why
	return r.save()
}

// end ends the effect in state with why: the journal, the event (ok for
// done, fail otherwise), the resolver's job cancelled and the resolved
// ref deleted. The caller holds the mirror's flock, or the effect has no
// resolved ref yet.
func (r *stepRun) end(ctx context.Context, state, why string) error {
	r.j.State, r.j.Why = state, why
	phase := PhaseFail
	if state == EffectDone {
		phase = PhaseOK
	}
	r.j.Phase = phase
	job := r.j.Job
	r.j.Job = ""
	if err := r.save(); err != nil {
		return err
	}
	r.event(r.j.Step, phase, why)
	r.c.d.Log.Info("staging: a deploy ended", "repo", r.repo, "effect", r.j.ID, "env", r.j.Env.Name, "by", r.j.By,
		"state", state, "step", r.j.Step, "merge", short(r.j.Merge))
	if job != "" && r.c.d.Resolver != nil {
		if err := r.c.d.Resolver.Cancel(ctx, job); err != nil {
			r.c.d.Log.Warn("staging: the resolver's job not cancelled", "effect", r.j.ID, "err", errLine(err))
		}
	}
	r.dropResolved(ctx)
	return nil
}

// dropResolved deletes the effect's resolved ref, when it has one.
func (r *stepRun) dropResolved(ctx context.Context) {
	if !r.j.Resolved || r.g == nil {
		return
	}
	if err := r.g.deleteRef(ctx, r.dir, resolvedRef(r.j.ID)); err != nil {
		r.c.d.Log.Warn("staging: a resolved ref not deleted", "effect", r.j.ID, "err", errLine(err))
		return
	}
	r.j.Resolved = false
	if err := r.save(); err != nil {
		r.c.d.Log.Warn("staging: a deploy's journal not written", "effect", r.j.ID, "err", errLine(err))
	}
}

// branch is the effect's staging branch.
func (r *stepRun) branch() string { return r.j.Env.Branch }

// tips fetches and reads the effect's branch tip and, with head, the PR's.
func (r *stepRun) tips(ctx context.Context, head bool) (string, string, error) {
	env, err := r.c.gitEnv(r.repo)
	if err != nil {
		return "", "", err
	}
	n := 0
	if head {
		n = r.j.PR.Number
	}
	tips, h, err := r.c.tipsOf(ctx, r.dir, env, []string{r.branch()}, n)
	if err != nil {
		return "", "", err
	}
	tip := tips[r.branch()]
	if tip == "" {
		return "", "", errNoBranch
	}
	return tip, h, nil
}

// errNoBranch is tips' error when the remote has no such branch: the effect
// fails, since a push would make the branch.
var errNoBranch = errors.New("the remote has no such branch")

// tipsFailed answers a failed read of the tips: no branch fails the effect,
// anything else runs the step again.
func (r *stepRun) tipsFailed(ctx context.Context, step string, err error) error {
	if errors.Is(err, errNoBranch) {
		return r.end(ctx, EffectFailed, "the remote has no branch "+r.branch()+"; nothing was pushed")
	}
	return r.retry(step, "the tips could not be read: "+errLine(err))
}

// headMoved is the Why of a PR whose head is no longer the plan's.
func (r *stepRun) headMoved(now string) string {
	return fmt.Sprintf("PR #%d's head moved since the plan (%s, now %s): plan again; nothing was pushed", r.j.PR.Number, short(r.j.PR.Head), short(now))
}

// plan re-reads the tip and the PR's head.
func (r *stepRun) plan(ctx context.Context) error {
	if err := r.begin(StepPlan, false, "reading "+r.branch()+"'s tip and PR #"+strconv.Itoa(r.j.PR.Number)+"'s head"); err != nil {
		return err
	}
	tip, head, err := r.tips(ctx, true)
	if err != nil {
		return r.tipsFailed(ctx, StepPlan, err)
	}
	if head != r.j.PR.Head {
		return r.end(ctx, EffectFailed, r.headMoved(head))
	}
	r.j.Tip = tip
	text := "tip " + short(tip)
	if tip != r.j.PlanTip {
		text += ", moved since the plan's " + short(r.j.PlanTip)
	}
	return r.ok(StepMerge, text)
}

// merge merges the head into the tip: a clean merge's commit, or a
// resolver's job; with a job, it reads the job's result.
func (r *stepRun) merge(ctx context.Context) error {
	if r.j.Job != "" {
		return r.resolution(ctx)
	}
	if err := r.begin(StepMerge, false, "merging "+short(r.j.PR.Head)+" into "+short(r.j.Tip)); err != nil {
		return err
	}
	env, err := r.c.gitEnv(r.repo)
	if err != nil {
		return r.retry(StepMerge, err.Error())
	}
	m, err := readMerge(ctx, r.g, r.dir, env, r.j.Tip, r.j.PR.Head)
	if err != nil {
		return r.retry(StepMerge, "the merge could not be made: "+err.Error())
	}
	if r.j.Tip != r.j.PlanTip {
		if unseen := without(m.migrations, r.j.PlanMigrations); len(unseen) > 0 {
			return r.end(ctx, EffectNeedsYou, "the tip moved since your plan, and the merge now adds migrations you did not see: "+listed(unseen)+"; plan again")
		}
		if len(m.workflows) > 0 && !r.j.PlanWorkflows {
			return r.end(ctx, EffectNeedsYou, "the tip moved since your plan, and the merge now changes .github/workflows paths you did not see: "+listed(m.workflows)+"; plan again")
		}
	}
	r.j.MergeTree, r.j.Conflicts = m.tree, m.conflicts
	if len(m.conflicts) > 0 {
		return r.resolve(ctx)
	}
	if r.c.d.CommitIdentity == nil {
		return r.end(ctx, EffectFailed, "commit identity not configured")
	}
	name, email, err := r.c.d.CommitIdentity(ctx, r.repo)
	if err == nil {
		err = checkIdentity(name, email)
	}
	if err != nil {
		return r.end(ctx, EffectFailed, "commit identity not configured: "+errLine(err))
	}
	sha, err := r.g.commitTree(ctx, r.dir, m.tree, []string{r.j.Tip, r.j.PR.Head}, r.message(ctx), name, email)
	if err != nil {
		return r.end(ctx, EffectFailed, "the merge commit could not be made: "+errLine(err))
	}
	r.j.Merge = sha
	return r.ok(StepVerify, "merge "+short(sha)+" on "+short(r.j.Tip))
}

// resolve starts a resolver's job on the conflict.
func (r *stepRun) resolve(ctx context.Context) error {
	paths := listed(r.j.Conflicts)
	switch {
	case r.c.d.Resolver == nil:
		return r.end(ctx, EffectFailed, "conflict: resolver not configured: "+paths)
	case r.j.Resolves >= maxResolves:
		return r.end(ctx, EffectFailed, fmt.Sprintf("conflict: the resolver had its %d tries: %s", maxResolves, paths))
	}
	owner, name, _ := strings.Cut(r.repo, "/")
	job, err := r.c.d.Resolver.Start(ctx, ResolveRequest{Repo: r.repo, Ref: owner + "/" + name + "#" + strconv.Itoa(r.j.PR.Number),
		Target: r.branch(), TargetTip: r.j.Tip, Head: r.j.PR.Head, Effect: string(r.j.ID), Try: r.j.Resolves + 1})
	if err == nil && job == "" {
		err = errors.New("no job")
	}
	if err != nil {
		return r.end(ctx, EffectFailed, "conflict: the resolver did not start: "+errLine(err)+": "+paths)
	}
	r.j.Job = job
	r.j.Resolves++
	r.j.Why = fmt.Sprintf("conflict in %d paths: the resolver works on it (try %d)", len(r.j.Conflicts), r.j.Resolves)
	if err := r.save(); err != nil {
		return err
	}
	r.event(StepMerge, PhaseBegin, r.j.Why)
	return nil
}

// resolution reads the resolver's job: running waits, failed fails, done
// fetches its merge into the mirror for verify.
func (r *stepRun) resolution(ctx context.Context) error {
	if r.c.d.Resolver == nil {
		return r.end(ctx, EffectFailed, "conflict: resolver not configured: "+listed(r.j.Conflicts))
	}
	res, err := r.c.d.Resolver.Result(ctx, r.j.Job)
	if err != nil {
		return r.retry(StepMerge, "the resolver's result could not be read: "+errLine(err))
	}
	switch res.State {
	case ResolveRunning:
		return r.wait(fmt.Sprintf("conflict in %d paths: the resolver works on it (try %d)", len(r.j.Conflicts), r.j.Resolves))
	case ResolveFailed:
		return r.end(ctx, EffectFailed, "conflict: the resolver failed: "+oneLine(res.Why))
	case ResolveDone:
	default:
		return r.end(ctx, EffectFailed, "conflict: the resolver answered "+strconv.Quote(oneLine(res.State)))
	}
	if !isOID(res.MergeSHA) {
		return r.end(ctx, EffectFailed, "the resolver's merge is no sha")
	}
	src, err := r.g.checkResolverDir(ctx, res.Dir, r.dir)
	if err != nil {
		return r.end(ctx, EffectFailed, "the resolver's merge is refused: "+errLine(err))
	}
	ref := resolvedRef(r.j.ID)
	r.j.Resolved = true // its ref goes when the effect ends
	ferr := r.g.deleteRef(ctx, r.dir, ref)
	if ferr == nil {
		ferr = r.g.fetchResolved(ctx, r.dir, src, res.MergeSHA, ref)
	}
	if has, err := r.g.hasCommit(ctx, r.dir, res.MergeSHA); err != nil || !has {
		why := "the resolver's merge is not in the mirror"
		if ferr != nil {
			why += ": " + errLine(ferr)
		}
		return r.end(ctx, EffectFailed, why)
	}
	r.j.Merge, r.j.Job = res.MergeSHA, ""
	return r.ok(StepVerify, "resolved merge "+short(res.MergeSHA)+" on "+short(r.j.Tip))
}

// verify checks the merge before any push: its parents, its tree, the
// PR's head, and the tip, which merges again when it moved.
func (r *stepRun) verify(ctx context.Context) error {
	if err := r.begin(StepVerify, false, "verifying "+short(r.j.Merge)); err != nil {
		return err
	}
	tip, head, err := r.tips(ctx, true)
	if err != nil {
		return r.tipsFailed(ctx, StepVerify, err)
	}
	tree, parents, err := r.g.commitInfo(ctx, r.dir, r.j.Merge)
	if err != nil {
		return r.end(ctx, EffectFailed, "the merge "+short(r.j.Merge)+" could not be read: "+errLine(err)+"; nothing was pushed")
	}
	if want := []string{r.j.Tip, r.j.PR.Head}; !slices.Equal(parents, want) {
		return r.end(ctx, EffectFailed, fmt.Sprintf("the merge's parents are %s, not [tip %s, head %s]: nothing was pushed",
			shorts(parents), short(want[0]), short(want[1])))
	}
	if tree != r.j.MergeTree {
		if len(r.j.Conflicts) == 0 {
			return r.end(ctx, EffectFailed, "the merge's tree is not merge-tree's: nothing was pushed")
		}
		env, err := r.c.gitEnv(r.repo)
		if err != nil {
			return r.retry(StepVerify, err.Error())
		}
		changes, err := r.g.treeDiff(ctx, r.dir, env, r.j.MergeTree, tree)
		if err != nil {
			return r.retry(StepVerify, "the resolved merge's paths could not be read: "+errLine(err))
		}
		var extra []string
		for _, ch := range changes {
			if !slices.Contains(r.j.Conflicts, ch.Path) {
				extra = append(extra, ch.Path)
			}
		}
		if len(extra) > 0 {
			return r.end(ctx, EffectFailed, "the resolved merge changes paths outside the conflicts: "+listed(extra)+"; nothing was pushed")
		}
	}
	if head != r.j.PR.Head {
		return r.end(ctx, EffectFailed, r.headMoved(head))
	}
	if tip != r.j.Tip {
		return r.remerge(ctx, tip, "the tip moved before the push")
	}
	return r.ok(StepPush, "merge "+short(r.j.Merge)+": parents and tree checked")
}

// remerge goes back to merge on the new tip, at most maxRemerges times.
func (r *stepRun) remerge(ctx context.Context, tip, why string) error {
	if r.j.Remerges >= maxRemerges {
		return r.end(ctx, EffectFailed, fmt.Sprintf("%s again (to %s): it moved %d times while deploying; nothing was pushed", why, short(tip), maxRemerges+1))
	}
	r.dropResolved(ctx)
	step := r.j.Step
	r.j.Remerges++
	r.j.Tip, r.j.Merge, r.j.MergeTree, r.j.Conflicts, r.j.PushTries = tip, "", "", nil, 0
	r.j.Step, r.j.Phase = StepMerge, ""
	r.j.Why = fmt.Sprintf("%s (to %s): merging again (try %d)", why, short(tip), 1+r.j.Remerges)
	if err := r.save(); err != nil {
		return err
	}
	r.event(step, PhaseFail, r.j.Why)
	return nil
}

// push pushes the verified merge, once: a branch that holds it already is
// not pushed again, a moved tip merges again.
func (r *stepRun) push(ctx context.Context) error {
	if err := r.begin(StepPush, false, "pushing "+short(r.j.Merge)+" to "+r.branch()); err != nil {
		return err
	}
	tip, _, err := r.tips(ctx, false)
	if err != nil {
		return r.tipsFailed(ctx, StepPush, err)
	}
	if done, err := r.landed(ctx, tip); err != nil || done {
		return err
	}
	if tip != r.j.Tip {
		return r.remerge(ctx, tip, "the tip moved before the push")
	}
	if r.c.d.PushEnv == nil {
		return r.end(ctx, EffectFailed, "push not configured: nothing was pushed")
	}
	base, err := r.c.d.Git.Base(ctx, r.dir, r.repo)
	if err != nil {
		return r.retry(StepPush, "the mirror's default branch could not be read: "+errLine(err))
	}
	if err := r.c.allowed(r.repo, r.j.Env, base); err != nil {
		return r.end(ctx, EffectFailed, errLine(err)+": nothing was pushed")
	}
	env, err := r.c.d.PushEnv(ctx, r.repo, r.j.Merge, r.branch())
	if err != nil {
		return r.end(ctx, EffectFailed, "push refused: "+errLine(err))
	}
	allowed := func(branch string) bool { return branch == r.branch() && r.c.allowed(r.repo, r.j.Env, base) == nil }
	perr := r.g.push(ctx, r.dir, env, r.j.Merge, r.branch(), allowed)
	if err := r.at(StepPush, "pushed"); err != nil {
		return err
	}
	if perr == nil {
		return r.pushed("pushed " + short(r.j.Merge) + " to " + r.branch())
	}
	// Read the tip again: a push that landed before its error, or a
	// rejection because another push came first (non-fast-forward).
	tip, _, err = r.tips(ctx, false)
	if err == nil {
		if done, err := r.landed(ctx, tip); err != nil || done {
			return err
		}
		if tip != r.j.Tip {
			return r.remerge(ctx, tip, "the push was rejected: the tip moved")
		}
	}
	r.j.PushTries++
	if r.j.PushTries >= maxPushTries {
		return r.end(ctx, EffectFailed, fmt.Sprintf("the push failed %d times: %s", r.j.PushTries, errLine(perr)))
	}
	return r.retry(StepPush, fmt.Sprintf("the push failed (try %d of %d): %s", r.j.PushTries, maxPushTries, errLine(perr)))
}

// landed reports whether the branch's tip holds the merge already (a push
// before a restart, or one whose answer was lost), and moves on to watch
// when it does: no second push.
func (r *stepRun) landed(ctx context.Context, tip string) (bool, error) {
	in, err := r.g.isAncestor(ctx, r.dir, r.j.Merge, tip)
	if err != nil {
		r.note(StepPush, "the tip's history could not be read: "+errLine(err))
		return true, nil
	}
	if !in {
		return false, nil
	}
	return true, r.pushed(r.branch() + " holds " + short(r.j.Merge) + " already: no push")
}

// pushed records the push and moves on to watch.
func (r *stepRun) pushed(text string) error {
	if r.j.PushedAt.IsZero() {
		r.j.PushedAt = r.c.d.Now()
	}
	return r.ok(StepWatch, text)
}

// watch reads ci.yml's run on the merge: running waits, a failure fails,
// a pass (or a cancel, a later push's run holding it, or no run within
// ConfirmAfterCI of the push) moves on to confirm.
func (r *stepRun) watch(ctx context.Context) error {
	if err := r.begin(StepWatch, true, "watching ci.yml on "+short(r.j.Merge)); err != nil {
		return err
	}
	runs, err := r.c.d.GitHub.RunsOf(ctx, r.repo, Workflow, r.j.Merge)
	if err != nil {
		return r.retry(StepWatch, "ci.yml's runs on the merge could not be read: "+errLine(err))
	}
	now := r.c.d.Now()
	if len(runs) == 0 {
		if now.Sub(r.j.PushedAt) >= ConfirmAfterCI {
			r.j.CIAt = now
			return r.ok(StepConfirm, "no ci.yml run on the merge within "+minutes(ConfirmAfterCI))
		}
		return r.wait("ci.yml has not started on the merge yet")
	}
	newest := slices.MaxFunc(runs, func(a, b Run) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	r.j.RunURL = newest.URL
	switch {
	case newest.Status != "completed":
		return r.wait("ci.yml runs on the merge")
	case passed(&newest):
		r.j.CIAt = now
		return r.ok(StepConfirm, "ci.yml passed")
	case newest.Conclusion == "cancelled":
		r.j.CIAt = now
		return r.ok(StepConfirm, "ci.yml's run on the merge was cancelled (a later push's run holds it)")
	}
	return r.end(ctx, EffectFailed, "ci.yml failed on the merge ("+oneLine(newest.Conclusion)+")")
}

// confirm reads the env's deployments (with their ETag): one whose history
// holds the merge ends the effect done; none ConfirmAfterCI after CI
// passed, or deployments not readable, end it done, not confirmed.
func (r *stepRun) confirm(ctx context.Context) error {
	if err := r.begin(StepConfirm, true, "waiting for a deployment of "+r.j.Env.Name+" that holds "+short(r.j.Merge)); err != nil {
		return err
	}
	now := r.c.d.Now()
	d := r.c.deploymentsOf(ctx, r.repo, r.j.Env.Name, r.j.Deploys, now)
	r.j.Deploys = d
	switch {
	case d.Why == WhyNotReadable:
		return r.end(ctx, EffectDone, NotConfirmed+": "+WhyNotReadable)
	case !d.Readable:
		return r.retry(StepConfirm, d.Why)
	}
	if i := slices.IndexFunc(d.List, func(x Deployment) bool { return x.Success }); i >= 0 {
		dep := cmp.Or(d.List[i].SHA, d.List[i].Ref)
		if isOID(dep) && r.holds(ctx, dep) {
			return r.end(ctx, EffectDone, "live: the deployment of "+short(dep)+" holds the merge")
		}
	}
	if now.Sub(r.j.CIAt) >= ConfirmAfterCI {
		return r.end(ctx, EffectDone, NotConfirmed+": no deployment of "+r.j.Env.Name+" holds the merge "+minutes(ConfirmAfterCI)+" after ci.yml passed")
	}
	return r.wait("waiting for Jenkins to deploy the merge")
}

// holds reports whether the deployed commit's history holds the merge,
// fetching the branch when the mirror lacks it.
func (r *stepRun) holds(ctx context.Context, dep string) bool {
	if dep == r.j.Merge {
		return true
	}
	has, err := r.g.hasCommit(ctx, r.dir, dep)
	if err == nil && !has {
		_, _, _ = r.tips(ctx, false)
		has, err = r.g.hasCommit(ctx, r.dir, dep)
	}
	if err != nil || !has {
		return false
	}
	in, err := r.g.isAncestor(ctx, r.dir, r.j.Merge, dep)
	return err == nil && in
}

// message is the merge's message: "Merge branch '<head branch>' into
// staging-<env>", the head branch from PR.HeadRef or GitHub, else "Merge
// pull request #N into staging-<env>".
func (r *stepRun) message(ctx context.Context) string {
	ref := r.j.PR.HeadRef
	if !validBranch(ref) {
		ref = ""
		if hr, ok := r.c.d.GitHub.(headRefs); ok {
			if got, err := hr.headRefOf(ctx, r.repo, r.j.PR.Number); err == nil && validBranch(got) {
				ref = got
			}
		}
	}
	if ref == "" {
		return fmt.Sprintf("Merge pull request #%d into %s", r.j.PR.Number, r.branch())
	}
	return "Merge branch '" + ref + "' into " + r.branch()
}

// headRefs is a GitHub that reads a PR's head branch: the adapter's.
type headRefs interface {
	headRefOf(ctx context.Context, repo string, number int) (string, error)
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

// listed names at most maxListed paths, and counts the rest.
func listed(paths []string) string {
	if len(paths) <= maxListed {
		return strings.Join(paths, ", ")
	}
	return strings.Join(paths[:maxListed], ", ") + fmt.Sprintf(" and %d more", len(paths)-maxListed)
}

// minutes is d in whole minutes, for a Why.
func minutes(d time.Duration) string { return strconv.Itoa(int(d.Minutes())) + " minutes" }

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
