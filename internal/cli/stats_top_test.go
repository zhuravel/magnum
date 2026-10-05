package cli

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// statsTimes are PRs with the agent time given in minutes, most first as
// the store returns them.
func statsTimes(repo string, minutes ...int) []store.PRAgentTime {
	var out []store.PRAgentTime
	for i, m := range minutes {
		out = append(out, store.PRAgentTime{PRID: int64(i + 1), Repo: repo, Number: 100 + i, Time: time.Duration(m) * time.Minute, Rounds: i + 1})
	}
	return out
}

func TestStatsTopKeepsTenPRsWithTheirShareOfAllAgentTime(t *testing.T) {
	// 12 PRs of 120, 110, ... 10 minutes: 780 minutes in all.
	times := statsTimes("talkable/talkable", 120, 110, 100, 90, 80, 70, 60, 50, 40, 30, 20, 10)
	top, total := statsTop(times, "")
	if total != 780*time.Minute {
		t.Fatalf("total = %v, want 13h0m0s (the PRs past the tenth count too)", total)
	}
	if len(top) != statsTopLimit || statsTopLimit != 10 {
		t.Fatalf("%d PRs, want 10", len(top))
	}
	first := statsTopPR{Repo: "talkable/talkable", Number: 100, AgentSeconds: 120 * 60, Rounds: 1, Share: 0.1538}
	if top[0] != first {
		t.Errorf("first = %+v, want %+v", top[0], first)
	}
	if last := top[9]; last.Number != 109 || last.AgentSeconds != 30*60 || last.Share != 0.0385 {
		t.Errorf("tenth = %+v", last)
	}
}

func TestStatsTopSkipsPRsWithoutAgentTimeAndKeepsOneRepository(t *testing.T) {
	times := []store.PRAgentTime{
		{PRID: 1, Repo: "talkable/talkable", Number: 5, Time: 30 * time.Minute, Rounds: 2},
		{PRID: 2, Repo: "example/widgets", Number: 7, Time: 10 * time.Minute, Rounds: 1},
		{PRID: 3, Repo: "example/widgets", Number: 8, Time: 30 * time.Minute, Rounds: 1},
		{PRID: 4, Repo: "example/widgets", Number: 9, Rounds: 1}, // a run that took no time
	}
	top, total := statsTop(times, "example/widgets")
	if total != 40*time.Minute {
		t.Fatalf("total = %v, want the repository's 40m", total)
	}
	want := []statsTopPR{
		{Repo: "example/widgets", Number: 8, AgentSeconds: 1800, Rounds: 1, Share: 0.75},
		{Repo: "example/widgets", Number: 7, AgentSeconds: 600, Rounds: 1, Share: 0.25},
	}
	if !reflect.DeepEqual(top, want) {
		t.Fatalf("top = %+v, want %+v (most time first)", top, want)
	}
	if none, total := statsTop(nil, ""); len(none) != 0 || total != 0 {
		t.Fatalf("no PRs = %+v, %v", none, total)
	}
	// Equal times keep a stable order: by repository, then number.
	tied, _ := statsTop([]store.PRAgentTime{
		{Repo: "talkable/talkable", Number: 2, Time: time.Minute}, {Repo: "example/widgets", Number: 9, Time: time.Minute},
		{Repo: "talkable/talkable", Number: 1, Time: time.Minute}}, "")
	var refs []string
	for _, p := range tied {
		refs = append(refs, p.Repo+"#"+strconv.Itoa(p.Number))
	}
	if want := []string{"example/widgets#9", "talkable/talkable#1", "talkable/talkable#2"}; !slices.Equal(refs, want) {
		t.Fatalf("tied order = %v, want %v", refs, want)
	}
}

// statsGather adds the PRs' agent time: the runs created in the window, each
// to the end of its work, a run still going to now.
func TestStatsGatherListsTheTopPRsByAgentTime(t *testing.T) {
	f := statsFixture(t)
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	since := statsNow.Add(-7 * 24 * time.Hour)
	r, err := statsGather(context.Background(), f.store(), f.Ctx.Config, since, statsNow, "")
	if err != nil {
		t.Fatal(err)
	}
	// 104's claude run has worked since day 3 14:00, 20 hours to statsNow;
	// 105's judge ran 10 minutes in the window (its other run is before it).
	want := []statsTopPR{
		{Repo: "talkable/talkable", Number: 104, AgentSeconds: 20 * 3600, Rounds: 1, Share: 0.8753},
		{Repo: "example/widgets", Number: 7, AgentSeconds: 49 * 60, Rounds: 1, Share: 0.0357},
		{Repo: "talkable/talkable", Number: 101, AgentSeconds: 40 * 60, Rounds: 1, Share: 0.0292},
		{Repo: "talkable/talkable", Number: 102, AgentSeconds: 38 * 60, Rounds: 1, Share: 0.0277},
		{Repo: "talkable/talkable", Number: 103, AgentSeconds: 34 * 60, Rounds: 1, Share: 0.0248},
		{Repo: "talkable/talkable", Number: 105, AgentSeconds: 10 * 60, Rounds: 1, Share: 0.0073},
	}
	if !reflect.DeepEqual(r.TopPRs, want) || r.AgentSeconds != 82260 {
		t.Fatalf("top PRs = %+v (%ds), want %+v (82260s)", r.TopPRs, r.AgentSeconds, want)
	}

	one, err := statsGather(context.Background(), f.store(), f.Ctx.Config, since, statsNow, "example/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if want := []statsTopPR{{Repo: "example/widgets", Number: 7, AgentSeconds: 49 * 60, Rounds: 1, Share: 1}}; !reflect.DeepEqual(one.TopPRs, want) || one.AgentSeconds != 49*60 {
		t.Fatalf("--repo top PRs = %+v (%ds), want %+v", one.TopPRs, one.AgentSeconds, want)
	}

	// A window that holds no run has an empty list, not a missing one.
	late, err := statsGather(context.Background(), f.store(), f.Ctx.Config, statsNow.Add(-time.Minute), statsNow, "")
	if err != nil || late.TopPRs == nil || len(late.TopPRs) != 0 || late.AgentSeconds != 0 {
		t.Fatalf("empty window: %+v, %v", late.TopPRs, err)
	}
}

func TestStatsCommandTextHasTheTopPRsSection(t *testing.T) {
	f := statsFixture(t)
	if code := f.run("stats"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	lines := statsLines(f.Out.String())
	at := slices.Index(lines, "TOP PRS BY AGENT TIME")
	if at < 0 {
		t.Fatalf("no TOP PRS BY AGENT TIME section in\n%s", f.Out.String())
	}
	want := []string{
		"PR AGENT TIME ROUNDS SHARE",
		"talkable/talkable#104 20h 1 88%",
		"example/widgets#7 49m 1 4%",
		"talkable/talkable#101 40m 1 3%",
		"talkable/talkable#102 38m 1 3%",
		"talkable/talkable#103 34m 1 2%",
		"talkable/talkable#105 10m 1 1%",
		"",
	}
	if got := lines[at+1 : at+1+len(want)]; !slices.Equal(got, want) {
		t.Fatalf("section =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// With --repo the section keeps that repository, shares of its own time.
	if code := f.run("stats", "--repo", "example/widgets"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	lines = statsLines(f.Out.String())
	if !slices.Contains(lines, "example/widgets#7 49m 1 100%") || slices.Contains(lines, "talkable/talkable#104 20h 1 88%") {
		t.Errorf("--repo example/widgets:\n%s", f.Out.String())
	}
}

func TestStatsCommandJSONHasTheTopPRs(t *testing.T) {
	f := statsFixture(t)
	if code := f.run("stats", "--json"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(f.Out.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, f.Out.String())
	}
	if doc["agent_seconds"] != 82260.0 {
		t.Errorf("agent_seconds = %v", doc["agent_seconds"])
	}
	top, ok := doc["top_prs"].([]any)
	if !ok || len(top) != 6 {
		t.Fatalf("top_prs = %v", doc["top_prs"])
	}
	first := statsObj(t, top[0])
	if got := statsKeys(first); !slices.Equal(got, []string{"agent_seconds", "number", "repo", "rounds", "share"}) {
		t.Errorf("top PR keys = %v", got)
	}
	if first["repo"] != "talkable/talkable" || first["number"] != 104.0 || first["agent_seconds"] != 72000.0 ||
		first["rounds"] != 1.0 || first["share"] != 0.8753 {
		t.Errorf("first top PR = %v", first)
	}
}
