package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
)

const (
	notesFile      = "/state/notes/talkable/talkable.md"
	notesParagraph = "Repository notes at " + notesFile + ": read them first; they are hints from earlier reviews, verify before relying on them."
)

// RoundInput.NotesPath reaches the RoleData and JudgeData of the round, so
// the real prompt files render the notes paragraph for the reviewer and the
// notes fields of the judge's <magnum> block (the skill holds the rewrite
// steps), and leave both out without notes.
func TestNotesPathReachesTheRolePromptsAndTheJudgePrompt(t *testing.T) {
	prev := &PreviousReview{ID: 900, Event: "CHANGES_REQUESTED", SHA: prevSHA, SubmittedAt: t0.Add(-3 * time.Hour)}
	for _, kind := range []string{KindInitial, KindRereview} {
		for _, notes := range []string{notesFile, ""} {
			name := kind + " with notes"
			if notes == "" {
				name = kind + " without notes"
			}
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}
				in := e.input(kind)
				in.NotesPath = notes
				if kind == KindRereview {
					in.Round, in.Previous, in.Since = 2, prev, t0.Add(-3*time.Hour)
				}
				if _, err := e.r.RunRound(e.ctx, in); err != nil {
					t.Fatalf("RunRound: %v", err)
				}
				claude := e.ag.submitsFor(agents.RoleClaude)[0].Text
				judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
				if notes == "" {
					for what, text := range map[string]string{"claude prompt": claude, "judge prompt": judge} {
						if strings.Contains(text, "Repository notes") || strings.Contains(text, "\nnotes") || strings.Contains(text, "rewrite") {
							t.Errorf("%s mentions notes without NotesPath:\n%s", what, text)
						}
					}
					return
				}
				mustContain(t, "claude prompt", claude, notesParagraph)
				lock := "/state/notes/talkable/talkable.lock"
				mustContain(t, "judge prompt", judge[strings.Index(judge, "<magnum>"):], "\nnotes: "+notesFile+"\n",
					"\nnotes_dir: /state/notes/talkable/talkable\n", "\nnotes_harness:\n",
					"\nnotes_lock: "+agents.NotesLockLine(lock)+"\n", "\nnotes_unlock: "+agents.NotesUnlockLine(lock)+"\n")
				if strings.Contains(judge, "Repository notes") || strings.Contains(judge, "Take the lock") {
					t.Errorf("the judge prompt repeats the skill's notes steps:\n%s", judge)
				}
			})
		}
	}
}

// The judge sees what the harness directory holds right now, so a script
// the notes no longer mention is visible, and the commands of the notes
// lock next to it.
func TestJudgePromptListsTheHarnessDirectory(t *testing.T) {
	e := newEnv(t)
	notes := filepath.Join(e.layout.State(), "notes", "talkable", "talkable.md")
	dir := strings.TrimSuffix(notes, ".md")
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"run-spec.sh", "jest-setup.js"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.NotesPath = notes
	if _, err := e.r.RunRound(e.ctx, in); err != nil {
		t.Fatal(err)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
	mustContain(t, "judge prompt", judge,
		"\nnotes: "+notes+"\nnotes_dir: "+dir+"\nnotes_harness: fixtures/, jest-setup.js, run-spec.sh\n",
		"\nnotes_lock: "+agents.NotesLockLine(dir+".lock")+"\n", "\nnotes_unlock: "+agents.NotesUnlockLine(dir+".lock")+"\n")
}

// A harness entry whose name the judge prompt cannot carry is left out, and
// the round's log says how many were, never the names.
func TestAHarnessNameLeftOutOfTheJudgePromptIsLogged(t *testing.T) {
	e := newEnv(t)
	notes := filepath.Join(e.layout.State(), "notes", "talkable", "talkable.md")
	dir := strings.TrimSuffix(notes, ".md")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"run-spec.sh", "two words.sh"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.NotesPath = notes
	if _, err := e.r.RunRound(e.ctx, in); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "\nnotes_harness: run-spec.sh (+1 more)\n")
	e.log.mu.Lock()
	defer e.log.mu.Unlock()
	found := false
	for _, l := range e.log.lines {
		if strings.Contains(l, "two words") {
			t.Errorf("log line names the entry: %q", l)
		}
		found = found || strings.Contains(l, "1 entry name(s) with characters outside")
	}
	if !found {
		t.Fatalf("log lines %q", e.log.lines)
	}
}
