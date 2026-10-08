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
