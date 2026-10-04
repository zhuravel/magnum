package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the "sqlite" driver used for the snapshot
)

// copyStore snapshots the registry at src into a fresh temporary file with
// VACUUM INTO over a read-only connection, so a dry run can record its
// decisions without touching the real database. A missing src yields a path
// for an empty database. The returned func removes the copy.
func copyStore(ctx context.Context, src string) (string, func() error, error) {
	dir, err := os.MkdirTemp("", "magnum-dry-run-*")
	if err != nil {
		return "", nil, err
	}
	remove := func() error { return os.RemoveAll(dir) }
	dst := filepath.Join(dir, "magnum.db")
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return dst, remove, nil
	}
	u := url.URL{Scheme: "file", Path: src, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		_ = remove()
		return "", nil, err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		_ = remove()
		return "", nil, fmt.Errorf("snapshot %s: %w", src, err)
	}
	return dst, remove, nil
}
