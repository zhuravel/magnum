package pipeline

// The reports a round's judge went without (DECISIONS "Auto-approval hears
// every reviewer"): the judge's no-findings event is COMMENT then
// (JudgeEvents), which an App posts anyway, so the round also keeps, in the
// PR's record, which roles were part of it and left no usable report.
// Auto-approval reads the record of the round whose review it approves and
// approves nothing after such a round, and a continue of the round reads it
// to keep what its paused judge went without (a role skipped as logged out
// leaves no run row). The record keeps one entry per round, so a later
// round (a reply round that posts no review) never rewrites the entry of
// the round whose review stands, and a round of the judge alone (a delta
// check, a same-head re-review, a reply round) carries what the round
// before it went without: its review builds on that round's.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// KVMissingReports is the kv key of the PR's record of missing reports: a
// MissingReports for each of its latest judged rounds (missingRecords), as
// a JSON list by round; one written before the record kept every round is
// a lone object.
func KVMissingReports(prID int64) string { return fmt.Sprintf("pr.%d.missing_reports", prID) }

// missingRecords is how many of the PR's latest judged rounds its record
// keeps: auto-approval reads the PR's latest posted review, and only rounds
// that posted none (reply rounds, failed ones) come after it.
const missingRecords = 20

// MissingReports is what the judge of a PR's round went without: the roles
// of the round that left no usable report (one that ran and wrote none,
// timed out or failed, one skipped as logged out), with why; empty when it
// heard every one. A role the round did not run (not configured to, or one
// a continue's paused round never ran) is none of them. A round that ran no
// reviewer (the judge alone: a delta check, a same-head re-review, a reply
// round) carries what the round before it went without (Carried, each with
// the round that went without it): its review builds on that round's, whose
// reviewers it did not hear again.
type MissingReports struct {
	Round   int             `json:"round"`
	Head    string          `json:"head"` // the commit the round reviewed
	Missing []MissingReport `json:"missing"`
	Carried []MissingReport `json:"carried,omitempty"`
}

// MissingReport is one role's report the judge went without and why: its
// report status ("missing", "timeout", "login_required", ...); Round, in
// MissingReports.Carried, is the round whose judge went without it.
type MissingReport struct {
	Role   string `json:"role"`
	Status string `json:"status"`
	Round  int    `json:"round,omitempty"`
}

// Unheard is every report the round's review went without: its judge's own
// (Missing) and those it carried.
func (m MissingReports) Unheard() []MissingReport {
	return append(slices.Clone(m.Missing), m.Carried...)
}

// String names the reports the round's review went without:
// "codex-review (login_required), claude-review (timeout)", a carried one
// with its round ("codex-review (login_required in round 5)"); "" when none.
func (m MissingReports) String() string {
	var out []string
	for _, r := range m.Missing {
		out = append(out, fmt.Sprintf("%s (%s)", r.Role, r.Status))
	}
	for _, r := range m.Carried {
		out = append(out, fmt.Sprintf("%s (%s in round %d)", r.Role, r.Status, r.Round))
	}
	return strings.Join(out, ", ")
}

// ReadMissingReports reads the PR's record of its latest judged round;
// false when there is none or it cannot be read.
func ReadMissingReports(ctx context.Context, st *store.Store, prID int64) (MissingReports, bool) {
	all, err := readMissing(ctx, st, prID)
	if err != nil || len(all) == 0 {
		return MissingReports{}, false
	}
	return all[len(all)-1], true
}

// ReadRoundMissingReports reads the PR's record of round; false when there
// is none (a round judged before the records, or before the latest
// missingRecords) or it cannot be read.
func ReadRoundMissingReports(ctx context.Context, st *store.Store, prID int64, round int) (MissingReports, bool) {
	all, err := readMissing(ctx, st, prID)
	if err != nil {
		return MissingReports{}, false
	}
	if i := slices.IndexFunc(all, func(m MissingReports) bool { return m.Round == round }); i >= 0 {
		return all[i], true
	}
	return MissingReports{}, false
}

// readMissing reads the PR's record by round: none when there is none or
// it does not parse (each round rewrites it whole).
func readMissing(ctx context.Context, st *store.Store, prID int64) ([]MissingReports, error) {
	if st == nil {
		return nil, nil
	}
	v, ok, err := st.GetKV(ctx, KVMissingReports(prID))
	if err != nil || !ok {
		return nil, err
	}
	var all []MissingReports
	if json.Unmarshal([]byte(v), &all) == nil {
		return all, nil
	}
	var one MissingReports
	if json.Unmarshal([]byte(v), &one) == nil {
		return []MissingReports{one}, nil
	}
	return nil, nil
}

// RecordMissingReports keeps, as the PR's record of round (which reviewed
// head), the reports its judge's prompt names as missing (reports,
// judgeReports'): a round already recorded (a continue's judge) is
// replaced, and a round that ran no reviewer (no reports) carries what the
// latest round before it went without. It returns the round's record.
func RecordMissingReports(ctx context.Context, st *store.Store, prID int64, round int, head string,
	reports []agents.Report) (MissingReports, error) {
	all, err := readMissing(ctx, st, prID)
	if err != nil {
		return MissingReports{}, err
	}
	m := MissingReports{Round: round, Head: head, Missing: []MissingReport{}}
	for _, r := range reports {
		if r.Missing || r.Path == "" {
			m.Missing = append(m.Missing, MissingReport{Role: r.Role, Status: cmp.Or(r.Status, r.Detail, ReportMissing)})
		}
	}
	all = slices.DeleteFunc(all, func(r MissingReports) bool { return r.Round == round })
	slices.SortFunc(all, func(a, b MissingReports) int { return cmp.Compare(a.Round, b.Round) })
	before := slices.IndexFunc(all, func(r MissingReports) bool { return r.Round > round })
	if before < 0 {
		before = len(all)
	}
	if len(reports) == 0 && before > 0 {
		m.Carried = carried(all[before-1])
	}
	all = slices.Insert(all, before, m)
	all = all[max(0, len(all)-missingRecords):]
	b, err := json.Marshal(all)
	if err != nil {
		return MissingReports{}, err
	}
	return m, st.SetKV(ctx, KVMissingReports(prID), string(b))
}

// carried is what a round of the judge alone after prev carries: prev's
// missing reports, named with prev's round, and those prev carried.
func carried(prev MissingReports) []MissingReport {
	var out []MissingReport
	for _, r := range prev.Missing {
		r.Round = prev.Round
		out = append(out, r)
	}
	return append(out, prev.Carried...)
}

// recordMissing keeps the PR's record of this round (RecordMissingReports).
// Best effort: a store error is a warning, and auto-approval then finds no
// record of the round and approves nothing after it.
func (rd *round) recordMissing(ctx context.Context, reports []agents.Report) {
	if _, err := RecordMissingReports(context.WithoutCancel(ctx), rd.r.Store, rd.in.PR.ID, rd.in.Round, rd.in.TargetSHA, reports); err != nil {
		rd.warn(ctx, "record the round's missing reports: %v", err)
	}
}
