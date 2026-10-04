package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ShouldSend is the notification dedup gate: it returns true (and records
// the send) when key was never sent or was last sent at least window ago;
// otherwise it counts the suppressed occurrence and returns false.
// notifications.count is the number of occurrences since the last send,
// including that send.
func (s *Store) ShouldSend(ctx context.Context, key string, window time.Duration) (bool, error) {
	send := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		var last string
		err := tx.QueryRowContext(ctx, "SELECT last_sent_at FROM notifications WHERE key = ?", key).Scan(&last)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			send = true
		case err != nil:
			return err
		default:
			t, err := ParseTime(last)
			if err != nil {
				return err
			}
			send = !t.Add(window).After(now)
		}
		if send {
			_, err = tx.ExecContext(ctx, `INSERT INTO notifications (key, last_sent_at, count) VALUES (?, ?, 1)
ON CONFLICT(key) DO UPDATE SET last_sent_at = excluded.last_sent_at, count = 1`, key, FormatTime(now))
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE notifications SET count = count + 1 WHERE key = ?", key)
		}
		return err
	})
	if err != nil {
		return false, fmt.Errorf("notification %s: %w", key, err)
	}
	return send, nil
}

// ForgetSend drops key's dedup record so the next ShouldSend for it sends
// at once (the condition the notification was about has cleared). Forgetting
// an unknown key is not an error.
func (s *Store) ForgetSend(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM notifications WHERE key = ?", key); err != nil {
		return fmt.Errorf("notification %s: %w", key, err)
	}
	return nil
}
