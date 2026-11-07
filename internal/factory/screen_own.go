package factory

// Your own open PRs on home: from magnum's registry (Magnum.OwnPRs, read
// once per frame), one row each, whether the factory wrote them or you
// did. A PR a task that is not done or stopped has stays that task's row.
// A row says what a published task's row says of its PR (magnumWait), and
// that decides its group, as for a task's: a PR whose verdict needs you,
// whose findings no loop takes, that waits on your merge, or that needs
// your attention or where magnum stopped arguing (whatever its verdict
// says, and without one) is in NEEDS YOU; one that waits on others (its
// reviewers, CI, magnum, a fix loop at work) in WAITS ON OTHERS; one
// magnum's look leaves alone (a baseline, fix off), or without a verdict
// one magnum does not review, is drawn dim with why in a group of its own
// (tui.GroupYours), oldest first, and asks nothing. The rows are read
// only: f, F, K and o on them, as on a task's row with a PR (screenPR), go
// through magnum's own request paths, as the board's keys do, never
// through the factory daemon.

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
		s.noteBuild(st)
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
// prKey): their rows say what the PRs wait on, so your PRs' rows leave them
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

// ownRow is your own PR st's row on home and its card (for b: its page),
// with its CI, FINDINGS and APPR cells, waiting since its wait began (the
// verdict's Since, a loop's start, an attention's row change), else since
// its last activity on GitHub, never magnum's last write of its row. It
// is magnumWait's wait, in NEEDS YOU when it is on you and in WAITS ON
// OTHERS when it waits on others, except for a PR magnum's look leaves
// alone (off), which is dim in a group of its own. An attention
// or a stalemate needs you whatever the verdict says (fix off does not
// hide it, and a verdict decided before it is stale), unless a loop works
// on the PR. Without a fresh verdict, a PR magnum does not review (a
// baseline, include_own = false, a mute) is off too: no review comes.
func (s *screen) ownRow(st PRState) (tui.FactoryRow, tui.TaskCard) {
	n := st.Ref.Number
	w := magnumWait(st, n, s.clock())
	fresh := st.HasOpen && !st.Stale
	off := offOf(st)
	switch {
	case st.Fixing:
	case st.State == store.PRNeedsAttention || st.Stalemate:
		if w.group != tui.GroupNeedsYou || fresh && st.Open.Verdict == engine.OpenOff {
			w = attentionWait(st, n)
		}
	case !fresh && off != "":
		w = youWait(off, clauseShort(off), "")
	}
	key := s.ownKey(st.Ref)
	row := tui.FactoryRow{ID: -st.ID, Key: key, GitHub: fmt.Sprintf("#%d", n), Repo: st.Ref.Repo, Title: st.Title,
		WaitsOn: []tui.WaitsOn{w.on}, Since: cmp.Or(w.since, st.ActivityAt, st.UpdatedAt), Group: tui.GroupYours, Mark: tui.MarkWaits}
	cellsOf(st).fill(&row)
	trackers := s.cfg.TrackersFor(st.Ref.Repo)
	row.Jira, _ = config.Issue(st.Title, trackers)
	row.PR = screenPR(st)
	switch {
	case off != "": // magnum leaves it alone: it asks nothing
	case w.group == tui.GroupNeedsYou:
		row.Group, row.Mark = tui.GroupNeedsYou, tui.MarkNeedsYou
		row.Move, row.MoveShort = cmp.Or(w.move, ownMove), cmp.Or(w.moveShort, ownMoveShort)
	default: // it waits on others, as a published task's row that says the same
		row.Group, row.Mark = tui.GroupWaits, w.mark
	}
	row.Help = cmp.Or(w.help, w.on.Who+": "+cmp.Or(w.on.Long, w.on.What))
	card := tui.TaskCard{Key: key, Title: st.Title, Repo: st.Ref.Repo, PR: fmt.Sprintf("#%d", n), PRURL: cmp.Or(st.URL, githubPR(st.Ref)),
		WaitingFor: cmp.Or(w.card, w.on.Who+": "+w.on.What), Links: w.links, FixNeeds: w.needs, FixReport: w.report,
		PRInfo: cardPR(st, s.clock())}
	card.Issue, card.IssueURL = config.Issue(st.Title, trackers)
	return row, card
}

// screenPR is open PR st as a row's f, F, K and o act on it, your PR's or
// a task's: its reference, whether magnum's fix loop runs on it, fix off,
// why magnum's look leaves it alone (offOf), and whether magnum's open of
// it restores it in a slot.
func screenPR(st PRState) *tui.FactoryPR {
	if !st.Found || st.GHState != store.GHOpen {
		return nil
	}
	fresh := st.HasOpen && !st.Stale
	return &tui.FactoryPR{Ref: fmt.Sprintf("%s#%d", st.Ref.Repo, st.Ref.Number), FixOff: st.FixOff, Off: offOf(st),
		Fixing: st.Fixing || fresh && st.Open.Verdict == engine.OpenFixing, Restores: st.OpenRestores, Hold: st.OpenHold}
}

// offOf is why magnum's look leaves PR st alone, as its row says it: its
// fresh verdict's off, or without one a PR magnum does not review (a
// baseline, include_own = false, a mute); "" while a loop works on it, it
// needs your attention or magnum stopped arguing there, and when the look
// covers it.
func offOf(st PRState) string {
	fresh := st.HasOpen && !st.Stale
	switch {
	case st.Fixing, st.State == store.PRNeedsAttention, st.Stalemate:
		return ""
	case fresh && st.Open.Verdict == engine.OpenOff:
		return offWait(st.Open.Off, st.Ref.Number)
	case !fresh && (st.State == store.PRIneligible || st.State == store.PRBaseline):
		why := engine.OpenOffIneligible
		if st.State == store.PRBaseline {
			why = engine.OpenOffBaseline
		}
		return offWait(why, st.Ref.Number)
	}
	return ""
}

// ownKey is the key of your PR's row: its number ("#4512") when the
// factory has one repository and the PR is of it, else its repository's
// name before it ("shop#12", "widget#12"), so two repositories' PRs are
// told apart in a y/N, a more line and to a double click. On home the
// row's REPO column names the repository and its GitHub cell the number.
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
