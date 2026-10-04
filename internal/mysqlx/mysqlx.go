// Package mysqlx inventories and drops the per-worktree MySQL databases that
// Talkable's bin/worktree-setup creates (talkable_<env>[_<role>]__<slug>) on
// the local DBngin server.
//
// Reads are unrestricted; the one destructive operation, Drop, is gated by a
// Guard that refuses unsuffixed base databases, system schemas and any slug
// the caller has not explicitly allowed. SQL identifiers are validated against
// ^[A-Za-z0-9_]+$ and back-quoted before they reach the server.
//
// mysqlx talks to MySQL over the wire (database/sql + go-sql-driver/mysql),
// not through a subprocess, so it does not use execx; it never logs a DSN.
package mysqlx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	mysqldrv "github.com/go-sql-driver/mysql"
)

// DefaultDSN is DBngin's stock local server: root, empty password, 127.0.0.1:3306.
// The timeout bounds only the TCP dial; per-call deadlines come from the
// context passed to each method.
const DefaultDSN = "root:@tcp(127.0.0.1:3306)/?timeout=5s"

// DefaultPattern is the schemata LIKE pattern ListSuffixed uses:
// talkable_%__% with every literal underscore escaped by likeEscape. Queries
// pass it with an explicit ESCAPE clause, so it means the same thing under
// every sql_mode (NO_BACKSLASH_ESCAPES turns the backslash into a plain
// character in LIKE patterns).
var DefaultPattern = likePattern("talkable_")

// ErrInvalidDSN is wrapped by Open when the DSN cannot be parsed. The error
// never carries the driver's message or any part of the DSN: those can echo
// the password (the driver reads "root:secret/" as a network named
// "root:secret").
var ErrInvalidDSN = errors.New("mysqlx: invalid DSN")

// ErrNotFound is wrapped when MySQL reports an unknown database (1049) or a
// missing table (1146), so callers can treat "no schema loaded yet" apart from
// a connection or permission failure.
var ErrNotFound = errors.New("mysqlx: database or table not found")

// MySQL server error numbers mapped to ErrNotFound.
const (
	errBadDB   = 1049
	errNoTable = 1146
)

// Database is one per-worktree schema found on the server.
type Database struct {
	Name   string  // full schema name, e.g. talkable_development__review3
	Slug   string  // text after the last "__", e.g. review3
	SizeMB float64 // data_length + index_length of its tables, in MiB (0 when it has none)
}

// Client is a handle on the local MySQL server. It is safe for concurrent use.
type Client struct {
	db *sql.DB
}

// Open prepares a client for dsn (go-sql-driver/mysql format); an empty dsn
// means DefaultDSN. It validates the DSN but does not connect, so the daemon
// can start while DBngin is down; use Ping to probe the server. multiStatements
// is refused: every statement magnum sends is a single statement.
func Open(dsn string) (*Client, error) {
	if dsn == "" {
		dsn = DefaultDSN
	}
	cfg, err := mysqldrv.ParseDSN(dsn)
	if err != nil {
		return nil, invalidDSN(dsn)
	}
	if cfg.MultiStatements {
		return nil, errors.New("mysqlx: multiStatements=true is not allowed")
	}
	connector, err := mysqldrv.NewConnector(cfg)
	if err != nil {
		return nil, invalidDSN(dsn)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(4)
	return &Client{db: db}, nil
}

// invalidDSN builds the sanitized error for a DSN the driver rejected. The
// text is fixed apart from a hint chosen by looking at the DSN's shape, so no
// byte of the DSN or of the driver's message reaches the caller or the logs.
func invalidDSN(dsn string) error {
	hint := "want [user[:password]@][net[(addr)]]/dbname[?params]"
	if !strings.Contains(dsn, "/") {
		hint = "missing the '/' before the database name"
	}
	return fmt.Errorf("%w (%s; the driver's message is withheld because it can echo credentials)", ErrInvalidDSN, hint)
}

// Close releases the client's connections.
func (c *Client) Close() error { return c.db.Close() }

// Ping verifies that the server is reachable and the credentials work.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.db.PingContext(ctx); err != nil {
		return fmt.Errorf("mysqlx: ping: %w", err)
	}
	return nil
}

// ListSuffixed returns every schema named talkable_%__% (the per-worktree
// databases) with its slug and on-disk size, ordered by name. Schemas whose
// name ends in "__" (empty slug) are not worktree databases and are skipped.
// It only sees the talkable_ family; ListPrefixed lists any other prefix.
func (c *Client) ListSuffixed(ctx context.Context) ([]Database, error) {
	all, err := c.schemata(ctx, []string{DefaultPattern})
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, db := range all {
		if db.Slug != "" {
			out = append(out, db)
		}
	}
	return out, nil
}

// ListPrefixed returns every schema named <prefix><slug> for one of prefixes,
// with its slug and on-disk size, ordered by name. Each prefix is a pool's
// database base followed by the separator ("talkable_development__"); the slug
// is the non-empty rest of the name and must be what Slug reports, so a name
// like talkable_development__a__b (slug "b" to Slug and the guard) is not
// listed under that prefix. Prefixes are matched literally (LIKE wildcards in
// them are escaped) and case-sensitively, whatever the server's collation.
// Duplicate prefixes are ignored; no prefix at all lists nothing. A prefix
// that does not end in "__" or has an empty base is an error: no database
// under it could ever be dropped through Guard.
func (c *Client) ListPrefixed(ctx context.Context, prefixes []string) ([]Database, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	prefixes = slices.Sorted(slices.Values(prefixes))
	prefixes = slices.Compact(prefixes)
	patterns := make([]string, len(prefixes))
	for i, p := range prefixes {
		if !strings.HasSuffix(p, sep) || len(p) == len(sep) {
			return nil, fmt.Errorf("mysqlx: list databases: prefix %q must be <base>%s", p, sep)
		}
		patterns[i] = escapeLike(p) + "%"
	}
	all, err := c.schemata(ctx, patterns)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(db Database) bool { return !hasPrefixSlug(db, prefixes) }), nil
}

// hasPrefixSlug reports whether db is one of prefixes followed by exactly its
// slug (so Slug and the guard agree on what the slug is).
func hasPrefixSlug(db Database, prefixes []string) bool {
	return db.Slug != "" && slices.ContainsFunc(prefixes, func(p string) bool {
		return strings.HasPrefix(db.Name, p) && db.Name[len(p):] == db.Slug
	})
}

// schemata returns every schema matching any of the already-escaped LIKE
// patterns (escape character likeEscape, passed explicitly so the result does
// not depend on sql_mode), ordered by name. Slug is empty for names Slug
// rejects. It is the SQL half of ListSuffixed and ListPrefixed, split out so
// tests can aim it at throwaway names.
func (c *Client) schemata(ctx context.Context, patterns []string) ([]Database, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("mysqlx: list databases: %w", err)
	}
	defer conn.Close()

	// MySQL 8 caches table statistics for a day, which would report sizes from
	// before setup/seed ran. Ask for fresh numbers; servers without the
	// variable (5.7, MariaDB) just keep their own behaviour.
	_, _ = conn.ExecContext(ctx, "SET SESSION information_schema_stats_expiry = 0")

	conds := make([]string, len(patterns))
	args := make([]any, len(patterns))
	for i, p := range patterns {
		conds[i] = "s.schema_name LIKE ? ESCAPE '" + likeEscape + "'"
		args[i] = p
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT s.schema_name, COALESCE(SUM(t.data_length + t.index_length), 0)
		FROM information_schema.schemata AS s
		LEFT JOIN information_schema.tables AS t ON t.table_schema = s.schema_name
		WHERE (`+strings.Join(conds, " OR ")+`)
		GROUP BY s.schema_name
		ORDER BY s.schema_name`, args...)
	if err != nil {
		return nil, fmt.Errorf("mysqlx: list databases: %w", err)
	}
	defer rows.Close()

	var out []Database
	for rows.Next() {
		var name string
		var bytes sql.NullFloat64
		if err := rows.Scan(&name, &bytes); err != nil {
			return nil, fmt.Errorf("mysqlx: scan database row: %w", err)
		}
		slug, _ := Slug(name)
		out = append(out, Database{Name: name, Slug: slug, SizeMB: bytes.Float64 / (1024 * 1024)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysqlx: list databases: %w", err)
	}
	return out, nil
}

// SchemaMigrationsMax returns MAX(version) from dbName's Rails schema_migrations
// table. Versions are compared as strings, which is correct for Rails'
// fixed-width timestamps. An empty table yields "". A missing database or table
// wraps ErrNotFound.
func (c *Client) SchemaMigrationsMax(ctx context.Context, dbName string) (string, error) {
	q, err := quoteIdent(dbName)
	if err != nil {
		return "", fmt.Errorf("mysqlx: schema_migrations max: %w", err)
	}
	var v sql.NullString
	if err := c.db.QueryRowContext(ctx, "SELECT MAX(version) FROM "+q+".`schema_migrations`").Scan(&v); err != nil {
		return "", fmt.Errorf("mysqlx: schema_migrations max of %s: %w", dbName, mapNotFound(err))
	}
	return v.String, nil
}

// Drop removes one database (DROP DATABASE IF EXISTS, so a retry after a crash
// is harmless) after g.Check approves its name. A refusal wraps ErrGuard and
// sends nothing to the server.
func (c *Client) Drop(ctx context.Context, name string, g Guard) error {
	if err := g.Check(name); err != nil {
		return err
	}
	q, err := quoteIdent(name)
	if err != nil { // unreachable after Check; kept so the SQL line stands on its own
		return fmt.Errorf("%w: %w", ErrGuard, err)
	}
	if _, err := c.db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+q); err != nil {
		return fmt.Errorf("mysqlx: drop %s: %w", name, err)
	}
	return nil
}

// DropResult is the outcome for one name in DropAll; Err is nil on success.
type DropResult struct {
	Name string
	Err  error
}

// DropAll drops each name in order with Drop and returns one result per name,
// in input order. A failure (guard refusal or server error) does not stop the
// remaining names; a cancelled context fails the names it never reached.
func (c *Client) DropAll(ctx context.Context, names []string, g Guard) []DropResult {
	out := make([]DropResult, 0, len(names))
	for _, n := range names {
		var err error
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("mysqlx: drop %s: %w", n, ctxErr)
		} else {
			err = c.Drop(ctx, n, g)
		}
		out = append(out, DropResult{Name: n, Err: err})
	}
	return out
}

// mapNotFound tags "unknown database" / "table doesn't exist" errors.
func mapNotFound(err error) error {
	var me *mysqldrv.MySQLError
	if errors.As(err, &me) && (me.Number == errBadDB || me.Number == errNoTable) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}
