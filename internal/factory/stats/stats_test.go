package stats_test

import (
	"encoding/json"
	"slices"
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

// input records an input of the task at offset after t0; a pane's turn with
// herdr's proof that the operator typed it.
func input(t *testing.T, r *registry.Registry, taskID int64, kind, text, class string, offset time.Duration) {
	t.Helper()
	in := registry.Input{TaskID: taskID, Kind: kind, Text: text, Class: class, At: t0.Add(offset)}
	if kind == registry.InputPaneTyped {
		in.Origin = registry.OriginHerdr
	}
	if _, err := r.RecordInput(t.Context(), in); err != nil {
		t.Fatalf("RecordInput(%s %q): %v", kind, text, err)
	}
}

// startInput records the message that started the task, marked as the engine
// marks it (the intake role), at offset after t0.
func startInput(t *testing.T, r *registry.Registry, taskID int64, kind, text, class string, offset time.Duration) {
	t.Helper()
	in := registry.Input{TaskID: taskID, Kind: kind, Text: text, Class: class, Role: registry.RoleIntake, At: t0.Add(offset)}
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

// The task's text, pane-typed, say-one-line, guidance and send-back inputs
// are what the operator typed (the task's first message counts, as the first
// message of every session does in the baseline); answers and decisions are
// counted apart, and the other inputs (a rating, a stop, a drop) are neither.
// An answer batch is a revision of questions that was answered, from the
// question records, whatever the answer inputs look like.
func TestStatsCountsTypedMessagesPerTask(t *testing.T) {
	t.Parallel()
	r, clk := openClocked(t)
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
	if _, err := r.RecordInput(ctx, registry.Input{TaskID: a.ID, Kind: registry.InputPaneTyped, DroppedReason: "a long paste", Origin: registry.OriginHerdr,
		At: t0.Add(8 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	// Questions: three answered together, one answered later, one asked and
	// never answered. The answers left an answer input each; they are no batch.
	askRevision(t, r, clk, a.ID, 1, 3, time.Hour, 61*time.Minute, 66*time.Minute)
	askRevision(t, r, clk, a.ID, 2, 1, 2*time.Hour, 121*time.Minute, 130*time.Minute)
	askRevision(t, r, clk, a.ID, 3, 2, 3*time.Hour, 181*time.Minute, 0)
	for i, text := range []string{"Seven days", "ask:rev-ann", "Yes", "Keep it"} {
		input(t, r, a.ID, registry.InputAnswer, text, "", time.Hour+time.Duration(i)*20*time.Minute)
	}
	// Decisions, the card's and the input it left.
	input(t, r, a.ID, registry.InputDecision, "send_back: scope", "", 4*time.Hour)
	input(t, r, a.ID, registry.InputDecision, "push", "", 5*time.Hour)
	for _, d := range []registry.Decision{
		{TaskID: a.ID, Kind: "ready_push", Choice: "send_back", Note: "scope", At: t0.Add(4 * time.Hour)},
		{TaskID: a.ID, Kind: "ready_push", Choice: "push", At: t0.Add(5 * time.Hour)},
	} {
		if err := r.RecordDecision(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	input(t, r, a.ID, registry.InputRating, "", "", 6*time.Hour)
	input(t, r, a.ID, registry.InputStop, "", "", 7*time.Hour)
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
	want := stats.TaskStats{TaskID: a.ID, Key: "PR-12301", Typed: 9, StepStarts: 1, Approvals: 1, Corrections: 2, Access: 1,
		AnswerBatches: 2, Decisions: 2, ReviewRounds: 2}
	got.OperatorSecs, got.WaitedOnYou, got.StartToReady = 0, 0, 0
	if got != want {
		t.Errorf("task A\n got %+v\nwant %+v", got, want)
	}
	if other := rep.Tasks[1]; other.Typed != 1 || other.Approvals != 1 || other.AnswerBatches != 0 || other.Decisions != 0 {
		t.Errorf("task B = %+v, want one typed message, an approval and nothing else", other)
	}
}

// askRevision stores revision rev of n questions of the task at offset asked
// after t0, shows it at shown and answers all of it at answered (an offset of
// zero: not shown, not answered).
func askRevision(t *testing.T, r *registry.Registry, clk *clock, taskID int64, rev, n int, asked, shown, answered time.Duration) {
	t.Helper()
	ctx := t.Context()
	qs := make([]registry.Question, n)
	answers := map[int]string{}
	for i := range qs {
		qs[i] = registry.Question{Text: "Which one?", Recommended: -1, RaisedBy: "fx-reproducer"}
		answers[i] = "The first"
	}
	clk.Set(t0.Add(asked))
	if err := r.ReplaceQuestions(ctx, taskID, rev, qs); err != nil {
		t.Fatalf("ask revision %d: %v", rev, err)
	}
	if shown > 0 {
		clk.Set(t0.Add(shown))
		if err := r.ShowQuestions(ctx, taskID, rev); err != nil {
			t.Fatalf("show revision %d: %v", rev, err)
		}
	}
	if answered > 0 {
		clk.Set(t0.Add(answered))
		if err := r.AnswerBatch(ctx, taskID, rev, answers); err != nil {
			t.Fatalf("answer revision %d: %v", rev, err)
		}
	}
}

// A step-starting message is a typed message the classifier calls a step
// start, or the stored class does (the stored one wins), or a retry: the
// operator starting a step by hand. The words of a start are classified like
// any typed words, as the engine classifies them when it records them.
func TestStatsCountsStepStartingMessages(t *testing.T) {
	t.Parallel()
	r := openRegistry(t)
	task := createTask(t, r, "PR-12303")

	startInput(t, r, task.ID, registry.InputGuidance, "merge master first", "", 0)                            // the start's words: typed, no step start
	input(t, r, task.ID, registry.InputPaneTyped, "merge master", "", time.Minute)                            // classified by its words
	input(t, r, task.ID, registry.InputSayOneLine, "go on with that", registry.ClassStepStart, 2*time.Minute) // class stored by the engine
	input(t, r, task.ID, registry.InputPaneTyped, "open PR", registry.ClassInfo, 3*time.Minute)               // the stored class wins
	input(t, r, task.ID, registry.InputRetry, "", "", 4*time.Minute)                                          // a step started by hand
	input(t, r, task.ID, registry.InputGuidance, "open PR when it is done", "", 5*time.Minute)                // guidance given later keeps its class
	input(t, r, task.ID, registry.InputAnswer, "do as recommended", "", 6*time.Minute)                        // an answer, apart
	input(t, r, task.ID, registry.InputDecision, "push", "", 7*time.Minute)                                   // a decision, apart

	rep, err := stats.ComputeAt(t.Context(), r, time.Time{}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := only(t, rep, "PR-12303")
	if got.StepStarts != 4 {
		t.Errorf("StepStarts = %d, want 4 (merge master, the stored step start, the retry, the later guidance; not the start's words)", got.StepStarts)
	}
	if got.Typed != 5 {
		t.Errorf("Typed = %d, want 5 (the retry is not a typed message; the answer and the decision are counted apart)", got.Typed)
	}
	if got.AnswerBatches != 0 {
		t.Errorf("AnswerBatches = %d, want 0: an answer input is no batch, an answered revision of questions is", got.AnswerBatches)
	}
}

// The message that starts a task is the one message the factory needs: it
// counts as typed and never as a step start, whatever its words or its stored
// class say, or the target of zero step starts could not be reached. That
// holds however the start is recorded (a prompt's text, or the guidance after
// a key) and wherever its input lies among the others: the engine marks it.
// Guidance or a send-back's note given later keeps its class, and the start's
// other classes (a correction, say) still count.
func TestStatsCountsAStartMessageAsTypedAndNeverAsAStepStart(t *testing.T) {
	t.Parallel()
	r := openRegistry(t)
	prompt := createTask(t, r, "PR-12310")
	keyed := createTask(t, r, "PR-12311")
	stored := createTask(t, r, "PR-12312")
	early := createTask(t, r, "PR-12313")
	corrects := createTask(t, r, "PR-12314")

	// A prompt's text, a key's guidance (the same words), both naming a step.
	input(t, r, prompt.ID, registry.InputTaskText, "fix the points, then open PR", "", 0)
	startInput(t, r, keyed.ID, registry.InputGuidance, "fix the points, then open PR", "", 0)
	// Stored as a step start by the engine: the start still is none; the
	// guidance and the note given later, with the same class, are.
	startInput(t, r, stored.ID, registry.InputGuidance, "fix the points", registry.ClassStepStart, 0)
	input(t, r, stored.ID, registry.InputGuidance, "open PR when it is done", registry.ClassStepStart, time.Minute)
	input(t, r, stored.ID, registry.InputSendBack, "push it again", registry.ClassStepStart, 2*time.Minute)
	// The mark tells the start, not its place: guidance recorded before it
	// keeps its class.
	input(t, r, early.ID, registry.InputGuidance, "push it", "", 0)
	startInput(t, r, early.ID, registry.InputGuidance, "fix the points, then open PR", "", time.Minute)
	// A start that corrects is still a correction.
	startInput(t, r, corrects.ID, registry.InputGuidance, "wrong table, use the ledger", registry.ClassCorrection, 0)

	rep, err := stats.ComputeAt(t.Context(), r, time.Time{}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		key                   string
		typed, steps, correct int
		what                  string
	}{
		{"PR-12310", 1, 0, 0, "a prompt's text that names a step"},
		{"PR-12311", 1, 0, 0, "a key's guidance that names a step"},
		{"PR-12312", 3, 2, 0, "a stored step start on the start, later guidance and a note (the last two count)"},
		{"PR-12313", 2, 1, 0, "guidance before the marked start"},
		{"PR-12314", 1, 0, 1, "a start that corrects"},
	} {
		if got := only(t, rep, c.key); got.Typed != c.typed || got.StepStarts != c.steps || got.Corrections != c.correct {
			t.Errorf("%s: typed %d, step starts %d, corrections %d; want %d, %d and %d", c.what, got.Typed, got.StepStarts, got.Corrections,
				c.typed, c.steps, c.correct)
		}
	}
}

// A turn typed in a pane counts as the operator's only with herdr's proof:
// one of unknown origin, of no origin or of any other is counted apart, for
// evaluation only, never a typed message, a step start or any class; one
// herdr proved is typed and classed as usual.
func TestStatsCountsAPaneTurnOfUnknownOriginApart(t *testing.T) {
	t.Parallel()
	r := openRegistry(t)
	task := createTask(t, r, "PR-12315")
	for i, in := range []registry.Input{
		{Kind: registry.InputPaneTyped, Text: "open PR", Origin: registry.OriginUnknown},
		{Kind: registry.InputPaneTyped, Text: "I gave you access to the sandbox", Origin: registry.OriginUnknown},
		{Kind: registry.InputPaneTyped, DroppedReason: "a long paste", Origin: registry.OriginUnknown},
		{Kind: registry.InputPaneTyped, Text: "push it"},
		{Kind: registry.InputPaneTyped, Text: "merge into staging-void", Origin: "a later writer's"},
		{Kind: registry.InputPaneTyped, Text: "merge master", Origin: registry.OriginHerdr},
	} {
		in.TaskID, in.At = task.ID, t0.Add(time.Duration(i)*time.Minute)
		if _, err := r.RecordInput(t.Context(), in); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := stats.ComputeAt(t.Context(), r, time.Time{}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := only(t, rep, "PR-12315")
	if got.UnknownOrigin != 5 || got.Typed != 1 || got.StepStarts != 1 || got.Access != 0 {
		t.Errorf("unknown origin %d, typed %d, step starts %d, access %d; want 5, 1, 1 and 0", got.UnknownOrigin, got.Typed, got.StepStarts,
			got.Access)
	}
}

// The kinds of input that are the operator's own words.
func TestIsTypedNamesTheOperatorsOwnWords(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{registry.InputTaskText, registry.InputGuidance, registry.InputSendBack, registry.InputPaneTyped, registry.InputSayOneLine} {
		if !stats.IsTyped(kind) {
			t.Errorf("IsTyped(%q) = false", kind)
		}
	}
	for _, kind := range []string{registry.InputAnswer, registry.InputDecision, registry.InputRating, registry.InputStop, registry.InputDrop,
		registry.InputRetry, "nonsense"} {
		if stats.IsTyped(kind) {
			t.Errorf("IsTyped(%q) = true", kind)
		}
	}
}

// The time a task waited on the operator is the sum of its spans that wait
// on "you"; a wait that is still open runs to now. That wait is not the
// operator's time on cards. Start to ready ends when the ready stage waits on
// the operator.
func TestStatsMeasuresTheWaitOnYouAndStartToReadyFromSpans(t *testing.T) {
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
	if want := 30*time.Minute + 15*time.Minute; got.WaitedOnYou != want {
		t.Errorf("WaitedOnYou = %s, want %s: 30m on the questions and 15m open on the ready card, up to now", got.WaitedOnYou, want)
	}
	if want := 2 * time.Hour; got.StartToReady != want {
		t.Errorf("StartToReady = %s, want %s", got.StartToReady, want)
	}
	if got.OperatorSecs != 0 {
		t.Errorf("OperatorSecs = %s, want 0: no card was shown or decided, however long the task waited", got.OperatorSecs)
	}

	// Decided at 2h30m, stopped at 3h: the card's wait closes there.
	move(2*time.Hour+30*time.Minute, nil, registry.TaskActive, "publish", "", "")
	move(3*time.Hour, nil, registry.TaskDone, "publish", "", "")
	rep, err = stats.ComputeAt(ctx, r, time.Time{}, t0.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := only(t, rep, "PR-12304"); got.WaitedOnYou != time.Hour || got.StartToReady != 2*time.Hour {
		t.Errorf("after the decision: WaitedOnYou = %s, StartToReady = %s; want 1h0m0s and 2h0m0s", got.WaitedOnYou, got.StartToReady)
	}
}

// The kinds' stages are data: a ready stage of another name ends start to
// ready the same way, found by its wait on you ("ready to push"), and a
// stage named "ready" that waits on you for something else does not.
func TestStartToReadyFindsTheReadyWaitWhateverTheStagesName(t *testing.T) {
	t.Parallel()
	r, clk := openClocked(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12305")
	move := func(offset time.Duration, to, stage, who, what string) {
		t.Helper()
		clk.Set(t0.Add(offset))
		if err := r.TransitionTask(ctx, task.ID, nil, to, func(u *registry.TaskUpdate) {
			u.Set("stage", stage)
			u.Set("waits_who", who)
			u.Set("waits_what", what)
		}); err != nil {
			t.Fatal(err)
		}
	}
	move(10*time.Minute, registry.TaskNeedsYou, "ready", "you", "the precheck did not pass")
	move(20*time.Minute, registry.TaskActive, "code", "", "")
	move(time.Hour, registry.TaskNeedsYou, "card", "you", registry.ReadyWait+" (card 1): PR-12305 a title")
	rep, err := stats.ComputeAt(ctx, r, time.Time{}, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := only(t, rep, "PR-12305"); got.StartToReady != time.Hour {
		t.Errorf("StartToReady = %s, want 1h0m0s: the card's wait, at a stage named card", got.StartToReady)
	}
}

// The operator's seconds are their time on cards: for each decision, from the
// card shown to the decision; for each answered revision of questions, from
// the questions shown to the answers. An item counts at most 30 minutes, so a
// card left open and idle does not; one that was never shown has no time to
// measure; questions asked and never answered are no item.
func TestStatsMeasuresTheOperatorsTimeOnCards(t *testing.T) {
	t.Parallel()
	r, clk := openClocked(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12309")
	askRevision(t, r, clk, task.ID, 1, 2, time.Minute, 2*time.Minute, 8*time.Minute)   // 6m
	askRevision(t, r, clk, task.ID, 2, 1, 20*time.Minute, 21*time.Minute, 3*time.Hour) // left open: 30m
	askRevision(t, r, clk, task.ID, 3, 1, 4*time.Hour, 4*time.Hour+time.Minute, 0)     // never answered: none
	askRevision(t, r, clk, task.ID, 4, 1, 5*time.Hour, 0, 5*time.Hour+10*time.Minute)  // never shown: a batch, no time
	for _, d := range []registry.Decision{
		{TaskID: task.ID, Kind: "ready_push", Choice: "send_back", Shown: t0.Add(6 * time.Hour), At: t0.Add(6*time.Hour + 12*time.Minute)}, // 12m
		{TaskID: task.ID, Kind: "ready_push", Choice: "send_back", Shown: t0.Add(7 * time.Hour), At: t0.Add(9 * time.Hour)},                // left open: 30m
		{TaskID: task.ID, Kind: "ready_push", Choice: "push", At: t0.Add(10 * time.Hour)},                                                  // never shown: no time
	} {
		if err := r.RecordDecision(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := stats.ComputeAt(ctx, r, time.Time{}, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := only(t, rep, "PR-12309")
	if want := 6*time.Minute + 30*time.Minute + 12*time.Minute + 30*time.Minute; got.OperatorSecs != want {
		t.Errorf("OperatorSecs = %s, want %s", got.OperatorSecs, want)
	}
	if got.AnswerBatches != 3 || got.Decisions != 3 {
		t.Errorf("AnswerBatches = %d, Decisions = %d; want 3 answered revisions and 3 decisions", got.AnswerBatches, got.Decisions)
	}
}

// The stats never read the surface of an input: what the operator answers
// and decides in the liaison's chat counts as what they answer and decide on
// a card, an answered batch and a decision each, and their time on it runs
// from the time it was shown to the answer or the decision.
func TestStatsCountALiaisonsAnswerAsACards(t *testing.T) {
	t.Parallel()
	r, clk := openClocked(t)
	ctx := t.Context()
	for key, surface := range map[string]string{"PR-12306": "screen", "PR-12307": "liaison"} {
		task := createTask(t, r, key)
		askRevision(t, r, clk, task.ID, 1, 2, time.Minute, 2*time.Minute, 8*time.Minute) // 6m
		d := registry.Decision{TaskID: task.ID, Kind: "ready_push", Choice: "push", Shown: t0.Add(time.Hour), DiffOpened: t0.Add(time.Hour),
			At: t0.Add(time.Hour + 12*time.Minute)} // 12m
		if err := r.RecordDecision(ctx, d); err != nil {
			t.Fatal(err)
		}
		for _, in := range []registry.Input{
			{TaskID: task.ID, Kind: registry.InputAnswer, Surface: surface, Text: "Seven days before it expires", At: t0.Add(8 * time.Minute)},
			{TaskID: task.ID, Kind: registry.InputDecision, Surface: surface, Text: "push", At: d.At},
		} {
			if _, err := r.RecordInput(ctx, in); err != nil {
				t.Fatal(err)
			}
		}
	}

	rep, err := stats.ComputeAt(ctx, r, time.Time{}, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	screen, liaison := only(t, rep, "PR-12306"), only(t, rep, "PR-12307")
	if want := 18 * time.Minute; liaison.OperatorSecs != want || liaison.AnswerBatches != 1 || liaison.Decisions != 1 || liaison.Typed != 0 {
		t.Errorf("the liaison's task = %+v, want one answered batch and one decision, %s of the operator's time and nothing typed", liaison, want)
	}
	screen.TaskID, screen.Key = liaison.TaskID, liaison.Key
	if screen != liaison {
		t.Errorf("the liaison's task\n got %+v\nwant %+v, as the same from the screen", liaison, screen)
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
	if got := only(t, rep, "PR-12305"); got.StartToReady != 0 || got.OperatorSecs != 0 || got.WaitedOnYou != 0 {
		t.Errorf("StartToReady = %s, OperatorSecs = %s, WaitedOnYou = %s; want 0 for all", got.StartToReady, got.OperatorSecs, got.WaitedOnYou)
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
			{TaskID: 1, Key: "PR-12301", Typed: 3, StepStarts: 0, Approvals: 1, Corrections: 1, Access: 0, UnknownOrigin: 4, AnswerBatches: 2, Decisions: 1,
				ReviewRounds: 2, OperatorSecs: 25*time.Minute + 30*time.Second, WaitedOnYou: time.Hour + 10*time.Minute, StartToReady: 2*time.Hour + 5*time.Minute},
			{TaskID: 2, Key: "task 2", Typed: 90, StepStarts: 12, AnswerBatches: 1, Decisions: 3, ReviewRounds: 1,
				OperatorSecs: 40 * time.Second, WaitedOnYou: 2 * time.Hour, StartToReady: 0},
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
		"| Task | Typed | Step starts | Approvals | Corrections | Access | Origin unknown | Answer batches | Decisions | Review rounds | On cards | Waited on you | Start to ready |",
		"| PR-12301 | 3 | 0 | 1 | 1 | 0 | 4 | 2 | 1 | 2 | 25m30s | 1h10m | 2h05m |",
		"| task 2 | 90 | 12 | 0 | 0 | 0 | 0 | 1 | 3 | 1 | 40s | 2h00m | - |",
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
		`{"task":1,"key":"PR-12301","typed":3,"step_starts":0,"approvals":1,"corrections":1,"access":0,"unknown_origin":4,"answer_batches":2,"decisions":1,"review_rounds":2,"operator_secs":1530,"waited_on_you_secs":4200,"start_to_ready_secs":7500},` +
		`{"task":2,"key":"task 2","typed":90,"step_starts":12,"approvals":0,"corrections":0,"access":0,"unknown_origin":0,"answer_batches":1,"decisions":3,"review_rounds":1,"operator_secs":40,"waited_on_you_secs":7200,"start_to_ready_secs":0}],` +
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

// A task's PR is its published one; the fix loops magnum started on it (read
// from magnum's registry by the CLI, SetFixLoops) show per task, by hand
// apart from autofix, with the total started by hand, the "no manual start"
// measure; a registry that could not be read says so and counts nothing.
func TestStatsShowTheFixLoopsOnTheTasksPRs(t *testing.T) {
	t.Parallel()
	r := openRegistry(t)
	ctx := t.Context()
	published := createTask(t, r, "PR-12320")
	createTask(t, r, "PR-12321")
	if err := r.LinkPR(ctx, registry.PRLink{TaskID: published.ID, Repo: "example/app", Number: 4530, Branch: "zhuravel-PR-12320"}); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionTask(ctx, published.ID, nil, registry.TaskPublished, func(u *registry.TaskUpdate) { u.Set("pr_number", 4530) }); err != nil {
		t.Fatal(err)
	}
	rep, err := stats.ComputeAt(ctx, r, time.Time{}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := only(t, rep, "PR-12320").PR; got != (stats.PR{Repo: "example/app", Number: 4530}) {
		t.Fatalf("the published task's PR %+v", got)
	}
	if got := only(t, rep, "PR-12321").PR; got != (stats.PR{}) {
		t.Fatalf("a task without a PR has PR %+v", got)
	}

	read := rep
	read.SetFixLoops(map[stats.PR]stats.FixLoops{{Repo: "example/app", Number: 4530}: {ByHand: 1, ByAutofix: 2}}, "")
	md := read.Markdown()
	for _, want := range []string{
		"## Fix loops on the factory's PRs",
		"- PR-12320 (example/app#4530): fix loops on its PR: 1 by hand, 2 by autofix",
		"Fix loops started by hand on factory PRs: 1.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "PR-12321 (") {
		t.Errorf("a task without a PR has a fix loops line:\n%s", md)
	}
	js, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"pr":"example/app#4530","fix_loops":{"by_hand":1,"by_autofix":2,"pushes":0,"commits":0}`, `"fix_loops_by_hand":1`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("the JSON lacks %s:\n%s", want, js)
		}
	}

	unread := rep
	unread.Tasks = slices.Clone(rep.Tasks)
	unread.SetFixLoops(nil, "magnum's registry is at schema 40 and this build needs 41")
	md = unread.Markdown()
	if !strings.Contains(md, "magnum's registry was not read (magnum's registry is at schema 40 and this build needs 41): no fix loop is counted") ||
		strings.Contains(md, "by hand on factory PRs") {
		t.Errorf("an unread registry:\n%s", md)
	}
}

// publish links task's PR number and moves the task to published.
func publish(t *testing.T, r *registry.Registry, task registry.Task, number int) {
	t.Helper()
	ctx := t.Context()
	if err := r.LinkPR(ctx, registry.PRLink{TaskID: task.ID, Repo: "example/app", Number: number, Branch: "zhuravel-" + task.JiraKey}); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionTask(ctx, task.ID, nil, registry.TaskPublished, func(u *registry.TaskUpdate) { u.Set("pr_number", number) }); err != nil {
		t.Fatal(err)
	}
}

// The quality part counts, per task with a PR and in total, what the local
// review missed at magnum's first posted round (by priority, those with no
// line apart, or "not yet" before such a round), the fixers' pushes and
// commits after the publish (from magnum's registry, SetFixLoops), and the
// PR's CI failures after the publish (GitHub's ci checks), those not ours
// apart; in the markdown and in the JSON. A task without a PR has none.
func TestStatsShowTheQualityOfThePublishedPRs(t *testing.T) {
	t.Parallel()
	r := openRegistry(t)
	ctx := t.Context()
	checked, waiting := createTask(t, r, "PR-12330"), createTask(t, r, "PR-12331")
	createTask(t, r, "PR-12332")
	publish(t, r, checked, 4530)
	publish(t, r, waiting, 4531)
	cy, err := r.BeginCycle(ctx, checked.ID, "abc123", "skill")
	if err != nil {
		t.Fatal(err)
	}
	near, err := r.SeeFinding(ctx, cy.ID, registry.Finding{Fingerprint: "app/a.rb:10:x", Path: "app/a.rb", StartLine: 10, EndLine: 10, Priority: "P2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RecordEscapeCheck(ctx, registry.EscapeCheck{TaskID: checked.ID, RunID: "r-1", Round: 1, HeadSHA: "abc123", Posted: 5, Comparable: 1},
		[]registry.Escape{
			{FindingID: 1, Path: "app/b.rb", StartLine: 3, EndLine: 3, Priority: "P2", Reason: registry.EscapeNoLocal},
			{FindingID: 2, Path: "app/a.rb", StartLine: 20, EndLine: 20, Priority: "P1", Reason: registry.EscapeFar, NearestID: near.ID, Distance: 10},
			{FindingID: 3, Priority: "P0", Reason: registry.EscapeUnplaced},
		}); err != nil {
		t.Fatal(err)
	}
	for _, ci := range [][3]string{{"abc123", registry.CheckFail, ""}, {"abc123", registry.CheckPass, ""}, {"def456", registry.CheckFail, "not_ours"},
		{"fed789", registry.CheckFail, ""}} {
		if _, err := r.RecordGitHubCI(ctx, checked.ID, ci[0], ci[1], ci[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.RecordCheck(ctx, registry.Check{TaskID: checked.ID, HeadSHA: "abc123", Kind: registry.CheckSpec, Result: registry.CheckFail,
		Source: "factory"}); err != nil { // the factory's own precheck: no CI after the publish
		t.Fatal(err)
	}
	rep, err := stats.ComputeAt(ctx, r, time.Time{}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	q := only(t, rep, "PR-12330").Quality
	if q == nil || q.Escapes == nil || *q.Escapes != (stats.Escapes{Posted: 5, P0: 1, P1: 1, P2: 1, Unplaced: 1}) || q.Escapes.Total() != 3 ||
		q.CIFailed != 2 || q.CINotOurs != 1 {
		t.Errorf("the checked task's quality %+v (escapes %+v)", q, q.Escapes)
	}
	if q := only(t, rep, "PR-12331").Quality; q == nil || q.Escapes != nil || q.CIFailed != 0 {
		t.Errorf("the task with no posted round yet: %+v, want its escapes not yet", q)
	}
	if q := only(t, rep, "PR-12332").Quality; q != nil {
		t.Errorf("a task without a PR has quality %+v", q)
	}

	rep.SetFixLoops(map[stats.PR]stats.FixLoops{{Repo: "example/app", Number: 4530}: {ByAutofix: 1, Pushes: 2, Commits: 3}}, "")
	md := rep.Markdown()
	for _, want := range []string{
		"## The quality of the published PRs",
		"| Task | PR | Escapes at the first posted round | Fix pushes after the publish | CI failures after the publish |",
		"| PR-12330 | example/app#4530 | 3 of 5 posted (P0 1, P1 1, P2 1; 1 with no line) | 2 (3 commits) | 2, and 1 not ours |",
		"| PR-12331 | example/app#4531 | not yet | 0 (0 commits) | 0 |",
		"In total over 2 PRs: 3 escapes at 1 checked PR (P0 1, P1 1, P2 1; 1 with no line), 1 not yet; 2 fix pushes (3 commits); " +
			"2 CI failures after the publish, and 1 not ours.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "| PR-12332 | example") {
		t.Errorf("a task without a PR has a quality row:\n%s", md)
	}
	js, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"pr":"example/app#4530","fix_loops":{"by_hand":0,"by_autofix":1,"pushes":2,"commits":3},` +
			`"quality":{"escapes":{"posted":5,"p0":1,"p1":1,"p2":1,"unplaced":1,"total":3},"ci_failed":2,"ci_failed_not_ours":1}}`,
		`"pr":"example/app#4531","fix_loops":{"by_hand":0,"by_autofix":0,"pushes":0,"commits":0},` +
			`"quality":{"escapes":null,"ci_failed":0,"ci_failed_not_ours":0}}`,
		`"quality":{"prs":2,"checked":1,"not_yet":1,"posted":5,"escapes":3,"p0":1,"p1":1,"p2":1,"unplaced":1,"ci_failed":2,"ci_failed_not_ours":1,` +
			`"fix_pushes":2,"fix_commits":3}`,
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("the JSON lacks %s:\n%s", want, js)
		}
	}
	if strings.Contains(string(js), `"key":"PR-12332","typed":0,"step_starts":0,"approvals":0,"corrections":0,"access":0,"unknown_origin":0,`+
		`"answer_batches":0,"decisions":0,"review_rounds":0,"operator_secs":0,"waited_on_you_secs":0,"start_to_ready_secs":0,"quality"`) {
		t.Errorf("a task without a PR has quality in the JSON:\n%s", js)
	}

	unread := rep
	unread.SetFixLoops(nil, "magnum's registry is at schema 40 and this build needs 41")
	md = unread.Markdown()
	if !strings.Contains(md, "| PR-12330 | example/app#4530 | 3 of 5 posted (P0 1, P1 1, P2 1; 1 with no line) | - | 2, and 1 not ours |") ||
		!strings.Contains(md, "1 not yet; fix pushes not read; 2 CI failures") {
		t.Errorf("an unread magnum registry leaves the fix pushes unknown:\n%s", md)
	}
	if js, _ := json.Marshal(unread); strings.Contains(string(js), "fix_pushes") {
		t.Errorf("an unread magnum registry counts fix pushes:\n%s", js)
	}
}
