package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/eval"
)

var evalTestHead = strings.Repeat("ab", 20)

func writeEvalFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// `eval score` on a saved replay whose review body names the defect only in its `Description: ✗`
// line and its Checks block scores it missed, which the replay's first score did not.
func TestEvalScoreOfASavedReplayLeavesTheDescriptionLineOut(t *testing.T) {
	f := newInspFixture(t)
	corpus := filepath.Join(t.TempDir(), "eval.toml")
	writeEvalFile(t, corpus, "[[case]]\nname = \"geo-segment-client\"\npr = \"example/web#3\"\nhead = \""+evalTestHead+"\"\n"+
		"[[case.defect]]\nid = \"two-segments\"\ntitle = \"Domain fallback before geo puts one visitor in two segments\"\n"+
		"match = [\"domain.{0,80}(geo|fallback)\"]\nbody = true\n\n"+
		"[[case.defect]]\nid = \"country-tests\"\ntitle = \"Country reporting is untested\"\npaths = [\"src/clients/*.js\"]\n"+
		"match = [\"regression tests\"]\n")
	result := filepath.Join(t.TempDir(), "codex-judge.json")
	writeEvalFile(t, result, `{"status":"dry_run","planned_review":{"event":"COMMENT",`+
		`"body":"Fix 1 problem before merging.\n\n- [P2] Country reporting has no production-client regression tests.\n\n`+
		`Description: ✗ Affiliates also use domain fallback. Nested authentication still checks data.email.\n\n`+
		`<details><summary>Found nearby, not this PR's (1)</summary>\n\n- src/clients/b.js:43: [P2] A missing cookie decodes null.\n\n</details>\n\n`+
		`<details><summary>Checks (1 run)</summary>\n\n- node probe.js (domain fallback before geo): 176 assertions passed.\n\n</details>\n",`+
		`"comments":[{"path":"src/clients/a.js","line":10,"body":"**P2** Country reporting has no production-client regression tests."}]}}`)
	first := eval.Score{Case: "geo-segment-client", Found: 2, Total: 2, Defects: []eval.DefectResult{
		{ID: "two-segments", Found: true, By: []int{1}, SeverityOK: true}, {ID: "country-tests", Found: true, By: []int{0}, Severity: "P2", SeverityOK: true}}}
	dir := filepath.Join(evalRunsRoot(f.Ctx.Layout), "20261007-163947")
	if err := eval.SaveRun(dir, eval.Run{ID: "20261007-163947", Corpus: corpus, Cases: []eval.CaseRun{{
		Case: "geo-segment-client", PR: "example/web#3", Head: evalTestHead, Outcome: "dry_run", ResultFile: result, Score: &first}}}); err != nil {
		t.Fatal(err)
	}
	if code := execute(f.Ctx, []string{"eval", "score", "20261007-163947"}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	if out := f.Out.String(); !strings.Contains(out, "1/2 (50%)") || !strings.Contains(out, "missed: geo-segment-client/two-segments") {
		t.Fatalf("output:\n%s", out)
	}
	run, err := eval.LoadRun(dir)
	if err != nil || run.Cases[0].Score.Found != 1 || run.Cases[0].Score.Defects[0].Found {
		t.Fatalf("saved run %+v, %v: want the description line's defect missed", run.Cases[0].Score, err)
	}
}

// `eval run --corpus FILE` reads that file alone, and `eval score` of the run reads it alone again:
// the learned corpus is not merged in, so a case name the file shares with it is no error, and a run
// given the learned corpus itself is scored on it once. A run recorded before it said so, whose corpus
// is the learned file, is scored on that file once too. Every case is on a flagged PR, so nothing
// replays.
func TestEvalScoreRereadsAnExplicitCorpusAlone(t *testing.T) {
	f := newInspFixture(t)
	learned := f.Ctx.Layout.LearnCorpus()
	if err := eval.WriteCorpus(learned, eval.Corpus{Cases: []eval.Case{{Name: "decline-7", PR: "example/gadgets#5", Head: evalTestHead,
		Defects: []eval.Defect{{ID: "decline-7", Title: "t", Expect: eval.ExpectAbsent, Paths: []string{"a.rb"}}}}}}); err != nil {
		t.Fatal(err)
	}
	operators := filepath.Join(t.TempDir(), "eval.toml") // a case of the operator's that the learned corpus also names
	writeEvalFile(t, operators, "[[case]]\nname = \"decline-7\"\npr = \"example/widgets#3\"\nhead = \""+evalTestHead+"\"\n\n"+
		"[[case.defect]]\nid = \"d\"\ntitle = \"t\"\npaths = [\"a.rb\"]\nmatch = [\"x\"]\n")
	for _, pr := range []string{"example/widgets#3", "example/gadgets#5"} {
		if err := eval.MarkFlagged(evalRunsRoot(f.Ctx.Layout), eval.Flagged{Case: "was-" + strings.NewReplacer("/", "-", "#", "-").Replace(pr), PR: pr, Run: "20261007-134000", Reason: "Codex refused"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, corpus := range []string{learned, operators} {
		f.Out.Reset()
		if code := execute(f.Ctx, []string{"eval", "run", "--corpus", corpus}); code != 0 {
			t.Fatalf("eval run --corpus %s: exit %d; stderr %s", corpus, code, f.Err)
		}
		runs, err := eval.ListRuns(evalRunsRoot(f.Ctx.Layout))
		if err != nil || len(runs) == 0 {
			t.Fatalf("runs = %+v, %v", runs, err)
		}
		run := runs[len(runs)-1]
		if run.Corpus != corpus || !run.CorpusAlone {
			t.Fatalf("eval run --corpus %s recorded corpus %q, alone %v; want the file alone", corpus, run.Corpus, run.CorpusAlone)
		}
		if code := execute(f.Ctx, []string{"eval", "score", run.ID}); code != 0 {
			t.Fatalf("eval score of the run given %s: exit %d; stderr %s", corpus, code, f.Err)
		}
	}
	dir := filepath.Join(evalRunsRoot(f.Ctx.Layout), "20261008-090000") // before runs recorded corpus_alone
	if err := eval.SaveRun(dir, eval.Run{ID: "20261008-090000", Corpus: learned, Cases: []eval.CaseRun{{
		Case: "decline-7", PR: "example/gadgets#5", Head: evalTestHead, Outcome: evalFlagged}}}); err != nil {
		t.Fatal(err)
	}
	if code := execute(f.Ctx, []string{"eval", "score", "20261008-090000"}); code != 0 {
		t.Fatalf("eval score of an older run given the learned corpus: exit %d; stderr %s", code, f.Err)
	}
}
