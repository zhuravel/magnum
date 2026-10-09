package engine

// The self-improving loop's schedule (DECISIONS "Learn jobs run one at a
// time" and "The editor runs after a retro with new general evidence"), its
// fifth stage. The learn jobs, the retro, a notes curation, a skill edit
// and the gate of a skill proposal, each run in a goroutine of their own,
// one at a time: a start holds learnMu while it checks that no other learn
// job runs and marks its own running, so the daily retro waits for a
// curation (and the other way round) instead of both driving agents at
// once, and the gate replays nothing while an edit writes. The editor runs
// on its own at the loop's scan (every learnCheckEvery) once a retro stored
// new general evidence (KVLearnEvidence: a new general miss of P0 to P2 or
// a new general finding declined as wrong or over-ranked), at most once a
// local day (KVLearnEditDay), and only when nothing holds it (editWait).

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// KVLearnEvidence marks new general evidence for the skill editor: the
	// time of the retro that stored a general miss of P0 to P2 or a general
	// finding declined as wrong or over-ranked that was not an input
	// before (store.FormatTime). The scheduled editor clears it when it
	// starts, and sets it again when its run stops short.
	KVLearnEvidence = "learn.evidence"
	// KVLearnEditDay is the local day (store.DayKey) the scheduled editor
	// last started on; it starts at most once a day.
	KVLearnEditDay = "learn.edit_day"
	// learnCheckEvery spaces the loop's scans (maybeLearnScan).
	learnCheckEvery = 10 * time.Minute
	// goProbeTimeout bounds the check that go runs in the checkout.
	goProbeTimeout = time.Minute
)

// learnRunning is the learn job running now, named, and when it started
// ("" = none): the retro, a notes curation, a skill edit (or revert) or the
// gate of a skill proposal.
func (e *Engine) learnRunning() (string, time.Time) {
	e.retroMu.Lock()
	retro, retroAt := e.retroCancel != nil, e.retroStarted
	e.retroMu.Unlock()
	if retro {
		return "the retro", retroAt
	}
	e.curateMu.Lock()
	curate, curateAt, repo := e.curateCancel != nil, e.curateStarted, e.curateRepo
	e.curateMu.Unlock()
	if curate {
		return "the notes curation of " + repo, curateAt
	}
	e.editMu.Lock()
	edit, editAt, what := e.editCancel != nil, e.editStarted, e.editWhat
	e.editMu.Unlock()
	if edit {
		return cmp.Or(what, "the skill edit"), editAt
	}
	e.gateMu.Lock()
	gate, gateAt, proposal := e.gateCancel != nil, e.gateStarted, e.gateProposal
	e.gateMu.Unlock()
	if gate {
		return fmt.Sprintf("the skill gate of proposal %d", proposal), gateAt
	}
	return "", time.Time{}
}

// learnBusy names the learn job running now ("" = none).
func (e *Engine) learnBusy() string {
	job, _ := e.learnRunning()
	return job
}

// learnOneAtATime is why a learn job may not start now, said for an
// answer: the other learn job that runs and since when ("" = none runs).
func (e *Engine) learnOneAtATime() string {
	job, since := e.learnRunning()
	if job == "" {
		return ""
	}
	return fmt.Sprintf("%s runs (started %s), and learn jobs run one at a time; ask again once it ends", job, since.Local().Format("15:04"))
}

// maybeLearnScan is the loop's scan, at most every learnCheckEvery: the
// proposals nobody decided within ProposalTTL expire
// (expireLearnProposals), a version that regressed gets a revert
// (maybeRegression, learn_revert.go), then the scheduled editor
// (maybeEdit). A dry run and `magnum daemon --once` scan nothing.
func (e *Engine) maybeLearnScan(ctx context.Context) {
	if e.d.DryRun || e.once {
		return
	}
	now := e.now()
	if !e.learnChecked.IsZero() && now.Sub(e.learnChecked) < learnCheckEvery {
		return
	}
	e.learnChecked = now
	e.expireLearnProposals(ctx)
	e.maybeRegression(ctx)
	e.maybeEdit(ctx)
}

// expireLearnProposals expires the proposals held for the operator or
// passed for the landing that nobody decided within ProposalTTL of when
// they began to wait so (store.LearnProposal.WaitingSince; their making
// when later or unknown), as a notes proposal expires: learn.expired, the
// worktree and the branch go, and the inputs stay as they are (new, for a
// later edit). A gating one is the gate's to decide; the registry keeps
// them all. One whose branch master holds already (a landing cut short
// after the fast-forward) is recorded as landed instead (learn_land.go).
func (e *Engine) expireLearnProposals(ctx context.Context) {
	waiting, err := e.st.LearnProposals(ctx, []string{store.LearnHeld, store.LearnPassed})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "skill proposals: list the waiting ones to expire")
		return
	}
	now := e.now()
	for _, p := range waiting {
		since := p.CreatedAt
		if p.WaitingSince != nil && p.WaitingSince.After(since) {
			since = *p.WaitingSince
		}
		if now.Sub(since) < ProposalTTL {
			continue
		}
		if landed, ok, err := e.landedAlready(ctx, p); err != nil {
			e.warnUnlessStopped(ctx, err, "skill proposals: is an expiring one on master", "proposal", p.ID)
			continue
		} else if ok {
			if _, _, err := e.recordLanding(context.WithoutCancel(ctx), p, landed, false); err != nil {
				e.log.Warn("skill proposals: record a landing cut short", "proposal", p.ID, "err", err)
			}
			continue
		}
		got, err := e.st.DecideLearnProposal(ctx, p.ID, []string{store.LearnHeld, store.LearnPassed}, store.LearnExpired,
			"nobody decided it within 7 days", now, nil)
		if err != nil {
			if !errors.Is(err, store.ErrConflict) {
				e.log.Warn("skill proposals: expire one", "proposal", p.ID, "err", err)
			}
			continue
		}
		e.dropCandidate(ctx, p.Scratch, p.Branch)
		e.event(ctx, "info", learnSubject, "learn.expired", fmt.Sprintf("skill proposal %d expired: %s for 7 days and nobody decided it",
			p.ID, p.State), map[string]any{"proposal": got.ID, "was": p.State})
	}
}

// maybeEdit starts the scheduled editor when editWait lets it: it records
// the day (KVLearnEditDay) and clears the evidence mark first, so it runs
// once a day and only on evidence newer than its start.
func (e *Engine) maybeEdit(ctx context.Context) {
	if why := e.editWait(ctx); why != "" {
		return
	}
	e.setKV(ctx, KVLearnEditDay, store.DayKey(e.now()))
	e.delKV(ctx, KVLearnEvidence)
	e.startEdit(ctx, store.LearnTriggerEvidence)
}

// editWait is why the scheduled editor does not start now ("" = it may),
// in the order it checks them: [learn] editor off, nothing to run it
// (editUnavailable), no new general evidence since its last start, a start
// today already, another learn job running, a proposal that waits for the
// gate, the landing or the operator, a round of any repository in its
// judge stage (the judge is about to read its skill copy), a drain or an
// infrastructure pause, `magnum pause`, a pause of the editor's CLI, and
// go that does not run in the checkout (the format gate runs it there).
func (e *Engine) editWait(ctx context.Context) string {
	if !e.cfg.Learn.Editor {
		return "[learn] editor is off"
	}
	if why := e.editUnavailable(); why != "" {
		return why
	}
	if v, ok := e.getKV(ctx, KVLearnEvidence); !ok || v == "" {
		return "no new general miss or declined finding since the last edit"
	}
	if day, _ := e.getKV(ctx, KVLearnEditDay); day == store.DayKey(e.now()) {
		return "the editor ran today"
	}
	if job := e.learnBusy(); job != "" {
		return job + " runs"
	}
	waiting, err := e.st.LearnProposals(ctx, []string{store.LearnGating, store.LearnPassed, store.LearnHeld})
	if err != nil {
		return "the proposals cannot be read"
	}
	if len(waiting) > 0 {
		p := waiting[len(waiting)-1]
		return fmt.Sprintf("proposal %d is %s", p.ID, p.State)
	}
	if e.notesJudging(ctx, 0) {
		return "a round is in its judge stage"
	}
	if why := cmp.Or(e.holdReason(ctx), e.userPause(ctx), e.editToolPause(ctx)); why != "" {
		return why
	}
	return e.goMissing(ctx)
}

// goMissing is why go does not run in the checkout's environment, where the
// format gate runs `go test` ("" = it runs): `go version` through
// Deps.GoTest, logged once a while it fails.
func (e *Engine) goMissing(ctx context.Context) string {
	gctx, cancel := context.WithTimeout(ctx, goProbeTimeout)
	defer cancel()
	out, err := e.d.GoTest(gctx, e.d.Checkout, "go", "version")
	if err == nil {
		return ""
	}
	why := "go does not run in the checkout: " + oneLine(cmp.Or(strings.TrimSpace(lastLines(out, 1)), err.Error()), retroWhyRunes)
	if e.logOnce("learn-go", why, e.now()) {
		e.log.Warn("the scheduled skill edit waits: " + why)
	}
	return why
}

// scheduledEditStopped follows a scheduled editor run that ended with err:
// one that stopped short with inputs left (a limit, a shutdown, an agent
// that cannot start) marks the evidence again, so a later day's scan tries
// again; one that found nothing to learn from does not.
func (e *Engine) scheduledEditStopped(ctx context.Context, err error) {
	if err == nil || errors.Is(err, errNothingToLearn) {
		return
	}
	octx := context.WithoutCancel(ctx)
	if ids, ierr := e.generalEvidence(octx); ierr != nil || len(ids) == 0 {
		return
	}
	e.setKV(octx, KVLearnEvidence, store.FormatTime(e.now()))
}

// errNothingToLearn is an edit run that found no input.
var errNothingToLearn = errors.New("no new general miss or declined finding to learn from")

// generalEvidence lists the editor's inputs now, by their inputs.json ids:
// the general misses of P0 to P2 and the general findings declined as
// wrong or over-ranked, still new.
func (e *Engine) generalEvidence(ctx context.Context) ([]string, error) {
	misses, err := e.st.Misses(ctx, store.MissFilter{Classes: []string{store.MissMiss}, Scopes: []string{store.MissScopeGeneral},
		States: []string{store.MissNew}})
	if err != nil {
		return nil, err
	}
	declines, err := e.st.Declines(ctx, store.DeclineFilter{Verdicts: []string{store.VerdictWrong, store.VerdictOverRanked},
		Scopes: []string{store.MissScopeGeneral}, States: []string{store.MissNew}})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range misses {
		if slices.Contains(editSeverities, m.Severity) {
			out = append(out, learn.MissInputID(m.ID))
		}
	}
	for _, d := range declines {
		out = append(out, learn.DeclineInputID(d.ID))
	}
	return out, nil
}

// newEvidence counts the inputs of after that before lacks.
func newEvidence(before, after []string) int {
	n := 0
	for _, id := range after {
		if !slices.Contains(before, id) {
			n++
		}
	}
	return n
}

// markEvidence records that a retro stored n new general inputs for the
// editor (KVLearnEvidence), which wakes the scheduled editor.
func (e *Engine) markEvidence(ctx context.Context, run string, n int) {
	if n <= 0 {
		return
	}
	e.setKV(ctx, KVLearnEvidence, store.FormatTime(e.now()))
	e.log.Info("retro: new general evidence for the skill editor", "run", run, "inputs", n)
}

// EditNext says when the scheduled editor runs next, for `magnum learn`
// and its JSON: from [learn] editor, the evidence mark (KVLearnEvidence)
// and the day of its last start (KVLearnEditDay).
func EditNext(editor bool, evidence, editDay string, now time.Time) string {
	switch {
	case !editor:
		return "on request: [learn] editor is off, and `magnum learn --propose` runs the editor now"
	case evidence != "" && editDay == store.DayKey(now):
		return "tomorrow: new general evidence waits, and the editor ran today (`magnum learn --propose` runs it now)"
	case evidence != "":
		return "at the next scan (every " + humanDuration(learnCheckEvery) + ") once nothing holds it: new general evidence waits"
	}
	return "after a retro that stores a new general miss of P0 to P2 or a general finding declined as wrong or over-ranked, " +
		"at most once a day (`magnum learn --propose` runs it now)"
}
