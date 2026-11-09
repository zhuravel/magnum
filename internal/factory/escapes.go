package factory

// What the local review of a published task missed (DECISIONS "The factory
// counts what its local review missed"), checked once per task at magnum's
// first round on the task's PR that posted a review and began after the
// publish (the first begin of its publish effect, else the PR's link): each
// P0 to P2 finding that round posted (P3 is optional, and internal/escapes
// never filters by priority, so this file does) that no finding of the
// task's local review caught (the same path, within escapes.NearLines, at a
// head whose line numbers name the same code) is an escape, stored with its
// reason (no_local, far, unplaced) in the registry's escape_checks and
// escapes, which `magnum factory stats` counts. A local finding counts
// whatever its outcome and its priority (one raised and waived was caught);
// its lines are those of the head its last cycle reviewed
// (review_cycles.head_before of its last_cycle: SeeFinding takes the newest
// sighting's lines, and a round reviews its cycle's head). The local
// review's fixes usually moved the head on before magnum's round, so a local
// finding at another head than the posted round's compares with it where
// the file did not change between the two heads: git diff --name-only in a
// copy of the task's repository (one whose .git is on disk), read only, once
// per file and heads in a check, the next copy asked when git fails in one.
//
// The tick's PR follow (followPRs) starts a task's check, off the loop,
// once magnum posted a review of the PR (PRState.Review) and while the
// task has no stored check: at most one check runs per task, none while
// the daemon drains, one that found no round after the publish (magnum
// posted only before it) is not started again before magnum posts another
// review, and one that stored nothing (a registry that could not be read,
// a panic) not before escapeRetry. A check is complete when every
// comparison of a posted finding's file at two heads ran (git answered in a
// copy, within escapeGitBound) and magnum recorded the findings its round's
// result counts. An incomplete check (no copy on disk, git failing in every
// copy, the diffs out of their bound, a round whose findings magnum has not
// recorded) stores nothing either and is tried again after escapeRetry;
// only its escapeTries-th try stores it, with the files it compared at the
// same head only and the findings magnum did not record counted on the
// check (SameHeadOnly, Unrecorded), so the stats tell it from a complete
// one. A check has escapeCheckBound; its result is written once its reads
// ended, whatever the drain does then (context.WithoutCancel), in one
// transaction, never twice (RecordEscapeCheck writes nothing for a task
// checked before), and a check the daemon's stop cut writes nothing.
// Nothing of a posted finding's title is kept but the factory's hash of it
// (titleHash); the event and the warnings carry counts and the first line
// of an error, never a title or the PR's text (execx logs the git commands
// it runs, their paths included, as it logs every command).

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/escapes"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// escapeCheckBound bounds one task's escape check off the loop: the read
	// of magnum's round, the factory registry's reads, the git diffs and the
	// write.
	escapeCheckBound = 2 * time.Minute
	// escapeGitBound bounds a check's git diffs together: a check whose
	// diffs run out of it is incomplete.
	escapeGitBound = time.Minute
	// escapeRetry is how long a task's next check waits after one that
	// stored nothing (a failure, an incomplete check, a panic).
	escapeRetry = 5 * time.Minute
	// escapeTries is how many tries an incomplete check has: the last one
	// stores what it found, flagged.
	escapeTries = 6
)

// escapeChecks is the engine's record of the escape checks that run off
// the loop, behind its mutex: the tasks whose check runs, those whose check
// is stored, by task the run of magnum's last posted review a check found
// no round after the publish for (none starts again before magnum posts
// another), when a task's next check may start after one that stored
// nothing, the incomplete tries so far, and the failure last logged per
// task. gitBound bounds a check's diffs (escapeGitBound; a test's seam).
type escapeChecks struct {
	mu       sync.Mutex
	running  map[int64]bool
	checked  map[int64]bool
	none     map[int64]string
	next     map[int64]time.Time
	tries    map[int64]int
	failed   map[int64]string
	gitBound time.Duration
}

func newEscapeChecks() escapeChecks {
	return escapeChecks{running: map[int64]bool{}, checked: map[int64]bool{}, none: map[int64]string{}, next: map[int64]time.Time{},
		tries: map[int64]int{}, failed: map[int64]string{}, gitBound: escapeGitBound}
}

// followEscapes starts published task t's escape check off the loop when it
// is due (see the file's comment): st is magnum's view of the task's PR ref.
func (e *Engine) followEscapes(ctx context.Context, t registry.Task, ref PRRef, st PRState) {
	if st.Review == nil || e.draining.Load() {
		return
	}
	now := e.now()
	e.esc.mu.Lock()
	none, noted := e.esc.none[t.ID]
	skip := e.esc.checked[t.ID] || e.esc.running[t.ID] || noted && none == st.Review.RunID || now.Before(e.esc.next[t.ID])
	e.esc.mu.Unlock()
	if skip {
		return
	}
	switch _, err := e.reg.EscapeCheck(ctx, t.ID); {
	case err == nil:
		e.esc.mu.Lock()
		e.esc.checked[t.ID] = true
		e.esc.mu.Unlock()
		return
	case !errors.Is(err, registry.ErrNotFound):
		e.escapeFailed(t, err)
		return
	}
	e.startEscapeCheck(t, ref, st.Review.RunID)
}

// escapeCopies are the directories task t's escape check may run git in,
// in order: the task's copy while it holds one (copyOfTask), then each copy
// of its repository ([[factory.repo]] copies, by number), each only while
// it is a worktree (its .git is on disk; a copy half provisioned or half
// removed is a bare directory, where git would look for a repository above
// it). A published task's copy was released at the publish, and every copy
// of a repository is a worktree of its main clone, whose objects hold every
// commit any copy made, so any copy answers a diff of two of the task's
// heads; never the main clone, which may be your own checkout. None when no
// copy is on disk or no [[factory.repo]] names the repository. It stats the
// disk: the check calls it off the loop.
func (e *Engine) escapeCopies(t registry.Task) []string {
	var out []string
	add := func(dir string) {
		if dir != "" && !slices.Contains(out, dir) && worktree(dir) {
			out = append(out, dir)
		}
	}
	add(e.copyOfTask(t).Path)
	repo, ok := e.cfg.FactoryRepo(t.Repo)
	if !ok || repo.Copies == "" {
		return out
	}
	for n := 1; n <= max(repo.MaxCopies, 1); n++ {
		add(repo.Pool().Path(n))
	}
	return out
}

// worktree reports whether dir is a git checkout: its .git (a worktree's
// file, a clone's directory) is on disk.
func worktree(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// startEscapeCheck runs task t's escape check of its PR ref off the loop,
// on the engine's background context, and records its end
// (escapeChecked); review is the run of magnum's last posted review the
// loop saw.
func (e *Engine) startEscapeCheck(t registry.Task, ref PRRef, review string) {
	e.esc.mu.Lock()
	e.esc.running[t.ID] = true
	gitBound := e.esc.gitBound
	e.esc.mu.Unlock()
	e.goBackground("a published task's escape check", func(bg context.Context) {
		ended := false
		defer func() {
			e.esc.mu.Lock()
			defer e.esc.mu.Unlock()
			delete(e.esc.running, t.ID)
			if !ended { // it panicked: goBackground logs it
				e.esc.next[t.ID] = e.now().Add(escapeRetry)
			}
		}()
		ctx, cancel := context.WithTimeout(bg, escapeCheckBound)
		defer cancel()
		out, err := e.escapeCheck(ctx, t, ref, gitBound)
		ended = true
		if bg.Err() != nil {
			return // the daemon stops: the next daemon checks it
		}
		e.escapeChecked(context.WithoutCancel(ctx), t, ref, review, out, err)
	})
}

// escapeOutcome is what one escape check found: whether magnum has a
// posted round after the publish, the check and its escapes to store, how
// many escapes are unplaced, and why some comparisons did not run (""
// when every one did: see copyDiff.why).
type escapeOutcome struct {
	found    bool
	check    registry.EscapeCheck
	escapes  []registry.Escape
	unplaced int
	why      string
}

// complete reports whether every comparison ran and magnum recorded every
// finding the round's result counts.
func (o escapeOutcome) complete() bool { return o.check.SameHeadOnly == 0 && o.check.Unrecorded == 0 }

// incomplete is why the check is not complete, in words for the log.
func (o escapeOutcome) incomplete() string {
	var parts []string
	if n := o.check.SameHeadOnly; n > 0 {
		parts = append(parts, fmt.Sprintf("%s of the posted findings not compared at two heads (%s)", textx.Count(n, "file", "files"), o.why))
	}
	if n := o.check.Unrecorded; n > 0 {
		parts = append(parts, fmt.Sprintf("magnum recorded none of the %s its round's result counts",
			textx.Count(n, "P0 to P2 finding", "P0 to P2 findings")))
	}
	return strings.Join(parts, ", and ")
}

// escapeCheck reads what task t's check of its PR ref compares and matches
// it (localEscapes), the diffs within gitBound.
func (e *Engine) escapeCheck(ctx context.Context, t registry.Task, ref PRRef, gitBound time.Duration) (escapeOutcome, error) {
	link, err := e.reg.LinkedPR(ctx, t.ID)
	if err != nil {
		return escapeOutcome{}, err
	}
	since, err := e.publishBegan(ctx, t, link)
	if err != nil {
		return escapeOutcome{}, err
	}
	round, ok, err := e.d.Magnum.FirstPostedRound(ctx, ref, since)
	if err != nil || !ok {
		return escapeOutcome{}, err
	}
	locals, err := e.reg.Findings(ctx, t.ID)
	if err != nil {
		return escapeOutcome{}, err
	}
	cycles, err := e.reg.Cycles(ctx, t.ID)
	if err != nil {
		return escapeOutcome{}, err
	}
	gctx, cancel := context.WithTimeout(ctx, gitBound)
	defer cancel()
	diff := &copyDiff{ctx: gctx, git: e.d.Git, memo: map[[3]string]bool{}, posted: map[string]bool{}, uncompared: map[string]bool{}}
	if diff.git != nil {
		diff.dirs = e.escapeCopies(t)
	}
	for _, f := range round.Findings {
		if _, ok := escapePriority(f.Priority); ok && f.Path != "" {
			diff.posted[f.Path] = true
		}
	}
	out := escapeOutcome{found: true}
	out.check, out.escapes = localEscapes(t.ID, link, locals, cycles, round, diff.same)
	out.check.SameHeadOnly, out.why = len(diff.uncompared), diff.why()
	for _, es := range out.escapes {
		if es.Reason == registry.EscapeUnplaced {
			out.unplaced++
		}
	}
	return out, nil
}

// publishBegan is when task t's publish began, the cutoff of magnum's
// first posted round: the first begin of its publish effect (publishKey of
// the head the link was approved at, which keeps that time across a resume
// and a run after a failure), else, or when the PR was linked earlier, the
// link's time.
func (e *Engine) publishBegan(ctx context.Context, t registry.Task, link registry.PRLink) (time.Time, error) {
	eff, err := e.reg.EffectByKey(ctx, publishKey(t.ID, link.ApprovedHead))
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return link.Opened, nil
	case err != nil:
		return time.Time{}, err
	case !eff.Begun.IsZero() && eff.Begun.Before(link.Opened):
		return eff.Begun, nil
	}
	return link.Opened, nil
}

// copyDiff answers escapes.SameCode for one check: whether a file reads
// the same at two heads (git diff --name-only, read only) in the first of
// dirs where git answers, once per file and heads. uncompared are the
// files of the posted findings (posted) a comparison of which got no
// answer: no git or no copy, git failing in every copy, or the diffs out
// of their bound (cut); a comparison of another file (the comparable count
// of local findings alone) counts in neither.
type copyDiff struct {
	ctx        context.Context
	git        *gitx.Client
	dirs       []string
	memo       map[[3]string]bool
	posted     map[string]bool
	uncompared map[string]bool
	cut        bool
}

func (d *copyDiff) same(path, localHead, postedHead string) bool {
	key := [3]string{path, localHead, postedHead}
	if v, ok := d.memo[key]; ok {
		return v
	}
	v, answered := d.diff(path, localHead, postedHead)
	if !answered && d.posted[path] {
		d.uncompared[path] = true
	}
	d.memo[key] = v
	return v
}

// diff asks each copy in turn whether path differs between a and b: the
// first git that answers says it; false when none does.
func (d *copyDiff) diff(path, a, b string) (same, answered bool) {
	for _, dir := range d.dirs {
		if d.ctx.Err() != nil {
			break
		}
		differ, err := d.git.DifferingPaths(d.ctx, dir, a, b, path)
		if err == nil {
			return len(differ) == 0, true
		}
		// git failed in this copy (an exit error, a bad object there): the
		// next one, unless the bound ran out
	}
	if d.ctx.Err() != nil && d.posted[path] {
		d.cut = true
	}
	return false, false
}

// why is why the comparisons of uncompared did not run, "" when they all
// did.
func (d *copyDiff) why() string {
	switch {
	case len(d.uncompared) == 0:
		return ""
	case d.git == nil:
		return "no git"
	case d.cut:
		return "the diffs ran out of their bound"
	case len(d.dirs) == 0:
		return "no copy of the repository is on disk"
	}
	return "git failed in every copy"
}

// escapePriority is a posted finding's priority as an escape counts it:
// P0, P1 or P2, whatever case and spaces the judge wrote; false for a P3 or
// another word.
func escapePriority(p string) (string, bool) {
	switch p = strings.ToUpper(strings.TrimSpace(p)); p {
	case "P0", "P1", "P2":
		return p, true
	}
	return "", false
}

// localEscapes is task taskID's escape check against magnum's posted round:
// its local findings locals (the head of each the one its last cycle
// reviewed, of cycles, else the head its PR was approved or opened at, of
// link) matched with the round's P0 to P2 findings by escapes.Match, heads
// compared by same (nil: only equal heads), and the escapes as the
// registry keeps them. Comparable counts the local findings placed at a
// head comparable with the round's; Unrecorded the P0 to P2 findings the
// round's result counts when magnum recorded none of the round's findings.
func localEscapes(taskID int64, link registry.PRLink, locals []registry.Finding, cycles []registry.Cycle, round PostedRound,
	same func(path, a, b string) bool) (registry.EscapeCheck, []registry.Escape) {
	heads := make(map[int]string, len(cycles))
	for _, c := range cycles {
		heads[c.N] = c.HeadBefore
	}
	fallback := cmp.Or(link.ApprovedHead, link.OpenedHead)
	local := make([]escapes.Finding, 0, len(locals))
	for _, f := range locals {
		local = append(local, escapes.Finding{Source: escapes.SourceLocal, Ref: strconv.FormatInt(f.ID, 10), Path: f.Path, StartLine: f.StartLine,
			EndLine: f.EndLine, Priority: f.Priority, TitleHash: f.TitleHash, Round: f.LastCycle, Head: cmp.Or(heads[f.LastCycle], fallback)})
	}
	var posted []escapes.Finding
	ids := map[string]int64{}
	for _, f := range round.Findings {
		p, ok := escapePriority(f.Priority)
		if !ok {
			continue
		}
		ref := strconv.FormatInt(f.ID, 10)
		ids[ref] = f.ID
		posted = append(posted, escapes.Finding{Source: escapes.SourcePosted, Ref: ref, Path: f.Path, StartLine: f.StartLine, EndLine: f.EndLine,
			Priority: p, TitleHash: titleHash(f.Title), Round: round.Round, Head: round.Head})
	}
	var opts []escapes.Option
	if same != nil {
		opts = append(opts, escapes.SameCode(same))
	}
	check := registry.EscapeCheck{TaskID: taskID, RunID: round.RunID, Round: round.Round, HeadSHA: round.Head, Posted: len(posted)}
	if len(round.Findings) == 0 && round.Counted {
		check.Unrecorded = round.Counts[0] + round.Counts[1] + round.Counts[2]
	}
	var out []registry.Escape
	for _, m := range escapes.Match(local, posted, opts...) {
		f := m.Finding
		es := registry.Escape{TaskID: taskID, FindingID: ids[f.Ref], Path: f.Path, StartLine: f.StartLine, EndLine: f.EndLine, Priority: f.Priority,
			TitleHash: f.TitleHash, Reason: string(m.Reason)}
		if m.Reason == escapes.ReasonFar {
			es.NearestID, _ = strconv.ParseInt(m.Nearest.Ref, 10, 64)
			es.Distance = m.Distance
		}
		out = append(out, es)
	}
	for _, l := range local {
		if _, _, placed := l.Lines(); !placed || l.Path == "" {
			continue
		}
		if l.Head == round.Head || l.Head != "" && same != nil && same(l.Path, l.Head, round.Head) {
			check.Comparable++
		}
	}
	return check, out
}

// escapeChecked records the end of task t's escape check of its PR ref
// (out, or the failure err): a complete check, or the last try of an
// incomplete one, stored with its escapes and an event that counts them;
// an incomplete check before its last try, and a failure, logged once while
// its cause stays the same, the next try after escapeRetry; a round not
// found yet noted against review (the run of magnum's last posted review),
// so no check starts again before magnum posts another.
func (e *Engine) escapeChecked(ctx context.Context, t registry.Task, ref PRRef, review string, out escapeOutcome, err error) {
	if err == nil && out.found && !out.complete() {
		e.esc.mu.Lock()
		e.esc.tries[t.ID]++
		last := e.esc.tries[t.ID] >= escapeTries
		e.esc.mu.Unlock()
		if !last {
			err = fmt.Errorf("the check is incomplete: %s", out.incomplete())
		}
	}
	if err == nil && out.found {
		var recorded bool
		if recorded, err = e.reg.RecordEscapeCheck(ctx, out.check, out.escapes); err == nil {
			e.esc.mu.Lock()
			e.esc.checked[t.ID] = true
			delete(e.esc.none, t.ID)
			delete(e.esc.next, t.ID)
			delete(e.esc.tries, t.ID)
			delete(e.esc.failed, t.ID)
			e.esc.mu.Unlock()
			if recorded {
				e.event(ctx, t.ID, "escapes", registry.PhaseInfo, out.words(ref))
			}
			return
		}
	}
	if err != nil {
		e.esc.mu.Lock()
		e.esc.next[t.ID] = e.now().Add(escapeRetry)
		e.esc.mu.Unlock()
		e.escapeFailed(t, err)
		return
	}
	e.esc.mu.Lock()
	e.esc.none[t.ID] = review
	delete(e.esc.next, t.ID)
	e.esc.mu.Unlock()
}

// escapeFailed logs task t's failed escape check, once while its cause
// stays the same.
func (e *Engine) escapeFailed(t registry.Task, err error) {
	msg := textx.FirstLine(execx.Redact(err.Error()))
	e.esc.mu.Lock()
	same := e.esc.failed[t.ID] == msg
	e.esc.failed[t.ID] = msg
	e.esc.mu.Unlock()
	if !same {
		e.log.Warn("the escape check of a published task's local review failed or is incomplete; it is tried again", "task", t.ID, "err", msg)
	}
}

// words is the escapes event of a check of PR ref: counts, never a title,
// a path or the PR's text.
func (o escapeOutcome) words(ref PRRef) string {
	var by [3]int
	for _, es := range o.escapes {
		if i := strings.TrimPrefix(es.Priority, "P"); len(i) == 1 && i[0] >= '0' && i[0] <= '2' {
			by[i[0]-'0']++
		}
	}
	c := o.check
	s := fmt.Sprintf("magnum's first posted round on #%d (round %d at %s) posted %s: %d escaped the local review (P0 %d, P1 %d, P2 %d)",
		ref.Number, c.Round, short(c.HeadSHA), textx.Count(c.Posted, "P0 to P2 finding", "P0 to P2 findings"), len(o.escapes), by[0], by[1], by[2])
	if o.unplaced > 0 {
		s += fmt.Sprintf(", %d of them with no line to place", o.unplaced)
	}
	s += fmt.Sprintf("; %s of the local review at a comparable head", textx.Count(c.Comparable, "finding", "findings"))
	if c.SameHeadOnly > 0 {
		s += fmt.Sprintf("; %s compared at the same head only (%s, after %d tries)", textx.Count(c.SameHeadOnly, "file", "files"), o.why,
			escapeTries)
	}
	if c.Unrecorded > 0 {
		s += fmt.Sprintf("; magnum recorded none of the %s its round's result counts, so the escapes are unknown",
			textx.Count(c.Unrecorded, "P0 to P2 finding", "P0 to P2 findings"))
	}
	return s
}
