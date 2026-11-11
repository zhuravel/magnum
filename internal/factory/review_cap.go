package factory

// The local review's cap (spec 6.5): a round at [factory.local_review]
// cycles that still blocks either goes on by itself, while the loop
// converges, for at most extra_cycles rounds (extraRound), or stops the
// loop for you (stopAtCap) with its findings, the loop's trend and the
// judge's verdict on the NEEDS YOU line and the task card. Its findings are
// written for the writer as a fixing round's are, and your "go on" at the
// head the cap round reviewed hands them to the writer before a fresh loop
// starts (capFix).

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// extraRound reports whether round `round` of loop, at the cap and
// blocking with `blocking` findings for the writer (res its result), goes
// on by itself, which extra round the next one is (next, from 1) and how
// many the loop may run (extra): [factory.local_review] extra_cycles, within
// config.MaxFactoryCycles counted rounds. It goes on while the loop
// converges: no P0 or P1 among the round's findings (new or still open),
// fewer findings for the writer than the round before it handed on, and a
// judge's verdict that does not block.
func (e *Engine) extraRound(dir string, loop []registry.Cycle, round int, res evalrun.LocalResult, blocking int) (next, extra int, ok bool) {
	limit := e.capRounds()
	extra = min(limit+max(e.cfg.Factory.LocalReview.ExtraCycles, 0), config.MaxFactoryCycles) - limit
	used := countedRound(dir, loop, round) - limit // the extra rounds this loop ran: this one among them, past the cap
	switch {
	case used >= extra, len(loop) < 2, res.Verdict == store.VerdictBlocking, highPriority(res):
		return 0, extra, false
	}
	prev, had, err := readReviewFile(dir, loop[len(loop)-2].N)
	if err != nil || !had || blocking >= len(prev.Findings) {
		return 0, extra, false
	}
	return used + 1, extra, true
}

// highPriority reports whether round result res has a P0 or a P1 finding:
// counted new or still open, or among its findings (not a simplification).
func highPriority(res evalrun.LocalResult) bool {
	if res.Counts[0]+res.Counts[1] > 0 || (res.OpenCounts != nil && res.OpenCounts[0]+res.OpenCounts[1] > 0) {
		return true
	}
	return slices.ContainsFunc(res.Findings, func(f evalrun.LocalFinding) bool {
		return !f.Simplification && (f.Severity == "P0" || f.Severity == "P1")
	})
}

// stopAtCap stops the loop at its cap on cycle cy, the last of loop (its
// result res): its live findings and questions go to review-<n>.json, as a
// fixing round's do, for the fix turn of your "go on" at its head (capFix),
// and you get the count (doubts among it), the trend of the loop's blocking
// and P1 counts, the judge's verdict and the findings, on the NEEDS YOU line
// and, from review-stop.json, the task card.
func (e *Engine) stopAtCap(ctx context.Context, t registry.Task, st config.FactoryStage, cy registry.Cycle, loop []registry.Cycle,
	res evalrun.LocalResult, live, questions, doubts []ReviewItem) error {
	rf, err := e.writeReview(ctx, t, st, cy, slices.Concat(live, questions))
	if err != nil {
		return err
	}
	head := fmt.Sprintf("%s after %s (%s; verdict %s)", plural(len(live)+len(questions)+len(doubts), "blocking finding"),
		plural(len(loop), "review round"), loopTrend(e.d.Layout.FactoryTaskDir(t.ID), loop), cmp.Or(res.Verdict, "none"))
	s := reviewStop{Why: head + ": " + itemsLine(rf.Findings), Head: head, Findings: rf.Findings}
	return e.stopWith(ctx, t, cy, registry.ExitCap, s, "the local review's cap")
}

// loopTrend is the trend of loop's rounds by what each handed the writer
// (its review-<n>.json in dir; none from a round whose result is there, a
// clean round a second opinion followed; a round with neither is left
// out): "blocking 11 → 8 → 5, P1 5 → 3 → 0".
func loopTrend(dir string, loop []registry.Cycle) string {
	var blocking, p1 []string
	for _, cy := range loop {
		rf, ok, err := readReviewFile(dir, cy.N)
		if err != nil || (!ok && !fsx.Exists(filepath.Join(dir, reviewRoundFile(cy.N)))) {
			continue
		}
		n := 0
		for _, i := range rf.Findings {
			if i.Severity == "P1" {
				n++
			}
		}
		blocking, p1 = append(blocking, strconv.Itoa(len(rf.Findings))), append(p1, strconv.Itoa(n))
	}
	return "blocking " + strings.Join(blocking, " → ") + ", P1 " + strings.Join(p1, " → ")
}

// capItemsShown bounds the findings the NEEDS YOU line of a stop at the cap
// names; the task card lists them all.
const capItemsShown = 3

// itemsLine is findings items on one line: the first capItemsShown, each
// its priority, title (clipped) and path, then how many more.
func itemsLine(items []ReviewItem) string {
	var parts []string
	for _, i := range items[:min(len(items), capItemsShown)] {
		s := strings.TrimSpace(i.Severity + " " + textx.Clip(i.Title, 100))
		if i.Path != "" {
			s += " (" + i.Path + ")"
		}
		parts = append(parts, s)
	}
	if n := len(items) - capItemsShown; n > 0 {
		parts = append(parts, fmt.Sprintf("and %d more", n))
	}
	return strings.Join(parts, "; ")
}

// capItem is a finding of a stop at the cap as the task card lists it: its
// priority, title and path.
func capItem(i ReviewItem) string {
	s := strings.TrimSpace(i.Severity + " " + i.Title)
	if i.Path != "" {
		s += " · " + i.Path
	}
	return s
}

// capFix is the fix turn of the loop's stop at its cap on cycle cy (ended
// cap, the newest of cycles, task t's): the cap round's findings (its
// review-<n>.json; for a stop of an older build, which wrote none, its
// result read again: capReview) go to the writer before a fresh loop
// starts. due is true when your "go on" finds the task's head where the cap
// round reviewed it, and, once the turn began, until its end closed the
// fix (review-stop.json's Fixed, closeFix): a new head (a send back's
// commits, the base merged, your own commit) starts the fresh loop at once,
// as a closed fix does. The questions among the findings are put to you
// first, as fixPhase does for any round.
func (e *Engine) capFix(ctx context.Context, t registry.Task, st config.FactoryStage, cy registry.Cycle, cycles []registry.Cycle) (ReviewFile, bool, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if s, ok := readReviewStop(dir); ok && s.Cycle == cy.ID && s.Fixed {
		return ReviewFile{}, false, nil
	}
	n, err := e.attemptsOf(ctx, t, st)
	if err != nil {
		return ReviewFile{}, false, err
	}
	rf, ok, err := readReviewFile(dir, cy.N)
	switch {
	case err != nil:
		return rf, false, err
	case ok && n > rf.AttemptsBefore:
		return rf, true, nil // its turn began: it goes on to its end
	case t.HeadSHA != cy.HeadBefore:
		return rf, false, nil // a head the cap round did not review: the fresh loop reviews it
	case !ok:
		if rf, ok, err = e.capReview(ctx, t, st, cy); err != nil || !ok {
			return rf, false, err
		}
	}
	var qs []ReviewItem
	for _, i := range rf.Findings {
		if i.Question {
			qs = append(qs, i)
		}
	}
	if len(qs) > 0 {
		if err := e.writeReviewQuestions(dir, st, qs, len(loopCycles(cycles[:len(cycles)-1]))+1); err != nil {
			return rf, false, err
		}
	}
	e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("you let the loop go on at %s, which cycle %d reviewed at the cap: "+
		"the writer fixes its %s (%s) before a fresh loop", short(cy.HeadBefore), cy.N, plural(len(rf.Findings), "finding"), reviewFile(cy.N)))
	return rf, true, nil
}

// capReview writes the review-<n>.json of cap cycle cy that a stop of an
// older build did not write: the cap round's result (local-review-<n>.json)
// read again as decideRound reads it (readRound: the registry has its
// findings already, and seeing them again in their cycle changes nothing),
// its live findings and questions. ok is false when there is no result, or
// nothing in it for the writer.
func (e *Engine) capReview(ctx context.Context, t registry.Task, st config.FactoryStage, cy registry.Cycle) (ReviewFile, bool, error) {
	res, ok, err := readRoundResult(e.d.Layout.FactoryTaskDir(t.ID), cy.N)
	if err != nil || !ok {
		return ReviewFile{}, false, err
	}
	blocks, unavailable, why := Blocks(res)
	if unavailable {
		return ReviewFile{}, false, nil
	}
	rr, err := e.readRound(ctx, t, cy, res, blocks, why)
	if err != nil {
		return ReviewFile{}, false, err
	}
	items := slices.Concat(rr.live, rr.questions)
	if !rr.blocks || len(items) == 0 {
		return ReviewFile{}, false, nil
	}
	rf, err := e.writeReview(ctx, t, st, cy, items)
	return rf, err == nil, err
}
