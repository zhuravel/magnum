package tui

import (
	"cmp"
	"slices"
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
	// A long name with an age and someone else's mark gives up its end, never
	// the age or the mark.
	js := f.Rows[5].JiraStatus
	js.Since, js.AssignedToYou = factoryAgo(factoryNow, 75*time.Hour), false
	frame, _ = factoryFrameOf(f, 200, factoryHomeHeight, true, IconsUnicode, factoryNow)
	if cells, _ = statusCells(t, frame, f.Rows); !strings.HasSuffix(cells["PR-12388"], "… 3d not you") {
		t.Errorf("a long name's STATUS cell reads %q, want its name cut before \"3d not you\"", cells["PR-12388"])
	}
}

// The STATUS column is the first column a screen gives up: below 180
// columns, however much room the rows leave, and on a wider one that
// cannot hold it beside every other column with a title of 24 cells (here
// a long blocker), the frame is the one without any Jira status, every
// other column where it was.
func TestTheStatusColumnGoesFirst(t *testing.T) {
	for _, w := range []int{100, 120, 140, 179} {
		with, _ := factoryFrameOf(fixtureJira(), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		without, _ := factoryFrameOf(fixtureHome(), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		if with != without {
			t.Errorf("%d columns: the Jira statuses changed the frame:\n%s\nwant:\n%s", w, ansi.Strip(with), ansi.Strip(without))
		}
	}
	// One short row would hold the column at any width: below 180 it has none.
	one := fixtureOne()
	one.Rows[0].JiraStatus = &FactoryJiraStatus{Name: "In Progress", Category: "indeterminate", Assigned: true, AssignedToYou: true,
		ReadAt: factoryNow, TriedAt: factoryNow}
	for _, w := range []int{100, 140, 179} {
		with, _ := factoryFrameOf(one, w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		without, _ := factoryFrameOf(fixtureOne(), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		if with != without {
			t.Errorf("%d columns, one row: the Jira status changed the frame:\n%s", w, ansi.Strip(with))
		}
	}
	if frame, _ := factoryFrameOf(one, 180, factoryHomeHeight, true, IconsUnicode, factoryNow); !strings.Contains(ansi.Strip(frame), "In Progress") {
		t.Errorf("180 columns, one row: no STATUS column:\n%s", ansi.Strip(frame))
	}
	long := func(f factoryFrame) factoryFrame {
		f.Rows[3].WaitsOn[0].What, f.Rows[3].WaitsOn[0].Long = "finding the cause in the coupon export's batch job, 2 of 3 agents done", ""
		return f
	}
	// Wherever the column shows, the title keeps 24 cells, a blocker of the
	// 140-column form 10 cells longer than the spec's too.
	longer := fixtureJira()
	longer.Rows[3].WaitsOn[0].What = "finding the cause in the export job, 2 of 3 done"
	for name, f := range map[string]factoryFrame{"spec": fixtureJira(), "longer": longer, "long": long(fixtureJira())} {
		for w := factoryStatusFrom; w <= 230; w++ {
			frame, _ := factoryFrameOf(f, w, factoryHomeHeight, true, IconsUnicode, factoryNow)
			lines := strings.Split(ansi.Strip(frame), "\n")
			at := strings.Index(lines[3], " STATUS ")
			if at < 0 {
				continue
			}
			row := factoryRowLines(t, frame, f.Rows)[0]
			before, _, ok := strings.Cut(row, "Deploy list")
			if !ok {
				t.Fatalf("%s at %d columns: PR-12310's row has no title: %q", name, w, row)
			}
			if title := ansi.StringWidth(lines[3][:at]) + 1 - 2 - ansi.StringWidth(before); title < factoryKeepTitle {
				t.Errorf("%s at %d columns: the STATUS column leaves the title %d cells, under %d:\n%s", name, w, title, factoryKeepTitle,
					ansi.Strip(frame))
			}
		}
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

// fixtureJiraCard is a published task's card whose issue is in code review
// for 13 hours, assigned to you, read 12 minutes ago.
func fixtureJiraCard() TaskCard {
	c := fixturePublishedCard(0)
	c.Issue = "PR-12320"
	read := factoryAgo(cardTaskNow, 12*time.Minute)
	c.JiraStatus = &FactoryJiraStatus{Name: "In Code Review", Category: "indeterminate", Since: factoryAgo(cardTaskNow, 13*time.Hour),
		Assigned: true, AssignedToYou: true, ReadAt: read, TriedAt: read}
	return c
}

// The task card's JIRA line says where the issue stands: its key, the
// status and since when, who has it, the last read's age and, when the
// last try failed, why ("now: ..."); a key whose reads never worked says
// so; a task the factory has not read yet has no line. Its words are the
// struct's alone. Before, the card named no Jira status.
func TestTaskCardSaysWhereItsJiraIssueStands(t *testing.T) {
	checkCardGolden(t, "task-jira", drawTask(fixtureJiraCard(), 0))
	read, tried := factoryAgo(cardTaskNow, 12*time.Minute), factoryAgo(cardTaskNow, 3*time.Minute)
	for name, c := range map[string]struct {
		js   *FactoryJiraStatus
		want string
	}{
		"yours": {fixtureJiraCard().JiraStatus, "PR-12320 · In Code Review for 13h · assigned to you · last read 12m ago"},
		"someone else's": {&FactoryJiraStatus{Name: "To Do", Category: "new", Since: factoryAgo(cardTaskNow, 50*time.Hour), Assigned: true,
			ReadAt: read, TriedAt: read}, "PR-12320 · To Do for 2d · assigned to someone else · last read 12m ago"},
		"nobody's, since unknown": {&FactoryJiraStatus{Name: "Released", Category: "done", ReadAt: read, TriedAt: read},
			"PR-12320 · Released · unassigned · last read 12m ago"},
		"the last try failed": {&FactoryJiraStatus{Name: "In Code Review", Category: "indeterminate", Assigned: true, AssignedToYou: true,
			Since: factoryAgo(cardTaskNow, 13*time.Hour), ReadAt: read, TriedAt: tried, LastError: "Jira did not answer within 30s"},
			"PR-12320 · In Code Review for 13h · assigned to you · last read 12m ago · now: Jira did not answer within 30s"},
		"never read": {&FactoryJiraStatus{TriedAt: tried, LastError: "Jira refused the account (HTTP 403)"},
			"PR-12320 · no read of it has worked yet · now: Jira refused the account (HTTP 403)"},
		"a hostile name": {&FactoryJiraStatus{Name: hostileJiraName, Category: "indeterminate",
			Assigned: true, AssignedToYou: true, ReadAt: read, TriedAt: read}, "PR-12320 · In Review · assigned to you · last read 12m ago"},
		"moved under a minute ago": {&FactoryJiraStatus{Name: "In Code Review", Category: "indeterminate", Assigned: true, AssignedToYou: true,
			Since: factoryAgo(cardTaskNow, 20*time.Second), ReadAt: read, TriedAt: read},
			"PR-12320 · In Code Review since just now · assigned to you · last read 12m ago"},
		"Jira's clock ahead": {&FactoryJiraStatus{Name: "In Code Review", Category: "indeterminate", Assigned: true, AssignedToYou: true,
			Since: cardTaskNow.Add(5 * time.Minute), ReadAt: read, TriedAt: read},
			"PR-12320 · In Code Review since just now · assigned to you · last read 12m ago"},
		"no daemon reads it": {&FactoryJiraStatus{Name: "In Code Review", Category: "indeterminate", Assigned: true, AssignedToYou: true,
			Since: factoryAgo(cardTaskNow, 13*time.Hour), ReadAt: read, TriedAt: read, DaemonDown: true},
			"PR-12320 · In Code Review for 13h · assigned to you · last read 12m ago · now: the factory daemon is down"},
		"assigned, not known to be you": {&FactoryJiraStatus{Name: "In Code Review", Category: "indeterminate", Assigned: true, YouUnknown: true,
			Since: factoryAgo(cardTaskNow, 13*time.Hour), ReadAt: read, TriedAt: read},
			"PR-12320 · In Code Review for 13h · assigned (not known to be you) · last read 12m ago"},
	} {
		card := fixtureJiraCard()
		card.JiraStatus = c.js
		frame := plainCard(drawTask(card, 0), 140)
		if got := strings.TrimSpace(cardLineWith(t, frame, "JIRA ")); got != "JIRA         "+c.want {
			t.Errorf("%s: the JIRA line is %q, want %q", name, got, "JIRA         "+c.want)
		}
	}
	card := fixtureJiraCard()
	card.JiraStatus = nil
	if strings.Contains(plainCard(drawTask(card, 0), 140), "JIRA ") {
		t.Error("a task whose key was never tried draws a JIRA line")
	}
}

// hostileJiraName is a status name that carries a color and a link: the
// jira package cleans it, and the painters clean it again.
const hostileJiraName = "In \x1b[31mReview\x1b]8;;https://example.com\x07"

// The painters clean a status's name themselves: no escape of it reaches
// the terminal, from the card's JIRA line or the row's STATUS cell.
func TestAHostileJiraNameReachesNoTerminal(t *testing.T) {
	card := fixtureJiraCard()
	card.JiraStatus.Name = hostileJiraName
	f := fixtureJira()
	f.Rows[0].JiraStatus.Name = hostileJiraName
	f.Cursor = -1 // the selected row is drawn in reverse video, without its own styles
	row, _ := factoryFrameOf(f, 200, factoryHomeHeight, true, IconsUnicode, factoryNow)
	for what, raw := range map[string]string{"the card": drawTask(card, 0)(140, 0, true, IconsUnicode), "the row": row} {
		if !strings.Contains(ansi.Strip(raw), "In Review") {
			t.Errorf("%s does not draw the name:\n%s", what, ansi.Strip(raw))
		}
		for _, esc := range []string{"\x1b[31mReview", "\x1b]8;;https://example.com"} {
			if strings.Contains(raw, esc) {
				t.Errorf("%s carries the name's escape %q to the terminal", what, esc)
			}
		}
	}
}

// A failure too long for the JIRA line goes under it, whole, in the
// failure's color; the status keeps its category's color.
func TestTaskCardsJiraFailureGoesUnderALineItDoesNotFit(t *testing.T) {
	card := fixtureJiraCard()
	card.JiraStatus.Category, card.JiraStatus.Name = "done", "Released"
	card.JiraStatus.LastError = "Jira refused the credentials (HTTP 401): check jira.user and jira.token_file"
	frame := plainCard(drawTask(card, 0), 100)
	line := cardLineWith(t, frame, "JIRA ")
	if strings.Contains(line, "now:") || !strings.Contains(line, "last read 12m ago") {
		t.Errorf("the JIRA line at 100 columns is %q, want the failure under it", line)
	}
	if !strings.Contains(cardSquash(frame), "now: Jira refused the credentials (HTTP 401): check jira.user and jira.token_file") {
		t.Errorf("the failure is not under the JIRA line whole:\n%s", frame)
	}
	st := newStyles(true)
	colored := drawTask(card, 0)(140, 0, true, IconsUnicode)
	for what, want := range map[string]string{"done green": st.OK.Render("Released"),
		"the failure red": st.Err.Render("now: Jira refused the credentials (HTTP 401): check jira.user and jira.token_file")} {
		if !strings.Contains(colored, want) {
			t.Errorf("%s: the card lacks %q", what, want)
		}
	}
}

// fixtureJiraDone is the published tasks' home with their open PRs read
// from magnum's registry: PR-12325's issue is done while its PR is open,
// PR-12324's is in code review, the merged task's issue is done, and
// PR-12399 (no PR read yet) has an issue that is done.
func fixtureJiraDone() factoryFrame {
	f := fixturePublished()
	read := factoryAgo(factoryNow, 4*time.Minute)
	status := func(name, category string, since time.Duration) *FactoryJiraStatus {
		return &FactoryJiraStatus{Name: name, Category: category, Since: factoryAgo(factoryNow, since), Assigned: true, AssignedToYou: true,
			ReadAt: read, TriedAt: read}
	}
	for i := range f.Rows {
		if r := &f.Rows[i]; r.Group != GroupDone {
			r.PR = &FactoryPR{Ref: "example/talkable" + r.GitHub}
		}
	}
	f.Rows[1].JiraStatus = status("In Code Review", "indeterminate", 5*time.Hour)
	f.Rows[4].JiraStatus = status("Done", "done", 2*time.Hour)
	f.Rows[5].JiraStatus = status("Done", "done", 40*time.Minute)
	f.Rows = append(f.Rows, FactoryRow{ID: 7, Key: "PR-12399", Title: "Coupon CSV export", Group: GroupWaits, Mark: MarkWaits,
		WaitsOn: []WaitsOn{{Who: "magnum", What: "reading #4540"}}, Since: factoryAgo(factoryNow, time.Hour),
		JiraStatus: status("Done", "done", time.Hour)})
	f.Cursor = -1
	return f
}

// Jira saying an issue is done while its task's PR is still open is a
// yellow fact on the row: its STATUS cell when the screen draws one, else
// "Jira done" before what it waits on; an issue done whose PR merged, or
// whose PR magnum has not read, is no such fact. Before, nothing said it.
func TestJiraDoneWhileThePRIsOpenIsAYellowFact(t *testing.T) {
	f := fixtureJiraDone()
	for _, w := range []int{100, 140} {
		frame, _ := factoryFrameOf(f, w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		if n := strings.Count(strings.Join(warnRuns(t, frame), "\n"), "Jira done"); n != 1 {
			t.Errorf("%d columns: %d yellow \"Jira done\", want 1: %q", w, n, warnRuns(t, frame))
		}
		lines := factoryRowLines(t, frame, f.Rows)
		for i, line := range lines {
			switch has := strings.Contains(line, "Jira done"); {
			case f.Rows[i].ID == 5 && (!has || !strings.Contains(line, "Jira done · reviewers:")):
				t.Errorf("%d columns: PR-12325's row does not say Jira done before what it waits on: %q", w, line)
			case f.Rows[i].ID != 5 && has:
				t.Errorf("%d columns: %s's row says Jira done: %q", w, f.Rows[i].Key, line)
			}
		}
	}
	frame, _ := factoryFrameOf(f, 200, factoryHomeHeight, true, IconsUnicode, factoryNow)
	cells, ok := statusCells(t, frame, f.Rows)
	if !ok {
		t.Fatalf("200 columns draw no STATUS column:\n%s", ansi.Strip(frame))
	}
	if cells["PR-12325"] != "Done 2h" || strings.Contains(ansi.Strip(frame), "Jira done") {
		t.Errorf("200 columns: PR-12325's STATUS reads %q, and the frame says Jira done %v times:\n%s", cells["PR-12325"],
			strings.Count(ansi.Strip(frame), "Jira done"), ansi.Strip(frame))
	}
	if runs := warnRuns(t, frame); !slices.Contains(runs, "Done") {
		t.Errorf("200 columns: PR-12325's Done is not yellow: %q", runs)
	}
	if st := newStyles(true); !strings.Contains(frame, st.OK.Render("Done")) {
		t.Error("200 columns: PR-12399's Done, whose PR magnum has not read, is not green")
	}
}

// The task card says the yellow fact in a short reason after its JIRA line,
// on a task whose PR is open; one without an open PR says nothing of it.
func TestTaskCardSaysJiraIsDoneWhileItsPRIsOpen(t *testing.T) {
	card := fixtureJiraCard()
	card.JiraStatus.Name, card.JiraStatus.Category = "Done", "done"
	card.PRInfo = &CardPR{State: "open · head a1b2c3d"}
	const reason = "Jira says done while #4530 is still open"
	if runs := warnRuns(t, drawTask(card, 0)(140, 0, true, IconsUnicode)); !slices.Contains(runs, reason) {
		t.Errorf("the card's yellow runs are %q, want %q", runs, reason)
	}
	if l := cardLineWith(t, plainCard(drawTask(card, 0), 140), "JIRA "); !strings.HasSuffix(strings.TrimSpace(l), "last read 12m ago · "+reason) {
		t.Errorf("the JIRA line is %q, want the reason after it", l)
	}
	card.JiraStatus.Name, card.JiraStatus.Category = "In Code Review", "indeterminate"
	if frame := plainCard(drawTask(card, 0), 140); strings.Contains(frame, "Jira says done") {
		t.Errorf("a card whose issue is in progress says Jira is done:\n%s", frame)
	}
	card.JiraStatus.Name, card.JiraStatus.Category = "Done", "done"
	card.PRInfo = nil
	if frame := plainCard(drawTask(card, 0), 140); strings.Contains(frame, "Jira says done") {
		t.Errorf("a card without an open PR says Jira is done while it is open:\n%s", frame)
	}
}

// The help glosses the STATUS column, its marks, the yellow fact and the
// card's JIRA line.
func TestTheHelpGlossesTheJiraStatusWords(t *testing.T) {
	h := testFactory(t, 140, 80)
	help := ansi.Strip(strings.Join(h.m.helpContent(220), "\n"))
	mustContain(t, help,
		"STATUS  the task's Jira status and how long the issue has held it (In Code Review 13h), on a screen of 180 columns or more",
		"Done, To Do, ?  STATUS's colors: done green, in progress plain, to do dim; ? no read of the issue worked yet (its card says why)",
		"not you, nobody  after the status: the issue is assigned to someone else, or to nobody",
		"assigned  after the status: assigned, not known to be you (Jira answered for the issue, not for your account)",
		"In Code Review 3d ?  all dim, ? after it: its last try failed or the factory daemon is down, so it may be old (no Jira done of it)",
		"Jira done  yellow: Jira says done while the task's PR is still open (in STATUS when shown): merge or close it, or move the issue back",
		"JIRA  on the task card: the issue key, status and for how long, who has it, the last read's age; now: why it is not read now")
	for l := range strings.SplitSeq(ansi.Strip(strings.Join(h.m.helpContent(140), "\n")), "\n") {
		if strings.HasPrefix(l, "STATUS ") || strings.HasPrefix(l, "Jira done ") || strings.HasPrefix(l, "JIRA ") || strings.HasPrefix(l, "Done, To Do") ||
			strings.HasPrefix(l, "assigned ") || strings.HasPrefix(l, "In Code Review 3d ?") || strings.HasPrefix(l, "not you, nobody ") {
			if strings.HasSuffix(l, "…") {
				t.Errorf("at 140 columns the help cuts %q", l)
			}
		}
	}
}

// A status whose last try failed before the issue answered (its read is
// older than the try), or that no factory daemon reads, is the last read's
// that worked and may no longer hold: its STATUS cell is dim, the mark
// too, with a dim ? after it, never green. A read whose issue answered
// while a part of it failed is drawn as it was read. Before, a read of three
// days ago looked like this minute's.
func TestAnOldJiraReadIsDrawnDimWithAMark(t *testing.T) {
	f := fixtureJira()
	f.Cursor = -1 // the selected row is drawn in reverse video
	for i := range f.Rows {
		switch js := f.Rows[i].JiraStatus; f.Rows[i].ID {
		case 1: // the token expired three days ago
			js.Since, js.ReadAt = factoryAgo(factoryNow, 80*time.Hour), factoryAgo(factoryNow, 72*time.Hour)
			js.TriedAt, js.LastError = factoryAgo(factoryNow, 3*time.Minute), "Jira refused the credentials (HTTP 401)"
		case 2: // the issue answered, its changelog did not
			js.LastError = "Jira answered for the issue but not for its changelog: Jira has no such issue, or the account cannot see it (HTTP 404)"
		case 3:
			js.DaemonDown = true
		}
	}
	frame, _ := factoryFrameOf(f, 200, factoryHomeHeight, true, IconsUnicode, factoryNow)
	cells, ok := statusCells(t, frame, f.Rows)
	if !ok {
		t.Fatalf("200 columns draw no STATUS column:\n%s", ansi.Strip(frame))
	}
	for key, want := range map[string]string{"PR-12310": "In Code Review 3d ?", "PR-12345": "To Do 2d nobody", "PR-12360": "Released 3h not you ?"} {
		if got := cells[key]; got != want {
			t.Errorf("%s's STATUS cell reads %q, want %q", key, got, want)
		}
	}
	st := newStyles(true)
	for _, want := range []string{st.Dim.Render("In Code Review"), st.Dim.Render("Released"), st.Dim.Render("not you"), st.Dim.Render(" ?")} {
		if !strings.Contains(frame, want) {
			t.Errorf("the frame lacks the old read's dim %q", ansi.Strip(want))
		}
	}
	if strings.Contains(frame, st.OK.Render("Released")) || strings.Contains(frame, st.Accent.Render("not you")) {
		t.Error("an old read keeps its category's green or its mark's accent")
	}
}

// Jira done on an old read (its last try failed before the issue answered,
// or no daemon reads it) is no yellow fact: not on the row at any width,
// not on the card. Before, a done read of last week stayed yellow.
func TestJiraDoneOnAnOldReadIsNoYellowFact(t *testing.T) {
	for name, old := range map[string]func(js *FactoryJiraStatus){
		"the last try failed": func(js *FactoryJiraStatus) {
			js.TriedAt, js.LastError = factoryAgo(factoryNow, time.Minute), "Jira did not answer within 30s"
		},
		"the daemon is down": func(js *FactoryJiraStatus) { js.DaemonDown = true },
	} {
		f := fixtureJiraDone()
		old(f.Rows[4].JiraStatus)
		for _, w := range []int{100, 140, 200} {
			frame, _ := factoryFrameOf(f, w, factoryHomeHeight, true, IconsUnicode, factoryNow)
			if strings.Contains(ansi.Strip(frame), "Jira done") || slices.Contains(warnRuns(t, frame), "Done") {
				t.Errorf("%s, %d columns: the old read is a yellow fact: %q", name, w, warnRuns(t, frame))
			}
		}
		card := fixtureJiraCard()
		card.JiraStatus.Name, card.JiraStatus.Category = "Done", "done"
		old(card.JiraStatus)
		card.PRInfo = &CardPR{State: "open · head a1b2c3d"}
		if frame := plainCard(drawTask(card, 0), 140); strings.Contains(frame, "Jira says done") {
			t.Errorf("%s: the card says Jira is done while the PR is open:\n%s", name, frame)
		}
	}
}

// An issue assigned while the read of your account failed is assigned, not
// known to be you: its STATUS cell says "assigned", never "not you".
// Before, it read as someone else's.
func TestAnAssigneeNotKnownToBeYouIsNotSaidToBeSomeoneElse(t *testing.T) {
	f := fixtureJira()
	js := f.Rows[0].JiraStatus
	js.Name, js.AssignedToYou, js.YouUnknown = "In Review", false, true
	js.LastError = "Jira answered for the issue but not for its account: Jira refused the account (HTTP 403)"
	frame, _ := factoryFrameOf(f, 200, factoryHomeHeight, true, IconsUnicode, factoryNow)
	if cells, _ := statusCells(t, frame, f.Rows); cells["PR-12310"] != "In Review 13h assigned" {
		t.Errorf("the STATUS cell reads %q, want \"In Review 13h assigned\"", cells["PR-12310"])
	}
}

// A STATUS column in the 140-column layout leaves the title line and the
// help line as the frame without it has them, whole where it says them
// whole: they span the screen and share no width with the table. Before,
// the column cut the memory fact's swap and the help line's question.
func TestTheStatusColumnLeavesTheTitleAndHelpLinesAsTheyWere(t *testing.T) {
	line := func(frame, sub string) string {
		for l := range strings.SplitSeq(ansi.Strip(frame), "\n") {
			if strings.Contains(l, sub) {
				return l
			}
		}
		return ""
	}
	shown := false
	for w := factoryStatusFrom; w <= 240; w++ {
		with, _ := factoryFrameOf(fixtureJira(), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		without, _ := factoryFrameOf(fixtureHome(), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		_, has := statusCells(t, with, fixtureJira().Rows)
		shown = shown || has
		for _, sub := range []string{"magnum factory", "Do we need the deploy list"} {
			if got, want := line(with, sub), line(without, sub); got != want {
				t.Errorf("%d columns: the STATUS column changed the line\n%q\nwant\n%q", w, got, want)
			}
		}
	}
	if !shown {
		t.Error("no width from 180 to 240 drew the STATUS column")
	}
	frame, _ := factoryFrameOf(fixtureJira(), 200, factoryHomeHeight, true, IconsUnicode, factoryNow)
	mustContain(t, ansi.Strip(frame), "memory full: new tasks wait (swap 91%)", `alice on #4512, 2h ago: "Do we need the deploy list for archived campaigns too, or only for live ones?"`)
}

// The JIRA line's parts go under it whole when the card is narrow, as many
// on a line as it holds: at 80 columns who has the issue and the last read's
// age are never cut, nor at 100 with a long status name. Before, the line was
// cut at the screen's edge.
func TestTaskCardsJiraLineWrapsAtItsParts(t *testing.T) {
	read := factoryAgo(cardTaskNow, 12*time.Minute)
	for name, c := range map[string]struct {
		width      int
		js         *FactoryJiraStatus
		first, two string
	}{
		"yours at 80": {80, fixtureJiraCard().JiraStatus, "PR-12320 · In Code Review for 13h · assigned to you", "last read 12m ago"},
		"someone else's at 80": {80, &FactoryJiraStatus{Name: "In Code Review", Category: "indeterminate", Since: factoryAgo(cardTaskNow, 50*time.Hour),
			Assigned: true, ReadAt: read, TriedAt: read}, "PR-12320 · In Code Review for 2d · assigned to someone else", "last read 12m ago"},
		"a long name at 100": {100, &FactoryJiraStatus{Name: "Waiting for the release train to the EU region and beyond", Category: "indeterminate",
			Since: factoryAgo(cardTaskNow, 13*time.Hour), Assigned: true, AssignedToYou: true, ReadAt: read, TriedAt: read},
			"PR-12320 · Waiting for the release train to the EU region and beyond for 13h", "assigned to you · last read 12m ago"},
	} {
		card := fixtureJiraCard()
		card.JiraStatus = c.js
		lines := strings.Split(plainCard(drawTask(card, 0), c.width), "\n")
		i := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, " JIRA ") })
		if i < 0 || i+1 >= len(lines) {
			t.Fatalf("%s: no JIRA line:\n%s", name, strings.Join(lines, "\n"))
		}
		x := strings.Index(lines[i], "PR-12320")
		if got := strings.TrimSpace(lines[i][x:]); got != c.first {
			t.Errorf("%s: the JIRA line reads %q, want %q", name, got, c.first)
		}
		if got := lines[i+1]; strings.TrimSpace(got) != c.two || strings.Index(got, c.two) != x {
			t.Errorf("%s: under the JIRA line %q, want %q from its column", name, got, c.two)
		}
	}
}
