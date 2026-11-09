package factory

// Your move on an open PR that needs you follows the reason magnum's look
// gave, in one place (reasonMove), for your own PRs and for a published
// task's: a fixed "b or f" sent you to the fixer for a check that never
// ran, and to a pair of keys when one step was the answer. The reason is
// read from magnum's registry alone: the fresh verdict that needs you (its
// Needs kinds and its words), the attention and the fixer's report, the
// stalemate. f is offered only where the fixer has work, open findings or
// a failed check, never for a check that did not run, a stalemate or a
// reason nothing here knows (then the PR's page is the move).

import (
	"strings"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// move is your move on a PR that needs you: the step in words and its
// short form, which fits the YOUR MOVE column of a narrow screen.
type move struct{ long, short string }

// The moves, by reason. The keys are those of the PR's card and of home's
// row (b the PR's page, e the fixer's report, f magnum's fixer).
var (
	moveRerunCI  = move{"b opens it: re-run CI on GitHub, or push", "b re-run CI"}
	moveFailedCI = move{"b opens its CI, f fixes it", "b CI · f fix"}
	moveThreads  = move{"b opens the threads", "b threads"}
	moveReport   = move{"e opens the report, b the PR", "e report · b PR"}
	moveFixes    = move{"f fixes, b opens it", "f fix · b PR"}
	moveOpen     = move{"b opens it", "b PR"}
)

// What a need of a verdict that needs you asks, as one move: a class of
// OpenNeed.Kind (needClass).
const (
	needMixed     = "mixed" // needs of more than one class: no single step
	needOther     = "other" // a kind with no step of its own (the PR's page is the move)
	needCIMissing = "ci_missing"
	needCIFailed  = "ci_failed"
	needThreads   = "threads"
	needFix       = "fix"
)

// The words of magnum's engine that tell apart the reasons one kind has
// (internal/engine, fix_open_verdict.go and fix_open_ci.go): "ci" is a
// required check that failed or, until the engine has the kind
// "ci_missing", one that did not run; "magnum" is a review that does not
// stand at the head, or a reply of magnum's that waits for you; "loop" is
// the day's loops used up, or a start the fix job refused.
const (
	didNotRunWords = " did not run at "
	noReviewWords  = "no review of magnum's"
	loopsUsedWords = "f starts another"
)

// reasonMove is your move on open PR st that needs you, by why it does:
// a required check that did not run (re-run it on GitHub, or push), one
// that failed (open its CI, or f), magnum stopped arguing in threads (the
// threads are yours), the fixer left you items (e opens its report), open
// findings the fixer can take (f), else the PR's page. An attention keeps
// its own words and takes the fixer's report when it has one.
func reasonMove(st PRState) move {
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
	case "ci":
		if strings.Contains(why, didNotRunWords) {
			return needCIMissing
		}
		return needCIFailed
	case "thread":
		return needThreads
	case "magnum":
		if strings.HasPrefix(why, noReviewWords) {
			return needOther
		}
		return needThreads
	case "finding", "findings":
		return needFix
	case "loop":
		if strings.Contains(why, loopsUsedWords) {
			return needFix
		}
	}
	return needOther
}
