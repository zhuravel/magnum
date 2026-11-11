package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// packageTests parses this package's test files once: the top-level
// functions (no receiver) of every _test.go file by name.
var packageTests = sync.OnceValues(func() (map[string]*ast.FuncDecl, error) {
	ents, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, ent := range ents {
		if !strings.HasSuffix(ent.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, ent.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Body != nil {
				funcs[fn.Name.Name] = fn
			}
		}
	}
	return funcs, nil
})

// calledNames is every name a call in n calls as a plain identifier, or
// passes as an argument (t.Run(name, helper)), function literals included.
func calledNames(n ast.Node) []string {
	var names []string
	ast.Inspect(n, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok {
				names = append(names, id.Name)
			}
			for _, a := range c.Args {
				if id, ok := a.(*ast.Ident); ok {
					names = append(names, id.Name)
				}
			}
		}
		return true
	})
	return names
}

// isCall reports whether c calls pkg.name (x.name for a receiver x).
func isCall(c *ast.CallExpr, x, name string) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == x
}

// harnessBuilders is every helper of the package's tests that runs its test
// in parallel: the ones that call storetest.Parallel (newHarness, appEngine)
// and every helper that calls one of them.
func harnessBuilders(funcs map[string]*ast.FuncDecl) map[string]bool {
	builders := map[string]bool{}
	for name, fn := range funcs {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && isCall(c, "storetest", "Parallel") && !strings.HasPrefix(name, "Test") {
				builders[name] = true
			}
			return !builders[name]
		})
	}
	for grew := true; grew; {
		grew = false
		for name, fn := range funcs {
			if builders[name] || strings.HasPrefix(name, "Test") {
				continue
			}
			if slices.ContainsFunc(calledNames(fn.Body), func(c string) bool { return builders[c] }) {
				builders[name], grew = true, true
			}
		}
	}
	return builders
}

// serialTables names the tests that call t.Run with subtests that build a
// harness (and so run in parallel with each other) but never run in parallel
// themselves: Go runs such a test, subtests and all, before it starts any
// parallel test of the package.
func serialTables(funcs map[string]*ast.FuncDecl) []string {
	builders := harnessBuilders(funcs)
	var found []string
	for name, fn := range funcs {
		if !strings.HasPrefix(name, "Test") || len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) != 1 {
			continue
		}
		tName := fn.Type.Params.List[0].Names[0].Name
		// The closures the test declares (setup := func(t *testing.T) ...),
		// which a subtest may call to build its harness.
		local := map[string]*ast.FuncLit{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
				for i, l := range as.Lhs {
					id, ok := l.(*ast.Ident)
					lit, isLit := as.Rhs[i].(*ast.FuncLit)
					if ok && isLit {
						local[id.Name] = lit
					}
				}
			}
			return true
		})
		var builds func(n ast.Node, seen map[string]bool) bool
		builds = func(n ast.Node, seen map[string]bool) bool {
			for _, c := range calledNames(n) {
				if builders[c] {
					return true
				}
				if lit, ok := local[c]; ok && !seen[c] {
					seen[c] = true
					if builds(lit.Body, seen) {
						return true
					}
				}
			}
			return false
		}
		// Walk the test's own statements and the closures that use its t (a
		// closure with a t of its own is a subtest's or a helper's): a call
		// that marks the test (storetest.Parallel or Serial, t.Parallel, a
		// harness of its own) anywhere in them keeps it out, since its
		// subtests start only once its body returns.
		marked, table := false, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				for _, p := range n.Type.Params.List {
					for _, id := range p.Names {
						if id.Name == tName {
							return false
						}
					}
				}
			case *ast.CallExpr:
				switch {
				case isCall(n, tName, "Run") && len(n.Args) == 2:
					if builds(n.Args[1], map[string]bool{}) {
						table = true
					}
					return false
				case isCall(n, "storetest", "Parallel"), isCall(n, "storetest", "Serial"), isCall(n, tName, "Parallel"):
					marked = true
				default:
					if id, ok := n.Fun.(*ast.Ident); ok && (builders[id.Name] || local[id.Name] != nil && builds(local[id.Name].Body, map[string]bool{})) {
						marked = true
					}
				}
			}
			return true
		})
		if table && !marked {
			found = append(found, name)
		}
	}
	slices.Sort(found)
	return found
}

// A table test whose cases each build a harness runs in parallel with the
// package's other tests: Go runs a test that never calls t.Parallel, its
// parallel subtests included, before it starts any parallel test, so 286
// such tables held the rest back for more than a third of the package's
// time under -race. Each calls storetest.Parallel(t) first, or
// storetest.Serial(t) when it swaps a package variable or waits in real time
// on a starved goroutine.
func TestATableWhoseCasesBuildAHarnessRunsBesideTheOtherTests(t *testing.T) {
	t.Parallel()
	funcs, err := packageTests()
	if err != nil {
		t.Fatal(err)
	}
	if b := harnessBuilders(funcs); !b["newHarness"] || !b["appEngine"] || !b["newMigHarness"] {
		t.Fatalf("the %d harness builders found miss newHarness, appEngine or newMigHarness: the parse finds none of them", len(b))
	}
	if found := serialTables(funcs); len(found) > 0 {
		t.Errorf("%d tests run subtests that build a harness, yet stay serial themselves; call storetest.Parallel(t) first (storetest.Serial(t) when the test swaps a package variable):\n%s",
			len(found), strings.Join(found, "\n"))
	}
}

// The race gate splits this package in two (make test-race with -run ^TestA
// and -run ^Test[B-Z]; CI's engine job does the same), so every test,
// example and fuzz test is named Test and a capital letter: anything else
// would run in neither half.
func TestEveryTestFallsInOneHalfOfTheRaceGatesSplit(t *testing.T) {
	t.Parallel()
	funcs, err := packageTests()
	if err != nil {
		t.Fatal(err)
	}
	half := regexp.MustCompile(`^Test[A-Z]`)
	var outside []string
	for name := range funcs {
		if name == "TestMain" {
			continue
		}
		if strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Example") || strings.HasPrefix(name, "Fuzz") {
			if !half.MatchString(name) {
				outside = append(outside, name)
			}
		}
	}
	if len(outside) > 0 {
		slices.Sort(outside)
		t.Errorf("tests outside -run ^TestA and -run ^Test[B-Z]: %v", outside)
	}
}
