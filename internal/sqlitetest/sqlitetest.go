// Package sqlitetest keeps a test's SQLite database in memory: a private
// database on one connection, loaded from the image of a database a test
// binary migrated once, so the test writes no database file. storetest and
// registrytest build their registries on it, and the store package's own
// tests use it directly (they cannot import storetest). It imports only the
// SQLite driver and the standard library. Only tests and those helpers
// import it.
package sqlitetest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"sync"

	"modernc.org/sqlite"
)

// Params are the connection settings of store.Open and registry.Open
// (busy_timeout 5 s, foreign keys on, temporary tables in memory, BEGIN
// IMMEDIATE) without the two that only a file has: journal_mode(WAL), which
// a memory database refuses (its journal is in memory), and synchronous.
const Params = "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)&_txlock=immediate"

// FastFile is what a test's own raw writer of a database file (an old schema
// a migration test starts from) adds to its DSN: the rollback journal stays
// in memory and nothing is synced, so the file is written once.
const FastFile = "_pragma=journal_mode(MEMORY)&_pragma=synchronous(OFF)"

// ErrLostConnection is what every use of a database from Open returns after
// its only connection was closed: the database lived in that connection, so
// a new one would be empty.
var ErrLostConnection = errors.New("sqlitetest: the in-memory database lost its only connection")

// Open opens a private in-memory database with params (Params, or a test's
// own), loaded from image (nil starts empty). The pool holds one connection,
// which never expires, because the connection is the database: once it is
// closed every use fails with ErrLostConnection rather than going on with an
// empty database. The image is copied, so one image serves every test of a
// binary. It must be a rollback image (header bytes 18 and 19 = 1), which
// Image returns; a WAL file's bytes are refused (FileImage turns one into the
// other).
func Open(image []byte, params string) (*sql.DB, error) {
	if len(image) > 19 && (image[18] != 1 || image[19] != 1) {
		return nil, fmt.Errorf("sqlitetest: the image's header says file format %d/%d, want 1/1 (a WAL file's bytes; use Image of an in-memory database)", image[18], image[19])
	}
	inner, err := sqlite.NewConnector(":memory:?" + params)
	if err != nil {
		return nil, fmt.Errorf("sqlitetest: %w", err)
	}
	db := sql.OpenDB(&connector{Connector: inner, image: image})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	return db, nil
}

// connector opens the database's one connection and loads the image into it;
// it refuses to open another.
type connector struct {
	driver.Connector
	image []byte

	mu     sync.Mutex
	opened bool
}

// Connect opens the connection the first time it succeeds and fails every
// time after.
func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.opened {
		return nil, ErrLostConnection
	}
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if c.image != nil {
		d, ok := conn.(interface{ Deserialize([]byte) error })
		if !ok {
			conn.Close()
			return nil, fmt.Errorf("sqlitetest: the driver's %T cannot load an image", conn)
		}
		if err := d.Deserialize(c.image); err != nil {
			conn.Close()
			return nil, fmt.Errorf("sqlitetest: load the image: %w", err)
		}
	}
	c.opened = true
	return conn, nil
}

// Image returns the bytes of db's main database (sqlite3_serialize): for an
// in-memory database, the file SQLite would write, with a rollback header
// (bytes 18 and 19 = 1), which Open loads.
func Image(ctx context.Context, db *sql.DB) ([]byte, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlitetest: %w", err)
	}
	defer conn.Close()
	var image []byte
	if err := conn.Raw(func(dc any) error {
		s, ok := dc.(interface{ Serialize() ([]byte, error) })
		if !ok {
			return fmt.Errorf("the driver's %T cannot serialize", dc)
		}
		var serr error
		image, serr = s.Serialize()
		return serr
	}); err != nil {
		return nil, fmt.Errorf("sqlitetest: serialize: %w", err)
	}
	return image, nil
}

// FileImage returns a copy of image with header bytes 18 and 19 set to 2,
// the bytes of a WAL database checkpointed and closed: what a test writes to
// a file that store.Open or registry.Open then opens.
func FileImage(image []byte) []byte {
	out := slices.Clone(image)
	if len(out) > 19 {
		out[18], out[19] = 2, 2
	}
	return out
}
