// Package stats measures how much the factory still asks of the operator: per
// task, the messages typed, the messages that only start a step, the answer
// batches, the card decisions, the operator's time on cards and the time a
// task waited on them, computed from the factory registry's evidence records
// alone (spans, inputs, questions, decisions and review rounds) and set
// against the baseline frozen before the first task (milestone 1's exit
// test). Compute only reads the registry.
package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// readyStage is the stage whose card waits for the operator's push: the
// built-in ready stage (config's FactoryDecisionReadyPush). A task is ready
// when that stage first waits on the operator.
const readyStage = "ready"

// cardCap is the most one card counts in OperatorSecs: a card left open and
// idle is not the operator's time.
const cardCap = 30 * time.Minute

// TaskStats is what one task asked of the operator. Typed counts the
// messages the operator typed (the task's text, typed in a pane, said in one
// line, the start's guidance, the note of a send-back); StepStarts, Approvals,
// Corrections and Access count those of them by class (a retry is a step the
// operator started by hand, so it counts as a step start and not as a typed
// message; the message that starts the task, however it is recorded, is the
// one the factory needs, so it is typed and never a step start, whatever its
// words say); a message of another class, or of none, is only typed.
// AnswerBatches counts the revisions of questions that were answered, and
// Decisions the decisions on cards; both are counted apart. OperatorSecs is
// the operator's time on cards: for each decision, from the card shown to the
// decision, and for each answered revision of questions, from the questions
// shown to the first answer, each at most cardCap (a card never shown has no
// time to measure). WaitedOnYou is how long the task waited on the operator,
// asleep or not (its spans that wait on "you"). StartToReady is how long it
// took to reach the ready card (zero before it does).
type TaskStats struct {
	TaskID int64
	Key    string

	Typed, StepStarts, Approvals, Corrections, Access int
	AnswerBatches, Decisions, ReviewRounds            int

	OperatorSecs time.Duration //nolint:staticcheck // ST1011: the spec's name for the measure ("the operator's seconds per task")
	WaitedOnYou  time.Duration
	StartToReady time.Duration
}

// IsTyped reports whether an input of the kind holds the operator's own
// words: the task's text, guidance, a send-back's note, what was typed into a
// pane and what was said in one line. The engine classifies the words of
// these when it records them, and Compute counts them as typed. An answer, a
// decision, a rating, a stop, a drop and a retry are the operator's acts, not
// messages.
func IsTyped(kind string) bool {
	switch kind {
	case registry.InputTaskText, registry.InputGuidance, registry.InputSendBack, registry.InputPaneTyped, registry.InputSayOneLine:
		return true
	}
	return false
}

// HasWords reports whether an input of the kind holds the operator's own
// words, which the engine classifies when it records them: the typed kinds
// (IsTyped) and an answer. A decision's text is the machine's choice ("push"),
// not the operator's words, and the other kinds have no text.
func HasWords(kind string) bool { return IsTyped(kind) || kind == registry.InputAnswer }

// startsTask reports whether in is the message that started its task: the
// task's text, or the guidance recorded with a key's start, which the engine
// marks with the intake role. Guidance given later is not marked, and a mark
// does not depend on the order of the task's inputs.
func startsTask(in registry.Input) bool {
	return in.Kind == registry.InputTaskText || (in.Kind == registry.InputGuidance && in.Role == registry.RoleIntake)
}

// MarshalJSON writes the task's stats with fixed keys and the durations in
// whole seconds.
func (t TaskStats) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Task          int64  `json:"task"`
		Key           string `json:"key"`
		Typed         int    `json:"typed"`
		StepStarts    int    `json:"step_starts"`
		Approvals     int    `json:"approvals"`
		Corrections   int    `json:"corrections"`
		Access        int    `json:"access"`
		AnswerBatches int    `json:"answer_batches"`
		Decisions     int    `json:"decisions"`
		ReviewRounds  int    `json:"review_rounds"`
		OperatorSecs  int64  `json:"operator_secs"`
		WaitedOnYou   int64  `json:"waited_on_you_secs"`
		StartToReady  int64  `json:"start_to_ready_secs"`
	}{t.TaskID, t.Key, t.Typed, t.StepStarts, t.Approvals, t.Corrections, t.Access, t.AnswerBatches, t.Decisions, t.ReviewRounds,
		seconds(t.OperatorSecs), seconds(t.WaitedOnYou), seconds(t.StartToReady)})
}

func seconds(d time.Duration) int64 { return int64(d.Round(time.Second) / time.Second) }

// Report is the stats of the tasks started at or after Since (the zero time:
// all of them), oldest first, and the baseline to read them against (nil
// when none is frozen; Compute leaves it to the caller to load).
type Report struct {
	Since    time.Time
	Tasks    []TaskStats
	Baseline *Baseline
}

// MarshalJSON writes the report with fixed keys, an empty list for no tasks
// and null for no window and no baseline.
func (r Report) MarshalJSON() ([]byte, error) {
	var since *time.Time
	if !r.Since.IsZero() {
		s := r.Since.UTC()
		since = &s
	}
	return json.Marshal(struct {
		Since    *time.Time  `json:"since"`
		Tasks    []TaskStats `json:"tasks"`
		Baseline *Baseline   `json:"baseline"`
	}{since, nonNilTasks(r.Tasks), r.Baseline})
}

func nonNilTasks(ts []TaskStats) []TaskStats {
	if ts == nil {
		return []TaskStats{}
	}
	return ts
}

// Compute is ComputeAt at the clock's now.
func Compute(ctx context.Context, reg *registry.Registry, since time.Time) (Report, error) {
	return ComputeAt(ctx, reg, since, time.Now())
}

// ComputeAt measures the tasks of reg started at or after since (the zero
// time: all), whole, whatever happened to them later, from their spans,
// inputs, questions, decisions and review rounds. now ends a wait on the operator that
// is still open. It reads and never writes. Nothing here reads a transcript
// or GitHub, so the numbers are the evidence the factory recorded.
func ComputeAt(ctx context.Context, reg *registry.Registry, since, now time.Time) (Report, error) {
	tasks, err := reg.Tasks(ctx)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Since: since, Tasks: []TaskStats{}}
	for _, t := range tasks {
		if !since.IsZero() && t.Created.Before(since) {
			continue
		}
		ts, err := taskStats(ctx, reg, t, now)
		if err != nil {
			return Report{}, fmt.Errorf("stats of task %d: %w", t.ID, err)
		}
		rep.Tasks = append(rep.Tasks, ts)
	}
	return rep, nil
}

func taskStats(ctx context.Context, reg *registry.Registry, t registry.Task, now time.Time) (TaskStats, error) {
	spans, err := reg.Spans(ctx, t.ID)
	if err != nil {
		return TaskStats{}, err
	}
	inputs, err := reg.Inputs(ctx, t.ID)
	if err != nil {
		return TaskStats{}, err
	}
	questions, err := reg.Questions(ctx, t.ID)
	if err != nil {
		return TaskStats{}, err
	}
	decisions, err := reg.Decisions(ctx, t.ID)
	if err != nil {
		return TaskStats{}, err
	}
	cycles, err := reg.Cycles(ctx, t.ID)
	if err != nil {
		return TaskStats{}, err
	}
	batches, onQuestions := answeredBatches(questions)
	ts := TaskStats{TaskID: t.ID, Key: taskKey(t), AnswerBatches: batches, Decisions: len(decisions), ReviewRounds: len(cycles),
		OperatorSecs: onQuestions + onDecisions(decisions), WaitedOnYou: waitedOnYou(spans, t, now), StartToReady: startToReady(spans)}
	for _, in := range inputs {
		switch {
		case IsTyped(in.Kind):
			ts.Typed++
			switch classOf(in) {
			case registry.ClassStepStart:
				// The message that starts the task is the one message the
				// factory needs: typed, never a step start, whatever its words
				// say.
				if !startsTask(in) {
					ts.StepStarts++
				}
			case registry.ClassApproval:
				ts.Approvals++
			case registry.ClassCorrection:
				ts.Corrections++
			case registry.ClassAccess:
				ts.Access++
			}
		case in.Kind == registry.InputRetry:
			ts.StepStarts++
		}
	}
	return ts, nil
}

// taskKey names a task as the factory's screens do: its Jira key, else
// "task <id>".
func taskKey(t registry.Task) string {
	if t.JiraKey != "" {
		return t.JiraKey
	}
	return "task " + strconv.FormatInt(t.ID, 10)
}

// classOf is the class of a typed input: the stored one (the engine's, or
// the operator's own correction of it), else Classify's reading of its words,
// which is what the engine would have stored; a dropped paste has no words
// and no class.
func classOf(in registry.Input) string {
	switch {
	case in.Class != "":
		return in.Class
	case in.Text != "":
		return Classify(in.Text)
	}
	return ""
}

// answeredBatches counts the answered batches of questions: one for each
// revision with an answered question, however many submissions answered it.
// The time is the operator's on them: for each such revision, from the first
// time its questions were shown to the first answer, at most cardCap (a
// revision never shown has no time to measure).
func answeredBatches(qs []registry.Question) (batches int, onCards time.Duration) {
	type revision struct{ shown, answered time.Time }
	byRevision := map[int]*revision{}
	for _, q := range qs {
		if q.State != registry.QuestionAnswered || q.Answered.IsZero() {
			continue
		}
		rv := byRevision[q.Revision]
		if rv == nil {
			rv = &revision{answered: q.Answered}
			byRevision[q.Revision] = rv
		}
		if q.Answered.Before(rv.answered) {
			rv.answered = q.Answered
		}
		if !q.Shown.IsZero() && (rv.shown.IsZero() || q.Shown.Before(rv.shown)) {
			rv.shown = q.Shown
		}
	}
	for _, rv := range byRevision {
		onCards += onCard(rv.shown, rv.answered)
	}
	return len(byRevision), onCards
}

// onDecisions adds the operator's time on the cards they decided: each from
// the card shown to the decision.
func onDecisions(ds []registry.Decision) time.Duration {
	var total time.Duration
	for _, d := range ds {
		total += onCard(d.Shown, d.At)
	}
	return total
}

// onCard is the time from a card shown to its answer or decision, at most
// cardCap; zero when it was never shown (or the times do not run forward).
func onCard(shown, answered time.Time) time.Duration {
	if shown.IsZero() || !answered.After(shown) {
		return 0
	}
	return min(answered.Sub(shown), cardCap)
}

// waitedOnYou adds up the spans that wait on the operator, each ending at the
// next span. A wait still open runs to now, or to the task's last change when
// the task is done or stopped.
func waitedOnYou(spans []registry.Span, t registry.Task, now time.Time) time.Duration {
	var total time.Duration
	for i, s := range spans {
		if !s.OnYou {
			continue
		}
		end := now
		switch {
		case i+1 < len(spans):
			end = spans[i+1].At
		case t.State == registry.TaskDone || t.State == registry.TaskStopped:
			end = t.Updated
		}
		if end.After(s.At) {
			total += end.Sub(s.At)
		}
	}
	return total
}

// startToReady is the time from the task's first span to the first one at the
// ready stage that waits on the operator: the card is drawn. Zero before.
func startToReady(spans []registry.Span) time.Duration {
	if len(spans) == 0 {
		return 0
	}
	for _, s := range spans {
		if s.Stage == readyStage && s.OnYou {
			return s.At.Sub(spans[0].At)
		}
	}
	return 0
}

// Markdown renders the report for reading and for the exit test: a table of
// the tasks, then each task set against the baseline's middle half.
func (r Report) Markdown() string {
	var b strings.Builder
	b.WriteString("# Factory stats\n\n")
	if r.Since.IsZero() {
		b.WriteString("All tasks.\n\n")
	} else {
		fmt.Fprintf(&b, "Tasks started since %s.\n\n", r.Since.UTC().Format("2006-01-02 15:04 MST"))
	}
	if len(r.Tasks) == 0 {
		b.WriteString("No tasks.\n")
	} else {
		b.WriteString("| Task | Typed | Step starts | Approvals | Corrections | Access | Answer batches | Decisions | Review rounds | On cards | Waited on you | Start to ready |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, t := range r.Tasks {
			ready := "-"
			if t.StartToReady > 0 {
				ready = fmtDuration(t.StartToReady)
			}
			fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d | %d | %d | %s | %s | %s |\n", t.Key, t.Typed, t.StepStarts, t.Approvals,
				t.Corrections, t.Access, t.AnswerBatches, t.Decisions, t.ReviewRounds, fmtDuration(t.OperatorSecs),
				fmtDuration(t.WaitedOnYou), ready)
		}
	}
	b.WriteString("\n## Against the baseline\n\n")
	bl := r.Baseline
	if bl == nil {
		b.WriteString("No baseline is frozen: `magnum factory baseline --transcripts <dir> --labelled <file>` writes one, once, before the first task.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "Baseline frozen %s over %d tickets, counted per ticket and not per session; the classifier got %.0f%% of the operator's labelled messages right. "+
		"Typed messages per ticket: %s. Step-starting messages per ticket: %s. The target for step starts is zero.\n",
		bl.Frozen.UTC().Format("2006-01-02 15:04 MST"), bl.Tickets, bl.Accuracy*100, bl.TypedPerTicket, bl.StepStartsPerTicket)
	if len(r.Tasks) == 0 {
		return b.String()
	}
	b.WriteString("\n| Task | Typed | Typed against the baseline | Step starts | Step starts against the baseline |\n|---|---|---|---|---|\n")
	for _, t := range r.Tasks {
		fmt.Fprintf(&b, "| %s | %d | %s | %d | %s |\n", t.Key, t.Typed, against(bl.TypedPerTicket, t.Typed), t.StepStarts,
			against(bl.StepStartsPerTicket, t.StepStarts))
	}
	return b.String()
}

// against says where count lies against d's middle half.
func against(d Dist, count int) string {
	return fmt.Sprintf("%s the baseline's middle half (median %s, %s to %s)", d.Where(float64(count)), num(d.Median), num(d.P25), num(d.P75))
}

// fmtDuration spells d to the second: 40s, 25m30s, 2h05m.
func fmtDuration(d time.Duration) string {
	s := seconds(d)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
}
