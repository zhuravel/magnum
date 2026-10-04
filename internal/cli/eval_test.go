package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/paths"
)

// Without a corpus, `eval run` says where it belongs and what to copy.
func TestEvalRunWithoutACorpusSaysHowToStart(t *testing.T) {
	f := newInspFixture(t)
	if code := execute(f.Ctx, []string{"eval", "run"}); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %s", code, f.Err)
	}
	if !strings.Contains(f.Err.String(), "eval.toml.example") || !strings.Contains(f.Err.String(), evalCorpusFile) {
		t.Fatalf("stderr lacks the fix: %s", f.Err)
	}
}

// A case of an unwatched repository fails before anything is checked out or
// started: magnum would not know its roles or identity.
func TestEvalCaseOfAnUnwatchedRepoFailsFirst(t *testing.T) {
	f := newInspFixture(t)
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	r := &evalRunner{c: f.Ctx, cfg: f.Ctx.Config, out: f.Out, dir: t.TempDir()}
	ec := eval.Case{Name: "x", PR: "example/widgets#3", Owner: "example", Repo: "widgets", Number: 3, Head: strings.Repeat("ab", 20)}
	cr := r.runCase(context.Background(), ec, eval.CaseRun{Case: "x"})
	if cr.Outcome != "error" || !strings.Contains(cr.Error, "[[watch]]") {
		t.Fatalf("case run %+v", cr)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "cases")); !os.IsNotExist(err) {
		t.Fatalf("scratch tree created for an unwatched case: %v", err)
	}
}

// The judge gets a copy of the repository notes and their harness scripts in
// the scratch tree, so its rewrite never reaches the live notes; a
// repository without notes is not an error.
func TestEvalCopiesTheNotesIntoTheScratchTree(t *testing.T) {
	home := t.TempDir()
	live, scratch := paths.Layout{Home: home}, paths.Layout{Home: home, Scratch: t.TempDir()}
	if err := evalCopyNotes(live, scratch, "talkable", "talkable"); err != nil {
		t.Fatalf("no notes: %v", err)
	}
	src, dir := engine.NotesPath(live, "talkable", "talkable"), engine.NotesDir(live, "talkable", "talkable")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{src: "# Notes", filepath.Join(dir, "sub", "qa.sh"): "echo qa"} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := evalCopyNotes(live, scratch, "talkable", "talkable"); err != nil {
		t.Fatal(err)
	}
	dst := engine.NotesPath(scratch, "talkable", "talkable")
	if !strings.HasPrefix(dst, scratch.Scratch) {
		t.Fatalf("scratch notes at %s", dst)
	}
	for p, want := range map[string]string{dst: "# Notes", filepath.Join(engine.NotesDir(scratch, "talkable", "talkable"), "sub", "qa.sh"): "echo qa"} {
		if b, err := os.ReadFile(p); err != nil || string(b) != want {
			t.Errorf("%s: %q, %v", p, b, err)
		}
	}
}

// RUN is an id or a unique prefix; no RUN is the latest; the previous run is
// the one before it.
func TestEvalFindsRunsByPrefix(t *testing.T) {
	l := paths.Layout{Home: t.TempDir()}
	root := evalRunsRoot(l)
	if _, err := evalFindRun(l, nil); err == nil || !strings.Contains(err.Error(), "magnum eval run") {
		t.Fatalf("no runs: %v", err)
	}
	for _, id := range []string{"20261003-101500", "20261004-090000", "20261004-153000"} {
		if err := eval.SaveRun(filepath.Join(root, id), eval.Run{ID: id, Started: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if r, err := evalFindRun(l, nil); err != nil || r.ID != "20261004-153000" {
		t.Fatalf("latest: %+v, %v", r.ID, err)
	}
	if r, err := evalFindRun(l, []string{"20261003"}); err != nil || r.ID != "20261003-101500" {
		t.Fatalf("prefix: %+v, %v", r.ID, err)
	}
	if _, err := evalFindRun(l, []string{"20261004"}); err == nil || !strings.Contains(err.Error(), "matches 2") {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := evalFindRun(l, []string{"1999"}); err == nil {
		t.Fatal("unknown run found")
	}
	if p := evalPrevious(l, "20261004-153000"); p == nil || p.ID != "20261004-090000" {
		t.Fatalf("previous: %+v", p)
	}
	if p := evalPrevious(l, "20261003-101500"); p != nil {
		t.Fatalf("previous of the first: %+v", p)
	}
}
