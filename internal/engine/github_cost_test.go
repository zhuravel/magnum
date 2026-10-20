package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
)

// costRecord is identity's cost record as the store holds it (ok false:
// none written).
func (h *harness) costRecord(identity string) (GitHubCost, bool) {
	h.t.Helper()
	v, ok := kvValue(h, KVGitHubCost(identity))
	if !ok {
		return GitHubCost{}, false
	}
	c, parsed := ParseGitHubCost(v)
	if !parsed {
		h.t.Fatalf("cost record %q does not parse", v)
	}
	return c, true
}

// metered has identity's meter hear one call of op: a GraphQL call that
// cost points with remaining of 5000 left until reset, or a REST call when
// points is negative.
func (h *harness) metered(identity, op string, points, remaining int, reset time.Time) {
	call := github.Call{Op: op, Kind: "details", GraphQL: points >= 0}
	if call.GraphQL {
		call.Rate = github.RateLimit{Limit: 5000, Cost: points, Remaining: remaining, ResetAt: reset}
	}
	h.e.meterGitHub(identity)(call)
}

// The meter adds every call up per identity, per op and per clock hour: a
// GraphQL call its points, a REST call one request, a call whose context
// carries no op under its own kind; the record keeps the latest rate limit,
// whatever order calls in flight return in.
func TestTheMeterAddsCallsUpPerIdentityOpAndHour(t *testing.T) {
	h := newHarness(t)
	start := h.clock.Now() // 10:00 local
	reset := start.Add(40 * time.Minute)
	h.metered("zhuravel", opRoundSetup, 12, 4900, reset)
	h.metered("zhuravel", opRoundSetup, 8, 4892, reset)
	h.metered("zhuravel", opRoundSettle, 5, 4880, reset)
	h.metered("zhuravel", opRoundSetup, 3, 4895, reset) // an earlier answer returning late: the rate stays 4880
	h.metered("zhuravel", "", -1, 0, time.Time{})       // REST, no op: its kind
	h.metered("talkable-app", opRound, 2, 4998, reset)
	h.advance(time.Hour)
	h.metered("zhuravel", "radar", 1, 4999, reset.Add(time.Hour)) // the next window
	h.e.flushGitHubCost(h.ctx, true)

	c, ok := h.costRecord("zhuravel")
	if !ok {
		t.Fatal("no cost record for zhuravel")
	}
	if len(c.Hours) != 2 {
		t.Fatalf("hours = %+v, want the two clock hours", c.Hours)
	}
	first := c.Hours[0]
	if !first.Hour.Equal(start.UTC().Truncate(time.Hour)) || first.Points != 28 || first.Calls != 5 || first.RESTCalls != 1 {
		t.Errorf("first hour = %+v", first)
	}
	if got := first.Ops[opRoundSetup]; got != (GitHubOpCost{Points: 23, Calls: 3}) {
		t.Errorf("round.setup = %+v, want 23 points in 3 calls", got)
	}
	if got := first.Ops["details"]; got != (GitHubOpCost{Calls: 1, RESTCalls: 1}) {
		t.Errorf("the unlabelled REST call = %+v, want one REST call under its kind", got)
	}
	if c.Rate == nil || c.Rate.Remaining != 4999 || !c.Rate.ResetAt.Equal(reset.Add(time.Hour).UTC()) {
		t.Errorf("rate = %+v, want the next window's 4999", c.Rate)
	}
	total, ops := c.Since(first.Hour)
	if total.Points != 29 || len(ops) != 4 || ops[0].Op != opRoundSetup || ops[1].Op != opRoundSettle {
		t.Errorf("since the first hour: %+v %+v", total, ops)
	}
	app, ok := h.costRecord("talkable-app")
	if !ok || len(app.Hours) != 1 || app.Hours[0].Ops[opRound].Points != 2 {
		t.Errorf("talkable-app = %+v, %v", app, ok)
	}

	// The rate within one window: the lowest remaining is the latest.
	h2 := newHarness(t)
	r := h2.clock.Now().Add(time.Hour)
	h2.metered("zhuravel", opRound, 1, 4000, r)
	h2.metered("zhuravel", opRound, 1, 4100, r)
	h2.e.flushGitHubCost(h2.ctx, true)
	if c, _ := h2.costRecord("zhuravel"); c.Rate == nil || c.Rate.Remaining != 4000 {
		t.Errorf("rate = %+v, want 4000: a higher remaining in the same window is an older answer", c.Rate)
	}
}

// costWrites counts the cost records the meter wrote.
func (h *harness) costWrites() int {
	h.e.ghMeter.mu.Lock()
	defer h.e.ghMeter.mu.Unlock()
	return h.e.ghMeter.writes
}

// lowRun is identity's run of low budget as the store holds it (ok false:
// none).
func (h *harness) lowRun(identity string) (GitHubLow, bool) {
	h.t.Helper()
	v, ok := kvValue(h, KVGitHubLow(identity))
	if !ok {
		return GitHubLow{}, false
	}
	l, parsed := ParseGitHubLow(v)
	if !parsed {
		h.t.Fatalf("low run %q does not parse", v)
	}
	return l, true
}

// The meter writes a record at most once a minute, and only with calls the
// store has not seen; the hours older than the last 24 go.
func TestTheMeterWritesAtMostOnceAMinuteAndKeeps24Hours(t *testing.T) {
	h := newHarness(t)
	reset := h.clock.Now().Add(time.Hour)
	h.metered("zhuravel", opRound, 1, 4000, reset)
	h.e.flushGitHubCost(h.ctx, false)
	if n := h.costWrites(); n != 1 {
		t.Fatalf("writes after the first flush = %d, want 1", n)
	}
	h.metered("zhuravel", opRound, 1, 3999, reset) // the same value would be written again: the count tells
	h.advance(30 * time.Second)
	h.e.flushGitHubCost(h.ctx, false)
	if n := h.costWrites(); n != 1 {
		t.Fatalf("writes within the minute = %d, want 1", n)
	}
	h.advance(31 * time.Second)
	h.e.flushGitHubCost(h.ctx, false)
	if n := h.costWrites(); n != 2 {
		t.Fatalf("writes after the minute = %d, want 2", n)
	}
	for range 3 { // nothing new: no write
		h.advance(2 * time.Minute)
		h.e.flushGitHubCost(h.ctx, false)
	}
	if n := h.costWrites(); n != 2 {
		t.Errorf("writes without new calls = %d, want 2", n)
	}

	for i := range 26 {
		h.advance(time.Hour)
		h.metered("zhuravel", opRound, 1, 4000, h.clock.Now().Add(time.Hour))
		if i%5 == 0 {
			h.e.flushGitHubCost(h.ctx, false)
		}
	}
	h.e.flushGitHubCost(h.ctx, false)
	c, ok := h.costRecord("zhuravel")
	if !ok || len(c.Hours) != 24 {
		t.Fatalf("hours = %d (%v), want the last 24", len(c.Hours), ok)
	}
	if want := h.clock.Now().UTC().Truncate(time.Hour).Add(-23 * time.Hour); !c.Hours[0].Hour.Equal(want) {
		t.Errorf("oldest hour = %s, want %s", c.Hours[0].Hour, want)
	}
}

// An identity that makes no more calls has its record written again
// without the hours that fell out of the last 24, by the next flush.
func TestAnIdleIdentitysExpiredHoursGo(t *testing.T) {
	h := newHarness(t)
	h.metered("talkable-app", opRound, 3, 4900, h.clock.Now().Add(time.Hour))
	h.advance(2 * time.Hour)
	h.metered("talkable-app", opRound, 2, 4900, h.clock.Now().Add(time.Hour))
	h.e.flushGitHubCost(h.ctx, false)
	if c, _ := h.costRecord("talkable-app"); len(c.Hours) != 2 {
		t.Fatalf("hours = %+v, want two", c.Hours)
	}
	h.advance(23 * time.Hour) // the first hour is out of the last 24, the second still in
	h.e.flushGitHubCost(h.ctx, false)
	c, _ := h.costRecord("talkable-app")
	if len(c.Hours) != 1 || c.Hours[0].Points != 2 {
		t.Fatalf("hours of the idle identity = %+v, want only the second", c.Hours)
	}
	h.advance(2 * time.Hour)
	h.e.flushGitHubCost(h.ctx, false)
	if c, _ := h.costRecord("talkable-app"); len(c.Hours) != 0 {
		t.Errorf("hours = %+v, want none left", c.Hours)
	}
}

// A call stamped before the newest hour (a clock that stepped back) counts
// in its own hour, made in its place when the record has none.
func TestACallOfAnEarlierHourCountsInItsOwnHour(t *testing.T) {
	h := newHarness(t)
	reset := h.clock.Now().Add(2 * time.Hour)
	h.advance(30 * time.Minute) // 10:30
	h.metered("zhuravel", opRound, 1, 4000, reset)
	h.advance(40 * time.Minute) // 11:10
	h.metered("zhuravel", opRound, 2, 3999, reset)
	h.advance(-20 * time.Minute) // 10:50: the clock stepped back
	h.metered("zhuravel", opRound, 4, 3998, reset)
	h.advance(-90 * time.Minute) // 09:20
	h.metered("zhuravel", opRound, 8, 3997, reset)
	h.e.flushGitHubCost(h.ctx, true)
	c, _ := h.costRecord("zhuravel")
	if len(c.Hours) != 3 {
		t.Fatalf("hours = %+v, want 09, 10 and 11", c.Hours)
	}
	for i, want := range []int{8, 5, 2} {
		if c.Hours[i].Points != want {
			t.Errorf("hour %s: %d points, want %d (hours %+v)", c.Hours[i].Hour, c.Hours[i].Points, want, c.Hours)
		}
	}
	if !c.Hours[0].Hour.Before(c.Hours[1].Hour) || !c.Hours[1].Hour.Before(c.Hours[2].Hour) {
		t.Errorf("hours out of order: %+v", c.Hours)
	}
}

// A record whose write fails (here the stop's cancelled context) stays to
// be written: the stop's own flush writes it, with no call since to mark it
// again.
func TestACostRecordWhoseWriteFailedIsWrittenByTheNextFlush(t *testing.T) {
	h := newHarness(t)
	reset := h.clock.Now().Add(time.Hour)
	h.metered("zhuravel", opRound, 5, 4000, reset)
	stopped, cancel := context.WithCancel(h.ctx)
	cancel()
	h.e.flushGitHubCost(stopped, false)
	if _, ok := h.costRecord("zhuravel"); ok {
		t.Fatal("a write on a cancelled context took")
	}
	h.e.flushGitHubCost(context.Background(), true) // the stop's flush, with no call since
	c, ok := h.costRecord("zhuravel")
	if !ok || len(c.Hours) != 1 || c.Hours[0].Points != 5 {
		t.Fatalf("the record after the stop's flush = %+v, %v; want the call the failed write had", c.Hours, ok)
	}
	h.metered("zhuravel", opRound, 2, 3999, reset)
	h.e.flushGitHubCost(context.Background(), true)
	if c, _ := h.costRecord("zhuravel"); c.Hours[0].Points != 7 {
		t.Fatalf("the record after a later call = %+v; want both calls", c.Hours)
	}
}

// A tick writes the meter's records, and the next start reads them back:
// the hours and the latest rate limit outlive a restart; the rows of an
// identity the config no longer declares go.
func TestTheMeterOutlivesARestart(t *testing.T) {
	h := newHarness(t)
	reset := h.clock.Now().Add(time.Hour)
	h.metered("zhuravel", opFixExternal, 40, 4100, reset)
	h.tick()
	if _, ok := h.costRecord("zhuravel"); !ok {
		t.Fatal("the tick wrote no cost record")
	}
	h.e.setKV(h.ctx, KVGitHubCost("example-app"), `{"identity":"example-app","hours":[]}`) // an identity the config dropped
	h.e.setKV(h.ctx, KVGitHubLow("example-app"), `{"remaining":1,"floor":500,"until":"2026-10-05T11:00:00Z"}`)
	next := New(h.d) // the restart: a new engine over the same registry
	next.loadGitHubCost(h.ctx)
	for _, key := range []string{KVGitHubCost("example-app"), KVGitHubLow("example-app")} {
		if v, ok := kvValue(h, key); ok {
			t.Errorf("%s of an identity the config no longer declares stays: %s", key, v)
		}
	}
	next.meterGitHub("zhuravel")(github.Call{Op: opFix, GraphQL: true, Rate: github.RateLimit{Limit: 5000, Cost: 2, Remaining: 4098, ResetAt: reset}})
	next.flushGitHubCost(h.ctx, true)
	c, _ := h.costRecord("zhuravel")
	if len(c.Hours) != 1 || c.Hours[0].Points != 42 || c.Hours[0].Ops[opFixExternal].Points != 40 || c.Rate.Remaining != 4098 {
		t.Errorf("the record after the restart = %+v, rate %+v", c.Hours, c.Rate)
	}
}

// A run of low budget is announced once: the tick that finds an identity
// below the floor writes the run (at once, apart from the cost records,
// which stay at their once a minute), a github.budget_low event and a toast
// "GitHub budget of <identity> at N points: new rounds wait until HH:MM";
// later ticks of the run say nothing more; the tick that finds it recovered
// removes the run with github.budget_recovered, and the next run is
// announced again. A restart within a run announces nothing, though the
// daemon stopped before the minute's flush wrote the low budget: the run
// holds it.
func TestARunOfLowBudgetIsAnnouncedOnce(t *testing.T) {
	h := newHarness(t)
	h.startup()
	h.metered("zhuravel", opRound, 1, 4800, h.clock.Now().Add(30*time.Minute))
	h.tick() // the first flush
	writes := h.costWrites()
	reset := h.clock.Now().Add(30 * time.Minute)
	h.metered("zhuravel", opRound, 1, 300, reset)
	h.tick()
	h.tick()
	low := h.events("github.budget_low")
	if len(low) != 1 || low[0].Level != "warn" ||
		!strings.Contains(low[0].Message, "GitHub budget of zhuravel at 300 points: new rounds wait until "+reset.Local().Format("15:04")) {
		t.Fatalf("github.budget_low events = %+v, want one", low)
	}
	h.wantToasts("GitHub budget of zhuravel at 300 points", 1)
	if l, ok := h.lowRun("zhuravel"); !ok || l.Remaining != 300 || l.Limit != 5000 || l.Floor != config.DefaultBudgetFloor || !l.Until.Equal(reset.UTC()) {
		t.Errorf("the run = %+v, %v", l, ok)
	}
	if n := h.costWrites(); n != writes {
		t.Errorf("cost writes = %d within the minute, want %d: the run's start must not force them", n, writes)
	}

	// A restart now: the cost record still says 4800, the run says 300.
	next := New(h.d)
	next.loadGitHubCost(h.ctx)
	if l, ok := next.githubLow("zhuravel"); !ok || l.remaining != 300 {
		t.Errorf("after a restart within the run the floor holds nothing (%+v, %v)", l, ok)
	}
	next.noteGitHubBudget(h.ctx)
	if n := len(h.events("github.budget_low")); n != 1 {
		t.Errorf("github.budget_low events after a restart within the run = %d, want 1", n)
	}
	if n := len(h.events("github.budget_recovered")); n != 0 {
		t.Errorf("github.budget_recovered events after a restart within the run = %d, want 0", n)
	}

	h.advance(31 * time.Minute) // past the reset
	h.tick()
	if rec := h.events("github.budget_recovered"); len(rec) != 1 || rec[0].Level != "info" {
		t.Fatalf("github.budget_recovered events = %+v, want one", rec)
	}
	if l, ok := h.lowRun("zhuravel"); ok {
		t.Errorf("the run stays after the recovery: %+v", l)
	}

	h.metered("zhuravel", opRound, 1, 120, h.clock.Now().Add(time.Hour))
	h.tick()
	if n := len(h.events("github.budget_low")); n != 2 {
		t.Errorf("github.budget_low events = %d, want a second for the second run", n)
	}
	h.wantToasts("GitHub budget of zhuravel", 2)
}

// budget_floor = 0 turns the floor off: a budget however low holds nothing
// and is not announced.
func TestABudgetFloorOfZeroHoldsNothing(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.GitHub.BudgetFloor = 0 })
	h.startup()
	h.metered("zhuravel", opRound, 1, 60, h.clock.Now().Add(30*time.Minute))
	if _, low := h.e.githubLow("zhuravel"); low {
		t.Error("githubLow with the floor off")
	}
	h.tick()
	if n := len(h.events("github.budget_low")); n != 0 {
		t.Errorf("github.budget_low events = %d with the floor off", n)
	}
}

// The op labels are in plain words for `magnum status`; a call's own kind
// stays as it is.
func TestGitHubOpWords(t *testing.T) {
	for op, want := range map[string]string{
		opRoundSetup:  "round setup",
		opRoundSettle: "settle",
		opFixExternal: "other reviewers",
		"radar":       "radar",
		"ci states":   "ci states",
		"new.thing_x": "new thing x",
	} {
		if got := GitHubOpWords(op); got != want {
			t.Errorf("GitHubOpWords(%q) = %q, want %q", op, got, want)
		}
	}
	for op := range githubOpWords {
		if !strings.Contains(op, ".") && !strings.Contains(op, "_") && GitHubOpWords(op) == "" {
			t.Errorf("op %q has no words", op)
		}
	}
}
