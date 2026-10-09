package learn

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/store"
)

const corpusSHA = "0123456789abcdef0123456789abcdef01234567"

func generalMiss() store.Miss {
	return store.Miss{ID: 12, Class: store.MissMiss, Scope: store.MissScopeGeneral, Severity: "P1", State: store.MissNew,
		ReviewedSHA: corpusSHA, Repo: "talkable/talkable", Number: 11, Path: "app/models/coupon.rb", Line: 41,
		Lines: []int{40, 42}, Match: []string{"site"}, Title: "Coupon lookup ignores the site"}
}

// A general miss of severity P0 to P2 that is new or used, on a commit magnum reviewed, is a learned case
// the review must find: miss-<id> on the PR at the reviewed commit, one defect with the row's path,
// lines, match and severity. Any other miss is none.
func TestCaseFromMissTakesOnlyGeneralP0ToP2Misses(t *testing.T) {
	c, ok := CaseFromMiss(generalMiss())
	if !ok {
		t.Fatal("a general P1 miss made no case")
	}
	want := eval.Case{Name: "miss-12", PR: "talkable/talkable#11", Head: corpusSHA, Why: "Coupon lookup ignores the site",
		Owner: "talkable", Repo: "talkable", Number: 11, Defects: []eval.Defect{{ID: "miss-12", Title: "Coupon lookup ignores the site",
			Severity: "P1", Paths: []string{"app/models/coupon.rb"}, Lines: []int{40, 42}, Match: []string{"site"}}}}
	if got := c; !reflect.DeepEqual(stripped(got), stripped(want)) {
		t.Fatalf("case = %+v\nwant %+v", got, want)
	}
	if s := eval.ScoreCase(c, eval.Result{Findings: []eval.Finding{{Path: "app/models/coupon.rb", Line: 41, Body: "scope it to the site", Severity: "P1"}}}); s.Found != 1 {
		t.Errorf("the finding that reports the miss does not find it: %+v", s)
	}

	for name, m := range map[string]func(*store.Miss){
		"P0":                                     func(m *store.Miss) { m.Severity = "P0" },
		"P2":                                     func(m *store.Miss) { m.Severity = "P2" },
		"used":                                   func(m *store.Miss) { m.State = store.MissUsed },
		"a path with glob characters is literal": func(m *store.Miss) { m.Path = "app/[id]/page*.tsx" },
		"a match that does not compile is left out": func(m *store.Miss) { m.Match = []string{"(unclosed", "site"} },
		"no lines":           func(m *store.Miss) { m.Lines, m.Line = nil, 0 },
		"one line":           func(m *store.Miss) { m.Lines = nil },
		"a title is made up": func(m *store.Miss) { m.Title = " " },
	} {
		mm := generalMiss()
		m(&mm)
		c, ok := CaseFromMiss(mm)
		if !ok {
			t.Errorf("%s: no case", name)
			continue
		}
		if err := c.Validate(); err != nil {
			t.Errorf("%s: invalid case: %v", name, err)
		}
		d := c.Defects[0]
		switch name {
		case "a path with glob characters is literal":
			if s := eval.ScoreCase(c, eval.Result{Findings: []eval.Finding{{Path: "app/[id]/page*.tsx", Line: 41, Body: "site"}}}); s.Found != 1 {
				t.Errorf("%s: paths %v do not match the path itself", name, d.Paths)
			}
			if s := eval.ScoreCase(c, eval.Result{Findings: []eval.Finding{{Path: "app/i/page1.tsx", Line: 41, Body: "site"}}}); s.Found != 0 {
				t.Errorf("%s: paths %v match another path", name, d.Paths)
			}
		case "a match that does not compile is left out":
			if !reflect.DeepEqual(d.Match, []string{"site"}) {
				t.Errorf("%s: match %v", name, d.Match)
			}
		case "no lines":
			if d.Lines != nil {
				t.Errorf("%s: lines %v", name, d.Lines)
			}
		case "one line":
			if !reflect.DeepEqual(d.Lines, []int{41, 41}) {
				t.Errorf("%s: lines %v, want the comment's line", name, d.Lines)
			}
		case "a title is made up":
			if d.Title != "miss 12" || c.Why != "" {
				t.Errorf("%s: title %q why %q", name, d.Title, c.Why)
			}
		}
	}

	for name, m := range map[string]func(*store.Miss){
		"repo scope":            func(m *store.Miss) { m.Scope = store.MissScopeRepo },
		"no scope":              func(m *store.Miss) { m.Scope = "" },
		"style":                 func(m *store.Miss) { m.Class = store.MissStyle },
		"P3":                    func(m *store.Miss) { m.Severity = "P3" },
		"no severity":           func(m *store.Miss) { m.Severity = "" },
		"dismissed":             func(m *store.Miss) { m.State = store.MissDismissed },
		"short reviewed commit": func(m *store.Miss) { m.ReviewedSHA = corpusSHA[:12] },
		"no PR":                 func(m *store.Miss) { m.Repo = "" },
		"no path nor match":     func(m *store.Miss) { m.Path, m.Match = "", []string{"(unclosed"} },
	} {
		mm := generalMiss()
		m(&mm)
		if c, ok := CaseFromMiss(mm); ok {
			t.Errorf("%s: case %+v, want none", name, c)
		}
	}
}

func generalDecline() store.Decline {
	return store.Decline{ID: 7, Verdict: store.VerdictWrong, Outcome: store.DeclineDeclined, Scope: store.MissScopeGeneral,
		State: store.MissNew, ReviewedSHA: corpusSHA, Repo: "talkable/talkable", Number: 11, Path: "app/x.rb", Line: 30,
		Priority: "P1", Title: "Token can be nil", Match: []string{"nil"}}
}

// A general decline whose verdict says magnum was wrong or ranked the point too high is a learned
// case the review must not report: decline-<id> with one absent defect on the finding's path and
// line. An over-ranked one is a hit only at the posted priority or stronger.
func TestCaseFromDeclineMakesAnAbsentDefect(t *testing.T) {
	c, ok := CaseFromDecline(generalDecline())
	if !ok {
		t.Fatal("a general wrong decline made no case")
	}
	want := eval.Case{Name: "decline-7", PR: "talkable/talkable#11", Head: corpusSHA, Why: "Token can be nil",
		Owner: "talkable", Repo: "talkable", Number: 11, Defects: []eval.Defect{{ID: "decline-7", Title: "Token can be nil",
			Expect: eval.ExpectAbsent, Paths: []string{"app/x.rb"}, Lines: []int{30, 30}, Match: []string{"nil"}}}}
	if !reflect.DeepEqual(stripped(c), stripped(want)) {
		t.Fatalf("case = %+v\nwant %+v", c, want)
	}
	hit := eval.Result{Findings: []eval.Finding{{Path: "app/x.rb", Line: 31, Body: "the token can be nil", Severity: "P2"}}}
	if s := eval.ScoreCase(c, hit); len(s.AbsentHits) != 1 || s.Total != 0 {
		t.Errorf("the declined finding again: %+v, want one absent hit", s)
	}

	over := generalDecline()
	over.Verdict = store.VerdictOverRanked
	c, ok = CaseFromDecline(over)
	if !ok || c.Defects[0].Severity != "P1" || c.Defects[0].Expect != eval.ExpectAbsent {
		t.Fatalf("over-ranked: %+v, %v; want an absent defect at P1", c, ok)
	}
	if s := eval.ScoreCase(c, hit); len(s.AbsentHits) != 0 || s.Noise != 0 {
		t.Errorf("the point ranked P2 on an over-ranked P1: %+v, want neither a hit nor noise", s)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("over-ranked case invalid: %v", err)
	}

	for name, m := range map[string]func(*store.Decline){
		"missing context":       func(d *store.Decline) { d.Verdict = store.VerdictMissingContext },
		"right":                 func(d *store.Decline) { d.Verdict = store.VerdictRight },
		"fixed":                 func(d *store.Decline) { d.Verdict, d.Outcome = "", store.DeclineFixed },
		"repo scope":            func(d *store.Decline) { d.Scope = store.MissScopeRepo },
		"dismissed":             func(d *store.Decline) { d.State = store.MissDismissed },
		"short reviewed commit": func(d *store.Decline) { d.ReviewedSHA = "abc" },
		// Without the posted priority every matching finding would be a hit, so the point ranked
		// lower, the fix the verdict asks for, could only stop being one by not being reported.
		"over-ranked, no priority":      func(d *store.Decline) { d.Verdict, d.Priority = store.VerdictOverRanked, "" },
		"over-ranked, unknown priority": func(d *store.Decline) { d.Verdict, d.Priority = store.VerdictOverRanked, "high" },
	} {
		d := generalDecline()
		m(&d)
		if c, ok := CaseFromDecline(d); ok {
			t.Errorf("%s: case %+v, want none", name, c)
		}
	}
	used := generalDecline()
	used.State, used.Line = store.MissUsed, 0
	if c, ok := CaseFromDecline(used); !ok || c.Defects[0].Lines != nil || !strings.HasPrefix(c.Name, "decline-") {
		t.Errorf("a used decline on a whole file: %+v, %v", c, ok)
	}
}

// A row a gate dismissed because its baseline covers it already keeps its
// learned case: it checks that a later candidate does not lose what the
// skill does. Any other dismissal drops it.
func TestALearnedCaseOutlivesADismissalAtTheBaseline(t *testing.T) {
	m := generalMiss()
	m.State, m.Dismissed = store.MissDismissed, store.LearnDismissedFound
	if _, ok := CaseFromMiss(m); !ok {
		t.Error("a miss found at the baseline lost its case")
	}
	d := generalDecline()
	d.State, d.Dismissed = store.MissDismissed, store.LearnDismissedNotReported
	if _, ok := CaseFromDecline(d); !ok {
		t.Error("a decline not reported at the baseline lost its case")
	}
	for _, why := range []string{store.LearnDismissedFailures, store.LearnDismissedRejections, ""} {
		m.Dismissed, d.Dismissed = why, why
		if _, ok := CaseFromMiss(m); ok {
			t.Errorf("a miss dismissed %q kept its case", why)
		}
		if _, ok := CaseFromDecline(d); ok {
			t.Errorf("a decline dismissed %q kept its case", why)
		}
	}
}

// stripped is c with its defects' compiled regexps dropped (Validate compiles them).
func stripped(c eval.Case) eval.Case {
	out := c
	out.Defects = nil
	for _, d := range c.Defects {
		out.Defects = append(out.Defects, eval.Defect{ID: d.ID, Title: d.Title, Severity: d.Severity, Paths: d.Paths,
			Lines: d.Lines, Match: d.Match, Body: d.Body, Expect: d.Expect})
	}
	return out
}
