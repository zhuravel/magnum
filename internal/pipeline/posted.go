package pipeline

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/textx"
)

// Checks of a verified review that never change the round's outcome: a
// review posted twice under the run's marker, local paths in the posted
// text, failures of the review machine the judge kept out of the review.

// ReviewDeleter is implemented by a GitHub client that can delete a
// pending (unsubmitted) review: DELETE /repos/{o}/{r}/pulls/{n}/reviews/{id}.
// GitHub refuses it for a submitted review. Optional: without it a
// duplicate draft is only reported.
type ReviewDeleter interface {
	DeletePendingReview(ctx context.Context, owner, repo string, number int, reviewID int64) error
}

// handleDuplicates reports reviews that carry the run's marker besides the
// kept one (round.duplicate_review) and deletes the pending ones. A
// submitted duplicate cannot be deleted through the API and stays, with a
// warning.
func (rd *round) handleDuplicates(ctx context.Context, p *postedReview) {
	if p == nil || len(p.duplicates)+len(p.pending) == 0 {
		return
	}
	var deleted, failed []int64
	del, canDelete := rd.r.GitHub.(ReviewDeleter)
	for _, id := range p.pending {
		if !canDelete {
			failed = append(failed, id)
			continue
		}
		if err := del.DeletePendingReview(ctx, rd.owner, rd.name, rd.in.PR.Number, id); err != nil {
			rd.logf("pipeline: delete pending review %d: %v", id, err)
			failed = append(failed, id)
			continue
		}
		deleted = append(deleted, id)
	}
	var msg []string
	if n := len(p.duplicates); n > 0 {
		msg = append(msg, fmt.Sprintf("%s (%s) carry the run's marker; kept %d, the first. GitHub cannot delete a submitted review: delete its comments or edit it by hand",
			textx.Count(n, "more submitted review", "more submitted reviews"), joinIDs(p.duplicates), p.id))
	}
	if len(deleted) > 0 {
		msg = append(msg, "deleted the pending duplicate "+joinIDs(deleted))
	}
	if len(failed) > 0 {
		msg = append(msg, "could not delete the pending duplicate "+joinIDs(failed))
	}
	text := strings.Join(msg, "; ")
	rd.mu.Lock()
	rd.res.Warnings = append(rd.res.Warnings, text)
	rd.mu.Unlock()
	rd.event(ctx, "warn", "round.duplicate_review", text, map[string]any{
		"kept": p.id, "duplicates": p.duplicates, "pending": p.pending, "deleted": deleted})
}

// localPathRe matches an absolute path of the review machine: under a temp
// directory or a home directory, not inside a URL or a relative path.
var localPathRe = regexp.MustCompile(`(?:^|[^\w.~/:-])((?:/private)?/(?:tmp|var/folders|Users|home)/[^\s"'` + "`" + `<>()\[\]{}|,;*]*)`)

// localPaths returns the distinct local paths in text, in order: matches of
// localPathRe and any of the literal paths (the checkout, magnum's home).
func localPaths(text string, literal []string) []string {
	var out []string
	add := func(p string) {
		p = strings.TrimRight(p, ".:!?")
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, m := range localPathRe.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, l := range literal {
		if len(l) > 1 && strings.Contains(text, l) && !slices.ContainsFunc(out, func(p string) bool { return strings.HasPrefix(p, l) }) {
			add(l)
		}
	}
	return out
}

// checkLocalPaths scans the posted review's body and inline comments for
// paths of the review machine (the author cannot open them) and records a
// round.local_paths warning; the review stays posted.
func (rd *round) checkLocalPaths(ctx context.Context, p *postedReview) {
	if p == nil || p.id == 0 {
		return
	}
	literal := []string{rd.in.SlotPath, rd.r.Layout.Home, rd.r.Layout.Data(), rd.r.Layout.State()}
	type hit struct {
		Where string   `json:"where"`
		Paths []string `json:"paths"`
	}
	var hits []hit
	if ps := localPaths(p.body, literal); len(ps) > 0 {
		hits = append(hits, hit{"body", ps})
	}
	for _, c := range rd.reviewComments(ctx, p.id) {
		if ps := localPaths(c.body, literal); len(ps) > 0 {
			hits = append(hits, hit{c.where, ps})
		}
	}
	if len(hits) == 0 {
		return
	}
	var parts []string
	for _, h := range hits {
		parts = append(parts, h.Where+": "+strings.Join(h.Paths, ", "))
	}
	text := fmt.Sprintf("review %d shows paths of the review machine, which the author cannot open: %s", p.id, strings.Join(parts, "; "))
	rd.mu.Lock()
	rd.res.Warnings = append(rd.res.Warnings, text)
	rd.mu.Unlock()
	rd.event(ctx, "warn", "round.local_paths", text, map[string]any{"review_id": p.id, "hits": hits})
}

// environmentFailures records the failures of the review machine the judge
// reported (round.environment): the operator fixes the machine, the author
// never sees them.
func (rd *round) environmentFailures(ctx context.Context, fs []envFailure) {
	if len(fs) == 0 {
		return
	}
	var parts []string
	for _, f := range fs {
		parts = append(parts, strings.TrimPrefix(f.Cmd+": "+f.Error, ": "))
	}
	rd.event(ctx, "warn", "round.environment", fmt.Sprintf("the judge hit %s of the review machine: %s", textx.Count(len(fs), "failure", "failures"), strings.Join(parts, "; ")),
		map[string]any{"failures": fs})
}

// ReviewCommentLister is implemented by a GitHub client that lists a
// review's inline comments (GET .../reviews/{id}/comments). Optional:
// without it only the review body is checked for local paths.
type ReviewCommentLister interface {
	ReviewComments(ctx context.Context, owner, repo string, number int, reviewID int64) ([]github.ReviewComment, error)
}

// commentText is an inline comment's location (path:line) and body.
type commentText struct{ where, body string }

// reviewComments lists the inline comments of review id; none when the
// client cannot list them or the call failed (logged).
func (rd *round) reviewComments(ctx context.Context, id int64) []commentText {
	lister, ok := rd.r.GitHub.(ReviewCommentLister)
	if !ok {
		return nil
	}
	comments, err := lister.ReviewComments(ctx, rd.owner, rd.name, rd.in.PR.Number, id)
	if err != nil {
		rd.logf("pipeline: list the comments of review %d: %v", id, err)
		return nil
	}
	out := make([]commentText, 0, len(comments))
	for _, c := range comments {
		where := c.Path
		if c.Line > 0 {
			where = fmt.Sprintf("%s:%d", c.Path, c.Line)
		}
		out = append(out, commentText{where, c.Body})
	}
	return out
}

func joinIDs(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = fmt.Sprint(id)
	}
	return strings.Join(s, ", ")
}
