package engine

// The factory's lease of a PR's fixes, as magnum reads it (Deps.FixLeaseOf:
// the factory's registry, read only, across processes), and what the
// review side does with it (DECISIONS "The fix loop moves into the
// factory": the review side fails open). For a PR the factory fixes (its
// owner row, fix_owner.go), dispatch holds the PR's rounds while the
// factory's fixer works on it (phase fixing), as it holds them for magnum's
// own fixer (fixHolds, WaitFixing), `magnum review` is refused while the
// factory holds the lease, and the status comment says that the factory
// fixes it. A read that fails (the owner row or the lease) holds the round
// for fixLeaseGrace from the first failure, then fails open: magnum
// reviews, a warn event says so once, and the PR's row (KVPRFixLeaseUnread,
// a key-value row: no migration) shows on the doctor's line until a read
// works again. A nil reader is inert: no lease is read, nothing is held or
// refused, no event or doctor line, as before the factory fixed PRs.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factoryread"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// FixLease is what the factory's registry says of its fix work on a PR:
// Fixing, the factory holds the PR's lease and works on it now, in Phase
// (FixPhaseFixing while its fixer works, FixPhaseReviewing while it waits
// for magnum's re-review of its push), on Head, in cycle Cycle of Cap (0:
// not known); Worked, the factory worked the PR since its import (a start,
// a push, a reply or a change of its ledger), which keeps it the factory's
// (`magnum fix --handover back` refuses it); ReadAt, when the reader read
// it. The zero lease is a PR the factory has no record of.
type FixLease struct {
	Fixing bool
	Phase  string
	Head   string
	Cycle  int
	Cap    int
	Worked bool
	ReadAt time.Time
}

// leaseFromFactory is the factory's lease as factoryread reads it from its
// PR work row, as the review side takes it, read at now: a lease that holds
// an agent or a push (factoryread.Lease.Fixing: phase fixing, local_review
// or pushing, under a lease that has not run out) is the factory's fixer at
// work (FixPhaseFixing, which holds the PR's round), a loop that waits for
// magnum's re-review is FixPhaseReviewing, and any other (idle, queued,
// held, a lease that ran out) holds nothing.
func leaseFromFactory(l factoryread.Lease, now time.Time) FixLease {
	out := FixLease{Fixing: l.Fixing, Head: l.Head, Cycle: l.Cycle, Cap: l.Cap, Worked: l.Worked, ReadAt: now}
	switch {
	case l.Fixing:
		out.Phase = FixPhaseFixing
	case l.Phase == registry.PRWorkReviewing:
		out.Phase = FixPhaseReviewing
	}
	return out
}

// factoryLeaseReader is Deps.FixLeaseOf over the factory's registry at path
// (factoryread.LeaseOf, read only, across processes), each read stamped by
// now: a registry, a table or a row that is not there is the zero lease,
// and a registry that cannot be read is an error.
func factoryLeaseReader(path string, now func() time.Time) func(ctx context.Context, repo string, number int) (FixLease, error) {
	return func(ctx context.Context, repo string, number int) (FixLease, error) {
		l, err := factoryread.LeaseOf(ctx, path, repo, number)
		if err != nil {
			return FixLease{}, err
		}
		return leaseFromFactory(l, now()), nil
	}
}

// fixLeaseGrace is how long a round waits on a PR the factory fixes while
// its lease cannot be read, from the first failed read: then magnum
// reviews it anyway (the review side fails open).
const fixLeaseGrace = 10 * time.Minute

// KVPRFixLeaseUnread holds, while it lasts, the failed read of the
// factory's lease of PR prID (LeaseUnread as JSON): the doctor names it.
func KVPRFixLeaseUnread(prID int64) string { return fmt.Sprintf("pr.%d.fix_lease_unread", prID) }

// LeaseUnread is a KVPRFixLeaseUnread row: the first failed read (Since),
// the last one's error in a line (Err, a registry's words, never PR text)
// and Open, the grace ended and magnum reviews the PR without the lease.
type LeaseUnread struct {
	Since time.Time `json:"since"`
	Err   string    `json:"err"`
	Open  bool      `json:"open,omitempty"`
}

// leaseView is what the review side reads of the factory's fixes of a PR:
// owned, the PR has an owner row (the factory's, or returning), its lease,
// and err, the owner row or the lease could not be read.
type leaseView struct {
	owned bool
	lease FixLease
	err   error
}

// fixing reports whether the factory works on the PR now (its lease held,
// in any phase).
func (v leaseView) fixing() bool { return v.owned && v.err == nil && v.lease.Fixing }

// reviewing reports whether the factory's loop waits for magnum's re-review
// of its push (phase reviewing, a lease that holds no agent, as factoryread
// reads it).
func (v leaseView) reviewing() bool {
	return v.owned && v.err == nil && v.lease.Phase == FixPhaseReviewing
}

// holds reports whether the factory's fixer works on the PR (phase
// fixing): its rounds wait.
func (v leaseView) holds() bool { return v.fixing() && v.lease.Phase == FixPhaseFixing }

// leaseOf reads the factory's fixes of pr: its owner row, then, for a PR
// the factory fixes or returns, its lease. ok false: no reader is wired,
// and nothing is read.
func (e *Engine) leaseOf(ctx context.Context, pr store.PR) (v leaseView, ok bool) {
	if e.d.FixLeaseOf == nil {
		return leaseView{}, false
	}
	_, _, owned, err := e.fixOwner(ctx, pr.ID)
	if err != nil || !owned {
		return leaseView{owned: owned, err: err}, true
	}
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return leaseView{owned: true, err: err}, true
	}
	l, err := e.d.FixLeaseOf(ctx, repo.FullName(), pr.Number)
	return leaseView{owned: true, lease: l, err: err}, true
}

// leaseUnreadOf is pr's failed lease read, as recorded (ok false: none).
func (e *Engine) leaseUnreadOf(ctx context.Context, prID int64) (LeaseUnread, bool) {
	raw, ok := e.getKV(ctx, KVPRFixLeaseUnread(prID))
	if !ok {
		return LeaseUnread{}, false
	}
	var u LeaseUnread
	if json.Unmarshal([]byte(raw), &u) != nil || u.Since.IsZero() {
		return LeaseUnread{}, false
	}
	return u, true
}

// leaseWait is the wait of pr's round on the factory, from v (ok false:
// nothing of the factory's holds it): its fixer at work, or a failed read
// within its grace (u, as recorded; a first failure counts from now). A
// read past its grace holds nothing: magnum reviews.
func (e *Engine) leaseWait(v leaseView, u LeaseUnread, recorded bool) (Wait, bool) {
	now := e.now()
	if v.err != nil {
		since := now
		if recorded {
			since = u.Since
		}
		until := since.Add(fixLeaseGrace)
		if !now.Before(until) {
			return Wait{}, false
		}
		return Wait{Reason: WaitFixing, Subject: waitFactory, Until: until,
			Detail: fmt.Sprintf("the factory's lease, which cannot be read (%s)", leaseErrWords(v.err))}, true
	}
	if !v.holds() {
		return Wait{}, false
	}
	detail := fmt.Sprintf("the factory's fixer (fix cycle %d)", v.lease.Cycle)
	if v.lease.Cap > 0 {
		detail = fmt.Sprintf("the factory's fixer (fix cycle %d of %d)", v.lease.Cycle, v.lease.Cap)
	}
	return Wait{Reason: WaitFixing, Subject: waitFactory, Count: v.lease.Cycle, Max: v.lease.Cap, Detail: detail}, true
}

// waitFactory is Wait.Subject of a round that waits on the factory
// (WaitFixing): `magnum abort` stops no fixer of the factory's.
const waitFactory = "factory"

// leaseErrWords is a failed lease read in a line.
func leaseErrWords(err error) string { return oneLine(err.Error(), 200) }

// factoryHolds reports whether the factory holds pr's next round
// (dispatch): its fixer works on the PR, or a read failed within its grace.
// It records the failed read (KVPRFixLeaseUnread) the first time, and once
// its grace is over marks it open with a warn event (fix.lease_open): the
// round goes on without the lease. A read that works again clears it, with
// fix.lease_read. With no reader it holds nothing.
func (e *Engine) factoryHolds(ctx context.Context, pr store.PR) bool {
	if e.factorysOwnRound(ctx, pr) {
		return false
	}
	v, ok := e.leaseOf(ctx, pr)
	if !ok {
		return false
	}
	return e.noteLease(ctx, pr, v)
}

// noteLease is factoryHolds on v, read already: it records what a failed
// read or a working one changes, and reports whether the round waits.
func (e *Engine) noteLease(ctx context.Context, pr store.PR, v leaseView) bool {
	u, recorded := e.leaseUnreadOf(ctx, pr.ID)
	subject := fmt.Sprintf("pr:%d", pr.ID)
	if repo, err := e.st.RepoByID(ctx, pr.RepoID); err == nil {
		subject = prSubject(repo, pr.Number)
	}
	if v.err == nil {
		if recorded && !e.d.DryRun {
			e.delKV(ctx, KVPRFixLeaseUnread(pr.ID))
			e.event(ctx, "info", subject, "fix.lease_read", "the factory's lease of the PR reads again", nil)
		}
		_, held := e.leaseWait(v, u, recorded)
		return held
	}
	if !recorded {
		u = LeaseUnread{Since: e.now().UTC(), Err: leaseErrWords(v.err)}
		if !e.d.DryRun {
			e.setKV(ctx, KVPRFixLeaseUnread(pr.ID), rowJSON(u))
		}
		e.log.Warn("the factory's lease of a PR cannot be read; its round waits", "subject", subject, "grace", fixLeaseGrace, "err", v.err)
	}
	_, held := e.leaseWait(v, u, true)
	if !held && !u.Open && !e.d.DryRun {
		u.Open, u.Err = true, leaseErrWords(v.err)
		e.setKV(ctx, KVPRFixLeaseUnread(pr.ID), rowJSON(u))
		e.event(ctx, "warn", subject, "fix.lease_open", fmt.Sprintf("the factory's lease of the PR cannot be read since %s (%s): magnum reviews it "+
			"without the lease after the %s grace", waitClock(u.Since, e.now()), u.Err, humanDuration(fixLeaseGrace)), map[string]any{"since": u.Since})
	}
	return held
}

// factorysOwnRound reports whether pr's next round is the factory's own
// re-review of its fixer's push (forced, with a kvPRFixRef row,
// fix_factory.go), which no lease of the factory's holds.
func (e *Engine) factorysOwnRound(ctx context.Context, pr store.PR) bool {
	if !pr.Forced || e.d.FixLeaseOf == nil {
		return false
	}
	r, _ := e.fixRefOf(ctx, pr.ID)
	return r != nil
}

// factoryWaitOf is the wait of pr while the factory holds its round
// (waitFor; ok false: it does not), read without recording anything.
func (e *Engine) factoryWaitOf(ctx context.Context, pr store.PR) (Wait, bool) {
	if e.factorysOwnRound(ctx, pr) {
		return Wait{}, false
	}
	v, ok := e.leaseOf(ctx, pr)
	if !ok {
		return Wait{}, false
	}
	u, recorded := e.leaseUnreadOf(ctx, pr.ID)
	return e.leaseWait(v, u, recorded)
}

// factoryReviewRefusal is `magnum review`'s refusal of pr (label) while the
// factory fixes it: its fixer at work, or its loop waiting for the
// re-review of its push (phase reviewing: that re-review is the factory's
// own request), its re-review waiting in line (forced, its fix kept,
// whatever the lease says: the operator's review would take its place and
// drop its fix, keepFixRef), or a failed read within its grace, which it
// records as dispatch does (noteLease). "" = it may review. With no reader
// it refuses nothing.
func (e *Engine) factoryReviewRefusal(ctx context.Context, pr store.PR, label string) string {
	v, ok := e.leaseOf(ctx, pr)
	if !ok {
		return ""
	}
	if r, _ := e.fixRefOf(ctx, pr.ID); r != nil && pr.Forced {
		return fmt.Sprintf("%s: the factory's re-review of its push of %s (cycle %d) waits in line; a review of yours would take its place",
			label, textx.ShortSHA(r.FixRef.Head), r.FixRef.Cycle)
	}
	held := e.noteLease(ctx, pr, v)
	switch {
	case v.err != nil && held:
		since := e.now()
		if u, ok := e.leaseUnreadOf(ctx, pr.ID); ok {
			since = u.Since
		}
		return fmt.Sprintf("%s: the factory's lease of it cannot be read (%s); ask again after %s, when magnum reviews it anyway",
			label, leaseErrWords(v.err), waitClock(since.Add(fixLeaseGrace), e.now()))
	case v.fixing() || v.reviewing():
		cycle := fmt.Sprintf("cycle %d", v.lease.Cycle)
		if v.lease.Cap > 0 {
			cycle = fmt.Sprintf("cycle %d of %d", v.lease.Cycle, v.lease.Cap)
		}
		return fmt.Sprintf("%s: the factory fixes it (%s, %s at %s); its fixer's push asks for its own re-review", label, cycle,
			phaseWords(v.lease.Phase), textx.ShortSHA(v.lease.Head))
	}
	return ""
}

// phaseWords is a lease's phase in words.
func phaseWords(phase string) string {
	switch phase {
	case FixPhaseFixing:
		return "its fixer at work"
	case FixPhaseReviewing:
		return "waiting for the re-review of its push"
	}
	return strings.TrimSpace("in phase " + phase)
}

// factoryFix is the PR's fix work of the factory's as the status comment
// shows it: its lease's cycle in phase fixing, as a FixState (ok false:
// the factory's fixer does not work on it, or nothing is read).
func (e *Engine) factoryFix(ctx context.Context, pr store.PR) (FixState, bool) {
	v, ok := e.leaseOf(ctx, pr)
	if !ok || !v.fixing() {
		return FixState{}, false
	}
	return FixState{Cycle: max(v.lease.Cycle, 1), Cap: v.lease.Cap, Phase: v.lease.Phase, Head: v.lease.Head}, true
}

// FixLeaseUnreads are the PRs whose factory lease magnum could not read
// (KVPRFixLeaseUnread rows that read), by PR id: the doctor's line.
func FixLeaseUnreads(ctx context.Context, st *store.Store) (map[int64]LeaseUnread, error) {
	vals, err := st.KVValues(ctx, "pr.*.fix_lease_unread")
	if err != nil {
		return nil, err
	}
	out := map[int64]LeaseUnread{}
	for key, raw := range vals {
		var id int64
		var u LeaseUnread
		if _, err := fmt.Sscanf(key, "pr.%d.", &id); err != nil || KVPRFixLeaseUnread(id) != key || json.Unmarshal([]byte(raw), &u) != nil {
			continue
		}
		out[id] = u
	}
	return out, nil
}
