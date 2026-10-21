package github

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// heard collects the calls a Client's Meter hears.
type heard struct {
	mu    sync.Mutex
	calls []Call
}

func (h *heard) meter(c Call) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, c)
}

func (h *heard) all() []Call {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Call(nil), h.calls...)
}

// The meter hears a GraphQL call with the op its context carries, the
// call's kind and what GitHub charged for it.
func TestTheMeterHearsAGraphQLCallWithItsOpAndCost(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: fixture(t, "reviews.json")}}}}
	var h heard
	c := &Client{Run: f, Meter: h.meter}
	if _, err := c.ReviewsWithMarker(WithOp(context.Background(), "round.setup"), "talkable", "talkable", 11973, ""); err != nil {
		t.Fatal(err)
	}
	want := Call{Op: "round.setup", Kind: "reviews", GraphQL: true,
		Rate: RateLimit{Limit: 5000, Cost: 1, Remaining: 4963, Used: 37, ResetAt: time.Date(2026, 10, 2, 21, 50, 10, 0, time.UTC)}}
	if got := h.all(); len(got) != 1 || got[0] != want {
		t.Fatalf("heard %+v, want [%+v]", got, want)
	}
}

// A REST call is heard as one request of its kind, with no rate limit; a
// call whose context carries no op is heard by its kind alone.
func TestTheMeterCountsARESTCall(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: fixture(t, "compare.json")}}}}
	var h heard
	c := &Client{Run: f, Meter: h.meter}
	if _, err := c.Compare(context.Background(), "talkable", "talkable", cmpBase, cmpHead); err != nil {
		t.Fatal(err)
	}
	if got := h.all(); len(got) != 1 || got[0] != (Call{Kind: "compare"}) {
		t.Fatalf("heard %+v, want one REST compare", got)
	}
}

// Every gh command is heard, a failed one and a retry after a 401 too: a
// call that failed before GraphQL answered costs no points it can name.
func TestTheMeterHearsFailedCallsAndRetries(t *testing.T) {
	f := reauthFake(t, 1)
	var h heard
	c := &Client{Run: f, Meter: h.meter, Reauth: func(context.Context) error { return nil }}
	if _, err := c.ReviewsWithMarker(WithOp(context.Background(), "fix.external"), "talkable", "talkable", 11973, ""); err != nil {
		t.Fatal(err)
	}
	got := h.all()
	if len(got) != 2 {
		t.Fatalf("heard %d calls, want the 401 and its retry: %+v", len(got), got)
	}
	if got[0].Rate != (RateLimit{}) || got[0].Op != "fix.external" || !got[0].GraphQL {
		t.Errorf("the 401 = %+v, want a GraphQL call of fix.external with no rate limit", got[0])
	}
	if got[1].Rate.Cost != 1 || got[1].Rate.Remaining != 4963 {
		t.Errorf("the retry = %+v, want its cost and remaining", got[1])
	}
}

// A meter that panics fails no call.
func TestAPanickingMeterFailsNoCall(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: fixture(t, "reviews.json")}}}}
	c := &Client{Run: f, Meter: func(Call) { panic("meter bug") }}
	got, err := c.ReviewsWithMarker(context.Background(), "talkable", "talkable", 11973, "")
	if err != nil || len(got) != 10 {
		t.Fatalf("got %d reviews, err %v; the meter's panic must not reach the call", len(got), err)
	}
}

// A call GitHub refused for its rate limit is heard as refused, with no
// points (the meter marks the identity's budget spent, whatever its last
// reading said): a GraphQL RATE_LIMITED answer and a REST 403 rate limit
// alike. A call that failed otherwise is not refused.
func TestTheMeterHearsARefusedCall(t *testing.T) {
	gqlRefused := compact(t, `{"errors":[{"type":"RATE_LIMIT","message":"API rate limit already exceeded for user ID 1."}]}`)
	restRefused := compact(t, `{"message":"API rate limit exceeded for user ID 1.","status":"403"}`)
	restForbidden := compact(t, `{"message":"Resource not accessible by integration","status":"403"}`)
	for _, c := range []struct {
		name string
		rule execx.Rule
		rest bool
		want Call
	}{
		{"graphql refused", execx.Rule{Prefix: []string{"gh", "api", "graphql"},
			Result: execx.Result{Stdout: gqlRefused, Stderr: []byte("gh: API rate limit already exceeded for user ID 1.\n"), Code: 1}},
			false, Call{Op: "fix.external", Kind: "reviews", GraphQL: true, RateLimited: true}},
		{"rest refused", execx.Rule{Prefix: []string{"gh", "api"},
			Result: execx.Result{Stdout: restRefused, Stderr: []byte("gh: API rate limit exceeded for user ID 1. (HTTP 403)\n"), Code: 1}},
			true, Call{Op: "fix.external", Kind: "review", RateLimited: true}},
		{"graphql 502", execx.Rule{Prefix: []string{"gh", "api", "graphql"},
			Result: execx.Result{Stderr: []byte("gh: Something went wrong (HTTP 502)\n"), Code: 1}},
			false, Call{Op: "fix.external", Kind: "reviews", GraphQL: true}},
		{"rest forbidden", execx.Rule{Prefix: []string{"gh", "api"},
			Result: execx.Result{Stdout: restForbidden, Stderr: []byte("gh: Resource not accessible by integration (HTTP 403)\n"), Code: 1}},
			true, Call{Op: "fix.external", Kind: "review"}},
	} {
		var h heard
		cl := &Client{Run: &execx.Fake{Rules: []execx.Rule{c.rule}}, Meter: h.meter}
		ctx := WithOp(context.Background(), "fix.external")
		var err error
		if c.rest {
			_, err = cl.ReviewREST(ctx, "talkable", "talkable", 12054, 2)
		} else {
			_, err = cl.ReviewsWithMarker(ctx, "talkable", "talkable", 12054, "")
		}
		if err == nil {
			t.Fatalf("%s: the call succeeded", c.name)
		}
		if got := h.all(); len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: heard %+v, want [%+v]", c.name, got, c.want)
		}
	}
}

// A label set deeper in the call chain wins.
func TestTheInnermostOpWins(t *testing.T) {
	ctx := WithOp(WithOp(context.Background(), "round"), "round.settle")
	if got := OpOf(ctx); got != "round.settle" {
		t.Errorf("OpOf = %q, want round.settle", got)
	}
	if got := OpOf(context.Background()); got != "" {
		t.Errorf("OpOf of a bare context = %q, want empty", got)
	}
}

// packageFiles parses the package's own (non-test) Go files.
func packageFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return fset, files
}

// mentionsRateLimit reports whether the expression names rateLimitFields.
func mentionsRateLimit(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "rateLimitFields" {
			found = true
		}
		return !found
	})
	return found
}

// Every GraphQL query asks for the rate limit, so the meter knows what each
// call cost: every query the package declares (a package-level const or var
// whose text starts with "query(") names rateLimitFields, and every
// c.graphql call passes one of them, or a query its own function builds with
// rateLimitFields.
func TestEveryGraphQLQueryAsksForTheRateLimit(t *testing.T) {
	fset, files := packageFiles(t)
	queries := map[string]bool{} // name -> asks for the rate limit
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || (g.Tok != token.CONST && g.Tok != token.VAR) {
				continue
			}
			for _, s := range g.Specs {
				vs := s.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					isQuery := false
					ast.Inspect(vs.Values[i], func(n ast.Node) bool {
						if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if text, err := strconv.Unquote(lit.Value); err == nil && strings.HasPrefix(strings.TrimSpace(text), "query(") {
								isQuery = true
							}
						}
						return !isQuery
					})
					if isQuery {
						queries[name.Name] = mentionsRateLimit(vs.Values[i])
					}
				}
			}
		}
	}
	if len(queries) < 10 { // the scan must find the queries, not silently skip them
		t.Fatalf("found only %d query declarations; the scan is broken", len(queries))
	}
	for name, ok := range queries {
		if !ok {
			t.Errorf("query %s does not ask for rateLimitFields", name)
		}
	}
	calls := 0
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "graphql" || len(call.Args) < 3 {
					return true
				}
				calls++
				pos := fset.Position(call.Pos())
				switch q := call.Args[2].(type) {
				case *ast.Ident:
					if _, known := queries[q.Name]; !known {
						t.Errorf("%s: c.graphql passes %s, which is no query the package declares", pos, q.Name)
					}
				default: // a query built in place: its function writes rateLimitFields
					if !mentionsRateLimit(fn.Body) {
						t.Errorf("%s: the query %s builds does not ask for rateLimitFields", pos, fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if calls < 10 {
		t.Fatalf("found only %d c.graphql calls; the scan is broken", calls)
	}
}

// Every call's error label names its kind, so a call no caller labelled
// is still counted by what it is, never as "other": the scan reads the
// labels the package builds (op := fmt.Sprintf("<kind> ...", ...)) and the
// kinds the batched calls pass.
func TestEveryCallLabelNamesItsKind(t *testing.T) {
	_, files := packageFiles(t)
	labels := 0
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			if id, ok := as.Lhs[0].(*ast.Ident); !ok || id.Name != "op" {
				return true
			}
			call, ok := as.Rhs[0].(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			format, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			labels++
			if strings.HasPrefix(format, "%s ") { // batch: the kind is its argument
				return true
			}
			if callKind(strings.ReplaceAll(format, "%", "x")) == "other" {
				t.Errorf("the label %q starts with no call kind (meter.go's callKinds)", format)
			}
			return true
		})
	}
	if labels < 20 {
		t.Fatalf("found only %d call labels; the scan is broken", labels)
	}
	for _, batch := range []string{"details", "confirm"} {
		if callKind(batch+" talkable/talkable (3)") == "other" {
			t.Errorf("the batch kind %q is no call kind", batch)
		}
	}
	for op, want := range map[string]string{
		"compare files talkable/talkable a...b":                "compare files",
		"compare talkable/talkable a...b":                      "compare",
		"review threads talkable/talkable#1":                   "review threads",
		"review threads talkable/talkable#1 (newest comments)": "review threads",
		"review talkable/talkable#1 (APPROVE)":                 "review",
		"reviews talkable/talkable#1":                          "reviews",
		"radar talkable page 2":                                "radar",
		"ci states call 1":                                     "ci states",
		"something new":                                        "other",
	} {
		if got := callKind(op); got != want {
			t.Errorf("callKind(%q) = %q, want %q", op, got, want)
		}
	}
}
