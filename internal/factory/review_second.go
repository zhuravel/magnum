package factory

// The second opinion of the local review: one clean round with one judge
// is noisy (two runs of the same roles on one head disagreed on a P2 in 2 of
// 4 PRs), and the same judge again mostly repeats itself. So after a clean
// round whose judge runs on Codex, on a wide change ([factory.local_review]
// second_judge: "wide", "always" or "never"), one more round runs before the
// stage ends, at the head the clean round reviewed: the same roles with the
// watch's judge_fallback in the judge's place (reviewRoles), once per task.
// Its clean result ends the stage clean; one with findings goes to the
// writer like any round's, within cycles, which do not count it. One that
// never ran (the head moved first, its judge went) or ended without a
// result (unavailable, at the operator) is not the task's one; one whose
// judge goes before it starts ends the stage clean on the round it followed.

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
	"github.com/zhuravel/magnum/internal/scrub"
)

// secondOpinion is review-second.json in the task's directory, the task's
// newest second opinion, written before the clean round's cycle it follows
// ends (decideSecond): that cycle's number (the second opinion runs in the
// next) and its round of the loop, the head it reviewed and why it gets one
// (because), the cycle of an earlier second opinion that ended without a
// result and how (again, againExit), the judge the second opinion runs and
// its agent kind, the judge it takes the place of and its kind, and the
// clean round's summary (Summary: what it left that does not block and its
// doubts the loop's end keeps at that head, withSecond; the whole of it
// ends the stage when the opinion is dropped). Moved is the head the branch
// moved to before it began (voidMoved), Dropped why it does not run, and
// DroppedAt the cycle whose clean end it ended in (the clean round's when it
// was pending, dropSecond; its own when it began and was cut, dropCut);
// either makes it void.
type secondOpinion struct {
	After        int           `json:"after"`
	Round        int           `json:"round"`
	Head         string        `json:"head"`
	Because      string        `json:"because"`
	Again        int           `json:"again,omitempty"`
	AgainExit    string        `json:"again_exit,omitempty"`
	Judge        string        `json:"judge"`
	Kind         string        `json:"kind"`
	Replaces     string        `json:"replaces"`
	ReplacesKind string        `json:"replaces_kind"`
	Summary      ReviewSummary `json:"summary"`
	Moved        string        `json:"moved,omitempty"`
	Dropped      string        `json:"dropped,omitempty"`
	DroppedAt    int           `json:"dropped_at,omitempty"`
}

// cycle is the number of the second opinion's own cycle.
func (so secondOpinion) cycle() int { return so.After + 1 }

// live reports whether the second opinion is not void: it runs, or ran.
func (so secondOpinion) live() bool { return so.Moved == "" && so.Dropped == "" }

// decided is the event of the decision for task key.
func (so secondOpinion) decided(key string) string {
	runs := "a second opinion runs"
	if so.Again > 0 {
		runs = fmt.Sprintf("a second opinion runs again (cycle %d's ended %s)", so.Again, so.AgainExit)
	}
	return fmt.Sprintf("round %d (cycle %d) is clean, and %s: %s, round %d with %s (%s) in place of %s (%s), once for %s", so.Round, so.After,
		so.Because, runs, so.Round+1, so.Judge, so.Kind, so.Replaces, so.ReplacesKind, key)
}

// readSecondOpinion reads the task's second opinion in dir; ok is false when
// the task has none. One that does not parse is one of no cycle: none runs
// for it, and the next decision writes another.
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
// that opinion: a new one only while the task's newest cycle is the clean
// round it follows, ended with the loop going on, and the task is at that
// round's head (settleSecond settles every other pending case first). One
// that began, was cut and runs again in its cycle, whose judge can no
// longer run (secondJudgeGone), runs no more: dropCut ends that cycle clean
// and the stage passes on it at the next pass (ended).
func (e *Engine) secondRound(ctx context.Context, t registry.Task, w *config.Watch, cy *registry.Cycle) (so secondOpinion, second, ended bool,
	err error) {
	so, ok := readSecondOpinion(e.d.Layout.FactoryTaskDir(t.ID))
	switch {
	case !ok || so.Moved != "":
		return so, false, false, nil
	case cy != nil && cy.N != so.cycle(), cy != nil && so.Dropped != "" && so.DroppedAt != cy.N:
		return so, false, false, nil // a fresh loop's round after a void or a pending drop
	case cy != nil:
		why := cmp.Or(so.Dropped, e.secondJudgeGone(w))
		if why == "" {
			return so, true, false, nil
		}
		return so, false, true, e.dropCut(ctx, t, *cy, so, why)
	case !so.live():
		return so, false, false, nil
	}
	cs, err := e.reg.Cycles(ctx, t.ID)
	if err != nil || len(cs) == 0 {
		return so, false, false, err
	}
	newest := cs[len(cs)-1]
	if newest.N != so.After || newest.Ended.IsZero() || newest.Exit != "" || t.HeadSHA != "" && t.HeadSHA != so.Head {
		return so, false, false, nil
	}
	return so, e.secondJudgeGone(w) == "", false, nil
}

// dropCut ends cut second opinion so, whose round runs again in its own
// cycle cy, when its judge can no longer run (why): as a pending one ends
// (dropSecond), with no further round. The drop is written first, its event
// next, then the clean round's summary from the record, then cy ends clean
// at its own head and any wait of the task not on you goes; the loop's end
// is said last. A stop before cy ends finishes the steps when the round
// would start again.
func (e *Engine) dropCut(ctx context.Context, t registry.Task, cy registry.Cycle, so secondOpinion, why string) error {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if so.Dropped == "" {
		so.Dropped, so.DroppedAt = why, cy.N
		if err := writeJSON(filepath.Join(dir, secondOpinionFile), so); err != nil {
			return err
		}
	}
	e.eventOnce(ctx, t, registry.PhaseInfo, fmt.Sprintf("the second opinion after round %d (cycle %d), cut in cycle %d, does not run again: %s, "+
		"so the local review ends clean on round %d", so.Round, so.After, cy.N, so.Dropped, so.Round))
	if err := writeJSON(filepath.Join(dir, reviewSummaryFile), so.Summary); err != nil {
		return err
	}
	if err := e.endCycle(ctx, cy, cy.HeadBefore, registry.ExitClean); err != nil {
		return err
	}
	e.clearWait(ctx, t)
	e.event(ctx, t.ID, "review", registry.PhaseOK, endedText(so.Summary)) // as endLoop says it, after the cycle's end
	return nil
}

// clearWait clears task t's wait unless it is on you: the wait a second
// opinion that does not run had (magnum, a heavy slot, Codex's budget) goes
// with it as the stage passes.
func (e *Engine) clearWait(ctx context.Context, t registry.Task) {
	if t.WaitsWho != waitsYou {
		e.waitOn(ctx, t, "", "")
	}
}

// secondJudgeGone says why second opinion so can no longer run for watch w
// ("" when it can): second_judge is "never" now, or the judges it needs are
// gone (secondJudges: w's judge is not on Codex any more, or its
// judge_fallback is of no other kind).
func (e *Engine) secondJudgeGone(w *config.Watch) string {
	if e.cfg.Factory.LocalReview.SecondJudge == config.FactorySecondJudgeNever {
		return "second_judge is " + config.FactorySecondJudgeNever + " now"
	}
	_, _, why := e.secondJudges(w)
	return why
}

// secondJudges are watch w's judge and the judge of another kind that takes
// its place in a second opinion (otherJudge), with why none can run ("" when
// one can): the watch's judge does not run on Codex, or its judge_fallback
// names no judge of another kind. The decision (decideSecond) and the run
// (secondJudgeGone) ask it alike.
func (e *Engine) secondJudges(w *config.Watch) (judge, other config.Role, why string) {
	judge = e.cfg.JudgeFor(w)
	other, ok := e.otherJudge(w)
	switch {
	case judge.AgentKind() != config.KindCodex:
		why = fmt.Sprintf("the review's judge %s runs on %s, and a second opinion follows a Codex judge's round only", judge.Name,
			cmp.Or(judge.AgentKind(), "no agent"))
	case !ok:
		why = fmt.Sprintf("[pipeline] judge_fallback names no judge of another kind than %s's (codex)", judge.Name)
	}
	return judge, other, why
}

// settleSecond settles task t's pending second opinion at a pass of its
// review stage, while last, the task's newest cycle, is the clean round it
// follows and ended with the loop going on: a branch that moved since voids
// it and starts a fresh loop (voidMoved); at the same head, a judge that can
// no longer run drops it and ends the loop clean on that round (dropSecond).
// ended reports that the loop ended at last, whose exit is clean now.
func (e *Engine) settleSecond(ctx context.Context, t registry.Task, last registry.Cycle) (ended bool, err error) {
	so, ok := readSecondOpinion(e.d.Layout.FactoryTaskDir(t.ID))
	if !ok || so.After != last.N || last.Ended.IsZero() || last.Exit != "" {
		return false, nil
	}
	if so.Moved != "" || t.HeadSHA != "" && t.HeadSHA != so.Head {
		return e.voidMoved(ctx, t, last, so)
	}
	return e.dropSecond(ctx, t, last, so)
}

// dropSecond drops second opinion so, pending at the head of the clean round
// it follows (last), when its judge can no longer run (secondJudgeGone) or
// it was dropped before a stop: the drop is written first, its event next,
// then the clean round's summary, as that round would have written it, and
// the loop ends clean on that round after all (CloseLoop), so the stage
// passes there with no further round, and any wait of the task not on you
// goes; the loop's end is said last, as endLoop says it. A stop before the end finishes the steps at the next
// pass. dropped reports that the loop ended.
func (e *Engine) dropSecond(ctx context.Context, t registry.Task, last registry.Cycle, so secondOpinion) (dropped bool, err error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if so.Dropped == "" {
		why := e.secondJudgeGone(e.cfg.WatchFor(t.Repo))
		if why == "" {
			return false, nil
		}
		so.Dropped, so.DroppedAt = why, so.After
		if err := writeJSON(filepath.Join(dir, secondOpinionFile), so); err != nil {
			return false, err
		}
	}
	e.eventOnce(ctx, t, registry.PhaseInfo, fmt.Sprintf("the second opinion after round %d (cycle %d) does not run: %s, so the local review "+
		"ends clean on round %d", so.Round, so.After, so.Dropped, so.Round))
	sum := so.Summary
	if err := writeJSON(filepath.Join(dir, reviewSummaryFile), sum); err != nil {
		return false, err
	}
	if err := e.reg.CloseLoop(ctx, last.ID, registry.ExitClean); err != nil {
		return false, err
	}
	e.clearWait(ctx, t)
	e.event(ctx, t.ID, "review", registry.PhaseOK, endedText(sum)) // as endLoop says it, after the cycle's end
	return true, nil
}

// voidMoved voids task t's pending second opinion so when the branch moved
// since the clean round it follows (last) reviewed it: a second opinion
// holds at that head only. The void is written first, its event next, and
// last that round's loop ends clean after all (CloseLoop), so a fresh loop
// starts at the new head from round 1; a stop between the steps finishes
// them at the next pass. ended reports that the loop ended at last.
func (e *Engine) voidMoved(ctx context.Context, t registry.Task, last registry.Cycle, so secondOpinion) (ended bool, err error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if so.Moved == "" {
		so.Moved = t.HeadSHA
		if err := writeJSON(filepath.Join(dir, secondOpinionFile), so); err != nil {
			return false, err
		}
	}
	e.eventOnce(ctx, t, registry.PhaseInfo, fmt.Sprintf("the second opinion after round %d (cycle %d) is void: the branch moved from %s to %s before it began, "+
		"so a fresh loop of the local review starts at round 1", so.Round, so.After, short(so.Head), short(so.Moved)))
	if err := e.reg.CloseLoop(ctx, last.ID, registry.ExitClean); err != nil {
		return false, err
	}
	return true, nil
}

// eventOnce writes the review event text, of phase, for task t unless the
// task has one that says it already: what a stop between a write and its
// event left unsaid, said once. A read that fails writes it. The event is
// kept scrubbed (Engine.event), so the text it is compared with is too.
func (e *Engine) eventOnce(ctx context.Context, t registry.Task, phase, text string) {
	evs, err := e.reg.TaskEvents(ctx, t.ID, "review", 0)
	kept := scrub.Text(text)
	if err == nil && slices.ContainsFunc(evs, func(ev registry.Event) bool { return ev.Text == kept }) {
		return
	}
	e.event(ctx, t.ID, "review", phase, text)
}

// secondIn is the index in loop of the cycle of the task's live second
// opinion, -1 when the loop has none.
func secondIn(dir string, loop []registry.Cycle) int {
	so, ok := readSecondOpinion(dir)
	if !ok || !so.live() {
		return -1
	}
	return slices.IndexFunc(loop, func(c registry.Cycle) bool { return c.N == so.cycle() })
}

// withSecond is sum, the end of a loop, with the loop's second opinion
// (secondIn): its judge and its round, and at the head the clean round it
// followed reviewed, that round's leftovers and doubts sum lacks.
func withSecond(dir string, loop []registry.Cycle, sum ReviewSummary) ReviewSummary {
	i := secondIn(dir, loop)
	if i < 0 {
		return sum
	}
	so, _ := readSecondOpinion(dir)
	sum.SecondJudge, sum.SecondRound = so.Judge, i+1
	if so.Head == sum.Head {
		sum.Leftovers, sum.Doubts = mergeItems(sum.Leftovers, so.Summary.Leftovers), mergeItems(sum.Doubts, so.Summary.Doubts)
	}
	return sum
}

// mergeItems is to with each item of from whose fingerprint it lacks.
func mergeItems(to, from []ReviewItem) []ReviewItem {
	for _, f := range from {
		if !slices.ContainsFunc(to, func(x ReviewItem) bool { return x.Fingerprint == f.Fingerprint }) {
			to = append(to, f)
		}
	}
	return to
}

// endClean ends the loop clean on cycle cy, the last of loop, with sum,
// after the task's second opinion when one runs first (decideSecond).
func (e *Engine) endClean(ctx context.Context, t registry.Task, c Copy, cy registry.Cycle, loop []registry.Cycle, sum ReviewSummary) (bool, error) {
	if second, err := e.decideSecond(ctx, t, c, cy, len(loop), sum); err != nil || second {
		return false, err
	}
	return e.endLoop(ctx, t, cy, withSecond(e.d.Layout.FactoryTaskDir(t.ID), loop, sum))
}

// atCap reports whether round `round` of loop, which blocks, is the loop's
// last: [factory.local_review] cycles rounds besides the second opinion
// (secondIn), which runs outside the count, and never more counted rounds
// than config.MaxFactoryCycles. The second opinion's round may be the
// eleventh: evalrun numbers rounds up to evalrun.MaxLocalRound. A round
// past it is a converging loop's extra round (extraRound), at the cap too.
func (e *Engine) atCap(dir string, loop []registry.Cycle, round int) bool {
	return countedRound(dir, loop, round) >= e.capRounds()
}

// capRounds is the loop's cap of counted rounds: [factory.local_review]
// cycles, 1 to config.MaxFactoryCycles.
func (e *Engine) capRounds() int {
	return min(max(1, e.cfg.Factory.LocalReview.Cycles), config.MaxFactoryCycles)
}

// countedRound is round `round` of loop as the cap counts it: the second
// opinion's round (secondIn) runs outside the count.
func countedRound(dir string, loop []registry.Cycle, round int) int {
	if secondIn(dir, loop) >= 0 {
		return round - 1
	}
	return round
}

// secondSpent reports whether task t's second opinion so is spent at clean
// cycle cy: it is live, and its cycle is cy (it ends clean now) or ended
// clean or with findings. A void one is not, nor one whose cycle ended
// unavailable or at the operator: unfinished is that cycle, with its exit.
func (e *Engine) secondSpent(ctx context.Context, t registry.Task, so secondOpinion, cy registry.Cycle) (spent bool, unfinished registry.Cycle, err error) {
	switch {
	case !so.live():
		return false, unfinished, nil
	case cy.N == so.cycle():
		return true, unfinished, nil
	}
	cs, err := e.reg.Cycles(ctx, t.ID)
	if err != nil {
		return false, unfinished, err
	}
	i := slices.IndexFunc(cs, func(c registry.Cycle) bool { return c.N == so.cycle() })
	switch {
	case i < 0:
		return false, unfinished, nil
	case cs[i].Exit == registry.ExitUnavailable || cs[i].Exit == registry.ExitOperator:
		return false, cs[i], nil
	}
	return true, unfinished, nil
}

// decideSecond decides, at the clean end of round `round` (cycle cy; sum its
// summary) of task t's local review in copy c, whether the task's second
// opinion runs before the stage ends. It runs while the task has none spent
// (secondSpent), on a change the round reviewed that is wide (changeWidth)
// unless second_judge is "always" ("never": none), when the watch's judge
// runs on Codex, a judge of another kind takes its place (otherJudge), Codex
// works on the task (else its rounds ran with that judge already) and the
// round is not the last a loop numbers; each of these that fails is an
// event saying so, and a narrow change or "never" none. The opinion is
// written first, its event next, and cy ends with the loop going on: the
// next round is the second opinion (secondRound). A daemon that stopped
// after the write ends cy now, and writes the event if it did not.
func (e *Engine) decideSecond(ctx context.Context, t registry.Task, c Copy, cy registry.Cycle, round int, sum ReviewSummary) (bool, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	so, ok := readSecondOpinion(dir)
	if ok && so.live() && so.After == cy.N { // written before a stop, whatever the setting is now: settleSecond takes it from here
		e.eventOnce(ctx, t, registry.PhaseInfo, so.decided(TaskKey(t)))
		return true, e.endCycle(ctx, cy, cy.HeadBefore, "")
	}
	setting := e.cfg.Factory.LocalReview.SecondJudge
	if setting == config.FactorySecondJudgeNever {
		return false, nil
	}
	var unfinished registry.Cycle
	if ok {
		spent, u, err := e.secondSpent(ctx, t, so, cy)
		if err != nil || spent {
			return false, err
		}
		unfinished = u
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
	judge, fb, why := e.secondJudges(w)
	switch {
	case why != "":
		return skip(why)
	case e.secondHeld(dir, cy) != "":
		return skip(e.secondHeld(dir, cy))
	case t.CodexNever:
		return skip(fmt.Sprintf("Codex never works on %s again, so its rounds ran with %s already", TaskKey(t), fb.Name))
	}
	so = secondOpinion{After: cy.N, Round: round, Head: cy.HeadBefore, Because: because, Again: unfinished.N, AgainExit: unfinished.Exit,
		Judge: fb.Name, Kind: fb.AgentKind(), Replaces: judge.Name, ReplacesKind: judge.AgentKind(), Summary: sum}
	if err := writeJSON(filepath.Join(dir, secondOpinionFile), so); err != nil {
		return false, err
	}
	if err := e.stopBeforeSecond("event"); err != nil {
		return false, err
	}
	e.event(ctx, t.ID, "review", registry.PhaseInfo, so.decided(TaskKey(t)))
	if err := e.stopBeforeSecond("end"); err != nil {
		return false, err
	}
	return true, e.endCycle(ctx, cy, cy.HeadBefore, "")
}

// stopBeforeSecond is the test's seam before step of decideSecond
// (beforeSecond); nil in a daemon.
func (e *Engine) stopBeforeSecond(step string) error {
	if e.beforeSecond == nil {
		return nil
	}
	return e.beforeSecond(step)
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
