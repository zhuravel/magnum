package engine

// `magnum fix --done` (the board's d): you did what a fix loop left you, its
// Needs you, so the PR goes back from needs_attention to reviewed at the
// head whose review stands, and no round runs. Magnum cannot read every such
// item as done: a finding about the PR's description has no thread, a call
// can be a decision made elsewhere, and the look of autofix = "open" lifts
// only its own raise (fix_open_needs.go). A person's item never closes on
// the fixer's word, so it closes on yours: the lift takes only the fixer's
// calls (attention.FixAsks, after "fix: "; not its cap or a failed run, even
// one that kept your decision calls after its words, keepDecisions, nor a
// round's attention, nor the look's raise, whose refusal names its lift on
// GitHub, attention.LookLift), and only when they are all there is (no line
// under them: a fixer that did not get the other reviewers' items, withUnread,
// may have left more), only the attention you saw (FixPayload.Seen; one you
// did not see, Seen "", is refused), only on the head magnum reviewed, and
// never while a loop runs or a fix is held, whose end could leave you
// something new. An attention the factory raised on a PR it fixes is the
// factory's to lift (factoryAttentionRefusal): --done refuses it.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// fixDoneBy is who lifts a loop's Needs you (the fix.acknowledged event).
const fixDoneBy = "magnum fix --done"

// fixDone serves `magnum fix --done` on pr: it moves the PR from
// needs_attention back to reviewed under the refusals above, with one
// compare-and-set on the head, the reviewed head and the attention's words
// it read, and drops the attention's rows. seen is the attention the
// operator saw, which must be the PR's now ("" = they saw none); a dry run
// answers what it would do.
func (e *Engine) fixDone(ctx context.Context, repo store.Repo, pr store.PR, seen string, dry bool) (string, error) {
	ref, msg := repo.FullName()+"#"+fmt.Sprint(pr.Number), deref(pr.LastError)
	look := "`magnum status " + ref + "` says what"
	if pr.State != store.PRNeedsAttention {
		return "", fmt.Errorf("PR #%d needs nothing of you (%s): `magnum fix --done` answers what a fix loop left you", pr.Number, pr.State)
	}
	if kind, _ := e.getKV(ctx, KVPRAttention(pr.ID)); kind != attention.KindFix {
		return "", fmt.Errorf("PR #%d needs attention for %s, not for what the fixer left you: %s",
			pr.Number, cmp.Or(kind, "a reason magnum did not record"), look)
	}
	if err := e.factoryAttentionRefusal(ctx, ref, pr, msg); err != nil { // fix_factory.go: the factory lifts its own
		return "", err
	}
	_, under, _ := strings.Cut(msg, "\n")
	switch reviewed := deref(pr.ReviewedSHA); {
	case attention.LookRaise(msg): // fix_open_needs.go: the look lifts it once GitHub shows the items answered
		return "", fmt.Errorf("PR #%d needs you for items fix loops left open on GitHub, which `magnum fix --done` does not take: %s",
			pr.Number, attention.LookLift)
	case !attention.FixAsks(msg) || !strings.HasPrefix(msg, attention.KindFix+": "): // a failure or the cap that kept your decision calls (keepDecisions) too
		return "", fmt.Errorf("PR #%d's fix loop asks for something else than calls you can mark done (%s): %s",
			pr.Number, oneLine(msg, 200), look)
	case strings.TrimSpace(under) != "":
		return "", fmt.Errorf("PR #%d's fixer did not get every item it had to read, so what it left may not be all: %s; nothing changed",
			pr.Number, oneLine(under, 300))
	case seen == "":
		return "", fmt.Errorf("PR #%d needs you for something you did not see: %s; nothing changed", pr.Number, look)
	case seen != msg:
		return "", fmt.Errorf("PR #%d needs you for something else since you looked: %s; nothing changed", pr.Number, look)
	case reviewed != pr.HeadSHA:
		return "", fmt.Errorf("PR #%d's head %s is not the one magnum reviewed (%s): its review comes first; nothing changed",
			pr.Number, textx.ShortSHA(pr.HeadSHA), textx.ShortSHA(reviewed))
	}
	switch st, running, err := e.fixState(ctx, pr.ID); {
	case err != nil:
		return "", err
	case running:
		return "", errors.New(fixRunningSentence(st))
	}
	// A held fix would start and rewrite the calls; one of a PR whose owner
	// row reads and names the factory or returning waits for the factory's
	// import, and magnum never starts it. A row that cannot be read refuses:
	// the held fix may be magnum's.
	if _, held := e.getKV(ctx, KVPRFixHeld(pr.ID)); held {
		switch _, _, owned, err := e.fixOwner(ctx, pr.ID); {
		case err != nil:
			return "", fmt.Errorf("a fix of PR #%d is held, and who fixes this PR cannot be read (%w): ask again shortly; nothing changed", pr.Number, err)
		case !owned:
			return "", fmt.Errorf("a fix of PR #%d is held to start later: `magnum abort %s` takes it back first", pr.Number, ref)
		}
	}
	head := textx.ShortSHA(pr.HeadSHA)
	if dry {
		return fmt.Sprintf("dry run: would put PR #%d back to reviewed at %s, with no review", pr.Number, head), nil
	}
	err := e.st.TransitionPR(ctx, pr.ID, []string{store.PRNeedsAttention}, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("last_error", nil)
		u.Where("head_sha", pr.HeadSHA)
		u.Where("reviewed_sha", pr.HeadSHA)
		u.Where("last_error", msg)
	})
	if errors.Is(err, store.ErrConflict) {
		return "", fmt.Errorf("PR #%d moved on while you answered: %s; nothing changed", pr.Number, look)
	}
	if err != nil {
		return "", err
	}
	e.delKV(ctx, KVPRAttention(pr.ID), kvPRAttentionRun(pr.ID), kvPRAttentionBy(pr.ID))
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.acknowledged",
		"you did what the fixer left: the review of "+head+" stands, with no new round", map[string]any{"head": pr.HeadSHA, "by": fixDoneBy})
	return fmt.Sprintf("PR #%d is back to reviewed at %s: you did what the fixer left; no review runs", pr.Number, head), nil
}
