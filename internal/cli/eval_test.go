package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/evalrun"
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

// `eval run` reads the learned corpus the daemon writes after the operator's corpus, and alone when
// the operator has none, and its start line counts the learned cases, which grow with every retro.
// An explicit --corpus is that file alone: the learned cases are not replayed with it. Every case here
// is on a flagged PR, so nothing replays.
func TestEvalRunReadsTheLearnedCorpusAfterTheOperators(t *testing.T) {
	f := newInspFixture(t)
	learned := f.Ctx.Layout.LearnCorpus()
	if err := eval.WriteCorpus(learned, eval.Corpus{Cases: []eval.Case{{Name: "decline-7", PR: "example/gadgets#5", Head: strings.Repeat("ab", 20),
		Defects: []eval.Defect{{ID: "decline-7", Title: "t", Expect: eval.ExpectAbsent, Paths: []string{"a.rb"}}}}}}); err != nil {
		t.Fatal(err)
	}
	for _, pr := range []string{"example/widgets#3", "example/gadgets#5"} {
		if err := eval.MarkFlagged(evalRunsRoot(f.Ctx.Layout), eval.Flagged{Case: "was-" + strings.NewReplacer("/", "-", "#", "-").Replace(pr), PR: pr, Run: "20261007-134000", Reason: "Codex refused"}); err != nil {
			t.Fatal(err)
		}
	}
	if code := execute(f.Ctx, []string{"eval", "run"}); code != 0 {
		t.Fatalf("learned corpus alone: exit %d; stderr %s", code, f.Err)
	}
	if out := f.Out.String(); !strings.Contains(out, "1 case(s) (1 learned)") || !strings.Contains(out, "decline-7 example/gadgets#5 refused") {
		t.Fatalf("learned corpus alone:\n%s", out)
	}
	f.Out.Reset()
	operators := evalCorpusOf(t, "oauth-session", "example/widgets#3")
	b, err := os.ReadFile(operators)
	if err != nil {
		t.Fatal(err)
	}
	writeEvalFile(t, filepath.Join(f.Ctx.Layout.Home, evalLegacyCorpusFile), string(b)) // the default corpus of this test's own
	if code := execute(f.Ctx, []string{"eval", "run"}); code != 0 {
		t.Fatalf("both corpora: exit %d; stderr %s", code, f.Err)
	}
	out := f.Out.String()
	first, second := strings.Index(out, "oauth-session example/widgets#3 refused"), strings.Index(out, "decline-7 example/gadgets#5 refused")
	if !strings.Contains(out, "2 case(s) (1 learned)") || first < 0 || second < first {
		t.Fatalf("both corpora, the operator's first:\n%s", out)
	}
	f.Out.Reset()
	if code := execute(f.Ctx, []string{"eval", "run", "--corpus", operators}); code != 0 {
		t.Fatalf("--corpus: exit %d; stderr %s", code, f.Err)
	}
	if out := f.Out.String(); !strings.Contains(out, "1 case(s), corpus") || strings.Contains(out, "learned") ||
		strings.Contains(out, "decline-7") || !strings.Contains(out, "oauth-session example/widgets#3 refused") {
		t.Fatalf("--corpus, that file alone:\n%s", out)
	}
}

// A candidate's prompts and skill are what its replay reads, so the inputs a run records are the
// candidate's: other texts are another hash, the same texts in another directory the same one (a
// baseline matches by content). `eval run --candidate` records the directory and those inputs, and
// refuses a directory without the skill before any case.
func TestEvalRunCandidateFlagChangesTheInputsHash(t *testing.T) {
	f := newInspFixture(t)
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	cfg := f.Ctx.Config
	judge := cfg.JudgeFor(nil).PromptFile(config.PromptInitial)
	cand := func(skill string) string {
		dir := t.TempDir()
		writeEvalFile(t, filepath.Join(dir, "prompts", judge), "Review {{.URL}} as the candidate.\n")
		writeEvalFile(t, filepath.Join(dir, evalrun.CandidateSkill), skill)
		return dir
	}
	inputs := func(dir string) string {
		c, err := evalrun.WithCandidate(cfg, dir)
		if err != nil {
			t.Fatal(err)
		}
		return evalrun.Inputs(c)
	}
	a, b, sameAsA := cand("# Skill A\n"), cand("# Skill B\n"), cand("# Skill A\n")
	if base := evalrun.Inputs(cfg); base == inputs(a) || inputs(a) == inputs(b) || inputs(a) != inputs(sameAsA) {
		t.Fatalf("inputs: checkout %s, A %s, B %s, A elsewhere %s", base, inputs(a), inputs(b), inputs(sameAsA))
	}
	writeEvalFile(t, filepath.Join(cfg.Pipeline.PromptsDir, judge), "Review {{.URL}} as the candidate.\n")
	writeEvalFile(t, f.Ctx.Layout.Skill(), "# Skill A\n")
	if base := evalrun.Inputs(cfg); base != inputs(a) || base != inputs(f.Ctx.Layout.Home) {
		t.Fatalf("the checkout with A's texts: %s, A %s, the checkout as a candidate %s", base, inputs(a), inputs(f.Ctx.Layout.Home))
	}

	corpus := evalCorpusOf(t, "oauth-session", "example/widgets#3")
	if err := eval.MarkFlagged(evalRunsRoot(f.Ctx.Layout), eval.Flagged{Case: "oauth-session", PR: "example/widgets#3", Run: "20261007-134000",
		Reason: "Codex refused"}); err != nil {
		t.Fatal(err)
	}
	if code := execute(f.Ctx, []string{"eval", "run", "--corpus", corpus, "--candidate", a}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	runs, err := eval.ListRuns(evalRunsRoot(f.Ctx.Layout))
	if err != nil || len(runs) != 1 || runs[0].Candidate != a || runs[0].Inputs != inputs(a) || runs[0].ReviewerInputs != evalrun.ReviewerInputs(cfg) {
		t.Fatalf("runs %+v, %v; want the candidate %s with inputs %s and the checkout's reviewer inputs", runs, err, a, inputs(a))
	}
	f.Err.Reset()
	if code := execute(f.Ctx, []string{"eval", "run", "--corpus", corpus, "--candidate", t.TempDir()}); code != 1 ||
		!strings.Contains(f.Err.String(), "prompts") {
		t.Fatalf("a directory that is no candidate: exit %d; stderr %s", code, f.Err)
	}
}

// `eval run --judge-only` replays the judge on a full replay's reports: a case that has none says
// which run to make, and nothing starts for it.
func TestEvalRunJudgeOnlyNeedsAFullReplay(t *testing.T) {
	f := newInspFixture(t)
	if code := execute(f.Ctx, []string{"eval", "run", "--judge-only", "--corpus", evalCorpusOf(t, "geo", "zhuravel/web#3")}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	if out := f.Out.String(); !strings.Contains(out, "no full replay") || !strings.Contains(out, "magnum eval run --case geo") {
		t.Fatalf("output:\n%s", out)
	}
	runs, err := eval.ListRuns(evalRunsRoot(f.Ctx.Layout))
	if err != nil || len(runs) != 1 || runs[0].Cases[0].Outcome != "error" || runs[0].Cases[0].JudgeOnly {
		t.Fatalf("runs %+v, %v", runs, err)
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

// The daemon's skill gate replays eval cases in-process: `eval run` refuses
// while one runs (the learn.gating mark names its proposal) and replays
// nothing, so the two never compete for a case, and runs once it ended.
func TestEvalRunRefusesWhileASkillGateRuns(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	b, err := json.Marshal(engine.GateMark{Proposal: 4, Started: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, engine.KVLearnGating, string(b)); err != nil {
		t.Fatal(err)
	}
	corpus := evalCorpusOf(t, "oauth-session", "example/widgets#3")
	if code := execute(f.Ctx, []string{"eval", "run", "--corpus", corpus}); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %s", code, f.Err)
	}
	if e := f.Err.String(); !strings.Contains(e, "the skill gate of proposal 4") || !strings.Contains(e, "fix:") {
		t.Fatalf("stderr = %s, want the gate named and a fix", e)
	}
	if runs, err := eval.ListRuns(evalRunsRoot(f.Ctx.Layout)); err != nil || len(runs) != 0 {
		t.Fatalf("runs = %+v, %v, want none", runs, err)
	}
	if err := st.DeleteKV(ctx, engine.KVLearnGating); err != nil {
		t.Fatal(err)
	}
	f.Err.Reset()
	execute(f.Ctx, []string{"eval", "run", "--corpus", corpus})
	if strings.Contains(f.Err.String(), "skill gate") {
		t.Fatalf("with no gate running: %s", f.Err)
	}
	if runs, err := eval.ListRuns(evalRunsRoot(f.Ctx.Layout)); err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, %v, want the run", runs, err)
	}
}

// `magnum eval`'s usage line names every subcommand the command registers and
// no other: it listed run|score|list|show and left out baseline.
func TestEvalUsageNamesEveryRegisteredSubcommand(t *testing.T) {
	c, _, _ := bareContext(t)
	cmd := newEvalCmd(c)
	var registered []string
	for _, sub := range cmd.Commands() {
		registered = append(registered, sub.Name())
	}
	named := strings.Split(evalUsage, "|")
	slices.Sort(registered)
	slices.Sort(named)
	if len(registered) < 5 { // the walk must find the subcommands, not silently skip them
		t.Fatalf("eval registers %v", registered)
	}
	if !slices.Equal(named, registered) {
		t.Errorf("evalUsage names %v, but eval registers %v", named, registered)
	}
	if !strings.Contains(cmd.Use, evalUsage) {
		t.Errorf("eval's Use %q lacks evalUsage %q", cmd.Use, evalUsage)
	}
}
