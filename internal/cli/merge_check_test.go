package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/mergecheck"
	"github.com/zhuravel/magnum/internal/store"
)

func TestMergeCheckArgsNameAMergedPROrACommit(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]struct {
		pr  int
		sha string
	}{
		"#11939":   {pr: 11939},
		"11939":    {pr: 11939},
		" ABCDEF1": {sha: "abcdef1"},
		"0123456789abcdef0123456789abcdef01234567": {sha: "0123456789abcdef0123456789abcdef01234567"},
	} {
		pr, sha, err := parseMergedArg(in)
		if err != nil || pr != want.pr || sha != want.sha {
			t.Errorf("parseMergedArg(%q) = %d, %q, %v; want %d, %q", in, pr, sha, err, want.pr, want.sha)
		}
	}
	for _, in := range []string{"#", "#x", "0", "abc12", "main", "origin/master", "abcdefg", "-x"} {
		if pr, sha, err := parseMergedArg(in); err == nil {
			t.Errorf("parseMergedArg(%q) = %d, %q; want an error", in, pr, sha)
		}
	}
}

func TestMergeCheckFindsThePoolByFullNameOrName(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Pools: []config.Pool{{Repo: "talkable/talkable"}, {Repo: "example/widget"}, {Repo: "zhuravel/widget"}}}
	for in, want := range map[string]string{"talkable": "talkable/talkable", "Talkable": "talkable/talkable", "example/widget": "example/widget"} {
		if p, err := mergeCheckPool(cfg, in); err != nil || p.Repo != want {
			t.Errorf("mergeCheckPool(%q) = %q, %v; want %s", in, p.Repo, err, want)
		}
	}
	for _, in := range []string{"widget", "example/none", "none"} {
		if p, err := mergeCheckPool(cfg, in); err == nil {
			t.Errorf("mergeCheckPool(%q) = %q; want an error", in, p.Repo)
		}
	}
}

// A Codex-flagged PR gets no merge-check, with the reason `magnum review`
// gives, before any slot is held or command run.
func TestMergeCheckRefusesACodexFlaggedPR(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	_, pr := inspSeedPR(t, st, "talkable/talkable", 5, store.PRIneligible, nil)
	flagPR(t, st, pr.ID, time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC))
	if code := f.run("merge-check", "talkable", "--merged", "abc1234", "--pr", "5"); code != 1 {
		t.Fatalf("exit %d: %s", code, f.Err.String())
	}
	if msg := f.Err.String(); !strings.Contains(msg, "talkable#5: Codex flagged this PR as a possible cybersecurity risk") ||
		!strings.Contains(msg, "magnum codex-flag clear talkable#5") {
		t.Fatalf("stderr = %q", msg)
	}
	if len(f.Runner.Calls) != 0 {
		t.Fatalf("ran %d commands", len(f.Runner.Calls))
	}
	// Another PR of the repository, flagged or not in the registry, is not refused for it.
	if err := mergeCheckFlagged(context.Background(), st, "talkable/talkable", "talkable/talkable", 6); err != nil {
		t.Fatalf("PR 6: %v", err)
	}
}

func TestMergeCheckRefusesWhileTheDaemonDrains(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	if err := st.SetKV(context.Background(), engine.KVDaemonDraining, "2026-10-07T15:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if code := f.run("merge-check", "talkable", "--merged", "abc1234", "--pr", "5"); code != 1 || !strings.Contains(f.Err.String(), "draining") {
		t.Fatalf("exit %d: %s", code, f.Err.String())
	}
}

func TestMergeCheckWithoutAFreeSlotHoldsAndRunsNothing(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	if _, err := st.CreateSlot(context.Background(), store.Slot{Name: "review1", RepoFullName: "talkable/talkable", Kind: store.SlotKindPool,
		Path: f.Home + "/talkable.review1", MainClone: f.Home + "/talkable", State: store.SlotFree, Pinned: true}); err != nil {
		t.Fatal(err)
	}
	if code := f.run("merge-check", "talkable", "--merged", "abc1234", "--pr", "5"); code != 1 || !strings.Contains(f.Err.String(), "no free slot") {
		t.Fatalf("exit %d: %s", code, f.Err.String())
	}
	if len(f.Runner.Calls) != 0 {
		t.Fatalf("ran %d commands", len(f.Runner.Calls))
	}
	if sl, err := st.SlotByName(context.Background(), "review1"); err != nil || sl.State != store.SlotFree || sl.HoldReason != nil {
		t.Fatalf("review1 = %+v, %v", sl, err)
	}
}

func TestMergeCheckUsage(t *testing.T) {
	f := newInspFixture(t)
	for _, args := range [][]string{
		{"talkable", "--merged", "abc1234"},
		{"talkable", "--pr", "5"},
		{"--merged", "abc1234", "--pr", "5"},
		{"talkable", "--merged", "main", "--pr", "5"},
	} {
		if code := f.run("merge-check", args...); code != 2 {
			t.Errorf("merge-check %v: exit %d, want 2 (%s)", args, code, f.Err.String())
		}
	}
}

func TestMergeCheckPrintsPRTextSafely(t *testing.T) {
	t.Parallel()
	r := mergecheck.Result{Repo: "talkable/talkable", PR: 11979, Merged: strings.Repeat("a", 40), MergedPR: 11939,
		Head: strings.Repeat("b", 40), Tree: strings.Repeat("c", 40), Slot: "review2", Verdict: mergecheck.VerdictClash,
		Specs: []string{"spec/models/a_spec.rb"},
		Failures: []mergecheck.Failure{{Kind: mergecheck.KindExample, File: "spec/models/a_spec.rb", Line: 12,
			Description: "A \x1b[31mworks\x1b[0m\tnow", AtHead: mergecheck.AtHeadPassed, Verdict: mergecheck.VerdictClash}},
		Released: true, File: "/tmp/x/result.json"}
	var b bytes.Buffer
	printMergeCheck(&b, r)
	out := b.String()
	if strings.ContainsAny(out, "\x1b") {
		t.Fatalf("an escape sequence reached the terminal: %q", out)
	}
	for _, want := range []string{"talkable/talkable#11979 merged onto aaaaaaa (#11939): clash",
		"1 failure on the merged tree (1 spec file): 1 clash, 0 already failing at the PR head",
		"PR head bbbbbbb, merged tree ccccccc, slot review2", "spec/models/a_spec.rb:12", "at head: passed",
		"review2 released", "result: /tmp/x/result.json"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
