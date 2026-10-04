package agents

import (
	"strings"
	"testing"
)

// After an identity migration the judge prompts name the former logins as
// the judge's own history (in words and in the magnum block), and a
// recovery names the threads file like a re-review.
func TestJudgePromptsNameTheFormerLogins(t *testing.T) {
	d := threadsFixture()
	d.FormerLogins = []string{"example[bot]"}
	for _, tc := range []struct{ golden, name string }{
		{"judge_rereview_former", "judge-rereview.md"},
		{"judge_recovery_former", "judge-recovery.md"},
	} {
		t.Run(tc.golden, func(t *testing.T) {
			got, err := RenderPrompt(prompt(t, tc.name), d)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, tc.golden, got)
			for _, want := range []string{
				"former_logins: example[bot]\n",
				"`example[bot]`",
				"Post everything new as `talkable[bot]`",
				"threads_file: " + d.ThreadsFile + "\n",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("prompt lacks %q:\n%s", want, got)
				}
			}
		})
	}
	// Without former logins neither prompt mentions them.
	d.FormerLogins = nil
	for _, name := range []string{"judge-rereview.md", "judge-recovery.md"} {
		got, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "before magnum moved") || !strings.Contains(got, "former_logins: \n") {
			t.Errorf("%s without former logins:\n%s", name, got)
		}
	}
}
