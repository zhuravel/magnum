package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

func readPrepass(t *testing.T, path string) Prepass {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p Prepass
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, b)
	}
	return p
}

// runPrepass runs an initial round on e that posts, and returns its
// prepass.json, which both prompts name.
func (e *env) runPrepass(id int64) (Prepass, string) {
	e.t.Helper()
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(id, "COMMENTED", "COMMENT").behavior(e.t)}
	in := e.input(KindInitial)
	in.Related = defaultRelated
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		e.t.Fatalf("RunRound = %+v, %v", res, err)
	}
	path := filepath.Join(res.ReportDir, PrepassFile)
	mustContain(e.t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "\nprepass: "+path+"\n", "Read `prepass` first")
	mustContain(e.t, "claude-review prompt", e.ag.submitsFor(agents.RoleClaude)[0].Text, "Read "+path+" first")
	return readPrepass(e.t, path), path
}

// prepassEvent returns the round's one round.prepass event's level, message
// and data.
func (e *env) prepassEvent() (level, msg string, data map[string]any) {
	e.t.Helper()
	evs := e.eventsOf("round.prepass")
	if len(evs) != 1 {
		e.t.Fatalf("round.prepass events = %+v, want one", evs)
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		e.t.Fatalf("round.prepass data %s: %v", evs[0].Data, err)
	}
	return evs[0].Level, evs[0].Message, data
}

// A colleague's review tool reads the diff before any model does, and its
// list of the references a PR broke led its reviewers to defects magnum's
// missed. Before the reviewers, a round writes prepass.json: each
// definition the diff removes or renames and does not add back under the
// same name, with the files of the head (the diff's own aside) that still
// name it as a whole word. A renamed Ruby method called from two other
// files lists both; a Go function no other file names, a method added back
// with another signature, a name under 4 characters and one of the stop
// list (call) are not listed. Both prompts name the file; the event counts
// and names no symbol or path.
func TestPrepassListsRemovedSymbolsStillUsed(t *testing.T) {
	e := newEnv(t)
	e.git.diff = []gitx.FileLines{
		{Path: "Gemfile.lock", Status: 'M', Removed: []string{"    rails (7.1)"}, Added: []string{"    rails (7.2)"}},
		{Path: "app/models/order.rb", Status: 'M', Removed: []string{"  def total_price", "  def call", "  def sum"},
			Added: []string{"  def total_amount"}},
		{Path: "internal/billing/charge.go", Status: 'M', Removed: []string{"func roundCents(v float64) int64 {"}},
		{Path: "lib/report.rb", Status: 'M', Removed: []string{"  def self.summarize(rows)"}, Added: []string{"  def self.summarize(rows, totals)"}},
		{Path: "spec/models/order_spec.rb", Status: 'M', Removed: []string{"    expect(order.total_price).to eq(3)"},
			Added: []string{"    expect(order.total_amount).to eq(3)"}},
	}
	e.git.tree = map[string]string{
		"app/models/order.rb":             "class Order\n  def total_amount\n  end\nend\n",
		"app/services/checkout.rb":        "class Checkout\n  def perform\n    order.total_price\n  end\nend\n",
		"app/views/orders/show.html.erb":  "<%= @order.total_price %>\n",
		"app/models/total_price_cache.rb": "class TotalPriceCache # total_price_cache\nend\n",
		"spec/models/order_spec.rb":       "expect(order.total_price).to eq(3)\n",
		"internal/billing/charge.go":      "package billing\n",
		"internal/billing/invoice.go":     "package billing\n\n// roundCentsUp rounds up.\n",
		"lib/report.rb":                   "module Report\n  def self.summarize(rows, totals)\n  end\nend\n",
		"app/jobs/report_job.rb":          "Report.summarize(rows)\n",
		"app/jobs/call_job.rb":            "Service.call\nitems.sum\n",
	}

	got, path := e.runPrepass(901)
	want := Prepass{PR: "talkable/talkable#11920", HeadSHA: target, BaseSHA: e.input(KindInitial).BaseSHA,
		RemovedSymbols: []RemovedSymbol{{Name: "total_price", File: "app/models/order.rb",
			Uses: []string{"app/services/checkout.rb:3", "app/views/orders/show.html.erb:1"}}},
		TestGaps: []string{"internal/billing/charge.go", "lib/report.rb"},
		Siblings: []Sibling{},
		Methods:  []Method{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prepass.json = %+v\nwant %+v", got, want)
	}
	base := e.input(KindInitial).BaseSHA
	if calls, want := e.git.prepassCallsMade(), []string{
		"stat " + base + "..." + target,
		"lines " + base + "..." + target + " Gemfile.lock",
		"uses " + target + " roundCents",
		"uses " + target + " total_price",
	}; !slices.Equal(calls, want) {
		t.Errorf("pre-pass git calls = %q\nwant %q (no search for a name added back, a short name or one of the stop list; the lockfile skipped)", calls, want)
	}

	level, msg, data := e.prepassEvent()
	if level != "info" || data["removed_symbols"] != float64(1) || data["test_gaps"] != float64(2) || data["siblings"] != float64(0) ||
		data["lines"] != float64(9) || data["file"] != path {
		t.Errorf("round.prepass = %s %q %v, want info with 1 removed symbol, 2 test gaps, 0 siblings and the 9 lines read", level, msg, data)
	}
	for _, ev := range e.events() {
		if s := ev.Message + string(ev.Data); strings.Contains(s, "total_price") || strings.Contains(s, "checkout") {
			t.Errorf("event %s names a symbol or a path: %s %s", ev.Kind, ev.Message, ev.Data)
		}
	}
}

// A changed source file whose test file, by the language's naming
// convention, the PR leaves alone is a test gap; with that test changed it
// is none. A view, a migration, a spec, a style, a document, a
// configuration file, a generated file and a deleted file are no source
// files.
func TestPrepassListsChangedSourceFilesWithoutATestChange(t *testing.T) {
	order := gitx.FileLines{Path: "app/models/order.rb", Status: 'M', Added: []string{"  validates :total, presence: true"}}
	notSource := []gitx.FileLines{
		{Path: "app/views/orders/show.html.erb", Status: 'M'},
		{Path: "app/views/orders/row.rb", Status: 'M'},
		{Path: "db/migrate/20261015120000_add_total_to_orders.rb", Status: 'A'},
		{Path: "spec/models/user_spec.rb", Status: 'M'},
		{Path: "app/assets/stylesheets/orders.scss", Status: 'M'},
		{Path: "docs/orders.md", Status: 'M'},
		{Path: "config/initializers/orders.rb", Status: 'M'},
		{Path: "db/schema.rb", Status: 'M'},
		{Path: "internal/api/api.pb.go", Status: 'M'},
		{Path: "web/jest.config.js", Status: 'M'},
		{Path: "app/models/legacy.rb", Status: 'D'},
	}
	e := newEnv(t)
	e.git.diff = append([]gitx.FileLines{order}, notSource...)
	if got, _ := e.runPrepass(902); !slices.Equal(got.TestGaps, []string{"app/models/order.rb"}) {
		t.Fatalf("test_gaps = %q, want app/models/order.rb alone", got.TestGaps)
	}

	e = newEnv(t)
	e.git.diff = append([]gitx.FileLines{order, {Path: "spec/models/order_spec.rb", Status: 'M'}}, notSource...)
	if got, _ := e.runPrepass(903); got.TestGaps == nil || len(got.TestGaps) != 0 {
		t.Fatalf("test_gaps = %#v, want an empty list", got.TestGaps)
	}

	// Each language's convention.
	for _, tc := range []struct {
		src  string
		test []string // a changed test file that matches; nil = none does
	}{
		{"app/services/mailer/client.rb", []string{"spec/services/mailer/client_spec.rb", "test/services/mailer/client_test.rb"}},
		{"lib/report/rows.rb", []string{"spec/report/rows_spec.rb", "spec/lib/report/rows_spec.rb", "test/lib/report/rows_test.rb"}},
		{"engines/billing/app/models/invoice.rb", []string{"engines/billing/spec/models/invoice_spec.rb"}},
		{"internal/billing/charge.go", []string{"internal/billing/charge_test.go"}},
		{"web/src/cart.ts", []string{"web/src/cart.test.ts", "web/src/cart.spec.tsx", "web/src/__tests__/cart.ts", "web/src/__tests__/cart.test.ts"}},
		{"web/src/Menu.jsx", []string{"web/src/Menu.test.jsx"}},
		{"tools/clean.py", []string{"tests/test_clean.py", "tools/clean_test.py"}},
	} {
		for _, test := range tc.test {
			if gaps := testGaps([]gitx.FileLines{{Path: tc.src, Status: 'M'}, {Path: test, Status: 'M'}}); len(gaps) != 0 {
				t.Errorf("%s with %s changed: gaps %q, want none", tc.src, test, gaps)
			}
		}
		if gaps := testGaps([]gitx.FileLines{{Path: tc.src, Status: 'A'}, {Path: "spec/other_spec.rb", Status: 'M'}}); !slices.Equal(gaps, []string{tc.src}) {
			t.Errorf("%s without its test: gaps %q, want it", tc.src, gaps)
		}
	}
	for _, other := range []struct{ src, test string }{
		{"internal/billing/charge.go", "internal/other/charge_test.go"},
		{"app/models/order.rb", "spec/services/order_spec.rb"},
		{"web/src/cart.ts", "web/lib/cart.test.ts"},
	} {
		if gaps := testGaps([]gitx.FileLines{{Path: other.src, Status: 'M'}, {Path: other.test, Status: 'M'}}); !slices.Equal(gaps, []string{other.src}) {
			t.Errorf("%s with only %s changed: gaps %q, want it", other.src, other.test, gaps)
		}
	}
}

// The reviewers read the closest existing file to each new one before the
// new file: the file of the same suffix in its directory, else in the
// parent, with the longest common prefix; another file the PR adds is no
// sibling, and a new file without one gets no entry. One ls-tree of the
// head lists the directories.
func TestPrepassNamesTheSiblingOfANewFile(t *testing.T) {
	e := newEnv(t)
	e.git.diff = []gitx.FileLines{
		{Path: "app/services/mailer/grouping.rb", Status: 'A', Added: []string{"class Grouping", "end"}},
		{Path: "app/services/mailer/client.rb", Status: 'M'},
		{Path: "app/services/payouts/payout.rb", Status: 'A'},
		{Path: "app/services/payouts/payout_batch.rb", Status: 'A'},
		{Path: "app/assets/images/logo.svg", Status: 'A'},
	}
	e.git.tree = map[string]string{
		"app/services/mailer/grouping.rb":      "class Grouping\nend\n",
		"app/services/mailer/client.rb":        "",
		"app/services/mailer/group_digest.rb":  "",
		"app/services/mailer/grouping.yml":     "",
		"app/services/payouts/payout.rb":       "",
		"app/services/payouts/payout_batch.rb": "",
		"app/services/payment.rb":              "",
		"app/services/notifier.rb":             "",
		"app/assets/images/logo.svg":           "",
		"app/assets/images/icon.png":           "",
		"app/assets/config.js":                 "",
	}
	got, _ := e.runPrepass(904)
	want := []Sibling{
		{File: "app/services/mailer/grouping.rb", Sibling: "app/services/mailer/group_digest.rb"},
		{File: "app/services/payouts/payout.rb", Sibling: "app/services/payment.rb"},
		{File: "app/services/payouts/payout_batch.rb", Sibling: "app/services/payment.rb"},
	}
	if !reflect.DeepEqual(got.Siblings, want) {
		t.Fatalf("siblings = %+v\nwant %+v", got.Siblings, want)
	}
	var dirs []string
	for _, c := range e.git.prepassCallsMade() {
		if strings.HasPrefix(c, "dirs ") {
			dirs = append(dirs, c)
		}
	}
	if want := []string{"dirs " + target + " app/services/mailer,app/services,app/services/payouts,app/assets/images,app/assets"}; !slices.Equal(dirs, want) {
		t.Errorf("DirFiles calls = %q, want %q", dirs, want)
	}
}

// textOf is a file of lines; bodyOf its lines first to last (1-based).
func textOf(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

func bodyOf(text string, first, last int) string {
	return strings.Join(strings.Split(text, "\n")[first-1:last], "\n")
}

// A reviewer reads the whole method a hunk changes, not the hunk alone: on
// talkable#11483 a write in the same method as a fix, outside its hunk, went
// unseen. prepass.json lists the enclosing method of each hunk of a changed
// source file, read from the head's tree, with its lines and its body.
// Ruby's def runs to its end at the same indentation; Go's and
// TypeScript's to the brace that closes it, braces in strings, runes, raw
// strings and comments aside; Python's to the first line at the def's
// indentation or less, a docstring's and a comment's lines aside. A method
// two hunks change is listed once, one a hunk only removes lines from too;
// a hunk in a file's header lists nothing, nor does a method the diff adds
// whole, and an added file and a test are never read.
func TestPrepassListsTheEnclosingMethodOfEachHunk(t *testing.T) {
	ruby := textOf(
		"# frozen_string_literal: true",
		"",
		"class Order",
		"  # Applies a discount.",
		"  def apply(discount)",
		"    self.total -= discount",
		"    self.tier = nil",
		"    save!",
		"  end",
		"",
		"  def total_cents",
		"    total * 100",
		"  end",
		"",
		"  def refund",
		"    update!(total: 0)",
		"  end",
		"end",
	)
	goSrc := textOf(
		"package billing",
		"",
		`import "fmt"`,
		"",
		"// Describe names the charge.",
		"func (c *Charge) Describe() string {",
		"\tparts := map[string]string{\"open\": \"{\"}",
		"\tif c.Paid {",
		"\t\tparts[\"state\"] = \"paid }\"",
		"\t}",
		"\t// a } in a comment",
		"\treturn fmt.Sprintf(\"%v %c\", parts, '}') /* } */",
		"}",
		"",
		"func Raw() string {",
		"\treturn `{",
		"` + \"x\"",
		"}",
	)
	py := textOf(
		"import os",
		"",
		"",
		"def clean(paths):",
		`    """Removes the files.`,
		"",
		"A note at column zero.",
		`    """`,
		"    for p in paths:",
		"# a comment at column zero",
		"        if os.path.exists(p):",
		"            os.remove(p)",
		"",
		"    return len(paths)",
		"",
		"",
		"def other():",
		"    pass",
	)
	ts := textOf(
		"export class Cart {",
		"  private items: Item[] = [];",
		"",
		"  total(): number {",
		"    return this.items.reduce((sum, i) => {",
		"      return sum + i.price;",
		"    }, 0);",
		"  }",
		"}",
		"",
		"export const clear = (cart: Cart) => {",
		"  cart.items = [];",
		"};",
	)
	e := newEnv(t)
	e.git.diff = []gitx.FileLines{
		{Path: "app/models/order.rb", Status: 'M',
			Hunks: []gitx.Hunk{{Start: 1, Count: 1}, {Start: 6, Count: 1}, {Start: 8, Count: 1}, {Start: 12}, {Start: 14, Count: 4}}},
		{Path: "app/models/coupon.rb", Status: 'A', Hunks: []gitx.Hunk{{Start: 1, Count: 4}}},
		{Path: "spec/models/order_spec.rb", Status: 'M', Hunks: []gitx.Hunk{{Start: 2, Count: 1}}},
		{Path: "internal/billing/charge.go", Status: 'M', Hunks: []gitx.Hunk{{Start: 3, Count: 1}, {Start: 9, Count: 1}, {Start: 17, Count: 1}}},
		{Path: "tools/clean.py", Status: 'M', Hunks: []gitx.Hunk{{Start: 1, Count: 1}, {Start: 12, Count: 1}}},
		{Path: "web/src/cart.ts", Status: 'M', Hunks: []gitx.Hunk{{Start: 6, Count: 1}, {Start: 12, Count: 1}}},
	}
	e.git.tree = map[string]string{
		"app/models/order.rb":        ruby,
		"app/models/coupon.rb":       "class Coupon\n  def code\n  end\nend\n",
		"spec/models/order_spec.rb":  "describe Order do\n  def helper\n  end\nend\n",
		"internal/billing/charge.go": goSrc,
		"tools/clean.py":             py,
		"web/src/cart.ts":            ts,
	}
	got, _ := e.runPrepass(910)
	want := []Method{
		{File: "app/models/order.rb", Name: "apply", FirstLine: 5, LastLine: 9, Body: bodyOf(ruby, 5, 9)},
		{File: "app/models/order.rb", Name: "total_cents", FirstLine: 11, LastLine: 13, Body: bodyOf(ruby, 11, 13)},
		{File: "internal/billing/charge.go", Name: "Describe", FirstLine: 6, LastLine: 13, Body: bodyOf(goSrc, 6, 13)},
		{File: "internal/billing/charge.go", Name: "Raw", FirstLine: 15, LastLine: 18, Body: bodyOf(goSrc, 15, 18)},
		{File: "tools/clean.py", Name: "clean", FirstLine: 4, LastLine: 14, Body: bodyOf(py, 4, 14)},
		{File: "web/src/cart.ts", Name: "total", FirstLine: 4, LastLine: 8, Body: bodyOf(ts, 4, 8)},
		{File: "web/src/cart.ts", Name: "clear", FirstLine: 11, LastLine: 13, Body: bodyOf(ts, 11, 13)},
	}
	if !reflect.DeepEqual(got.Methods, want) || got.UnlistedMethods != 0 || got.DroppedBodies != 0 {
		t.Fatalf("methods = %+v (unlisted %d, dropped %d)\nwant %+v", got.Methods, got.UnlistedMethods, got.DroppedBodies, want)
	}
	var reads []string
	for _, c := range e.git.prepassCallsMade() {
		if strings.HasPrefix(c, "file ") {
			reads = append(reads, c)
		}
	}
	if want := []string{"file " + target + " app/models/order.rb", "file " + target + " internal/billing/charge.go",
		"file " + target + " tools/clean.py", "file " + target + " web/src/cart.ts"}; !slices.Equal(reads, want) {
		t.Errorf("FileAt calls = %q\nwant %q (an added file and a test never read)", reads, want)
	}
	if level, msg, data := e.prepassEvent(); level != "info" || data["methods"] != float64(7) || !strings.Contains(msg, "7 changed method(s)") ||
		strings.Contains(msg, "apply") {
		t.Errorf("round.prepass = %s %q %v, want info counting 7 methods, naming none", level, msg, data)
	}
}

// Each language's method runs from its first line to its end in the shapes
// code has it: Ruby's private, endless, class and one-line defs; Python's
// signature over several lines and a one-line def; TypeScript's arrow
// function with its parameters on their own lines, an overload without a
// body (none), a component whose parameters destructure and whose JSX holds
// an apostrophe; Go's method with a brace in its result type and a
// one-line func.
func TestEachLanguagesMethodRunsToItsEnd(t *testing.T) {
	for _, tc := range []struct {
		path  string
		lines []string
		want  []methodSpan
	}{
		{"app/models/a.rb", []string{
			"class A",
			"  private def secret",
			"    1",
			"  end",
			"",
			`  def name = "a"`,
			"",
			"  def self.build(x)",
			"    new(x)",
			"  end",
			"",
			"  def run; go; end",
			"end",
		}, []methodSpan{{"secret", 2, 4}, {"name", 6, 6}, {"build", 8, 10}, {"run", 12, 12}}},
		{"tools/c.py", []string{
			"class C:",
			"    def method(",
			"        self,",
			"        a,",
			"    ):",
			"        return a",
			"",
			"    async def other(self): pass",
		}, []methodSpan{{"method", 2, 6}, {"other", 8, 8}}},
		{"web/src/order.tsx", []string{
			"export const createOrder = async (",
			"  input: Input,",
			"): Promise<Order> => {",
			"  return save({ ...input });",
			"};",
			"",
			"export function overload(a: string): void;",
			"export function overload(a: any) {",
			"  return a;",
			"}",
			"",
			"const View = ({ name }: Props) => {",
			"  if (name) {",
			"    return <p>Don't {name}</p>;",
			"  }",
			"  return null;",
			"};",
		}, []methodSpan{{"createOrder", 1, 5}, {"overload", 8, 10}, {"View", 12, 17}}},
		{"internal/set/set.go", []string{
			"func (s *Set[T]) Items() interface{} {",
			"\treturn s.items",
			"}",
			"",
			"func One() int { return 1 }",
		}, []methodSpan{{"Items", 1, 3}, {"One", 5, 5}}},
	} {
		if got := sourceLang(tc.path).methods(tc.lines); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: methods = %+v\nwant %+v", tc.path, got, tc.want)
		}
	}
}

// The bodies stay bounded: a method over 300 lines is listed with its lines
// and no body; at most 120 methods are listed, the rest counted in
// unlisted_methods; and past 400 KB of bodies the largest are dropped first
// and counted in dropped_bodies.
func TestPrepassCapsTheMethodBodies(t *testing.T) {
	funcOf := func(name string, n int, line string) []string { // a Go func of n lines
		out := []string{"func " + name + "() {"}
		for range n - 2 {
			out = append(out, "\t"+line)
		}
		return append(out, "}")
	}
	long := textOf(slices.Concat(funcOf("Long", 301, "x()"), []string{""}, funcOf("Fits", 300, "x()"))...)
	e := newEnv(t)
	e.git.diff = []gitx.FileLines{{Path: "internal/big/big.go", Status: 'M', Hunks: []gitx.Hunk{{Start: 2, Count: 1}, {Start: 304, Count: 1}}}}
	e.git.tree = map[string]string{"internal/big/big.go": long}
	got, _ := e.runPrepass(911)
	want := []Method{
		{File: "internal/big/big.go", Name: "Long", FirstLine: 1, LastLine: 301},
		{File: "internal/big/big.go", Name: "Fits", FirstLine: 303, LastLine: 602, Body: bodyOf(long, 303, 602)},
	}
	if !reflect.DeepEqual(got.Methods, want) || got.UnlistedMethods != 0 || got.DroppedBodies != 0 {
		t.Fatalf("methods = %+v (unlisted %d, dropped %d)\nwant %+v", got.Methods, got.UnlistedMethods, got.DroppedBodies, want)
	}

	var many []string
	var hunks []gitx.Hunk
	for i := range 121 {
		hunks = append(hunks, gitx.Hunk{Start: len(many) + 2, Count: 1})
		many = append(many, funcOf(fmt.Sprintf("F%d", i), 3, "x()")...)
	}
	e = newEnv(t)
	e.git.diff = []gitx.FileLines{{Path: "internal/many/many.go", Status: 'M', Hunks: hunks}}
	e.git.tree = map[string]string{"internal/many/many.go": textOf(many...)}
	got, _ = e.runPrepass(912)
	if len(got.Methods) != 120 || got.Methods[119].Name != "F119" || got.Methods[119].Body == "" || got.UnlistedMethods != 1 {
		t.Fatalf("121 methods: %d listed (unlisted %d), want the first 120 and one unlisted", len(got.Methods), got.UnlistedMethods)
	}
	if _, _, data := e.prepassEvent(); data["methods"] != float64(120) || data["unlisted_methods"] != float64(1) {
		t.Errorf("round.prepass data = %v, want 120 methods and 1 unlisted", data)
	}

	wide := strings.Repeat("y", 1000)
	sized := textOf(slices.Concat(funcOf("A", 152, wide), funcOf("B", 202, wide), funcOf("C", 102, wide))...)
	e = newEnv(t)
	e.git.diff = []gitx.FileLines{{Path: "internal/wide/wide.go", Status: 'M',
		Hunks: []gitx.Hunk{{Start: 2, Count: 1}, {Start: 154, Count: 1}, {Start: 356, Count: 1}}}}
	e.git.tree = map[string]string{"internal/wide/wide.go": sized}
	got, _ = e.runPrepass(913)
	want = []Method{
		{File: "internal/wide/wide.go", Name: "A", FirstLine: 1, LastLine: 152, Body: bodyOf(sized, 1, 152)},
		{File: "internal/wide/wide.go", Name: "B", FirstLine: 153, LastLine: 354},
		{File: "internal/wide/wide.go", Name: "C", FirstLine: 355, LastLine: 456, Body: bodyOf(sized, 355, 456)},
	}
	if !reflect.DeepEqual(got.Methods, want) || got.DroppedBodies != 1 {
		t.Fatalf("over 400 KB: methods %d, dropped %d; want B's body dropped", len(got.Methods), got.DroppedBodies)
	}
}

// Past 50,000 changed lines (the watch's related_ignore paths aside) the
// pre-pass does not read the diff: prepass.json says why, the prompts still
// name it, and the event gives the reason. A round without a base sha says
// so too, without a git call.
func TestPrepassSkipsAHugeDiffAndSaysWhy(t *testing.T) {
	e := newEnv(t)
	e.git.stats = []gitx.FileStat{
		{Path: "Gemfile.lock", Added: 30000, Removed: 29000},
		{Path: "app/models/order.rb", Added: 45000, Removed: 5001},
	}
	got, _ := e.runPrepass(905)
	want := Prepass{PR: "talkable/talkable#11920", HeadSHA: target, BaseSHA: e.input(KindInitial).BaseSHA,
		Skipped: "the diff has 50001 changed lines, over 50000"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prepass.json = %+v\nwant %+v", got, want)
	}
	if calls := e.git.prepassCallsMade(); len(calls) != 1 || !strings.HasPrefix(calls[0], "stat ") {
		t.Errorf("pre-pass git calls = %q, want the line count only", calls)
	}
	if level, msg, data := e.prepassEvent(); level != "info" || !strings.Contains(msg, "50001 changed lines") || data["skipped"] != want.Skipped {
		t.Errorf("round.prepass = %s %q %v, want the reason", level, msg, data)
	}

	e = newEnv(t)
	e.git.stats = []gitx.FileStat{
		{Path: "Gemfile.lock", Added: 30000, Removed: 29000},
		{Path: "app/models/order.rb", Added: 45000, Removed: 5000},
	}
	if got, _ := e.runPrepass(906); got.Skipped != "" {
		t.Fatalf("50,000 lines: prepass.json = %+v, want it read", got)
	}

	e = newEnv(t)
	e.git.diff = []gitx.FileLines{{Path: "app/models/order.rb", Status: 'M', Removed: []string{"  def total_price"}}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(907, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.BaseSHA = ""
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if got := readPrepass(t, filepath.Join(res.ReportDir, PrepassFile)); got.Skipped != "no base sha" || got.RemovedSymbols != nil {
		t.Fatalf("without a base sha: prepass.json = %+v", got)
	}
	if calls := e.git.prepassCallsMade(); len(calls) != 0 {
		t.Errorf("without a base sha: pre-pass git calls = %q, want none", calls)
	}
}

// A pre-pass whose git fails, or that runs past PrepassTimeout, never fails
// the round: prepass.json says it was skipped and why, the event warns, and
// the review goes on.
func TestAFailedOrSlowPrepassIsSkipped(t *testing.T) {
	storetest.Serial(t) // swaps PrepassTimeout
	defer func(d time.Duration) { PrepassTimeout = d }(PrepassTimeout)
	PrepassTimeout = 50 * time.Millisecond

	e := newEnv(t)
	e.git.diffErr = errors.New("fatal: bad object " + target)
	got, path := e.runPrepass(908)
	if !strings.HasPrefix(got.Skipped, "the pre-pass failed: ") || !strings.Contains(got.Skipped, "bad object") || got.TestGaps != nil {
		t.Fatalf("prepass.json = %+v, want skipped with the failure", got)
	}
	if level, _, data := e.prepassEvent(); level != "warn" || data["skipped"] != got.Skipped || data["file"] != path {
		t.Errorf("round.prepass = %s %v, want a warning with the reason", level, data)
	}

	e = newEnv(t)
	e.git.diffHangs = true
	got, _ = e.runPrepass(909)
	if got.Skipped != "the pre-pass took more than 50ms" {
		t.Fatalf("prepass.json = %+v, want skipped past the timeout", got)
	}
	if level, _, _ := e.prepassEvent(); level != "warn" {
		t.Errorf("round.prepass level = %s, want warn", level)
	}
}
