package factory

// The local review's round limit (the operator's decision of 2026-11-13):
// [factory.local_review] max_rounds bounds the rounds a task runs over its
// life, every round counted (a cycle of the registry: the task's round n is
// its cycle n). When a round ends at or past it, no round starts after it
// (endAtRound): a safe stop (unsafeWhy, as cap_go_on's) passes the stage
// with the round's open findings left for the ready card's KNOWN row, and
// any other stops for you as at the cap, its words naming the limit, your
// retry handing its findings to the writer. review-end.json records the end
// by its cycle before the cycle ends, so a restart decides alike, and a
// pass after it (endedAt) passes the stage at the head it ended at, or
// starts a fresh loop at a head since, whose round ends at the limit again.

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

// exitRoundLimit is the exit of review-summary.json of a loop that ended
// at the round limit (a word of the file only: the cycle's exit in the
// registry is the cap's).
const exitRoundLimit = "round_limit"

// loopEndFile is review-end.json in the task's directory: the newest end
// of the loop by the round limit (loopEnd).
const loopEndFile = "review-end.json"

// endByLimit is the loopEnd.By of an end at [factory.local_review]
// max_rounds.
const endByLimit = "limit"

// loopEnd is review-end.json: the loop's end at cycle Cycle by By (the
// round limit), the rounds the task had run (Rounds, the cycle's number)
// and max_rounds then (Limit), whether the stop was safe (Safe: the stage
// passed; else it stopped for you) and when it was decided (At).
type loopEnd struct {
	Cycle  int64     `json:"cycle"`
	By     string    `json:"by"`
	Rounds int       `json:"rounds"`
	Limit  int       `json:"limit,omitempty"`
	Safe   bool      `json:"safe"`
	At     time.Time `json:"at"`
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
// is decided alike), else one by the round limit when cy is the task's
// max_rounds-th round or later; ok is false for none.
func (e *Engine) endBy(dir string, cy registry.Cycle) (end loopEnd, recorded, ok bool) {
	if end, ok := readLoopEnd(dir); ok && end.Cycle == cy.ID {
		return end, true, true
	}
	if limit := e.cfg.Factory.LocalReview.MaxRounds; limit > 0 && cy.N >= limit {
		return loopEnd{Cycle: cy.ID, By: endByLimit, Rounds: cy.N, Limit: limit, At: e.now()}, false, true
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
// round left it) by end: a safe stop (unsafeWhy) writes the summary with
// the round's open findings as its leftovers (exit round_limit), records
// the end, ends the cycle at the cap and passes the stage; any other
// records the end and stops for you on the round's findings, as at the cap
// (stopOnFindings), its words naming the limit. A recorded end (recorded)
// keeps its verdict on safety.
func (e *Engine) endAtRound(ctx context.Context, t registry.Task, st config.FactoryStage, cy registry.Cycle, loop []registry.Cycle,
	res evalrun.LocalResult, rr roundRead, sum ReviewSummary, end loopEnd, recorded bool) (bool, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	why := unsafeWhy(dir, loop, res)
	if !recorded {
		end.Safe = why == ""
	}
	at := limitWords(end.Limit, end.Rounds)
	if !end.Safe {
		if err := writeJSON(filepath.Join(dir, loopEndFile), end); err != nil {
			return false, err
		}
		head := func() string {
			return fmt.Sprintf("%s %s (%s; verdict %s), not a safe stop: %s", plural(len(rr.live)+len(rr.questions)+len(rr.doubts), "blocking finding"),
				at, loopTrend(dir, loop), cmp.Or(res.Verdict, "none"), cmp.Or(why, "decided so before a restart"))
		}
		return false, e.stopOnFindings(ctx, t, st, cy, slices.Concat(rr.live, rr.questions), head, registry.ExitCap, "the local review's round limit")
	}
	sum.Exit, sum.Leftovers = exitRoundLimit, slices.Concat(rr.leftovers, rr.live, rr.questions)
	sum.Limit, sum.TaskRounds = end.Limit, end.Rounds
	sum = withSecond(dir, loop, sum)
	if err := writeJSON(filepath.Join(dir, reviewSummaryFile), sum); err != nil {
		return false, err
	}
	if err := writeJSON(filepath.Join(dir, loopEndFile), end); err != nil {
		return false, err
	}
	if err := e.endCycle(ctx, cy, cy.HeadBefore, registry.ExitCap); err != nil {
		return false, err
	}
	e.event(ctx, t.ID, "review", registry.PhaseOK, fmt.Sprintf("the local review ended %s after round %d (cycle %d), a safe stop (no P0 or P1, "+
		"a verdict that does not block, no finding declined in its loop): no round starts, and %s go to the ready card under KNOWN; "+
		"%d declined, %d doubts", at, len(loop), cy.N, plural(len(sum.Leftovers), "open finding"), len(sum.Declined), len(sum.Doubts)))
	return true, nil
}

// endedAt is what a pass of the review stage of task t does after the
// loop ended by the round limit at its newest cycle last (review-end.json
// names last): a safe end passes the stage at the head it ended at
// (endPass), and a head since it (QA's fix, a send back's commits) starts
// a fresh loop at once (endFresh), whose round ends at the limit again. An
// end that stopped for you is the cap's stop (endNone): toldOfStop and
// capFix take it from there.
func (e *Engine) endedAt(t registry.Task, last registry.Cycle) endAct {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	end, ok := readLoopEnd(dir)
	switch {
	case !ok || end.Cycle != last.ID || last.Ended.IsZero() || !end.Safe:
		return endNone
	case summaryEndsAt(dir, t.HeadSHA):
		return endPass
	}
	return endFresh
}

// endAct is what a pass of the review stage does after the loop's end by
// the round limit (endedAt).
type endAct int

const (
	endNone  endAct = iota // no such end: the stage goes on as before
	endPass                // the stage passes
	endFresh               // a fresh loop starts
)

// summaryEndsAt reports whether review-summary.json in dir ended the loop
// at the round limit at head.
func summaryEndsAt(dir, head string) bool {
	var sum ReviewSummary
	b, err := os.ReadFile(filepath.Join(dir, reviewSummaryFile))
	return err == nil && json.Unmarshal(b, &sum) == nil && sum.Exit == exitRoundLimit && head != "" && sum.Head == head
}

// secondHeld is why the task's second opinion, one round more, does not
// run after clean cycle cy: the task ran [factory.local_review] max_rounds
// rounds; "" when it may.
func (e *Engine) secondHeld(cy registry.Cycle) string {
	if limit := e.cfg.Factory.LocalReview.MaxRounds; limit > 0 && cy.N >= limit {
		return fmt.Sprintf("the task ran %s, at [factory.local_review] max_rounds %d, so no round starts", plural(cy.N, "round"), limit)
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
