package postreview

import (
	"strings"
	"testing"
)

// Every rule of the review file, each case named: the judge gets status
// invalid (exit 2) naming the field, and nothing reaches GitHub.
func TestParseRefusesEachInvalidReview(t *testing.T) {
	long := strings.Repeat("x", MaxBody+1)
	nearLimit := strings.Repeat("x", MaxBody-10) // the marker pushes it over
	cases := []struct {
		name, file, want string
	}{
		{"not JSON", `event: COMMENT`, "not a review object"},
		{"two JSON values", review("COMMENT", "Fix 1 problem.") + `{}`, "more than one JSON value"},
		{"unknown field", `{"event":"COMMENT","body":"x","commit_id":"abc"}`, `unknown field "commit_id"`},
		{"unknown comment field", review("COMMENT", "x", `{"path":"app/x.rb","position":3,"body":"x"}`), `unknown field "position"`},
		{"unknown event", review("LGTM", "x"), `event "LGTM" is not COMMENT, REQUEST_CHANGES or APPROVE`},
		{"lowercase event", review("comment", "x"), `event "comment"`},
		{"empty body", review("COMMENT", "  \n"), "body is empty"},
		{"footer in body", review("COMMENT", "No problems found.\n\n<!-- magnum:footer -->\nAbout"), "magnum appends the footer"},
		{"body too long", review("COMMENT", long), "GitHub takes at most 65536"},
		{"body too long with the marker", review("COMMENT", nearLimit), "the run marker included"},
		{"comment without path", review("COMMENT", "x", comment("", 3, "")), "comments[0]: path is empty"},
		{"comment without line", review("COMMENT", "x", `{"path":"app/x.rb","body":"x"}`), "comments[0]: line 0 is not a line number"},
		{"comment with empty body", review("COMMENT", "x", `{"path":"app/x.rb","line":3,"body":""}`), "comments[0]: body is empty"},
		{"comment body too long", review("COMMENT", "x", `{"path":"app/x.rb","line":3,"body":"`+long+`"}`), "comments[0]: body is 65537 characters"},
		{"unknown side", review("COMMENT", "x", comment("app/x.rb", 3, `"side":"BOTH"`)), `comments[0]: side "BOTH" is not RIGHT or LEFT`},
		{"start_side alone", review("COMMENT", "x", comment("app/x.rb", 3, `"start_side":"RIGHT"`)), "comments[0]: start_side without start_line"},
		{"start_line after line", review("COMMENT", "x", comment("app/x.rb", 3, `"start_line":5`)), "comments[0]: start_line 5 is not before line 3"},
		{"start_line equal to line", review("COMMENT", "x", comment("app/x.rb", 3, `"start_line":3`)), "start_line 3 is not before line 3"},
		{"negative start_line", review("COMMENT", "x", comment("app/x.rb", 3, `"start_line":-1`)), "start_line -1 is not a line number"},
		{"sides differ", review("COMMENT", "x", comment("app/x.rb", 14, `"side":"LEFT","start_line":12,"start_side":"RIGHT"`)),
			`comments[0]: start_side "RIGHT" differs from side "LEFT"`},
		{"second comment named", review("COMMENT", "x", comment("app/x.rb", 3, ""), comment("app/x.rb", 0, "")), "comments[1]: line 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, problems := Parse([]byte(tc.file), opts())
			if !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Errorf("problems = %q, want one containing %q", problems, tc.want)
			}
			w := newWorld(t)
			out := w.run(opts(), tc.file)
			if out.Status != StatusInvalid || out.ExitCode() != 2 || len(w.fake.Calls) != 0 {
				t.Errorf("Run: status %s exit %d after %d calls, want invalid, 2, none", out.Status, out.ExitCode(), len(w.fake.Calls))
			}
		})
	}
}

// The run marker is appended as the body's last line when the body lacks
// it, and only then: a rerun with the marker in place leaves the body as is.
func TestParseAppendsTheRunMarkerOnce(t *testing.T) {
	marker := "<!-- magnum:run=" + runID + " head=d4e5f6a -->"
	r, appended, problems := Parse([]byte(review("COMMENT", "Fix 1 problem before merging.\n")), opts())
	if len(problems) > 0 || !appended || r.Body != "Fix 1 problem before merging.\n\n"+marker {
		t.Fatalf("body %q appended %v problems %q", r.Body, appended, problems)
	}
	again, appended, _ := Parse([]byte(review("COMMENT", r.Body)), opts())
	if appended || again.Body != r.Body || strings.Count(again.Body, "magnum:run=") != 1 {
		t.Fatalf("second pass: body %q appended %v", again.Body, appended)
	}
	// Another run's id that merely starts with this one's is not the marker.
	longer := review("COMMENT", "x\n<!-- magnum:run="+runID+"0 head=d4e5f6a -->")
	if _, appended, _ := Parse([]byte(longer), opts()); !appended {
		t.Error("the marker of run …70 was taken for run …7")
	}
}

// Sides default to RIGHT and a multi-line comment's start side to its side,
// so the request names both.
func TestParseFillsInSides(t *testing.T) {
	r, _, problems := Parse([]byte(review("COMMENT", "x", comment("app/x.rb", 14, `"start_line":12`), comment("app/x.rb", 14, `"side":"LEFT"`))), opts())
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	if c := r.Comments[0]; c.Side != SideRight || c.StartSide != SideRight {
		t.Errorf("first comment sides = %q, %q", c.Side, c.StartSide)
	}
	if c := r.Comments[1]; c.Side != SideLeft || c.StartSide != "" {
		t.Errorf("second comment sides = %q, %q", c.Side, c.StartSide)
	}
}

func TestParsePatchReadsHunksOnBothSides(t *testing.T) {
	fd, ok := parsePatch(appPatch + "\n@@ -80 +81,0 @@\n-gone")
	if !ok {
		t.Fatal("no hunk")
	}
	if got := strings.Join(fd.valid(SideRight), " "); got != "10-16 40-45" {
		t.Errorf("RIGHT = %s", got)
	}
	if got := strings.Join(fd.valid(SideLeft), " "); got != "10-15 39-43 80" {
		t.Errorf("LEFT = %s", got)
	}
	if _, ok := parsePatch("Binary files a/logo.png and b/logo.png differ"); ok {
		t.Error("a binary diff has hunks")
	}
}
