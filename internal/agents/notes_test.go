package agents

import (
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

func TestNotesParagraphInJudgePrompts(t *testing.T) {
	for _, name := range []string{"judge-initial.md", "judge-rereview.md", "judge-recovery.md", "judge-continue.md"} {
		t.Run(name, func(t *testing.T) {
			p := prompt(t, name)
			d := judgeFixture()
			plain, err := RenderPrompt(p, d)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plain, "Repository notes") || strings.Contains(plain, "Notes for") {
				t.Fatalf("without NotesPath:\n%s", plain)
			}
			d.NotesPath = testNotesPath
			for _, data := range []any{d, &d} { // values and pointers render alike
				got, err := RenderPrompt(p, data)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{testNotesParagraph,
					"update " + testNotesPath + " when this round taught you something durable",
					"rewrite it, never append", "At most about 80 lines",
					"`# Notes for talkable/talkable (updated YYYY-MM-DD)`", "harness directory /m/state/notes/talkable/talkable holds",
					"before you write the result file", "Nothing secret", "Take the lock", "Release the lock",
				} {
					if !strings.Contains(got, want) {
						t.Errorf("missing %q:\n%s", want, got)
					}
				}
				// The <magnum> block stays last and unchanged.
				if i := strings.Index(got, "\n\n<magnum>\n"); i < strings.Index(got, testNotesParagraph) || !strings.HasSuffix(got, "</magnum>") {
					t.Errorf("the notes paragraphs must come before the <magnum> block:\n%s", got)
				}
				if tail := got[strings.Index(got, "<magnum>"):]; !strings.Contains(plain, tail) {
					t.Errorf("the <magnum> block changed:\n%s\n--- plain:\n%s", tail, plain)
				}
			}
		})
	}
	// The short follow-ups restate no setup, so they stay as they are.
	for _, name := range []string{"judge-nudge.md", "judge-stop.md"} {
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
