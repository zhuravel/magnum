package tui

import (
	"cmp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// A task's Jira status, as the factory's screen data carries it on its row
// and its task card, reaches the screen's model whole with each load (the
// painters draw it from there), and a row without one keeps none.
func TestATasksJiraStatusReachesTheFactoryScreensModel(t *testing.T) {
	st := &FactoryJiraStatus{Name: "In Review", Category: "indeterminate", Since: factoryAgo(factoryNow, 26*time.Hour),
		Assigned: true, AssignedToYou: true, ReadAt: factoryAgo(factoryNow, 2*time.Minute), LastError: "Jira did not answer within 20s"}
	d := FactoryData{
		Repos: []string{"example/shop"},
		Rows: []FactoryRow{
			{ID: 1, Key: "PR-12360", Title: "Coupon rounding", Group: GroupWaits, Mark: MarkWaits, JiraStatus: st,
				WaitsOn: []WaitsOn{{Who: "reviewers", What: "no approval yet"}}, Since: factoryAgo(factoryNow, time.Hour)},
			{ID: 2, Key: "PR-12361", Title: "Offer email locale", Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "agent", What: "finding the cause"}}, Since: factoryAgo(factoryNow, time.Hour)},
		},
		Cards: map[int64]FactoryCards{1: {Task: TaskCard{Key: "PR-12360", Title: "Coupon rounding", JiraStatus: st}}},
	}
	h := testFactoryWith(t, 160, 40, FactoryOptions{}, d)
	if !h.m.haveData {
		t.Fatal("the screen did not load its data")
	}
	for _, row := range h.m.data.Rows {
		switch row.ID {
		case 1:
			if row.JiraStatus == nil || *row.JiraStatus != *st {
				t.Errorf("row 1's Jira status in the model: %+v, want %+v", row.JiraStatus, st)
			}
		case 2:
			if row.JiraStatus != nil {
				t.Errorf("row 2's Jira status in the model: %+v, want none", row.JiraStatus)
			}
		}
	}
	if c := h.m.data.Cards[1].Task.JiraStatus; c == nil || *c != *st {
		t.Errorf("the task card's Jira status in the model: %+v, want %+v", c, st)
	}
	_ = h.view() // the painters take a row that carries one
}

// fixtureJira is home with each kind of Jira status on its task rows: in
// code review for 13h and assigned to you, to do for 2 days and assigned
// to nobody, released 3h ago and assigned to someone else, never tried
// (nil), every read failed (no name), a long name whose since is not
// known, and a done task's issue that is done.
func fixtureJira() factoryFrame {
	f := fixtureHome()
	now := factoryNow
	read := factoryAgo(now, 4*time.Minute)
	statuses := map[int64]*FactoryJiraStatus{
		1: {Name: "In Code Review", Category: "indeterminate", Since: factoryAgo(now, 13*time.Hour), Assigned: true, AssignedToYou: true,
			ReadAt: read, TriedAt: read},
		2: {Name: "To Do", Category: "new", Since: factoryAgo(now, 50*time.Hour), ReadAt: read, TriedAt: read},
		3: {Name: "Released", Category: "done", Since: factoryAgo(now, 3*time.Hour), Assigned: true, ReadAt: read, TriedAt: read},
		5: {TriedAt: read, LastError: "Jira has no such issue, or the account cannot see it (HTTP 404)"},
		6: {Name: "Waiting for the release train to the EU region", Category: "indeterminate", Assigned: true, AssignedToYou: true,
			ReadAt: read, TriedAt: read},
		7: {Name: "Done", Category: "done", Since: factoryAgo(now, 50*time.Minute), Assigned: true, AssignedToYou: true,
			ReadAt: read, TriedAt: read},
	}
	for i := range f.Rows {
		f.Rows[i].JiraStatus = statuses[f.Rows[i].ID]
	}
	return f
}

// statusCells are the STATUS column's cells of frame's rows, by their key,
// and whether the frame has the column.
func statusCells(t *testing.T, frame string, rows []FactoryRow) (map[string]string, bool) {
	t.Helper()
	lines := strings.Split(ansi.Strip(frame), "\n")
	head := lines[3]
	at, waits := strings.Index(head, " STATUS "), strings.Index(head, " WAITS ON ")
	if at < 0 {
		return nil, false
	}
	x, end := ansi.StringWidth(head[:at])+1, ansi.StringWidth(head[:waits])+1
	cells := map[string]string{}
	for i, line := range factoryRowLines(t, frame, rows) {
		cells[cmp.Or(rows[i].Jira, rows[i].Key)] = strings.TrimSpace(ansi.Cut(line, x, end))
	}
	return cells, true
}

// A wide screen draws a STATUS column on the task rows: the issue's status
// and how long it has held it, a mark when it is assigned to someone else
// or to nobody, the name alone when since is not known, the name cut
// before its age and mark, a dim ? for a key whose reads all failed, and
// nothing for a task the factory has not read yet. Before, nothing drew
// the Jira status the factory reads.
func TestAWideScreenDrawsEachTasksJiraStatusInAColumn(t *testing.T) {
	f := fixtureJira()
	frame, _ := factoryFrameOf(f, 200, factoryHomeHeight, true, IconsUnicode, factoryNow)
	checkFactoryFrame(t, "jira-200.txt", ansi.Strip(frame), 200)
	checkFactoryFrame(t, "jira-200-dark.ansi", frame, 200)
	cells, ok := statusCells(t, frame, f.Rows)
	if !ok {
		t.Fatalf("200 columns draw no STATUS column:\n%s", ansi.Strip(frame))
	}
	for key, want := range map[string]string{
		"PR-12310": "In Code Review 13h", "PR-12345": "To Do 2d nobody", "PR-12360": "Released 3h not you",
		"PR-12371": "", "PR-12330": "?", "PR-12388": "Waiting for the release t…", "PR-12302": "Done 50m",
	} {
		if got := cells[key]; got != want {
			t.Errorf("%s's STATUS cell reads %q, want %q", key, got, want)
		}
	}
	header := strings.Split(ansi.Strip(frame), "\n")[3]
	for _, col := range []string{"WAITS ON", "YOUR MOVE", "VOID", "BART", "BASTION", "WAITED"} {
		if !strings.Contains(header, col) {
			t.Errorf("the STATUS column cost the wide screen its %s column: %q", col, header)
		}
	}
}

// The STATUS column is the first column a screen gives up: below 180
// columns, and on a wider one that cannot hold it beside every other
// column with a title of 24 cells (here a long blocker), the frame is the
// one without any Jira status, every other column where it was.
func TestTheStatusColumnGoesFirst(t *testing.T) {
	for _, w := range []int{100, 120, 140, 179} {
		with, _ := factoryFrameOf(fixtureJira(), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		without, _ := factoryFrameOf(fixtureHome(), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		if with != without {
			t.Errorf("%d columns: the Jira statuses changed the frame:\n%s\nwant:\n%s", w, ansi.Strip(with), ansi.Strip(without))
		}
	}
	long := func(f factoryFrame) factoryFrame {
		f.Rows[3].WaitsOn[0].What, f.Rows[3].WaitsOn[0].Long = "finding the cause in the coupon export's batch job, 2 of 3 agents done", ""
		return f
	}
	for _, w := range []int{180, 190, 200} {
		f := long(fixtureJira())
		with, _ := factoryFrameOf(f, w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		without, _ := factoryFrameOf(long(fixtureHome()), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		if _, ok := statusCells(t, with, f.Rows); ok || with != without {
			t.Errorf("%d columns with a long blocker: the STATUS column took another's room:\n%s\nwant:\n%s", w, ansi.Strip(with), ansi.Strip(without))
		}
	}
}

// The STATUS cell is drawn in its category's color, as the screen draws
// other states: done green, in progress plain, to do dim; the ? of a key
// never read dim, and a done task's row dim as the rest of it.
func TestTheStatusCellIsColoredByItsCategory(t *testing.T) {
	f := fixtureJira()
	f.Cursor = -1 // the selected row is drawn in reverse video
	frame, _ := factoryFrameOf(f, 200, factoryHomeHeight, true, IconsUnicode, factoryNow)
	st := newStyles(true)
	for what, want := range map[string]string{
		"done green": st.OK.Render("Released"), "to do dim": st.Dim.Render("To Do"), "the ? dim": st.Dim.Render("?"),
		"a done task's dim": st.Dim.Render("Done"), "the assignee mark": st.Accent.Render("not you"),
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("%s: the frame lacks %q", what, want)
		}
	}
	for _, not := range []string{st.OK.Render("In Code Review"), st.Dim.Render("In Code Review"), st.Warn.Render("In Code Review")} {
		if strings.Contains(frame, not) {
			t.Errorf("an issue in progress is not drawn plain: %q", not)
		}
	}
}

// The Jira statuses' frames never draw a line wider than the screen.
func TestFactoryJiraFramesNeverOverflow(t *testing.T) {
	for _, mode := range []IconMode{IconsUnicode, IconsNerd, IconsASCII} {
		for _, w := range factoryWidths(40) {
			for h := 8; h <= 32; h += 6 {
				frame, _ := factoryFrameOf(fixtureJira(), w, h, true, mode, factoryNow)
				for i, l := range strings.Split(frame, "\n") {
					if lw := ansi.StringWidth(l); lw > w {
						t.Errorf("%s %dx%d: line %d is %d cells wide: %q", mode, w, h, i+1, lw, ansi.Strip(l))
					}
				}
			}
		}
	}
}
