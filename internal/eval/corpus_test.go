package eval

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

const validCorpus = `
[[case]]
name = "oauth-hmac"
pr = "talkable/talkable#11932"
head = "` + sha + `"
base = "master"
why = "a human reviewer found two critical issues magnum missed"

[[case.defect]]
id = "hmac-skip"
title = "OAuth callback skips HMAC verification"
severity = "P1"
paths = ["app/controllers/oauth/*.rb", "lib/shopify_auth.rb"]
lines = [40, 75]
match = ["hmac", "signature"]
body = true

[[case.defect]]
id = "discount-reuse"
title = "Discount code can be reused after refund"
match = ["discount.*(reuse|twice)"]

[[case]]
name = "sandbox"
pr = "example/sandbox#1"
head = "` + sha + `"

[[case.defect]]
id = "sql-injection"
title = "Search interpolates the query into SQL"
paths = ["app/**/search*.rb"]
`

func TestParseCorpusReadsAFullCorpus(t *testing.T) {
	c, err := parseCorpus("", []byte(validCorpus))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cases) != 2 {
		t.Fatalf("cases = %d, want 2", len(c.Cases))
	}
	cs := c.Cases[0]
	if cs.Name != "oauth-hmac" || cs.PR != "talkable/talkable#11932" || cs.Head != sha || cs.Base != "master" || cs.Why == "" {
		t.Errorf("case fields not read: %+v", cs)
	}
	if cs.Owner != "talkable" || cs.Repo != "talkable" || cs.Number != 11932 {
		t.Errorf("pr not split: %q %q %d", cs.Owner, cs.Repo, cs.Number)
	}
	if len(cs.Defects) != 2 {
		t.Fatalf("defects = %d, want 2", len(cs.Defects))
	}
	d := cs.Defects[0]
	if d.ID != "hmac-skip" || d.Severity != "P1" || !slices.Equal(d.Paths, []string{"app/controllers/oauth/*.rb", "lib/shopify_auth.rb"}) ||
		!slices.Equal(d.Lines, []int{40, 75}) || !slices.Equal(d.Match, []string{"hmac", "signature"}) || !d.Body {
		t.Errorf("defect fields not read: %+v", d)
	}
	if len(d.res) != 2 {
		t.Errorf("compiled regexps = %d, want 2", len(d.res))
	}
	if c.Cases[1].Owner != "example" || c.Cases[1].Repo != "sandbox" || c.Cases[1].Number != 1 {
		t.Errorf("second pr not split: %+v", c.Cases[1])
	}
}

func TestParseCorpusRejectsUnknownKeysNamingTheirCaseAndDefect(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "top level",
			src:  "colour = \"red\"\n" + validCorpus,
			want: []string{`unknown key "colour"`},
		},
		{
			name: "case level",
			src:  strings.Replace(validCorpus, `why = "a human`, "owner = \"x\"\nwhy = \"a human", 1),
			want: []string{`case "oauth-hmac"`, `unknown key "owner"`},
		},
		{
			name: "defect level typo",
			src:  strings.Replace(validCorpus, `severity = "P1"`, `sevrity = "P1"`, 1),
			want: []string{`case "oauth-hmac"`, `defect "hmac-skip"`, `unknown key "sevrity"`},
		},
		{
			name: "only the case that has it is named",
			src:  strings.Replace(validCorpus, `paths = ["app/**/search*.rb"]`, "paths = [\"app/**/search*.rb\"]\nfile = \"x\"", 1),
			want: []string{`case "sandbox"`, `defect "sql-injection"`, `unknown key "file"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCorpus("", []byte(tt.src))
			if err == nil {
				t.Fatal("unknown key was accepted")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
			if tt.name == "only the case that has it is named" && strings.Contains(err.Error(), "oauth-hmac") {
				t.Errorf("error names a case that does not have the key: %v", err)
			}
		})
	}
}

func TestParseCorpusValidationRules(t *testing.T) {
	// each entry breaks one rule of an otherwise valid single-case corpus
	single := func(caseExtra, defect string) string {
		return `[[case]]
name = "c"
pr = "example/repo#7"
head = "` + sha + `"
` + caseExtra + `
[[case.defect]]
id = "d"
title = "t"
` + defect
	}
	tests := []struct {
		name string
		src  string
		want []string // every substring the error must contain
	}{
		{"no cases", "", []string{"no cases"}},
		{"case without a name", strings.Replace(single("", `match = ["x"]`), `name = "c"`, "", 1), []string{"case #1", "name is required"}},
		{"case name uppercase", strings.Replace(single("", `match = ["x"]`), `name = "c"`, `name = "Bad"`, 1), []string{`case "Bad"`, "name must match"}},
		{"case name too long", strings.Replace(single("", `match = ["x"]`), `name = "c"`, `name = "`+strings.Repeat("a", 49)+`"`, 1), []string{"name must match"}},
		{"pr without number", strings.Replace(single("", `match = ["x"]`), "example/repo#7", "example/repo", 1), []string{`case "c"`, "pr must be owner/repo#N"}},
		{"pr number zero", strings.Replace(single("", `match = ["x"]`), "#7", "#0", 1), []string{"pr must be owner/repo#N with N > 0"}},
		{"pr number negative", strings.Replace(single("", `match = ["x"]`), "#7", "#-3", 1), []string{"pr must be owner/repo#N"}},
		{"pr with a URL", strings.Replace(single("", `match = ["x"]`), "example/repo#7", "https://github.com/example/repo/pull/7", 1), []string{"pr must be owner/repo#N"}},
		{"head too short", strings.Replace(single("", `match = ["x"]`), sha, "0123abc", 1), []string{`case "c"`, "head must be a full 40-character"}},
		{"head upper case", strings.Replace(single("", `match = ["x"]`), sha, strings.ToUpper(sha), 1), []string{"head must be a full 40-character"}},
		{"head missing", strings.Replace(single("", `match = ["x"]`), `head = "`+sha+`"`, "", 1), []string{"head must be a full 40-character"}},
		{"base looks like a flag", single(`base = "--upload-pack=x"`, `match = ["x"]`), []string{"base", "not a branch name"}},
		{"base with a space", single(`base = "my branch"`, `match = ["x"]`), []string{"not a branch name"}},
		{"case without defects", `[[case]]
name = "c"
pr = "example/repo#7"
head = "` + sha + `"
`, []string{`case "c"`, "at least one [[case.defect]]"}},
		{"defect without id", strings.Replace(single("", `match = ["x"]`), `id = "d"`, "", 1), []string{`case "c": defect #1`, "id is required"}},
		{"defect id uppercase", strings.Replace(single("", `match = ["x"]`), `id = "d"`, `id = "D"`, 1), []string{`defect "D"`, "id must match"}},
		{"defect without title", strings.Replace(single("", `match = ["x"]`), `title = "t"`, "", 1), []string{`case "c"`, `defect "d"`, "title is required"}},
		{"defect severity unknown", single("", "match = [\"x\"]\nseverity = \"P4\""), []string{`defect "d"`, "severity must be one of P0, P1, P2, P3"}},
		{"defect severity lower case", single("", "match = [\"x\"]\nseverity = \"p1\""), []string{"severity must be one of"}},
		{"defect with neither paths nor match", single("", "lines = [1, 2]"), []string{`defect "d"`, "needs paths or match"}},
		{"defect with nothing", single("", ""), []string{"needs paths or match"}},
		{"body without match", single("", "paths = [\"a.rb\"]\nbody = true"), []string{`defect "d"`, "body = true needs match"}},
		{"lines with one number", single("", "match = [\"x\"]\nlines = [5]"), []string{"lines must be exactly two numbers"}},
		{"lines with three numbers", single("", "match = [\"x\"]\nlines = [1, 2, 3]"), []string{"lines must be exactly two numbers"}},
		{"lines below one", single("", "match = [\"x\"]\nlines = [0, 4]"), []string{"1 <= from <= to"}},
		{"lines reversed", single("", "match = [\"x\"]\nlines = [9, 4]"), []string{"1 <= from <= to", "[9, 4]"}},
		{"lines not numbers", single("", "match = [\"x\"]\nlines = [\"a\", \"b\"]"), []string{"lines"}},
		{"match does not compile", single("", `match = ["(unclosed"]`), []string{`defect "d"`, `match "(unclosed"`}},
		{"match is empty", single("", `match = [""]`), []string{"empty regexp"}},
		{"path pattern malformed", single("", `paths = ["app/[a-.rb"]`), []string{`defect "d"`, "app/[a-.rb"}},
		{"path pattern empty", single("", `paths = [""]`), []string{"empty path pattern"}},
		{"path pattern absolute", single("", `paths = ["/app/a.rb"]`), []string{"empty segment"}},
		{"duplicate defect id", single("", "match = [\"x\"]") + "\n[[case.defect]]\nid = \"d\"\ntitle = \"again\"\nmatch = [\"y\"]\n", []string{`defect "d"`, "duplicate id"}},
		{"duplicate case name", single("", `match = ["x"]`) + "\n" + single("", `match = ["x"]`), []string{`case "c"`, "duplicate name"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCorpus("", []byte(tt.src))
			if err == nil {
				t.Fatal("invalid corpus was accepted")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
			if !strings.HasPrefix(err.Error(), "eval: ") {
				t.Errorf("error %q lacks the eval: prefix", err)
			}
		})
	}
}

func TestParseCorpusReportsEveryProblemAtOnce(t *testing.T) {
	src := `[[case]]
name = "one"
pr = "nope"
head = "abc"

[[case]]
name = "two"
pr = "example/repo#2"
head = "` + sha + `"

[[case.defect]]
id = "d"
title = "t"
`
	_, err := parseCorpus("", []byte(src))
	if err == nil {
		t.Fatal("invalid corpus was accepted")
	}
	for _, w := range []string{`case "one"`, "pr must be", "head must be", "at least one [[case.defect]]", `case "two"`, `defect "d"`, "needs paths or match"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q lacks %q", err, w)
		}
	}
}

func TestParseCorpusAcceptsGlobsAndMatchRegexpsThatAreValid(t *testing.T) {
	src := `[[case]]
name = "c"
pr = "example/repo#7"
head = "` + sha + `"
[[case.defect]]
id = "d"
title = "t"
paths = ["**/*.rb", "app/**/oauth*.rb", "lib/[a-c]*.rb", "x/**"]
match = ["(?i)foo|bar", "a{2}", "\\bP1\\b"]
`
	if _, err := parseCorpus("", []byte(src)); err != nil {
		t.Fatal(err)
	}
}

func TestParseCorpusReportsTOMLSyntaxErrors(t *testing.T) {
	if _, err := parseCorpus("", []byte("[[case]\nname =")); err == nil || !strings.HasPrefix(err.Error(), "eval: corpus") {
		t.Fatalf("syntax error = %v, want an eval: corpus error", err)
	}
}

func TestLoadCorpusReadsAFileAndNamesItInErrors(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "corpus.toml")
	if err := os.WriteFile(good, []byte(validCorpus), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCorpus(good)
	if err != nil || len(c.Cases) != 2 {
		t.Fatalf("LoadCorpus = %v, %v", c, err)
	}

	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("[[case]]\nname = \"Bad\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCorpus(bad); err == nil || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "name must match") {
		t.Errorf("invalid file error = %v, want the path and the rule", err)
	}
	if _, err := LoadCorpus(filepath.Join(dir, "missing.toml")); err == nil || !strings.Contains(err.Error(), "missing.toml") {
		t.Errorf("missing file error = %v, want the path", err)
	}
}

func TestSelect(t *testing.T) {
	c, err := parseCorpus("", []byte(validCorpus))
	if err != nil {
		t.Fatal(err)
	}
	names := func(cs []Case) []string {
		var out []string
		for _, cs := range cs {
			out = append(out, cs.Name)
		}
		return out
	}
	tests := []struct {
		name    string
		pick    []string
		want    []string
		wantErr string
	}{
		{"nil selects all", nil, []string{"oauth-hmac", "sandbox"}, ""},
		{"empty selects all", []string{}, []string{"oauth-hmac", "sandbox"}, ""},
		{"one", []string{"sandbox"}, []string{"sandbox"}, ""},
		{"corpus order wins", []string{"sandbox", "oauth-hmac"}, []string{"oauth-hmac", "sandbox"}, ""},
		{"repeated name once", []string{"sandbox", "sandbox"}, []string{"sandbox"}, ""},
		{"unknown name", []string{"sandbox", "nope"}, nil, `"nope"`},
		{"unknown names are all listed", []string{"nope", "nada"}, nil, `"nope", "nada"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.Select(tt.pick)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "oauth-hmac") {
					t.Fatalf("Select error = %v, want it to name %s and what the corpus has", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(names(got), tt.want) {
				t.Errorf("Select = %v, want %v", names(got), tt.want)
			}
		})
	}
}

// expect is "found" (the default) or "absent"; anything else is refused, naming the defect.
func TestCorpusRejectsAnUnknownExpect(t *testing.T) {
	src := func(expect string) string {
		return "[[case]]\nname = \"c\"\npr = \"example/repo#7\"\nhead = \"" + sha + "\"\n[[case.defect]]\nid = \"d\"\ntitle = \"t\"\nmatch = [\"x\"]\n" + expect
	}
	for _, ok := range []string{"", `expect = "found"`, `expect = "absent"`} {
		if _, err := parseCorpus("", []byte(src(ok))); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	_, err := parseCorpus("", []byte(src(`expect = "maybe"`)))
	if err == nil || !strings.Contains(err.Error(), `defect "d"`) || !strings.Contains(err.Error(), "expect must be") ||
		!strings.Contains(err.Error(), `"maybe"`) {
		t.Fatalf("unknown expect: %v", err)
	}
}

// withoutRegexps is c without the compiled match expressions, which two parses compile apart.
func withoutRegexps(c *Corpus) Corpus {
	out := Corpus{Cases: slices.Clone(c.Cases)}
	for i := range out.Cases {
		out.Cases[i].Defects = slices.Clone(out.Cases[i].Defects)
		for j := range out.Cases[i].Defects {
			out.Cases[i].Defects[j].res = nil
		}
	}
	return out
}

// WriteCorpus writes TOML that LoadCorpus reads back as the same corpus (absent defects, regexps
// with backslashes and quotes, optional fields left out), private, and leaves a file that already
// holds the same text alone; a corpus LoadCorpus would refuse is not written.
func TestWriteCorpusRoundTrips(t *testing.T) {
	want, err := parseCorpus("", []byte(validCorpus+`
[[case]]
name = "decline-7"
pr = "example/sandbox#2"
head = "`+sha+`"
why = "the author said the guard \"is not needed\""

[[case.defect]]
id = "decline-7"
title = "Nil guard on a checked token"
expect = "absent"
severity = "P1"
paths = ["app/\\[id\\]/page.tsx"]
lines = [12, 12]
match = ["\\bnil\\b", "guard (is|was) missing"]
`))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "learn", "corpus.toml")
	if err := WriteCorpus(path, *want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCorpus(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withoutRegexps(got), withoutRegexps(want)) {
		t.Fatalf("round trip changed the corpus:\n got %+v\nwant %+v", got, want)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("corpus mode = %v (%v), want 0600", st.Mode().Perm(), err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "body = false") || strings.Contains(string(b), `base = ""`) {
		t.Errorf("the file spells out the optional fields left empty:\n%s", b)
	}

	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := WriteCorpus(path, *got); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || !st.ModTime().Equal(old) {
		t.Errorf("the same corpus was written again (mtime %v, %v)", st.ModTime(), err)
	}

	bad := *want
	bad.Cases = []Case{{Name: "Bad", PR: "example/x#1", Head: sha, Defects: []Defect{{ID: "d", Title: "t", Match: []string{"x"}}}}}
	if err := WriteCorpus(path, bad); err == nil || !strings.Contains(err.Error(), "name must match") {
		t.Fatalf("invalid corpus: %v", err)
	}
	if got, err := LoadCorpus(path); err != nil || len(got.Cases) != len(want.Cases) {
		t.Errorf("an invalid corpus replaced the file: %+v, %v", got, err)
	}
}

// LoadCorpora reads the operator's corpus and then the learned one; a learned file that is not there
// adds nothing, a user corpus may be left out, and a case name in both files is refused by name.
func TestLoadCorporaRefusesACaseNameInBothFiles(t *testing.T) {
	dir := t.TempDir()
	user, learned := filepath.Join(dir, "eval.toml"), filepath.Join(dir, "learn", "corpus.toml")
	if err := os.WriteFile(user, []byte(validCorpus), 0o600); err != nil {
		t.Fatal(err)
	}
	names := func(c *Corpus) []string {
		var out []string
		for _, cs := range c.Cases {
			out = append(out, cs.Name)
		}
		return out
	}
	if c, err := LoadCorpora(user, learned); err != nil || !slices.Equal(names(c), []string{"oauth-hmac", "sandbox"}) {
		t.Fatalf("no learned file: %v, %v", c, err)
	}
	one := func(name string) Case {
		return Case{Name: name, PR: "example/sandbox#3", Head: sha, Defects: []Defect{{ID: name, Title: "t", Expect: ExpectAbsent, Match: []string{"x"}}}}
	}
	if err := WriteCorpus(learned, Corpus{Cases: []Case{one("miss-3"), one("decline-4")}}); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadCorpora(user, learned); err != nil || !slices.Equal(names(c), []string{"oauth-hmac", "sandbox", "miss-3", "decline-4"}) {
		t.Fatalf("both files: %v, %v", c, err)
	}
	if c, err := LoadCorpora("", learned); err != nil || !slices.Equal(names(c), []string{"miss-3", "decline-4"}) || c.Cases[0].Owner != "example" {
		t.Fatalf("the learned file alone: %v, %v", c, err)
	}
	if _, err := LoadCorpora("", filepath.Join(dir, "none.toml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no corpus at all: %v, want the learned file's not-exist error", err)
	}
	if err := WriteCorpus(learned, Corpus{Cases: []Case{one("sandbox"), one("miss-3")}}); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCorpora(user, learned)
	if err == nil || !strings.Contains(err.Error(), `"sandbox"`) || strings.Contains(err.Error(), "miss-3") ||
		!strings.Contains(err.Error(), user) || !strings.Contains(err.Error(), learned) {
		t.Fatalf("a name in both: %v, want an error naming the case and both files", err)
	}
}

// A run given the learned corpus with --corpus records that file as its corpus, and an `eval score`
// that reads it as the operator's corpus and as the learned one reads it once: the same path,
// cleaned, is one file, not two that share every case name.
func TestLoadCorporaReadsAFileNamedTwiceOnce(t *testing.T) {
	dir := t.TempDir()
	learned := filepath.Join(dir, "learn", "corpus.toml")
	one := Case{Name: "decline-4", PR: "example/sandbox#3", Head: sha, Defects: []Defect{{ID: "decline-4", Title: "t", Expect: ExpectAbsent, Match: []string{"x"}}}}
	if err := WriteCorpus(learned, Corpus{Cases: []Case{one}}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{learned, dir + "/learn/./corpus.toml", dir + "/learn/../learn/corpus.toml"} {
		c, err := LoadCorpora(user, learned)
		if err != nil || len(c.Cases) != 1 || c.Cases[0].Name != "decline-4" {
			t.Fatalf("LoadCorpora(%s, %s) = %+v, %v; want the one case once", user, learned, c, err)
		}
	}
}

// The shipped example corpus is valid: a user copies it to start.
func TestTheExampleCorpusLoads(t *testing.T) {
	c, err := LoadCorpus(filepath.Join("..", "..", "eval.toml.example"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cases) == 0 || len(c.Cases[0].Defects) == 0 || c.Cases[0].Owner != "talkable" {
		t.Fatalf("example corpus %+v", c)
	}
}
