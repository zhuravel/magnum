package factory

// The factory's one write into magnum's registry (DECISIONS "The factory
// becomes the only fixer"): a request in magnum's requests table, as a
// command of magnum's CLI writes one (the re-review of a fixer's push, the
// abort of the round it forced, a raise or a lift of a PR's attention), and
// a wake of magnum's daemon, which answers it. Everything else of magnum's
// the factory only reads (magnum.go). The writer never migrates magnum's
// registry and never creates one: it opens the file with every migration
// refused (refuseMagnumMigration), so a registry of another schema than
// this build's (a review daemon still on an older build, or a newer one) is
// an error that leaves the file as it was, and nothing is written.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// MagnumWriter writes the factory's requests into magnum's registry and
// wakes magnum's daemon. It is safe for concurrent use: each write opens
// the registry and closes it again.
type MagnumWriter struct {
	l    paths.Layout
	kick func() (int, error)
}

// NewMagnumWriter is the writer of the requests to magnum's daemon of
// layout l (its registry Layout.DB, its pidfile Layout.Pid).
func NewMagnumWriter(l paths.Layout) *MagnumWriter {
	return &MagnumWriter{l: l, kick: func() (int, error) { return engine.KickDaemon(l) }}
}

// EnqueueRequest writes a pending request of kind, payload as its JSON,
// into magnum's requests table and returns its id. A kind magnum's daemon
// does not know fails there as unknown, which an older build answers. It
// refuses, writing nothing, a registry that does not exist (no review
// daemon ever ran here) and one whose schema is not this build's: it never
// migrates it.
func (w *MagnumWriter) EnqueueRequest(ctx context.Context, kind string, payload any) (int64, error) {
	if kind == "" {
		return 0, errors.New("magnum's registry: a request needs a kind")
	}
	if _, err := os.Stat(w.l.DB()); errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("magnum's registry %s does not exist: magnum's daemon never ran here, so the factory writes it no request", w.l.DB())
	} else if err != nil {
		return 0, fmt.Errorf("magnum's registry: %w", err)
	}
	st, err := store.OpenWith(w.l.DB(), store.Options{BeforeMigrate: refuseMagnumMigration})
	if err != nil {
		return 0, fmt.Errorf("magnum's registry: %w", err)
	}
	defer st.Close()
	id, err := st.EnqueueRequest(ctx, kind, payload)
	if err != nil {
		return 0, fmt.Errorf("magnum's registry: %w", err)
	}
	return id, nil
}

// KickDaemon wakes magnum's daemon (SIGUSR1, engine.KickDaemon), so it
// answers the factory's request at once rather than at its next poll; it
// returns the daemon's pid, 0 when none runs (the request waits for it).
func (w *MagnumWriter) KickDaemon() (int, error) { return w.kick() }
