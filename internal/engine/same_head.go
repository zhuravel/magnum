package engine

// Same-head re-reviews (DECISIONS "A re-review of an unchanged head is the
// judge alone"): `magnum review` on a PR whose head magnum had reviewed, to
// have the judge re-read an author's reply, ran claude-review, codex-review
// and the judge for 17 minutes though no code changed. A re-review with no
// commits since the reviewed one, forced or requested, now runs as a round
// of the judge alone, as a delta check does (deltacheck.go): no triage, no
// reruns, no restarts, at the judge's rereview effort, in a fresh session
// too (checkFresh); its prompt has it re-decide its earlier findings from
// the replies (pipeline.RoundInput.SameHead). A request that names roles
// or asks for fresh sessions runs in full, as before.

import (
	"context"
	"fmt"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// sameHeadDue reports whether pr's next re-review is of the head magnum
// last reviewed (no new commits), which the judge re-decides alone: not a
// post-merge review, no roles named (`magnum review --role`, --simplify)
// and no fresh sessions asked for (--fresh).
func (e *Engine) sameHeadDue(ctx context.Context, pr store.PR) bool {
	if pr.HeadSHA == "" || pr.HeadSHA != deref(pr.ReviewedSHA) || postMerge(pr) {
		return false
	}
	if v, _ := e.getKV(ctx, store.KVPRFresh(pr.ID)); v == "1" {
		return false
	}
	return len(e.requestedRoles(ctx, pr.ID)) == 0
}

// confirmSameHead reports whether the checkout of a round dispatched as a
// same-head re-review found the reviewed commit (target). A newer head (a
// push the poller had not seen) makes it a re-review of those commits, in
// full, with round.same_head_dropped.
func (e *Engine) confirmSameHead(ctx context.Context, job *roundJob, target string) bool {
	reviewed := deref(job.pr.ReviewedSHA)
	if target == reviewed {
		return true
	}
	e.event(ctx, "info", prSubject(job.repo, job.pr.Number), "round.same_head_dropped",
		fmt.Sprintf("a full round instead of the judge alone: the checkout found %s, not the reviewed %s", textx.ShortSHA(target), textx.ShortSHA(reviewed)),
		map[string]any{"reviewed_sha": reviewed, "target_sha": target})
	return false
}
