package pipeline

// Reply rounds (DECISIONS "Replies on magnum's threads get an answer
// without a push"): replies on the judge's review with no new commits start
// a round of the judge alone on the same head (RoundInput.SameHead), which
// re-decides the threads. When its verdict and event stay, it posts no
// review: it answers in its threads with `magnum post-review --replies`,
// each reply carrying its run's reply marker, and the round ends
// OutcomeReplied, verified by those replies on GitHub rather than by a
// review. A changed verdict posts a review as any round does.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/postreview"
)

// replyRound reports whether the round may end with replies instead of a
// review: a same-head re-review started by replies (RoundInput.Replies), in
// the judge's session or a fresh one, or the continued turn of one.
func (rd *round) replyRound() bool {
	return rd.in.Replies > 0 && (rd.sameHead() || rd.in.Kind == KindContinue)
}

// replies is JudgeData.Replies: RoundInput.Replies in a reply round, else
// 0.
func (rd *round) replies() int {
	if !rd.replyRound() {
		return 0
	}
	return rd.in.Replies
}

// previousID is the id of the review a replied round leaves standing (0 =
// unknown).
func previousID(p *PreviousReview) int64 {
	if p == nil {
		return 0
	}
	return p.ID
}

// errNoThreadLister: the round's GitHub client cannot list review threads,
// so replies cannot be verified.
var errNoThreadLister = errors.New("pipeline: the GitHub client cannot list review threads")

// findReplies lists the replies the reviewer login posted in the PR's
// threads carrying the reply marker of one of markers (the run ids the
// round's judge posts under): what a replied round posted, in thread order.
func (rd *round) findReplies(ctx context.Context, markers []string) ([]PostedReply, error) {
	lister, ok := rd.r.GitHub.(ThreadLister)
	if !ok {
		return nil, errNoThreadLister
	}
	ts, err := lister.ReviewThreads(ctx, rd.owner, rd.name, rd.in.PR.Number)
	if err != nil {
		return nil, err
	}
	var out []PostedReply
	for _, t := range ts {
		if len(t.Comments) < 2 {
			continue
		}
		for _, c := range t.Comments[1:] {
			if !rd.isReviewer(c.AuthorLogin, c.AuthorType) {
				continue
			}
			if run, kind, ok := postreview.ParseReplyMarker(c.Body); ok && slices.Contains(markers, run) {
				out = append(out, PostedReply{CommentID: t.Comments[0].ID, ID: c.ID, Kind: kind, URL: c.URL})
			}
		}
	}
	return out, nil
}

// repliedVerdict decides a reply round's turn that posted no review: the
// replies GitHub shows with the run's marker make it replied; else a result
// that says replied with none listed (nothing needed an answer) does; a
// result that lists replies GitHub does not show needs attention. ok is
// false when neither says replied: the usual verdict follows.
func (rd *round) repliedVerdict(ctx context.Context, markers []string, res *judgeResult) (verdict, bool) {
	ask := func() ([]PostedReply, error) { return rd.findReplies(ctx, markers) }
	var found []PostedReply
	var err error
	if res != nil && res.Status == statusReplied {
		// An error here ends the round: ask again after a network failure.
		found, err = askAgainOnNetwork(ctx, rd, "the replies", ask)
	} else {
		found, err = ask()
	}
	if err != nil {
		if ctx.Err() != nil {
			return verdict{final: true, outcome: OutcomeStopped, err: ctx.Err()}, true
		}
		if res == nil || res.Status != statusReplied {
			rd.logf("pipeline: look for the round's replies: %v", err)
			return verdict{}, false
		}
		return verdict{final: true, outcome: OutcomeError, result: res, err: fmt.Errorf("%w: the replies: %w", errUnverified, err)}, true
	}
	switch {
	case len(found) > 0:
		if res != nil && len(res.Replies) > len(found) {
			rd.warn(ctx, "the judge reported %d replies; GitHub shows %d with the run's marker", len(res.Replies), len(found))
		}
		return verdict{final: true, outcome: OutcomeReplied, result: res, replies: found}, true
	case res == nil || res.Status != statusReplied:
		return verdict{}, false
	case len(res.Replies) == 0:
		return verdict{final: true, outcome: OutcomeReplied, result: res}, true
	}
	return verdict{final: true, outcome: OutcomeNeedsAttention, result: res,
		err: fmt.Errorf("the judge reported %d replies, but GitHub shows none by %s with the run's marker", len(res.Replies), rd.login)}, true
}

// replyKinds counts replies by kind for an event: "1 rebuttal, 2 answers".
func replyKinds(replies []PostedReply) string {
	counts := map[string]int{}
	for _, r := range replies {
		counts[r.Kind]++
	}
	var parts []string
	for _, k := range []struct{ kind, one, many string }{
		{postreview.ReplyAck, "acknowledgement", "acknowledgements"},
		{postreview.ReplyRebuttal, "rebuttal", "rebuttals"},
		{postreview.ReplyAnswer, "answer", "answers"},
	} {
		if n := counts[k.kind]; n > 0 {
			word := k.many
			if n == 1 {
				word = k.one
			}
			parts = append(parts, fmt.Sprintf("%d %s", n, word))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
