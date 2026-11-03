package registry

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/store"
)

// Input kinds: what the operator did. The text of an input is whatever words
// came with it; a stop, a drop or a retry has none.
const (
	InputTaskText   = "task_text"
	InputGuidance   = "guidance"
	InputAnswer     = "answer"
	InputDecision   = "decision"
	InputSendBack   = "send_back"
	InputRating     = "rating"
	InputStop       = "stop"
	InputDrop       = "drop"
	InputRetry      = "retry"
	InputPaneTyped  = "pane_typed"
	InputSayOneLine = "say_one_line"
)

// maxInputBefore is the most bytes of the agent's turns kept with an input.
const maxInputBefore = 1024

var inputKinds = map[string]bool{
	InputTaskText: true, InputGuidance: true, InputAnswer: true, InputDecision: true, InputSendBack: true,
	InputRating: true, InputStop: true, InputDrop: true, InputRetry: true, InputPaneTyped: true, InputSayOneLine: true,
}

// Input classes, set later by a classifier: what an input was for.
const (
	ClassStepStart  = "step_start"
	ClassApproval   = "approval"
	ClassCorrection = "correction"
	ClassAccess     = "access"
	ClassInfo       = "info"
)

var inputClasses = map[string]bool{
	ClassStepStart: true, ClassApproval: true, ClassCorrection: true, ClassAccess: true, ClassInfo: true,
}

// Input is one thing the operator said or did to a task. Surface is where it
// came from (a screen, a pane, the CLI), Stage and Role where in the task, Text
// the scrubbed words or DroppedReason why the words were not kept (never both),
// Origin the proof that the operator and no agent produced it, Before the
// agent's two turns before it (scrubbed, at most 1 KiB) and AgentState the
// agent's state when it arrived. Class stays empty until classified.
type Input struct {
	ID, TaskID    int64
	Kind          string
	Surface       string
	Stage, Role   string
	Text          string
	DroppedReason string
	Origin        string
	Before        string
	AgentState    string
	Class         string
	At            time.Time
}

const inputColumns = "id, task_id, kind, surface, stage, role, text, dropped_reason, origin, before_text, agent_state, class, at"

func scanInput(sc scanner) (Input, error) {
	var in Input
	err := sc.Scan(&in.ID, &in.TaskID, &in.Kind, &in.Surface, &in.Stage, &in.Role, &in.Text, &in.DroppedReason,
		&in.Origin, &in.Before, &in.AgentState, &in.Class, timeCol{&in.At})
	return in, err
}

// cutTail keeps the last limit bytes of s, starting at a rune boundary.
func cutTail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	start := len(s) - limit
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// RecordInput stores in and returns its id. The kind is required and must be
// one of the input kinds; Text and DroppedReason are never both set. Before
// keeps its last 1 KiB (the turns nearest the input), cut at a rune boundary.
// Class is normally empty (ClassifyInput sets it later) but may carry a class
// the caller already knows. At defaults to the clock and the task must exist
// (ErrNotFound). The caller scrubs the text and Before (secrets, tokens,
// emails, hosts) first: the registry stores what it is given.
func (r *Registry) RecordInput(ctx context.Context, in Input) (int64, error) {
	if !inputKinds[in.Kind] {
		return 0, fmt.Errorf("record input: unknown kind %q", in.Kind)
	}
	if in.Text != "" && in.DroppedReason != "" {
		return 0, errors.New("record input: an input has a text or a reason it was dropped, never both")
	}
	if in.Class != "" && !inputClasses[in.Class] {
		return 0, fmt.Errorf("record input: unknown class %q", in.Class)
	}
	if in.At.IsZero() {
		in.At = r.now()
	}
	res, err := r.db.ExecContext(ctx, `INSERT INTO operator_inputs (task_id, kind, surface, stage, role, text, dropped_reason, origin, before_text, agent_state, class, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.TaskID, in.Kind, in.Surface, in.Stage, in.Role, in.Text, in.DroppedReason, in.Origin,
		cutTail(in.Before, maxInputBefore), in.AgentState, in.Class, store.FormatTime(in.At))
	if err != nil {
		return 0, fmt.Errorf("record %s input for task %d: %w", in.Kind, in.TaskID, mapErr(err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("record %s input for task %d: %w", in.Kind, in.TaskID, err)
	}
	return id, nil
}

// ClassifyInput sets the class of input id: step_start, approval, correction,
// access or info. A classifier may set it again. A missing input is ErrNotFound.
func (r *Registry) ClassifyInput(ctx context.Context, id int64, class string) error {
	if !inputClasses[class] {
		return fmt.Errorf("classify input %d: unknown class %q", id, class)
	}
	res, err := r.db.ExecContext(ctx, "UPDATE operator_inputs SET class = ? WHERE id = ?", class, id)
	if err != nil {
		return fmt.Errorf("classify input %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("classify input %d: %w", id, ErrNotFound)
	}
	return nil
}

// Inputs returns a task's inputs in the order they were recorded.
func (r *Registry) Inputs(ctx context.Context, taskID int64) ([]Input, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+inputColumns+" FROM operator_inputs WHERE task_id = ? ORDER BY id", taskID)
	if err != nil {
		return nil, fmt.Errorf("inputs of task %d: %w", taskID, err)
	}
	ins, err := collect(rows, scanInput)
	if err != nil {
		return nil, fmt.Errorf("inputs of task %d: %w", taskID, err)
	}
	return ins, nil
}

// Rating is a + (Value 1) or − (Value -1) on a finished task, with the one key
// for the reason and whether the operator opened the diff before giving it (a
// rating given without opening the diff counts as noise).
type Rating struct {
	TaskID          int64
	Value           int
	ReasonKey       string
	DiffOpenedFirst bool
	At              time.Time
}

// RecordRating stores r for its task (ErrNotFound if there is none). Value must
// be 1 or -1; At defaults to the clock. A task may be rated again; every rating
// is kept.
func (r *Registry) RecordRating(ctx context.Context, rt Rating) error {
	if rt.Value != 1 && rt.Value != -1 {
		return fmt.Errorf("record rating: value must be 1 or -1, got %d", rt.Value)
	}
	if rt.At.IsZero() {
		rt.At = r.now()
	}
	_, err := r.db.ExecContext(ctx, "INSERT INTO ratings (task_id, value, reason_key, diff_opened_first, at) VALUES (?, ?, ?, ?, ?)",
		rt.TaskID, rt.Value, rt.ReasonKey, boolInt(rt.DiffOpenedFirst), store.FormatTime(rt.At))
	if err != nil {
		return fmt.Errorf("record rating for task %d: %w", rt.TaskID, mapErr(err))
	}
	return nil
}

// Ratings returns a task's ratings in the order they were given.
func (r *Registry) Ratings(ctx context.Context, taskID int64) ([]Rating, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT task_id, value, reason_key, diff_opened_first, at FROM ratings WHERE task_id = ? ORDER BY id", taskID)
	if err != nil {
		return nil, fmt.Errorf("ratings of task %d: %w", taskID, err)
	}
	rs, err := collect(rows, func(sc scanner) (Rating, error) {
		var rt Rating
		err := sc.Scan(&rt.TaskID, &rt.Value, &rt.ReasonKey, &rt.DiffOpenedFirst, timeCol{&rt.At})
		return rt, err
	})
	if err != nil {
		return nil, fmt.Errorf("ratings of task %d: %w", taskID, err)
	}
	return rs, nil
}
