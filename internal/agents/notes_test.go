package agents

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// notesPartsSentence is the fixed sentence every prompt that names the
// notes puts before their parts.
const notesPartsSentence = "Read the repository notes in these parts, one part per tool call. Never print two parts in one call."

const testNotesPath = "/m/state/notes/talkable/talkable.md"

// testNotesParagraph is the reviewer prompts' notes paragraph for np.
func testNotesParagraph(np NotesParts) string {
	s := fmt.Sprintf("Repository notes at %s (%d bytes, %d lines, frozen for this round): read them first; they are hints from earlier reviews, verify before relying on them. %s",
		np.Path, np.Bytes, np.Lines, notesPartsSentence)
	for _, p := range np.Parts {
		s += "\n- `" + p.Command + "`"
	}
	return s
}

// Every reviewer prompt shows the notes paragraph, the round's frozen copy
// in parts, when the round has notes to read and omits it otherwise (no
// notes file yet, or an empty one, though NotesPath is set); a reviewer
// never rewrites the notes.
func TestNotesParagraphInRolePrompts(t *testing.T) {
	for _, name := range []string{"claude-review.md", "claude-rereview.md", "claude-restart.md", "claude-simplify.md"} {
		t.Run(name, func(t *testing.T) {
			p := prompt(t, name)
			d := roleFixture()
			plain, err := RenderPrompt(p, d)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plain, "Repository notes") || strings.HasSuffix(plain, "\n") {
				t.Fatalf("without NotesPath:\n%s", plain)
			}
			d.NotesPath = testNotesPath
			if got, err := RenderPrompt(p, d); err != nil || got != plain {
				t.Fatalf("NotesPath without parts (no notes yet): %v\n%s", err, got)
			}
			d.NotesParts = notesPartsFixture()
			got, err := RenderPrompt(p, d)
			if err != nil {
				t.Fatal(err)
			}
			if want := plain + "\n\n" + testNotesParagraph(d.NotesParts); got != want {
				t.Fatalf("with notes:\n%s\nwant the plain prompt plus a paragraph:\n%s", got, want)
			}
			if strings.Contains(got, "rewrite") {
				t.Errorf("a reviewer must not rewrite the notes:\n%s", got)
			}
		})
	}
}

// testNotesPartsField is the judge's `notes_parts` field for np.
func testNotesPartsField(np NotesParts) string {
	s := fmt.Sprintf("\nnotes_parts: %s (%d bytes, %d lines), a copy of `notes` frozen for this round. %s", np.Path, np.Bytes, np.Lines, notesPartsSentence)
	for _, p := range np.Parts {
		s += "\n  - `" + p.Command + "`"
	}
	return s + "\n"
}

// testNotesLockFields are the judge's lock fields for the notes file
// notes: the lock, the re-read of the live file in parts beside it, the
// unlock.
func testNotesLockFields(notes string) string {
	_, lock := NotesFiles(notes)
	return "\nnotes_lock: " + NotesLockLine(lock) +
		"\nnotes_reread: once `notes_lock` printed `notes locked`, before you merge, read the live `notes` again in the parts this prints, one part per tool call: `" +
		NotesRereadLine(notes) + "`\nnotes_unlock: " + NotesUnlockLine(lock) + "\n"
}

// The judge prompts pass the notes as <magnum> fields (the file, the
// round's frozen copy in parts, its harness directory and listing, the lock
// commands and the re-read of the live file beside them), and only when
// there are notes, the parts only when there is a copy to read; the steps
// are the skill's (SKILL.md section 2), so no prompt repeats them and the
// text before the block is the same with or without notes.
func TestJudgePromptsPassTheNotesAsMagnumFields(t *testing.T) {
	dir, _ := NotesFiles(testNotesPath)
	for _, name := range []string{"judge-initial.md", "judge-rereview.md", "judge-recovery.md", "judge-continue.md", "judge-own-pass.md"} {
		t.Run(name, func(t *testing.T) {
			p := prompt(t, name)
			d := judgeFixture()
			if name == "judge-own-pass.md" {
				d.Mode, d.Phase, d.OwnFindings, d.Reports = "initial", PhaseOwnPass, "/rep/"+OwnFindingsFile, nil
			}
			plain, err := RenderPrompt(p, d)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plain, "\nnotes") {
				t.Fatalf("without NotesPath:\n%s", plain)
			}
			d.NotesPath = testNotesPath
			if got, err := RenderPrompt(p, d); err != nil || strings.Contains(got, "notes_parts") || strings.Contains(got, notesPartsSentence) {
				t.Errorf("NotesPath without parts (no notes yet): %v\n%s", err, got)
			}
			d.NotesParts = notesPartsFixture()
			for _, data := range []any{d, &d} { // values and pointers render alike
				got, err := RenderPrompt(p, data)
				if err != nil {
					t.Fatal(err)
				}
				at := strings.Index(got, "<magnum>")
				block := got[at:]
				for _, want := range []string{"\nnotes: " + testNotesPath + testNotesPartsField(d.NotesParts) + "notes_dir: " + dir + "\n", "\nnotes_harness:\n",
					testNotesLockFields(testNotesPath)} {
					if !strings.Contains(block, want) {
						t.Errorf("the <magnum> block lacks %q:\n%s", want, block)
					}
				}
				if !strings.HasSuffix(got, "</magnum>") || got[:at] != plain[:strings.Index(plain, "<magnum>")] {
					t.Errorf("the notes changed the text before the block:\n%s\n--- plain:\n%s", got, plain)
				}
				for _, step := range notesSteps {
					if strings.Contains(got, step) {
						t.Errorf("the prompt repeats the skill's notes step %q:\n%s", step, got)
					}
				}
			}
		})
	}
	// The short follow-ups restate no setup, so they stay as they are.
	for _, name := range []string{"judge-nudge.md"} {
		d := judgeFixture()
		plain, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatal(err)
		}
		d.NotesPath = testNotesPath
		if got, err := RenderPrompt(prompt(t, name), d); err != nil || got != plain {
			t.Errorf("%s changed with NotesPath: %q, %v", name, got, err)
		}
	}
}

// notesSteps are phrases of the notes procedure the judge prompts carried
// before it moved to the skill (about 1.6 KB of every re-review prompt).
// ("notes busy" is also what the lock command prints, so only the files are
// checked for it.)
var notesSteps = []string{"Repository notes at", "Take the lock", "Release the lock", "read them first",
	"again now", ".tmp", "never append", "Notes for", "At most about 80 lines", "orphan", "Nothing secret"}

// No judge prompt file carries the notes procedure or the readiness
// paragraph, whatever its data: both live in the skill only.
func TestJudgePromptFilesCarryNoNotesStepsOrReadinessParagraph(t *testing.T) {
	for _, name := range []string{"judge-initial.md", "judge-rereview.md", "judge-recovery.md", "judge-continue.md", "judge-nudge.md"} {
		text := prompt(t, name).Text
		for _, step := range append(slices.Clone(notesSteps), "notes busy", "did not pass", "rerun them", "rediscover", "environment_failures") {
			if strings.Contains(text, step) {
				t.Errorf("%s carries %q", name, step)
			}
		}
	}
}
