package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/zhuravel/magnum/internal/textx"
)

// pendingAction is an action waiting for y/N in the footer. The review
// keys, release, abort, ignore, mute, unmute and the verdicts ask first (see
// actionQuestion): a review round runs for minutes and posts to GitHub, so
// a stray key must not start one.
type pendingAction struct {
	question string     // what will happen, ending in "?"
	what     string     // the action's label while it runs ("fresh review talkable#7")
	target   string     // the PR (or slot) it acts on
	fn       actionFunc // runs on y
}

// confirms reports whether k answers yes to a pending action. Only y
// does: enter cancels like any other key, so a stray R followed by a
// habitual enter starts nothing.
func confirms(k string) bool { return k == "y" || k == "Y" }

const (
	confirmYN   = " y/N"
	confirmKeys = "y confirms, any other key cancels"
)

// confirmLine is a pending question in width cells: the question (cut to
// fit, so "y/N" always shows), then the keys when there is room.
func (st styles) confirmLine(question string, width int) string {
	q := question
	if width > 0 {
		q = truncate(question, max(width-len(confirmYN), 1))
	}
	line := st.Warn.Render(q + confirmYN)
	if width <= 0 || ansi.StringWidth(q)+len(confirmYN)+2+len(confirmKeys) <= width {
		line += "  " + st.Dim.Render(confirmKeys)
	}
	return line
}

// reviewQuestion asks before a review round of ref: what the variant
// does, then facts, what is known about the PR (may be empty).
func reviewQuestion(ref string, o ReviewOpts, facts string) string {
	var q string
	switch {
	case o.Fresh:
		q = "Fresh review of " + ref + " in new agent sessions"
	case o.Simplify:
		q = "Simplify review of " + ref
	default:
		q = "Review " + ref + " now"
	}
	if o.Simplify {
		q += ", also running the simplify role"
	}
	if facts != "" {
		q += " (" + facts + ")"
	}
	return q + "?"
}

// postMergeQuestion asks before a post-merge review of ref, a PR GitHub
// merged before magnum reviewed its last push: what the variant does, and
// that the review only comments (there is nothing left to approve or block).
func postMergeQuestion(ref string, o ReviewOpts) string {
	switch {
	case o.Fresh:
		return "Fresh post-merge review of " + ref + " in new agent sessions (comment only)?"
	case o.Simplify:
		return "Post-merge review of " + ref + ", also running the simplify role (comment only)?"
	}
	return "Post-merge review " + ref + " (comment only)?"
}

// releaseQuestion asks before handing back target's slot.
func releaseQuestion(target string) string {
	return "Release " + target + ": hand back its slot now, sessions parked and worktree reset?"
}

// verdictQuestion asks before posting the reviewer's verdict on ref: it
// says on which head and what magnum's review f concluded, so the decision
// is an informed one. A head that moved since is refused before asking
// (actionRefusal): the screens cannot pass --force.
func verdictQuestion(ref string, approve bool, f *FindingsInfo) string {
	verb := "Request changes on"
	if approve {
		verb = "Approve"
	}
	q := verb + " " + ref
	if f == nil {
		return q + "?"
	}
	if f.SHA != "" {
		q += " at " + f.SHA[:min(7, len(f.SHA))]
	}
	var parts []string
	for i, n := range f.Counts {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d P%d", n, i))
		}
	}
	found := "no findings"
	if len(parts) > 0 {
		found = strings.Join(parts, ", ")
	}
	return q + "? magnum found " + found
}

// unmuteIgnoredQuestion asks before unmuting a PR `magnum ignore` muted,
// which undoes the ignore.
func unmuteIgnoredQuestion(ref string) string {
	return "Unmute " + ref + ": stop ignoring it and review it on its next push?"
}

// muteQuestion asks before muting (or unmuting) ref, a PR that is open.
func muteQuestion(ref string, mute bool) string {
	if mute {
		return "Mute " + ref + ": stop automatic reviews of it?"
	}
	return "Unmute " + ref + ": resume automatic reviews of it?"
}

// muteAct is what M means on a PR, given what GitHub says of it.
type muteAct int

const (
	muteAsk     muteAct = iota // an open PR: stop its automatic reviews (muteQuestion)
	muteDismiss                // merged before its last push was reviewed: dismiss that flag
	muteRestore                // flag dismissed by a mute: restore it with an unmute
	muteNothing                // merged or closed with no flag to dismiss or restore
)

// muteActFor says what M does on a PR whose GitHub state is ghState (any
// case; "" = not known, treated as open), flagged merged unreviewed or
// muted with that flag dismissed. A merged or closed PR gets no automatic
// reviews, so asking to stop them is meaningless: muting it means dismissing
// the flag, and state (merged or closed) names it for the flash of
// muteNothing.
func muteActFor(ghState string, flagged, dismissed bool) (act muteAct, state string) {
	state = strings.ToLower(strings.TrimSpace(ghState))
	switch {
	case state != "merged" && state != "closed":
		return muteAsk, ""
	case flagged:
		return muteDismiss, state
	case dismissed:
		return muteRestore, state
	}
	return muteNothing, state
}

// dismissFlagQuestion asks before muting a PR GitHub merged before magnum
// reviewed its last push, which dismisses the merged-unreviewed flag.
func dismissFlagQuestion(ref string) string {
	return "Dismiss the merged-unreviewed flag on " + ref + "? (r still runs a post-merge review)"
}

// restoreFlagQuestion asks before unmuting a merged PR whose flag a mute
// dismissed.
func restoreFlagQuestion(ref string) string {
	return "Restore the merged-unreviewed flag on " + ref + "?"
}

// nothingToMute is the flash for M on a merged or closed PR with no flag to
// dismiss or restore; state is "merged" or "closed".
func nothingToMute(ref, state string) string {
	return ref + " is " + state + ": nothing to mute"
}

// boardReviewFacts says what a review of the board row would look at:
// the commits since the last review ("no new commits" when the head is
// the reviewed one), that review, then the head. The commit count comes
// first so a narrow footer cuts the details, not the news.
func boardReviewFacts(r PRBoardRow, now time.Time) string {
	head := prefixed("head ", textx.ShortSHA(r.HeadSHA))
	lr := r.LastReview
	if lr == nil || (lr.CommitSHA == "" && lr.SubmittedAt.IsZero() && lr.Login == "") {
		return joinFacts("not reviewed yet", head)
	}
	when := ""
	if !lr.SubmittedAt.IsZero() {
		when = " " + HumanAgo(max(now.Sub(lr.SubmittedAt), time.Second))
	}
	if lr.Login != "" {
		when += " by " + lr.Login
	}
	if head != "" && sameSHA(r.HeadSHA, lr.CommitSHA) {
		return "no new commits since " + head + " was reviewed" + when
	}
	last := "the last review" + prefixed(" of ", textx.ShortSHA(lr.CommitSHA)) + when
	d := r.SinceReview
	if d != nil && d.Base != "reviewed" {
		d = nil // a delta from the base branch says nothing about the review
	}
	switch {
	case d != nil && d.Commits <= 0:
		return joinFacts("no new commits since "+last, head)
	case d != nil:
		n := textx.Count(d.Commits, "commit", "commits")
		if d.Truncated {
			n = "at least " + n
		}
		return joinFacts(n+" since "+last, head)
	case head != "" && lr.CommitSHA != "":
		return joinFacts("new commits since "+last, head)
	}
	return joinFacts("last review"+prefixed(" of ", textx.ShortSHA(lr.CommitSHA))+when, head)
}

// ReviewFacts is what the y/N question before a review says about a PR on
// the dashboard and in the picker (the board reads its own rows): the
// head, the last reviewed head and the commits since it.
type ReviewFacts struct {
	HeadSHA     string
	ReviewedSHA string    // the last reviewed head; "" when never reviewed
	ReviewedAt  time.Time // zero when unknown
	ReviewedBy  string    // who posted that review; "" when unknown
	// SinceReview is what changed since ReviewedSHA (Base "reviewed");
	// nil when unknown, e.g. not counted yet for the current head.
	SinceReview *ReviewDelta
}

// reviewFacts says what a review of a PR in state would look at: f's
// commits since the last review when known, else what the state tells.
func reviewFacts(state, next string, f *ReviewFacts, now time.Time) string {
	if f == nil {
		return stateReviewFacts(state, next)
	}
	r := PRBoardRow{HeadSHA: cleanText(f.HeadSHA), SinceReview: f.SinceReview}
	if f.ReviewedSHA != "" || !f.ReviewedAt.IsZero() {
		r.LastReview = &ReviewInfo{Login: cleanText(f.ReviewedBy), SubmittedAt: f.ReviewedAt, CommitSHA: cleanText(f.ReviewedSHA)}
	}
	return boardReviewFacts(r, now)
}

// stateReviewFacts says what magnum's state tells about a review, for the
// dashboard and the picker when they know no heads. state may
// carry the picker's flags ("reviewed,pinned"); next is the dashboard's
// "what happens next", added where the state alone says little.
func stateReviewFacts(state, next string) string {
	s, _, _ := strings.Cut(state, ",")
	var f string
	switch s = normState(s); s {
	case "":
		return next
	case "reviewed":
		return "no new commits since the last review"
	case "rereview_pending":
		f = "reviewed before, re-review pending"
	case "queued":
		f = "not reviewed yet, queued"
	case "baseline":
		f = "not reviewed yet, baseline: waiting for a new push"
	case "new":
		f = "not in magnum yet"
	default:
		f = "state " + s
	}
	return joinFacts(f, prefixed("next: ", next))
}

// sameSHA reports whether a and b name the same commit (either may be
// abbreviated to 7 or more characters).
func sameSHA(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	switch {
	case a == "" || b == "":
		return false
	case len(a) < 7 || len(b) < 7:
		return a == b
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// prefixed is p+s, or "" when s is empty.
func prefixed(p, s string) string {
	if s == "" {
		return ""
	}
	return p + s
}

// joinFacts joins the non-empty facts with commas.
func joinFacts(facts ...string) string {
	var out []string
	for _, f := range facts {
		if f != "" {
			out = append(out, f)
		}
	}
	return strings.Join(out, ", ")
}
