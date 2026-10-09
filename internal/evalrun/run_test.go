package evalrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

func widgetsCase() eval.Case {
	return eval.Case{Name: "x", PR: "example/widgets#3", Owner: "example", Repo: "widgets", Number: 3, Head: strings.Repeat("ab", 20)}
}

// A case of an unwatched repository fails before anything is checked out or started: magnum would
// not know its roles or identity. So does a candidate that is no candidate.
func TestEvalCaseOfAnUnwatchedRepoFailsFirst(t *testing.T) {
	d := Deps{Layout: paths.Layout{Home: t.TempDir()}, Config: testConfig(t)}
	o := Options{RunDir: t.TempDir()}
	cr := RunCase(context.Background(), d, widgetsCase(), o)
	if cr.Outcome != OutcomeError || !strings.Contains(cr.Error, "[[watch]]") || cr.Case != "x" || cr.Started.IsZero() {
		t.Fatalf("case run %+v", cr)
	}
	o.Candidate = t.TempDir()
	if cr := RunCase(context.Background(), d, widgetsCase(), o); cr.Outcome != OutcomeError || !strings.Contains(cr.Error, "prompts") {
		t.Fatalf("no candidate: %+v", cr)
	}
	if _, err := os.Stat(filepath.Join(o.RunDir, "cases")); !os.IsNotExist(err) {
		t.Fatalf("scratch tree created for an unwatched case: %v", err)
	}
}

// A case whose PR the live registry holds a Codex flag for is refused, with the flag's sentence,
// whether the registry is the daemon's open store or the file read-only; so is one an earlier
// replay was refused on.
func TestACaseWhosePRIsFlaggedIsRefused(t *testing.T) {
	l := paths.Layout{Home: t.TempDir()}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, l.DB())
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_widgets", Owner: "example", Name: "widgets", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	up, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "PR_3", Number: 3, URL: "https://github.com/example/widgets/pull/3",
		HeadSHA: strings.Repeat("ab", 20), GHState: store.GHOpen, InitialState: store.PRIneligible, Identity: "zhuravel"})
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Layout: l, Config: testConfig(t)}
	if why := Refused(ctx, d, widgetsCase()); why != "" {
		t.Fatalf("no flag yet: %q", why)
	}
	b, _ := json.Marshal(engine.CodexFlag{Kind: "codex", Role: "codex-judge", Run: "r-1", At: time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC)})
	if err := st.SetKV(ctx, store.KVPRCodexFlag(up.PR.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]Deps{"the file": d, "the store": {Layout: l, Config: d.Config, Store: st}} {
		if why := Refused(ctx, d, widgetsCase()); !strings.Contains(why, "Codex flagged this PR") {
			t.Errorf("%s: %q, want the flag's sentence", name, why)
		}
		if cr := RunCase(ctx, d, widgetsCase(), Options{RunDir: t.TempDir()}); cr.Outcome != OutcomeFlagged {
			t.Errorf("%s: RunCase %+v", name, cr)
		}
	}
	other := eval.Case{Name: "y", PR: "example/gadgets#5", Owner: "example", Repo: "gadgets", Number: 5, Head: strings.Repeat("cd", 20)}
	if err := eval.MarkFlagged(RunsRoot(l), eval.Flagged{Case: "renamed", PR: other.PR, Run: "20261007-134000", Reason: "Codex refused"}); err != nil {
		t.Fatal(err)
	}
	if why := Refused(ctx, d, other); !strings.Contains(why, "flagged in run 20261007-134000") {
		t.Errorf("a PR an earlier replay was refused on: %q", why)
	}
}

// The judge gets a copy of the repository notes and their harness scripts in the scratch tree, so
// its rewrite never reaches the live notes; a repository without notes is not an error.
func TestEvalCopiesTheNotesIntoTheScratchTree(t *testing.T) {
	home := t.TempDir()
	live, scratch := paths.Layout{Home: home}, paths.Layout{Home: home, Scratch: t.TempDir()}
	if err := copyNotes(live, scratch, "talkable", "talkable"); err != nil {
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
	if err := copyNotes(live, scratch, "talkable", "talkable"); err != nil {
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

// A case records the hashes of the notes its judge reads, from the scratch copy, and the Codex
// gauge as the account's newest session reports it.
func TestEvalRecordsTheNotesAndTheCodexGauge(t *testing.T) {
	scratch := paths.Layout{Home: t.TempDir(), Scratch: t.TempDir()}
	writeFile(t, engine.NotesPath(scratch, "example", "web"), "# Notes")
	writeFile(t, filepath.Join(engine.NotesDir(scratch, "example", "web"), "qa.sh"), "echo qa")
	got := notesHashes(scratch, "example", "web")
	if len(got) != 2 || got["web.md"] == "" || got["web/qa.sh"] == "" {
		t.Fatalf("notes hashes %v", got)
	}

	cfg := &config.Config{}
	cfg.Usage.CodexHome = t.TempDir()
	if p := codexUsed(context.Background(), cfg); p != nil {
		t.Fatalf("no sessions: gauge %v", *p)
	}
	writeFile(t, filepath.Join(cfg.Usage.CodexHome, "sessions", "2026", "10", "08", "rollout-2026-10-08T10-00-00-x.jsonl"),
		`{"timestamp":"2026-10-08T07:00:00.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":`+
			`{"primary":{"used_percent":31.0,"window_minutes":10080,"resets_at":4102444800},"secondary":null}}}`+"\n")
	if p := codexUsed(context.Background(), cfg); p == nil || *p != 31 {
		t.Fatalf("gauge %v, want 31", p)
	}
}
