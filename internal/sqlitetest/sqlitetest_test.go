package sqlitetest_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/sqlitetest"
)

// schemaImage is the image of an in-memory database with a parent and a
// child table and one parent row.
func schemaImage(t *testing.T) []byte {
	t.Helper()
	db, err := sqlitetest.Open(nil, sqlitetest.Params)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, q := range []string{
		"CREATE TABLE parent (id INTEGER PRIMARY KEY)",
		"CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent(id))",
		"INSERT INTO parent (id) VALUES (1)",
		"PRAGMA user_version = 7",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	image, err := sqlitetest.Image(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func open(t *testing.T, image []byte) *sql.DB {
	t.Helper()
	db, err := sqlitetest.Open(image, sqlitetest.Params)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func pragma[T any](t *testing.T, db *sql.DB, name string) T {
	t.Helper()
	var v T
	if err := db.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func TestOpenLoadsTheImageWithTheRegistrysConnectionSettings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := open(t, schemaImage(t))
	if v := pragma[int](t, db, "user_version"); v != 7 {
		t.Fatalf("user_version = %d, want the image's 7", v)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO child (id, parent_id) VALUES (1, 1)"); err != nil {
		t.Fatalf("insert a child of the image's parent: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO child (id, parent_id) VALUES (2, 99)"); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("an orphan child = %v, want a FOREIGN KEY error", err)
	}
	if v := pragma[int](t, db, "foreign_keys"); v != 1 {
		t.Errorf("foreign_keys = %d, want 1", v)
	}
	if v := pragma[int](t, db, "busy_timeout"); v != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", v)
	}
	if v := pragma[int](t, db, "temp_store"); v != 2 {
		t.Errorf("temp_store = %d, want 2 (memory)", v)
	}
}

func TestOpenKeepsTheDatabaseOffTheDisk(t *testing.T) {
	t.Parallel()
	db := open(t, schemaImage(t))
	if v := pragma[string](t, db, "journal_mode"); v != "memory" {
		t.Errorf("journal_mode = %q, want memory", v)
	}
	var seq int
	var name, file string
	if err := db.QueryRowContext(context.Background(), "PRAGMA database_list").Scan(&seq, &name, &file); err != nil {
		t.Fatal(err)
	}
	if name != "main" || file != "" {
		t.Errorf("database_list = %q %q, want main with no file", name, file)
	}
}

func TestOpenGivesEachDatabaseItsOwnCopyOfTheImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	image := schemaImage(t)
	a, b := open(t, image), open(t, image)
	if _, err := a.ExecContext(ctx, "INSERT INTO parent (id) VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.QueryRowContext(ctx, "SELECT count(*) FROM parent").Scan(&n); err != nil || n != 1 {
		t.Fatalf("parents in the other copy = %d, %v, want 1", n, err)
	}
	if empty := open(t, nil); pragma[int](t, empty, "user_version") != 0 {
		t.Fatal("a database opened without an image is not empty")
	}
}

func TestImageIsARollbackImageAndFileImageAWALFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	image := schemaImage(t)
	if image[18] != 1 || image[19] != 1 {
		t.Fatalf("Image header bytes 18, 19 = %d, %d, want 1, 1", image[18], image[19])
	}
	file := sqlitetest.FileImage(image)
	if file[18] != 2 || file[19] != 2 || image[18] != 1 {
		t.Fatalf("FileImage header = %d, %d (image now %d), want 2, 2 on a copy", file[18], file[19], image[18])
	}
	path := filepath.Join(t.TempDir(), "file.db")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if v := pragma[string](t, db, "journal_mode"); v != "wal" {
		t.Errorf("journal_mode of the file = %q, want wal", v)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM parent").Scan(&n); err != nil || n != 1 {
		t.Fatalf("parents in the file = %d, %v, want 1", n, err)
	}
	if _, err := sqlitetest.Open(file, sqlitetest.Params); err == nil || !strings.Contains(err.Error(), "WAL") {
		t.Fatalf("Open of a WAL file's bytes = %v, want a refusal naming WAL", err)
	}
}

func TestOpenKeepsTheDataWhenAQueryIsCancelled(t *testing.T) {
	t.Parallel()
	db := open(t, schemaImage(t))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var n int
	err := db.QueryRowContext(ctx, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT count(*) FROM c").Scan(&n)
	if err == nil {
		t.Fatal("an endless query ended without an error")
	}
	if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM parent").Scan(&n); err != nil || n != 1 {
		t.Fatalf("parents after a cancelled query = %d, %v, want 1: the database went with its connection", n, err)
	}
}

func TestALostConnectionFailsEveryLaterUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := open(t, schemaImage(t))
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A driver connection that reports itself bad is closed by database/sql.
	if err := conn.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Raw = %v", err)
	}
	conn.Close()
	if _, err := db.ExecContext(ctx, "INSERT INTO parent (id) VALUES (3)"); !errors.Is(err, sqlitetest.ErrLostConnection) {
		t.Fatalf("Exec after the connection was lost = %v, want ErrLostConnection", err)
	}
}
