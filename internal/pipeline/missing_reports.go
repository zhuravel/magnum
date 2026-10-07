package pipeline

// The reports a round's judge went without (DECISIONS "Auto-approval hears
// every reviewer"): the judge's no-findings event is COMMENT then
// (JudgeEvents), which an App posts anyway, so the round also keeps, in the
// PR's record, which roles were part of it and left no usable report.
// Auto-approval reads it and approves nothing after such a round, and a
// continue of the round reads it to keep what its paused judge went
// without (a role skipped as logged out leaves no run row).

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// KVMissingReports is the kv key of the PR's MissingReports, which each
// judge prompt of its rounds rewrites.
func KVMissingReports(prID int64) string { return fmt.Sprintf("pr.%d.missing_reports", prID) }

// MissingReports is what the judge of a PR's round went without: the roles
// of the round that left no usable report (one that ran and wrote none,
// timed out or failed, one skipped as logged out), with why; empty when it
// heard every one. A role the round did not run (not configured to, or one
// a continue's paused round never ran) is none of them.
type MissingReports struct {
	Round   int             `json:"round"`
	Head    string          `json:"head"` // the commit the round reviewed
	Missing []MissingReport `json:"missing"`
}

// MissingReport is one role's report the judge went without and why: its
// report status ("missing", "timeout", "login_required", ...).
type MissingReport struct {
	Role   string `json:"role"`
	Status string `json:"status"`
}

// String names the missing reports: "codex-review (login_required),
// claude-review (timeout)"; "" when none.
func (m MissingReports) String() string {
	out := make([]string, len(m.Missing))
	for i, r := range m.Missing {
		out[i] = fmt.Sprintf("%s (%s)", r.Role, r.Status)
	}
	return strings.Join(out, ", ")
}

// ReadMissingReports reads the PR's record of its latest judged round;
// false when there is none or it cannot be read.
func ReadMissingReports(ctx context.Context, st *store.Store, prID int64) (MissingReports, bool) {
	if st == nil {
		return MissingReports{}, false
	}
	v, ok, err := st.GetKV(ctx, KVMissingReports(prID))
	if err != nil || !ok {
		return MissingReports{}, false
	}
	var m MissingReports
	if json.Unmarshal([]byte(v), &m) != nil {
		return MissingReports{}, false
	}
	return m, true
}

// recordMissing keeps, as the PR's record of this round, the reports the
// judge's prompt names as missing (reports, judgeReports'). Best effort: a
// store error is a warning, and auto-approval then goes by the record it
// finds (another round's is not this one's).
func (rd *round) recordMissing(ctx context.Context, reports []agents.Report) {
	m := MissingReports{Round: rd.in.Round, Head: rd.in.TargetSHA, Missing: []MissingReport{}}
	for _, r := range reports {
		if r.Missing || r.Path == "" {
			m.Missing = append(m.Missing, MissingReport{Role: r.Role, Status: cmp.Or(r.Status, r.Detail, ReportMissing)})
		}
	}
	b, err := json.Marshal(m)
	if err == nil {
		err = rd.r.Store.SetKV(context.WithoutCancel(ctx), KVMissingReports(rd.in.PR.ID), string(b))
	}
	if err != nil {
		rd.warn(ctx, "record the round's missing reports: %v", err)
	}
}
