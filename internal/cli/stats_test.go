package cli

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// statsNow is the CLI clock of the stats tests; the default window (7d)
// starts 7 days before it. Every instant is built in the local zone, so the
// calendar day of each (store.DayKey) does not depend on the machine's.
var statsNow = time.Date(2026, 10, 4, 10, 0, 0, 0, time.Local)

// statsAt is 2026-<month>-<day> at hour:min, local.
func statsAt(month time.Month, day, hour, min int) time.Time {
	return time.Date(2026, month, day, hour, min, 0, 0, time.Local)
}

func statsOct(day, hour, min int) time.Time { return statsAt(time.October, day, hour, min) }

// statsRunSpec describes one run to seed; zero times mean "not set".
type statsRunSpec struct {
	pr                         store.PR
	round                      int
	role, state                string
	created                    time.Time
	submitted, ended, verified time.Time
	outcome, result            string
}

func statsOpt(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func statsStrOpt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func statsMkRun(t *testing.T, st *store.Store, s statsRunSpec) store.Run {
	t.Helper()
	r, err := st.CreateRun(context.Background(), store.Run{PRID: s.pr.ID, Round: s.round, Role: s.role, Kind: store.RunInitial,
		State: s.state, TargetSHA: "abcdef0123456789", Identity: "talkable-app", ReviewerLogin: "talkable[bot]", PromptText: "x",
		CreatedAt: s.created, SubmittedAt: statsOpt(s.submitted), EndedAt: statsOpt(s.ended), VerifiedAt: statsOpt(s.verified),
		Outcome: statsStrOpt(s.outcome), ResultJSON: statsStrOpt(s.result)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// statsEvent appends an audit event at at; data is raw JSON or "".
func statsEvent(t *testing.T, st *store.Store, at time.Time, subject, kind, msg, data string) {
	t.Helper()
	ev := store.Event{At: at, Kind: kind, Message: msg}
	if subject != "" {
		ev.Subject = &subject
	}
	if data != "" {
		ev.Data = json.RawMessage(data)
	}
	if _, err := st.AppendEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

// statsSeed fills st with a week of work in talkable/talkable and
// example/widgets (see statsWantGroups for what it adds up to).
func statsSeed(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	const talkable, widgets = "talkable/talkable", "example/widgets"
	_, pr101 := inspSeedPR(t, st, talkable, 101, store.PRReviewed, nil)
	_, pr102 := inspSeedPR(t, st, talkable, 102, store.PRReviewed, nil)
	_, pr103 := inspSeedPR(t, st, talkable, 103, store.PRReviewed, nil)
	_, pr104 := inspSeedPR(t, st, talkable, 104, store.PRBaseline, nil)
	_, pr105 := inspSeedPR(t, st, talkable, 105, store.PRReviewed, nil)
	_, pr7 := inspSeedPR(t, st, widgets, 7, store.PRReviewed, nil)

	// Round A (day 2, posted): both reviewers and the judge; the result file
	// counts 1 P1 and 2 P2; five findings of provenance.
	statsMkRun(t, st, statsRunSpec{pr: pr101, round: 1, role: store.RoleClaude, state: store.RunVerified, outcome: "ok",
		created: statsOct(2, 12, 0), submitted: statsOct(2, 12, 0), ended: statsOct(2, 12, 10), verified: statsOct(2, 12, 11)})
	statsMkRun(t, st, statsRunSpec{pr: pr101, round: 1, role: store.RoleCodexReview, state: store.RunVerified, outcome: "ok",
		created: statsOct(2, 12, 0), submitted: statsOct(2, 12, 0), ended: statsOct(2, 12, 20), verified: statsOct(2, 12, 21)})
	judgeA := statsMkRun(t, st, statsRunSpec{pr: pr101, round: 1, role: store.RoleJudge, state: store.RunVerified, outcome: "posted",
		created: statsOct(2, 12, 22), submitted: statsOct(2, 12, 25), ended: statsOct(2, 12, 35), verified: statsOct(2, 12, 36),
		result: `{"status":"posted","findings":{"P0":0,"P1":1,"P2":2,"P3":0}}`})
	statsEvent(t, st, statsOct(2, 12, 36), "pr:talkable/talkable#101", "round.end",
		"round 1 ended: posted https://example.com/talkable/talkable/pull/101#pullrequestreview-1", `{"outcome":"posted"}`)
	if err := st.RecordFindings(ctx, judgeA.ID, pr101.ID, 1, []store.Finding{
		{FindingID: "F1", Severity: "P2", Sources: []string{"claude-review"}, Verdict: store.FindingPosted, CreatedAt: statsOct(2, 12, 36)},
		{FindingID: "F2", Severity: "P1", Sources: []string{"claude-review", "codex-review"}, Verdict: store.FindingPosted, CreatedAt: statsOct(2, 12, 36)},
		{FindingID: "F3", Severity: "P3", Sources: []string{"codex-review"}, Verdict: store.FindingRejected, ReasonCode: "style_only", CreatedAt: statsOct(2, 12, 36)},
		{FindingID: "F4", Severity: "P2", Sources: []string{"judge"}, Verdict: store.FindingPosted, CreatedAt: statsOct(2, 12, 36)},
		{FindingID: "F5", Severity: "P3", Sources: []string{"claude-review"}, Verdict: store.FindingRejected, CreatedAt: statsOct(2, 12, 36)},
	}); err != nil {
		t.Fatal(err)
	}

	// Round B (day 3, posted; no round.end event, so the judge run's outcome
	// counts); its result file has counts as numeric strings and junk.
	statsMkRun(t, st, statsRunSpec{pr: pr102, round: 1, role: store.RoleClaude, state: store.RunVerified, outcome: "ok",
		created: statsOct(3, 12, 0), submitted: statsOct(3, 12, 0), ended: statsOct(3, 12, 20), verified: statsOct(3, 12, 21)})
	judgeB := statsMkRun(t, st, statsRunSpec{pr: pr102, round: 1, role: store.RoleJudge, state: store.RunVerified, outcome: "posted",
		created: statsOct(3, 12, 22), submitted: statsOct(3, 12, 22), ended: statsOct(3, 12, 40), verified: statsOct(3, 12, 41),
		result: `{"findings":{"P0":"x","P1":-2,"P2":"3","P3":1,"P4":9}}`})
	if err := st.RecordFindings(ctx, judgeB.ID, pr102.ID, 1, []store.Finding{
		{FindingID: "G1", Severity: "P2", Sources: []string{"codex-review"}, Verdict: store.FindingPosted, CreatedAt: statsOct(3, 12, 41)},
	}); err != nil {
		t.Fatal(err)
	}

	// Round C (day 3): paused on a usage limit, then stopped; the latest
	// round.end wins.
	statsMkRun(t, st, statsRunSpec{pr: pr103, round: 1, role: store.RoleClaude, state: store.RunVerified, outcome: "ok",
		created: statsOct(3, 13, 0), submitted: statsOct(3, 13, 0), ended: statsOct(3, 13, 30), verified: statsOct(3, 13, 31)})
	statsMkRun(t, st, statsRunSpec{pr: pr103, round: 1, role: store.RoleJudge, state: store.RunAbandoned,
		created: statsOct(3, 13, 31), submitted: statsOct(3, 13, 31), ended: statsOct(3, 13, 35)})
	statsEvent(t, st, statsOct(3, 13, 20), "pr:talkable/talkable#103", "round.end", "round 1 ended: usage_limit", `{"outcome":"usage_limit"}`)
	statsEvent(t, st, statsOct(3, 13, 35), "pr:talkable/talkable#103", "round.end", "round 1 ended: stopped", `{"outcome":"stopped"}`)

	// Round D (day 3): still running, no judge yet.
	statsMkRun(t, st, statsRunSpec{pr: pr104, round: 1, role: store.RoleClaude, state: store.RunWorking,
		created: statsOct(3, 14, 0), submitted: statsOct(3, 14, 0)})

	// Round E began before the window and went on inside it: left out whole,
	// and so is its finding recorded before the window.
	statsMkRun(t, st, statsRunSpec{pr: pr105, round: 1, role: store.RoleClaude, state: store.RunVerified, outcome: "ok",
		created: statsAt(time.September, 25, 12, 0), submitted: statsAt(time.September, 25, 12, 0), ended: statsAt(time.September, 25, 12, 10)})
	judgeE := statsMkRun(t, st, statsRunSpec{pr: pr105, round: 1, role: store.RoleJudge, state: store.RunVerified, outcome: "posted",
		created: statsOct(1, 12, 0), submitted: statsOct(1, 12, 0), ended: statsOct(1, 12, 10), verified: statsOct(1, 12, 11),
		result: `{"findings":{"P0":5}}`})
	statsEvent(t, st, statsOct(1, 12, 10), "pr:talkable/talkable#105", "round.end", "round 1 ended: posted", `{"outcome":"posted"}`)
	if err := st.RecordFindings(ctx, judgeE.ID, pr105.ID, 1, []store.Finding{
		{FindingID: "E1", Severity: "P0", Sources: []string{"claude-review"}, Verdict: store.FindingPosted, CreatedAt: statsAt(time.September, 20, 12, 0)},
	}); err != nil {
		t.Fatal(err)
	}

	// Round F (day 2, example/widgets): the judge failed.
	statsMkRun(t, st, statsRunSpec{pr: pr7, round: 1, role: store.RoleCodexReview, state: store.RunVerified, outcome: "ok",
		created: statsOct(2, 15, 0), submitted: statsOct(2, 15, 0), ended: statsOct(2, 15, 40), verified: statsOct(2, 15, 41)})
	judgeF := statsMkRun(t, st, statsRunSpec{pr: pr7, round: 1, role: store.RoleJudge, state: store.RunVerified, outcome: "error",
		created: statsOct(2, 15, 41), submitted: statsOct(2, 15, 41), ended: statsOct(2, 15, 50)})
	if err := st.RecordFindings(ctx, judgeF.ID, pr7.ID, 1, []store.Finding{
		{FindingID: "H1", Severity: "P2", Sources: []string{"claude-review"}, Verdict: store.FindingRejected, ReasonCode: "duplicate", CreatedAt: statsOct(2, 15, 50)},
	}); err != nil {
		t.Fatal(err)
	}

	// Operations. Two events are older than the window and two have no PR
	// subject to count them under.
	deny := `{"role":"claude-review"}`
	statsEvent(t, st, statsOct(2, 12, 5), "pr:talkable/talkable#101", "agent.prompt_denied", "denied a prompt", deny)
	statsEvent(t, st, statsOct(2, 12, 15), "pr:talkable/talkable#101", "agent.prompt_denied", "denied a prompt", deny)
	statsEvent(t, st, statsOct(2, 15, 10), "pr:example/widgets#7", "agent.prompt_denied", "denied a prompt", deny)
	statsEvent(t, st, statsOct(4, 9, 0), "pr:example/widgets#7", "agent.prompt_denied", "denied a prompt", deny)
	statsEvent(t, st, statsOct(3, 12, 10), "pr:talkable/talkable#102", "round.model_fallback", "switched model", deny)
	statsEvent(t, st, statsOct(3, 13, 10), "pr:talkable/talkable#103", "round.restarted", "the PR head moved", "")
	statsEvent(t, st, statsOct(2, 15, 30), "pr:example/widgets#7", "round.restarted", "the PR head moved", "")
	statsEvent(t, st, statsAt(time.September, 20, 12, 0), "pr:talkable/talkable#101", "round.restarted", "too old", "")
	statsEvent(t, st, statsAt(time.September, 20, 12, 0), "pr:talkable/talkable#101", "agent.prompt_denied", "too old", deny)
	statsEvent(t, st, statsOct(3, 13, 0), "slot:review1", "round.restarted", "not a PR subject", "")
	statsEvent(t, st, statsOct(3, 13, 0), "pr:oops", "round.restarted", "not a PR subject", "")
}

// statsFixture is a magnum home whose registry holds statsSeed and whose CLI
// clock is statsNow.
func statsFixture(t *testing.T) *inspFixture {
	t.Helper()
	f := newInspFixture(t)
	prev := inspNow
	inspNow = func() time.Time { return statsNow }
	t.Cleanup(func() { inspNow = prev })
	st := f.store()
	st.Clock = func() time.Time { return statsNow }
	statsSeed(t, st)
	st.Close()
	return f
}

// statsDur is a statsDuration of n spans with the median and p90 in minutes.
func statsDur(n int, median, p90 int) statsDuration {
	return statsDuration{N: n, MedianSeconds: int64(median) * 60, P90Seconds: int64(p90) * 60}
}

// statsDays are the local days of the seeded work.
func statsDays() (d1, d2, d3 string) {
	return store.DayKey(statsOct(2, 12, 0)), store.DayKey(statsOct(3, 12, 0)), store.DayKey(statsOct(4, 9, 0))
}

// statsWantGroups are the groups and the total statsSeed adds up to.
func statsWantGroups() ([]statsGroup, statsGroup) {
	d1, d2, d3 := statsDays()
	groups := []statsGroup{
		{Day: d1, Repo: "example/widgets",
			Rounds:    statsRounds{Started: 1, Outcomes: map[string]int{"error": 1}},
			Durations: map[string]statsDuration{"codex-review": statsDur(1, 40, 40), "codex-judge": statsDur(1, 9, 9), "round": statsDur(1, 50, 50)},
			Sources:   map[string]*statsSource{"claude-review": {Judged: 1, Rejected: 1, Reasons: map[string]int{"duplicate": 1}}},
			Denies:    1, Restarts: 1},
		{Day: d1, Repo: "talkable/talkable",
			Rounds: statsRounds{Started: 1, Posted: 1, Outcomes: map[string]int{"posted": 1}},
			Durations: map[string]statsDuration{"claude-review": statsDur(1, 10, 10), "codex-review": statsDur(1, 20, 20),
				"codex-judge": statsDur(1, 10, 10), "round": statsDur(1, 36, 36)},
			FindingsPosted: map[string]int{"P1": 1, "P2": 2},
			Sources: map[string]*statsSource{
				"claude-review": {Judged: 3, Posted: 2, Unique: 1, Rejected: 1, Reasons: map[string]int{"unspecified": 1}},
				"codex-review":  {Judged: 2, Posted: 1, Unique: 0, Rejected: 1, Reasons: map[string]int{"style_only": 1}},
				"judge":         {Judged: 1, Posted: 1, Unique: 1},
			},
			Denies: 2},
		{Day: d2, Repo: "talkable/talkable",
			Rounds: statsRounds{Started: 3, Posted: 1, Outcomes: map[string]int{"posted": 1, "stopped": 1, "running": 1}},
			Durations: map[string]statsDuration{"claude-review": statsDur(2, 20, 30), "codex-judge": statsDur(2, 4, 18),
				"round": statsDur(2, 35, 41)},
			FindingsPosted: map[string]int{"P2": 3, "P3": 1},
			Sources:        map[string]*statsSource{"codex-review": {Judged: 1, Posted: 1, Unique: 1}},
			ModelSwitches:  1, Restarts: 1},
		{Day: d3, Repo: "example/widgets", Denies: 1},
	}
	total := statsGroup{
		Rounds: statsRounds{Started: 5, Posted: 2, Outcomes: map[string]int{"posted": 2, "error": 1, "stopped": 1, "running": 1}},
		Durations: map[string]statsDuration{"claude-review": statsDur(3, 20, 30), "codex-review": statsDur(2, 20, 40),
			"codex-judge": statsDur(4, 9, 18), "round": statsDur(4, 36, 50)},
		FindingsPosted: map[string]int{"P1": 1, "P2": 5, "P3": 1},
		Sources: map[string]*statsSource{
			"claude-review": {Judged: 4, Posted: 2, Unique: 1, Rejected: 2, Reasons: map[string]int{"unspecified": 1, "duplicate": 1}},
			"codex-review":  {Judged: 3, Posted: 2, Unique: 1, Rejected: 1, Reasons: map[string]int{"style_only": 1}},
			"judge":         {Judged: 1, Posted: 1, Unique: 1},
		},
		ModelSwitches: 1, Denies: 4, Restarts: 2,
	}
	return groups, total
}

// statsJSON is v as indented JSON: empty and nil maps look alike, as in the
// command's output.
func statsJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStatsGatherAddsUpPerDayAndRepo(t *testing.T) {
	f := statsFixture(t)
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	since := statsNow.Add(-7 * 24 * time.Hour)
	r, err := statsGather(context.Background(), f.store(), f.Ctx.Config, since, statsNow, "")
	if err != nil {
		t.Fatal(err)
	}
	groups, total := statsWantGroups()
	if got, want := statsJSON(t, r.Groups), statsJSON(t, groups); got != want {
		t.Fatalf("groups =\n%s\nwant\n%s", got, want)
	}
	if got, want := statsJSON(t, r.Total), statsJSON(t, total); got != want {
		t.Fatalf("total =\n%s\nwant\n%s", got, want)
	}
	if r.Repo != "" || !r.Since.Equal(since) || !r.Until.Equal(statsNow) {
		t.Fatalf("window = %v .. %v repo %q", r.Since, r.Until, r.Repo)
	}
	// Three claude-review turns of 10, 20 and 30 minutes: the median is the
	// middle one and the p90 the longest.
	if d := r.Total.Durations["claude-review"]; d.N != 3 || d.MedianSeconds != 20*60 || d.P90Seconds != 30*60 {
		t.Fatalf("claude-review durations = %+v", d)
	}
	// The window's edge: a round that began a minute before it is out.
	late, err := statsGather(context.Background(), f.store(), f.Ctx.Config, statsOct(2, 12, 1), statsNow, "")
	if err != nil {
		t.Fatal(err)
	}
	if late.Total.Rounds.Started != 4 { // A (12:00) is out, B, C, D and F are in
		t.Fatalf("started since 12:01 on day 2 = %d, want 4", late.Total.Rounds.Started)
	}
}

func TestStatsGatherKeepsOneRepository(t *testing.T) {
	f := statsFixture(t)
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	r, err := statsGather(context.Background(), f.store(), f.Ctx.Config, statsNow.Add(-7*24*time.Hour), statsNow, "example/widgets")
	if err != nil {
		t.Fatal(err)
	}
	groups, _ := statsWantGroups()
	if got, want := statsJSON(t, r.Groups), statsJSON(t, []statsGroup{groups[0], groups[3]}); got != want {
		t.Fatalf("groups =\n%s\nwant\n%s", got, want)
	}
	tot := r.Total
	if r.Repo != "example/widgets" || tot.Rounds.Started != 1 || tot.Denies != 2 || tot.Restarts != 1 || tot.ModelSwitches != 0 ||
		tot.Sources["claude-review"] == nil || tot.Sources["claude-review"].Judged != 1 || tot.Sources["codex-review"] != nil ||
		len(tot.FindingsPosted) != 0 {
		t.Fatalf("total = %s", statsJSON(t, tot))
	}
}

func TestStatsParseSince(t *testing.T) {
	now := statsNow
	for in, want := range map[string]time.Time{
		"7d":         now.Add(-7 * 24 * time.Hour),
		" 1d ":       now.Add(-24 * time.Hour),
		"36h":        now.Add(-36 * time.Hour),
		"90m":        now.Add(-90 * time.Minute),
		"2026-10-01": time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local),
		"2026-10-04": time.Date(2026, 10, 4, 0, 0, 0, 0, time.Local),
		"36500d":     now.Add(-36500 * 24 * time.Hour),
	} {
		got, err := statsParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("statsParseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "x", "0d", "-3d", "7", "1.5d", "d", "0h", "-2h", "2026-13-01", "2026-10-05", "36501d", "2026-10-4"} {
		if got, err := statsParseSince(in, now); err == nil {
			t.Errorf("statsParseSince(%q) = %v, want an error", in, got)
		}
	}
}

func TestStatsRankIsNearestRank(t *testing.T) {
	min10 := func(n ...int) []time.Duration {
		var out []time.Duration
		for _, m := range n {
			out = append(out, time.Duration(m)*time.Minute)
		}
		return out
	}
	for _, c := range []struct {
		spans    []time.Duration
		med, p90 time.Duration
	}{
		{min10(10, 20, 30), 20 * time.Minute, 30 * time.Minute},
		{min10(7), 7 * time.Minute, 7 * time.Minute},
		{min10(1, 2), time.Minute, 2 * time.Minute},
		{min10(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 5 * time.Minute, 9 * time.Minute},
	} {
		if got := statsRank(c.spans, 50); got != c.med {
			t.Errorf("median of %v = %v, want %v", c.spans, got, c.med)
		}
		if got := statsRank(c.spans, 90); got != c.p90 {
			t.Errorf("p90 of %v = %v, want %v", c.spans, got, c.p90)
		}
	}
}

// statsRR builds one run of a synthetic round for statsCompute.
func statsRR(id string, pr int64, number, round int, role, state string, created time.Time) store.RoundRun {
	return store.RoundRun{Repo: "talkable/talkable", Number: number,
		Run: store.Run{ID: id, PRID: pr, Round: round, Role: role, Kind: store.RunInitial, State: state, CreatedAt: created}}
}

func statsEnd(at time.Time, subject, msg, data string) store.Event {
	ev := store.Event{At: at, Subject: &subject, Kind: "round.end", Message: msg}
	if data != "" {
		ev.Data = json.RawMessage(data)
	}
	return ev
}

func TestStatsComputeEdges(t *testing.T) {
	t0 := statsOct(3, 10, 0)
	since := t0.Add(-time.Hour)
	isJudge := func(role string) bool { return role == store.RoleJudge }

	// Round 2 of PR 5: three judge runs; the result of the last one that has
	// a result counts, and the latest round.end (its Data) decides the outcome.
	earlier := statsRR("j1", 1, 5, 2, store.RoleJudge, store.RunVerified, t0)
	earlier.ResultJSON = store.Ptr(`{"findings":{"P0":9}}`)
	later := statsRR("j2", 1, 5, 2, store.RoleJudge, store.RunVerified, t0.Add(time.Hour))
	later.ResultJSON = store.Ptr(`{"findings":{"P1":1,"p2":"2","P3":2.5}}`)
	noResult := statsRR("j3", 1, 5, 2, store.RoleJudge, store.RunVerified, t0.Add(2*time.Hour))
	// Round 3: only a message tells the outcome (a trailing error text follows it).
	msgOnly := statsRR("m1", 1, 5, 3, store.RoleClaude, store.RunFailed, t0)
	// Round 1 of PR 6: a negative span and no judge; nothing finished is
	// timed, a missing end time gives no round duration.
	skew := statsRR("s1", 2, 6, 1, store.RoleClaude, store.RunVerified, t0)
	skew.SubmittedAt, skew.EndedAt = statsOpt(t0.Add(10*time.Minute)), statsOpt(t0.Add(5*time.Minute))
	noEnd := statsRR("s2", 2, 6, 1, store.RoleCodexReview, store.RunAbandoned, t0)
	// Round 1 of PR 7: abandoned without any end time and no outcome.
	lost := statsRR("l1", 3, 7, 1, store.RoleClaude, store.RunAbandoned, t0)
	events := []store.Event{
		statsEnd(t0.Add(time.Minute), "pr:talkable/talkable#5", "round 2 ended: error: boom", `{"outcome":"error"}`),
		statsEnd(t0.Add(3*time.Hour), "pr:talkable/talkable#5", "round 2 ended: posted", `{"outcome":"posted"}`),
		statsEnd(t0.Add(time.Minute), "pr:talkable/talkable#5", "round 3 ended: needs_attention: no result", ""),
		statsEnd(t0.Add(time.Minute), "pr:talkable/talkable#5", "not a round end", `{"outcome":"posted"}`),
		{At: t0, Kind: "round.end", Message: "round 1 ended: posted"}, // no subject
	}
	runs := []store.RoundRun{earlier, later, noResult, msgOnly, skew, noEnd, lost}

	r := statsCompute(runs, nil, events, isJudge, since, t0.Add(4*time.Hour), "")
	if len(r.Groups) != 1 {
		t.Fatalf("groups = %s", statsJSON(t, r.Groups))
	}
	tot := r.Total
	if tot.Rounds.Started != 4 || tot.Rounds.Posted != 1 ||
		!reflect.DeepEqual(tot.Rounds.Outcomes, map[string]int{"posted": 1, "needs_attention": 1, "unknown": 2}) {
		t.Errorf("rounds = %+v", tot.Rounds)
	}
	if !reflect.DeepEqual(tot.FindingsPosted, map[string]int{"P1": 1, "P2": 2}) {
		t.Errorf("findings_posted = %v", tot.FindingsPosted)
	}
	// Spans: only PR 6 has end times. Its claude-review span (submitted after
	// it ended) is clamped to 0; its round runs from the round's first run to
	// that end. The judge runs of PR 5 never ended, so nothing of them is timed.
	if d, ok := tot.Durations["claude-review"]; !ok || d.N != 1 || d.MedianSeconds != 0 {
		t.Errorf("negative span = %+v (%v)", d, ok)
	}
	if d, ok := tot.Durations["round"]; !ok || d.N != 1 || d.MedianSeconds != 300 {
		t.Errorf("round of the skewed run = %+v (%v)", d, ok)
	}
	if _, ok := tot.Durations[store.RoleJudge]; ok {
		t.Errorf("a judge without end times has a duration: %+v", tot.Durations)
	}
	if _, ok := tot.Durations["codex-review"]; ok {
		t.Errorf("a role without an end time has a duration: %+v", tot.Durations)
	}

	// A nil judge predicate finds no judge runs: no findings counted.
	r = statsCompute(runs, nil, events, nil, since, t0.Add(4*time.Hour), "")
	if len(r.Total.FindingsPosted) != 0 || r.Total.Rounds.Posted != 1 {
		t.Errorf("without a judge: %+v", r.Total)
	}
	// An unrelated repository leaves nothing.
	if r = statsCompute(runs, nil, events, isJudge, since, t0, "example/widgets"); len(r.Groups) != 0 || r.Total.Rounds.Started != 0 {
		t.Errorf("other repo: %s", statsJSON(t, r))
	}
}

func TestStatsComputeSourcesDeduplicatesAndSkipsEmpty(t *testing.T) {
	t0 := statsOct(3, 10, 0)
	fs := []store.Finding{
		{FindingID: "A", Sources: []string{"claude-review", "claude-review"}, Verdict: store.FindingPosted, CreatedAt: t0, Repo: "talkable/talkable"},
		{FindingID: "B", Sources: nil, Verdict: store.FindingPosted, CreatedAt: t0, Repo: "talkable/talkable"},
		{FindingID: "C", Sources: []string{"codex-review"}, Verdict: store.FindingRejected, ReasonCode: "duplicate", CreatedAt: t0, Repo: "example/widgets"},
	}
	r := statsCompute(nil, fs, nil, nil, t0.Add(-time.Hour), t0.Add(time.Hour), "")
	if len(r.Groups) != 2 {
		t.Fatalf("groups = %s", statsJSON(t, r.Groups))
	}
	if s := r.Total.Sources["claude-review"]; s == nil || s.Judged != 1 || s.Posted != 1 || s.Unique != 1 {
		t.Errorf("a source named twice counts once: %+v", s)
	}
	if s := r.Total.Sources["codex-review"]; s == nil || s.Rejected != 1 || s.Reasons["duplicate"] != 1 || s.Posted != 0 {
		t.Errorf("rejected source: %+v", s)
	}
}

func TestStatsSubjectRepo(t *testing.T) {
	for in, want := range map[string]string{
		"pr:talkable/talkable#101": "talkable/talkable",
		"pr:example/widgets#7":     "example/widgets",
	} {
		if got, ok := statsSubjectRepo(in); !ok || got != want {
			t.Errorf("statsSubjectRepo(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "pr:", "pr:oops", "pr:7", "pr:a/b", "pr:a/b#x", "pr:a/b#0", "pr:/b#1", "pr:a/#1", "pr:a/b/c#1", "slot:review1", "request:7"} {
		if got, ok := statsSubjectRepo(in); ok {
			t.Errorf("statsSubjectRepo(%q) = %q, want none", in, got)
		}
	}
}

// statsKeys are the sorted keys of a decoded JSON object.
func statsKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func statsObj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v is not a JSON object", v)
	}
	return m
}

func TestStatsCommandJSON(t *testing.T) {
	f := statsFixture(t)
	if code := f.run("stats", "--json"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(f.Out.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, f.Out.String())
	}
	if got := statsKeys(doc); !slices.Equal(got, []string{"groups", "since", "total", "until"}) {
		t.Fatalf("top-level keys = %v", got)
	}
	if since, err := time.Parse(time.RFC3339, doc["since"].(string)); err != nil || !since.Equal(statsNow.Add(-7*24*time.Hour)) {
		t.Fatalf("since = %v (%v)", doc["since"], err)
	}
	if until, err := time.Parse(time.RFC3339, doc["until"].(string)); err != nil || !until.Equal(statsNow) {
		t.Fatalf("until = %v (%v)", doc["until"], err)
	}
	groups := doc["groups"].([]any)
	if len(groups) != 4 {
		t.Fatalf("%d groups", len(groups))
	}
	d1, _, _ := statsDays()
	g1 := statsObj(t, groups[1]) // day 1, talkable/talkable: every section
	if g1["day"] != d1 || g1["repo"] != "talkable/talkable" {
		t.Fatalf("group 1 = %v", g1)
	}
	want := []string{"day", "denies", "durations", "findings_posted", "model_switches", "repo", "restarts", "rounds", "sources"}
	if got := statsKeys(g1); !slices.Equal(got, want) {
		t.Fatalf("group keys = %v, want %v", got, want)
	}
	if got := statsKeys(statsObj(t, g1["rounds"])); !slices.Equal(got, []string{"outcomes", "posted", "started"}) {
		t.Errorf("rounds keys = %v", got)
	}
	dur := statsObj(t, statsObj(t, g1["durations"])["claude-review"])
	if got := statsKeys(dur); !slices.Equal(got, []string{"median_seconds", "n", "p90_seconds"}) || dur["median_seconds"] != 600.0 {
		t.Errorf("duration = %v", dur)
	}
	src := statsObj(t, statsObj(t, g1["sources"])["claude-review"])
	if got := statsKeys(src); !slices.Equal(got, []string{"judged", "posted", "reasons", "rejected", "unique"}) {
		t.Errorf("source keys = %v", got)
	}
	if got := statsObj(t, g1["findings_posted"]); got["P1"] != 1.0 || got["P2"] != 2.0 || len(got) != 2 {
		t.Errorf("findings_posted = %v", got)
	}
	// A group with operations only keeps the counts and drops the empty maps.
	g3 := statsObj(t, groups[3])
	if got := statsKeys(g3); !slices.Equal(got, []string{"day", "denies", "model_switches", "repo", "restarts", "rounds"}) {
		t.Errorf("operations-only group keys = %v", got)
	}
	if got := statsObj(t, g3["rounds"]); got["started"] != 0.0 || got["posted"] != 0.0 || len(got) != 2 {
		t.Errorf("empty rounds = %v", got)
	}
	// The total has no day or repository.
	tot := statsObj(t, doc["total"])
	want = []string{"denies", "durations", "findings_posted", "model_switches", "restarts", "rounds", "sources"}
	if got := statsKeys(tot); !slices.Equal(got, want) {
		t.Errorf("total keys = %v, want %v", got, want)
	}
	if statsObj(t, tot["rounds"])["started"] != 5.0 || tot["denies"] != 4.0 {
		t.Errorf("total = %v", tot)
	}

	// --repo keeps one repository (a bare name takes daemon.default_repo's owner) and says so.
	for _, arg := range []string{"talkable/talkable", "talkable"} {
		if code := f.run("stats", "--json", "--repo", arg); code != 0 {
			t.Fatalf("--repo %s: code %d, stderr %s", arg, code, f.Err.String())
		}
		doc = nil
		if err := json.Unmarshal(f.Out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc["repo"] != "talkable/talkable" || len(doc["groups"].([]any)) != 2 ||
			statsObj(t, statsObj(t, doc["total"])["rounds"])["started"] != 4.0 {
			t.Errorf("--repo %s: %s", arg, f.Out.String())
		}
	}
}

// statsLines are the output's lines with runs of blanks folded to one space.
func statsLines(out string) []string {
	var lines []string
	for l := range strings.SplitSeq(out, "\n") {
		lines = append(lines, strings.Join(strings.Fields(l), " "))
	}
	return lines
}

func TestStatsCommandText(t *testing.T) {
	f := statsFixture(t)
	if code := f.run("stats"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	out := f.Out.String()
	d1, d2, d3 := statsDays()
	lines := statsLines(out)
	header := "stats since " + statsNow.Add(-7*24*time.Hour).Format(statsTimeFmt) + " (7d), all repositories"
	if lines[0] != header {
		t.Fatalf("header = %q, want %q", lines[0], header)
	}
	want := []string{
		"ROUNDS",
		"DAY REPO STARTED POSTED OTHER",
		d1 + " example/widgets 1 0 error 1",
		d1 + " talkable/talkable 1 1",
		d2 + " talkable/talkable 3 1 running 1, stopped 1",
		"all 5 2 error 1, running 1, stopped 1",
		"FINDINGS POSTED",
		"DAY REPO P0 P1 P2 P3",
		d1 + " talkable/talkable 0 1 2 0",
		d2 + " talkable/talkable 0 0 3 1",
		"all 0 1 5 1",
		"DURATIONS",
		"ROLE N MEDIAN P90",
		"claude-review 3 20m 30m",
		"codex-judge 4 9m 18m",
		"codex-review 2 20m 40m",
		"round 4 36m 50m",
		"SOURCES",
		"SOURCE JUDGED POSTED UNIQUE ACCEPTED REJECTED",
		"claude-review 4 2 1 50% 2: duplicate 1, unspecified 1",
		"codex-review 3 2 1 67% 1: style_only 1",
		"judge 1 1 1 100% 0",
		"OPERATIONS",
		"DAY REPO MODEL SWITCHES DENIES RESTARTS",
		d1 + " example/widgets 0 1 1",
		d1 + " talkable/talkable 0 2 0",
		d2 + " talkable/talkable 1 0 1",
		d3 + " example/widgets 0 1 0",
		"all 1 4 2",
	}
	at := 0 // the rows come in this order
	for _, w := range want {
		i := slices.Index(lines[at:], w)
		if i < 0 {
			t.Fatalf("missing line %q (after line %d) in\n%s", w, at, out)
		}
		at += i + 1
	}
	// A day with operations only has no rounds row.
	if slices.Contains(lines, d3+" example/widgets 0 0") {
		t.Errorf("empty rounds row in\n%s", out)
	}
	// With --repo the header names the repository. 3d back from the morning
	// of day 4 reaches round F, the only round of example/widgets.
	if code := f.run("stats", "--repo", "example/widgets", "--since", "3d"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	lines = statsLines(f.Out.String())
	if want := "stats since " + statsNow.Add(-3*24*time.Hour).Format(statsTimeFmt) + " (3d), example/widgets"; lines[0] != want {
		t.Errorf("header = %q, want %q", lines[0], want)
	}
	if !slices.Contains(lines, "all 1 0 error 1") || !slices.Contains(lines, "codex-review 1 40m 40m") {
		t.Errorf("--since 3d --repo example/widgets:\n%s", f.Out.String())
	}
	if strings.Contains(f.Out.String(), "talkable/talkable") {
		t.Errorf("other repository in\n%s", f.Out.String())
	}
	// 36h back reaches only day 4: the morning's denied prompt, no round.
	if code := f.run("stats", "--repo", "example/widgets", "--since", "36h"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	lines = statsLines(f.Out.String())
	if want := "stats since " + statsNow.Add(-36*time.Hour).Format(statsTimeFmt) + " (36h), example/widgets"; lines[0] != want {
		t.Errorf("header = %q, want %q", lines[0], want)
	}
	if !slices.Contains(lines, "all 0 1 0") || slices.Contains(lines, "ROUNDS") {
		t.Errorf("--since 36h --repo example/widgets:\n%s", f.Out.String())
	}
}

func TestStatsCommandNothingInTheWindow(t *testing.T) {
	f := newInspFixture(t)
	prev := inspNow
	inspNow = func() time.Time { return statsNow }
	t.Cleanup(func() { inspNow = prev })
	f.store().Close()
	if code := f.run("stats"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	since := statsNow.Add(-7 * 24 * time.Hour).Format(statsTimeFmt)
	want := "stats since " + since + " (7d), all repositories\n\nno rounds since " + since + "\n"
	if f.Out.String() != want {
		t.Fatalf("out = %q, want %q", f.Out.String(), want)
	}
	// The seeded registry has nothing in a window that ended before it.
	g := statsFixture(t)
	if code := g.run("stats", "--since", "2026-10-04"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, g.Err.String())
	}
	if out := g.Out.String(); !strings.Contains(out, "OPERATIONS") || strings.Contains(out, "ROUNDS") {
		t.Fatalf("a window with operations only:\n%s", out)
	}
	if code := g.run("stats", "--json", "--since", "30m"); code != 0 || !strings.Contains(g.Out.String(), `"groups": []`) {
		t.Fatalf("empty window json (code %d):\n%s", code, g.Out.String())
	}
}

func TestStatsUsageErrors(t *testing.T) {
	f := statsFixture(t)
	for name, args := range map[string][]string{
		"bad since":         {"--since", "soon"},
		"zero days":         {"--since", "0d"},
		"future date":       {"--since", "2026-10-05"},
		"positional":        {"extra"},
		"repo with slashes": {"--repo", "a/b/c"},
	} {
		if code := f.run("stats", args...); code != 2 {
			t.Errorf("%s: code %d, want 2 (stderr %s)", name, code, f.Err.String())
			continue
		}
		if !strings.Contains(f.Err.String(), "usage: magnum stats ") || f.Out.Len() != 0 {
			t.Errorf("%s: stderr %q, stdout %q", name, f.Err.String(), f.Out.String())
		}
	}
	if code := f.run("stats", "--nope"); code != 2 {
		t.Errorf("unknown flag: code %d, want 2", code)
	}
}
