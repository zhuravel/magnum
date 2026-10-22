package store

import (
	"context"
	"fmt"
	"time"
)

// SessionsStartedSince returns the sessions of agent kind kind (agent_kind)
// started at or after since, in any state, newest first (started_at, then
// id). Doctor and the daemon read the session ids of a kind's recent
// sessions with it (agents.RecentSessionIDs).
func (s *Store) SessionsStartedSince(ctx context.Context, kind string, since time.Time) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("", sessionColumns)+
		" FROM sessions WHERE agent_kind = ? AND started_at >= ? ORDER BY started_at DESC, id DESC", kind, FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("sessions of %s since %s: %w", kind, FormatTime(since), err)
	}
	return collect(rows, scanSession)
}
