package pipeline

// A delta check (DECISIONS "A small re-review delta gets a judge-only
// check"): the engine dispatches a re-review whose delta since the reviewed
// commit is small as a round of the judge alone, in its own session, and
// says so in RoundInput.DeltaCheck. The judge's prompt asks for a short
// review of those commits and names a file listing them: the file names are
// the PR's, so no prompt prints them.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/fsx"
)

// DeltaCheckFile is the delta check's file list in the round's report
// directory.
const DeltaCheckFile = "delta-check.json"

// DeltaCheck is a delta check's measure of the commits since the judge's
// last review: their changed code lines (the re-review threshold's measure)
// and their files.
type DeltaCheck struct {
	Lines int         `json:"lines"`
	Files []DeltaFile `json:"files"`
}

// DeltaFile is a file of a delta check. Binary: modified without a patch
// (an image, a font), counted as 0 lines.
type DeltaFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Binary bool   `json:"binary,omitempty"`
}

// addDeltaCheck fills jd's delta check fields for a re-review that is one,
// with DeltaCheckFile written to the report directory (a failure leaves the
// prompt without the file: the judge reads the commits itself).
func (rd *round) addDeltaCheck(ctx context.Context, jd *agents.JudgeData) {
	dc := rd.in.DeltaCheck
	if dc == nil || rd.in.Kind != KindRereview {
		return
	}
	jd.DeltaCheck, jd.DeltaLines = true, dc.Lines
	b, err := json.MarshalIndent(dc, "", "  ")
	if err == nil {
		path := filepath.Join(rd.dir, DeltaCheckFile)
		if err = fsx.WriteFileAtomic(path, append(b, '\n'), 0o600); err == nil {
			jd.DeltaFile = path
			return
		}
	}
	rd.event(ctx, "warn", "round.delta_check", fmt.Sprintf("could not write the delta check's file list; the judge reads the commits itself: %v", err), nil)
}
