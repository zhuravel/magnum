package store

// Declines (migration 0023): how the authors of the PRs magnum reviewed
// answered its own findings, one row per review thread whose root comment is
// magnum's and that someone else replied to, as the retro's classifier read
// the replies. Reply text is never stored. States are the misses' (MissNew,
// MissUsed, MissDismissed): a lesson drawn from a decline uses it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// What the author did with a finding (declines.outcome).
const (
	DeclineFixed     = "fixed"     // changed the code as the finding asked
	DeclineDeclined  = "declined"  // will not change it
	DeclineDeferred  = "deferred"  // agreed, for later (a ticket, a follow-up PR)
	DeclineArgued    = "argued"    // disputed the finding
	DeclineQuestion  = "question"  // asked about it, no decision
	DeclineUnrelated = "unrelated" // the replies are about something else
)

// Whether magnum was right, for a declined or argued finding
// (declines.verdict).
const (
	VerdictWrong          = "wrong"           // the finding was wrong
	VerdictOverRanked     = "over_ranked"     // right, but its priority was too high
	VerdictMissingContext = "missing_context" // right in general, wrong for this repository
	VerdictRight          = "right"           // the finding stands
)

// The values the schema's CHECKs accept, checked in Go first for a clear error.
var (
	declineOutcomes = []string{DeclineFixed, DeclineDeclined, DeclineDeferred, DeclineArgued, DeclineQuestion, DeclineUnrelated}
	declineVerdicts = []string{VerdictWrong, VerdictOverRanked, VerdictMissingContext, VerdictRight}
)

// Decline is how an author answered one of magnum's posted findings
// (declines).
type Decline struct {
	ID        int64  `json:"id"`
	PRID      int64  `json:"pr_id"`
	SourceURL string `json:"source_url"` // GitHub's link to the finding's comment; the key UpsertDecline stores by
	CommentID int64  `json:"comment_id"` // the finding's comment (REST id)
	ReviewID  int64  `json:"review_id,omitempty"`
	// RunID is the run that posted the review (runs.review_id), and
	// FindingRowID the findings row of that run on the same path within 3
	// lines (nil when none, or once the run's findings were recorded again).
	RunID        string `json:"run_id,omitempty"`
	FindingRowID *int64 `json:"finding_row_id,omitempty"`
	Path         string `json:"path,omitempty"`
	Line         int    `json:"line,omitempty"`
	Title        string `json:"title,omitempty"`
	Priority     string `json:"priority,omitempty"` // P0..P3 as the comment said
	ReviewedSHA  string `json:"reviewed_sha"`       // the commit the review covered
	// SkillSHA is the 12-hex hash of the judge skill copy the round ran with.
	SkillSHA string   `json:"skill_sha,omitempty"`
	Outcome  string   `json:"outcome"`           // Decline*
	Verdict  string   `json:"verdict,omitempty"` // Verdict*: declined and argued only
	Lesson   string   `json:"lesson,omitempty"`  // wrong, over_ranked and missing_context only
	Scope    string   `json:"scope,omitempty"`   // MissScopeRepo | MissScopeGeneral, with a lesson
	Match    []string `json:"match"`             // match_json
	State    string   `json:"state"`             // MissNew | MissUsed | MissDismissed
	// CreatedAt and UpdatedAt are when the row was first and last written.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Repo ("owner/name") and Number name the decline's PR; Declines fills
	// them.
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	// Dismissed says why the decision of skill proposal DismissedBy
	// dismissed the decline (LearnDismissed*), "" and nil when none did;
	// Declines fills them.
	Dismissed   string `json:"dismissed,omitempty"`
	DismissedBy *int64 `json:"dismissed_by,omitempty"`
}

// simplificationTitle starts the title learn gives an own thread whose
// root is a simplification (learn's findingTitle). It stands alone or is
// followed by one of simplificationTails: ": " and the first sentence of
// what the proposal removes (since 2026-10-12), and in rows stored before
// that the heading's tag, " (optional)" or, with its bold left in,
// "** (optional, no reply needed)".
const simplificationTitle = "Simplification"

var simplificationTails = []string{":", " (", "*"}

// IsSimplification reports whether d answers a simplification: no
// priority (a finding always has one), and a title learn gave one, in any
// of its stored forms, matched case-sensitively. simplificationSQL says
// the same in SQL.
func (d Decline) IsSimplification() bool {
	rest, ok := strings.CutPrefix(d.Title, simplificationTitle)
	return d.Priority == "" && ok && (rest == "" || slices.ContainsFunc(simplificationTails, func(t string) bool {
		return strings.HasPrefix(rest, t)
	}))
}

// simplificationSQL is IsSimplification over a declines row named d.
// GLOB is case-sensitive, as strings.HasPrefix is, and `[*]` is a literal
// star.
const simplificationSQL = `(COALESCE(d.priority, '') = '' AND (COALESCE(d.title, '') = 'Simplification'
    OR COALESCE(d.title, '') GLOB 'Simplification:*' OR COALESCE(d.title, '') GLOB 'Simplification (*'
    OR COALESCE(d.title, '') GLOB 'Simplification[*]*'))`

var declineColumns = []string{"id", "pr_id", "source_url", "comment_id", "review_id", "run_id", "finding_row_id",
	"path", "line", "title", "priority", "reviewed_sha", "skill_sha", "outcome", "verdict", "lesson", "scope",
	"match_json", "state", "created_at", "updated_at"}

// scanDecline scans a row of declineColumns, followed by the PR's
// repository and number and the skill proposal's dismissal
// (learnDismissalDecline) when withPR is set.
func scanDecline(sc scanner, withPR bool) (Decline, error) {
	var d Decline
	var reviewID, line sql.NullInt64
	dest := []any{&d.ID, &d.PRID, &d.SourceURL, &d.CommentID, &reviewID, textCol(&d.RunID), &d.FindingRowID,
		textCol(&d.Path), &line, textCol(&d.Title), textCol(&d.Priority), &d.ReviewedSHA, textCol(&d.SkillSHA),
		&d.Outcome, textCol(&d.Verdict), textCol(&d.Lesson), textCol(&d.Scope), jsonCol(&d.Match), &d.State,
		timeCol(&d.CreatedAt), timeCol(&d.UpdatedAt)}
	if withPR {
		dest = append(dest, &d.Repo, &d.Number, textCol(&d.Dismissed), &d.DismissedBy)
	}
	if err := sc.Scan(dest...); err != nil {
		return d, err
	}
	d.ReviewID, d.Line = reviewID.Int64, int(line.Int64)
	if d.Match == nil {
		d.Match = []string{}
	}
	return d, nil
}

// UpsertDecline stores d by SourceURL and returns the stored row. A new
// decline is inserted (an empty State is MissNew); an existing one is
// updated in place and keeps its id, state and created_at, so running the
// retro again never brings back a dismissed or used decline. Every other
// column takes d's value: each write carries the classifier's whole answer.
// A nil Match is an empty list, and "", 0 or nil in an optional column is
// stored as NULL. PRID, SourceURL, CommentID, ReviewedSHA and Outcome are
// required.
func (s *Store) UpsertDecline(ctx context.Context, d Decline) (Decline, error) {
	if d.PRID == 0 || d.SourceURL == "" || d.CommentID == 0 || d.ReviewedSHA == "" {
		return Decline{}, fmt.Errorf("upsert decline %q: pr_id, source_url, comment_id and reviewed_sha are required", d.SourceURL)
	}
	if d.State == "" {
		d.State = MissNew
	}
	for _, c := range []struct {
		what, v  string
		allowed  []string
		optional bool // "" is allowed
	}{{"outcome", d.Outcome, declineOutcomes, false}, {"verdict", d.Verdict, declineVerdicts, true},
		{"scope", d.Scope, missScopes, true}, {"state", d.State, missStates, false}} {
		if c.optional && c.v == "" {
			continue
		}
		if err := oneOf(c.what, c.v, c.allowed); err != nil {
			return Decline{}, fmt.Errorf("upsert decline %s: %w", d.SourceURL, err)
		}
	}
	if d.Match == nil {
		d.Match = []string{}
	}
	match, err := json.Marshal(d.Match)
	if err != nil {
		return Decline{}, fmt.Errorf("upsert decline %s: %w", d.SourceURL, err)
	}
	var findingRow any
	if d.FindingRowID != nil {
		findingRow = *d.FindingRowID
	}
	now := FormatTime(s.now())
	got, err := scanDecline(s.db.QueryRowContext(ctx, `
INSERT INTO declines (pr_id, source_url, comment_id, review_id, run_id, finding_row_id, path, line, title, priority,
  reviewed_sha, skill_sha, outcome, verdict, lesson, scope, match_json, state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source_url) DO UPDATE SET pr_id = excluded.pr_id, comment_id = excluded.comment_id,
  review_id = excluded.review_id, run_id = excluded.run_id, finding_row_id = excluded.finding_row_id,
  path = excluded.path, line = excluded.line, title = excluded.title, priority = excluded.priority,
  reviewed_sha = excluded.reviewed_sha, skill_sha = excluded.skill_sha, outcome = excluded.outcome,
  verdict = excluded.verdict, lesson = excluded.lesson, scope = excluded.scope, match_json = excluded.match_json,
  updated_at = excluded.updated_at
RETURNING `+cols("", declineColumns),
		d.PRID, d.SourceURL, d.CommentID, nullInt(d.ReviewID), nullString(d.RunID), findingRow, nullString(d.Path),
		nullInt(d.Line), nullString(d.Title), nullString(d.Priority), d.ReviewedSHA, nullString(d.SkillSHA), d.Outcome,
		nullString(d.Verdict), nullString(d.Lesson), nullString(d.Scope), string(match), d.State, now, now), false)
	if err != nil {
		return Decline{}, fmt.Errorf("upsert decline %s: %w", d.SourceURL, mapErr(err))
	}
	return got, nil
}

// DeclineFilter selects declines; zero values select everything.
type DeclineFilter struct {
	Outcomes []string // empty = every outcome
	Verdicts []string // empty = any verdict, none included
	Scopes   []string // empty = any scope, none included
	States   []string // empty = every state
	PRID     int64    // 0 = every PR
	RepoID   int64    // 0 = every repository
	SkillSHA string   // "" = any skill
}

// where renders the WHERE clause of f over declines aliased d ("" when f
// selects everything).
func (f DeclineFilter) where() (string, []any) {
	var conds []string
	var args []any
	for _, in := range []struct {
		col  string
		vals []string
	}{{"d.outcome", f.Outcomes}, {"d.verdict", f.Verdicts}, {"d.scope", f.Scopes}, {"d.state", f.States}} {
		if len(in.vals) > 0 {
			conds = append(conds, in.col+" IN ("+placeholders(len(in.vals))+")")
			args = append(args, anys(in.vals)...)
		}
	}
	if f.PRID != 0 {
		conds = append(conds, "d.pr_id = ?")
		args = append(args, f.PRID)
	}
	if f.RepoID != 0 {
		conds = append(conds, "d.pr_id IN (SELECT id FROM prs WHERE repo_id = ?)")
		args = append(args, f.RepoID)
	}
	if f.SkillSHA != "" {
		conds = append(conds, "d.skill_sha = ?")
		args = append(args, f.SkillSHA)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// Declines returns the declines f selects, newest first, each with its PR's
// repository and number and why a skill proposal dismissed it, if one did.
func (s *Store) Declines(ctx context.Context, f DeclineFilter) ([]Decline, error) {
	where, args := f.where()
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("d", declineColumns)+", rp.owner || '/' || rp.name, p.number, "+learnDismissalDecline+
		" FROM declines d JOIN prs p ON p.id = d.pr_id JOIN repos rp ON rp.id = p.repo_id"+where+
		" ORDER BY d.created_at DESC, d.id DESC", args...)
	if err != nil {
		return nil, fmt.Errorf("declines: %w", err)
	}
	out, err := collect(rows, func(sc scanner) (Decline, error) { return scanDecline(sc, true) })
	if err != nil {
		return nil, fmt.Errorf("declines: %w", err)
	}
	return out, nil
}

// CountDeclines counts the declines f selects.
func (s *Store) CountDeclines(ctx context.Context, f DeclineFilter) (int, error) {
	where, args := f.where()
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM declines d"+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count declines: %w", err)
	}
	return n, nil
}

// SetDeclineState moves each decline of ids that is in one of the states
// from to state to (compare-and-set per row) and returns how many moved; a
// decline in another state is left alone.
func (s *Store) SetDeclineState(ctx context.Context, ids []int64, from []string, to string) (int, error) {
	return setDeclineState(ctx, s.db, ids, from, to, s.now())
}

// setDeclineState is SetDeclineState on q (the store's pool or a
// transaction), stamping updated_at with at.
func setDeclineState(ctx context.Context, q querier, ids []int64, from []string, to string, at time.Time) (int, error) {
	if err := oneOf("state", to, missStates); err != nil {
		return 0, fmt.Errorf("set decline state: %w", err)
	}
	if len(from) == 0 {
		return 0, fmt.Errorf("set decline state: no from-state")
	}
	for _, f := range from {
		if err := oneOf("from state", f, missStates); err != nil {
			return 0, fmt.Errorf("set decline state: %w", err)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	args := []any{to, FormatTime(at)}
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, anys(from)...)
	res, err := q.ExecContext(ctx, "UPDATE declines SET state = ?, updated_at = ? WHERE id IN ("+placeholders(len(ids))+
		") AND state IN ("+placeholders(len(from))+")", args...)
	if err != nil {
		return 0, fmt.Errorf("set decline state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("set decline state: %w", err)
	}
	return int(n), nil
}
