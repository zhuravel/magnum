package tui

import (
	"context"
	"errors"
	"fmt"
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
		r.UpdatedAt = boardNow.Add(-time.Duration(i) * 17 * time.Minute)
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
	m := newDashboardModel(context.Background(), src, &fakeActions{}, DashboardOptions{Now: func() time.Time { return dashNow }})
	n, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	n, _ = n.Update(dashDataMsg{data: src.data})
	return n.(dashboardModel)
}

// storm feeds msg n times, rendering after each, the way a wheel or
// trackpad swipe reaches the screen: down keys with the mouse off (iTerm2
// turns the wheel into arrow keys), wheel events with it on.
func storm(m tea.Model, n int, msg tea.Msg) (tea.Model, time.Duration) {
	start := time.Now()
	for range n {
		m, _ = m.Update(msg)
		_ = m.View()
	}
	return m, time.Since(start)
}

func TestWheelStormStaysCheap(t *testing.T) {
	for _, c := range []struct {
		name string
		m    tea.Model
	}{{"board", stormBoard(t, 200)}, {"dashboard", stormDash(t, 200)}} {
		for _, ev := range []struct {
			name string
			msg  tea.Msg
		}{{"down keys", keyMsg("down")}, {"wheel events", tea.MouseWheelMsg{X: 20, Y: 20, Button: tea.MouseWheelDown}}} {
			_, d := storm(c.m, 5000, ev.msg)
			t.Logf("%s: 5000 %s, each rendered: %v", c.name, ev.name, d)
			if limit := raceSlowdown * time.Second; d > limit {
				t.Errorf("%s: 5000 %s took %v, want well under %v", c.name, ev.name, d, limit)
			}
		}
	}
}

// Wheel events up and down the whole board, each notch moving every row
// on screen, still reuse the layout and the drawn rows: a frame that
// measured the rows again would take this past its limit many times over.
func TestWheelEventsUpAndDownStayCheap(t *testing.T) {
	m := tea.Model(stormBoard(t, 200))
	down, up := tea.MouseWheelMsg{Button: tea.MouseWheelDown}, tea.MouseWheelMsg{Button: tea.MouseWheelUp}
	start := time.Now()
	for i := range 1000 {
		msg := down
		if (i/60)%2 == 1 {
			msg = up
		}
		m, _ = m.Update(msg)
		_ = m.View()
	}
	d := time.Since(start)
	t.Logf("1000 wheel events up and down, each rendered: %v", d)
	if limit := raceSlowdown * time.Second; d > limit {
		t.Errorf("1000 wheel events took %v, want well under %v", d, limit)
	}
}

func BenchmarkBoardFrameMoving(b *testing.B) {
	m := stormBoard(b, 200)
	up, down := keyMsg("k"), keyMsg("j")
	b.ResetTimer()
	for i := range b.N {
		k := down
		if (i/150)%2 == 1 {
			k = up
		}
		n, _ := m.Update(k)
		m = n.(prBoardModel)
		_ = m.View()
	}
}

func BenchmarkBoardFrameAtBottom(b *testing.B) {
	m := stormBoard(b, 200)
	n, _ := m.Update(keyMsg("G"))
	m = n.(prBoardModel)
	down := keyMsg("j")
	b.ResetTimer()
	for range b.N {
		n, _ := m.Update(down)
		m = n.(prBoardModel)
		_ = m.View()
	}
}

func BenchmarkDashboardFrameMoving(b *testing.B) {
	m := stormDash(b, 200)
	up, down := keyMsg("k"), keyMsg("j")
	b.ResetTimer()
	for i := range b.N {
		k := down
		if (i/150)%2 == 1 {
			k = up
		}
		n, _ := m.Update(k)
		m = n.(dashboardModel)
		_ = m.View()
	}
}

func BenchmarkDashboardFrameAtBottom(b *testing.B) {
	m := stormDash(b, 200)
	n, _ := m.Update(keyMsg("end"))
	m = n.(dashboardModel)
	down := keyMsg("j")
	b.ResetTimer()
	for range b.N {
		n, _ := m.Update(down)
		m = n.(dashboardModel)
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
		{name: "ask", msgs: keys("x"), want: "y/N"},
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
		{name: "ask", msgs: keys("x"), want: "y/N"},
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
	d := newDashboardModel(context.Background(), &fakeSource{}, nil, DashboardOptions{Now: clock})
	next, _ := d.Update(dashDataMsg{data: dashData()})
	d = next.(dashboardModel)
	mustContain(t, viewOf(d), "updated 3s ago")
	now = now.Add(time.Minute)
	mustContain(t, viewOf(d), "updated 1m")

	b := newPRBoardModel(context.Background(), &fakeBoardSource{}, nil, PRBoardOptions{Now: clock})
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
