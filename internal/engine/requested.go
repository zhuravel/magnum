package engine

// Requested reviews run without delay: a review request for the poll login
// or a posting identity (or for a team a watch lists in request_teams), and
// a draft marked ready for review, start a round after [daemon]
// request_debounce instead of the quiet period, and skip the re-review
// interval, the small-delta threshold and the daily cap. Requests are
// edge-triggered by their time in the PR's timeline: the pending
// reviewRequests list cannot tell a new request from an old one, and GitHub
// drops a request once the reviewer reviews.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// KVPRRequestAt holds the time of the newest review request magnum handled
// for a PR (the edge it reacted to); KVPRRequestBy who asked: the login of
// the request's actor (else the reviewer it named), or RequestReadyForReview.
func KVPRRequestAt(prID int64) string { return fmt.Sprintf("pr.%d.request_at", prID) }
func KVPRRequestBy(prID int64) string { return fmt.Sprintf("pr.%d.request_by", prID) }

// kvRequestsSince is when this daemon first tracked review requests:
// requests older than it never count, so an upgrade does not turn every
// request ever made into a round.
const kvRequestsSince = "daemon.requests_since"

// RequestReadyForReview is KVPRRequestBy for a draft that became ready for
// review.
const RequestReadyForReview = "(ready for review)"

// Request is a review request magnum handled for a PR.
type Request struct {
	At time.Time
	By string // a login, or RequestReadyForReview
}

// Phrase is how screens name the request: "requested by alice" or "ready
// for review".
func (r Request) Phrase() string {
	if r.By == RequestReadyForReview {
		return "ready for review"
	}
	if r.By == "" {
		return "review requested"
	}
	return "requested by " + r.By
}

// noteRequestsSince records when request tracking started (once).
func (e *Engine) noteRequestsSince(ctx context.Context) {
	if _, ok := e.kvTime(ctx, kvRequestsSince); !ok {
		e.setKV(ctx, kvRequestsSince, store.FormatTime(e.now()))
	}
}

// requestLogins are the logins a review request for magnum names: the
// watch's poll login, the PR's identity's and every watch's posting
// identity's.
func (e *Engine) requestLogins(w config.Watch, prIdentity string) []string {
	names := []string{w.PollIdentity, w.Identity, prIdentity}
	for _, x := range e.cfg.Watches {
		names = append(names, x.Identity)
	}
	var out []string
	for _, n := range names {
		if id := e.cfg.IdentityByName(n); id != nil && id.Login != "" && !slices.Contains(out, id.Login) {
			out = append(out, id.Login)
		}
	}
	return out
}

// requestForUs reports whether ev asks one of logins (ignoring "[bot]", so
// GraphQL and REST forms match) or a team of w's request_teams for a review.
func requestForUs(w config.Watch, logins []string, ev github.ReviewRequestEvent) bool {
	if ev.Reviewer.Type == "Team" {
		return slices.ContainsFunc(w.RequestTeams, func(t string) bool { return strings.EqualFold(strings.TrimSpace(t), ev.Reviewer.Login) })
	}
	return slices.ContainsFunc(logins, func(l string) bool { return github.SameLogin(ev.Reviewer.Login, l) })
}

// noteRequest records the newest review request of d for magnum that is
// newer than the last one handled (KVPRRequestAt), than the repository's
// first sync and than kvRequestsSince, and reports it. A repository on its
// first sync records none: its open PRs are a baseline.
func (e *Engine) noteRequest(ctx context.Context, w config.Watch, repo store.Repo, pr store.PR, d github.PRDetails) (Request, bool) {
	if repo.FirstSyncedAt == nil {
		return Request{}, false
	}
	floor := *repo.FirstSyncedAt
	if since, ok := e.kvTime(ctx, kvRequestsSince); ok && since.After(floor) {
		floor = since
	}
	if last, ok := e.kvTime(ctx, KVPRRequestAt(pr.ID)); ok && last.After(floor) {
		floor = last
	}
	logins := e.requestLogins(w, pr.Identity)
	var pick *github.ReviewRequestEvent
	for i := range d.ReviewRequestEvents {
		ev := &d.ReviewRequestEvents[i]
		if ev.CreatedAt.After(floor) && requestForUs(w, logins, *ev) && (pick == nil || ev.CreatedAt.After(pick.CreatedAt)) {
			pick = ev
		}
	}
	if pick == nil {
		return Request{}, false
	}
	by := pick.Actor
	if by == "" {
		by = pick.Reviewer.Login
	}
	req := Request{At: pick.CreatedAt.UTC(), By: by}
	e.recordRequest(ctx, repo, pr, req, fmt.Sprintf("review requested from %s by %s", requestedName(pick.Reviewer), by))
	return req, true
}

// requestedName is how an event names the reviewer it asked.
func requestedName(r github.Reviewer) string {
	if r.Type == "Team" {
		return "team " + r.Login
	}
	return r.Login
}

// noteReady records a draft that became ready for review as a request, when
// the PR has a head no review covers yet.
func (e *Engine) noteReady(ctx context.Context, repo store.Repo, pr store.PR, now time.Time) (Request, bool) {
	if rs := deref(pr.ReviewedSHA); rs != "" && rs == pr.HeadSHA {
		return Request{}, false
	}
	req := Request{At: now.UTC(), By: RequestReadyForReview}
	e.recordRequest(ctx, repo, pr, req, "draft marked ready for review")
	return req, true
}

// recordRequest stores req as the PR's newest handled request and records
// pr.review_requested.
func (e *Engine) recordRequest(ctx context.Context, repo store.Repo, pr store.PR, req Request, what string) {
	e.setKV(ctx, KVPRRequestAt(pr.ID), store.FormatTime(req.At))
	e.setKV(ctx, KVPRRequestBy(pr.ID), req.By)
	msg := what
	_, pending := e.pendingRequest(ctx, pr)
	switch d := e.cfg.Daemon.RequestDebounce.Duration; {
	case !pending:
		msg += "; a round that started after it, or a review of the head posted after it, answers it"
	case d > 0:
		msg += fmt.Sprintf("; the next round skips the timing rules and the daily cap and waits %s after it and the last push", humanDuration(d))
	default:
		msg += "; the next round skips the timing rules and the daily cap"
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.review_requested", msg,
		map[string]any{"at": req.At, "by": req.By, "state": pr.State, "pending": pending})
}

// pendingRequest is the PR's request no round has served yet: one newer
// than the PR's last round start, and not answered by a review of the
// current head recorded after it (a request that arrived during a round
// whose review covers the head).
func (e *Engine) pendingRequest(ctx context.Context, pr store.PR) (Request, bool) {
	at, ok := e.kvTime(ctx, KVPRRequestAt(pr.ID))
	if !ok {
		return Request{}, false
	}
	if pr.LastRoundStartedAt != nil && !at.After(*pr.LastRoundStartedAt) {
		return Request{}, false
	}
	if rs := deref(pr.ReviewedSHA); rs != "" && rs == pr.HeadSHA && pr.ReviewedAt != nil && !pr.ReviewedAt.Before(at) {
		return Request{}, false
	}
	by, _ := e.getKV(ctx, KVPRRequestBy(pr.ID))
	return Request{At: at, By: by}, true
}

// onRequest acts on a request the poll just recorded: a waiting PR gets its
// next_eligible_at again (the request's debounce), and a reviewed,
// needs-attention, baseline or ineligible PR the watch's filters accept is
// queued. A PR in a round keeps it for later: the round's review answers
// it when it covers the head, else the re-review it leaves runs as
// requested. A muted PR stays muted.
func (e *Engine) onRequest(ctx context.Context, repo store.Repo, w config.Watch, prID int64, req Request, now time.Time) error {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil {
		return err
	}
	if _, ok := e.pendingRequest(ctx, pr); !ok || pr.GHState != store.GHOpen {
		return nil
	}
	why := req.Phrase()
	switch pr.State {
	case store.PRQueued, store.PRRereviewPending:
		if pr.Forced {
			return nil
		}
		td := e.throttle(ctx, w, pr, e.factsFor(pr, w, now), now)
		if pr.NextEligibleAt != nil && pr.NextEligibleAt.Equal(td.NextEligibleAt) {
			return nil
		}
		err := e.st.TransitionPR(ctx, pr.ID, []string{pr.State}, pr.State, func(u *store.PRUpdate) {
			u.Set("next_eligible_at", td.NextEligibleAt)
			u.Set("skip_reason", nil)
		})
		if err != nil {
			return err
		}
		e.event(ctx, "info", prSubject(repo, pr.Number), "pr."+pr.State,
			fmt.Sprintf("%s: eligible at %s", why, td.NextEligibleAt.Local().Format("15:04:05")), nil)
		return nil
	case store.PRReviewed, store.PRNeedsAttention, store.PRBaseline, store.PRIneligible:
		if pr.Muted || pr.DetailsAt == nil {
			return nil
		}
		if dec := e.classify(ctx, w, pr, now); !dec.Eligible {
			return nil
		}
		// A new ask: the wait, the retry budget and the backoff start over.
		return e.queue(ctx, pr, w, []string{pr.State}, true, now, why)
	}
	return nil
}
