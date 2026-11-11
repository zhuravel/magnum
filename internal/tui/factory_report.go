package tui

// The reports e opens in the pager, asking nothing (spec 9.D: opening asks
// nothing): magnum's fixer's on home's row of a published task or of your
// PR whose fix loop left you items, and on the task card; the QA run's
// (summary.md) on the ready card of a change visual QA walked, and on the
// task card of a task whose QA fix loop stopped on the writer's declines,
// so you see the shots before A accepts them. The task card's FixReport
// names the fixer's (factory.ScreenData sets it only for a report under
// magnum's reviews directory), and the task card lists the items it left
// you under WAITING FOR (fixNeedRows); the ready card's QAReport and the
// task card's name the QA run's.

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// reportKey opens the fixer's report, or on the ready card the QA run's.
const reportKey = "e"

// defaultPager is $PAGER (its words split, as defaultEditor splits
// $EDITOR's), else less, on path, with -+F added to LESS (yours, else R) so
// less never quits at once on a report that fits the screen, as the diff's
// pager does.
func defaultPager(ctx context.Context, path string) *exec.Cmd {
	args := strings.Fields(cmp.Or(strings.TrimSpace(os.Getenv("PAGER")), "less"))
	cmd := exec.CommandContext(ctx, args[0], append(args[1:], path)...) //nolint:gosec // the operator's own pager on magnum's own report
	cmd.Env = append(os.Environ(), "LESS="+cmp.Or(strings.TrimSpace(os.Getenv("LESS")), "R")+" -+F")
	return cmd
}

// reportOf is the report e opens on the row or task id ("" for none) and
// its name: on the ready card the QA run's (summary.md), elsewhere the
// fixer's ("claude-fix.md"), else the QA run's of a stop on the writer's
// declines.
func (m factoryModel) reportOf(id int64) (path, name string) {
	cards := m.data.Cards[id]
	path = cmp.Or(cards.Task.FixReport, cards.Task.QAReport)
	if m.view == fvReady {
		path = ""
		if cards.Ready != nil {
			path = cards.Ready.QAReport
		}
	}
	if path == "" {
		return "", ""
	}
	return path, cleanText(filepath.Base(path))
}

// reportTarget is what e acts on: home's row, the task card's task, the
// ready card's; false on any other view.
func (m factoryModel) reportTarget() (int64, bool) {
	switch m.view {
	case fvHome:
		r, ok := m.selectedRow()
		return r.ID, ok
	case fvTask, fvReady:
		return m.task, true
	}
	return 0, false
}

// openReport runs the pager on the report of e's row or card (reportOf;
// asks nothing, and on the ready card it is no read of the diff); one
// without a report says so in the footer.
func (m *factoryModel) openReport() tea.Cmd {
	id, ok := m.reportTarget()
	if !ok {
		return nil
	}
	path, _ := m.reportOf(id)
	switch {
	case m.view == fvReady && (path == "" || !filepath.IsAbs(path)):
		return m.note(fmt.Sprintf("no QA report for %s: e opens one when visual QA walked the change", m.taskKey(id)))
	case path == "" || !filepath.IsAbs(path):
		return m.note(fmt.Sprintf("no report of magnum's fixer for %s: e opens one when its fix loop left you items", m.taskKey(id)))
	}
	return tea.ExecProcess(m.pager(m.ctx, path), func(err error) tea.Msg { return factoryPagerMsg{task: id, err: err} })
}

// reportHints are hs with e and the report's name after the last hint
// whose key is one of after (at the end when none is), when task or row
// id has a report.
func (m factoryModel) reportHints(hs []hint, id int64, after ...string) []hint {
	_, name := m.reportOf(id)
	if name == "" {
		return hs
	}
	i := len(hs)
	for j, h := range hs {
		if slices.Contains(after, h.key) {
			i = j + 1
		}
	}
	return slices.Insert(slices.Clone(hs), i, hint{reportKey, name})
}

// reportItem is the menu's item for e on task or row id: dimmed without a
// report.
func (m factoryModel) reportItem(id int64) menuItem {
	path, _ := m.reportOf(id)
	label := "read the fixer's report"
	if c := m.data.Cards[id].Task; c.FixReport == "" && c.QAReport != "" {
		label = "read the QA report"
	}
	return menuItem{label, reportKey, reportKey, path != ""}
}

// cardFixNeedsShown bounds the items WAITING FOR lists; a last line counts
// the rest, which the report has.
const cardFixNeedsShown = 5

// fixNeedRows are the items magnum's fixer left you, under WAITING FOR's
// text: one line each behind a dash, cut at the edge, at most
// cardFixNeedsShown and a line that counts the rest.
func (p *cardPainter) fixNeedRows(items []string) []string {
	return p.itemRows(items, cardFixNeedsShown, fmt.Sprintf("more: %s opens the report", reportKey))
}

// cardFindingsShown bounds the local review's findings WAITING FOR lists
// at a stop at its cap; a last line counts the rest, which the task's
// review-<n>.json has.
const cardFindingsShown = 8

// findingRows are the local review's findings of a stop at its cap, under
// WAITING FOR's text, as fixNeedRows draws the fixer's items.
func (p *cardPainter) findingRows(items []string) []string {
	return p.itemRows(items, cardFindingsShown, "more in the task's review file")
}

// itemRows are items under WAITING FOR's text: one line each behind a
// dash, cut at the edge, at most shown and a line that counts the rest
// ("+3 " and more).
func (p *cardPainter) itemRows(items []string, most int, more string) []string {
	const x = 1 + len("WAITING FOR") + 2
	w := max(p.width-1-x, 1)
	shown := items[:min(len(items), most)]
	out := make([]string, 0, len(shown)+1)
	for _, it := range shown {
		out = append(out, p.line(at(x, w, seg{p.dash() + " ", p.st.Dim}, seg{it, noStyle})))
	}
	if n := len(items) - len(shown); n > 0 {
		out = append(out, p.line(at(x, w, seg{fmt.Sprintf("+%d %s", n, more), p.st.Dim})))
	}
	return out
}
