package engine

// The review status comment ([pipeline] status_comment, a [[watch]]'s
// status_comment): one comment on the conversation of each PR magnum means
// to review, posted as the PR's posting identity and edited in place, so the
// PR's people see that a review is queued (its head and why it waits), in
// progress or posted, and neither merge before it nor ask the operator where
// it is. The tick reconciles it with the registry (noteStatusComments, after
// noteWaits): its text follows from the PR's state, its recorded wait and
// its last review, so no round, request or poll writes it, a write GitHub
// refused is tried again by a later tick, and a restart goes on from the
// registry. A comment ends truthfully: a PR closed, merged or turned into a
// draft before its review posted gets one last edit that says so, and
// nothing more until a new head or a new review request. A review request
// gets an answer even where no review comes: a draft someone asked to
// review opens the comment as a ready PR does, and a request that came
// while the PR's mute or the watch's filters kept it out of review opens or
// turns it to the line that magnum is not reviewing the PR. The text is
// magnum's own: a head's short sha, times in UTC, the posted review's
// verdict in a few words and fixed words for the waits, never a PR's text
// nor an error's, and never why a PR is left out. Of auto-approval it says
// only approvalLine's fixed words: the login magnum approves as and the
// class of why a clean review brought no approval, never the gate's own.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kvPRStatusComment holds the PR's status comment (statusRecord as JSON):
// its id, who posted it and what was last written.
func kvPRStatusComment(prID int64) string { return fmt.Sprintf("pr.%d.status_comment", prID) }

const (
	// statusMarker is the status comment's first line.
	statusMarker = "<!-- magnum:status -->"
	// statusEvery is the least time between two writes of one PR's comment
	// (a failed one counts): the tick's waits change often, GitHub need not
	// hear of each. A last edit (statusView.ends) waits only after a failed
	// attempt.
	statusEvery = 2 * time.Minute
	// statusWarnEvery is the least time between two pr.status_comment_failed
	// events of one PR.
	statusWarnEvery = time.Hour
	// statusWritesPerTick caps a tick's writes (switching the comment on
	// with many PRs waiting): the rest go out at the next ticks.
	statusWritesPerTick = 10
	// statusReleasedWithin is how long after its release a PR's comment may
	// still get its last edit (one GitHub refused while the PR was closed):
	// the tick reads the released PRs whose row changed within it.
	statusReleasedWithin = 24 * time.Hour
	// statusMergeSentence and statusUpdates close a review to come.
	statusMergeSentence = "A merge before the review posts skips it."
	statusUpdates       = "This comment updates by itself."
)

// The stages of a status comment (statusRecord.Stage, the pr.status_comment
// event's stage).
const (
	statusScheduled    = "scheduled"
	statusInProgress   = "in_progress"
	statusPosted       = "posted"
	statusNotReviewing = "not_reviewing"
	statusMerged       = "merged" // GitHub merged the PR before its review posted
	statusClosed       = "closed" // closed without a merge before its review posted
	statusDraft        = "draft"  // the PR turned into a draft before its review posted
	statusFixing       = "fixing" // a fix loop's fixer works on the posted review's findings

	// statusFailed: magnum's own failure parked the PR (needs_attention
	// with a cause on its side, attention.OnMagnumsSide), where
	// statusNotReviewing is a PR muted, ignored, Codex-flagged or parked by
	// a fix job.
	statusFailed = "failed"
	// statusRequestDeclined: a review request came while the PR's mute or
	// the watch's filters kept it out of review (requestDeclined).
	statusRequestDeclined = "request_declined"
)

// The reviews a status comment follows (statusView.kind): the head's first
// review, a re-review of a new head, and another review of the reviewed
// head that someone asked for (anotherReview).
const (
	statusReview   = "review"
	statusRereview = "re-review"
	statusAnother  = "another review"
)

// statusStates are the PR states noteStatusComments reads: those of a
// review to come or under way, a posted one, one magnum will not review
// and a close (the released PRs of the last statusReleasedWithin come from
// a second read). A claiming PR keeps what it says until its round starts.
var statusStates = []string{store.PRQueued, store.PRRereviewPending, store.PRReviewing, store.PRVerifying, store.PRPaused,
	store.PRReviewed, store.PRNeedsAttention, store.PRIneligible, store.PRClosed, store.PRReleasing}

// statusEndStates are the states of a PR GitHub closed or merged whose
// comment gets its last edit; statusRoundStates those of a round under way.
var (
	statusEndStates   = []string{store.PRClosed, store.PRReleasing, store.PRReleased}
	statusRoundStates = []string{store.PRReviewing, store.PRVerifying, store.PRPaused}
)

// statusRecord is what kvPRStatusComment keeps of a PR's status comment.
type statusRecord struct {
	ID       int64     `json:"id,omitempty"`       // the comment's REST id; 0 = none stands
	Identity string    `json:"identity,omitempty"` // the identity that posted it, which edits it
	Sum      string    `json:"sum,omitempty"`      // statusSum of the text last written, its time left out
	Stage    string    `json:"stage,omitempty"`    // the stage last written
	Head     string    `json:"head,omitempty"`     // the head it was last written for
	Kind     string    `json:"kind,omitempty"`     // statusReview, statusRereview or statusAnother, as last written
	At       time.Time `json:"at,omitzero"`        // the last write or attempt
	Failed   bool      `json:"failed,omitempty"`   // the last attempt failed
	Warned   time.Time `json:"warned,omitzero"`    // the last pr.status_comment_failed
	Gone     int       `json:"gone,omitempty"`     // times a person deleted it
	// Off is the head a second deletion came on: no comment until another
	// head, since someone does not want one.
	Off string `json:"off,omitempty"`
	// State fingerprints the stage and the wait last written (statusView.
	// state), and Changed is when they last changed: the header's time,
	// which an edit of anything else leaves.
	State   string    `json:"state,omitempty"`
	Changed time.Time `json:"changed,omitzero"`
	// End is the head the comment's last edit was for ("" = it goes on),
	// EndAt when it was written and EndForced whether the PR was forced
	// then: nothing is written after it until statusResumes.
	End       string    `json:"end,omitempty"`
	EndAt     time.Time `json:"end_at,omitzero"`
	EndForced bool      `json:"end_forced,omitempty"`
}

// promised reports whether the comment last said that a review is to come
// or under way.
func (r statusRecord) promised() bool {
	return r.Stage == statusScheduled || r.Stage == statusInProgress
}

// statusView is what a status comment says.
type statusView struct {
	stage string
	kind  string    // statusReview, statusRereview or statusAnother
	head  string    // the commit it is about
	wait  string    // statusScheduled: why the review waits (statusWaitWords; "" = not known)
	since time.Time // statusInProgress: when the round started (zero = not known)
	link  string    // statusPosted: the review's link ("" = not known)
	at    time.Time // statusPosted: when the review posted (zero = not known)
	// verdict is statusPosted's review's verdict in a few words
	// (store.ReviewSummary.VerdictWords; "" = not known).
	verdict string
	// asked: someone asked for the review (a review request, `magnum
	// review`). Only a draft's review to come or under way, which opens a
	// comment only when asked, and a declined request read it.
	asked bool
	// earlier and earlierLink are the latest earlier review's head and link
	// a re-review shows ("" = none known), open the earlier findings that
	// review recorded still open (0 = none or not known).
	earlier, earlierLink string
	open                 int
	// cycle and cycles are statusFixing's fix cycle and the loop's cap;
	// fixes marks the review of what a fix loop's fixer pushed.
	cycle, cycles int
	fixes         bool
	// ends: the comment's last edit, for a PR closed, merged or turned into
	// a draft (statusRecord.End).
	ends bool
	// fullPass: the round under way is a scheduled full pass (fullpass.go).
	fullPass bool
	// held: the PR's last review held its approval, and a full pass of the
	// whole PR follows before the approval (approval_hold.go): the posted
	// line says so, and the full pass is no review someone asked for.
	held bool
	// approval is statusPosted's line under the posted one: why the clean
	// review brought no approval as the auto_approve_as login (approvalLine;
	// "" = nothing to say).
	approval string
}

// opens reports whether the stage posts a comment where none stands: a
// review to come or under way (a draft's only when someone asked for it),
// and the answer to a declined review request. The others only edit one.
func (v statusView) opens() bool {
	return v.stage == statusScheduled || v.stage == statusInProgress || v.stage == statusRequestDeclined
}

// what is the view's subject for an event: "review of a1b2c3d".
func (v statusView) what() string { return v.kind + " of " + textx.ShortSHA(v.head) }

// state fingerprints what moves the header's time: the stage, the review,
// its wait and the approval line.
func (v statusView) state() string {
	return statusSum(strings.Join([]string{v.stage, v.kind, v.head, v.wait, v.approval}, "\x00"))
}

// label leads the checklist: the review to come, in plain words.
func (v statusView) label() string {
	switch {
	case v.fixes:
		return "Reviewing the fixes in"
	case v.kind == statusAnother && v.held:
		return "Full pass before approval of"
	case v.kind == statusRereview:
		return "Re-review queued for"
	case v.kind == statusAnother:
		return "Another review requested for"
	}
	return "Review queued for"
}

// postedLine is the posted review in one line: "Review of `a1b2c3d` posted
// 18:52 UTC: Fix 1 problem before merging, 4 optional. <link>" (the verdict
// words left out when not known), and for a review that held its approval
// ". A full pass follows before approval." after it.
func (v statusView) postedLine(now time.Time) string {
	line := "Review of `" + textx.ShortSHA(v.head) + "` posted"
	if !v.at.IsZero() {
		line += " " + statusClock(v.at, now)
	}
	var parts []string
	if v.verdict != "" {
		parts = append(parts, v.verdict+".")
	}
	if v.link != "" {
		parts = append(parts, v.link)
	}
	if len(parts) == 0 {
		line += "."
	} else {
		line += ": " + strings.Join(parts, " ")
	}
	if v.held {
		if v.link != "" {
			line += "."
		}
		line += " A full pass follows before approval."
	}
	return line
}

// body is the comment below its header: the checklist (ticked down to the
// stage) for a review to come or under way, with the earlier review a
// re-review follows, the two fixed sentences and the operator's note; one
// line for a posted review (and the approval line under it) and for a last
// edit; one line and the note when magnum will not review the head, failed
// to, or answers a review request it declines (no reason named: a mute's or
// a filter's would tell a public repository who is reviewed).
func (v statusView) body(note string, now time.Time) string {
	sha := "`" + textx.ShortSHA(v.head) + "`"
	var lines, tail []string
	switch v.stage {
	case statusNotReviewing:
		lines, tail = []string{"Magnum will not review this head. The operator was told."}, []string{note}
	case statusFailed:
		lines = []string{"Magnum failed to finish the review of " + sha + ". The fault is on its side, not in your PR. " +
			"The operator was told. A new push starts a new review."}
		tail = []string{note}
	case statusRequestDeclined:
		lines, tail = []string{"Magnum is not reviewing this PR. The operator was told."}, []string{note}
	case statusMerged:
		lines = []string{"Merged before the review of " + sha + " posted."}
	case statusClosed:
		lines = []string{"Closed before the review of " + sha + " posted."}
	case statusDraft:
		lines = []string{"Not reviewing: the PR is a draft."}
	case statusPosted:
		lines = []string{v.postedLine(now)}
		if v.approval != "" {
			lines = append(lines, v.approval)
		}
	case statusFixing:
		lines = []string{"- [x] " + v.postedLine(now), fmt.Sprintf("- [ ] Applying automatic fixes, cycle %d of %d", v.cycle, v.cycles)}
		tail = []string{statusUpdates, note}
	default:
		first, progress := "- [x] "+v.label()+" "+sha, "- [ ] In progress"
		if v.stage == statusScheduled && v.wait != "" {
			first += ": " + v.wait
		}
		if v.stage == statusInProgress {
			progress = "- [x] In progress"
			if v.fullPass {
				progress += " (full pass)"
			}
			if !v.since.IsZero() {
				progress += " since " + statusClock(v.since, now)
			}
		}
		lines = []string{first, progress, "- [ ] Posted"}
		if v.earlierLink != "" {
			lines = append(lines, "", "Earlier review of `"+textx.ShortSHA(v.earlier)+"`: "+v.earlierLink)
			if v.open > 0 {
				lines = append(lines, "The last review recorded "+textx.Count(v.open, "earlier finding", "earlier findings")+" still open.")
			}
		}
		tail = []string{statusUpdates, statusMergeSentence, note}
	}
	tail = slices.DeleteFunc(tail, func(s string) bool { return strings.TrimSpace(s) == "" })
	for i := range tail {
		tail[i] = strings.TrimSpace(tail[i])
	}
	out := strings.Join(lines, "\n")
	if len(tail) > 0 {
		out += "\n\n" + strings.Join(tail, "\n")
	}
	return out
}

// statusText is the whole comment: the marker, the header with the time the
// stage or the wait last changed, and body.
func statusText(body string, changed time.Time) string {
	return statusMarker + "\n**Magnum review status** (status changed " + statusDay(changed) + ")\n" + body
}

// statusSum fingerprints a comment's body, which a write compares with the
// last one's (statusRecord.Sum).
func statusSum(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:8])
}

// statusDay is t in UTC with its day: "14 Oct, 18:43 UTC".
func statusDay(t time.Time) string { return t.UTC().Format("2 Jan, 15:04") + " UTC" }

// statusClock is t in UTC as a status comment writes it: "12:34 UTC" on
// now's UTC day, else with the day (statusDay).
func statusClock(t, now time.Time) string {
	t, now = t.UTC(), now.UTC()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04") + " UTC"
	}
	return statusDay(t)
}

// statusWaitWords is why a PR's review waits, as its status comment says
// it, in fixed words: "other reviews are running" for the slots and the
// capacity, the earliest start of a timing rule's wait ("not before 12:45
// UTC (the push quiet period)", never a promise of the start, which
// capacity can delay), and a fixed phrase for the others, whose own words
// can carry an error (a branch's name in a failed fetch), a path, an
// identity or a pool. A time is the wait's end in UTC, rounded up to the
// minute, as on the screens.
func statusWaitWords(w Wait, now time.Time) string {
	var rule, s string
	switch w.Reason {
	case WaitNext:
		return "starts shortly"
	case WaitSlot, WaitCapacity:
		return "other reviews are running"
	case WaitRequested:
		if !w.Until.After(now) {
			return "starts shortly"
		}
		rule = "a short delay after the review request"
	case WaitQuiet:
		rule = "the push quiet period"
	case WaitBurst:
		rule = "the quiet period after a burst of pushes"
	case WaitInterval, WaitOwnInterval:
		rule = "the re-review interval"
	case WaitDraftInterval:
		rule = "the draft re-review interval"
	case WaitSnoozed:
		rule = "a snooze"
	case WaitCap:
		rule = "the daily round cap"
	case WaitDelta:
		rule = "a small change since the last review"
	case WaitQuietHours:
		rule = "quiet hours"
	case WaitRetry:
		s = "a retry after a failed attempt"
	default:
		s = "held on Magnum's side"
	}
	until := ""
	if w.Until.After(now) {
		u := w.Until
		if m := u.Truncate(time.Minute); m.Before(u) {
			u = m.Add(time.Minute)
		}
		until = statusClock(u, now)
	}
	switch {
	case rule != "" && until != "":
		return "not before " + until + " (" + rule + ")"
	case rule != "":
		return "waits for " + rule
	case until != "":
		return s + ", not before " + until
	}
	return s
}

// statusCommentsOn reports whether any watch has the status comment on.
func (e *Engine) statusCommentsOn() bool {
	if e.cfg.StatusCommentFor(nil) {
		return true
	}
	for i := range e.cfg.Watches {
		if e.cfg.StatusCommentFor(&e.cfg.Watches[i]) {
			return true
		}
	}
	return false
}

// noteStatusComments brings the status comment of every PR whose watch has
// it on in line with the registry (statusFor), once per tick after
// noteWaits recorded the waits: a PR without one gets it once its review is
// queued or under way, one with one has it edited when its text changed,
// at most once every statusEvery (a last edit at once), and a tick writes
// at most statusWritesPerTick. Never in a dry run nor on a scratch registry
// (an eval replay). A failure is the PR's alone: the tick and the rounds go
// on. A baseline PR is read only when a review request on it was declined
// (declinedRequests): that comment answers the request.
func (e *Engine) noteStatusComments(ctx context.Context) {
	if e.d.DryRun || e.d.Layout.Scratch != "" || !e.statusCommentsOn() {
		return
	}
	now := e.now()
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: statusStates})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "status comments: PRs")
		return
	}
	released, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRReleased}, UpdatedSince: now.Add(-statusReleasedWithin)})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "status comments: released PRs")
		return
	}
	prs = append(prs, released...)
	flagged, err := e.st.CodexFlaggedPRs(ctx)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "status comments: Codex flags")
		return
	}
	declined, err := e.declinedRequests(ctx)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "status comments: declined requests")
		return
	}
	prs = e.withDeclinedBaselines(ctx, prs, declined)
	var reviewed []int64
	for _, pr := range prs {
		if deref(pr.LastReviewID) != 0 {
			reviewed = append(reviewed, pr.ID)
		}
	}
	sums, err := e.st.LastReviewSummaries(ctx, reviewed)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "status comments: review summaries")
		return
	}
	refusals, holds, err := e.autoApprovalReads(ctx, reviewed)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "status comments: auto-approval")
		return
	}
	repos := map[int64]store.Repo{}
	writes := 0
	for _, pr := range prs {
		if writes >= statusWritesPerTick || ctx.Err() != nil {
			return
		}
		repo, ok := repos[pr.RepoID]
		if !ok {
			if repo, err = e.st.RepoByID(ctx, pr.RepoID); err != nil {
				e.warnUnlessStopped(ctx, err, "status comments: repository", "pr", pr.ID)
				continue
			}
			repos[pr.RepoID] = repo
		}
		if w := e.cfg.WatchFor(repo.FullName()); w == nil || !e.cfg.StatusCommentFor(w) {
			continue
		}
		_, unwritten := e.unwrittenFlag(pr.ID)
		reads := statusReads{flagged: flagged[pr.ID] || unwritten, last: sums[pr.ID], declined: declined[pr.ID]}
		if id := e.cfg.AutoApproveFor(repo.FullName()); id != nil {
			reads.approval = approvalReads{login: id.Login, refused: refusals[pr.ID], hold: holdOf(holds, pr.ID)}
		}
		if e.writeStatus(ctx, repo, pr, reads, now) {
			writes++
		}
	}
}

// statusReads is what noteStatusComments reads once a tick for all PRs and
// hands each PR's comment: whether the PR holds a Codex flag, the summary
// of its last posted review (store.LastReviewSummaries; zero = none), the
// time of the review request on it its mute or the filters declined
// (kvPRRequestDeclined; zero = none), and what auto-approval recorded of
// it where its repository is auto-approved (zero = it is not).
type statusReads struct {
	flagged  bool
	last     store.ReviewSummary
	declined time.Time
	approval approvalReads
}

// approvalReads is what approvalLine decides from: the auto_approve_as
// login of the PR's repository ("" = none), and the PR's refusal record
// (KVPRAutoApproveRefused; nil = none) and auto-approval hold (nil = none).
type approvalReads struct {
	login   string
	refused *AutoApproveRefused
	hold    *store.AutoApproveHold
}

// autoApprovalReads reads, in one query each, the refusal records of every
// PR (KVPRAutoApproveRefused) and the auto-approval holds of prIDs, the PRs
// with a review: nothing when no watch auto-approves.
func (e *Engine) autoApprovalReads(ctx context.Context, prIDs []int64) (map[int64]*AutoApproveRefused, map[int64]store.AutoApproveHold, error) {
	if !e.cfg.AutoApproves() {
		return nil, nil, nil
	}
	rows, err := e.st.KVValues(ctx, "pr.*.auto_approve_refused")
	if err != nil {
		return nil, nil, err
	}
	refusals := map[int64]*AutoApproveRefused{}
	for key, v := range rows {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.auto_approve_refused", &id); err != nil || KVPRAutoApproveRefused(id) != key {
			continue
		}
		if r, ok := ParseAutoApproveRefused(v); ok {
			refusals[id] = &r
		}
	}
	holds, err := e.st.AutoApproveHolds(ctx, prIDs)
	if err != nil {
		return nil, nil, err
	}
	return refusals, holds, nil
}

// approvalLine is the line under a posted review of pr (last: its summary)
// on why it brought no approval as a.login, in fixed words (publicText);
// "" when there is none to say: no login, a PR GitHub closed or that is
// the login's own, a review after magnum's round (a manual verdict), an
// approval of the head by the login on GitHub (GitHub's latest reviews),
// and a refusal record of another head or round or without a class. A stop
// of auto-approval of the PR wins over the record, unless the round left
// something to fix or cannot tell.
func approvalLine(pr store.PR, last store.ReviewSummary, a approvalReads) string {
	switch {
	case a.login == "", pr.GHState != store.GHOpen,
		github.SameAccount(github.Account(deref(pr.AuthorLogin), deref(pr.AuthorType)), a.login),
		last.ReviewID == 0, last.ReviewID != deref(pr.LastReviewID), loginApproved(pr, a.login):
		return ""
	}
	if a.hold != nil && a.hold.Held {
		if blocks, known := roundBlocks(last, ""); blocks || !known {
			return ""
		}
		return publicText(publicStopped, a.login)
	}
	if r := a.refused; r != nil && r.Head == pr.HeadSHA && r.RunID == last.RunID {
		return publicText(publicRefusal(r.Public), a.login)
	}
	return ""
}

// loginApproved reports whether GitHub's latest reviews of pr (as the poller
// last read them) hold login's approval of its head.
func loginApproved(pr store.PR, login string) bool {
	return slices.ContainsFunc(pr.LatestReviews, func(r store.LatestReview) bool {
		return github.SameAccount(r.Login, login) && strings.EqualFold(r.State, "APPROVED") && r.CommitSHA == pr.HeadSHA
	})
}

// publicText is what the status comment says of a refusal of class p as
// login (plain text: an @ would notify them at every edit), in the third
// person; "" for a class it has no words for, which says nothing.
func publicText(p publicRefusal, login string) string {
	switch p {
	case publicWorkflows, publicAgentConfig, publicTooManyFiles:
		return "Not approved as " + login + ": " + publicReason(p, login) + "."
	case publicUnheard:
		return "Not approved as " + login + ": " + publicReason(p, login) + ". The next review, or a review by " + login + ", decides."
	case publicCIRuns:
		return "Approval as " + login + " waits for the checks of this head to pass."
	case publicCIFails:
		return "Approval as " + login + " waits for the checks of this head to pass. Some fail now."
	case publicStopped:
		return "Magnum stopped approving this PR as " + login + ". A review by " + login + " decides."
	case publicPostFailed:
		return "The approval as " + login + " did not post. Magnum told " + login + "."
	}
	return ""
}

// publicReason is why a refusal of class p holds back the approval as
// login, as one clause: the status comment's line after "Not approved as
// <login>:" (publicText), and a withdrawal's dismissal message
// (standingWithdrawal); "" for a class a withdrawal never has.
func publicReason(p publicRefusal, login string) string {
	inPerson := ", so " + login + " must review and approve it in person"
	switch p {
	case publicWorkflows:
		return "the PR changes its own CI workflows or actions" + inPerson
	case publicAgentConfig:
		return "the PR changes the instructions or configuration of the AI review agents" + inPerson
	case publicTooManyFiles:
		return "the PR changes more files than Magnum can check" + inPerson
	case publicUnheard:
		return "one of Magnum's reviewers did not finish on this head, and Magnum approves only a complete review"
	case publicCIFails:
		return "some checks of this head fail, and Magnum approves as " + login + " only once they pass"
	}
	return ""
}

// declinedRequests reads every PR's declined review request
// (kvPRRequestDeclined) in one query: the PR's id and the request's time.
func (e *Engine) declinedRequests(ctx context.Context) (map[int64]time.Time, error) {
	rows, err := e.st.KVValues(ctx, "pr.*.request_declined")
	if err != nil {
		return nil, err
	}
	out := map[int64]time.Time{}
	for key, v := range rows {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.request_declined", &id); err != nil || kvPRRequestDeclined(id) != key {
			continue
		}
		if at, err := store.ParseTime(v); err == nil {
			out[id] = at
		}
	}
	return out, nil
}

// withDeclinedBaselines adds to prs the baseline PRs a declined review
// request names (statusStates leaves baseline PRs out: they are every PR
// open at a repository's first sync), and drops the declined request of a
// PR released since, which no comment answers any more.
func (e *Engine) withDeclinedBaselines(ctx context.Context, prs []store.PR, declined map[int64]time.Time) []store.PR {
	listed := map[int64]bool{}
	for _, pr := range prs {
		listed[pr.ID] = true
	}
	for _, id := range slices.Sorted(maps.Keys(declined)) {
		if listed[id] {
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			e.delKV(ctx, kvPRRequestDeclined(id))
		case err != nil:
			e.warnUnlessStopped(ctx, err, "status comments: PR of a declined request", "pr", id)
		case pr.State == store.PRBaseline:
			prs = append(prs, pr)
		case pr.State == store.PRReleased:
			e.delKV(ctx, kvPRRequestDeclined(id))
		}
	}
	return prs
}

// statusRecordOf is the PR's statusRecord (zero: no comment yet).
func (e *Engine) statusRecordOf(ctx context.Context, prID int64) statusRecord {
	var rec statusRecord
	if v, ok := e.getKV(ctx, kvPRStatusComment(prID)); ok {
		_ = json.Unmarshal([]byte(v), &rec)
	}
	return rec
}

// saveStatus records rec as the PR's statusRecord.
func (e *Engine) saveStatus(ctx context.Context, prID int64, rec statusRecord) {
	if b, err := json.Marshal(rec); err == nil {
		e.setKV(ctx, kvPRStatusComment(prID), string(b))
	}
}

// endStatus marks rec ended on pr's head: its last edit stands.
func endStatus(rec *statusRecord, pr store.PR, now time.Time) {
	rec.End, rec.EndAt, rec.EndForced = pr.HeadSHA, now, pr.Forced
}

// statusResumes reports whether a comment that ended may be written again:
// the PR has a new head, a review request came after the end, it was forced
// since, GitHub has it open again, or it is no draft any more.
func (e *Engine) statusResumes(ctx context.Context, pr store.PR, rec statusRecord) bool {
	switch {
	case pr.HeadSHA != rec.End, pr.Forced && !rec.EndForced:
		return true
	case rec.Stage == statusDraft:
		if !pr.IsDraft {
			return true
		}
	case pr.GHState == store.GHOpen:
		return true
	}
	at, ok := e.kvTime(ctx, KVPRRequestAt(pr.ID))
	return ok && at.After(rec.EndAt)
}

// writeStatus posts or edits pr's status comment (reads: what the tick read
// for it) when what it says changed and its last write is statusEvery old
// (a last edit goes at once, unless its attempt just failed), and reports
// whether it called GitHub. A comment that ended is left alone until
// statusResumes. A draft's comment opens only for a review someone asked
// for. An edit that finds the comment gone posts it once more, the first
// time a person deleted it (a last edit posts nothing); after a second
// deletion the PR gets no comment until another head.
func (e *Engine) writeStatus(ctx context.Context, repo store.Repo, pr store.PR, reads statusReads, now time.Time) bool {
	ctx = github.WithOp(ctx, opStatusComment) // github_cost.go
	rec := e.statusRecordOf(ctx, pr.ID)
	if rec.End != "" {
		if !e.statusResumes(ctx, pr, rec) {
			return false
		}
		rec.End, rec.EndAt, rec.EndForced = "", time.Time{}, false
	}
	v, ok := e.statusFor(ctx, pr, rec, reads, now)
	if !ok {
		return false
	}
	if rec.Off != "" {
		if rec.Off == pr.HeadSHA {
			return false
		}
		rec.Off = "" // a new head: someone may want it now
	}
	if rec.ID == 0 && (!v.opens() || (pr.IsDraft && !v.asked)) {
		return false
	}
	if v.stage == statusPosted && rec.Head == v.head && rec.Kind != "" {
		v.kind = rec.Kind // the review the comment followed: a re-review stays one
	}
	body := v.body(e.cfg.Pipeline.StatusCommentNote, now)
	sum := statusSum(body)
	if rec.ID != 0 && sum == rec.Sum {
		if v.ends { // the comment says it already: it ends as it stands
			endStatus(&rec, pr, now)
			e.saveStatus(ctx, pr.ID, rec)
		}
		return false
	}
	if !rec.At.IsZero() && now.Sub(rec.At) < statusEvery && (!v.ends || rec.Failed) {
		return false
	}
	rec.At = now
	state, changed := v.state(), rec.Changed
	if state != rec.State || changed.IsZero() {
		changed = now
	}
	text := statusText(body, changed)
	ident := pr.Identity
	if rec.ID != 0 {
		ident = cmp.Or(rec.Identity, pr.Identity)
	}
	var c github.IssueComment
	err := e.statusCall(ident, func(gh GitHub) (err error) {
		if rec.ID == 0 {
			c, err = gh.CreateIssueComment(ctx, repo.Owner, repo.Name, pr.Number, text)
			return err
		}
		c, err = gh.EditIssueComment(ctx, repo.Owner, repo.Name, rec.ID, text)
		return err
	})
	posted := rec.ID == 0
	if rec.ID != 0 && errors.Is(err, github.ErrNotFound) {
		rec.Gone++
		rec.ID, rec.Sum, rec.Stage, rec.Failed = 0, "", "", false
		switch {
		case v.ends: // a last edit of a comment someone deleted: nothing to post
			endStatus(&rec, pr, now)
			e.saveStatus(ctx, pr.ID, rec)
			e.log.Info("status comment deleted before its last edit", "pr", pr.ID)
			return true
		case rec.Gone > 1:
			rec.Off = pr.HeadSHA
			e.saveStatus(ctx, pr.ID, rec)
			e.log.Info("status comment deleted again: none until the next push", "pr", pr.ID, "head", textx.ShortSHA(pr.HeadSHA))
			return true
		}
		e.log.Info("status comment deleted: posting it once more", "pr", pr.ID)
		posted, ident = true, pr.Identity
		err = e.statusCall(ident, func(gh GitHub) (err error) {
			c, err = gh.CreateIssueComment(ctx, repo.Owner, repo.Name, pr.Number, text)
			return err
		})
	}
	if err != nil {
		if isStop(ctx, err) {
			return true // the stop is no failure; the next start writes it
		}
		rec.Failed = true
		e.statusFailed(ctx, repo, pr, &rec, v, posted, err, now)
		e.saveStatus(ctx, pr.ID, rec)
		return true
	}
	if c.ID != 0 {
		rec.ID = c.ID
	}
	rec.Identity, rec.Sum, rec.Stage, rec.Head, rec.Kind = ident, sum, v.stage, v.head, v.kind
	rec.Failed, rec.State, rec.Changed = false, state, changed
	if v.ends {
		endStatus(&rec, pr, now)
	}
	e.saveStatus(ctx, pr.ID, rec)
	verb := "updated"
	if posted {
		verb = "posted"
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.status_comment",
		fmt.Sprintf("status comment %s: %s (%s)", verb, strings.ReplaceAll(v.stage, "_", " "), v.what()),
		map[string]any{"stage": v.stage, "comment_id": rec.ID, "url": c.URL, "head_sha": v.head, "identity": ident, "posted": posted})
	return true
}

// statusCall runs call with ident's GitHub client; an identity without one
// is a failure as GitHub's would be.
func (e *Engine) statusCall(ident string, call func(GitHub) error) error {
	gh := e.gh(ident)
	if gh == nil {
		return fmt.Errorf("identity %q has no GitHub client", ident)
	}
	return call(gh)
}

// statusFailed records a write GitHub refused: a pr.status_comment_failed
// warning, at most one per PR every statusWarnEvery.
func (e *Engine) statusFailed(ctx context.Context, repo store.Repo, pr store.PR, rec *statusRecord, v statusView, posted bool, err error, now time.Time) {
	if !rec.Warned.IsZero() && now.Sub(rec.Warned) < statusWarnEvery {
		e.log.Debug("status comment failed again", "pr", pr.ID, "err", err)
		return
	}
	rec.Warned = now
	verb := "not updated"
	if posted {
		verb = "not posted"
	}
	e.event(ctx, "warn", prSubject(repo, pr.Number), "pr.status_comment_failed",
		fmt.Sprintf("status comment %s (%s): %v; the review goes on, a later tick tries again", verb, strings.ReplaceAll(v.stage, "_", " "), err),
		map[string]any{"stage": v.stage, "comment_id": rec.ID, "head_sha": v.head})
}

// statusFor is what pr's status comment says now (rec: what it said last;
// reads: what the tick read for it); ok is false when it has nothing to say
// of the PR: a post-merge review, a dry run, a claimed round not started
// yet, a draft that never had one, and a PR left out of review for a reason
// of its filters that nobody asked to review. A PR GitHub closed or merged
// gets its last edit (the review posted, or "closed" or "merged before the
// review"), a review request its mute or its filters declined gets the line
// that magnum is not reviewing the PR, a draft whose comment promised a
// review gets "not reviewing", and a round on the reviewed head leaves it
// posted unless someone asked for it (anotherReview), and so does a held
// review's full pass while it waits (approvalPassDue: the posted line says
// that it follows). A park whose cause is on magnum's side says that magnum
// failed (parkedOnOurSide). It reads the registry only for a forced PR's dry
// run, a declined request, a parked PR's cause, a waiting PR's wait, a
// draft's request, whether a round under way is a full pass, a round on the
// reviewed head and a held review: the tick asks it of every PR.
func (e *Engine) statusFor(ctx context.Context, pr store.PR, rec statusRecord, reads statusReads, now time.Time) (statusView, bool) {
	reviewed := deref(pr.ReviewedSHA)
	v := statusView{kind: statusReview, head: pr.HeadSHA}
	if reviewed != "" {
		v.kind, v.earlier, v.earlierLink = statusRereview, reviewed, reviewLink(pr)
	}
	switch {
	case pr.HeadSHA == "":
		return v, false
	case slices.Contains(statusEndStates, pr.State) && pr.GHState != store.GHOpen:
		v = statusEnding(pr, rec, v)
		if v.stage == statusPosted {
			v.verdict = postedVerdict(pr, reads.last)
		}
		return v, true
	case postMerge(pr) || pr.GHState == store.GHClosed:
		return v, false // a post-merge review, or a round GitHub closed the PR under: its close ends it
	case pr.Forced && e.hasDryRun(ctx, pr.ID):
		return v, false
	case e.requestDeclined(ctx, pr, reads.declined):
		v.stage, v.asked = statusRequestDeclined, true
		return v, true
	case pr.IsDraft && !pr.Forced && pr.HeadSHA != reviewed && !slices.Contains(statusRoundStates, pr.State) &&
		pr.State != store.PRQueued && pr.State != store.PRRereviewPending:
		if !rec.promised() {
			return v, false
		}
		v.stage, v.ends = statusDraft, true
		return v, true
	case pr.HeadSHA == reviewed:
		// A held review's full pass is magnum's own: while it waits, the
		// comment stays at the posted line, which says that it follows.
		held := e.approvalPassDue(ctx, pr)
		if (held && !slices.Contains(statusRoundStates, pr.State)) || !e.anotherReview(ctx, pr, rec) {
			v.stage, v.kind, v.link = statusPosted, statusReview, reviewLink(pr)
			v.at, v.held, v.verdict = deref(pr.ReviewedAt), held, postedVerdict(pr, reads.last)
			if st, ok := e.statusFix(ctx, pr.ID); ok && st.Phase == FixPhaseFixing {
				v.stage, v.cycle, v.cycles = statusFixing, st.Cycle, st.Cap
			} else if !held {
				v.approval = approvalLine(pr, reads.last, reads.approval) // the posted line says that a held one's full pass follows
			}
			return v, true
		}
		v.kind, v.held = statusAnother, held
	}
	switch {
	case pr.State == store.PRNeedsAttention:
		v.stage = statusNotReviewing
		if !pr.Muted && e.parkedOnOurSide(ctx, pr) {
			v.stage = statusFailed
		}
		return v, true
	case pr.State == store.PRIneligible:
		if pr.Muted || reads.flagged {
			v.stage = statusNotReviewing
			return v, true
		}
		return v, false
	case pr.Muted && !pr.Forced:
		v.stage = statusNotReviewing
		return v, true
	case pr.State == store.PRQueued || pr.State == store.PRRereviewPending:
		v.stage = statusScheduled
		raw, _ := e.getKV(ctx, KVPRWait(pr.ID))
		if w, ok := ParseWait(raw); ok {
			v.wait = statusWaitWords(w, now)
		}
		e.statusEarlier(ctx, pr, &v, reads.last)
		v.asked = pr.IsDraft && e.reviewAsked(ctx, pr)
		return v, true
	case slices.Contains(statusRoundStates, pr.State):
		v.stage = statusInProgress
		if pr.LastRoundStartedAt != nil {
			v.since = *pr.LastRoundStartedAt
		}
		v.fullPass = e.fullPassRunning(ctx, pr.ID)
		e.statusEarlier(ctx, pr, &v, reads.last)
		v.asked = pr.IsDraft && e.reviewAsked(ctx, pr)
		return v, true
	}
	return v, false
}

// statusEarlier adds to a review to come or under way what the review
// before it left (last: the PR's last review's summary): the earlier
// findings it recorded still open (earlierOpen), and whether it reviews
// what a fix loop's fixer pushed (a loop held for a restart had that
// re-review: the next round is another).
func (e *Engine) statusEarlier(ctx context.Context, pr store.PR, v *statusView, last store.ReviewSummary) {
	v.open = earlierOpen(pr, *v, last)
	if st, ok := e.statusFix(ctx, pr.ID); ok && st.Phase == FixPhaseReviewing && st.Head == pr.HeadSHA && !st.Held {
		v.fixes = true
	}
}

// statusFix is the PR's fix loop (FixState), when one can be read (dispatch
// warns of one that cannot, fixHolds).
func (e *Engine) statusFix(ctx context.Context, prID int64) (FixState, bool) {
	st, ok, err := e.fixState(ctx, prID)
	if err != nil {
		e.log.Debug("status comment: the fix loop cannot be read", "pr", prID, "err", err)
		return FixState{}, false
	}
	return st, ok
}

// statusEnding is the last edit of the comment of pr, which GitHub closed
// or merged: the review of its head posted, the line magnum will not review
// it or is not reviewing the PR (still true), else "merged" or "closed
// before the review" (also after magnum's own failure, whose new push will
// not come).
func statusEnding(pr store.PR, rec statusRecord, v statusView) statusView {
	v.ends, v.earlierLink = true, ""
	switch {
	case pr.HeadSHA == deref(pr.ReviewedSHA):
		v.stage, v.kind, v.link, v.at = statusPosted, statusReview, reviewLink(pr), deref(pr.ReviewedAt)
	case rec.Stage == statusNotReviewing, rec.Stage == statusRequestDeclined:
		v.stage = rec.Stage
	case postMerge(pr):
		v.stage = statusMerged
	default:
		v.stage = statusClosed
	}
	return v
}

// postedVerdict is the verdict words of pr's last review
// (store.ReviewSummary.VerdictWords) when last is that review's summary;
// "" when they are not known: a review no round of magnum's posted (a
// manual verdict), or a result that did not parse.
func postedVerdict(pr store.PR, last store.ReviewSummary) string {
	if id := deref(pr.LastReviewID); id == 0 || last.ReviewID != id {
		return ""
	}
	return last.VerdictWords()
}

// parkedOnOurSide reports whether the cause that parked pr in
// needs_attention is on magnum's side, not in the PR
// (attention.OnMagnumsSide): the kind the engine recorded with the park
// (KVPRAttention), else, for an older row, the one attention.Explain reads
// from the stored error.
func (e *Engine) parkedOnOurSide(ctx context.Context, pr store.PR) bool {
	kind, _ := e.getKV(ctx, KVPRAttention(pr.ID))
	if kind == "" {
		kind = attention.Explain("", deref(pr.LastError), "").Kind
	}
	return attention.OnMagnumsSide(kind)
}

// requestDeclined reports whether the review request onRequest declined at
// at (kvPRRequestDeclined: it came while the PR's mute or the watch's
// filters kept it out of review; zero = none) is still the PR's newest one
// and unanswered (pendingRequest), and nothing reviews the PR yet: it is
// not forced and neither waits for a round nor runs one, so an unmute or a
// change of the filters that queued it hands the comment back to its
// review.
func (e *Engine) requestDeclined(ctx context.Context, pr store.PR, at time.Time) bool {
	if at.IsZero() || pr.Forced || pr.State == store.PRClaiming || slices.Contains(waitingStates, pr.State) ||
		slices.Contains(statusRoundStates, pr.State) {
		return false
	}
	req, ok := e.pendingRequest(ctx, pr)
	return ok && req.At.Equal(at)
}

// reviewAsked reports whether someone asked for pr's review to come or
// under way: `magnum review` (forced), a review request no round served
// yet, or the running round one of them started (roundAsked).
func (e *Engine) reviewAsked(ctx context.Context, pr store.PR) bool {
	if pr.Forced {
		return true
	}
	if _, ok := e.pendingRequest(ctx, pr); ok {
		return true
	}
	return slices.Contains(statusRoundStates, pr.State) && e.roundAsked(pr.ID)
}

// reviewLink is the link of pr's last review ("" = not known).
func reviewLink(pr store.PR) string {
	if id := deref(pr.LastReviewID); id > 0 && pr.URL != "" {
		return fmt.Sprintf("%s#pullrequestreview-%d", pr.URL, id)
	}
	return ""
}

// anotherReview reports whether the round due or running on pr's reviewed
// head is one someone asked for: `magnum review` (forced, unless it asks to
// re-decide the replies) or a review request (pending, or the round it
// started), or one the comment already shows. A reply round, a judge check
// of red CI and a re-decision are not: the comment stays at posted.
func (e *Engine) anotherReview(ctx context.Context, pr store.PR, rec statusRecord) bool {
	waiting := pr.State == store.PRQueued || pr.State == store.PRRereviewPending
	if !waiting && !slices.Contains(statusRoundStates, pr.State) {
		return false
	}
	if v, _ := e.getKV(ctx, kvPRRedecide(pr.ID)); v == "1" {
		return false
	}
	switch {
	case pr.Forced, rec.Kind == statusAnother && rec.Head == pr.HeadSHA && rec.promised():
		return true
	case waiting:
		_, ok := e.pendingRequest(ctx, pr)
		return ok
	}
	return e.roundAsked(pr.ID)
}

// roundAsked reports whether the review round running on the PR is one
// someone asked for (roundHandle.asked).
func (e *Engine) roundAsked(prID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	h, ok := e.rounds[prID]
	return ok && h.asked && !h.open && !h.fix && !h.whiteboard
}

// earlierOpen is how many earlier findings the latest earlier review a
// re-review shows recorded still open (ReviewSummary.Open), as that review
// recorded them (last: the PR's last review's summary), never a live
// count; 0 when there is no such review, last is not its summary, or none
// were open.
func earlierOpen(pr store.PR, v statusView, last store.ReviewSummary) int {
	if v.earlierLink == "" || last.ReviewID == 0 || last.ReviewID != deref(pr.LastReviewID) || last.SHA != v.earlier {
		return 0
	}
	return last.Open
}

// hasDryRun reports whether a dry-run round is asked for the PR
// (store.KVPRDryRun): it posts nothing, and its comment says nothing of it.
func (e *Engine) hasDryRun(ctx context.Context, prID int64) bool {
	_, ok := e.getKV(ctx, store.KVPRDryRun(prID))
	return ok
}
