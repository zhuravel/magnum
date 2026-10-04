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
// judge (with the rewrite instruction) and leave it out without notes.
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
						if strings.Contains(text, "Repository notes") || strings.Contains(text, "Notes for") || strings.Contains(text, "rewrite") {
							t.Errorf("%s mentions notes without NotesPath:\n%s", what, text)
						}
					}
					return
				}
				mustContain(t, "claude prompt", claude, notesParagraph)
				mustContain(t, "judge prompt", judge, notesParagraph,
					"update "+notesFile+" when this round taught you something durable",
					"rewrite it, never append", "`# Notes for talkable/talkable (updated YYYY-MM-DD)`", "before you write the result file",
					"harness directory /state/notes/talkable/talkable holds no files yet", agents.NotesLockLine("/state/notes/talkable/talkable.lock"))
				if strings.Index(judge, notesParagraph) > strings.Index(judge, "<magnum>") {
					t.Errorf("the notes paragraph comes after the <magnum> block:\n%s", judge)
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
		"harness directory "+dir+" holds `fixtures/`, `jest-setup.js`, `run-spec.sh`.",
		agents.NotesLockLine(dir+".lock"), agents.NotesUnlockLine(dir+".lock"), "Read "+notes+" again now")
}
