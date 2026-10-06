package pipeline

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// The judge reports the harness files it ran or read (harness_used, SKILL.md
// section 8); the round hands them to the engine as written, trimmed, without
// repeats.
func TestHarnessUsedReachesTheRoundResult(t *testing.T) {
	e := newEnv(t)
	post := e.judgePosts(601, "COMMENTED", "COMMENT")
	post.extra = map[string]any{"harness_used": []string{"run_spec.sh", " qa/smoke.rb ", "run_spec.sh", ""}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.HarnessUsed, []string{"run_spec.sh", "qa/smoke.rb"}) {
		t.Errorf("HarnessUsed = %q", res.HarnessUsed)
	}
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{`{"status":"posted","harness_used":"one.sh"}`, []string{"one.sh"}},
		{`{"status":"posted","harness_used":[1,"two.sh",null]}`, []string{"two.sh"}},
		{`{"status":"posted","harness_used":{"x":1}}`, nil},
		{`{"status":"posted"}`, nil},
	} {
		r, ok := parseResult([]byte(tc.raw))
		if !ok || !slices.Equal(r.HarnessUsed, tc.want) {
			t.Errorf("parseResult(%s).HarnessUsed = %q, want %q", tc.raw, r.HarnessUsed, tc.want)
		}
	}
}

// Right before the judge is prompted the round snapshots the notes and the
// harness into its report directory, so the engine can tell what the judge's
// turn changed; a blind replay (scratch notes) takes none.
func TestTheJudgePromptFollowsANotesSnapshot(t *testing.T) {
	for _, blind := range []bool{false, true} {
		e := newEnv(t)
		root := filepath.Join(t.TempDir(), "notes")
		notesPath := filepath.Join(root, "talkable", "talkable.md")
		if err := os.MkdirAll(filepath.Join(root, "talkable", "talkable"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(notesPath, []byte("# Notes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "talkable", "talkable", "run.sh"), []byte("bin/rspec\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		var sawSnapshot bool
		post := e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)
		e.ag.behaviors[agents.RoleJudge] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
			s, err := notes.ReadSnapshot(e.reportDir())
			sawSnapshot = err == nil && s.Round == 1 && len(s.Harness) == 1 && s.Harness[0].Name == "run.sh"
			// The judge then rewrites the notes: the snapshot keeps what it found.
			if err := os.WriteFile(notesPath, []byte("# Notes\n- new\n"), 0o600); err != nil {
				return err
			}
			return post(f, run, text)
		}}
		in := e.input(KindInitial)
		in.NotesPath, in.Blind = notesPath, blind
		if _, err := e.r.RunRound(e.ctx, in); err != nil {
			t.Fatal(err)
		}
		if sawSnapshot == blind {
			t.Errorf("blind %v: a snapshot at the judge's prompt = %v", blind, sawSnapshot)
		}
		if blind {
			continue
		}
		s, err := notes.ReadSnapshot(e.reportDir())
		if err != nil || s.NotesSHA != notes.TextSHA([]byte("# Notes\n")) {
			t.Errorf("snapshot after the round = %+v, %v; want the notes before the judge", s, err)
		}
	}
}
