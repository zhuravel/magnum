package cli

import (
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/evalrun"
)

// The daemon's gate reads the local cases' corpus apart from the corpora `eval run` reads by default:
// its default corpora hold no local case, the local file's cases come from its own seam (nil, nil when
// there is no file, an error when it does not parse), and a local round's inputs are those with the
// local reviewer prompt, for the config and for a candidate.
func TestDaemonGateReadsTheLocalCasesCorpusApart(t *testing.T) {
	f := newInspFixture(t)
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	f.Ctx.Layout.UserConfig = filepath.Join(f.Home, "user", "config.toml") // the operator's corpus is beside it, never the real one
	a := &app.App{Config: f.Ctx.Config, Layout: f.Ctx.Layout, Logger: slog.New(slog.DiscardHandler)}
	var d engine.Deps
	daemonGate(f.Ctx, a)(&d)
	if d.EvalLocalCorpus == nil || d.EvalLocalInputs == nil || d.EvalRefused == nil {
		t.Fatal("the local seams are not wired")
	}
	if c, err := d.EvalLocalCorpus(); c != nil || err != nil {
		t.Fatalf("no local file: %+v, %v", c, err)
	}
	writeEvalFile(t, filepath.Join(f.Ctx.Layout.ConfigDir(), evalCorpusFile), "[[case]]\nname = \"geo\"\npr = \"example/web#3\"\nhead = \""+evalTestHead+
		"\"\n[[case.defect]]\nid = \"d\"\ntitle = \"t\"\nmatch = [\"x\"]\n")
	writeEvalFile(t, f.Ctx.Layout.LearnLocalCorpus(), "[[case]]\nname = \"task-1-round-1\"\nkind = \"local\"\npr = \"example/web#900011\"\nhead = \""+
		evalTestHead+"\"\n[[case.defect]]\nid = \"q\"\ntitle = \"t\"\nquestion = true\nmatch = [\"x\"]\n")
	local, err := d.EvalLocalCorpus()
	if err != nil || local == nil || len(local.Cases) != 1 || !local.Cases[0].Local() {
		t.Fatalf("the local cases: %+v, %v", local, err)
	}
	posted, _, err := d.EvalCorpus()
	if err != nil || posted == nil || slices.ContainsFunc(posted.Cases, eval.Case.Local) {
		t.Fatalf("the default corpora: %+v, %v", posted, err)
	}
	if in, err := d.EvalLocalInputs(""); err != nil || in != evalrun.LocalReviewInputs(a.Config) || in == evalrun.Inputs(a.Config) {
		t.Fatalf("local inputs %q, %v", in, err)
	}
	if _, err := d.EvalLocalInputs(t.TempDir()); err == nil {
		t.Fatal("a directory that is no candidate hashed")
	}
	writeEvalFile(t, f.Ctx.Layout.LearnLocalCorpus(), "[[case]]\nname = \"task-1-round-1\"\nkind = \"local\"\nbogus = 1\n")
	if _, err := d.EvalLocalCorpus(); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("a broken local file: %v", err)
	}
}
