package factory

// A published task's row on home says what its PR waits on, from magnum's
// registry (Magnum.PRStates, read once per frame): a merge is done, a close
// waits for a reopen and then needs you, and an open PR shows the autofix =
// "open" verdict magnum keeps for it, never idle unless that says so. A
// verdict the look owes (one gone stale, or one still to come) is magnum
// looking at the PR only while that is true (lookOwed: four of magnum's
// polls, with its daemon ticking); past that, and without a verdict (the
// watch does not run autofix = "open"), the row says what magnum's row says
// (rowWait): its fix loop, its attention or stalemate, magnum's review to
// come, else magnum's outcome at the head and whom it waits on next, never
// a bare "reviewed". A PR magnum has not read is magnum looking at it until
// it should have found it (pickupFor), then it needs you; a registry that
// cannot be read is magnum looking. A fix attention whose loop left you
// items says the fixer's own items from its report (fixerWait). The row's
// WAITED counts from when the PR began to wait as it does (the verdict's
// Since), so a PR that came to need you while you were away is new for
// you. Without a Magnum the row keeps its words: magnum reviews the PR.

import (
	"cmp"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// prWait is what a published task waits on, by its PR: its group and mark
// on home, its blocker, the links of what needs you, your move, and since
// when the PR has waited so (zero: not known, the task's last span tells).
// A fix attention adds the fixer's report (report, which e opens) and,
// when the fixer left you items, those items (needs), the help line that
// lists them (help) and the card's WAITING FOR (card); "" keeps the
// blocker's words there (fixerWait).
type prWait struct {
	group           tui.FactoryGroup
	mark            tui.FactoryMark
	on              tui.WaitsOn
	links           []string
	move, moveShort string
	since           time.Time
	report          string
	needs           []string
	help, card      string
	// cells are the PR's CI, FINDINGS and APPR cells (prCells).
	cells prCells
}

// prCells are an open PR's cells on its row: its CI, magnum's findings and
// the people's reviews, as magnum's registry tells them.
type prCells struct {
	ci        *tui.CIInfo
	findings  *tui.FindingsInfo
	approvals *tui.FactoryApprovals
}

// cellsOf are open PR st's cells; none for a PR magnum does not have or
// that is no longer open.
func cellsOf(st PRState) prCells {
	if !st.Found || st.GHState != store.GHOpen {
		return prCells{}
	}
	return prCells{ci: st.CI, findings: st.Findings, approvals: approvalsOf(st)}
}

// approvalsOf counts st's people's reviews as GitHub's merge gate does
// (its latest opinion per reviewer with write access); nil before magnum
// read the gate.
func approvalsOf(st PRState) *tui.FactoryApprovals {
	if st.Gate == nil {
		return nil
	}
	a := &tui.FactoryApprovals{Decision: st.Gate.Decision}
	for _, o := range st.Gate.Opinions {
		switch o.State {
		case "APPROVED":
			a.Approved++
			if o.CommitSHA != st.Head {
				a.Stale++
			}
		case "CHANGES_REQUESTED":
			a.Changes++
		}
	}
	return a
}

// fill sets row's PR cells.
func (c prCells) fill(row *tui.FactoryRow) {
	row.CI, row.Findings, row.Approvals = c.ci, c.findings, c.approvals
}

// prRef is the PR of task view v as magnum's registry names it.
func prRef(v *taskView) PRRef {
	return PRRef{Repo: cmp.Or(v.link.Repo, v.t.Repo), Number: cmp.Or(v.link.Number, v.t.PRNumber)}
}

// readPRs reads, once, from magnum's registry the PRs of the published
// views and of the tasks done today or since lastSeen (the digest's, when
// there is one). It sets each published view's wait and group (prWaitOf),
// and since when it waits so: the later of the task's last span (its
// publish, a reopen) and when its PR began to wait as it does. A done
// task's PR says when it merged (merged): the tick made the task done
// when it saw the merge, maybe days later (the first tick after an
// install), so home drops a done task whose PR merged before today. Without
// a Magnum, or with no such task, it reads nothing; a read that fails
// leaves the done tasks at the time the tick saw the merge.
func (s *screen) readPRs(m Magnum, lastSeen time.Time) error {
	if m == nil {
		return nil
	}
	var refs []PRRef
	views := map[int64]*taskView{}
	for _, v := range s.views {
		views[v.t.ID] = v
		if v.t.State == registry.TaskPublished && prRef(v).Number > 0 {
			refs = append(refs, prRef(v))
		}
	}
	done := map[int64]PRRef{}
	for _, t := range s.tasks {
		if t.State != registry.TaskDone || !sameDay(t.Updated, s.now) && !(s.away(lastSeen) && t.Updated.After(lastSeen)) {
			continue
		}
		v, ok := views[t.ID]
		if !ok {
			l, err := s.reg.LinkedPR(s.ctx, t.ID)
			if err != nil && !errors.Is(err, registry.ErrNotFound) {
				return err
			}
			v = &taskView{t: t, link: l}
		}
		if ref := prRef(v); ref.Number > 0 {
			done[t.ID] = ref
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	states, err := m.PRStates(s.ctx, refs)
	if err == nil {
		s.merged = map[int64]time.Time{}
		for id, ref := range done {
			if st := states[ref]; st.GHState == store.GHMerged && st.MergedAt != nil {
				s.merged[id] = *st.MergedAt
			}
		}
		s.views = slices.DeleteFunc(s.views, func(v *taskView) bool {
			at, ok := s.merged[v.t.ID]
			return ok && !sameDay(at, s.now)
		})
	}
	for _, v := range s.views {
		if v.t.State != registry.TaskPublished || prRef(v).Number <= 0 {
			continue
		}
		st, ok := states[prRef(v)]
		s.noteBuild(st)
		w := s.prWaitOf(v, st, ok && err == nil, err)
		if ok && err == nil {
			w.cells = cellsOf(st)
		}
		v.pr = &w
		v.group = w.group
		if w.since.After(v.since) {
			v.since = w.since
		}
	}
	return nil
}

// noteBuild keeps the old build magnum's daemon runs, as PR st's read
// says it (PRState.OldBuild), for home's title line (oldBuildLines).
func (s *screen) noteBuild(st PRState) {
	if st.OldBuild != nil && s.oldBuild == nil {
		s.oldBuild = st.OldBuild
	}
}

// oldBuildLines are home's blocker lines, short and long, for magnum's
// daemon on an older build than the one on disk: its look may write no
// verdict the screen reads until it restarts. None when no PR's read says
// so.
func (s *screen) oldBuildLines() (short, long string) {
	b := s.oldBuild
	if b == nil {
		return "", ""
	}
	long = "magnum's daemon runs " + b.Label()
	if !b.StartedAt.IsZero() {
		long += " since " + when(b.StartedAt, s.now)
	}
	return "magnum's daemon runs an old build", long + ": verdicts wait for `magnum daemon-restart --drain`"
}

// doneAt is when done task t's PR merged, as magnum's registry tells it
// (readPRs), else when the tick saw the merge.
func (s *screen) doneAt(t registry.Task) time.Time {
	if at, ok := s.merged[t.ID]; ok {
		return at
	}
	return t.Updated
}

// The words of who a published task's PR waits on besides you, magnum and
// its fixer at work (waitMagnum, waitAgent): the PR's reviewers, its CI,
// and nothing once it merged. The PR's status file maps them to its own
// words (whoOf).
const (
	waitReviewers = "reviewers"
	waitCI        = "CI"
	waitNothing   = "nothing"
)

// prWaitOf is the wait of published task v whose PR magnum's registry
// tells as st (read false: it could not be read, err says why), with since
// when the PR has waited so where magnum's registry tells it: a verdict's
// Since (its At in a row of an older build), a loop's start, the close and
// the grace's end, the attention's row change (magnum's row changed then
// last).
func (s *screen) prWaitOf(v *taskView, st PRState, read bool, err error) prWait {
	t := v.t
	looking := fmt.Sprintf("looking at #%d", prRef(v).Number)
	switch {
	case !read && waitsOnYou(t): // the tick's own wait on you (a close past the grace) stands
		return youWait(t.WaitsWhat, clauseShort(t.WaitsWhat), "")
	case !read:
		why := "magnum's registry cannot be read"
		if err != nil {
			why += ": " + textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 200)
		}
		return waitsWait(waitMagnum, looking, "", looking+": "+why)
	case !st.Found && st.Watched && st.DaemonUp && !v.since.IsZero() && s.now.Sub(v.since) > pickupFor(s.clock()):
		// published long enough ago that magnum's poll should have found it
		ref := prRef(v)
		what := fmt.Sprintf("magnum has not picked #%d up since %s", ref.Number, when(v.since, s.now))
		w := youWait(fmt.Sprintf("%s: `magnum review %s#%d` asks for it", what, ref.Repo, ref.Number), what, "")
		w.move, w.moveShort = fmt.Sprintf("magnum review %s#%d", ref.Repo, ref.Number), "magnum review"
		return w
	}
	return publishedWait(t, prRef(v), st, s.clock())
}

// prClock is when a PR's wait is read and the timings of magnum's daemon it
// is read by: [daemon] close_grace, and poll_interval (30s unset).
type prClock struct {
	now         time.Time
	grace, poll time.Duration
}

// clockOf is cfg's prClock at now.
func clockOf(cfg *config.Config, now time.Time) prClock {
	c := prClock{now: now, poll: 30 * time.Second}
	if cfg != nil {
		c.grace, c.poll = cfg.Daemon.CloseGrace.Duration, cmp.Or(cfg.Daemon.PollInterval.Duration, c.poll)
	}
	return c
}

// clock is the screen's prClock.
func (s *screen) clock() prClock { return clockOf(s.cfg, s.now) }

// publishedWait is the wait of published task t whose PR ref magnum's
// registry tells as st, at c: a repository no watch covers is yours, a
// merge waits on nothing, a close waits c.grace for a reopen and then on
// you, and an open PR waits as magnum tells it (magnumWait). The screen's
// row (prWaitOf) and the PR's status file (nowOf) both say it.
func publishedWait(t registry.Task, ref PRRef, st PRState, c prClock) prWait {
	n := ref.Number
	closedYou := func() prWait {
		w := youWait(fmt.Sprintf("PR #%d closed without a merge", n), "", fmt.Sprintf("PR #%d closed without a merge: %s", n, reopenMove))
		w.move, w.moveShort = reopenMove, "reopen or x"
		return w
	}
	switch {
	case !st.Found && !st.Watched: // nothing tells the task when you did
		what := fmt.Sprintf("magnum does not watch %s: merge or close #%d yourself, then x drops the task", ref.Repo, n)
		w := youWait(what, clauseShort(what), "")
		w.move, w.moveShort = fmt.Sprintf("merge or close #%d, then x drops the task", n), "then x"
		return w
	case st.GHState == store.GHMerged:
		return prWait{group: tui.GroupDone, mark: tui.MarkDone, on: tui.WaitsOn{Who: waitNothing, What: "merged " + clock(cmp.Or(store.Deref(st.MergedAt), t.Updated))}}
	case st.GHState == store.GHClosed:
		closed := cmp.Or(store.Deref(st.ClosedAt), t.Updated)
		until := closed.Add(c.grace)
		if waitsOnYou(t) { // the tick's span says since when
			return closedYou()
		}
		if !c.now.Before(until) { // the tick has not marked it yet
			w := closedYou()
			w.since = until
			return w
		}
		what := fmt.Sprintf("#%d closed %s; the task waits until %s for a reopen", n, clock(closed), clock(until))
		w := waitsWait(waitMagnum, what, clauseShort(what), "")
		w.since = closed
		return w
	}
	return magnumWait(st, n, false, c)
}

// magnumWait is the wait of open PR #n as magnum's registry tells it (st):
// its daemon down, the PR not read yet, its fix loop (the loop's row, which
// a start writes at once, before the look's next verdict says fixing), its
// fresh verdict (verdictWait), its attention or stalemate (attentionWait),
// a verdict the look owes (a stale one, or one still to come: magnum
// looking at the PR, while that is true, lookOwed; past it what magnum's
// row says, with the look named), else what magnum's row says (rowWait).
// own says the row is your PR's, whose F and f work on the factory screen
// too (offWait).
func magnumWait(st PRState, n int, own bool, c prClock) prWait {
	looking := fmt.Sprintf("looking at #%d", n)
	switch {
	case !st.DaemonUp:
		return waitsWait("magnum", "daemon down", "", fmt.Sprintf("magnum's daemon is down: nothing looks at #%d", n))
	case !st.Found:
		return waitsWait("magnum", looking, "", "")
	case st.Fixing:
		w := fixingWait(st, n)
		w.since = st.Fix.StartedAt
		return w
	case st.State == store.PRNeedsAttention && (!st.HasOpen || st.Stale):
		// magnum's own row says it needs you: a verdict to come or one gone
		// stale never hides it as "looking at".
		return attentionWait(st, n)
	case st.HasOpen && !st.Stale:
		w := verdictWait(st, n, own, c)
		w.since = cmp.Or(st.Open.Since, st.Open.At)
		return w
	case st.State == store.PRNeedsAttention || st.Stalemate:
		return attentionWait(st, n)
	case st.HasOpen || st.OpenWatch: // the look owes a verdict: a stale one, or one still to come
		owed, why := lookOwed(st, c)
		if why == "" {
			w := waitsWait(waitMagnum, looking, "", "")
			w.since = owed
			return w
		}
		w := rowWait(st, n, own, c)
		note := fmt.Sprintf("magnum's look has not decided #%d", n)
		if !owed.IsZero() {
			note += " since " + when(owed, c.now)
		}
		if why != lookLate {
			note += ": " + why
		}
		w.on.Long = cmp.Or(w.on.Long, w.on.What) + " · " + note
		if w.help != "" {
			w.help += " · " + note
		}
		return w
	}
	return rowWait(st, n, own, c)
}

// lookLate is lookOwed's why for a look past its polls with magnum's
// daemon ticking.
const lookLate = "late"

// lookOwed is since when magnum's look owes PR st a verdict (the later of
// its stale verdict's decision and the row's last change, the row's last
// change for one still to come), and why "looking at" is no longer true:
// "" while it is (four of magnum's polls since then, OpenVerdict.Stale's
// count, with magnum's daemon ticking), magnum's daemon on an older build
// than the one on disk (its look may predate the verdict it owes), one that
// stopped ticking, else lookLate.
func lookOwed(st PRState, c prClock) (time.Time, string) {
	owed := st.UpdatedAt
	if st.HasOpen {
		owed = later(owed, st.Open.At)
	}
	look := lookPolls * c.poll
	switch {
	case c.now.Sub(owed) > look && st.OldBuild != nil:
		return owed, "magnum's daemon runs an old build (`magnum daemon-restart --drain`)"
	case !st.LastTick.IsZero() && c.now.Sub(st.LastTick) > look:
		return owed, "magnum's daemon has not ticked since " + when(st.LastTick, c.now)
	case c.now.Sub(owed) > look:
		return owed, lookLate
	}
	return owed, ""
}

// lookPolls is how many of magnum's polls its look may owe a verdict
// before "looking at" is no longer true: OpenVerdict.Stale's count.
const lookPolls = 4

// pickupMin is the least a published PR may wait for magnum's poll to find
// it before it needs you (pickupFor).
const pickupMin = 10 * time.Minute

// pickupFor is how long a published PR may wait for magnum's poll to find
// it: lookPolls of its polls, at least pickupMin.
func pickupFor(c prClock) time.Duration { return max(lookPolls*c.poll, pickupMin) }

// when is t as a wait at now says it: its clock today, with its day
// before.
func when(t, now time.Time) string {
	if sameDay(t, now) {
		return clock(t)
	}
	return t.Local().Format("Jan 2 15:04")
}

// rowWait is what open PR #n waits on by magnum's row alone (st), with no
// verdict that tells it: magnum's round under way, magnum's review of the
// head to come, else magnum's outcome at the head (outcomeOf), never a bare
// "reviewed", with what comes next and whom it waits on. Problems no loop
// of magnum's takes (autofix is "never", or fix is off) wait on you, which
// f fixes on your PR; ones magnum's fixer takes (autofix "own" or "open")
// wait on magnum; a head clean on magnum's side waits as nextWait says, on
// the reviewers, CI or you, never on magnum. own as for magnumWait.
func rowWait(st PRState, n int, own bool, c prClock) prWait {
	short := textx.ShortSHA(st.Head)
	switch {
	case st.State == store.PRBaseline || st.State == store.PRIneligible:
		why := engine.OpenOffIneligible
		if st.State == store.PRBaseline {
			why = engine.OpenOffBaseline
		}
		what := offWait(why, n, own)
		return youWait(what, clauseShort(what), "")
	case slices.Contains(store.InFlightStates, st.State):
		w := waitsWait(waitMagnum, "magnum reviews "+short+" now", "", "")
		w.since = st.HeadChangedAt
		return w
	case st.ReviewedSHA == "" || st.ReviewedSHA != st.Head:
		return waitsWait(waitMagnum, fmt.Sprintf("review of #%d", n), "", "")
	}
	what, count, problems := outcomeOf(st)
	since := st.HeadChangedAt
	if st.Review != nil && st.Review.SHA == st.Head && st.Review.At.After(since) {
		since = st.Review.At
	}
	var w prWait
	switch {
	case problems && (st.Autofix == config.AutofixOwn || st.Autofix == config.AutofixOpen) && !st.FixOff:
		w = waitsWait(waitMagnum, what+" · magnum's fixer takes them", count, "")
	case problems && own:
		w = youWait(what+" · f fixes them", count, "")
		w.move, w.moveShort = "f fixes them", "f"
	case problems:
		w = youWait(what+" · f on the board fixes them", count, "")
		w.move, w.moveShort = "fix them, or f on the board", "fix them"
	default:
		w = nextWait(st, n, what, true, c)
		if w.since.After(since) {
			since = w.since
		}
	}
	w.since = since
	return w
}

// outcomeOf is magnum's review of st's head in words: the findings it
// keeps open there by priority, new and earlier ones ("4 findings open at
// a1b2c3d (P1 P2×3)"), only optional ones ("no problems at a1b2c3d, 2
// optional (P3×2)"), the earlier ones all fixed ("all 3 findings fixed at
// a1b2c3d"), magnum's approval or none ("magnum approved a1b2c3d", "no
// findings at a1b2c3d"); a review whose result magnum did not keep says
// what it posted. count is the short form of open findings ("4 findings
// open", "" for none), and problems says a P0 to P2 is open (or, without
// its result, that magnum asked for changes).
func outcomeOf(st PRState) (what, count string, problems bool) {
	short := textx.ShortSHA(st.Head)
	r := st.Review
	if r == nil || r.SHA != st.Head {
		switch st.ReviewEvent {
		case "APPROVED":
			return "magnum approved " + short, "", false
		case "CHANGES_REQUESTED":
			return "magnum asked for changes at " + short, "changes asked", true
		case "COMMENTED":
			return "magnum commented at " + short, "", false
		}
		return "magnum's review of " + short + " (its findings not kept)", "", false
	}
	all, unknown := r.Counts, 0
	if r.OpenCounts != nil {
		for i, n := range r.OpenCounts {
			all[i] += n
		}
	} else {
		unknown = r.Open // earlier findings still open, of priorities the result did not give
	}
	var list []string
	total := unknown
	for i, n := range all {
		total += n
		switch {
		case n == 1:
			list = append(list, fmt.Sprintf("P%d", i))
		case n > 1:
			list = append(list, fmt.Sprintf("P%d×%d", i, n))
		}
	}
	if unknown > 0 {
		list = append(list, fmt.Sprintf("%d earlier", unknown))
	}
	switch {
	case all[0]+all[1]+all[2]+unknown > 0:
		count = plural(total, "finding") + " open"
		return fmt.Sprintf("%s at %s (%s)", count, short, strings.Join(list, " ")), count, true
	case all[3] > 0:
		return fmt.Sprintf("no problems at %s, %d optional (%s)", short, all[3], strings.Join(list, " ")), "", false
	case r.Fixed == 1:
		return "1 finding fixed at " + short, "", false
	case r.Fixed > 1:
		return fmt.Sprintf("all %d findings fixed at %s", r.Fixed, short), "", false
	case st.ReviewEvent == "APPROVED" || r.Event == "APPROVE" || r.Event == "APPROVED":
		return "magnum approved " + short, "", false
	}
	return "no findings at " + short, "", false
}

// nextWait is what a PR clean on magnum's side (clean says so: "nothing
// open at a1b2c3d", "magnum approved a1b2c3d") waits on next: with ci, its
// CI first (failed: you; running, or a required check that has not run:
// CI); then GitHub's review gate: approved or no review required, your
// merge; changes asked, your answer; the reviewers asked, with how long
// ago; nobody asked, you to ask one. A gate magnum has not read waits on
// the reviewers. since is the newest ask.
func nextWait(st PRState, n int, clean string, ci bool, c prClock) prWait {
	with := func(next string) string { return clean + " · " + next }
	if ci && st.CI != nil {
		switch state, name, progress := ciOf(st.CI); state {
		case "failed":
			next := "CI failed"
			if name != "" {
				next += ": " + name
			}
			w := youWait(with(next), next, "")
			w.move, w.moveShort = "fix the CI", "fix CI"
			return w
		case "pending":
			next := "CI runs"
			if p := cmp.Or(progress, name); p != "" {
				next += ": " + p
			}
			return waitsWait(waitCI, with(next), next, "")
		case "stale":
			next := "CI of " + textx.ShortSHA(st.Head) + " not read yet"
			return waitsWait(waitCI, with(next), next, "")
		case "missing":
			next := name + " has not run at " + textx.ShortSHA(st.Head)
			return waitsWait(waitCI, with(next), name+" has not run", "")
		}
	}
	merge := func(next string) prWait {
		w := youWait(with(next), "approved, merge is yours", "")
		w.move, w.moveShort = fmt.Sprintf("merge #%d on GitHub", n), "merge it"
		return w
	}
	g := st.Gate
	switch {
	case g == nil:
		return waitsWait(waitReviewers, with("no approval yet"), "no approval yet", "")
	case g.Decision == "APPROVED":
		if by := opinionsOf(g, "APPROVED"); by != "" {
			return merge("approved by " + by + ", merge is yours")
		}
		return merge("approved, merge is yours")
	case g.Decision == "CHANGES_REQUESTED":
		by := cmp.Or(opinionsOf(g, "CHANGES_REQUESTED"), "a reviewer")
		w := youWait(with(by+" asked for changes"), by+" asked for changes", "")
		w.move, w.moveShort = "answer "+by, "answer them"
		return w
	case g.Decision == "":
		w := merge("no review required, merge is yours")
		w.on.Short = "merge is yours"
		return w
	case len(st.Asked) > 0:
		names := make([]string, len(st.Asked))
		var since time.Time
		for i, a := range st.Asked {
			names[i] = a.Who
			if !a.At.IsZero() {
				names[i] += " " + shortAge(max(c.now.Sub(a.At), 0)) + " ago"
				since = later(since, a.At)
			}
		}
		next := "asked " + andList(names)
		w := waitsWait(waitReviewers, with(next), next, "")
		w.since = since
		return w
	}
	w := youWait(with("no reviewer asked"), "no reviewer asked", "")
	w.move, w.moveShort = "ask a reviewer", "ask one"
	return w
}

// ciOf is CI c's state as a wait reads it: "stale" (the checks are an
// older commit's: the head's are not read yet), "failed" (with the failing
// required check's name, else the first failing check's), "pending" (with
// the running required check's name, else the progress, "25/41"),
// "missing" (a required check that has not run at the head, by name), else
// "passed" or "none".
func ciOf(c *tui.CIInfo) (state, name, progress string) {
	if c.Stale {
		return "stale", "", ""
	}
	for _, r := range c.Required {
		if r.State == store.CheckFailed {
			return "failed", cmp.Or(r.Label, r.Name), ""
		}
	}
	if c.State == "failed" {
		if len(c.Failing) > 0 {
			name = c.Failing[0]
		}
		return "failed", name, ""
	}
	for _, r := range c.Required {
		if r.State == store.CheckPending {
			return "pending", cmp.Or(r.Label, r.Name), ""
		}
	}
	if c.State == "pending" {
		if c.Total > 0 {
			progress = fmt.Sprintf("%d/%d", c.Total-c.Pending, c.Total)
		}
		return "pending", "", progress
	}
	for _, r := range c.Required {
		if r.State == "missing" {
			return "missing", cmp.Or(r.Label, r.Name), ""
		}
	}
	return c.State, "", ""
}

// opinionsOf are the reviewers whose latest opinion in gate g is state, in
// words ("rev-ann and rev-bob"); "" for none.
func opinionsOf(g *store.ReviewGate, state string) string {
	var who []string
	for _, o := range g.Opinions {
		if o.State == state && o.Login != "" {
			who = append(who, o.Login)
		}
	}
	return andList(who)
}

// andList joins items as a sentence does: "a", "a and b", "a, b and c".
func andList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// later is the later of a and b.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// attentionWait is the wait on you of PR #n that magnum's row (st) says
// needs you: its attention, in magnum's words, else its stalemate (magnum
// stopped arguing in a thread), since the row last changed.
func attentionWait(st PRState, n int) prWait {
	var w prWait
	if st.State == store.PRNeedsAttention {
		msg := cmp.Or(st.AttentionMsg, st.Attention, "needs attention")
		w = fixerWait(st, youWait("magnum: "+msg, "magnum: "+firstClause(msg), ""))
	} else {
		what := fmt.Sprintf("magnum stopped arguing on #%d: the thread is yours", n)
		w = youWait(what, clauseShort(what), "")
	}
	w.since = st.UpdatedAt
	return w
}

// The runes of the fixer's first item a row draws, by its form (100, 140
// and 200 columns): the wide form names the fixer before it.
const (
	fixerItemShort = 32
	fixerItemWhat  = 64
	fixerItemLong  = 120
)

// fixerItemRe is the comment id that starts an entry of the fixer's Needs
// you ("2101 P1 app/a.rb:42: …"), which a row and a card leave out.
var fixerItemRe = regexp.MustCompile(`^\d{1,20}\s+`)

// fixerWait is w, the wait on you of PR st's attention or of its verdict
// that needs you, with what the fix job's attention keeps of the fixer
// (PRState.FixReport, FixNeeds): its report, which e opens, and, when the
// fixer left you items, the first one on the row in its own words without
// its comment id (cut to fit the row's form; the wide form names the fixer
// before it, and " · 1 more" counts the rest), every one on the help line
// and the card's list, and on the card's WAITING FOR who left how many and
// that e opens the report. Any other attention, and a fix attention without
// a report, keep w; its links, move and since stay.
func fixerWait(st PRState, w prWait) prWait {
	if st.State != store.PRNeedsAttention || st.FixReport == "" {
		return w
	}
	w.report = st.FixReport
	if len(st.FixNeeds) == 0 {
		return w
	}
	fixer := strings.TrimSuffix(filepath.Base(st.FixReport), ".md")
	items := make([]string, len(st.FixNeeds))
	for i, e := range st.FixNeeds {
		items[i] = cmp.Or(fixerItemRe.ReplaceAllString(e, ""), e)
	}
	more := ""
	if n := len(items) - 1; n > 0 {
		more = fmt.Sprintf(" · %d more", n)
	}
	w.on = tui.WaitsOn{Who: waitsYou, OnYou: true, Short: textx.Clip(items[0], fixerItemShort),
		What: textx.Clip(items[0], fixerItemWhat) + more, Long: fixer + ": " + textx.Clip(items[0], fixerItemLong) + more}
	left := fmt.Sprintf("%s: %s left you %s", waitsYou, fixer, plural(len(items), "item"))
	w.needs, w.help, w.card = items, left+": "+strings.Join(items, " · "), left+": e opens its report"
	return w
}

// waitsWait is a wait on others: who, and what in its three forms.
func waitsWait(who, what, short, long string) prWait {
	return prWait{group: tui.GroupWaits, mark: tui.MarkWaits, on: tui.WaitsOn{Who: who, What: what, Short: short, Long: long}}
}

// youWait is a wait on you: what in its three forms.
func youWait(what, short, long string) prWait {
	return prWait{group: tui.GroupNeedsYou, mark: tui.MarkNeedsYou,
		on: tui.WaitsOn{Who: waitsYou, What: what, Short: short, Long: long, OnYou: true}}
}

// fixingWait is magnum's fix loop at work on PR #n (st.Fix): an agent at
// work, the moving mark.
func fixingWait(st PRState, n int) prWait {
	what := fmt.Sprintf("magnum fixes #%d", n)
	switch {
	case st.Fix.Phase == engine.FixPhaseReviewing:
		what += " (re-review of " + textx.ShortSHA(st.Fix.Head) + ")"
	case st.Fix.Cycle > 0:
		what += fmt.Sprintf(" (cycle %d of %d)", st.Fix.Cycle, st.Fix.Cap)
	}
	w := waitsWait("agent", what, clauseShort(what), "")
	w.mark = tui.MarkWorking
	return w
}

// reopenMove is your move on a task whose PR closed without a merge.
const reopenMove = "reopen it, or x drops the task"

// verdictWait is the wait of a PR whose verdict st.Open tells its state;
// own as for magnumWait. An idle one that no person approved waits as
// nextWait says, once magnum read GitHub's review gate: on the reviewers
// asked, else on you to ask one.
func verdictWait(st PRState, n int, own bool, c prClock) prWait {
	waits, you := waitsWait, youWait
	o := st.Open
	short := clauseShort(o.Why)
	switch o.Verdict {
	case engine.OpenFixing:
		return fixingWait(st, n)
	case engine.OpenNeedsYou:
		w := you(o.Why, short, "")
		for _, need := range o.Needs {
			if need.URL != "" {
				w.links = append(w.links, need.URL)
			}
		}
		return fixerWait(st, w)
	case engine.OpenIdle:
		if st.Approved {
			w := you(o.Why, short, "")
			w.move, w.moveShort = fmt.Sprintf("merge #%d on GitHub", n), "merge it"
			return w
		}
		if st.Gate == nil {
			return waits(waitReviewers, o.Why, short, "")
		}
		clean, _, _ := strings.Cut(o.Why, " · ")
		return nextWait(st, n, clean, false, c)
	case engine.OpenOff:
		what := offWait(o.Off, n, own)
		return you(what, clauseShort(what), "")
	case engine.OpenWaits:
		who := "magnum"
		switch {
		case strings.HasPrefix(o.Why, "CI "):
			who = waitCI
		case strings.HasPrefix(o.Why, "settling"):
			who = waitReviewers
		}
		return waits(who, o.Why, short, "")
	}
	looking := fmt.Sprintf("looking at #%d", n) // unknown, or a verdict this build does not know
	return waits("magnum", looking, "", looking+": "+o.Why)
}

// offWait is why magnum's look leaves PR #n alone (OpenVerdict.Off), as
// your move on it; own says it is your PR's row, whose own F turns fix on.
func offWait(off string, n int, own bool) string {
	switch off {
	case engine.OpenOffFixOff:
		if own {
			return fmt.Sprintf("fix is off for #%d (F turns it on)", n)
		}
		return fmt.Sprintf("fix is off for #%d (F on the board turns it on)", n)
	case engine.OpenOffDraft:
		return fmt.Sprintf("#%d is a draft: mark it ready", n)
	case engine.OpenOffFork:
		return fmt.Sprintf("#%d's head lives in a fork: magnum fixes nothing there, so its feedback is yours", n)
	case engine.OpenOffIncludeOwn:
		return fmt.Sprintf("#%d is yours to follow: its watch leaves your own PRs out (include_own)", n)
	case engine.OpenOffMuted:
		return fmt.Sprintf("#%d is muted: U on the board unmutes it", n)
	case engine.OpenOffBaseline:
		return fmt.Sprintf("#%d is yours to follow: it was open before magnum first synced the repository", n)
	}
	return fmt.Sprintf("#%d is yours to follow: magnum does not review it", n)
}

// firstClause is s up to its first clause's end (": ", "; ", " · " or "
// ("), s when it has one clause.
func firstClause(s string) string {
	end := len(s)
	for _, sep := range []string{": ", "; ", " · ", " ("} {
		if i := strings.Index(s, sep); i > 0 && i < end {
			end = i
		}
	}
	return s[:end]
}

// clauseShort is s's first clause as a blocker's Short: "" when s has only
// one, so the blocker keeps its What.
func clauseShort(s string) string {
	if c := firstClause(s); c != s {
		return c
	}
	return ""
}
