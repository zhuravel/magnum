package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// waitsOnYou is the WaitsWho value that means the task waits for the operator.
const waitsOnYou = "you"

// Span is one move of a task between states: where it came from and where it
// went, the stage, who it then waited on and for what, whether that is the
// operator, and why. CreateTask writes the first span (From is empty) and
// TransitionTask one for each move.
type Span struct {
	ID, TaskID int64
	From, To   string
	Stage      string
	// WaitsWho and WaitsWhat are the task's after the move; OnYou is true when
	// WaitsWho is "you".
	WaitsWho, WaitsWhat string
	OnYou               bool
	Reason              string
	At                  time.Time
}

// spanShape is what a span records of a task, and what decides whether a
// state-keeping update is a move.
type spanShape struct {
	state, stage, waitsWho, waitsWhat string
}

func insertSpan(ctx context.Context, tx *sql.Tx, taskID int64, from string, to spanShape, reason string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO task_spans (task_id, from_state, to_state, stage, waits_who, waits_what, on_you, reason, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID, from, to.state, to.stage, to.waitsWho, to.waitsWhat, boolInt(to.waitsWho == waitsOnYou), reason, store.FormatTime(at))
	if err != nil {
		return fmt.Errorf("write span: %w", mapErr(err))
	}
	return nil
}

// Spans returns a task's spans oldest first; a task with none, or no task,
// has an empty list.
func (r *Registry) Spans(ctx context.Context, taskID int64) ([]Span, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT id, task_id, from_state, to_state, stage, waits_who, waits_what, on_you, reason, at FROM task_spans WHERE task_id = ? ORDER BY id", taskID)
	if err != nil {
		return nil, fmt.Errorf("spans of task %d: %w", taskID, err)
	}
	spans, err := collect(rows, func(sc scanner) (Span, error) {
		var s Span
		err := sc.Scan(&s.ID, &s.TaskID, &s.From, &s.To, &s.Stage, &s.WaitsWho, &s.WaitsWhat, &s.OnYou, &s.Reason, timeCol{&s.At})
		return s, err
	})
	if err != nil {
		return nil, fmt.Errorf("spans of task %d: %w", taskID, err)
	}
	return spans, nil
}

// RuleRef names a rule that went into a role's prompts, by id and version.
type RuleRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// RoleRun holds the facts of one role of one stage attempt (an attempt runs
// several roles): what ran it (CLI, model, versions, the hashes of the prompt,
// the skill and the config, the rules the prompts carried), where (session and
// transcript), when (launch, first output, end, verification), how it ended
// (the end fact and why, the nudges, the tokens by model, the active and idle
// seconds) and the outcome: verified, failed:<cause>, voided, stopped, lost or
// paused. A field that is not known is its zero value.
type RoleRun struct {
	AttemptID, Role string
	CLI, Model      string
	CLIVersion      string
	MagnumBuild     string
	PromptHash      string
	SkillHash       string
	ConfigHash      string
	Rules           []RuleRef
	SessionID       string
	Transcript      string
	Launched        time.Time
	FirstOutput     time.Time
	Ended           time.Time
	Verified        time.Time
	EndFact, EndWhy string
	Nudges          int
	Outcome         string
	Tokens          map[string]int64
	ActiveSecs      int
	IdleSecs        int
}

func validOutcome(o string) bool {
	switch o {
	case "", "verified", "voided", "stopped", "lost", "paused":
		return true
	}
	cause, ok := strings.CutPrefix(o, "failed:")
	return ok && cause != ""
}

// roleRunColumn is one column of attempt_roles beyond its key, in the order
// of the upsert: kind is 't' (text, ” = unknown), 'i' (integer, 0 = unknown)
// or 'm' (time, NULL = unknown).
type roleRunColumn struct {
	name string
	kind byte
	val  any
}

// RecordRoleRun stores rr, a row for each attempt and role: the first call
// inserts, later calls fill in what they know. A field left at its zero value
// never clears one that is set, and a value replaces an earlier one, so the
// launch and the end of a role can each record their own facts. The attempt
// must exist (ErrNotFound), the role is required, and Outcome must be one of
// the outcomes or failed:<cause>.
func (r *Registry) RecordRoleRun(ctx context.Context, rr RoleRun) error {
	if rr.AttemptID == "" || rr.Role == "" {
		return errors.New("record role run: attempt and role are required")
	}
	if !validOutcome(rr.Outcome) {
		return fmt.Errorf("record role run %s/%s: unknown outcome %q (want verified, failed:<cause>, voided, stopped, lost or paused)", rr.AttemptID, rr.Role, rr.Outcome)
	}
	var rules, tokens string
	if len(rr.Rules) > 0 {
		b, err := json.Marshal(rr.Rules)
		if err != nil {
			return fmt.Errorf("record role run %s/%s: %w", rr.AttemptID, rr.Role, err)
		}
		rules = string(b)
	}
	if len(rr.Tokens) > 0 {
		b, err := json.Marshal(rr.Tokens)
		if err != nil {
			return fmt.Errorf("record role run %s/%s: %w", rr.AttemptID, rr.Role, err)
		}
		tokens = string(b)
	}
	cols := []roleRunColumn{
		{"cli", 't', rr.CLI}, {"model", 't', rr.Model}, {"cli_version", 't', rr.CLIVersion},
		{"magnum_build", 't', rr.MagnumBuild}, {"prompt_hash", 't', rr.PromptHash}, {"skill_hash", 't', rr.SkillHash},
		{"config_hash", 't', rr.ConfigHash}, {"rules_json", 't', rules}, {"session_id", 't', rr.SessionID},
		{"transcript", 't', rr.Transcript},
		{"launched_at", 'm', timeArg(rr.Launched)}, {"first_output_at", 'm', timeArg(rr.FirstOutput)},
		{"ended_at", 'm', timeArg(rr.Ended)}, {"verified_at", 'm', timeArg(rr.Verified)},
		{"end_fact", 't', rr.EndFact}, {"end_why", 't', rr.EndWhy}, {"nudges", 'i', rr.Nudges},
		{"outcome", 't', rr.Outcome}, {"tokens_json", 't', tokens},
		{"active_secs", 'i', rr.ActiveSecs}, {"idle_secs", 'i', rr.IdleSecs},
	}
	names := []string{"attempt_id", "role"}
	args := []any{rr.AttemptID, rr.Role}
	var sets []string
	for _, c := range cols {
		names = append(names, c.name)
		args = append(args, c.val)
		switch c.kind {
		case 't':
			sets = append(sets, c.name+" = CASE WHEN excluded."+c.name+" != '' THEN excluded."+c.name+" ELSE attempt_roles."+c.name+" END")
		case 'i':
			sets = append(sets, c.name+" = CASE WHEN excluded."+c.name+" != 0 THEN excluded."+c.name+" ELSE attempt_roles."+c.name+" END")
		case 'm':
			sets = append(sets, c.name+" = COALESCE(excluded."+c.name+", attempt_roles."+c.name+")")
		}
	}
	q := "INSERT INTO attempt_roles (" + strings.Join(names, ", ") + ") VALUES (" + placeholders(len(names)) + ") ON CONFLICT (attempt_id, role) DO UPDATE SET " + strings.Join(sets, ", ")
	if _, err := r.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("record role run %s/%s: %w", rr.AttemptID, rr.Role, mapErr(err))
	}
	return nil
}

// RoleRuns returns the roles of an attempt in the order they were first
// recorded.
func (r *Registry) RoleRuns(ctx context.Context, attemptID string) ([]RoleRun, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT attempt_id, role, cli, model, cli_version, magnum_build, prompt_hash, skill_hash, config_hash,
		rules_json, session_id, transcript, launched_at, first_output_at, ended_at, verified_at, end_fact, end_why, nudges, outcome,
		tokens_json, active_secs, idle_secs FROM attempt_roles WHERE attempt_id = ? ORDER BY id`, attemptID)
	if err != nil {
		return nil, fmt.Errorf("role runs of attempt %s: %w", attemptID, err)
	}
	runs, err := collect(rows, func(sc scanner) (RoleRun, error) {
		var rr RoleRun
		var rules, tokens string
		err := sc.Scan(&rr.AttemptID, &rr.Role, &rr.CLI, &rr.Model, &rr.CLIVersion, &rr.MagnumBuild, &rr.PromptHash, &rr.SkillHash,
			&rr.ConfigHash, &rules, &rr.SessionID, &rr.Transcript, timeCol{&rr.Launched}, timeCol{&rr.FirstOutput}, timeCol{&rr.Ended},
			timeCol{&rr.Verified}, &rr.EndFact, &rr.EndWhy, &rr.Nudges, &rr.Outcome, &tokens, &rr.ActiveSecs, &rr.IdleSecs)
		if err != nil {
			return RoleRun{}, err
		}
		if rules != "" {
			if err := json.Unmarshal([]byte(rules), &rr.Rules); err != nil {
				return RoleRun{}, fmt.Errorf("role %s: rules: %w", rr.Role, err)
			}
		}
		if tokens != "" {
			if err := json.Unmarshal([]byte(tokens), &rr.Tokens); err != nil {
				return RoleRun{}, fmt.Errorf("role %s: tokens: %w", rr.Role, err)
			}
		}
		return rr, nil
	})
	if err != nil {
		return nil, fmt.Errorf("role runs of attempt %s: %w", attemptID, err)
	}
	return runs, nil
}

// Suspension records a task held while it waits for the operator, for the
// stage's attempt and role that were running and the things that were open
// (question ids, say) when it stopped. At most one suspension of a task is
// open (Resumed is the zero time); Resume closes it.
type Suspension struct {
	TaskID    int64
	Stage     string
	AttemptID string
	Role      string
	Open      []string
	Suspended time.Time
	Resumed   time.Time
}

// Suspend opens a suspension of s.TaskID, stamped with the clock. The task must
// exist (ErrNotFound) and have no open suspension (ErrConflict); a stage is
// required, and an empty AttemptID means no attempt.
func (r *Registry) Suspend(ctx context.Context, s Suspension) error {
	if s.Stage == "" {
		return errors.New("suspend: a stage is required")
	}
	open, err := json.Marshal(append([]string{}, s.Open...))
	if err != nil {
		return fmt.Errorf("suspend task %d: %w", s.TaskID, err)
	}
	var attempt any
	if s.AttemptID != "" {
		attempt = s.AttemptID
	}
	_, err = r.db.ExecContext(ctx, "INSERT INTO suspensions (task_id, stage, attempt_id, role, open_json, suspended_at) VALUES (?, ?, ?, ?, ?, ?)",
		s.TaskID, s.Stage, attempt, s.Role, string(open), store.FormatTime(r.now()))
	if err != nil {
		return fmt.Errorf("suspend task %d: %w", s.TaskID, mapErr(err))
	}
	return nil
}

// OpenSuspension returns the task's open suspension; a task that is not
// suspended is ErrNotFound.
func (r *Registry) OpenSuspension(ctx context.Context, taskID int64) (Suspension, error) {
	var s Suspension
	var attempt sql.NullString
	var open string
	err := r.db.QueryRowContext(ctx, "SELECT task_id, stage, attempt_id, role, open_json, suspended_at, resumed_at FROM suspensions WHERE task_id = ? AND resumed_at IS NULL", taskID).
		Scan(&s.TaskID, &s.Stage, &attempt, &s.Role, &open, timeCol{&s.Suspended}, timeCol{&s.Resumed})
	if err != nil {
		return Suspension{}, notFound(err, "open suspension of task", taskID)
	}
	s.AttemptID = attempt.String
	if err := json.Unmarshal([]byte(open), &s.Open); err != nil {
		return Suspension{}, fmt.Errorf("suspension of task %d: %w", taskID, err)
	}
	if len(s.Open) == 0 {
		s.Open = nil
	}
	return s, nil
}

// Resume closes the task's open suspension at the clock's time. A task with
// none open (it never suspended, or already resumed) is ErrNotFound, so a
// suspension resumes once.
func (r *Registry) Resume(ctx context.Context, taskID int64) error {
	res, err := r.db.ExecContext(ctx, "UPDATE suspensions SET resumed_at = ? WHERE task_id = ? AND resumed_at IS NULL", store.FormatTime(r.now()), taskID)
	if err != nil {
		return fmt.Errorf("resume task %d: %w", taskID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("resume task %d: %w: no open suspension", taskID, ErrNotFound)
	}
	return nil
}
