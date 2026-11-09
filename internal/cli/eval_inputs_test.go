package cli

import (
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/evalrun"
)

// `eval baseline` names, per case, the stored replays at the corpus's head whose inputs are the
// ones a run would have now, newest first, and `run` for a case none covers. It writes nothing.
func TestEvalBaselineNamesTheStoredReplaysWithTheSameInputs(t *testing.T) {
	f := newInspFixture(t)
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	corpus := filepath.Join(t.TempDir(), "eval.toml")
	writeEvalFile(t, corpus, "[[case]]\nname = \"geo\"\npr = \"example/web#3\"\nhead = \""+evalTestHead+"\"\n"+
		"[[case.defect]]\nid = \"d\"\ntitle = \"t\"\nmatch = [\"x\"]\n\n"+
		"[[case]]\nname = \"copy\"\npr = \"example/web#4\"\nhead = \""+evalTestHead+"\"\n"+
		"[[case.defect]]\nid = \"d\"\ntitle = \"t\"\nmatch = [\"x\"]\n")
	inputs := evalrun.Inputs(f.Ctx.Config)
	root := evalRunsRoot(f.Ctx.Layout)
	geo := func(found int) []eval.CaseRun {
		return []eval.CaseRun{{Case: "geo", Head: evalTestHead, Outcome: "dry_run", Score: &eval.Score{Found: found, Total: 2}}}
	}
	for _, r := range []eval.Run{
		{ID: "20261006-100000", Inputs: inputs, Commit: "abc1234", Cases: geo(1)},
		{ID: "20261007-100000", Inputs: "0123456789ab", Commit: "def5678", Cases: geo(2)},
		{ID: "20261007-110000", Inputs: inputs, Commit: "fed4321", Cases: geo(2)},
	} {
		if err := eval.SaveRun(filepath.Join(root, r.ID), r); err != nil {
			t.Fatal(err)
		}
	}
	if code := execute(f.Ctx, []string{"eval", "baseline", "--corpus", corpus}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	want := "inputs\t" + inputs + "\n" +
		"reuse\tgeo\t20261007-110000\tfed4321\t2/2\n" +
		"reuse\tgeo\t20261006-100000\tabc1234\t1/2\n" +
		"run\tcopy\n"
	if got := f.Out.String(); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
	if runs, err := eval.ListRuns(root); err != nil || len(runs) != 3 {
		t.Fatalf("runs after eval baseline: %d, %v; want the 3 stored ones", len(runs), err)
	}
}

// A local case's stored replays are reused on the inputs of its round (evalrun.LocalReviewInputs, the
// local reviewer prompt in the posted one's place), named on a line of their own when the corpus has
// one; a replay of it on the run's inputs alone (from before its round's were recorded) is none.
func TestEvalBaselineMatchesALocalCaseOnItsRoundsInputs(t *testing.T) {
	f := newInspFixture(t)
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	corpus := filepath.Join(t.TempDir(), "eval.toml")
	writeEvalFile(t, corpus, "[[case]]\nname = \"task-1-round-1\"\nkind = \"local\"\npr = \"example/web#900011\"\nhead = \""+evalTestHead+"\"\n"+
		"[[case.defect]]\nid = \"d\"\ntitle = \"t\"\nquestion = true\nmatch = [\"x\"]\n")
	inputs, local := evalrun.Inputs(f.Ctx.Config), evalrun.LocalReviewInputs(f.Ctx.Config)
	root := evalRunsRoot(f.Ctx.Layout)
	lc := func(in string) []eval.CaseRun {
		return []eval.CaseRun{{Case: "task-1-round-1", Kind: eval.KindLocal, Head: evalTestHead, Outcome: "dry_run", Score: &eval.Score{Found: 1, Total: 1}, Inputs: in}}
	}
	for _, r := range []eval.Run{
		{ID: "20261006-100000", Inputs: inputs, Commit: "abc1234", Cases: lc("")},
		{ID: "20261007-100000", Inputs: inputs, Commit: "def5678", Cases: lc(local)},
	} {
		if err := eval.SaveRun(filepath.Join(root, r.ID), r); err != nil {
			t.Fatal(err)
		}
	}
	if code := execute(f.Ctx, []string{"eval", "baseline", "--corpus", corpus}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	want := "inputs\t" + inputs + "\nlocal-inputs\t" + local + "\nreuse\ttask-1-round-1\t20261007-100000\tdef5678\t1/1\n"
	if got := f.Out.String(); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
}
