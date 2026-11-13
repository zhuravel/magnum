package fixwork

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// ci-failures.md heads each failed check by its required entry, in the
// verdict's words: a workflow entry as the required workflow (with no
// check of its own, as magnum's run of it gives), the check's name beside
// an entry that is not it.
func TestTheCIFailuresSummaryHeadsEachCheckByItsRequiredEntry(t *testing.T) {
	repo, pr := store.Repo{Owner: "talkable", Name: "talkable"}, store.PR{Number: 2}
	md := CIFailuresText(repo, pr, CIFailures{Head: fixHead, Checks: []ciFailureJSON{
		{Entry: "workflow:Other", WorkflowFile: "ci.yml", RunID: ciRunID, Attempt: 1},
		{Entry: "workflow:CI", Name: "rspec (3)", WorkflowFile: "ci.yml", RunID: ciRunID, Attempt: 1},
		{Entry: "Completion", Name: "Completion", WorkflowFile: "ci.yml", RunID: ciRunID, Attempt: 1},
	}})
	for _, want := range []string{"\n## required workflow Other, ci.yml, run 500 attempt 1\n", "\n## required workflow CI (rspec (3)), ci.yml, run 500 attempt 1\n",
		"\n## required check Completion, ci.yml, run 500 attempt 1\n"} {
		if !strings.Contains(md, want) {
			t.Errorf("ci-failures.md lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "required check  ") || strings.Contains(md, "required check rspec") {
		t.Errorf("ci-failures.md names a job as the required check:\n%s", md)
	}
}
