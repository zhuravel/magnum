package factory

// `magnum factory finish-review <task>` and the factory screen's S: your
// request to end a task's local review now (ReqFinishReview). The daemon
// keeps it in review-finish.json until an end answers it (review_end.go):
// the round in flight finishes, never cut, and no round starts after it;
// its end, or, between rounds, the writer's fix of the last round's
// findings (finishBetween), ends the stage under the round limit's rule: a
// safe stop goes to the ready card, which says the review was ended by you,
// any other waits on you. A daemon that stops anywhere between decides
// alike after its restart.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
)

// ReqFinishReview is your request to end a task's local review now
// (FinishReviewPayload).
const ReqFinishReview = "finish_review"

// FinishReviewPayload is a ReqFinishReview: end task TaskID's local review
// now. Surface is where you asked (the screen, the CLI), for the record.
type FinishReviewPayload struct {
	TaskID  int64
	Surface string
}

// finishFile is review-finish.json in the task's directory: your request
// to end its local review (finishMark), until an end answers it.
const finishFile = "review-finish.json"

// finishMark is review-finish.json: when you asked, and where.
type finishMark struct {
	At      time.Time `json:"at"`
	Surface string    `json:"surface,omitempty"`
}

// readFinish reads review-finish.json in dir; ok is false when there is
// none, or it does not parse.
func readFinish(dir string) (finishMark, bool) {
	var m finishMark
	b, err := os.ReadFile(filepath.Join(dir, finishFile))
	return m, err == nil && json.Unmarshal(b, &m) == nil
}

// dropFinish removes your request in dir that end answered: an end of
// yours answers the one it was decided on (a request you made since
// stays); an end by the limit answers none.
func dropFinish(dir string, end loopEnd) error {
	m, ok := readFinish(dir)
	if !ok || end.By != endByYou || !m.At.Equal(end.Asked) {
		return nil
	}
	return dropFinishFile(dir)
}

// dropFinishFile removes review-finish.json in dir, if any.
func dropFinishFile(dir string) error {
	if err := os.Remove(filepath.Join(dir, finishFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// reviewStageOf is task t's current stage under cfg when it is a review
// stage of its kind.
func reviewStageOf(cfg *config.Config, t registry.Task) (config.FactoryStage, bool) {
	if cfg == nil || t.Stage == "" {
		return config.FactoryStage{}, false
	}
	kind := cfg.Factory.Kinds[t.Kind]
	i := stageIndex(kind, t.Stage)
	if i < 0 || kind.Stages[i].Type != config.FactoryStageReview {
		return config.FactoryStage{}, false
	}
	return kind.Stages[i], true
}

// FinishReviewRefusal is why finish-review refuses task t under cfg, nil
// when it goes on: the task is live (at work, or waiting on someone else)
// at a review stage of its kind. A task that waits on you is refused (your
// retry or your answers come first), as is one done, stopped, published,
// failed or queued, or at another stage.
func FinishReviewRefusal(cfg *config.Config, t registry.Task) error {
	key := TaskKey(t)
	_, review := reviewStageOf(cfg, t)
	switch {
	case t.State == registry.TaskNeedsYou && review:
		return fmt.Errorf("%s waits on you (%s): let it go on (`magnum factory retry %d`), answer it, or drop it; finish-review ends a "+
			"local review at work", key, cmp.Or(t.WaitsWhat, "its local review stopped"), t.ID)
	case !isLive(t) && t.State != registry.TaskNeedsYou:
		return fmt.Errorf("%s is %s: it has no local review at work to end", key, t.State)
	case !review:
		return fmt.Errorf("%s is at its %s stage, not in its local review: there is no local review to end", key, cmp.Or(t.Stage, "first"))
	}
	return nil
}

// finishThen is what follows the round finish-review lets finish, as its
// plan says it.
const finishThen = "no round starts after it. A safe stop (no P0 or P1, a verdict that does not block, no finding declined in its loop, " +
	"no product question open) goes to the ready card with its open findings under KNOWN, the card saying the review was ended by you; " +
	"any other waits on you"

// finishCleanMerge is what the plan at a head no round reviewed says of a
// head that only merges the base cleanly into the head the review passed
// (mergedBase): it gets no round, and the stage passes at once with the
// review's end carried there, not one you ended.
const finishCleanMerge = ". A head that only merges the base cleanly into the head the review passed gets no round: the stage passes at once, " +
	"and the review's end before the merge holds there, the card saying the base merged cleanly"

// FinishReviewPlan is what finish-review does to task t now, read from reg
// and the task's directory under l, as its y/N, its --dry-run, the
// screen's question and the daemon's answer say it: the round in flight
// finishes (or is read, or the writer's fix of its findings finishes), or,
// between rounds, the review ends at once on the writer's fix of the last
// round's findings, which no round reviews; a head no round reviewed yet
// gets its round first, but for a clean merge of the base into the head the
// review passed, which gets none (finishCleanMerge: the review passes there
// as before the merge). Then no round starts. A second opinion that has
// not begun after a clean round does not run, and the review ends clean on
// that round (secondPending, dropSecond).
func FinishReviewPlan(ctx context.Context, reg *registry.Registry, l paths.Layout, t registry.Task) (string, error) {
	cycles, err := reg.Cycles(ctx, t.ID)
	if err != nil {
		return "", err
	}
	dir := l.FactoryTaskDir(t.ID)
	now := "no round of its local review ran yet: its first round runs and finishes, then"
	plan, after := "", ""
	if n := len(cycles); n > 0 {
		last := cycles[n-1]
		fixing := fmt.Sprintf("the writer fixes round %d's findings: its fix finishes, the review ends at once on it (FIXED AFTER on "+
			"the card), and", last.N)
		switch open := last.Ended.IsZero(); {
		case !open && secondPending(dir, last, t.HeadSHA):
			plan = fmt.Sprintf("round %d of its local review ended clean, and its second opinion, one round more, has not begun: it does not "+
				"run, and the review ends clean on round %d at once", last.N, last.N)
		case open && fsx.Exists(filepath.Join(dir, reviewFile(last.N))):
			now = fixing
		case open && fsx.Exists(filepath.Join(dir, reviewRoundFile(last.N))):
			now = fmt.Sprintf("round %d of its local review ended: it is read, then", last.N)
		case open:
			now = fmt.Sprintf("round %d of its local review runs: it finishes, then", last.N)
		case fixedAtHead(dir, last, t.HeadSHA):
			now = fmt.Sprintf("no round runs: the review ends at once, on the writer's fix of round %d's findings, which no round "+
				"reviews (FIXED AFTER on the card), and", last.N)
		case stopFixDue(dir, last, t.HeadSHA):
			now = fixing
		default:
			now, after = "no round reviewed its head yet: one round runs and finishes, then", finishCleanMerge
		}
	}
	if plan == "" {
		plan = now + " " + finishThen + after
	}
	if m, ok := readFinish(dir); ok {
		plan += fmt.Sprintf(" (you asked already, at %s)", m.At.Local().Format("Jan 2 15:04"))
	}
	return plan, nil
}

// FinishReviewQuestion is the y/N of finish-review on the task of key,
// whose plan (FinishReviewPlan) it names: the CLI's and the screen's S's.
func FinishReviewQuestion(key, plan string) string {
	return "End " + key + "'s local review: " + plan + "?"
}

// secondPending reports whether the task's second opinion (review-second.json
// in dir) waits to begin after ended cycle last, the clean round it follows,
// at head: a request to end the review drops it (dropSecond).
func secondPending(dir string, last registry.Cycle, head string) bool {
	so, ok := readSecondOpinion(dir)
	return ok && so.Moved == "" && so.After == last.N && last.Exit == "" && (head == "" || head == so.Head)
}

// fixedAtHead reports whether the writer's fix of ended cycle last's
// findings closed at head (task directory dir): a fixing round's cycle
// (its review-<n>.json for the writer; the clean round a second opinion
// follows has none) ends at the head the factory's checks passed the fix
// at (closeFix), and a stop's at its own head, its fix's head in
// review-stop.json.
func fixedAtHead(dir string, last registry.Cycle, head string) bool {
	switch last.Exit {
	case "":
		return head != "" && last.HeadAfter == head && fsx.Exists(filepath.Join(dir, reviewFile(last.N)))
	case registry.ExitCap, registry.ExitOperator:
		s, ok := readReviewStop(dir)
		return ok && s.Cycle == last.ID && s.Fixed && head != "" && s.FixedAt == head
	}
	return false
}

// stopFixDue reports whether the writer's fix of the findings of ended
// cycle last, a stop at the cap or at the loop's end that you let go on
// (review-stop.json names it, not fixed yet), comes before any round: the
// task's head is the one its round reviewed (capFix).
func stopFixDue(dir string, last registry.Cycle, head string) bool {
	s, ok := readReviewStop(dir)
	return (last.Exit == registry.ExitCap || last.Exit == registry.ExitOperator) && ok && s.Cycle == last.ID && !s.Fixed &&
		head == last.HeadBefore && fsx.Exists(filepath.Join(dir, reviewFile(last.N)))
}

// finishReview is your finish-review (ReqFinishReview): a task the guard
// lets go on (FinishReviewRefusal) keeps your request in its directory,
// and the answer is the plan (FinishReviewPlan). A request made already is
// kept as it was.
func (e *Engine) finishReview(ctx context.Context, p FinishReviewPayload) (string, error) {
	t, err := e.reg.TaskByID(ctx, p.TaskID)
	if err != nil {
		return "", err
	}
	if err := FinishReviewRefusal(e.cfg, t); err != nil {
		return "", err
	}
	plan, err := FinishReviewPlan(ctx, e.reg, e.d.Layout, t)
	if err != nil {
		return "", err
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if _, ok := readFinish(dir); !ok {
		if err := writeJSON(filepath.Join(dir, finishFile), finishMark{At: e.now(), Surface: p.Surface}); err != nil {
			return "", err
		}
		e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("you asked to end the local review (%s): %s", cmp.Or(p.Surface, "cli"), plan))
	}
	return TaskKey(t) + ": " + plan, nil
}

// finishBetween ends task t's local review on your request between rounds
// (handled; done when the stage passes): the newest of cycles ended, and
// the writer's fix of its findings closed at the task's head
// (fixedAtHead). Its round's result is read again (readRound: the registry
// has its findings, and its outcomes since), and the loop through it
// decides (unsafeWhy). A safe stop passes the stage at the writer's head:
// the findings it fixed are the summary's FixedAfter, which no round
// reviewed, the ones it left open its leftovers (exit ended_by_you); the
// summary goes first, the end next, then the loop closes at that cycle (a
// cycle that ended with the loop going on takes the operator's exit). Any
// other stops for you with its line, and your retry starts a fresh loop
// (endedAt). Anything else (no request, a round in flight, a head no round
// reviewed) is not handled: the round runs, and its end decides.
func (e *Engine) finishBetween(ctx context.Context, t registry.Task, cycles []registry.Cycle) (handled, done bool, err error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	m, ok := readFinish(dir)
	if !ok || len(cycles) == 0 {
		return false, false, nil
	}
	last := cycles[len(cycles)-1]
	if last.Ended.IsZero() || !fixedAtHead(dir, last, t.HeadSHA) {
		return false, false, nil
	}
	res, ok, err := readRoundResult(dir, last.N)
	if err != nil || !ok {
		return false, false, err
	}
	rf, ok, err := readReviewFile(dir, last.N)
	if err != nil || !ok {
		return false, false, err
	}
	blocks, unavailable, why := Blocks(res)
	if unavailable {
		return false, false, nil
	}
	rr, err := e.readRound(ctx, t, last, res, blocks, why)
	if err != nil {
		return false, false, err
	}
	loop := append(loopCycles(cycles[:len(cycles)-1]), last)
	end := loopEnd{Cycle: last.ID, By: endByYou, Rounds: last.N, Between: true, At: e.now(), Asked: m.At}
	unsafe := unsafeWhy(dir, loop, res)
	end.Safe = unsafe == ""
	if !end.Safe {
		end.Why = fmt.Sprintf("you ended the local review after %s, at the writer's fix of round %d's findings (%s), which no round "+
			"reviewed; not a safe stop: %s. Let it go on (a round reviews the fix) or drop it", plural(last.N, "round"), last.N, reviewFile(last.N), unsafe)
		if err := writeJSON(filepath.Join(dir, loopEndFile), end); err != nil {
			return true, false, err
		}
		if err := e.settleEnd(ctx, dir, last, end); err != nil {
			return true, false, err
		}
		e.event(ctx, t.ID, "review", registry.PhaseFail, end.Why)
		e.toOperator(ctx, t, end.Why, "you ended the local review")
		return true, false, nil
	}
	sum := ReviewSummary{Exit: exitByYou, Rounds: len(loop), Head: t.HeadSHA, Doubts: rr.doubts, Missing: loopMissing(dir, loop), TaskRounds: last.N}
	sum.FixedAfter, sum.Leftovers = fixedAfter(dir, last.N, rf, rr.leftovers)
	sum.Declined = slices.SortedFunc(maps.Values(readDeclined(dir)), func(a, b ReviewItem) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
	sum = withSecond(dir, loop, sum)
	if err := writeJSON(filepath.Join(dir, reviewSummaryFile), sum); err != nil {
		return true, false, err
	}
	if err := writeJSON(filepath.Join(dir, loopEndFile), end); err != nil {
		return true, false, err
	}
	if err := e.settleEnd(ctx, dir, last, end); err != nil {
		return true, false, err
	}
	e.event(ctx, t.ID, "review", registry.PhaseOK, fmt.Sprintf("you ended the local review after %s, at the writer's fix of round %d's findings "+
		"at %s, a safe stop: no round starts, %s fixed after its last round (not reviewed again) and %s go to the ready card", plural(last.N, "round"),
		last.N, short(t.HeadSHA), plural(len(sum.FixedAfter), "finding"), plural(len(sum.Leftovers), "open finding")))
	return true, true, nil
}
