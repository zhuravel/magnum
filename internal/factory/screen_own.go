package factory

// YOUR PRS on home: your own open PRs from magnum's registry
// (Magnum.OwnPRs, read once per frame), one row each, whether the factory
// wrote them or you did. A PR a task that is not done or stopped has stays
// that task's row. A PR whose verdict needs you, that is idle and approved
// (your merge), or that needs your attention or where magnum stopped
// arguing (whatever its verdict says, and without one) is in NEEDS YOU;
// every other one waits in YOUR PRS, oldest wait first, with the words a
// published task's row has (magnumWait); one magnum's look leaves alone (a
// baseline, fix off), or without a verdict one magnum does not review, is
// drawn dim with why, after the others, and asks nothing. The rows are
// read only: f, F and x on them go through magnum's own request paths, as
// the board's keys do, never through the factory daemon.

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// The blocker line of your PRs that could not be read, short and long.
const ownUnread = "your PRs not read"

// The move on your own PR that needs you, long and short: its page, where
// what needs you is, or the fixer.
const (
	ownMove      = "b opens it, f fixes"
	ownMoveShort = "b or f"
)

// ownRows are the rows and cards of your own open PRs that magnum's
// registry lists (m.OwnPRs) and no task of the screen's has; an error is
// why they could not be read (none are drawn then).
func (s *screen) ownRows(m Magnum) ([]tui.FactoryRow, map[int64]tui.FactoryCards, error) {
	if m == nil {
		return nil, nil, nil
	}
	prs, err := m.OwnPRs(s.ctx)
	if err != nil {
		return nil, nil, err
	}
	tasks := s.taskPRs()
	var rows []tui.FactoryRow
	cards := map[int64]tui.FactoryCards{}
	for _, st := range prs {
		if st.ID <= 0 || tasks[prKey(st.Ref)] {
			continue
		}
		row, card := s.ownRow(st)
		rows = append(rows, row)
		cards[row.ID] = tui.FactoryCards{Task: card}
	}
	return rows, cards, nil
}

// taskPRs are the PRs of the tasks that are neither done nor stopped (by
// prKey): their rows say what the PRs wait on, so YOUR PRS leaves them
// out.
func (s *screen) taskPRs() map[string]bool {
	out := map[string]bool{}
	live := func(t registry.Task) bool { return t.State != registry.TaskDone && t.State != registry.TaskStopped }
	for _, v := range s.views {
		if live(v.t) && prRef(v).Number > 0 {
			out[prKey(prRef(v))] = true
		}
	}
	for _, t := range s.tasks {
		if live(t) && t.PRNumber > 0 {
			out[prKey(PRRef{Repo: t.Repo, Number: t.PRNumber})] = true
		}
	}
	return out
}

// prKey is r as a key that ignores the case of the repository's name.
func prKey(r PRRef) string { return fmt.Sprintf("%s#%d", strings.ToLower(r.Repo), r.Number) }

// ownRow is your own PR st's row on home and its card (for b: its page):
// magnumWait's wait, in NEEDS YOU when it is on you, except for a PR
// magnum's look leaves alone (off), which is dim in YOUR PRS. An attention
// or a stalemate needs you whatever the verdict says (fix off does not
// hide it, and a verdict decided before it is stale), unless a loop works
// on the PR. Without a verdict, a PR magnum does not review (include_own =
// false, a mute, a baseline) is off too.
func (s *screen) ownRow(st PRState) (tui.FactoryRow, tui.TaskCard) {
	n := st.Ref.Number
	w := magnumWait(st, n, true)
	fresh := st.HasOpen && !st.Stale
	off := ""
	switch {
	case st.Fixing:
	case st.State == store.PRNeedsAttention || st.Stalemate:
		if w.group != tui.GroupNeedsYou || fresh && st.Open.Verdict == engine.OpenOff {
			w = attentionWait(st, n)
		}
	case fresh && st.Open.Verdict == engine.OpenOff:
		off = w.on.What
	case !st.HasOpen && !st.OpenWatch && (st.State == store.PRIneligible || st.State == store.PRBaseline):
		why := engine.OpenOffIneligible
		if st.State == store.PRBaseline {
			why = engine.OpenOffBaseline
		}
		off = offWait(why, n, true)
		w = youWait(off, clauseShort(off), "")
	}
	key := s.ownKey(st.Ref)
	row := tui.FactoryRow{ID: -st.ID, Key: key, GitHub: key, Title: st.Title, WaitsOn: []tui.WaitsOn{w.on},
		Since: cmp.Or(w.since, st.UpdatedAt), Group: tui.GroupYours, Mark: tui.MarkWaits}
	trackers := s.cfg.TrackersFor(st.Ref.Repo)
	row.Jira, _ = config.Issue(st.Title, trackers)
	row.PR = &tui.FactoryPR{Ref: fmt.Sprintf("%s#%d", st.Ref.Repo, n), FixOff: st.FixOff, Off: off,
		Fixing: st.Fixing || fresh && st.Open.Verdict == engine.OpenFixing}
	switch {
	case off == "" && w.group == tui.GroupNeedsYou:
		row.Group, row.Mark = tui.GroupNeedsYou, tui.MarkNeedsYou
		row.Move, row.MoveShort = cmp.Or(w.move, ownMove), cmp.Or(w.moveShort, ownMoveShort)
	case w.mark == tui.MarkWorking:
		row.Mark = tui.MarkWorking
	}
	row.Help = w.on.Who + ": " + cmp.Or(w.on.Long, w.on.What)
	card := tui.TaskCard{Key: key, Title: st.Title, PR: fmt.Sprintf("#%d", n), PRURL: cmp.Or(st.URL, githubPR(st.Ref)),
		WaitingFor: w.on.Who + ": " + w.on.What, Links: w.links}
	card.Issue, card.IssueURL = config.Issue(st.Title, trackers)
	return row, card
}

// ownKey is the key of your PR's row: its number ("#4512") when the
// factory has one repository and the PR is of it, else its repository's
// name before it ("shop#12", "widget#12"), so two repositories' PRs are
// told apart, on home, in a y/N and to a double click.
func (s *screen) ownKey(r PRRef) string {
	if repos := s.cfg.Factory.Repos; len(repos) == 1 && strings.EqualFold(repos[0].Repo, r.Repo) {
		return fmt.Sprintf("#%d", r.Number)
	}
	_, name, _ := strings.Cut(r.Repo, "/")
	return fmt.Sprintf("%s#%d", cmp.Or(name, r.Repo), r.Number)
}

// githubPR is r's page on GitHub.
func githubPR(r PRRef) string { return fmt.Sprintf("https://github.com/%s/pull/%d", r.Repo, r.Number) }

// ownUnreadLong is the long blocker line of your PRs that could not be
// read, with why.
func ownUnreadLong(err error) string {
	return ownUnread + " (" + textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 200) + ")"
}
