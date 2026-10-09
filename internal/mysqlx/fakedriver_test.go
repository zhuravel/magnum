package mysqlx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// fakeServer is a scripted stand-in for MySQL registered as a database/sql
// driver, so the SQL magnum sends can be asserted without a server.
type fakeServer struct {
	mu    sync.Mutex
	execs []string // statements run through Exec, in order
	// queries records Query statements with their args.
	queries []fakeQuery
	// onQuery returns the result set (or an error) for a query.
	onQuery func(query string, args []driver.NamedValue) (cols []string, rows [][]driver.Value, err error)
	// onExec returns an error for a statement, or nil.
	onExec func(stmt string) error
	// stall makes every Exec, Query and Ping wait until its context ends
	// and return its error, as a stalled mysqld, or a DROP waiting on a
	// metadata lock, does.
	stall bool
}

// wait is the stall: ctx's error once it ends, at once without a stall.
func (s *fakeServer) wait(ctx context.Context) error {
	if !s.stall {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

type fakeQuery struct {
	SQL  string
	Args []driver.NamedValue
}

var (
	fakeMu      sync.Mutex
	fakeServers = map[string]*fakeServer{}
)

func init() { sql.Register("magnumfake", fakeDriver{}) }

// newFakeClient registers srv under the test's name and returns a Client on it.
func newFakeClient(t *testing.T, srv *fakeServer) *Client {
	t.Helper()
	fakeMu.Lock()
	fakeServers[t.Name()] = srv
	fakeMu.Unlock()
	// Drop the registration with the test: the map is global and would
	// otherwise retain every fixture (and its closures) for the whole run.
	t.Cleanup(func() {
		fakeMu.Lock()
		delete(fakeServers, t.Name())
		fakeMu.Unlock()
	})
	db, err := sql.Open("magnumfake", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() }) // runs before the delete above (LIFO), so no late Open can miss the server
	return &Client{db: db}
}

func (s *fakeServer) execed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.execs...)
}

func (s *fakeServer) queried() []fakeQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeQuery(nil), s.queries...)
}

type fakeDriver struct{}

func (fakeDriver) Open(name string) (driver.Conn, error) {
	fakeMu.Lock()
	srv := fakeServers[name]
	fakeMu.Unlock()
	if srv == nil {
		return nil, errors.New("fakedriver: unknown server " + name)
	}
	return &fakeConn{srv: srv}, nil
}

type fakeConn struct{ srv *fakeServer }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fakedriver: Prepare unsupported")
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fakedriver: Begin unsupported")
}

// Ping lets db.PingContext succeed (after the stall, if any).
func (c *fakeConn) Ping(ctx context.Context) error { return c.srv.wait(ctx) }

func (c *fakeConn) ExecContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.srv.mu.Lock()
	c.srv.execs = append(c.srv.execs, q)
	c.srv.mu.Unlock()
	if err := c.srv.wait(ctx); err != nil {
		return nil, err
	}
	if c.srv.onExec != nil {
		if err := c.srv.onExec(q); err != nil {
			return nil, err
		}
	}
	return driver.RowsAffected(0), nil
}

func (c *fakeConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.srv.mu.Lock()
	c.srv.queries = append(c.srv.queries, fakeQuery{SQL: q, Args: args})
	c.srv.mu.Unlock()
	if err := c.srv.wait(ctx); err != nil {
		return nil, err
	}
	if c.srv.onQuery == nil {
		return nil, errors.New("fakedriver: no onQuery")
	}
	cols, rows, err := c.srv.onQuery(q, args)
	if err != nil {
		return nil, err
	}
	return &fakeRows{cols: cols, rows: rows}, nil
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

// flat collapses whitespace so SQL assertions ignore formatting.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }
