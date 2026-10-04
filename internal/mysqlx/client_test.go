package mysqlx

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	mysqldrv "github.com/go-sql-driver/mysql"
)

var reviewGuard = Guard{AllowRegexp: regexp.MustCompile(`^review\d+$`)}

func TestOpenValidatesDSN(t *testing.T) {
	if _, err := Open("not a dsn"); err == nil {
		t.Fatal("Open accepted a malformed DSN")
	}
	c, err := Open("")
	if err != nil {
		t.Fatalf("Open(\"\") should use DefaultDSN: %v", err)
	}
	defer c.Close()
	// Lazy: no connection is made, so this succeeds with no server running.
	c2, err := Open("root:@tcp(127.0.0.1:1)/?timeout=100ms")
	if err != nil {
		t.Fatalf("Open must not dial: %v", err)
	}
	defer c2.Close()
	if err := c2.Ping(context.Background()); err == nil {
		t.Fatal("Ping against a closed port should fail")
	}
}

func TestOpenRejectsMultiStatements(t *testing.T) {
	if _, err := Open("root:@tcp(127.0.0.1:3306)/?multiStatements=true"); err == nil {
		t.Fatal("Open accepted multiStatements=true")
	}
}

// The driver's own messages can echo the DSN ("default addr for network
// 'root:hunter2secret' unknown"), so Open never returns them.
func TestOpenErrorDoesNotLeakPassword(t *testing.T) {
	for _, dsn := range []string{
		"root:hunter2secret@bogus",
		"root:hunter2secret/",                      // no @: user:password parsed as the network name
		"root:hunter2secret",                       // no / at all
		"root:hunter2secret@",                      // nothing after @
		"root:hunter2secret@tcp(127.0.0.1:3306",    // unterminated address
		"root:hunter2secret@tcp(127.0.0.1:3306)",   // no slash
		"root:hunter2secret@unix2/db",              // unknown network, default address missing
		"root:hunter2secret@tcp(h:1)/db?timeout=x", // bad parameter value
		"root:hunter2secret@tcp(h:1)/db?x=%zz",     // bad escape in a parameter
		"root:hunter2secret@tcp(h:1)/db?tls=hunter2secret",
		"root:hunter2secret@tcp(h:1)/db?parseTime=hunter2secret",
		"root:hunter2secret@tcp(h:1)/db?interpolateParams=true&collation=big5_chinese_ci",
	} {
		c, err := Open(dsn)
		if err == nil {
			c.Close()
			t.Errorf("Open(%q) accepted a malformed DSN", dsn)
			continue
		}
		if strings.Contains(err.Error(), "hunter2secret") {
			t.Errorf("Open(%q) error leaks the password: %v", dsn, err)
		}
		if !errors.Is(err, ErrInvalidDSN) {
			t.Errorf("Open(%q) error = %v, want ErrInvalidDSN", dsn, err)
		}
	}
}

func TestPing(t *testing.T) {
	c := newFakeClient(t, &fakeServer{})
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func listServer(rows [][]driver.Value) *fakeServer {
	return &fakeServer{onQuery: func(q string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return []string{"schema_name", "bytes"}, rows, nil
	}}
}

func TestListSuffixed(t *testing.T) {
	srv := listServer([][]driver.Value{
		{"talkable_development__review1", int64(3 * 1024 * 1024)},
		{"talkable_test_shard_1001__repo12", []byte("5242880")}, // DECIMAL arrives as text
		{"talkable_test__empty", nil},                           // schema without tables
		{"talkable_x__", int64(1)},                              // empty slug is skipped
		{"talkable_test__pr27087fix", float64(1572864)},
	})
	c := newFakeClient(t, srv)
	got, err := c.ListSuffixed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Database{
		{Name: "talkable_development__review1", Slug: "review1", SizeMB: 3},
		{Name: "talkable_test_shard_1001__repo12", Slug: "repo12", SizeMB: 5},
		{Name: "talkable_test__empty", Slug: "empty", SizeMB: 0},
		{Name: "talkable_test__pr27087fix", Slug: "pr27087fix", SizeMB: 1.5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListSuffixed =\n%+v\nwant\n%+v", got, want)
	}

	qs := srv.queried()
	if len(qs) != 1 {
		t.Fatalf("want 1 query, got %d: %+v", len(qs), qs)
	}
	q := flat(qs[0].SQL)
	for _, frag := range []string{"information_schema.schemata", "information_schema.tables", "LIKE ? ESCAPE '|'", "data_length + t.index_length"} {
		if !strings.Contains(q, frag) {
			t.Errorf("query lacks %q: %s", frag, q)
		}
	}
	// '|' is the explicit escape, so the pattern means the same under
	// NO_BACKSLASH_ESCAPES; a backslash escape would not.
	if len(qs[0].Args) != 1 || qs[0].Args[0].Value != `talkable|_%|_|_%` {
		t.Fatalf("LIKE arg = %+v, want the |-escaped pattern", qs[0].Args)
	}
	if strings.Contains(q, `\`) || strings.Contains(fmt.Sprint(qs[0].Args[0].Value), `\`) {
		t.Fatalf("query or pattern relies on backslash escapes: %s / %v", q, qs[0].Args)
	}
	// Sizes should be refreshed (best effort) before reading information_schema.
	var sawStats bool
	for _, e := range srv.execed() {
		if strings.Contains(e, "information_schema_stats_expiry") {
			sawStats = true
		}
	}
	if !sawStats {
		t.Errorf("no stats refresh issued; execs=%v", srv.execed())
	}
}

func TestListSuffixedToleratesStatsVariableMissing(t *testing.T) {
	srv := listServer([][]driver.Value{{"talkable_test__a", int64(0)}})
	srv.onExec = func(string) error { return &mysqldrv.MySQLError{Number: 1193, Message: "Unknown system variable"} }
	c := newFakeClient(t, srv)
	got, err := c.ListSuffixed(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("ListSuffixed = %v, %v", got, err)
	}
}

func TestListSuffixedQueryError(t *testing.T) {
	srv := &fakeServer{onQuery: func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return nil, nil, errors.New("boom")
	}}
	c := newFakeClient(t, srv)
	if _, err := c.ListSuffixed(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want wrapped boom, got %v", err)
	}
}

// The fake server returns rows a LIKE scan could not (case-insensitive hits,
// names whose slug Slug disagrees with): the SQL side is only a coarse filter
// and ListPrefixed re-checks every name.
func TestListPrefixedQueryAndFilter(t *testing.T) {
	srv := listServer([][]driver.Value{
		{"talkable_development__review1", int64(2 * 1024 * 1024)},
		{"talkable_development__a__b", int64(1)},    // slug is "b" to Slug: not under this prefix
		{"talkable_development___x", int64(1)},      // Slug says "x", rest is "_x"
		{"Talkable_Development__review2", int64(1)}, // case-insensitive collation hit
		{"talkable_development__", int64(1)},        // empty slug
		{"talkable_test__repo3", []byte("1048576")}, // second prefix
		{"your_app_development__pr7", nil},          // third prefix, no tables
		{"your_app_development_x__pr8", int64(1)},   // LIKE wildcard artefact: _ matched "_x"
		{"talkable_testing__repo3", int64(1)},       // starts like talkable_test but not the prefix
	})
	c := newFakeClient(t, srv)
	got, err := c.ListPrefixed(context.Background(), []string{
		"talkable_test__", "your_app_development__", "talkable_development__", "talkable_test__", // unsorted + duplicate
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Database{
		{Name: "talkable_development__review1", Slug: "review1", SizeMB: 2},
		{Name: "talkable_test__repo3", Slug: "repo3", SizeMB: 1},
		{Name: "your_app_development__pr7", Slug: "pr7"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListPrefixed =\n%+v\nwant\n%+v", got, want)
	}

	qs := srv.queried()
	if len(qs) != 1 {
		t.Fatalf("want 1 query, got %d: %+v", len(qs), qs)
	}
	q := flat(qs[0].SQL)
	wantWhere := "WHERE (s.schema_name LIKE ? ESCAPE '|' OR s.schema_name LIKE ? ESCAPE '|' OR s.schema_name LIKE ? ESCAPE '|')"
	if !strings.Contains(q, wantWhere) || !strings.HasSuffix(q, "GROUP BY s.schema_name ORDER BY s.schema_name") {
		t.Fatalf("query = %s\nwant it to contain %s", q, wantWhere)
	}
	var args []any
	for _, a := range qs[0].Args {
		args = append(args, a.Value)
	}
	wantArgs := []any{`talkable|_development|_|_%`, `talkable|_test|_|_%`, `your|_app|_development|_|_%`} // sorted, deduplicated
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %q, want %q", args, wantArgs)
	}
}

func TestListPrefixedEdgeCases(t *testing.T) {
	srv := listServer(nil)
	c := newFakeClient(t, srv)
	for _, none := range [][]string{nil, {}} {
		got, err := c.ListPrefixed(context.Background(), none)
		if got != nil || err != nil {
			t.Errorf("ListPrefixed(%v) = %v, %v; want nil, nil", none, got, err)
		}
	}
	if len(srv.queried()) != 0 {
		t.Fatalf("an empty prefix list must not query: %+v", srv.queried())
	}
	for _, bad := range []string{"talkable_development", "talkable_development_", "talkable_", "", "__", "talkable_development__ "} {
		if _, err := c.ListPrefixed(context.Background(), []string{"talkable_test__", bad}); err == nil {
			t.Errorf("prefix %q accepted", bad)
		}
	}
	if len(srv.queried()) != 0 {
		t.Fatalf("a rejected prefix must not query: %+v", srv.queried())
	}
	// A valid list that matches nothing is an empty result, not an error.
	if got, err := c.ListPrefixed(context.Background(), []string{"talkable_test__"}); err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestListPrefixedEscapesWildcards(t *testing.T) {
	srv := listServer(nil)
	c := newFakeClient(t, srv)
	if _, err := c.ListPrefixed(context.Background(), []string{"a%b|c_d__"}); err != nil {
		t.Fatal(err)
	}
	qs := srv.queried()
	if len(qs) != 1 || len(qs[0].Args) != 1 || qs[0].Args[0].Value != `a|%b||c|_d|_|_%` {
		t.Fatalf("queries = %+v", qs)
	}
}

func TestListPrefixedQueryError(t *testing.T) {
	srv := &fakeServer{onQuery: func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return nil, nil, errors.New("boom")
	}}
	c := newFakeClient(t, srv)
	if _, err := c.ListPrefixed(context.Background(), []string{"talkable_test__"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want wrapped boom, got %v", err)
	}
}

// ListPrefixed satisfies the interface internal/slots looks for.
var _ interface {
	ListPrefixed(ctx context.Context, prefixes []string) ([]Database, error)
} = (*Client)(nil)

func TestSchemaMigrationsMax(t *testing.T) {
	srv := &fakeServer{onQuery: func(q string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return []string{"max"}, [][]driver.Value{{[]byte("20260620120000")}}, nil
	}}
	c := newFakeClient(t, srv)
	got, err := c.SchemaMigrationsMax(context.Background(), "talkable_development__review1")
	if err != nil || got != "20260620120000" {
		t.Fatalf("got %q, %v", got, err)
	}
	if q := flat(srv.queried()[0].SQL); q != "SELECT MAX(version) FROM `talkable_development__review1`.`schema_migrations`" {
		t.Fatalf("unexpected SQL: %s", q)
	}
}

func TestSchemaMigrationsMaxEmptyTable(t *testing.T) {
	srv := &fakeServer{onQuery: func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return []string{"max"}, [][]driver.Value{{nil}}, nil
	}}
	c := newFakeClient(t, srv)
	got, err := c.SchemaMigrationsMax(context.Background(), "talkable_development__review1")
	if err != nil || got != "" {
		t.Fatalf("got %q, %v; want empty, nil", got, err)
	}
}

func TestSchemaMigrationsMaxNotFound(t *testing.T) {
	for _, num := range []uint16{1049, 1146} {
		srv := &fakeServer{onQuery: func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
			return nil, nil, &mysqldrv.MySQLError{Number: num, Message: "gone"}
		}}
		c := newFakeClient(t, srv)
		_, err := c.SchemaMigrationsMax(context.Background(), "talkable_development__review1")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("error %d: want ErrNotFound, got %v", num, err)
		}
	}
	// Other server errors are not "not found".
	srv := &fakeServer{onQuery: func(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return nil, nil, &mysqldrv.MySQLError{Number: 1045, Message: "denied"}
	}}
	c := newFakeClient(t, srv)
	if _, err := c.SchemaMigrationsMax(context.Background(), "talkable_development__review1"); errors.Is(err, ErrNotFound) || err == nil {
		t.Fatalf("access denied must not map to ErrNotFound: %v", err)
	}
}

func TestSchemaMigrationsMaxRejectsBadName(t *testing.T) {
	srv := &fakeServer{}
	c := newFakeClient(t, srv)
	if _, err := c.SchemaMigrationsMax(context.Background(), "x`; DROP DATABASE y; --"); err == nil {
		t.Fatal("accepted an injection-shaped name")
	}
	if len(srv.queried()) != 0 {
		t.Fatalf("query ran for an invalid name: %+v", srv.queried())
	}
}

func TestDropIssuesQuotedDrop(t *testing.T) {
	srv := &fakeServer{}
	c := newFakeClient(t, srv)
	if err := c.Drop(context.Background(), "talkable_development__review3", reviewGuard); err != nil {
		t.Fatal(err)
	}
	want := []string{"DROP DATABASE IF EXISTS `talkable_development__review3`"}
	if got := srv.execed(); !reflect.DeepEqual(got, want) {
		t.Fatalf("execs = %v, want %v", got, want)
	}
}

func TestDropRefusedNeverReachesServer(t *testing.T) {
	srv := &fakeServer{}
	c := newFakeClient(t, srv)
	for _, name := range []string{
		"talkable_development",        // unsuffixed base
		"talkable_development__repo3", // slug not allowed
		"mysql",                       // system schema
		"talkable_x__review1`; --",    // injection
		"",
	} {
		err := c.Drop(context.Background(), name, reviewGuard)
		if !errors.Is(err, ErrGuard) {
			t.Errorf("Drop(%q) = %v, want ErrGuard", name, err)
		}
	}
	if err := c.Drop(context.Background(), "talkable_development__review3", Guard{}); !errors.Is(err, ErrGuard) {
		t.Errorf("zero Guard must refuse: %v", err)
	}
	if got := srv.execed(); len(got) != 0 {
		t.Fatalf("server saw statements for refused drops: %v", got)
	}
}

func TestDropServerError(t *testing.T) {
	srv := &fakeServer{onExec: func(string) error { return errors.New("lock wait timeout") }}
	c := newFakeClient(t, srv)
	err := c.Drop(context.Background(), "talkable_test__review2", reviewGuard)
	if err == nil || !strings.Contains(err.Error(), "lock wait timeout") || !strings.Contains(err.Error(), "talkable_test__review2") {
		t.Fatalf("want wrapped error naming the db, got %v", err)
	}
	if errors.Is(err, ErrGuard) {
		t.Fatal("server failure misreported as guard refusal")
	}
}

func TestDropAllPerNameResults(t *testing.T) {
	srv := &fakeServer{onExec: func(stmt string) error {
		if strings.Contains(stmt, "talkable_test__review2") {
			return errors.New("busy")
		}
		return nil
	}}
	c := newFakeClient(t, srv)
	names := []string{
		"talkable_development__review1",
		"talkable_test__review2",      // server error
		"talkable_development__repo9", // guard refusal
		"talkable_test_reporting__review4",
	}
	res := c.DropAll(context.Background(), names, reviewGuard)
	if len(res) != len(names) {
		t.Fatalf("want one result per name, got %d", len(res))
	}
	for i, r := range res {
		if r.Name != names[i] {
			t.Errorf("result %d name = %q, want input order %q", i, r.Name, names[i])
		}
	}
	if res[0].Err != nil || res[3].Err != nil {
		t.Errorf("successes carry errors: %v / %v", res[0].Err, res[3].Err)
	}
	if res[1].Err == nil || errors.Is(res[1].Err, ErrGuard) {
		t.Errorf("server error expected for review2: %v", res[1].Err)
	}
	if !errors.Is(res[2].Err, ErrGuard) {
		t.Errorf("guard refusal expected for repo9: %v", res[2].Err)
	}
	// Drops continue past failures; the refused name never reaches the server.
	want := []string{
		"DROP DATABASE IF EXISTS `talkable_development__review1`",
		"DROP DATABASE IF EXISTS `talkable_test__review2`",
		"DROP DATABASE IF EXISTS `talkable_test_reporting__review4`",
	}
	if got := srv.execed(); !reflect.DeepEqual(got, want) {
		t.Fatalf("execs = %v, want %v", got, want)
	}
}

func TestDropAllStopsOnCancelledContext(t *testing.T) {
	srv := &fakeServer{}
	c := newFakeClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := c.DropAll(ctx, []string{"talkable_test__review1", "talkable_test__review2"}, reviewGuard)
	for _, r := range res {
		if !errors.Is(r.Err, context.Canceled) {
			t.Errorf("%s: want context.Canceled, got %v", r.Name, r.Err)
		}
	}
	if got := srv.execed(); len(got) != 0 {
		t.Fatalf("statements ran on a cancelled context: %v", got)
	}
}

func TestDropAllEmpty(t *testing.T) {
	c := newFakeClient(t, &fakeServer{})
	if res := c.DropAll(context.Background(), nil, reviewGuard); len(res) != 0 {
		t.Fatalf("want no results, got %v", res)
	}
}
