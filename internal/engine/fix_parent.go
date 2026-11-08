package engine

// The PR's parent (the operator, 2026-11-08: "the fix process should
// always first merge parent resolving any conflicts"). The parent is the
// PR's base branch as GitHub names it when the job starts (jobBase), never
// the repository's default branch: a stacked PR's parent is the branch
// below it. The fix job merges it first: its guard looks whether the head
// conflicts with the base's tip (checkFixBase), and when it does not but
// lacks that tip (behindBase), the daemon merges it itself before the
// fixer starts (mergeParent, the fix.parent step), on a loop's first cycle
// only: a later cycle merges again only on a conflict, which is the
// fixer's (skills/magnum-fix, section 2). A branch that is only behind
// never starts a loop by itself. The daemon's merge needs no agent:
// `git merge-tree` of the head and the tip, `git commit-tree` of that tree
// with the head as its first parent and the tip as its second, by the fix
// identity's bot as author and committer, then a push with a lease on the
// head to the PR's branch as the fixer App, never a rebase or a force.
// A merge that brings the base's own workflow changes is not pushed: the
// fixer App's token may not write them. A merge that changes the pool's
// schema paths marks the slot's schema dirty and loads the merged schema
// before the fixer starts, so its specs run on it.
//
// The merge mode (FixModeMerge, `magnum merge-parent`) is a fix job of
// the merge alone: no magnum review needs to stand on the head, and its
// slot is claimed at the head. A clean merge is the daemon's, as above,
// and ends the job; a conflict goes to the fix role with a prompt of the
// merge alone, and a conflict that needs a product decision is listed for
// the operator (`conflict:`). Either push is judged as any push of a merge
// is (no forced re-review: "A push that only merges the base branch is not
// re-reviewed").

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// FixModeMerge is the mode of a fix request and loop that merges the PR's
// parent alone (FixPayload.Mode, FixState.Mode).
const FixModeMerge = "merge"

// fixMergeBy is who starts a merge of the parent (FixState.By).
const fixMergeBy = "magnum merge-parent"

// workflowsPath is where a repository's GitHub workflows live: a merge
// that changes it is not the fixer App's to push.
const workflowsPath = ".github/workflows"

// parentGit is the git the daemon's merge of a PR's parent takes beyond Git
// (*gitx.Client has it): the merged tree, its merge commit, the paths a
// change touches, the paths two commits have otherwise, a first-parent
// chain's commits and the push as the fixer App. A git without it (a
// test's) merges no parent: the fixer starts as before.
type parentGit interface {
	MergeTree(ctx context.Context, dir, head, base string) (tree string, conflicts []string, err error)
	CommitTree(ctx context.Context, dir, tree string, parents []string, message string, who gitx.Signature) (string, error)
	ChangedPaths(ctx context.Context, dir, base, head string, pathspecs ...string) ([]string, error)
	DifferingPaths(ctx context.Context, dir, a, b string, paths ...string) ([]string, error)
	FirstParentIdentities(ctx context.Context, dir, from, to string) ([]gitx.CommitIdentity, error)
	PushLeaseGitHubAs(ctx context.Context, dir, owner, repo, sha, branch, expect string, env map[string]string) error
}

var _ parentGit = (*gitx.Client)(nil)

// daemonPusher is the identity a push of a merge that carries its base's
// workflow files goes out as (*identity.App has it): a token of the
// daemon's own with Workflows write, which no pane holds.
type daemonPusher interface {
	DaemonPushEnv(ctx context.Context, workflows bool) (map[string]string, func(), error)
}

var _ daemonPusher = (*identity.App)(nil)

// workflowsWord is how the daemon's events say it pushed with its own
// token that may write workflows (pushMerge).
const workflowsWord = "with Workflows"

// pushMerge pushes merge, a merge of b's tip into head, to the job's PR
// branch as the fix identity id, with a lease on head, from the job's
// slot, never to master, main, the default branch or the PR's base
// (refusedHeadBranch). A merge that changes no workflow file goes with the
// App's token (its gh config dir, the pane's too). One that brings the
// base's workflow changes goes only when b is on the base as GitHub has it
// now and new to head (baseOnGitHub: never a ref of the slot, which the
// fixer's pane can write) and every workflow file of the merge is as that
// tip on GitHub has it, never with the PR's own, and then with a token of
// the daemon's own with Workflows write (identity.App.DaemonPushEnv: never
// in a pane's gh config dir), released after the push. how is
// workflowsWord for such a push; refused says why nothing was pushed (the
// branch, the PR's own workflow changes, a base GitHub has otherwise, or
// an App without Workflows: "needs Workflows on the App").
func (e *Engine) pushMerge(ctx context.Context, job *fixJob, pg parentGit, id *config.Identity, head, merge string, b fixConflict) (how, refused string, err error) {
	if why := refusedHeadBranch(job.repo, job.pr, b.base); why != "" {
		return "", why, nil
	}
	src := e.d.Identities[id.Name]
	if src == nil {
		return "", "", fmt.Errorf("identity %q is not configured", id.Name)
	}
	dir, at := job.slot.Path, fmt.Sprintf("the merge of %s at %s", b.base, textx.ShortSHA(b.sha))
	workflows, err := pg.ChangedPaths(ctx, dir, head, merge, workflowsPath)
	if err != nil {
		return "", "", err
	}
	var env map[string]string
	release := func() {}
	if len(workflows) == 0 {
		if env, err = src.Env(ctx); err != nil {
			return "", "", fmt.Errorf("the token of %s: %w", id.Name, err)
		}
	} else {
		files := textx.Count(len(workflows), "file", "files")
		tip, why := e.baseOnGitHub(ctx, job, b, head)
		if why != "" {
			return "", fmt.Sprintf("%s brings workflow changes (%s), and %s", at, files, why), nil
		}
		own, err := pg.DifferingPaths(ctx, dir, merge, tip, workflowsPath)
		if err != nil {
			return "", "", fmt.Errorf("whether %s keeps the PR's own workflow changes: %w", at, err)
		}
		dp, ok := src.(daemonPusher)
		switch {
		case len(own) > 0 && tip != b.sha:
			return "", fmt.Sprintf("%s has workflow files that are not as the tip %s of %s on GitHub has them (%s), which %s never pushes",
				at, textx.ShortSHA(tip), b.base, strings.Join(own[:min(3, len(own))], ", "), id.Login), nil
		case len(own) > 0:
			return "", fmt.Sprintf("%s keeps the PR's own workflow changes (%s), which %s never pushes", at, strings.Join(own[:min(3, len(own))], ", "), id.Login), nil
		case !ok:
			return "", fmt.Sprintf("%s brings its workflow changes (%s): %s mints no token that may write them", at, files, id.Name), nil
		}
		env, release, err = dp.DaemonPushEnv(ctx, true)
		switch {
		case errors.Is(err, identity.ErrWorkflowsNotGranted):
			return "", fmt.Sprintf("%s brings its workflow changes (%s): needs Workflows on the App (%s lacks it)", at, files, id.Login), nil
		case err != nil:
			return "", "", fmt.Errorf("a push token with Workflows for %s: %w", id.Name, err)
		}
		how = workflowsWord
	}
	defer release()
	if err := pg.PushLeaseGitHubAs(ctx, dir, job.repo.Owner, job.repo.Name, merge, deref(job.pr.HeadRef), head, env); err != nil {
		return "", "", fmt.Errorf("the push of the merge %s: %w", textx.ShortSHA(merge), err)
	}
	return how, "", nil
}

// refusedHeadBranch says why magnum never pushes to pr's branch, "" when
// it may: master, main, repo's default branch and the PR's base (its
// row's, and base, the one the job merges), as `magnum push` refuses them
// (internal/cli/push.go). A PR from main into production is not the bot's
// to push to.
func refusedHeadBranch(repo store.Repo, pr store.PR, base string) string {
	branch := deref(pr.HeadRef)
	if slices.ContainsFunc([]string{"master", "main", repo.DefaultBranch, deref(pr.BaseRef), base}, func(b string) bool { return b != "" && strings.EqualFold(b, branch) }) {
		return fmt.Sprintf("magnum never pushes to %s (master, main, the default branch or the PR's base)", branch)
	}
	return ""
}

// baseOnGitHub checks b, the commit of the PR's base a merge into head
// merges, against the base as GitHub has it now (the poll identity's
// Details, never a ref of the slot, which the fixer's pane can write): the
// PR's base on GitHub is still b.base, b.sha is on it (its tip has it, as
// the slot's git knows the tip: a fetch of the base brings it) and head
// lacks b.sha. It returns that tip, or why not (refused), in a few words
// for pushMerge's refusal.
func (e *Engine) baseOnGitHub(ctx context.Context, job *fixJob, b fixConflict, head string) (tip, refused string) {
	gh := e.gh(job.watch.PollIdentity)
	if gh == nil {
		return "", fmt.Sprintf("no GitHub client of %s says where the tip of %s is", job.watch.PollIdentity, b.base)
	}
	det, _, err := gh.Details(ctx, job.repo.Owner, job.repo.Name, []int{job.pr.Number})
	d, ok := det[job.pr.Number]
	switch {
	case err != nil:
		return "", fmt.Sprintf("GitHub does not say where the tip of %s is: %s", b.base, oneLine(err.Error(), 200))
	case !ok || d.BaseRefOid == "":
		return "", fmt.Sprintf("GitHub does not say where the tip of %s is", b.base)
	case d.BaseRefName != b.base:
		return "", fmt.Sprintf("the PR's base on GitHub is %s now, not %s", d.BaseRefName, b.base)
	}
	dir, short := job.slot.Path, textx.ShortSHA(b.sha)
	switch on, err := e.d.Git.IsAncestor(ctx, dir, b.sha, d.BaseRefOid); {
	case err != nil:
		return "", fmt.Sprintf("whether %s is on %s as GitHub has it (its tip is %s) could not be checked: %s", short, b.base, textx.ShortSHA(d.BaseRefOid), oneLine(err.Error(), 200))
	case !on:
		return "", fmt.Sprintf("%s is not on %s as GitHub has it (its tip is %s)", short, b.base, textx.ShortSHA(d.BaseRefOid))
	}
	switch in, err := e.d.Git.IsAncestor(ctx, dir, b.sha, head); {
	case err != nil:
		return "", fmt.Sprintf("whether the head has %s already could not be checked: %s", short, oneLine(err.Error(), 200))
	case in:
		return "", fmt.Sprintf("the head has %s already: the merge brings nothing of %s", short, b.base)
	}
	return d.BaseRefOid, ""
}

// fixerSignature is the identity the fix role whose identity is name acts
// as and who its commits are by, the bot's login and its noreply email, as
// its pane's commits are (identity.FixerGitEnv); ok is false for any
// identity but a dedicated fixer App with its bot's user id.
func (e *Engine) fixerSignature(name string) (*config.Identity, gitx.Signature, bool) {
	if !e.cfg.DedicatedFixApp(name) {
		return nil, gitx.Signature{}, false
	}
	id := e.cfg.IdentityByName(name)
	env := identity.FixerGitEnv(*id)
	if env == nil {
		return nil, gitx.Signature{}, false
	}
	return id, gitx.Signature{Name: env["GIT_AUTHOR_NAME"], Email: env["GIT_AUTHOR_EMAIL"]}, true
}

// errNoParentGit is behindBase's answer for a git that cannot merge a
// parent (no parentGit).
var errNoParentGit = errors.New("magnum has no git that merges")

// behindBase is the PR's base whose tip, as last fetched, the job's head
// lacks, in the job's slot (zero: the head has it). The error says git
// could not tell, or cannot merge a parent (errNoParentGit). The guard asks
// only for a head that merges that tip cleanly.
func (e *Engine) behindBase(ctx context.Context, job *fixJob) (fixConflict, error) {
	if _, ok := e.d.Git.(parentGit); !ok {
		return fixConflict{}, errNoParentGit
	}
	base := deref(job.pr.BaseRef)
	tip, err := e.d.Git.RevParse(ctx, job.slot.Path, "refs/remotes/origin/"+base)
	if err != nil {
		return fixConflict{}, fmt.Errorf("the tip of %s: %w", base, err)
	}
	has, err := e.d.Git.IsAncestor(ctx, job.slot.Path, tip, job.head)
	switch {
	case err != nil:
		return fixConflict{}, fmt.Errorf("whether the head has the tip of %s: %w", base, err)
	case has:
		return fixConflict{}, nil
	}
	return fixConflict{base: base, sha: tip}, nil
}

// fixNothingToMerge is the guard's end of a merge of the parent whose head
// has the base's tip already (checkFixBase): nothing failed and nothing is
// left to do.
type fixNothingToMerge struct{ label, base string }

func (e fixNothingToMerge) Error() string {
	return fmt.Sprintf("nothing to merge: %s has the tip of %s already", e.label, e.base)
}

// mergeParent is the fix.parent step: the daemon's merge of the tip of
// job.behind's base into the job's head, which merges it cleanly, in the
// job's slot (see the file's comment). After the push it moves the slot to
// the merge, records it on the PR row as the poller would and lets
// onHeadChange see it, takes the merge as the job's head (job.merged), and
// loads the merged schema when the merge changed it (parentSchema). It
// returns why it merged nothing (refused: the fix role is no fixer App,
// or the merge brings the base's workflow changes), "" once merged; the
// error is a merge or a push that failed. A head that conflicts by now
// (the guard looked a moment before) becomes the fixer's conflict.
func (e *Engine) mergeParent(ctx context.Context, job *fixJob) (refused string, err error) {
	b := job.behind
	job.behind = fixConflict{}
	pg, ok := e.d.Git.(parentGit)
	id, who, app := e.fixerSignature(job.role.Identity)
	switch {
	case !ok:
		return errNoParentGit.Error(), nil
	case !app:
		refused = fmt.Sprintf("magnum merges a parent as a fixer App, and the fix role's identity %s is not one", job.role.Identity)
		e.event(ctx, "info", job.subject(), "fix.parent_skipped", fmt.Sprintf("%s lacks the tip %s of %s: %s", job.label(), textx.ShortSHA(b.sha), b.base, refused),
			map[string]any{"base": b.base, "tip": b.sha, "cycle": job.state.Cycle})
		return refused, nil
	}
	branch, dir, head := deref(job.pr.HeadRef), job.slot.Path, job.head
	tree, conflicts, err := pg.MergeTree(ctx, dir, head, b.sha)
	switch {
	case err != nil:
		return "", err
	case len(conflicts) > 0: // the base moved since the guard looked
		job.conflict = fixConflict{base: b.base, sha: b.sha, paths: conflicts}
		job.forMerge = job.mode == FixModeMerge
		return "", nil
	}
	merge, err := pg.CommitTree(ctx, dir, tree, []string{head, b.sha}, fmt.Sprintf("Merge branch '%s' into %s", b.base, branch), who)
	if err != nil {
		return "", err
	}
	data := map[string]any{"base": b.base, "tip": b.sha, "head": head, "merge": merge, "identity": id.Name, "cycle": job.state.Cycle}
	how, refused, err := e.pushMerge(ctx, job, pg, id, head, merge, b)
	switch {
	case err != nil:
		return "", err
	case refused != "":
		e.event(ctx, "warn", job.subject(), "fix.parent_refused", fmt.Sprintf("%s: %s", job.label(), refused), data)
		return refused, nil
	}
	e.notePush(ctx, job.pr.ID, merge) // ci_dispatch.go: its CI starts after the grace
	if err := e.resyncFixSlot(ctx, job, merge); err != nil {
		return "", fmt.Errorf("slot %s to the merge %s: %w", job.slot.Name, textx.ShortSHA(merge), err)
	}
	if err := e.parentOnRow(ctx, job, merge); err != nil {
		return "", err
	}
	job.head, job.merged = merge, b
	as := id.Login
	if how != "" {
		as, data["workflows"] = as+" "+how, true
	}
	msg := fmt.Sprintf("magnum merged %s at %s into %s as %s: the head is %s", b.base, textx.ShortSHA(b.sha), job.label(), as, textx.ShortSHA(merge))
	if schema := e.parentSchema(ctx, job, head, merge, job.mode != FixModeMerge); schema != "" {
		job.schema = schema
		msg += "; " + schema
		data["schema"] = schema
	}
	e.event(ctx, "info", job.subject(), "fix.parent_merged", msg, data)
	return "", nil
}

// parentOnRow records head, a push magnum made to the job's PR, on its row
// the way the poller records one (recordFixPush) and lets onHeadChange see
// it (seeFixPush), whose rules then judge it as any push: a merge of the
// base alone keeps the review's verdict. The job's PR is the row then.
func (e *Engine) parentOnRow(ctx context.Context, job *fixJob, head string) error {
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		return err
	}
	pr, moved, err := e.recordFixPush(ctx, job, pr, head)
	if err != nil {
		return fmt.Errorf("record the push of %s: %w", textx.ShortSHA(head), err)
	}
	if moved {
		e.seeFixPush(ctx, job, pr.ID)
	}
	if cur, err := e.st.PRByID(ctx, pr.ID); err == nil {
		pr = cur
	}
	if job.pr.BaseRef != nil { // the guard's, as GitHub names it (checkFixBase): the row may hold the poll's older read
		pr.BaseRef = job.pr.BaseRef
	}
	job.pr = pr
	return nil
}

// fixHandRefused is pushedHead's end of a fixer's merge that magnum push
// handed to magnum and that the daemon would not push either (pushHanded's
// refusal): the operator merges it.
type fixHandRefused struct{ why string }

func (e fixHandRefused) Error() string { return e.why }

// pushHanded pushes head, the fixer's base merge that `magnum push` handed
// to magnum (exit 4: the merge carries the base's workflow files, which
// the pane's token may not push), as the settle finds it: GitHub still
// has the job's head, and head is one merge, by the fix identity's bot, of
// a commit of the PR's base into it that changes workflow files. The
// daemon pushes it as any merge of the parent (pushMerge: only with every
// workflow file as that commit has it, with its own Workflows token).
// Any other head is left to confirmPush. It returns why it pushed nothing
// (fixHandRefused's words).
func (e *Engine) pushHanded(ctx context.Context, job *fixJob, head string) (string, error) {
	pg, ok := e.d.Git.(parentGit)
	id, who, app := e.fixerSignature(job.role.Identity)
	base := deref(job.pr.BaseRef)
	if !ok || !app || base == "" || head == job.head {
		return "", nil
	}
	tip, handed := e.handedMerge(ctx, job, pg, who, head)
	if !handed {
		return "", nil
	}
	// the tip GitHub names is in the slot's git after this fetch, whose
	// forced refspec also writes over any ref of the base the pane wrote
	if err := e.fetchBase(ctx, cmp.Or(job.slot.MainClone, job.slot.Path), base, fixRequestFetch); err != nil {
		return "", fmt.Errorf("the fetch of %s: %w", base, err)
	}
	b := fixConflict{base: base, sha: tip}
	how, refused, err := e.pushMerge(ctx, job, pg, id, job.head, head, b)
	if err != nil || refused != "" {
		return refused, err
	}
	e.event(ctx, "info", job.subject(), "fix.parent_pushed", fmt.Sprintf("magnum pushed %s's merge of %s at %s into %s as %s %s: the head is %s",
		job.role.Name, base, textx.ShortSHA(tip), job.label(), id.Login, how, textx.ShortSHA(head)),
		map[string]any{"base": base, "tip": tip, "head": job.head, "merge": head, "identity": id.Name, "cycle": job.state.Cycle})
	return "", nil
}

// handedMerge reports whether head is a merge `magnum push` handed to
// magnum, as pushHanded reads it, and the commit it merges: GitHub still
// has the job's head, head is one merge, by who as author and committer,
// of that commit into the job's head, and it changes workflow files. Any
// read that fails says no. Whether that commit is on base, as GitHub has
// it, is pushMerge's to check (baseOnGitHub): the slot's refs are the
// pane's to write.
func (e *Engine) handedMerge(ctx context.Context, job *fixJob, pg parentGit, who gitx.Signature, head string) (string, bool) {
	dir := job.slot.Path
	if got, err := e.remoteHead(ctx, job); err != nil || got != job.head { // pushed by the pane, or the branch moved
		return "", false
	}
	commits, err := pg.FirstParentIdentities(ctx, dir, job.head, head)
	if err != nil || len(commits) != 1 {
		return "", false
	}
	c := commits[0]
	if len(c.Parents) != 2 || c.Parents[0] != job.head || !strings.EqualFold(c.AuthorEmail, who.Email) || !strings.EqualFold(c.CommitterEmail, who.Email) {
		return "", false
	}
	if workflows, err := pg.ChangedPaths(ctx, dir, job.head, head, workflowsPath); err != nil || len(workflows) == 0 {
		return "", false
	}
	return c.Parents[1], true
}

// parentSchema marks the job's pool slot for a schema reload when the
// merge from..to changed the pool's schema_paths (dirty_schema, which the
// release reads), and, with load (a job whose fixer runs specs next),
// loads the merged schema into the slot's databases now
// (Slots.EnsureSchema), so they do not fall to `not run: environment`. It
// returns what it did ("" for nothing).
func (e *Engine) parentSchema(ctx context.Context, job *fixJob, from, to string, load bool) string {
	pool := job.pool
	pg, ok := e.d.Git.(parentGit)
	if !ok || pool == nil || len(pool.SchemaPaths) == 0 || job.slot.Kind != store.SlotKindPool || e.d.Slots == nil {
		return ""
	}
	changed, err := pg.ChangedPaths(ctx, job.slot.Path, from, to, pool.SchemaPaths...)
	if err != nil || len(changed) == 0 {
		if err != nil {
			e.log.Info("fix: whether the parent merge changed the schema", "subject", job.subject(), "err", err)
		}
		return ""
	}
	if err := e.st.UpdateSlotFields(ctx, job.slot.ID, func(u *store.SlotUpdate) { u.Set("dirty_schema", true) }); err != nil {
		e.log.Warn("fix: mark the slot's schema dirty", "subject", job.subject(), "slot", job.slot.Name, "err", err)
	}
	if !load {
		return "the merge changes the schema: the next round loads it"
	}
	slot, err := e.st.SlotByID(ctx, job.slot.ID)
	if err != nil {
		return "the merge changes the schema: " + err.Error()
	}
	note, err := e.d.Slots.EnsureSchema(ctx, slot, *pool)
	switch {
	case err != nil:
		e.event(ctx, "warn", "slot:"+slot.Name, "slot.schema_reset_failed", "the parent merge: "+err.Error(), nil)
		return "the merge changes the schema, which could not be loaded (the next round reloads it): " + oneLine(err.Error(), 200)
	case note == "":
		return "the merge changes the schema: the next round loads it"
	}
	return note
}

// endParentMerged ends the loop of a merge of the parent the daemon made
// (mergeParent): its row goes, and a fix.ended event and an info toast say
// what it merged.
func (e *Engine) endParentMerged(ctx context.Context, job *fixJob) {
	e.endFixState(ctx, job)
	m := job.merged
	why := fmt.Sprintf("merged %s at %s with no conflict: the head is %s", m.base, textx.ShortSHA(m.sha), textx.ShortSHA(job.head))
	data := job.eventData()
	data["merged_base"], data["merged_sha"], data["head"] = m.base, m.sha, job.head
	e.event(ctx, "info", job.subject(), "fix.ended", "the merge of the parent ends: "+why, data)
	title := fmt.Sprintf("magnum: merged the parent of %s#%d", job.repo.Name, job.pr.Number)
	e.info(notify.Item{Key: fmt.Sprintf("parent-merged:%d:%s", job.pr.ID, job.head), Title: title, Body: job.pr.URL + "\n" + why,
		Line: strings.TrimPrefix(title, "magnum: ") + ": " + why + " " + job.pr.URL, Kind: kindFixEnded})
}

// parentRefused ends the loop of a merge of the parent the daemon would not
// push (mergeParent's refusal): the operator merges it, so the PR needs
// attention with why.
func (e *Engine) parentRefused(ctx context.Context, job *fixJob, why string) {
	e.endFixState(ctx, job)
	msg := fmt.Sprintf("fix: magnum did not merge the parent of %s: %s; merge it on GitHub (Update branch)", job.label(), why)
	e.event(ctx, "warn", job.subject(), "fix.needs_you", msg, job.eventData())
	e.fixAttention(ctx, job, msg)
}

// parentPushed settles a merge of the parent whose fixer pushed head, its
// resolved merge: the loop ends, the push goes on the PR row as the
// poller would record it, whose rules judge it (no forced re-review), and
// what the fixer left the operator (needs: its conflict entries, asked)
// makes the PR need attention.
func (e *Engine) parentPushed(ctx context.Context, job *fixJob, head, needs string, asked bool) {
	e.endFixState(ctx, job)
	if err := e.parentOnRow(ctx, job, head); err != nil {
		e.log.Warn("fix: the merge's push on the PR row", "subject", job.subject(), "err", err)
	}
	data := job.eventData()
	data["head"] = head
	if asked {
		e.event(ctx, "warn", job.subject(), "fix.needs_you", needs, data)
		e.fixAttention(ctx, job, needs)
		return
	}
	why := fmt.Sprintf("%s merged the parent and pushed it: the head is %s", job.role.Name, textx.ShortSHA(head))
	e.event(ctx, "info", job.subject(), "fix.ended", "the merge of the parent ends: "+why, data)
	title := fmt.Sprintf("magnum: merged the parent of %s#%d", job.repo.Name, job.pr.Number)
	e.info(notify.Item{Key: fmt.Sprintf("parent-merged:%d:%s", job.pr.ID, head), Title: title, Body: job.pr.URL + "\n" + why,
		Line: strings.TrimPrefix(title, "magnum: ") + ": " + why + " " + job.pr.URL, Kind: kindFixEnded})
}

// requestMerge handles a merge of pr's parent (FixPayload.Mode
// FixModeMerge): prepareMerge's refusals, then the job (startMerge), and
// the answer says who merges what: the daemon a clean merge, the fix role
// a conflict. A head that has the base's tip already is answered so, with
// no job. A dry run answers what would run and fetches nothing.
func (e *Engine) requestMerge(ctx context.Context, pr store.PR, dry bool) (string, error) {
	if e.draining(ctx) {
		return "", errors.New("the daemon drains for a restart: ask again after it")
	}
	job, nothing, err := e.prepareMerge(ctx, pr, !dry)
	switch {
	case err != nil:
		return "", err
	case nothing != "":
		return nothing, nil
	}
	id, _, _ := e.fixerSignature(job.role.Identity)
	what := job.mergeWords(id.Login)
	if dry {
		return fmt.Sprintf("dry run: would merge the parent of %s: %s", job.label(), what), nil
	}
	if err := e.startMerge(ctx, job); err != nil {
		return "", err
	}
	return fmt.Sprintf("merging the parent of %s: %s; `magnum abort %s` stops it", job.label(), what, job.label()), nil
}

// mergeWords says who merges what into which branch in a merge of the
// parent, as the request sees it, as login (the y/N of `magnum
// merge-parent` and the screens' P puts it): "magnum merges master at
// c0c0c0c into feature as fixer-app[bot], with no conflict" or "claude-fix
// merges master at c0c0c0c (2 conflicting files) into feature as
// fixer-app[bot]".
func (j *fixJob) mergeWords(login string) string {
	branch := deref(j.pr.HeadRef)
	if c := j.conflict; c.sha != "" {
		return fmt.Sprintf("%s %s into %s as %s", j.role.Name, c.words(), branch, login)
	}
	return fmt.Sprintf("magnum merges %s at %s into %s as %s, with no conflict", j.behind.base, textx.ShortSHA(j.behind.sha), branch, login)
}

// prepareMerge decides whether a merge of pr's parent may start, as
// prepareFix does for a fix with the refusals that fit a merge: a fix role
// that is no fixer App (the daemon pushes as one), a running loop, a PR
// that is not the operator's own (the fixer's ownership check), a fork, a
// PR not open or with a round in flight, an unknown branch or base, a
// branch the bot never pushes to (refusedHeadBranch), a slot or PR that
// cannot take the job (pinned or held), a snooze, a
// person's recent typing, a paused agent kind and no free slot. No review
// needs to stand on the head. It then looks at the base (fetched within
// fixRequestFetch when fetch is set): a conflict is the fix role's to
// merge, a clean merge of a tip the head lacks the daemon's, and a head
// that has the tip already leaves nothing (the sentence). The job's
// state.Role holds the login it acts as, for the answer.
func (e *Engine) prepareMerge(ctx context.Context, pr store.PR, fetch bool) (*fixJob, string, error) {
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return nil, "", err
	}
	full := repo.FullName()
	label := fmt.Sprintf("%s#%d", full, pr.Number)
	w := e.cfg.WatchFor(full)
	if w == nil {
		return nil, "", fmt.Errorf("%s is not watched (no [[watch]] covers it)", full)
	}
	role, ok := e.cfg.FixRoleFor(w)
	if !ok {
		return nil, "", fmt.Errorf("no fix role for %s", full)
	}
	id, _, app := e.fixerSignature(role.Identity)
	switch {
	case !app:
		return nil, "", fmt.Errorf("magnum merges a PR's parent as a fixer App, and the fix role's identity %s is not one: set the watch's fix_identity to a dedicated App", role.Identity)
	case !slices.ContainsFunc(id.TokenRepositories, func(r string) bool { return strings.EqualFold(r, repo.Name) }):
		return nil, "", fmt.Errorf("the fixer App %s's tokens do not reach %s: add %s to its token_repositories", id.Name, full, repo.Name)
	}
	if cur, running, err := e.fixState(ctx, pr.ID); err != nil {
		return nil, "", err
	} else if running {
		return nil, "", errors.New(fixRunningSentence(cur))
	}
	switch author := deref(pr.AuthorLogin); {
	case author == "":
		return nil, "", fmt.Errorf("PR #%d's author is not known yet (no details from GitHub)", pr.Number)
	case !e.ownPR(pr):
		return nil, "", fmt.Errorf("PR #%d is %s's, not yours: magnum merges the parent of your own PRs alone", pr.Number, author)
	case pr.IsCrossRepo:
		return nil, "", errors.New("the head lives in a fork magnum cannot push to")
	case pr.GHState != store.GHOpen:
		return nil, "", fmt.Errorf("%s is not open (GitHub %s): nothing to merge", label, pr.GHState)
	case slices.Contains(store.InFlightStates, pr.State) || pr.State == store.PRPaused || e.fixHeld(pr.ID, fixPinRefuses):
		return nil, "", fmt.Errorf("%s: a round is already in progress (%s)", label, pr.State)
	case deref(pr.HeadRef) == "":
		return nil, "", fmt.Errorf("%s: its branch is not known yet (no details from GitHub)", label)
	case deref(pr.BaseRef) == "":
		return nil, "", fmt.Errorf("%s: no parent merge: the PR's base branch is not known yet", label)
	case refusedHeadBranch(repo, pr, "") != "":
		return nil, "", fmt.Errorf("%s: %s", label, refusedHeadBranch(repo, pr, ""))
	}
	slot, has, err := e.slotOf(ctx, pr.ID)
	if err != nil {
		return nil, "", err
	}
	if err := fixSlotRefusal(label, pr, slot, has, fixPinRefuses, false, ""); err != nil {
		return nil, "", err
	}
	now := e.now()
	if s, ok := e.snoozeOf(ctx, pr.ID, now); ok {
		return nil, "", fmt.Errorf("%s is snoozed until %s; `magnum snooze %s --off` lifts it", label, waitClock(s.Until, now), label)
	}
	if until := e.cfg.Daemon.CooldownUntil(pr.HumanActiveAt); now.Before(until) {
		return nil, "", fmt.Errorf("%s: someone typed into its panes; ask again after %s (human_cooldown)", label, waitClock(until, now))
	}
	if _, why := e.pausedKind(ctx, []string{role.AgentKind()}); why != "" {
		return nil, "", fmt.Errorf("%s: %s", label, why)
	}
	if err := e.fixSlotFree(ctx, repo, pr, label, has, false, fixPinRefuses); err != nil {
		return nil, "", err
	}
	var c fixConflict
	if _, err := e.requestBaseCheck(ctx, repo, pr, slot, has, fetch, &c)(); err != nil {
		return nil, "", fmt.Errorf("%s: %w", label, err)
	}
	job := &fixJob{repo: repo, watch: *w, pool: e.cfg.PoolFor(full), pr: pr, slot: slot, role: role, head: pr.HeadSHA,
		round: e.lastRound(ctx, pr), conflict: c, forMerge: c.sha != "", mode: FixModeMerge, state: FixState{Cap: 1, Mode: FixModeMerge}}
	if c.sha == "" {
		probe := *job
		if !has {
			probe.slot.Path = e.cloneOf(repo)
		}
		b, err := e.behindBase(ctx, &probe)
		switch {
		case err != nil:
			return nil, "", fmt.Errorf("%s: its merge with %s could not be checked: %w", label, deref(pr.BaseRef), err)
		case b.sha == "":
			return nil, fmt.Sprintf("nothing to merge: %s has the tip of %s already", label, deref(pr.BaseRef)), nil
		}
		job.behind = b
	}
	return job, "", nil
}

// lastRound is the round of pr's last posted review, the run of a fixer of
// its merge goes in (1 for a PR magnum never reviewed: NewRun's floor).
func (e *Engine) lastRound(ctx context.Context, pr store.PR) int {
	sums, err := e.st.LastReviewSummaries(ctx, []int64{pr.ID})
	if err != nil {
		return 0
	}
	sum, ok := sums[pr.ID]
	if !ok {
		return 0
	}
	run, err := e.st.RunByID(ctx, sum.RunID)
	if err != nil {
		return 0
	}
	return run.Round
}

// startMerge starts the merge of the parent prepareMerge prepared: the PR
// reserved as a fix job holds it (roundHandle.fix), the loop's row in
// phase fixing at cycle 1 of 1 in FixModeMerge, and the job (runFix) on a
// goroutine of its own under the reservation, on the daemon's context.
func (e *Engine) startMerge(ctx context.Context, job *fixJob) error {
	if e.d.Agents == nil || e.d.Slots == nil {
		return errors.New("the daemon has no agents or slots manager")
	}
	st := FixState{Cycle: 1, Cap: 1, Since: job.head, By: fixMergeBy, StartedAt: e.now().UTC(), Phase: FixPhaseFixing, Head: job.head,
		Role: job.role.Name, Mode: FixModeMerge}
	job.state = st
	rctx, cancel := context.WithCancel(ctx)
	if !e.reserve(job.pr.ID, &roundHandle{cancel: cancel, fix: true}) {
		cancel()
		return fmt.Errorf("%s: %s; ask again shortly", job.label(), e.heldReason(job.pr.ID))
	}
	if err := e.claimFixRow(ctx, job, FixState{}, st); err != nil {
		e.unreserve(job.pr.ID)
		cancel()
		return err
	}
	e.roundWG.Go(func() {
		defer e.unreserve(job.pr.ID)
		defer e.workEnded(rctx, job.pr.ID)
		e.safely(rctx, "fix", job.subject(), func() { e.runFix(rctx, job) },
			func(ctx context.Context, msg string) { e.fixPanicked(ctx, job, msg) })
	})
	return nil
}
