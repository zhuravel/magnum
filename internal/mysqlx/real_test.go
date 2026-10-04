package mysqlx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The tests in this file talk to the real DBngin server and are skipped when
// it is unreachable. The only mutations are creating and dropping databases
// whose names start with testPrefix; testDropper refuses anything else.
const testPrefix = "magnum_test_"

// liveOpTimeout bounds every single SQL call in this file: the DSN timeout
// covers only the TCP dial, so a stalled server must not hang the test binary
// until it is killed (leaving schemas behind).
const liveOpTimeout = 15 * time.Second

// boundedCtx returns a context cancelled with the test and after d.
func boundedCtx(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(t.Context(), d)
}

// cleanupCtx returns a fresh bounded context for t.Cleanup bodies:
// t.Context() is already cancelled when cleanups run, and a cancelled context
// would skip the very drops that remove the test's schemas.
func cleanupCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), liveOpTimeout)
}

// realClient returns a client on the local server or skips the test.
func realClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("MAGNUM_LIVE_MYSQL") == "" {
		t.Skip("set MAGNUM_LIVE_MYSQL=1 to run against the local DBngin server")
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:3306", 500*time.Millisecond)
	if err != nil {
		t.Skipf("MySQL not reachable at 127.0.0.1:3306: %v", err)
	}
	conn.Close()
	c, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx, cancel := boundedCtx(t, 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("MySQL answered but ping failed (credentials?): %v", err)
	}
	return c
}

// testDropper creates and removes throwaway databases, all named magnum_test_*.
type testDropper struct {
	t *testing.T
	c *Client
}

func (d testDropper) mustBeTestDB(name string) {
	d.t.Helper()
	if !strings.HasPrefix(name, testPrefix) || validIdent(name) != nil {
		d.t.Fatalf("refusing to touch %q: tests may only manage %s* databases", name, testPrefix)
	}
}

func (d testDropper) create(name string) {
	d.t.Helper()
	d.mustBeTestDB(name)
	ctx, cancel := boundedCtx(d.t, liveOpTimeout)
	defer cancel()
	if _, err := d.c.db.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		d.t.Fatalf("create %s: %v", name, err)
	}
	d.t.Cleanup(func() { d.drop(name) })
}

// drop runs in t.Cleanup (after t.Context() is cancelled), so it uses its own
// bounded context.
func (d testDropper) drop(name string) {
	d.t.Helper()
	d.mustBeTestDB(name)
	ctx, cancel := cleanupCtx()
	defer cancel()
	if _, err := d.c.db.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
		d.t.Errorf("cleanup drop %s: %v", name, err)
	}
}

func (d testDropper) exists(name string) bool {
	d.t.Helper()
	ctx, cancel := boundedCtx(d.t, liveOpTimeout)
	defer cancel()
	var n int
	if err := d.c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?", name).Scan(&n); err != nil {
		d.t.Fatal(err)
	}
	return n == 1
}

func randID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func TestRealServerLifecycle(t *testing.T) {
	c := realClient(t)
	d := testDropper{t, c}
	ctx, cancel := boundedCtx(t, 60*time.Second)
	defer cancel()

	id := randID(t)
	base := testPrefix + id
	rev1, rev2, pr9 := base+"__review1", base+"__review2", base+"__pr9fix"
	decoyNoSuffix := base + "_s1"     // single underscore: not a suffixed database
	decoyUnderscores := base + "_a_b" // would match an UNESCAPED LIKE "...%__%"
	for _, n := range []string{rev1, rev2, pr9, decoyNoSuffix, decoyUnderscores} {
		d.create(n)
	}
	if _, err := c.db.ExecContext(ctx, "CREATE TABLE `"+rev1+"`.schema_migrations (version VARCHAR(255) NOT NULL PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, "INSERT INTO `"+rev1+"`.schema_migrations VALUES ('20260101000000'), ('20260620120000'), ('20250505050505')"); err != nil {
		t.Fatal(err)
	}

	t.Run("list uses an escaped LIKE and reports slugs and sizes", func(t *testing.T) {
		got, err := c.schemata(ctx, []string{likePattern(base)})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		bySlug := map[string]Database{}
		for _, db := range got {
			names = append(names, db.Name)
			bySlug[db.Slug] = db
		}
		want := []string{pr9, rev1, rev2}
		sort.Strings(want)
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Fatalf("names = %v, want %v (decoys must not match the SQL filter)", names, want)
		}
		if bySlug["review1"].SizeMB <= 0 {
			t.Errorf("review1 has a table but SizeMB = %v (stale statistics?)", bySlug["review1"].SizeMB)
		}
		if bySlug["review2"].SizeMB != 0 {
			t.Errorf("review2 is empty but SizeMB = %v", bySlug["review2"].SizeMB)
		}
	})

	t.Run("unescaped pattern would have matched the decoy", func(t *testing.T) {
		// Guards the test itself: proves the decoy exists to catch missing escapes.
		got, err := c.schemata(ctx, []string{base + "%__%"})
		if err != nil {
			t.Fatal(err)
		}
		var sawDecoy bool
		for _, db := range got {
			if db.Name == decoyUnderscores {
				sawDecoy = true
			}
		}
		if !sawDecoy {
			t.Fatalf("decoy %s not matched by the unescaped pattern; the escaping test proves nothing", decoyUnderscores)
		}
	})

	t.Run("ListPrefixed lists exactly the prefix's slugs", func(t *testing.T) {
		got, err := c.ListPrefixed(ctx, []string{base + "__"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{pr9, rev1, rev2}
		sort.Strings(want)
		if names := dbNames(got); strings.Join(names, ",") != strings.Join(want, ",") {
			t.Fatalf("names = %v, want %v (decoys must not match)", names, want)
		}
		for _, db := range got {
			if slug, ok := Slug(db.Name); !ok || slug != db.Slug {
				t.Errorf("bad row %+v", db)
			}
		}
	})

	t.Run("patterns do not depend on NO_BACKSLASH_ESCAPES", func(t *testing.T) {
		// A private pool on one connection so the session mode sticks.
		c2, err := Open("")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c2.Close() })
		c2.db.SetMaxOpenConns(1)
		if _, err := c2.db.ExecContext(ctx, "SET SESSION sql_mode = CONCAT_WS(',', NULLIF(@@SESSION.sql_mode, ''), 'NO_BACKSLASH_ESCAPES')"); err != nil {
			t.Fatal(err)
		}
		var mode string
		if err := c2.db.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(mode, "NO_BACKSLASH_ESCAPES") {
			t.Fatalf("session sql_mode = %q; the mode did not stick, so this test proves nothing", mode)
		}
		want := []string{pr9, rev1, rev2}
		sort.Strings(want)
		viaPrefix, err := c2.ListPrefixed(ctx, []string{base + "__"})
		if err != nil {
			t.Fatal(err)
		}
		viaPattern, err := c2.schemata(ctx, []string{likePattern(base)})
		if err != nil {
			t.Fatal(err)
		}
		for label, got := range map[string][]Database{"ListPrefixed": viaPrefix, "schemata(likePattern)": viaPattern} {
			if names := dbNames(got); strings.Join(names, ",") != strings.Join(want, ",") {
				t.Errorf("%s under NO_BACKSLASH_ESCAPES: names = %v, want %v", label, names, want)
			}
		}
	})

	t.Run("schema migrations max", func(t *testing.T) {
		v, err := c.SchemaMigrationsMax(ctx, rev1)
		if err != nil || v != "20260620120000" {
			t.Fatalf("got %q, %v", v, err)
		}
		if _, err := c.SchemaMigrationsMax(ctx, rev2); !errors.Is(err, ErrNotFound) {
			t.Errorf("database without schema_migrations: want ErrNotFound, got %v", err)
		}
		if _, err := c.SchemaMigrationsMax(ctx, base+"__nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown database: want ErrNotFound, got %v", err)
		}
	})

	g := Guard{AllowRegexp: regexp.MustCompile(`^review\d+$`), Prefix: testPrefix}

	t.Run("drop honours the guard", func(t *testing.T) {
		for _, n := range []string{pr9, decoyNoSuffix} {
			if err := c.Drop(ctx, n, g); !errors.Is(err, ErrGuard) {
				t.Errorf("Drop(%s) = %v, want ErrGuard", n, err)
			}
			if !d.exists(n) {
				t.Errorf("%s vanished although the guard refused it", n)
			}
		}
		if err := c.Drop(ctx, rev1, g); err != nil {
			t.Fatal(err)
		}
		if d.exists(rev1) {
			t.Fatalf("%s still exists after Drop", rev1)
		}
		if err := c.Drop(ctx, rev1, g); err != nil {
			t.Fatalf("second Drop should be a no-op, got %v", err)
		}
	})

	t.Run("dropall reports per name", func(t *testing.T) {
		res := c.DropAll(ctx, []string{rev2, pr9}, g)
		if len(res) != 2 || res[0].Err != nil || !errors.Is(res[1].Err, ErrGuard) {
			t.Fatalf("results = %+v", res)
		}
		if d.exists(rev2) || !d.exists(pr9) {
			t.Fatalf("rev2 exists=%v (want false), pr9 exists=%v (want true)", d.exists(rev2), d.exists(pr9))
		}
	})
}

// TestRealServerListSuffixedReadOnly runs the production query against the real
// inventory without changing anything.
func TestRealServerListSuffixedReadOnly(t *testing.T) {
	c := realClient(t)
	ctx, cancel := boundedCtx(t, 30*time.Second)
	defer cancel()
	got, err := c.ListSuffixed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].Name < got[j].Name }) {
		t.Error("result is not ordered by name")
	}
	for _, db := range got {
		if !strings.HasPrefix(db.Name, "talkable_") || !strings.Contains(db.Name, "__") {
			t.Errorf("unexpected name %q", db.Name)
		}
		if slug, ok := Slug(db.Name); !ok || slug != db.Slug || db.SizeMB < 0 {
			t.Errorf("bad row %+v", db)
		}
	}
	t.Logf("%d suffixed databases on the server", len(got))

	// ListPrefixed on a real prefix is the same family, narrowed.
	const prefix = "talkable_development__"
	sub, err := c.ListPrefixed(ctx, []string{prefix})
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range sub {
		if !strings.HasPrefix(db.Name, prefix) || db.Name[len(prefix):] != db.Slug {
			t.Errorf("ListPrefixed(%s) returned %+v", prefix, db)
		}
	}
}

func dbNames(dbs []Database) []string {
	out := make([]string, len(dbs))
	for i, db := range dbs {
		out[i] = db.Name
	}
	return out
}

func TestRealServerPingAndOpenAreIndependent(t *testing.T) {
	c := realClient(t)
	ctx, cancel := boundedCtx(t, 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}
