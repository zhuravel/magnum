package factoryread

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// ErrNoTask is LocalReview's error when the factory's registry holds no
// task of that id.
var ErrNoTask = errors.New("the factory's registry has no such task")

// LocalTask is a task of the factory as the replay of a round of its local
// review needs it (`magnum eval add --local-task`): its repository
// (owner/name), branch, title and Jira key, whether Codex never works on it
// again (CodexNever, tasks.codex_never: Codex refused it), each cycle of its
// local review the registry records (Rounds, by cycle), and its answered
// questions (Questions, by revision and position), from which the answers
// a round could read are rebuilt (AnswersAt).
type LocalTask struct {
	ID                           int64
	Repo, Branch, Title, JiraKey string
	CodexNever                   bool
	Rounds                       []LocalRound
	Questions                    []LocalQuestion
}

// LocalQuestion is one of a task's answered questions: its row, with the
// fields answers.md renders (registry.AnswersMarkdown: its revision,
// position, text, options, recommendation, answer, who raised it, when it
// was first answered and whether its answer takes the recommendation), and
// every answer it had (History, question_answers, oldest first). A row from
// before the registry kept them has none: its one answer is the one given
// when it was answered.
type LocalQuestion struct {
	registry.Question
	History []registry.AnswerEdit
}

// AnswersAt are t's questions as they stood at at (a round's start), by
// revision and position, for registry.AnswersMarkdown: each one answered
// before at, with its newest answer before at and whether that answer
// takes the recommendation (as the factory decides it: the recommended
// label, spaces trimmed). left counts the answers given at at or later,
// first answers and revisions alike, which are not there.
func (t LocalTask) AnswersAt(at time.Time) (qs []registry.Question, left int) {
	for _, lq := range t.Questions {
		history := lq.History
		if len(history) == 0 {
			history = []registry.AnswerEdit{{Answer: lq.Answer, At: lq.Answered}}
		}
		q, stood := lq.Question, false
		for _, e := range history {
			if e.At.IsZero() || !e.At.Before(at) {
				left++
				continue
			}
			if e.Answer != lq.Answer {
				q.Answer, q.TookRecommendation = e.Answer, takesRecommendation(q, e.Answer)
			} else {
				q.Answer, q.TookRecommendation = lq.Answer, lq.TookRecommendation
			}
			stood = true
		}
		if stood {
			qs = append(qs, q)
		}
	}
	return qs, left
}

// takesRecommendation reports whether answer is the label of q's
// recommended option, both trimmed of spaces: the registry's
// took_recommendation rule (SQLite's trim).
func takesRecommendation(q registry.Question, answer string) bool {
	return q.Recommended >= 0 && q.Recommended < len(q.Options) && strings.Trim(answer, " ") == strings.Trim(q.Options[q.Recommended].Label, " ")
}

// LocalRound is one cycle of a task's local review (review_cycles): its
// number, the head it reviewed (head_before) and when it began, with the
// loop's round and the roles the cycle's newest begin event names (the
// factory's "round 1 (cycle 1) at <short head>: <role>, <role>"; a round
// that ran again in its cycle names the roles of its last run). Round is 0
// and Roles nil when no begin event of the cycle reads so for its head.
type LocalRound struct {
	Cycle   int
	Round   int
	Head    string
	Started time.Time
	Roles   []string
}

// roundBegins is the text of the factory's event at the start of a local
// round (internal/factory/review.go, launchRound): the loop's round, the
// cycle, the head's first digits and the roles.
var roundBegins = regexp.MustCompile(`^round ([1-9][0-9]*) \(cycle ([1-9][0-9]*)\) at ([0-9a-f]{7,64}): (.+)$`)

// LocalReview reads task taskID of the registry at path, read only and in
// one transaction (one snapshot of what the factory writes): the task, its
// review cycles with their round and roles (LocalRound), and its answered
// questions with their answers (LocalQuestion). A registry that does not exist is ErrNoRegistry, a task it does not
// hold ErrNoTask; one that cannot be read is another error.
func LocalReview(ctx context.Context, path string, taskID int64) (LocalTask, error) {
	out := LocalTask{ID: taskID}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return out, ErrNoRegistry
	} else if err != nil {
		return out, fmt.Errorf("the factory's registry: %w", err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return out, fmt.Errorf("open the factory's registry: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, fmt.Errorf("read the factory's registry: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a read: nothing to keep
	err = tx.QueryRowContext(ctx, "SELECT repo, branch, title, jira_key, codex_never FROM tasks WHERE id = ?", taskID).
		Scan(&out.Repo, &out.Branch, &out.Title, &out.JiraKey, &out.CodexNever)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return out, fmt.Errorf("task %d: %w", taskID, ErrNoTask)
	case err != nil:
		return out, fmt.Errorf("read the factory's registry: %w", err)
	}
	if out.Rounds, err = cycles(ctx, tx, taskID); err != nil {
		return out, fmt.Errorf("read the factory's registry: %w", err)
	}
	if err := roundsBegun(ctx, tx, taskID, out.Rounds); err != nil {
		return out, fmt.Errorf("read the factory's registry: %w", err)
	}
	if out.Questions, err = answeredQuestions(ctx, tx, taskID); err != nil {
		return out, fmt.Errorf("read the factory's registry: %w", err)
	}
	return out, nil
}

// TaskCodex is what the factory's registry at path says of Codex and task
// taskID, read only: whether Codex never works on the task again
// (tasks.codex_never, which the factory sets when Codex refuses it: the
// account is at stake) and the PR the task published (pr_links,
// "owner/repo#N"; "" before its publish). The replay of a local case reads
// it before anything starts (evalrun.Refused). A registry that does not
// exist is ErrNoRegistry, a task it does not hold ErrNoTask; one that cannot
// be read is another error.
func TaskCodex(ctx context.Context, path string, taskID int64) (never bool, pr string, err error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return false, "", ErrNoRegistry
	} else if err != nil {
		return false, "", fmt.Errorf("the factory's registry: %w", err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return false, "", fmt.Errorf("open the factory's registry: %w", err)
	}
	defer db.Close()
	var repo sql.NullString
	var number sql.NullInt64
	err = db.QueryRowContext(ctx, "SELECT t.codex_never, l.repo, l.number FROM tasks t LEFT JOIN pr_links l ON l.task_id = t.id WHERE t.id = ?", taskID).
		Scan(&never, &repo, &number)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, "", fmt.Errorf("task %d: %w", taskID, ErrNoTask)
	case err != nil:
		return false, "", fmt.Errorf("read the factory's registry: %w", err)
	}
	if repo.Valid && repo.String != "" && number.Int64 > 0 {
		pr = fmt.Sprintf("%s#%d", repo.String, number.Int64)
	}
	return never, pr, nil
}

// cycles are task taskID's review cycles, by number.
func cycles(ctx context.Context, tx *sql.Tx, taskID int64) ([]LocalRound, error) {
	rows, err := tx.QueryContext(ctx, "SELECT n, head_before, started_at FROM review_cycles WHERE task_id = ? ORDER BY n", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LocalRound
	for rows.Next() {
		var r LocalRound
		var started string
		if err := rows.Scan(&r.Cycle, &r.Head, &started); err != nil {
			return nil, err
		}
		if r.Started, err = time.Parse(time.RFC3339Nano, started); err != nil {
			return nil, fmt.Errorf("cycle %d's start %q: %w", r.Cycle, started, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// roundsBegun names each of rounds by the newest begin event of its cycle
// whose head is the cycle's (roundBegins).
func roundsBegun(ctx context.Context, tx *sql.Tx, taskID int64, rounds []LocalRound) error {
	rows, err := tx.QueryContext(ctx, "SELECT text FROM events WHERE task_id = ? AND kind = 'review' AND phase = 'begin' ORDER BY id", taskID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return err
		}
		m := roundBegins.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		round, err1 := strconv.Atoi(m[1])
		cycle, err2 := strconv.Atoi(m[2])
		if err1 != nil || err2 != nil {
			continue
		}
		for i := range rounds {
			if rounds[i].Cycle != cycle || !strings.HasPrefix(rounds[i].Head, m[3]) {
				continue
			}
			rounds[i].Round, rounds[i].Roles = round, nil
			for role := range strings.SplitSeq(m[4], ", ") {
				if role = strings.TrimSpace(role); role != "" {
					rounds[i].Roles = append(rounds[i].Roles, role)
				}
			}
		}
	}
	return rows.Err()
}

// answeredQuestions are task taskID's answered questions, by revision and
// position, each with every answer it had, oldest first.
func answeredQuestions(ctx context.Context, tx *sql.Tx, taskID int64) ([]LocalQuestion, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, revision, idx, text, options_json, recommended, answer, raised_by, answered_at, took_recommendation
FROM questions WHERE task_id = ? AND state = 'answered' ORDER BY revision, idx`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LocalQuestion
	for rows.Next() {
		q := LocalQuestion{Question: registry.Question{TaskID: taskID, State: registry.QuestionAnswered}}
		var options string
		var at sql.NullString
		if err := rows.Scan(&q.ID, &q.Revision, &q.Index, &q.Text, &options, &q.Recommended, &q.Answer, &q.RaisedBy, &at,
			&q.TookRecommendation); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(options), &q.Options); err != nil {
			return nil, fmt.Errorf("question %d's options: %w", q.ID, err)
		}
		if len(q.Options) == 0 {
			q.Options = nil
		}
		if at.Valid {
			if q.Answered, err = time.Parse(time.RFC3339Nano, at.String); err != nil {
				return nil, fmt.Errorf("question %d's answer time %q: %w", q.ID, at.String, err)
			}
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, answerHistories(ctx, tx, taskID, out)
}

// answerHistories adds to each of qs every answer it had (question_answers),
// oldest first.
func answerHistories(ctx context.Context, tx *sql.Tx, taskID int64, qs []LocalQuestion) error {
	rows, err := tx.QueryContext(ctx, `SELECT a.question_id, a.answer, a.at FROM question_answers a JOIN questions q ON q.id = a.question_id
WHERE q.task_id = ? AND q.state = 'answered' ORDER BY a.id`, taskID)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[int64]*LocalQuestion, len(qs))
	for i := range qs {
		byID[qs[i].ID] = &qs[i]
	}
	for rows.Next() {
		var id int64
		var e registry.AnswerEdit
		var at string
		if err := rows.Scan(&id, &e.Answer, &at); err != nil {
			return err
		}
		if e.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return fmt.Errorf("question %d's answer time %q: %w", id, at, err)
		}
		if q := byID[id]; q != nil {
			q.History = append(q.History, e)
		}
	}
	return rows.Err()
}
