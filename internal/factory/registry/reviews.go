package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// Exits of a review cycle: the loop ended clean, at its cap of rounds, by the
// operator, on an oscillation, or because the review was unavailable. A cycle
// after which the loop goes on has no exit.
const (
	ExitClean       = "clean"
	ExitCap         = "cap"
	ExitOperator    = "operator"
	ExitOscillation = "oscillation"
	ExitUnavailable = "unavailable"
)

var cycleExits = map[string]bool{
	"": true, ExitClean: true, ExitCap: true, ExitOperator: true, ExitOscillation: true, ExitUnavailable: true,
}

// Cycle is one round of the local magnum review of a task: the heads before
// and after it, the change between them (the sha256 of a patch artifact, say),
// the hash of the review skill, the time and the tokens it took, and the exit.
// N counts the task's rounds from 1.
type Cycle struct {
	ID, TaskID            int64
	N                     int
	HeadBefore, HeadAfter string
	Delta, SkillHash      string
	Started, Ended        time.Time // Ended is the zero time while the round runs
	Tokens                int64
	Exit                  string
}

const cycleColumns = "id, task_id, n, head_before, head_after, delta, skill_hash, started_at, ended_at, tokens, exit"

func scanCycle(sc scanner) (Cycle, error) {
	var c Cycle
	err := sc.Scan(&c.ID, &c.TaskID, &c.N, &c.HeadBefore, &c.HeadAfter, &c.Delta, &c.SkillHash,
		timeCol{&c.Started}, timeCol{&c.Ended}, &c.Tokens, &c.Exit)
	return c, err
}

// BeginCycle starts the next round of the task's local review, numbered after
// its last one, on the head it starts from and with the review skill's hash. A
// missing task is ErrNotFound.
func (r *Registry) BeginCycle(ctx context.Context, taskID int64, headBefore, skillHash string) (Cycle, error) {
	var out Cycle
	err := r.tx(ctx, func(tx *sql.Tx) error {
		if err := taskExists(ctx, tx, taskID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO review_cycles (task_id, n, head_before, skill_hash, started_at)
			VALUES (?, (SELECT COALESCE(MAX(n), 0) + 1 FROM review_cycles WHERE task_id = ?), ?, ?, ?)`,
			taskID, taskID, headBefore, skillHash, store.FormatTime(r.now()))
		if err != nil {
			return mapErr(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		out, err = scanCycle(tx.QueryRowContext(ctx, "SELECT "+cycleColumns+" FROM review_cycles WHERE id = ?", id))
		return err
	})
	if err != nil {
		return Cycle{}, fmt.Errorf("begin review cycle of task %d: %w", taskID, err)
	}
	return out, nil
}

// EndCycle ends a running round with the head it left, the change between the
// heads, its exit (empty when the loop goes on) and the tokens it took. A round
// ends once (ErrConflict) and a missing one is ErrNotFound.
func (r *Registry) EndCycle(ctx context.Context, id int64, headAfter, delta, exit string, tokens int64) error {
	if !cycleExits[exit] {
		return fmt.Errorf("end review cycle %d: unknown exit %q", id, exit)
	}
	return r.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE review_cycles SET head_after = ?, delta = ?, exit = ?, tokens = ?, ended_at = ? WHERE id = ? AND ended_at IS NULL",
			headAfter, delta, exit, tokens, store.FormatTime(r.now()), id)
		if err != nil {
			return fmt.Errorf("end review cycle %d: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		var one int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM review_cycles WHERE id = ?", id).Scan(&one); err != nil {
			return notFound(err, "review cycle", id)
		}
		return fmt.Errorf("end review cycle %d: %w: it has ended", id, ErrConflict)
	})
}

// CloseLoop ends the loop at round id after all: a round that ended with
// the loop going on (exit "") takes exit, and keeps its heads, its end and
// its tokens (the round the local review's second opinion was to follow,
// once that opinion is void). A round with exit already is fine; a running
// round or one that ended with another exit is ErrConflict, a missing one
// ErrNotFound, and exit must be one of the exits.
func (r *Registry) CloseLoop(ctx context.Context, id int64, exit string) error {
	if exit == "" || !cycleExits[exit] {
		return fmt.Errorf("close the loop at review cycle %d: unknown exit %q", id, exit)
	}
	return r.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE review_cycles SET exit = ? WHERE id = ? AND exit = '' AND ended_at IS NOT NULL", exit, id)
		if err != nil {
			return fmt.Errorf("close the loop at review cycle %d: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		var cur string
		var ended sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT exit, ended_at FROM review_cycles WHERE id = ?", id).Scan(&cur, &ended); err != nil {
			return notFound(err, "review cycle", id)
		}
		if cur == exit {
			return nil
		}
		why := "it ended " + cur
		if !ended.Valid {
			why = "it runs"
		}
		return fmt.Errorf("close the loop at review cycle %d: %w: %s", id, ErrConflict, why)
	})
}

// Cycles returns a task's review rounds in order.
func (r *Registry) Cycles(ctx context.Context, taskID int64) ([]Cycle, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+cycleColumns+" FROM review_cycles WHERE task_id = ? ORDER BY n", taskID)
	if err != nil {
		return nil, fmt.Errorf("review cycles of task %d: %w", taskID, err)
	}
	cs, err := collect(rows, scanCycle)
	if err != nil {
		return nil, fmt.Errorf("review cycles of task %d: %w", taskID, err)
	}
	return cs, nil
}

// Finding outcomes.
const (
	FindingOpen     = "open"
	FindingFixed    = "fixed"
	FindingWaived   = "waived"
	FindingRebutted = "rebutted"
)

var findingOutcomes = map[string]bool{FindingOpen: true, FindingFixed: true, FindingWaived: true, FindingRebutted: true}

// Finding is one finding of the local review, followed across the rounds by its
// fingerprint (path, lines and a hash of the title). Sources are the reviewers
// that reported it, Verdict and VerdictWhy the judge's, and Blocking whether it
// blocks the round. FirstCycle and LastCycle are round numbers (Cycle.N),
// Reopened counts how often a fixed finding came back, and FixCommit is the
// last commit that claimed to fix it.
type Finding struct {
	ID, TaskID            int64
	Fingerprint, Path     string
	StartLine, EndLine    int
	TitleHash             string
	Priority, Class       string
	Sources               []string
	Verdict, VerdictWhy   string
	Blocking              bool
	FirstCycle, LastCycle int
	Outcome, FixCommit    string
	Reopened              int
}

const findingColumns = "id, task_id, fingerprint, path, start_line, end_line, title_hash, priority, class, sources_json, verdict, verdict_why, blocking, first_cycle, last_cycle, outcome, fix_commit, reopened"

func scanFinding(sc scanner) (Finding, error) {
	var f Finding
	var sources string
	err := sc.Scan(&f.ID, &f.TaskID, &f.Fingerprint, &f.Path, &f.StartLine, &f.EndLine, &f.TitleHash, &f.Priority, &f.Class,
		&sources, &f.Verdict, &f.VerdictWhy, &f.Blocking, &f.FirstCycle, &f.LastCycle, &f.Outcome, &f.FixCommit, &f.Reopened)
	if err != nil {
		return Finding{}, err
	}
	if err := json.Unmarshal([]byte(sources), &f.Sources); err != nil {
		return Finding{}, fmt.Errorf("finding %d: sources: %w", f.ID, err)
	}
	if len(f.Sources) == 0 {
		f.Sources = nil
	}
	return f, nil
}

// SeeFinding records that round cycleID saw f, and returns the finding as the
// registry now holds it. The finding of a task is told by its fingerprint: a
// new one is stored open with that round as its first and last; one seen
// before moves LastCycle to this round, takes the newest sighting's location,
// priority, class, verdict and blocking flag (a value left empty keeps the old
// one) and the union of the sources, and, if it was fixed and this round is
// after its last, comes back open with Reopened + 1. Waived and rebutted
// findings stay so. f.TaskID may be left 0; f.Outcome, FirstCycle, LastCycle
// and Reopened are the registry's. A missing round is ErrNotFound.
func (r *Registry) SeeFinding(ctx context.Context, cycleID int64, f Finding) (Finding, error) {
	if f.Fingerprint == "" {
		return Finding{}, errors.New("see finding: a fingerprint is required")
	}
	var out Finding
	err := r.tx(ctx, func(tx *sql.Tx) error {
		var taskID int64
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT task_id, n FROM review_cycles WHERE id = ?", cycleID).Scan(&taskID, &n); err != nil {
			return notFound(err, "review cycle", cycleID)
		}
		if f.TaskID != 0 && f.TaskID != taskID {
			return fmt.Errorf("the finding is of task %d, the review cycle of task %d", f.TaskID, taskID)
		}
		cur, err := scanFinding(tx.QueryRowContext(ctx, "SELECT "+findingColumns+" FROM review_findings WHERE task_id = ? AND fingerprint = ?", taskID, f.Fingerprint))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			sources, err := json.Marshal(append([]string{}, dedupe(f.Sources)...))
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO review_findings (task_id, fingerprint, path, start_line, end_line, title_hash, priority, class, sources_json,
					verdict, verdict_why, blocking, first_cycle, last_cycle, outcome)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				taskID, f.Fingerprint, f.Path, f.StartLine, f.EndLine, f.TitleHash, f.Priority, f.Class, string(sources),
				f.Verdict, f.VerdictWhy, boolInt(f.Blocking), n, n, FindingOpen)
			if err != nil {
				return mapErr(err)
			}
		case err != nil:
			return err
		default:
			cur.Path = keepIfEmpty(f.Path, cur.Path)
			if f.StartLine != 0 || f.EndLine != 0 {
				cur.StartLine, cur.EndLine = f.StartLine, f.EndLine
			}
			cur.TitleHash = keepIfEmpty(f.TitleHash, cur.TitleHash)
			cur.Priority = keepIfEmpty(f.Priority, cur.Priority)
			cur.Class = keepIfEmpty(f.Class, cur.Class)
			cur.Verdict = keepIfEmpty(f.Verdict, cur.Verdict)
			cur.VerdictWhy = keepIfEmpty(f.VerdictWhy, cur.VerdictWhy)
			cur.Blocking = f.Blocking
			for _, s := range f.Sources {
				if !slices.Contains(cur.Sources, s) {
					cur.Sources = append(cur.Sources, s)
				}
			}
			if cur.Outcome == FindingFixed && n > cur.LastCycle {
				cur.Outcome = FindingOpen
				cur.Reopened++
			}
			cur.LastCycle = max(cur.LastCycle, n)
			sources, err := json.Marshal(append([]string{}, cur.Sources...))
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE review_findings SET path = ?, start_line = ?, end_line = ?, title_hash = ?, priority = ?, class = ?, sources_json = ?,
					verdict = ?, verdict_why = ?, blocking = ?, last_cycle = ?, outcome = ?, reopened = ? WHERE id = ?`,
				cur.Path, cur.StartLine, cur.EndLine, cur.TitleHash, cur.Priority, cur.Class, string(sources),
				cur.Verdict, cur.VerdictWhy, boolInt(cur.Blocking), cur.LastCycle, cur.Outcome, cur.Reopened, cur.ID)
			if err != nil {
				return err
			}
		}
		out, err = scanFinding(tx.QueryRowContext(ctx, "SELECT "+findingColumns+" FROM review_findings WHERE task_id = ? AND fingerprint = ?", taskID, f.Fingerprint))
		return err
	})
	if err != nil {
		return Finding{}, fmt.Errorf("see finding %s in review cycle %d: %w", f.Fingerprint, cycleID, err)
	}
	return out, nil
}

func keepIfEmpty(next, old string) string {
	if next != "" {
		return next
	}
	return old
}

// dedupe returns list without repeats, in the order of first appearance.
func dedupe(list []string) []string {
	var out []string
	for _, s := range list {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// SettleFinding sets the outcome of finding id: fixed, waived, rebutted or open.
// A non-empty fixCommit is kept as the commit that claimed the fix. A missing
// finding is ErrNotFound.
func (r *Registry) SettleFinding(ctx context.Context, id int64, outcome, fixCommit string) error {
	if !findingOutcomes[outcome] {
		return fmt.Errorf("settle finding %d: unknown outcome %q", id, outcome)
	}
	res, err := r.db.ExecContext(ctx, "UPDATE review_findings SET outcome = ?, fix_commit = CASE WHEN ? != '' THEN ? ELSE fix_commit END WHERE id = ?", outcome, fixCommit, fixCommit, id)
	if err != nil {
		return fmt.Errorf("settle finding %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("settle finding %d: %w", id, ErrNotFound)
	}
	return nil
}

// Findings returns a task's findings in the order they were first seen.
func (r *Registry) Findings(ctx context.Context, taskID int64) ([]Finding, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+findingColumns+" FROM review_findings WHERE task_id = ? ORDER BY id", taskID)
	if err != nil {
		return nil, fmt.Errorf("findings of task %d: %w", taskID, err)
	}
	fs, err := collect(rows, scanFinding)
	if err != nil {
		return nil, fmt.Errorf("findings of task %d: %w", taskID, err)
	}
	return fs, nil
}

// Check kinds, results, causes and sources.
const (
	CheckSpec      = "spec"
	CheckLint      = "lint"
	CheckReview    = "review"
	CheckCI        = "ci"
	CheckQA        = "qa"
	CheckMigration = "migration"

	CheckPass    = "pass"
	CheckFail    = "fail"
	CheckSkipped = "skipped"

	// SourceGitHub is a check GitHub ran (its CI on the task's PR), and
	// CauseNotOurs a failure that is not of the task's change.
	SourceGitHub = "github"
	CauseNotOurs = "not_ours"
)

var (
	checkKinds   = map[string]bool{CheckSpec: true, CheckLint: true, CheckReview: true, CheckCI: true, CheckQA: true, CheckMigration: true}
	checkResults = map[string]bool{CheckPass: true, CheckFail: true, CheckSkipped: true}
	checkCauses  = map[string]bool{"": true, "code": true, "infra": true, "flaky": true, "env": true, "access": true, "not_ours": true, "unknown": true}
	checkSources = map[string]bool{"writer": true, "factory": true, "github": true}
)

// Check is one run of a check on a head: its kind (spec, lint, review, ci, qa
// or migration), result (pass, fail or skipped), the source that ran it (the
// writer's claim, the factory's own run, or GitHub), the command and its exit
// code and duration. Cause is set only on a failure (code, infra, flaky, env,
// access, not_ours or unknown) and may stay empty until someone classifies it.
// AttemptID is empty for a check that no attempt ran, such as GitHub's.
type Check struct {
	ID, TaskID                  int64
	AttemptID, HeadSHA          string
	Kind, Result, Cause, Source string
	Command                     string
	Exit                        int
	Took                        time.Duration // kept to the millisecond
	At                          time.Time
}

// RecordCheck stores c and returns its id. Kind, Result and Source are
// required and must be listed; Cause must be empty unless the result is a
// failure. At defaults to the clock; the task (and the attempt, if given) must
// exist (ErrNotFound).
func (r *Registry) RecordCheck(ctx context.Context, c Check) (int64, error) {
	switch {
	case !checkKinds[c.Kind]:
		return 0, fmt.Errorf("record check: unknown kind %q", c.Kind)
	case !checkResults[c.Result]:
		return 0, fmt.Errorf("record check: unknown result %q", c.Result)
	case !checkSources[c.Source]:
		return 0, fmt.Errorf("record check: unknown source %q", c.Source)
	case !checkCauses[c.Cause]:
		return 0, fmt.Errorf("record check: unknown cause %q", c.Cause)
	case c.Cause != "" && c.Result != CheckFail:
		return 0, fmt.Errorf("record check: a %s has no cause (got %q); only a failure does", c.Result, c.Cause)
	}
	if c.At.IsZero() {
		c.At = r.now()
	}
	var attempt any
	if c.AttemptID != "" {
		attempt = c.AttemptID
	}
	res, err := r.db.ExecContext(ctx, `INSERT INTO checks (task_id, attempt_id, head_sha, kind, result, cause, source, command, exit_code, took_ms, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.TaskID, attempt, c.HeadSHA, c.Kind, c.Result, c.Cause, c.Source, c.Command, c.Exit, c.Took.Milliseconds(), store.FormatTime(c.At))
	if err != nil {
		return 0, fmt.Errorf("record %s check for task %d: %w", c.Kind, c.TaskID, mapErr(err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("record %s check for task %d: %w", c.Kind, c.TaskID, err)
	}
	return id, nil
}

// Checks returns a task's checks in the order they were recorded.
func (r *Registry) Checks(ctx context.Context, taskID int64) ([]Check, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, task_id, COALESCE(attempt_id, ''), head_sha, kind, result, cause, source, command, exit_code, took_ms, at
		FROM checks WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("checks of task %d: %w", taskID, err)
	}
	cs, err := collect(rows, func(sc scanner) (Check, error) {
		var c Check
		var tookMS int64
		err := sc.Scan(&c.ID, &c.TaskID, &c.AttemptID, &c.HeadSHA, &c.Kind, &c.Result, &c.Cause, &c.Source, &c.Command, &c.Exit, &tookMS, timeCol{&c.At})
		c.Took = time.Duration(tookMS) * time.Millisecond
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("checks of task %d: %w", taskID, err)
	}
	return cs, nil
}
