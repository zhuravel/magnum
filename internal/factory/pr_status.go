package factory

// Each tick writes the status file of every PR a task published
// (internal/prstatus: <data dir>/factory/pr-status/<owner>/<repo>/<n>.json),
// which magnum's status comment on the PR renders as its factory part. It
// holds fixed words only (counts, enums and times): from what the factory
// recorded of the task (its questions, the ready card you pushed, its
// proof, the PR's link) and from magnum's registry, the PR as followPRs
// read it (who it waits on) and the fix loops' record (Magnum.FixRecords);
// never the text of a PR, a ticket, a question or an agent. The Jira row
// stays empty, since the factory keeps no status of the issue and the
// contract takes a key only with one. The stagings are the PR's staging
// status this tick (stagingPass: each env of its repository, in the
// config's order, the first also as the staging row); a repository without
// staging envs has none, and its staging row stays none.
// prstatus.Write writes only what changed, so an unchanged PR costs no
// write and no edit of the comment. A task that ended (done, stopped,
// failed) gets one last write, its end as its stage, and none after: only
// a task whose file still says another stage gets it, which a daemon
// checks once per task. Nothing is written before the publish, without a
// Magnum, or while magnum's registry cannot be read (the file keeps its
// last content). A failure is logged once while it stays the same and
// fails no tick. The pass only reads: no git, no GitHub.

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/prstatus"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// statusTask is a task whose PR's status file the tick writes: the task,
// its PR as magnum's registry names it, the PR's link and the file.
type statusTask struct {
	t    registry.Task
	ref  PRRef
	link registry.PRLink
	path string
}

// writePRStatuses writes the status files of the PRs that tasks published
// (see the file's comment), states being magnum's view of the published
// tasks' PRs as followPRs read it (nil: it read none) and stagings their
// staging status, by task (stagingPass). A task that ended since has its PR
// read here, once.
func (e *Engine) writePRStatuses(ctx context.Context, tasks []registry.Task, states map[PRRef]PRState, stagings map[int64]taskStaging) {
	if e.d.Magnum == nil {
		return
	}
	var jobs []statusTask
	var refs, ended []PRRef
	for _, t := range tasks {
		j, ok := e.statusTaskOf(ctx, t)
		if !ok {
			continue
		}
		jobs = append(jobs, j)
		refs = append(refs, j.ref)
		if t.State != registry.TaskPublished {
			ended = append(ended, j.ref)
		}
	}
	if len(jobs) == 0 {
		return
	}
	read := true
	if len(ended) > 0 {
		if more, err := e.d.Magnum.PRStates(ctx, ended); err != nil {
			e.statusReadFailed(err) // the ended tasks' last writes wait for a read that works
			read = false
		} else {
			states = maps.Clone(states)
			if states == nil {
				states = map[PRRef]PRState{}
			}
			maps.Copy(states, more)
		}
	}
	fixes, err := e.d.Magnum.FixRecords(ctx, refs)
	if err != nil {
		e.statusReadFailed(err)
		return
	}
	if read {
		e.statusReadErr = ""
	}
	now := e.now()
	for _, j := range jobs {
		st, ok := states[j.ref]
		if !ok { // magnum's registry was not read for it: the file keeps its last content
			continue
		}
		s, err := e.prStatus(ctx, j, st, fixes[j.ref], stagings[j.t.ID], now)
		if err == nil {
			_, err = prstatus.Write(j.path, s)
		}
		if err != nil {
			e.statusFailed(j.t.ID, err)
			continue
		}
		delete(e.statusErrs, j.t.ID)
		if j.t.State != registry.TaskPublished {
			e.statusFinal[j.t.ID] = true
		}
	}
}

// statusTaskOf is task t's PR's status file to write this tick: ok is
// false for a task without a published PR (none, its link not written,
// still at work), and for an ended one whose last write is done or that
// never had a file. A task whose file cannot be named is logged.
func (e *Engine) statusTaskOf(ctx context.Context, t registry.Task) (statusTask, bool) {
	if t.PRNumber <= 0 || e.statusFinal[t.ID] {
		return statusTask{}, false
	}
	ended := t.State != registry.TaskPublished
	switch t.State {
	case registry.TaskPublished, registry.TaskDone, registry.TaskStopped, registry.TaskFailed:
	default:
		return statusTask{}, false
	}
	link, err := e.reg.LinkedPR(ctx, t.ID)
	if err != nil {
		switch {
		case !errors.Is(err, registry.ErrNotFound):
			e.statusFailed(t.ID, err)
		case ended: // no link, no publish time: nothing to write
			e.statusFinal[t.ID] = true
		}
		return statusTask{}, false
	}
	ref := PRRef{Repo: cmp.Or(link.Repo, t.Repo), Number: cmp.Or(link.Number, t.PRNumber)}
	owner, name, _ := strings.Cut(ref.Repo, "/")
	path, err := prstatus.Path(e.d.Layout, owner, name, ref.Number)
	if err != nil { // a name GitHub does not take: never a file
		e.statusFailed(t.ID, err)
		return statusTask{}, false
	}
	if ended {
		old, ok, err := prstatus.Read(path)
		if err == nil && (!ok || endStage(old.Now.Stage)) { // never written, or written last already
			e.statusFinal[t.ID] = true
			return statusTask{}, false
		}
	}
	return statusTask{t: t, ref: ref, link: link, path: path}, true
}

// endStage reports whether s is a stage a task ends at.
func endStage(s prstatus.Stage) bool {
	return s == prstatus.StageDone || s == prstatus.StageStopped || s == prstatus.StageFailed
}

// prStatus is j's PR's status at now, by magnum's view of the PR st, its
// fix loops' record fix and its staging status stg (none for an ended task
// or a repository without staging envs: an ended task's stay as the file
// has them).
func (e *Engine) prStatus(ctx context.Context, j statusTask, st PRState, fix FixRecord, stg taskStaging, now time.Time) (prstatus.Status, error) {
	qs, err := e.reg.Questions(ctx, j.t.ID)
	if err != nil {
		return prstatus.Status{}, err
	}
	dir := e.d.Layout.FactoryTaskDir(j.t.ID)
	card, err := readCard(dir)
	if err != nil {
		card = ReadyCardFile{} // none: no commit and no review to count
	}
	s := prstatus.Status{UpdatedAt: now, TaskID: j.t.ID, Plan: planOf(qs), PublishedAt: j.link.Opened,
		Code: prstatus.Code{Commits: card.Commits, Proof: prstatus.ProofNone},
		LocalReview: prstatus.LocalReview{Rounds: card.Review.Rounds, Verdict: verdictOf(card.Review.Exit),
			NotesOpen: len(knownLines(card.Review, false))},
		FixLoop: fixLoopOf(st, fix), Staging: prstatus.Staging{State: prstatus.StagingNone}, Now: nowOf(j.t, st, clockOf(e.cfg, now))}
	if p, ok := readProof(dir); ok {
		s.Code.Proof, s.Code.Specs = proofOf(p, true), len(p.Specs)
	}
	old, ok, err := prstatus.Read(j.path)
	if err == nil && ok { // magnum prunes old events: a count never goes down
		s.FixLoop.Pushes = max(s.FixLoop.Pushes, old.FixLoop.Pushes)
		s.FixLoop.ThreadsAnswered = max(s.FixLoop.ThreadsAnswered, old.FixLoop.ThreadsAnswered)
	}
	switch {
	case len(stg.envs) > 0:
		s.Stagings = prStagings(stg.envs, old.Stagings)
		s.Staging = s.Stagings[0] // the readers from before stagings render the first
	case ok && (j.t.State != registry.TaskPublished || len(e.cfg.Stagings(j.t.Repo)) > 0):
		// An ended task's last write, or a tick that read no staging status
		// of a repository with envs (its PR's link not read), keeps where
		// its PR stood.
		s.Staging, s.Stagings = old.Staging, old.Stagings
	}
	return s, nil
}

// planOf counts the questions qs asked you that stand (open or answered;
// one a later revision voided was replaced) and those you answered.
func planOf(qs []registry.Question) prstatus.Plan {
	var p prstatus.Plan
	for _, q := range qs {
		switch q.State {
		case registry.QuestionAnswered:
			p.QuestionsAsked++
			p.QuestionsAnswered++
		case registry.QuestionOpen:
			p.QuestionsAsked++
		}
	}
	return p
}

// proofOf is the factory's proof p (ok: read) in the file's words: red on
// the base and green at the head, green at the head (a kind whose proof is
// green), red on the base with the head not green yet, else none.
func proofOf(p Proof, ok bool) prstatus.Proof {
	red := p.Base.Command != ""
	switch {
	case !ok:
		return prstatus.ProofNone
	case p.Passed && red:
		return prstatus.ProofRedToGreen
	case p.Passed:
		return prstatus.ProofGreen
	case red && p.Base.Exit != 0:
		return prstatus.ProofRed
	}
	return prstatus.ProofNone
}

// verdictOf is the local review's end exit in the file's words: clean,
// findings left (an oscillation, the cap, a stop for you), failed (the
// review was unavailable), else none.
func verdictOf(exit string) prstatus.Verdict {
	switch exit {
	case registry.ExitClean:
		return prstatus.VerdictClean
	case registry.ExitOscillation, registry.ExitCap, registry.ExitOperator:
		return prstatus.VerdictFindings
	case registry.ExitUnavailable:
		return prstatus.VerdictFailed
	}
	return prstatus.VerdictNone
}

// fixLoopOf is magnum's fix loop on the PR in the file's words, from its
// row (st: running, its cycle, or fix off) and its record (fix): none
// before a first loop, needs you while the newest loop's end left the PR
// to you and it still needs your attention, else done.
func fixLoopOf(st PRState, fix FixRecord) prstatus.FixLoop {
	fl := prstatus.FixLoop{State: prstatus.FixDone, Cycle: fix.Cycle, Pushes: fix.Pushes, ThreadsAnswered: fix.Threads}
	switch {
	case st.Fixing:
		fl.State, fl.Cycle = prstatus.FixRunning, max(st.Fix.Cycle, 0)
	case st.FixOff:
		fl.State = prstatus.FixOff
	case fix.Loops == 0:
		fl.State = prstatus.FixNone
	case fix.LeftToYou && st.State == store.PRNeedsAttention:
		fl.State = prstatus.FixNeedsYou
	}
	return fl
}

// nowOf is who task t's PR waits on and the task's stage, in the file's
// words: an ended task's end, waiting on nobody; a published one's PR
// waits as its row on the screen says (publishedWait, by magnum's view st
// at c, a closed PR waiting c.grace for a reopen).
func nowOf(t registry.Task, st PRState, c prClock) prstatus.Now {
	switch t.State {
	case registry.TaskDone:
		return prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StageDone}
	case registry.TaskStopped:
		return prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StageStopped}
	case registry.TaskFailed:
		return prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StageFailed}
	}
	return prstatus.Now{Who: whoOf(publishedWait(t, st.Ref, st, c).on.Who), Stage: prstatus.StagePublished}
}

// whoOf is who a published task's row waits on (publishedWait) in the
// file's words: magnum for magnum, its fixer at work and any word of a
// later build.
func whoOf(who string) prstatus.Who {
	switch who {
	case waitsYou:
		return prstatus.WhoYou
	case waitReviewers:
		return prstatus.WhoReviewers
	case waitCI:
		return prstatus.WhoCI
	case waitNothing:
		return prstatus.WhoNone
	}
	return prstatus.WhoMagnum
}

// statusFailed logs the failure err of task id's status file, once until
// a write of it succeeds: the error's words change from one try to the next
// (a refused temporary file has a new random name each time).
func (e *Engine) statusFailed(id int64, err error) {
	if e.statusErrs[id] {
		return
	}
	e.statusErrs[id] = true
	e.log.Warn("a published PR's status file is not written", "task", id, "err", textx.FirstLine(execx.Redact(err.Error())))
}

// statusReadFailed logs a failed read of magnum's registry for the status
// files, once while it stays the same.
func (e *Engine) statusReadFailed(err error) {
	msg := textx.FirstLine(execx.Redact(err.Error()))
	if msg == e.statusReadErr {
		return
	}
	e.statusReadErr = msg
	e.log.Warn("the published PRs' status files keep their content: magnum's registry cannot be read", "err", msg)
}
