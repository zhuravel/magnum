package eval

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sampleRun is the run of the report examples: 4 of 5 defects found, one case unscored.
func sampleRun() Run {
	return Run{
		ID: "20261004-153000", Label: "sol judge", Commit: "6824a5e", Dirty: true,
		Models:  map[string]string{"judge": "opus", "claude-review": ""},
		Corpus:  "/home/me/corpus.toml",
		Started: time.Date(2026, 10, 4, 15, 30, 0, 0, time.UTC), Finished: time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC),
		Cases: []CaseRun{
			{
				Case: "oauth-hmac", PR: "talkable/talkable#11932", Head: sha, Outcome: "dry_run",
				Score: &Score{
					Case: "oauth-hmac", Found: 1, Total: 2, Noise: 3, Simplifications: 4,
					Defects: []DefectResult{
						{ID: "hmac-skip", Title: "OAuth callback skips HMAC verification", Want: "P1", Found: true, By: []int{0}, Severity: "P1", SeverityOK: true},
						{ID: "discount-reuse", Title: "Discount code can be reused after refund", Want: "P2"},
					},
					Unmatched: []Finding{
						{Path: "app/models/user.rb", Line: 7, Body: "**P2** Missing index\nsecond line is not shown"},
						{Path: "app/models/order.rb", Body: strings.Repeat("é", 120)},
						{Body: "no path at all"},
					},
				},
			},
			{
				Case: "sandbox", PR: "example/sandbox#1", Head: sha, Outcome: "dry_run",
				Score: &Score{
					Case: "sandbox", Found: 3, Total: 3, Simplifications: 1,
					Defects: []DefectResult{
						{ID: "a", Title: "A", Found: true, Severity: "P3", SeverityOK: true},
						{ID: "b", Title: "B", Want: "P2", Found: true, Severity: "P1", SeverityOK: true},
						{ID: "c", Title: "C", Want: "P1", Found: true, Severity: "P1", SeverityOK: true},
					},
				},
			},
		},
	}
}

func TestTotals(t *testing.T) {
	scored := func(found, total, noise, simpl, sevOK int) CaseRun {
		s := &Score{Found: found, Total: total, Noise: noise, Simplifications: simpl}
		for i := range total {
			s.Defects = append(s.Defects, DefectResult{Found: i < found, SeverityOK: i < sevOK})
		}
		return CaseRun{Score: s}
	}
	tests := []struct {
		name string
		run  Run
		want Totals
	}{
		{"no cases", Run{}, Totals{}},
		{"unscored cases count only as cases", Run{Cases: []CaseRun{{Outcome: "error"}, {Outcome: "skipped"}}}, Totals{Cases: 2}},
		{"scored case with no defects has no recall", Run{Cases: []CaseRun{scored(0, 0, 2, 1, 0)}}, Totals{Cases: 1, Scored: 1, Noise: 2, Simplifications: 1}},
		{"sums over scored cases", Run{Cases: []CaseRun{scored(1, 2, 3, 4, 1), {Outcome: "error"}, scored(3, 3, 0, 1, 3)}},
			Totals{Cases: 3, Scored: 2, Found: 4, Total: 5, Noise: 3, Simplifications: 5, SeverityOK: 4, Recall: 0.8}},
		{"nothing found", Run{Cases: []CaseRun{scored(0, 4, 0, 0, 0)}}, Totals{Cases: 1, Scored: 1, Total: 4}},
		{"one third", Run{Cases: []CaseRun{scored(1, 3, 0, 0, 1)}}, Totals{Cases: 1, Scored: 1, Found: 1, Total: 3, SeverityOK: 1, Recall: 1.0 / 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.run.Totals(); got != tt.want {
				t.Errorf("Totals = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSaveRunLoadRunRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs", "20261004-153000")
	want := sampleRun()
	want.Cases[0].Error = "something"
	want.Cases[0].ResultFile = "/abs/result.json"
	want.Cases[0].ReportDir = "/abs/report"
	want.Cases[0].Started = time.Date(2026, 10, 4, 15, 31, 0, 0, time.UTC)
	want.Cases = append(want.Cases, CaseRun{Case: "broken", PR: "example/x#2", Head: sha, Outcome: "error", Error: "boom"})

	if err := SaveRun(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the run:\n got %+v\nwant %+v", got, want)
	}

	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("run dir mode = %v (%v), want 0700", st.Mode().Perm(), err)
	}
	if st, err := os.Stat(filepath.Join(dir, "run.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("run.json mode = %v (%v), want 0600", st.Mode().Perm(), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "run.json" {
		t.Errorf("run dir holds %v (%v), want only run.json: the temporary file must not stay", entries, err)
	}
}

func TestSaveRunReplacesThePreviousFile(t *testing.T) {
	dir := t.TempDir()
	first := Run{ID: "x", Label: "first"}
	second := Run{ID: "x", Label: "second", Cases: []CaseRun{{Case: "c"}}}
	for _, r := range []Run{first, second} {
		if err := SaveRun(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LoadRun(dir)
	if err != nil || got.Label != "second" || len(got.Cases) != 1 {
		t.Errorf("LoadRun = %+v, %v, want the second run", got, err)
	}
}

func TestSaveRunFailsWhenTheDirectoryCannotBeCreated(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := SaveRun(filepath.Join(blocker, "run"), Run{ID: "x"})
	if err == nil || !strings.HasPrefix(err.Error(), "eval: save run x") {
		t.Errorf("SaveRun error = %v, want an eval: save run error", err)
	}
}

func TestLoadRunErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadRun(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing run.json error = %v, want not-exist", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRun(dir); err == nil || !strings.HasPrefix(err.Error(), "eval: load run") {
		t.Errorf("corrupt run.json error = %v, want an eval: load run error", err)
	}
}

func TestLoadRunNamesARunWithoutIDAfterItsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "20260101-000000")
	if err := SaveRun(dir, Run{Label: "no id"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRun(dir)
	if err != nil || got.ID != "20260101-000000" {
		t.Errorf("LoadRun = %+v, %v, want the directory name as ID", got, err)
	}
}

func TestListRunsReturnsRunsOldestFirstAndSkipsWhatIsNotARun(t *testing.T) {
	root := t.TempDir()
	// directory names do not follow the IDs, so sorting by name would give the wrong order
	for dirName, id := range map[string]string{"z": "20261003-101500", "m": "20261005-090000", "a": "20261004-153000"} {
		if err := SaveRun(filepath.Join(root, dirName), Run{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "corrupt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "corrupt", "run.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	runs, err := ListRuns(root)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	want := []string{"20261003-101500", "20261004-153000", "20261005-090000"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("ListRuns IDs = %v, want %v", ids, want)
	}
}

func TestListRunsOfAMissingRootIsEmpty(t *testing.T) {
	runs, err := ListRuns(filepath.Join(t.TempDir(), "never-created"))
	if err != nil || len(runs) != 0 {
		t.Errorf("ListRuns = %v, %v, want none and no error", runs, err)
	}
}

// rescoreFixture is a corpus whose defect matches "hmac", and a run whose saved result talks about
// a "signature" instead: the defect was missed because of the expression, not the review.
func rescoreFixture(t *testing.T, match string) (*Corpus, Run, func(string) ([]byte, error)) {
	t.Helper()
	c, err := ParseCorpus([]byte(`[[case]]
name = "oauth-hmac"
pr = "talkable/talkable#11932"
head = "` + sha + `"
[[case.defect]]
id = "hmac-skip"
title = "OAuth callback skips HMAC verification"
severity = "P1"
match = ["` + match + `"]
`))
	if err != nil {
		t.Fatal(err)
	}
	result := `{"status":"dry_run","planned_review":{"comments":[{"path":"app/oauth.rb","line":4,"body":"**P1** The request signature is never checked"}]}}`
	files := map[string]string{"/abs/oauth-hmac/result.json": result}
	read := func(path string) ([]byte, error) {
		if s, ok := files[path]; ok {
			return []byte(s), nil
		}
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	old := ScoreCase(c.Cases[0], mustParseResult(t, result))
	run := Run{ID: "r1", Cases: []CaseRun{{Case: "oauth-hmac", PR: "talkable/talkable#11932", Outcome: "dry_run", ResultFile: "/abs/oauth-hmac/result.json", Score: &old}}}
	return c, run, read
}

func mustParseResult(t *testing.T, s string) Result {
	t.Helper()
	r, err := ParseResult([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRescorePicksUpACorrectedMatchExpression(t *testing.T) {
	_, run, read := rescoreFixture(t, "hmac")
	if run.Cases[0].Score.Found != 0 {
		t.Fatal("fixture: the original expression should miss the defect")
	}
	fixed, _, _ := rescoreFixture(t, "hmac|signature")

	got := Rescore(run, fixed, read)
	s := got.Cases[0].Score
	if s == nil || s.Found != 1 || !s.Defects[0].SeverityOK || s.Defects[0].Severity != "P1" {
		t.Fatalf("rescored score = %+v, want the defect found at P1", s)
	}
	if got.Totals().Found != 1 {
		t.Errorf("Totals.Found = %d, want 1", got.Totals().Found)
	}
	if run.Cases[0].Score.Found != 0 {
		t.Error("Rescore changed the run it was given")
	}
}

func TestRescoreKeepsWhatItCannotRescore(t *testing.T) {
	c, run, read := rescoreFixture(t, "hmac|signature")
	old := run.Cases[0].Score

	t.Run("case no longer in the corpus", func(t *testing.T) {
		other, err := ParseCorpus([]byte(`[[case]]
name = "other"
pr = "example/x#1"
head = "` + sha + `"
[[case.defect]]
id = "d"
title = "t"
match = ["x"]
`))
		if err != nil {
			t.Fatal(err)
		}
		got := Rescore(run, other, read)
		if got.Cases[0].Score != old || got.Cases[0].Error != "" {
			t.Errorf("case = %+v, want the old score and no error", got.Cases[0])
		}
	})

	t.Run("nil corpus", func(t *testing.T) {
		if got := Rescore(run, nil, read); got.Cases[0].Score != old {
			t.Errorf("score = %+v, want the old one", got.Cases[0].Score)
		}
	})

	t.Run("no result file", func(t *testing.T) {
		r := run
		r.Cases = []CaseRun{run.Cases[0]}
		r.Cases[0].ResultFile = ""
		if got := Rescore(r, c, read); got.Cases[0].Score != old || got.Cases[0].Error != "" {
			t.Errorf("case = %+v, want it untouched", got.Cases[0])
		}
	})

	t.Run("unreadable result keeps the score and says why", func(t *testing.T) {
		r := run
		r.Cases = []CaseRun{run.Cases[0]}
		r.Cases[0].ResultFile = "/abs/gone.json"
		got := Rescore(r, c, read)
		if got.Cases[0].Score != old {
			t.Errorf("score = %+v, want the old one", got.Cases[0].Score)
		}
		if !strings.HasPrefix(got.Cases[0].Error, "rescore: ") || !strings.Contains(got.Cases[0].Error, "/abs/gone.json") {
			t.Errorf("error = %q, want a rescore error naming the file", got.Cases[0].Error)
		}
	})

	t.Run("unparsable result keeps the score and says why", func(t *testing.T) {
		got := Rescore(run, c, func(string) ([]byte, error) { return []byte("{nope"), nil })
		if got.Cases[0].Score != old || !strings.HasPrefix(got.Cases[0].Error, "rescore: ") {
			t.Errorf("case = %+v, want the old score and a rescore error", got.Cases[0])
		}
	})

	t.Run("nothing to lose when there never was a score", func(t *testing.T) {
		r := run
		r.Cases = []CaseRun{{Case: "oauth-hmac", Outcome: "error", Error: "agent crashed", ResultFile: "/abs/gone.json"}}
		got := Rescore(r, c, read)
		if got.Cases[0].Score != nil || got.Cases[0].Error != "agent crashed" {
			t.Errorf("case = %+v, want the original error kept", got.Cases[0])
		}
	})
}

func TestRescoreScoresACaseThatHadNoScoreWhenItsResultIsThere(t *testing.T) {
	c, run, read := rescoreFixture(t, "hmac|signature")
	run.Cases[0].Score = nil
	got := Rescore(run, c, read)
	if got.Cases[0].Score == nil || got.Cases[0].Score.Found != 1 {
		t.Errorf("score = %+v, want the defect found", got.Cases[0].Score)
	}
}

func TestRescoreReadsFilesWhenGivenNoReader(t *testing.T) {
	c, run, _ := rescoreFixture(t, "hmac|signature")
	path := filepath.Join(t.TempDir(), "result.json")
	if err := os.WriteFile(path, []byte(`{"planned_review":{"comments":[{"path":"a.rb","line":1,"body":"signature"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run.Cases[0].ResultFile = path
	if got := Rescore(run, c, nil); got.Cases[0].Score.Found != 1 {
		t.Errorf("score = %+v, want the defect found", got.Cases[0].Score)
	}
}

func text(r Run, prev *Run) string {
	var b bytes.Buffer
	WriteText(&b, r, prev)
	return b.String()
}

func TestWriteTextGolden(t *testing.T) {
	prev := Run{ID: "20261003-101500", Cases: []CaseRun{{Score: &Score{Found: 3, Total: 5, Noise: 5}}}}
	got := text(sampleRun(), &prev)
	want := `run 20261004-153000  "sol judge"  magnum 6824a5e (dirty)
CASE        PR                       FOUND      SEV  NOISE  SIMPL  OUTCOME
oauth-hmac  talkable/talkable#11932  1/2        1/1  3      4      dry_run
sandbox     example/sandbox#1        3/3        3/3  0      1      dry_run
total                                4/5 (80%)  4/4  3      5
vs 20261003-101500: recall 60% → 80% (+20 pts), noise 5 → 3
missed: oauth-hmac/discount-reuse "Discount code can be reused after refund"
`
	if got != want {
		t.Errorf("WriteText output differs.\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteTextWithoutAPreviousRunHasNoComparison(t *testing.T) {
	got := text(sampleRun(), nil)
	if strings.Contains(got, "vs ") {
		t.Errorf("output has a comparison without a previous run:\n%s", got)
	}
	if !strings.Contains(got, "missed: oauth-hmac/discount-reuse") {
		t.Errorf("output lacks the missed defect:\n%s", got)
	}
}

func TestWriteTextComparisonShowsRegressionsAndNoChange(t *testing.T) {
	r := Run{ID: "new", Cases: []CaseRun{{Case: "c", Score: &Score{Found: 1, Total: 4, Noise: 9}}}}
	tests := []struct {
		name string
		prev Run
		want string
	}{
		{"recall fell", Run{ID: "old", Cases: []CaseRun{{Score: &Score{Found: 3, Total: 4, Noise: 2}}}}, "vs old: recall 75% → 25% (-50 pts), noise 2 → 9"},
		{"no change", Run{ID: "old", Cases: []CaseRun{{Score: &Score{Found: 1, Total: 4, Noise: 9}}}}, "vs old: recall 25% → 25% (+0 pts), noise 9 → 9"},
		{"previous run had nothing scored", Run{ID: "old"}, "vs old: recall 0% → 25% (+25 pts), noise 0 → 9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := text(r, &tt.prev); !strings.Contains(got, tt.want+"\n") {
				t.Errorf("output lacks %q:\n%s", tt.want, got)
			}
		})
	}
}

func TestWriteTextListsEveryMissedDefectOnItsOwnLine(t *testing.T) {
	r := Run{ID: "r", Cases: []CaseRun{
		{Case: "a", Score: &Score{Total: 2, Defects: []DefectResult{{ID: "one", Title: "First"}, {ID: "two", Title: `Has "quotes"`}}}},
		{Case: "b", Score: &Score{Total: 1, Defects: []DefectResult{{ID: "three", Title: "Third"}}}},
	}}
	got := text(r, nil)
	for _, want := range []string{"missed: a/one \"First\"\n", "missed: a/two \"Has \\\"quotes\\\"\"\n", "missed: b/three \"Third\"\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestWriteTextShowsCasesWithoutAScoreAndTheirErrors(t *testing.T) {
	r := Run{ID: "r", Cases: []CaseRun{
		{Case: "ok", PR: "example/a#1", Outcome: "dry_run", Score: &Score{Found: 1, Total: 1, Defects: []DefectResult{{ID: "d", Found: true, SeverityOK: true}}}},
		{Case: "usage", PR: "example/b#2", Outcome: "usage_limit", Error: "weekly limit reached\nresets Friday"},
		{Case: "skipped", PR: "example/c#3", Outcome: "skipped"},
	}}
	got := text(r, nil)
	want := `run r
CASE     PR            FOUND      SEV  NOISE  SIMPL  OUTCOME
ok       example/a#1   1/1        1/1  0      0      dry_run
usage    example/b#2   -          -    -      -      usage_limit
skipped  example/c#3   -          -    -      -      skipped
total                  1/1 (100%)  1/1  0      0
error: usage: weekly limit reached
`
	// column widths are the tabwriter's business; compare the cells
	if !sameCells(got, want) {
		t.Errorf("WriteText output differs.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// sameCells compares two outputs line by line ignoring runs of spaces.
func sameCells(a, b string) bool {
	norm := func(s string) []string {
		var out []string
		for l := range strings.Lines(s) {
			out = append(out, strings.Join(strings.Fields(l), " "))
		}
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

func TestWriteTextHeaderNamesWhatIsKnown(t *testing.T) {
	tests := []struct {
		name string
		run  Run
		want string
	}{
		{"everything", Run{ID: "r1", Label: "sol judge", Commit: "abc1234", Dirty: true}, `run r1  "sol judge"  magnum abc1234 (dirty)`},
		{"clean", Run{ID: "r1", Label: "x", Commit: "abc1234"}, `run r1  "x"  magnum abc1234`},
		{"no label", Run{ID: "r1", Commit: "abc1234"}, `run r1  magnum abc1234`},
		{"unknown commit, dirty", Run{ID: "r1", Dirty: true}, `run r1  magnum (dirty)`},
		{"nothing known", Run{ID: "r1"}, `run r1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, _, _ := strings.Cut(text(tt.run, nil), "\n")
			if first != tt.want {
				t.Errorf("header = %q, want %q", first, tt.want)
			}
		})
	}
}

func TestWriteMarkdownGolden(t *testing.T) {
	var b bytes.Buffer
	WriteMarkdown(&b, sampleRun())
	want := "# Eval run 20261004-153000\n" +
		"\n" +
		"- Label: sol judge\n" +
		"- Magnum: magnum 6824a5e (dirty)\n" +
		"- Corpus: `/home/me/corpus.toml`\n" +
		"- Models: claude-review=default, judge=opus\n" +
		"- Started: 2026-10-04T15:30:00Z\n" +
		"\n" +
		"## Summary\n" +
		"\n" +
		"| Case | PR | Found | Severity | Noise | Simplifications | Outcome |\n" +
		"|---|---|---|---|---|---|---|\n" +
		"| oauth-hmac | talkable/talkable#11932 | 1/2 | 1/1 | 3 | 4 | dry_run |\n" +
		"| sandbox | example/sandbox#1 | 3/3 | 3/3 | 0 | 1 | dry_run |\n" +
		"| **total** | | 4/5 (80%) | 4/4 | 3 | 5 | |\n" +
		"\n" +
		"## oauth-hmac\n" +
		"\n" +
		"talkable/talkable#11932 at `0123456789ab`, outcome dry_run\n" +
		"\n" +
		"- found `hmac-skip` OAuth callback skips HMAC verification (P1, wanted P1)\n" +
		"- missed `discount-reuse` Discount code can be reused after refund (wanted P2)\n" +
		"\n" +
		"Unmatched findings:\n" +
		"\n" +
		"- `app/models/user.rb:7` **P2** Missing index\n" +
		"- `app/models/order.rb` " + strings.Repeat("é", 100) + "…\n" +
		"- `(no path)` no path at all\n" +
		"\n" +
		"## sandbox\n" +
		"\n" +
		"example/sandbox#1 at `0123456789ab`, outcome dry_run\n" +
		"\n" +
		"- found `a` A (P3)\n" +
		"- found `b` B (P1, wanted P2)\n" +
		"- found `c` C (P1, wanted P1)\n"
	if got := b.String(); got != want {
		t.Errorf("WriteMarkdown output differs.\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteMarkdownDescribesSeverityShortfallsAndUnscoredCases(t *testing.T) {
	r := Run{ID: "r", Cases: []CaseRun{
		{Case: "weak", PR: "example/a#1", Outcome: "dry_run", Score: &Score{Total: 3, Found: 2, Defects: []DefectResult{
			{ID: "low", Title: "Too low", Want: "P1", Found: true, Severity: "P3"},
			{ID: "unlabelled", Title: "No label", Want: "P2", Found: true},
			{ID: "gone", Title: "Never seen"},
		}}},
		{Case: "broken", PR: "example/b#2", Outcome: "error", Error: "agent crashed\nstack trace"},
	}}
	var b bytes.Buffer
	WriteMarkdown(&b, r)
	got := b.String()
	for _, want := range []string{
		"- found `low` Too low (P3, below the wanted P1)\n",
		"- found `unlabelled` No label (no severity, below the wanted P2)\n",
		"- missed `gone` Never seen\n",
		"| broken | example/b#2 | - | - | - | - | error |\n",
		"## broken\n\nexample/b#2, outcome error\n\nError: agent crashed\n\nNo result to score.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown lacks %q:\n%s", want, got)
		}
	}
}
