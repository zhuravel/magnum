package learn

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

var outCands = []Candidate{{ID: "t1"}, {ID: "r2"}}

const goodMiss = `{"id":"t1","class":"miss","severity":"P1","title":"Coupon lookup ignores the site",
 "lesson":"When a finder takes a code from the request, check it is scoped to the current tenant because codes repeat across tenants.",
 "scope":"general","lines":[40,42],"match":["tenant|site","scope"]}`

// TestParseOutputKeepsOneItemPerCandidate: a valid answer gives every
// candidate its item; a miss keeps its fields, another class only its id
// and class.
func TestParseOutputKeepsOneItemPerCandidate(t *testing.T) {
	items, err := ParseOutput([]byte(`{"items":[`+goodMiss+`,{"id":"r2","class":"style","title":"x","lines":[0,0]}]}`), outCands)
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

// TestParseOutputRefusesOffSchemaAnswers: unknown or duplicate ids, a
// missing item, an unknown class and a miss without its fields are errors
// that name ids and fields, never the classifier's text.
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
	} {
		_, err := ParseOutput([]byte(tc.out), outCands)
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
