package engine

// The factory's lease of a PR's fixes, as magnum reads it (Deps.FixLeaseOf:
// the factory's registry, read only, across processes), and what the
// review side does with it (DECISIONS "The fix loop moves into the
// factory": the review side fails open). For a PR the factory fixes (its
// owner row, fix_owner.go), dispatch holds the PR's rounds while the
// factory's fixer works on it (phase fixing), as it holds them for magnum's
// own fixer (fixHolds, WaitFixing), `magnum review` is refused while the
// factory's fixer works on it or its loop waits for magnum's re-review of
// its push, and the status comment says that the factory fixes it. A read
// that fails (an owner row this build cannot parse, or the lease) holds the
// round for fixLeaseGrace from the first failure, then fails open: magnum
// reviews, a warn event says so once, and the PR's row (KVPRFixLeaseUnread,
// a key-value row: no migration) shows on the doctor's line until a read
// works again, which each tick tries (sweepLeaseUnreads), or the PR leaves
// the factory or closes. A read of magnum's own registry that fails says nothing of
// the factory: the round waits, as for a fix loop's row that cannot be read,
// and nothing is recorded. A nil reader is inert: no lease is read, nothing
// is held or refused, no event or doctor line, as before the factory fixed
// PRs.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// fixLeaseGap is the least time after the last failed read of a PR's lease
// at which a failed read is a first one again, whose grace starts anew
// (leaseGap): each tick reads it (sweepLeaseUnreads), every poll interval,
// so a gap this long means no tick ran (the daemon stopped, or the Mac
// slept).
const fixLeaseGap = fixLeaseGrace / 2

// leaseGapPolls is how many poll intervals a gap between failed reads spans
// at least before the grace starts anew: a tick and the wait after it take
// one, and a slow tick (a GitHub outage that stretches the poll) more.
const leaseGapPolls = 3

// leaseGap is the gap after which a failed read is a first one again:
// fixLeaseGap, or leaseGapPolls poll intervals when that is longer, so that
// under a long poll_interval each tick does not start the grace anew, which
// would never end (as fix_open.go and sleptBefore scale their gaps).
func (e *Engine) leaseGap() time.Duration {
	return max(fixLeaseGap, leaseGapPolls*e.cfg.Daemon.PollInterval.Duration)
}

// KVPRFixLeaseUnread holds, while it lasts, the failed read of the
// factory's lease of PR prID (LeaseUnread as JSON): the doctor names it.
func KVPRFixLeaseUnread(prID int64) string { return fmt.Sprintf("pr.%d.fix_lease_unread", prID) }

// LeaseUnread is a KVPRFixLeaseUnread row: the first failed read (Since)
// and the last one (Last: a row written before it was kept counts from
// Since), the last one's error in a line (Err, a registry's words, never
// PR text), Open, the grace ended and magnum reviews the PR without the
// lease, and Source, what failed: the factory's lease ("") or the PR's
// owner row in magnum's own registry, which this build cannot parse
// (LeaseFromOwner).
type LeaseUnread struct {
	Since  time.Time `json:"since"`
	Last   time.Time `json:"last,omitzero"`
	Err    string    `json:"err"`
	Open   bool      `json:"open,omitempty"`
	Source string    `json:"source,omitempty"`
}

// lastFailure is when u's last failed read was.
func (u LeaseUnread) lastFailure() time.Time { return cmp.Or(u.Last, u.Since) }

// LeaseFromOwner is LeaseUnread.Source of a PR whose owner row in magnum's
// registry does not parse (a newer build wrote it, or it is damaged).
const LeaseFromOwner = "owner"

// leaseView is what the review side reads of the factory's fixes of a PR:
// owned, the PR has an owner row (the factory's, or returning), its lease,
// and err, the owner row (one this build cannot parse) or the lease could
// not be read. unknown is a read of magnum's registry itself that failed
// (the owner row, or the PR's repository): who fixes the PR is not known,
// so it is no failed read of the factory's lease.
type leaseView struct {
	owned     bool
	lease     FixLease
	err       error
	fromOwner bool // err is the owner row's, which does not parse, not the factory's lease's
	unknown   error
}

// source is LeaseUnread.Source of v's failed read.
func (v leaseView) source() string {
	if v.fromOwner {
		return LeaseFromOwner
	}
	return ""
}

// unreadWords names what v could not read, as a wait says it.
func (v leaseView) unreadWords() string {
	if v.fromOwner {
		return "its owner row, which this build cannot read"
	}
	return "the factory's lease, which cannot be read"
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
	_, raw, owned, err := e.fixOwner(ctx, pr.ID)
	switch {
	case err != nil && raw == "": // the registry's read failed: who fixes the PR is not known
		return leaseView{unknown: err}, true
	case err != nil: // an owner row this build cannot parse (a newer build's, or damaged): there is one
		return leaseView{owned: true, err: err, fromOwner: true}, true
	case !owned:
		return leaseView{}, true
	}
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return leaseView{unknown: err}, true
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
			Detail: fmt.Sprintf("%s (%s)", v.unreadWords(), leaseErrWords(v.err))}, true
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
// (dispatch): its fixer works on the PR, or a read failed within its grace,
// or magnum's registry cannot read who fixes it (leaseView.unknown), which
// records nothing.
// It records the failed read (KVPRFixLeaseUnread) the first time, and once
// its grace is over marks it open with a warn event (fix.lease_open): the
// round goes on without the lease. A read that works again clears it, with
// fix.lease_read. With no reader it holds nothing.
func (e *Engine) factoryHolds(ctx context.Context, pr store.PR) bool {
	if e.factorysOwnRound(ctx, pr) {
		return false
	}
	v, ok := e.leaseOf(ctx, pr)
	switch {
	case !ok:
		return false
	case v.unknown != nil: // as a fix loop's row that cannot be read holds it (fixHolds), with no lease row
		if e.logOnce(fmt.Sprintf("fix-owner:%d", pr.ID), v.unknown.Error(), e.now()) {
			e.log.Warn("dispatch holds a PR whose owner row cannot be read", "pr", pr.ID, "err", v.unknown)
		}
		return true
	}
	return e.noteLease(ctx, pr, v)
}

// noteLease is factoryHolds on v, read already: it records what a failed
// read or a working one changes, and reports whether the round waits. A
// dry-run daemon records it too, in its private copy of the registry
// (Deps.DryRun), so it warns once and shows the fail-open the real daemon
// would do.
func (e *Engine) noteLease(ctx context.Context, pr store.PR, v leaseView) bool {
	u, recorded := e.leaseUnreadOf(ctx, pr.ID)
	subject := fmt.Sprintf("pr:%d", pr.ID)
	if repo, err := e.st.RepoByID(ctx, pr.RepoID); err == nil {
		subject = prSubject(repo, pr.Number)
	}
	if v.err == nil {
		if recorded {
			e.delKV(ctx, KVPRFixLeaseUnread(pr.ID))
			msg := "the factory's lease of the PR reads again"
			if !v.owned {
				msg = "the factory no longer fixes the PR: its failed lease read is dropped"
			}
			e.event(ctx, "info", subject, "fix.lease_read", msg, nil)
		}
		_, held := e.leaseWait(v, u, recorded)
		return held
	}
	now := e.now()
	if recorded && now.Sub(u.lastFailure()) > e.leaseGap() { // no failed read for a while: the daemon stopped, or the Mac slept
		recorded = false
	}
	if !recorded {
		u = LeaseUnread{Since: now.UTC()}
		e.log.Warn("the factory's lease of a PR cannot be read; its round waits", "subject", subject, "grace", fixLeaseGrace, "err", v.err)
	}
	u.Last, u.Err, u.Source = now.UTC(), leaseErrWords(v.err), v.source()
	_, held := e.leaseWait(v, u, true)
	if !held && !u.Open {
		u.Open = true
		what := "the factory's lease of the PR cannot be read"
		if v.fromOwner {
			what = "the PR's owner row cannot be read by this build"
		}
		e.event(ctx, "warn", subject, "fix.lease_open", fmt.Sprintf("%s since %s (%s): magnum reviews it "+
			"without the lease after the %s grace", what, waitClock(u.Since, e.now()), u.Err, humanDuration(fixLeaseGrace)),
			map[string]any{"since": u.Since, "source": cmp.Or(u.Source, "lease")})
	}
	e.setKV(ctx, KVPRFixLeaseUnread(pr.ID), rowJSON(u)) // each failed read: its time is the gap's start
	return held
}

// sweepLeaseUnreads is the tick's look at every failed lease read recorded
// (KVPRFixLeaseUnread), dispatch candidate or not: a PR reviewed after it
// failed open is none, and only the reads of dispatch and `magnum review`
// cleared a row, so the doctor's WARN outlived the failure for good on a
// reviewed or closed PR. It reads the PR's owner row and lease again, as
// dispatch does (noteLease): the row goes once the lease reads or the
// factory no longer fixes the PR, a read that still fails keeps it (and a
// grace that ends meanwhile fails open), and magnum's own failed read of
// the owner row leaves it as it is. A PR gone or closed (merged too) drops
// its row at once, and so does a daemon that reads no lease.
func (e *Engine) sweepLeaseUnreads(ctx context.Context) {
	keys, err := e.st.KVKeys(ctx, "pr.*.fix_lease_unread")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "fix lease: the failed reads")
		return
	}
	for _, key := range keys {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.", &id); err != nil || KVPRFixLeaseUnread(id) != key {
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			e.delKV(ctx, key)
			continue
		case err != nil:
			continue
		case pr.GHState != store.GHOpen || slices.Contains(closingStates, pr.State):
			e.delKV(ctx, key)
			continue
		}
		switch v, ok := e.leaseOf(ctx, pr); {
		case !ok:
			e.delKV(ctx, key)
		case v.unknown == nil:
			e.noteLease(ctx, pr, v)
		}
	}
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
// (waitFor; ok false: it does not), read without recording anything. A
// failed read is a wait only once dispatch or `magnum review` recorded it,
// which counts its grace: before (a PR that is no dispatch candidate yet)
// its grace would start anew at each tick, and what holds the PR is
// another wait's.
func (e *Engine) factoryWaitOf(ctx context.Context, pr store.PR) (Wait, bool) {
	if e.factorysOwnRound(ctx, pr) {
		return Wait{}, false
	}
	v, ok := e.leaseOf(ctx, pr)
	switch {
	case !ok:
		return Wait{}, false
	case v.unknown != nil:
		return Wait{Reason: WaitFixing, Subject: waitFactory, Detail: fmt.Sprintf("its owner row, which cannot be read (%s)", leaseErrWords(v.unknown))}, true
	}
	u, recorded := e.leaseUnreadOf(ctx, pr.ID)
	if v.err != nil && !recorded {
		return Wait{}, false
	}
	return e.leaseWait(v, u, recorded)
}

// factoryReviewRefusal is `magnum review`'s refusal of pr (label) while the
// factory fixes it: its fixer at work, or its loop waiting for the
// re-review of its push (phase reviewing: that re-review is the factory's
// own request), its re-review waiting in line (forced, its fix kept,
// whatever the lease says: the operator's review would take its place and
// drop its fix, keepFixRef), a failed read within its grace, which it
// records as dispatch does (noteLease), or magnum's own failed read of who
// fixes it, which records nothing. "" = it may review. With no reader it
// refuses nothing.
func (e *Engine) factoryReviewRefusal(ctx context.Context, pr store.PR, label string) string {
	v, ok := e.leaseOf(ctx, pr)
	if !ok {
		return ""
	}
	if r, _ := e.fixRefOf(ctx, pr.ID); r != nil && pr.Forced {
		return fmt.Sprintf("%s: the factory's re-review of its push of %s (cycle %d) waits in line; a review of yours would take its place",
			label, textx.ShortSHA(r.FixRef.Head), r.FixRef.Cycle)
	}
	if v.unknown != nil {
		return fmt.Sprintf("%s: who fixes it cannot be read (%s); ask again shortly", label, leaseErrWords(v.unknown))
	}
	held := e.noteLease(ctx, pr, v)
	switch {
	case v.err != nil && held:
		since := e.now()
		if u, ok := e.leaseUnreadOf(ctx, pr.ID); ok {
			since = u.Since
		}
		what := "the factory's lease of it cannot be read"
		if v.fromOwner {
			what = "its owner row cannot be read by this build"
		}
		return fmt.Sprintf("%s: %s (%s); ask again after %s, when magnum reviews it anyway",
			label, what, leaseErrWords(v.err), waitClock(since.Add(fixLeaseGrace), e.now()))
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
