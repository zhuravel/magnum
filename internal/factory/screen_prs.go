package factory

// A published task's row on home says what its PR waits on, from magnum's
// registry (Magnum.PRStates, read once per frame): a merge is done, a close
// waits for a reopen and then needs you, and an open PR shows the autofix =
// "open" verdict magnum keeps for it. A verdict that no longer tells the
// PR's state, a PR magnum has not read and a registry that cannot be read
// are magnum looking at the PR, never idle. Without a verdict (the watch
// does not run autofix = "open") the fix loop, the attention and magnum's
// review still show. The row's WAITED counts from when the PR began to wait
// as it does (the verdict's Since), so a PR that came to need you while you
// were away is new for you. Without a Magnum the row keeps its words:
// magnum reviews the PR.

import (
	"cmp"
	"fmt"
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
type prWait struct {
	group           tui.FactoryGroup
	mark            tui.FactoryMark
	on              tui.WaitsOn
	links           []string
	move, moveShort string
	since           time.Time
}

// prRef is the PR of task view v as magnum's registry names it.
func prRef(v *taskView) PRRef {
	return PRRef{Repo: cmp.Or(v.link.Repo, v.t.Repo), Number: cmp.Or(v.link.Number, v.t.PRNumber)}
}

// readPRs reads the PRs of the published views from magnum's registry,
// once, and sets each one's wait and group (prWaitOf), and since when it
// waits so: the later of the task's last span (its publish, a reopen) and
// when its PR began to wait as it does; without a Magnum, or with no
// published task, it reads nothing.
func (s *screen) readPRs(m Magnum) {
	var refs []PRRef
	for _, v := range s.views {
		if v.t.State == registry.TaskPublished && prRef(v).Number > 0 {
			refs = append(refs, prRef(v))
		}
	}
	if m == nil || len(refs) == 0 {
		return
	}
	states, err := m.PRStates(s.ctx, refs)
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
	waits := func(who, what, short, long string) prWait {
		return prWait{group: tui.GroupWaits, mark: tui.MarkWaits, on: tui.WaitsOn{Who: who, What: what, Short: short, Long: long}}
	}
	you := func(what, short, long string) prWait {
		return prWait{group: tui.GroupNeedsYou, mark: tui.MarkNeedsYou,
			on: tui.WaitsOn{Who: waitsYou, What: what, Short: short, Long: long, OnYou: true}}
	}
	fixing := func() prWait {
		what := fmt.Sprintf("magnum fixes #%d", n)
		switch {
		case st.Fix.Phase == engine.FixPhaseReviewing:
			what += " (re-review of " + textx.ShortSHA(st.Fix.Head) + ")"
		case st.Fix.Cycle > 0:
			what += fmt.Sprintf(" (cycle %d of %d)", st.Fix.Cycle, st.Fix.Cap)
		}
		w := waits("agent", what, clauseShort(what), "")
		w.mark = tui.MarkWorking
		return w
	}
	closedYou := func() prWait {
		w := you(fmt.Sprintf("PR #%d closed without a merge", n), "", fmt.Sprintf("PR #%d closed without a merge: %s", n, reopenMove))
		w.move, w.moveShort = reopenMove, "reopen or x"
		return w
	}
	switch {
	case !read && waitsOnYou(t): // the tick's own wait on you (a close past the grace) stands
		return you(t.WaitsWhat, clauseShort(t.WaitsWhat), "")
	case !read:
		why := "magnum's registry cannot be read"
		if err != nil {
			why += ": " + textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 200)
		}
		return waits("magnum", looking, "", looking+": "+why)
	case !st.Found && !st.Watched:
		what := fmt.Sprintf("magnum does not watch %s: merge #%d or close it yourself", ref.Repo, n)
		return you(what, clauseShort(what), "")
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
		w := waits("magnum", what, clauseShort(what), "")
		w.since = closed
		return w
	case !st.DaemonUp:
		return waits("magnum", "daemon down", "", fmt.Sprintf("magnum's daemon is down: nothing looks at #%d", n))
	case !st.Found:
		return waits("magnum", looking, "", "")
	case st.HasOpen && st.Stale:
		return waits("magnum", looking, "", "")
	case st.HasOpen:
		w := s.verdictWait(st, n, waits, you, fixing)
		w.since = cmp.Or(st.Open.Since, st.Open.At)
		return w
	case st.OpenWatch: // its verdict is still to come
		return waits("magnum", looking, "", "")
	case st.Fixing:
		w := fixing()
		w.since = st.Fix.StartedAt
		return w
	case st.State == store.PRNeedsAttention:
		msg := cmp.Or(st.AttentionMsg, st.Attention, "needs attention")
		w := you("magnum: "+msg, "magnum: "+firstClause(msg), "")
		w.since = st.UpdatedAt
		return w
	}
	return waits("magnum", fmt.Sprintf("review of #%d", n), "", "")
}

// reopenMove is your move on a task whose PR closed without a merge.
const reopenMove = "reopen it, or x drops the task"

// verdictWait is the wait of a PR whose verdict st.Open tells its state.
func (s *screen) verdictWait(st PRState, n int, waits func(who, what, short, long string) prWait,
	you func(what, short, long string) prWait, fixing func() prWait) prWait {
	o := st.Open
	short := clauseShort(o.Why)
	switch o.Verdict {
	case engine.OpenFixing:
		return fixing()
	case engine.OpenNeedsYou:
		w := you(o.Why, short, "")
		for _, need := range o.Needs {
			if need.URL != "" {
				w.links = append(w.links, need.URL)
			}
		}
		return w
	case engine.OpenIdle:
		if st.Approved {
			w := you(o.Why, short, "")
			w.move, w.moveShort = fmt.Sprintf("merge #%d on GitHub", n), "merge it"
			return w
		}
		return waits("reviewers", o.Why, short, "")
	case engine.OpenOff:
		what := offWait(o.Off, n)
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
// your move on it.
func offWait(off string, n int) string {
	switch off {
	case engine.OpenOffFixOff:
		return fmt.Sprintf("fix is off for #%d (F on the board turns it on)", n)
	case engine.OpenOffDraft:
		return fmt.Sprintf("#%d is a draft: mark it ready", n)
	case engine.OpenOffFork:
		return fmt.Sprintf("#%d's head lives in a fork: magnum fixes nothing there, so its feedback is yours", n)
	case engine.OpenOffIncludeOwn:
		return fmt.Sprintf("#%d is yours to follow: its watch leaves your own PRs out (include_own)", n)
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
