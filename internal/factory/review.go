package factory

// The local magnum review loop (spec 6.5): before any PR exists, magnum's
// full review runs on the task's change in its copy, as a blind dry run of
// the repository's watch's review roles (evalrun.RunLocal, through
// Deps.Review), round after round, at most [factory.local_review] cycles of
// them. A round runs off the loop at the lowest priority: it waits while
// magnum has a real round in flight or due, while Codex is past its soft
// cap, while an agent or you are at work in the copy and for a heavy slot.
// The factory reads its result itself (Blocks): a P0 to P2, new or still
// open, or a blocking verdict blocks; a round that did not finish is review
// unavailable, and the task needs you, never a clean pass. A blocking round
// hands its findings to the writer (review-<n>.json) for one fix turn of
// the coding stage's roles, whose end fact is the coding stage's own (the
// factory's proof and precheck at the new head, provenHead) and the
// review's (the writer's reply, review-reply-<n>.json, and a commit since
// the round unless it declined every finding with its risk); then the next
// round runs. A finding is followed by its fingerprint: one that comes back
// twice after its fix is a doubt, and when only doubts block, the loop ends
// on the oscillation and the stage passes; one the writer declined with
// its risk blocks no more; a product question joins the task's question
// batch. Every round is a cycle of the registry with its findings, and
// what a round left lives in the task's directory, so a restart at any
// point runs a round again only when it left no result.

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
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/usage"
)

// ReviewFixPrompt is the prompt of the writer's fix turn in the local
// review: the coding stage's roles get it in place of their own prompt.
const ReviewFixPrompt = "factory-review-fix.md"

// localReviewer is the LocalReviewer the factory daemon wires:
// evalrun.RunLocal over d, whose config is the review candidate's
// (WriteReviewCandidate, config.WithCandidate).
type localReviewer struct{ d evalrun.Deps }

// NewLocalReviewer is the local reviewer that runs each round with
// evalrun.RunLocal over d.
func NewLocalReviewer(d evalrun.Deps) LocalReviewer { return localReviewer{d: d} }

// Round runs local round c (evalrun.RunLocal).
func (r localReviewer) Round(ctx context.Context, c evalrun.LocalCase) (evalrun.LocalResult, error) {
	return evalrun.RunLocal(ctx, r.d, c)
}

// The files of the local review in the task's directory: a round's result
// as the round read it (local-review-<n>.json, n the cycle's number; a
// round set aside to run again keeps its file as
// local-review-<n>-<why>-<k>.json), the findings it hands the writer
// (review-<n>.json), the writer's reply (review-reply-<n>.json), the risks
// of the findings the writer declined (review-declined.json) and the
// loop's end for the ready card and the PR body (review-summary.json).
const (
	reviewSummaryFile  = "review-summary.json"
	reviewDeclinedFile = "review-declined.json"
)

func reviewRoundFile(n int) string { return fmt.Sprintf("local-review-%d.json", n) }
func reviewFile(n int) string      { return fmt.Sprintf("review-%d.json", n) }
func reviewReplyFile(n int) string { return fmt.Sprintf("review-reply-%d.json", n) }

// reviewFileRe is a round's review-<n>.json.
var reviewFileRe = regexp.MustCompile(`^review-(\d+)\.json$`)

// newestReview is the number of the newest review-<n>.json in dir, 0 for
// none.
func newestReview(dir string) int {
	names, _ := filepath.Glob(filepath.Join(dir, "review-*.json"))
	n := 0
	for _, p := range names {
		if m := reviewFileRe.FindStringSubmatch(filepath.Base(p)); m != nil {
			if k, err := strconv.Atoi(m[1]); err == nil {
				n = max(n, k)
			}
		}
	}
	return n
}

// The waits of a local round, besides the agents' and the checks': magnum
// has a round in flight or due, and the task's round runs.
const (
	waitMagnum = "magnum"
	waitReview = "review"
)

// heavyReview is a local round's use of a heavy slot.
const heavyReview = "local review"

// LocalRoundCap ends a round of the local review that runs this long (a
// hang): a round takes 39 minutes at the median, its roles' own timeouts
// end within the hour. `magnum factory install --drain` waits longer.
const LocalRoundCap = 3 * time.Hour

// ReviewItem is one finding of the local review as the factory hands it on:
// its id in the round's file (F1, F2, ...), its fingerprint, severity,
// title, path, line and body, whether the judge marked it a product
// question, and, for one the writer declined, the risk it named.
type ReviewItem struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	Severity    string `json:"severity,omitempty"`
	Title       string `json:"title"`
	Path        string `json:"path,omitempty"`
	Line        int    `json:"line,omitempty"`
	Body        string `json:"body,omitempty"`
	Question    bool   `json:"question,omitempty"`
	Risk        string `json:"risk,omitempty"`
}

// ReviewFile is review-<n>.json, what a blocking round hands the writer:
// the cycle's number, the head it reviewed, how many attempts the review
// stage had when it was written (the fix turn is the first attempt after
// them) and the findings to fix, the questions among them.
type ReviewFile struct {
	Round          int          `json:"round"`
	Head           string       `json:"head"`
	AttemptsBefore int          `json:"attempts_before"`
	Findings       []ReviewItem `json:"findings"`
}

// ReviewReply is review-reply-<n>.json, the writer's: per finding, fixed,
// or declined with the risk of its suggested change.
type ReviewReply struct {
	Replies []struct {
		ID     string `json:"id"`
		Action string `json:"action"`
		Risk   string `json:"risk"`
	} `json:"replies"`
}

// The actions of a reply.
const (
	replyFixed    = "fixed"
	replyDeclined = "declined"
)

// declinedWithRisk are the ids of rep's findings declined with a risk, and
// that risk.
func (rep ReviewReply) declinedWithRisk() map[string]string {
	out := map[string]string{}
	for _, r := range rep.Replies {
		if r.Action == replyDeclined && strings.TrimSpace(r.Risk) != "" {
			out[r.ID] = strings.TrimSpace(r.Risk)
		}
	}
	return out
}

// ReviewSummary is review-summary.json: how the local review ended (exit
// clean or oscillation), after how many rounds, at which head (the one its
// last round reviewed), and what it left that does not block, for the
// ready card and the PR body's "Known": the leftovers of its last round
// (P3s, simplifications, product questions you answered, and on a clean
// exit the findings at a blocking priority that the round's counts and
// verdict left out), the findings the writer declined with their risks
// (whichever round raised them), the doubts (findings that came back twice
// after their fix, each told there only) and the reviewers a round of the
// loop went without (Missing).
type ReviewSummary struct {
	Exit      string       `json:"exit"`
	Rounds    int          `json:"rounds"`
	Head      string       `json:"head"`
	Leftovers []ReviewItem `json:"leftovers"`
	Declined  []ReviewItem `json:"declined"`
	Doubts    []ReviewItem `json:"doubts"`
	Missing   []string     `json:"missing,omitempty"`
}

// localRound is a round that runs off the loop: its cycle (id and number),
// the loop's round number, done closed when it ended, cancel to end it,
// stopped when the factory ended it (the task stopped being live: nothing
// of it is read), and what it ended with (written before done closes): a
// result the round's goroutine wrote to the task's directory, or drained,
// the daemon stopping it, or err, its result that could not be written.
type localRound struct {
	cycle    int64
	n, round int
	done     chan struct{}
	cancel   context.CancelFunc
	stopped  atomic.Bool
	drained  bool
	err      error
}

func (r *localRound) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// roundOf is the round of task id off the loop, nil for none.
func (e *Engine) roundOf(id int64) *localRound {
	e.reviewMu.Lock()
	defer e.reviewMu.Unlock()
	return e.reviews[id]
}

// forgetRound drops r, the round of task id.
func (e *Engine) forgetRound(id int64, r *localRound) {
	e.reviewMu.Lock()
	defer e.reviewMu.Unlock()
	if e.reviews[id] == r {
		delete(e.reviews, id)
	}
}

// sweepRounds ends the rounds of the tasks that are not live (a stop, a
// NEEDS YOU, gone): nothing of them is read, the round's cycle stays open
// and runs again once the task is back at work.
func (e *Engine) sweepRounds(tasks []registry.Task) {
	live := map[int64]bool{}
	for _, t := range tasks {
		if isLive(t) {
			live[t.ID] = true
		}
	}
	e.reviewMu.Lock()
	defer e.reviewMu.Unlock()
	for id, r := range e.reviews {
		if live[id] {
			continue
		}
		if !r.ended() {
			r.stopped.Store(true)
			r.cancel()
			continue
		}
		delete(e.reviews, id)
	}
}

// runReview runs one pass of review stage st of task t (the engine's runner
// for config.FactoryStageReview); done reports that the loop ended clean or
// on an oscillation at the task's head. The newest cycle decides: one that
// ended the stage at the task's head passes it again (its move did not
// land); an open one goes on (goOnCycle); else the next round of the loop
// starts, once it may (startRound). A loop is the cycles after the newest
// that ended one: with you (cap, unavailable, operator), and your "go on"
// after it starts a loop afresh, or clean or on an oscillation, and a head
// the loop did not review (a send back's commits, the base merged after a
// conflict at the ready card) starts a fresh loop of up to cycles rounds. A
// build without the local reviewer, agents or git leaves the task where it
// is.
func (e *Engine) runReview(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
	if e.d.Review == nil || e.d.Runtime == nil || e.d.Git == nil {
		return false, nil
	}
	cycles, err := e.reg.Cycles(ctx, t.ID)
	if err != nil {
		return false, err
	}
	var last *registry.Cycle
	if len(cycles) > 0 {
		last = &cycles[len(cycles)-1]
	}
	if last != nil && !last.Ended.IsZero() && (last.Exit == registry.ExitClean || last.Exit == registry.ExitOscillation) &&
		last.HeadBefore == t.HeadSHA {
		return true, nil
	}
	if last != nil && !last.Ended.IsZero() && (last.Exit == registry.ExitUnavailable || last.Exit == registry.ExitCap) {
		if told, err := e.toldOfStop(ctx, t, *last); err != nil || !told {
			return false, err // the move to you was lost: made now, and no fresh loop starts
		}
	}
	loop := loopCycles(cycles)
	c, err := e.claimCopy(ctx, t)
	if err != nil {
		return false, err
	}
	if last != nil && last.Ended.IsZero() {
		return e.goOnCycle(ctx, t, st, c, *last, loop)
	}
	return false, e.startRound(ctx, t, c, nil, len(loop)+1)
}

// reviewStopFile is review-stop.json in the task's directory: why the loop
// stopped for you, by the cycle that stopped it, written before that cycle
// ends (stopForYou).
const reviewStopFile = "review-stop.json"

// reviewStop is review-stop.json.
type reviewStop struct {
	Cycle int64  `json:"cycle"`
	Why   string `json:"why"`
}

// stopForYou ends the loop on cycle cy with exit (unavailable, the cap) and
// moves task t to you with why. why goes to the task's directory first, the
// cycle ends next, the move comes last: a daemon that stops before the move
// finds the ended cycle with no word to you since and makes the move then
// (toldOfStop), never a fresh loop.
func (e *Engine) stopForYou(ctx context.Context, t registry.Task, cy registry.Cycle, exit, why, because string) error {
	if err := writeJSON(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), reviewStopFile), reviewStop{Cycle: cy.ID, Why: why}); err != nil {
		return err
	}
	if err := e.endCycle(ctx, cy, cy.HeadBefore, exit); err != nil {
		return err
	}
	e.event(ctx, t.ID, "review", registry.PhaseFail, fmt.Sprintf("cycle %d ended %s: %s", cy.N, exit, why))
	if e.beforeStop != nil {
		if err := e.beforeStop(); err != nil {
			return err
		}
	}
	e.toOperator(ctx, t, why, because)
	return nil
}

// toldOfStop reports whether you were told of the loop's stop on cycle cy
// (an alert since the cycle ended): a live task told so is one you let go
// on, and a fresh loop starts. One not told is moved to you now, with the
// stop's reason (review-stop.json).
func (e *Engine) toldOfStop(ctx context.Context, t registry.Task, cy registry.Cycle) (bool, error) {
	evs, err := e.reg.Events(ctx, t.ID, 200)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(evs, func(ev registry.Event) bool { return ev.Kind == "alert" && !ev.At.Before(cy.Ended) }) {
		return true, nil
	}
	why := "the local review stopped (" + cy.Exit + ")"
	if b, err := os.ReadFile(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), reviewStopFile)); err == nil {
		var s reviewStop
		if json.Unmarshal(b, &s) == nil && s.Cycle == cy.ID && s.Why != "" {
			why = s.Why
		}
	}
	e.toOperator(ctx, t, why, "the local review stopped for you")
	return false, nil
}

// loopCycles are the cycles of the current loop: those after the newest
// whose exit ended a loop, with you or clean or on an oscillation (a loop
// of a head reviewed before: runReview passes on it, or starts afresh).
func loopCycles(cs []registry.Cycle) []registry.Cycle {
	for i, c := range slices.Backward(cs) {
		switch c.Exit {
		case registry.ExitCap, registry.ExitUnavailable, registry.ExitOperator, registry.ExitClean, registry.ExitOscillation:
			return cs[i+1:]
		}
	}
	return cs
}

// startRound starts round `round` of the loop in copy c, in cycle cy (a
// round that runs again: a restart, a hang, a refusal) or in a new one,
// once it may: not while the daemon drains (a stop's drain, or install's),
// and after the waits of the lowest priority (reviewWait) and a heavy slot.
// Its roles are the watch's (reviewRoles), its case the change at the
// task's proven head (localCase); the round runs off the loop (launchRound).
func (e *Engine) startRound(ctx context.Context, t registry.Task, c Copy, cy *registry.Cycle, round int) error {
	if e.startsHeld() {
		return nil
	}
	if r := e.roundOf(t.ID); r != nil {
		if !r.ended() {
			return nil
		}
		e.forgetRound(t.ID, r) // an ended round of another cycle: read already
	}
	w := e.cfg.WatchFor(t.Repo)
	if w == nil {
		return e.stageFailed(ctx, t, needsYou{"no [[watch]] covers " + t.Repo + ": the local review runs its review roles"})
	}
	roles, err := e.reviewRoles(t, w, round)
	if err != nil {
		return e.stageFailed(ctx, t, err)
	}
	who, what, err := e.reviewWait(ctx, t, roles)
	if err != nil {
		return err
	}
	if who != "" {
		e.waitOn(ctx, t, who, what)
		return heldError{who: who, what: what}
	}
	repo, err := e.factoryRepo(t)
	if err != nil {
		return err
	}
	if p, pending, err := e.keptFirst(ctx, &t); err != nil || pending { // what a check kept is never reviewed past
		return err
	} else if p != nil {
		return e.stageFailed(ctx, t, needsYou{p.why})
	}
	again := cy != nil
	if again { // a run again in its cycle: what a stopped run left goes back first, as at a round's end
		left, err := e.roundLeft(ctx, t, c, cy.HeadBefore, cy.N)
		if err != nil {
			return err
		}
		if !left.none() && left.moved == "" {
			if why, err := e.putBackRound(ctx, t, c, *cy, round, left); err != nil {
				return err
			} else if why != "" {
				return e.stageFailed(ctx, t, needsYou{why})
			}
		}
	}
	bh, err := e.branchHead(ctx, t, c, repo)
	switch {
	case err != nil:
		return err
	case bh.Problem != "":
		return e.stageFailed(ctx, t, needsYou{"the local review cannot start: " + bh.Problem})
	case t.HeadSHA != "" && bh.Head != t.HeadSHA: // the local review reviews a checked head only
		return e.headMoved(ctx, t, bh.Head)
	}
	lc := e.localCase(t, *w, c, bh, repo, roles, round)
	release, ok := e.heavy(ctx, t, heavyReview)
	if !ok {
		return nil // the task waits "heavy: …"
	}
	if cy == nil {
		begun, err := e.reg.BeginCycle(ctx, t.ID, bh.Head, skillHash(e.d.ReviewCandidate))
		if err != nil {
			release()
			return err
		}
		cy = &begun
	}
	// The untracked files the copy has before the cycle's first run, file by
	// file: what a round leaves beyond them is the round's (roundLeft), never
	// committed by the writer's `git add -A`. A run again in its cycle keeps
	// the cycle's listing, so nothing a stopped run left counts as the copy's.
	listing := filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), untrackedBeforeFile(cy.N))
	if _, err := os.Stat(listing); !again || err != nil {
		untracked, err := e.d.Git.UntrackedFiles(ctx, c.Path)
		if err == nil {
			err = writeJSON(listing, append([]string{}, untracked...))
		}
		if err != nil {
			release()
			return err
		}
	}
	e.launchRound(ctx, t, *cy, lc, release)
	return nil
}

// untrackedBeforeFile is the listing of the copy's untracked files taken
// before cycle n's first run (gitx.UntrackedFiles: file by file; an older
// build's names a wholly untracked directory once, ending in /).
func untrackedBeforeFile(n int) string { return fmt.Sprintf("untracked-before-%d.json", n) }

// beforeListing is the listing taken before a cycle's first run
// (untrackedBeforeFile) as a set: its files, and the directories an older
// build's listing names (ending in /).
type beforeListing struct {
	files map[string]bool
	dirs  []string
}

// newBeforeListing is the listing paths as a set.
func newBeforeListing(paths []string) *beforeListing {
	b := &beforeListing{files: make(map[string]bool, len(paths))}
	for _, p := range paths {
		b.files[p] = true
		if strings.HasSuffix(p, "/") {
			b.dirs = append(b.dirs, p)
		}
	}
	return b
}

// names reports whether path p was in the copy before the round as an
// untracked file git did not ignore: listed, or under a directory listed.
func (b *beforeListing) names(p string) bool {
	return b.files[p] || slices.ContainsFunc(b.dirs, func(d string) bool { return strings.HasPrefix(p, d) })
}

// reviewWait is what a local round waits for before it starts, "" for
// nothing: a hold of the machine (mayLaunch), an agent or you at work in
// the task's copy, the factory's checks of the head, magnum's real rounds
// (Magnum.Busy: in flight and due, at its limit; its registry that cannot be read is a
// wait too, with why), and, when the round runs a Codex role, Codex at or
// past [usage] codex_soft, and magnum's review session that does not
// answer (reviewHerdrDown). who and what are the task's wait.
func (e *Engine) reviewWait(ctx context.Context, t registry.Task, roles []string) (who, what string, err error) {
	if who, what, ok := e.mayLaunch(); !ok {
		return who, what, nil
	}
	role, you, err := e.d.Runtime.AtWork(ctx, t)
	switch {
	case err != nil:
		return "", "", err
	case you:
		return waitAgent, "you're in a pane of " + cmp.Or(t.Copy, "the copy") + "; the local review waits until you leave it", nil
	case role != "":
		return waitAgent, "the " + role + " agent works in " + cmp.Or(t.Copy, "the copy") + "; the local review waits for its turn to end", nil
	}
	if e.checkRuns(t.ID) {
		return waitPrecheck, cmp.Or(e.checkNote(t.ID), waitPrecheck+": runs"), nil
	}
	if e.d.Magnum != nil {
		busy, why, err := e.d.Magnum.Busy(ctx)
		switch {
		case err != nil:
			return waitMagnum, textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 300), nil
		case busy:
			return waitMagnum, why + "; the local review yields to it", nil
		case why != "" && e.magnumNotes[t.ID] != why:
			e.magnumNotes[t.ID] = why // said once: a due round of magnum's that never starts holds it no longer
			e.event(ctx, t.ID, "review", registry.PhaseInfo, why)
		}
	}
	if e.runsCodex(roles) {
		if what := e.codexSoft(ctx); what != "" {
			return waitUsage, what, nil
		}
	}
	if what := e.reviewHerdrDown(ctx); what != "" {
		return HoldHerdr, what, nil
	}
	return "", "", nil
}

// reviewHerdrDown says that the herdr session of magnum's review panes,
// where a local round opens its panes (Deps.ReviewHerdrUp), does not answer
// ("" when it does, or nothing asks): the round waits for it, the last of
// its waits, and a round that ended while it was down runs again
// (decideRound), never ending its cycle unavailable for it. While no
// herdr server runs there (herdr.NoServer), ReviewReveal opens it, at most
// every herdrRevealGap, as the factory opens its own session; one that runs
// but does not answer keeps its client, which an open would only bring to
// the front. Your own session the factory never opens: it says the command.
// The session is pinged once a tick (reviewPing), as checkHerdr pings the
// factory's: the tasks that wait on it, and their rounds' ends, read that
// answer, so a server that hangs costs the loop one ping's timeout a tick.
func (e *Engine) reviewHerdrDown(ctx context.Context) string {
	if e.d.ReviewHerdrUp == nil {
		return ""
	}
	if e.ticks == 0 || e.reviewPing.tick != e.ticks {
		e.reviewPing.tick, e.reviewPing.err = e.ticks, e.d.ReviewHerdrUp(ctx)
	}
	err := e.reviewPing.err
	if err == nil {
		return ""
	}
	name := e.cfg.ReviewHerdr().Name
	start := "herdr"
	if name != "default" {
		start += " --session " + name
	}
	if !herdr.NoServer(err) {
		return fmt.Sprintf("magnum's review session %s does not answer; the local review waits for it", name)
	}
	what := fmt.Sprintf("no herdr server runs in magnum's review session %s; the local review waits for it", name)
	if e.d.ReviewReveal == nil {
		return what + ": start it (`" + start + "`)"
	}
	if now := e.now(); e.reviewRevealed.IsZero() || now.Sub(e.reviewRevealed) >= herdrRevealGap {
		e.reviewRevealed = now
		phase, text := registry.PhaseInfo, "herdr: opened `"+start+"` for the local review"
		if rerr := e.d.ReviewReveal(ctx); rerr != nil {
			phase, text = registry.PhaseFail, text+": "+rerr.Error()
		}
		e.event(ctx, 0, "hold", phase, text)
	}
	return what + " (the factory opens `" + start + "` at most twice an hour)"
}

// runsCodex reports whether a round of roles runs a Codex role.
func (e *Engine) runsCodex(roles []string) bool {
	return slices.ContainsFunc(roles, func(name string) bool {
		r, ok := e.cfg.RoleByNameOrAlias(nil, name)
		return ok && r.AgentKind() == config.KindCodex
	})
}

// codexSoft says that Codex is at or past [usage] codex_soft, "" when it is
// not, the cap is off or no gauge reads (the round's own health checks stop
// at a limit then).
func (e *Engine) codexSoft(ctx context.Context) string {
	u := e.cfg.Usage
	if u.CodexSoft <= 0 && u.CodexHard <= 0 {
		return ""
	}
	read := e.d.Usage
	if read == nil {
		home := u.CodexHome
		read = func(ctx context.Context, now time.Time) (usage.Snapshot, error) { return usage.Codex(ctx, home, now) }
	}
	snap, err := read(ctx, e.now())
	if err != nil || usage.Decide(snap, u.CodexSoft, u.CodexHard) == usage.OK {
		return ""
	}
	return fmt.Sprintf("codex at %.0f%%, at or past [usage] codex_soft %.0f%%: the local review yields to magnum's reviews", snap.Used(), u.CodexSoft)
}

// reviewRoles are the roles round `round` of task t's local review runs:
// the watch's review roles, without [factory.local_review] later_without
// after round 1. On a task Codex never works on again, no Codex role runs,
// and magnum's judge_fallback takes the Codex judge's place; a judge that
// cannot be replaced so is a needsYou.
func (e *Engine) reviewRoles(t registry.Task, w *config.Watch, round int) ([]string, error) {
	var out []string
	for _, r := range e.cfg.RolesFor(w) {
		if round > 1 && slices.ContainsFunc(e.cfg.Factory.LocalReview.LaterWithout, r.Matches) {
			continue
		}
		if t.CodexNever && r.AgentKind() == config.KindCodex {
			if !r.Judge {
				continue // Codex never sees the task again
			}
			fb, ok := e.cfg.JudgeFallbackFor(w)
			if !ok || !fb.IsAgent() || fb.AgentKind() == config.KindCodex {
				return nil, needsYou{"Codex never works on " + TaskKey(t) + " again and the review's judge " + r.Name +
					" runs on Codex: set [pipeline] judge_fallback (or the watch's) to a judge of another kind"}
			}
			r = fb
		}
		if !slices.Contains(out, r.Name) {
			out = append(out, r.Name)
		}
	}
	return out, nil
}

// localCase is round `round` of task t's change in copy c: the branch's
// head and its base (bh), the title with the Jira key, the plan as the body
// (report.md), the ticket (ticket.md, else the operator's request), the
// watch, the round's roles and the copy's environment, the one the
// factory's agents get in it (its panes use the copy's databases).
func (e *Engine) localCase(t registry.Task, w config.Watch, c Copy, bh branchHead, repo config.FactoryRepo, roles []string, round int) evalrun.LocalCase {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return ""
		}
		return string(b)
	}
	title := strings.TrimSpace(t.Title)
	if t.JiraKey != "" && !strings.HasPrefix(title, t.JiraKey) {
		title = strings.TrimSpace(t.JiraKey + " " + title)
	}
	return evalrun.LocalCase{Checkout: c.Path, BaseSHA: bh.Base, HeadSHA: bh.Head, Branch: t.Branch, Title: title, Body: read(reportFile),
		TicketMD: cmp.Or(read(ticketFile), read("prompt.md")), Watch: w, Roles: roles, Round: round, TaskID: t.ID, Repo: t.Repo, Base: baseOf(repo),
		Env: maps.Clone(c.Env)}
}

// launchRound runs local round lc of cycle cy off the loop, holding the
// heavy slot (release frees it), on the engine's context and within
// roundCap, while the task waits saying the round runs and the record of
// the work a restart would cut names it. Its result goes to the task's
// directory as local-review-<n>.json, however it ended: a round over its
// cap as a timeout, one that could not run as an error, a panic too. A
// drain, or the factory stopping it (sweepRounds), writes nothing: the
// round runs again in its cycle.
func (e *Engine) launchRound(ctx context.Context, t registry.Task, cy registry.Cycle, lc evalrun.LocalCase, release func()) {
	rctx, cancel := context.WithTimeout(e.bgCtx, e.roundCap)
	r := &localRound{cycle: cy.ID, n: cy.N, round: lc.Round, done: make(chan struct{}), cancel: cancel}
	e.reviewMu.Lock()
	e.reviews[t.ID] = r
	e.reviewMu.Unlock()
	endWork := e.beginWork(WorkRound, t.ID, fmt.Sprintf("local review round %d of %s", lc.Round, TaskKey(t)))
	e.waitOn(ctx, t, waitReview, fmt.Sprintf("round %d of the local review runs", lc.Round))
	e.event(ctx, t.ID, "review", registry.PhaseBegin, fmt.Sprintf("round %d (cycle %d) at %s: %s", lc.Round, cy.N, short(lc.HeadSHA),
		strings.Join(lc.Roles, ", ")))
	path := filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), reviewRoundFile(cy.N))
	e.bg.Go(func() {
		defer endWork()
		defer close(r.done)
		defer cancel()
		defer release()
		var res evalrun.LocalResult
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					e.log.Error("panic in a local review round; the daemon goes on", "task", t.ID, "panic", fmt.Sprint(p),
						"stack", execx.Redact(string(debug.Stack())))
					res, err = evalrun.LocalResult{}, fmt.Errorf("the round panicked: %v", p)
				}
			}()
			res, err = e.d.Review.Round(rctx, lc)
		}()
		bg := context.Background()
		switch {
		case e.bgCtx.Err() != nil || r.stopped.Load():
			r.drained = true
			e.event(bg, t.ID, "review", registry.PhaseFail, fmt.Sprintf("round %d (cycle %d) stopped before its end: it runs again", lc.Round, cy.N))
			return
		case rctx.Err() != nil && (err != nil || res.Outcome == "" || res.Outcome == pipeline.OutcomeStopped):
			res = evalrun.LocalResult{Outcome: pipeline.OutcomeTimeout, Error: "over its cap of " + e.roundCap.String()}
		case err != nil:
			res = evalrun.LocalResult{Outcome: pipeline.OutcomeError, Error: err.Error()}
			if ce, ok := errors.AsType[*evalrun.ConfigError](err); ok {
				res.ConfigKey = ce.Key // running it again changes nothing
			}
		}
		res.Round = lc.Round
		b, merr := json.MarshalIndent(res, "", "  ")
		if merr == nil {
			merr = fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
		}
		if merr != nil {
			r.err = fmt.Errorf("record round %d of the local review: %w", lc.Round, merr)
			e.event(bg, t.ID, "review", registry.PhaseFail, r.err.Error())
			return
		}
		text := fmt.Sprintf("round %d (cycle %d) ended %s", lc.Round, cy.N, res.Outcome)
		if res.Error != "" {
			text += ": " + roundError(res.Error)
		}
		e.event(bg, t.ID, "review", registry.PhaseOK, text)
	})
}

// goOnCycle goes on with open cycle cy, round `round` of the loop: its
// round while it runs (the task's wait says so), then its result as the
// round left it (local-review-<n>.json; none, after a restart or a round
// set aside, runs the round again in the cycle), read once (decideRound) and
// then its fix (fixPhase).
func (e *Engine) goOnCycle(ctx context.Context, t registry.Task, st config.FactoryStage, c Copy, cy registry.Cycle, loop []registry.Cycle) (bool, error) {
	round := len(loop)
	if r := e.roundOf(t.ID); r != nil {
		if r.cycle == cy.ID && !r.ended() {
			return false, nil
		}
		e.forgetRound(t.ID, r)
		if r.cycle == cy.ID && r.err != nil {
			return false, r.err // its result was lost: the round runs again on the next pass
		}
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	res, ok, err := readRoundResult(dir, cy.N)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, e.startRound(ctx, t, c, &cy, round)
	}
	if t.WaitsWho == waitReview {
		e.waitOn(ctx, t, "", "")
		t.WaitsWho, t.WaitsWhat = "", ""
	}
	if rf, ok, err := readReviewFile(dir, cy.N); err != nil {
		return false, err
	} else if ok {
		return e.fixPhase(ctx, t, st, cy, rf)
	}
	// What the round left in the copy is never the writer's, nor read past:
	// the round's roles work in the copy, and its result is read only on a
	// copy as the round found it. It is saved and put back when only the
	// round could have made it, else the task needs you (putBackRound).
	if p, pending, err := e.keptFirst(ctx, &t); err != nil || pending {
		return false, err
	} else if p != nil {
		return false, e.stageFailed(ctx, t, needsYou{p.why})
	}
	left, err := e.roundLeft(ctx, t, c, cy.HeadBefore, cy.N)
	if err != nil {
		return false, err
	}
	if !left.none() {
		why, err := e.putBackRound(ctx, t, c, cy, round, left)
		if err != nil {
			return false, err
		}
		if why != "" {
			return false, e.stageFailed(ctx, t, needsYou{why})
		}
	}
	return e.decideRound(ctx, t, st, cy, loop, res)
}

// roundLeft is what a round left in task t's copy c that keeps its result
// from being read (roundLeftovers): tracked changes, the copy off the
// task's branch or the branch at another head than head, the one the round
// reviewed, and untracked files, one by one, that the listing taken before
// cycle n (untrackedBeforeFile) does not have and that are no tracked
// change's path (a file the round took out of the index goes back as
// tracked: unindexed), which the writer's `git add -A` would commit. A
// tracked change of a path HEAD lacks (the round staged it) or that the
// listing names may be a file of yours, ignored or not (unstage): only its
// index goes back. Files are compared, never git status's entries, which
// group a directory's files once and not once the round stages one of them.
// Nothing of it is removed here.
func (e *Engine) roundLeft(ctx context.Context, t registry.Task, c Copy, head string, n int) (roundLeftovers, error) {
	var left roundLeftovers
	git := e.d.Git
	entries, err := git.StatusPaths(ctx, c.Path)
	if err != nil {
		return left, err
	}
	headLacks, keep := map[string]bool{}, map[string]bool{}
	for _, en := range entries {
		if en.Untracked() {
			continue
		}
		left.tracked = append(left.tracked, en.Path)
		// A path the index got since HEAD (added, intent to add, a rename's
		// or a copy's new name): it may be a file of yours the round staged.
		// When the work tree changed or deleted it since, the index holds the
		// only copy of what was staged: it stays staged.
		if strings.ContainsRune("ARC", rune(en.X)) || strings.ContainsRune("ARC", rune(en.Y)) {
			headLacks[en.Path] = true
			keep[en.Path] = strings.ContainsRune("MTD", rune(en.Y))
		}
	}
	slices.Sort(left.tracked)
	left.tracked = slices.Compact(left.tracked)
	left.keepStaged = slices.DeleteFunc(slices.Clone(left.tracked), func(p string) bool { return !keep[p] })
	cur, err := git.CurrentBranch(ctx, c.Path)
	if err != nil {
		return left, err
	}
	if cur != t.Branch {
		left.moved = fmt.Sprintf("the copy on %s, not on %s", cmpJoin(nonEmpty(cur), "a detached HEAD"), t.Branch)
	} else {
		now, err := git.RevParse(ctx, c.Path, "HEAD")
		if err != nil {
			return left, err
		}
		if now != head {
			left.moved = fmt.Sprintf("%s at %s, not at %s, which it reviewed", t.Branch, short(now), short(head))
		}
	}
	// Without a listing (a round an older build began) or with one that does
	// not parse, nothing tells new files from yours: none is new, and every
	// file the round staged goes back out of the index only.
	var listed []string
	b, err := os.ReadFile(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), untrackedBeforeFile(n)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return left, err
	}
	if known := err == nil && json.Unmarshal(b, &listed) == nil; !known {
		left.unstage = slices.DeleteFunc(slices.Clone(left.tracked), func(p string) bool { return !headLacks[p] })
		return left, nil
	}
	left.before = newBeforeListing(listed)
	untracked, err := git.UntrackedFiles(ctx, c.Path)
	if err != nil {
		return left, err
	}
	inTree := make(map[string]bool, len(untracked))
	for _, p := range untracked {
		inTree[p] = true
	}
	for _, p := range left.tracked {
		switch {
		case headLacks[p] || left.before.names(p): // HEAD lacks it: only the index goes back
			left.unstage = append(left.unstage, p)
		case inTree[p]: // out of the index, still in the work tree
			left.unindexed = append(left.unindexed, p)
		}
	}
	tracked := make(map[string]bool, len(left.tracked))
	for _, p := range left.tracked {
		tracked[p] = true
	}
	left.added = slices.DeleteFunc(untracked, func(p string) bool { return left.before.names(p) || tracked[p] })
	return left, nil
}

// readRoundResult reads the result round n left in dir; ok is false when
// there is none. One that does not parse is set aside, so the round runs
// again.
func readRoundResult(dir string, n int) (evalrun.LocalResult, bool, error) {
	path := filepath.Join(dir, reviewRoundFile(n))
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return evalrun.LocalResult{}, false, nil
	}
	if err != nil {
		return evalrun.LocalResult{}, false, err
	}
	var res evalrun.LocalResult
	if err := json.Unmarshal(b, &res); err != nil {
		if err := setAside(dir, n, "unread"); err != nil {
			return res, false, err
		}
		return res, false, nil
	}
	return res, true, nil
}

// readReviewFile reads review-<n>.json in dir; ok is false when there is
// none.
func readReviewFile(dir string, n int) (ReviewFile, bool, error) {
	var rf ReviewFile
	b, err := os.ReadFile(filepath.Join(dir, reviewFile(n)))
	if errors.Is(err, fs.ErrNotExist) {
		return rf, false, nil
	}
	if err != nil {
		return rf, false, err
	}
	if err := json.Unmarshal(b, &rf); err != nil {
		return rf, false, fmt.Errorf("%s: %w", reviewFile(n), err)
	}
	return rf, true, nil
}

// setAside keeps round n's result in dir under another name
// (local-review-<n>-<why>-<k>.json), so the round runs again in its cycle.
func setAside(dir string, n int, why string) error {
	for k := 1; ; k++ {
		to := filepath.Join(dir, fmt.Sprintf("local-review-%d-%s-%d.json", n, why, k))
		if fsx.Exists(to) {
			continue
		}
		return os.Rename(filepath.Join(dir, reviewRoundFile(n)), to)
	}
}

// roundTries counts round n's results set aside, for why, to run it again:
// "retry" a round that did not finish (an error, a timeout), "missing" one
// that went without a reviewer's report. Each cause has its one run again.
func roundTries(dir string, n int, why string) int {
	names, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("local-review-%d-%s-*.json", n, why)))
	return len(names)
}

// decideRound reads cycle cy's round, round `round` of the loop (res):
//   - a Codex refusal marks the task Codex-never for good, and the round
//     runs again without Codex (reviewRoles), never on it;
//   - a round that could not run for a fault of the configuration (no gh
//     identity: res.ConfigKey) ends its cycle unavailable at once, and the
//     task needs you naming the key: running it again changes nothing;
//   - a round that did not finish, or went without a reviewer's report,
//     while magnum's review session does not answer runs again once it
//     does (reviewHerdrDown), never unavailable for it;
//   - a round that did not finish (Blocks: unavailable) runs once more when
//     it ended in an error or a timeout, then ends its cycle unavailable,
//     and the task needs you with why: never read as clean;
//   - a clean round that went without a reviewer's report runs once more,
//     and a second one without it ends its cycle unavailable (review
//     unavailable, no report from the reviewers): what they would have
//     found is unknown;
//   - else its findings are recorded (seeFindings) and what blocks decides:
//     nothing (or only findings the writer declined with their risk) ends
//     the loop clean, only doubts on an oscillation (the stage passes for
//     both, with the summary for the ready card), anything else at the cap
//     needs you, and before it the writer gets the findings (fixPhase), the
//     product questions among them asked first.
func (e *Engine) decideRound(ctx context.Context, t registry.Task, st config.FactoryStage, cy registry.Cycle, loop []registry.Cycle,
	res evalrun.LocalResult) (bool, error) {
	round := len(loop)
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	again := func(why, said string) (bool, error) {
		if err := setAside(dir, cy.N, why); err != nil {
			return false, err
		}
		e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("round %d (cycle %d) %s: it runs once more", round, cy.N, said))
		return false, nil
	}
	if res.Outcome == pipeline.OutcomeRefused && res.Refused == config.KindCodex {
		if err := e.markCodexNever(ctx, t, fmt.Sprintf("Codex refused round %d of the local review", round)); err != nil {
			return false, err
		}
		return again("refused", "was refused by Codex; Codex never works on "+TaskKey(t)+" again")
	}
	blocks, unavailable, why := Blocks(res)
	switch {
	case unavailable && res.ConfigKey != "":
		// A configuration fault: running it again changes nothing.
		return false, e.stopForYou(ctx, t, cy, registry.ExitUnavailable, why+": "+roundError(res.Error), "the local review cannot run")
	case (unavailable || (!blocks && len(res.Missing) > 0)) && e.reviewHerdrDown(ctx) != "":
		// magnum's review session went away under the round: it runs
		// again once the session answers (reviewWait), never unavailable
		// for that.
		return again("herdr", "did not finish while magnum's review session did not answer")
	case unavailable && roundTries(dir, cy.N, "retry") == 0 && (res.Outcome == pipeline.OutcomeError || res.Outcome == pipeline.OutcomeTimeout):
		return again("retry", "ended "+res.Outcome)
	case unavailable:
		if res.Error != "" {
			why += ": " + roundError(res.Error)
		}
		return false, e.stopForYou(ctx, t, cy, registry.ExitUnavailable, why, "the local review did not finish")
	case !blocks && len(res.Missing) > 0 && roundTries(dir, cy.N, "missing") == 0:
		return again("missing", "went without "+strings.Join(res.Missing, ", ")+"'s report")
	case !blocks && len(res.Missing) > 0:
		// A second round without it: what it would have found is unknown,
		// so the review did not finish (Review Focus 1).
		return false, e.stopForYou(ctx, t, cy, registry.ExitUnavailable, "review unavailable (no report from "+strings.Join(res.Missing, ", ")+")",
			"the local review went without a reviewer twice")
	}
	seen, err := e.seeFindings(ctx, cy, res)
	if err != nil {
		return false, err
	}
	answered, err := e.answeredQuestions(ctx, t)
	if err != nil {
		return false, err
	}
	declined := readDeclined(dir)
	var live, questions, doubts, rebutted, leftovers []ReviewItem
	for _, s := range seen {
		item := s.item
		switch {
		case s.f.Simplification || !blockingSeverity(s.f.Severity):
			leftovers = append(leftovers, item)
		case s.reg.Outcome == registry.FindingRebutted || s.reg.Outcome == registry.FindingWaived:
			rebutted = append(rebutted, item)
		case s.f.Question && strings.EqualFold(strings.TrimSpace(answered[QuestionKey(item.Title)]), changeIt):
			live = append(live, item) // you said change it, and the round finds it still: the writer did not
		case s.f.Question && answered[QuestionKey(item.Title)] != "":
			leftovers = append(leftovers, item) // you decided it: keep it
		case s.f.Question:
			questions = append(questions, item)
		case s.reg.Reopened >= 2:
			doubts = append(doubts, item)
		default:
			live = append(live, item)
		}
	}
	if blocks && len(live)+len(questions)+len(doubts)+len(rebutted) == 0 {
		// The judge's verdict blocks with no P0 to P2 among the findings:
		// the writer gets what it flagged.
		for _, s := range seen {
			if !s.f.Simplification {
				live = append(live, s.item)
			}
		}
		if len(live) == 0 {
			live = append(live, ReviewItem{ID: "F1", Fingerprint: "verdict", Title: "the review's verdict is blocking", Body: why})
		}
		leftovers = slices.DeleteFunc(leftovers, func(i ReviewItem) bool {
			return slices.ContainsFunc(live, func(l ReviewItem) bool { return l.ID == i.ID })
		})
	}
	cycles := max(1, e.cfg.Factory.LocalReview.Cycles)
	// Every finding the writer declined with its risk, whichever round
	// raised it: the ready card and the PR body's "Known" name them.
	all := slices.SortedFunc(maps.Values(declined), func(a, b ReviewItem) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
	sum := ReviewSummary{Rounds: round, Head: cy.HeadBefore, Leftovers: leftovers, Declined: all, Doubts: doubts, Missing: loopMissing(dir, loop)}
	switch {
	case !blocks || len(live)+len(questions)+len(doubts) == 0:
		// A finding posted at a blocking priority that the counts left out
		// (the judge disagreeing with itself) is no blocker, but stays told;
		// a doubt is told among the doubts, once.
		sum.Exit, sum.Leftovers = registry.ExitClean, slices.Concat(leftovers, live, questions)
		return e.endLoop(ctx, t, cy, sum)
	case len(live)+len(questions) == 0:
		sum.Exit = registry.ExitOscillation
		return e.endLoop(ctx, t, cy, sum)
	case round >= cycles:
		return false, e.stopForYou(ctx, t, cy, registry.ExitCap, fmt.Sprintf("%s after %s", plural(len(live)+len(questions)+len(doubts), "blocking finding"),
			plural(round, "review round")), "the local review's cap")
	}
	n, err := e.attemptsOf(ctx, t, st)
	if err != nil {
		return false, err
	}
	rf := ReviewFile{Round: cy.N, Head: cy.HeadBefore, AttemptsBefore: n, Findings: append(live, questions...)}
	if len(questions) > 0 {
		if err := e.writeReviewQuestions(dir, st, questions, round); err != nil {
			return false, err
		}
	}
	if err := writeJSON(filepath.Join(dir, reviewFile(cy.N)), rf); err != nil {
		return false, err
	}
	e.event(ctx, t.ID, "review", registry.PhaseInfo, fmt.Sprintf("round %d (cycle %d) blocks: %s for the writer (%s)%s", round, cy.N,
		plural(len(live), "finding"), reviewFile(cy.N), map[bool]string{true: fmt.Sprintf(", %s for you first", plural(len(questions), "question")), false: ""}[len(questions) > 0]))
	return e.fixPhase(ctx, t, st, cy, rf)
}

// roundError is a round's own error as you read it: its first line,
// redacted and clipped.
func roundError(s string) string {
	return textx.Clip(textx.FirstLine(execx.Redact(s)), 240)
}

// loopMissing are the reviewers a round of the loop went without, by each
// cycle's result in dir (a round set aside and run again counts by its run
// again), sorted.
func loopMissing(dir string, loop []registry.Cycle) []string {
	var out []string
	for _, cy := range loop {
		var res evalrun.LocalResult
		if b, err := os.ReadFile(filepath.Join(dir, reviewRoundFile(cy.N))); err == nil && json.Unmarshal(b, &res) == nil {
			out = append(out, res.Missing...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// seenFinding is a finding of a round, as the round has it (f), as the
// factory hands it on (item) and as the registry holds it once seen (reg).
type seenFinding struct {
	f    evalrun.LocalFinding
	item ReviewItem
	reg  registry.Finding
}

// seeFindings records every finding of cycle cy's round in the registry
// (SeeFinding, by its fingerprint: seen again in one cycle, nothing
// changes), with its class (a defect, a simplification or a question) and
// whether it blocks.
func (e *Engine) seeFindings(ctx context.Context, cy registry.Cycle, res evalrun.LocalResult) ([]seenFinding, error) {
	out := make([]seenFinding, 0, len(res.Findings))
	for i, f := range res.Findings {
		fp := Fingerprint(f)
		class := "defect"
		switch {
		case f.Simplification:
			class = "simplification"
		case f.Question:
			class = "question"
		}
		reg, err := e.reg.SeeFinding(ctx, cy.ID, registry.Finding{Fingerprint: fp, Path: f.Path, StartLine: f.Line, EndLine: f.Line,
			TitleHash: titleHash(f.Title), Priority: f.Severity, Class: class, Verdict: "posted", Blocking: blockingSeverity(f.Severity) && !f.Simplification})
		if err != nil {
			return nil, err
		}
		out = append(out, seenFinding{f: f, reg: reg, item: ReviewItem{ID: fmt.Sprintf("F%d", i+1), Fingerprint: fp, Severity: f.Severity,
			Title: f.Title, Path: f.Path, Line: f.Line, Body: f.Body, Question: f.Question}})
	}
	return out, nil
}

// answeredQuestions are your answers to task t's questions, by their key
// (QuestionKey); an answer the registry keeps no text of is "answered".
func (e *Engine) answeredQuestions(ctx context.Context, t registry.Task) (map[string]string, error) {
	qs, err := e.reg.Questions(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, q := range qs {
		if q.State == registry.QuestionAnswered {
			out[QuestionKey(q.Text)] = cmp.Or(q.Answer, "answered")
		}
	}
	return out, nil
}

// changeIt is the answer to a product question of the local review that
// has the writer change the code as the finding asks (its option's label).
const changeIt = "change it"

// writeReviewQuestions writes the product questions of round `round` as
// the review stage's questions file (questions-<stage>.json, raised by the
// stage), which the task's question batch reads: each the finding's title,
// its place and body as the context, and the two ways on, none
// recommended.
func (e *Engine) writeReviewQuestions(dir string, st config.FactoryStage, qs []ReviewItem, round int) error {
	out := make([]QuestionFile, 0, len(qs))
	for _, q := range qs {
		where := q.Path
		if q.Line > 0 {
			where += ":" + strconv.Itoa(q.Line)
		}
		out = append(out, QuestionFile{Text: q.Title, Context: textx.Clip(strings.TrimSpace(strings.Join([]string{where, q.Body}, ": ")), 600),
			Options: []registry.Option{{Label: changeIt, Means: "the writer changes the code as the finding asks"},
				{Label: "keep it", Means: "the code stays as it is, and the finding is no defect"}},
			Why:      "no recommendation: it is the product's call, which the ticket leaves open",
			Evidence: fmt.Sprintf("round %d of the local review", round)})
	}
	return writeJSON(filepath.Join(dir, roleQuestionsFile(st.Name)), out)
}

// attemptsOf counts task t's attempts of stage st.
func (e *Engine) attemptsOf(ctx context.Context, t registry.Task, st config.FactoryStage) (int, error) {
	as, err := e.reg.Attempts(ctx, t.ID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range as {
		if a.Stage == st.Name {
			n = max(n, a.N)
		}
	}
	return n, nil
}

// fixPhase is cycle cy's fix: your answers to its product questions first
// (the task's question batch), then one fix turn of the coding stage's
// roles in review stage st with ReviewFixPrompt (runTurn, its review's end
// fact: reviewTurn), until an attempt after the round's passed; then the
// cycle ends at the head the checks passed (closeFix), and the next round
// runs on the next pass.
func (e *Engine) fixPhase(ctx context.Context, t registry.Task, st config.FactoryStage, cy registry.Cycle, rf ReviewFile) (bool, error) {
	if slices.ContainsFunc(rf.Findings, func(i ReviewItem) bool { return i.Question }) {
		if waits, err := e.awaitAnswers(ctx, t); err != nil || waits {
			return false, err
		}
	}
	done, err := e.fixDone(ctx, t, st, rf)
	if err != nil {
		return false, err
	}
	if !done {
		roles := e.writerRoles(t)
		if len(roles) == 0 {
			return false, e.stageFailed(ctx, t, needsYou{"no stage of the " + t.Kind + " kind writes the change: the local review has no writer to fix it"})
		}
		var live []string
		for _, i := range rf.Findings {
			if !i.Question {
				live = append(live, i.ID)
			}
		}
		turn := config.FactoryStage{Name: st.Name, Type: config.FactoryStageAgents, Roles: roles, End: config.FactoryEndProof, Prompt: ReviewFixPrompt}
		if done, err = e.runTurn(ctx, t, turn, agentsTurn{review: &reviewTurn{n: cy.N, reviewed: cy.HeadBefore, live: live}}); err != nil || !done {
			return false, err
		}
	}
	return false, e.closeFix(ctx, t, cy, rf)
}

// fixDone reports whether a fix turn of round rf passed: an attempt of the
// review stage after the round's is verified.
func (e *Engine) fixDone(ctx context.Context, t registry.Task, st config.FactoryStage, rf ReviewFile) (bool, error) {
	as, err := e.reg.Attempts(ctx, t.ID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(as, func(a registry.Attempt) bool {
		return a.Stage == st.Name && a.N > rf.AttemptsBefore && a.State == registry.AttemptVerified
	}), nil
}

// writerRoles are the roles of the first agents stage of task t's kind that
// ends on the proof: the writer.
func (e *Engine) writerRoles(t registry.Task) []string {
	for _, st := range e.cfg.Factory.Kinds[t.Kind].Stages {
		if st.Type == config.FactoryStageAgents && st.End == config.FactoryEndProof {
			return st.Roles
		}
	}
	return nil
}

// closeFix ends cycle cy after its fix passed: each finding the writer
// declined with its risk is rebutted (the risk kept for the ready card),
// one declined without a risk stays open, every other one is fixed by the
// head the factory's checks passed at, which the cycle ends at (it is the
// task's head too). A cycle ended already is fine: a restart came between.
func (e *Engine) closeFix(ctx context.Context, t registry.Task, cy registry.Cycle, rf ReviewFile) error {
	cur, err := e.reg.TaskByID(ctx, t.ID)
	if err != nil {
		return err
	}
	head := cur.HeadSHA
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	var rep ReviewReply
	if b, err := os.ReadFile(filepath.Join(dir, reviewReplyFile(cy.N))); err == nil {
		_ = json.Unmarshal(b, &rep) // the end fact read it
	}
	risks := rep.declinedWithRisk()
	declinedNoRisk := map[string]bool{}
	for _, r := range rep.Replies {
		if r.Action == replyDeclined && risks[r.ID] == "" {
			declinedNoRisk[r.ID] = true
		}
	}
	findings, err := e.reg.Findings(ctx, t.ID)
	if err != nil {
		return err
	}
	byFP := map[string]registry.Finding{}
	for _, f := range findings {
		byFP[f.Fingerprint] = f
	}
	declined := readDeclined(dir)
	fixed, kept := 0, 0
	for _, i := range rf.Findings {
		f, ok := byFP[i.Fingerprint]
		switch {
		case !ok:
		case risks[i.ID] != "":
			if err := e.reg.SettleFinding(ctx, f.ID, registry.FindingRebutted, ""); err != nil {
				return err
			}
			i.Risk = risks[i.ID]
			declined[i.Fingerprint] = i
			kept++
		case declinedNoRisk[i.ID]:
			kept++ // a decline without a risk counts as open
		default:
			if err := e.reg.SettleFinding(ctx, f.ID, registry.FindingFixed, head); err != nil {
				return err
			}
			fixed++
		}
	}
	if len(declined) > 0 {
		if err := writeJSON(filepath.Join(dir, reviewDeclinedFile), declined); err != nil {
			return err
		}
	}
	if err := e.endCycle(ctx, cy, head, ""); err != nil {
		return err
	}
	e.event(ctx, t.ID, "review", registry.PhaseOK, fmt.Sprintf("cycle %d's fix at %s: %d fixed, %d declined", cy.N, short(head), fixed, kept))
	return nil
}

// readDeclined is review-declined.json in dir: the findings the writer
// declined with the risk it named, by fingerprint.
func readDeclined(dir string) map[string]ReviewItem {
	out := map[string]ReviewItem{}
	if b, err := os.ReadFile(filepath.Join(dir, reviewDeclinedFile)); err == nil {
		_ = json.Unmarshal(b, &out) // an unreadable one names no risk
	}
	return out
}

// endCycle ends cycle cy at head with exit; one ended already (a restart
// came between) is fine.
func (e *Engine) endCycle(ctx context.Context, cy registry.Cycle, head, exit string) error {
	delta := ""
	if cy.HeadBefore != "" && head != "" && head != cy.HeadBefore {
		delta = short(cy.HeadBefore) + ".." + short(head)
	}
	if err := e.reg.EndCycle(ctx, cy.ID, head, delta, exit, 0); err != nil && !errors.Is(err, registry.ErrConflict) {
		return err
	}
	return nil
}

// endLoop ends the loop on cycle cy with sum's exit (clean or an
// oscillation): the summary for the ready card and the PR body first, then
// the cycle; the stage passes.
func (e *Engine) endLoop(ctx context.Context, t registry.Task, cy registry.Cycle, sum ReviewSummary) (bool, error) {
	if err := writeJSON(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), reviewSummaryFile), sum); err != nil {
		return false, err
	}
	if err := e.endCycle(ctx, cy, cy.HeadBefore, sum.Exit); err != nil {
		return false, err
	}
	e.event(ctx, t.ID, "review", registry.PhaseOK, fmt.Sprintf("the local review ended %s after %s: %d left, %d declined, %d doubts",
		sum.Exit, plural(sum.Rounds, "round"), len(sum.Leftovers), len(sum.Declined), len(sum.Doubts)))
	return true, nil
}

// markCodexNever marks task t never for Codex again (a refusal of a local
// round, why), with a codex_never event; a task marked already is left.
func (e *Engine) markCodexNever(ctx context.Context, t registry.Task, why string) error {
	if t.CodexNever {
		return nil
	}
	if err := e.reg.TransitionTask(ctx, t.ID, nil, "", func(u *registry.TaskUpdate) {
		u.Set("codex_never", true)
		u.Because(why)
	}); err != nil {
		return err
	}
	e.event(ctx, t.ID, "codex_never", registry.PhaseInfo, why+"; Codex never works on "+TaskKey(t)+" again")
	return nil
}

// writeJSON writes v to path as indented JSON, atomically and private.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// reviewTurn is the review's part of a fix turn's end fact (agentsTurn): the
// cycle's number n, the head the round reviewed, and the ids of the
// findings that need a fix (not the questions).
type reviewTurn struct {
	n        int
	reviewed string
	live     []string
}

// replyProblem is what keeps the writer's reply from being read: missing,
// older than since, not JSON, or a reply with another action than fixed or
// declined.
func (rt *reviewTurn) replyProblem(dir string, since time.Time) *endProblem {
	name := reviewReplyFile(rt.n)
	text, p := readFresh(dir, name, since)
	if p != nil {
		return p
	}
	var rep ReviewReply
	if err := json.Unmarshal([]byte(text), &rep); err != nil {
		return &endProblem{file: name, why: "not valid JSON (" + err.Error() + `): write {"replies": [{"id": "F1", "action": "fixed"}]}`}
	}
	for _, r := range rep.Replies {
		if r.Action != replyFixed && r.Action != replyDeclined {
			return &endProblem{file: name, why: fmt.Sprintf("the reply to %s says %q: write fixed, or declined with its risk", r.ID, r.Action)}
		}
	}
	return nil
}

// commitProblem is the fix turn's change problem when the branch has no
// commit since the round reviewed it (head is still that head) while a
// finding needs a fix: one the writer neither fixed in a commit nor
// declined with its risk.
func (rt *reviewTurn) commitProblem(dir, head string) *endProblem {
	if head == "" || head != rt.reviewed {
		return nil
	}
	var rep ReviewReply
	if b, err := os.ReadFile(filepath.Join(dir, reviewReplyFile(rt.n))); err == nil {
		_ = json.Unmarshal(b, &rep) // replyProblem read it
	}
	risks := rep.declinedWithRisk()
	if !slices.ContainsFunc(rt.live, func(id string) bool { return risks[id] == "" }) {
		return nil // every finding declined with its risk: nothing to commit
	}
	return &endProblem{file: "the branch", change: true, why: fmt.Sprintf("the branch has no commit since the local review reviewed %s: "+
		"commit your fixes, or decline each finding with its risk", short(rt.reviewed))}
}
