package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// evalFlaggedCorpus is a corpus of one case that an earlier replay's refusal
// flagged, so `eval run` records the run without replaying anything.
func evalFlaggedCorpus(t *testing.T, f *inspFixture) string {
	t.Helper()
	if err := eval.MarkFlagged(evalRunsRoot(f.Ctx.Layout), eval.Flagged{Case: "oauth-session", PR: "example/widgets#3", Run: "20261007-134000",
		Reason: "Codex refused the review"}); err != nil {
		t.Fatal(err)
	}
	return evalCorpusOf(t, "oauth-session", "example/widgets#3")
}

// `eval run` records magnum's own commit and whether the checkout has
// uncommitted changes through the eval seam's runner, never a git binary of
// its own: two eval CLI tests ran the real git in their temp home.
func TestEvalRunReadsMagnumsCommitThroughItsRunner(t *testing.T) {
	storetest.Serial(t)
	f := newInspFixture(t)
	home := f.Ctx.Layout.Home
	f.Runner.Rules = []execx.Rule{
		{Prefix: []string{"git", "-C", home, "rev-parse"}, Result: execx.Result{Stdout: []byte(strings.Repeat("ab", 20) + "\n")}},
		{Prefix: []string{"git", "-C", home, "status"}, Result: execx.Result{Stdout: []byte(" M prompts/judge-own-pass.md\x00")}},
	}
	if code := execute(f.Ctx, []string{"eval", "run", "--corpus", evalFlaggedCorpus(t, f)}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	runs, err := eval.ListRuns(evalRunsRoot(f.Ctx.Layout))
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %+v, %v", runs, err)
	}
	if runs[0].Commit != "abababa" || !runs[0].Dirty {
		t.Fatalf("commit %q dirty %v", runs[0].Commit, runs[0].Dirty)
	}
	if n := len(f.Runner.CallsWithPrefix("git")); n != 2 {
		t.Fatalf("git ran %d times through the fake: %+v", n, f.Runner.Calls)
	}
}

// Run ids had one-second resolution: two runs started in the same second
// shared a run directory, so the second overwrote the first's record, and
// an agent tag, so its agents could adopt the first's. A run that finds its
// id taken takes the next free one ("<id>-2"); the two runs keep two
// records with different tags, and `eval show <id>` names the first exactly.
func TestTwoEvalRunsInOneSecondKeepTwoRecordsAndTags(t *testing.T) {
	storetest.Serial(t)
	f := newInspFixture(t)
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	evalSys.Now = func() time.Time { return at }
	corpus := evalFlaggedCorpus(t, f)
	for range 2 {
		if code := execute(f.Ctx, []string{"eval", "run", "--corpus", corpus}); code != 0 {
			t.Fatalf("exit %d; stderr %s", code, f.Err)
		}
	}
	runs, err := eval.ListRuns(evalRunsRoot(f.Ctx.Layout))
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs %+v, %v", runs, err)
	}
	first, second := runs[0].ID, runs[1].ID
	if first != "20261009-120000" || second != "20261009-120000-2" {
		t.Fatalf("run ids %q and %q", first, second)
	}
	if evalAgentTag(first) == evalAgentTag(second) {
		t.Fatalf("runs %s and %s share the agent tag %s", first, second, evalAgentTag(first))
	}
	for _, r := range runs {
		if len(r.Cases) != 1 || r.Cases[0].Outcome != evalFlagged || !r.Started.Equal(at) {
			t.Fatalf("run %s: %+v", r.ID, r)
		}
	}
	f.Out.Reset()
	if code := execute(f.Ctx, []string{"eval", "show", first}); code != 0 {
		t.Fatalf("eval show %s: exit %d; stderr %s", first, code, f.Err)
	}
}
