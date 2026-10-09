package engine

// The Codex flag (DECISIONS "A Codex safety warning ends the round and
// flags the PR"): Codex flagged a PR's review as a possible cybersecurity
// risk (pipeline.OutcomeRefused; Codex may block an account it takes for a
// cyber abuser, though all magnum does is review the team's own code), so
// magnum never reviews that PR again: no automatic round on any head, no
// retry, no continue, no reply round or delta check, and no forced round
// either (`magnum review` and the board's review keys refuse with the
// reason). The flag is the PR's store.KVPRCodexFlag record (the refused
// head, role, run and time), set by the refused round (onRefused, one
// toast) or by `magnum codex-flag set` for a PR found before, and lifted
// only by `magnum codex-flag clear`, which asks y/N naming the account
// risk. The engine's classify reads it like a filter (the PR is ineligible
// with the flag's reason, also after a push or a review request), and
// dispatch holds whatever still waits (holdFlagged).
//
// The flag fails closed (DECISIONS "A Codex flag fails closed"): a record
// that cannot be parsed is a flag, a registry that cannot be read holds the
// PR this time, and a refused round whose write failed keeps the flag in
// memory (unwritten) and writes it again every tick. Each tick a flagged PR
// that `magnum open` did not pin gives back what it holds (DECISIONS "A
// Codex-flagged PR gives back its slot and its agents", releaseFlagged):
// its idle agents are parked and its pool slot released.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// ReqCodexFlag flags a PR or clears its flag (CodexFlagPayload).
const ReqCodexFlag = "codex-flag"

// EvPRMuted is the event a mute records, its reason in the data's
// "reason" (requestMute); the board's card shows the latest one's next to
// the flag.
const EvPRMuted = "pr.muted"

// CodexFlagPayload is a `magnum codex-flag set|clear` request: flag the PR
// (Reason: why, from the operator) or clear its flag. By says who asks
// ("magnum codex-flag").
type CodexFlagPayload struct {
	PRTarget
	Clear  bool   `json:"clear,omitempty"`
	Reason string `json:"reason,omitempty"`
	By     string `json:"by,omitempty"`
}

// CodexFlag is why magnum never reviews a PR again: the agent kind whose
// provider flagged it, the role and run it refused (none when set by hand),
// the PR's head then, the refusal's line or the operator's reason, when and
// by whom ("round 3", "magnum codex-flag").
type CodexFlag struct {
	Kind   string    `json:"kind"`
	Role   string    `json:"role,omitempty"`
	Run    string    `json:"run,omitempty"`
	Head   string    `json:"head,omitempty"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
	By     string    `json:"by,omitempty"`
	// unread: the registry could not say whether the PR is flagged (the
	// read failed). It counts as flagged, but holdFlagged leaves the PR's
	// state alone: the next tick reads again.
	unread bool
}

// ParseCodexFlag reads a store.KVPRCodexFlag value. A value it cannot read
// is a flag still (UnreadableCodexFlag): magnum never reviews a PR on a
// flag it failed to read.
func ParseCodexFlag(s string) CodexFlag {
	var f CodexFlag
	if json.Unmarshal([]byte(s), &f) != nil {
		return UnreadableCodexFlag("its record is no flag")
	}
	return f
}

// UnreadableCodexFlag is the flag of a PR whose flag record could not be
// read (why): it counts as flagged.
func UnreadableCodexFlag(why string) CodexFlag {
	return CodexFlag{Kind: config.KindCodex, Detail: "the flag could not be read (" + why + "), so it counts"}
}

// Who names the provider that flagged the PR: "Codex".
func (f CodexFlag) Who() string { return agents.KindName(cmp.Or(f.Kind, config.KindCodex)) }

// Short is the flag in one cell: "Codex flagged · never reviewed again".
func (f CodexFlag) Short() string { return f.Who() + " flagged · never reviewed again" }

// SkipReason is the flagged PR's skip_reason.
func (f CodexFlag) SkipReason() string {
	if f.unread {
		return "its " + f.Who() + " flag could not be read: not reviewed until it can be"
	}
	return f.Who() + " flagged it as a possible cybersecurity risk: never reviewed again"
}

// Sentence is the flag in full, for the card, `magnum status` and the
// refusals: when and where it was flagged, what happens and how to lift it
// (ref names the PR on the command line).
func (f CodexFlag) Sentence(ref string) string {
	s := f.Who() + " flagged this PR as a possible cybersecurity risk"
	if !f.At.IsZero() {
		s += " on " + f.At.Local().Format("Jan 2 15:04")
	}
	var where []string
	if f.Role != "" {
		where = append(where, f.Role)
	}
	if f.Run != "" {
		where = append(where, "run "+f.Run)
	}
	if f.Head != "" {
		where = append(where, "head "+textx.ShortSHA(f.Head))
	}
	if f.Role == "" && f.By != "" {
		where = append(where, "set by "+f.By)
	}
	if len(where) > 0 {
		s += " (" + strings.Join(where, ", ") + ")"
	}
	if f.Detail != "" {
		s += ": " + textx.Clip(f.Detail, 160)
	}
	return s + ". magnum never reviews it again, not even `magnum review`: review it by hand; `magnum codex-flag clear " + ref +
		"` lifts the flag (" + f.Who() + " may block an account it flags)"
}

func (f CodexFlag) marshal() (string, error) {
	b, err := json.Marshal(f)
	return string(b), err
}

// codexFlag is the PR's flag, when it has one: one a refused round could
// not write yet (unwritten), else its record, whatever it holds
// (ParseCodexFlag). A registry that cannot be read counts as a flag
// (unread), so nothing starts on a PR whose flag magnum could not read.
func (e *Engine) codexFlag(ctx context.Context, prID int64) (CodexFlag, bool) {
	if f, ok := e.unwrittenFlag(prID); ok {
		return f, true
	}
	v, ok, err := e.st.GetKV(ctx, store.KVPRCodexFlag(prID))
	switch {
	case err != nil:
		e.warnUnlessStopped(ctx, err, "read a Codex flag", "pr", prID)
		f := UnreadableCodexFlag(err.Error())
		f.unread = true
		return f, true
	case !ok:
		return CodexFlag{}, false
	}
	return ParseCodexFlag(v), true
}

// setCodexFlag records f as the PR's flag.
func (e *Engine) setCodexFlag(ctx context.Context, prID int64, f CodexFlag) error {
	v, err := f.marshal()
	if err != nil {
		return err
	}
	return e.st.SetKV(ctx, store.KVPRCodexFlag(prID), v)
}

// unwrittenFlag is the flag a refused round could not write for the PR.
func (e *Engine) unwrittenFlag(prID int64) (CodexFlag, bool) {
	e.flagMu.Lock()
	defer e.flagMu.Unlock()
	f, ok := e.unwritten[prID]
	return f, ok
}

// keepUnwritten keeps f, which could not be written, as the PR's flag
// until writeUnwritten writes it (or the flag is cleared).
func (e *Engine) keepUnwritten(prID int64, f CodexFlag) {
	e.flagMu.Lock()
	defer e.flagMu.Unlock()
	if e.unwritten == nil {
		e.unwritten = map[int64]CodexFlag{}
	}
	e.unwritten[prID] = f
}

// dropUnwritten forgets the PR's unwritten flag (written, or cleared).
func (e *Engine) dropUnwritten(prID int64) {
	e.flagMu.Lock()
	defer e.flagMu.Unlock()
	delete(e.unwritten, prID)
}

// writeUnwritten writes the flags refused rounds could not write, each
// tick until the write takes; a PR flagged since keeps its flag (the first
// one stands).
func (e *Engine) writeUnwritten(ctx context.Context) {
	e.flagMu.Lock()
	ids := slices.Sorted(maps.Keys(e.unwritten))
	e.flagMu.Unlock()
	for _, id := range ids {
		f, ok := e.unwrittenFlag(id)
		if !ok {
			continue
		}
		if _, has, err := e.st.GetKV(ctx, store.KVPRCodexFlag(id)); err == nil && has {
			e.dropUnwritten(id)
			continue
		}
		if err := e.setCodexFlag(ctx, id, f); err != nil {
			e.warnUnlessStopped(ctx, err, "write a Codex flag again", "pr", id)
			continue
		}
		e.dropUnwritten(id)
		e.log.Info("wrote the Codex flag a refused round could not write", "pr", id)
	}
}

// flagTick is the Codex flag's part of the tick: the flags refused rounds
// could not write are written again (writeUnwritten), and the flagged PRs
// give back their agents and slots (releaseFlagged, park.go).
func (e *Engine) flagTick(ctx context.Context, ts tickState) {
	e.writeUnwritten(ctx)
	e.releaseFlagged(ctx, ts)
}

// codexFlagRefusal is why a round asked for (magnum review, the board's
// review keys) does not start on a flagged PR ("" = it is not flagged).
func (e *Engine) codexFlagRefusal(ctx context.Context, label string, pr store.PR) string {
	f, ok := e.codexFlag(ctx, pr.ID)
	if !ok {
		return ""
	}
	return label + ": " + f.Sentence(label)
}

// flagHolds takes a flagged candidate out of dispatch (holdFlagged) and
// reports whether it was flagged (or its flag could not be read).
func (e *Engine) flagHolds(ctx context.Context, pr store.PR) bool {
	f, ok := e.codexFlag(ctx, pr.ID)
	if !ok {
		return false
	}
	e.holdFlagged(ctx, pr, f)
	return true
}

// flagHeldStates are the states holdFlagged takes a flagged PR out of.
var flagHeldStates = []string{store.PRQueued, store.PRRereviewPending, store.PRPaused, store.PRNeedsAttention, store.PRBaseline}

// holdFlagged takes a flagged PR out of line: one that waits for a round
// (queued or rereview_pending, forced or not), paused mid-turn, needing
// attention or never reviewed becomes ineligible with the flag's reason (a
// merged PR's post-merge round: closed again), and its forced mark and what
// a request asked for this round go. A reviewed PR keeps its review, an
// ineligible one its reason (a mute, a filter), and a round in flight
// finishes. A flag the registry could not read moves nothing: dispatch
// passes the PR over this tick, the next one reads again. It reports
// whether it moved the PR.
func (e *Engine) holdFlagged(ctx context.Context, pr store.PR, f CodexFlag) bool {
	if f.unread || !slices.Contains(flagHeldStates, pr.State) {
		return false
	}
	to := store.PRIneligible
	if postMerge(pr) {
		to = store.PRClosed
	}
	err := e.st.TransitionPR(ctx, pr.ID, []string{pr.State}, to, func(u *store.PRUpdate) {
		if to == store.PRClosed {
			u.Set("release_after", e.releaseAfter())
		} else {
			u.Set("skip_reason", f.SkipReason())
		}
		u.Set("forced", false)
		u.Set("next_attempt_at", nil)
		u.Set("next_eligible_at", nil)
	})
	if err != nil {
		e.log.Info("hold a flagged PR", "pr", pr.ID, "err", err)
		return false
	}
	e.delKV(ctx, store.KVPRDryRun(pr.ID), store.KVPRFresh(pr.ID), kvPRRedecide(pr.ID), store.KVPRGate(pr.ID), kvPRGateReason(pr.ID), KVPRWait(pr.ID))
	e.clearRequested(ctx, pr.ID)
	repo, _ := e.st.RepoByID(ctx, pr.RepoID)
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.codex_flag_held", fmt.Sprintf("%s → %s: %s", pr.State, to, f.SkipReason()),
		map[string]any{"from": pr.State, "to": to})
	return true
}

// onRefused ends a round its provider refused (pipeline.OutcomeRefused):
// the PR is flagged for good (the refused head, role, run and time), the
// PR's agents still working are interrupted as an abort does
// (interruptPR; the pipeline cut the turns it ran), a kept approval goes,
// the PR becomes ineligible with the flag's reason and the refusal as its
// last error (a post-merge round's PR: closed again), and one toast says so.
// A flag it cannot write is kept in memory, which holds the PR as the
// record would, and written again every tick (writeUnwritten). The tick it
// asks for (Kick) parks the PR's agents and releases its slot
// (releaseFlagged) once the round let go of the PR.
func (e *Engine) onRefused(ctx context.Context, job *roundJob, pr store.PR, in pipeline.RoundInput, res pipeline.RoundResult, from []string) {
	now := e.now()
	ref := pipeline.Refusal{Detail: res.Error}
	if res.Refusal != nil {
		ref = *res.Refusal
	}
	msg := cmp.Or(res.Error, ref.Sentence())
	f := CodexFlag{Kind: ref.Kind, Role: ref.Role, Run: ref.RunID, Head: in.TargetSHA, Detail: ref.Detail, At: now.UTC(),
		By: fmt.Sprintf("round %d", res.Round)}
	if err := e.setCodexFlag(context.WithoutCancel(ctx), pr.ID, f); err != nil {
		e.keepUnwritten(pr.ID, f)
		e.log.Warn("flag a refused PR: the write failed; the flag holds the PR and is written again every tick", "pr", pr.ID, "err", err)
	}
	defer e.Kick()
	subject := prSubject(job.repo, pr.Number)
	if n := e.interruptPR(ctx, pr, &job.watch); n > 0 { // abort.go
		e.log.Info("refused round: agents interrupted", "subject", subject, "agents", n)
	}
	e.keptApprovalFailed(ctx, job.repo, pr, f.Who()+" refused its round")
	to := store.PRIneligible
	if job.postMerge {
		to = store.PRClosed
	}
	err := e.st.TransitionPR(ctx, pr.ID, from, to, func(u *store.PRUpdate) {
		if to == store.PRClosed {
			u.Set("release_after", e.releaseAfter())
		} else {
			u.Set("skip_reason", f.SkipReason())
		}
		u.Set("last_error", msg)
		u.Set("forced", false)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
		u.Set("next_eligible_at", nil)
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		e.log.Warn("refused round: PR state", "pr", pr.ID, "err", err)
	}
	e.delKV(ctx, store.KVPRDryRun(pr.ID), kvPRRedecide(pr.ID))
	e.clearRequested(ctx, pr.ID)
	if job.hasSlot {
		_ = e.st.TransitionSlot(ctx, job.slot.ID, []string{store.SlotClaimed, store.SlotBusy}, store.SlotHeld, nil)
	}
	label := fmt.Sprintf("%s#%d", job.repo.FullName(), pr.Number)
	e.event(ctx, "error", subject, "pr.codex_flagged", msg+"; flagged: never reviewed again",
		map[string]any{"kind": f.Kind, "role": f.Role, "run": f.Run, "head": f.Head, "by": f.By})
	e.urgent(fmt.Sprintf("codex-flag:%d", pr.ID), fmt.Sprintf("magnum: %s#%d: %s", job.repo.Name, pr.Number, f.Short()),
		msg+". Review it by hand; `magnum codex-flag clear "+label+"` lifts the flag ("+f.Who()+" may block an account it flags).", attentionWindow)
}

// requestCodexFlag flags a PR by hand (magnum codex-flag set: one found
// before, or warned about elsewhere) or clears its flag (clear: the CLI
// asked y/N naming the account risk). A flagged PR waiting for a round
// leaves the line (holdFlagged); a round in flight finishes, and the answer
// says how to stop it. A cleared PR is judged by its watch again (a PR the
// flag held becomes reviewed or waits in line, as after any filter change).
func (e *Engine) requestCodexFlag(ctx context.Context, p CodexFlagPayload) (string, error) {
	repo, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		return "", err
	}
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	subject := prSubject(repo, pr.Number)
	by := cmp.Or(p.By, "magnum codex-flag")
	now := e.now()
	if p.Clear {
		if _, ok := e.codexFlag(ctx, pr.ID); !ok {
			return label + " is not flagged", nil
		}
		if err := e.st.DeleteKV(ctx, store.KVPRCodexFlag(pr.ID)); err != nil {
			return "", err
		}
		e.dropUnwritten(pr.ID)
		e.event(ctx, "warn", subject, "pr.codex_flag_cleared", "the Codex flag was cleared by "+by+": magnum reviews the PR again",
			map[string]any{"by": by})
		if w := e.cfg.WatchFor(repo.FullName()); w != nil && pr.State == store.PRIneligible {
			if cur, err := e.st.PRByID(ctx, pr.ID); err == nil {
				_ = e.onSeenPR(ctx, repo, *w, cur, store.PRUpsert{PR: cur, Changed: true}, now)
			}
		}
		cur, _ := e.st.PRByID(ctx, pr.ID)
		return fmt.Sprintf("cleared the Codex flag of %s: magnum reviews it again (now %s)", label, cmp.Or(cur.State, pr.State)), nil
	}
	reason := clipRunes(strings.Join(strings.Fields(p.Reason), " "), muteReasonRunes)
	f := CodexFlag{Kind: config.KindCodex, Head: pr.HeadSHA, Detail: reason, At: now.UTC(), By: by}
	if prev, ok := e.codexFlag(ctx, pr.ID); ok && !prev.unread {
		f = prev // the first flag stands
	} else if err := e.setCodexFlag(ctx, pr.ID, f); err != nil {
		return "", err
	}
	msg := "flagged " + label + " by hand"
	if reason != "" {
		msg += ": " + reason
	}
	e.event(ctx, "warn", subject, "pr.codex_flagged", msg, map[string]any{"by": by, "head": f.Head})
	res := "flagged " + label + ": magnum never reviews it again, not even `magnum review` (`magnum codex-flag clear " + label + "` lifts it)"
	switch {
	case e.roundActive(pr.ID) || slices.Contains(store.InFlightStates, pr.State):
		res += "; the round in flight finishes, `magnum abort " + label + "` stops it now"
	case e.holdFlagged(ctx, pr, f):
		res += "; its waiting round was taken back"
	}
	return res, nil
}
