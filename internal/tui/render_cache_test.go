package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// synthBoardRows are n board rows shaped like real ones: several owners,
// reviewers with verdicts, labels and deltas, so a frame costs what a busy
// board costs.
func synthBoardRows(n int) []PRBoardRow {
	base := boardRows()
	rows := make([]PRBoardRow, n)
	for i := range rows {
		r := base[i%len(base)]
		r.Number = 1000 + i
		r.Owner = []string{"talkable", "example"}[i%2]
		r.Repo = []string{"talkable", "widgets", "site"}[i%3]
		r.Ref = fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number)
		r.URL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", r.Owner, r.Repo, r.Number)
		r.Title = fmt.Sprintf("%s (part %d)", r.Title, i)
		r.ActivityAt = boardNow.Add(-time.Duration(i) * 17 * time.Minute)
		rows[i] = r
	}
	return rows
}

// synthStatus is dashData with n queued PRs.
func synthStatus(n int) StatusData {
	d := dashData()
	d.Queue = make([]PRRow, n)
	for i := range d.Queue {
		d.Queue[i] = PRRow{
			Ref: fmt.Sprintf("talkable#%d", 2000+i), Title: fmt.Sprintf("Change number %d to the referral flow", i),
			Author: "@ann", State: "queued", Next: "review when quiet", Age: fmt.Sprintf("%dm", i%60),
			URL: fmt.Sprintf("https://github.com/talkable/talkable/pull/%d", 2000+i),
		}
	}
	return d
}

func stormBoard(t testing.TB, rows int) prBoardModel {
	m, src, _ := newBoard(t, 120, 40, PRBoardOptions{})
	src.rows = synthBoardRows(rows)
	n, _ := m.Update(prbDataMsg{rows: src.rows})
	return n.(prBoardModel)
}

func stormDash(t testing.TB, rows int) dashboardModel {
	src := &fakeSource{data: synthStatus(rows)}
	m := testDashboard(context.Background(), src, &fakeActions{}, DashboardOptions{Now: func() time.Time { return dashNow }})
	n, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	n, _ = n.Update(dashDataMsg{data: src.data})
	return n.(dashboardModel)
}

// parts is what a partCache holds at one moment: its key and its map. While
// both stay, the cache built only the parts it lacked; a new key starts a
// new map, and every part is built again.
type parts[K comparable] struct {
	key K
	m   uintptr
	n   int
}

func partsOf[K, P comparable, V any](c *partCache[K, P, V]) parts[K] {
	c.mu.Lock()
	defer c.mu.Unlock()
	return parts[K]{key: c.key, m: reflect.ValueOf(c.parts).Pointer(), n: len(c.parts)}
}

// measures is what a screen's caches hold: the rows measured for the
// column widths and the layout (board), the rows drawn (board), the body
// and the header (dashboard). A frame that measures or draws every row again
// changes one of them by more than the rows it adds.
type measures struct {
	natural, layout, summary, rows parts[prbRowsKey]
	body                           parts[dashBodyKey]
	header                         parts[dashHeaderKey]
}

func measuresOf(m tea.Model) measures {
	switch m := m.(type) {
	case prBoardModel:
		c := m.cache
		return measures{natural: partsOf(&c.natural), layout: partsOf(&c.layout), summary: partsOf(&c.summary), rows: partsOf(&c.rows)}
	case dashboardModel:
		return measures{body: partsOf(&m.cache.body), header: partsOf(&m.cache.header)}
	}
	panic(fmt.Sprintf("measuresOf(%T)", m))
}

// sameMeasures reports whether after holds what before held, plus drawn
// rows at most: no part was measured or drawn again.
func sameMeasures(before, after measures) bool {
	rows := after.rows
	rows.n = before.rows.n
	after.rows = rows
	return before == after
}

// storm feeds msg n times, rendering after each, the way a wheel or
// trackpad swipe reaches the screen: down keys with the mouse off (iTerm2
// turns the wheel into arrow keys), wheel events with it on. It fails t
// when a frame measured or drew again what an earlier one had, and returns
// the rows drawn in all.
func storm(t *testing.T, m tea.Model, n int, msg func(i int) tea.Msg) int {
	t.Helper()
	_ = m.View()
	first := measuresOf(m)
	start := time.Now()
	for i := range n {
		m, _ = m.Update(msg(i))
		_ = m.View()
		if now := measuresOf(m); !sameMeasures(first, now) {
			t.Fatalf("event %d measured or drew the screen again: caches %+v, before %+v", i, now, first)
		}
	}
	t.Logf("%d events, each rendered: %v", n, time.Since(start))
	return measuresOf(m).rows.n
}

// A storm of down keys or wheel events reuses what the first frame
// measured and drew: the column widths, the layout, the summary and the
// dashboard's body and header; each row of the board is drawn at most once
// with the cursor on it and once without.
func TestWheelStormStaysCheap(t *testing.T) {
	for _, c := range []struct {
		name string
		m    func() tea.Model
	}{{"board", func() tea.Model { return stormBoard(t, 200) }}, {"dashboard", func() tea.Model { return stormDash(t, 200) }}} {
		for _, ev := range []struct {
			name string
			msg  tea.Msg
		}{{"down keys", keyMsg("down")}, {"wheel events", tea.MouseWheelMsg{X: 20, Y: 20, Button: tea.MouseWheelDown}}} {
			t.Run(c.name+" "+ev.name, func(t *testing.T) {
				if drawn := storm(t, c.m(), 5000, func(int) tea.Msg { return ev.msg }); drawn > 2*200 {
					t.Errorf("%d rows drawn for a board of 200", drawn)
				}
			})
		}
	}
}

// Wheel events up and down the whole board, each notch moving every row
// on screen, still reuse the layout and the drawn rows: no frame measures
// the rows again, and each row is drawn at most once with the cursor on it
// and once without.
func TestWheelEventsUpAndDownStayCheap(t *testing.T) {
	down, up := tea.MouseWheelMsg{Button: tea.MouseWheelDown}, tea.MouseWheelMsg{Button: tea.MouseWheelUp}
	drawn := storm(t, stormBoard(t, 200), 1000, func(i int) tea.Msg {
		if (i/60)%2 == 1 {
			return up
		}
		return down
	})
	if drawn == 0 || drawn > 2*200 {
		t.Errorf("%d rows drawn for a board of 200, want some, each at most twice", drawn)
	}
}

// The frame benchmarks draw one frame before the loop, so they time the
// frames after it (what a key storm costs); the Cold ones time a first frame,
// every cache empty.

func BenchmarkBoardFrameMoving(b *testing.B) {
	m := stormBoard(b, 200)
	_ = m.View()
	up, down := keyMsg("k"), keyMsg("j")
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		k := down
		if (i/150)%2 == 1 {
			k = up
		}
		i++
		n, _ := m.Update(k)
		m = n.(prBoardModel)
		_ = m.View()
	}
}

func BenchmarkBoardFrameAtBottom(b *testing.B) {
	m := stormBoard(b, 200)
	n, _ := m.Update(keyMsg("G"))
	m = n.(prBoardModel)
	_ = m.View()
	down := keyMsg("j")
	b.ReportAllocs()
	for b.Loop() {
		n, _ := m.Update(down)
		m = n.(prBoardModel)
		_ = m.View()
	}
}

// BenchmarkBoardSpinnerFrame is one frame of the reviewing pills' spinner
// on a board of 200 rows, some of them in review.
func BenchmarkBoardSpinnerFrame(b *testing.B) {
	m := stormBoard(b, 200)
	if !m.working {
		b.Fatal("no row of the board is in review")
	}
	_ = m.View()
	b.ReportAllocs()
	for b.Loop() {
		n, _ := m.Update(prbAnimMsg{})
		m = n.(prBoardModel)
		_ = m.View()
	}
}

func BenchmarkBoardFrameCold(b *testing.B) {
	m := stormBoard(b, 200)
	b.ReportAllocs()
	for b.Loop() {
		m.cache = &prbCache{}
		_ = m.View()
	}
}

func BenchmarkDashboardFrameMoving(b *testing.B) {
	m := stormDash(b, 200)
	_ = m.View()
	up, down := keyMsg("k"), keyMsg("j")
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		k := down
		if (i/150)%2 == 1 {
			k = up
		}
		i++
		n, _ := m.Update(k)
		m = n.(dashboardModel)
		_ = m.View()
	}
}

func BenchmarkDashboardFrameAtBottom(b *testing.B) {
	m := stormDash(b, 200)
	n, _ := m.Update(keyMsg("end"))
	m = n.(dashboardModel)
	_ = m.View()
	down := keyMsg("j")
	b.ReportAllocs()
	for b.Loop() {
		n, _ := m.Update(down)
		m = n.(dashboardModel)
		_ = m.View()
	}
}

func BenchmarkDashboardFrameCold(b *testing.B) {
	m := stormDash(b, 200)
	b.ReportAllocs()
	for b.Loop() {
		m.cache = &dashCache{}
		_ = m.View()
	}
}

// uncachedBoard and uncachedDash draw m's frame with no cache at all.
func uncachedBoard(m prBoardModel) string  { m.cache = nil; return m.render() }
func uncachedDash(m dashboardModel) string { m.cache = nil; return m.render() }

// step is one change to a screen and what the frame must then show.
type step struct {
	name string
	msgs []tea.Msg
	want string // the new frame contains it; "" checks only that it changed
	same bool   // the change must leave the frame as it was
}

// runSteps applies each step and checks that the cached frame is the one
// an uncached render draws, and that it changed (or not) as expected.
func runSteps[M tea.Model](t *testing.T, m M, fresh func(M) string, steps []step) {
	t.Helper()
	before := m.View().Content
	for _, s := range steps {
		for _, msg := range s.msgs {
			next, _ := m.Update(msg)
			m = next.(M)
		}
		got := m.View().Content
		if want := fresh(m); got != want {
			t.Fatalf("%s: the cached frame differs from a fresh one:\n%s\n---\n%s", s.name, got, want)
		}
		if s.same != (got == before) {
			t.Errorf("%s: frame changed = %v, want %v", s.name, got != before, !s.same)
		}
		if s.want != "" && !strings.Contains(viewOf(m), s.want) {
			t.Errorf("%s: frame lacks %q", s.name, s.want)
		}
		before = got
	}
}

func TestBoardFrameCacheFollowsEveryChange(t *testing.T) {
	m := stormBoard(t, 60)
	renamed := synthBoardRows(60)
	for i := range renamed {
		renamed[i].Title = "Renamed " + renamed[i].Title
	}
	steps := []step{
		{name: "cursor down", msgs: keys("j")},
		{name: "cursor up", msgs: keys("k")},
		{name: "same key at the top", msgs: keys("k"), same: true},
		{name: "bottom", msgs: keys("G")},
		{name: "down at the bottom", msgs: keys("j"), same: true},
		{name: "data refresh", msgs: []tea.Msg{prbDataMsg{rows: renamed}}, want: "Renamed"},
		{name: "resize", msgs: []tea.Msg{tea.WindowSizeMsg{Width: 90, Height: 30}}},
		{name: "details", msgs: keys("enter")},
		{name: "card scroll", msgs: keys("j")},
		{name: "back", msgs: keys("esc")},
		{name: "help", msgs: keys("?"), want: "close help"},
		{name: "help scroll", msgs: keys("j")},
		{name: "close help", msgs: keys("?")},
		{name: "sort", msgs: keys("s")},
		{name: "sort direction", msgs: keys("S")},
		{name: "filter", msgs: keys("/")},
		{name: "filter edit", msgs: typed("tal"), want: "tal"},
		{name: "keep filter", msgs: keys("enter")},
		{name: "clear filter", msgs: keys("esc")},
		{name: "load error", msgs: []tea.Msg{prbDataMsg{err: errors.New("registry locked")}}, want: "registry locked"},
		{name: "ask", msgs: keys("I"), want: "y/N"},
		{name: "cancel", msgs: keys("n"), want: "cancelled"},
		{name: "flash", msgs: []tea.Msg{actionDoneMsg{what: "pin", text: "pinned it"}}, want: "pinned it"},
		{name: "another flash", msgs: []tea.Msg{actionDoneMsg{what: "unpin", text: "unpinned it"}}, want: "unpinned it"},
		{name: "stale flash timer", msgs: []tea.Msg{flashExpireMsg{seq: -1}}, same: true},
		{name: "owner", msgs: keys("O"), want: "owner: example"},
		{name: "next owner", msgs: keys("O"), want: "owner: talkable"},
		{name: "every owner", msgs: keys("O"), want: "owner: all"},
	}
	runSteps(t, m, uncachedBoard, steps)
}

func TestDashboardFrameCacheFollowsEveryChange(t *testing.T) {
	m := stormDash(t, 60)
	d := synthStatus(60)
	d.Queue[len(d.Queue)-1].Title = "Renamed referral flow"
	steps := []step{
		{name: "cursor down", msgs: keys("j")},
		{name: "cursor up", msgs: keys("k")},
		{name: "same key at the top", msgs: keys("k"), same: true},
		{name: "end", msgs: keys("end")},
		{name: "data refresh", msgs: []tea.Msg{dashDataMsg{data: d}}, want: "Renamed referral flow"},
		{name: "home", msgs: keys("home")},
		{name: "resize", msgs: []tea.Msg{tea.WindowSizeMsg{Width: 90, Height: 20}}},
		{name: "manual", msgs: keys("w")}, // below the screen: the line count changes
		{name: "help", msgs: keys("?"), want: "Keys"},
		{name: "help scroll", msgs: keys("j")},
		{name: "close help", msgs: keys("?")},
		{name: "load error", msgs: []tea.Msg{dashDataMsg{err: errors.New("registry locked")}}, want: "registry locked"},
		{name: "ask", msgs: keys("M"), want: "y/N"},
		{name: "cancel", msgs: keys("n"), want: "cancelled"},
		{name: "flash", msgs: []tea.Msg{actionDoneMsg{what: "pin", text: "pinned it"}}, want: "pinned it"},
		{name: "another flash", msgs: []tea.Msg{actionDoneMsg{what: "unpin", text: "unpinned it"}}, want: "unpinned it"},
	}
	runSteps(t, m, uncachedDash, steps)
}

// TestFrameCacheFollowsTheClock: ages and "updated 3s ago" move with the
// clock even when nothing else changes.
func TestFrameCacheFollowsTheClock(t *testing.T) {
	now := dashNow
	clock := func() time.Time { return now }
	d := testDashboard(context.Background(), &fakeSource{}, nil, DashboardOptions{Now: clock})
	next, _ := d.Update(dashDataMsg{data: dashData()})
	d = next.(dashboardModel)
	mustContain(t, viewOf(d), "updated 3s ago")
	now = now.Add(time.Minute)
	mustContain(t, viewOf(d), "updated 1m")

	b := testPRBoard(context.Background(), &fakeBoardSource{}, nil, PRBoardOptions{Now: clock})
	next, _ = b.Update(prbDataMsg{rows: boardRows()})
	b = next.(prBoardModel)
	v := viewOf(b)
	now = now.Add(48 * time.Hour)
	if viewOf(b) == v {
		t.Error("the board's ages did not move with the clock")
	}
}

// TestModelCopiesNeverShareFrames: two copies of one model given
// different data must each show their own.
func TestModelCopiesNeverShareFrames(t *testing.T) {
	base := stormBoard(t, 5)
	one, two := synthBoardRows(5), synthBoardRows(5)
	one[0].Title, two[0].Title = "first copy", "second copy"
	a, _ := base.Update(prbDataMsg{rows: one})
	b, _ := base.Update(prbDataMsg{rows: two})
	mustContain(t, viewOf(a), "first copy")
	mustContain(t, viewOf(b), "second copy")
	mustContain(t, viewOf(a), "first copy")
}

// A frame of the reviewing pills' spinner draws again only the rows on
// screen whose pill spins, and the summary (whose reviewing count spins in
// the nerd mode): the column widths, the layout and every other drawn row
// are reused, and each frame is the one an uncached render draws.
func TestASpinnerFrameRedrawsOnlyTheSpinningRows(t *testing.T) {
	for _, mode := range []IconMode{IconsUnicode, IconsNerd} {
		t.Run(string(mode), func(t *testing.T) {
			m, src, _ := newBoard(t, 120, 40, PRBoardOptions{Icons: mode})
			src.rows = synthBoardRows(200)
			n, _ := m.Update(prbDataMsg{rows: src.rows})
			m = n.(prBoardModel)
			if !m.working {
				t.Fatal("no row of the board is in review")
			}
			start := min(m.scroll, max(len(m.view)-1, 0))
			end, _ := m.visible(start)
			spinning := 0
			for _, r := range m.view[start:end] {
				if workingState(rowState(r)) {
					spinning++
				}
			}
			if spinning == 0 {
				t.Fatal("no row on screen is in review")
			}
			prev := m.View().Content
			first := measuresOf(m)
			for i := range len(m.g.working) + 1 {
				before := measuresOf(m)
				n, _ := m.Update(prbAnimMsg{})
				m = n.(prBoardModel)
				got := m.View().Content
				if want := uncachedBoard(m); got != want {
					t.Fatalf("frame %d differs from a fresh one:\n%s\n---\n%s", i, got, want)
				}
				if got == prev {
					t.Fatalf("frame %d: the spinner did not move", i)
				}
				prev = got
				now := measuresOf(m)
				if now.natural != first.natural || now.layout != first.layout {
					t.Fatalf("frame %d measured the rows again: %+v, before %+v", i, now, first)
				}
				if now.rows.key != first.rows.key || now.rows.m != first.rows.m || now.summary.key != first.summary.key || now.summary.m != first.summary.m {
					t.Fatalf("frame %d emptied the rows' or the summary's cache: %+v, before %+v", i, now, first)
				}
				if drawn := now.rows.n - before.rows.n; i < len(m.g.working)-1 && drawn != spinning {
					t.Errorf("frame %d drew %d rows, want the %d whose pill spins", i, drawn, spinning)
				}
			}
		})
	}
}
