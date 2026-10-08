package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/paths"
)

// The inputs a replay records name what its review read besides the PR and the notes: the judge
// skill, the roles' prompts and the role config. Any of them changed is another baseline.
func TestEvalInputsChangeWithTheSkillAPromptOrARole(t *testing.T) {
	f := newInspFixture(t)
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	cfg := f.Ctx.Config
	base := evalInputs(cfg)
	if len(base) != 12 || evalInputs(cfg) != base {
		t.Fatalf("inputs %q, then %q: want one 12-hex hash", base, evalInputs(cfg))
	}
	seen := map[string]string{base: "the start"}
	changed := func(what string) {
		t.Helper()
		got := evalInputs(cfg)
		if prev, ok := seen[got]; ok {
			t.Fatalf("after %s the inputs are %s, as after %s", what, got, prev)
		}
		seen[got] = what
	}
	writeEvalFile(t, f.Ctx.Layout.Skill(), "# Review skill\n")
	changed("writing the skill")
	judge := cfg.JudgeFor(nil)
	writeEvalFile(t, filepath.Join(cfg.Pipeline.PromptsDir, judge.PromptFile(config.PromptInitial)), "Review {{.URL}}.\n")
	changed("writing the judge's prompt")
	cfg.Roles[0].Effort = "low"
	changed("changing a role's effort")
}

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
	inputs := evalInputs(f.Ctx.Config)
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

// A case records the hashes of the notes its judge reads, from the scratch copy, and the Codex
// gauge as the account's newest session reports it.
func TestEvalRecordsTheNotesAndTheCodexGauge(t *testing.T) {
	scratch := paths.Layout{Home: t.TempDir(), Scratch: t.TempDir()}
	writeEvalFile(t, engine.NotesPath(scratch, "example", "web"), "# Notes")
	writeEvalFile(t, filepath.Join(engine.NotesDir(scratch, "example", "web"), "qa.sh"), "echo qa")
	got := evalNotesHashes(scratch, "example", "web")
	if len(got) != 2 || got["web.md"] == "" || got["web/qa.sh"] == "" {
		t.Fatalf("notes hashes %v", got)
	}

	cfg := &config.Config{}
	cfg.Usage.CodexHome = t.TempDir()
	if p := evalCodexUsed(context.Background(), cfg); p != nil {
		t.Fatalf("no sessions: gauge %v", *p)
	}
	writeEvalFile(t, filepath.Join(cfg.Usage.CodexHome, "sessions", "2026", "10", "08", "rollout-2026-10-08T10-00-00-x.jsonl"),
		`{"timestamp":"2026-10-08T07:00:00.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":`+
			`{"primary":{"used_percent":31.0,"window_minutes":10080,"resets_at":4102444800},"secondary":null}}}`+"\n")
	if p := evalCodexUsed(context.Background(), cfg); p == nil || *p != 31 {
		t.Fatalf("gauge %v, want 31", p)
	}
}
