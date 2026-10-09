package store

// What a landed skill version did (DECISIONS "A landed version is measured
// over its window"): the rounds that ran it, from the restart that brought
// it to the rounds to the restart on the next version, and what came of
// their findings. The window holds the posted judge runs (a run whose
// review was posted, runs.created_at in the window); their posted findings
// by priority; how authors answered the findings of those runs that the
// round's skill copy was the version's (declines by outcome, skill_sha =
// the version's); and what other reviewers caught that those runs missed
// (misses of class miss on a commit one of them reviewed, by scope).

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
	// JudgeRuns counts the posted reviews, Posted the findings they posted
	// and ByPriority those by priority ("" for none).
	JudgeRuns  int            `json:"judge_runs"`
	Posted     int            `json:"posted"`
	ByPriority map[string]int `json:"by_priority"`
	// Outcomes counts how authors answered those findings (Decline*).
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
// (open when next is nil or has not restarted yet). A v without a SkillSHA
// (the skill before the first version, which no row records) counts every
// decline of the window's runs.
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
	// window are the posted judge runs of the window.
	const window = `SELECT id, pr_id, target_sha FROM runs WHERE outcome = 'posted' AND review_id IS NOT NULL AND created_at >= ?1
  AND (?2 IS NULL OR created_at < ?2)`
	from := FormatTime(m.From)
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ("+window+")", from, to).Scan(&m.JudgeRuns); err != nil {
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
		`)) GROUP BY f.severity`, from, to, FindingPosted); err != nil {
		return m, fmt.Errorf("measure version %d: findings: %w", v.ID, err)
	}
	for _, n := range m.ByPriority {
		m.Posted += n
	}
	if err := counts(m.Outcomes, `SELECT d.outcome, COUNT(*) FROM declines d WHERE d.run_id IN (SELECT id FROM (`+window+`))
  AND (?3 = '' OR d.skill_sha = ?3) GROUP BY d.outcome`, from, to, v.SkillSHA); err != nil {
		return m, fmt.Errorf("measure version %d: declines: %w", v.ID, err)
	}
	scopes := map[string]int{}
	if err := counts(scopes, `SELECT m.scope, COUNT(*) FROM misses m WHERE m.class = ?3 AND EXISTS (SELECT 1 FROM (`+window+`) w
  WHERE w.pr_id = m.pr_id AND w.target_sha = m.reviewed_sha) GROUP BY m.scope`, from, to, MissMiss); err != nil {
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
