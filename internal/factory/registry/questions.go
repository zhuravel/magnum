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

// Question flags, the operator's verdict on a question: it was needless, it
// was needed, or it was unclear.
const (
	FlagNeedless = "needless"
	FlagNeeded   = "needed"
	FlagUnclear  = "unclear"
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
	// RaisedBy is the role that asked the question, Evidence what it cited.
	RaisedBy, Evidence string
	// Asked is when the revision was stored, Shown when a card first showed
	// it, Answered when the first answer was (the zero time while the
	// question is open or voided).
	Asked, Shown, Answered time.Time
	// TookRecommendation says the current answer is the recommended option.
	TookRecommendation bool
	// Flag is the operator's verdict (needless, needed or unclear; empty
	// until given) and FlagWhy its reason.
	Flag, FlagWhy string
	// VoidedBy is the revision that voided the question, 0 if none did.
	VoidedBy int
}

const questionColumns = "id, task_id, revision, idx, text, context, example, options_json, recommended, why, answer, state, raised_by, evidence, asked_at, shown_at, answered_at, took_recommendation, flag, flag_why, voided_by"

func scanQuestion(sc scanner) (Question, error) {
	var q Question
	var options string
	err := sc.Scan(&q.ID, &q.TaskID, &q.Revision, &q.Index, &q.Text, &q.Context, &q.Example, &options,
		&q.Recommended, &q.Why, &q.Answer, &q.State, &q.RaisedBy, &q.Evidence, timeCol{&q.Asked}, timeCol{&q.Shown},
		timeCol{&q.Answered}, &q.TookRecommendation, &q.Flag, &q.FlagWhy, &q.VoidedBy)
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

// newestRevision is the task's question revision (tasks.question_rev), 0 when
// it has asked nothing. A missing task is ErrNotFound.
func newestRevision(ctx context.Context, q querier, taskID int64) (int, error) {
	var rev int
	if err := q.QueryRowContext(ctx, "SELECT question_rev FROM tasks WHERE id = ?", taskID).Scan(&rev); err != nil {
		return 0, notFound(err, "task", taskID)
	}
	return rev, nil
}

// tookRecommendation is the SQL expression, with one placeholder for the
// answer, that says whether the answer is the label of the recommended option.
const tookRecommendation = "COALESCE(CASE WHEN recommended >= 0 THEN trim(?) = trim(json_extract(options_json, '$[' || recommended || '].label')) END, 0)"

// ReplaceQuestions stores qs as revision rev of the task's questions and
// voids the open questions of every earlier revision (answered ones stay),
// recording rev as the voiding revision. Each question's Index is its position
// in qs, whatever it carries; its State is open; RaisedBy and Evidence are
// kept. rev must be above the task's newest revision (ErrStaleRevision), so a
// caller asks for newest+1, and it becomes the task's newest even when qs is
// empty: an empty qs voids the open questions and asks nothing. A question
// needs Text, and Recommended must be -1 or an index into its Options.
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
		newest, err := newestRevision(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if rev <= newest {
			return fmt.Errorf("replace questions of task %d: %w: revision %d, newest is %d", taskID, ErrStaleRevision, rev, newest)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE questions SET state = ?, voided_by = ? WHERE task_id = ? AND state = ?", QuestionVoided, rev, taskID, QuestionOpen); err != nil {
			return fmt.Errorf("replace questions of task %d: %w", taskID, err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET question_rev = ? WHERE id = ?", rev, taskID); err != nil {
			return fmt.Errorf("replace questions of task %d: %w", taskID, err)
		}
		now := store.FormatTime(r.now())
		for i, q := range qs {
			_, err := tx.ExecContext(ctx, `INSERT INTO questions (task_id, revision, idx, text, context, example, options_json, recommended, why, raised_by, evidence, state, asked_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				taskID, rev, i, q.Text, q.Context, q.Example, optionsJSON[i], q.Recommended, q.Why, q.RaisedBy, q.Evidence, QuestionOpen, now)
			if err != nil {
				return fmt.Errorf("replace questions of task %d: %w", taskID, mapErr(err))
			}
		}
		return nil
	})
}

// AnswerBatch records answers (question Index -> the answer text) to the open
// questions of revision rev, all or none, and keeps each as the first entry of
// the question's answer history. It returns ErrNotFound when the task does not
// exist, ErrStaleRevision when rev is not the task's newest revision, so
// answers to questions that newer evidence replaced are never taken, and
// ErrConflict when a batch names a question that is not open in that revision
// (ReviseAnswer edits an answered one). Questions the batch does not name stay
// open. An empty batch or a blank answer is refused.
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
			var id int64
			err := tx.QueryRowContext(ctx, "UPDATE questions SET answer = ?, state = ?, answered_at = ?, took_recommendation = "+tookRecommendation+
				" WHERE task_id = ? AND revision = ? AND idx = ? AND state = ? RETURNING id",
				answers[idx], QuestionAnswered, now, answers[idx], taskID, rev, idx, QuestionOpen).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("answer batch of task %d: %w: question %d of revision %d is not open", taskID, ErrConflict, idx, rev)
			}
			if err != nil {
				return fmt.Errorf("answer batch of task %d: %w", taskID, err)
			}
			if err := addAnswer(ctx, tx, id, answers[idx], now); err != nil {
				return fmt.Errorf("answer batch of task %d: %w", taskID, err)
			}
		}
		return nil
	})
}

func addAnswer(ctx context.Context, tx *sql.Tx, questionID int64, answer, at string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO question_answers (question_id, answer, at) VALUES (?, ?, ?)", questionID, answer, at)
	return err
}

// ReviseAnswer replaces the answer of an answered question of the task's
// newest revision and keeps the old one in the answer history. The same words
// again are not an edit. It returns ErrNotFound when the task does not exist,
// ErrStaleRevision when rev is not its newest revision and ErrConflict when
// the question is not answered (AnswerBatch answers an open one). The question
// keeps the time of its first answer.
func (r *Registry) ReviseAnswer(ctx context.Context, taskID int64, rev, idx int, answer string) error {
	if strings.TrimSpace(answer) == "" {
		return fmt.Errorf("revise answer of task %d: the answer to question %d is blank", taskID, idx)
	}
	return r.tx(ctx, func(tx *sql.Tx) error {
		newest, err := newestRevision(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if rev != newest {
			return fmt.Errorf("revise answer of task %d: %w: revision %d, newest is %d", taskID, ErrStaleRevision, rev, newest)
		}
		var id int64
		var state, current string
		err = tx.QueryRowContext(ctx, "SELECT id, state, answer FROM questions WHERE task_id = ? AND revision = ? AND idx = ?", taskID, rev, idx).Scan(&id, &state, &current)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("revise answer of task %d: %w: no question %d in revision %d", taskID, ErrConflict, idx, rev)
		}
		if err != nil {
			return fmt.Errorf("revise answer of task %d: %w", taskID, err)
		}
		if state != QuestionAnswered {
			return fmt.Errorf("revise answer of task %d: %w: question %d of revision %d is %s, not answered", taskID, ErrConflict, idx, rev, state)
		}
		if current == answer {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE questions SET answer = ?, took_recommendation = "+tookRecommendation+" WHERE id = ?", answer, answer, id); err != nil {
			return fmt.Errorf("revise answer of task %d: %w", taskID, err)
		}
		if err := addAnswer(ctx, tx, id, answer, store.FormatTime(r.now())); err != nil {
			return fmt.Errorf("revise answer of task %d: %w", taskID, err)
		}
		return nil
	})
}

// AnswerEdit is one answer a question has had and when it was given.
type AnswerEdit struct {
	Answer string
	At     time.Time
}

// AnswerHistory returns every answer a question has had, the first one and
// each edit, oldest first; a question that was never answered, or does not
// exist, has none.
func (r *Registry) AnswerHistory(ctx context.Context, questionID int64) ([]AnswerEdit, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT answer, at FROM question_answers WHERE question_id = ? ORDER BY id", questionID)
	if err != nil {
		return nil, fmt.Errorf("answer history of question %d: %w", questionID, err)
	}
	edits, err := collect(rows, func(sc scanner) (AnswerEdit, error) {
		var e AnswerEdit
		err := sc.Scan(&e.Answer, timeCol{&e.At})
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("answer history of question %d: %w", questionID, err)
	}
	return edits, nil
}

// ShowQuestions records that a card showed the questions of revision rev of the
// task. A question keeps the first time it was shown, whatever later shows
// say. A revision that has no questions (or a task that does not exist) is
// ErrNotFound.
func (r *Registry) ShowQuestions(ctx context.Context, taskID int64, rev int) error {
	res, err := r.db.ExecContext(ctx, "UPDATE questions SET shown_at = COALESCE(shown_at, ?) WHERE task_id = ? AND revision = ?", store.FormatTime(r.now()), taskID, rev)
	if err != nil {
		return fmt.Errorf("show questions of task %d: %w", taskID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("show questions of task %d: %w: no questions in revision %d", taskID, ErrNotFound, rev)
	}
	return nil
}

// FlagQuestion records the operator's verdict on question id: needless, needed
// or unclear, with the reason in words. A later flag replaces an earlier one. A
// missing question is ErrNotFound.
func (r *Registry) FlagQuestion(ctx context.Context, id int64, flag, why string) error {
	switch flag {
	case FlagNeedless, FlagNeeded, FlagUnclear:
	default:
		return fmt.Errorf("flag question %d: unknown flag %q (want %s, %s or %s)", id, flag, FlagNeedless, FlagNeeded, FlagUnclear)
	}
	res, err := r.db.ExecContext(ctx, "UPDATE questions SET flag = ?, flag_why = ? WHERE id = ?", flag, why, id)
	if err != nil {
		return fmt.Errorf("flag question %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("flag question %d: %w", id, ErrNotFound)
	}
	return nil
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
// the decision. Shown is when the card was shown, DiffOpened when the operator
// opened the diff (the zero time if they did not) and DiffDwell how long they
// read it. RecommendationShown and RecommendationFollowed say whether the card
// carried a recommendation and the choice took it. Patch and Proof are the
// sha256 of the ready patch and of the proof snapshot (artifacts).
type Decision struct {
	TaskID                        int64
	Kind, Choice, Note            string
	HeadSHA, ReportHash, DiffHash string
	Shown, DiffOpened, At         time.Time
	DiffDwell                     time.Duration // kept to the millisecond
	RecommendationShown           bool
	RecommendationFollowed        bool
	Patch, Proof                  string
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
	_, err := r.db.ExecContext(ctx, `INSERT INTO decisions (task_id, kind, choice, note, head_sha, report_hash, diff_hash, shown_at, diff_opened_at, diff_dwell_ms,
			recommendation_shown, recommendation_followed, patch, proof, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.TaskID, d.Kind, d.Choice, d.Note, d.HeadSHA, d.ReportHash, d.DiffHash, timeArg(d.Shown), timeArg(d.DiffOpened), d.DiffDwell.Milliseconds(),
		boolInt(d.RecommendationShown), boolInt(d.RecommendationFollowed), d.Patch, d.Proof, store.FormatTime(d.At))
	if err != nil {
		return fmt.Errorf("record decision %s for task %d: %w", d.Kind, d.TaskID, mapErr(err))
	}
	return nil
}

// Decisions returns a task's decisions in the order they were made.
func (r *Registry) Decisions(ctx context.Context, taskID int64) ([]Decision, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT task_id, kind, choice, note, head_sha, report_hash, diff_hash, shown_at, diff_opened_at, diff_dwell_ms,
			recommendation_shown, recommendation_followed, patch, proof, at FROM decisions WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("decisions of task %d: %w", taskID, err)
	}
	ds, err := collect(rows, func(sc scanner) (Decision, error) {
		var d Decision
		var dwellMS int64
		err := sc.Scan(&d.TaskID, &d.Kind, &d.Choice, &d.Note, &d.HeadSHA, &d.ReportHash, &d.DiffHash, timeCol{&d.Shown}, timeCol{&d.DiffOpened},
			&dwellMS, &d.RecommendationShown, &d.RecommendationFollowed, &d.Patch, &d.Proof, timeCol{&d.At})
		d.DiffDwell = time.Duration(dwellMS) * time.Millisecond
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("decisions of task %d: %w", taskID, err)
	}
	return ds, nil
}
