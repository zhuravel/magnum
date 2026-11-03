package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// Question states: open until the operator answers it or newer evidence
// replaces it (voided).
const (
	QuestionOpen     = "open"
	QuestionAnswered = "answered"
	QuestionVoided   = "voided"
)

// Option is one answer a question offers: its label, and what it means for
// the question's example.
type Option struct {
	Label string `json:"label"`
	Means string `json:"means"`
}

// Question is one question of a task to the operator. A revision is the set
// of questions the agents asked at one time; Index is the position within it.
type Question struct {
	ID       int64
	TaskID   int64
	Revision int
	Index    int
	Text     string
	// Context is the one line that says why it is asked, Example a concrete
	// case the options can be read against.
	Context, Example string
	Options          []Option
	// Recommended is an index into Options, or -1 for "no recommendation";
	// Why is the reason for the recommendation.
	Recommended int
	Why         string
	Answer      string
	State       string
	// Asked is when the revision was stored, Answered when the answer was
	// (the zero time while the question is open or voided).
	Asked, Answered time.Time
}

const questionColumns = "id, task_id, revision, idx, text, context, example, options_json, recommended, why, answer, state, asked_at, answered_at"

func scanQuestion(sc scanner) (Question, error) {
	var q Question
	var options string
	err := sc.Scan(&q.ID, &q.TaskID, &q.Revision, &q.Index, &q.Text, &q.Context, &q.Example, &options,
		&q.Recommended, &q.Why, &q.Answer, &q.State, timeCol{&q.Asked}, timeCol{&q.Answered})
	if err != nil {
		return Question{}, err
	}
	if err := json.Unmarshal([]byte(options), &q.Options); err != nil {
		return Question{}, fmt.Errorf("question %d: options: %w", q.ID, err)
	}
	if len(q.Options) == 0 {
		q.Options = nil
	}
	return q, nil
}

// newestRevision is the highest question revision of the task, 0 when it has
// no questions.
func newestRevision(ctx context.Context, q querier, taskID int64) (int, error) {
	var rev int
	err := q.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision), 0) FROM questions WHERE task_id = ?", taskID).Scan(&rev)
	if err != nil {
		return 0, fmt.Errorf("newest question revision of task %d: %w", taskID, err)
	}
	return rev, nil
}

// ReplaceQuestions stores qs as revision rev of the task's questions and
// voids the open questions of every earlier revision (answered ones stay).
// Each question's Index is its position in qs, whatever it carries; its State
// is open. rev must be above the task's newest revision (ErrStaleRevision),
// so a caller asks for newest+1. An empty qs voids the open questions and
// asks nothing. A question needs Text, and Recommended must be -1 or an index
// into its Options.
func (r *Registry) ReplaceQuestions(ctx context.Context, taskID int64, rev int, qs []Question) error {
	if rev < 1 {
		return fmt.Errorf("replace questions of task %d: %w: revision %d", taskID, ErrStaleRevision, rev)
	}
	optionsJSON := make([]string, len(qs))
	for i, q := range qs {
		if strings.TrimSpace(q.Text) == "" {
			return fmt.Errorf("replace questions of task %d: question %d has no text", taskID, i)
		}
		if q.Recommended < -1 || q.Recommended >= len(q.Options) {
			return fmt.Errorf("replace questions of task %d: question %d recommends option %d of %d (use -1 for no recommendation)", taskID, i, q.Recommended, len(q.Options))
		}
		b, err := json.Marshal(append([]Option{}, q.Options...))
		if err != nil {
			return fmt.Errorf("replace questions of task %d: %w", taskID, err)
		}
		optionsJSON[i] = string(b)
	}
	return r.tx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE id = ?", taskID).Scan(&exists); err != nil {
			return notFound(err, "task", taskID)
		}
		newest, err := newestRevision(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if rev <= newest {
			return fmt.Errorf("replace questions of task %d: %w: revision %d, newest is %d", taskID, ErrStaleRevision, rev, newest)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE questions SET state = ? WHERE task_id = ? AND state = ?", QuestionVoided, taskID, QuestionOpen); err != nil {
			return fmt.Errorf("replace questions of task %d: %w", taskID, err)
		}
		now := store.FormatTime(r.now())
		for i, q := range qs {
			_, err := tx.ExecContext(ctx, `INSERT INTO questions (task_id, revision, idx, text, context, example, options_json, recommended, why, state, asked_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				taskID, rev, i, q.Text, q.Context, q.Example, optionsJSON[i], q.Recommended, q.Why, QuestionOpen, now)
			if err != nil {
				return fmt.Errorf("replace questions of task %d: %w", taskID, mapErr(err))
			}
		}
		return nil
	})
}

// AnswerBatch records answers (question Index -> the answer text) to the open
// questions of revision rev, all or none. It returns ErrStaleRevision when rev
// is not the task's newest revision, so answers to questions that newer
// evidence replaced are never taken, and ErrConflict when a batch names a
// question that is not open in that revision. Questions the batch does not
// name stay open. An empty batch or a blank answer is refused.
func (r *Registry) AnswerBatch(ctx context.Context, taskID int64, rev int, answers map[int]string) error {
	if len(answers) == 0 {
		return fmt.Errorf("answer batch of task %d: no answers", taskID)
	}
	indexes := make([]int, 0, len(answers))
	for idx, a := range answers {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("answer batch of task %d: the answer to question %d is blank", taskID, idx)
		}
		indexes = append(indexes, idx)
	}
	slices.Sort(indexes)
	return r.tx(ctx, func(tx *sql.Tx) error {
		newest, err := newestRevision(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if rev != newest {
			return fmt.Errorf("answer batch of task %d: %w: revision %d, newest is %d", taskID, ErrStaleRevision, rev, newest)
		}
		now := store.FormatTime(r.now())
		for _, idx := range indexes {
			res, err := tx.ExecContext(ctx, "UPDATE questions SET answer = ?, state = ?, answered_at = ? WHERE task_id = ? AND revision = ? AND idx = ? AND state = ?",
				answers[idx], QuestionAnswered, now, taskID, rev, idx, QuestionOpen)
			if err != nil {
				return fmt.Errorf("answer batch of task %d: %w", taskID, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return fmt.Errorf("answer batch of task %d: %w: question %d of revision %d is not open", taskID, ErrConflict, idx, rev)
			}
		}
		return nil
	})
}

// Questions returns every question of a task, voided and answered ones
// included, by revision and position.
func (r *Registry) Questions(ctx context.Context, taskID int64) ([]Question, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+questionColumns+" FROM questions WHERE task_id = ? ORDER BY revision, idx", taskID)
	if err != nil {
		return nil, fmt.Errorf("questions of task %d: %w", taskID, err)
	}
	qs, err := collect(rows, scanQuestion)
	if err != nil {
		return nil, fmt.Errorf("questions of task %d: %w", taskID, err)
	}
	return qs, nil
}

// Decision is what the operator decided on a card. HeadSHA, ReportHash and
// DiffHash are what the card showed, so a later change to any of them voids
// the decision; DiffOpened is when the operator opened the diff first (the
// zero time if they did not).
type Decision struct {
	TaskID                        int64
	Kind, Choice, Note            string
	HeadSHA, ReportHash, DiffHash string
	DiffOpened, At                time.Time
}

// RecordDecision stores d. Kind and Choice are required, At defaults to the
// clock, and the task must exist (ErrNotFound).
func (r *Registry) RecordDecision(ctx context.Context, d Decision) error {
	if d.Kind == "" || d.Choice == "" {
		return errors.New("record decision: kind and choice are required")
	}
	if d.At.IsZero() {
		d.At = r.now()
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO decisions (task_id, kind, choice, note, head_sha, report_hash, diff_hash, diff_opened_at, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.TaskID, d.Kind, d.Choice, d.Note, d.HeadSHA, d.ReportHash, d.DiffHash, timeArg(d.DiffOpened), store.FormatTime(d.At))
	if err != nil {
		return fmt.Errorf("record decision %s for task %d: %w", d.Kind, d.TaskID, mapErr(err))
	}
	return nil
}

// Decisions returns a task's decisions in the order they were made.
func (r *Registry) Decisions(ctx context.Context, taskID int64) ([]Decision, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT task_id, kind, choice, note, head_sha, report_hash, diff_hash, diff_opened_at, at FROM decisions WHERE task_id = ? ORDER BY id", taskID)
	if err != nil {
		return nil, fmt.Errorf("decisions of task %d: %w", taskID, err)
	}
	ds, err := collect(rows, func(sc scanner) (Decision, error) {
		var d Decision
		err := sc.Scan(&d.TaskID, &d.Kind, &d.Choice, &d.Note, &d.HeadSHA, &d.ReportHash, &d.DiffHash, timeCol{&d.DiffOpened}, timeCol{&d.At})
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("decisions of task %d: %w", taskID, err)
	}
	return ds, nil
}
