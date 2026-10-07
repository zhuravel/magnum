package engine

// Reply rounds (DECISIONS "Replies on magnum's threads get an answer
// without a push"): a reply on magnum's latest review triggered nothing
// until the next push. On one PR three author replies waited 16.5 hours for
// a verdict; on another the operator forced a 17-minute round to have the
// judge read a declined finding. Now the Details the poll reads for a PR
// whose activity moved list its reviews and issue comments (github.Remark),
// the replies among them are kept (store.Reply: the PR author's own, and
// a reply in one of magnum's threads by the author or by an owner, member
// or collaborator of the repository; magnum's own logins never count), and
// a reviewed PR whose head has not moved since gets a same-head
// round of the judge alone [daemon] reply_debounce after the last reply, at
// most once per head every reply_min_interval. Its judge answers in the
// threads (pipeline.OutcomeReplied) or posts a new review when its verdict
// changes. A push meanwhile wins: the re-review it gets reads the replies.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kvRepliesSince is when this daemon first tracked replies: older replies
// start no round, so an upgrade does not re-decide every reply ever left
// (the board still shows them, and `r` re-decides them).
const kvRepliesSince = "daemon.replies_since"

// KVPRReplyRound holds the last reply round of a PR (ReplyRound as JSON):
// the head it ran on and when it started, for reply_min_interval, and
// whether a continued turn of it may still end replied.
func KVPRReplyRound(prID int64) string { return fmt.Sprintf("pr.%d.reply_round", prID) }

// kvPRRedecide marks a forced round the operator asked to re-decide the
// replies (`magnum review --replies`, the board's r on a PR with replies):
// a reply round, though forced. It ends with its request: the end of the
// round however it ended (posted, replied, needs_attention, a dry run), an
// abort, or a later `magnum review` without --replies.
func kvPRRedecide(prID int64) string { return fmt.Sprintf("pr.%d.redecide", prID) }

// ReplyRound is a reply round as KVPRReplyRound records it.
type ReplyRound struct {
	Head    string    `json:"head"`
	At      time.Time `json:"at"`
	Replies int       `json:"replies"`
}

// noteRepliesSince records when reply tracking started (once).
func (e *Engine) noteRepliesSince(ctx context.Context) {
	if _, ok := e.kvTime(ctx, kvRepliesSince); !ok {
		e.setKV(ctx, kvRepliesSince, store.FormatTime(e.now()))
	}
}

// ownLogins are the logins magnum posts as on pr (REST form): every
// watch's posting identity, the PR's (or, for a PR not recorded yet, the
// watch's) and its former identities'. Replies by them are magnum's own.
func (e *Engine) ownLogins(ctx context.Context, w config.Watch, pr store.PR) []string {
	var out []string
	add := func(l string) {
		if l != "" && !slices.ContainsFunc(out, func(x string) bool { return github.SameAccount(x, l) }) {
			out = append(out, l)
		}
	}
	add(e.reviewerLogin(cmp.Or(pr.Identity, w.Identity)))
	for _, x := range e.cfg.Watches {
		add(e.reviewerLogin(x.Identity))
	}
	if pr.ID != 0 {
		for _, l := range e.formerLogins(ctx, pr) {
			add(l)
		}
	}
	return out
}

// repliesOf picks the replies to magnum's reviews from the Details'
// remarks: by none of own (magnum's logins), either the PR author's own
// review or comment (unless the author is a bot: a bot's remarks count only
// in magnum's threads), or a review that replies in a thread one of own
// started (Remark.Answers) by the PR author or by an owner, member or
// collaborator of the repository (trustedReplier): in a public repository
// anyone may reply, and a reply round's decision can become a standing one
// in the notes. Other replies start nothing; the judge still reads them in
// the threads file. "(Claude)" replies an author's agent posts as the
// author are the author's. Nil when the Details carry no timeline.
func repliesOf(d github.PRDetails, own []string) []store.Reply {
	if d.Remarks == nil {
		return nil
	}
	isOwn := func(login string) bool {
		return slices.ContainsFunc(own, func(l string) bool { return github.SameAccount(l, login) })
	}
	author := github.Account(d.AuthorLogin, d.AuthorType)
	authorBot := github.IsBot(d.AuthorType, d.AuthorLogin)
	out := []store.Reply{}
	for _, r := range d.Remarks {
		if r.Author == "" || isOwn(r.Author) {
			continue
		}
		byAuthor := github.SameAccount(r.Author, author)
		thread := r.Review && slices.ContainsFunc(r.Answers, isOwn)
		if (thread && (byAuthor || trustedReplier(r.Association))) || (!authorBot && !r.Bot && byAuthor) {
			out = append(out, store.Reply{At: r.At.UTC(), By: r.Author, Thread: thread})
		}
	}
	return out
}

// trustedReplier reports whether an authorAssociation is one whose reply in
// magnum's threads starts a reply round: an owner, member or collaborator
// of the repository.
func trustedReplier(association string) bool {
	switch association {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	}
	return false
}

// replyTrigger is the PR's latest reply that starts a reply round, and how
// many replies its judge has not re-decided: replies on (reply_debounce >
// 0), an open PR magnum reviewed at its head (no push since: a push wins),
// not muted, not post-merge, with a pending reply newer than reply tracking
// (kvRepliesSince). ok is false otherwise.
func (e *Engine) replyTrigger(ctx context.Context, pr store.PR) (latest time.Time, n int, ok bool) {
	if e.cfg.Daemon.ReplyDebounce.Duration <= 0 || pr.GHState != store.GHOpen || pr.Muted || postMerge(pr) ||
		pr.HeadSHA == "" || pr.HeadSHA != deref(pr.ReviewedSHA) {
		return time.Time{}, 0, false
	}
	pending := pr.PendingReplies()
	if len(pending) == 0 {
		return time.Time{}, 0, false
	}
	latest = pending[len(pending)-1].At
	if since, ok := e.kvTime(ctx, kvRepliesSince); ok && !latest.After(since) {
		return time.Time{}, 0, false
	}
	return latest, len(pending), true
}

// lastReplyRound is the PR's last reply round on its current head (zero:
// none).
func (e *Engine) lastReplyRound(ctx context.Context, pr store.PR) ReplyRound {
	v, ok := e.getKV(ctx, KVPRReplyRound(pr.ID))
	var r ReplyRound
	if !ok || json.Unmarshal([]byte(v), &r) != nil || r.Head != pr.HeadSHA {
		return ReplyRound{}
	}
	return r
}

// considerReplies starts the wait for a reply round of a PR whose replies
// may need one (replyTrigger): a reviewed PR the watch's filters still
// accept moves to rereview_pending, held by the reply debounce and interval
// (eligibility.Throttle with RepliedAt); one already waiting for them gets
// its time again, and one whose replies are gone goes back to reviewed
// (dropReplyWait). Anything else (a round in flight, a push pending, a forced
// or paused PR) is left alone: what comes next reads the replies.
func (e *Engine) considerReplies(ctx context.Context, repo store.Repo, w config.Watch, prID int64, now time.Time) error {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil {
		return err
	}
	_, n, ok := e.replyTrigger(ctx, pr)
	if !ok {
		return e.dropReplyWait(ctx, repo, pr)
	}
	if pr.Forced || pr.DetailsAt == nil {
		return nil
	}
	why := textx.Count(n, "reply", "replies") + " to the review"
	switch pr.State {
	case store.PRReviewed:
		if dec := e.classify(ctx, w, pr, now); !dec.Eligible {
			return nil
		}
		return e.queue(ctx, pr, w, []string{store.PRReviewed}, false, now, why)
	case store.PRRereviewPending:
		td := e.throttle(ctx, w, pr, e.factsFor(ctx, pr, w, now), now)
		if pr.NextEligibleAt != nil && pr.NextEligibleAt.Equal(td.NextEligibleAt) {
			return nil
		}
		err := e.st.TransitionPR(ctx, pr.ID, []string{pr.State}, pr.State, func(u *store.PRUpdate) {
			u.Set("next_eligible_at", td.NextEligibleAt)
		})
		if err != nil {
			return err
		}
		e.event(ctx, "info", prSubject(repo, pr.Number), "pr.rereview_pending",
			fmt.Sprintf("%s: eligible at %s", why, td.NextEligibleAt.Local().Format("15:04:05")), nil)
	}
	return nil
}

// dropReplyWait takes back the wait for a reply round whose replies are
// gone (deleted, or past the timeline items GitHub lists): the PR waits at
// the head magnum reviewed (rereview_pending, not forced, open) for nothing
// else, so it goes back to reviewed in a compare-and-set on both commits;
// its round, no longer a reply round, would post a review of the unchanged
// head. A review request
// waiting for its round keeps the PR in line, and so does the retry of a
// round that was not a reply round (a failed requested one): attempts, a
// retry time or an error, and no reply round on the head since the last
// round start (continuedReplies).
func (e *Engine) dropReplyWait(ctx context.Context, repo store.Repo, pr store.PR) error {
	if pr.State != store.PRRereviewPending || pr.Forced || pr.GHState != store.GHOpen || postMerge(pr) ||
		pr.HeadSHA == "" || pr.HeadSHA != deref(pr.ReviewedSHA) {
		return nil
	}
	if _, ok := e.pendingRequest(ctx, pr); ok {
		return nil
	}
	retry := pr.Attempts > 0 || pr.NextAttemptAt != nil || pr.LastError != nil
	if retry && e.continuedReplies(ctx, pr, pr.HeadSHA) == 0 {
		return nil
	}
	err := e.st.TransitionPR(ctx, pr.ID, []string{store.PRRereviewPending}, store.PRReviewed, func(u *store.PRUpdate) {
		u.Where("head_sha", pr.HeadSHA)
		u.Where("reviewed_sha", pr.HeadSHA)
		u.Set("next_eligible_at", nil)
		u.Set("pending_since", nil)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
		u.Set("last_error", nil)
		u.Set("skip_reason", nil)
	})
	if err != nil {
		return err
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.reviewed",
		"rereview_pending → reviewed: the replies that queued the re-decision are gone; the review stands", nil)
	return nil
}

// replyRoundDue is how many replies a round dispatched for pr re-decides as
// a reply round (0: none): a same-head re-review (sameHead) that no review
// request started, of a PR with pending replies, either automatic or
// forced by the operator to re-decide them (kvPRRedecide).
func (e *Engine) replyRoundDue(ctx context.Context, pr store.PR, sameHead, requested bool) int {
	if !sameHead || requested {
		return 0
	}
	if pr.Forced {
		if v, _ := e.getKV(ctx, kvPRRedecide(pr.ID)); v != "1" {
			return 0
		}
		return len(pr.PendingReplies())
	}
	if _, n, ok := e.replyTrigger(ctx, pr); ok {
		return n
	}
	return 0
}

// noteReplyRound records the reply round starting now on target
// (KVPRReplyRound).
func (e *Engine) noteReplyRound(ctx context.Context, prID int64, target string, replies int, at time.Time) {
	if b, err := json.Marshal(ReplyRound{Head: target, At: at.UTC(), Replies: replies}); err == nil {
		e.setKV(ctx, KVPRReplyRound(prID), string(b))
	}
}

// continuedReplies is RoundInput.Replies of a continued turn: the paused
// round's, when it was a reply round on target (KVPRReplyRound started with
// the paused round), else 0.
func (e *Engine) continuedReplies(ctx context.Context, pr store.PR, target string) int {
	v, ok := e.getKV(ctx, KVPRReplyRound(pr.ID))
	var r ReplyRound
	if !ok || json.Unmarshal([]byte(v), &r) != nil || r.Head != target || pr.LastRoundStartedAt == nil ||
		r.At.Before(pr.LastRoundStartedAt.Add(-time.Second)) {
		return 0
	}
	return r.Replies
}

// repliesReadAt is when the round's judge read the replies it re-decided
// (prs.replies_read_at): its prompt, after which a reply is new. A first
// review answers no earlier one, so only replies after it count: at, the
// time the round is recorded; a continued turn read them when its round
// started (started, the PR's last round start), and a round whose prompt
// time is unknown at that start too, else at.
func repliesReadAt(kind string, res pipeline.RoundResult, started *time.Time, at time.Time) time.Time {
	switch {
	case kind == pipeline.KindInitial:
		return at
	case kind == pipeline.KindContinue || res.JudgePromptedAt.IsZero():
		if started != nil && started.Before(at) {
			return *started
		}
		return at
	}
	return res.JudgePromptedAt
}

// replyWord says how many replies a reply round answered: "replied in 2
// threads", or "nothing to answer".
func replyWord(res pipeline.RoundResult) string {
	if len(res.Replies) == 0 {
		return "nothing to answer"
	}
	return "replied in " + textx.Count(len(res.Replies), "thread", "threads")
}

// onReplied records a reply round whose judge answered in its threads (or
// found nothing to answer) and posted no review: the review stands, so the
// PR goes back to reviewed with the replies read up to the judge's prompt;
// replies that came after it wait for their own round. A push that arrived
// meanwhile is settled as after a review round (settleHead): a trivial one
// leaves the review standing for the new head, any other is measured for
// the re-review (a small one gets a delta check), which reads the replies
// too. The round only answered, so the re-review interval stays timed from
// the last review round: last_round_started_at goes back to the start
// before this one (job.prevStart); reply rounds keep their own spacing
// (reply_min_interval, KVPRReplyRound).
func (e *Engine) onReplied(ctx context.Context, job *roundJob, pr store.PR, in pipeline.RoundInput, res pipeline.RoundResult, from []string) {
	now := e.now()
	readAt := repliesReadAt(in.Kind, res, pr.LastRoundStartedAt, now)
	m := e.settleHead(ctx, job, pr, in.TargetSHA, now)
	to, next, trivial := m.to, m.next, m.trivial
	restore := job.started && pr.LastRoundStartedAt != nil && pr.LastRoundStartedAt.Equal(job.startedAt)
	set := func(u *store.PRUpdate) {
		if to == store.PRReviewed {
			u.Where("head_sha", m.reviewed)
			if trivial != nil {
				u.Set("reviewed_sha", m.reviewed)
			}
		}
		if restore {
			if job.prevStart != nil {
				u.Set("last_round_started_at", *job.prevStart)
			} else {
				u.Set("last_round_started_at", nil)
			}
		}
		u.Set("replies_read_at", readAt)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
		u.Set("last_error", nil)
		u.Set("forced", false)
		u.Set("skip_reason", nil)
		if to == store.PRRereviewPending {
			u.Set("next_eligible_at", next)
			if pr.PendingSince == nil {
				u.Set("pending_since", pr.HeadChangedAt)
			}
		} else {
			u.Set("next_eligible_at", nil)
		}
	}
	err := e.st.TransitionPR(ctx, pr.ID, from, to, set)
	if err != nil && to == store.PRReviewed {
		if cur, rerr := e.st.PRByID(ctx, pr.ID); rerr == nil && slices.Contains(from, cur.State) && cur.HeadSHA != m.reviewed {
			pr, to, next, trivial = cur, store.PRRereviewPending, e.rereviewAt(ctx, cur, job.watch, in.TargetSHA, now), nil
			err = e.st.TransitionPR(ctx, pr.ID, from, to, set)
		}
	}
	if err != nil {
		e.log.Warn("record replies", "pr", pr.ID, "err", err)
		_ = e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("replies_read_at", readAt) })
	} else if trivial != nil {
		e.recordTrivial(ctx, job.repo, pr, TrivialSkip{From: in.TargetSHA, To: m.reviewed, Classes: trivial, Files: m.settled.files, At: now},
			fmt.Sprintf("the push to %s during the reply round %s since %s; no re-review, the review stands",
				textx.ShortSHA(m.reviewed), m.settled.change(), textx.ShortSHA(in.TargetSHA)))
	}
	subject := prSubject(job.repo, pr.Number)
	msg := fmt.Sprintf("%s on %s; no new review: review %d stands", replyWord(res), textx.ShortSHA(in.TargetSHA), deref(pr.LastReviewID))
	if to == store.PRRereviewPending {
		msg += "; the re-review of the new head follows"
	}
	e.event(ctx, "info", subject, "pr.replied", msg,
		map[string]any{"replies": len(res.Replies), "kinds": replyKindCounts(res.Replies), "target_sha": in.TargetSHA, "state": to})
	e.delKV(ctx, kvPRDryRun(pr.ID), kvPRRedecide(pr.ID))
	e.clearRequested(ctx, pr.ID)
	if k := e.cfg.JudgeFor(&job.watch).AgentKind(); k != "" {
		e.delKV(ctx, kvToolBackoff(k))
	}
	if pr.Forced {
		e.toastRequestedReplied(job, pr, res)
	}
	if to == store.PRReviewed {
		if err := e.considerReplies(ctx, job.repo, job.watch, pr.ID, now); err != nil {
			e.log.Info("replies after the round", "pr", pr.ID, "err", err)
		}
	}
}

// replyKindCounts counts replies by kind for an event's data.
func replyKindCounts(replies []pipeline.PostedReply) map[string]int {
	out := map[string]int{}
	for _, r := range replies {
		out[r.Kind]++
	}
	return out
}

// replyLabel is a reply round's name in events: "reply round (2 replies)".
func replyLabel(n int) string {
	return "reply round (" + textx.Count(n, "reply", "replies") + ")"
}

// stalemateWord names the threads magnum stopped arguing in.
func stalemateWord(n int) string { return textx.Count(n, "thread", "threads") }
