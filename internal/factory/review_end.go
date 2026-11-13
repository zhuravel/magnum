package factory

// The local review's end by its round limit or by you (the operator's
// decision of 2026-11-13): [factory.local_review] max_rounds bounds the
// rounds a task runs over its life, every round counted (a cycle of the
// registry: the task's round n is its cycle n), and `magnum factory
// finish-review` (review_finish.go) ends a task's loop now. When a round
// ends at or past the limit, or after your request, no round starts after
// it (endAtRound): a safe stop (unsafeWhy, as cap_go_on's, with no product
// question open) passes the stage with the round's open findings left for
// the ready card's KNOWN row, and
// any other stops for you as at the cap, its words naming the limit or you,
// your retry handing its findings to the writer. review-end.json records
// the end by its cycle before the cycle ends, so a restart decides alike,
// and a pass after it (endedAt) passes the stage at the head it ended at,
// or starts a fresh loop at a head since, whose round ends at the limit
// again.

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
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
)

// The exits of review-summary.json of a loop that ended at the round limit
// or by you, on a safe stop (words of the file only: the cycle's exit in
// the registry is the cap's, or the operator's).
const (
	exitRoundLimit = "round_limit"
	exitByYou      = "ended_by_you"
)

// loopEndFile is review-end.json in the task's directory: the newest end
// of the loop by the round limit or by you (loopEnd).
const loopEndFile = "review-end.json"

// Who ended the loop (loopEnd.By): [factory.local_review] max_rounds, or
// you (finish-review).
const (
	endByLimit = "limit"
	endByYou   = "you"
)

// loopEnd is review-end.json: the loop's end at cycle Cycle by By, the
// rounds the task had run (Rounds, the cycle's number) and max_rounds then
// (Limit), whether the stop was safe (Safe: the stage passed; else it
// stopped for you) and when it was decided (At). Your end carries the time
// of the request it answers (Asked), and one between rounds (Between: at
// the writer's fix of cycle Cycle's findings, no round in flight) the
// NEEDS YOU line of its stop for you (Why).
type loopEnd struct {
	Cycle   int64     `json:"cycle"`
	By      string    `json:"by"`
	Rounds  int       `json:"rounds"`
	Limit   int       `json:"limit,omitempty"`
	Safe    bool      `json:"safe"`
	Between bool      `json:"between,omitempty"`
	Why     string    `json:"why,omitempty"`
	At      time.Time `json:"at"`
	Asked   time.Time `json:"asked,omitzero"`
}

// exit is the exit end's cycle takes in the registry: the cap's for the
// round limit, the operator's for you.
func (end loopEnd) exit() string {
	if end.By == endByYou {
		return registry.ExitOperator
	}
	return registry.ExitCap
}

// words are end's words for a line: "at the round limit (8 rounds)" (with
// the rounds the task ran when they are more: "(8 rounds; it ran 14)"), or
// "when you ended the local review after 3 rounds".
func (end loopEnd) words() string {
	if end.By == endByYou {
		return "when you ended the local review after " + plural(end.Rounds, "round")
	}
	return limitWords(end.Limit, end.Rounds)
}

// readLoopEnd reads review-end.json in dir; ok is false when there is
// none, or it does not parse.
func readLoopEnd(dir string) (loopEnd, bool) {
	var end loopEnd
	b, err := os.ReadFile(filepath.Join(dir, loopEndFile))
	return end, err == nil && json.Unmarshal(b, &end) == nil
}

// endBy is the end of the loop at the end of cycle cy's round, in task
// directory dir: the one recorded for cy (a restart came between, and it
// is decided alike: recorded), else yours when your request waits
// (finish-review), else one by the round limit when cy is the task's
// max_rounds-th round or later; ok is false for none.
func (e *Engine) endBy(dir string, cy registry.Cycle) (end loopEnd, recorded, ok bool) {
	if end, ok := readLoopEnd(dir); ok && end.Cycle == cy.ID {
		return end, true, true
	}
	end = loopEnd{Cycle: cy.ID, Rounds: cy.N, Limit: e.cfg.Factory.LocalReview.MaxRounds, At: e.now()}
	if m, ok := readFinish(dir); ok {
		end.By, end.Asked, end.Limit = endByYou, m.At, 0
		return end, false, true
	}
	if end.Limit > 0 && cy.N >= end.Limit {
		end.By = endByLimit
		return end, false, true
	}
	return loopEnd{}, false, false
}

// limitWords are the words of an end at the round limit: "at the round
// limit (8 rounds)", and the rounds the task ran when they are more ("(8
// rounds; it ran 14)").
func limitWords(limit, rounds int) string {
	s := "at the round limit (" + plural(limit, "round")
	if rounds > limit {
		s += fmt.Sprintf("; it ran %d", rounds)
	}
	return s + ")"
}

// endAtRound ends the loop at the end of cycle cy's round (the last of
// loop; res its result, rr its findings, sum the loop's summary as the
// round left it) by end: a safe stop (unsafeWhy, and no product question
// open: questionsOpen; your retry after the stop asks them first, capFix)
// writes the summary with the round's open findings as its leftovers (a
// change you asked for that the round still finds as a defect: owedChanges;
// exit round_limit, or ended_by_you), records the end, ends the cycle (end.exit) and passes the
// stage; any other records the end and stops for you on the round's
// findings, as at the cap (stopOnFindings), its words naming the limit or
// you. A recorded end (recorded) keeps its verdict on safety. Your request
// it answers goes last.
func (e *Engine) endAtRound(ctx context.Context, t registry.Task, st config.FactoryStage, cy registry.Cycle, loop []registry.Cycle,
	res evalrun.LocalResult, rr roundRead, sum ReviewSummary, end loopEnd, recorded bool) (bool, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	why := cmp.Or(unsafeWhy(dir, loop, res), questionsOpen(len(rr.questions)))
	if !recorded {
		end.Safe = why == ""
	}
	if !end.Safe {
		if err := writeJSON(filepath.Join(dir, loopEndFile), end); err != nil {
			return false, err
		}
		head := func() string {
			return fmt.Sprintf("%s %s (%s; verdict %s), not a safe stop: %s", plural(len(rr.live)+len(rr.questions)+len(rr.doubts), "blocking finding"),
				end.words(), loopTrend(dir, loop), cmp.Or(res.Verdict, "none"), cmp.Or(why, "decided so before a restart"))
		}
		because := "the local review's round limit"
		if end.By == endByYou {
			because = "you ended the local review"
		}
		if err := e.stopOnFindings(ctx, t, st, cy, slices.Concat(rr.live, rr.questions), head, end.exit(), because); err != nil {
			return false, err
		}
		return false, dropFinish(dir, end)
	}
	sum.Exit, sum.Leftovers, sum.TaskRounds = exitRoundLimit, slices.Concat(rr.leftovers, owedChanges(rr.live), rr.questions), end.Rounds
	if end.By == endByYou {
		sum.Exit = exitByYou
	} else {
		sum.Limit = end.Limit
	}
	sum = withSecond(dir, loop, sum)
	if err := writeJSON(filepath.Join(dir, reviewSummaryFile), sum); err != nil {
		return false, err
	}
	if err := writeJSON(filepath.Join(dir, loopEndFile), end); err != nil {
		return false, err
	}
	if err := e.endCycle(ctx, cy, cy.HeadBefore, end.exit()); err != nil {
		return false, err
	}
	e.event(ctx, t.ID, "review", registry.PhaseOK, fmt.Sprintf("the local review ended %s, after round %d (cycle %d), a safe stop (no P0 or P1, "+
		"a verdict that does not block, no finding declined in its loop, no product question open): no round starts, and %s go to the ready card under KNOWN; "+
		"%d declined, %d doubts", strings.TrimPrefix(end.words(), "when "), len(loop), cy.N, plural(len(sum.Leftovers), "open finding"),
		len(sum.Declined), len(sum.Doubts)))
	return true, dropFinish(dir, end)
}

// owedChanges are a round's live findings as a safe stop leaves them for
// the ready card: a product question among them is one you answered "change
// it" that the round still finds (sortRound), a change the writer owes, so
// it is a defect there (knownDefects keeps it and the card's w sends it),
// never one you decided to keep.
func owedChanges(live []ReviewItem) []ReviewItem {
	out := slices.Clone(live)
	for i := range out {
		out[i].Question = false
	}
	return out
}

// questionsOpen is why n product questions still open make a stop at the
// round limit or by you unsafe ("" for none): only you answer them, and
// under KNOWN they would read as decided by you, never asked.
func questionsOpen(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "a product question is open"
	}
	return plural(n, "product question") + " are open"
}

// endedAt is what a pass of the review stage of task t does after the
// loop ended by the round limit or by you at its newest cycle last
// (review-end.json names last): a safe end passes the stage at the head it
// ended at (endPass), and a head since it (QA's fix, a send back's commits)
// starts a fresh loop at once (endFresh), whose round ends at the limit
// again. Your end between rounds that stopped for you moves the task to you
// again when that move was lost (endHeld: toldOfEnd), and your retry after
// it starts a fresh loop (endFresh), since the writer fixed the last
// round's findings already. Any other end that stopped for you is the cap's
// stop (endNone): toldOfStop and capFix take it from there. What a daemon
// that stopped midway left is done first (settleEnd).
func (e *Engine) endedAt(ctx context.Context, t registry.Task, last registry.Cycle) (endAct, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	end, ok := readLoopEnd(dir)
	if !ok || end.Cycle != last.ID || last.Ended.IsZero() {
		return endNone, nil
	}
	if err := e.settleEnd(ctx, dir, last, end); err != nil {
		return endNone, err
	}
	switch {
	case end.Safe && summaryEndsAt(dir, t.HeadSHA):
		return endPass, nil
	case end.Safe:
		return endFresh, nil
	case !end.Between:
		return endNone, nil
	}
	if told, err := e.toldOfEnd(ctx, t, end); err != nil || !told {
		return endHeld, err
	}
	return endFresh, nil
}

// settleEnd does what end, at cycle last, left undone when a daemon
// stopped midway: an end between rounds closes the loop at last (a cycle
// that ended with the loop going on takes end's exit), and your request
// end answered goes.
func (e *Engine) settleEnd(ctx context.Context, dir string, last registry.Cycle, end loopEnd) error {
	if end.Between && last.Exit == "" {
		if err := e.reg.CloseLoop(ctx, last.ID, end.exit()); err != nil {
			return err
		}
	}
	return dropFinish(dir, end)
}

// toldOfEnd reports whether you were told of your end between rounds that
// stopped for you (an alert since it was decided): one not told is moved
// to you now, with its line.
func (e *Engine) toldOfEnd(ctx context.Context, t registry.Task, end loopEnd) (bool, error) {
	evs, err := e.reg.Events(ctx, t.ID, 200)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(evs, func(ev registry.Event) bool { return ev.Kind == "alert" && !ev.At.Before(end.At) }) {
		return true, nil
	}
	e.toOperator(ctx, t, cmp.Or(end.Why, "you ended the local review, and its stop is not safe"), "you ended the local review")
	return false, nil
}

// endAct is what a pass of the review stage does after the loop's end by
// the round limit or by you (endedAt).
type endAct int

const (
	endNone  endAct = iota // no such end, or a stop for you as at the cap: the stage goes on as before
	endPass                // the stage passes
	endFresh               // a fresh loop starts
	endHeld                // the task waits on you
)

// summaryEndsAt reports whether review-summary.json in dir ended the loop
// at the round limit or by you at head.
func summaryEndsAt(dir, head string) bool {
	return summaryExitAt(dir, head, exitRoundLimit, exitByYou)
}

// summaryExitAt reports whether review-summary.json in dir ended the loop
// at head with one of exits.
func summaryExitAt(dir, head string, exits ...string) bool {
	var sum ReviewSummary
	b, err := os.ReadFile(filepath.Join(dir, reviewSummaryFile))
	return err == nil && json.Unmarshal(b, &sum) == nil && slices.Contains(exits, sum.Exit) && head != "" && sum.Head == head
}

// secondHeld is why the task's second opinion, one round more, does not
// run after clean cycle cy (in task directory dir), when it is decided
// (decideSecond) and while it waits to begin (dropSecond): the task ran
// [factory.local_review] max_rounds rounds, or you asked to end its local
// review; "" when it may.
func (e *Engine) secondHeld(dir string, cy registry.Cycle) string {
	if _, ok := readFinish(dir); ok {
		return "you asked to end the local review"
	}
	if limit := e.cfg.Factory.LocalReview.MaxRounds; limit > 0 && cy.N >= limit {
		return fmt.Sprintf("the task ran %s, at [factory.local_review] max_rounds %d", plural(cy.N, "round"), limit)
	}
	return ""
}

// unsafeWhy is why a stop of loop (in task directory dir), whose last
// round's result is res, is not safe to go on from or to pass without you,
// "" when it is: the round has a P0 or P1, new or still open
// (highPriority), its judge's verdict blocks, or the writer declined a
// finding it got in the loop (a reply of a round of it,
// review-reply-<n>.json, with a risk or without). A reply that cannot be
// read is not known to decline nothing.
func unsafeWhy(dir string, loop []registry.Cycle, res evalrun.LocalResult) string {
	switch {
	case highPriority(res):
		return "a P0 or P1 is open"
	case res.Verdict == store.VerdictBlocking:
		return "the judge's verdict blocks"
	}
	for _, cy := range loop {
		b, err := os.ReadFile(filepath.Join(dir, reviewReplyFile(cy.N)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		var rep ReviewReply
		switch {
		case err != nil, json.Unmarshal(b, &rep) != nil:
			return "the writer's reply " + reviewReplyFile(cy.N) + " cannot be read"
		case len(rep.declinedIDs()) > 0:
			return "the writer declined " + strings.Join(slices.Sorted(maps.Keys(rep.declinedIDs())), ", ") + " of " + reviewFile(cy.N)
		}
	}
	return ""
}
