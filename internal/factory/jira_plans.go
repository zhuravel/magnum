package factory

// The factory's Jira status moves and its Jira comment, in shadow (a
// [[factory.repo]]'s jira_moves and jira_comment = "shadow"): the factory
// plans each move and writes the comment it would keep, and writes nothing
// to Jira (its Jira interface has reads alone).
//
// A task of such a repository with a Jira key of its own (jiraKeyOf: never
// a key of another tracker, never one found in any text) has milestones:
// start (its kind's coding stage, or a later one, began), published (its
// PR is open), merged (done with its PR: the comment only, no move). At
// the latest milestone a live task reached, the tick begins the move's
// plan once (an effect, jira:<task>:<milestone>, with its begin event),
// and the tracking's next batch reads the issue for it (trackJira: a plan
// job, first after the ended tasks' last reads, two of the tick's Jira
// budget): its status (the tracking's own read, whose row it writes) and,
// when the move could go, its transitions. The next pass settles the plan
// with its outcome and an event (settlePlan): planned (from a status of
// the move's from list to its target through a transition), nothing to
// move (at the target or past it), or one the factory could not plan
// (assigned to someone else, no transition to the target, the transition
// wants fields, a target done in Jira). A read that fails leaves the plan
// begun, tried again as the tracking's reads are (trackEvery after the
// try), its failure on the card's JIRA line; the next milestone, or the
// task's end, drops it (could not plan: unread, its last cause). A restart
// finds the plan begun and reads it again: one plan, one begin, one end.
//
// After a planned move, the first read of the issue in another status
// records what you did (matchPlan, an effect beside the plan's and an
// info event): your move to the target, with its time and the lag from the
// plan, or a move elsewhere; a task that ended with no move, after its last
// read, records that. The card's JIRA MOVE line and `magnum factory stats`
// read these records.
//
// The comment (jiraComments): at the latest milestone of a task of such a
// repository with its own key, once (an effect per milestone, with its
// events), the factory writes jira-comment.md in the task's folder, the
// text it would keep on the issue: its own words (the milestone, the task,
// the PR's repository, number and link, the task's state and stage, the
// time), never the PR's, the ticket's or a finding's text. A merged task
// gets one only after an earlier one. None of this ever changes a task.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/textx"
)

// jiraCommentFile is the comment the factory would keep on a task's issue,
// in the task's folder.
const jiraCommentFile = "jira-comment.md"

// maxPlanTransitions is the most transitions a plan without one to its
// target names.
const maxPlanTransitions = 12

// planJob is a begun plan a tracking read is for: its milestone, the
// repository's move there, and when it began.
type planJob struct {
	milestone string
	move      config.FactoryJiraMove
	begun     time.Time
}

// jiraMilestone is the latest milestone task t reached: merged (done with
// its PR), published, start (its kind's coding stage or a later one
// began), "" for none (before the coding stage, or ended otherwise).
func (e *Engine) jiraMilestone(t registry.Task) string {
	switch {
	case t.State == registry.TaskDone && t.PRNumber > 0:
		return config.JiraMilestoneMerged
	case t.State == registry.TaskPublished:
		return config.JiraMilestonePublished
	case trackEnded(t):
		return ""
	}
	kind := e.cfg.Factory.Kinds[t.Kind]
	coding := slices.IndexFunc(kind.Stages, func(st config.FactoryStage) bool {
		return st.Type == config.FactoryStageAgents && st.End == config.FactoryEndProof
	})
	if at := stageIndex(kind, t.Stage); coding >= 0 && at >= coding {
		return config.JiraMilestoneStart
	}
	return ""
}

// milestoneWords say a milestone in the events and the comment.
func milestoneWords(m string) string {
	switch m {
	case config.JiraMilestoneStart:
		return "the start (its coding stage)"
	case config.JiraMilestonePublished:
		return "its PR's publish"
	}
	return m
}

// jiraPlans is the tick's pass over the plans (see the file's comment):
// it drops the begun plans no read will make (the task ended, a later
// milestone came, the moves are off), records what was seen after the
// planned moves, and begins the plan of each live task's milestone. It
// returns the begun plans a read is due for, by task. A failure of the
// registry is logged once while it stays, and costs no task anything.
func (e *Engine) jiraPlans(ctx context.Context, tasks []registry.Task, rows map[int64]registry.Tracking) map[int64]planJob {
	if e.d.Jira == nil || e.draining.Load() {
		return nil
	}
	all, err := e.reg.JiraPlans(ctx)
	if err != nil {
		e.trackStoreFailed(err)
		return nil
	}
	if len(all) == 0 && !slices.ContainsFunc(e.cfg.Factory.Repos, config.FactoryRepo.JiraMovesShadow) {
		return nil
	}
	plans := map[int64]map[string]registry.JiraPlan{}
	for _, p := range all {
		if plans[p.TaskID] == nil {
			plans[p.TaskID] = map[string]registry.JiraPlan{}
		}
		plans[p.TaskID][p.Milestone] = p
	}
	due := map[int64]planJob{}
	now := e.now()
	for _, t := range tasks {
		key := e.jiraKeyOf(t)
		if key == "" {
			continue
		}
		repo, _ := e.cfg.FactoryRepo(t.Repo)
		at := e.jiraMilestone(t)
		mine := plans[t.ID]
		row, had := rows[t.ID]
		had = had && row.JiraKey == key
		for _, m := range []string{config.JiraMilestoneStart, config.JiraMilestonePublished} {
			p, ok := mine[m]
			switch {
			case !ok:
			case p.State == registry.EffectBegin:
				if why := e.planDropped(t, repo, p.Milestone, at); why != "" {
					e.dropPlan(ctx, t, key, p, repo, row, had, why)
				}
			case p.Result.Outcome == registry.PlanPlanned && p.Seen == nil:
				e.matchPlan(ctx, t, key, p, row, had, now)
			}
		}
		move, ok := repo.JiraMove(at)
		if !repo.JiraMovesShadow() || !ok || trackEnded(t) || t.WaitsWho == waitJira {
			continue
		}
		p, ok := mine[at]
		if !ok {
			if p, ok = e.beginPlan(ctx, t, key, at, move); !ok {
				continue
			}
		}
		if p.State == registry.EffectBegin {
			due[t.ID] = planJob{milestone: at, move: move, begun: p.Begun}
		}
	}
	return due
}

// planDropped is why task t's begun plan at milestone m will never be
// made, "" while it may: the moves turned off, the task ended, a later
// milestone (at) came.
func (e *Engine) planDropped(t registry.Task, repo config.FactoryRepo, m, at string) string {
	switch {
	case !repo.JiraMovesShadow():
		return "jira_moves is no longer shadow for " + t.Repo
	case trackEnded(t):
		return "the task ended first"
	case at == m:
		return ""
	case at == config.JiraMilestonePublished:
		return "its PR was published first"
	case at == "":
		return "the task went back before its coding stage"
	}
	return "the task moved on first"
}

// beginPlan begins task t's plan at milestone m, with its event; false
// when it did not begin (logged).
func (e *Engine) beginPlan(ctx context.Context, t registry.Task, key, m string, move config.FactoryJiraMove) (registry.JiraPlan, bool) {
	ef, run, err := e.reg.BeginEffect(ctx, registry.JiraPlanKey(t.ID, m), registry.EffectJiraPlan, t.ID)
	if err != nil {
		e.trackStoreFailed(err)
		return registry.JiraPlan{}, false
	}
	if !run { // settled since the plans were read
		return registry.JiraPlan{}, false
	}
	e.event(ctx, t.ID, "jira_plan", registry.PhaseBegin, fmt.Sprintf("%s reached %s: the factory plans its Jira move to %s from %s "+
		"(shadow: it writes nothing to Jira)", key, milestoneWords(m), move.To, strings.Join(move.From, ", ")))
	return registry.JiraPlan{TaskID: t.ID, Milestone: m, State: registry.EffectBegin, Begun: ef.Begun}, true
}

// dropPlan ends task t's begun plan p, which no read will make (why), as
// a plan the factory could not make: unread, with the last read's cause.
func (e *Engine) dropPlan(ctx context.Context, t registry.Task, key string, p registry.JiraPlan, repo config.FactoryRepo,
	row registry.Tracking, had bool, why string) {
	move, _ := repo.JiraMove(p.Milestone)
	cause := "no read of the issue was made"
	if had && row.LastError != "" {
		cause = row.LastError
	}
	res := registry.JiraPlanResult{Mode: config.JiraShadow, Outcome: registry.PlanUnread, To: move.To, Cause: cause}
	if err := e.reg.EndEffect(ctx, registry.JiraPlanKey(t.ID, p.Milestone), registry.EffectFail, res.Encode()); err != nil {
		if !errors.Is(err, registry.ErrConflict) {
			e.trackStoreFailed(err)
		}
		return
	}
	e.event(ctx, t.ID, "jira_plan", registry.PhaseFail, fmt.Sprintf("the plan of %s's Jira move at %s was not made: %s; the last read: %s",
		key, milestoneWords(p.Milestone), why, cause))
}

// matchPlan records what was seen after task t's planned move p, once: the
// first read after the plan that finds the issue in another status than
// the plan read is your move, to the target or elsewhere, at the
// changelog's time of it when that falls after the plan, else at the read;
// a task that ended with no move records none once its last read is no
// longer owed.
func (e *Engine) matchPlan(ctx context.Context, t registry.Task, key string, p registry.JiraPlan, row registry.Tracking, had bool, now time.Time) {
	pr := p.Result
	var seen registry.JiraSeenResult
	var words string
	switch {
	case had && row.Status != "" && row.ReadAt.After(pr.At) && !strings.EqualFold(row.Status, pr.From):
		at := row.ReadAt
		if row.Since.After(pr.At) && !row.Since.After(row.ReadAt) {
			at = row.Since
		}
		seen = registry.JiraSeenResult{Moved: registry.SeenElsewhere, To: row.Status, At: at, LagSecs: int64(at.Sub(pr.At) / time.Second)}
		words = fmt.Sprintf("you moved %s to %s, not %s, %s after the plan", key, row.Status, pr.To, shortAge(seen.Lag()))
		if strings.EqualFold(row.Status, pr.To) {
			seen.Moved = registry.SeenTarget
			words = fmt.Sprintf("you moved %s to %s %s after the plan", key, row.Status, shortAge(seen.Lag()))
		}
	case trackEnded(t) && !e.lastReadOwed(t, row, had, now):
		seen = registry.JiraSeenResult{Moved: registry.SeenNone, At: t.Updated, LagSecs: int64(max(t.Updated.Sub(pr.At), 0) / time.Second)}
		words = fmt.Sprintf("%s ended in %s: no move after the plan to %s", key, pr.From, pr.To)
	default:
		return
	}
	k := registry.JiraSeenKey(t.ID, p.Milestone)
	if _, run, err := e.reg.BeginEffect(ctx, k, registry.EffectJiraSeen, t.ID); err != nil || !run {
		if err != nil {
			e.trackStoreFailed(err)
		}
		return
	}
	if err := e.reg.EndEffect(ctx, k, registry.EffectOK, seen.Encode()); err != nil {
		e.trackStoreFailed(err)
		return
	}
	e.event(ctx, t.ID, "jira_plan", registry.PhaseInfo, words+" (shadow)")
}

// planNeedsTransitions reports whether the plan of move needs the issue's
// transitions after its status read st (err): the issue answered, its
// assignee is known, and the move could go (a status it moves from, not
// someone else's issue).
func planNeedsTransitions(move config.FactoryJiraMove, st jira.IssueStatus, err error) bool {
	if st.Name == "" || assigneeUnknown(err) {
		return false
	}
	return !strings.EqualFold(st.Name, move.To) && move.Moves(st.Name) && (!st.Assigned || st.AssigneeYou)
}

// assigneeUnknown reports whether a read's failure leaves the assignee not
// known to be you.
func assigneeUnknown(err error) bool {
	pe, ok := errors.AsType[*jira.PartError](err)
	return ok && pe.Part == jira.PartAccount
}

// planOutcome is the plan of move from read r: its result and whether it
// is settled (false: the read failed, and the plan waits for the next).
func planOutcome(move config.FactoryJiraMove, r trackRead) (registry.JiraPlanResult, bool) {
	st := r.st
	if st.Name == "" || assigneeUnknown(r.err) {
		return registry.JiraPlanResult{}, false
	}
	res := registry.JiraPlanResult{Mode: config.JiraShadow, From: st.Name, To: move.To, At: r.at}
	switch {
	case strings.EqualFold(st.Name, move.To) || !move.Moves(st.Name):
		res.Outcome = registry.PlanAlready
		return res, true
	case st.Assigned && !st.AssigneeYou:
		res.Outcome = registry.PlanAssignedOther
		return res, true
	case r.transErr != nil:
		return registry.JiraPlanResult{}, false
	}
	var to []jira.Transition
	for _, tr := range r.trans {
		if strings.EqualFold(tr.To, move.To) {
			to = append(to, tr)
		}
	}
	switch i := slices.IndexFunc(to, func(tr jira.Transition) bool { return !tr.NeedsFields }); {
	case len(to) == 0:
		res.Outcome = registry.PlanNoTransition
		for _, tr := range r.trans[:min(len(r.trans), maxPlanTransitions)] {
			res.Transitions = append(res.Transitions, tr.Name+" → "+tr.To)
		}
	case slices.ContainsFunc(to, func(tr jira.Transition) bool { return tr.ToCategory == "done" }):
		res.Outcome, res.Transition = registry.PlanTargetDone, to[0].Name
	case i < 0:
		res.Outcome, res.Transition = registry.PlanNeedsFields, to[0].Name
	default:
		res.Outcome, res.Transition = registry.PlanPlanned, to[i].Name
	}
	return res, true
}

// settlePlan settles the plan read r was for, from what it read, with its
// event: ok for a move planned or nothing to move, fail for one the factory
// could not plan; a read that failed leaves it begun. A plan settled since
// (dropped) is left alone.
func (e *Engine) settlePlan(ctx context.Context, r trackRead) {
	j := r.job
	res, ok := planOutcome(j.plan.move, r)
	if !ok {
		return
	}
	state, phase := registry.EffectOK, registry.PhaseOK
	if res.Outcome != registry.PlanPlanned && res.Outcome != registry.PlanAlready {
		state, phase = registry.EffectFail, registry.PhaseFail
	}
	if err := e.reg.EndEffect(ctx, registry.JiraPlanKey(j.t.ID, j.plan.milestone), state, res.Encode()); err != nil {
		if !errors.Is(err, registry.ErrConflict) {
			e.trackStoreFailed(err)
		}
		return
	}
	e.event(ctx, j.t.ID, "jira_plan", phase, planWords(j.key, res))
}

// planWords say a settled plan in its event.
func planWords(key string, p registry.JiraPlanResult) string {
	switch p.Outcome {
	case registry.PlanPlanned:
		return fmt.Sprintf("planned %s's Jira move from %s to %s through %s (shadow: nothing was written to Jira)", key, p.From, p.To, p.Transition)
	case registry.PlanAlready:
		if strings.EqualFold(p.From, p.To) {
			return fmt.Sprintf("%s is already %s: nothing to move (shadow)", key, p.To)
		}
		return fmt.Sprintf("%s is %s, past %s: nothing to move (shadow)", key, p.From, p.To)
	case registry.PlanAssignedOther:
		return fmt.Sprintf("%s is assigned to someone else: move it yourself (shadow)", key)
	case registry.PlanNoTransition:
		if len(p.Transitions) == 0 {
			return fmt.Sprintf("Jira offers no transition for %s (the account may lack the Transition issues permission): move it yourself (shadow)", key)
		}
		return fmt.Sprintf("Jira offers %s no transition to %s (it offers %s): move it yourself (shadow)", key, p.To, strings.Join(p.Transitions, ", "))
	case registry.PlanNeedsFields:
		return fmt.Sprintf("Jira wants fields for %s's move to %s through %s: move it yourself (shadow)", key, p.To, p.Transition)
	case registry.PlanTargetDone:
		return fmt.Sprintf("%s is a done status in %s's workflow: the factory never closes or resolves an issue (shadow)", p.To, key)
	}
	return fmt.Sprintf("the plan of %s's Jira move to %s ended: %s", key, p.To, p.Outcome)
}

// jiraComments writes the comment the factory would keep on each task's
// issue at its latest milestone, once (see the file's comment). A failed
// write fails its effect, with its event, logged once while it stays: the
// next milestone writes the file again.
func (e *Engine) jiraComments(ctx context.Context, tasks []registry.Task) {
	if e.draining.Load() || !slices.ContainsFunc(e.cfg.Factory.Repos, config.FactoryRepo.JiraCommentShadow) {
		return
	}
	efs, err := e.reg.EffectsOfKind(ctx, registry.EffectJiraComment)
	if err != nil {
		e.trackStoreFailed(err)
		return
	}
	done := map[string]registry.Effect{}
	for _, ef := range efs {
		done[ef.Key] = ef
	}
	for _, t := range tasks {
		repo, _ := e.cfg.FactoryRepo(t.Repo)
		key, m := e.jiraKeyOf(t), e.jiraMilestone(t)
		if !repo.JiraCommentShadow() || key == "" || m == "" {
			continue
		}
		k := registry.JiraCommentKey(t.ID, m)
		if ef, ok := done[k]; ok && ef.State != registry.EffectBegin {
			continue
		}
		_, earlier := done[registry.JiraCommentKey(t.ID, config.JiraMilestonePublished)]
		_, first := done[registry.JiraCommentKey(t.ID, config.JiraMilestoneStart)]
		if m == config.JiraMilestoneMerged && !earlier && !first { // merged before the comments were on
			continue
		}
		e.writeComment(ctx, t, key, m, k)
	}
}

// writeComment writes task t's comment at milestone m under its effect k.
func (e *Engine) writeComment(ctx context.Context, t registry.Task, key, m, k string) {
	if _, run, err := e.reg.BeginEffect(ctx, k, registry.EffectJiraComment, t.ID); err != nil || !run {
		if err != nil {
			e.trackStoreFailed(err)
		}
		return
	}
	e.event(ctx, t.ID, "jira_comment", registry.PhaseBegin, fmt.Sprintf("write the Jira comment on %s at %s to %s (shadow: not posted)",
		key, milestoneWords(m), jiraCommentFile))
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	err := os.MkdirAll(dir, 0o700)
	if err == nil {
		err = fsx.WriteFileAtomic(filepath.Join(dir, jiraCommentFile), []byte(jiraComment(t, m, e.now())), 0o600)
	}
	if err != nil {
		msg := textx.FirstLine(execx.Redact(err.Error()))
		if eerr := e.reg.EndEffect(ctx, k, registry.EffectFail, msg); eerr != nil {
			e.trackStoreFailed(eerr)
		}
		e.event(ctx, t.ID, "jira_comment", registry.PhaseFail, "the Jira comment was not written: "+msg)
		e.log.Warn("the factory could not write a task's Jira comment (shadow)", "task", t.ID, "err", msg)
		return
	}
	if err := e.reg.EndEffect(ctx, k, registry.EffectOK, jiraCommentFile); err != nil {
		e.trackStoreFailed(err)
		return
	}
	e.event(ctx, t.ID, "jira_comment", registry.PhaseOK, fmt.Sprintf("wrote the Jira comment on %s at %s to %s (shadow: nothing was "+
		"posted to Jira)", key, milestoneWords(m), jiraCommentFile))
}

// jiraComment is task t's comment at milestone m at time at, in the
// factory's own words: what the milestone is, the task, its PR (the
// repository, the number and the link), its state and stage, the time.
func jiraComment(t registry.Task, m string, at time.Time) string {
	var what string
	switch m {
	case config.JiraMilestoneStart:
		what = "The factory started coding this issue"
	case config.JiraMilestonePublished:
		what = "The factory published its PR for review"
	case config.JiraMilestoneMerged:
		what = "The factory's PR is merged"
	}
	pr := "none yet"
	if t.PRNumber > 0 {
		pr = fmt.Sprintf("%s#%d %s", t.Repo, t.PRNumber, prURL(t, registry.PRLink{}))
	}
	state := t.State
	if t.Stage != "" {
		state += ", stage " + t.Stage
	}
	return fmt.Sprintf("%s (magnum factory, task %d).\nPR: %s\nState: %s\nUpdated: %s\n", what, t.ID, pr, state,
		at.UTC().Format("2006-01-02 15:04 UTC"))
}
