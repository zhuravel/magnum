package agents

import (
	"slices"
	"strings"
	"testing"
)

const testNotesPath = "/m/state/notes/talkable/talkable.md"

const testNotesParagraph = "Repository notes at " + testNotesPath + ": read them first; they are hints from earlier reviews, verify before relying on them."

// Every prompt that starts or continues a review shows the notes paragraph
// when the data has a NotesPath and omits it otherwise; the judge's also
// tell it to rewrite the file at the end of the round.
func TestNotesParagraphInRolePrompts(t *testing.T) {
	for _, name := range []string{"claude-review.md", "claude-rereview.md", "claude-simplify.md"} {
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
			got, err := RenderPrompt(p, d)
			if err != nil {
				t.Fatal(err)
			}
			if want := plain + "\n\n" + testNotesParagraph; got != want {
				t.Fatalf("with NotesPath:\n%s\nwant the plain prompt plus a paragraph:\n%s", got, want)
			}
			if strings.Contains(got, "rewrite") {
				t.Errorf("a reviewer must not rewrite the notes:\n%s", got)
			}
		})
	}
}

// The judge prompts pass the notes as <magnum> fields (the file, its harness
// directory and listing, the lock commands), and only when there are notes;
// the steps are the skill's (SKILL.md section 2), so no prompt repeats them
// and the text before the block is the same with or without notes.
func TestJudgePromptsPassTheNotesAsMagnumFields(t *testing.T) {
	dir, lock := NotesFiles(testNotesPath)
	for _, name := range []string{"judge-initial.md", "judge-rereview.md", "judge-recovery.md", "judge-continue.md"} {
		t.Run(name, func(t *testing.T) {
			p := prompt(t, name)
			d := judgeFixture()
			plain, err := RenderPrompt(p, d)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plain, "\nnotes") {
				t.Fatalf("without NotesPath:\n%s", plain)
			}
			d.NotesPath = testNotesPath
			for _, data := range []any{d, &d} { // values and pointers render alike
				got, err := RenderPrompt(p, data)
				if err != nil {
					t.Fatal(err)
				}
				at := strings.Index(got, "<magnum>")
				block := got[at:]
				for _, want := range []string{"\nnotes: " + testNotesPath + "\n", "\nnotes_dir: " + dir + "\n", "\nnotes_harness:\n",
					"\nnotes_lock: " + NotesLockLine(lock) + "\n", "\nnotes_unlock: " + NotesUnlockLine(lock) + "\n"} {
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
