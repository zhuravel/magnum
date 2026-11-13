package engine

// The factory's lease of a PR's fixes, as magnum reads it (Deps.FixLeaseOf:
// the factory's registry, read only, across processes), and what the
// review side does with it (DECISIONS "The fix loop moves into the
// factory": the review side fails open). For a PR the factory fixes (its
// owner row, fix_owner.go), dispatch holds the PR's rounds while the
// factory's fixer works on it (phase fixing), as it holds them for magnum's
// own fixer (fixHolds, WaitFixing), `magnum review` is refused while the
// factory's fixer works on it or its re-review of its push is still to
// come on magnum's side, and the status comment says that the factory
// fixes it. A read
// that fails (the PR's owner row, which magnum's registry cannot read or
// this build cannot parse, or the factory's lease) holds the round for
// fixLeaseGrace from the first failure, then fails open: magnum reviews, a
// warn event says so once, and the PR's row (KVPRFixLeaseUnread, a
// key-value row: no migration) shows on the doctor's line until a read
// works again, which each tick tries (sweepLeaseUnreads), or the PR leaves
// the factory or closes. Each names what failed (its Source): a read of
// magnum's own registry says nothing of the factory's. A nil reader is
// inert: no lease is read, nothing is held or refused, no event or doctor
// line, as before the factory fixed PRs.

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
	"github.com/zhuravel/magnum/internal/fixwork"
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
// lease, and Source, what failed: the factory's lease (""), or the PR's
// owner row in magnum's own registry, which this build cannot parse
// (LeaseFromOwner) or the registry cannot read (LeaseFromRegistry).
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

// LeaseFromRegistry is LeaseUnread.Source of a PR whose owner row magnum's
// own registry could not read (a busy or damaged database): who fixes the
// PR is not known.
const LeaseFromRegistry = "registry"

// leaseView is what the review side reads of the factory's fixes of a PR:
// owned, the PR has an owner row (the factory's, or returning), its lease,
// and err, a read that failed, from source (LeaseUnread.Source): the
// factory's lease (""), an owner row this build cannot parse
// (LeaseFromOwner), or a read of magnum's registry itself, the owner row or
// the PR's repository (LeaseFromRegistry), when who fixes the PR is not
// known (owned false).
type leaseView struct {
	owned  bool
	lease  FixLease
	err    error
	source string
}

// unreadWords are the words of a failed read from its source
// (LeaseUnread.Source), so that each place names what failed: the
// factory's lease, or magnum's own owner row, never the one for the other.
type unreadWords struct {
	wait    string // a wait's detail
	warn    string // the daemon's warn at a first failure
	open    string // fix.lease_open's subject
	without string // what magnum reviews the PR without, after the grace
	refusal string // `magnum review`'s refusal, of the PR as "it"
	read    string // fix.lease_read, once it reads again
	gone    string // fix.lease_read, once it reads again and magnum fixes the PR
}

// wordsOf are the words of a failed read whose source is source.
func wordsOf(source string) unreadWords {
	switch source {
	case LeaseFromRegistry:
		return unreadWords{
			wait:    "its owner row, which magnum's registry cannot read",
			warn:    "magnum's registry cannot read the owner row of a PR; its round waits",
			open:    "magnum's registry cannot read the PR's owner row",
			without: "without knowing who fixes it",
			refusal: "magnum's registry cannot read its owner row",
			read:    "magnum's registry reads the PR's owner row again",
			gone:    "magnum's registry reads the PR's owner row again: magnum fixes the PR, and its failed read is dropped",
		}
	case LeaseFromOwner:
		return unreadWords{
			wait:    "its owner row, which this build cannot read",
			warn:    "the owner row of a PR cannot be parsed by this build; its round waits",
			open:    "the PR's owner row cannot be read by this build",
			without: "without knowing who fixes it",
			refusal: "its owner row cannot be read by this build",
			read:    "the PR's owner row reads again",
			gone:    "the PR's owner row reads again: magnum fixes the PR, and its failed read is dropped",
		}
	}
	return unreadWords{
		wait:    "the factory's lease, which cannot be read",
		warn:    "the factory's lease of a PR cannot be read; its round waits",
		open:    "the factory's lease of the PR cannot be read",
		without: "without the lease",
		refusal: "the factory's lease of it cannot be read",
		read:    "the factory's lease of the PR reads again",
		gone:    "the factory no longer fixes the PR: its failed lease read is dropped",
	}
}

// unreadWords names what v could not read, as a wait says it.
func (v leaseView) unreadWords() string { return wordsOf(v.source).wait }

// fixing reports whether the factory works on the PR now (its lease held,
// in any phase).
func (v leaseView) fixing() bool { return v.owned && v.err == nil && v.lease.Fixing }

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
		return leaseView{err: err, source: LeaseFromRegistry}, true
	case err != nil: // an owner row this build cannot parse (a newer build's, or damaged): there is one
		return leaseView{owned: true, err: err, source: LeaseFromOwner}, true
	case !owned:
		return leaseView{}, true
	}
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return leaseView{err: err, source: LeaseFromRegistry}, true
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
// (dispatch): its fixer works on the PR, or a read failed within its grace
// (the lease, or who fixes the PR, which magnum's registry cannot read).
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
			words := wordsOf(u.Source) // what failed, as recorded
			msg := words.read
			if !v.owned {
				msg = words.gone
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
		e.log.Warn(wordsOf(v.source).warn, "subject", subject, "grace", fixLeaseGrace, "err", v.err)
	}
	u.Last, u.Err, u.Source = now.UTC(), leaseErrWords(v.err), v.source
	_, held := e.leaseWait(v, u, true)
	if !held && !u.Open {
		u.Open = true
		words := wordsOf(u.Source)
		e.event(ctx, "warn", subject, "fix.lease_open", fmt.Sprintf("%s since %s (%s): magnum reviews it %s after the %s grace",
			words.open, waitClock(u.Since, e.now()), u.Err, words.without, humanDuration(fixLeaseGrace)),
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
// grace that ends meanwhile fails open), whatever its source. A PR gone or
// closed (merged too) drops its row at once, and so does a daemon that
// reads no lease.
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
		if v, ok := e.leaseOf(ctx, pr); ok {
			e.noteLease(ctx, pr, v)
		} else {
			e.delKV(ctx, key)
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
	if !ok {
		return Wait{}, false
	}
	u, recorded := e.leaseUnreadOf(ctx, pr.ID)
	if v.err != nil && !recorded {
		return Wait{}, false
	}
	return e.leaseWait(v, u, recorded)
}

// factoryReviewRefusal is `magnum review`'s refusal of pr (label) while the
// factory fixes it: its fixer at work, its re-review still to come on
// magnum's side (factoryRoundRefusal, whatever the lease says: the
// operator's review would take its place and end its fix, keepFixRef), or a
// failed read within its grace (the lease, or who fixes the PR), which it
// records as dispatch does (noteLease). The factory's loop waiting in phase
// reviewing refuses nothing by itself: once magnum's re-review ran, that
// phase lasts until the factory reads its end, and a factory that stopped
// meanwhile would hold off every review of the PR for good. "" = it may
// review. With no reader it refuses nothing.
func (e *Engine) factoryReviewRefusal(ctx context.Context, pr store.PR, label string) string {
	v, ok := e.leaseOf(ctx, pr)
	if !ok {
		return ""
	}
	if why := e.factoryRoundRefusal(ctx, pr, label); why != "" {
		return why
	}
	held := e.noteLease(ctx, pr, v)
	switch {
	case v.err != nil && held:
		since := e.now()
		if u, ok := e.leaseUnreadOf(ctx, pr.ID); ok {
			since = u.Since
		}
		return fmt.Sprintf("%s: %s (%s); ask again after %s, when magnum reviews it anyway",
			label, wordsOf(v.source).refusal, leaseErrWords(v.err), waitClock(since.Add(fixLeaseGrace), e.now()))
	case v.fixing():
		cycle := fmt.Sprintf("cycle %d", v.lease.Cycle)
		if v.lease.Cap > 0 {
			cycle = fmt.Sprintf("cycle %d of %d", v.lease.Cycle, v.lease.Cap)
		}
		return fmt.Sprintf("%s: the factory fixes it (%s, %s at %s); its fixer's push asks for its own re-review", label, cycle,
			phaseWords(v.lease.Phase), textx.ShortSHA(v.lease.Head))
	}
	return ""
}

// factoryRoundRefusal is `magnum review`'s refusal of pr (label) while the
// factory's re-review of its push is still to come on magnum's side: a
// round of its fix in flight (roundHandle.fixRef), or its fix kept
// (kvPRFixRef) on a PR open on GitHub that is forced and in line (queued,
// rereview_pending, or paused mid-turn) or in its round. A fix row a closed
// PR kept, or one no forced round is coming for, refuses nothing: no round
// of it runs. "" = no refusal.
func (e *Engine) factoryRoundRefusal(ctx context.Context, pr store.PR, label string) string {
	e.mu.Lock()
	var running *fixwork.FixRef
	if h := e.rounds[pr.ID]; h != nil {
		running = h.fixRef
	}
	e.mu.Unlock()
	if running != nil {
		return fmt.Sprintf("%s: the factory's re-review of its push of %s (cycle %d) runs; a review of yours would take its place",
			label, textx.ShortSHA(running.Head), running.Cycle)
	}
	r, _ := e.fixRefOf(ctx, pr.ID)
	if r == nil || !pr.Forced || pr.GHState != store.GHOpen {
		return ""
	}
	where := ""
	switch {
	case slices.Contains(store.InFlightStates, pr.State):
		where = "runs"
	case pr.State == store.PRQueued || pr.State == store.PRRereviewPending || pr.State == store.PRPaused:
		where = "waits in line"
	default:
		return ""
	}
	return fmt.Sprintf("%s: the factory's re-review of its push of %s (cycle %d) %s; a review of yours would take its place",
		label, textx.ShortSHA(r.FixRef.Head), r.FixRef.Cycle, where)
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
