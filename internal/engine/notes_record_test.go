package engine

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
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// notesOf is the harness's talkable/talkable notes.
func notesOf(t *testing.T, h *harness) notes.Repo {
	t.Helper()
	nr, ok := notesRepo(h.layout, "talkable", "talkable")
	if !ok {
		t.Fatal("no notes path")
	}
	return nr
}

func writeNotes(t *testing.T, nr notes.Repo, text string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(nr.Harness(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nr.Notes(), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		p := filepath.Join(nr.Harness(), filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// judgeRounds makes every round of the harness a judge round as the pipeline
// runs it: the notes are snapshot before the judge, then judge (when set)
// changes them, and the result names the harness files the judge used and a
// reviewer report that names one by its path.
func judgeRounds(t *testing.T, h *harness, judge func(nr notes.Repo), used []string, reportNames string) {
	t.Helper()
	n := 0
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		n++
		res, err := h.rd.posted(h.ctx, in, n)
		if err != nil {
			return res, err
		}
		nr, ok := notes.RepoOf(in.NotesPath)
		if !ok {
			t.Errorf("the round has no notes: %q", in.NotesPath)
			return res, nil
		}
		res.ReportDir = h.layout.ReviewDir("talkable", "talkable", in.PR.Number, in.TargetSHA)
		snap, err := notes.Take(nr, res.Round, h.clock.Now())
		if err == nil {
			err = notes.WriteSnapshot(res.ReportDir, snap)
		}
		if err != nil {
			t.Error(err)
		}
		if judge != nil {
			judge(nr)
		}
		res.JudgeRunID = "r-judge-" + itoa(int64(n))
		res.HarnessUsed = used
		if reportNames != "" {
			p := filepath.Join(res.ReportDir, "claude-review.md")
			if err := os.WriteFile(p, []byte("Ran `"+filepath.Join(nr.Harness(), reportNames)+" spec/models`.\n"), 0o600); err != nil {
				t.Error(err)
			}
			res.Reports[agents.Role("claude-review")] = pipeline.RoleReport{Role: "claude-review", Status: pipeline.ReportOK, Path: p, RunID: "r-claude-" + itoa(int64(n))}
		}
		return res, nil
	}
}

// knownRepo brings talkable/talkable into the registry (a first poll), then
// starts the daemon again, as a new binary finds the registry.
func knownRepo(t *testing.T, h *harness) {
	t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.startup()
}

// reviewNext reviews PR #n at head, the repository known; the pool's one
// slot goes to it once the PR before it is past min_warm.
func reviewNext(t *testing.T, h *harness, n int, head string) {
	t.Helper()
	h.advance(31 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: n, head: head})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.tick()
	h.wantState(n, store.PRReviewed)
}

func notesEvents(t *testing.T, h *harness, kind string) []store.Event {
	t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, "notes:talkable/talkable", 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func repoID(t *testing.T, h *harness) int64 {
	t.Helper()
	repo, err := h.st.RepoByFullName(h.ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	return repo.ID
}

// A judge round that rewrote the notes leaves a version of the judge, with
// the round's PR and run, and a notes.changed event that counts lines and
// harness files without quoting them; a round that changed nothing records
// nothing.
func TestAJudgeRoundThatChangedTheNotesRecordsAVersion(t *testing.T) {
	h := newHarness(t)
	nr := notesOf(t, h)
	writeNotes(t, nr, "# Notes\n- run bin/rspec\n", map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n"})
	judgeRounds(t, h, func(nr notes.Repo) {
		writeNotes(t, nr, "# Notes\n- run bin/rspec\n- lint with bin/rubocop\n", map[string]string{"lint.sh": "bin/rubocop\n"})
	}, nil, "")
	knownRepo(t, h)
	reviewNext(t, h, 2, "b1")

	hist, err := h.st.NotesHistory(h.ctx, repoID(t, h), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[0].Source != store.NotesFromJudge || hist[0].PRNumber != 2 || hist[0].RunID != "r-judge-1" ||
		hist[1].Source != store.NotesFromImport {
		t.Fatalf("history = %+v, want the startup import, then the judge's version of #2", hist)
	}
	evs := notesEvents(t, h, "notes.changed")
	if len(evs) != 2 {
		t.Fatalf("notes.changed events = %d, want 2", len(evs))
	}
	slices.SortFunc(evs, func(a, b store.Event) int { return int(b.ID - a.ID) }) // newest first
	var data map[string]any
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["lines_added"] != float64(1) || data["lines_removed"] != float64(0) || data["source"] != "judge" || data["pr"] != float64(2) ||
		!slices.Equal(anyStrings(data["harness_added"]), []string{"lint.sh"}) {
		t.Errorf("notes.changed data = %v", data)
	}
	if strings.Contains(evs[0].Message, "rubocop") || strings.Contains(string(evs[0].Data), "rubocop") {
		t.Errorf("the event quotes the notes: %s %s", evs[0].Message, evs[0].Data)
	}

	// A round whose judge leaves the notes alone records nothing.
	judgeRounds(t, h, nil, nil, "")
	reviewNext(t, h, 3, "c1")
	if hist, _ := h.st.NotesHistory(h.ctx, repoID(t, h), 0); len(hist) != 2 {
		t.Errorf("a round without a change added a version: %+v", hist)
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// The judge's harness_used and a reviewer report that names a harness file
// by its path count as uses; every round counts for the files present.
func TestHarnessUsesAreCountedPerRound(t *testing.T) {
	h := newHarness(t)
	nr := notesOf(t, h)
	writeNotes(t, nr, "# Notes\n", map[string]string{"run_spec.sh": "x", "qa/smoke.rb": "y", "probe_spec.rb": "z"})
	judgeRounds(t, h, nil, []string{"run_spec.sh", nr.Harness() + "/qa/smoke.rb", "gone.sh"}, "probe_spec.rb")
	h.reviewedPR(2, "b1")

	uses, err := h.st.NotesFileUses(h.ctx, repoID(t, h))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.NotesFileUse{}
	for _, u := range uses {
		got[u.File] = u
	}
	for _, f := range []string{"run_spec.sh", "qa/smoke.rb", "probe_spec.rb"} {
		if u := got[f]; u.Rounds != 1 || u.Uses != 1 {
			t.Errorf("%s = %+v, want 1 round and 1 use", f, u)
		}
	}
	if _, ok := got["gone.sh"]; ok {
		t.Error("a file the harness does not hold was counted")
	}
}

// Past a limit, a notes.over_limit event names it and the repository is
// marked; back within every limit, the mark goes. Nothing holds the round.
func TestNotesPastALimitAreMarkedForCuration(t *testing.T) {
	h := newHarness(t)
	h.cfg.Notes.MaxBytes = 40
	nr := notesOf(t, h)
	writeNotes(t, nr, "# Notes\n", nil)
	judgeRounds(t, h, func(nr notes.Repo) {
		writeNotes(t, nr, "# Notes\n"+strings.Repeat("- a long-winded lesson\n", 4), nil)
	}, nil, "")
	h.reviewedPR(2, "b1")

	if v, _ := h.e.getKV(h.ctx, KVNotesOver("talkable/talkable")); v != "max_bytes" {
		t.Fatalf("mark = %q, want max_bytes", v)
	}
	evs := notesEvents(t, h, "notes.over_limit")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "max_bytes (") {
		t.Fatalf("over_limit events = %+v", evs)
	}

	writeNotes(t, nr, "# Notes\n", nil)
	h.e.syncNotes(h.ctx, false)
	if v, ok := h.e.getKV(h.ctx, KVNotesOver("talkable/talkable")); ok {
		t.Errorf("mark after the notes shrank = %q", v)
	}
	if len(notesEvents(t, h, "notes.within_limits")) != 1 {
		t.Error("no notes.within_limits event")
	}
}

// The first start imports every repository's notes and harness; a later
// start finds them recorded and adds nothing.
func TestTheFirstStartImportsTheNotes(t *testing.T) {
	h := newHarness(t)
	nr := notesOf(t, h)
	writeNotes(t, nr, "# Notes\n", map[string]string{"run_spec.sh": "x"})
	knownRepo(t, h) // a start with the repository in the registry
	h.e.startup(context.Background())
	h.settle()
	hist, err := h.st.NotesHistory(h.ctx, repoID(t, h), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Source != store.NotesFromImport || len(hist[0].Files) != 1 {
		t.Fatalf("history = %+v, want one import", hist)
	}
}
