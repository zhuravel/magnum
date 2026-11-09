package engine

// The operator's staging deploys (`magnum staging <pr> --deploy`, the
// board's and the dashboard's E): a request of kind staging_deploy, which
// the review daemon answers off the tick (a plan fetches the mirror and
// merges in it: minutes on a big repository). A dry run plans the merge of
// the PR's head into each env asked and answers what each would get (a
// clean merge and the migrations it adds, the head there already, the
// conflicted paths), who pushes (the fixer App's bot) and whether the
// default branch's CI runs (a production deploy may be building), for the
// y/N; the deploy then plans again, refuses a head that moved since the
// question, an env that cannot take the merge now and a merge that adds
// what the question did not name, and starts the effects, which Step
// advances. An env whose merge conflicts takes the deploy too: the merge
// step hands the conflict to the resolver (staging_resolve.go, at most
// StagingResolveTries jobs), and the question says so. Own PRs only for now; a request of kind staging_cancel stops an
// effect. The answer is StagingDeployAnswer as JSON on its first line and
// its words on the last.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// The request kinds of the staging deploys.
const (
	// ReqStagingDeploy plans (DryRun) or starts a staging deploy of a PR
	// (StagingDeployPayload); its answer is StagingDeployAnswer.
	ReqStagingDeploy = "staging_deploy"
	// ReqStagingCancel stops a staging deploy's effect
	// (StagingCancelPayload).
	ReqStagingCancel = "staging_cancel"
)

// StagingAll asks for every staging env of the PR's repository.
const StagingAll = "all"

// StagingDeployWait is how long a staging deploy waits for its answer: the
// CLI waits that long, and a deploy asked longer ago is refused (nobody
// waits for it any more).
const StagingDeployWait = 10 * time.Minute

// StagingDeployPayload is a staging deploy of the operator's own PR to
// Envs (env names, in any order, or StagingAll). DryRun only plans it. A
// deploy names Head, the head the operator was asked about (a head that
// moved since is refused), and Seen, what the question named of each env's
// plan (nil when nothing was asked: --yes), so a merge that now adds a
// migration or a workflow change it did not name is refused.
type StagingDeployPayload struct {
	PRTarget
	Envs   []string      `json:"envs,omitempty"`
	Head   string        `json:"head,omitempty"`
	DryRun bool          `json:"dry_run,omitempty"`
	Seen   []StagingSeen `json:"seen,omitempty"`
}

// StagingSeen is what a deploy's question named of one env's plan: its
// migrations and whether it changes .github/workflows.
type StagingSeen struct {
	Env        string   `json:"env"`
	Migrations []string `json:"migrations,omitempty"`
	Workflows  bool     `json:"workflows,omitempty"`
}

// StagingCancelPayload stops the staging deploy's effect Effect (its id,
// as `magnum staging` lists it).
type StagingCancelPayload struct {
	Effect string `json:"effect"`
}

// StagingDeployAnswer is the daemon's answer to a staging deploy: the PR
// (owner/name#N) and the head planned, who pushes (the fixer App's bot's
// login) and who asked (the operator's), the default branch's CI while it
// runs (MasterCI, a warning: "" otherwise), each env's plan in the
// config's order, and, for a deploy, the effects it started. Text is the
// answer in words, the request's last line.
type StagingDeployAnswer struct {
	Ref      string           `json:"ref"`
	Head     string           `json:"head"`
	DryRun   bool             `json:"dry_run,omitempty"`
	Pusher   string           `json:"pusher"`
	AskedBy  string           `json:"asked_by"`
	MasterCI string           `json:"master_ci,omitempty"`
	Envs     []StagingPlanEnv `json:"envs"`
	Effects  []string         `json:"effects,omitempty"`
	Text     string           `json:"text"`
}

// StagingPlanEnv is one env's plan (staging.EnvPlan): its branch and tip,
// whether the head merges cleanly, else the conflicted paths and whether
// the review daemon's resolver merges them (Resolves: then the env takes
// the deploy), the migrations the merge adds, whether it changes
// .github/workflows, and why the env takes no merge (the head there
// already, no branch). The paths are made safe for a terminal.
type StagingPlanEnv struct {
	Env        string   `json:"env"`
	Branch     string   `json:"branch"`
	Tip        string   `json:"tip,omitempty"`
	Clean      bool     `json:"clean"`
	Conflicts  []string `json:"conflicts,omitempty"`
	Resolves   bool     `json:"resolves,omitempty"`
	Migrations []string `json:"migrations,omitempty"`
	Workflows  bool     `json:"workflows,omitempty"`
	Why        string   `json:"why,omitempty"`
}

// StagingResolveTries is how many resolver jobs a deploy's conflict gets
// at most (internal/staging's bound): a conflict that comes back after a
// moved tip gets one more.
const StagingResolveTries = maxResolveTry

// Deployable reports whether the env takes the merge: nothing stops it,
// and it is clean or the resolver merges its conflicts.
func (p StagingPlanEnv) Deployable() bool { return p.Why == "" && (p.Clean || p.Resolves) }

// ParseStagingDeployAnswer reads a staging deploy request's answer (its
// first line); false when it is none (a refusal's words).
func ParseStagingDeployAnswer(result string) (StagingDeployAnswer, bool) {
	line, _, _ := strings.Cut(strings.TrimSpace(result), "\n")
	var a StagingDeployAnswer
	if json.Unmarshal([]byte(line), &a) != nil || a.Ref == "" {
		return StagingDeployAnswer{}, false
	}
	return a, true
}

// result is the request's answer: a as JSON, then its words.
func (a StagingDeployAnswer) result() (string, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	return string(b) + "\n" + a.Text, nil
}

// errStagingOff refuses a staging request without a staging client.
var errStagingOff = errors.New("staging deploys are off: no watched repository has [[factory.repo.staging]] entries in config.toml")

// requestStaging answers a staging deploy or cancel on a goroutine of its
// own (stagingWG, cut by the stop): a plan and a cancel wait for the
// mirror's and the effect's locks, which a deploy's step may hold for
// minutes. A request the stop cut stays pending for the next start.
func (e *Engine) requestStaging(ctx context.Context, req store.Request) {
	var fn func(context.Context) (string, error)
	switch req.Kind {
	case ReqStagingDeploy:
		p, ok := payload[StagingDeployPayload](e, ctx, req)
		if !ok {
			return
		}
		fn = func(ctx context.Context) (string, error) { return e.requestStagingDeploy(ctx, p, req.CreatedAt) }
	case ReqStagingCancel:
		p, ok := payload[StagingCancelPayload](e, ctx, req)
		if !ok {
			return
		}
		fn = func(ctx context.Context) (string, error) { return e.requestStagingCancel(ctx, p) }
	}
	if e.stagingCtx.Err() != nil {
		return // the daemon stops: the next start answers it
	}
	e.mu.Lock()
	e.inflight[req.ID] = true
	e.mu.Unlock()
	e.stagingWG.Go(func() {
		defer func() {
			e.mu.Lock()
			delete(e.inflight, req.ID)
			e.mu.Unlock()
		}()
		e.safely(ctx, "staging request", "", func() {
			rctx, cancel := context.WithCancel(ctx)
			defer cancel()
			defer context.AfterFunc(e.stagingCtx, cancel)()
			res, err := fn(rctx)
			if err != nil && rctx.Err() != nil {
				return // the stop: the request stays pending
			}
			e.complete(context.WithoutCancel(ctx), req.ID, err, res)
		}, func(ctx context.Context, msg string) {
			e.complete(ctx, req.ID, fmt.Errorf("the staging request panicked: %s", msg), "")
		})
	})
}

// requestStagingDeploy plans p's PR on the envs asked and, unless a dry
// run, starts their effects, after its refusals; askedAt is when the
// request was queued (a deploy asked StagingDeployWait or longer before
// is refused: before its plan, and again after it, which can wait for
// minutes).
func (e *Engine) requestStagingDeploy(ctx context.Context, p StagingDeployPayload, askedAt time.Time) (string, error) {
	s := e.d.Staging
	if s == nil || len(StagingRepos(e.cfg)) == 0 {
		return "", errStagingOff
	}
	repo, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		return "", err
	}
	full := repo.FullName()
	label := fmt.Sprintf("%s#%d", full, pr.Number)
	envs, err := stagingChosen(full, StagingEnvs(e.cfg, full), p.Envs)
	if err != nil {
		return "", err
	}
	if why := e.stagingDeployRefusal(pr, label, p); why != "" {
		return "", errors.New(why)
	}
	if why := e.stagingAskedTooLongAgo(label, p, askedAt); why != "" {
		return "", errors.New(why)
	}
	id, _, err := e.stagingFixer(full)
	if err != nil {
		return "", fmt.Errorf("%s: a staging deploy pushes as the fixer App's bot: %w", label, err)
	}
	asked := "you"
	if w := e.cfg.WatchFor(full); w != nil {
		asked = cmp.Or(e.cfg.OperatorLogin(*w), asked)
	}
	plan, err := s.Plan(ctx, staging.PR{Repo: full, Number: pr.Number, Head: pr.HeadSHA, Base: deref(pr.BaseRef), HeadRef: deref(pr.HeadRef)}, envs)
	if err != nil {
		return "", err
	}
	ans := StagingDeployAnswer{Ref: label, Head: pr.HeadSHA, DryRun: p.DryRun, Pusher: id.Login, AskedBy: asked,
		MasterCI: e.masterCIRunning(ctx, repo)}
	for _, ep := range plan.Envs {
		ans.Envs = append(ans.Envs, stagingPlanEnvOf(ep, e.d.stagingResolver != nil))
	}
	if p.DryRun {
		ans.Text = "dry run: would merge " + label + " at " + textx.ShortSHA(pr.HeadSHA) + " as " + id.Login + " into " + stagingPlanWords(ans.Envs)
		if ans.MasterCI != "" {
			ans.Text += "; " + ans.MasterCI
		}
		return ans.result()
	}
	if why := stagingPlanRefusal(ans.Envs, p.Seen); why != "" {
		return "", fmt.Errorf("%s: %s", label, why)
	}
	if why := e.stagingAskedTooLongAgo(label, p, askedAt); why != "" {
		return "", errors.New(why) // the plan waited
	}
	ids, err := s.Deploy(ctx, plan, staging.ByCLI)
	if err != nil {
		return "", err
	}
	names := make([]string, len(ans.Envs))
	for i, ep := range ans.Envs {
		names[i] = ep.Env
	}
	for _, id := range ids {
		ans.Effects = append(ans.Effects, string(id))
	}
	what := "staging deploy of " + label + " at " + textx.ShortSHA(pr.HeadSHA) + " to " + andList(names)
	e.event(ctx, "info", prSubject(repo, pr.Number), "staging.deploy_asked",
		what+" asked by "+asked+", pushed as "+id.Login+": "+strings.Join(ans.Effects, ", "),
		map[string]any{"envs": names, "head": pr.HeadSHA, "effects": ans.Effects, "asked_by": asked, "pusher": id.Login})
	ans.Text = "deploying " + label + " at " + textx.ShortSHA(pr.HeadSHA) + " to " + andList(names) + " as " + id.Login + ": " +
		textx.Count(len(ans.Effects), "effect", "effects") + " " + strings.Join(ans.Effects, ", ") +
		" (`magnum staging " + label + "` follows them, `magnum staging --cancel <effect>` stops one)"
	return ans.result()
}

// stagingDeployRefusal says why pr cannot be deployed as p asks before
// anything is planned: not open, not the operator's own (coworkers' PRs
// come with a gate of their own), a fork, a head branch that is master,
// main or a staging branch, and a deploy whose head is not the PR's.
func (e *Engine) stagingDeployRefusal(pr store.PR, label string, p StagingDeployPayload) string {
	ref := deref(pr.HeadRef)
	switch {
	case pr.GHState != store.GHOpen:
		return label + " is not open: only open PRs deploy to staging"
	case !e.ownPR(pr):
		return label + " is not yours: magnum deploys only your own PRs to staging for now"
	case pr.IsCrossRepo:
		return label + " comes from a fork: a fork's PR never deploys to staging"
	case ref == "master" || ref == "main" || strings.HasPrefix(ref, "staging-"):
		return label + "'s head branch is " + ref + ": master, main and the staging branches are never merged into a staging"
	case p.DryRun:
	case p.Head == "":
		return label + ": the deploy names no head: ask again (`magnum staging " + label + " --deploy <envs>`)"
	case !strings.EqualFold(p.Head, pr.HeadSHA):
		return label + ": the head moved since you were asked: " + textx.ShortSHA(p.Head) + " is now " + textx.ShortSHA(pr.HeadSHA) + ": ask again"
	}
	return ""
}

// stagingAskedTooLongAgo refuses a deploy (never a dry run) asked at
// askedAt, StagingDeployWait or longer before now: nobody waits for its
// answer any more (a stop cut it and the next start found it pending, or
// its plan waited), and the envs may be in other hands by now.
func (e *Engine) stagingAskedTooLongAgo(label string, p StagingDeployPayload, askedAt time.Time) string {
	age := e.now().Sub(askedAt)
	if p.DryRun || askedAt.IsZero() || age < StagingDeployWait {
		return ""
	}
	return label + ": asked " + textx.DurationWords(age.Truncate(time.Minute)) + " ago, longer than a deploy waits for its answer (" +
		textx.DurationWords(StagingDeployWait) + "): nothing was deployed; ask again"
}

// stagingChosen is the envs names asks for among the repository's (envs,
// in the config's order): StagingAll for every one; an unknown name or none
// is refused.
func stagingChosen(repo string, envs []staging.Env, names []string) ([]staging.Env, error) {
	all := make([]string, len(envs))
	for i, e := range envs {
		all[i] = e.Name
	}
	if len(envs) == 0 {
		return nil, fmt.Errorf("%s has no staging envs: its [[factory.repo]] in config.toml names no [[factory.repo.staging]]", repo)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("which staging envs? name them (%s) or %s", strings.Join(all, ", "), StagingAll)
	}
	want := map[string]bool{}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		switch {
		case n == StagingAll:
			return envs, nil
		case !slices.Contains(all, n):
			return nil, fmt.Errorf("%q is not a staging env of %s (%s)", n, repo, strings.Join(all, ", "))
		}
		want[n] = true
	}
	return slices.DeleteFunc(slices.Clone(envs), func(e staging.Env) bool { return !want[e.Name] }), nil
}

// stagingPlanEnvOf is ep as the answer carries it, its words made safe;
// resolver says the daemon has the resolver, which merges a conflict.
func stagingPlanEnvOf(ep staging.EnvPlan, resolver bool) StagingPlanEnv {
	clean := func(ps []string) []string {
		out := make([]string, len(ps))
		for i, p := range ps {
			out[i] = stagingClean(p)
		}
		return out
	}
	return StagingPlanEnv{Env: ep.Env.Name, Branch: ep.Env.Branch, Tip: ep.Tip, Clean: ep.Clean && ep.Why == "",
		Conflicts: clean(ep.Conflicts), Resolves: resolver && ep.Why == "" && !ep.Clean && len(ep.Conflicts) > 0,
		Migrations: clean(ep.Migrations), Workflows: ep.Workflows, Why: stagingClean(ep.Why)}
}

// stagingPlanWords says what each env would get, in one line.
func stagingPlanWords(envs []StagingPlanEnv) string {
	parts := make([]string, len(envs))
	for i, ep := range envs {
		parts[i] = ep.Env + " (" + StagingPlanSays(ep) + ")"
	}
	return strings.Join(parts, "; ")
}

// StagingPlanSays is what one env's plan says in words: why it takes no
// merge, its conflicts (and that the resolver merges them first), or a
// clean merge, with the migrations it adds and its workflow changes.
func StagingPlanSays(ep StagingPlanEnv) string {
	switch {
	case ep.Why != "":
		return ep.Why
	case len(ep.Conflicts) > 0 && !ep.Resolves:
		return "conflicts in " + listedPaths(ep.Conflicts)
	}
	out := "clean"
	if len(ep.Conflicts) > 0 {
		out = "conflicts in " + listedPaths(ep.Conflicts) + ": " + StagingResolverWords
	}
	if n := len(ep.Migrations); n > 0 {
		out += ", adds " + textx.Count(n, "migration", "migrations") + ": " + listedPaths(ep.Migrations)
	}
	if ep.Workflows {
		out += ", changes .github/workflows"
	}
	return out
}

// listedPaths names at most five paths, then counts the rest.
func listedPaths(ps []string) string {
	const shown = 5
	if len(ps) <= shown {
		return strings.Join(ps, ", ")
	}
	return strings.Join(ps[:shown], ", ") + fmt.Sprintf(" and %d more", len(ps)-shown)
}

// StagingResolverWords say, in a few words, what becomes of a conflict at
// a deploy: the question and the plan name them.
const StagingResolverWords = "the resolver merges it first (at most 2 tries)"

// stagingPlanRefusal says why a deploy of envs does not run as asked: an
// env that cannot take the merge now (the head there already, a conflict
// without a resolver), and, once something was asked (seen), an env the
// question did not show, or a merge that now adds a migration or changes
// .github/workflows where the question did not say so.
func stagingPlanRefusal(envs []StagingPlanEnv, seen []StagingSeen) string {
	for _, ep := range envs {
		switch {
		case ep.Why != "":
			return ep.Env + ": " + ep.Why + ": nothing was deployed"
		case len(ep.Conflicts) > 0 && !ep.Resolves:
			return ep.Env + " conflicts in " + listedPaths(ep.Conflicts) + ": nothing was deployed (no resolver here to merge it: merge it by hand)"
		case !ep.Clean && !ep.Resolves:
			return ep.Env + " does not merge cleanly: nothing was deployed"
		}
	}
	if seen == nil {
		return ""
	}
	for _, ep := range envs {
		i := slices.IndexFunc(seen, func(s StagingSeen) bool { return s.Env == ep.Env })
		if i < 0 {
			return ep.Env + " was not in the question: ask again"
		}
		for _, m := range ep.Migrations {
			if !slices.Contains(seen[i].Migrations, m) {
				return ep.Env + " now adds " + m + ", which the question did not name: ask again"
			}
		}
		if ep.Workflows && !seen[i].Workflows {
			return ep.Env + " now changes .github/workflows, which the question did not name: ask again"
		}
	}
	return ""
}

// masterCIRunning warns while ci.yml runs at the tip of repo's default
// branch in its staging mirror (a production deploy may be building, and
// every staging build on the shared Jenkins slows it); "" otherwise, or
// when it cannot be read.
func (e *Engine) masterCIRunning(ctx context.Context, repo store.Repo) string {
	full := repo.FullName()
	dir, ok := staging.MirrorDir(e.cfg.Staging.MirrorDir)(full)
	w := e.cfg.WatchFor(full)
	if !ok || w == nil {
		return ""
	}
	branch := cmp.Or(repo.DefaultBranch, "master")
	tip, err := e.d.Git.RevParse(ctx, dir, "refs/heads/"+branch)
	gh := e.gh(cmp.Or(w.PollIdentity, w.Identity))
	if err != nil || !fullSHA.MatchString(tip) || gh == nil {
		return ""
	}
	checks, err := gh.CommitChecks(ctx, repo.Owner, repo.Name, tip)
	if err != nil || !slices.ContainsFunc(checks, func(c github.Check) bool {
		return c.State == github.CheckPending && c.WorkflowFile == staging.Workflow
	}) {
		return ""
	}
	return branch + "'s CI is running at " + textx.ShortSHA(tip) + ": a production deploy may be building, and every staging build slows it"
}

// requestStagingCancel stops effect p.Effect.
func (e *Engine) requestStagingCancel(ctx context.Context, p StagingCancelPayload) (string, error) {
	s := e.d.Staging
	if s == nil || len(StagingRepos(e.cfg)) == 0 {
		return "", errStagingOff
	}
	if err := s.Cancel(ctx, staging.EffectID(p.Effect)); err != nil {
		return "", err
	}
	return "cancelled staging deploy " + p.Effect + ": it never pushes after; a push before the cancel stands", nil
}
