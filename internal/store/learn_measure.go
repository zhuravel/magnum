package store

// What a landed skill version did (DECISIONS "A landed version is measured
// over its window"): the rounds that ran it, from the restart that brought
// it to the rounds to the restart on the next version, and what came of
// their findings. The window holds the posted judge runs of the rounds
// that ran the version's skill (a run whose review was posted,
// runs.created_at in the window, and a run of the review whose prompt names
// the version's skill copy, <state>/skill/<skill_sha>/: a skill changed
// outside the loop runs other rounds); their reviews; their posted findings
// by priority; how authors answered those findings (declines by outcome,
// skill_sha = the version's, without the simplifications refused for cost
// alone); and what other reviewers caught that those
// runs missed (misses of class miss on a commit one of them reviewed, by
// scope).

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// VersionMeasure is what the rounds of one landed version did in its window
// (Store.VersionMeasure).
type VersionMeasure struct {
	VersionID int64      `json:"version"` // learn_versions.id; 0 for the skill before the first version
	Commit    string     `json:"commit,omitempty"`
	From      time.Time  `json:"from"`
	To        *time.Time `json:"to,omitempty"` // nil: the window is open (the version runs now)
	// JudgeRuns counts the posted reviews (a nudged round's once), Posted
	// the findings they posted and ByPriority those by priority ("" for
	// none).
	JudgeRuns  int            `json:"judge_runs"`
	Posted     int            `json:"posted"`
	ByPriority map[string]int `json:"by_priority"`
	// Outcomes counts how authors answered those findings (Decline*),
	// without the simplifications declined or argued that the retro judged
	// right: a refusal for cost alone is the author's choice for one
	// thread, no answer against the version, so it moves neither Declined
	// nor the regression check.
	Outcomes map[string]int `json:"outcomes"`
	// MissesGeneral and MissesRepo count what other reviewers caught on a
	// commit those reviews covered, by the lesson's scope.
	MissesGeneral int `json:"misses_general"`
	MissesRepo    int `json:"misses_repo"`
}

// Declined is how many of the findings authors declined or argued.
func (m VersionMeasure) Declined() int {
	return m.Outcomes[DeclineDeclined] + m.Outcomes[DeclineArgued]
}

// DeclinedShare is the declined and argued findings' share of the posted
// ones, in percent; false without a posted finding.
func (m VersionMeasure) DeclinedShare() (float64, bool) {
	if m.Posted == 0 {
		return 0, false
	}
	return 100 * float64(m.Declined()) / float64(m.Posted), true
}

// VersionMeasure measures v over its window: from v.RestartedAt (its
// LandedAt while the daemon has not restarted on it) to next.RestartedAt
// (open when next is nil or has not restarted yet), over the rounds that
// ran v's skill copy. A v without a SkillSHA (the skill before the first
// version, which no row records) counts every round of the window and
// every decline of its runs.
func (s *Store) VersionMeasure(ctx context.Context, v LearnVersion, next *LearnVersion) (VersionMeasure, error) {
	m := VersionMeasure{VersionID: v.ID, Commit: v.LandedSHA, From: v.LandedAt, ByPriority: map[string]int{}, Outcomes: map[string]int{}}
	if v.RestartedAt != nil {
		m.From = *v.RestartedAt
	}
	var to any
	if next != nil && next.RestartedAt != nil {
		t := *next.RestartedAt
		m.To, to = &t, FormatTime(t)
	}
	// window are the posted judge runs of the window whose round ran the
	// version's skill: a review one of whose runs names its copy (?4; a
	// nudged round's later runs name none), every review when v names no
	// skill.
	const window = `SELECT id, pr_id, target_sha, review_id FROM runs WHERE outcome = 'posted' AND review_id IS NOT NULL
  AND created_at >= ?1 AND (?2 IS NULL OR created_at < ?2)
  AND (?4 = '' OR review_id IN (SELECT r.review_id FROM runs r WHERE r.review_id IS NOT NULL AND r.created_at >= ?1
    AND (?2 IS NULL OR r.created_at < ?2) AND instr(r.prompt_text, '/skill/' || ?4 || '/') > 0))`
	from := FormatTime(m.From)
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(DISTINCT review_id) FROM ("+window+")", from, to, nil, v.SkillSHA).Scan(&m.JudgeRuns); err != nil {
		return m, fmt.Errorf("measure version %d: %w", v.ID, err)
	}
	counts := func(into map[string]int, q string, args ...any) error {
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		_, err = collect(rows, func(sc scanner) (struct{}, error) {
			var k sql.NullString
			var n int
			if err := sc.Scan(&k, &n); err != nil {
				return struct{}{}, err
			}
			into[k.String] += n
			return struct{}{}, nil
		})
		return err
	}
	if err := counts(m.ByPriority, `SELECT f.severity, COUNT(*) FROM findings f WHERE f.verdict = ?3 AND f.run_id IN (SELECT id FROM (`+window+
		`)) GROUP BY f.severity`, from, to, FindingPosted, v.SkillSHA); err != nil {
		return m, fmt.Errorf("measure version %d: findings: %w", v.ID, err)
	}
	for _, n := range m.ByPriority {
		m.Posted += n
	}
	// A simplification (simplificationSQL: Decline.IsSimplification in SQL)
	// declined or argued and judged right is a refusal for cost alone
	// (prompts/retro.md), no answer against the version. COALESCE keeps the
	// NOT true for a row with no verdict, priority or title.
	if err := counts(m.Outcomes, `SELECT d.outcome, COUNT(*) FROM declines d WHERE d.run_id IN (SELECT id FROM (`+window+`))
  AND (?3 = '' OR d.skill_sha = ?3) AND NOT (d.outcome IN (?5, ?6) AND COALESCE(d.verdict, '') = ?7 AND `+simplificationSQL+`)
  GROUP BY d.outcome`, from, to, v.SkillSHA, v.SkillSHA, DeclineDeclined, DeclineArgued, VerdictRight); err != nil {
		return m, fmt.Errorf("measure version %d: declines: %w", v.ID, err)
	}
	scopes := map[string]int{}
	if err := counts(scopes, `SELECT m.scope, COUNT(*) FROM misses m WHERE m.class = ?3 AND EXISTS (SELECT 1 FROM (`+window+`) w
  WHERE w.pr_id = m.pr_id AND w.target_sha = m.reviewed_sha) GROUP BY m.scope`, from, to, MissMiss, v.SkillSHA); err != nil {
		return m, fmt.Errorf("measure version %d: misses: %w", v.ID, err)
	}
	m.MissesGeneral, m.MissesRepo = scopes[MissScopeGeneral], scopes[MissScopeRepo]
	return m, nil
}

// VersionMeasures measures the newest n versions of vs (newest first, as
// LearnVersions lists them; n <= 0 = all), each up to the restart on the
// version after it.
func (s *Store) VersionMeasures(ctx context.Context, vs []LearnVersion, n int) ([]VersionMeasure, error) {
	if n <= 0 || n > len(vs) {
		n = len(vs)
	}
	out := make([]VersionMeasure, 0, n)
	for i := range n {
		var next *LearnVersion
		if i > 0 {
			next = &vs[i-1]
		}
		m, err := s.VersionMeasure(ctx, vs[i], next)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}
