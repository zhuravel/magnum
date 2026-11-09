package factory

// The second opinion of the local review: one clean round with one judge
// is noisy (two runs of the same roles on one head disagreed on a P2 in 2 of
// 4 PRs), and the same judge again mostly repeats itself. So after a clean
// round whose judge runs on Codex, on a wide change ([factory.local_review]
// second_judge: "wide", "always" or "never"), one more round runs before the
// stage ends: the same roles with the watch's judge_fallback in the judge's
// place (reviewRoles), once per task. Its clean result ends the stage clean;
// one with findings goes to the writer like any round's, within cycles,
// which do not count it.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// secondOpinion is review-second.json in the task's directory, written
// once, before the clean round's cycle it follows ends (decideSecond): that
// cycle's number (the second opinion runs in the next), the head it
// reviewed, the judge the second opinion runs and its agent kind, the judge
// it takes the place of and its kind, and what the clean round left that
// does not block, which the loop's end keeps at that head (withSecond).
type secondOpinion struct {
	After        int          `json:"after"`
	Head         string       `json:"head"`
	Judge        string       `json:"judge"`
	Kind         string       `json:"kind"`
	Replaces     string       `json:"replaces"`
	ReplacesKind string       `json:"replaces_kind"`
	Leftovers    []ReviewItem `json:"leftovers,omitempty"`
}

// cycle is the number of the second opinion's own cycle.
func (so secondOpinion) cycle() int { return so.After + 1 }

// readSecondOpinion reads the task's second opinion in dir; ok is false when
// the task has none. One that does not parse counts as one that ran at no
// known cycle: the task gets no other.
func readSecondOpinion(dir string) (so secondOpinion, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, secondOpinionFile))
	if errors.Is(err, fs.ErrNotExist) {
		return so, false
	}
	if err != nil || json.Unmarshal(b, &so) != nil {
		return secondOpinion{After: -1}, true
	}
	return so, true
}

// secondRound reports whether the round to start in cycle cy (nil: a new
// one, numbered after task t's newest) is the task's second opinion, with
// that opinion.
func (e *Engine) secondRound(ctx context.Context, t registry.Task, cy *registry.Cycle) (secondOpinion, bool, error) {
	so, ok := readSecondOpinion(e.d.Layout.FactoryTaskDir(t.ID))
	switch {
	case !ok:
		return so, false, nil
	case cy != nil:
		return so, cy.N == so.cycle(), nil
	}
	cs, err := e.reg.Cycles(ctx, t.ID)
	if err != nil {
		return so, false, err
	}
	return so, len(cs) > 0 && cs[len(cs)-1].N == so.After, nil
}

// secondIn is the index in loop of the second opinion's cycle, -1 when the
// loop has none.
func secondIn(dir string, loop []registry.Cycle) int {
	so, ok := readSecondOpinion(dir)
	if !ok {
		return -1
	}
	return slices.IndexFunc(loop, func(c registry.Cycle) bool { return c.N == so.cycle() })
}

// withSecond is sum, the end of a loop, with the loop's second opinion
// (secondIn): its judge and its round, and at the head the clean round it
// followed reviewed, that round's leftovers sum lacks.
func withSecond(dir string, loop []registry.Cycle, sum ReviewSummary) ReviewSummary {
	i := secondIn(dir, loop)
	if i < 0 {
		return sum
	}
	so, _ := readSecondOpinion(dir)
	sum.SecondJudge, sum.SecondRound = so.Judge, i+1
	if so.Head != sum.Head {
		return sum
	}
	for _, l := range so.Leftovers {
		if !slices.ContainsFunc(sum.Leftovers, func(x ReviewItem) bool { return x.Fingerprint == l.Fingerprint }) {
			sum.Leftovers = append(sum.Leftovers, l)
		}
	}
	return sum
}

// endClean ends the loop clean on cycle cy, the last of loop, with sum,
// after the task's second opinion when one runs first (decideSecond).
func (e *Engine) endClean(ctx context.Context, t registry.Task, c Copy, cy registry.Cycle, loop []registry.Cycle, sum ReviewSummary) (bool, error) {
	if second, err := e.decideSecond(ctx, t, c, cy, len(loop), sum.Leftovers); err != nil || second {
		return false, err
	}
	return e.endLoop(ctx, t, cy, withSecond(e.d.Layout.FactoryTaskDir(t.ID), loop, sum))
}

// atCap reports whether round `round` of loop, which blocks, is the loop's
// last: [factory.local_review] cycles rounds besides the second opinion
// (secondIn), which runs outside the count, and never past the rounds a
// loop numbers (config.MaxFactoryCycles).
func (e *Engine) atCap(dir string, loop []registry.Cycle, round int) bool {
	counted := round
	if secondIn(dir, loop) >= 0 {
		counted--
	}
	return counted >= max(1, e.cfg.Factory.LocalReview.Cycles) || round >= config.MaxFactoryCycles
}

// decideSecond decides, at the clean end of round `round` (cycle cy, which
// left leftovers) of task t's local review in copy c, whether the task's
// second opinion runs before the stage ends. It runs once per task, on a
// change the round reviewed that is wide (changeWidth) unless second_judge
// is "always" ("never": none), when the watch's judge runs on Codex, a
// judge of another kind takes its place (otherJudge), Codex works on the
// task (else its rounds ran with that judge already) and the round is not
// the last a loop numbers; each of these that fails is an event saying so,
// and a narrow change or "never" none. The opinion is written first, an
// event says it, and cy ends with the loop going on: the next round is the
// second opinion (secondRound). A daemon that stopped between the write and
// the end of cy ends cy now.
func (e *Engine) decideSecond(ctx context.Context, t registry.Task, c Copy, cy registry.Cycle, round int, leftovers []ReviewItem) (bool, error) {
	setting := e.cfg.Factory.LocalReview.SecondJudge
	if setting == config.FactorySecondJudgeNever {
		return false, nil
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if so, ok := readSecondOpinion(dir); ok {
		if so.After != cy.N {
			return false, nil // once per task
		}
		return true, e.endCycle(ctx, cy, cy.HeadBefore, "") // written before a stop
	}
	skip := func(why string) (bool, error) {
		e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("no second opinion after round %d (cycle %d): %s", round, cy.N, why))
		return false, nil
	}
	because := "second_judge is " + config.FactorySecondJudgeAlways
	if setting != config.FactorySecondJudgeAlways {
		wide, why, err := e.reviewedWidth(ctx, t, c, cy.HeadBefore)
		if err != nil || !wide {
			return false, err
		}
		because = why
	}
	w := e.cfg.WatchFor(t.Repo)
	if w == nil {
		return false, nil // startRound needed one: no round ran without it
	}
	judge := e.cfg.JudgeFor(w)
	fb, ok := e.otherJudge(w)
	switch {
	case judge.AgentKind() != config.KindCodex:
		return skip(fmt.Sprintf("the review's judge %s runs on %s, and a second opinion follows a Codex judge's round only", judge.Name,
			cmp.Or(judge.AgentKind(), "no agent")))
	case !ok:
		return skip(fmt.Sprintf("[pipeline] judge_fallback names no judge of another kind than %s's (codex): set it, or the watch's, to one",
			judge.Name))
	case t.CodexNever:
		return skip(fmt.Sprintf("Codex never works on %s again, so its rounds ran with %s already", TaskKey(t), fb.Name))
	case round >= config.MaxFactoryCycles:
		return skip(fmt.Sprintf("round %d would pass the %d rounds of a loop", round+1, config.MaxFactoryCycles))
	}
	so := secondOpinion{After: cy.N, Head: cy.HeadBefore, Judge: fb.Name, Kind: fb.AgentKind(), Replaces: judge.Name,
		ReplacesKind: judge.AgentKind(), Leftovers: leftovers}
	if err := writeJSON(filepath.Join(dir, secondOpinionFile), so); err != nil {
		return false, err
	}
	e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("round %d (cycle %d) is clean, and %s: a second opinion runs, round %d with %s (%s) "+
		"in place of %s (%s), once for %s", round, cy.N, because, round+1, so.Judge, so.Kind, so.Replaces, so.ReplacesKind, TaskKey(t)))
	if e.beforeSecond != nil {
		if err := e.beforeSecond(); err != nil {
			return false, err
		}
	}
	return true, e.endCycle(ctx, cy, cy.HeadBefore, "")
}

// reviewedWidth is the width (changeWidth) of the change task t's round
// reviewed at head in copy c: the files head changes against its merge base
// with the repository's base.
func (e *Engine) reviewedWidth(ctx context.Context, t registry.Task, c Copy, head string) (bool, string, error) {
	repo, err := e.factoryRepo(t)
	if err != nil {
		return false, "", err
	}
	changes, err := e.d.Git.ChangedFiles(ctx, c.Path, "origin/"+baseOf(repo), head)
	if err != nil {
		return false, "", err
	}
	files := make([]string, 0, len(changes))
	for _, ch := range changes {
		files = append(files, ch.Path)
	}
	wide, why := changeWidth("the change", files)
	return wide, why, nil
}
