package store

// Repository notes in the registry (migration 0013): every version of a
// repository's notes and harness, every proposal for them, and which harness
// files the rounds use. None of these tables is pruned (Prune touches events
// and requests only): a curation can always be redone from the full history.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// Notes version sources (notes_versions.source).
const (
	NotesFromJudge    = "judge"    // a round's judge changed them
	NotesFromCuration = "curation" // a curator proposed them (applied or not)
	NotesFromHuman    = "human"    // `magnum notes --edit`, or a restore the operator applied
	NotesFromImport   = "import"   // found on disk without a record of who wrote them
)

// Notes proposal kinds and states (notes_proposals.kind, .state).
const (
	ProposalCuration = "curation"
	ProposalRestore  = "restore"

	ProposalPending  = "pending"
	ProposalApplied  = "applied"
	ProposalRejected = "rejected"
	ProposalExpired  = "expired"
	ProposalInvalid  = "invalid"
	// ProposalSuperseded is a stale curation (the notes changed since it
	// was made) that a new curation of the notes now follows up on, reading
	// it as input (migration 0017).
	ProposalSuperseded = "superseded"
)

var (
	notesSources    = []string{NotesFromJudge, NotesFromCuration, NotesFromHuman, NotesFromImport}
	proposalKinds   = []string{ProposalCuration, ProposalRestore}
	proposalStates  = []string{ProposalPending, ProposalApplied, ProposalRejected, ProposalExpired, ProposalInvalid, ProposalSuperseded}
	errNoNotesState = errors.New("store: notes version without content")
)

// NotesUnusedRounds is how many judge rounds a harness file must have
// existed for, with no recorded use, to be a curation candidate.
const NotesUnusedRounds = 20

// NotesBlob is one harness file of a version: its path relative to the
// harness directory, its content's SHA-256 and size, and (when read with
// NotesVersionContent, or recorded) its body.
type NotesBlob struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Body   []byte `json:"-"`
}

// NotesVersion is one recorded state of a repository's notes.
type NotesVersion struct {
	ID         int64       `json:"id"`
	RepoID     int64       `json:"repo_id"`
	At         time.Time   `json:"at"`
	Source     string      `json:"source"`
	PRID       *int64      `json:"pr_id,omitempty"`
	PRNumber   int         `json:"pr_number,omitempty"` // the PR's number (read only)
	RunID      string      `json:"run_id,omitempty"`
	ProposalID *int64      `json:"proposal_id,omitempty"`
	Bytes      int64       `json:"bytes"`
	SHA256     string      `json:"sha256"`
	Files      []NotesBlob `json:"files"`
}

// HarnessBytes is the total size of v's harness files.
func (v NotesVersion) HarnessBytes() int64 {
	var n int64
	for _, f := range v.Files {
		n += f.Bytes
	}
	return n
}

// NotesContent is what a version holds: the notes text and every harness
// file with its body, sorted by path.
type NotesContent struct {
	Notes []byte
	Files []NotesBlob
}

// NotesVersionInput is a state to record (RecordNotesVersion).
type NotesVersionInput struct {
	RepoID     int64
	At         time.Time // zero = now
	Source     string    // NotesFrom*
	PRID       int64     // 0 = none
	RunID      string
	ProposalID int64 // 0 = none
	Content    NotesContent
	// Dedupe: when the latest version of the repository's history already
	// holds this state, nothing is recorded and that version is returned.
	Dedupe bool
}

// historyClause selects a repository's history: every version but the
// proposed states of curations (a curation proposal's version_id, of
// source curation), which join it only through the version recorded when
// one is applied. A restore's version_id is a version of the history (an
// applied curation's, say) and stays in it, whatever becomes of the restore.
const historyClause = `v.repo_id = ? AND NOT (v.source = 'curation' AND v.id IN
  (SELECT p.version_id FROM notes_proposals p WHERE p.kind = 'curation' AND p.version_id IS NOT NULL))`

// RecordNotesVersion records in's state as a version of its repository: the
// notes text gzip-compressed (or NULL when an earlier version holds the same
// text), the harness files by path and each body once per content
// (harness_blobs). It reports whether a version was added (false: Dedupe
// found the latest version identical).
func (s *Store) RecordNotesVersion(ctx context.Context, in NotesVersionInput) (NotesVersion, bool, error) {
	var out NotesVersion
	added := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		out, added, err = recordNotesVersion(ctx, tx, in, s.now())
		return err
	})
	if err != nil {
		return NotesVersion{}, false, fmt.Errorf("record notes version: %w", err)
	}
	return out, added, nil
}

func recordNotesVersion(ctx context.Context, tx *sql.Tx, in NotesVersionInput, now time.Time) (NotesVersion, bool, error) {
	if err := oneOf("notes source", in.Source, notesSources); err != nil {
		return NotesVersion{}, false, err
	}
	if in.RepoID == 0 {
		return NotesVersion{}, false, errors.New("a repository is required")
	}
	files := slices.Clone(in.Content.Files)
	for i := range files {
		files[i].SHA256 = sha256Hex(files[i].Body)
		files[i].Bytes = int64(len(files[i].Body))
	}
	slices.SortFunc(files, func(a, b NotesBlob) int { return strings.Compare(a.Path, b.Path) })
	sum := sha256Hex(in.Content.Notes)
	if in.Dedupe {
		latest, err := latestNotesVersion(ctx, tx, in.RepoID)
		switch {
		case err == nil && latest.SHA256 == sum && sameBlobs(latest.Files, files):
			return latest, false, nil
		case err != nil && !errors.Is(err, ErrNotFound):
			return NotesVersion{}, false, err
		}
	}
	var body any
	var stored int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM notes_versions WHERE repo_id = ? AND sha256 = ? AND body IS NOT NULL",
		in.RepoID, sum).Scan(&stored); err != nil {
		return NotesVersion{}, false, err
	}
	if stored == 0 {
		z, err := gzipBytes(in.Content.Notes)
		if err != nil {
			return NotesVersion{}, false, err
		}
		body = z
	}
	at := in.At
	if at.IsZero() {
		at = now
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO notes_versions (repo_id, at, source, pr_id, run_id, proposal_id, bytes, sha256, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, in.RepoID, FormatTime(at), in.Source, nullInt(in.PRID), nullString(in.RunID),
		nullInt(in.ProposalID), len(in.Content.Notes), sum, body)
	if err != nil {
		return NotesVersion{}, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return NotesVersion{}, false, err
	}
	for _, f := range files {
		if f.Path == "" {
			return NotesVersion{}, false, errors.New("a harness file without a path")
		}
		var have int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_blobs WHERE sha256 = ?", f.SHA256).Scan(&have); err != nil {
			return NotesVersion{}, false, err
		}
		if have == 0 {
			z, err := gzipBytes(f.Body)
			if err != nil {
				return NotesVersion{}, false, err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO harness_blobs (sha256, bytes, body) VALUES (?, ?, ?)", f.SHA256, f.Bytes, z); err != nil {
				return NotesVersion{}, false, err
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO notes_version_files (version_id, path, sha256) VALUES (?, ?, ?)",
			id, f.Path, f.SHA256); err != nil {
			return NotesVersion{}, false, mapErr(err)
		}
	}
	v, err := notesVersionByID(ctx, tx, id)
	return v, true, err
}

// sameBlobs reports whether two sorted file lists name the same paths with
// the same content.
func sameBlobs(a, b []NotesBlob) bool {
	return slices.EqualFunc(a, b, func(x, y NotesBlob) bool { return x.Path == y.Path && x.SHA256 == y.SHA256 })
}

const notesVersionSelect = `SELECT v.id, v.repo_id, v.at, v.source, v.pr_id, COALESCE(p.number, 0), v.run_id, v.proposal_id, v.bytes, v.sha256
FROM notes_versions v LEFT JOIN prs p ON p.id = v.pr_id`

func scanNotesVersion(sc scanner) (NotesVersion, error) {
	var v NotesVersion
	var run sql.NullString
	err := sc.Scan(&v.ID, &v.RepoID, timeCol(&v.At), &v.Source, &v.PRID, &v.PRNumber, &run, &v.ProposalID, &v.Bytes, &v.SHA256)
	v.RunID = run.String
	return v, err
}

// withFiles fills the Files (without bodies) of vs.
func withFiles(ctx context.Context, q querier, vs []NotesVersion) error {
	for i := range vs {
		rows, err := q.QueryContext(ctx, `SELECT f.path, f.sha256, b.bytes FROM notes_version_files f
JOIN harness_blobs b ON b.sha256 = f.sha256 WHERE f.version_id = ? ORDER BY f.path`, vs[i].ID)
		if err != nil {
			return err
		}
		vs[i].Files, err = collect(rows, func(sc scanner) (NotesBlob, error) {
			var f NotesBlob
			err := sc.Scan(&f.Path, &f.SHA256, &f.Bytes)
			return f, err
		})
		if err != nil {
			return err
		}
		if vs[i].Files == nil {
			vs[i].Files = []NotesBlob{}
		}
	}
	return nil
}

func notesVersionByID(ctx context.Context, q querier, id int64) (NotesVersion, error) {
	v, err := scanNotesVersion(q.QueryRowContext(ctx, notesVersionSelect+" WHERE v.id = ?", id))
	if err != nil {
		return NotesVersion{}, notFound(err, "notes version", id)
	}
	vs := []NotesVersion{v}
	if err := withFiles(ctx, q, vs); err != nil {
		return NotesVersion{}, err
	}
	return vs[0], nil
}

func latestNotesVersion(ctx context.Context, q querier, repoID int64) (NotesVersion, error) {
	v, err := scanNotesVersion(q.QueryRowContext(ctx, notesVersionSelect+" WHERE "+historyClause+" ORDER BY v.id DESC LIMIT 1", repoID))
	if err != nil {
		return NotesVersion{}, notFound(err, "notes history of repo", repoID)
	}
	vs := []NotesVersion{v}
	if err := withFiles(ctx, q, vs); err != nil {
		return NotesVersion{}, err
	}
	return vs[0], nil
}

// NotesVersionByID reads a version (its files without bodies).
func (s *Store) NotesVersionByID(ctx context.Context, id int64) (NotesVersion, error) {
	return notesVersionByID(ctx, s.db, id)
}

// LatestNotesVersion is the newest version of repoID's history (ErrNotFound
// when none was recorded).
func (s *Store) LatestNotesVersion(ctx context.Context, repoID int64) (NotesVersion, error) {
	return latestNotesVersion(ctx, s.db, repoID)
}

// NotesHistory lists repoID's history, newest first, at most limit versions
// (limit <= 0 = all): every recorded version except the proposed states of
// curations, which appear as the version recorded when one was applied.
func (s *Store) NotesHistory(ctx context.Context, repoID int64, limit int) ([]NotesVersion, error) {
	q := notesVersionSelect + " WHERE " + historyClause + " ORDER BY v.id DESC"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q, repoID)
	if err != nil {
		return nil, fmt.Errorf("notes history: %w", err)
	}
	vs, err := collect(rows, scanNotesVersion)
	if err != nil {
		return nil, fmt.Errorf("notes history: %w", err)
	}
	if err := withFiles(ctx, s.db, vs); err != nil {
		return nil, fmt.Errorf("notes history: %w", err)
	}
	return vs, nil
}

// NotesVersionContent reads what version id holds: its notes text and its
// harness files with their bodies.
func (s *Store) NotesVersionContent(ctx context.Context, id int64) (NotesContent, error) {
	v, err := s.NotesVersionByID(ctx, id)
	if err != nil {
		return NotesContent{}, err
	}
	var z []byte
	err = s.db.QueryRowContext(ctx, "SELECT body FROM notes_versions WHERE repo_id = ? AND sha256 = ? AND body IS NOT NULL ORDER BY id LIMIT 1",
		v.RepoID, v.SHA256).Scan(&z)
	if err != nil {
		return NotesContent{}, fmt.Errorf("notes version %d: %w", id, errors.Join(errNoNotesState, err))
	}
	var out NotesContent
	if out.Notes, err = gunzipBytes(z); err != nil {
		return NotesContent{}, fmt.Errorf("notes version %d: %w", id, err)
	}
	for _, f := range v.Files {
		if err := s.db.QueryRowContext(ctx, "SELECT body FROM harness_blobs WHERE sha256 = ?", f.SHA256).Scan(&z); err != nil {
			return NotesContent{}, fmt.Errorf("notes version %d: %s: %w", id, f.Path, err)
		}
		if f.Body, err = gunzipBytes(z); err != nil {
			return NotesContent{}, fmt.Errorf("notes version %d: %s: %w", id, f.Path, err)
		}
		out.Files = append(out.Files, f)
	}
	return out, nil
}

// NotesProposal is a proposal for a repository's notes: a curator's, or a
// restore of an earlier version.
type NotesProposal struct {
	ID               int64           `json:"id"`
	RepoID           int64           `json:"repo_id"`
	Kind             string          `json:"kind"`              // ProposalCuration | ProposalRestore
	Trigger          string          `json:"trigger,omitempty"` // why a curation ran: over_limit, weekly, request
	BaseVersionID    *int64          `json:"base_version_id,omitempty"`
	VersionID        *int64          `json:"version_id,omitempty"`         // the proposed state
	AppliedVersionID *int64          `json:"applied_version_id,omitempty"` // recorded when it was applied
	Changes          json.RawMessage `json:"changes,omitempty"`            // the curator's changes.json
	State            string          `json:"state"`
	Reason           string          `json:"reason,omitempty"` // rejected: the operator's; expired, invalid, superseded: magnum's
	Model            string          `json:"model,omitempty"`
	PromptSHA256     string          `json:"prompt_sha256,omitempty"`
	Scratch          string          `json:"scratch,omitempty"` // the curation's directory
	CreatedAt        time.Time       `json:"created_at"`
	DecidedAt        *time.Time      `json:"decided_at,omitempty"`
}

const proposalColumns = `id, repo_id, kind, trigger_reason, base_version_id, version_id, applied_version_id, changes_json, state,
reason, model, prompt_sha256, scratch, created_at, decided_at`

func scanNotesProposal(sc scanner) (NotesProposal, error) {
	var p NotesProposal
	var trigger, reason, model, prompt, scratch sql.NullString
	err := sc.Scan(&p.ID, &p.RepoID, &p.Kind, &trigger, &p.BaseVersionID, &p.VersionID, &p.AppliedVersionID, rawCol(&p.Changes),
		&p.State, &reason, &model, &prompt, &scratch, timeCol(&p.CreatedAt), nullTime(&p.DecidedAt))
	p.Trigger, p.Reason, p.Model, p.PromptSHA256, p.Scratch = trigger.String, reason.String, model.String, prompt.String, scratch.String
	return p, err
}

// NotesProposalInput is a proposal to store (CreateNotesProposal).
type NotesProposalInput struct {
	RepoID        int64
	Kind          string // ProposalCuration | ProposalRestore
	Trigger       string
	BaseVersionID int64 // 0 = none
	// Proposed is a curation's proposed state, recorded as a version of
	// source curation tied to the proposal; nil for a restore (VersionID)
	// or an invalid proposal (no state kept).
	Proposed *NotesContent
	// VersionID is the version a restore proposes.
	VersionID int64
	Changes   []byte // changes.json (nil = none)
	State     string // ProposalPending, or ProposalInvalid with Reason
	Reason    string
	Model     string
	PromptSHA string
	Scratch   string
	At        time.Time // zero = now
	// Misses is what a curation did with each miss it was given
	// (notes_proposal_misses).
	Misses []ProposalMiss
}

// CreateNotesProposal stores a proposal (and a curation's proposed state as
// a version of source curation, and what it did with its misses) in one
// transaction.
func (s *Store) CreateNotesProposal(ctx context.Context, in NotesProposalInput) (NotesProposal, error) {
	if err := errors.Join(oneOf("proposal kind", in.Kind, proposalKinds), oneOf("proposal state", in.State, proposalStates)); err != nil {
		return NotesProposal{}, err
	}
	if in.Changes != nil && !json.Valid(in.Changes) {
		return NotesProposal{}, errors.New("create notes proposal: changes are not JSON")
	}
	at := in.At
	if at.IsZero() {
		at = s.now()
	}
	var out NotesProposal
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var changes any
		if in.Changes != nil {
			changes = string(in.Changes)
		}
		var decided any
		if in.State != ProposalPending {
			decided = FormatTime(at)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO notes_proposals (repo_id, kind, trigger_reason, base_version_id, version_id,
  changes_json, state, reason, model, prompt_sha256, scratch, created_at, decided_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.RepoID, in.Kind, nullString(in.Trigger), nullInt(in.BaseVersionID), nullInt(in.VersionID), changes, in.State,
			nullString(in.Reason), nullString(in.Model), nullString(in.PromptSHA), nullString(in.Scratch), FormatTime(at), decided)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if in.Proposed != nil {
			v, _, err := recordNotesVersion(ctx, tx, NotesVersionInput{RepoID: in.RepoID, At: at, Source: NotesFromCuration,
				ProposalID: id, Content: *in.Proposed}, at)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE notes_proposals SET version_id = ? WHERE id = ?", v.ID, id); err != nil {
				return err
			}
		}
		if err := linkProposalMisses(ctx, tx, id, in.Misses); err != nil {
			return err
		}
		out, err = scanNotesProposal(tx.QueryRowContext(ctx, "SELECT "+proposalColumns+" FROM notes_proposals WHERE id = ?", id))
		return err
	})
	if err != nil {
		return NotesProposal{}, fmt.Errorf("create notes proposal: %w", err)
	}
	return out, nil
}

// NotesProposalFilter selects proposals: of a repository (0 = any), in
// States (empty = any), newest first, at most Limit (0 = all).
type NotesProposalFilter struct {
	RepoID int64
	States []string
	Limit  int
}

// NotesProposals lists the proposals f selects.
func (s *Store) NotesProposals(ctx context.Context, f NotesProposalFilter) ([]NotesProposal, error) {
	q := "SELECT " + proposalColumns + " FROM notes_proposals WHERE 1=1"
	var args []any
	if f.RepoID != 0 {
		q += " AND repo_id = ?"
		args = append(args, f.RepoID)
	}
	if len(f.States) > 0 {
		q += " AND state IN (" + placeholders(len(f.States)) + ")"
		args = append(args, anys(f.States)...)
	}
	q += " ORDER BY id DESC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("notes proposals: %w", err)
	}
	ps, err := collect(rows, scanNotesProposal)
	if err != nil {
		return nil, fmt.Errorf("notes proposals: %w", err)
	}
	return ps, nil
}

// CountNotesProposals counts the proposals in state.
func (s *Store) CountNotesProposals(ctx context.Context, state string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notes_proposals WHERE state = ?", state).Scan(&n)
	return n, err
}

// DecideNotesProposal moves proposal id from one of from to state to, with
// reason, compare-and-set (ErrConflict when it is in another state). With
// applied, the state the proposal leads to is recorded in the same
// transaction as a version of the repository (applied.RepoID and ProposalID
// are set here) and linked as the proposal's applied_version_id. The misses
// the proposal was given follow it (decideProposalMisses).
func (s *Store) DecideNotesProposal(ctx context.Context, id int64, from []string, to, reason string, at time.Time, applied *NotesVersionInput) (NotesProposal, error) {
	if err := oneOf("proposal state", to, proposalStates); err != nil {
		return NotesProposal{}, err
	}
	if at.IsZero() {
		at = s.now()
	}
	var out NotesProposal
	err := s.tx(ctx, func(tx *sql.Tx) error {
		p, err := scanNotesProposal(tx.QueryRowContext(ctx, "SELECT "+proposalColumns+" FROM notes_proposals WHERE id = ?", id))
		if err != nil {
			return notFound(err, "notes proposal", id)
		}
		if !slices.Contains(from, p.State) {
			return fmt.Errorf("%w: notes proposal %d is %s", ErrConflict, id, p.State)
		}
		var appliedID any
		if applied != nil {
			in := *applied
			in.RepoID, in.ProposalID = p.RepoID, id
			v, _, err := recordNotesVersion(ctx, tx, in, at)
			if err != nil {
				return err
			}
			appliedID = v.ID
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notes_proposals SET state = ?, reason = COALESCE(?, reason), decided_at = ?,
  applied_version_id = COALESCE(?, applied_version_id) WHERE id = ?`, to, nullString(reason), FormatTime(at), appliedID, id); err != nil {
			return err
		}
		if err := decideProposalMisses(ctx, tx, id, to, at); err != nil {
			return err
		}
		out, err = scanNotesProposal(tx.QueryRowContext(ctx, "SELECT "+proposalColumns+" FROM notes_proposals WHERE id = ?", id))
		return err
	})
	if err != nil {
		return NotesProposal{}, fmt.Errorf("decide notes proposal %d: %w", id, err)
	}
	return out, nil
}

// NotesFileUse is how a harness file of a repository was used: since when
// it exists, how many judge rounds it existed for and how many of them (or
// their reviewers) used it.
type NotesFileUse struct {
	File      string     `json:"file"`
	FirstSeen time.Time  `json:"first_seen"`
	Rounds    int        `json:"rounds"`
	Uses      int        `json:"uses"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

// Unused reports whether the file is a curation candidate: it existed for
// NotesUnusedRounds judge rounds or more and no round recorded a use.
func (u NotesFileUse) Unused() bool { return u.Rounds >= NotesUnusedRounds && u.Uses == 0 }

// RecordNotesRound counts a judge round of repoID for the harness files
// present now: each counts one more round, a new one starts at one (first
// seen at), and the rows of files that are gone are dropped (a file that
// comes back starts over).
func (s *Store) RecordNotesRound(ctx context.Context, repoID int64, files []string, at time.Time) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		q := "DELETE FROM notes_files WHERE repo_id = ?"
		args := []any{repoID}
		if len(files) > 0 {
			q += " AND file NOT IN (" + placeholders(len(files)) + ")"
			args = append(args, anys(files)...)
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
		for _, f := range files {
			if _, err := tx.ExecContext(ctx, `INSERT INTO notes_files (repo_id, file, first_seen, rounds) VALUES (?, ?, ?, 1)
ON CONFLICT (repo_id, file) DO UPDATE SET rounds = rounds + 1`, repoID, f, FormatTime(at)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("record notes round: %w", err)
	}
	return nil
}

// RecordNotesUsage records that run runID used files of repoID's harness
// (once per file and run).
func (s *Store) RecordNotesUsage(ctx context.Context, repoID int64, runID string, files []string, at time.Time) error {
	if runID == "" || len(files) == 0 {
		return nil
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, f := range files {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO notes_usage (repo_id, file, run_id, used_at) VALUES (?, ?, ?, ?)",
				repoID, f, runID, FormatTime(at)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("record notes usage: %w", err)
	}
	return nil
}

// NotesFileUses lists repoID's harness files with their rounds and the uses
// recorded since each was first seen, by file name.
func (s *Store) NotesFileUses(ctx context.Context, repoID int64) ([]NotesFileUse, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.file, f.first_seen, f.rounds,
  (SELECT COUNT(*) FROM notes_usage u WHERE u.repo_id = f.repo_id AND u.file = f.file AND u.used_at >= f.first_seen),
  (SELECT MAX(u.used_at) FROM notes_usage u WHERE u.repo_id = f.repo_id AND u.file = f.file AND u.used_at >= f.first_seen)
FROM notes_files f WHERE f.repo_id = ? ORDER BY f.file`, repoID)
	if err != nil {
		return nil, fmt.Errorf("notes file uses: %w", err)
	}
	return collect(rows, func(sc scanner) (NotesFileUse, error) {
		var u NotesFileUse
		err := sc.Scan(&u.File, timeCol(&u.FirstSeen), &u.Rounds, &u.Uses, nullTime(&u.LastUsed))
		return u, err
	})
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(z []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
