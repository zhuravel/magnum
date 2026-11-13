package engine

// What the factory asks of magnum for a PR it fixes (DECISIONS "The fix
// loop moves into the factory"), through magnum's requests table and the
// payloads both share (internal/fixwork/requests.go): the forced re-review
// of its fixer's push, a review request with FixRef (and LocalRound when a
// local round ran at that head), and the PR's attention (fixwork.
// ReqFixAttention). The abort of its forced round is magnum's abort
// request with that FixRef (TargetPayload.FixRef, requestFactoryAbort),
// which stops only a round of that fix, or takes back its re-review in
// line; an abort without it is the operator's. Each is taken only for a PR
// whose owner row says the factory fixes it, and the end of each round of
// its fix, or the stop that ends it, is a fix.factory_round event.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/fixwork"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// factoryFixBy is who forces the factory's re-review (the pr.forced
// event): no act of the operator's.
const factoryFixBy = "the factory's fix"

// kvPRFixRef holds the factory's fix a forced round of PR prID re-reviews
// (fixRefRow as JSON), from the request to the end of the round that takes
// it.
func kvPRFixRef(prID int64) string { return fmt.Sprintf("pr.%d.fix_ref", prID) }

// fixRefRow is a kvPRFixRef row: the factory's fix, its local round (nil:
// none) and when the request came.
type fixRefRow struct {
	FixRef     fixwork.FixRef      `json:"fix_ref"`
	LocalRound *fixwork.LocalRound `json:"local_round,omitempty"`
	At         time.Time           `json:"at"`
}

// fixRefRefusal is the refusal of the factory's re-review request p of pr
// (label), "" when it may go: only a PR the factory fixes, and a fix that
// names its head.
func (e *Engine) fixRefRefusal(ctx context.Context, pr store.PR, label string, p ReviewPayload) string {
	o, _, ok, err := e.fixOwner(ctx, pr.ID)
	switch {
	case err != nil:
		return fmt.Sprintf("%s: who fixes this PR cannot be read (%v)", label, err)
	case !ok || !o.Factory():
		return fmt.Sprintf("%s: the factory does not fix this PR; a fix's re-review comes from the fixer that fixes it", label)
	case strings.TrimSpace(p.FixRef.Head) == "":
		return fmt.Sprintf("%s: the factory's fix names no head", label)
	}
	return ""
}

// keepFixRef keeps with pr the factory's fix that the forced round p asks
// for re-reviews (kvPRFixRef), or, for any other forced round (by: who
// forces it), ends one an earlier request left (endFixRef: stopped, by),
// whose round this one takes the place of: no end of the factory's fix
// goes unsaid. A fix is never ended from inside the round that holds it
// (C4): the full pass a held approval forces from its own round
// (settleApprovalHold) leaves the row to that round, whose end names it
// with its outcome and review (fixRefAfterRound), before the full pass can
// start.
func (e *Engine) keepFixRef(ctx context.Context, repo store.Repo, pr store.PR, p ReviewPayload, by string) {
	if p.FixRef == nil {
		if e.roundOfFix(pr.ID) {
			return
		}
		e.endFixRef(ctx, repo, pr, pipeline.OutcomeStopped, by)
		return
	}
	e.setKV(ctx, kvPRFixRef(pr.ID), rowJSON(fixRefRow{FixRef: *p.FixRef, LocalRound: p.LocalRound, At: e.now().UTC()}))
}

// roundOfFix reports whether a round of the factory's fix holds PR prID
// (roundHandle.fixRef): that round names the fix's end.
func (e *Engine) roundOfFix(prID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.rounds[prID]
	return h != nil && h.fixRef != nil
}

// fixRefOf is the factory's fix PR prID's forced round re-reviews (nil:
// none), and its row as stored.
func (e *Engine) fixRefOf(ctx context.Context, prID int64) (*fixRefRow, string) {
	raw, ok := e.getKV(ctx, kvPRFixRef(prID))
	if !ok {
		return nil, ""
	}
	var r fixRefRow
	if json.Unmarshal([]byte(raw), &r) != nil {
		return nil, raw
	}
	return &r, raw
}

// fixRefAfterRound passes on the factory's fix the round of job
// re-reviewed (roundJob.fixRef, taken at its dispatch): the round's end,
// its outcome, head and review, with the fix and its local round, in a
// fix.factory_round event, which the factory reads. The event is written
// once: only when the round's own delete takes the PR's row of the fix, the
// one the round took, so a stop that ended the fix first (between the
// round's unreserve and this, endFixRef) leaves it unsaid here. A round
// whose judge turn paused on a usage limit or a lost login (pauseContinues)
// has not ended: the turn continues as the same round once the pause
// lifts, so the row stays for the continue to take at its dispatch, and no
// event says it ended (as magnum's own loop waits for it, continueFixLoop).
func (e *Engine) fixRefAfterRound(ctx context.Context, job *roundJob, res pipeline.RoundResult) {
	if job.fixRef == nil || e.pauseContinues(ctx, job.pr, res.Outcome) {
		return
	}
	switch gone, err := e.st.DeleteKVIf(ctx, kvPRFixRef(job.pr.ID), job.fixRefRaw); {
	case err != nil:
		e.warnUnlessStopped(ctx, err, "drop the factory's fix of a round", "pr", job.pr.ID)
		return
	case !gone: // another end of the fix took the row: it said so
		return
	}
	r := job.fixRef
	data := map[string]any{"fix_ref": r.FixRef, "round": res.Round, "outcome": res.Outcome, "target_sha": res.TargetSHA}
	if r.LocalRound != nil {
		data["local_round"] = r.LocalRound
	}
	if res.ReviewURL != "" {
		data["review_url"] = res.ReviewURL
	}
	e.event(ctx, "info", prSubject(job.repo, job.pr.Number), "fix.factory_round", fmt.Sprintf("%s ended %s: the re-review of the factory's push of %s (cycle %d)",
		roundName(res), cmpOrOutcome(res.Outcome), textx.ShortSHA(r.FixRef.Head), r.FixRef.Cycle), data)
}

// factoryAbortBy is who stops the factory's re-review when the factory
// aborts it (fix.factory_round's by).
const factoryAbortBy = "the factory's abort"

// endFixRef ends the factory's fix that pr's forced round re-reviews (its
// kvPRFixRef row) when a stop or a close ends that round before it ran to
// its end (stopPR, dropQueued, endFixRefAtClose): fix.factory_round names
// the fix with the outcome (stopped, or closed) and who stopped it (by:
// `magnum abort`, `magnum ignore`, the factory's abort, the PR's close), so
// the factory's loop stops waiting for the round, and the row goes. The
// event is written once: only when this delete takes the row read, so the
// round's own end, or another stop, that took it first said so already. A
// row that does not read goes without an event; a PR with none is left
// alone.
func (e *Engine) endFixRef(ctx context.Context, repo store.Repo, pr store.PR, outcome, by string) {
	r, raw := e.fixRefOf(ctx, pr.ID)
	if raw == "" {
		return
	}
	switch gone, err := e.st.DeleteKVIf(ctx, kvPRFixRef(pr.ID), raw); {
	case err != nil:
		e.warnUnlessStopped(ctx, err, "drop the factory's fix of a stopped round", "pr", pr.ID)
		return
	case !gone:
		return
	}
	if r != nil {
		data := map[string]any{"fix_ref": r.FixRef, "outcome": outcome, "by": by}
		if r.LocalRound != nil {
			data["local_round"] = r.LocalRound
		}
		e.event(ctx, "info", prSubject(repo, pr.Number), "fix.factory_round", fmt.Sprintf("the re-review of the factory's push of %s (cycle %d) ended %s: %s",
			textx.ShortSHA(r.FixRef.Head), r.FixRef.Cycle, outcome, by), data)
	}
}

// endFixRefAtClose ends the factory's fix that pr kept (its kvPRFixRef row)
// when the poll confirmed the PR closed or merged on GitHub (confirmMissing,
// c): a re-review of it that waits in line or paused mid-turn never runs on
// a closed PR, so fix.factory_round names it, closed by the close, and the
// row goes. A round of the fix in flight, which a close lets run on as one
// asked for (stopClosedRound), names its own end (fixRefAfterRound).
func (e *Engine) endFixRefAtClose(ctx context.Context, repo store.Repo, pr store.PR, c closeStop) {
	if !e.roundOfFix(pr.ID) {
		e.endFixRef(ctx, repo, pr, pipeline.OutcomeClosed, c.by())
	}
}

// cmpOrOutcome is a round's outcome, error when it names none.
func cmpOrOutcome(o string) string {
	if o == "" {
		return pipeline.OutcomeError
	}
	return o
}

// factoryReReviewWords names the factory's re-review of fix.
func factoryReReviewWords(fix fixwork.FixRef) string {
	return fmt.Sprintf("the factory's re-review of %s (cycle %d)", textx.ShortSHA(fix.Head), fix.Cycle)
}

// requestFactoryAbort serves the factory's abort (request id, TargetPayload
// with FixRef ref) of pr, of repo: only the round of its own fix stops. A
// round in flight whose request carried that same fix is stopped as `magnum
// abort` stops a round (stopHeld; the round's end answers the request), a
// forced re-review of it still in line is taken back (dropQueued), and one
// whose turn paused mid-way, or whose state stays without its round after a
// restart, is stopped as `magnum abort` stops it (stopReserved). Any
// other round runs on, and the request is refused, saying why; so is an
// ignore, and a PR the factory does not fix. The operator's abort, which
// names no fix, is requestAbort's own.
func (e *Engine) requestFactoryAbort(ctx context.Context, id int64, repo store.Repo, pr store.PR, ref fixwork.FixRef, ignore bool) {
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	fix := factoryReReviewWords(ref)
	refuse := func(why string) { e.complete(ctx, id, fmt.Errorf("%s: %s", label, why), "") }
	switch o, _, ok, err := e.fixOwner(ctx, pr.ID); {
	case ignore:
		refuse("the factory's stop of its round is an abort, never an ignore")
		return
	case err != nil:
		refuse(fmt.Sprintf("who fixes this PR cannot be read (%v)", err))
		return
	case !ok || !o.Factory():
		refuse("the factory does not fix this PR: its rounds are magnum's to stop")
		return
	}
	if e.stopHeld(ctx, id, repo, pr, false, &ref) { // a round of its fix stops, and any other holder runs on
		return
	}
	// No round holds the PR: its re-review of the fix waits in line, or its
	// state stays without its round, or its turn paused mid-way (a usage
	// limit, or a restart during the judge's turn), whose continue would
	// run past the hold of the factory's next fixer: stopped as `magnum
	// abort` stops one (requestAbort).
	kept, _ := e.fixRefOf(ctx, pr.ID)
	queued := pr.State == store.PRQueued || pr.State == store.PRRereviewPending
	stale := slices.Contains(store.InFlightStates, pr.State) || pr.State == store.PRPaused
	if (!queued && !stale) || !pr.Forced || kept == nil || kept.FixRef != ref {
		refuse("no round of " + fix + " runs or waits: nothing to abort")
		return
	}
	if e.d.DryRun {
		e.complete(ctx, id, nil, "dry run: would stop "+fix+" of "+label)
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	if !e.reserve(pr.ID, &roundHandle{cancel: cancel, stop: roundStop{stopping: true}}) {
		cancel()
		refuse(e.heldReason(pr.ID) + "; ask again shortly")
		return
	}
	if stale {
		e.stopReserved(rctx, id, repo, pr, false, true)
		return
	}
	res, err := e.dropQueued(ctx, repo, pr, factoryAbortBy)
	e.unreserve(pr.ID)
	cancel()
	e.complete(ctx, id, err, res)
}

// kvPRAttentionBy holds who raised PR prID's attention when it was not
// magnum (attentionBy as JSON): the factory's raise (requestFixAttention),
// which only the factory lifts.
func kvPRAttentionBy(prID int64) string { return fmt.Sprintf("pr.%d.attention_by", prID) }

// attentionBy is a kvPRAttentionBy row: who raised the attention whose
// message is Message (the PR's last_error then: a later attention with
// other words is not the row's), with what cause, and when.
type attentionBy struct {
	By      string    `json:"by"`
	Message string    `json:"message"`
	Cause   string    `json:"cause,omitempty"`
	At      time.Time `json:"at"`
}

// attentionByFactory is attentionBy.By of the factory's raise.
const attentionByFactory = "factory"

// attentionByOf is PR prID's kvPRAttentionBy row (ok false: none, or one
// that does not parse, which marks nothing).
func (e *Engine) attentionByOf(ctx context.Context, prID int64) (attentionBy, bool) {
	raw, ok := e.getKV(ctx, kvPRAttentionBy(prID))
	var by attentionBy
	if !ok || json.Unmarshal([]byte(raw), &by) != nil {
		return attentionBy{}, false
	}
	return by, true
}

// factoryAttentionRefusal is `magnum fix --done`'s refusal of pr's
// attention msg (ref, its owner/name#N) while the factory raised it and
// fixes the PR (the engine owner's ruling on R6): the factory lifts it
// itself once the operator answers it there, and would never learn of a
// lift by magnum, so its loop would wait while magnum shows the PR
// reviewed. A row of who fixes the PR that cannot be read refuses too. nil
// for magnum's own attention, and once the PR is magnum's again, whose
// factory lifts nothing.
func (e *Engine) factoryAttentionRefusal(ctx context.Context, ref string, pr store.PR, msg string) error {
	if by, ok := e.attentionByOf(ctx, pr.ID); !ok || by.By != attentionByFactory || by.Message != msg {
		return nil
	}
	switch o, _, owned, err := e.fixOwner(ctx, pr.ID); {
	case err != nil:
		return fmt.Errorf("PR #%d needs you for an attention the factory raised, and who fixes the PR cannot be read (%w): ask again shortly; nothing changed",
			pr.Number, err)
	case !owned || !o.Factory():
		return nil
	}
	return fmt.Errorf("PR #%d needs you for what the factory's fix left, and the factory raised it: it lifts it once you answer it there "+
		"(`magnum fix --done` lifts only what magnum's own fix left), or `magnum fix --handover back %s` returns the PR to magnum; nothing changed",
		pr.Number, ref)
}

// attentionCauseRe is the factory's cause of an attention: a short word.
var attentionCauseRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// requestFixAttention raises or lifts the attention of a PR the factory
// fixes (fixwork.AttentionPayload), silently: no toast and no reveal, since
// the factory alerts the operator itself. A raise moves a reviewed PR to
// needs attention, kind fix, with the factory's why as its message, and
// marks it as the factory's (kvPRAttentionBy), which `magnum fix --done`
// refuses (factoryAttentionRefusal); a lift puts it back while its
// attention is still that one. Both say so in an event
// (fix.factory_attention), with the cause.
func (e *Engine) requestFixAttention(ctx context.Context, p fixwork.AttentionPayload) (string, error) {
	repo, pr, err := e.resolve(ctx, PRTarget{Repo: p.Repo, Number: p.Number})
	if err != nil {
		return "", err
	}
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	why := oneLine(strings.TrimSpace(p.Why), 500)
	switch o, _, ok, err := e.fixOwner(ctx, pr.ID); {
	case err != nil:
		return "", fmt.Errorf("%s: who fixes this PR cannot be read (%w)", label, err)
	case !ok || !o.Factory():
		return "", fmt.Errorf("%s: the factory does not fix this PR: its attention is magnum's", label)
	case !attentionCauseRe.MatchString(p.Cause):
		return "", fmt.Errorf("%s: the attention's cause %q is no short word", label, p.Cause)
	case why == "":
		return "", fmt.Errorf("%s: the attention names no why", label)
	}
	msg := why
	if !strings.HasPrefix(msg, attention.KindFix+": ") {
		msg = attention.KindFix + ": " + msg
	}
	subject := prSubject(repo, pr.Number)
	data := map[string]any{"cause": p.Cause, "raise": p.Raise}
	if !p.Raise {
		if pr.State != store.PRNeedsAttention || deref(pr.LastError) != msg {
			return fmt.Sprintf("%s: nothing to lift: its attention is not that one", label), nil
		}
		to := store.PRReviewed
		if deref(pr.ReviewedSHA) != pr.HeadSHA {
			to = claimableState(pr)
		}
		err := e.st.TransitionPR(ctx, pr.ID, []string{store.PRNeedsAttention}, to, func(u *store.PRUpdate) {
			u.Set("last_error", nil)
			u.Where("last_error", msg)
		})
		if err != nil {
			return "", fmt.Errorf("%s moved on meanwhile: %w", label, err)
		}
		e.delKV(ctx, KVPRAttention(pr.ID), kvPRAttentionRun(pr.ID), kvPRAttentionBy(pr.ID))
		e.event(ctx, "info", subject, "fix.factory_attention", "the factory lifted the PR's attention ("+p.Cause+")", data)
		return fmt.Sprintf("%s: the attention is lifted: PR %s", label, to), nil
	}
	switch {
	case pr.State == store.PRNeedsAttention && deref(pr.LastError) == msg:
		return fmt.Sprintf("%s needs you for that already", label), nil
	case pr.State != store.PRReviewed:
		return "", fmt.Errorf("%s is %s: the factory raises the attention of a reviewed PR alone", label, pr.State)
	}
	err = e.st.TransitionPR(ctx, pr.ID, []string{store.PRReviewed}, store.PRNeedsAttention, func(u *store.PRUpdate) {
		u.Set("last_error", msg)
		u.Set("next_attempt_at", nil)
	})
	if err != nil {
		return "", fmt.Errorf("%s moved on meanwhile: %w", label, err)
	}
	e.setKV(ctx, KVPRAttention(pr.ID), attention.KindFix)
	e.setKV(ctx, kvPRAttentionBy(pr.ID), rowJSON(attentionBy{By: attentionByFactory, Message: msg, Cause: p.Cause, At: e.now().UTC()})) // only the factory lifts it
	e.event(ctx, "warn", subject, "fix.factory_attention", "the factory raised the PR's attention ("+p.Cause+"): "+msg, data)
	return fmt.Sprintf("%s needs you: %s", label, msg), nil
}
