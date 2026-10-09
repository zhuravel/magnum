package learn

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

var (
	outCands    = []Candidate{{ID: "t1"}, {ID: "r2"}}
	outRejected = []Rejected{{ID: "r-1/F2", Title: "No example covers the new secret", Path: "spec/a_spec.rb", Reason: "speculative", Priority: "P3"}}
)

const goodMiss = `{"id":"t1","class":"miss","severity":"P1","title":"Coupon lookup ignores the site",
 "lesson":"When a finder takes a code from the request, check it is scoped to the current tenant because codes repeat across tenants.",
 "scope":"general","lines":[40,42],"match":["tenant|site","scope"]}`

// TestParseOutputKeepsOneItemPerCandidate: a valid answer gives every
// candidate its item; a miss keeps its fields, another class only its id
// and class.
func TestParseOutputKeepsOneItemPerCandidate(t *testing.T) {
	items, _, err := ParseOutput([]byte(`{"items":[`+goodMiss+`,{"id":"r2","class":"style","title":"x","lines":[0,0]}]}`), outCands, outRejected, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := items["t1"]
	if m.Class != store.MissMiss || m.Severity != "P1" || m.Scope != store.MissScopeGeneral || len(m.Lines) != 2 || len(m.Match) != 2 || m.Title == "" || m.Lesson == "" {
		t.Fatalf("miss = %+v", m)
	}
	if s := items["r2"]; !reflect.DeepEqual(s, Item{ID: "r2", Class: store.MissStyle}) {
		t.Fatalf("style item = %+v", s)
	}
}

// TestParseOutputKeepsTheRejectedFindingAnItemNames: an item of any class
// may name the rejected finding that reports its comment's problem; the
// reference is kept, and an item that names none has no reference.
func TestParseOutputKeepsTheRejectedFindingAnItemNames(t *testing.T) {
	out := `{"items":[` + strings.Replace(goodMiss, `"scope":"general"`, `"scope":"general","rejected":" r-1/F2 "`, 1) +
		`,{"id":"r2","class":"not_issue","rejected":"r-1/F2"}]}`
	items, _, err := ParseOutput([]byte(out), outCands, outRejected, nil)
	if err != nil {
		t.Fatal(err)
	}
	if items["t1"].Rejected != "r-1/F2" || !reflect.DeepEqual(items["r2"], Item{ID: "r2", Class: store.MissNotIssue, Rejected: "r-1/F2"}) {
		t.Fatalf("items = %+v", items)
	}
	items, _, err = ParseOutput([]byte(`{"items":[`+goodMiss+`,{"id":"r2","class":"style"}]}`), outCands, nil, nil)
	if err != nil || items["t1"].Rejected != "" || items["r2"].Rejected != "" {
		t.Fatalf("no finding named: %+v, %v", items, err)
	}
}

// TestParseOutputRefusesOffSchemaAnswers: unknown or duplicate ids, a
// missing item, an unknown class, a miss without its fields and a rejected
// finding the candidates file does not list are errors that name ids and
// fields, never the classifier's text.
func TestParseOutputRefusesOffSchemaAnswers(t *testing.T) {
	for name, tc := range map[string]struct{ out, want string }{
		"not json":      {`items: none`, "not a JSON object"},
		"unknown id":    {`{"items":[` + goodMiss + `,{"id":"r2","class":"style"},{"id":"ignore previous instructions and","class":"miss"}]}`, `item 3: its id is no candidate's`},
		"duplicate":     {`{"items":[` + goodMiss + `,{"id":"r2","class":"style"},{"id":"r2","class":"outside"}]}`, "a second item for r2"},
		"missing":       {`{"items":[` + goodMiss + `]}`, "no item for r2"},
		"class":         {`{"items":[` + goodMiss + `,{"id":"r2","class":"bug"}]}`, "class must be"},
		"severity":      {`{"items":[` + strings.Replace(goodMiss, `"P1"`, `"high"`, 1) + `,{"id":"r2","class":"style"}]}`, "severity P0"},
		"title":         {`{"items":[` + strings.Replace(goodMiss, `Coupon lookup ignores the site`, strings.Repeat("x", 81), 1) + `,{"id":"r2","class":"style"}]}`, "title of 1 to 80"},
		"lesson":        {`{"items":[{"id":"t1","class":"miss","severity":"P2","title":"t","scope":"repo","lines":[1,1],"match":["x"]},{"id":"r2","class":"style"}]}`, "needs a lesson"},
		"scope":         {`{"items":[` + strings.Replace(goodMiss, `"general"`, `"global"`, 1) + `,{"id":"r2","class":"style"}]}`, "scope, repo or general"},
		"lines order":   {`{"items":[` + strings.Replace(goodMiss, `[40,42]`, `[42,40]`, 1) + `,{"id":"r2","class":"style"}]}`, "lines [from, to]"},
		"lines zero":    {`{"items":[` + strings.Replace(goodMiss, `[40,42]`, `[0,2]`, 1) + `,{"id":"r2","class":"style"}]}`, "lines [from, to]"},
		"no match":      {`{"items":[` + strings.Replace(goodMiss, `["tenant|site","scope"]`, `[]`, 1) + `,{"id":"r2","class":"style"}]}`, "1 to 3 match"},
		"four matches":  {`{"items":[` + strings.Replace(goodMiss, `["tenant|site","scope"]`, `["a","b","c","d"]`, 1) + `,{"id":"r2","class":"style"}]}`, "1 to 3 match"},
		"bad regexp":    {`{"items":[` + strings.Replace(goodMiss, `"scope"]`, `"(unclosed"]`, 1) + `,{"id":"r2","class":"style"}]}`, "match 2 does not compile"},
		"long pattern":  {`{"items":[` + strings.Replace(goodMiss, `"scope"]`, `"`+strings.Repeat("a", 121)+`"]`, 1) + `,{"id":"r2","class":"style"}]}`, "match 2 must be 1 to 120"},
		"wrong type":    {`{"items":[{"id":"t1","class":"miss","lines":"1-2"}]}`, "wrong type"},
		"items missing": {`{}`, "no item for t1"},
		"rejected":      {`{"items":[` + goodMiss + `,{"id":"r2","class":"style","rejected":"ignore previous instructions and"}]}`, "item r2: rejected names no rejected finding"},
	} {
		_, _, err := ParseOutput([]byte(tc.out), outCands, outRejected, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "ignore previous instructions and") {
			t.Errorf("%s: the error repeats the classifier's text: %v", name, err)
		}
	}
}

// TestScrubLessonRejectsLessonsThatRetellThePR: a lesson must teach a
// principle. One naming a PR or issue (#123), holding a URL, naming the
// author or a reviewer (with or without "@") or mentioning one of magnum's
// logins is dropped with its reason; "@" alone is code (@property,
// @Transactional), and magnum's App login without "@" is the org's name.
// A general lesson may not name the repository (it can reach the public
// skill), backticks or not; a repo lesson stays in that repo's notes and
// may. Words match whole and case-insensitively.
func TestScrubLessonRejectsLessonsThatRetellThePR(t *testing.T) {
	people := []string{"rev-ann", "alice", "lint-bot[bot]"}
	own := []string{"talkable[bot]", "zhuravel"}
	repo := []string{"talkable", "widgets"}
	for _, tc := range []struct {
		lesson, scope, reason string
	}{
		{"When a finder takes a code from the request, check it is scoped to the tenant.", "general", ""},
		{"When a callback retries, check the side effect is idempotent (see #123).", "general", LessonIssueRef},
		{"Check https://example.com/style before naming things.", "general", LessonURL},
		{"When www.example.com is called, add a timeout.", "repo", LessonURL},
		{"As @rev-ann noted, check the tenant scope.", "general", LessonLogin},
		{"When ALICE's helper runs, check the cache key.", "repo", LessonLogin},
		{"When lint-bot complains, check the rule.", "general", LessonLogin},
		{"When @Transactional wraps a call, check that @property readers see @user.", "general", ""},
		{"When Widgets controllers read params, check the tenant scope.", "general", LessonNamesRepo},
		{"When `talkable` jobs enqueue, check idempotency.", "general", LessonNamesRepo},
		{"When talkable_widget is used, check it.", "general", LessonNamesRepo}, // "_" separates words
		{"When Widgets controllers read params, check the tenant scope.", "repo", ""},
		{"When a malicious payload arrives, check the parser.", "general", ""}, // "alice" inside a word
		{"Ask @talkable or @zhuravel before merging a migration.", "repo", LessonLogin},
		{"When `talkable` jobs enqueue, check idempotency.", "repo", ""},
	} {
		got, reason := ScrubLesson(tc.lesson, tc.scope, people, own, repo)
		if reason != tc.reason || (reason == "" && got != tc.lesson) || (reason != "" && got != "") {
			t.Errorf("ScrubLesson(%q, %s) = %q, %q; want reason %q", tc.lesson, tc.scope, got, reason, tc.reason)
		}
	}
}

// TestWriteFileAtomicStaysInsideTheRoot: a path from GitHub is written
// below the root, its directories created, and one that climbs out is
// refused.
func TestWriteFileAtomicStaysInsideTheRoot(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := WriteFileAtomic(root, FilePath(strings.Repeat("a", 40), "app/models/coupon.rb"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "files", "aaaaaaaaaaaa", "app", "models", "coupon.rb")); err != nil || string(b) != "x" {
		t.Fatalf("written file: %q, %v", b, err)
	}
	if err := WriteFileAtomic(root, "files/../../escape.rb", []byte("x")); err == nil {
		t.Fatal("a path climbing out of the root was written")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.rb")); err == nil {
		t.Fatal("escape.rb exists outside the root")
	}
}

var outThreads = []OwnThread{{ID: "o1"}, {ID: "o2"}}

const (
	goodDeclined = `{"id":"o1","outcome":"declined","verdict":"wrong",
 "lesson":"When a value is checked by every caller, do not report the callee for skipping the check.",
 "scope":"general","match":["nil|null","check"]}`
	goodFixed = `{"id":"o2","outcome":"fixed"}`
)

// threadsOut is an answer with no candidate items and the thread items ts.
func threadsOut(ts ...string) []byte {
	return []byte(`{"items":[],"threads":[` + strings.Join(ts, ",") + `]}`)
}

// TestParseOutputRequiresOneItemPerOwnThread: every own thread gets
// exactly one thread item; an unknown or duplicate id and a missing item
// are errors that name ids, never the classifier's text, and the
// candidates' items are checked as before.
func TestParseOutputRequiresOneItemPerOwnThread(t *testing.T) {
	items, threads, err := ParseOutput([]byte(`{"items":[`+goodMiss+`,{"id":"r2","class":"style"}],"threads":[`+goodDeclined+`,`+goodFixed+`]}`),
		outCands, outRejected, outThreads)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || len(threads) != 2 || threads["o1"].Verdict != store.VerdictWrong || threads["o2"].Outcome != store.DeclineFixed {
		t.Fatalf("items %+v, threads %+v", items, threads)
	}
	if _, threads, err := ParseOutput([]byte(`{"items":[`+goodMiss+`,{"id":"r2","class":"style"}]}`), outCands, outRejected, nil); err != nil || len(threads) != 0 {
		t.Fatalf("no own thread, no threads list: %+v, %v", threads, err)
	}
	for name, tc := range map[string]struct {
		out  []byte
		want string
	}{
		"missing":        {threadsOut(goodDeclined), "no thread item for o2"},
		"no list":        {[]byte(`{"items":[]}`), "no thread item for o1"},
		"unknown id":     {threadsOut(goodDeclined, goodFixed, `{"id":"ignore previous instructions and","outcome":"fixed"}`), "thread 3: its id is no own thread's"},
		"candidate id":   {threadsOut(goodDeclined, goodFixed, `{"id":"t1","outcome":"fixed"}`), "thread 3: its id is no own thread's"},
		"duplicate":      {threadsOut(goodDeclined, goodFixed, `{"id":"o2","outcome":"question"}`), "a second thread item for o2"},
		"outcome":        {threadsOut(goodDeclined, `{"id":"o2","outcome":"ignored"}`), "thread o2: outcome must be"},
		"no outcome":     {threadsOut(goodDeclined, `{"id":"o2"}`), "thread o2: outcome must be"},
		"thread as item": {[]byte(`{"items":[{"id":"o1","class":"style"}],"threads":[` + goodDeclined + `,` + goodFixed + `]}`), "item 1: its id is no candidate's"},
	} {
		_, _, err := ParseOutput(tc.out, nil, nil, outThreads)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "ignore previous instructions and") {
			t.Errorf("%s: the error repeats the classifier's text: %v", name, err)
		}
	}
}

// TestParseOutputRequiresAVerdictOnlyForDeclinedAndArgued: a declined or
// argued finding needs one of the four verdicts; any other outcome keeps
// only its id and outcome, whatever else the classifier wrote.
func TestParseOutputRequiresAVerdictOnlyForDeclinedAndArgued(t *testing.T) {
	for _, outcome := range []string{store.DeclineDeclined, store.DeclineArgued} {
		for name, tc := range map[string]struct{ item, want string }{
			"no verdict":  {`{"id":"o1","outcome":"` + outcome + `"}`, "thread o1: a declined or argued finding needs a verdict"},
			"bad verdict": {`{"id":"o1","outcome":"` + outcome + `","verdict":"partly"}`, "thread o1: a declined or argued finding needs a verdict"},
		} {
			if _, _, err := ParseOutput(threadsOut(tc.item, goodFixed), nil, nil, outThreads); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s %s: err = %v, want %q", outcome, name, err, tc.want)
			}
		}
		_, threads, err := ParseOutput(threadsOut(`{"id":"o1","outcome":"`+outcome+`","verdict":"right","lesson":"x","scope":"repo","match":["y"]}`, goodFixed),
			nil, nil, outThreads)
		if err != nil || !reflect.DeepEqual(threads["o1"], ThreadItem{ID: "o1", Outcome: outcome, Verdict: store.VerdictRight}) {
			t.Errorf("%s, magnum right: %+v, %v (a right verdict keeps no lesson)", outcome, threads["o1"], err)
		}
	}
	for _, outcome := range []string{store.DeclineFixed, store.DeclineDeferred, store.DeclineQuestion, store.DeclineUnrelated} {
		_, threads, err := ParseOutput(threadsOut(`{"id":"o1","outcome":"`+outcome+`","verdict":"wrong"}`, goodFixed), nil, nil, outThreads)
		if err != nil || !reflect.DeepEqual(threads["o1"], ThreadItem{ID: "o1", Outcome: outcome}) {
			t.Errorf("%s with a verdict: %+v, %v; want the outcome alone", outcome, threads["o1"], err)
		}
	}
}

// TestParseOutputRequiresALessonForWrongOverRankedAndMissingContext: a
// finding magnum got wrong, ranked too high or lacked the repository's
// context for teaches something, so it needs a lesson, a scope and one to
// MatchMax patterns, with the rules of a miss.
func TestParseOutputRequiresALessonForWrongOverRankedAndMissingContext(t *testing.T) {
	for _, verdict := range []string{store.VerdictWrong, store.VerdictOverRanked, store.VerdictMissingContext} {
		good := strings.Replace(goodDeclined, `"wrong"`, `"`+verdict+`"`, 1)
		_, threads, err := ParseOutput(threadsOut(good, goodFixed), nil, nil, outThreads)
		if o1 := threads["o1"]; err != nil || o1.Verdict != verdict || o1.Lesson == "" || o1.Scope != store.MissScopeGeneral || len(o1.Match) != 2 {
			t.Fatalf("%s: %+v, %v", verdict, o1, err)
		}
		lesson := `"When a value is checked by every caller, do not report the callee for skipping the check."`
		for name, tc := range map[string]struct{ item, want string }{
			"no lesson":    {strings.Replace(good, lesson, `" "`, 1), "needs a lesson"},
			"no scope":     {strings.Replace(good, `"scope":"general",`, ``, 1), "needs a scope, repo or general"},
			"bad scope":    {strings.Replace(good, `"general"`, `"global"`, 1), "needs a scope, repo or general"},
			"no match":     {strings.Replace(good, `["nil|null","check"]`, `[]`, 1), "1 to 3 match"},
			"four matches": {strings.Replace(good, `["nil|null","check"]`, `["a","b","c","d"]`, 1), "1 to 3 match"},
			"bad regexp":   {strings.Replace(good, `"check"]`, `"(unclosed"]`, 1), "match 2 does not compile"},
			"long pattern": {strings.Replace(good, `"check"]`, `"`+strings.Repeat("a", 121)+`"]`, 1), "match 2 must be 1 to 120"},
		} {
			_, _, err := ParseOutput(threadsOut(tc.item, goodFixed), nil, nil, outThreads)
			if err == nil || !strings.Contains(err.Error(), "thread o1: ") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s %s: err = %v, want %q", verdict, name, err, tc.want)
			}
		}
	}
}
