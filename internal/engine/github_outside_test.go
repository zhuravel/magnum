package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
)

// answered has identity's meter hear a GraphQL call that cost points and
// answered used of 5000 in the window resetting at reset.
func (h *harness) answered(identity string, points, used int, reset time.Time) {
	h.e.meterGitHub(identity)(github.Call{Op: opRound, Kind: "details", GraphQL: true,
		Rate: github.RateLimit{Limit: 5000, Cost: points, Used: used, Remaining: 5000 - used, ResetAt: reset}})
}

// windowsOf is identity's windows as e's meter keeps them.
func windowsOf(e *Engine, identity string) []GitHubWindow {
	e.ghMeter.mu.Lock()
	defer e.ghMeter.mu.Unlock()
	c := e.ghMeter.ids[identity]
	if c == nil {
		return nil
	}
	return slices.Clone(c.Windows)
}

// wantWindows fails unless identity's windows are want, oldest reset first.
func (h *harness) wantWindows(identity string, want ...GitHubWindow) {
	h.t.Helper()
	got := windowsOf(h.e, identity)
	if len(got) != len(want) {
		h.t.Fatalf("%s's windows = %+v, want %+v", identity, got, want)
	}
	for i := range want {
		want[i].ResetAt = want[i].ResetAt.UTC()
		if got[i] != want[i] {
			h.t.Fatalf("%s's window %d = %+v, want %+v (all %+v)", identity, i, got[i], want[i], got)
		}
	}
}

// The meter keeps, per identity, each GraphQL window its calls answered in,
// keyed by the window's reset: its limit, the highest used any call saw in
// it (GitHub's used, else limit − remaining) and the points the daemon's
// calls cost in it. What GitHub counts used beyond those points was spent
// outside magnum's calls, before the daemon's first call of the window
// included.
func TestTheMeterKeepsWhatEachWindowSpentOutsideItsCalls(t *testing.T) {
	h := newHarness(t)
	reset := h.clock.Now().Add(40 * time.Minute)
	h.answered("zhuravel", 3, 1203, reset)
	h.answered("zhuravel", 2, 1210, reset)
	h.metered("zhuravel", opRound, 1, 3780, reset) // no used in the answer: 5000 − 3780
	h.wantWindows("zhuravel", GitHubWindow{ResetAt: reset, Limit: 5000, Used: 1220, Points: 6})
	if w := windowsOf(h.e, "zhuravel")[0]; w.Outside() != 1214 {
		t.Errorf("outside = %d, want 1214 (1220 used, 6 by magnum)", w.Outside())
	}
	if w := (GitHubWindow{Used: 10, Points: 12}); w.Outside() != 0 {
		t.Errorf("outside of a window whose calls cost more than it used = %d, want 0", w.Outside())
	}
}

// One login can show two windows at once (on 2026-10-21 the daemon's calls
// landed in windows resetting at 18:29:46Z and 18:31:00Z): calls answered in
// either are kept apart by their reset, whatever order they come in.
func TestTwoWindowsOfOneLoginAreKeptApart(t *testing.T) {
	h := newHarness(t)
	h.clock.Set(time.Date(2026, 10, 21, 17, 40, 0, 0, time.UTC))
	a := time.Date(2026, 10, 21, 18, 29, 46, 0, time.UTC)
	b := time.Date(2026, 10, 21, 18, 31, 0, 0, time.UTC)
	h.answered("zhuravel", 1, 4000, a)
	h.answered("zhuravel", 2, 300, b)
	h.answered("zhuravel", 1, 4002, a)
	h.answered("zhuravel", 2, 305, b)
	h.answered("talkable-app", 1, 40, a) // another identity's window of the same reset
	h.wantWindows("zhuravel",
		GitHubWindow{ResetAt: a, Limit: 5000, Used: 4002, Points: 2},
		GitHubWindow{ResetAt: b, Limit: 5000, Used: 305, Points: 4})
	h.wantWindows("talkable-app", GitHubWindow{ResetAt: a, Limit: 5000, Used: 40, Points: 1})

	// A window with a later reset seen first goes after one seen later.
	h.answered("example-app", 1, 10, b)
	h.answered("example-app", 1, 20, a)
	h.wantWindows("example-app",
		GitHubWindow{ResetAt: a, Limit: 5000, Used: 20, Points: 1},
		GitHubWindow{ResetAt: b, Limit: 5000, Used: 10, Points: 1})
}

// A reset a few seconds off a kept window's is that window, GitHub's own
// wobble (on 2026-10-21 the radar read 47 and 44 points a second apart with
// resets 11:28:10Z and 11:28:11Z): its points and used go to the window,
// which keeps its first reset, so its outside spend is counted once. Up to
// 10 seconds off is one window; resets 51 seconds apart are two.
func TestAResetSecondsOffAKeptWindowsIsThatWindow(t *testing.T) {
	h := newHarness(t)
	a := h.clock.Now().Add(30 * time.Minute)
	h.answered("zhuravel", 2, 4000, a)
	h.answered("zhuravel", 3, 4010, a.Add(time.Second))
	h.answered("zhuravel", 1, 4012, a.Add(-time.Second))
	h.answered("zhuravel", 1, 4013, a.Add(10*time.Second))
	h.answered("zhuravel", 1, 4014, a.Add(-10*time.Second))
	h.answered("zhuravel", 1, 300, a.Add(51*time.Second))
	h.wantWindows("zhuravel",
		GitHubWindow{ResetAt: a, Limit: 5000, Used: 4014, Points: 8},
		GitHubWindow{ResetAt: a.Add(51 * time.Second), Limit: 5000, Used: 300, Points: 1})
	outside := 0
	for _, w := range windowsOf(h.e, "zhuravel") {
		outside += w.Outside()
	}
	if outside != 4006+299 {
		t.Errorf("outside over the windows = %d, want %d: the wobbling window's spend counted once", outside, 4006+299)
	}

	// 11 seconds off is another window.
	h.answered("talkable-app", 1, 100, a)
	h.answered("talkable-app", 1, 110, a.Add(11*time.Second))
	h.wantWindows("talkable-app",
		GitHubWindow{ResetAt: a, Limit: 5000, Used: 100, Points: 1},
		GitHubWindow{ResetAt: a.Add(11 * time.Second), Limit: 5000, Used: 110, Points: 1})
}

// Calls in flight return in any order: a window's used is the highest any
// of them answered, its points their sum, so the order changes nothing.
func TestAWindowIsTheSameWhateverOrderItsCallsReturnIn(t *testing.T) {
	type answer struct{ points, used int }
	calls := []answer{{1, 1005}, {3, 1010}, {2, 1008}}
	orders := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, order := range orders {
		h := newHarness(t)
		reset := h.clock.Now().Add(time.Hour)
		for _, i := range order {
			h.answered("zhuravel", calls[i].points, calls[i].used, reset)
		}
		h.wantWindows("zhuravel", GitHubWindow{ResetAt: reset, Limit: 5000, Used: 1010, Points: 6})
	}
}

// A call no rate limit came back with (one that failed before GraphQL
// answered, one GitHub refused for its rate limit) and a REST call add
// nothing to any window.
func TestACallWithoutARateLimitAddsToNoWindow(t *testing.T) {
	h := newHarness(t)
	h.e.meterGitHub("zhuravel")(github.Call{Op: opRound, Kind: "details", GraphQL: true})                           // failed before GraphQL answered
	h.e.meterGitHub("zhuravel")(github.Call{Op: opRound, Kind: "review threads", GraphQL: true, RateLimited: true}) // refused
	h.metered("zhuravel", opRound, -1, 0, time.Time{})                                                              // REST
	h.wantWindows("zhuravel")
	reset := h.clock.Now().Add(time.Hour)
	h.answered("zhuravel", 2, 50, reset)
	h.e.meterGitHub("zhuravel")(github.Call{Op: opRound, Kind: "details", GraphQL: true})
	h.metered("zhuravel", opRound, -1, 0, time.Time{})
	h.wantWindows("zhuravel", GitHubWindow{ResetAt: reset, Limit: 5000, Used: 50, Points: 2})
}

// The radar's reading (recordRateLimit) is of calls the meter counted
// already: it raises its window's used, never its points, and the next
// flush writes it even when the reading is not the budget to keep.
func TestTheRadarsReadingRaisesUsedWithoutPoints(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	a, b := now.Add(20*time.Minute), now.Add(22*time.Minute)
	h.answered("zhuravel", 2, 4000, a)
	h.e.flushGitHubCost(h.ctx, true)
	h.e.ghMeter.sawRate("zhuravel", github.RateLimit{Limit: 5000, Cost: 7, Used: 4100, Remaining: 900, ResetAt: a}, now)
	h.e.ghMeter.sawRate("zhuravel", github.RateLimit{Limit: 5000, Cost: 3, Used: 3990, Remaining: 1010, ResetAt: a}, now) // lower: no change
	h.e.ghMeter.sawRate("zhuravel", github.RateLimit{Limit: 5000, Cost: 4, Used: 300, Remaining: 4700, ResetAt: b}, now)  // the other window, not the budget kept
	h.wantWindows("zhuravel",
		GitHubWindow{ResetAt: a, Limit: 5000, Used: 4100, Points: 2},
		GitHubWindow{ResetAt: b, Limit: 5000, Used: 300})
	h.e.flushGitHubCost(h.ctx, true)
	c, _ := h.costRecord("zhuravel")
	if len(c.Windows) != 2 || c.Windows[0].Used != 4100 || c.Windows[1].Used != 300 {
		t.Errorf("the written windows = %+v, want the radar's readings", c.Windows)
	}
}

// A window goes once its reset is more than 24 hours old, as the hours do,
// and the next flush writes the record without it.
func TestAWindowIsPruned24HoursAfterItsReset(t *testing.T) {
	h := newHarness(t)
	reset := h.clock.Now().Add(30 * time.Minute)
	h.answered("zhuravel", 1, 100, reset)
	h.advance(30*time.Minute + 24*time.Hour) // exactly 24 hours after the reset: kept
	h.e.flushGitHubCost(h.ctx, true)
	if c, _ := h.costRecord("zhuravel"); len(c.Windows) != 1 {
		t.Fatalf("windows 24 hours after the reset = %+v, want it kept", c.Windows)
	}
	h.advance(time.Minute)
	h.e.flushGitHubCost(h.ctx, false)
	if c, _ := h.costRecord("zhuravel"); len(c.Windows) != 0 {
		t.Errorf("windows more than 24 hours after the reset = %+v, want none", c.Windows)
	}
}

// The windows are written with the cost record (its "windows") and read
// back at the next start, so a restart neither forgets what was spent nor
// counts it again.
func TestTheWindowsOutliveARestart(t *testing.T) {
	h := newHarness(t)
	reset := h.clock.Now().Add(30 * time.Minute)
	h.answered("zhuravel", 4, 1300, reset)
	h.e.flushGitHubCost(h.ctx, true)
	v, _ := kvValue(h, KVGitHubCost("zhuravel"))
	var raw struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(v), &raw); err != nil || len(raw.Windows) != 1 {
		t.Fatalf("the record's windows = %s (%v)", v, err)
	}
	for _, key := range []string{"reset_at", "limit", "used", "points"} {
		if _, ok := raw.Windows[0][key]; !ok {
			t.Errorf("the stored window lacks %q: %s", key, v)
		}
	}
	next := New(h.d)
	next.loadGitHubCost(h.ctx)
	got := windowsOf(next, "zhuravel")
	if len(got) != 1 || got[0] != (GitHubWindow{ResetAt: reset.UTC(), Limit: 5000, Used: 1300, Points: 4}) {
		t.Fatalf("windows after the restart = %+v", got)
	}
	next.meterGitHub("zhuravel")(github.Call{Op: opRound, GraphQL: true,
		Rate: github.RateLimit{Limit: 5000, Cost: 2, Used: 1310, Remaining: 3690, ResetAt: reset}})
	if got := windowsOf(next, "zhuravel"); len(got) != 1 || got[0].Points != 6 || got[0].Used != 1310 {
		t.Errorf("windows after a call since the restart = %+v, want 6 points of 1310 used", got)
	}
}

// eventSubjects are the subjects of the events of kind, oldest first.
func (h *harness) eventSubjects(kind string) []string {
	h.t.Helper()
	rows, err := h.st.DB().QueryContext(h.ctx, "SELECT subject FROM events WHERE kind = ? ORDER BY id", kind)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// The first tick that finds a live window's spend outside magnum at a fifth
// of its limit or more (1,000 of 5,000) says so once: a github.outside_spend
// warning and a toast, remembered in the window (told) and written at once,
// so neither a later tick nor a restart says it again; the next window
// that gets there is told again. Below a fifth nothing is said.
func TestAWindowsOutsideSpendIsToldOnceAtAFifthOfItsLimit(t *testing.T) {
	h := newHarness(t)
	h.startup()
	reset := h.clock.Now().Add(50 * time.Minute)
	h.answered("zhuravel", 10, 1009, reset) // 999 outside
	h.e.meterGitHub("talkable-app")(github.Call{Op: opRound, GraphQL: true,
		Rate: github.RateLimit{Limit: 15000, Cost: 1, Used: 2001, Remaining: 12999, ResetAt: reset}}) // 2,000 of 15,000
	h.tick()
	if ev := h.events("github.outside_spend"); len(ev) != 0 {
		t.Fatalf("github.outside_spend below a fifth of the limit: %+v", ev)
	}
	h.wantToasts("outside magnum", 0)

	h.answered("zhuravel", 1, 1061, reset) // 1,050 outside
	h.tick()
	const want = "GitHub budget of zhuravel: 1,050 points of this window went to calls outside magnum (another tool on this login, or the agents' own gh)"
	ev := h.events("github.outside_spend")
	if len(ev) != 1 || ev[0].Level != "warn" || !strings.Contains(ev[0].Message, want) {
		t.Fatalf("github.outside_spend events = %+v, want one warning saying %q", ev, want)
	}
	if s := h.eventSubjects("github.outside_spend"); len(s) != 1 || s[0] != "github:zhuravel" {
		t.Errorf("subjects = %q, want the identity", s)
	}
	h.wantToasts("GitHub budget of zhuravel: 1,050 points outside magnum", 1)
	if got := toastsWith(h, "points outside magnum |"); len(got) != 1 || !strings.Contains(got[0], " | "+want+". GitHub counts 1,061 of the window's 5,000 points used") {
		t.Errorf("toasts = %q, want the body to say %q", got, want)
	}
	if c, ok := h.costRecord("zhuravel"); !ok || len(c.Windows) != 1 || !c.Windows[0].Told {
		t.Errorf("the record after the notice = %+v, %v; want its window told, written at once", c.Windows, ok)
	}

	h.answered("zhuravel", 1, 2500, reset)
	h.tick()
	next := New(h.d) // a restart within the window
	next.loadGitHubCost(h.ctx)
	next.noteGitHubOutside(h.ctx)
	if n := len(h.events("github.outside_spend")); n != 1 {
		t.Errorf("github.outside_spend events after more spend and a restart = %d, want 1", n)
	}
	h.wantToasts("outside magnum", 1)

	h.advance(51 * time.Minute) // the next window
	h.answered("zhuravel", 2, 1500, h.clock.Now().Add(time.Hour))
	h.tick()
	if n := len(h.events("github.outside_spend")); n != 2 {
		t.Errorf("github.outside_spend events = %d, want a second for the next window", n)
	}
	h.wantToasts("outside magnum", 2)
}

// A fifth is of the window's own limit (an App's installation has more than
// 5,000 points), reached at exactly a fifth, and a window that has reset is
// never told, however much went outside magnum in it.
func TestTheOutsideNoticeIsAtAFifthOfTheWindowsOwnLimit(t *testing.T) {
	h := newHarness(t)
	h.startup()
	now := h.clock.Now()
	app := func(points, used int, reset time.Time) {
		h.e.meterGitHub("talkable-app")(github.Call{Op: opRound, GraphQL: true,
			Rate: github.RateLimit{Limit: 15000, Cost: points, Used: used, Remaining: 15000 - used, ResetAt: reset}})
	}
	app(1, 3000, now.Add(time.Minute))  // 2,999 of 15,000
	app(1, 4000, now.Add(-time.Minute)) // a window that has reset
	h.tick()
	if ev := h.events("github.outside_spend"); len(ev) != 0 {
		t.Fatalf("github.outside_spend below a fifth or for a window that reset: %+v", ev)
	}
	app(0, 3001, now.Add(time.Minute)) // 3,000 of 15,000: a fifth
	h.tick()
	if ev := h.events("github.outside_spend"); len(ev) != 1 || !strings.Contains(ev[0].Message, "GitHub budget of talkable-app: 3,000 points of this window") {
		t.Fatalf("github.outside_spend events = %+v, want one at a fifth of 15,000", ev)
	}
}
