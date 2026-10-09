package store

// The self-improving loop's proposals (migration 0024): every change the
// skill editor drafted to the judge skill and the prompts, the inputs it was
// given (general misses and general declined findings) with what it did with
// each, and every landed change as a version. The inputs follow the
// proposal's decision (DecideLearnProposal, GateLearnProposal) as the misses
// of a notes curation follow theirs: landed, the noted ones are used;
// rejected, one in MissDismissRejections rejected proposals is dismissed; a
// gate's decision dismisses one its baseline already covered, and a failed
// gate one LearnDismissFailures failed gates compared without improving it;
// anything else leaves them new. A dismissal says why on the proposal's link
// row (learn_proposal_inputs.dismissed), which Misses and Declines report.
// None of these tables is pruned.

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

// Proposal states (learn_proposals.state). A proposal is stored when its
// editor run ends (an edit in flight is the KV mark learn.editing, not a
// row), and none replaces another (an edit waits while one waits), so there
// is no editing or superseded state.
const (
	LearnInvalid  = "invalid"  // its result failed magnum's checks after a nudge: Reason says how
	LearnStopped  = "stopped"  // the editor stopped (a pause, a timeout, a shutdown)
	LearnGating   = "gating"   // committed on its branch, waiting for the gate
	LearnPassed   = "passed"   // the gate passed it: it lands
	LearnHeld     = "held"     // the gate could not decide, or left it to the operator: the operator does
	LearnFailed   = "failed"   // the gate failed it
	LearnLanded   = "landed"   // fast-forwarded onto master
	LearnRejected = "rejected" // the operator said no: Reason says why
	LearnExpired  = "expired"  // nobody decided within a week
)

// The kinds of a proposal's input (learn_proposal_inputs.kind).
const (
	LearnInputMiss    = "miss"    // misses.id
	LearnInputDecline = "decline" // declines.id
)

// What a proposal's gate made of the learned case of an input it noted
// (learn_proposal_inputs.gate; "" = no gate compared it).
const (
	LearnTargetImproved    = "improved"     // the candidate found the miss more often, or no longer reported the declined finding
	LearnTargetNotImproved = "not_improved" // compared, and no better than the baseline
	// LearnTargetBaseline: the baseline already found the miss, or no
	// longer reported the declined finding: no target, and the input is
	// dismissed.
	LearnTargetBaseline = "baseline"
)

// Why a proposal's decision dismissed an input
// (learn_proposal_inputs.dismissed, LearnInput.Dismissed, Miss.Dismissed,
// Decline.Dismissed).
const (
	LearnDismissedFound       = "found at the baseline"        // a miss its gate's baseline found
	LearnDismissedNotReported = "not reported at the baseline" // a decline its gate's baseline no longer reported
	LearnDismissedFailures    = "noted in 2 failed gates"      // LearnDismissFailures
	LearnDismissedRejections  = "in 2 rejected proposals"      // MissDismissRejections
)

var (
	learnTriggers = []string{LearnTriggerEvidence, LearnTriggerRequest, LearnTriggerRegression}
	learnTargets  = []string{LearnTargetImproved, LearnTargetNotImproved, LearnTargetBaseline}
	// learnGated are the states a gate decides a proposal to.
	learnGated  = []string{LearnPassed, LearnHeld, LearnFailed}
	learnStates = []string{LearnInvalid, LearnStopped, LearnGating, LearnPassed, LearnHeld, LearnFailed, LearnLanded, LearnRejected,
		LearnExpired}
	// learnWaiting are the states a proposal can still leave on its own
	// way (no decided_at); every other state is a decision.
	learnWaiting = []string{LearnGating, LearnPassed, LearnHeld}
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
	// WaitingSince is when it last became passed or held, to wait for the
	// landing or the operator (nil: it never did); its expiry counts from
	// it.
	WaitingSince *time.Time `json:"waiting_since,omitempty"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	LandedSHA    string     `json:"landed_sha,omitempty"`
	LandedAt     *time.Time `json:"landed_at,omitempty"`
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
	// Gate is what the proposal's gate made of the input's learned case
	// (LearnTarget*; "" = it compared none), Dismissed why the proposal's
	// decision dismissed the input ("" = it did not), and Gated how many
	// gates of any proposal made something of that case
	// (LearnProposalInputs fills them; CreateLearnProposal stores none).
	Gate      string `json:"gate,omitempty"`
	Dismissed string `json:"dismissed,omitempty"`
	Gated     int    `json:"gated,omitempty"`
}

// LearnTarget is what a gate made of the learned case of an input its
// proposal noted (GateLearnProposal).
type LearnTarget struct {
	Kind    string // LearnInputMiss | LearnInputDecline
	InputID int64
	Gate    string // LearnTarget*
}

// LearnInputGate is an earlier proposal that noted an input and failed at
// its gate, with the gate's rule (LearnInputHistory).
type LearnInputGate struct {
	Proposal int64  `json:"proposal"`
	State    string `json:"state"`
	Rule     string `json:"rule"`
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
prompt_sha256, scratch, run_dir, created_at, waiting_since, decided_at, landed_sha, landed_at, reverts`

// learnWaitsFor are the states a proposal waits in for the landing or the
// operator, which stamp waiting_since.
var learnWaitsFor = []string{LearnPassed, LearnHeld}

func scanLearnProposal(sc scanner) (LearnProposal, error) {
	var p LearnProposal
	var reverts sql.NullInt64
	err := sc.Scan(&p.ID, &p.Trigger, &p.BaseSHA, &p.Branch, textCol(&p.CandidateSHA), jsonCol(&p.Files), &p.State, rawCol(&p.Gate),
		textCol(&p.Reason), textCol(&p.Model), textCol(&p.PromptSHA256), textCol(&p.Scratch), textCol(&p.RunDir), timeCol(&p.CreatedAt),
		nullTime(&p.WaitingSince), nullTime(&p.DecidedAt), textCol(&p.LandedSHA), nullTime(&p.LandedAt), &reverts)
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
// (anything but gating, passed or held) is decided at once, and
// one made passed or held waits since it is made (waiting_since).
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
		var decided, waiting, gate any
		if !slices.Contains(learnWaiting, in.State) {
			decided = FormatTime(at)
		}
		if slices.Contains(learnWaitsFor, in.State) {
			waiting = FormatTime(at)
		}
		if in.Gate != nil {
			gate = string(in.Gate)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO learn_proposals (trigger_reason, base_sha, branch, candidate_sha, files_json, state,
  gate_json, reason, model, prompt_sha256, scratch, run_dir, created_at, waiting_since, decided_at, reverts)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.Trigger, in.BaseSHA, in.Branch, nullString(in.CandidateSHA), string(filesJSON), in.State, gate, nullString(in.Reason),
			nullString(in.Model), nullString(in.PromptSHA), nullString(in.Scratch), nullString(in.RunDir), FormatTime(at), waiting, decided,
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
// and id, each with the input's state now, what the proposal's gate made of
// its case, why the proposal dismissed it and how many gates made something
// of its case.
func (s *Store) LearnProposalInputs(ctx context.Context, id int64) ([]LearnInput, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.kind, l.input_id, l.outcome, l.location, l.reason,
  CASE l.kind WHEN 'miss' THEN (SELECT m.state FROM misses m WHERE m.id = l.input_id)
              ELSE (SELECT d.state FROM declines d WHERE d.id = l.input_id) END,
  l.gate, l.dismissed,
  (SELECT COUNT(*) FROM learn_proposal_inputs o WHERE o.kind = l.kind AND o.input_id = l.input_id AND o.gate IS NOT NULL)
FROM learn_proposal_inputs l WHERE l.proposal_id = ? ORDER BY l.kind, l.input_id`, id)
	if err != nil {
		return nil, fmt.Errorf("learn proposal inputs: %w", err)
	}
	out, err := collect(rows, func(sc scanner) (LearnInput, error) {
		var l LearnInput
		err := sc.Scan(&l.Kind, &l.InputID, &l.Outcome, textCol(&l.Where), textCol(&l.Reason), textCol(&l.State), textCol(&l.Gate),
			textCol(&l.Dismissed), &l.Gated)
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("learn proposal inputs: %w", err)
	}
	return out, nil
}

// LearnInputHistory lists the proposals that noted the input (kind, id) and
// failed at their gate, oldest first, each with the gate's rule (its
// reason): what the editor tried for it that did not help.
func (s *Store) LearnInputHistory(ctx context.Context, kind string, id int64) ([]LearnInputGate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.state, p.reason FROM learn_proposal_inputs l JOIN learn_proposals p ON p.id = l.proposal_id
WHERE l.kind = ? AND l.input_id = ? AND l.outcome = ? AND p.state = ? ORDER BY p.id`, kind, id, MissNoted, LearnFailed)
	if err != nil {
		return nil, fmt.Errorf("learn input history: %w", err)
	}
	out, err := collect(rows, func(sc scanner) (LearnInputGate, error) {
		var g LearnInputGate
		err := sc.Scan(&g.Proposal, &g.State, textCol(&g.Rule))
		return g, err
	})
	if err != nil {
		return nil, fmt.Errorf("learn input history: %w", err)
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
// nil, a state that is a decision stamps decided_at with at (zero = now)
// and passed or held stamps waiting_since with it (a landing that holds a
// held proposal again too). The inputs follow it in the same transaction
// (decideLearnInputs).
// It never lands one: LandLearnProposal records a landing with its commit
// and its version.
func (s *Store) DecideLearnProposal(ctx context.Context, id int64, from []string, to, reason string, at time.Time, gate []byte) (LearnProposal, error) {
	return s.decideLearnProposal(ctx, id, from, to, reason, at, gate, nil)
}

// GateLearnProposal is the gate's decision on proposal id: from gating to
// passed, held or failed, as DecideLearnProposal moves it, with what the
// gate made of the learned case of each input it noted (targets, stored on
// the input's link row) in the same transaction: an input whose case the
// baseline already covered is dismissed (LearnDismissedFound,
// LearnDismissedNotReported) whatever the verdict, and a failed decision
// dismisses an input LearnDismissFailures failed gates compared without
// improving it (decideLearnInputs).
func (s *Store) GateLearnProposal(ctx context.Context, id int64, to, reason string, at time.Time, gate []byte, targets []LearnTarget) (LearnProposal, error) {
	if err := oneOf("gate verdict", to, learnGated); err != nil {
		return LearnProposal{}, fmt.Errorf("gate learn proposal %d: %w", id, err)
	}
	for _, t := range targets {
		if err := errors.Join(oneOf("learn input kind", t.Kind, learnKinds), oneOf("gate target", t.Gate, learnTargets)); err != nil {
			return LearnProposal{}, fmt.Errorf("gate learn proposal %d: %w", id, err)
		}
	}
	return s.decideLearnProposal(ctx, id, []string{LearnGating}, to, reason, at, gate, targets)
}

// decideLearnProposal is DecideLearnProposal with the gate's targets
// (GateLearnProposal; nil for any other decision).
func (s *Store) decideLearnProposal(ctx context.Context, id int64, from []string, to, reason string, at time.Time, gate []byte,
	targets []LearnTarget) (LearnProposal, error) {
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
		var decided, waiting, gateArg any
		if !slices.Contains(learnWaiting, to) {
			decided = FormatTime(at)
		}
		if slices.Contains(learnWaitsFor, to) {
			waiting = FormatTime(at)
		}
		if gate != nil {
			gateArg = string(gate)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE learn_proposals SET state = ?, reason = COALESCE(?, reason),
  gate_json = COALESCE(?, gate_json), waiting_since = COALESCE(?, waiting_since), decided_at = COALESCE(?, decided_at) WHERE id = ?`,
			to, nullString(reason), gateArg, waiting, decided, id); err != nil {
			return err
		}
		if err := gateLearnInputs(ctx, tx, id, targets, at); err != nil {
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

// gateLearnInputs stores what proposal id's gate made of its inputs'
// learned cases on their link rows, and dismisses the inputs whose case the
// baseline already covered.
func gateLearnInputs(ctx context.Context, tx *sql.Tx, id int64, targets []LearnTarget, at time.Time) error {
	covered := map[string][]int64{}
	for _, t := range targets {
		if _, err := tx.ExecContext(ctx, "UPDATE learn_proposal_inputs SET gate = ? WHERE proposal_id = ? AND kind = ? AND input_id = ?",
			t.Gate, id, t.Kind, t.InputID); err != nil {
			return err
		}
		if t.Gate == LearnTargetBaseline {
			covered[t.Kind] = append(covered[t.Kind], t.InputID)
		}
	}
	return dismissLearnInputs(ctx, tx, id, covered, func(kind string) string {
		if kind == LearnInputDecline {
			return LearnDismissedNotReported
		}
		return LearnDismissedFound
	}, at)
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
// MissDismissRejections rejected proposals is dismissed; failed, a new one
// whose case this gate compared without improving it and that is now in
// LearnDismissFailures failed proposals so is dismissed; any other state
// moves nothing. A dismissal says why on this proposal's link row.
func decideLearnInputs(ctx context.Context, tx *sql.Tx, id int64, to string, at time.Time) error {
	var q, why string
	var args []any
	switch to {
	case LearnLanded:
		rows, err := tx.QueryContext(ctx, "SELECT kind, input_id FROM learn_proposal_inputs WHERE proposal_id = ? AND outcome = ?", id, MissNoted)
		if err != nil {
			return err
		}
		ids, err := inputIDs(rows)
		if err != nil {
			return err
		}
		_, err = moveLearnInputs(ctx, tx, ids, MissUsed, at)
		return err
	case LearnRejected:
		q, why, args = `SELECT l.kind, l.input_id FROM learn_proposal_inputs l WHERE l.proposal_id = ? AND
  (SELECT COUNT(*) FROM learn_proposal_inputs o JOIN learn_proposals p ON p.id = o.proposal_id
   WHERE o.kind = l.kind AND o.input_id = l.input_id AND p.state = ?) >= ?`, LearnDismissedRejections,
			[]any{id, LearnRejected, MissDismissRejections}
	case LearnFailed:
		q, why, args = `SELECT l.kind, l.input_id FROM learn_proposal_inputs l WHERE l.proposal_id = ? AND l.gate = ? AND
  (SELECT COUNT(*) FROM learn_proposal_inputs o JOIN learn_proposals p ON p.id = o.proposal_id
   WHERE o.kind = l.kind AND o.input_id = l.input_id AND o.gate = ? AND p.state = ?) >= ?`, LearnDismissedFailures,
			[]any{id, LearnTargetNotImproved, LearnTargetNotImproved, LearnFailed, LearnDismissFailures}
	default:
		return nil
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	ids, err := inputIDs(rows)
	if err != nil {
		return err
	}
	return dismissLearnInputs(ctx, tx, id, ids, func(string) string { return why }, at)
}

// moveLearnInputs moves the inputs of ids (by kind) that are new to state
// to and returns those it moved, by kind.
func moveLearnInputs(ctx context.Context, tx *sql.Tx, ids map[string][]int64, to string, at time.Time) (map[string][]int64, error) {
	moved := map[string][]int64{}
	for kind, table := range map[string]string{LearnInputMiss: "misses", LearnInputDecline: "declines"} {
		list := ids[kind]
		if len(list) == 0 {
			continue
		}
		args := []any{MissNew}
		for _, n := range list {
			args = append(args, n)
		}
		rows, err := tx.QueryContext(ctx, "SELECT id FROM "+table+" WHERE state = ? AND id IN ("+placeholders(len(list))+")", args...)
		if err != nil {
			return nil, err
		}
		fresh, err := collect(rows, func(sc scanner) (int64, error) {
			var n int64
			err := sc.Scan(&n)
			return n, err
		})
		if err != nil || len(fresh) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}
		if kind == LearnInputDecline {
			if _, err := setDeclineState(ctx, tx, fresh, []string{MissNew}, to, at); err != nil {
				return nil, err
			}
		} else {
			args := []any{to, FormatTime(at)}
			for _, n := range fresh {
				args = append(args, n)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE misses SET state = ?, updated_at = ? WHERE id IN ("+placeholders(len(fresh))+")",
				args...); err != nil {
				return nil, err
			}
		}
		moved[kind] = fresh
	}
	return moved, nil
}

// dismissLearnInputs dismisses the inputs of ids (by kind) that are new,
// and records why (by kind) on proposal id's link rows of those it moved.
func dismissLearnInputs(ctx context.Context, tx *sql.Tx, id int64, ids map[string][]int64, why func(kind string) string, at time.Time) error {
	moved, err := moveLearnInputs(ctx, tx, ids, MissDismissed, at)
	if err != nil {
		return err
	}
	for kind, list := range moved {
		args := []any{why(kind), id, kind}
		for _, n := range list {
			args = append(args, n)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE learn_proposal_inputs SET dismissed = ? WHERE proposal_id = ? AND kind = ? AND input_id IN ("+
			placeholders(len(list))+")", args...); err != nil {
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
