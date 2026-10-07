package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// Request kinds (requests.kind) the daemon consumes.
const (
	ReqReview  = "review"
	ReqRelease = "release"
	ReqPin     = "pin"
	ReqUnpin   = "unpin"
	ReqMute    = "mute"
	ReqUnmute  = "unmute"
	ReqCleanup = "cleanup"
	ReqKick    = "kick"
	ReqPause   = "pause"
	ReqResume  = "resume"
	// ReqOpen restores a parked PR for a human (magnum open): see
	// OpenPayload.
	ReqOpen = "open"
	// ReqProvision, ReqRepair and ReqAdopt are `magnum slots provision |
	// repair | adopt` handed to the running daemon (heavy worker).
	ReqProvision = "provision"
	ReqRepair    = "repair"
	ReqAdopt     = "adopt"
	// ReqIdentityVerdict records a `magnum identities check` verdict
	// (IdentityVerdictPayload) as the daemon records its own checks.
	ReqIdentityVerdict = "identity_verdict"
	// ReqRetro starts a retro now (RetroPayload, retro.go).
	ReqRetro = "retro"
)

// maxRequestsPerTick bounds the requests one tick handles.
const maxRequestsPerTick = 50

// PRTarget names a PR in a request: Repo ("owner/name") and Number win;
// otherwise Ref is any form github.ParseRef accepts, resolved against
// daemon.default_repo.
type PRTarget struct {
	Ref    string `json:"ref,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
}

// ReviewPayload is a `magnum review` request: a forced round that bypasses
// eligibility, the throttle and quiet hours. On a PR GitHub merged it is a
// post-merge review (post_merge.go).
type ReviewPayload struct {
	PRTarget
	// Again is ignored: a forced round reviews the head even when it was
	// reviewed already. No CLI sends it any more; it stays so a request an
	// older CLI queued with --again still decodes (decode refuses unknown fields).
	Again bool `json:"again,omitempty"`
	Fresh bool `json:"fresh,omitempty"` // park the sessions and start new conversations
	// Simplify asks for the role answering to "simplify" (claude-simplify
	// by default) this round, whatever its runs; kept for older CLIs, the
	// same as Roles ["simplify"].
	Simplify bool `json:"simplify,omitempty"`
	// Roles asks for these roles of the PR's watch (names or aliases) this
	// round, whatever their runs ("first" or "manual"; a "never" role is
	// refused). The daemon stores the resolved names until a review posts.
	Roles []string `json:"roles,omitempty"`
	As    string   `json:"as,omitempty"` // post as this identity from now on
	// DryRun runs the round with pipeline.RoundInput.DryRun: the judge
	// writes its planned review and posts nothing; afterwards the PR returns
	// to the state it had (reviewed_sha does not move).
	DryRun bool `json:"dry_run,omitempty"`
	// Replies asks the judge alone to re-decide the replies on its review
	// now (`magnum review --replies`, the board's r on a PR with replies): a
	// reply round, which answers in the threads unless the verdict changes.
	// It takes a head magnum reviewed and replies it has not re-decided;
	// otherwise the round is an ordinary forced one.
	Replies bool `json:"replies,omitempty"`
}

// OpenPayload is a `magnum open` request for a PR without a live session:
// the daemon gives it a slot (its own, a free pool slot or a per-PR
// worktree), checks out reviewed_sha (else the head) when the slot holds
// something else, starts or resumes the judge and Role's agent (an
// on-demand role gets its pane), pins the PR and its slot, and completes
// with the pane id of Role. A PR whose session is live, or whose round is
// running, completes at once.
type OpenPayload struct {
	PRTarget
	Role string `json:"role,omitempty"` // a role of the PR's watch, by name or alias; "" = its judge
}

// ProvisionPayload provisions Count pool slots (resuming interrupted ones
// first; Count <= 0 = up to pool.min) of the pool of repository Pool
// ("" = daemon.default_repo, else the only pool).
type ProvisionPayload struct {
	Pool  string `json:"pool,omitempty"`
	Count int    `json:"count,omitempty"`
}

// RepairPayload re-runs provisioning of a free, broken, provisioning or lost
// pool slot.
type RepairPayload struct {
	Slot string `json:"slot"`
}

// AdoptPayload registers the existing checkout at Path as a free slot of the
// pool of repository Pool ("" = the pool whose slot_path matches Path).
type AdoptPayload struct {
	Pool string `json:"pool,omitempty"`
	Path string `json:"path"`
}

// TargetPayload names a PR or a slot (release, pin, unpin, mute, unmute).
type TargetPayload struct {
	PRTarget
	Slot  string `json:"slot,omitempty"`
	Force bool   `json:"force,omitempty"` // release: see cleanup.Options.Force
	// Reason is why the operator mutes the PR (`magnum mute <ref> [reason…]`),
	// kept in the request, its answer and the pr.muted event.
	Reason string `json:"reason,omitempty"`
}

// CleanupPayload is a cleanup request: Plan (re-checked by Apply) or
// Options (planned again by the daemon). Confirmed is the typed
// confirmation for actions that need it.
type CleanupPayload struct {
	Options   cleanup.Options `json:"options"`
	Plan      *cleanup.Plan   `json:"plan,omitempty"`
	Confirmed bool            `json:"confirmed,omitempty"`
}

// IdentityVerdictPayload is the verdict of `magnum identities check` for the
// configured identity Name; Reason says why a failed check failed.
type IdentityVerdictPayload struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Reason string `json:"reason,omitempty"`
}

// PausePayload pauses (pause) or resumes (resume) automation. For resume,
// Tool (an agent kind such as "codex", "claude", "droid", or "all") lifts
// that kind's pause and Watch a watch owner's identity-leak pause instead of
// the daemon pause.
type PausePayload struct {
	Reason string     `json:"reason,omitempty"`
	Until  *time.Time `json:"until,omitempty"`
	Tool   string     `json:"tool,omitempty"`
	Watch  string     `json:"watch,omitempty"`
}

// handleRequests consumes pending CLI requests, oldest first. Quick ones
// complete in the tick; release, cleanup, provision, repair and adopt run on
// the heavy worker and open in its own goroutine, which complete them.
func (e *Engine) handleRequests(ctx context.Context) {
	reqs, err := e.st.PendingRequests(ctx, maxRequestsPerTick)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "requests")
		return
	}
	for _, req := range reqs {
		e.mu.Lock()
		busy := e.inflight[req.ID]
		e.mu.Unlock()
		if busy {
			continue
		}
		e.handleRequest(ctx, req)
	}
}

func (e *Engine) complete(ctx context.Context, id int64, err error, result string) {
	state := store.RequestDone
	if err != nil {
		state = store.RequestFailed
		result = err.Error()
	}
	if cerr := e.st.CompleteRequest(ctx, id, state, result); cerr != nil {
		e.warnUnlessStopped(ctx, cerr, "complete request", "id", id)
	}
	e.event(ctx, "info", fmt.Sprintf("request:%d", id), "request."+state, result, nil)
}

func (e *Engine) handleRequest(ctx context.Context, req store.Request) {
	switch req.Kind {
	case ReqKick:
		e.complete(ctx, req.ID, nil, "ok")
	case ReqReview:
		answer(e, ctx, req, e.requestReview)
	case ReqPin, ReqUnpin:
		answer(e, ctx, req, func(ctx context.Context, p TargetPayload) (string, error) {
			return e.requestPin(ctx, p, req.Kind == ReqPin)
		})
	case ReqMute, ReqUnmute:
		answer(e, ctx, req, func(ctx context.Context, p TargetPayload) (string, error) {
			return e.requestMute(ctx, p, req.Kind == ReqMute)
		})
	case ReqSnooze: // snooze.go
		answer(e, ctx, req, e.requestSnooze)
	case ReqApprove, ReqRequestChanges:
		event := map[string]string{ReqApprove: "APPROVE", ReqRequestChanges: "REQUEST_CHANGES"}[req.Kind]
		answer(e, ctx, req, func(ctx context.Context, p VerdictPayload) (string, error) {
			return e.requestVerdict(ctx, p, event)
		})
	case ReqUnapprove: // autoapprove.go
		answer(e, ctx, req, e.requestUnapprove)
	case ReqAbort, ReqIgnore: // abort.go
		if p, ok := payload[TargetPayload](e, ctx, req); ok {
			e.requestAbort(ctx, req.ID, p, req.Kind == ReqIgnore)
		}
	case ReqPause, ReqResume:
		answer(e, ctx, req, func(ctx context.Context, p PausePayload) (string, error) {
			return e.requestPause(ctx, p, req.Kind == ReqPause)
		})
	case ReqIdentityVerdict:
		answer(e, ctx, req, e.requestIdentityVerdict)
	case ReqRetro:
		answer(e, ctx, req, e.requestRetro)
	case ReqNotesCurate: // notes_curate.go
		answer(e, ctx, req, e.requestCurate)
	case ReqRelease:
		p, ok := payload[TargetPayload](e, ctx, req)
		if !ok {
			return
		}
		opts := cleanup.Options{Force: p.Force}
		if p.Slot != "" {
			opts.Slot = p.Slot
		} else {
			repo, pr, err := e.resolve(ctx, p.PRTarget)
			if err != nil {
				e.complete(ctx, req.ID, err, "")
				return
			}
			opts.PR = &cleanup.PRRef{Repo: repo.FullName(), Number: pr.Number}
		}
		e.cleanupAsync(ctx, req.ID, CleanupPayload{Options: opts})
	case ReqCleanup:
		if p, ok := payload[CleanupPayload](e, ctx, req); ok {
			e.cleanupAsync(ctx, req.ID, p)
		}
	case ReqOpen:
		if p, ok := payload[OpenPayload](e, ctx, req); ok {
			e.requestOpen(ctx, req.ID, p)
		}
	case ReqProvision:
		p, ok := payload[ProvisionPayload](e, ctx, req)
		if !ok {
			return
		}
		pool, err := e.poolNamed(p.Pool)
		if err != nil {
			e.complete(ctx, req.ID, err, "")
			return
		}
		e.heavyRequest(ctx, req.ID, func(ctx context.Context) (string, error) { return e.provisionSlots(ctx, pool, p.Count) })
	case ReqRepair:
		if p, ok := payload[RepairPayload](e, ctx, req); ok {
			e.heavyRequest(ctx, req.ID, func(ctx context.Context) (string, error) { return e.repairSlot(ctx, p.Slot) })
		}
	case ReqAdopt:
		if p, ok := payload[AdoptPayload](e, ctx, req); ok {
			e.heavyRequest(ctx, req.ID, func(ctx context.Context) (string, error) { return e.adoptSlot(ctx, p) })
		}
	default:
		e.complete(ctx, req.ID, fmt.Errorf("unknown request kind %q", req.Kind), "")
	}
}

// payload decodes req's payload into a T; a bad payload completes the
// request as failed and reports false.
func payload[T any](e *Engine, ctx context.Context, req store.Request) (T, bool) {
	var p T
	if err := decode(req, &p); err != nil {
		e.complete(ctx, req.ID, err, "")
		return p, false
	}
	return p, true
}

// answer completes a quick request with fn's result on its decoded payload.
func answer[T any](e *Engine, ctx context.Context, req store.Request, fn func(context.Context, T) (string, error)) {
	if p, ok := payload[T](e, ctx, req); ok {
		res, err := fn(ctx, p)
		e.complete(ctx, req.ID, err, res)
	}
}

// decode reads req's payload into v. A field v does not know comes from a
// newer CLI: the request fails instead of running without it (a dropped
// dry_run would post), so a payload field, once added, is never removed.
func decode(req store.Request, v any) error {
	if len(req.Payload) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(req.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return fmt.Errorf("request %d (%s): this daemon predates %s: restart it (`magnum daemon-restart`), then retry", req.ID, req.Kind, field)
		}
		return fmt.Errorf("request %d (%s): bad payload: %w", req.ID, req.Kind, err)
	}
	return nil
}

// requestIdentityVerdict records a CLI identity check through the path the
// daemon's own checks take (recordIdentityVerdict), with the CLI's
// identity.checked event.
func (e *Engine) requestIdentityVerdict(ctx context.Context, p IdentityVerdictPayload) (string, error) {
	if e.cfg.IdentityByName(p.Name) == nil {
		return "", fmt.Errorf("unknown identity %q", p.Name)
	}
	e.recordIdentityVerdict(ctx, p.Name, p.Pass, p.Reason)
	result, level := "identities check: pass", "info"
	if !p.Pass {
		result, level = "identities check: fail: "+p.Reason, "warn"
	}
	e.event(ctx, level, "identity:"+p.Name, "identity.checked", result, nil)
	return result, nil
}

// resolve loads the PR a request names.
func (e *Engine) resolve(ctx context.Context, t PRTarget) (store.Repo, store.PR, error) {
	ref := t.Ref
	if t.Repo != "" && t.Number > 0 {
		ref = fmt.Sprintf("%s#%d", t.Repo, t.Number)
	}
	if ref == "" {
		return store.Repo{}, store.PR{}, errors.New("request names no PR")
	}
	return app.LookupPR(ctx, e.st, app.RefParser{DefaultRepo: e.cfg.Daemon.DefaultRepo}, ref)
}

// requestReview forces a round: forced=1, throttle and backoff cleared, the
// PR queued (or rereview_pending); Candidates then puts it first. A PR
// GitHub merged gets a post-merge review (post_merge.go): it leaves closed
// or released the same way, unless its merged head was reviewed already or
// its checkout is being released; a PR closed without merging is refused.
func (e *Engine) requestReview(ctx context.Context, p ReviewPayload) (string, error) {
	repo, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		return "", err
	}
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	w := e.cfg.WatchFor(repo.FullName())
	merged := postMerge(pr)
	switch {
	case w == nil:
		return "", fmt.Errorf("%s is not watched (no [[watch]] covers it)", repo.FullName())
	case slices.Contains(store.InFlightStates, pr.State) || e.roundActive(pr.ID):
		return "", fmt.Errorf("%s: a round is already in progress (%s)", label, pr.State)
	case merged:
		if why := postMergeRefusal(label, pr); why != "" {
			return "", errors.New(why)
		}
	case pr.GHState == store.GHClosed:
		return "", fmt.Errorf("%s was closed without merging (%s, GitHub %s): only open or merged PRs are reviewed", label, pr.State, pr.GHState)
	case slices.Contains(closingStates, pr.State) || pr.GHState != store.GHOpen:
		return "", fmt.Errorf("%s is not open (%s, GitHub %s): only open or merged PRs are reviewed", label, pr.State, pr.GHState)
	}
	if p.As != "" && e.cfg.IdentityByName(p.As) == nil {
		return "", fmt.Errorf("unknown identity %q", p.As)
	}
	asked := slices.Clone(p.Roles)
	if p.Simplify {
		asked = append(asked, "simplify")
	}
	requested, err := e.resolveRequested(repo, asked)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	// The dry-run marker is settled before the PR is forced: a forced PR
	// must never run in the wrong mode because a kv write failed.
	wroteMarker, err := e.markDryRun(ctx, pr, p.DryRun)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	to := claimableState(pr)
	err = e.st.TransitionPR(ctx, pr.ID, []string{pr.State}, to, func(u *store.PRUpdate) {
		u.Set("forced", true)
		u.Set("next_eligible_at", nil)
		u.Set("next_attempt_at", nil)
		u.Set("attempts", 0)
		u.Set("skip_reason", nil)
		u.Set("last_error", nil)
		if p.As != "" {
			u.Set("identity", p.As)
		}
	})
	if err != nil {
		if wroteMarker {
			e.delKV(ctx, kvPRDryRun(pr.ID))
		}
		return "", fmt.Errorf("%s: %w", label, err)
	}
	e.addRequested(ctx, pr.ID, requested...)
	if p.As != "" {
		e.setKV(ctx, KVPRIdentityPinned(pr.ID), p.As) // never migrated to the watch's identity
	}
	if p.Fresh {
		e.setKV(ctx, kvPRFresh(pr.ID), "1")
	}
	if p.Replies {
		e.setKV(ctx, kvPRRedecide(pr.ID), "1")
	}
	e.seeStalemates(ctx, pr.ID) // the operator acts on the PR: the threads magnum stopped arguing in are seen
	forced := "forced"
	if merged {
		forced = "forced, post-merge"
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.forced", fmt.Sprintf("magnum review: %s → %s (%s)", pr.State, to, forced), nil)
	// A forced review should not wait for the periodic identity re-check when
	// the identity it posts as (the one --as names, else the PR's) was marked
	// unhealthy (for example by a judge whose own check hit a transient
	// network error): re-check it now.
	posting := pr.Identity
	if p.As != "" {
		posting = p.As
	}
	if ok, _ := e.identityHealthy(ctx, posting); !ok {
		e.warmIdentities(ctx, false)
	}
	unpinned, slotHeld := e.unpinForReview(ctx, repo, pr) // review_unpin.go
	pos := 0
	if cands, err := e.st.Candidates(ctx, store.CandidateParams{Now: e.now()}); err == nil {
		for i, c := range cands {
			if c.ID == pr.ID {
				pos = i + 1
			}
		}
	}
	res := fmt.Sprintf("queued %s (%s, forced)", label, to)
	if p.DryRun {
		res = fmt.Sprintf("queued %s (%s, forced, dry run: nothing is posted)", label, to)
	}
	if len(requested) > 0 {
		res += " with " + strings.Join(requested, ", ")
	}
	if merged {
		res += ": " + postMergeScope(pr)
	}
	if pos > 0 {
		res += fmt.Sprintf(", position %d", pos)
	}
	if reason := e.holdReason(ctx); reason != "" { // a forced review passes `magnum pause`
		res += "; waiting: " + reason
	} else if reason := e.kindPauseReason(ctx, agentKinds(e.cfg.RolesFor(w))); reason != "" {
		res += "; waiting: " + reason
	}
	if unpinned != "" {
		res += "; " + unpinned
	}
	if slotHeld != "" {
		res += "; waiting: " + slotHeld
	}
	return res, nil
}

// markDryRun settles the PR's dry-run marker for a review request before the
// PR is forced: a dry run stores the state the PR returns to afterwards (a
// request replayed after a crash, or a second one while the first is
// pending, keeps the state from before the first); a real one removes any
// marker. It reports whether it wrote a new marker. Errors are returned, so
// the request fails instead of running in the wrong mode.
func (e *Engine) markDryRun(ctx context.Context, pr store.PR, dry bool) (bool, error) {
	key := kvPRDryRun(pr.ID)
	if !dry {
		if err := e.st.DeleteKV(ctx, key); err != nil {
			return false, fmt.Errorf("clear the dry-run marker: %w", err)
		}
		return false, nil
	}
	_, ok, err := e.st.GetKV(ctx, key)
	if err != nil {
		return false, fmt.Errorf("read the dry-run marker: %w", err)
	}
	if ok && pr.Forced {
		return false, nil
	}
	if err := e.st.SetKV(ctx, key, pr.State); err != nil {
		return false, fmt.Errorf("set the dry-run marker: %w", err)
	}
	return true, nil
}

// resolveRequested resolves requested role names or aliases against the
// roles of the repository's watch; an unknown role, the judge (it always
// runs) and a runs = "never" role are refused.
func (e *Engine) resolveRequested(repo store.Repo, asked []string) ([]string, error) {
	w := e.cfg.WatchFor(repo.FullName())
	var out []string
	for _, s := range asked {
		if strings.TrimSpace(s) == "" {
			continue
		}
		r, ok := e.cfg.RoleByNameOrAlias(w, s)
		switch {
		case !ok:
			return nil, fmt.Errorf("unknown role %q (the roles of %s: %s)", s, repo.FullName(), strings.Join(roleNames(e.cfg.RolesFor(w)), ", "))
		case r.Runs == config.RunsNever:
			return nil, fmt.Errorf("role %s is disabled (runs = \"never\")", r.Name)
		case r.Judge:
			continue // the judge runs every round anyway
		}
		if !slices.Contains(out, r.Name) {
			out = append(out, r.Name)
		}
	}
	return out, nil
}

func (e *Engine) requestPin(ctx context.Context, p TargetPayload, pin bool) (string, error) {
	verb := map[bool]string{true: "pinned", false: "unpinned"}[pin]
	var slot store.Slot
	hasSlot := false
	label, subject := p.Slot, ""
	if p.Slot != "" {
		sl, err := e.st.SlotByName(ctx, p.Slot)
		if err != nil {
			return "", err
		}
		slot, hasSlot = sl, true
		if sl.PRID != nil {
			_ = e.st.UpdatePR(ctx, *sl.PRID, func(u *store.PRUpdate) { u.Set("pinned", pin) })
		}
	} else {
		repo, pr, err := e.resolve(ctx, p.PRTarget)
		if err != nil {
			return "", err
		}
		label, subject = fmt.Sprintf("%s#%d", repo.FullName(), pr.Number), prSubject(repo, pr.Number)
		if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("pinned", pin) }); err != nil {
			return "", err
		}
		if slot, hasSlot, err = e.slotOf(ctx, pr.ID); err != nil {
			return "", err
		}
	}
	if hasSlot && e.d.Slots != nil {
		var err error
		if pin {
			err = e.d.Slots.Pin(ctx, slot)
		} else {
			err = e.d.Slots.Unpin(ctx, slot)
		}
		if err != nil {
			return "", fmt.Errorf("%s %s: %w", verb, slot.Name, err)
		}
		label += " (slot " + slot.Name + ")"
	}
	e.pinEvent(ctx, subject, slot, p.Slot != "", pin, verb+" "+label) // review_unpin.go
	return verb + " " + label, nil
}

// requestMute mutes or unmutes a PR. Muting a PR GitHub no longer lists as
// open (merged or closed) also clears its forced mark, unless a post-merge
// round is due or running: the mark a review request left on a PR that closed
// before its round ran would keep the PR flagged merged unreviewed
// (store.IsMergedUnreviewed counts a muted PR that is forced), and the mute is
// how the user dismisses that flag. The clear is its own compare-and-set on the
// state the decision read, so a post-merge review requested in between keeps
// its mark. An unmute never sets the mark again: the flag comes back with the
// PR unmuted.
//
// A mute takes a PR waiting for an automatic round (queued, rereview_pending,
// not forced) out of the queue at once: it becomes ineligible with the
// filters' reason, as the next restart's reclassification would make it, and
// an unmute decides its eligibility again. The mute's reason (p.Reason) goes
// into the answer and the pr.muted event.
func (e *Engine) requestMute(ctx context.Context, p TargetPayload, mute bool) (string, error) {
	repo, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		return "", err
	}
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	reason := clipRunes(strings.Join(strings.Fields(p.Reason), " "), muteReasonRunes)
	if mute && reason != "" {
		label += " (" + reason + ")"
	}
	if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) {
		u.Set("muted", mute)
		if !mute && deref(pr.SkipReason) == skipIgnored {
			u.Set("skip_reason", nil) // magnum ignore's mark goes with its mute
		}
	}); err != nil {
		return "", err
	}
	dismissed := false
	if mute && pr.Forced && pr.GHState != store.GHOpen && !slices.Contains(postMergeRoundStates, pr.State) {
		switch err := e.st.TransitionPR(ctx, pr.ID, []string{pr.State}, "", func(u *store.PRUpdate) {
			u.Set("forced", false)
		}); {
		case err == nil:
			cur, err := e.st.PRByID(ctx, pr.ID)
			dismissed = err == nil && pr.MergedUnreviewed() && !cur.MergedUnreviewed()
		case errors.Is(err, store.ErrConflict):
			e.log.Info("PR moved on while it was muted; its forced mark stays", "pr", pr.ID, "state", pr.State)
		default:
			return "", err
		}
	}
	if !mute && pr.State == store.PRIneligible {
		if w := e.cfg.WatchFor(repo.FullName()); w != nil {
			cur, err := e.st.PRByID(ctx, pr.ID)
			if err == nil {
				res := store.PRUpsert{PR: cur, Changed: true}
				_ = e.onSeenPR(ctx, repo, *w, pr, res, e.now())
			}
		}
	}
	if mute && !pr.Forced && slices.Contains(waitingStates, pr.State) {
		// A round that claimed the PR in between finds it muted at its end.
		if err := e.markIneligible(ctx, pr, waitingStates, skipMuted, false, e.now()); err != nil && !errors.Is(err, store.ErrConflict) {
			return "", err
		}
	}
	if mute {
		msg, data := "muted", map[string]any{}
		if reason != "" {
			msg, data["reason"] = "muted: "+reason, reason
		}
		e.event(ctx, "info", prSubject(repo, pr.Number), evPRMuted, msg, data)
		e.seeStalemates(ctx, pr.ID) // stalemate.go
		if dismissed {
			return "muted " + label + ": merged-unreviewed flag dismissed", nil
		}
		return "muted " + label, nil
	}
	return "unmuted " + label, nil
}

const (
	// evPRMuted records a mute and its reason (requestMute).
	evPRMuted = "pr.muted"
	// skipMuted is the skip reason of a muted PR: eligibility.Classify's.
	skipMuted = "muted"
	// muteReasonRunes bounds a mute's reason.
	muteReasonRunes = 200
)

// waitingStates are the states of a PR waiting for a round.
var waitingStates = []string{store.PRQueued, store.PRRereviewPending}

func (e *Engine) requestPause(ctx context.Context, p PausePayload, pause bool) (string, error) {
	if pause {
		if v, _ := e.getKV(ctx, KVDaemonPaused); v != "1" { // a pause renewed keeps its start
			e.setKV(ctx, KVDaemonPausedAt, store.FormatTime(e.now()))
		}
		e.setKV(ctx, KVDaemonPaused, "1")
		e.setKV(ctx, KVDaemonPausedReason, p.Reason)
		if p.Until != nil && !p.Until.IsZero() {
			e.setKV(ctx, KVDaemonPausedUntil, store.FormatTime(*p.Until))
		} else {
			e.delKV(ctx, KVDaemonPausedUntil)
		}
		e.event(ctx, "info", "", "daemon.paused", strings.TrimSpace("automation paused "+p.Reason), nil)
		return "paused", nil
	}
	switch {
	case p.Watch != "":
		_, paused := e.getKV(ctx, KVWatchPaused(p.Watch))
		if !paused && !slices.ContainsFunc(e.cfg.Watches, func(w config.Watch) bool { return strings.EqualFold(w.Owner, p.Watch) }) {
			return "", fmt.Errorf("no [[watch]] for %q and no pause recorded for it", p.Watch)
		}
		e.delKV(ctx, KVWatchPaused(p.Watch))
		return "resumed watch " + p.Watch, nil
	case p.Tool == "all":
		var cleared []string
		for _, t := range e.kinds() {
			e.clearToolPause(ctx, t)
			cleared = append(cleared, e.clearModelLimits(ctx, t)...)
		}
		return "resumed " + strings.Join(e.kinds(), ", ") + modelLimitsNote(cleared), nil
	case p.Tool != "":
		if !e.isKind(p.Tool) {
			return "", fmt.Errorf("unknown tool %q (%s or all)", p.Tool, strings.Join(e.kinds(), ", "))
		}
		e.clearToolPause(ctx, p.Tool)
		return "resumed " + p.Tool + modelLimitsNote(e.clearModelLimits(ctx, p.Tool)), nil
	}
	e.delKV(ctx, KVDaemonPaused, KVDaemonPausedReason, KVDaemonPausedUntil, KVDaemonPausedAt, KVDaemonPausedHeld)
	if _, ok := e.infraPause(ctx); ok {
		e.clearInfraPause(ctx, false)
		e.event(ctx, "info", "", "infra.resumed", "infrastructure pause lifted (magnum resume)", nil)
	}
	e.event(ctx, "info", "", "daemon.resumed", "automation resumed", nil)
	return "resumed", nil
}

// clearModelLimits lifts kind's per-model limits (ClearModelLimits) and
// returns them as "kind/model".
func (e *Engine) clearModelLimits(ctx context.Context, kind string) []string {
	models, err := ClearModelLimits(ctx, e.st, kind)
	if err != nil {
		e.log.Warn("clear model limits", "kind", kind, "err", err)
	}
	out := make([]string, len(models))
	for i, m := range models {
		out[i] = kind + "/" + m
	}
	if len(out) > 0 {
		e.event(ctx, "info", "tool:"+kind, "tool.model_limits_cleared", "model limits cleared: "+strings.Join(out, ", "), nil)
	}
	return out
}

// modelLimitsNote is ` (model limits cleared: codex/x)` for a resume's answer.
func modelLimitsNote(cleared []string) string {
	if len(cleared) == 0 {
		return ""
	}
	return " (model limits cleared: " + strings.Join(cleared, ", ") + ")"
}

// cleanupAsync hands a release/cleanup request to the heavy worker, which
// completes it with the rendered report.
func (e *Engine) cleanupAsync(ctx context.Context, id int64, p CleanupPayload) {
	if e.d.Cleanup == nil {
		e.complete(ctx, id, errors.New("cleanup is not available"), "")
		return
	}
	e.heavyRequest(ctx, id, func(ctx context.Context) (string, error) {
		plan := p.Plan
		if plan == nil {
			opts := p.Options
			opts.DryRun = opts.DryRun || e.d.DryRun
			pl, err := e.d.Cleanup.Plan(ctx, opts)
			if err != nil {
				return "", err
			}
			plan = &pl
		}
		if e.d.DryRun {
			plan.DryRun = true
		}
		rep, err := e.d.Cleanup.Apply(ctx, *plan, p.Confirmed)
		out := strings.TrimSpace(cleanup.Render(*plan) + "\n" + cleanup.RenderReport(rep))
		if err != nil {
			return "", fmt.Errorf("%s\n%w", out, err)
		}
		return out, nil
	})
}

// heavyRequest runs fn for request id on the heavy worker and completes the
// request with its result. The request stays in flight (skipped by
// handleRequests) until then; a full queue leaves it pending for the next
// tick.
func (e *Engine) heavyRequest(ctx context.Context, id int64, fn func(ctx context.Context) (string, error)) {
	e.mu.Lock()
	e.inflight[id] = true
	e.mu.Unlock()
	done := func() {
		e.mu.Lock()
		delete(e.inflight, id)
		e.mu.Unlock()
	}
	queued := e.enqueueHeavy(fmt.Sprintf("request:%d", id), func(ctx context.Context) error {
		defer done()
		res, err := fn(ctx)
		if ctx.Err() != nil {
			return err // shutting down: the request stays pending for the next daemon (steps resume)
		}
		e.complete(ctx, id, err, res)
		return err
	})
	if !queued {
		done()
	}
}
