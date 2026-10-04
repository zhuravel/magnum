package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// pendingAction is an action waiting for y/N in the footer. The review
// keys, release, abort, ignore, mute and unmute ask first: a review round runs for
// minutes and posts to GitHub, so a stray key must not start one.
type pendingAction struct {
	question string     // what will happen, ending in "?"
	what     string     // the action's label while it runs ("fresh review talkable#7")
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
	case o.Again:
		q = "Review " + ref + " again"
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

// releaseQuestion asks before handing back target's slot.
func releaseQuestion(target string) string {
	return "Release " + target + ": hand back its slot now, sessions parked and worktree reset?"
}

// abortQuestion asks before killing ref's running review.
func abortQuestion(ref string) string {
	return "Kill the running review of " + ref + "?"
}

// ignoreQuestion asks before ignoring ref.
func ignoreQuestion(ref string) string {
	return "Ignore " + ref + ": kill its review, mute it and free its slot?"
}

// unmuteIgnoredQuestion asks before unmuting a PR `magnum ignore` muted,
// which undoes the ignore.
func unmuteIgnoredQuestion(ref string) string {
	return "Unmute " + ref + ": stop ignoring it and review it on its next push?"
}

// muteQuestion asks before muting (or unmuting) ref.
func muteQuestion(ref string, mute bool) string {
	if mute {
		return "Mute " + ref + ": stop automatic reviews of it?"
	}
	return "Unmute " + ref + ": resume automatic reviews of it?"
}

// boardReviewFacts says what a review of the board row would look at:
// the commits since the last review ("no new commits" when the head is
// the reviewed one), that review, then the head. The commit count comes
// first so a narrow footer cuts the details, not the news.
func boardReviewFacts(r PRBoardRow, now time.Time) string {
	head := prefixed("head ", shortSHA(r.HeadSHA))
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
	last := "the last review" + prefixed(" of ", shortSHA(lr.CommitSHA)) + when
	d := r.SinceReview
	if d != nil && d.Base != "reviewed" {
		d = nil // a delta from the base branch says nothing about the review
	}
	switch {
	case d != nil && d.Commits <= 0:
		return joinFacts("no new commits since "+last, head)
	case d != nil:
		n := plural(d.Commits, "commit", "commits")
		if d.Truncated {
			n = "at least " + n
		}
		return joinFacts(n+" since "+last, head)
	case head != "" && lr.CommitSHA != "":
		return joinFacts("new commits since "+last, head)
	}
	return joinFacts("last review"+prefixed(" of ", shortSHA(lr.CommitSHA))+when, head)
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
	facts := boardReviewFacts(r, now)
	s, _, _ := strings.Cut(state, ",")
	switch normState(s) {
	case "claiming", "reviewing", "verifying":
		return joinFacts("a round is already in progress", facts)
	}
	return facts
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
	case "claiming", "reviewing", "verifying":
		return "a round is already in progress"
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
