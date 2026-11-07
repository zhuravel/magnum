package factory

// A published task's row on home says what its PR waits on, from magnum's
// registry (Magnum.PRStates, read once per frame): a merge is done, a close
// waits for a reopen and then needs you, and an open PR shows the autofix =
// "open" verdict magnum keeps for it. A verdict that no longer tells the
// PR's state, a PR magnum has not read and a registry that cannot be read
// are magnum looking at the PR, never idle. Without a verdict (the watch
// does not run autofix = "open") the fix loop, the attention and magnum's
// review still show, and so does a stalemate. A fix attention whose loop left
// you items says the fixer's own items from its report (fixerWait). The row's WAITED counts from when the PR began to wait
// as it does (the verdict's Since), so a PR that came to need you while you
// were away is new for you. Without a Magnum the row keeps its words:
// magnum reviews the PR.

import (
	"cmp"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

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
		w := s.prWaitOf(v, st, ok && err == nil, err)
		v.pr = &w
		v.group = w.group
		if w.since.After(v.since) {
			v.since = w.since
		}
	}
	return nil
}

// doneAt is when done task t's PR merged, as magnum's registry tells it
// (readPRs), else when the tick saw the merge.
func (s *screen) doneAt(t registry.Task) time.Time {
	if at, ok := s.merged[t.ID]; ok {
		return at
	}
	return t.Updated
}

// prWaitOf is the wait of published task v whose PR magnum's registry
// tells as st (read false: it could not be read, err says why), with since
// when the PR has waited so where magnum's registry tells it: a verdict's
// Since (its At in a row of an older build), a loop's start, the close and
// the grace's end, the attention's row change (magnum's row changed then
// last).
func (s *screen) prWaitOf(v *taskView, st PRState, read bool, err error) prWait {
	t := v.t
	ref := prRef(v)
	n := ref.Number
	looking := fmt.Sprintf("looking at #%d", n)
	closedYou := func() prWait {
		w := youWait(fmt.Sprintf("PR #%d closed without a merge", n), "", fmt.Sprintf("PR #%d closed without a merge: %s", n, reopenMove))
		w.move, w.moveShort = reopenMove, "reopen or x"
		return w
	}
	switch {
	case !read && waitsOnYou(t): // the tick's own wait on you (a close past the grace) stands
		return youWait(t.WaitsWhat, clauseShort(t.WaitsWhat), "")
	case !read:
		why := "magnum's registry cannot be read"
		if err != nil {
			why += ": " + textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 200)
		}
		return waitsWait("magnum", looking, "", looking+": "+why)
	case !st.Found && !st.Watched: // nothing tells the task when you did
		what := fmt.Sprintf("magnum does not watch %s: merge or close #%d yourself, then x drops the task", ref.Repo, n)
		w := youWait(what, clauseShort(what), "")
		w.move, w.moveShort = fmt.Sprintf("merge or close #%d, then x drops the task", n), "then x"
		return w
	case st.GHState == store.GHMerged:
		return prWait{group: tui.GroupDone, mark: tui.MarkDone, on: tui.WaitsOn{Who: "nothing", What: "merged " + clock(cmp.Or(store.Deref(st.MergedAt), t.Updated))}}
	case st.GHState == store.GHClosed:
		closed := cmp.Or(store.Deref(st.ClosedAt), t.Updated)
		until := closed.Add(s.cfg.Daemon.CloseGrace.Duration)
		if waitsOnYou(t) { // the tick's span says since when
			return closedYou()
		}
		if !s.now.Before(until) { // the tick has not marked it yet
			w := closedYou()
			w.since = until
			return w
		}
		what := fmt.Sprintf("#%d closed %s; the task waits until %s for a reopen", n, clock(closed), clock(until))
		w := waitsWait("magnum", what, clauseShort(what), "")
		w.since = closed
		return w
	}
	return magnumWait(st, n, false)
}

// magnumWait is the wait of open PR #n as magnum's registry tells it (st):
// its daemon down, the PR not read yet, its fix loop (the loop's row, which
// a start writes at once, before the look's next verdict says fixing), its
// verdict (verdictWait; a stale one is magnum looking), a verdict still to
// come, its attention or stalemate (attentionWait), magnum's review of the
// head, else magnum's review to come. own says the row is your PR's, whose
// F works on the factory screen too (offWait).
func magnumWait(st PRState, n int, own bool) prWait {
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
	case st.HasOpen && st.Stale:
		return waitsWait("magnum", looking, "", "")
	case st.HasOpen:
		w := verdictWait(st, n, own)
		w.since = cmp.Or(st.Open.Since, st.Open.At)
		return w
	case st.OpenWatch: // its verdict is still to come
		return waitsWait("magnum", looking, "", "")
	case st.State == store.PRNeedsAttention || st.Stalemate:
		return attentionWait(st, n)
	case st.ReviewedSHA != "" && st.ReviewedSHA == st.Head && st.State == store.PRReviewed:
		return waitsWait("magnum", "reviewed "+textx.ShortSHA(st.Head), "", "")
	}
	return waitsWait("magnum", fmt.Sprintf("review of #%d", n), "", "")
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
// own as for magnumWait.
func verdictWait(st PRState, n int, own bool) prWait {
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
		return waits("reviewers", o.Why, short, "")
	case engine.OpenOff:
		what := offWait(o.Off, n, own)
		return you(what, clauseShort(what), "")
	case engine.OpenWaits:
		who := "magnum"
		switch {
		case strings.HasPrefix(o.Why, "CI "):
			who = "CI"
		case strings.HasPrefix(o.Why, "settling"):
			who = "reviewers"
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
