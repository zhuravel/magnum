// Package stats measures how much the factory still asks of the operator: per
// task, the messages typed, the messages that only start a step, the answer
// batches, the card decisions, the operator's time on cards and the time a
// task waited on them, computed from the factory registry's evidence records
// alone (spans, inputs, questions, decisions and review rounds) and set
// against the baseline frozen before the first task (milestone 1's exit
// test); and, for a task with a published PR, the PR's quality: what the
// local review missed at magnum's first posted round (the escape checks)
// and the PR's CI failures after the publish (its CI checks). Compute only
// reads the registry.
package stats

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

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
// UnknownOrigin counts apart the turns typed in a pane that nothing proves the
// operator typed (any origin but registry.OriginHerdr: a turn counts as the
// operator's only with positive proof): kept for evaluation only, they are
// never a typed message, a step start or any class.
// AnswerBatches counts the revisions of questions that were answered, and
// Decisions the decisions on cards; both are counted apart. OperatorSecs is
// the operator's time on cards: for each decision, from the card shown to the
// decision, and for each answered revision of questions, from the questions
// shown to the first answer, each at most cardCap (a card never shown has no
// time to measure). WaitedOnYou is how long the task waited on the operator,
// asleep or not (its spans that wait on "you"). StartToReady is how long it
// took to reach the ready card (zero before it does). PR is the task's PR
// (zero before it has one), FixLoops the fix loops magnum started on it
// (nil: not counted, SetFixLoops), and Quality what the PR said of the
// factory's work after the publish (nil for a task without a PR).
// Questions counts the questions the task asked, each once whatever
// revisions carried it; QuestionsFrom counts those of one source by that
// source, and QuestionsShared those of several by the sources joined with
// "+" (questionSources): what each role, a second reader of the ticket's
// points among them, adds alone to what the operator is asked, and what two
// readers raise both. ReviewJudges is the agent kind of the judge each
// round of the local review ran with, by cycle ("" for a round whose begin
// event names no judge: an older build's; nil when no round names one), and
// SecondOpinion the cycle of the task's second opinion (0 for none), from
// the rounds' begin events (roundJudges).
type TaskStats struct {
	TaskID   int64
	Key      string
	PR       PR
	FixLoops *FixLoops
	Quality  *Quality

	Typed, StepStarts, Approvals, Corrections, Access int
	UnknownOrigin                                     int
	AnswerBatches, Decisions, ReviewRounds            int
	Questions                                         int
	QuestionsFrom, QuestionsShared                    map[string]int
	ReviewJudges                                      []string
	SecondOpinion                                     int

	OperatorSecs time.Duration //nolint:staticcheck // ST1011: the spec's name for the measure ("the operator's seconds per task")
	WaitedOnYou  time.Duration
	StartToReady time.Duration
}

// PR names a task's PR: its repository (owner/name) and number.
type PR struct {
	Repo   string
	Number int
}

// String is "owner/name#N", "" for none.
func (p PR) String() string {
	if p.Number <= 0 {
		return ""
	}
	return fmt.Sprintf("%s#%d", p.Repo, p.Number)
}

// FixLoops counts the fix loops magnum started on a PR: by hand (`magnum
// fix`, f on the board) and by autofix. The loops started by hand are the
// "no manual start" measure: the target is zero. Pushes and Commits are the
// fixers' pushes on the PR and the commits they pushed: the fix-loop
// commits after the publish, a measure of what the factory's PR still
// needed.
type FixLoops struct {
	ByHand    int `json:"by_hand"`
	ByAutofix int `json:"by_autofix"`
	Pushes    int `json:"pushes"`
	Commits   int `json:"commits"`
}

// The states of a task's escapes (Quality.EscapesState).
const (
	// EscapesChecked: the task's check is stored.
	EscapesChecked = "checked"
	// EscapesUnrecorded: the check is stored, but magnum recorded none of
	// the P0 to P2 findings its round's result counts: the escapes are
	// unknown.
	EscapesUnrecorded = "unrecorded"
	// EscapesNotYet: the task is published and magnum has posted no round
	// since the publish (or its check is still to run).
	EscapesNotYet = "not_yet"
	// EscapesNotChecked: the task ended (done, stopped, failed) without a
	// check, which only a published task gets: none will come.
	EscapesNotChecked = "not_checked"
)

// Quality is what a task's published PR said of the factory's work after
// the publish: EscapesState whether its local review was checked
// (EscapesChecked and the others), Escapes what the check found (nil
// without a check), CIFailed the failed CI on the PR's heads (GitHub's ci
// checks, one per head and result) but those marked not ours, and
// CINotOurs those apart.
type Quality struct {
	EscapesState string   `json:"escapes_state"`
	Escapes      *Escapes `json:"escapes"`
	CIFailed     int      `json:"ci_failed"`
	CINotOurs    int      `json:"ci_failed_not_ours"`
}

// Escapes counts what the local review missed at magnum's first posted
// round on a task's PR (the registry's escape check): Posted is how many
// P0 to P2 findings that round posted, P0, P1 and P2 the escapes by
// priority (the P0 to P2 findings no local finding caught), and Unplaced
// those of them with no line any local finding could be near (a finding of
// the review's body, say). SameHeadOnly counts the files of the posted
// findings the check compared at the same head only (git could not compare
// them at two heads, so a local finding at another head caught nothing
// there), and Unrecorded the P0 to P2 findings the round's result counts
// that magnum recorded no row of (the escapes are then unknown).
type Escapes struct {
	Posted, P0, P1, P2, Unplaced int
	SameHeadOnly, Unrecorded     int
}

// Total is the escapes of every priority.
func (e Escapes) Total() int { return e.P0 + e.P1 + e.P2 }

// MarshalJSON writes the counts with fixed keys, and their total.
func (e Escapes) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Posted       int `json:"posted"`
		P0           int `json:"p0"`
		P1           int `json:"p1"`
		P2           int `json:"p2"`
		Unplaced     int `json:"unplaced"`
		SameHeadOnly int `json:"same_head_only"`
		Unrecorded   int `json:"unrecorded"`
		Total        int `json:"total"`
	}{e.Posted, e.P0, e.P1, e.P2, e.Unplaced, e.SameHeadOnly, e.Unrecorded, e.Total()})
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
		Task            int64          `json:"task"`
		Key             string         `json:"key"`
		Typed           int            `json:"typed"`
		StepStarts      int            `json:"step_starts"`
		Approvals       int            `json:"approvals"`
		Corrections     int            `json:"corrections"`
		Access          int            `json:"access"`
		UnknownOrigin   int            `json:"unknown_origin"`
		AnswerBatches   int            `json:"answer_batches"`
		Decisions       int            `json:"decisions"`
		ReviewRounds    int            `json:"review_rounds"`
		ReviewJudges    []string       `json:"review_judges,omitempty"`
		SecondOpinion   int            `json:"second_opinion,omitempty"`
		OperatorSecs    int64          `json:"operator_secs"`
		WaitedOnYou     int64          `json:"waited_on_you_secs"`
		StartToReady    int64          `json:"start_to_ready_secs"`
		Questions       int            `json:"questions,omitempty"`
		QuestionsFrom   map[string]int `json:"questions_from,omitempty"`
		QuestionsShared map[string]int `json:"questions_shared,omitempty"`
		PR              string         `json:"pr,omitempty"`
		FixLoops        *FixLoops      `json:"fix_loops,omitempty"`
		Quality         *Quality       `json:"quality,omitempty"`
	}{t.TaskID, t.Key, t.Typed, t.StepStarts, t.Approvals, t.Corrections, t.Access, t.UnknownOrigin, t.AnswerBatches, t.Decisions,
		t.ReviewRounds, t.ReviewJudges, t.SecondOpinion, seconds(t.OperatorSecs), seconds(t.WaitedOnYou), seconds(t.StartToReady), t.Questions, t.QuestionsFrom,
		t.QuestionsShared, t.PR.String(), t.FixLoops, t.Quality})
}

func seconds(d time.Duration) int64 { return int64(d.Round(time.Second) / time.Second) }

// Report is the stats of the tasks started at or after Since (the zero time:
// all of them), oldest first, and the baseline to read them against (nil
// when none is frozen; Compute leaves it to the caller to load).
// FixLoopsRead says SetFixLoops counted the tasks' fix loops, and
// FixLoopsUnread why it could not ("" when it could, or was not asked).
type Report struct {
	Since          time.Time
	Tasks          []TaskStats
	Baseline       *Baseline
	FixLoopsRead   bool
	FixLoopsUnread string
	// JiraMoves are the factory's plans of its Jira moves over the tasks
	// (nil when none has one): the shadow week's evidence.
	JiraMoves *JiraMoves
	// Take is whether your answers took the recommendations, and Ratings
	// what you rated with + and −, over the tasks (nil when none is
	// recorded): your explicit signals, from the registry's records.
	Take    *TakeTotals
	Ratings *RatingTotals
}

// JiraMoves count the factory's plans of the Jira moves of a report's
// tasks, from the records the factory writes (registry.JiraPlans): Plans
// the plans made (settled); Planned those that planned a move, and of
// them Matched those your own move to the target followed (MedianLagSecs
// the median lag from the plan), TargetByOther and TargetByUnknown those
// moved to the target by someone else (Jira's automation among them) or by
// an author not known, Elsewhere those moved elsewhere, NotMoved those
// whose task ended with no move, Open those nothing was seen after yet; NothingToMove the plans of an issue at the target or past it;
// CouldNot the plans the factory could not make, by outcome
// (registry.PlanAssignedOther and the others); Pending the plans that wait
// for their read.
type JiraMoves struct {
	Plans           int            `json:"plans"`
	Planned         int            `json:"planned"`
	Matched         int            `json:"matched"`
	MedianLagSecs   int64          `json:"median_lag_secs"`
	TargetByOther   int            `json:"target_by_other"`
	TargetByUnknown int            `json:"target_by_unknown"`
	Elsewhere       int            `json:"elsewhere"`
	NotMoved        int            `json:"not_moved"`
	Open            int            `json:"open"`
	NothingToMove   int            `json:"nothing_to_move"`
	CouldNot        map[string]int `json:"could_not"`
	Pending         int            `json:"pending"`
}

// jiraMoves counts the plans of the tasks ids (nil when none has one).
func jiraMoves(plans []registry.JiraPlan, ids map[int64]bool) *JiraMoves {
	var m JiraMoves
	var lags []int64
	found := false
	for _, p := range plans {
		if !ids[p.TaskID] {
			continue
		}
		found = true
		if p.State == registry.EffectBegin {
			m.Pending++
			continue
		}
		m.Plans++
		switch p.Result.Outcome {
		case registry.PlanPlanned:
			m.Planned++
			switch {
			case p.Seen == nil:
				m.Open++
			case p.Seen.Moved == registry.SeenTarget && p.Seen.Yours():
				m.Matched++
				lags = append(lags, p.Seen.LagSecs)
			case p.Seen.Moved == registry.SeenTarget && p.Seen.By == "":
				m.TargetByUnknown++
			case p.Seen.Moved == registry.SeenTarget:
				m.TargetByOther++
			case p.Seen.Moved == registry.SeenElsewhere:
				m.Elsewhere++
			default:
				m.NotMoved++
			}
		case registry.PlanAlready:
			m.NothingToMove++
		default:
			if m.CouldNot == nil {
				m.CouldNot = map[string]int{}
			}
			m.CouldNot[cmp.Or(p.Result.Outcome, "unknown")]++
		}
	}
	if !found {
		return nil
	}
	if n := len(lags); n > 0 {
		slices.Sort(lags)
		m.MedianLagSecs = (lags[(n-1)/2] + lags[n/2]) / 2
	}
	return &m
}

// couldNotWords say why a plan could not be made, by outcome.
var couldNotWords = map[string]string{
	registry.PlanAssignedOther: "assigned to someone else",
	registry.PlanNoTransition:  "no transition",
	registry.PlanNeedsFields:   "fields wanted",
	registry.PlanTargetDone:    "a done target",
	registry.PlanUnread:        "unread",
}

// jiraMovesMarkdown is the shadow week's line of the Jira moves, when a
// task has a plan.
func (r Report) jiraMovesMarkdown(b *strings.Builder) {
	m := r.JiraMoves
	if m == nil {
		return
	}
	lag := "no lag"
	if m.Matched > 0 {
		lag = "median lag " + fmtDuration(time.Duration(m.MedianLagSecs)*time.Second)
	}
	couldNot := 0
	var reasons []string
	for _, k := range slices.SortedFunc(maps.Keys(m.CouldNot), func(a, b string) int {
		return strings.Compare(cmp.Or(couldNotWords[a], a), cmp.Or(couldNotWords[b], b))
	}) {
		couldNot += m.CouldNot[k]
		reasons = append(reasons, fmt.Sprintf("%s %d", cmp.Or(couldNotWords[k], k), m.CouldNot[k]))
	}
	fmt.Fprintf(b, "\n## Jira moves\n\nJira moves in shadow: %s: %d planned (%d matched your move, %s; %d moved to the target by someone "+
		"else; %d moved to the target, its author unknown; %d moved elsewhere; %d not moved by the task's end; %d open), %d with nothing to "+
		"move, %d could not be made", plural(m.Plans, "plan"), m.Planned, m.Matched, lag, m.TargetByOther, m.TargetByUnknown, m.Elsewhere,
		m.NotMoved, m.Open, m.NothingToMove, couldNot)
	if len(reasons) > 0 {
		fmt.Fprintf(b, " (%s)", strings.Join(reasons, ", "))
	}
	waits := "wait for their read"
	if m.Pending == 1 {
		waits = "waits for its read"
	}
	fmt.Fprintf(b, "; %d %s.\n", m.Pending, waits)
	b.WriteString("\nThe factory plans each move at its milestone and writes nothing to Jira (jira_moves = \"shadow\"); a match is your own " +
		"move to the plan's target after it, by the changelog entry's author, its lag from the plan to your move; a move to the target by " +
		"another account (a person, or Jira's automation) or by an author not known is no match.\n")
}

// SetFixLoops gives each task with a PR the fix loops magnum started on it
// (loops, read from magnum's registry by the caller: this package reads
// only the factory's; a PR loops lacks counts zero), or, with unread set,
// records why they could not be read and counts none.
func (r *Report) SetFixLoops(loops map[PR]FixLoops, unread string) {
	r.FixLoopsRead, r.FixLoopsUnread = unread == "", unread
	for i := range r.Tasks {
		t := &r.Tasks[i]
		t.FixLoops = nil
		if unread == "" && t.PR.Number > 0 {
			c := loops[t.PR]
			t.FixLoops = &c
		}
	}
}

// fixLoopsByHand is the fix loops started by hand on the tasks' PRs.
func (r Report) fixLoopsByHand() int {
	n := 0
	for _, t := range r.Tasks {
		if t.FixLoops != nil {
			n += t.FixLoops.ByHand
		}
	}
	return n
}

// MarshalJSON writes the report with fixed keys, an empty list for no tasks
// and null for no window and no baseline.
func (r Report) MarshalJSON() ([]byte, error) {
	var since *time.Time
	if !r.Since.IsZero() {
		s := r.Since.UTC()
		since = &s
	}
	var byHand *int
	if r.FixLoopsRead {
		n := r.fixLoopsByHand()
		byHand = &n
	}
	return json.Marshal(struct {
		Since          *time.Time     `json:"since"`
		Tasks          []TaskStats    `json:"tasks"`
		Baseline       *Baseline      `json:"baseline"`
		FixLoopsByHand *int           `json:"fix_loops_by_hand,omitempty"`
		FixLoopsUnread string         `json:"fix_loops_unread,omitempty"`
		Quality        *QualityTotals `json:"quality,omitempty"`
		JiraMoves      *JiraMoves     `json:"jira_moves,omitempty"`
		Take           *TakeTotals    `json:"take,omitempty"`
		Ratings        *RatingTotals  `json:"ratings,omitempty"`
	}{since, nonNilTasks(r.Tasks), r.Baseline, byHand, r.FixLoopsUnread, r.Quality(), r.JiraMoves, r.Take, r.Ratings})
}

// QualityTotals are the quality numbers over a report's tasks with a PR:
// how many PRs; how many were checked for escapes (Checked, those whose
// round's findings magnum recorded), how many are not yet, not checked, or
// checked with their findings unrecorded (ChecksUnrecorded), and how many
// of the checked ones compared some files at the same head only
// (ChecksSameHeadOnly); the P0 to P2 findings the checked ones' first
// posted rounds posted, the escapes in all and by priority with those with
// no line apart; the CI failures after the publish with those not ours
// apart; and, when SetFixLoops read them, the fixers' pushes and their
// commits (nil: not read).
type QualityTotals struct {
	PRs                int  `json:"prs"`
	Checked            int  `json:"checked"`
	NotYet             int  `json:"not_yet"`
	NotChecked         int  `json:"not_checked"`
	ChecksUnrecorded   int  `json:"checks_unrecorded"`
	ChecksSameHeadOnly int  `json:"checks_same_head_only"`
	Posted             int  `json:"posted"`
	EscapesTotal       int  `json:"escapes_total"`
	P0                 int  `json:"p0"`
	P1                 int  `json:"p1"`
	P2                 int  `json:"p2"`
	Unplaced           int  `json:"unplaced"`
	CIFailed           int  `json:"ci_failed"`
	CINotOurs          int  `json:"ci_failed_not_ours"`
	FixPushes          *int `json:"fix_pushes,omitempty"`
	FixCommits         *int `json:"fix_commits,omitempty"`
}

// Quality totals the report's tasks with a PR (Quality); nil when no task
// has one.
func (r Report) Quality() *QualityTotals {
	var q QualityTotals
	pushes, commits := 0, 0
	for _, t := range r.Tasks {
		if t.Quality == nil {
			continue
		}
		q.PRs++
		q.CIFailed += t.Quality.CIFailed
		q.CINotOurs += t.Quality.CINotOurs
		if t.FixLoops != nil {
			pushes += t.FixLoops.Pushes
			commits += t.FixLoops.Commits
		}
		e := t.Quality.Escapes
		switch t.Quality.EscapesState {
		case EscapesNotYet:
			q.NotYet++
			continue
		case EscapesNotChecked:
			q.NotChecked++
			continue
		case EscapesUnrecorded:
			q.ChecksUnrecorded++
			continue
		}
		if e == nil {
			continue
		}
		q.Checked++
		if e.SameHeadOnly > 0 {
			q.ChecksSameHeadOnly++
		}
		q.Posted += e.Posted
		q.EscapesTotal += e.Total()
		q.P0, q.P1, q.P2, q.Unplaced = q.P0+e.P0, q.P1+e.P1, q.P2+e.P2, q.Unplaced+e.Unplaced
	}
	if q.PRs == 0 {
		return nil
	}
	if r.FixLoopsRead {
		q.FixPushes, q.FixCommits = &pushes, &commits
	}
	return &q
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
// inputs, questions, decisions and review rounds, and a published task's
// escape check, escapes and CI checks. now ends a wait on the operator that
// is still open. It reads and never writes. Nothing here reads a transcript
// or GitHub, so the numbers are the evidence the factory recorded.
func ComputeAt(ctx context.Context, reg *registry.Registry, since, now time.Time) (Report, error) {
	tasks, err := reg.Tasks(ctx)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Since: since, Tasks: []TaskStats{}}
	ids := map[int64]bool{}
	sig := newSignals()
	for _, t := range tasks {
		if !since.IsZero() && t.Created.Before(since) {
			continue
		}
		ts, err := taskStats(ctx, reg, t, now)
		if err != nil {
			return Report{}, fmt.Errorf("stats of task %d: %w", t.ID, err)
		}
		if err := sig.add(ctx, reg, t); err != nil {
			return Report{}, fmt.Errorf("stats of task %d: %w", t.ID, err)
		}
		rep.Tasks = append(rep.Tasks, ts)
		ids[t.ID] = true
	}
	rep.Take, rep.Ratings = sig.totals()
	plans, err := reg.JiraPlans(ctx)
	if err != nil {
		return Report{}, err
	}
	rep.JiraMoves = jiraMoves(plans, ids)
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
	pr := PR{Repo: t.Repo, Number: t.PRNumber}
	if l, err := reg.LinkedPR(ctx, t.ID); err == nil && l.Number > 0 {
		pr = PR{Repo: cmp.Or(l.Repo, t.Repo), Number: l.Number}
	} else if err != nil && !errors.Is(err, registry.ErrNotFound) {
		return TaskStats{}, err
	}
	var quality *Quality
	if pr.Number <= 0 {
		pr = PR{}
	} else if quality, err = qualityOf(ctx, reg, t); err != nil {
		return TaskStats{}, err
	}
	relays, err := relayRoles(ctx, reg, t.ID)
	if err != nil {
		return TaskStats{}, err
	}
	judges, second, err := roundJudges(ctx, reg, t.ID, cycles)
	if err != nil {
		return TaskStats{}, err
	}
	batches, onQuestions := answeredBatches(questions)
	asked, from, shared := questionSources(questions, relays)
	ts := TaskStats{TaskID: t.ID, Key: taskKey(t), PR: pr, Quality: quality, AnswerBatches: batches, Decisions: len(decisions),
		Questions: asked, QuestionsFrom: from, QuestionsShared: shared,
		ReviewRounds: len(cycles), ReviewJudges: judges, SecondOpinion: second, OperatorSecs: onQuestions + onDecisions(decisions), WaitedOnYou: waitedOnYou(spans, t, now),
		StartToReady: startToReady(spans)}
	for _, in := range inputs {
		switch {
		case in.Kind == registry.InputPaneTyped && in.Origin != registry.OriginHerdr:
			ts.UnknownOrigin++ // for evaluation only: nothing proves the operator typed it
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

// qualityOf is what task t's PR said of the factory's work after the
// publish, from the registry: its escape check and escapes (none stored:
// not yet while it is published, else not checked, as only a published
// task is checked), and its failed CI checks of GitHub's, those not ours
// apart.
func qualityOf(ctx context.Context, reg *registry.Registry, t registry.Task) (*Quality, error) {
	id := t.ID
	q := &Quality{EscapesState: EscapesNotChecked}
	if t.State == registry.TaskPublished {
		q.EscapesState = EscapesNotYet
	}
	switch c, err := reg.EscapeCheck(ctx, id); {
	case err == nil:
		es, err := reg.Escapes(ctx, id)
		if err != nil {
			return nil, err
		}
		q.EscapesState = EscapesChecked
		if c.Unrecorded > 0 {
			q.EscapesState = EscapesUnrecorded
		}
		q.Escapes = &Escapes{Posted: c.Posted, SameHeadOnly: c.SameHeadOnly, Unrecorded: c.Unrecorded}
		for _, e := range es {
			switch e.Priority {
			case "P0":
				q.Escapes.P0++
			case "P1":
				q.Escapes.P1++
			case "P2":
				q.Escapes.P2++
			}
			if e.Reason == registry.EscapeUnplaced {
				q.Escapes.Unplaced++
			}
		}
	case !errors.Is(err, registry.ErrNotFound):
		return nil, err
	}
	checks, err := reg.Checks(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, c := range checks {
		switch {
		case c.Kind != registry.CheckCI || c.Source != registry.SourceGitHub || c.Result != registry.CheckFail:
		case c.Cause == registry.CauseNotOurs:
			q.CINotOurs++
		default:
			q.CIFailed++
		}
	}
	return q, nil
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

// findingsRef is an investigator's findings file a question's evidence
// names, findings-<role>.md: its role is the question's source too.
var findingsRef = regexp.MustCompile(`findings-([A-Za-z0-9_.-]+?)\.md\b`)

// relayRoles are the roles that ran task id's report stage (a role run whose
// end fact is the report): the judge, whose questions.json relays what the
// investigators' findings leave open, citing their findings files.
func relayRoles(ctx context.Context, reg *registry.Registry, id int64) (map[string]bool, error) {
	as, err := reg.Attempts(ctx, id)
	if err != nil {
		return nil, err
	}
	relays := map[string]bool{}
	for _, a := range as {
		rrs, err := reg.RoleRuns(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		for _, rr := range rrs {
			if rr.EndFact == config.FactoryEndReport {
				relays[rr.Role] = true
			}
		}
	}
	return relays, nil
}

// roundJudge is the begin event of a local round that names its judge
// (internal/factory/review.go, launchRound: "round 2 (cycle 2) at <head>:
// <roles>; judge claude-judge (claude), a second opinion in place of ..."):
// its cycle, its judge's agent kind, and whether it is a second opinion.
var roundJudge = regexp.MustCompile(`^round [1-9][0-9]* \(cycle ([1-9][0-9]*)\) at [0-9a-f]{7,64}: [^;]+; judge [a-z][a-z0-9_-]* ` +
	`\(([a-z][a-z0-9_-]*)\)(, a second opinion in place of )?`)

// roundJudges are the judge kinds of task id's local rounds, by cycle
// (TaskStats.ReviewJudges: nil when no begin event names a judge), and the
// cycle of its second opinion (0 for none), each from its cycle's newest
// begin event.
func roundJudges(ctx context.Context, reg *registry.Registry, id int64, cycles []registry.Cycle) ([]string, int, error) {
	evs, err := reg.TaskEvents(ctx, id, "review", 0)
	if err != nil {
		return nil, 0, err
	}
	kinds, seconds := map[int]string{}, map[int]bool{}
	for _, ev := range evs {
		m := roundJudge.FindStringSubmatch(ev.Text)
		if ev.Phase != registry.PhaseBegin || m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		kinds[n], seconds[n] = m[2], m[3] != ""
	}
	if len(kinds) == 0 {
		return nil, 0, nil
	}
	out, second := make([]string, len(cycles)), 0
	for i, c := range cycles {
		out[i] = kinds[c.N]
		if seconds[c.N] {
			second = c.N
		}
	}
	return out, second, nil
}

// questionSources counts the task's questions qs once each, by their text
// (a revision carries an open question over word for word), and each by its
// sources: the roles its raised_by names ("fx-judge, fx-points" when two
// roles' files hold it) other than the relays (relayRoles, the judge), each
// of which raised it in its own questions file; for a question no other
// role raised, the investigators whose findings files its evidence names
// (the judge cites the findings a question comes from, and only relays it),
// else the roles its raised_by names. So the evidence decides for a question
// the judge alone raised, never for another role's: fx-points' question that
// the judge asks too, citing the twin's findings, is fx-points'. A question
// of one source counts in from for that source; one of several counts in
// shared for the sources sorted and joined with "+". A task with no relay
// (no run of it ended on the report: an older task, or a judge whose run
// never settled) has no judge to tell apart, so the evidence decides for
// its every question, else raised_by. The keys are role names only. from
// and shared are nil when empty.
func questionSources(qs []registry.Question, relays map[string]bool) (asked int, from, shared map[string]int) {
	seen := map[string]bool{}
	count := func(m *map[string]int, key string) {
		if *m == nil {
			*m = map[string]int{}
		}
		(*m)[key]++
	}
	for _, q := range qs {
		if seen[q.Text] {
			continue
		}
		seen[q.Text] = true
		asked++
		switch names := sourcesOf(q, relays); len(names) {
		case 0:
		case 1:
			count(&from, names[0])
		default:
			count(&shared, strings.Join(names, "+"))
		}
	}
	return asked, from, shared
}

// sourcesOf is the sources of question q, sorted: see questionSources for how
// the roles its raised_by names and the findings its evidence cites decide.
func sourcesOf(q registry.Question, relays map[string]bool) []string {
	var raisers []string
	for r := range strings.SplitSeq(q.RaisedBy, ",") {
		if r = strings.TrimSpace(r); r != "" {
			raisers = append(raisers, r)
		}
	}
	roles := map[string]bool{}
	for _, r := range raisers {
		if len(relays) > 0 && !relays[r] {
			roles[r] = true
		}
	}
	if len(roles) == 0 {
		for _, m := range findingsRef.FindAllStringSubmatch(q.Evidence, -1) {
			roles[m[1]] = true
		}
	}
	if len(roles) == 0 {
		for _, r := range raisers {
			roles[r] = true
		}
	}
	return slices.Sorted(maps.Keys(roles))
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

// startToReady is the time from the task's first span to the first one that
// waits on the operator at a ready card (registry.ReadyWait): the card is
// drawn. The card's wait is found by its words, whatever the kind names the
// stage that draws it. Zero before.
func startToReady(spans []registry.Span) time.Duration {
	if len(spans) == 0 {
		return 0
	}
	for _, s := range spans {
		if s.OnYou && strings.HasPrefix(s.WaitsWhat, registry.ReadyWait) {
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
		b.WriteString("| Task | Typed | Step starts | Approvals | Corrections | Access | Origin unknown | Answer batches | Decisions | Review rounds | On cards | Waited on you | Start to ready |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, t := range r.Tasks {
			ready := "-"
			if t.StartToReady > 0 {
				ready = fmtDuration(t.StartToReady)
			}
			fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %s | %s | %s |\n", t.Key, t.Typed, t.StepStarts, t.Approvals,
				t.Corrections, t.Access, t.UnknownOrigin, t.AnswerBatches, t.Decisions, t.ReviewRounds, fmtDuration(t.OperatorSecs),
				fmtDuration(t.WaitedOnYou), ready)
		}
		b.WriteString("\nOrigin unknown: turns typed in a pane that nothing proves you typed, kept for evaluation only and never counted as typed.\n")
	}
	r.questionsMarkdown(&b)
	r.judgesMarkdown(&b)
	r.fixLoopsMarkdown(&b)
	r.qualityMarkdown(&b)
	r.takeMarkdown(&b)
	r.ratingsMarkdown(&b)
	r.jiraMovesMarkdown(&b)
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

// questionsMarkdown is the section of the questions each task asked, by
// source (TaskStats.QuestionsFrom, roles by name), for the tasks that asked
// any; none, no section.
func (r Report) questionsMarkdown(b *strings.Builder) {
	head := false
	for _, t := range r.Tasks {
		if t.Questions == 0 {
			continue
		}
		if !head {
			b.WriteString("\n## Questions by source\n\n| Task | Questions | From one source | Shared |\n|---|---|---|---|\n")
			head = true
		}
		fmt.Fprintf(b, "| %s | %d | %s | %s |\n", t.Key, t.Questions, countsLine(t.QuestionsFrom), countsLine(t.QuestionsShared))
	}
	if head {
		b.WriteString("\nA question counts once per task. Its sources are the investigators whose findings file its evidence names, else the roles " +
			"that raised it: one source counts for that role alone (what it adds), several count apart as shared.\n")
	}
}

// judgesMarkdown is the section of the judge kinds each task's local
// rounds ran with (TaskStats.ReviewJudges), the second opinion named, for
// the tasks whose rounds name any; none, no section.
func (r Report) judgesMarkdown(b *strings.Builder) {
	head := false
	for _, t := range r.Tasks {
		if t.ReviewJudges == nil {
			continue
		}
		if !head {
			b.WriteString("\n## The local review's judges\n\n| Task | Rounds | Judge kinds |\n|---|---|---|\n")
			head = true
		}
		kinds := make([]string, len(t.ReviewJudges))
		for i, k := range t.ReviewJudges {
			kinds[i] = cmp.Or(k, "unknown")
			if i+1 == t.SecondOpinion {
				kinds[i] += " (the second opinion)"
			}
		}
		fmt.Fprintf(b, "| %s | %d | %s |\n", t.Key, t.ReviewRounds, strings.Join(kinds, ", "))
	}
	if head {
		b.WriteString("\nEach round's judge by its agent kind, in order; the second opinion is the round after a clean one with the judge of the " +
			"other kind (`[factory.local_review] second_judge`); unknown: a round from before the factory named its judge.\n")
	}
}

// countsLine is counts as "key n" by key, comma-separated; "-" for none.
func countsLine(counts map[string]int) string {
	if len(counts) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(counts))
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

// fixLoopsMarkdown is the section of the fix loops magnum started on the
// tasks' PRs, when SetFixLoops was asked: per task with a PR, by hand and
// by autofix, then the total started by hand (the target is zero); a
// registry it could not read says so.
func (r Report) fixLoopsMarkdown(b *strings.Builder) {
	switch {
	case r.FixLoopsUnread != "":
		fmt.Fprintf(b, "\n## Fix loops on the factory's PRs\n\nmagnum's registry was not read (%s): no fix loop is counted.\n", r.FixLoopsUnread)
		return
	case !r.FixLoopsRead:
		return
	}
	b.WriteString("\n## Fix loops on the factory's PRs\n\n")
	counted := false
	for _, t := range r.Tasks {
		if t.FixLoops == nil {
			continue
		}
		counted = true
		fmt.Fprintf(b, "- %s (%s): fix loops on its PR: %d by hand, %d by autofix\n", t.Key, t.PR, t.FixLoops.ByHand, t.FixLoops.ByAutofix)
	}
	if !counted {
		b.WriteString("No task has a PR yet.\n")
		return
	}
	fmt.Fprintf(b, "\nFix loops started by hand on factory PRs: %d. A loop you start by hand (f on the board, `magnum fix`) is one "+
		"autofix did not start: the target is zero. Counted from magnum's events, which it keeps for [daemon] keep_events.\n", r.fixLoopsByHand())
}

// qualityMarkdown is the section of the published PRs' quality, when a
// task has a PR: per task, the escapes at magnum's first posted round ("not
// yet" before one), the fixers' pushes after the publish ("-" when magnum's
// registry was not read) and the CI failures after the publish, those not
// ours apart; then the totals, and what an escape is.
func (r Report) qualityMarkdown(b *strings.Builder) {
	q := r.Quality()
	if q == nil {
		return
	}
	b.WriteString("\n## The quality of the published PRs\n\n")
	b.WriteString("| Task | PR | Escapes at the first posted round | Fix pushes after the publish | CI failures after the publish |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, t := range r.Tasks {
		if t.Quality == nil {
			continue
		}
		pushes := "-"
		if r.FixLoopsRead && t.FixLoops != nil {
			pushes = fmt.Sprintf("%d (%s)", t.FixLoops.Pushes, plural(t.FixLoops.Commits, "commit"))
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", t.Key, t.PR, escapesWords(*t.Quality), pushes,
			ciWords(t.Quality.CIFailed, t.Quality.CINotOurs))
	}
	fmt.Fprintf(b, "\nIn total over %s: %s at %d checked %s", plural(q.PRs, "PR"), plural(q.EscapesTotal, "escape"), q.Checked, word(q.Checked, "PR"))
	var notes []string
	if q.EscapesTotal > 0 {
		notes = append(notes, byPriority(q.P0, q.P1, q.P2, q.Unplaced))
	}
	if q.ChecksSameHeadOnly > 0 {
		notes = append(notes, plural(q.ChecksSameHeadOnly, "PR")+" with files compared at the same head only")
	}
	if len(notes) > 0 {
		fmt.Fprintf(b, " (%s)", strings.Join(notes, "; "))
	}
	fmt.Fprintf(b, ", %d not yet", q.NotYet)
	if q.NotChecked > 0 {
		fmt.Fprintf(b, ", %d not checked", q.NotChecked)
	}
	if q.ChecksUnrecorded > 0 {
		fmt.Fprintf(b, ", %d whose findings magnum did not record", q.ChecksUnrecorded)
	}
	b.WriteString("; ")
	if q.FixPushes != nil {
		fmt.Fprintf(b, "%s (%s); ", plural(*q.FixPushes, "fix push"), plural(*q.FixCommits, "commit"))
	} else {
		b.WriteString("fix pushes not read; ")
	}
	fmt.Fprintf(b, "%s after the publish", plural(q.CIFailed, "CI failure"))
	if q.CINotOurs > 0 {
		fmt.Fprintf(b, ", and %d not ours", q.CINotOurs)
	}
	b.WriteString(".\n\nAn escape is a P0 to P2 finding of magnum's first round on the PR that posted a review after the publish, which no " +
		"finding of the local review caught: none on its file within 3 lines, at a head where the file reads the same (a P3 is optional and " +
		"never an escape). A file compared at the same head only is one git could not compare at the local finding's head and the posted " +
		"round's (no copy of the repository on disk, git failing, or its bound running out, at the last of a check's tries): a local " +
		"finding at another head caught nothing there. Not yet: magnum has posted no round on the PR since the publish. Not checked: the task " +
		"ended without a check, which only a published task gets. Unknown: magnum recorded none of the findings its round's result counts. " +
		"The fix pushes are those of magnum's fixers on " +
		"the PR, counted from magnum's events; the CI failures are the failed heads of the PR's CI as magnum last read it, one per head.\n")
}

// escapesWords is a task's escapes in the quality table: "not yet" or
// "not checked" without a check, "unknown: ..." for a check whose round's
// findings magnum did not record, "none of 4 posted", or "2 of 4 posted
// (P0 0, P1 1, P2 1)", with the files compared at the same head only after.
func escapesWords(q Quality) string {
	e := q.Escapes
	switch {
	case q.EscapesState == EscapesNotChecked:
		return "not checked"
	case e == nil:
		return "not yet"
	case q.EscapesState == EscapesUnrecorded:
		return fmt.Sprintf("unknown: magnum recorded none of the %s its round counts", plural(e.Unrecorded, "P0 to P2 finding"))
	}
	s := fmt.Sprintf("none of %d posted", e.Posted)
	if e.Total() > 0 {
		s = fmt.Sprintf("%d of %d posted (%s)", e.Total(), e.Posted, byPriority(e.P0, e.P1, e.P2, e.Unplaced))
	}
	if e.SameHeadOnly > 0 {
		s += "; " + plural(e.SameHeadOnly, "file") + " compared at the same head only"
	}
	return s
}

// byPriority is escapes by priority, those with no line after.
func byPriority(p0, p1, p2, unplaced int) string {
	s := fmt.Sprintf("P0 %d, P1 %d, P2 %d", p0, p1, p2)
	if unplaced > 0 {
		s += fmt.Sprintf("; %d with no line", unplaced)
	}
	return s
}

// ciWords is a task's CI failures after the publish: "2", or "2, and 1 not
// ours".
func ciWords(failed, notOurs int) string {
	if notOurs == 0 {
		return strconv.Itoa(failed)
	}
	return fmt.Sprintf("%d, and %d not ours", failed, notOurs)
}

// plural is "1 thing" or "n things" ("es" after an s or an h).
func plural(n int, thing string) string { return strconv.Itoa(n) + " " + word(n, thing) }

// word is thing, in the plural unless n is 1.
func word(n int, thing string) string {
	switch {
	case n == 1:
		return thing
	case strings.HasSuffix(thing, "s"), strings.HasSuffix(thing, "h"):
		return thing + "es"
	}
	return thing + "s"
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
