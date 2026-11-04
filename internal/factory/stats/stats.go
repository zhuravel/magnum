// Package stats measures how much the factory still asks of the operator: per
// task, the messages typed, the messages that only start a step, the answer
// batches, the card decisions and the time a task waited on them, computed
// from the factory registry's evidence records alone and set against the
// baseline frozen before the first task (milestone 1's exit test). Compute
// only reads the registry.
package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// readyStage is the stage whose card waits for the operator's push: the
// built-in ready stage (config's FactoryDecisionReadyPush). A task is ready
// when that stage first waits on the operator.
const readyStage = "ready"

// answerBatchGap is the longest pause inside one batch of answers. The
// factory records one answer input per question, in a row, when the operator
// sends a batch; a human sends the next batch seconds later at the earliest.
const answerBatchGap = 2 * time.Second

// TaskStats is what one task asked of the operator. Typed counts the
// messages the operator typed (typed in a pane, said in one line, the start's
// guidance, the note of a send-back); StepStarts, Approvals, Corrections and
// Access count those of them by class (a retry is a step the operator started
// by hand, so it counts as a step start and not as a typed message); a message
// of another class, or of none, is only typed. AnswerBatches and Decisions
// are counted apart. OperatorSecs is how long the task waited on the operator
// (its spans that wait on "you"), StartToReady how long it took to reach the
// ready card (zero before it does).
type TaskStats struct {
	TaskID int64
	Key    string

	Typed, StepStarts, Approvals, Corrections, Access int
	AnswerBatches, Decisions, ReviewRounds            int

	OperatorSecs time.Duration //nolint:staticcheck // ST1011: the spec's name for the measure ("the operator's seconds per task")
	StartToReady time.Duration
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
		StartToReady  int64  `json:"start_to_ready_secs"`
	}{t.TaskID, t.Key, t.Typed, t.StepStarts, t.Approvals, t.Corrections, t.Access, t.AnswerBatches, t.Decisions, t.ReviewRounds,
		seconds(t.OperatorSecs), seconds(t.StartToReady)})
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
// inputs, decisions and review rounds. now ends a wait on the operator that
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
	decisions, err := reg.Decisions(ctx, t.ID)
	if err != nil {
		return TaskStats{}, err
	}
	cycles, err := reg.Cycles(ctx, t.ID)
	if err != nil {
		return TaskStats{}, err
	}
	ts := TaskStats{TaskID: t.ID, Key: taskKey(t), Decisions: len(decisions), ReviewRounds: len(cycles),
		OperatorSecs: waitedOnYou(spans, t, now), StartToReady: startToReady(spans)}
	var answered []time.Time
	for _, in := range inputs {
		switch in.Kind {
		case registry.InputPaneTyped, registry.InputSayOneLine, registry.InputGuidance, registry.InputSendBack:
			ts.Typed++
			switch classOf(in) {
			case registry.ClassStepStart:
				ts.StepStarts++
			case registry.ClassApproval:
				ts.Approvals++
			case registry.ClassCorrection:
				ts.Corrections++
			case registry.ClassAccess:
				ts.Access++
			}
		case registry.InputRetry:
			ts.StepStarts++
		case registry.InputAnswer:
			answered = append(answered, in.At)
		}
	}
	ts.AnswerBatches = batches(answered)
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
// the operator's own correction of it), else, for what was typed into a pane
// or said in one line, Classify's reading of its words. The start's guidance
// and a send-back's note are the operator's words to the factory, not
// messages to an agent that start a step, so they stay unclassified until
// the engine stores a class.
func classOf(in registry.Input) string {
	switch {
	case in.Class != "":
		return in.Class
	case (in.Kind == registry.InputPaneTyped || in.Kind == registry.InputSayOneLine) && in.Text != "":
		return Classify(in.Text)
	}
	return ""
}

// batches counts the batches of answers sent at the given times: answers
// less than answerBatchGap apart belong to one.
func batches(at []time.Time) int {
	slices.SortFunc(at, time.Time.Compare)
	n := 0
	for i, a := range at {
		if i == 0 || a.Sub(at[i-1]) > answerBatchGap {
			n++
		}
	}
	return n
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
		b.WriteString("| Task | Typed | Step starts | Approvals | Corrections | Access | Answer batches | Decisions | Review rounds | On you | Start to ready |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, t := range r.Tasks {
			ready := "-"
			if t.StartToReady > 0 {
				ready = fmtDuration(t.StartToReady)
			}
			fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d | %d | %d | %s | %s |\n", t.Key, t.Typed, t.StepStarts, t.Approvals,
				t.Corrections, t.Access, t.AnswerBatches, t.Decisions, t.ReviewRounds, fmtDuration(t.OperatorSecs), ready)
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
