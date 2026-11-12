package factory

// The local review's cap (spec 6.5): a round at [factory.local_review]
// cycles that still blocks either goes on by itself, while the loop
// converges, for at most extra_cycles rounds (extraRound), or stops the
// loop (capStops). A safe stop (capSafe) goes on by itself, at most
// cap_go_on times in the task's life (goOnAtCap), and past them has the
// writer fix its findings before the stage passes (passAfterCap); any
// other stop is for you (stopAtCap), with its findings, the loop's trend
// and the judge's verdict on the NEEDS YOU line and the task card. Its
// findings are written for the writer as a fixing round's are, and your
// "go on" at the head the cap round reviewed, or the factory's own, hands
// them to the writer before a fresh loop starts (capFix).

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
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/scrub"
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
	head := capHead(e.d.Layout.FactoryTaskDir(t.ID), loop, res, len(live)+len(questions)+len(doubts))
	s := reviewStop{Why: head + ": " + itemsLine(rf.Findings), Head: head, Findings: rf.Findings}
	return e.stopWith(ctx, t, cy, registry.ExitCap, s, "the local review's cap")
}

// capHead is the head of the line of a stop at the cap of loop (its last
// round's result res, n blocking findings): the count, the trend of the
// loop's rounds (in task directory dir) and the judge's verdict.
func capHead(dir string, loop []registry.Cycle, res evalrun.LocalResult, n int) string {
	return fmt.Sprintf("%s after %s (%s; verdict %s)", plural(n, "blocking finding"), plural(len(loop), "review round"),
		loopTrend(dir, loop), cmp.Or(res.Verdict, "none"))
}

// capStops stops the loop at its cap on cycle cy, the last of loop (its
// result res), a round that does not converge (extraRound): a safe stop
// (capSafe) goes on without you while [factory.local_review] cap_go_on is
// above 0 (goOnAtCap), any other stops for you (stopAtCap). sum is the
// loop's summary as the round left it, for a stop past the task's go-ons.
func (e *Engine) capStops(ctx context.Context, t registry.Task, st config.FactoryStage, cy registry.Cycle, loop []registry.Cycle,
	res evalrun.LocalResult, rr roundRead, sum ReviewSummary) error {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if e.cfg.Factory.LocalReview.CapGoOn > 0 && capSafe(dir, loop, res) {
		return e.goOnAtCap(ctx, t, st, cy, loop, res, rr, sum)
	}
	return e.stopAtCap(ctx, t, st, cy, loop, res, rr.live, rr.questions, rr.doubts)
}

// capSafe reports whether a stop at the cap of loop (in task directory
// dir), whose last round's result is res, is safe to go on from without
// you: the round has no P0 or P1, new or still open (highPriority), its
// judge's verdict does not block, and the writer declined no finding it got
// in the loop (a reply of a round of it, review-reply-<n>.json, with a risk
// or without). A reply that cannot be read is not known to decline
// nothing.
func capSafe(dir string, loop []registry.Cycle, res evalrun.LocalResult) bool {
	if highPriority(res) || res.Verdict == store.VerdictBlocking {
		return false
	}
	for _, cy := range loop {
		b, err := os.ReadFile(filepath.Join(dir, reviewReplyFile(cy.N)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		var rep ReviewReply
		if err != nil || json.Unmarshal(b, &rep) != nil || len(rep.declinedIDs()) > 0 {
			return false
		}
	}
	return true
}

// exitCapFixed is the exit of review-summary.json of a loop that passed at
// its cap after the writer's fix of its last round's findings, which no
// round reviewed (passAfterCap). Its cycle's exit in the registry stays the
// cap's; a summary of exit cap (a stop for you) never has a card.
const exitCapFixed = "cap_fixed"

// capGoOnFile is review-go-on.json in the task's directory: the stops at
// the local review's cap that went on without you over the task's life,
// oldest first, each by its cycle (capGoOn). It is written before the
// cycle ends, so a restart neither counts a stop twice nor loses one.
const capGoOnFile = "review-go-on.json"

// The words of a go-on at the cap, which its event and its alert say
// (goOnAlert reads them): one within the task's go-ons ("went on by
// itself at the cap (1 of 2)"), and one past them, whose fix passes the
// stage ("passes the cap after its fix (2 go-ons spent)").
const (
	goOnWords = "went on by itself at the cap"
	goOnLine  = goOnWords + " (%d of %d)"
	passWords = "passes the cap after its fix"
)

// capGoOn is a stop at the cap that went on without you: its cycle, its
// number among the task's go-ons (N, from 1; 0 for a stop past them) out
// of the budget then (Of: [factory.local_review] cap_go_on), the line its
// event and alert say and whether they were said (Said), and for a stop
// past the go-ons the loop's summary as its last round left it (Pass): the
// writer's fix of the round's findings passes the stage (passAfterCap),
// and no fresh loop starts.
type capGoOn struct {
	Cycle int64          `json:"cycle"`
	N     int            `json:"n,omitempty"`
	Of    int            `json:"of"`
	Line  string         `json:"line"`
	Said  bool           `json:"said,omitempty"`
	Pass  *ReviewSummary `json:"pass,omitempty"`
}

// readGoOns reads task directory dir's go-ons at the cap (capGoOnFile),
// none when there is no file.
func readGoOns(dir string) ([]capGoOn, error) {
	b, err := os.ReadFile(filepath.Join(dir, capGoOnFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var gs []capGoOn
	if err := json.Unmarshal(b, &gs); err != nil {
		return nil, fmt.Errorf("%s: %w", capGoOnFile, err)
	}
	return gs, nil
}

// goOnOf is the go-on of cap cycle id in task directory dir; ok is false
// when that stop was for you.
func goOnOf(dir string, id int64) (capGoOn, bool, error) {
	gs, err := readGoOns(dir)
	if err != nil {
		return capGoOn{}, false, err
	}
	i := slices.IndexFunc(gs, func(g capGoOn) bool { return g.Cycle == id })
	if i < 0 {
		return capGoOn{}, false, nil
	}
	return gs[i], true, nil
}

// goOnAtCap goes on from the safe stop at the cap on cycle cy, the last of
// loop (its result res, its findings rr, the loop's summary sum), as your
// "go on" would: the go-on is recorded (recordGoOn), the cycle ends at the
// cap, the round's findings go to review-<n>.json and review-stop.json,
// and an event and an alert say it (sayGoOn). The task stays at work: the
// next pass hands the findings to the writer (capFix), as after your retry,
// and its fix starts a fresh loop, or, past the task's go-ons, passes the
// stage (passAfterCap). Every step before the alert is one a restart finds
// done or does again: a cycle that did not end is read again and decided
// alike, a review-<n>.json not written is written by capFix (capReview).
func (e *Engine) goOnAtCap(ctx context.Context, t registry.Task, st config.FactoryStage, cy registry.Cycle, loop []registry.Cycle,
	res evalrun.LocalResult, rr roundRead, sum ReviewSummary) error {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	items := slices.Concat(rr.live, rr.questions)
	g, err := e.recordGoOn(dir, cy, items, withSecond(dir, loop, sum))
	if err != nil {
		return err
	}
	if err := e.endCycle(ctx, cy, cy.HeadBefore, registry.ExitCap); err != nil {
		return err
	}
	rf, err := e.writeReview(ctx, t, st, cy, items)
	if err != nil {
		return err
	}
	s := reviewStop{Cycle: cy.ID, Why: g.Line, Head: capHead(dir, loop, res, len(items)+len(rr.doubts)), Findings: rf.Findings}
	if err := writeJSON(filepath.Join(dir, reviewStopFile), s); err != nil {
		return err
	}
	if e.beforeStop != nil {
		if err := e.beforeStop(); err != nil {
			return err
		}
	}
	return e.sayGoOn(ctx, t, cy, g)
}

// recordGoOn records the go-on of cap cycle cy in task directory dir, its
// findings for the writer items, and returns it: the one recorded already
// when a restart came between, else the next of the task's go-ons, or,
// once [factory.local_review] cap_go_on of them went, a stop past them
// with the loop's summary sum.
func (e *Engine) recordGoOn(dir string, cy registry.Cycle, items []ReviewItem, sum ReviewSummary) (capGoOn, error) {
	gs, err := readGoOns(dir)
	if err != nil {
		return capGoOn{}, err
	}
	if i := slices.IndexFunc(gs, func(g capGoOn) bool { return g.Cycle == cy.ID }); i >= 0 {
		return gs[i], nil
	}
	used := 0
	for _, g := range gs {
		if g.N > 0 {
			used++
		}
	}
	g := capGoOn{Cycle: cy.ID, Of: e.cfg.Factory.LocalReview.CapGoOn}
	if used < g.Of {
		g.N = used + 1
		g.Line = fmt.Sprintf(goOnLine+": %s to the writer", g.N, g.Of, severityCount(items))
	} else {
		g.Pass = &sum
		g.Line = fmt.Sprintf("%s (%s spent): %s to the writer, not reviewed again", passWords, plural(used, "go-on"), severityCount(items))
	}
	return g, writeJSON(filepath.Join(dir, capGoOnFile), append(gs, g))
}

// sayGoOn writes go-on g's event and alert for cap cycle cy of task t,
// once, and marks it said in task directory's capGoOnFile: one marked is
// left, and so is one whose alert a daemon that stopped before the mark
// wrote (an alert of its line since the cycle began). The mark keeps a
// pass of the review long after it (the task back from QA or the ready
// card) from reading the events again.
func (e *Engine) sayGoOn(ctx context.Context, t registry.Task, cy registry.Cycle, g capGoOn) error {
	if g.Said {
		return nil
	}
	alert := TaskKey(t) + ": " + g.Line
	evs, err := e.reg.Events(ctx, t.ID, 200)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(evs, func(ev registry.Event) bool {
		return ev.Kind == "alert" && !ev.At.Before(cy.Started) && ev.Text == scrub.Text(alert)
	}) {
		then := "a fresh loop reviews the fix"
		if g.Pass != nil {
			then = "the stage passes once the factory's checks pass on the fix"
		}
		e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("cycle %d ended %s, a safe stop (no P0 or P1, a verdict that does not block, "+
			"no finding declined in its loop): the loop %s (%s), and %s", cy.N, registry.ExitCap, g.Line, reviewFile(cy.N), then))
		e.alert(ctx, t.ID, alert)
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	gs, err := readGoOns(dir)
	if err != nil {
		return err
	}
	for i := range gs {
		gs[i].Said = gs[i].Said || gs[i].Cycle == cy.ID
	}
	return writeJSON(filepath.Join(dir, capGoOnFile), gs)
}

// goOnAlert reports whether text is the alert of a go-on at the cap
// (sayGoOn): news of a task that goes on, which waits on nobody.
func goOnAlert(text string) bool {
	_, what, _ := strings.Cut(text, ": ")
	return strings.HasPrefix(what, goOnWords+" (") || strings.HasPrefix(what, passWords+" (")
}

// severityCount is items counted for a line: "3 P2 findings" when they
// share a priority, else "3 findings".
func severityCount(items []ReviewItem) string {
	sev := map[string]bool{}
	for _, i := range items {
		sev[i.Severity] = true
	}
	if len(sev) == 1 && !sev[""] {
		return plural(len(items), slices.Collect(maps.Keys(sev))[0]+" finding")
	}
	return plural(len(items), "finding")
}

// passAfterCap passes review stage st of task t at cap cycle cy whose stop
// went on past the task's go-ons (its go-on's Pass), once the writer's fix
// of the cap round's findings passed the factory's checks (closeFix:
// review-stop.json's Fixed) at the task's head: the loop's summary goes to
// review-summary.json at that head, with the findings the writer fixed
// after the last round (FixedAfter, fixedAfter) for the ready card and the
// PR body, and done is true. A head since that fix (QA's fix, a send back,
// the base merged) is not passed: a fresh loop reviews it.
func (e *Engine) passAfterCap(ctx context.Context, t registry.Task, cy registry.Cycle) (bool, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	g, ok, err := goOnOf(dir, cy.ID)
	if err != nil || !ok || g.Pass == nil {
		return false, err
	}
	s, ok := readReviewStop(dir)
	if !ok || s.Cycle != cy.ID || !s.Fixed || s.FixedAt == "" || s.FixedAt != t.HeadSHA {
		return false, nil
	}
	rf, ok, err := readReviewFile(dir, cy.N)
	if err != nil || !ok {
		return false, err
	}
	sum := *g.Pass
	sum.Exit, sum.Head = exitCapFixed, t.HeadSHA
	sum.FixedAfter, sum.Leftovers = fixedAfter(dir, cy.N, rf, sum.Leftovers)
	sum.Declined = slices.SortedFunc(maps.Values(readDeclined(dir)), func(a, b ReviewItem) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
	if err := writeJSON(filepath.Join(dir, reviewSummaryFile), sum); err != nil {
		return false, err
	}
	e.event(ctx, t.ID, "review", registry.PhaseOK, fmt.Sprintf("the local review passes at %s after the writer's fix of cycle %d at the cap: "+
		"%s fixed after the last round, not reviewed again; %d left, %d declined, %d doubts", short(t.HeadSHA), cy.N,
		plural(len(sum.FixedAfter), "finding"), len(sum.Leftovers), len(sum.Declined), len(sum.Doubts)))
	return true, nil
}

// fixedAfter sorts the findings of cap cycle n's review file rf by the
// writer's reply (review-reply-<n>.json in dir): those it fixed, and the
// leftovers left with the ones it declined without a risk or did not reply
// to (one declined with its risk is among the declined findings already,
// closeFix).
func fixedAfter(dir string, n int, rf ReviewFile, leftovers []ReviewItem) (fixed, left []ReviewItem) {
	var rep ReviewReply
	if b, err := os.ReadFile(filepath.Join(dir, reviewReplyFile(n))); err == nil {
		_ = json.Unmarshal(b, &rep) // the fix turn's end fact read it
	}
	action, risks := map[string]string{}, rep.declinedWithRisk()
	for _, r := range rep.Replies {
		action[r.ID] = r.Action
	}
	left = slices.Clone(leftovers)
	for _, i := range rf.Findings {
		switch {
		case action[i.ID] == replyFixed:
			fixed = append(fixed, i)
		case risks[i.ID] == "":
			left = append(left, i)
		}
	}
	return fixed, left
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
// starts. due is true when your "go on", or the factory's own at a safe
// stop (goOnAtCap), finds the task's head where the cap round reviewed it,
// and, once the turn began, until its end closed the
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
	who, then := "you let the loop go on", "a fresh loop"
	if g, auto, err := goOnOf(dir, cy.ID); err != nil {
		return rf, false, err
	} else if auto {
		who = "the loop goes on by itself"
		if g.Pass != nil {
			then = "the stage passes, not reviewed again"
		}
	}
	e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("%s at %s, which cycle %d reviewed at the cap: the writer fixes its %s (%s) before %s",
		who, short(cy.HeadBefore), cy.N, plural(len(rf.Findings), "finding"), reviewFile(cy.N), then))
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
