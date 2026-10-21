package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// ghClock is a local wall-clock time as the status lines write it.
func ghClock(t time.Time) string { return t.Local().Format("15:04") }

// putGitHubCost stores one identity's cost record as the daemon's meter does.
func putGitHubCost(t *testing.T, st *store.Store, c engine.GitHubCost) {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(context.Background(), engine.KVGitHubCost(c.Identity), string(b)); err != nil {
		t.Fatal(err)
	}
}

// ghNow is ten past two in the afternoon, UTC: the clock hour began 10
// minutes ago, so a status window reaches back to the hour before.
var ghNow = time.Date(2026, 10, 3, 14, 10, 0, 0, time.UTC)

// ghCosts seeds two identities: zhuravel with a healthy budget and calls in
// three clock hours (the oldest outside the window), and talkable-app in a
// run of low budget with REST calls besides a few radar points.
func ghCosts(t *testing.T, st *store.Store) (zhuravel, app engine.GitHubCost) {
	t.Helper()
	hour := ghNow.Truncate(time.Hour)
	zhuravel = engine.GitHubCost{Identity: "zhuravel",
		Rate: &engine.GitHubRate{Remaining: 4692, Limit: 5000, ResetAt: ghNow.Add(40 * time.Minute), SeenAt: ghNow.Add(-time.Minute)},
		Hours: []engine.GitHubHour{
			{Hour: hour.Add(-2 * time.Hour), Points: 900, Calls: 31, RESTCalls: 9, Ops: map[string]engine.GitHubOpCost{
				"round.setup": {Points: 900, Calls: 22},
				"compare":     {Calls: 9, RESTCalls: 9},
			}},
			{Hour: hour.Add(-time.Hour), Points: 800, Calls: 37, RESTCalls: 5, Ops: map[string]engine.GitHubOpCost{
				"round.setup":  {Points: 400, Calls: 12},
				"round.settle": {Points: 200, Calls: 6},
				"radar":        {Points: 100, Calls: 12},
				"details":      {Points: 100, Calls: 2},
				"compare":      {Calls: 5, RESTCalls: 5},
			}},
			{Hour: hour, Points: 400, Calls: 24, RESTCalls: 7, Ops: map[string]engine.GitHubOpCost{
				"round.setup":  {Points: 220, Calls: 7},
				"round.settle": {Points: 100, Calls: 4},
				"radar":        {Points: 80, Calls: 6},
				"compare":      {Calls: 7, RESTCalls: 7},
			}},
		}}
	app = engine.GitHubCost{Identity: "talkable-app",
		Rate: &engine.GitHubRate{Remaining: 312, Limit: 5000, ResetAt: ghNow.Add(30 * time.Minute), SeenAt: ghNow.Add(-time.Minute)},
		Hours: []engine.GitHubHour{
			{Hour: hour, Points: 40, Calls: 15, RESTCalls: 12, Ops: map[string]engine.GitHubOpCost{
				"radar":   {Points: 40, Calls: 3},
				"compare": {Calls: 12, RESTCalls: 12},
			}},
		}}
	putGitHubCost(t, st, zhuravel)
	putGitHubCost(t, st, app)
	putGitHubLow(t, st, "talkable-app", ghAppLow)
	return zhuravel, app
}

// ghAppLow is talkable-app's run of low budget in ghCosts.
var ghAppLow = engine.GitHubLow{Since: ghNow.Add(-5 * time.Minute), Remaining: 312, Floor: 500, Until: ghNow.Add(30 * time.Minute)}

// putGitHubLow stores identity's run of low budget as the daemon does.
func putGitHubLow(t *testing.T, st *store.Store, identity string, l engine.GitHubLow) {
	t.Helper()
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(context.Background(), engine.KVGitHubLow(identity), string(b)); err != nil {
		t.Fatal(err)
	}
}

// `magnum status` says, for each identity the daemon meters, what is left of
// its GitHub budget and what the calls since the hour's start cost, by op.
func TestStatusShowsEachIdentitysGitHubBudgetAndWhatItCost(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	d.Now = func() time.Time { return ghNow }
	zhuravel, app := ghCosts(t, st)
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	since := ghClock(ghNow.Truncate(time.Hour).Add(-time.Hour))
	// talkable-app sorts before zhuravel; the second line is indented to the first's column.
	wantA := fmt.Sprintf("github:   talkable-app 312/5000, resets %s · below the floor of 500: new work waits until %s · since %s 40 points, 12 REST calls: radar 40, compare 12 REST",
		ghClock(app.Rate.ResetAt), ghClock(ghAppLow.Until), since)
	wantZ := fmt.Sprintf("          zhuravel 4692/5000, resets %s · since %s 1,200 points, 12 REST calls: round setup 620, settle 300, radar 180, …",
		ghClock(zhuravel.Rate.ResetAt), since)
	lines := strings.Split(b.String(), "\n")
	var got []string
	for i, l := range lines {
		if strings.HasPrefix(l, "github:") {
			got = lines[i : i+2]
		}
	}
	if len(got) != 2 || got[0] != wantA || got[1] != wantZ {
		t.Fatalf("github lines =\n%q\nwant\n%q\n%q\nin\n%s", got, wantA, wantZ, b.String())
	}
	if strings.Contains(b.String(), "points left") {
		t.Errorf("the old line stays beside the records:\n%s", b.String())
	}
}

// The window starts with the clock hour, but reaches back to the hour before
// while the current one is younger than half an hour: right after the turn a
// window of the new hour alone would show almost nothing.
func TestTheGitHubWindowReachesBackWhileTheHourIsYoung(t *testing.T) {
	hour := ghNow.Truncate(time.Hour)
	for _, tc := range []struct {
		name  string
		now   time.Time
		start time.Time
		want  string
	}{
		{"ten past", ghNow, hour.Add(-time.Hour), "1,200 points"},
		{"at the turn", hour, hour.Add(-time.Hour), "1,200 points"},
		{"half past", hour.Add(30 * time.Minute), hour, "400 points"},
		{"twenty to", hour.Add(40 * time.Minute), hour, "400 points"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, st, d, _ := statusFixture(t)
			d.Now = func() time.Time { return tc.now }
			ghCosts(t, st)
			r, err := statusGather(context.Background(), d, statusOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var z *statusGitHubIdentity
			for i := range r.GitHub.Identities {
				if r.GitHub.Identities[i].Identity == "zhuravel" {
					z = &r.GitHub.Identities[i]
				}
			}
			if z == nil || !z.WindowStart.Equal(tc.start) {
				t.Fatalf("zhuravel = %+v, want the window to start at %s", z, tc.start)
			}
			var b bytes.Buffer
			statusRender(&b, r)
			want := fmt.Sprintf("· since %s %s", ghClock(tc.start), tc.want)
			if !strings.Contains(b.String(), want) {
				t.Errorf("status lacks %q:\n%s", want, b.String())
			}
		})
	}
}

// A rate limit whose window has ended says when it reset and that no call has
// updated it since, not a count of points that GitHub has refilled.
func TestAGitHubBudgetPastItsResetIsNotShownAsLeft(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	d.Now = func() time.Time { return ghNow }
	reset := ghNow.Add(-8 * time.Minute)
	putGitHubCost(t, st, engine.GitHubCost{Identity: "zhuravel",
		Rate: &engine.GitHubRate{Remaining: 41, Limit: 5000, ResetAt: reset, SeenAt: ghNow.Add(-time.Hour)}})
	putGitHubCost(t, st, engine.GitHubCost{Identity: "example-app"})
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	since := ghClock(ghNow.Truncate(time.Hour).Add(-time.Hour))
	for _, want := range []string{
		fmt.Sprintf("github:   example-app budget unknown · no calls since %s\n", since),
		fmt.Sprintf("          zhuravel reset at %s (no call since) · no calls since %s\n", ghClock(reset), since),
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "41/5000") {
		t.Errorf("a budget past its reset is shown as left:\n%s", b.String())
	}
}

// Without a readable record (a daemon that does not meter yet) the line is
// the radar's, as it was.
func TestStatusKeepsTheRadarsGitHubLineWithoutCostRecords(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	if err := st.SetKV(context.Background(), engine.KVGitHubCost("zhuravel"), "not json"); err != nil {
		t.Fatal(err)
	}
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.GitHub.Identities) != 0 {
		t.Fatalf("identities = %+v, want none from an unreadable record", r.GitHub.Identities)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	if !strings.Contains(b.String(), "github:   4890/5000 points left\n") {
		t.Errorf("the radar's line is gone:\n%s", b.String())
	}
}

// An identity below the budget floor holds new work, and the pauses say so.
func TestAnIdentityBelowTheGitHubFloorIsListedAsAPause(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	d.Now = func() time.Time { return ghNow }
	ghCosts(t, st)
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var pauses []statusPause
	for _, p := range r.Pauses {
		if strings.HasPrefix(p.Scope, "github:") {
			pauses = append(pauses, p)
		}
	}
	if len(pauses) != 1 {
		t.Fatalf("github pauses = %+v, want one for talkable-app", pauses)
	}
	p := pauses[0]
	if p.Scope != "github:talkable-app" || p.Reason != "budget floor" || p.Until == nil || !p.Until.Equal(ghAppLow.Until) ||
		!strings.Contains(p.Detail, "fix jobs") || !strings.Contains(p.Detail, "running rounds finish") {
		t.Fatalf("pause = %+v", p)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	want := fmt.Sprintf("  github:talkable-app: budget floor until %s (in 30m) — new rounds, fix jobs, whiteboards, the retro and notes curations wait; running rounds finish\n",
		inspClock(ghNow, ghAppLow.Until))
	if !strings.Contains(b.String(), want) {
		t.Errorf("pauses lack %q:\n%s", want, b.String())
	}
}

// A run of low budget whose reset passed is over, though no daemon cleared
// it (one that stopped during the run): no pause, no floor on the line.
func TestALowRunPastItsResetIsNoPause(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	d.Now = func() time.Time { return ghNow.Add(31 * time.Minute) }
	ghCosts(t, st)
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Pauses {
		if strings.HasPrefix(p.Scope, "github:") {
			t.Errorf("a pause for a run past its reset: %+v", p)
		}
	}
	var b bytes.Buffer
	statusRender(&b, r)
	if strings.Contains(b.String(), "below the floor") {
		t.Errorf("the line names the floor past the reset:\n%s", b.String())
	}
}

// A run of low budget the daemon wrote before the identity's first cost
// record (the run is written at once, the record once a minute) is shown
// all the same, with the budget unknown.
func TestARunOfLowBudgetBeforeTheFirstRecordIsShown(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	d.Now = func() time.Time { return ghNow }
	putGitHubLow(t, st, "zhuravel", ghAppLow)
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.GitHub.Identities) != 1 || r.GitHub.Identities[0].Identity != "zhuravel" || r.GitHub.Identities[0].Low == nil {
		t.Fatalf("identities = %+v, want zhuravel with its run", r.GitHub.Identities)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	if want := "github:   zhuravel budget unknown · below the floor of 500: new work waits until " + ghClock(ghAppLow.Until); !strings.Contains(b.String(), want) {
		t.Errorf("status lacks %q:\n%s", want, b.String())
	}
}

// --json carries what the line sums up: the window's totals and ops, the
// budget, the run of low budget and the hours as the meter stored them.
func TestStatusJSONCarriesTheGitHubCostBreakdown(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	d.Now = func() time.Time { return ghNow }
	ghCosts(t, st)
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	type opJSON struct {
		Op        string `json:"op"`
		Words     string `json:"words"`
		Points    int    `json:"points"`
		Calls     int    `json:"calls"`
		RESTCalls int    `json:"rest_calls"`
	}
	type idJSON struct {
		Identity  string     `json:"identity"`
		Remaining *int       `json:"remaining"`
		Limit     *int       `json:"limit"`
		ResetAt   *time.Time `json:"reset_at"`
		SeenAt    *time.Time `json:"seen_at"`
		Low       *struct {
			Remaining int       `json:"remaining"`
			Floor     int       `json:"floor"`
			Until     time.Time `json:"until"`
		} `json:"low"`
		WindowStart     time.Time `json:"window_start"`
		WindowPoints    int       `json:"window_points"`
		WindowCalls     int       `json:"window_calls"`
		WindowRESTCalls int       `json:"window_rest_calls"`
		WindowOps       []opJSON  `json:"window_ops"`
		Hours           []struct {
			Hour time.Time `json:"hour"`
			Ops  map[string]struct {
				Points int `json:"points"`
			} `json:"ops"`
		} `json:"hours"`
	}
	var doc struct {
		GitHub struct {
			Remaining  *int     `json:"remaining"`
			Identities []idJSON `json:"identities"`
		} `json:"github"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.GitHub.Remaining == nil || *doc.GitHub.Remaining != 4890 {
		t.Errorf("the radar's remaining is gone: %+v", doc.GitHub.Remaining)
	}
	ids := doc.GitHub.Identities
	if len(ids) != 2 || ids[0].Identity != "talkable-app" || ids[1].Identity != "zhuravel" {
		t.Fatalf("identities = %+v, want talkable-app then zhuravel", ids)
	}
	a, z := ids[0], ids[1]
	if z.Remaining == nil || *z.Remaining != 4692 || *z.Limit != 5000 || z.ResetAt == nil || z.SeenAt == nil || z.Low != nil {
		t.Errorf("zhuravel = %+v", z)
	}
	if !z.WindowStart.Equal(ghNow.Truncate(time.Hour).Add(-time.Hour)) || z.WindowPoints != 1200 || z.WindowCalls != 61 || z.WindowRESTCalls != 12 {
		t.Errorf("zhuravel window = %v %d points %d calls %d REST", z.WindowStart, z.WindowPoints, z.WindowCalls, z.WindowRESTCalls)
	}
	if len(z.WindowOps) != 5 || z.WindowOps[0].Op != "round.setup" || z.WindowOps[0].Words != "round setup" || z.WindowOps[0].Points != 620 ||
		z.WindowOps[0].Calls != 19 || z.WindowOps[4].Op != "compare" || z.WindowOps[4].RESTCalls != 12 {
		t.Errorf("zhuravel window ops = %+v", z.WindowOps)
	}
	if len(z.Hours) != 3 || z.Hours[0].Ops["round.setup"].Points != 900 {
		t.Errorf("zhuravel hours = %+v, want the 3 stored hours with their ops", z.Hours)
	}
	if a.Low == nil || a.Low.Remaining != 312 || a.Low.Floor != 500 || !a.Low.Until.Equal(ghNow.Add(30*time.Minute)) {
		t.Errorf("talkable-app low = %+v", a.Low)
	}
}

// An identity's name and an op's words come off the registry, and the lines
// show no control character.
func TestTheGitHubLineCleansWhatTheRecordsHold(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	d.Now = func() time.Time { return ghNow }
	putGitHubCost(t, st, engine.GitHubCost{Identity: "evil\x1b[31m\nname",
		Rate: &engine.GitHubRate{Remaining: 9, Limit: 5000, ResetAt: ghNow.Add(time.Minute)},
		Hours: []engine.GitHubHour{{Hour: ghNow.Truncate(time.Hour), Points: 5, Calls: 1, Ops: map[string]engine.GitHubOpCost{
			"x\x1b[2Jop\n": {Points: 5, Calls: 1}}}}})
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	if strings.ContainsRune(b.String(), 0x1b) {
		t.Errorf("a control character reached the output:\n%q", b.String())
	}
	// Control characters become spaces: the name stays on its line.
	if want := "github:   evil [31m name 9/5000"; !strings.Contains(b.String(), want) {
		t.Errorf("status lacks %q:\n%s", want, b.String())
	}
}

// ghOutsideLine is zhuravel's github line, and its identity in the report,
// with windows: a budget of 4692 resetting in 20 minutes and no calls in the
// status window.
func ghOutsideLine(t *testing.T, windows ...engine.GitHubWindow) (string, statusGitHubIdentity) {
	t.Helper()
	_, st, d, _ := statusFixture(t)
	d.Now = func() time.Time { return ghNow }
	putGitHubCost(t, st, engine.GitHubCost{Identity: "zhuravel",
		Rate:    &engine.GitHubRate{Remaining: 4692, Limit: 5000, ResetAt: ghNow.Add(20 * time.Minute), SeenAt: ghNow.Add(-time.Minute)},
		Windows: windows})
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.GitHub.Identities) != 1 {
		t.Fatalf("identities = %+v, want zhuravel", r.GitHub.Identities)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	for line := range strings.Lines(b.String()) {
		if strings.HasPrefix(line, "github:") {
			return strings.TrimSuffix(line, "\n"), r.GitHub.Identities[0]
		}
	}
	t.Fatalf("no github line in\n%s", b.String())
	return "", statusGitHubIdentity{}
}

// On 2026-10-21 the meter counted 526 points of zhuravel's window while
// GitHub counted about 4,900 used: the github line ends with what its live
// windows spent outside magnum's calls and since when (the earliest live
// window's start, an hour before its reset).
func TestTheGitHubLineSaysWhatWasSpentOutsideMagnum(t *testing.T) {
	reset := ghNow.Add(20 * time.Minute)
	got, _ := ghOutsideLine(t, engine.GitHubWindow{ResetAt: reset, Limit: 5000, Used: 4938, Points: 526})
	want := fmt.Sprintf("github:   zhuravel 4692/5000, resets %s · no calls since %s · 4,412 points outside magnum since %s",
		ghClock(reset), ghClock(ghNow.Truncate(time.Hour).Add(-time.Hour)), ghClock(reset.Add(-time.Hour)))
	if got != want {
		t.Errorf("github line =\n%q\nwant\n%q", got, want)
	}
}

// Below 100 points outside the line says nothing of it.
func TestTheGitHubLineSaysNothingOfLessThan100PointsOutside(t *testing.T) {
	reset := ghNow.Add(20 * time.Minute)
	got, _ := ghOutsideLine(t, engine.GitHubWindow{ResetAt: reset, Limit: 5000, Used: 625, Points: 526})
	if strings.Contains(got, "outside") {
		t.Errorf("github line names 99 points outside: %q", got)
	}
	got, _ = ghOutsideLine(t, engine.GitHubWindow{ResetAt: reset, Limit: 5000, Used: 626, Points: 526})
	if want := " · 100 points outside magnum since " + ghClock(reset.Add(-time.Hour)); !strings.HasSuffix(got, want) {
		t.Errorf("github line = %q, want it to end %q", got, want)
	}
}

// A login that shows two windows at once has both summed, since the
// earlier one's start, whatever order the record lists them in.
func TestTheGitHubLineSumsTheLiveWindows(t *testing.T) {
	early, late := ghNow.Add(19*time.Minute+46*time.Second), ghNow.Add(21*time.Minute)
	got, _ := ghOutsideLine(t,
		engine.GitHubWindow{ResetAt: late, Limit: 5000, Used: 1500, Points: 88},
		engine.GitHubWindow{ResetAt: early, Limit: 5000, Used: 3400, Points: 400})
	if want := " · 4,412 points outside magnum since " + ghClock(early.Add(-time.Hour)); !strings.HasSuffix(got, want) {
		t.Errorf("github line = %q, want it to end %q", got, want)
	}
}

// A window whose reset passed counts no more: its spend is GitHub's past.
func TestTheGitHubLineLeavesOutAnExpiredWindow(t *testing.T) {
	live := ghNow.Add(50 * time.Minute)
	got, _ := ghOutsideLine(t,
		engine.GitHubWindow{ResetAt: ghNow.Add(-10 * time.Minute), Limit: 5000, Used: 4900, Points: 900},
		engine.GitHubWindow{ResetAt: ghNow, Limit: 5000, Used: 4900, Points: 900}, // resets now: over
		engine.GitHubWindow{ResetAt: live, Limit: 5000, Used: 500, Points: 88})
	if want := " · 412 points outside magnum since " + ghClock(live.Add(-time.Hour)); !strings.HasSuffix(got, want) {
		t.Errorf("github line = %q, want it to end %q", got, want)
	}
	got, _ = ghOutsideLine(t, engine.GitHubWindow{ResetAt: ghNow.Add(-10 * time.Minute), Limit: 5000, Used: 4900, Points: 900})
	if strings.Contains(got, "outside") {
		t.Errorf("github line names an expired window's spend: %q", got)
	}
}

// --json has every kept window of each identity, expired ones included,
// with what it spent outside magnum's calls.
func TestStatusJSONCarriesTheGitHubWindows(t *testing.T) {
	expired, live := ghNow.Add(-10*time.Minute), ghNow.Add(50*time.Minute)
	_, id := ghOutsideLine(t,
		engine.GitHubWindow{ResetAt: expired, Limit: 5000, Used: 4900, Points: 900, Told: true},
		engine.GitHubWindow{ResetAt: live, Limit: 5000, Used: 500, Points: 520})
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Windows []struct {
			ResetAt time.Time `json:"reset_at"`
			Limit   int       `json:"limit"`
			Used    int       `json:"used"`
			Points  int       `json:"points"`
			Outside *int      `json:"outside"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	ws := doc.Windows
	if len(ws) != 2 || !ws[0].ResetAt.Equal(expired) || ws[0].Limit != 5000 || ws[0].Used != 4900 || ws[0].Points != 900 ||
		ws[0].Outside == nil || *ws[0].Outside != 4000 || !ws[1].ResetAt.Equal(live) || ws[1].Outside == nil || *ws[1].Outside != 0 {
		t.Errorf("windows = %s, want both with their outside spend", b)
	}
	if strings.Contains(string(b), "told") {
		t.Errorf("the status JSON carries the daemon's told mark: %s", b)
	}
	_, none := ghOutsideLine(t)
	if b, _ := json.Marshal(none); !strings.Contains(string(b), `"windows":[]`) {
		t.Errorf("an identity without windows = %s, want an empty windows list", b)
	}
}
