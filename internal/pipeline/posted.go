package pipeline

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Checks of a verified review that never change the round's outcome: a
// review posted twice under the run's marker, local paths in the posted
// text, failures of the review machine the judge kept out of the review;
// and the identity's footer, which magnum appends to it.

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
			rd.logErr(ctx, err, "pipeline: delete pending review %d: %v", id, err)
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

// footerMarker starts the footer magnum appends to a verified review
// (appendFooter): a later edit replaces everything from it on instead of
// adding a second footer, and AppendToReview puts its notes above it.
const footerMarker = "<!-- magnum:footer -->"

// oldJudgeFooter starts the footer judges wrote themselves before magnum
// appended one (the default review_footer until 2026-10-06): a review that
// carries it gets no second footer.
const oldJudgeFooter = "_Automated review by [Magnum](https://github.com/zhuravel/magnum)."

// simplifyAlias is the alias of the role that proposes simplifications
// (config.defaults.toml; `magnum review --simplify` means it too).
const simplifyAlias = "simplify"

// appendFooter renders the identity's footer template for the verified
// review p (res is the judge's result: whether the review is clean) and
// appends it, after a blank line and footerMarker, through the
// author-checked edit AppendToReview makes (round.footer). It replaces a
// footer magnum appended before, so verifying a review again changes
// nothing. review_footer = "" appends nothing; a footer that does not
// render, or an edit GitHub refuses, is a warning.
func (rd *round) appendFooter(ctx context.Context, p *postedReview, res *judgeResult) {
	tmpl := rd.idCfg.Footer()
	if p == nil || p.id == 0 || tmpl == "" || rd.in.DryRun {
		return
	}
	text, err := config.RenderFooter(tmpl, rd.footerData(p, res))
	if err != nil {
		rd.warn(ctx, "review %d: the footer does not render: %v", p.id, err)
		return
	}
	if text == "" {
		return
	}
	changed, err := rd.r.editReview(ctx, rd.owner, rd.name, rd.in.PR.Number, p.id, func(body string) (string, bool) {
		return withFooter(body, footerMarker+"\n"+text)
	})
	if err != nil {
		rd.warn(ctx, "review %d: could not append the footer: %v", p.id, err)
		return
	}
	if changed {
		rd.event(ctx, "info", "round.footer", fmt.Sprintf("review %d: appended the identity's footer", p.id), map[string]any{"review_id": p.id})
	}
}

// footerData is what the footer template of review p renders with: the
// PR's watch says whether drafts are skipped and whose review request
// starts a round (its poll login when a gh identity's, else the posting
// login), [daemon] quiet_hours when pushes wait.
func (rd *round) footerData(p *postedReview, res *judgeResult) config.FooterData {
	sha := cmp.Or(p.commit, rd.in.TargetSHA)
	w := rd.r.Config.WatchFor(rd.owner + "/" + rd.name)
	_, simplify := rd.r.Config.RoleByNameOrAlias(w, simplifyAlias)
	event := normalizeEvent(p.state)
	if event == "" && res != nil {
		event = normalizeEvent(res.Event)
	}
	request := rd.login
	if w != nil {
		if poll := rd.r.Config.IdentityByName(w.PollIdentity); poll != nil && poll.Kind == "gh" && poll.Login != "" {
			request = poll.Login
		}
	}
	return config.FooterData{SHA: sha, Short: sha[:min(len(sha), 10)], Repo: rd.owner + "/" + rd.name, Number: rd.in.PR.Number,
		Login: rd.login, Simplify: simplify, Clean: clean(res), Event: reviewEvent(event),
		PostMerge: rd.in.PostMerge, DeltaCheck: rd.deltaCheck() != nil,
		QuietHours: config.QuietHoursLabel(rd.r.Config.Daemon.QuietHours, rd.r.now()), DraftsSkipped: w != nil && !w.DraftsIncluded(),
		RequestLogin: request}
}

// clean reports whether the judge's result counts nothing at all: no
// finding of any priority, no earlier finding still open, no
// simplification. Without a result it is false.
func clean(res *judgeResult) bool {
	var sum store.ReviewSummary
	if res == nil || !store.ParseReviewResult([]byte(res.Raw), &sum) {
		return false
	}
	return sum.Findings() == 0 && sum.Open == 0 && sum.Simplifications == 0
}

// reviewEvent is the review event (APPROVE, COMMENT, REQUEST_CHANGES) of a
// review state normalizeEvent returns.
func reviewEvent(state string) string {
	switch state {
	case "APPROVED":
		return "APPROVE"
	case "COMMENTED":
		return "COMMENT"
	case "CHANGES_REQUESTED":
		return "REQUEST_CHANGES"
	}
	return state
}

// withFooter puts footer (footerMarker first) last in body, after a blank
// line: in place of everything from an earlier footerMarker on, else after
// the body. false when body ends with that footer already, or carries a
// footer its judge wrote (oldJudgeFooter), which is left as it is.
func withFooter(body, footer string) (string, bool) {
	const space = "\r\n\t "
	head := body
	if before, _, ok := strings.CutLast(body, footerMarker); ok {
		head = before
	} else if strings.Contains(body, oldJudgeFooter) {
		return body, false
	}
	out := footer
	if head = strings.TrimRight(head, space); head != "" {
		out = head + "\n\n" + footer
	}
	if strings.TrimRight(body, space) == out {
		return body, false
	}
	return out, true
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

// envFailureRunes clips each failure in a round.environment message: a
// judge quotes its command's output (1.4K characters seen); the event's data
// and the result file keep the whole text.
const envFailureRunes = 300

// environmentFailures records the failures of the review machine the judge
// reported (round.environment): the operator fixes the machine, the author
// never sees them.
func (rd *round) environmentFailures(ctx context.Context, fs []envFailure) {
	if len(fs) == 0 {
		return
	}
	var parts []string
	for _, f := range fs {
		parts = append(parts, textx.Clip(strings.TrimPrefix(f.Cmd+": "+f.Error, ": "), envFailureRunes))
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
		rd.logErr(ctx, err, "pipeline: list the comments of review %d: %v", id, err)
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
