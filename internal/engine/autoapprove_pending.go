package engine

// The operator's approval writes survive a restart (DECISIONS "Magnum never
// mistakes its own dismissal for the operator's"): magnum and the operator
// act as the same login, so the actor GitHub names on a dismissal or an
// approval cannot tell them apart; only magnum's record of its own request
// can. Before magnum sends an automatic approval or its withdrawal it records
// the request as pending (kvAutoApprovePending, kvAutoWithdrawPending), and a
// known answer clears the record; an answer it never got (a stop, a timeout,
// a network failure, a 5xx: GitHub may have applied it) leaves it. A retry or
// the next check that finds what the pending request asked for (the review
// dismissed; an approval by the operator's login on that head) counts it as
// magnum's own, "the first request's answer was lost", and stops nothing.
// A stop gives a write that started operatorWriteGrace to finish
// (writeContext), and the restart's guards count it as work in flight
// (beginWork, work.go).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/zhuravel/magnum/internal/github"
)

// kvAutoApprovePending holds the head of the PR's automatic approval whose
// post went to GitHub without an answer magnum knows (set before the post,
// deleted once its outcome is known); kvAutoWithdrawPending the id of the
// review whose withdrawal did.
func kvAutoApprovePending(prID int64) string { return fmt.Sprintf("pr.%d.auto_approve_pending", prID) }

func kvAutoWithdrawPending(prID int64) string {
	return fmt.Sprintf("pr.%d.auto_withdraw_pending", prID)
}

// lostAnswer ends the event of a write magnum counts as its own because its
// pending record says an earlier request of it went out unanswered.
const lostAnswer = " (the first request's answer was lost)"

// operatorWriteGrace is how long a stop lets a GitHub write as the operator
// that started run before it cancels it. The gh call's own timeout
// (execx.DefaultTimeout, 2 minutes) is longer than launchd lets a stopping
// daemon live (launchd.ExitTimeOut, 45 s, then SIGKILL), so the grace is the
// 30 s the shutdown already waits for rounds: GitHub itself ends a request it
// takes more than 10 s on, so 30 s covers gh's start, the request and its
// answer, and leaves the shutdown time to record it.
const operatorWriteGrace = 30 * time.Second

// writeContext is the context of one GitHub write as the operator and of
// the recording of its answer: ctx's values, never cancelled with it until
// e.writeGrace after ctx ends (a stop, a round's cancellation), so a request
// GitHub may apply is not cut short as it goes. cancel releases it.
func (e *Engine) writeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	grace := e.writeGrace
	stop := context.AfterFunc(ctx, func() {
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-wctx.Done():
		case <-t.C:
			e.log.Info("a GitHub write as the operator outlived the stop's grace; cancelled (the next daemon settles it)", "grace", grace)
			cancel()
		}
	})
	return wctx, func() { stop(); cancel() }
}

// writeAnswered reports whether err, a GitHub write's, tells what GitHub did
// with it: nothing failed (nil), or GitHub refused it (an HTTP 4xx). A call
// that was cancelled or timed out, one that never reached GitHub, and a 5xx
// (a gateway may time out on a request GitHub then applies) do not.
func writeAnswered(err error) bool {
	if err == nil {
		return true
	}
	apiErr, ok := errors.AsType[*github.APIError](err)
	return ok && apiErr.Status >= 400 && apiErr.Status < 500
}

// pendingAutoWithdrawal reports whether the PR's pending withdrawal record names
// review id: a dismissal of it went out and its answer was lost.
func (e *Engine) pendingAutoWithdrawal(ctx context.Context, prID, id int64) bool {
	v, ok := e.getKV(ctx, kvAutoWithdrawPending(prID))
	return ok && v == strconv.FormatInt(id, 10)
}

// pendingAutoApproval is the head of the PR's pending automatic approval ("" =
// none): an approval of it went out and its answer was lost.
func (e *Engine) pendingAutoApproval(ctx context.Context, prID int64) string {
	v, _ := e.getKV(ctx, kvAutoApprovePending(prID))
	return v
}

// dropPending deletes the pending record key while it still names value (a
// head, a review id): that request's outcome is known.
func (e *Engine) dropPending(ctx context.Context, key, value string) {
	if _, err := e.st.DeleteKVIf(ctx, key, value); err != nil {
		e.warnUnlessStopped(ctx, err, "pending GitHub write: record not dropped", "key", key)
	}
}
