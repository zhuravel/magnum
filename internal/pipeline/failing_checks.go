package pipeline

// The head's failing checks (DECISIONS "The judge sees the head's failing
// checks"): a PR got LGTM and the operator's auto-approval while its RSpec
// check had failed on that head 25 minutes earlier, and the review's Checks
// did not mention it. When a judge prompt goes out, the registry's checks
// of the PR (prs.ci_json, as the poller's last Details fetch saw them) that
// belong to the head under review and failed go to failing-checks.json in
// the report directory, named as `failing_checks`; the skill gives each one
// a Checks line saying whether the PR causes it. The PR's workflows name
// the checks, so the names stay in the file, never in a prompt or an event.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// FailingChecksFile is the head's failing checks in the round's report
// directory.
const FailingChecksFile = "failing-checks.json"

// FailingChecks is failing-checks.json.
type FailingChecks struct {
	PR      string `json:"pr"`       // owner/repo#N
	HeadSHA string `json:"head_sha"` // the head under review, which the checks ran on
	// Complete is false when GitHub listed only part of the head's checks
	// (at most 100): more may have failed.
	Complete bool                `json:"complete"`
	Checks   []store.CheckResult `json:"checks"` // the failed ones: name, state, workflow, time
}

// failingChecks is failing-checks.json's content: the failed checks of
// ci when it is the head's; nil Checks otherwise.
func (rd *round) failingChecks(ci *store.CIStatus) FailingChecks {
	out := FailingChecks{PR: fmt.Sprintf("%s/%s#%d", rd.owner, rd.name, rd.in.PR.Number), HeadSHA: rd.in.TargetSHA}
	if ci == nil || ci.SHA == "" || ci.SHA != rd.in.TargetSHA {
		return out
	}
	out.Complete = ci.Complete
	for _, c := range ci.Checks {
		if c.State == store.CheckFailed {
			out.Checks = append(out.Checks, c)
		}
	}
	return out
}

// addFailingChecks writes failing-checks.json for the judge prompt about to
// go out and names it in jd, when a check of the head under review failed,
// from the registry's latest read of the PR (the round's input when that
// read fails); a blind replay, which reads no CI result, gets none. A
// failure to write leaves the prompt without it (a warning).
func (rd *round) addFailingChecks(ctx context.Context, jd *agents.JudgeData) {
	if rd.in.Blind || rd.r.Store == nil {
		return
	}
	pr := rd.in.PR
	if cur, err := rd.r.Store.PRByID(ctx, pr.ID); err == nil {
		pr = cur
	}
	fc := rd.failingChecks(pr.CI)
	if len(fc.Checks) == 0 {
		return
	}
	path := filepath.Join(rd.dir, FailingChecksFile)
	b, err := json.MarshalIndent(fc, "", "  ")
	if err == nil {
		err = fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
	}
	if err != nil {
		if ctx.Err() == nil {
			rd.event(ctx, "warn", "round.failing_checks", fmt.Sprintf("could not write the head's failing checks; the judge goes without: %v", err), nil)
		}
		return
	}
	jd.FailingChecks = path
	rd.event(ctx, "info", "round.failing_checks", fmt.Sprintf("%s: %d failing check(s) on the head", FailingChecksFile, len(fc.Checks)),
		map[string]any{"failing": len(fc.Checks), "file": path})
}
