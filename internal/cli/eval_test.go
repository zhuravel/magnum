package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
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

// A case whose replay was refused before (Codex flagged it) is never
// replayed: `eval run` refuses it with the reason and runs nothing for it.
func TestEvalRunRefusesAFlaggedCase(t *testing.T) {
	f := newInspFixture(t)
	corpus := filepath.Join(t.TempDir(), "eval.toml")
	if err := os.WriteFile(corpus, []byte("[[case]]\nname = \"oauth-session\"\npr = \"example/widgets#3\"\nhead = \""+
		strings.Repeat("ab", 20)+"\"\n\n[[case.defect]]\nid = \"d\"\ntitle = \"t\"\npaths = [\"a.rb\"]\nmatch = [\"x\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := eval.MarkFlagged(evalRunsRoot(f.Ctx.Layout), eval.Flagged{Case: "oauth-session", PR: "example/widgets#3", Run: "20261007-134000",
		Reason: "Codex refused the review: content flagged as a cybersecurity risk (codex-judge, run r-1)"}); err != nil {
		t.Fatal(err)
	}
	if code := execute(f.Ctx, []string{"eval", "run", "--corpus", corpus}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	out := f.Out.String()
	if !strings.Contains(out, "oauth-session example/widgets#3 refused: flagged in run 20261007-134000") || strings.Contains(out, "[[watch]]") {
		t.Fatalf("output:\n%s", out)
	}
	runs, err := eval.ListRuns(evalRunsRoot(f.Ctx.Layout))
	if err != nil || len(runs) != 1 || len(runs[0].Cases) != 1 || runs[0].Cases[0].Outcome != evalFlagged {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
}

// evalCorpusOf writes a corpus of one case, name on pr, and returns its path.
func evalCorpusOf(t *testing.T, name, pr string) string {
	t.Helper()
	corpus := filepath.Join(t.TempDir(), "eval.toml")
	if err := os.WriteFile(corpus, []byte("[[case]]\nname = \""+name+"\"\npr = \""+pr+"\"\nhead = \""+
		strings.Repeat("ab", 20)+"\"\n\n[[case.defect]]\nid = \"d\"\ntitle = \"t\"\npaths = [\"a.rb\"]\nmatch = [\"x\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return corpus
}

// Only a '#' in the corpus kept the flagged talkable#11999 out of a replay:
// `eval run` read flagged.json alone, by case name. A case whose PR the live
// registry holds a Codex flag for is refused whatever the case is called,
// with the flag's sentence, and so is one whose PR flagged.json names under
// another case name. Nothing runs for either.
func TestEvalRunRefusesACaseWhosePRIsFlagged(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	_, pr := inspSeedPR(t, st, "example/widgets", 3, store.PRIneligible, nil)
	flagPR(t, st, pr.ID, time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC))
	if code := execute(f.Ctx, []string{"eval", "run", "--corpus", evalCorpusOf(t, "renamed-case", "example/widgets#3")}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	if out := f.Out.String(); !strings.Contains(out, "renamed-case example/widgets#3 refused: Codex flagged this PR") ||
		strings.Contains(out, "[[watch]]") {
		t.Fatalf("output:\n%s", out)
	}
	runs, err := eval.ListRuns(evalRunsRoot(f.Ctx.Layout))
	if err != nil || len(runs) != 1 || len(runs[0].Cases) != 1 || runs[0].Cases[0].Outcome != evalFlagged {
		t.Fatalf("runs = %+v, %v", runs, err)
	}

	f.Out.Reset()
	if err := eval.MarkFlagged(evalRunsRoot(f.Ctx.Layout), eval.Flagged{Case: "oauth-session", PR: "example/gadgets#5", Run: "20261007-134000",
		Reason: "Codex refused the review"}); err != nil {
		t.Fatal(err)
	}
	if code := execute(f.Ctx, []string{"eval", "run", "--corpus", evalCorpusOf(t, "session-renamed", "example/gadgets#5")}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	if out := f.Out.String(); !strings.Contains(out, "session-renamed example/gadgets#5 refused: flagged in run 20261007-134000") ||
		strings.Contains(out, "[[watch]]") {
		t.Fatalf("output:\n%s", out)
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

// Each run tags its agents with a tag of its own, so no role's agent name
// is that of another run (StartAgent adopts an agent carrying the role's
// name: on 2026-10-08 a run adopted the agents the run before it left
// behind) or of the PR's own agents; one run's tag stays the same for all
// its cases.
func TestEvalRunsTagTheirAgentsApart(t *testing.T) {
	runs := []string{"20261008-204959", "20261008-214426", "20261009-204959"}
	tags := map[string]string{}
	for _, id := range runs {
		tag := evalAgentTag(id)
		if tag != evalAgentTag(id) || !strings.HasPrefix(tag, "eval-") || len(tag) > len("eval-")+6 {
			t.Fatalf("run %s: tag %q", id, tag)
		}
		if other, ok := tags[tag]; ok {
			t.Fatalf("runs %s and %s share the tag %q", other, id, tag)
		}
		tags[tag] = id
	}
	for _, role := range []agents.Role{agents.RoleJudge, agents.RoleClaude, agents.RoleSimplify} {
		seen := map[string]string{agents.AgentName("talkable/talkable", 11920, role): "the PR's own"}
		for _, id := range runs {
			name := agents.TaggedAgentName(evalAgentTag(id), "talkable/talkable", 11920, role)
			if other, ok := seen[name]; ok {
				t.Fatalf("%s: run %s's agent name %s is %s's too", role, id, name, other)
			}
			seen[name] = "run " + id
		}
	}
}
