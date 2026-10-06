package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Retro outcomes per PR (retro_prs.status).
const (
	RetroNothing      = "nothing" // no candidate: nothing to classify
	RetroClassified   = "classified"
	RetroUnclassified = "unclassified" // candidates stored without a classifier
	RetroFailed       = "failed"
)

// Miss classes (misses.class).
const (
	MissUnclassified = "unclassified"
	MissMiss         = "miss"
	MissNotIssue     = "not_issue"
	MissStyle        = "style"
	MissOutside      = "outside"
)

// Miss sources (misses.source_kind): a review thread or a review body.
const (
	MissSourceThread = "thread"
	MissSourceReview = "review"
)

// What magnum did with the point of a miss (misses.raised): it never raised
// it, or raised it and the judge rejected it.
const (
	MissRaisedNone     = "none"
	MissRaisedRejected = "rejected"
)

// Miss states (misses.state): new until a lesson was drawn from it (used) or
// someone dropped it (dismissed).
const (
	MissNew       = "new"
	MissUsed      = "used"
	MissDismissed = "dismissed"
)

// Miss scopes (misses.scope): a lesson for the PR's repository or for every
// repository.
const (
	MissScopeRepo    = "repo"
	MissScopeGeneral = "general"
)

// The values the schema's CHECKs accept, checked in Go first for a clear error.
var (
	retroStatuses = []string{RetroNothing, RetroClassified, RetroUnclassified, RetroFailed}
	missClasses   = []string{MissUnclassified, MissMiss, MissNotIssue, MissStyle, MissOutside}
	missSources   = []string{MissSourceThread, MissSourceReview}
	missRaised    = []string{MissRaisedNone, MissRaisedRejected}
	missStates    = []string{MissNew, MissUsed, MissDismissed}
	missScopes    = []string{MissScopeRepo, MissScopeGeneral}
)

// oneOf returns an error unless v is one of allowed.
func oneOf(what, v string, allowed []string) error {
	if slices.Contains(allowed, v) {
		return nil
	}
	return fmt.Errorf("%s %q is not one of %s", what, v, strings.Join(allowed, ", "))
}

// RetroMaxAttempts is how many retros in a row may fail on a PR before the
// retro gives it up (RetroDue; `magnum retro --again` still takes it).
const RetroMaxAttempts = 3

// RetroPR is one PR's retro record (retro_prs).
type RetroPR struct {
	PRID       int64     `json:"pr_id"`
	RetroAt    time.Time `json:"retro_at"`
	Day        string    `json:"day"` // DayKey of the retro
	Status     string    `json:"status"`
	Candidates int       `json:"candidates"`
	Error      string    `json:"error,omitempty"`
	// Attempts counts the retros in a row that failed on the PR (0 once
	// one did not); RecordRetroPR keeps it.
	Attempts int `json:"attempts"`
}

// RecordRetroPR stores r as the retro record of r.PRID, replacing an earlier
// one. A zero RetroAt is now and an empty Day is DayKey(RetroAt); Status must
// be one of the Retro* values. An empty Error is stored as NULL. Attempts is
// not taken from r: a failed status adds one to the PR's count, any other
// sets it to 0.
func (s *Store) RecordRetroPR(ctx context.Context, r RetroPR) error {
	if r.PRID == 0 {
		return fmt.Errorf("record retro: pr id is required")
	}
	if err := oneOf("status", r.Status, retroStatuses); err != nil {
		return fmt.Errorf("record retro of pr %d: %w", r.PRID, err)
	}
	if r.RetroAt.IsZero() {
		r.RetroAt = s.now()
	}
	if r.Day == "" {
		r.Day = DayKey(r.RetroAt)
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO retro_prs (pr_id, retro_at, day, status, candidates, error, attempts) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(pr_id) DO UPDATE SET retro_at = excluded.retro_at, day = excluded.day, status = excluded.status,
  candidates = excluded.candidates, error = excluded.error,
  attempts = CASE WHEN excluded.status = 'failed' THEN retro_prs.attempts + 1 ELSE 0 END`,
		r.PRID, FormatTime(r.RetroAt), r.Day, r.Status, r.Candidates, nullString(r.Error), boolInt(r.Status == RetroFailed))
	if err != nil {
		return fmt.Errorf("record retro of pr %d: %w", r.PRID, mapErr(err))
	}
	return nil
}

// RetroPRByID returns the retro record of prID, or an error matching
// ErrNotFound when the PR has none.
func (s *Store) RetroPRByID(ctx context.Context, prID int64) (RetroPR, error) {
	var r RetroPR
	err := s.db.QueryRowContext(ctx, "SELECT pr_id, retro_at, day, status, candidates, error, attempts FROM retro_prs WHERE pr_id = ?", prID).
		Scan(&r.PRID, timeCol(&r.RetroAt), &r.Day, &r.Status, &r.Candidates, textCol(&r.Error), &r.Attempts)
	if err != nil {
		return RetroPR{}, notFound(err, "retro of pr", prID)
	}
	return r, nil
}

// Miss is a comment another reviewer made on a PR magnum reviewed, with what the retro made of it (misses).
type Miss struct {
	ID          int64     `json:"id"`
	PRID        int64     `json:"pr_id"`
	SourceURL   string    `json:"source_url"`  // GitHub's link to the comment; the key UpsertMiss stores by
	SourceKind  string    `json:"source_kind"` // MissSourceThread | MissSourceReview
	Reviewer    string    `json:"reviewer"`
	Path        string    `json:"path,omitempty"`
	Line        int       `json:"line,omitempty"` // 0 = none (a comment on the review body)
	ReviewedSHA string    `json:"reviewed_sha"`   // the commit magnum's review covered
	Class       string    `json:"class"`          // Miss* class
	Severity    string    `json:"severity,omitempty"`
	Raised      string    `json:"raised"`                // MissRaisedNone | MissRaisedRejected
	FindingRef  string    `json:"finding_ref,omitempty"` // the rejected finding: "<run id>/<finding id>" (findings)
	ReasonCode  string    `json:"reason_code,omitempty"` // why the judge rejected it
	Title       string    `json:"title,omitempty"`
	Lesson      string    `json:"lesson,omitempty"`
	Scope       string    `json:"scope,omitempty"` // MissScopeRepo | MissScopeGeneral
	Lines       []int     `json:"lines"`           // lines_json
	Match       []string  `json:"match"`           // match_json
	State       string    `json:"state"`           // MissNew | MissUsed | MissDismissed
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Repo ("owner/name") and Number name the miss's PR; Misses fills them.
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	// ProposalID and ProposalState name the latest notes proposal the miss
	// was given to (notes_proposal_misses) and where that one stands: the
	// proposal that used it once it is used; Misses fills them.
	ProposalID    *int64 `json:"proposal_id,omitempty"`
	ProposalState string `json:"proposal_state,omitempty"`
}

var missColumns = []string{"id", "pr_id", "source_url", "source_kind", "reviewer", "path", "line", "reviewed_sha",
	"class", "severity", "raised", "finding_ref", "reason_code", "title", "lesson", "scope", "lines_json",
	"match_json", "state", "created_at", "updated_at"}

// missListColumns follow missColumns in a listing (Misses): the PR's
// repository and number, and the latest proposal the miss was given to
// with its state.
const missListColumns = `rp.owner || '/' || rp.name, p.number,
  (SELECT l.proposal_id FROM notes_proposal_misses l WHERE l.miss_id = m.id ORDER BY l.proposal_id DESC LIMIT 1),
  (SELECT np.state FROM notes_proposal_misses l JOIN notes_proposals np ON np.id = l.proposal_id
   WHERE l.miss_id = m.id ORDER BY l.proposal_id DESC LIMIT 1)`

// scanMiss scans a row of missColumns, followed by missListColumns when
// withPR is set.
func scanMiss(sc scanner, withPR bool) (Miss, error) {
	var m Miss
	var line sql.NullInt64
	var proposalState sql.NullString
	dest := []any{&m.ID, &m.PRID, &m.SourceURL, &m.SourceKind, &m.Reviewer, textCol(&m.Path), &line, &m.ReviewedSHA,
		&m.Class, textCol(&m.Severity), &m.Raised, textCol(&m.FindingRef), textCol(&m.ReasonCode), textCol(&m.Title),
		textCol(&m.Lesson), textCol(&m.Scope), jsonCol(&m.Lines), jsonCol(&m.Match), &m.State,
		timeCol(&m.CreatedAt), timeCol(&m.UpdatedAt)}
	if withPR {
		dest = append(dest, &m.Repo, &m.Number, &m.ProposalID, &proposalState)
	}
	if err := sc.Scan(dest...); err != nil {
		return m, err
	}
	m.Line, m.ProposalState = int(line.Int64), proposalState.String
	return m, nil
}

// UpsertMiss stores m by SourceURL and returns the stored row. A new miss is
// inserted (an empty State is MissNew); an existing one is updated in place
// and keeps its id, state and created_at, so running the retro again never
// brings back a dismissed or used miss. Every other column takes m's value,
// except that an unclassified m (a retro whose classifier failed) never
// replaces a class an earlier retro set, nor what came with it (severity,
// title, lesson, scope, lines, match).
// Empty Class is MissUnclassified, empty Raised is MissRaisedNone, nil Lines
// and Match are empty lists, and "" or 0 in an optional column is stored as
// NULL. PRID, SourceURL, SourceKind, Reviewer and ReviewedSHA are required.
func (s *Store) UpsertMiss(ctx context.Context, m Miss) (Miss, error) {
	if m.PRID == 0 || m.SourceURL == "" || m.SourceKind == "" || m.Reviewer == "" || m.ReviewedSHA == "" {
		return Miss{}, fmt.Errorf("upsert miss %q: pr_id, source_url, source_kind, reviewer and reviewed_sha are required", m.SourceURL)
	}
	if m.Class == "" {
		m.Class = MissUnclassified
	}
	if m.Raised == "" {
		m.Raised = MissRaisedNone
	}
	if m.State == "" {
		m.State = MissNew
	}
	for _, c := range []struct {
		what, v string
		allowed []string
	}{{"source_kind", m.SourceKind, missSources}, {"class", m.Class, missClasses}, {"raised", m.Raised, missRaised},
		{"state", m.State, missStates}} {
		if err := oneOf(c.what, c.v, c.allowed); err != nil {
			return Miss{}, fmt.Errorf("upsert miss %s: %w", m.SourceURL, err)
		}
	}
	if m.Scope != "" {
		if err := oneOf("scope", m.Scope, missScopes); err != nil {
			return Miss{}, fmt.Errorf("upsert miss %s: %w", m.SourceURL, err)
		}
	}
	if m.Lines == nil {
		m.Lines = []int{}
	}
	if m.Match == nil {
		m.Match = []string{}
	}
	lines, err := json.Marshal(m.Lines)
	if err != nil {
		return Miss{}, fmt.Errorf("upsert miss %s: %w", m.SourceURL, err)
	}
	match, err := json.Marshal(m.Match)
	if err != nil {
		return Miss{}, fmt.Errorf("upsert miss %s: %w", m.SourceURL, err)
	}
	now := FormatTime(s.now())
	got, err := scanMiss(s.db.QueryRowContext(ctx, `
INSERT INTO misses (pr_id, source_url, source_kind, reviewer, path, line, reviewed_sha, class, severity, raised,
  finding_ref, reason_code, title, lesson, scope, lines_json, match_json, state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source_url) DO UPDATE SET pr_id = excluded.pr_id, source_kind = excluded.source_kind,
  reviewer = excluded.reviewer, path = excluded.path, line = excluded.line, reviewed_sha = excluded.reviewed_sha,
  raised = excluded.raised, finding_ref = excluded.finding_ref, reason_code = excluded.reason_code,
  class = `+keepClass("class")+`, severity = `+keepClass("severity")+`, title = `+keepClass("title")+`,
  lesson = `+keepClass("lesson")+`, scope = `+keepClass("scope")+`, lines_json = `+keepClass("lines_json")+`,
  match_json = `+keepClass("match_json")+`, updated_at = excluded.updated_at
RETURNING `+cols("", missColumns),
		m.PRID, m.SourceURL, m.SourceKind, m.Reviewer, nullString(m.Path), nullInt(m.Line), m.ReviewedSHA, m.Class,
		nullString(m.Severity), m.Raised, nullString(m.FindingRef), nullString(m.ReasonCode), nullString(m.Title),
		nullString(m.Lesson), nullString(m.Scope), string(lines), string(match), m.State, now, now), false)
	if err != nil {
		return Miss{}, fmt.Errorf("upsert miss %s: %w", m.SourceURL, mapErr(err))
	}
	return got, nil
}

// keepClass is the upsert's value of a column that comes with a miss's
// class: the stored one when an unclassified write meets a classified row,
// else the new one.
func keepClass(col string) string {
	return "CASE WHEN excluded.class = '" + MissUnclassified + "' AND misses.class <> '" + MissUnclassified +
		"' THEN misses." + col + " ELSE excluded." + col + " END"
}

// MissFilter selects misses; zero values select everything.
type MissFilter struct {
	Classes []string // empty = every class
	States  []string // empty = every state
	Scopes  []string // empty = any scope, none included
	PRID    int64    // 0 = every PR
	RepoID  int64    // 0 = every repository
}

// where renders the WHERE clause of f over misses aliased m ("" when f selects everything).
func (f MissFilter) where() (string, []any) {
	var conds []string
	var args []any
	if len(f.Classes) > 0 {
		conds = append(conds, "m.class IN ("+placeholders(len(f.Classes))+")")
		args = append(args, anys(f.Classes)...)
	}
	if len(f.States) > 0 {
		conds = append(conds, "m.state IN ("+placeholders(len(f.States))+")")
		args = append(args, anys(f.States)...)
	}
	if len(f.Scopes) > 0 {
		conds = append(conds, "m.scope IN ("+placeholders(len(f.Scopes))+")")
		args = append(args, anys(f.Scopes)...)
	}
	if f.PRID != 0 {
		conds = append(conds, "m.pr_id = ?")
		args = append(args, f.PRID)
	}
	if f.RepoID != 0 {
		conds = append(conds, "m.pr_id IN (SELECT id FROM prs WHERE repo_id = ?)")
		args = append(args, f.RepoID)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// Misses returns the misses f selects, newest first, each with its PR's
// repository and number and the latest proposal it was given to.
func (s *Store) Misses(ctx context.Context, f MissFilter) ([]Miss, error) {
	where, args := f.where()
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("m", missColumns)+", "+missListColumns+
		" FROM misses m JOIN prs p ON p.id = m.pr_id JOIN repos rp ON rp.id = p.repo_id"+where+
		" ORDER BY m.created_at DESC, m.id DESC", args...)
	if err != nil {
		return nil, fmt.Errorf("misses: %w", err)
	}
	out, err := collect(rows, func(sc scanner) (Miss, error) { return scanMiss(sc, true) })
	if err != nil {
		return nil, fmt.Errorf("misses: %w", err)
	}
	return out, nil
}

// CountMisses counts the misses f selects.
func (s *Store) CountMisses(ctx context.Context, f MissFilter) (int, error) {
	where, args := f.where()
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM misses m"+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count misses: %w", err)
	}
	return n, nil
}

// RetroQuery selects the PRs a retro looks at.
type RetroQuery struct {
	Since time.Time // closed or merged at or after Since (ignored when PRIDs is set)
	// Until is the settle delay's bound: closed or merged at or before it
	// (zero = no bound; ignored when PRIDs is set).
	Until time.Time
	Again bool    // also PRs that already have a retro_prs row
	PRIDs []int64 // only these PRs, whenever they closed
}

// retroClosedAt is when a PR closed for the retro: closed_at, else merged_at.
const retroClosedAt = "COALESCE(p.closed_at, p.merged_at)"

// retroConds are RetroDue's conditions but the settle delay's.
func (q RetroQuery) retroConds() ([]string, []any) {
	conds := []string{"p.gh_state IN ('" + GHMerged + "', '" + GHClosed + "')",
		"EXISTS (SELECT 1 FROM runs r WHERE r.pr_id = p.id AND r.review_id IS NOT NULL)"}
	var args []any
	if len(q.PRIDs) > 0 {
		conds = append(conds, "p.id IN ("+placeholders(len(q.PRIDs))+")")
		for _, id := range q.PRIDs {
			args = append(args, id)
		}
	} else {
		conds = append(conds, retroClosedAt+" >= ?")
		args = append(args, FormatTime(q.Since))
	}
	if !q.Again {
		conds = append(conds, "NOT EXISTS (SELECT 1 FROM retro_prs t WHERE t.pr_id = p.id AND NOT (t.status = ? AND t.attempts < ?))")
		args = append(args, RetroFailed, RetroMaxAttempts)
	}
	return conds, args
}

// RetroDue lists the PRs due for a retro, newest closed first: merged or
// closed, closed (closed_at, else merged_at) at or after q.Since and, with
// q.Until, at or before it, unless q.PRIDs names the PRs, with at least one
// run that posted a review, and unless q.Again without a retro record, or
// with a failed one of fewer than RetroMaxAttempts attempts.
func (s *Store) RetroDue(ctx context.Context, q RetroQuery) ([]PR, error) {
	conds, args := q.retroConds()
	if len(q.PRIDs) == 0 && !q.Until.IsZero() {
		conds = append(conds, retroClosedAt+" <= ?")
		args = append(args, FormatTime(q.Until))
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("p", prColumns)+" FROM prs p WHERE "+strings.Join(conds, " AND ")+
		" ORDER BY "+retroClosedAt+" DESC, p.id DESC", args...)
	if err != nil {
		return nil, fmt.Errorf("retro due: %w", err)
	}
	out, err := collect(rows, scanPR)
	if err != nil {
		return nil, fmt.Errorf("retro due: %w", err)
	}
	return out, nil
}

// RetroSettling counts the PRs that would be due for q's retro but closed
// after q.Until: they wait for the settle delay. 0 without Until or with
// PRIDs.
func (s *Store) RetroSettling(ctx context.Context, q RetroQuery) (int, error) {
	if q.Until.IsZero() || len(q.PRIDs) > 0 {
		return 0, nil
	}
	conds, args := q.retroConds()
	conds = append(conds, retroClosedAt+" > ?")
	args = append(args, FormatTime(q.Until))
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM prs p WHERE "+strings.Join(conds, " AND "), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("retro settling: %w", err)
	}
	return n, nil
}

// textScanner scans a nullable TEXT column into a string (NULL is "").
type textScanner struct{ p *string }

func textCol(p *string) textScanner { return textScanner{p} }

func (t textScanner) Scan(v any) error {
	s, _, err := textOf(v)
	if err != nil {
		return err
	}
	*t.p = s
	return nil
}
