package factory

// Your move on an open PR that needs you follows the reason magnum's look
// gave, in one place (reasonMove), for your own PRs and for a published
// task's: a fixed "b or f" sent you to the fixer for a check that never
// ran, and to a pair of keys when one step was the answer. The reason is
// read from magnum's registry alone: the fresh verdict that needs you (its
// Needs kinds and its words), the attention and the fixer's report, the
// stalemate. f is offered only where the fixer has work, open findings it
// can take: it reads no CI, so a check that did not run or that failed is
// no move for it, nor is a stalemate or a reason nothing here knows (then
// the PR's page is the move).

import (
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// moveWords is your move on a PR that needs you: the step in words and its
// short form, which fits the YOUR MOVE column of a narrow screen.
type moveWords struct{ long, short string }

// The moves, by reason. The keys are those of the PR's card and of home's
// row (b the PR's page, e the fixer's report, f magnum's fixer, r magnum's
// review, which the card's hint bar offers).
var (
	moveRerunCI  = moveWords{"b: re-run CI on GitHub, or push", "b re-run CI"}
	moveFailedCI = moveWords{"b opens its CI", "b CI"}
	moveThreads  = moveWords{"b opens the threads", "b threads"}
	moveReport   = moveWords{"e opens the report, b the PR", "e report · b PR"}
	moveFixes    = moveWords{"f fixes, b opens it", "f fix · b PR"}
	moveReview   = moveWords{"r reviews it", "r review"}
	moveOpen     = moveWords{"b opens it", "b PR"}
)

// What a need of a verdict that needs you asks, as one move: a class of
// OpenNeed.Kind (needClass).
const (
	needMixed     = "mixed" // needs of more than one class: no single step
	needOther     = "other" // a kind with no step of its own (the PR's page is the move)
	needCIMissing = "ci_missing"
	needCIFailed  = "ci_failed"
	needThreads   = "threads"
	needReview    = "review"
	needFix       = "fix"
)

// The words of magnum's engine that tell apart the reasons one kind has
// (internal/engine, fix_open_verdict.go and fix_open_ci.go). Each class
// matches on the words of its own reason and nothing else, so a reword of
// the engine's falls back to the PR's page and never to a wrong key: "ci"
// is a required check that did not run (until the engine has the kind
// "ci_missing") or that failed; "magnum" is a review that does not stand
// at the head, or a reply of magnum's that waits for you; "loop" is the
// day's loops used up (a start the fix job refused says other words).
// TestTheEnginesWordsThatTheMoveReadsAreStillTheEngines keeps them the
// engine's.
const (
	didNotRunWords  = " did not run at "
	failedWords     = " failed at "
	noReviewWords   = "no review of magnum's"
	replyWaitsWords = "reply waits for your answer"
	loopsUsedWords  = "f starts another"
)

// replyWords are the words of a verdict whose need of kind "magnum" is a
// reply of magnum's that waits for you: its sentence, or an item count.
var replyWords = []string{replyWaitsWords, "reply of magnum's", "replies of magnum's"}

// reasonMove is your move on open PR st that needs you, by why it does:
// a required check that did not run (re-run it on GitHub, or push), one
// that failed (open its CI), magnum stopped arguing in threads (the
// threads are yours), the fixer left you items (e opens its report),
// magnum's review late or missing (r reviews it), open findings the fixer
// can take (f), else the PR's page. An attention keeps its own words and
// takes the fixer's report when it has one.
func reasonMove(st PRState) moveWords {
	class := needClass(st)
	attention := st.State == store.PRNeedsAttention
	switch {
	case class == needCIMissing:
		return moveRerunCI
	case class == needCIFailed:
		return moveFailedCI
	case attention && st.FixReport != "":
		return moveReport
	case attention:
		return moveOpen
	case class == needThreads, class == "" && st.Stalemate:
		return moveThreads
	case class == needReview:
		return moveReview
	case class == needFix:
		return moveFixes
	}
	return moveOpen
}

// reasonWait is w, a wait on you that gives no move of its own, with the
// move of its reason (reasonMove): a verdict that needs you, an attention
// or a stalemate. A wait with a move (a merge, an answer) keeps it.
func reasonWait(st PRState, w prWait) prWait {
	if w.move == "" {
		m := reasonMove(st)
		w.move, w.moveShort = m.long, m.short
	}
	return w
}

// needClass is what the needs of st's fresh verdict that needs you ask:
// "" for none (no verdict, a stale one, one that needs nothing of you, or
// without needs), a class when they all ask the same step, else needMixed.
func needClass(st PRState) string {
	if !st.HasOpen || st.Stale || st.Open.Verdict != engine.OpenNeedsYou {
		return ""
	}
	class := ""
	for _, n := range st.Open.Needs {
		c := kindClass(n.Kind, st.Open.Why)
		if class != "" && c != class {
			return needMixed
		}
		class = c
	}
	return class
}

// kindClass is the class of a need of kind in a verdict whose words are
// why.
func kindClass(kind, why string) string {
	switch kind {
	case "ci_missing":
		return needCIMissing
	case "ci": // until the engine has the kind "ci_missing", the words tell
		switch {
		case strings.Contains(why, didNotRunWords):
			return needCIMissing
		case strings.Contains(why, failedWords):
			return needCIFailed
		}
	case "thread":
		return needThreads
	case "magnum":
		switch {
		case strings.HasPrefix(why, noReviewWords):
			return needReview
		case slices.ContainsFunc(replyWords, func(w string) bool { return strings.Contains(why, w) }):
			return needThreads
		}
	case "magnum_late":
		return needReview
	case "finding", "findings":
		return needFix
	case "loop":
		if strings.Contains(why, loopsUsedWords) {
			return needFix
		}
	}
	return needOther
}
