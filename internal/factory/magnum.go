package factory

// The review daemon as the factory's local review sees it (spec 5 and 7.2):
// a local round runs at the lowest priority and yields to magnum's real
// rounds, so it waits while one is in flight or due. The factory reads
// magnum's registry and never writes or migrates it: a registry of another
// schema than this build's is an error, and its file stays as it was.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"

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

// Busy reports whether magnum has a review round in flight (a PR claimed,
// reviewing or verifying) or due: a queued PR whose next try has come and
// that magnum's gates would dispatch now. Inside [daemon] quiet_hours they
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
	if _, err := os.Stat(m.l.DB()); errors.Is(err, fs.ErrNotExist) {
		return false, "", nil
	} else if err != nil {
		return false, "", fmt.Errorf("magnum's registry: %w", err)
	}
	st, err := store.OpenWith(m.l.DB(), store.Options{BeforeMigrate: refuseMagnumMigration})
	if err != nil {
		return false, "", err
	}
	defer st.Close()
	prs, err := st.ListPRs(ctx, store.PRFilter{States: append(slices.Clone(store.InFlightStates), store.PRQueued)})
	if err != nil {
		return false, "", fmt.Errorf("magnum's registry: %w", err)
	}
	now := m.now()
	quiet := ""
	if m.cfg != nil {
		quiet = m.cfg.Daemon.QuietHours
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[int64]bool{}
	due, stale := false, false
	for _, p := range prs {
		if slices.Contains(store.InFlightStates, p.State) {
			return true, "a review round of magnum's runs", nil
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
			due = true
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
	case due:
		return true, "a review round of magnum's is due", nil
	case stale:
		return false, fmt.Sprintf("a review round of magnum's is due but has not started for %s (magnum's own gates hold it): "+
			"the local review goes on", dueHold), nil
	}
	return false, "", nil
}
