package factoryread

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrNoTask is LocalReview's error when the factory's registry holds no
// task of that id.
var ErrNoTask = errors.New("the factory's registry has no such task")

// LocalTask is a task of the factory as the replay of a round of its local
// review needs it (`magnum eval add --local-task`): its repository
// (owner/name), branch, title and Jira key, each cycle of its local review
// the registry records (Rounds, by cycle), and when each of its answered
// questions was answered (Answered, oldest first).
type LocalTask struct {
	ID                           int64
	Repo, Branch, Title, JiraKey string
	Rounds                       []LocalRound
	Answered                     []time.Time
}

// AnsweredAfter counts t's questions answered after at: answers the
// operator gave once a round that began at at was under way, which that
// round could not read.
func (t LocalTask) AnsweredAfter(at time.Time) int {
	n := 0
	for _, a := range t.Answered {
		if a.After(at) {
			n++
		}
	}
	return n
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
// review cycles with their round and roles (LocalRound), and its answer
// times. A registry that does not exist is ErrNoRegistry, a task it does not
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
	err = tx.QueryRowContext(ctx, "SELECT repo, branch, title, jira_key FROM tasks WHERE id = ?", taskID).
		Scan(&out.Repo, &out.Branch, &out.Title, &out.JiraKey)
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
	if out.Answered, err = answerTimes(ctx, tx, taskID); err != nil {
		return out, fmt.Errorf("read the factory's registry: %w", err)
	}
	return out, nil
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

// answerTimes are when task taskID's answered questions were answered,
// oldest first.
func answerTimes(ctx context.Context, tx *sql.Tx, taskID int64) ([]time.Time, error) {
	rows, err := tx.QueryContext(ctx, `SELECT answered_at FROM questions WHERE task_id = ? AND state = 'answered' AND answered_at IS NOT NULL
ORDER BY answered_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, fmt.Errorf("an answer's time %q: %w", at, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
