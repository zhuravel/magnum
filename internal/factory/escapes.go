package factory

// What the local review of a published task missed (DECISIONS "The factory
// counts what its local review missed"), checked once per task at magnum's
// first round on the task's PR that posted a review and began after the
// publish (pr_links.opened_at): each P0 to P2 finding that round posted
// (P3 is optional, and internal/escapes never filters by priority, so this
// file does) that no finding of the task's local review caught (the same
// path, within escapes.NearLines, at a head whose line numbers name the
// same code) is an escape, stored with its reason (no_local, far, unplaced)
// in the registry's escape_checks and escapes, which `magnum factory stats`
// counts. A local finding counts whatever its outcome and its priority (one
// raised and waived was caught); its lines are those of the head its last
// cycle reviewed (review_cycles.head_before of its last_cycle: SeeFinding
// takes the newest sighting's lines, and a round reviews its cycle's head).
// The local review's fixes usually moved the head on before magnum's
// round, so a local finding at another head than the posted round's
// compares with it where the file did not change between the two heads:
// git diff --name-only in a copy of the task's repository, read only, once
// per file and heads in a check; without a copy on disk, or where git
// fails, only findings at the same head compare.
//
// The tick's PR follow (followPRs) starts a task's check, off the loop,
// once magnum posted a review of the PR (PRState.Review) and while the
// task has no stored check: at most one check runs per task, none while
// the daemon drains, one that found no round after the publish (magnum
// posted only before it) is not started again before magnum posts another
// review, and one that stored nothing (a registry that could not be read,
// a panic) not before escapeRetry. A check has escapeCheckBound and its git
// diffs escapeGitBound, after which the files left compare at the same head
// only. Its result is written whatever the drain does once its reads ended
// (context.WithoutCancel), in one transaction, never twice (RecordEscapeCheck
// writes nothing for a task checked before). Nothing of a posted finding's
// title is kept but the factory's hash of it (titleHash), and the event
// and the log carry counts, never a title, a path or any text of the PR.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
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
	// escapeGitBound bounds a check's git diffs together: past it, the files
	// left compare at the same head only, as where git fails.
	escapeGitBound = time.Minute
	// escapeRetry is how long a task's next check waits after one that
	// stored nothing for a failure (or panicked).
	escapeRetry = 5 * time.Minute
)

// escapeChecks is the engine's record of the escape checks that run off
// the loop, behind its mutex: the tasks whose check runs, those whose check
// is stored, by task the run of magnum's last posted review a check found
// no round after the publish for (none starts again before magnum posts
// another), when a task's next check may start after one that stored
// nothing for a failure, and the failure last logged per task.
type escapeChecks struct {
	mu      sync.Mutex
	running map[int64]bool
	checked map[int64]bool
	none    map[int64]string
	next    map[int64]time.Time
	failed  map[int64]string
}

func newEscapeChecks() escapeChecks {
	return escapeChecks{running: map[int64]bool{}, checked: map[int64]bool{}, none: map[int64]string{}, next: map[int64]time.Time{},
		failed: map[int64]string{}}
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
	e.startEscapeCheck(t, ref, st.Review.RunID, e.escapeCopy(t))
}

// escapeCopy is the directory task t's escape check runs git in: the task's
// copy while it holds one (copyOfTask), else the first copy of its
// repository on disk ([[factory.repo]] copies, by number). A published
// task's copy was released at the publish, and every copy of a repository
// is a worktree of its main clone, whose objects hold every commit any copy
// made, so any copy answers a diff of two of the task's heads; never the
// main clone, which may be your own checkout. "" when no copy is on disk or
// no [[factory.repo]] names the repository.
func (e *Engine) escapeCopy(t registry.Task) string {
	if c := e.copyOfTask(t); c.Path != "" && dirExists(c.Path) {
		return c.Path
	}
	repo, ok := e.cfg.FactoryRepo(t.Repo)
	if !ok || repo.Copies == "" {
		return ""
	}
	for n := 1; n <= max(repo.MaxCopies, 1); n++ {
		if p := repo.Pool().Path(n); dirExists(p) {
			return p
		}
	}
	return ""
}

// dirExists reports whether path is a directory.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// startEscapeCheck runs task t's escape check of its PR ref off the loop,
// git in dir ("": no copy), on the engine's background context, and records
// its end (escapeChecked); review is the run of magnum's last posted
// review the loop saw.
func (e *Engine) startEscapeCheck(t registry.Task, ref PRRef, review, dir string) {
	e.esc.mu.Lock()
	e.esc.running[t.ID] = true
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
		out, err := e.escapeCheck(ctx, t, ref, dir)
		ended = true
		if bg.Err() != nil {
			return // the daemon stops: the next daemon checks it
		}
		e.escapeChecked(context.WithoutCancel(ctx), t, ref, review, out, err)
	})
}

// escapeOutcome is what one escape check found: whether magnum has a
// posted round after the publish, the check and its escapes to store, how
// many escapes are unplaced, and why heads compared only when equal (""
// when the copy's git compared every file it was asked).
type escapeOutcome struct {
	found    bool
	check    registry.EscapeCheck
	escapes  []registry.Escape
	unplaced int
	sameHead string
}

// escapeCheck reads what task t's check of its PR ref compares and matches
// it (localEscapes), git in dir.
func (e *Engine) escapeCheck(ctx context.Context, t registry.Task, ref PRRef, dir string) (escapeOutcome, error) {
	link, err := e.reg.LinkedPR(ctx, t.ID)
	if err != nil {
		return escapeOutcome{}, err
	}
	round, ok, err := e.d.Magnum.FirstPostedRound(ctx, ref, link.Opened)
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
	out := escapeOutcome{found: true}
	var (
		diff *copyDiff
		same func(path, a, b string) bool
	)
	switch {
	case e.d.Git == nil:
		out.sameHead = "no git"
	case dir == "":
		out.sameHead = "no copy of the repository is on disk"
	default:
		gctx, cancel := context.WithTimeout(ctx, escapeGitBound)
		defer cancel()
		diff = &copyDiff{ctx: gctx, git: e.d.Git, dir: dir, memo: map[[3]string]bool{}}
		same = diff.same
	}
	out.check, out.escapes = localEscapes(t.ID, link, locals, cycles, round, same)
	if diff != nil && diff.failed > 0 {
		out.sameHead = "git could not compare " + textx.Count(diff.failed, "file and heads", "files and heads")
	}
	for _, es := range out.escapes {
		if es.Reason == registry.EscapeUnplaced {
			out.unplaced++
		}
	}
	return out, nil
}

// copyDiff answers escapes.SameCode for one check: whether a file reads
// the same at two heads (git diff --name-only in dir, read only), once per
// file and heads; failed counts the diffs git could not answer (a file
// they ask of compares at the same head only).
type copyDiff struct {
	ctx    context.Context
	git    *gitx.Client
	dir    string
	memo   map[[3]string]bool
	failed int
}

func (d *copyDiff) same(path, localHead, postedHead string) bool {
	key := [3]string{path, localHead, postedHead}
	if v, ok := d.memo[key]; ok {
		return v
	}
	differ, err := d.git.DifferingPaths(d.ctx, d.dir, localHead, postedHead, path)
	if err != nil {
		d.failed++
	}
	v := err == nil && len(differ) == 0
	d.memo[key] = v
	return v
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
// head comparable with the round's.
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
// (out, or the failure err): the check and its escapes stored, with an
// event that counts them; a round not found yet noted against review (the
// run of magnum's last posted review), so no check starts again before
// magnum posts another; a failure logged once while it stays the same, and
// the next check after escapeRetry.
func (e *Engine) escapeChecked(ctx context.Context, t registry.Task, ref PRRef, review string, out escapeOutcome, err error) {
	if err == nil && out.found {
		var recorded bool
		recorded, err = e.reg.RecordEscapeCheck(ctx, out.check, out.escapes)
		if err == nil {
			e.esc.mu.Lock()
			e.esc.checked[t.ID] = true
			delete(e.esc.none, t.ID)
			delete(e.esc.next, t.ID)
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
		e.log.Warn("the escape check of a published task's local review failed; it is tried again", "task", t.ID, "err", msg)
	}
}

// words is the escapes event of a check of PR ref: counts only, never a
// title, a path or the PR's text.
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
	if o.sameHead != "" {
		s += " (" + o.sameHead + ": findings compared only at the same head)"
	}
	return s
}
