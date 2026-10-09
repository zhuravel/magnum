package store

// The self-improving loop's proposals (migration 0024): every change the
// skill editor drafted to the judge skill and the prompts, the inputs it was
// given (general misses and general declined findings) with what it did with
// each, and every landed change as a version. The inputs follow the
// proposal's decision (DecideLearnProposal) as the misses of a notes
// curation follow theirs: landed, the noted ones are used; rejected, one in
// MissDismissRejections rejected proposals is dismissed; anything else
// leaves them new. None of these tables is pruned.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Why the editor ran (learn_proposals.trigger_reason).
const (
	LearnTriggerEvidence   = "evidence"   // the daily retro stored new general misses or declines
	LearnTriggerRequest    = "request"    // `magnum learn --propose`
	LearnTriggerRegression = "regression" // a landed version measured worse: a revert
)

// Proposal states (learn_proposals.state).
const (
	LearnEditing    = "editing"    // the editor works on it
	LearnInvalid    = "invalid"    // its result failed magnum's checks after a nudge: Reason says how
	LearnStopped    = "stopped"    // the editor stopped (a pause, a timeout, a shutdown)
	LearnGating     = "gating"     // committed on its branch, waiting for the gate
	LearnPassed     = "passed"     // the gate passed it: it lands
	LearnHeld       = "held"       // the gate could not decide: the operator does
	LearnFailed     = "failed"     // the gate failed it
	LearnLanded     = "landed"     // fast-forwarded onto master
	LearnRejected   = "rejected"   // the operator said no: Reason says why
	LearnExpired    = "expired"    // nobody decided within a week
	LearnSuperseded = "superseded" // a newer proposal replaced it
)

// The kinds of a proposal's input (learn_proposal_inputs.kind).
const (
	LearnInputMiss    = "miss"    // misses.id
	LearnInputDecline = "decline" // declines.id
)

var (
	learnTriggers = []string{LearnTriggerEvidence, LearnTriggerRequest, LearnTriggerRegression}
	learnStates   = []string{LearnEditing, LearnInvalid, LearnStopped, LearnGating, LearnPassed, LearnHeld, LearnFailed,
		LearnLanded, LearnRejected, LearnExpired, LearnSuperseded}
	// learnWaiting are the states a proposal can still leave on its own
	// way (no decided_at); every other state is a decision.
	learnWaiting = []string{LearnEditing, LearnGating, LearnPassed, LearnHeld}
	learnKinds   = []string{LearnInputMiss, LearnInputDecline}
)

// LearnProposal is a change the skill editor drafted (learn_proposals).
type LearnProposal struct {
	ID           int64           `json:"id"`
	Trigger      string          `json:"trigger"`                 // LearnTrigger*
	BaseSHA      string          `json:"base_sha"`                // master when the editor started
	Branch       string          `json:"branch"`                  // learn/<run>
	CandidateSHA string          `json:"candidate_sha,omitempty"` // the commit on Branch
	Files        []string        `json:"files"`                   // the files the commit changes
	State        string          `json:"state"`                   // Learn*
	Gate         json.RawMessage `json:"gate,omitempty"`          // the gate's result
	Reason       string          `json:"reason,omitempty"`        // invalid, stopped: magnum's; rejected: the operator's
	Model        string          `json:"model,omitempty"`         // the editor's
	PromptSHA256 string          `json:"prompt_sha256,omitempty"`
	Scratch      string          `json:"scratch,omitempty"` // the worktree
	RunDir       string          `json:"run_dir,omitempty"` // inputs.json, pins.json, the changes.json copy
	CreatedAt    time.Time       `json:"created_at"`
	DecidedAt    *time.Time      `json:"decided_at,omitempty"`
	LandedSHA    string          `json:"landed_sha,omitempty"`
	LandedAt     *time.Time      `json:"landed_at,omitempty"`
	// Reverts is the version whose files a revert restores as they were
	// before it (0 = not a revert): the gate holds it for the operator.
	Reverts int64 `json:"reverts,omitempty"`
}

// LearnInput is what a proposal did with one input it was given
// (learn_proposal_inputs).
type LearnInput struct {
	Kind    string `json:"kind"`             // LearnInputMiss | LearnInputDecline
	InputID int64  `json:"input_id"`         // misses.id or declines.id
	Outcome string `json:"outcome"`          // MissNoted | MissSkipped
	Where   string `json:"where,omitempty"`  // noted: the file and section that cover it now
	Reason  string `json:"reason,omitempty"` // skipped: why; noted: optional
	// State is the input's state now (LearnProposalInputs fills it; ""
	// when its row is gone).
	State string `json:"state,omitempty"`
}

// LearnProposalInput is a proposal to store (CreateLearnProposal).
type LearnProposalInput struct {
	Trigger      string
	BaseSHA      string
	Branch       string
	CandidateSHA string
	Files        []string
	State        string
	Gate         []byte // nil = none
	Reason       string
	Model        string
	PromptSHA    string
	Scratch      string
	RunDir       string
	At           time.Time // zero = now
	Inputs       []LearnInput
	Reverts      int64 // the version a revert restores the files before (0 = none)
}

const learnProposalColumns = `id, trigger_reason, base_sha, branch, candidate_sha, files_json, state, gate_json, reason, model,
prompt_sha256, scratch, run_dir, created_at, decided_at, landed_sha, landed_at, reverts`

func scanLearnProposal(sc scanner) (LearnProposal, error) {
	var p LearnProposal
	var reverts sql.NullInt64
	err := sc.Scan(&p.ID, &p.Trigger, &p.BaseSHA, &p.Branch, textCol(&p.CandidateSHA), jsonCol(&p.Files), &p.State, rawCol(&p.Gate),
		textCol(&p.Reason), textCol(&p.Model), textCol(&p.PromptSHA256), textCol(&p.Scratch), textCol(&p.RunDir), timeCol(&p.CreatedAt),
		nullTime(&p.DecidedAt), textCol(&p.LandedSHA), nullTime(&p.LandedAt), &reverts)
	p.Reverts = reverts.Int64
	if p.Files == nil {
		p.Files = []string{}
	}
	return p, err
}

func learnProposalByID(ctx context.Context, q querier, id int64) (LearnProposal, error) {
	p, err := scanLearnProposal(q.QueryRowContext(ctx, "SELECT "+learnProposalColumns+" FROM learn_proposals WHERE id = ?", id))
	if err != nil {
		return LearnProposal{}, notFound(err, "learn proposal", id)
	}
	return p, nil
}

// CreateLearnProposal stores a proposal and what it did with every input it
// was given, in one transaction. A proposal in a state that is a decision
// (anything but editing, gating, passed or held) is decided at once.
// Trigger, BaseSHA, Branch and State are required; an input is in it once.
func (s *Store) CreateLearnProposal(ctx context.Context, in LearnProposalInput) (LearnProposal, error) {
	if err := errors.Join(oneOf("learn trigger", in.Trigger, learnTriggers), oneOf("learn proposal state", in.State, learnStates)); err != nil {
		return LearnProposal{}, fmt.Errorf("create learn proposal: %w", err)
	}
	if in.BaseSHA == "" || in.Branch == "" {
		return LearnProposal{}, errors.New("create learn proposal: base_sha and branch are required")
	}
	if in.Gate != nil && !json.Valid(in.Gate) {
		return LearnProposal{}, errors.New("create learn proposal: the gate is not JSON")
	}
	files := in.Files
	if files == nil {
		files = []string{}
	}
	filesJSON, err := json.Marshal(files)
	if err != nil {
		return LearnProposal{}, fmt.Errorf("create learn proposal: %w", err)
	}
	at := in.At
	if at.IsZero() {
		at = s.now()
	}
	var out LearnProposal
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var decided, gate any
		if !slices.Contains(learnWaiting, in.State) {
			decided = FormatTime(at)
		}
		if in.Gate != nil {
			gate = string(in.Gate)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO learn_proposals (trigger_reason, base_sha, branch, candidate_sha, files_json, state,
  gate_json, reason, model, prompt_sha256, scratch, run_dir, created_at, decided_at, reverts) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.Trigger, in.BaseSHA, in.Branch, nullString(in.CandidateSHA), string(filesJSON), in.State, gate, nullString(in.Reason),
			nullString(in.Model), nullString(in.PromptSHA), nullString(in.Scratch), nullString(in.RunDir), FormatTime(at), decided,
			nullInt(in.Reverts))
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		for _, l := range in.Inputs {
			if err := errors.Join(oneOf("learn input kind", l.Kind, learnKinds), oneOf("learn input outcome", l.Outcome, missOutcomes)); err != nil {
				return err
			}
			if l.InputID == 0 {
				return errors.New("a learn proposal's input without an id")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO learn_proposal_inputs (proposal_id, kind, input_id, outcome, location, reason)
VALUES (?, ?, ?, ?, ?, ?)`, id, l.Kind, l.InputID, l.Outcome, nullString(l.Where), nullString(l.Reason)); err != nil {
				return mapErr(err)
			}
		}
		out, err = learnProposalByID(ctx, tx, id)
		return err
	})
	if err != nil {
		return LearnProposal{}, fmt.Errorf("create learn proposal: %w", err)
	}
	return out, nil
}

// LearnProposalByID reads proposal id (ErrNotFound when there is none).
func (s *Store) LearnProposalByID(ctx context.Context, id int64) (LearnProposal, error) {
	return learnProposalByID(ctx, s.db, id)
}

// LearnProposals lists the proposals in states (empty = every state),
// newest first.
func (s *Store) LearnProposals(ctx context.Context, states []string) ([]LearnProposal, error) {
	q := "SELECT " + learnProposalColumns + " FROM learn_proposals"
	var args []any
	if len(states) > 0 {
		q += " WHERE state IN (" + placeholders(len(states)) + ")"
		args = anys(states)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY id DESC", args...)
	if err != nil {
		return nil, fmt.Errorf("learn proposals: %w", err)
	}
	ps, err := collect(rows, scanLearnProposal)
	if err != nil {
		return nil, fmt.Errorf("learn proposals: %w", err)
	}
	return ps, nil
}

// CountLearnProposals counts the proposals in state.
func (s *Store) CountLearnProposals(ctx context.Context, state string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM learn_proposals WHERE state = ?", state).Scan(&n); err != nil {
		return 0, fmt.Errorf("count learn proposals: %w", err)
	}
	return n, nil
}

// LearnProposalInputs lists what proposal id did with its inputs, by kind
// and id, each with the input's state now.
func (s *Store) LearnProposalInputs(ctx context.Context, id int64) ([]LearnInput, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.kind, l.input_id, l.outcome, l.location, l.reason,
  CASE l.kind WHEN 'miss' THEN (SELECT m.state FROM misses m WHERE m.id = l.input_id)
              ELSE (SELECT d.state FROM declines d WHERE d.id = l.input_id) END
FROM learn_proposal_inputs l WHERE l.proposal_id = ? ORDER BY l.kind, l.input_id`, id)
	if err != nil {
		return nil, fmt.Errorf("learn proposal inputs: %w", err)
	}
	out, err := collect(rows, func(sc scanner) (LearnInput, error) {
		var l LearnInput
		err := sc.Scan(&l.Kind, &l.InputID, &l.Outcome, textCol(&l.Where), textCol(&l.Reason), textCol(&l.State))
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("learn proposal inputs: %w", err)
	}
	return out, nil
}

// LearnInputRejections returns the operator's reasons of the rejected
// proposals the input (kind, id) was in, oldest first; an empty reason is
// left out.
func (s *Store) LearnInputRejections(ctx context.Context, kind string, id int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.reason FROM learn_proposal_inputs l JOIN learn_proposals p ON p.id = l.proposal_id
WHERE l.kind = ? AND l.input_id = ? AND p.state = ? AND p.reason IS NOT NULL AND p.reason <> '' ORDER BY p.id`, kind, id, LearnRejected)
	if err != nil {
		return nil, fmt.Errorf("learn input rejections: %w", err)
	}
	out, err := collect(rows, func(sc scanner) (string, error) {
		var r string
		err := sc.Scan(&r)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("learn input rejections: %w", err)
	}
	return out, nil
}

// DecideLearnProposal moves proposal id from one of from to state to,
// compare-and-set (ErrConflict when it is in another state): reason
// replaces the stored one when not empty, gate the gate's result when not
// nil, and a state that is a decision stamps decided_at with at (zero =
// now). The inputs follow it in the same transaction (decideLearnInputs).
// It never lands one: LandLearnProposal records a landing with its commit
// and its version.
func (s *Store) DecideLearnProposal(ctx context.Context, id int64, from []string, to, reason string, at time.Time, gate []byte) (LearnProposal, error) {
	if err := oneOf("learn proposal state", to, learnStates); err != nil {
		return LearnProposal{}, fmt.Errorf("decide learn proposal %d: %w", id, err)
	}
	if to == LearnLanded {
		return LearnProposal{}, fmt.Errorf("decide learn proposal %d: a landing is LandLearnProposal's, with its commit and its version", id)
	}
	if gate != nil && !json.Valid(gate) {
		return LearnProposal{}, fmt.Errorf("decide learn proposal %d: the gate is not JSON", id)
	}
	if at.IsZero() {
		at = s.now()
	}
	var out LearnProposal
	err := s.tx(ctx, func(tx *sql.Tx) error {
		p, err := learnProposalByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if !slices.Contains(from, p.State) {
			return fmt.Errorf("%w: learn proposal %d is %s", ErrConflict, id, p.State)
		}
		var decided, gateArg any
		if !slices.Contains(learnWaiting, to) {
			decided = FormatTime(at)
		}
		if gate != nil {
			gateArg = string(gate)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE learn_proposals SET state = ?, reason = COALESCE(?, reason),
  gate_json = COALESCE(?, gate_json), decided_at = COALESCE(?, decided_at) WHERE id = ?`, to, nullString(reason), gateArg, decided, id); err != nil {
			return err
		}
		if err := decideLearnInputs(ctx, tx, id, to, at); err != nil {
			return err
		}
		out, err = learnProposalByID(ctx, tx, id)
		return err
	})
	if err != nil {
		return LearnProposal{}, fmt.Errorf("decide learn proposal %d: %w", id, err)
	}
	return out, nil
}

// LandLearnProposal records that proposal id landed, in one transaction:
// it moves from one of from to landed (compare-and-set, ErrConflict when
// it is in another state), decided_at and landed_at become v.LandedAt
// (zero = now) and landed_sha v.LandedSHA, its noted inputs become used
// (decideLearnInputs), and v is stored as the version that landed, its
// ProposalID id (LandedSHA, SkillSHA and PromptsSHA are required, as for
// CreateLearnVersion). Nothing of it is written when any of it fails, so a
// landed proposal always has its commit and its version.
func (s *Store) LandLearnProposal(ctx context.Context, id int64, from []string, v LearnVersion) (LearnProposal, LearnVersion, error) {
	if v.LandedSHA == "" || v.SkillSHA == "" || v.PromptsSHA == "" {
		return LearnProposal{}, LearnVersion{}, fmt.Errorf("land learn proposal %d: landed_sha, skill_sha and prompts_sha are required", id)
	}
	if v.LandedAt.IsZero() {
		v.LandedAt = s.now()
	}
	v.ProposalID = id
	var p LearnProposal
	err := s.tx(ctx, func(tx *sql.Tx) error {
		cur, err := learnProposalByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if !slices.Contains(from, cur.State) {
			return fmt.Errorf("%w: learn proposal %d is %s", ErrConflict, id, cur.State)
		}
		at := FormatTime(v.LandedAt)
		if _, err := tx.ExecContext(ctx, "UPDATE learn_proposals SET state = ?, decided_at = ?, landed_sha = ?, landed_at = ? WHERE id = ?",
			LearnLanded, at, v.LandedSHA, at, id); err != nil {
			return err
		}
		if err := decideLearnInputs(ctx, tx, id, LearnLanded, v.LandedAt); err != nil {
			return err
		}
		if v, err = scanLearnVersion(tx.QueryRowContext(ctx, `INSERT INTO learn_versions (proposal_id, landed_sha, skill_sha, prompts_sha,
  landed_at) VALUES (?, ?, ?, ?, ?) RETURNING `+learnVersionColumns, id, v.LandedSHA, v.SkillSHA, v.PromptsSHA, at)); err != nil {
			return mapErr(err)
		}
		p, err = learnProposalByID(ctx, tx, id)
		return err
	})
	if err != nil {
		return LearnProposal{}, LearnVersion{}, fmt.Errorf("land learn proposal %d: %w", id, err)
	}
	return p, v, nil
}

// decideLearnInputs moves the inputs of proposal id as its new state to
// says: landed, the noted ones that are new become used (a skipped one stays
// new for a later edit); rejected, a new one that is now in
// MissDismissRejections rejected proposals is dismissed; any other state
// moves nothing.
func decideLearnInputs(ctx context.Context, tx *sql.Tx, id int64, to string, at time.Time) error {
	var ids map[string][]int64
	var target string
	switch to {
	case LearnLanded:
		target = MissUsed
		rows, err := tx.QueryContext(ctx, "SELECT kind, input_id FROM learn_proposal_inputs WHERE proposal_id = ? AND outcome = ?", id, MissNoted)
		if err != nil {
			return err
		}
		if ids, err = inputIDs(rows); err != nil {
			return err
		}
	case LearnRejected:
		target = MissDismissed
		rows, err := tx.QueryContext(ctx, `SELECT l.kind, l.input_id FROM learn_proposal_inputs l WHERE l.proposal_id = ? AND
  (SELECT COUNT(*) FROM learn_proposal_inputs o JOIN learn_proposals p ON p.id = o.proposal_id
   WHERE o.kind = l.kind AND o.input_id = l.input_id AND p.state = ?) >= ?`, id, LearnRejected, MissDismissRejections)
		if err != nil {
			return err
		}
		if ids, err = inputIDs(rows); err != nil {
			return err
		}
	default:
		return nil
	}
	if ms := ids[LearnInputMiss]; len(ms) > 0 {
		args := []any{target, FormatTime(at)}
		for _, m := range ms {
			args = append(args, m)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE misses SET state = ?, updated_at = ? WHERE state = '"+MissNew+"' AND id IN ("+
			placeholders(len(ms))+")", args...); err != nil {
			return err
		}
	}
	if ds := ids[LearnInputDecline]; len(ds) > 0 {
		if _, err := setDeclineState(ctx, tx, ds, []string{MissNew}, target, at); err != nil {
			return err
		}
	}
	return nil
}

// inputIDs collects (kind, input_id) rows by kind.
func inputIDs(rows *sql.Rows) (map[string][]int64, error) {
	out := map[string][]int64{}
	_, err := collect(rows, func(sc scanner) (struct{}, error) {
		var kind string
		var id int64
		if err := sc.Scan(&kind, &id); err != nil {
			return struct{}{}, err
		}
		out[kind] = append(out[kind], id)
		return struct{}{}, nil
	})
	return out, err
}

// LearnVersion is a landed change of the skill and the prompts
// (learn_versions): its measurement window starts at RestartedAt and ends
// at the next version's.
type LearnVersion struct {
	ID          int64      `json:"id"`
	ProposalID  int64      `json:"proposal_id,omitempty"` // 0 = none
	LandedSHA   string     `json:"landed_sha"`
	SkillSHA    string     `json:"skill_sha"`
	PromptsSHA  string     `json:"prompts_sha"`
	LandedAt    time.Time  `json:"landed_at"`
	RestartedAt *time.Time `json:"restarted_at,omitempty"`
}

const learnVersionColumns = "id, proposal_id, landed_sha, skill_sha, prompts_sha, landed_at, restarted_at"

func scanLearnVersion(sc scanner) (LearnVersion, error) {
	var v LearnVersion
	var proposal sql.NullInt64
	err := sc.Scan(&v.ID, &proposal, &v.LandedSHA, &v.SkillSHA, &v.PromptsSHA, timeCol(&v.LandedAt), nullTime(&v.RestartedAt))
	v.ProposalID = proposal.Int64
	return v, err
}

// CreateLearnVersion records a landed change (a zero LandedAt is now;
// RestartedAt is MarkLearnRestarted's). LandedSHA, SkillSHA and PromptsSHA
// are required.
func (s *Store) CreateLearnVersion(ctx context.Context, v LearnVersion) (LearnVersion, error) {
	if v.LandedSHA == "" || v.SkillSHA == "" || v.PromptsSHA == "" {
		return LearnVersion{}, errors.New("create learn version: landed_sha, skill_sha and prompts_sha are required")
	}
	if v.LandedAt.IsZero() {
		v.LandedAt = s.now()
	}
	out, err := scanLearnVersion(s.db.QueryRowContext(ctx, `INSERT INTO learn_versions (proposal_id, landed_sha, skill_sha, prompts_sha, landed_at)
VALUES (?, ?, ?, ?, ?) RETURNING `+learnVersionColumns, nullInt(v.ProposalID), v.LandedSHA, v.SkillSHA, v.PromptsSHA, FormatTime(v.LandedAt)))
	if err != nil {
		return LearnVersion{}, fmt.Errorf("create learn version: %w", mapErr(err))
	}
	return out, nil
}

// LearnVersions lists the landed versions, newest first.
func (s *Store) LearnVersions(ctx context.Context) ([]LearnVersion, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+learnVersionColumns+" FROM learn_versions ORDER BY id DESC")
	if err != nil {
		return nil, fmt.Errorf("learn versions: %w", err)
	}
	vs, err := collect(rows, scanLearnVersion)
	if err != nil {
		return nil, fmt.Errorf("learn versions: %w", err)
	}
	return vs, nil
}

// MarkLearnRestarted records the first restart of the daemon on version id
// at at, which starts its measurement window; a later restart changes
// nothing (ErrNotFound when there is no such version).
func (s *Store) MarkLearnRestarted(ctx context.Context, id int64, at time.Time) error {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM learn_versions WHERE id = ?", id).Scan(&n); err != nil {
		return fmt.Errorf("mark learn version %d restarted: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("mark learn version %d restarted: %w", id, ErrNotFound)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE learn_versions SET restarted_at = ? WHERE id = ? AND restarted_at IS NULL", FormatTime(at), id); err != nil {
		return fmt.Errorf("mark learn version %d restarted: %w", id, err)
	}
	return nil
}
