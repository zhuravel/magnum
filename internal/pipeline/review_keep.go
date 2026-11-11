package pipeline

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"regexp"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/fsx"
)

// reviewFileMax bounds the copy of a posted review's file (keepReviewFile):
// a review and its inline comments, never near it.
const reviewFileMax = 8 << 20

// runIDRe is the shape of a run id that names a file (keepReviewFile).
var runIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// keepReviewFile keeps the review file of a posted run (runID, the judge's
// run that posted it) as review-<run id>.json beside it. The report
// directory is per head, so a later round on the same head writes its
// review over review.json (agents.PostReviewFile), which stays the latest
// for its readers (the fixer's prompt, the improvement loop): ten report
// directories held two or three posted reviews each while only the last
// one's text stayed. A file that is missing, not a regular file or too
// large is not kept, with a warning; the review is posted either way.
func (rd *round) keepReviewFile(ctx context.Context, runID string) {
	if !runIDRe.MatchString(runID) {
		return
	}
	src := filepath.Join(rd.dir, agents.PostReviewFile)
	b, err := fsx.ReadRegular(src, reviewFileMax)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err == nil {
		err = fsx.WriteFileAtomic(filepath.Join(rd.dir, "review-"+runID+".json"), b, 0o600)
	}
	if err != nil {
		rd.warn(ctx, "keep the posted review's file of run %s: %v", runID, err)
	}
}
