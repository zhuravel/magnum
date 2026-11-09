package evalrun

// The replay of a local case (eval.KindLocal): a round of the factory's
// local review, from the inputs frozen when its case was added
// (FreezeLocal, LocalCaseDir), in a case worktree of its own, scored as a
// posted case is.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factoryread"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/pipeline"
)

// localCaseInputs are local case ec's frozen inputs, once it may replay:
// not judge-only (a local round runs every role), with its inputs in its
// directory and of its own round (the case's synthetic PR and head), every
// frozen role one cfg declares and a gh identity to run as (LocalIdentity):
// eval.OutcomeConfigError otherwise, since a replay changes nothing until
// the config does. outcome is the case's outcome when err is not nil.
func localCaseInputs(d Deps, cfg *config.Config, w *config.Watch, ec eval.Case, o Options) (in LocalInputs, outcome string, err error) {
	if o.JudgeOnly {
		return in, OutcomeError, fmt.Errorf("case %q is a local round, which replays every role: --judge-only replays posted cases only", ec.Name)
	}
	if in, err = LoadLocalInputs(LocalCaseDir(d.Layout, ec.Name)); err != nil {
		if task, round, ok := LocalRoundOf(ec.Number); ok {
			err = fmt.Errorf("%w; freeze the round again: magnum eval add --local-task %d --round %d --name %s", err, task, round, ec.Name)
		}
		return in, OutcomeError, err
	}
	if !strings.EqualFold(in.PR(), ec.PR) || in.HeadSHA != ec.Head {
		return in, OutcomeError, fmt.Errorf("case %q names %s at %s, but its frozen inputs are round %d of task %d, %s at %s: "+
			"fix the case's pr and head, or freeze its round again", ec.Name, ec.PR, short(ec.Head), in.Round, in.Task, in.PR(), short(in.HeadSHA))
	}
	var gone []string
	for _, role := range in.Roles {
		if _, ok := cfg.RoleByNameOrAlias(nil, role); !ok {
			gone = append(gone, role)
		}
	}
	if len(gone) > 0 {
		return in, eval.OutcomeConfigError, fmt.Errorf("case %q ran with %s, which the config no longer declares: not replayed",
			ec.Name, strings.Join(gone, ", "))
	}
	if _, err := LocalIdentity(cfg, w); err != nil {
		return in, eval.OutcomeConfigError, err
	}
	return in, "", nil
}

// localCodexRefusal says why local case ec is not replayed for Codex's sake
// ("" = it may be): its round ran a Codex role (its frozen roles as d's
// config declares them) on a task Codex must not see again. That is a task
// the factory marked never for Codex again (Codex refused it, and the
// account is at stake), when the case was added (LocalInputs.CodexNever)
// or since (factoryread.TaskCodex, read only), or one whose published PR is
// flagged: an earlier replay of it was refused (flagged, by its PR) or the
// live registry holds a Codex flag for it (liveFlag). A factory registry
// that cannot say refuses the case too, as liveFlag's does; one that is not
// there, or does not hold the task, adds no mark to the frozen one. Inputs
// it cannot read are RunCase's to report (localCaseInputs), before
// anything starts.
func localCodexRefusal(ctx context.Context, d Deps, ec eval.Case, flagged map[string]eval.Flagged) string {
	in, err := LoadLocalInputs(LocalCaseDir(d.Layout, ec.Name))
	if err != nil {
		return ""
	}
	codex := codexRoles(d.Config, in.Roles)
	if len(codex) == 0 {
		return "" // no Codex works on the replay
	}
	ran := fmt.Sprintf("round %d of task %d ran %s: the case is not replayed", in.Round, in.Task, strings.Join(codex, ", "))
	if in.CodexNever {
		return fmt.Sprintf("Codex never works on task %d again (the factory's mark when the case was added), and %s", in.Task, ran)
	}
	never, pr, err := factoryread.TaskCodex(ctx, d.Layout.FactoryDB(), in.Task)
	switch {
	case errors.Is(err, factoryread.ErrNoRegistry), errors.Is(err, factoryread.ErrNoTask):
		return ""
	case err != nil:
		return fmt.Sprintf("the factory's registry cannot say whether Codex may work on task %d (%v), and %s", in.Task, err, ran)
	case never:
		return fmt.Sprintf("Codex never works on task %d again (the factory's mark), and %s", in.Task, ran)
	}
	published, ok := prCase(pr)
	if !ok {
		return ""
	}
	if f, ok := eval.FlaggedCase(flagged, published); ok {
		return fmt.Sprintf("task %d's PR %s was %s, and %s", in.Task, pr, f.Refusal(), ran)
	}
	if why := liveFlag(ctx, d, published); why != "" {
		return fmt.Sprintf("%s; task %d published it, and %s", why, in.Task, ran)
	}
	return ""
}

// codexRoles are the roles among names that run on Codex as cfg declares
// them (a Codex agent, or a shell role whose tool is Codex); every name
// without a config to tell.
func codexRoles(cfg *config.Config, names []string) []string {
	if cfg == nil {
		return slices.Clone(names)
	}
	var out []string
	for _, name := range names {
		if r, ok := cfg.RoleByNameOrAlias(nil, name); ok && r.AgentKind() == config.KindCodex {
			out = append(out, name)
		}
	}
	return out
}

// prCase is the posted case of pr (owner/repo#N) that Refused's checks of
// a PR read: its PR, owner, repository and number, no name.
func prCase(pr string) (eval.Case, bool) {
	repo, num, ok := strings.Cut(pr, "#")
	owner, name, ok2 := strings.Cut(repo, "/")
	n, err := strconv.Atoi(num)
	if !ok || !ok2 || owner == "" || name == "" || err != nil || n <= 0 {
		return eval.Case{}, false
	}
	return eval.Case{PR: pr, Owner: owner, Repo: name, Number: n}, true
}

// replayLocal replays local case ec from its frozen inputs in, after
// RunCase's checks (cr is its record so far, cfg the config with the
// candidate's prompts and skill, w the repository's watch):
//
//   - a detached worktree at the frozen head, in the case's worktree path
//     (caseWorktree, the one a posted case's replays use, replaced when an
//     earlier run left it, removed after unless o.Keep), of the factory
//     repository's main clone, which holds every commit its copies made,
//     else of the clone a posted case's replay takes (clone); the head and
//     the base are fetched there by id when it lacks them; the worktree is
//     prepared as a per-PR review checkout of the repository for the
//     round's synthetic number, and torn down before it goes
//     (addCaseWorktree), a setup that fails being the case's
//     eval.OutcomeEnvironment;
//   - a LocalCase of the frozen inputs in that worktree, with the
//     environment its preparation ran with, a posted replay's for the
//     round's synthetic number under the worktree's own workspace name
//     (caseSlug: WT_BRANCH=magnum-eval-<N>-<hex> and the [[repo]] env, or
//     the pool's), never the factory copy's, whose databases a task may be
//     using; its scratch directory under the run's case directory, never the
//     factory's task directory; the notes as o says (a case whose
//     [[factory.repo]] prepares its copies while its review checkout has
//     nothing to prepare is said to replay in a bare worktree);
//   - RunLocal, then the round's findings (LocalResult.Findings, a
//     question marked, and the planned review's body from the result file
//     of the judge the round ran, LocalResult.ResultFile) scored as a
//     posted case's (eval.ScoreCase), kept on the record
//     (eval.CaseRun.Findings).
//
// A refused round is flagged as a posted one is (eval.MarkFlagged); one
// that went without a reviewer's report is eval.OutcomeMissingRole, its
// error ending with the pane line an agent died on (the round's own error,
// else a missing reviewer's report detail: LocalResult.MissingDetail), as a
// posted replay's error carries it (agents.PaneErrorOf: the skill gate
// tells a CLI's outage by it); one without a judge's result is unscored,
// with why: none is scored as a miss.
func (d Deps) replayLocal(ctx context.Context, cfg *config.Config, w *config.Watch, ec eval.Case, in LocalInputs, o Options, cr eval.CaseRun) eval.CaseRun {
	fail := func(outcome string, err error) eval.CaseRun {
		cr.Outcome, cr.Error = outcome, err.Error()
		return cr
	}
	run := d.Runner
	if run == nil {
		run = &execx.Real{}
	}
	git := gitx.New(run)
	git.HTTPSFetch = true // as the app's: fetch over HTTPS through gh
	mainClone := ""
	if fr, ok := cfg.FactoryRepo(in.Repo); ok && fr.MainClone != "" {
		mainClone = fr.MainClone
	} else {
		var err error
		if mainClone, err = clone(ctx, git, cfg, w, ec); err != nil {
			return fail(OutcomeError, err)
		}
	}
	for _, sha := range []string{in.HeadSHA, in.BaseSHA} {
		if err := git.FetchCommit(ctx, mainClone, sha, 0); err != nil {
			return fail(OutcomeError, fmt.Errorf("commit %s of round %d of task %d is not in %s and cannot be fetched by id: %w", short(sha), in.Round, in.Task,
				tilde(mainClone), err))
		}
	}
	prep, done, outcome, err := d.addCaseWorktree(ctx, cfg, git, run, o, ec.Name, in.Repo, in.Number(), mainClone, in.HeadSHA,
		cmp.Or(in.Base, config.DefaultFactoryBase))
	defer done()
	if err != nil {
		return fail(outcome, err)
	}
	wt := prep.wt
	if fr, ok := cfg.FactoryRepo(in.Repo); ok && prep.bare() && (len(fr.CopyFiles) > 0 || len(fr.Setup) > 0) {
		// The round ran in a copy [[factory.repo]] prepared; the review
		// checkout this worktree stands for has nothing to prepare.
		d.warn("case %s: its round ran in a factory copy that [[factory.repo]] %s prepared (copy_files, setup), while a review checkout of it "+
			"has nothing to prepare (no [[pool]] or [[repo]] copy_files or setup, no .config/wt.toml hooks): the replay's worktree is bare, "+
			"so a spec run can fail where the round's passed", ec.Name, in.Repo)
	}
	if !o.NoNotes {
		cr.Notes = notesHashes(d.Layout, ec.Owner, ec.Repo) // the live notes the round copies as it starts
	}
	lc := LocalCase{Checkout: wt, BaseSHA: in.BaseSHA, HeadSHA: in.HeadSHA, Branch: in.Branch, Title: in.Title, Body: in.Body,
		TicketMD: in.TicketMD, AnswersMD: in.AnswersMD, Watch: *w, Roles: slices.Clone(in.Roles), Round: in.Round, TaskID: in.Task,
		Repo: in.Repo, Base: in.Base, Env: prep.plan.Env,
		ScratchRoot: filepath.Join(o.RunDir, "cases", ec.Name), NoNotes: o.NoNotes}
	ld := d
	ld.Config = cfg
	res, err := RunLocal(ctx, ld, lc)
	cr.ReportDir = res.ReportDir
	switch ce := (*ConfigError)(nil); {
	case errors.As(err, &ce):
		return fail(eval.OutcomeConfigError, err)
	case err != nil:
		return fail(cmp.Or(res.Outcome, OutcomeError), err)
	}
	cr.Outcome, cr.Error = cmp.Or(res.Outcome, OutcomeError), res.Error
	switch {
	case cr.Outcome == pipeline.OutcomeRefused || res.Refused != "":
		cr.Outcome = pipeline.OutcomeRefused
		if err := eval.MarkFlagged(RunsRoot(d.Layout), eval.Flagged{Case: ec.Name, PR: ec.PR, Head: ec.Head, Run: filepath.Base(o.RunDir),
			Reason: cmp.Or(cr.Error, "refused by "+cmp.Or(res.Refused, "its agent's provider")), At: d.now()}); err != nil {
			d.warn("%v", err)
		}
		return cr
	case len(res.Missing) > 0:
		msg := fmt.Sprintf("the round went without the report of %s: what it would have found is unknown, so it is not scored",
			strings.Join(res.Missing, ", "))
		if res.Error != "" {
			msg += ": " + res.Error
		}
		if agents.PaneErrorOf(res.Error) == "" && res.MissingDetail != "" { // the line is read from the end
			msg += "; " + res.MissingDetail
		}
		return fail(eval.OutcomeMissingRole, errors.New(msg))
	case res.ReportDir == "" || res.Verdict == "":
		cr.Error = cmp.Or(cr.Error, "no judge result to score")
		return cr
	}
	cr.ResultFile = res.ResultFile // the frozen roles' judge's, which may not be the watch's
	result := localScoreResult(res, cr.ResultFile)
	score := eval.ScoreCase(ec, result)
	cr.Score, cr.Findings = &score, result.Findings
	return cr
}

// localScoreResult is what a local round's findings are scored on: its
// result's findings as the factory acts on them (LocalResult.Findings,
// each one's title before its body, so a defect's words match either; a
// question marked), then the planned review's body from the judge's result
// file (an InBody finding, which only a defect with body = true reads, and
// never noise), when the file has one. The finding the result makes of the
// body (localBodyTitle) is that body, so it is not scored twice, nor as
// noise.
func localScoreResult(res LocalResult, resultFile string) eval.Result {
	out := eval.Result{Status: pipeline.OutcomeDryRun}
	for _, f := range res.Findings {
		if f.Title == localBodyTitle && f.Path == "" && f.Line == 0 {
			continue
		}
		body := strings.TrimSpace(f.Body)
		if t := strings.TrimSpace(f.Title); t != "" && !strings.Contains(body, t) {
			body = strings.TrimSpace(t + "\n\n" + body)
		}
		out.Findings = append(out.Findings, eval.Finding{Path: f.Path, Line: f.Line, Body: body, Severity: f.Severity,
			Simplification: f.Simplification, Question: f.Question})
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		return out
	}
	parsed, err := eval.ParseResult(data)
	if err != nil {
		return out
	}
	out.Status, out.Event = cmp.Or(parsed.Status, out.Status), parsed.Event
	for _, f := range parsed.Findings {
		if f.InBody {
			out.Findings = append(out.Findings, f)
		}
	}
	return out
}
