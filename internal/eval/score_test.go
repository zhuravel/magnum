package eval

import (
	"slices"
	"testing"
)

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"app/controllers/oauth/*.rb", "app/controllers/oauth/callback.rb", true},
		{"app/controllers/oauth/*.rb", "app/controllers/oauth/sub/callback.rb", false},
		{"app/controllers/oauth/*.rb", "app/controllers/oauth/callback.js", false},
		{"lib/shopify_auth.rb", "lib/shopify_auth.rb", true},
		{"lib/shopify_auth.rb", "other/lib/shopify_auth.rb", false},
		{"lib/[a-c]*.rb", "lib/beta.rb", true},
		{"lib/[a-c]*.rb", "lib/delta.rb", false},
		{"app/**/oauth*.rb", "app/oauth.rb", true},
		{"app/**/oauth*.rb", "app/oauth_callback.rb", true},
		{"app/**/oauth*.rb", "app/controllers/oauth_callback.rb", true},
		{"app/**/oauth*.rb", "app/a/b/c/oauth.rb", true},
		{"app/**/oauth*.rb", "app/a/b/c/other.rb", false},
		{"app/**/oauth*.rb", "lib/oauth.rb", false},
		{"app/**/oauth*.rb", "app/oauth/readme.md", false},
		{"**/*.rb", "a.rb", true},
		{"**/*.rb", "a/b/c.rb", true},
		{"**/*.rb", "a/b/c.js", false},
		{"app/**", "app/a/b.rb", true},
		{"app/**", "lib/a.rb", false},
		{"app/**/models/**/user.rb", "app/x/models/y/z/user.rb", true},
		{"app/**/models/**/user.rb", "app/models/user.rb", true},
		{"app/**/models/**/user.rb", "app/x/user.rb", false},
		{"app/**oauth.rb", "app/my_oauth.rb", true},
		{"app/**oauth.rb", "app/x/my_oauth.rb", false},
		{"app/[", "app/x", false},
		{"app/**/[", "app/a/x", false},
	}
	for _, tt := range tests {
		if got := matchGlob(tt.pattern, tt.name); got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

// caseOf parses a one-case corpus holding the given defects (TOML text after the case header).
func caseOf(t *testing.T, defects string) Case {
	t.Helper()
	c, err := parseCorpus("", []byte(`[[case]]
name = "c"
pr = "example/repo#7"
head = "`+sha+`"
`+defects))
	if err != nil {
		t.Fatal(err)
	}
	return c.Cases[0]
}

func inline(path string, line int, body string) Finding {
	return Finding{Path: path, Line: line, Body: body}
}

func TestScoreCaseMatching(t *testing.T) {
	const defect = `[[case.defect]]
id = "d"
title = "t"
`
	tests := []struct {
		name    string
		defect  string
		finding Finding
		want    bool
	}{
		{"path only", `paths = ["a/*.rb"]`, inline("a/x.rb", 10, "anything"), true},
		{"wrong path", `paths = ["a/*.rb"]`, inline("b/x.rb", 10, "anything"), false},
		{"second path", `paths = ["a/*.rb", "b/*.rb"]`, inline("b/x.rb", 10, "anything"), true},
		{"double star path", `paths = ["app/**/oauth*.rb"]`, inline("app/controllers/oauth_cb.rb", 1, "x"), true},
		{"match only, any path", `match = ["hmac"]`, inline("whatever.rb", 3, "HMAC is skipped"), true},
		{"match is case insensitive", `match = ["hmac"]`, inline("a.rb", 3, "Hmac skipped"), true},
		{"match alternatives", `match = ["hmac", "signature"]`, inline("a.rb", 3, "bad signature check"), true},
		{"match misses", `match = ["hmac", "signature"]`, inline("a.rb", 3, "unrelated"), false},
		{"match is a regexp", `match = ["discount.*(reuse|twice)"]`, inline("a.rb", 3, "discount can be used twice"), true},
		{"match across lines needs no dot-all", `match = ["hmac"]`, inline("a.rb", 3, "first line\nthen hmac"), true},
		{"path and match both required", "paths = [\"a.rb\"]\nmatch = [\"hmac\"]", inline("a.rb", 3, "unrelated"), false},
		{"path and match both satisfied", "paths = [\"a.rb\"]\nmatch = [\"hmac\"]", inline("a.rb", 3, "hmac"), true},
		{"line inside", "paths = [\"a.rb\"]\nlines = [40, 75]", inline("a.rb", 50, "x"), true},
		{"line on the range start", "paths = [\"a.rb\"]\nlines = [40, 75]", inline("a.rb", 40, "x"), true},
		{"line on the range end", "paths = [\"a.rb\"]\nlines = [40, 75]", inline("a.rb", 75, "x"), true},
		{"line three before is within slack", "paths = [\"a.rb\"]\nlines = [40, 75]", inline("a.rb", 37, "x"), true},
		{"line four before is out", "paths = [\"a.rb\"]\nlines = [40, 75]", inline("a.rb", 36, "x"), false},
		{"line three after is within slack", "paths = [\"a.rb\"]\nlines = [40, 75]", inline("a.rb", 78, "x"), true},
		{"line four after is out", "paths = [\"a.rb\"]\nlines = [40, 75]", inline("a.rb", 79, "x"), false},
		{"slack near the top of the file", "paths = [\"a.rb\"]\nlines = [1, 2]", inline("a.rb", 5, "x"), true},
		{"multi line range reaching into the defect", "paths = [\"a.rb\"]\nlines = [40, 75]", Finding{Path: "a.rb", Line: 90, StartLine: 70, Body: "x"}, true},
		{"multi line range around the defect", "paths = [\"a.rb\"]\nlines = [40, 45]", Finding{Path: "a.rb", Line: 90, StartLine: 10, Body: "x"}, true},
		{"multi line range ending before the slack", "paths = [\"a.rb\"]\nlines = [40, 75]", Finding{Path: "a.rb", Line: 30, StartLine: 20, Body: "x"}, false},
		{"multi line range starting after the slack", "paths = [\"a.rb\"]\nlines = [40, 75]", Finding{Path: "a.rb", Line: 99, StartLine: 80, Body: "x"}, false},
		{"comment on a whole file has no line to compare", "paths = [\"a.rb\"]\nlines = [1, 5]", inline("a.rb", 0, "x"), false},
		{"comment on a whole file matches a defect without lines", `paths = ["a.rb"]`, inline("a.rb", 0, "x"), true},
		{"lines need the path too", "paths = [\"a.rb\"]\nlines = [40, 75]", inline("b.rb", 50, "x"), false},
		{"lines with match", "match = [\"hmac\"]\nlines = [40, 75]", inline("any.rb", 50, "hmac"), true},
		{"lines with match, wrong line", "match = [\"hmac\"]\nlines = [40, 75]", inline("any.rb", 10, "hmac"), false},
		{"simplification never matches", `paths = ["a.rb"]`, Finding{Path: "a.rb", Line: 1, Body: "Simplification (optional): hmac", Simplification: true}, false},
		{"review body does not match an inline defect", `match = ["hmac"]`, Finding{Body: "hmac is skipped", InBody: true}, false},
		{"review body matches a body defect", "match = [\"hmac\"]\nbody = true", Finding{Body: "HMAC is skipped", InBody: true}, true},
		{"review body ignores the paths of a body defect", "paths = [\"a.rb\"]\nmatch = [\"hmac\"]\nbody = true", Finding{Body: "hmac is skipped", InBody: true}, true},
		{"review body that does not match a body defect", "match = [\"hmac\"]\nbody = true", Finding{Body: "all fine", InBody: true}, false},
		{"inline comment still matches a body defect", "match = [\"hmac\"]\nbody = true", inline("a.rb", 1, "hmac"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := caseOf(t, defect+tt.defect+"\n")
			s := ScoreCase(c, Result{Findings: []Finding{tt.finding}})
			if s.Defects[0].Found != tt.want {
				t.Errorf("Found = %v, want %v", s.Defects[0].Found, tt.want)
			}
			if tt.want && !slices.Equal(s.Defects[0].By, []int{0}) {
				t.Errorf("By = %v, want [0]", s.Defects[0].By)
			}
		})
	}
}

func TestScoreCaseCountsFoundDefectsAndNoise(t *testing.T) {
	c := caseOf(t, `[[case.defect]]
id = "hmac"
title = "HMAC skipped"
match = ["hmac"]

[[case.defect]]
id = "reuse"
title = "Discount reused"
match = ["reuse"]
`)
	r := Result{Status: "dry_run", Event: "COMMENT", Findings: []Finding{
		inline("a.rb", 1, "hmac skipped"),
		inline("b.rb", 2, "unrelated one"),
		inline("c.rb", 3, "unrelated two"),
		{Path: "d.rb", Line: 4, Body: "Simplification (optional): shorter", Simplification: true},
		{Body: "the review body mentions nothing", InBody: true},
	}}
	s := ScoreCase(c, r)
	if s.Case != "c" || s.Status != "dry_run" || s.Event != "COMMENT" {
		t.Errorf("identity = %q %q %q", s.Case, s.Status, s.Event)
	}
	if s.Found != 1 || s.Total != 2 {
		t.Errorf("found = %d/%d, want 1/2", s.Found, s.Total)
	}
	if !s.Defects[0].Found || s.Defects[1].Found {
		t.Errorf("defects = %+v", s.Defects)
	}
	if s.Defects[1].By != nil || s.Defects[1].SeverityOK {
		t.Errorf("missed defect has matches or a severity verdict: %+v", s.Defects[1])
	}
	if s.Noise != 2 || len(s.Unmatched) != 2 || s.Unmatched[0].Path != "b.rb" || s.Unmatched[1].Path != "c.rb" {
		t.Errorf("noise = %d unmatched = %+v, want the two unrelated inline findings", s.Noise, s.Unmatched)
	}
	if s.Simplifications != 1 {
		t.Errorf("simplifications = %d, want 1", s.Simplifications)
	}
}

func TestScoreCaseSimplificationsAreNeverNoiseNorDefects(t *testing.T) {
	c := caseOf(t, `[[case.defect]]
id = "d"
title = "t"
paths = ["a.rb"]
`)
	s := ScoreCase(c, Result{Findings: []Finding{
		{Path: "a.rb", Line: 1, Body: "Simplification (optional): x", Simplification: true},
		{Path: "z.rb", Line: 1, Body: "Simplification (optional): y", Simplification: true},
	}})
	if s.Found != 0 || s.Noise != 0 || len(s.Unmatched) != 0 || s.Simplifications != 2 {
		t.Errorf("found=%d noise=%d unmatched=%d simplifications=%d, want 0 0 0 2", s.Found, s.Noise, len(s.Unmatched), s.Simplifications)
	}
}

func TestScoreCaseReviewBodyIsNeitherNoiseNorAnInlineMatch(t *testing.T) {
	c := caseOf(t, `[[case.defect]]
id = "inline"
title = "inline only"
match = ["hmac"]

[[case.defect]]
id = "either"
title = "body also counts"
match = ["refund"]
body = true
`)
	s := ScoreCase(c, Result{Findings: []Finding{{Body: "hmac and refund are both discussed here", InBody: true}}})
	if s.Defects[0].Found {
		t.Error("a review body mention satisfied a defect that needs an inline comment")
	}
	if !s.Defects[1].Found {
		t.Error("a review body mention did not satisfy a body = true defect")
	}
	if s.Noise != 0 {
		t.Errorf("noise = %d, want 0: the review body is not an inline finding", s.Noise)
	}
}

func TestScoreCaseOneFindingMayMatchSeveralDefects(t *testing.T) {
	c := caseOf(t, `[[case.defect]]
id = "a"
title = "A"
match = ["hmac"]

[[case.defect]]
id = "b"
title = "B"
paths = ["oauth.rb"]
`)
	s := ScoreCase(c, Result{Findings: []Finding{inline("oauth.rb", 5, "hmac skipped")}})
	if s.Found != 2 || !slices.Equal(s.Defects[0].By, []int{0}) || !slices.Equal(s.Defects[1].By, []int{0}) {
		t.Errorf("found=%d defects=%+v, want both defects found by finding 0", s.Found, s.Defects)
	}
	if s.Noise != 0 {
		t.Errorf("noise = %d, want 0", s.Noise)
	}
}

func TestScoreCaseCollectsEveryMatchingFinding(t *testing.T) {
	c := caseOf(t, `[[case.defect]]
id = "a"
title = "A"
paths = ["oauth.rb"]
`)
	s := ScoreCase(c, Result{Findings: []Finding{
		inline("oauth.rb", 1, "one"),
		inline("other.rb", 2, "two"),
		inline("oauth.rb", 3, "three"),
	}})
	if !slices.Equal(s.Defects[0].By, []int{0, 2}) || s.Found != 1 || s.Noise != 1 {
		t.Errorf("By=%v found=%d noise=%d, want [0 2] 1 1", s.Defects[0].By, s.Found, s.Noise)
	}
}

func TestScoreCaseSeverity(t *testing.T) {
	tests := []struct {
		name     string
		want     string // the defect's severity
		findings []Finding
		wantSev  string
		wantOK   bool
	}{
		{"exact severity", "P1", []Finding{{Path: "a.rb", Line: 1, Body: "x", Severity: "P1"}}, "P1", true},
		{"stronger than wanted", "P2", []Finding{{Path: "a.rb", Line: 1, Body: "x", Severity: "P1"}}, "P1", true},
		{"weaker than wanted", "P1", []Finding{{Path: "a.rb", Line: 1, Body: "x", Severity: "P2"}}, "P2", false},
		{"no severity wanted", "", []Finding{{Path: "a.rb", Line: 1, Body: "x", Severity: "P3"}}, "P3", true},
		{"no severity wanted, none given", "", []Finding{{Path: "a.rb", Line: 1, Body: "x"}}, "", true},
		{"severity wanted, none given", "P2", []Finding{{Path: "a.rb", Line: 1, Body: "x"}}, "", false},
		{"strongest of several wins", "P1", []Finding{
			{Path: "a.rb", Line: 1, Body: "x", Severity: "P3"},
			{Path: "a.rb", Line: 2, Body: "x", Severity: "P0"},
			{Path: "a.rb", Line: 3, Body: "x", Severity: "P2"},
		}, "P0", true},
		{"strongest of several, still too weak", "P0", []Finding{
			{Path: "a.rb", Line: 1, Body: "x", Severity: "P3"},
			{Path: "a.rb", Line: 2, Body: "x", Severity: "P1"},
		}, "P1", false},
		{"unlabelled finding does not hide a labelled one", "P2", []Finding{
			{Path: "a.rb", Line: 1, Body: "x"},
			{Path: "a.rb", Line: 2, Body: "x", Severity: "P2"},
		}, "P2", true},
		{"found only in the review body has no severity", "P2", nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defect := "[[case.defect]]\nid = \"d\"\ntitle = \"t\"\npaths = [\"a.rb\"]\n"
			if tt.want != "" {
				defect += "severity = \"" + tt.want + "\"\n"
			}
			findings := tt.findings
			if findings == nil {
				defect += "match = [\"hmac\"]\nbody = true\n"
				findings = []Finding{{Body: "hmac is skipped", InBody: true}}
			}
			s := ScoreCase(caseOf(t, defect), Result{Findings: findings})
			d := s.Defects[0]
			if !d.Found {
				t.Fatal("defect not found")
			}
			if d.Severity != tt.wantSev || d.SeverityOK != tt.wantOK || d.Want != tt.want {
				t.Errorf("Want=%q Severity=%q SeverityOK=%v, want %q %q %v", d.Want, d.Severity, d.SeverityOK, tt.want, tt.wantSev, tt.wantOK)
			}
		})
	}
}

func TestScoreCaseAMissedDefectIsNeverSeverityOK(t *testing.T) {
	c := caseOf(t, `[[case.defect]]
id = "d"
title = "t"
paths = ["a.rb"]
`)
	s := ScoreCase(c, Result{})
	if d := s.Defects[0]; d.Found || d.SeverityOK || d.Severity != "" {
		t.Errorf("defect = %+v, want an empty miss", d)
	}
	if s.SeverityOK() != 0 {
		t.Errorf("SeverityOK() = %d, want 0", s.SeverityOK())
	}
}

func TestScoreCaseSeverityOKCountsOnlyDefectsAtTheWantedSeverity(t *testing.T) {
	c := caseOf(t, `[[case.defect]]
id = "a"
title = "A"
severity = "P1"
paths = ["a.rb"]

[[case.defect]]
id = "b"
title = "B"
severity = "P1"
paths = ["b.rb"]

[[case.defect]]
id = "c"
title = "C"
paths = ["c.rb"]
`)
	s := ScoreCase(c, Result{Findings: []Finding{
		{Path: "a.rb", Line: 1, Body: "x", Severity: "P0"},
		{Path: "b.rb", Line: 1, Body: "x", Severity: "P3"},
		{Path: "c.rb", Line: 1, Body: "x"},
	}})
	if s.Found != 3 || s.SeverityOK() != 2 {
		t.Errorf("found=%d severityOK=%d, want 3 and 2", s.Found, s.SeverityOK())
	}
}

func TestScoreCaseWorksOnADefectBuiltByHand(t *testing.T) {
	// no parseCorpus: the regexps are compiled when scoring
	c := Case{Name: "hand", Defects: []Defect{
		{ID: "a", Title: "A", Match: []string{"hmac"}},
		{ID: "b", Title: "B", Match: []string{"(unclosed"}},
		{ID: "c", Title: "C", Match: []string{"(unclosed", "refund"}},
		{ID: "empty", Title: "never distinguishable"},
	}}
	s := ScoreCase(c, Result{Findings: []Finding{inline("x.rb", 1, "HMAC and a refund")}})
	got := []bool{s.Defects[0].Found, s.Defects[1].Found, s.Defects[2].Found, s.Defects[3].Found}
	if !slices.Equal(got, []bool{true, false, true, false}) {
		t.Errorf("found = %v, want [true false true false]", got)
	}
}

func TestScoreCaseWithoutDefectsOrFindings(t *testing.T) {
	s := ScoreCase(Case{Name: "empty"}, Result{})
	if s.Found != 0 || s.Total != 0 || s.Noise != 0 || len(s.Defects) != 0 {
		t.Errorf("score = %+v", s)
	}
}
