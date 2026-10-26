package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// A replay records what notes the judge read: the notes file (or that there was none) and every
// file of its harness directory, each by its SHA-256.
func TestHashNotesNamesEveryFileTheJudgeReads(t *testing.T) {
	root := t.TempDir()
	file, dir := filepath.Join(root, "web.md"), filepath.Join(root, "web")
	got, err := HashNotes(file, dir)
	if err != nil || !reflect.DeepEqual(got, map[string]string{"web.md": ""}) {
		t.Fatalf("no notes: %v, %v; want the notes file named with no hash", got, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{file: "# Notes", filepath.Join(dir, "sub", "qa.sh"): "echo qa", filepath.Join(dir, "probe.cjs"): "x"} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err = HashNotes(file, dir)
	want := map[string]string{"web.md": sum("# Notes"), "web/sub/qa.sh": sum("echo qa"), "web/probe.cjs": sum("x")}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("HashNotes = %v, %v; want %v", got, err, want)
	}
}

// Two compared runs whose judge read other notes for the same case say so: an eval comparison
// would otherwise mix a notes change with a skill change.
func TestWriteTextSaysWhenTheComparedRunsReadOtherNotes(t *testing.T) {
	scored := &Score{Found: 1, Total: 1, Defects: []DefectResult{{ID: "d", Found: true, SeverityOK: true}}}
	run := func(id string, notes map[string]string) Run {
		return Run{ID: id, Cases: []CaseRun{{Case: "geo", PR: "example/web#1", Outcome: "dry_run", Score: scored, Notes: notes}}}
	}
	old := map[string]string{"web.md": sum("a"), "web/qa.sh": sum("q"), "web/old.sh": sum("o")}
	tests := []struct {
		name      string
		r, prev   Run
		want      string // "" = no notes line
		wantNotIn string
	}{
		{"same notes", run("new", old), run("old", old), "", ""},
		{"edited, added and removed files", run("new", map[string]string{"web.md": sum("b"), "web/qa.sh": sum("q"), "web/probe.cjs": sum("p")}), run("old", old),
			"notes differ from old: geo (web.md changed, web/old.sh gone, web/probe.cjs new)", ""},
		{"notes written since", run("new", map[string]string{"web.md": sum("a")}), run("old", map[string]string{"web.md": ""}),
			"notes differ from old: geo (web.md new)", ""},
		{"withheld in this run", Run{ID: "new", NoNotes: true, Cases: []CaseRun{{Case: "geo", Score: scored}}}, run("old", old),
			"notes differ from old: geo (withheld in new)", ""},
		{"a run that recorded no notes", run("new", old), run("old", nil), "", "notes differ"},
		{"a case only one run has", run("new", old), Run{ID: "old", Cases: []CaseRun{{Case: "other", Notes: map[string]string{"x.md": ""}}}}, "", "notes differ"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := text(tt.r, &tt.prev)
			switch {
			case tt.want != "" && !strings.Contains(got, tt.want+"\n"):
				t.Errorf("output lacks %q:\n%s", tt.want, got)
			case tt.want == "" && strings.Contains(got, "notes differ"):
				t.Errorf("output names a notes difference:\n%s", got)
			}
		})
	}
}

// The points a case used are what the Codex gauge moved while it ran; a reset in between or a
// gauge not read leaves them unknown.
func TestWriteTextNamesTheCodexPointsEachCaseUsed(t *testing.T) {
	pct := func(f float64) *float64 { return &f }
	r := Run{ID: "r", Cases: []CaseRun{
		{Case: "geo", Outcome: "dry_run", CodexBefore: pct(30), CodexAfter: pct(31)},
		{Case: "copy", Outcome: "dry_run", CodexBefore: pct(31), CodexAfter: pct(31)},
		{Case: "reset", Outcome: "dry_run", CodexBefore: pct(98), CodexAfter: pct(1)},
		{Case: "unread", Outcome: "error"},
	}}
	if p, ok := r.Cases[0].Points(); !ok || p != 1 {
		t.Errorf("Points = %v, %v; want 1", p, ok)
	}
	if _, ok := r.Cases[2].Points(); ok {
		t.Error("a case across a reset has points")
	}
	if got := text(r, nil); !strings.Contains(got, "codex: +1 points (geo +1, copy +0)\n") {
		t.Errorf("output lacks the codex line:\n%s", got)
	}
	if got := text(Run{ID: "r", Cases: []CaseRun{{Case: "unread"}}}, nil); strings.Contains(got, "codex:") {
		t.Errorf("a run with no points read has a codex line:\n%s", got)
	}
}

// A stored replay is a baseline for a new run with the same inputs: the same skill, prompts and
// role config (Run.Inputs), the case at the same head with a score, and a clean checkout.
func TestBaselinesAreScoredReplaysWithTheSameInputsNewestFirst(t *testing.T) {
	c := Case{Name: "geo", Head: sha}
	scored := &Score{Found: 1, Total: 2}
	runs := []Run{
		{ID: "1-old", Inputs: "aaa", Commit: "c1", Cases: []CaseRun{{Case: "geo", Head: sha, Score: scored}}},
		{ID: "2-other-inputs", Inputs: "bbb", Cases: []CaseRun{{Case: "geo", Head: sha, Score: scored}}},
		{ID: "3-no-inputs", Cases: []CaseRun{{Case: "geo", Head: sha, Score: scored}}},
		{ID: "4-other-head", Inputs: "aaa", Cases: []CaseRun{{Case: "geo", Head: strings.Repeat("f", 40), Score: scored}}},
		{ID: "5-unscored", Inputs: "aaa", Cases: []CaseRun{{Case: "geo", Head: sha, Outcome: "usage_limit"}}},
		{ID: "6-dirty", Inputs: "aaa", Dirty: true, Cases: []CaseRun{{Case: "geo", Head: sha, Score: scored}}},
		{ID: "7-other-case", Inputs: "aaa", Cases: []CaseRun{{Case: "copy", Head: sha, Score: scored}}},
		{ID: "8-new", Inputs: "aaa", Commit: "c8", Cases: []CaseRun{{Case: "copy"}, {Case: "geo", Head: sha, Score: scored}}},
	}
	got := Baselines(runs, "aaa", c)
	if len(got) != 2 || got[0].Run != "8-new" || got[0].Commit != "c8" || got[1].Run != "1-old" || got[0].Case.Score != scored {
		t.Fatalf("Baselines = %+v, want 8-new then 1-old", got)
	}
	if got := Baselines(runs, "", c); len(got) != 0 {
		t.Errorf("unknown inputs reuse %+v", got)
	}
}

// A judge-only replay scored the judge on another run's reviewer reports: a full run would not
// repeat it, so it is no baseline.
func TestBaselinesLeaveOutJudgeOnlyReplays(t *testing.T) {
	c := Case{Name: "geo", Head: sha}
	scored := &Score{Found: 1, Total: 2}
	runs := []Run{
		{ID: "1-full", Inputs: "aaa", Cases: []CaseRun{{Case: "geo", Head: sha, Score: scored}}},
		{ID: "2-judge-only", Inputs: "aaa", Cases: []CaseRun{{Case: "geo", Head: sha, Score: scored, JudgeOnly: true, ReportsRun: "1-full"}}},
	}
	if got := Baselines(runs, "aaa", c); len(got) != 1 || got[0].Run != "1-full" {
		t.Fatalf("Baselines = %+v, want the full replay alone", got)
	}
}

// A judge's baseline for a judge-only comparison read the same reviewer reports: the full run that
// made them, when its judge read the same inputs, or a judge-only replay on them; a replay on
// another run's reports, with other inputs, from a dirty checkout or unscored is none.
func TestJudgeBaselinesTakeOnlyTheSameReports(t *testing.T) {
	c := Case{Name: "geo", Head: sha}
	scored := &Score{Found: 1, Total: 2}
	runs := []Run{
		{ID: "1-full", Inputs: "aaa", Cases: []CaseRun{{Case: "geo", Head: sha, Score: scored}}},
		{ID: "2-full-other-judge", Inputs: "bbb", Cases: []CaseRun{{Case: "geo", Head: sha, Score: scored}}},
		{ID: "3-judge-on-2", Inputs: "aaa", Commit: "c3", Cases: []CaseRun{{Case: "geo", Head: sha, Score: scored, JudgeOnly: true, ReportsRun: "2-full-other-judge"}}},
		{ID: "4-judge-on-1", Inputs: "aaa", Cases: []CaseRun{{Case: "geo", Head: sha, Score: scored, JudgeOnly: true, ReportsRun: "1-full"}}},
		{ID: "5-dirty", Inputs: "aaa", Dirty: true, Cases: []CaseRun{{Case: "geo", Head: sha, Score: scored, JudgeOnly: true, ReportsRun: "2-full-other-judge"}}},
		{ID: "6-unscored", Inputs: "aaa", Cases: []CaseRun{{Case: "geo", Head: sha, JudgeOnly: true, ReportsRun: "2-full-other-judge"}}},
		{ID: "7-other-inputs", Inputs: "ccc", Cases: []CaseRun{{Case: "geo", Head: sha, Score: scored, JudgeOnly: true, ReportsRun: "2-full-other-judge"}}},
	}
	if got := JudgeBaselines(runs, "aaa", c, "2-full-other-judge"); len(got) != 1 || got[0].Run != "3-judge-on-2" || got[0].Commit != "c3" {
		t.Fatalf("JudgeBaselines on 2 = %+v, want the judge-only replay on its reports", got)
	}
	if got := JudgeBaselines(runs, "aaa", c, "1-full"); len(got) != 2 || got[0].Run != "4-judge-on-1" || got[1].Run != "1-full" {
		t.Fatalf("JudgeBaselines on 1 = %+v, want the replay on its reports, then the full run itself", got)
	}
	if got := JudgeBaselines(runs, "bbb", c, "1-full"); len(got) != 0 {
		t.Fatalf("JudgeBaselines with other inputs = %+v", got)
	}
	if got := JudgeBaselines(runs, "", c, "1-full"); len(got) != 0 {
		t.Fatalf("unknown inputs reuse %+v", got)
	}
}

// A case's newest replay, which the skill gate's guards read, is its newest scored replay at the
// corpus's head of any kind (full or judge-only, any inputs, a dirty checkout too) but those whose
// run recorded the inputs left out; it is dated by the case's start, else its run's.
func TestNewestReplayIsTheNewestScoredReplayAtTheHead(t *testing.T) {
	c := Case{Name: "geo", Head: sha}
	found, missed := &Score{Found: 1, Total: 1}, &Score{Total: 1}
	day := func(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }
	runs := []Run{
		{ID: "1-full", Inputs: "aaa", Started: day(1), Cases: []CaseRun{{Case: "geo", Head: sha, Score: missed}}},
		{ID: "2-dirty-judge-only", Inputs: "bbb", Dirty: true, Started: day(2),
			Cases: []CaseRun{{Case: "geo", Head: sha, Score: found, JudgeOnly: true, Started: day(2).Add(time.Hour)}}},
		{ID: "3-candidate", Inputs: "cand", Started: day(3), Cases: []CaseRun{{Case: "geo", Head: sha, Score: missed}}},
		{ID: "4-other-head", Inputs: "aaa", Cases: []CaseRun{{Case: "geo", Head: strings.Repeat("f", 40), Score: missed}}},
		{ID: "5-unscored", Inputs: "aaa", Cases: []CaseRun{{Case: "geo", Head: sha, Outcome: "usage_limit"}}},
		{ID: "6-other-case", Inputs: "aaa", Cases: []CaseRun{{Case: "copy", Head: sha, Score: found}}},
	}
	if r, ok := NewestReplay(runs, c, ""); !ok || r.Run != "3-candidate" || r.Inputs != "cand" || r.Case.Score != missed || !r.At.Equal(day(3)) {
		t.Fatalf("NewestReplay = %+v, %v; want run 3, dated by its run", r, ok)
	}
	if r, ok := NewestReplay(runs, c, "cand"); !ok || r.Run != "2-dirty-judge-only" || r.Case.Score != found || !r.At.Equal(day(2).Add(time.Hour)) {
		t.Fatalf("NewestReplay without the candidate's = %+v, %v; want run 2, dated by its case", r, ok)
	}
	if r, ok := NewestReplay(runs, Case{Name: "map", Head: sha}, ""); ok {
		t.Fatalf("a case never replayed: %+v", r)
	}
}

// A run lives for as long as its cases replay: the skill gate makes its base run at its first
// baseline replay and replays a lost guard's base into it later, after the candidate's runs were
// made. The newest replay of a case is the one that replayed last (the case's start, else its
// run's), not the one in the newest run, and a run's stored replays say when they were made
// (Baseline.At), so a replay of the base made since a guard's baseline is known as such.
func TestNewestReplayIsTheLatestReplayNotTheLatestRun(t *testing.T) {
	c := Case{Name: "geo", Head: sha}
	found, missed := &Score{Found: 1, Total: 1}, &Score{Total: 1}
	at := func(h int) time.Time { return time.Date(2026, 10, 20, h, 0, 0, 0, time.UTC) }
	runs := []Run{
		{ID: "20261020-090000", Inputs: "in-base", Started: at(9), Cases: []CaseRun{
			{Case: "copy", Head: sha, Score: missed, Started: at(9)},
			{Case: "geo", Head: sha, Score: found, Started: at(12)}}},
		{ID: "20261020-100000", Inputs: "in-cand", Started: at(10), Cases: []CaseRun{{Case: "geo", Head: sha, Score: missed, Started: at(10)}}},
		{ID: "20261020-110000", Inputs: "in-cand", Started: at(11), Cases: []CaseRun{{Case: "geo", Head: sha, Score: missed, Started: at(11)}}},
	}
	if r, ok := NewestReplay(runs, c, ""); !ok || r.Run != "20261020-090000" || r.Case.Score != found || !r.At.Equal(at(12)) {
		t.Fatalf("NewestReplay = %+v, %v; want the base's replay of 12:00 in the run made at 9:00", r, ok)
	}
	if bs := Baselines(runs, "in-base", c); len(bs) != 1 || !bs[0].At.Equal(at(12)) {
		t.Fatalf("Baselines = %+v, want the base's replay dated 12:00", bs)
	}
	// Two replays dated alike (neither recorded a start): the later run's is the newer.
	same := []Run{{ID: "1", Cases: []CaseRun{{Case: "geo", Head: sha, Score: found}}}, {ID: "2", Cases: []CaseRun{{Case: "geo", Head: sha, Score: missed}}}}
	if r, ok := NewestReplay(same, c, ""); !ok || r.Run != "2" {
		t.Fatalf("NewestReplay of two undated replays = %+v, %v; want the later run's", r, ok)
	}
}

// The replays a proposal's gate reads its guards from are those made before the proposal: a run
// made before it keeps only the cases it replayed before it, and a run made since is left out; a
// zero time keeps every run. The runs given are not changed.
func TestRunsBeforeKeepsOnlyTheReplaysMadeBeforeATime(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 20, h, 0, 0, 0, time.UTC) }
	runs := []Run{
		{ID: "old", Started: at(8), Cases: []CaseRun{{Case: "geo"}, {Case: "copy", Started: at(11)}}},
		{ID: "long", Started: at(9), Cases: []CaseRun{{Case: "geo", Started: at(9)}, {Case: "map", Started: at(12)}}},
		{ID: "since", Started: at(11), Cases: []CaseRun{{Case: "geo"}}},
	}
	var names []string
	for _, r := range RunsBefore(runs, at(10)) {
		for _, c := range r.Cases {
			names = append(names, r.ID+"/"+c.Case)
		}
	}
	if !reflect.DeepEqual(names, []string{"old/geo", "long/geo"}) {
		t.Fatalf("RunsBefore = %q, want the replays made before 10:00", names)
	}
	if len(runs[0].Cases) != 2 || len(runs[1].Cases) != 2 {
		t.Fatalf("RunsBefore changed the runs given: %+v", runs)
	}
	if got := RunsBefore(runs, time.Time{}); len(got) != 3 || len(got[1].Cases) != 2 {
		t.Fatalf("RunsBefore of a zero time = %+v, want every run", got)
	}
}
