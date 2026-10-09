package engine

// Reverts of a landed skill version (DECISIONS "A regression proposes a
// revert, held for review"), the self-improving loop's last stage. A
// revert is a proposal like the editor's, without an agent: in a worktree
// of the checkout on a branch of its own from master, the skill and prompt
// files the version's commit changed are restored as the commit before it
// had them, the design log gets an entry that names the version and why,
// and the candidate passes the same checks and format gate as an edit
// before it is committed and waits for the gate. The gate replays it like
// any proposal, but holds it whatever it finds: a revert lands only on the
// operator's y. One is proposed by `magnum learn --revert <version>`
// (ReqLearnRevert) and, once per version, by the loop's scan when the
// newest version's reviews had their findings declined or argued
// [learn] regression_points more often than the version before
// (maybeRegression, KVLearnRegression).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVLearnRegression is the newest version a regression revert was proposed
// for (its id): the scan proposes one revert per version, never two.
const KVLearnRegression = "learn.regression"

// revertWhyRunes clips the operator's reason for a revert.
const revertWhyRunes = learn.EditReasonRunes

// LearnRevertPayload is a `magnum learn --revert <version>` request: a
// proposal that restores the files of the version before Version, for
// Reason (the design log's entry says it).
type LearnRevertPayload struct {
	Version int64  `json:"version"`
	Reason  string `json:"reason,omitempty"`
}

// revertUnavailable is why no revert can run ("" = one can): no checkout,
// no git or no format gate (a revert needs no agent).
func (e *Engine) revertUnavailable() string {
	switch {
	case e.d.Checkout == "":
		return "no checkout of magnum to revert in (an installed binary has none)"
	case e.d.LearnGit == nil:
		return "no git client"
	case e.d.GoTest == nil:
		return "no format gate (go and mise in the checkout)"
	}
	return ""
}

// revertPlan is what a revert of v restores: the commit before v landed
// and the skill and prompt files v's commit changed. It is an error when v
// is not on master, changed none of those files, or master changed one of
// them since (the revert would undo that change too).
func (e *Engine) revertPlan(ctx context.Context, v store.LearnVersion) (string, []string, error) {
	git, checkout := e.d.LearnGit, e.d.Checkout
	master, err := git.RevParse(ctx, checkout, "refs/heads/"+learnBase)
	if err != nil {
		return "", nil, err
	}
	if base, err := git.MergeBase(ctx, checkout, v.LandedSHA, master); err != nil || base != v.LandedSHA {
		return "", nil, fmt.Errorf("version %d (%s) is not on %s", v.ID, learnShort(v.LandedSHA), learnBase)
	}
	prev, err := git.RevParse(ctx, checkout, v.LandedSHA+"^")
	if err != nil {
		return "", nil, fmt.Errorf("the commit before version %d: %w", v.ID, err)
	}
	changed, err := git.ChangedPaths(ctx, checkout, prev, v.LandedSHA)
	if err != nil {
		return "", nil, err
	}
	files := slices.DeleteFunc(changed, func(p string) bool { return !learn.AllowedEditFile(p) })
	if len(files) == 0 {
		return "", nil, fmt.Errorf("version %d changed none of the skill and prompt files", v.ID)
	}
	slices.Sort(files)
	later, err := git.ChangedPaths(ctx, checkout, v.LandedSHA, master, files...)
	if err != nil {
		return "", nil, err
	}
	if len(later) > 0 {
		return "", nil, fmt.Errorf("%s changed on %s since version %d landed, and a revert would undo that too: revert by hand",
			strings.Join(later, ", "), learnBase, v.ID)
	}
	return prev, files, nil
}

// startRevert runs runRevert of v in the edit's slot (a learn job: it
// marks KVLearnEditing and counts as background work), as startEditJob
// says.
func (e *Engine) startRevert(ctx context.Context, v store.LearnVersion, trigger, why string) (bool, time.Time) {
	return e.startEditJob(ctx, EditMark{Trigger: trigger, Revert: v.ID}, fmt.Sprintf("the revert of skill version %d", v.ID),
		func(ectx context.Context, started time.Time) { _, _ = e.runRevertAt(ectx, v, trigger, why, started) })
}

// runRevert proposes a revert of v for why: the files v changed restored
// from the commit before it on a new branch from master, checked and
// committed as an edit is, and stored for the gate (gating, Reverts v),
// or stored invalid with the problems, or stopped when it cannot run.
func (e *Engine) runRevert(ctx context.Context, v store.LearnVersion, trigger, why string) (store.LearnProposal, error) {
	return e.runRevertAt(ctx, v, trigger, why, e.now())
}

// runRevertAt is runRevert started at start (its run's id).
func (e *Engine) runRevertAt(ctx context.Context, v store.LearnVersion, trigger, why string, start time.Time) (store.LearnProposal, error) {
	run := EditRun{ID: start.Local().Format(editRunFormat)}
	run.Dir = filepath.Join(e.d.Layout.LearnSkill(), run.ID)
	run.Worktree = filepath.Join(e.d.Layout.LearnWorktrees(), run.ID)
	branch := learnBranchPrefix + run.ID
	octx := context.WithoutCancel(ctx)
	e.event(ctx, "info", learnSubject, "learn.revert_start", fmt.Sprintf("the revert of skill version %d started (%s, run %s)", v.ID, trigger, run.ID),
		map[string]any{"run": run.ID, "version": v.ID, "trigger": trigger})
	base, added := "", false
	record := func(state, reason string) store.LearnProposal {
		if base == "" {
			return store.LearnProposal{}
		}
		p, err := e.st.CreateLearnProposal(octx, store.LearnProposalInput{Trigger: trigger, BaseSHA: base, Branch: branch, State: state,
			Reason: textx.Clip(reason, editReasonRunes), Scratch: run.Worktree, RunDir: run.Dir, At: start, Reverts: v.ID})
		if err != nil {
			e.log.Warn("skill revert: store the proposal", "run", run.ID, "state", state, "err", err)
		}
		return p
	}
	stop := func(why string) (store.LearnProposal, error) {
		if added {
			e.dropCandidate(octx, run.Worktree, branch)
		}
		p := record(store.LearnStopped, why)
		e.event(octx, "warn", learnSubject, "learn.edit_stopped", fmt.Sprintf("the revert of skill version %d stopped: %s", v.ID, why),
			map[string]any{"run": run.ID, "version": v.ID, "proposal": p.ID})
		e.toastRevert(v, run, "stopped", why+".")
		return p, errors.New(why)
	}
	if why := e.revertUnavailable(); why != "" {
		return stop(why)
	}
	git, checkout := e.d.LearnGit, e.d.Checkout
	e.dropOrphanCandidates(ctx)
	prev, files, err := e.revertPlan(ctx, v)
	if err != nil {
		return stop(oneLine(err.Error(), retroWhyRunes))
	}
	if base, err = git.RevParse(ctx, checkout, "refs/heads/"+learnBase); err != nil {
		return stop("the checkout's " + learnBase + ": " + oneLine(err.Error(), retroWhyRunes))
	}
	if err := os.MkdirAll(filepath.Dir(run.Worktree), 0o700); err != nil {
		return stop("the worktrees' directory: " + oneLine(err.Error(), retroWhyRunes))
	}
	if err := git.WorktreeAdd(ctx, checkout, run.Worktree, base, false, branch); err != nil {
		return stop("add the worktree: " + oneLine(err.Error(), retroWhyRunes))
	}
	added = true
	if err := git.RestoreFrom(ctx, run.Worktree, prev, files...); err != nil {
		return stop("restore the files: " + oneLine(err.Error(), retroWhyRunes))
	}
	if problems := e.revertCheck(ctx, run); len(problems) > 0 {
		if ctx.Err() != nil {
			return stop("the daemon is stopping")
		}
		e.dropCandidate(octx, run.Worktree, branch)
		p := record(store.LearnInvalid, strings.Join(problems, "; "))
		e.event(octx, "warn", learnSubject, "learn.edit_invalid", fmt.Sprintf("the revert of skill version %d is invalid (%s); kept as proposal %d",
			v.ID, textx.Count(len(problems), "problem", "problems"), p.ID), map[string]any{"run": run.ID, "version": v.ID, "proposal": p.ID,
			"problems": len(problems)})
		e.toastRevert(v, run, "is invalid", fmt.Sprintf("The restored files fail magnum's checks (%s); `magnum learn` lists them.",
			textx.Count(len(problems), "problem", "problems")))
		return p, nil
	}
	decision := fmt.Sprintf("Reverts skill version %d: %s. It restores %s as they were before the version landed (commit %s, the version's %s); "+
		"the gate replays it, and the operator decides.", v.ID, why, strings.Join(files, ", "), learnShort(prev), learnShort(v.LandedSHA))
	sha, committed, err := e.commitRevert(octx, run, base, v, prev, decision, start)
	if err != nil {
		return stop("commit the revert: " + oneLine(err.Error(), retroWhyRunes))
	}
	if err := e.keepRevertChanges(run, files, v, decision); err != nil {
		e.log.Warn("skill revert: keep its changes.json", "run", run.ID, "err", err)
	}
	p, err := e.st.CreateLearnProposal(octx, store.LearnProposalInput{Trigger: trigger, BaseSHA: base, Branch: branch, CandidateSHA: sha,
		Files: committed, State: store.LearnGating, Scratch: run.Worktree, RunDir: run.Dir, At: start, Reverts: v.ID})
	if err != nil {
		return stop("store the proposal: " + oneLine(err.Error(), retroWhyRunes))
	}
	e.event(octx, "info", learnSubject, "learn.revert_ready", fmt.Sprintf("the revert of skill version %d is proposal %d on %s: %s restored",
		v.ID, p.ID, branch, strings.Join(files, ", ")), map[string]any{"run": run.ID, "version": v.ID, "proposal": p.ID, "files": len(committed)})
	return p, nil
}

// revertCheck checks the restored files as an edit's are checked: the
// skill within the cap and keeping every sentence the tests pin now (from
// the worktree's pins file), the prompts rendering (CheckPrompts on the
// config pointed at the worktree), then the format gate; the problems of
// the first stage that has some.
func (e *Engine) revertCheck(ctx context.Context, run EditRun) []string {
	wt := run.Worktree
	b, err := fsx.ReadRegular(filepath.Join(wt, filepath.FromSlash(learn.PinsSource)), 1<<20)
	if err != nil {
		return []string{"the skill's pins (" + learn.PinsSource + "): " + oneLine(err.Error(), retroWhyRunes)}
	}
	pins, limit, err := learn.ParsePins(b)
	if err != nil {
		return []string{"the skill's pins (" + learn.PinsSource + "): " + oneLine(err.Error(), retroWhyRunes)}
	}
	skill, err := fsx.ReadRegular(filepath.Join(wt, filepath.FromSlash(learn.SkillFile)), notes.MaxFileBytes)
	if err != nil {
		return []string{"the restored " + learn.SkillFile + ": " + oneLine(err.Error(), retroWhyRunes)}
	}
	var problems []string
	if len(skill) > limit {
		problems = append(problems, fmt.Sprintf("the restored %s is %d bytes, over its cap of %d now", learn.SkillFile, len(skill), limit))
	}
	for _, p := range pins {
		if !strings.Contains(string(skill), p) {
			problems = append(problems, fmt.Sprintf("the restored %s lacks a sentence the tests pin now: %q", learn.SkillFile, textx.Clip(p, 120)))
		}
	}
	if len(problems) > 0 {
		return problems
	}
	cfg, err := e.cfg.WithCandidate(wt)
	if err == nil {
		_, err = CheckPrompts(cfg)
	}
	if err != nil {
		return []string{"the restored prompts do not render: " + oneLine(err.Error(), 600)}
	}
	for _, args := range formatGate {
		if out, err := e.d.GoTest(ctx, wt, args...); err != nil {
			return []string{fmt.Sprintf("the format gate's `%s` failed (%s):\n%s", strings.Join(args, " "), oneLine(err.Error(), retroWhyRunes),
				lastLines(execx.Redact(out), editGateLines))}
		}
	}
	return nil
}

// commitRevert appends the revert's entry to the worktree's design log and
// commits the restored files, the regenerated goldens and the log on the
// branch; the commit must sit on base and change exactly those files. It
// returns the commit and its files.
func (e *Engine) commitRevert(ctx context.Context, run EditRun, base string, v store.LearnVersion, prev, decision string, start time.Time) (string, []string, error) {
	wt := run.Worktree
	logPath := filepath.Join(wt, filepath.FromSlash(learn.DecisionsFile))
	doc, err := fsx.ReadRegular(logPath, 64<<20)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", learn.DecisionsFile, err)
	}
	fi, err := os.Stat(logPath)
	if err != nil {
		return "", nil, err
	}
	if err := fsx.WriteFileAtomic(logPath, learn.AppendDecision(doc, learn.RevertEntry(decision, start.Local().Format("2006-01-02"))), fi.Mode().Perm()); err != nil {
		return "", nil, err
	}
	changed, goldens, err := e.editChanged(ctx, wt)
	if err != nil {
		return "", nil, err
	}
	files := slices.Concat(changed, goldens)
	slices.Sort(files)
	files = slices.Compact(files)
	size := -1
	if b, err := os.ReadFile(filepath.Join(wt, learn.SkillFile)); err == nil {
		size = len(b)
	}
	head, _ := learn.Headline(decision)
	msg := fmt.Sprintf("%s\n\nA revert of skill version %d (run %s): restores what version %d changed as commit %s had it. SKILL.md %d bytes. "+
		"Changed: %s. No new dependencies.\n", head, v.ID, run.ID, v.ID, learnShort(prev), size, strings.Join(files, ", "))
	git := e.d.LearnGit
	sha, err := git.CommitAll(ctx, wt, msg, files...)
	if err != nil {
		return "", nil, err
	}
	parent, err := git.RevParse(ctx, wt, sha+"^")
	if err != nil || parent != base {
		return "", nil, errors.Join(fmt.Errorf("the commit %s does not sit on the base %s", learnShort(sha), learnShort(base)), err)
	}
	got, err := git.ChangedPaths(ctx, wt, base, sha)
	if err != nil {
		return "", nil, err
	}
	slices.Sort(got)
	if !slices.Equal(got, files) {
		return "", nil, fmt.Errorf("the commit changes %s, not the files checked (%s)", strings.Join(got, ", "), strings.Join(files, ", "))
	}
	return sha, files, nil
}

// keepRevertChanges writes the revert's changes.json into its run
// directory, as an edit keeps the editor's, so `magnum learn --review`
// shows its decision.
func (e *Engine) keepRevertChanges(run EditRun, files []string, v store.LearnVersion, decision string) error {
	ch := learn.Changes{Inputs: []learn.InputChange{}, Decision: decision}
	for _, f := range files {
		ch.Files = append(ch.Files, learn.FileChange{Name: f, Action: learn.FileEdited, Reason: fmt.Sprintf("restored as before version %d", v.ID)})
	}
	b, err := json.MarshalIndent(ch, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(run.Dir, 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(filepath.Join(run.Dir, learn.ChangesFile), b, 0o600)
}

// toastRevert says that a revert of v did not become a proposal, and why:
// the operator asked for it, or a regression did.
func (e *Engine) toastRevert(v store.LearnVersion, run EditRun, what, body string) {
	title := fmt.Sprintf("the revert of skill version %d %s", v.ID, what)
	e.info(notify.Item{Key: "learn-revert:" + run.ID, Title: title, Line: title,
		Body: oneLine(body, 240) + fmt.Sprintf(" `magnum learn --revert %d` asks again.", v.ID)})
}

// requestLearnRevert serves `magnum learn --revert <version>`: a revert of
// the version now, for the operator's reason, unless a drain or an
// infrastructure pause holds it, another learn job runs, a waiting
// proposal already reverts that version, the reason names a pull request,
// a link or a login (it goes into the public design log), or the version
// cannot be reverted (revertPlan says why).
func (e *Engine) requestLearnRevert(ctx context.Context, p LearnRevertPayload) (string, error) {
	vs, err := e.st.LearnVersions(ctx)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(vs, func(v store.LearnVersion) bool { return v.ID == p.Version })
	if i < 0 {
		return "", fmt.Errorf("there is no skill version %d (`magnum learn` lists them)", p.Version)
	}
	v := vs[i]
	reason := textx.Clip(strings.Join(strings.Fields(p.Reason), " "), revertWhyRunes)
	if reason != "" {
		var own []string
		for _, id := range e.cfg.Identities {
			own = append(own, id.Login)
		}
		_, why := learn.ScrubLesson(reason, store.MissScopeGeneral, nil, own, nil)
		if leaks := notes.CheckLeaks(reason, notes.Check{}); why != "" || len(leaks) > 0 {
			return "", errors.New("the reason goes into the public design log, and it names a pull request, a link, a login, a secret or a home " +
				"directory: say why without them")
		}
	}
	if why := cmp.Or(e.holdReason(ctx), e.learnOneAtATime()); why != "" {
		return "", fmt.Errorf("no revert now: %s", why)
	}
	waiting, err := e.st.LearnProposals(ctx, []string{store.LearnGating, store.LearnPassed, store.LearnHeld})
	if err != nil {
		return "", err
	}
	if j := slices.IndexFunc(waiting, func(w store.LearnProposal) bool { return w.Reverts == v.ID }); j >= 0 {
		return "", fmt.Errorf("proposal %d already reverts version %d (%s)", waiting[j].ID, v.ID, waiting[j].State)
	}
	if e.d.DryRun {
		e.rec.Record(ctx, learnSubject, "learn_revert", fmt.Sprintf("a revert of skill version %d", v.ID))
		return fmt.Sprintf("dry run: would propose a revert of skill version %d", v.ID), nil
	}
	if why := e.revertUnavailable(); why != "" {
		return "", fmt.Errorf("no revert: %s", why)
	}
	if _, _, err := e.revertPlan(ctx, v); err != nil {
		return "", fmt.Errorf("no revert of version %d: %w", v.ID, err)
	}
	started, at := e.startRevert(ctx, v, store.LearnTriggerRequest, cmp.Or(reason, "the operator asked for it"))
	switch {
	case !started && at.IsZero():
		return "", fmt.Errorf("no revert now: %s", cmp.Or(e.stopping(ctx), "the daemon is stopping"))
	case !started:
		return "", fmt.Errorf("no revert now: %s", cmp.Or(e.learnOneAtATime(), "a learn job runs"))
	}
	return fmt.Sprintf("the revert of skill version %d runs: the files it changed restored as they were before it, then the format gate and "+
		"the eval's gate, which holds the proposal for `magnum learn --review` (a toast says so)", v.ID), nil
}

// maybeRegression proposes a revert of the newest version, once (the
// loop's scan): when the daemon restarted on it (a version still waiting
// for its restart has no reviews yet), it and the version before it each
// posted at least [learn] regression_min_findings findings, and the share
// of them authors declined or argued grew by [learn] regression_points or
// more. The version before the first is the skill before the loop, over a
// window as long as the first version's. KVLearnRegression keeps the
// version, so a later scan proposes nothing more for it; an event and a
// toast say so, and why when no revert is possible. It waits while another
// learn job runs or a drain or infrastructure pause holds it.
func (e *Engine) maybeRegression(ctx context.Context) {
	if e.revertUnavailable() != "" || e.learnBusy() != "" || e.holdReason(ctx) != "" {
		return
	}
	vs, err := e.st.LearnVersions(ctx)
	if err != nil || len(vs) == 0 || vs[0].RestartedAt == nil {
		return
	}
	v := vs[0]
	id := strconv.FormatInt(v.ID, 10)
	if mark, _ := e.getKV(ctx, KVLearnRegression); mark == id {
		return
	}
	minFindings := e.cfg.Learn.RegressionMinFindings
	cur, err := e.st.VersionMeasure(ctx, v, nil)
	if err != nil || cur.Posted < minFindings {
		return
	}
	before, label := store.LearnVersion{LandedAt: v.RestartedAt.Add(-e.now().Sub(*v.RestartedAt))}, "the skill before it"
	if len(vs) > 1 {
		before, label = vs[1], fmt.Sprintf("version %d", vs[1].ID)
	}
	prev, err := e.st.VersionMeasure(ctx, before, &v)
	if err != nil || prev.Posted < minFindings {
		return
	}
	now, _ := cur.DeclinedShare()
	was, _ := prev.DeclinedShare()
	if now-was < float64(e.cfg.Learn.RegressionPoints) {
		return
	}
	e.setKV(ctx, KVLearnRegression, id)
	why := fmt.Sprintf("the authors declined or argued %.0f%% of its %d posted findings, against %.0f%% of %d for %s", now, cur.Posted, was,
		prev.Posted, label)
	data := map[string]any{"version": v.ID, "share": now, "posted": cur.Posted, "previous_share": was, "previous_posted": prev.Posted}
	title := fmt.Sprintf("skill version %d regressed", v.ID)
	if _, _, err := e.revertPlan(ctx, v); err != nil {
		e.event(ctx, "warn", learnSubject, "learn.regression", fmt.Sprintf("skill version %d regressed: %s; no revert: %s", v.ID, why,
			oneLine(err.Error(), retroWhyRunes)), data)
		e.info(notify.Item{Key: "learn-regression:" + id, Title: title, Line: title,
			Body: oneLine(why+"; no revert proposal: "+err.Error(), 240)})
		return
	}
	e.event(ctx, "warn", learnSubject, "learn.regression", fmt.Sprintf("skill version %d regressed: %s; a revert is proposed", v.ID, why), data)
	e.info(notify.Item{Key: "learn-regression:" + id, Title: title, Line: title,
		Body: oneLine(why, 200) + "; a revert goes through the gate and waits for `magnum learn --review`."})
	e.startRevert(ctx, v, store.LearnTriggerRegression, why)
}
