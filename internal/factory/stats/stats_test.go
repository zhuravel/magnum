package stats_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/factory/stats"
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// clock is a registry clock a test moves.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// openRegistry opens a factory registry whose clock stands at t0.
func openRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	r, _ := openClocked(t)
	return r
}

func openClocked(t *testing.T) (*registry.Registry, *clock) {
	t.Helper()
	c := &clock{now: t0}
	return registrytest.OpenWith(t, registry.Options{Clock: c.Now}), c
}

func createTask(t *testing.T, r *registry.Registry, key string) registry.Task {
	t.Helper()
	got, err := r.CreateTask(t.Context(), registry.Task{JiraKey: key, Kind: "bug", Repo: "example/app", Title: "A title"})
	if err != nil {
		t.Fatalf("CreateTask(%q): %v", key, err)
	}
	return got
}

// input records an input of the task at offset after t0.
func input(t *testing.T, r *registry.Registry, taskID int64, kind, text, class string, offset time.Duration) {
	t.Helper()
	in := registry.Input{TaskID: taskID, Kind: kind, Text: text, Class: class, At: t0.Add(offset)}
	if _, err := r.RecordInput(t.Context(), in); err != nil {
		t.Fatalf("RecordInput(%s %q): %v", kind, text, err)
	}
}

func only(t *testing.T, rep stats.Report, key string) stats.TaskStats {
	t.Helper()
	for _, ts := range rep.Tasks {
		if ts.Key == key {
			return ts
		}
	}
	t.Fatalf("the report has no task %s: %+v", key, rep.Tasks)
	return stats.TaskStats{}
}

// Pane-typed, say-one-line, guidance and send-back inputs are what the
// operator typed; answers and decisions are counted apart, and the other
// inputs (the task's text, a rating, a stop, a drop) are neither.
func TestStatsCountsTypedMessagesPerTask(t *testing.T) {
	t.Parallel()
	r := openRegistry(t)
	ctx := t.Context()
	a, b := createTask(t, r, "PR-12301"), createTask(t, r, "PR-12302")

	input(t, r, a.ID, registry.InputTaskText, "fix the points", "", 0)
	input(t, r, a.ID, registry.InputPaneTyped, "the refund path only matters for expired points", "", time.Minute)
	input(t, r, a.ID, registry.InputPaneTyped, "open PR", "", 2*time.Minute)
	input(t, r, a.ID, registry.InputSayOneLine, "yes", "", 3*time.Minute)
	input(t, r, a.ID, registry.InputPaneTyped, "wrong table, use the ledger", "", 4*time.Minute)
	input(t, r, a.ID, registry.InputPaneTyped, "I gave you access to the sandbox", "", 5*time.Minute)
	input(t, r, a.ID, registry.InputGuidance, "only the refund path", "", 6*time.Minute)
	input(t, r, a.ID, registry.InputSendBack, "keep archived out", registry.ClassCorrection, 7*time.Minute)
	if _, err := r.RecordInput(ctx, registry.Input{TaskID: a.ID, Kind: registry.InputPaneTyped, DroppedReason: "a long paste", At: t0.Add(8 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	// Answers: three in one batch, one in a second batch an hour later.
	input(t, r, a.ID, registry.InputAnswer, "Seven days", "", time.Hour)
	input(t, r, a.ID, registry.InputAnswer, "ask:rev-ann", "", time.Hour+10*time.Millisecond)
	input(t, r, a.ID, registry.InputAnswer, "Yes", "", time.Hour+20*time.Millisecond)
	input(t, r, a.ID, registry.InputAnswer, "Keep it", "", 2*time.Hour)
	// Decisions, the card's and the input it left.
	input(t, r, a.ID, registry.InputDecision, "send_back: scope", "", 3*time.Hour)
	input(t, r, a.ID, registry.InputDecision, "push", "", 4*time.Hour)
	for _, d := range []registry.Decision{
		{TaskID: a.ID, Kind: "ready_push", Choice: "send_back", Note: "scope", At: t0.Add(3 * time.Hour)},
		{TaskID: a.ID, Kind: "ready_push", Choice: "push", At: t0.Add(4 * time.Hour)},
	} {
		if err := r.RecordDecision(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	input(t, r, a.ID, registry.InputRating, "", "", 5*time.Hour)
	input(t, r, a.ID, registry.InputStop, "", "", 6*time.Hour)
	for range 2 {
		c, err := r.BeginCycle(ctx, a.ID, "aaa", "skill")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.EndCycle(ctx, c.ID, "bbb", "d", registry.ExitClean, 10); err != nil {
			t.Fatal(err)
		}
	}
	// Another task's inputs never count for this one.
	input(t, r, b.ID, registry.InputPaneTyped, "yes", "", time.Minute)

	rep, err := stats.ComputeAt(ctx, r, time.Time{}, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Tasks) != 2 || rep.Tasks[0].Key != "PR-12301" || rep.Tasks[1].Key != "PR-12302" {
		t.Fatalf("tasks = %+v, want the two tasks by id", rep.Tasks)
	}
	got := rep.Tasks[0]
	want := stats.TaskStats{TaskID: a.ID, Key: "PR-12301", Typed: 8, StepStarts: 1, Approvals: 1, Corrections: 2, Access: 1,
		AnswerBatches: 2, Decisions: 2, ReviewRounds: 2}
	got.OperatorSecs, got.StartToReady = 0, 0
	if got != want {
		t.Errorf("task A\n got %+v\nwant %+v", got, want)
	}
	if other := rep.Tasks[1]; other.Typed != 1 || other.Approvals != 1 || other.AnswerBatches != 0 || other.Decisions != 0 {
		t.Errorf("task B = %+v, want one typed message, an approval and nothing else", other)
	}
}

// A step-starting message is a typed message the classifier calls a step
// start, or the stored class does (the stored one wins), or a retry: the
// operator starting a step by hand.
func TestStatsCountsStepStartingMessages(t *testing.T) {
	t.Parallel()
	r := openRegistry(t)
	task := createTask(t, r, "PR-12303")

	input(t, r, task.ID, registry.InputPaneTyped, "merge master", "", time.Minute)                            // classified by its words
	input(t, r, task.ID, registry.InputSayOneLine, "go on with that", registry.ClassStepStart, 2*time.Minute) // class stored by the engine
	input(t, r, task.ID, registry.InputPaneTyped, "open PR", registry.ClassInfo, 3*time.Minute)               // the stored class wins
	input(t, r, task.ID, registry.InputRetry, "", "", 4*time.Minute)                                          // a step started by hand
	input(t, r, task.ID, registry.InputGuidance, "open PR when it is done", "", 5*time.Minute)                // the start's words are guidance
	input(t, r, task.ID, registry.InputAnswer, "do as recommended", "", 6*time.Minute)                        // an answer, apart
	input(t, r, task.ID, registry.InputDecision, "push", "", 7*time.Minute)                                   // a decision, apart

	rep, err := stats.ComputeAt(t.Context(), r, time.Time{}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := only(t, rep, "PR-12303")
	if got.StepStarts != 3 {
		t.Errorf("StepStarts = %d, want 3 (merge master, the stored step start, the retry)", got.StepStarts)
	}
	if got.Typed != 4 {
		t.Errorf("Typed = %d, want 4 (the retry is not a typed message; the answer and the decision are counted apart)", got.Typed)
	}
	if got.AnswerBatches != 1 {
		t.Errorf("AnswerBatches = %d, want 1", got.AnswerBatches)
	}
}

// The operator's seconds are the time a task waited on them, from its
// spans; a wait that is still open runs to now. Start to ready ends when the
// ready stage waits on the operator.
func TestStatsMeasuresOperatorSecondsAndStartToReadyFromSpans(t *testing.T) {
	t.Parallel()
	r, clk := openClocked(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12304") // queued at t0
	move := func(offset time.Duration, from []string, to, stage, who, what string) {
		t.Helper()
		clk.Set(t0.Add(offset))
		err := r.TransitionTask(ctx, task.ID, from, to, func(u *registry.TaskUpdate) {
			u.Set("stage", stage)
			u.Set("waits_who", who)
			u.Set("waits_what", what)
		})
		if err != nil {
			t.Fatalf("move to %s at %s: %v", to, offset, err)
		}
	}
	move(5*time.Minute, nil, registry.TaskActive, "investigate", "", "")
	move(20*time.Minute, nil, registry.TaskNeedsYou, "investigate", "you", "2 questions")
	move(50*time.Minute, nil, registry.TaskActive, "investigate", "", "")
	move(90*time.Minute, nil, registry.TaskActive, "code", "", "")
	move(2*time.Hour, nil, registry.TaskNeedsYou, "ready", "you", "ready to push (card 1)")

	rep, err := stats.ComputeAt(ctx, r, time.Time{}, t0.Add(2*time.Hour+15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	got := only(t, rep, "PR-12304")
	if want := 30*time.Minute + 15*time.Minute; got.OperatorSecs != want {
		t.Errorf("OperatorSecs = %s, want %s: 30m on the questions and 15m open on the ready card, up to now", got.OperatorSecs, want)
	}
	if want := 2 * time.Hour; got.StartToReady != want {
		t.Errorf("StartToReady = %s, want %s", got.StartToReady, want)
	}

	// Decided at 2h30m, stopped at 3h: the card's wait closes there.
	move(2*time.Hour+30*time.Minute, nil, registry.TaskActive, "publish", "", "")
	move(3*time.Hour, nil, registry.TaskDone, "publish", "", "")
	rep, err = stats.ComputeAt(ctx, r, time.Time{}, t0.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := only(t, rep, "PR-12304"); got.OperatorSecs != time.Hour || got.StartToReady != 2*time.Hour {
		t.Errorf("after the decision: OperatorSecs = %s, StartToReady = %s; want 1h0m0s and 2h0m0s", got.OperatorSecs, got.StartToReady)
	}
}

// A task that never reached the ready stage has no time to ready.
func TestStatsLeavesStartToReadyEmptyBeforeTheReadyCard(t *testing.T) {
	t.Parallel()
	r := openRegistry(t)
	createTask(t, r, "PR-12305")
	rep, err := stats.ComputeAt(t.Context(), r, time.Time{}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := only(t, rep, "PR-12305"); got.StartToReady != 0 || got.OperatorSecs != 0 {
		t.Errorf("StartToReady = %s, OperatorSecs = %s; want 0 and 0", got.StartToReady, got.OperatorSecs)
	}
}

// --since keeps the tasks started at or after it, whole.
func TestStatsSinceKeepsTheTasksStartedInTheWindow(t *testing.T) {
	t.Parallel()
	r, clk := openClocked(t)
	createTask(t, r, "PR-12306")
	clk.Set(t0.Add(48 * time.Hour))
	createTask(t, r, "PR-12307")
	prompt, err := r.CreateTask(t.Context(), registry.Task{Prompt: "tidy the mailer", Kind: "chore"})
	if err != nil {
		t.Fatal(err)
	}

	since := t0.Add(24 * time.Hour)
	rep, err := stats.ComputeAt(t.Context(), r, since, t0.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Since.Equal(since) {
		t.Errorf("Since = %s, want %s", rep.Since, since)
	}
	var keys []string
	for _, ts := range rep.Tasks {
		keys = append(keys, ts.Key)
	}
	if want := "PR-12307,task " + strconv.FormatInt(prompt.ID, 10); strings.Join(keys, ",") != want {
		t.Errorf("tasks = %q, want %q (a task of a free prompt is named by its id)", keys, want)
	}
}

// Compute is ComputeAt at the clock's now, and an empty registry has an
// empty list, not a missing one.
func TestStatsComputeOfAnEmptyRegistry(t *testing.T) {
	t.Parallel()
	rep, err := stats.Compute(t.Context(), openRegistry(t), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tasks == nil || len(rep.Tasks) != 0 || rep.Baseline != nil {
		t.Errorf("report of an empty registry = %+v, want an empty, non-nil task list and no baseline", rep)
	}
}

func sampleReport() stats.Report {
	return stats.Report{
		Since: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		Tasks: []stats.TaskStats{
			{TaskID: 1, Key: "PR-12301", Typed: 3, StepStarts: 0, Approvals: 1, Corrections: 1, Access: 0, AnswerBatches: 2, Decisions: 1,
				ReviewRounds: 2, OperatorSecs: 25*time.Minute + 30*time.Second, StartToReady: 2*time.Hour + 5*time.Minute},
			{TaskID: 2, Key: "task 2", Typed: 90, StepStarts: 12, AnswerBatches: 1, Decisions: 3, ReviewRounds: 1,
				OperatorSecs: 40 * time.Second, StartToReady: 0},
		},
		Baseline: &stats.Baseline{
			Frozen: time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC), Tickets: 12, Accuracy: 0.93,
			TypedPerTicket:      stats.Dist{Median: 41, P25: 30, P75: 77.5, Min: 12, Max: 120},
			StepStartsPerTicket: stats.Dist{Median: 9, P25: 5, P75: 14, Min: 1, Max: 30},
		},
	}
}

// The markdown puts each task beside the baseline: its typed messages and its
// step starts against the baseline's median and the middle half.
func TestStatsMarkdownComparesWithTheBaseline(t *testing.T) {
	t.Parallel()
	md := sampleReport().Markdown()
	for _, want := range []string{
		"# Factory stats",
		"Tasks started since 2026-11-01",
		"| Task | Typed | Step starts | Approvals | Corrections | Access | Answer batches | Decisions | Review rounds | On you | Start to ready |",
		"| PR-12301 | 3 | 0 | 1 | 1 | 0 | 2 | 1 | 2 | 25m30s | 2h05m |",
		"| task 2 | 90 | 12 | 0 | 0 | 0 | 1 | 3 | 1 | 40s | - |",
		"## Against the baseline",
		"frozen 2026-11-02",
		"12 tickets",
		"93%",
		"| PR-12301 | 3 | below the baseline's middle half (median 41, 30 to 77.5) | 0 | below the baseline's middle half (median 9, 5 to 14) |",
		"| task 2 | 90 | above the baseline's middle half (median 41, 30 to 77.5) | 12 | within the baseline's middle half (median 9, 5 to 14) |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the markdown lacks %q:\n%s", want, md)
		}
	}

	noBaseline := sampleReport()
	noBaseline.Baseline = nil
	md = noBaseline.Markdown()
	if strings.Contains(md, "below the baseline") || !strings.Contains(md, "No baseline is frozen") ||
		!strings.Contains(md, "magnum factory baseline") {
		t.Errorf("without a baseline the markdown says how to freeze one and compares nothing:\n%s", md)
	}

	none := stats.Report{Tasks: []stats.TaskStats{}}
	if md := none.Markdown(); !strings.Contains(md, "No tasks") {
		t.Errorf("the markdown of no tasks says so:\n%s", md)
	}
}

// The JSON has fixed keys, the tasks in order, durations in whole seconds and
// no volatile field, so two runs over one registry print the same bytes.
func TestStatsJSONIsStable(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(sampleReport())
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"since":"2026-11-01T00:00:00Z","tasks":[` +
		`{"task":1,"key":"PR-12301","typed":3,"step_starts":0,"approvals":1,"corrections":1,"access":0,"answer_batches":2,"decisions":1,"review_rounds":2,"operator_secs":1530,"start_to_ready_secs":7500},` +
		`{"task":2,"key":"task 2","typed":90,"step_starts":12,"approvals":0,"corrections":0,"access":0,"answer_batches":1,"decisions":3,"review_rounds":1,"operator_secs":40,"start_to_ready_secs":0}],` +
		`"baseline":{"frozen":"2026-11-02T09:30:00Z","tickets":12,` +
		`"typed_per_ticket":{"median":41,"p25":30,"p75":77.5,"min":12,"max":120},` +
		`"step_starts_per_ticket":{"median":9,"p25":5,"p75":14,"min":1,"max":30},"accuracy":0.93}}`
	if string(got) != want {
		t.Errorf("JSON\n got %s\nwant %s", got, want)
	}

	empty, err := json.Marshal(stats.Report{})
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{"since":null,"tasks":[],"baseline":null}` {
		t.Errorf("JSON of an empty report = %s", empty)
	}

	// Two runs over one registry, with the same now, print the same bytes.
	r := openRegistry(t)
	task := createTask(t, r, "PR-12308")
	input(t, r, task.ID, registry.InputPaneTyped, "open PR", "", time.Minute)
	input(t, r, task.ID, registry.InputAnswer, "yes", "", 2*time.Minute)
	var runs [2][]byte
	for i := range runs {
		rep, err := stats.ComputeAt(t.Context(), r, time.Time{}, t0.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if runs[i], err = json.Marshal(rep); err != nil {
			t.Fatal(err)
		}
	}
	if string(runs[0]) != string(runs[1]) {
		t.Errorf("two runs differ:\n%s\n%s", runs[0], runs[1])
	}
}
