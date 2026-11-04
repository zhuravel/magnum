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
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// magnumRegistry is Magnum over magnum's registry and its daemon's pidfile
// in layout l.
type magnumRegistry struct {
	l   paths.Layout
	now func() time.Time
}

// NewMagnum is magnum's review daemon of layout l as the factory's local
// review sees it (Magnum).
func NewMagnum(l paths.Layout) Magnum { return &magnumRegistry{l: l, now: time.Now} }

// refuseMagnumMigration refuses every migration of magnum's registry
// (store.Options.BeforeMigrate): the factory never migrates it.
func refuseMagnumMigration(from, to int) error {
	return fmt.Errorf("magnum's registry is at schema %d and this build needs %d; the factory never migrates it: "+
		"restart magnum's daemon first (`magnum daemon-restart --drain`), then the factory goes on", from, to)
}

// Busy reports whether magnum has a review round in flight (a PR claimed,
// reviewing or verifying) or due (a queued PR whose next try has come). No
// magnum daemon running (its pidfile names no live process) runs no round,
// and no registry holds none: neither is busy, and nothing is created. The
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
	due := false
	for _, p := range prs {
		if slices.Contains(store.InFlightStates, p.State) {
			return true, "a review round of magnum's runs", nil
		}
		if (p.NextAttemptAt == nil || !p.NextAttemptAt.After(now)) && (p.NextEligibleAt == nil || !p.NextEligibleAt.After(now)) {
			due = true
		}
	}
	if due {
		return true, "a review round of magnum's is due", nil
	}
	return false, "", nil
}
