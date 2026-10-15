package pipeline

// Audit passes (DECISIONS "Every fifth re-review audits what no round
// probed"): the engine makes every [pipeline] audit_every-th automatic
// re-review of a PR whose evidence map holds unknown or stale areas an
// audit pass (engine audit.go), a recovery with a fresh judge whose
// reviewers re-review the delta (RoundInput.ColdJudge), and hands it the
// areas to probe (RoundInput.Audit): at most three unknown or stale ones,
// the riskiest first, and one earlier probe to run again. Their paths and
// behaviours come from the PR, so the round writes them to audit.json in
// its report directory and the judge's recovery prompt names the file
// (JudgeData.AuditFile), never an area.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/fsx"
)

// AuditFile is an audit pass's areas in the round's report directory.
const AuditFile = "audit.json"

// AuditArea is an area an audit pass probes: an entry of the PR's evidence
// map (unknown, stale, or a probe to run again) with the head and round
// that wrote it and why the engine chose it: the risk it ranked by
// ("irreversible write", "authorization", "money", "optional branch",
// "pre-pass lead"), "" for none, or "rotation" for the probe run again.
type AuditArea struct {
	EvidenceEntry
	Was   string `json:"was,omitempty"`  // a stale entry's state when written
	Head  string `json:"head,omitempty"` // the head its round examined
	Round int    `json:"round,omitempty"`
	Why   string `json:"why,omitempty"`
}

// auditFile is audit.json.
type auditFile struct {
	PR      string      `json:"pr"`       // owner/repo#N
	HeadSHA string      `json:"head_sha"` // the head under review
	Areas   []AuditArea `json:"areas"`
}

// audit is the round's audit areas: RoundInput.Audit in a recovery, none
// in any other round.
func (rd *round) audit() []AuditArea {
	if rd.in.Kind != KindRecovery {
		return nil
	}
	return rd.in.Audit
}

// addAudit writes an audit pass's areas to AuditFile in the report
// directory and names the file in jd (AuditFile, AuditAreas). A file that
// cannot be written leaves the prompt without the clause, so the round
// runs as an ordinary recovery, with a warning.
func (rd *round) addAudit(ctx context.Context, jd *agents.JudgeData) {
	areas := rd.audit()
	if len(areas) == 0 {
		return
	}
	b, err := json.MarshalIndent(auditFile{PR: fmt.Sprintf("%s/%s#%d", rd.owner, rd.name, rd.in.PR.Number), HeadSHA: rd.in.TargetSHA, Areas: areas}, "", "  ")
	if err == nil {
		path := filepath.Join(rd.dir, AuditFile)
		if err = fsx.WriteFileAtomic(path, append(b, '\n'), 0o600); err == nil {
			jd.AuditFile, jd.AuditAreas = path, len(areas)
			return
		}
	}
	rd.warn(ctx, "could not write %s; the audit pass runs as a recovery: %v", AuditFile, err)
}
