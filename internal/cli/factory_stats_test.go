package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/factory/stats"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// seedStatsTask makes a task in the factory registry of dt, created at when
// (the zero time: now), with the typed messages and that many revisions of
// questions asked and answered.
func seedStatsTask(t *testing.T, dt *daemonGroupTest, key string, when time.Time, typed []string, answered int) registry.Task {
	t.Helper()
	registrytest.Seed(t, dt.ctx.Layout.FactoryDB())
	opts := registry.Options{}
	if !when.IsZero() {
		opts.Clock = func() time.Time { return when }
	}
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	ctx := context.Background()
	task, err := reg.CreateTask(ctx, registry.Task{JiraKey: key, Kind: "bug", Repo: "example/shop", Title: "Points vanish"})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range typed {
		if _, err := reg.RecordInput(ctx, registry.Input{TaskID: task.ID, Kind: registry.InputPaneTyped, Text: text,
			Origin: registry.OriginHerdr}); err != nil {
			t.Fatal(err)
		}
	}
	for rev := 1; rev <= answered; rev++ {
		if err := reg.ReplaceQuestions(ctx, task.ID, rev, []registry.Question{{Text: "Which one?", Recommended: -1, RaisedBy: "fx-reproducer"}}); err != nil {
			t.Fatal(err)
		}
		if err := reg.AnswerBatch(ctx, task.ID, rev, map[int]string{0: "The first"}); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

// seedDecision records a decision on the task that its card showed ago
// before.
func seedDecision(t *testing.T, dt *daemonGroupTest, taskID int64, ago time.Duration) {
	t.Helper()
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	at := time.Now()
	if err := reg.RecordDecision(context.Background(), registry.Decision{TaskID: taskID, Kind: "ready_push", Choice: "push", Shown: at.Add(-ago), At: at}); err != nil {
		t.Fatal(err)
	}
}

func freezeStatsBaseline(t *testing.T, l paths.Layout) {
	t.Helper()
	err := stats.FreezeBaseline(l.FactoryBaseline(), stats.Baseline{Frozen: time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC), Tickets: 12,
		Accuracy: 0.93, TypedPerTicket: stats.Dist{Median: 41, P25: 30, P75: 77.5, Min: 12, Max: 120},
		StepStartsPerTicket: stats.Dist{Median: 9, P25: 5, P75: 14, Min: 1, Max: 30}})
	if err != nil {
		t.Fatal(err)
	}
}

// `magnum factory stats` prints the markdown report (--markdown spells the
// default) with each task set against the frozen baseline.
func TestFactoryStatsPrintsMarkdownAgainstTheBaseline(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	task := seedStatsTask(t, dt, "PR-12311", time.Time{}, []string{"open PR", "yes", "the refund path only matters for expired points"}, 3)
	seedDecision(t, dt, task.ID, 10*time.Minute)
	freezeStatsBaseline(t, dt.ctx.Layout)

	seedUnknownTurn(t, dt, task.ID, "open PR")

	if code := dt.run("factory", "stats"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	out := dt.stdout.String()
	for _, want := range []string{
		"# Factory stats",
		"| Access | Origin unknown | Answer batches |",
		"| PR-12311 | 3 | 1 | 1 | 0 | 0 | 1 | 3 | 1 | 0 | 10m00s | 0s | - |",
		"## Against the baseline",
		"12 tickets",
		"| PR-12311 | 3 | below the baseline's middle half (median 41, 30 to 77.5) | 1 | below the baseline's middle half (median 9, 5 to 14) |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}

	dt.stdout.Reset()
	if code := dt.run("factory", "stats", "--markdown"); code != 0 || dt.stdout.String() != out {
		t.Errorf("--markdown: exit %d, differs from the default:\n%s", code, dt.stdout)
	}
}

// Without a frozen baseline the report still prints, and says how to freeze
// one.
func TestFactoryStatsWithoutABaselineSaysHowToFreezeOne(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	seedStatsTask(t, dt, "PR-12312", time.Time{}, nil, 0)
	if code := dt.run("factory", "stats"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if out := dt.stdout.String(); !strings.Contains(out, "PR-12312") || !strings.Contains(out, "No baseline is frozen") {
		t.Errorf("report:\n%s", out)
	}
	if _, err := os.Stat(dt.ctx.Layout.FactoryBaseline()); !os.IsNotExist(err) {
		t.Errorf("stats wrote a baseline: %v", err)
	}
}

func TestFactoryStatsJSONHasTheTasksAndTheBaseline(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	task := seedStatsTask(t, dt, "PR-12313", time.Time{}, []string{"merge master", "push"}, 0)
	seedUnknownTurn(t, dt, task.ID, "open PR")
	freezeStatsBaseline(t, dt.ctx.Layout)

	if code := dt.run("factory", "stats", "--json"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	var got struct {
		Since *time.Time `json:"since"`
		Tasks []struct {
			Key           string `json:"key"`
			Typed         int    `json:"typed"`
			StepStarts    int    `json:"step_starts"`
			UnknownOrigin int    `json:"unknown_origin"`
		} `json:"tasks"`
		Baseline *struct {
			Tickets int `json:"tickets"`
		} `json:"baseline"`
	}
	if err := json.Unmarshal(dt.stdout.Bytes(), &got); err != nil {
		t.Fatalf("%v:\n%s", err, dt.stdout)
	}
	if got.Since != nil || len(got.Tasks) != 1 || got.Tasks[0].Key != "PR-12313" || got.Tasks[0].Typed != 2 || got.Tasks[0].StepStarts != 2 ||
		got.Tasks[0].UnknownOrigin != 1 || got.Baseline == nil || got.Baseline.Tickets != 12 {
		t.Errorf("report %+v, want the one task with two step starts, one turn of unknown origin apart, no window and the 12-ticket baseline", got)
	}
}

// seedUnknownTurn records a turn typed in a pane of task id whose origin
// nothing proves.
func seedUnknownTurn(t *testing.T, dt *daemonGroupTest, id int64, text string) {
	t.Helper()
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if _, err := reg.RecordInput(context.Background(), registry.Input{TaskID: id, Kind: registry.InputPaneTyped, Text: text,
		Origin: registry.OriginUnknown}); err != nil {
		t.Fatal(err)
	}
}

// --since keeps the tasks started in the window; the flags that do not go
// together, and a window that is none, are refused before anything is read.
func TestFactoryStatsSinceAndItsFlags(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	seedStatsTask(t, dt, "PR-12314", time.Now().Add(-30*24*time.Hour), nil, 0)
	seedStatsTask(t, dt, "PR-12315", time.Time{}, nil, 0)

	if code := dt.run("factory", "stats", "--since", "7d"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if out := dt.stdout.String(); !strings.Contains(out, "PR-12315") || strings.Contains(out, "PR-12314") {
		t.Errorf("--since 7d:\n%s", out)
	}
	dt.stdout.Reset()
	if code := dt.run("factory", "stats"); code != 0 || !strings.Contains(dt.stdout.String(), "PR-12314") {
		t.Errorf("without --since every task is counted: exit %d\n%s", code, dt.stdout)
	}

	for _, args := range [][]string{{"--json", "--markdown"}, {"--since", "soon"}, {"extra"}} {
		dt.stdout.Reset()
		dt.stderr.Reset()
		if code := dt.run("factory", append([]string{"stats"}, args...)...); code != 2 || dt.stdout.Len() != 0 {
			t.Errorf("stats %v: exit %d, stdout %q; want a usage error (2) and no report", args, code, dt.stdout)
		}
	}
}

// A factory that has no task yet has no registry, and stats creates none.
func TestFactoryStatsWithoutARegistryCreatesNothing(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	if code := dt.run("factory", "stats"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if out := dt.stdout.String(); !strings.Contains(out, "No tasks") {
		t.Errorf("report:\n%s", out)
	}
	for _, p := range []string{dt.ctx.Layout.FactoryDB(), dt.ctx.Layout.FactoryBaseline()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stats created %s: %v", p, err)
		}
	}
}

// Stats opens the registry as the other factory commands do: a registry that
// needs a migration is not migrated under a running factory daemon.
func TestFactoryStatsRefusesToMigrateUnderARunningDaemon(t *testing.T) {
	storetest.Serial(t)
	comm, args := daemonRulePS("/opt/bin/magnum", "/opt/bin/magnum factory daemon")
	dt := newDaemonGroupTest(t, comm, args)
	daemonSys.FactoryPID = func(paths.Layout) (int, error) { return 250, nil }
	db := dt.ctx.Layout.FactoryDB()
	writeFactoryRegistryAt(t, db, 1)

	if code := dt.run("factory", "stats"); code != 1 || !strings.Contains(dt.stderr.String(), "pid 250") {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if v := factorySchema(t, db); v != 1 {
		t.Fatalf("user_version %d after the refusal, want 1 (untouched)", v)
	}
}

func TestFactoryStatsRefusesABaselineItCannotRead(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	if err := os.MkdirAll(filepath.Dir(dt.ctx.Layout.FactoryBaseline()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dt.ctx.Layout.FactoryBaseline(), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := dt.run("factory", "stats"); code != 1 || !strings.Contains(dt.stderr.String(), "baseline.json") {
		t.Errorf("exit %d, stderr %s; want a refusal that names baseline.json", code, dt.stderr)
	}
}

// --- magnum factory baseline ---

// baselineFixture is a transcripts directory of two tickets (three sessions
// of the first), a labelled file, and a config that knows the tracker and
// your GitHub login.
func baselineFixture(t *testing.T, dt *daemonGroupTest) (transcripts, labelled string) {
	t.Helper()
	cfg := *dt.ctx.Config
	cfg.Board.Trackers = []string{"https://example.atlassian.net/browse/PR-{num}"}
	cfg.Identities = append(slices.Clone(cfg.Identities), config.Identity{Name: "me", Kind: "gh", Login: "zhuravel"})
	dt.ctx.Config = &cfg

	transcripts = filepath.Join(t.TempDir(), "transcripts")
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	line := func(offset time.Duration, text string) string {
		b, _ := json.Marshal(map[string]any{"type": "user", "timestamp": at.Add(offset).Format(time.RFC3339), "turnOrigin": "human",
			"message": map[string]any{"role": "user", "content": text}})
		return string(b)
	}
	write := func(name string, lines ...string) {
		t.Helper()
		p := filepath.Join(transcripts, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("proj/a.jsonl", line(0, "work on PR-12321, points vanish"), line(time.Minute, "open PR"), line(2*time.Minute, "yes"))
	write("proj/b.jsonl", line(time.Hour, "back to PR-12321"), line(time.Hour+time.Minute, "merge master"))
	write("proj/c.jsonl", line(2*time.Hour, "PR-12321 again: push"))
	write("proj/d.jsonl", line(3*time.Hour, "look at PR-12322"), line(3*time.Hour+time.Minute, "the mailer is slow"))
	write("proj/e.jsonl", line(4*time.Hour, "no ticket in this one"))

	labelled = filepath.Join(t.TempDir(), "labelled.jsonl")
	rows := `{"text": "open PR", "class": "step_start"}` + "\n" + `{"text": "push", "class": "step_start"}` + "\n" +
		`{"text": "yes", "class": "approval"}` + "\n" + `{"text": "the mailer is slow", "class": "info"}` + "\n"
	if err := os.WriteFile(labelled, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	return transcripts, labelled
}

func TestFactoryBaselineFreezesOnceFromTranscriptsPerTicket(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	transcripts, labelled := baselineFixture(t, dt)
	path := dt.ctx.Layout.FactoryBaseline()

	if code := dt.run("factory", "baseline", "--transcripts", transcripts, "--labelled", labelled); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	got, err := stats.LoadBaseline(path)
	if err != nil || got == nil {
		t.Fatalf("LoadBaseline = %v, %v", got, err)
	}
	// PR-12321: 3 + 2 + 1 messages over three sessions = 6; PR-12322: 2.
	if got.Tickets != 2 || got.TypedPerTicket.Max != 6 || got.TypedPerTicket.Min != 2 || got.StepStartsPerTicket.Max != 3 || got.Accuracy != 1 {
		t.Errorf("baseline %+v, want 2 tickets (four sessions of two keys), 6 and 2 typed, at most 3 step starts, accuracy 1", got)
	}
	out := dt.stdout.String()
	for _, want := range []string{path, "2 tickets", "4 sessions", "1 transcript skipped", "100%"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary lacks %q:\n%s", want, out)
		}
	}
	if dt.stderr.Len() != 0 {
		t.Errorf("stderr: %s", dt.stderr)
	}
	before, _ := os.ReadFile(path)

	// Once: the second run says so and leaves the file alone.
	dt.stdout.Reset()
	dt.stderr.Reset()
	if code := dt.run("factory", "baseline", "--transcripts", transcripts, "--labelled", labelled); code != 1 {
		t.Fatalf("a second freeze: exit %d, want 1", code)
	}
	for _, want := range []string{path, "frozen", "never rewrite"} {
		if !strings.Contains(dt.stderr.String(), want) {
			t.Errorf("the refusal lacks %q:\n%s", want, dt.stderr)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("the frozen baseline changed:\n%s\n%s", before, after)
	}
	if dt.stdout.Len() != 0 {
		t.Errorf("stdout of the refusal: %s", dt.stdout)
	}
}

// The sessions magnum and the factory start hold magnum's prompts, not your
// words: a session that started in magnum's data or state directory, in a
// [[pool]] slot (any number, not only up to max), in a per-PR worktree or in
// a factory copy, or below one, is magnum's; your own clones and worktrees
// beside them are not.
func TestBaselineKnowsWhereMagnumStartsSessions(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Pools:   []config.Pool{{Repo: "example/shop", SlotName: "review{n}", SlotPath: "/p/shop.review{n}", Max: 2}},
		Factory: config.Factory{Repos: []config.FactoryRepo{{Repo: "example/shop", Copies: "/p/copies/shop{n}", MaxCopies: 2}}},
	}
	magnums := baselineMagnumDir(paths.Layout{DataDir: "/x/share/magnum", StateDir: "/x/state/magnum"}, cfg)
	for dir, want := range map[string]bool{
		"/x/share/magnum":                     true,
		"/x/share/magnum/notes/.curate-shop":  true,
		"/x/share/magnum/learn/retro":         true,
		"/x/state/magnum/eval/wt/case-1":      true,
		"/p/shop.review3":                     true,
		"/p/shop.review12/app":                true,
		"/p/copies/shop2/lib/":                true,
		"/p/shop__worktrees/pr-12":            true,
		"/p/shop__worktrees/pr-12/app/models": true,
		"/p/shop":                             false,
		"/p/shop.review":                      false,
		"/p/shop.review3x":                    false,
		"/p/shop.repo1":                       false,
		"/p/shop__worktrees/feature-points":   false,
		"/p/copies":                           false,
		"/x/share/magnum-notes":               false,
		"/x/share":                            false,
		"notes":                               false,
		"":                                    false,
	} {
		if got := magnums(dir); got != want {
			t.Errorf("magnum's (%q) = %v, want %v", dir, got, want)
		}
	}
}

// The command reads only your interactive sessions, and says how many it
// left out for each reason, beside those with no ticket key or no message
// of yours.
func TestFactoryBaselineNamesWhyItSkippedEachTranscript(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	transcripts, labelled := baselineFixture(t, dt)
	cfg := *dt.ctx.Config
	slots := t.TempDir()
	cfg.Pools = []config.Pool{{Repo: "example/shop", SlotName: "review{n}", SlotPath: filepath.Join(slots, "shop.review{n}"), Max: 2}}
	dt.ctx.Config = &cfg
	at := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	write := func(name string, lines ...string) {
		t.Helper()
		p := filepath.Join(transcripts, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	line := func(v map[string]any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	prompt := func(entrypoint, cwd, text string) string {
		return line(map[string]any{"type": "user", "timestamp": at.Format(time.RFC3339), "turnOrigin": "human", "entrypoint": entrypoint,
			"cwd": cwd, "message": map[string]any{"role": "user", "content": text}})
	}
	rollout := func(originator string, source any, text string) []string {
		return []string{
			line(map[string]any{"type": "session_meta", "payload": map[string]any{"originator": originator, "source": source, "cwd": "/work/shop"}}),
			line(map[string]any{"type": "event_msg", "timestamp": at.Format(time.RFC3339), "payload": map[string]any{"type": "user_message", "message": text}}),
		}
	}
	write("proj/p.jsonl", prompt("sdk-cli", "/work/shop", "PR-12331: summarize the diff"))
	write("proj/0b8a/subagents/agent-a1.jsonl", prompt("cli", "/work/shop", "PR-12332: find the callers"))
	write("codex/rollout-2026-10-02T10-00-00-a.jsonl", rollout("Claude Code", "exec", "PR-12333: review the change")...)
	write("codex/rollout-2026-10-02T10-00-01-b.jsonl", rollout("codex-tui", map[string]any{"subagent": "review"}, "PR-12334: review")...)
	write("slot/a.jsonl", prompt("cli", filepath.Join(slots, "shop.review7"), "PR-12335: the round's prompt"))
	write("notes/a.jsonl", prompt("cli", filepath.Join(dt.ctx.Layout.Data(), "notes", ".curate-shop"), "PR-12336: curate"))
	write("proj/kept.jsonl", prompt("cli", "/work/shop", "PR-12337: the points vanish"))

	if code := dt.run("factory", "baseline", "--transcripts", transcripts, "--labelled", labelled); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	got, err := stats.LoadBaseline(dt.ctx.Layout.FactoryBaseline())
	if err != nil || got == nil || got.Tickets != 3 {
		t.Fatalf("baseline %+v, %v; want 3 tickets (the fixture's two and PR-12337)", got, err)
	}
	out := dt.stdout.String()
	for _, want := range []string{
		"3 tickets, from 5 sessions; 7 transcripts skipped",
		"1 with no ticket key", "0 with no message of yours",
		"1 headless", "2 of subagents", "1 Codex session Claude Code started",
		"2 started where magnum or the factory starts its own",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary lacks %q:\n%s", want, out)
		}
	}
}

// Ruling: only your own tickets count. A ticket counts when one of its
// sessions ran on your own branch (your GitHub login and a dash, from the
// config's identity, or --branch-prefix) or began by naming its key; a
// ticket whose sessions all ran on a colleague's branch without that is
// someone else's, skipped and counted apart.
func TestFactoryBaselineCountsOnlyYourOwnTickets(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	transcripts, labelled := baselineFixture(t, dt)
	at := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	write := func(name, branch string, texts ...string) {
		t.Helper()
		var lines []string
		for i, text := range texts {
			b, _ := json.Marshal(map[string]any{"type": "user", "timestamp": at.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
				"turnOrigin": "human", "entrypoint": "cli", "cwd": "/work/shop", "gitBranch": branch,
				"message": map[string]any{"role": "user", "content": text}})
			lines = append(lines, string(b))
		}
		p := filepath.Join(transcripts, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("own/a.jsonl", "zhuravel-PR-12341-points", "look at the points", "open PR", "push")
	write("colleague/a.jsonl", "alice-PR-12342", "check the mailer")
	write("colleague/b.jsonl", "alice-PR-12343", "PR-12343: the export for alice", "yes")
	path := dt.ctx.Layout.FactoryBaseline()

	// Your login, zhuravel: PR-12341 is yours by its branch, PR-12343 by its
	// first message, PR-12342 is alice's. Typed: 6, 2, 3 and 2.
	if code := dt.run("factory", "baseline", "--transcripts", transcripts, "--labelled", labelled); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	got, err := stats.LoadBaseline(path)
	if err != nil || got == nil || got.Tickets != 4 || got.TypedPerTicket.Min != 2 {
		t.Fatalf("baseline %+v, %v; want 4 tickets, the fewest of 2 messages (alice's PR-12342 skipped)", got, err)
	}
	if out := dt.stdout.String(); !strings.Contains(out, "4 tickets, from 6 sessions") || !strings.Contains(out, "others' tickets: 1 skipped") {
		t.Errorf("the summary lacks the tickets or the others' tickets:\n%s", out)
	}

	// --branch-prefix wins over the login: as alice-, PR-12342 counts and
	// PR-12341 is someone else's.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	dt.stdout.Reset()
	if code := dt.run("factory", "baseline", "--transcripts", transcripts, "--labelled", labelled, "--branch-prefix", "alice-"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if got, err := stats.LoadBaseline(path); err != nil || got == nil || got.Tickets != 4 || got.TypedPerTicket.Min != 1 {
		t.Errorf("baseline %+v, %v; want 4 tickets, the fewest of 1 message (PR-12342)", got, err)
	}
}

// Without your login there is no telling your branches from others', so the
// command refuses before it reads anything, and names the flag that says it.
func TestFactoryBaselineNeedsYourBranchPrefix(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	transcripts, labelled := baselineFixture(t, dt)
	cfg := *dt.ctx.Config
	cfg.Identities = nil
	dt.ctx.Config = &cfg
	if code := dt.run("factory", "baseline", "--transcripts", transcripts, "--labelled", labelled); code != 1 ||
		!strings.Contains(dt.stderr.String(), "--branch-prefix") {
		t.Errorf("exit %d, stderr %q; want a refusal that names --branch-prefix", code, dt.stderr)
	}
	if _, err := os.Stat(dt.ctx.Layout.FactoryBaseline()); !os.IsNotExist(err) {
		t.Fatalf("a baseline was written: %v", err)
	}
	dt.stderr.Reset()
	if code := dt.run("factory", "baseline", "--transcripts", transcripts, "--labelled", labelled, "--branch-prefix", "zhuravel-"); code != 0 {
		t.Errorf("with --branch-prefix: exit %d, stderr: %s", code, dt.stderr)
	}
}

// The existing file is the first thing the command looks at, before it reads
// a transcript.
func TestFactoryBaselineRefusesBeforeItReadsAnything(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	freezeStatsBaseline(t, dt.ctx.Layout)
	if code := dt.run("factory", "baseline", "--transcripts", filepath.Join(t.TempDir(), "nope"),
		"--labelled", filepath.Join(t.TempDir(), "nope.jsonl")); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if e := dt.stderr.String(); !strings.Contains(e, "frozen") || strings.Contains(e, "nope") {
		t.Errorf("stderr should say the baseline is frozen, not that the inputs are missing:\n%s", e)
	}
}

func TestFactoryBaselineWritesNothingWhenItCannotMeasure(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	transcripts, labelled := baselineFixture(t, dt)
	if err := os.WriteFile(labelled, []byte(`{"text": "open PR", "class": "chatter"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"a bad labelled file":       {"--transcripts", transcripts, "--labelled", labelled},
		"a missing transcripts dir": {"--transcripts", filepath.Join(t.TempDir(), "nope"), "--labelled", labelled},
	} {
		dt.stderr.Reset()
		if code := dt.run("factory", append([]string{"baseline"}, args...)...); code != 1 || dt.stderr.Len() == 0 {
			t.Errorf("%s: exit %d, stderr %q; want a failure that says why", name, code, dt.stderr)
		}
	}
	// No tracker, no way to tell a ticket from a word.
	cfg := *dt.ctx.Config
	cfg.Board.Trackers = nil
	dt.ctx.Config = &cfg
	fine := filepath.Join(t.TempDir(), "fine.jsonl")
	if err := os.WriteFile(fine, []byte(`{"text": "open PR", "class": "step_start"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dt.stderr.Reset()
	if code := dt.run("factory", "baseline", "--transcripts", transcripts, "--labelled", fine); code != 1 ||
		!strings.Contains(dt.stderr.String(), "trackers") {
		t.Errorf("without a tracker: exit %d, stderr %q; want one that names trackers", code, dt.stderr)
	}
	if _, err := os.Stat(dt.ctx.Layout.FactoryBaseline()); !os.IsNotExist(err) {
		t.Errorf("a baseline was written: %v", err)
	}
}

// A classifier that gets the operator's labels wrong is a baseline to
// distrust, so the command says so (it is a file the operator can move away
// and freeze again).
func TestFactoryBaselineWarnsOfAWeakClassifier(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	transcripts, labelled := baselineFixture(t, dt)
	rows := `{"text": "open PR", "class": "step_start"}` + "\n" + `{"text": "the mailer is slow", "class": "correction"}` + "\n"
	if err := os.WriteFile(labelled, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := dt.run("factory", "baseline", "--transcripts", transcripts, "--labelled", labelled); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if e := dt.stderr.String(); !strings.Contains(e, "warning") || !strings.Contains(e, "50%") {
		t.Errorf("stderr lacks the warning with the accuracy:\n%s", e)
	}
}

func TestFactoryBaselineNeedsItsTwoFlags(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	for _, args := range [][]string{{}, {"--transcripts", "x"}, {"--labelled", "x"}, {"--transcripts", "x", "--labelled", "y", "extra"}} {
		dt.stderr.Reset()
		if code := dt.run("factory", append([]string{"baseline"}, args...)...); code != 2 {
			t.Errorf("baseline %v: exit %d, want 2 (usage)", args, code)
		}
	}
}

// The baseline loads the configuration itself, as a run from the terminal
// starts with none: the trackers that tell a ticket's key come from it.
func TestFactoryBaselineLoadsTheConfiguration(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	transcripts, labelled := baselineFixture(t, dt)
	user := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(user, []byte("[board]\ntrackers = [\"https://example.atlassian.net/browse/PR-{num}\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dt.ctx.Config = nil // as Main leaves it: no command has loaded it yet
	dt.ctx.Layout.UserConfig = user

	code := dt.run("factory", "baseline", "--transcripts", transcripts, "--labelled", labelled, "--branch-prefix", "zhuravel-")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if got, err := stats.LoadBaseline(dt.ctx.Layout.FactoryBaseline()); err != nil || got == nil || got.Tickets != 2 {
		t.Fatalf("LoadBaseline = %+v, %v; want the 2 tickets of the fixture", got, err)
	}
}

// Stats counts, per factory task, the fix loops magnum started on its PR,
// from magnum's registry (read only, as the screen reads it): a loop's later cycles are
// not loops, autofix's count apart, and the total of the loops you started
// by hand is the "no manual start" measure.
func TestStatsCountsManualFixStartsOnFactoryPRs(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	task := seedStatsTask(t, dt, "PR-12316", time.Time{}, nil, 0)
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := reg.LinkPR(ctx, registry.PRLink{TaskID: task.ID, Repo: "example/shop", Number: 4530, Branch: "zhuravel-PR-12316"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.TransitionTask(ctx, task.ID, nil, registry.TaskPublished, func(u *registry.TaskUpdate) { u.Set("pr_number", 4530) }); err != nil {
		t.Fatal(err)
	}
	reg.Close()
	st := storetest.OpenFile(t, dt.ctx.Layout.DB())
	if _, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_shop", Owner: "example", Name: "shop", Mode: store.RepoModePerPR}); err != nil {
		t.Fatal(err)
	}
	subject := "pr:example/shop#4530"
	for _, ev := range []struct {
		cycle int
		by    string
	}{{1, "magnum fix"}, {2, "magnum fix"}, {1, engine.AutofixBy}, {2, engine.AutofixBy}, {3, engine.AutofixBy}, {1, "magnum fix"}} {
		data, err := json.Marshal(map[string]any{"cycle": ev.cycle, "by": ev.by})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendEvent(ctx, store.Event{Subject: &subject, Kind: "fix.start", Message: "claude-fix fixes", Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	if code := dt.run("factory", "stats"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	out := dt.stdout.String()
	for _, want := range []string{
		"- PR-12316 (example/shop#4530): fix loops on its PR: 2 by hand, 1 by autofix",
		"Fix loops started by hand on factory PRs: 2.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
}

// Stats shows the quality of the factory's published PRs, in the markdown
// and in --json: what the local review missed at magnum's first posted
// round (from the factory's registry), the fixers' pushes and commits on
// the PR (from magnum's registry, read only) and the CI failures after the
// publish; a published task magnum posted no round on yet reads "not yet".
func TestFactoryStatsShowsTheQualityOfThePublishedPRs(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	checked := seedStatsTask(t, dt, "PR-12340", time.Time{}, nil, 0)
	waiting := seedStatsTask(t, dt, "PR-12341", time.Time{}, nil, 0)
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i, task := range []registry.Task{checked, waiting} {
		n := 4540 + i
		if err := reg.LinkPR(ctx, registry.PRLink{TaskID: task.ID, Repo: "example/shop", Number: n, Branch: "zhuravel-" + task.JiraKey}); err != nil {
			t.Fatal(err)
		}
		if err := reg.TransitionTask(ctx, task.ID, nil, registry.TaskPublished, func(u *registry.TaskUpdate) { u.Set("pr_number", n) }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reg.RecordEscapeCheck(ctx, registry.EscapeCheck{TaskID: checked.ID, RunID: "r-1", Round: 1, HeadSHA: "abc123", Posted: 2},
		[]registry.Escape{{FindingID: 7, Path: "app/models/credit.rb", StartLine: 40, EndLine: 40, Priority: "P2", Reason: registry.EscapeNoLocal}}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RecordGitHubCI(ctx, checked.ID, "abc123", registry.CheckFail, ""); err != nil {
		t.Fatal(err)
	}
	reg.Close()
	st := storetest.OpenFile(t, dt.ctx.Layout.DB())
	if _, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_shop", Owner: "example", Name: "shop", Mode: store.RepoModePerPR}); err != nil {
		t.Fatal(err)
	}
	subject := "pr:example/shop#4540"
	for _, ev := range []struct {
		kind string
		data map[string]any
	}{{"fix.start", map[string]any{"cycle": 1, "by": engine.AutofixBy}}, {"fix.pushed", map[string]any{"cycle": 1, "commits": 2}}} {
		data, err := json.Marshal(ev.data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendEvent(ctx, store.Event{Subject: &subject, Kind: ev.kind, Message: ev.kind, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	if code := dt.run("factory", "stats"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	out := dt.stdout.String()
	for _, want := range []string{
		"## The quality of the published PRs",
		"| PR-12340 | example/shop#4540 | 1 of 2 posted (P0 0, P1 0, P2 1) | 1 (2 commits) | 1 |",
		"| PR-12341 | example/shop#4541 | not yet | 0 (0 commits) | 0 |",
		"In total over 2 PRs: 1 escape at 1 checked PR (P0 0, P1 0, P2 1), 1 not yet; 1 fix push (2 commits); 1 CI failure after the publish.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
	dt.stdout.Reset()
	if code := dt.run("factory", "stats", "--json"); code != 0 {
		t.Fatalf("--json: exit %d, stderr: %s", code, dt.stderr)
	}
	var rep struct {
		Tasks []struct {
			Key      string          `json:"key"`
			FixLoops *stats.FixLoops `json:"fix_loops"`
			Quality  *struct {
				Escapes *struct {
					Posted int `json:"posted"`
					P2     int `json:"p2"`
					Total  int `json:"total"`
				} `json:"escapes"`
				CIFailed int `json:"ci_failed"`
			} `json:"quality"`
		} `json:"tasks"`
		Quality *stats.QualityTotals `json:"quality"`
	}
	if err := json.Unmarshal(dt.stdout.Bytes(), &rep); err != nil {
		t.Fatalf("%v:\n%s", err, dt.stdout)
	}
	if len(rep.Tasks) != 2 || rep.Tasks[0].Quality == nil || rep.Tasks[0].Quality.Escapes == nil || rep.Tasks[0].Quality.Escapes.Total != 1 ||
		rep.Tasks[0].Quality.Escapes.Posted != 2 || rep.Tasks[0].Quality.CIFailed != 1 || rep.Tasks[0].FixLoops == nil ||
		rep.Tasks[0].FixLoops.Commits != 2 || rep.Tasks[1].Quality == nil || rep.Tasks[1].Quality.Escapes != nil {
		t.Errorf("the JSON's tasks:\n%s", dt.stdout)
	}
	if q := rep.Quality; q == nil || q.Checked != 1 || q.NotYet != 1 || q.Escapes != 1 || q.P2 != 1 || q.CIFailed != 1 || q.FixPushes == nil ||
		*q.FixPushes != 1 || *q.FixCommits != 2 {
		t.Errorf("the JSON's totals %+v:\n%s", q, dt.stdout)
	}
}
