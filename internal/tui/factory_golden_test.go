package tui

import (
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

var updateFactoryGolden = flag.Bool("update-factory", false, "rewrite the factory screen's golden frames under testdata/factory/")

// The golden frames are the spec's home mockups (section 9.C) drawn by the
// real painter. Their heights are the mockups' own line counts.
const (
	factoryHomeHeight    = 18
	factoryNothingHeight = 20
)

// checkFactoryFrame compares a frame of the real painter with its golden
// file and checks that no line is wider than the screen.
func checkFactoryFrame(t *testing.T, name, frame string, width int) {
	t.Helper()
	for i, l := range strings.Split(frame, "\n") {
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("%s: line %d is %d cells wide, over %d: %q", name, i+1, w, width, ansi.Strip(l))
		}
	}
	path := filepath.Join("testdata", "factory", name)
	if *updateFactoryGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(frame+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run %s -update-factory)", err, t.Name())
	}
	if got := frame + "\n"; got != string(want) {
		t.Errorf("%s differs from its golden frame (go test -run %s -update-factory rewrites it)\ngot:\n%s\nwant:\n%s",
			name, t.Name(), got, want)
	}
}

// factoryFrameOf draws f the way the screen does.
func factoryFrameOf(f factoryFrame, w, h int, dark bool, mode IconMode, now time.Time) (string, map[int64]int) {
	return renderFactory(f, w, h, newStyles(dark), newGlyphs(mode), now)
}

func TestFactoryHomeAt140(t *testing.T) {
	frame, _ := factoryFrameOf(fixtureHome(), 140, factoryHomeHeight, true, IconsUnicode, factoryNow)
	checkFactoryFrame(t, "home-140.txt", ansi.Strip(frame), 140)
}

func TestFactoryHomeAt100(t *testing.T) {
	frame, _ := factoryFrameOf(fixtureHome(), 100, factoryHomeHeight, true, IconsUnicode, factoryNow)
	checkFactoryFrame(t, "home-100.txt", ansi.Strip(frame), 100)
}

func TestFactoryHomeAt200(t *testing.T) {
	frame, _ := factoryFrameOf(fixtureHome(), 200, factoryHomeHeight, true, IconsUnicode, factoryNow)
	checkFactoryFrame(t, "home-200.txt", ansi.Strip(frame), 200)
}

func TestFactoryHomeNothingWaitsOnYou(t *testing.T) {
	frame, _ := factoryFrameOf(fixtureNothing(), 100, factoryNothingHeight, true, IconsUnicode, factoryNothingNow)
	checkFactoryFrame(t, "nothing-100.txt", ansi.Strip(frame), 100)
}

func TestFactoryHomeASCII(t *testing.T) {
	for _, w := range []int{100, 140} {
		frame, _ := factoryFrameOf(fixtureHome(), w, factoryHomeHeight, true, IconsASCII, factoryNow)
		checkFactoryFrame(t, fmt.Sprintf("home-%d-ascii.txt", w), ansi.Strip(frame), w)
	}
}

func TestFactoryHomeDarkANSI(t *testing.T) {
	frame, _ := factoryFrameOf(fixtureHome(), 140, factoryHomeHeight, true, IconsUnicode, factoryNow)
	checkFactoryFrame(t, "home-140-dark.ansi", frame, 140)
}

func TestFactoryHomeLightANSI(t *testing.T) {
	frame, _ := factoryFrameOf(fixtureHome(), 140, factoryHomeHeight, false, IconsUnicode, factoryNow)
	checkFactoryFrame(t, "home-140-light.ansi", frame, 140)
}

func TestFactoryHomeNeverOverflows(t *testing.T) {
	frames := map[string]struct {
		f   factoryFrame
		now time.Time
	}{"home": {fixtureHome(), factoryNow}, "nothing": {fixtureNothing(), factoryNothingNow}}
	for name, c := range frames {
		for _, mode := range []IconMode{IconsUnicode, IconsNerd, IconsASCII} {
			for w := 40; w <= 220; w += 7 {
				for h := 8; h <= 50; h += 6 {
					frame, _ := factoryFrameOf(c.f, w, h, true, mode, c.now)
					lines := strings.Split(frame, "\n")
					if len(lines) > h {
						t.Errorf("%s %s %dx%d: %d lines, over %d", name, mode, w, h, len(lines), h)
					}
					for i, l := range lines {
						if lw := ansi.StringWidth(l); lw > w {
							t.Errorf("%s %s %dx%d: line %d is %d cells wide: %q", name, mode, w, h, i+1, lw, ansi.Strip(l))
						}
					}
				}
			}
		}
	}
}

// warnRuns are the texts the dark palette draws in its warning style.
func warnRuns(t *testing.T, frame string) []string {
	t.Helper()
	open, _, ok := strings.Cut(newStyles(true).Warn.Render("x"), "x")
	if !ok || open == "" {
		t.Fatal("the warning style draws no escape sequence")
	}
	var runs []string
	rest := frame
	for {
		i := strings.Index(rest, open)
		if i < 0 {
			return runs
		}
		rest = rest[i+len(open):]
		end := strings.Index(rest, "\x1b[")
		if end < 0 {
			end = len(rest)
		}
		runs = append(runs, rest[:end])
	}
}

var waitedText = regexp.MustCompile(`^(\d+)(m|h|d)$`)

// minutesOf is a WAITED cell ("41m", "2h", "3d") in minutes.
func minutesOf(t *testing.T, cell string) int {
	t.Helper()
	m := waitedText.FindStringSubmatch(cell)
	if m == nil {
		t.Fatalf("%q is not a wait", cell)
	}
	n, _ := strconv.Atoi(m[1])
	return n * map[string]int{"m": 1, "h": 60, "d": 24 * 60}[m[2]]
}

func TestFactoryYellowMeansOnYouOnly(t *testing.T) {
	f := fixtureHome()
	f.Cursor = 3 // a row that waits on others: the NEEDS YOU rows are drawn in their own colors
	frame, _ := factoryFrameOf(f, 140, factoryHomeHeight, true, IconsUnicode, factoryNow)
	seen := map[string]bool{}
	for _, r := range warnRuns(t, frame) {
		seen[r] = true
		switch {
		case r == "●", r == "you:", r == "1 rule to approve (L)",
			strings.HasPrefix(r, "● ") && strings.HasSuffix(r, " need you"):
		case waitedText.MatchString(r):
			if minutesOf(t, r) <= 30 {
				t.Errorf("a wait of %q is yellow, but it is not past 30 minutes", r)
			}
		default:
			t.Errorf("yellow on %q: only the mark, you:, a wait past 30 minutes and the rule chip may be yellow", r)
		}
	}
	for _, want := range []string{"●", "you:", "2h", "41m", "1 rule to approve (L)", "● 3 tasks need you"} {
		if !seen[want] {
			t.Errorf("%q is not yellow; yellow runs: %q", want, slices.Sorted(maps.Keys(seen)))
		}
	}
	for _, not := range []string{"12m", "9m", "3h", "18m", "agent:", "rev-ann:", "memory:", "nothing:"} {
		if seen[not] {
			t.Errorf("%q is yellow, but it is not on the operator", not)
		}
	}
}

func TestFactoryTheSpinnerMovesOnlyOnRowsWhoseBlockerIsAWorkingAgent(t *testing.T) {
	f := fixtureHome()
	f.Cursor = 5
	a, _ := factoryFrameOf(f, 140, factoryHomeHeight, true, IconsUnicode, factoryNow)
	f.Spinner = 1
	b, _ := factoryFrameOf(f, 140, factoryHomeHeight, true, IconsUnicode, factoryNow)
	al, bl := strings.Split(ansi.Strip(a), "\n"), strings.Split(ansi.Strip(b), "\n")
	var moved []string
	for i := range al {
		if al[i] != bl[i] {
			moved = append(moved, strings.Fields(bl[i])[1])
		}
	}
	// #4512 has an agent at work too, but it needs the operator: it stays still.
	if want := []string{"PR-12371"}; !slices.Equal(moved, want) {
		t.Errorf("rows that moved with the spinner = %q, want %q", moved, want)
	}
}

func TestFactoryTheSpinnerOfASCIIModeIsALine(t *testing.T) {
	f := fixtureHome()
	for i, want := range []string{"|", "/", "-", "\\", "|"} {
		f.Spinner = i
		frame, _ := factoryFrameOf(f, 100, factoryHomeHeight, true, IconsASCII, factoryNow)
		if !strings.Contains(ansi.Strip(frame), "  "+want+" PR-12371") {
			t.Errorf("spinner frame %d is not %q:\n%s", i, want, ansi.Strip(frame))
		}
	}
}

func TestFactoryRowLinesNameTheLineOfEachTask(t *testing.T) {
	frame, rowLines := factoryFrameOf(fixtureHome(), 140, factoryHomeHeight, true, IconsUnicode, factoryNow)
	lines := strings.Split(ansi.Strip(frame), "\n")
	f := fixtureHome()
	if len(rowLines) != len(f.Rows) {
		t.Fatalf("rowLines has %d tasks, want %d: %v", len(rowLines), len(f.Rows), rowLines)
	}
	for _, r := range f.Rows {
		at, ok := rowLines[r.ID]
		if !ok || at < 0 || at >= len(lines) {
			t.Errorf("task %d (%s) has no line: %v", r.ID, r.Key, rowLines)
			continue
		}
		if !strings.Contains(lines[at], r.Key) {
			t.Errorf("task %d (%s) is said to be on line %d, which reads %q", r.ID, r.Key, at, lines[at])
		}
	}
}

func TestFactoryTheCursorRowIsSelectedAndOthersAreNot(t *testing.T) {
	f := fixtureHome()
	f.Cursor = 2
	frame, rowLines := factoryFrameOf(f, 140, factoryHomeHeight, true, IconsUnicode, factoryNow)
	plain := strings.Split(ansi.Strip(frame), "\n")
	raw := strings.Split(frame, "\n")
	reverse := newStyles(true).Selected.Render("x")
	reverse, _, _ = strings.Cut(reverse, "x")
	for _, r := range f.Rows {
		at := rowLines[r.ID]
		selected := r.ID == f.Rows[f.Cursor].ID
		if got := strings.HasPrefix(plain[at], "› "); got != selected {
			t.Errorf("%s: cursor mark = %v, want %v: %q", r.Key, got, selected, plain[at])
		}
		if got := strings.Contains(raw[at], reverse); got != selected {
			t.Errorf("%s: reverse video = %v, want %v", r.Key, got, selected)
		}
	}
}

func TestFactoryStagingColumnsAreLeftOutWhenNoRowHasAny(t *testing.T) {
	f := fixtureHome()
	for i := range f.Rows {
		f.Rows[i].Stagings = nil
	}
	frame := ansi.Strip(frameOnly(factoryFrameOf(f, 140, factoryHomeHeight, true, IconsUnicode, factoryNow)))
	for _, name := range []string{"VOID", "BART", "BASTION"} {
		if strings.Contains(frame, name) {
			t.Errorf("the %s column is drawn though no row has a staging cell:\n%s", name, frame)
		}
	}
	if !strings.Contains(frame, "WAITED") || !strings.Contains(frame, "YOUR MOVE") {
		t.Errorf("the other column names are gone:\n%s", frame)
	}
}

func TestFactoryStagingColumnsTakeTheirNamesFromTheData(t *testing.T) {
	f := fixtureHome()
	for i := range f.Rows {
		for j := range f.Rows[i].Stagings {
			f.Rows[i].Stagings[j].Name = []string{"alpha", "beta", "gamma"}[j]
		}
	}
	head := strings.Split(ansi.Strip(frameOnly(factoryFrameOf(f, 140, factoryHomeHeight, true, IconsUnicode, factoryNow))), "\n")[3]
	for _, name := range []string{"ALPHA", "BETA", "GAMMA"} {
		if !strings.Contains(head, name) {
			t.Errorf("column heading line lacks %s: %q", name, head)
		}
	}
}

func TestFactoryABigCiCountUsesThePercentFormWhenItDoesNotFitNineCells(t *testing.T) {
	f := fixtureHome()
	f.Rows[4].Stagings[0] = StagingCell{Name: "void", State: "ci", Count: "128/412"}
	f.Rows[3].Stagings = []StagingCell{{Name: "void", State: "ci", Count: "12/41"}}
	frame := ansi.Strip(frameOnly(factoryFrameOf(f, 140, factoryHomeHeight, true, IconsUnicode, factoryNow)))
	if !strings.Contains(frame, "CI 31%") || strings.Contains(frame, "128/412") {
		t.Errorf("CI 128/412 should read CI 31%% at 140:\n%s", frame)
	}
	if !strings.Contains(frame, "CI 12/41") {
		t.Errorf("CI 12/41 fits nine cells and should stay:\n%s", frame)
	}
	narrow := ansi.Strip(frameOnly(factoryFrameOf(f, 100, factoryHomeHeight, true, IconsUnicode, factoryNow)))
	if !strings.Contains(narrow, "CI 29%") {
		t.Errorf("CI 12/41 should read CI 29%% at 100:\n%s", narrow)
	}
}

func TestFactoryAConflictOutOfTriesIsYellowAndOneWithTriesLeftIsNot(t *testing.T) {
	f := fixtureHome()
	f.Cursor = 3
	conflict := func(f factoryFrame) []string {
		frame, _ := factoryFrameOf(f, 140, factoryHomeHeight, true, IconsUnicode, factoryNow)
		var out []string
		for _, r := range warnRuns(t, frame) {
			if strings.HasPrefix(r, "conflict") {
				out = append(out, r)
			}
		}
		return out
	}
	if got := conflict(f); len(got) != 0 {
		t.Errorf("a conflict with fix tries left is yellow: %q", got)
	}
	f.Rows[0].Stagings[1].OnYou = true
	if got := conflict(f); len(got) != 1 {
		t.Errorf("a conflict out of fix tries should be yellow once, got %q", got)
	}
}

func TestFactoryAShortListScrollsToTheCursorAndCountsWhatIsBelow(t *testing.T) {
	f := fixtureHome()
	f.Cursor = 6
	frame, rowLines := factoryFrameOf(f, 100, 10, true, IconsUnicode, factoryNow)
	lines := strings.Split(ansi.Strip(frame), "\n")
	if len(lines) != 10 {
		t.Fatalf("%d lines, want 10:\n%s", len(lines), frame)
	}
	at, ok := rowLines[f.Rows[6].ID]
	if !ok || !strings.HasPrefix(lines[at], "› ") {
		t.Errorf("the cursor row is not drawn selected (line %d, %v):\n%s", at, ok, frame)
	}
	if len(rowLines) >= len(f.Rows) {
		t.Errorf("all %d rows are said to be on screen in 10 lines: %v", len(rowLines), rowLines)
	}
	if !strings.Contains(lines[3], "WAITS ON") {
		t.Errorf("the column names scrolled away: %q", lines[3])
	}
	if strings.Contains(lines[len(lines)-3], "more") {
		t.Errorf("the cursor is on the last row, so nothing is below: %q", lines[len(lines)-3])
	}
	f.Cursor = 0
	top, _ := factoryFrameOf(f, 100, 10, true, IconsUnicode, factoryNow)
	if rule := strings.Split(ansi.Strip(top), "\n")[7]; !strings.Contains(rule, "▼ 4 more") {
		t.Errorf("the rows below the screen are not counted: %q", rule)
	}
}

func TestFactoryGroupsRowsByWhoTheyWaitOnAndTheCursorFollowsItsRow(t *testing.T) {
	f := fixtureHome()
	slices.Reverse(f.Rows)     // done first, needs-you last
	f.Cursor = len(f.Rows) - 1 // the row that was first: #4512
	frame, rowLines := factoryFrameOf(f, 140, factoryHomeHeight, true, IconsUnicode, factoryNow)
	lines := strings.Split(ansi.Strip(frame), "\n")
	var groups []string
	for _, l := range lines {
		switch strings.TrimSpace(l) {
		case "WAITS ON OTHERS", "DONE TODAY":
			groups = append(groups, strings.TrimSpace(l))
		default:
			if strings.HasPrefix(l, " NEEDS YOU") {
				groups = append(groups, "NEEDS YOU")
			}
		}
	}
	if want := []string{"NEEDS YOU", "WAITS ON OTHERS", "DONE TODAY"}; !slices.Equal(groups, want) {
		t.Errorf("groups = %q, want %q", groups, want)
	}
	if got := lines[rowLines[1]]; !strings.HasPrefix(got, "› ● #4512") {
		t.Errorf("the cursor stayed with its row, but the row reads %q", got)
	}
}

func TestFactoryAnEmptyListSaysNothingWaitsOnYouAndStillDrawsItsFooter(t *testing.T) {
	f := factoryFrame{Repo: "talkable", Hints: []hint{{"n", "new task"}, {"?", "more"}}}
	frame, rowLines := factoryFrameOf(f, 100, 12, true, IconsUnicode, factoryNow)
	plain := ansi.Strip(frame)
	for _, want := range []string{"✓ nothing waits on you", "✓ Nothing waits on you.", "n new task · ? more"} {
		if !strings.Contains(plain, want) {
			t.Errorf("no %q in\n%s", want, plain)
		}
	}
	if len(rowLines) != 0 || strings.Contains(plain, "WAITS ON OTHERS") || strings.Contains(plain, "DONE TODAY") {
		t.Errorf("an empty list draws rows or groups:\n%s", plain)
	}
}

func TestFactoryTextFromTheRegistryIsCleanedBeforeItIsDrawn(t *testing.T) {
	f := fixtureHome()
	f.Rows[0].Title = "Deploy\x1b[31m list\n on campaigns"
	f.Rows[0].WaitsOn[0].What = "reply\x07 to alice"
	f.Flash = "14:02\x1b[2J you said MFA done"
	frame, _ := factoryFrameOf(f, 140, factoryHomeHeight, true, IconsUnicode, factoryNow)
	if strings.Contains(frame, "\x1b[2J") || strings.Contains(frame, "\x1b[31m") || strings.Contains(frame, "\x07") {
		t.Errorf("a control sequence from the data reached the frame: %q", frame)
	}
}

// frameOnly is the frame of a call that also returns the row lines.
func frameOnly(frame string, _ map[int64]int) string { return frame }
