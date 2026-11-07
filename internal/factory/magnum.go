package factory

// The review daemon as the factory's local review sees it (spec 5 and 7.2):
// a local round runs at the lowest priority and yields to magnum's real
// rounds, so it waits while one is in flight or due. The screen and the
// tick read the factory's PRs there too (PRStates): magnum's row, its
// autofix = "open" verdict, its fix loop, its stalemate and its attention.
// The factory reads magnum's registry and never writes or migrates it: a
// registry of another schema than this build's is an error, and its file
// stays as it was.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// magnumRegistry is Magnum over magnum's registry and its daemon's pidfile
// in layout l.
type magnumRegistry struct {
	l   paths.Layout
	cfg *config.Config
	now func() time.Time
	// dueSince is when each queued PR was first seen due, by its id: one
	// that never starts holds the local review dueHold at most.
	mu       sync.Mutex
	dueSince map[int64]time.Time
}

// dueHold is how long a queued round of magnum's that is due and never
// starts may hold the local review: magnum's gates that the factory cannot
// read (the GitHub floor, a pinned slot waiting for unpin, a paused agent
// kind) hold such a PR without a next try it could see.
const dueHold = 30 * time.Minute

// NewMagnum is magnum's review daemon of layout l, configured by cfg, as
// the factory's local review sees it (Magnum).
func NewMagnum(l paths.Layout, cfg *config.Config) Magnum {
	return &magnumRegistry{l: l, cfg: cfg, now: time.Now, dueSince: map[int64]time.Time{}}
}

// refuseMagnumMigration refuses every migration of magnum's registry
// (store.Options.BeforeMigrate): the factory never migrates it.
func refuseMagnumMigration(from, to int) error {
	return fmt.Errorf("magnum's registry is at schema %d and this build needs %d; the factory never migrates it: "+
		"restart magnum's daemon first (`magnum daemon-restart --drain`), then the factory goes on", from, to)
}

// open opens magnum's registry with every migration refused
// (refuseMagnumMigration), so one of a schema this build does not know is
// an error and keeps its file; nil and no error when there is no registry,
// which it never creates.
func (m *magnumRegistry) open() (*store.Store, error) {
	if _, err := os.Stat(m.l.DB()); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("magnum's registry: %w", err)
	}
	return store.OpenWith(m.l.DB(), store.Options{BeforeMigrate: refuseMagnumMigration})
}

// Busy reports whether magnum's review rounds fill its limit
// ([daemon] max_concurrent_reviews, at least 1): the rounds in flight (a PR
// claimed, reviewing or verifying) and the rounds due (a queued PR whose
// next try has come and that magnum's gates would dispatch now). Below the
// limit the local review takes a free round, as one more of magnum's. Inside [daemon] quiet_hours they
// hold every queued round nobody forced (eligibility.QuietHoursHold, the
// dispatcher's own rule; a re-review of the judge alone that they let start
// shows as in flight once it does), and a due PR that has not started
// within dueHold is one of the gates the factory cannot read: it holds the
// local review no longer, and why says so while busy is false. No magnum
// daemon running (its pidfile names no live process) runs no round, and no
// registry holds none: neither is busy, and nothing is created. The
// registry opens with every migration refused (refuseMagnumMigration), so
// one that is a version behind this build (magnum's daemon still runs the
// build before) is an error and keeps its schema. why is plain words,
// never a PR's text.
func (m *magnumRegistry) Busy(ctx context.Context) (bool, string, error) {
	pid, err := engine.DaemonPID(m.l)
	if err != nil {
		return false, "", fmt.Errorf("magnum's daemon: %w", err)
	}
	if pid == 0 {
		return false, "", nil
	}
	st, err := m.open()
	if st == nil || err != nil {
		return false, "", err
	}
	defer st.Close()
	prs, err := st.ListPRs(ctx, store.PRFilter{States: append(slices.Clone(store.InFlightStates), store.PRQueued)})
	if err != nil {
		return false, "", fmt.Errorf("magnum's registry: %w", err)
	}
	now := m.now()
	quiet, limit := "", 1
	if m.cfg != nil {
		quiet, limit = m.cfg.Daemon.QuietHours, max(1, m.cfg.Daemon.MaxConcurrentReviews)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[int64]bool{}
	inFlight, due, stale := 0, 0, false
	for _, p := range prs {
		if slices.Contains(store.InFlightStates, p.State) {
			inFlight++
			continue
		}
		switch {
		case p.NextAttemptAt != nil && p.NextAttemptAt.After(now), p.NextEligibleAt != nil && p.NextEligibleAt.After(now):
			continue // its next try is later
		case !p.Forced && quiet != "" && eligibility.QuietHoursHold(quiet, now.Local(), false):
			continue // quiet hours hold it
		}
		seen[p.ID] = true
		since, ok := m.dueSince[p.ID]
		if !ok {
			since = now
			m.dueSince[p.ID] = now
		}
		if now.Sub(since) < dueHold {
			due++
		} else {
			stale = true
		}
	}
	for id := range m.dueSince {
		if !seen[id] {
			delete(m.dueSince, id)
		}
	}
	switch {
	case inFlight >= limit && limit == 1:
		return true, "a review round of magnum's runs", nil
	case inFlight >= limit:
		return true, fmt.Sprintf("magnum runs %d review rounds, its limit of %d", inFlight, limit), nil
	case inFlight+due >= limit && limit == 1:
		return true, "a review round of magnum's is due", nil
	case inFlight+due >= limit:
		return true, fmt.Sprintf("magnum's review rounds fill its limit of %d (%d running, %d due)", limit, inFlight, due), nil
	case stale:
		return false, fmt.Sprintf("a review round of magnum's is due but has not started for %s (magnum's own gates hold it): "+
			"the local review goes on", dueHold), nil
	}
	return false, "", nil
}

// PRRef names a PR as the factory knows it: its repository (owner/name)
// and number.
type PRRef struct {
	Repo   string
	Number int
}

// PRState is what magnum's registry says of a factory PR (Magnum.PRStates).
// Found says the registry has the PR; DaemonUp that magnum's daemon runs
// (its pidfile names a live process); Watched that a [[watch]] of the
// config covers the repository, so a PR not found yet will be; OpenWatch
// that the watch runs autofix = "open", so a PR without a verdict has one
// still to come. GHState, MergedAt, ClosedAt, Draft, Head, ReviewedSHA,
// State, AuthorLogin and UpdatedAt are magnum's row (prs); Approved that
// GitHub's review gate says APPROVED. Open is the autofix = "open" verdict
// (engine.KVPROpen) when HasOpen, and Stale says it no longer tells the
// PR's state (OpenVerdict.Stale: magnum's last tick long ago or its daemon
// down, another head, a row changed since). Fix is the fix loop that runs
// (engine.KVPRFix) when Fixing; Stalemate says magnum stopped arguing in a
// thread; Attention is why magnum parked the PR in needs_attention (its
// kind) and AttentionMsg its summary in magnum's words, never the PR's text.
type PRState struct {
	Ref                                   PRRef
	Found, DaemonUp, Watched, OpenWatch   bool
	GHState                               string
	MergedAt, ClosedAt                    *time.Time
	Draft                                 bool
	Head, ReviewedSHA, State, AuthorLogin string
	UpdatedAt                             time.Time
	Approved                              bool
	Open                                  engine.OpenVerdict
	HasOpen, Stale                        bool
	Fix                                   engine.FixState
	Fixing, Stalemate                     bool
	Attention, AttentionMsg               string
}

// PRStates reads refs' PRs from magnum's registry, one view per ref (see
// PRState), in one opening of the registry: per frame of the screen or per
// tick, never per task. It reads and never writes: the prs rows, the kv
// rows of the verdict, the fix loop, the stalemate and the attention, and
// magnum's last tick. No registry finds nothing and creates none; one of a
// schema this build does not know is an error (refuseMagnumMigration), as
// is any read that fails, and the caller then knows nothing of the PRs.
func (m *magnumRegistry) PRStates(ctx context.Context, refs []PRRef) (map[PRRef]PRState, error) {
	pid, err := engine.DaemonPID(m.l)
	if err != nil {
		return nil, fmt.Errorf("magnum's daemon: %w", err)
	}
	out := make(map[PRRef]PRState, len(refs))
	for _, r := range refs {
		v := PRState{Ref: r, DaemonUp: pid > 0}
		if m.cfg != nil {
			if w := m.cfg.WatchFor(r.Repo); w != nil {
				v.Watched, v.OpenWatch = true, m.cfg.AutofixFor(w) == config.AutofixOpen
			}
		}
		out[r] = v
	}
	st, err := m.open()
	if st == nil || err != nil {
		return out, err
	}
	defer st.Close()
	lastTick := time.Time{}
	if s, ok, err := st.GetKV(ctx, store.KVDaemonLastTick); err != nil {
		return nil, fmt.Errorf("magnum's registry: %w", err)
	} else if ok {
		lastTick, _ = store.ParseTime(s)
	}
	if pid == 0 {
		lastTick = time.Time{} // no daemon decides anything now: every verdict is stale
	}
	var poll time.Duration
	if m.cfg != nil {
		poll = m.cfg.Daemon.PollInterval.Duration
	}
	now := m.now()
	for _, r := range refs {
		v := out[r]
		if err := m.readPR(ctx, st, &v, lastTick, now, poll); err != nil {
			return nil, fmt.Errorf("magnum's registry: %s#%d: %w", r.Repo, r.Number, err)
		}
		out[r] = v
	}
	return out, nil
}

// readPR fills v from st: magnum's row of v.Ref and its kv rows; a
// repository or a PR st does not have leaves v not found.
func (m *magnumRegistry) readPR(ctx context.Context, st *store.Store, v *PRState, lastTick, now time.Time, poll time.Duration) error {
	repo, err := st.RepoByFullName(ctx, v.Ref.Repo)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	pr, err := st.PRByRepoNumber(ctx, repo.ID, v.Ref.Number)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	v.Found = true
	v.GHState, v.MergedAt, v.ClosedAt, v.Draft = pr.GHState, pr.MergedAt, pr.ClosedAt, pr.IsDraft
	v.Head, v.ReviewedSHA, v.State, v.AuthorLogin = pr.HeadSHA, store.Deref(pr.ReviewedSHA), pr.State, store.Deref(pr.AuthorLogin)
	v.UpdatedAt = pr.UpdatedAt
	v.Approved = pr.ReviewGate != nil && pr.ReviewGate.Decision == "APPROVED"
	kv := func(key string) (string, error) {
		s, _, err := st.GetKV(ctx, key)
		return s, err
	}
	s, err := kv(engine.KVPROpen(pr.ID))
	if err != nil {
		return err
	}
	if v.Open, v.HasOpen = engine.ParseOpenVerdict(s); v.HasOpen {
		v.Stale = v.Open.Stale(pr.HeadSHA, pr.UpdatedAt, lastTick, now, poll)
	}
	if s, err = kv(engine.KVPRFix(pr.ID)); err != nil {
		return err
	}
	v.Fix, v.Fixing = engine.ParseFixState(s)
	if s, err = kv(engine.KVPRStalemate(pr.ID)); err != nil {
		return err
	}
	_, v.Stalemate = engine.ParseStalemate(s)
	if pr.State == store.PRNeedsAttention {
		if v.Attention, err = kv(engine.KVPRAttention(pr.ID)); err != nil {
			return err
		}
		ref := fmt.Sprintf("%s#%d", repo.Name, pr.Number)
		v.AttentionMsg = attention.Explain(v.Attention, store.Deref(pr.LastError), ref).Summary
	}
	return nil
}

// FixLoops counts the fix loops magnum started on a PR: by hand (`magnum
// fix`, f on the board) and by autofix.
type FixLoops struct {
	ByHand, ByAutofix int
}

// fixStartKind is the event magnum writes as a fix loop's cycle starts.
const fixStartKind = "fix.start"

// FixLoopsOf counts, per PR of refs, the fix loops magnum's registry in
// layout l recorded: its fix.start events of cycle 1 (a later cycle is the
// same loop), by who started each (engine.AutofixBy, or by hand), over the
// whole history the registry keeps ([daemon] keep_events). A PR it has no
// event of counts zero. It reads and never writes or migrates the
// registry, as PRStates; no registry counts nothing and creates none.
func FixLoopsOf(ctx context.Context, l paths.Layout, refs []PRRef) (map[PRRef]FixLoops, error) {
	out := make(map[PRRef]FixLoops, len(refs))
	st, err := (&magnumRegistry{l: l}).open()
	if st == nil || err != nil {
		return out, err
	}
	defer st.Close()
	for _, r := range refs {
		repo, err := st.RepoByFullName(ctx, r.Repo)
		if errors.Is(err, store.ErrNotFound) {
			out[r] = FixLoops{}
			continue
		} else if err != nil {
			return nil, fmt.Errorf("magnum's registry: %w", err)
		}
		evs, err := st.EventsBySubject(ctx, engine.PRSubject(repo, r.Number), 0) // the whole history: a busy PR has hundreds
		if err != nil {
			return nil, fmt.Errorf("magnum's registry: %w", err)
		}
		var c FixLoops
		for _, ev := range evs {
			var d struct {
				Cycle int    `json:"cycle"`
				By    string `json:"by"`
			}
			if ev.Kind != fixStartKind || json.Unmarshal(ev.Data, &d) != nil || d.Cycle != 1 {
				continue
			}
			if d.By == engine.AutofixBy {
				c.ByAutofix++
			} else {
				c.ByHand++
			}
		}
		out[r] = c
	}
	return out, nil
}
