package fixwork

import (
	"reflect"
	"strings"
	"testing"
)

const (
	// refusalLogin is the fixer App that would push the merge.
	refusalLogin = "fixer-app[bot]"
	// refusalAt is the merge in words, as the engine's pushMerge names it.
	refusalAt = "the merge of master at c0c0c0c"
)

// refusalMerge is the merge of master's tip c0c0c0c... that the refusal
// decides on.
var refusalMerge = Conflict{Base: "master", SHA: strings.Repeat("c0", 20)}

// fourWorkflows are four workflow files, of which the words name three.
var fourWorkflows = []string{".github/workflows/a.yml", ".github/workflows/b.yml", ".github/workflows/c.yml", ".github/workflows/d.yml"}

// A merge whose workflow files are all as the base's tip on GitHub has
// them is not refused, whether it merged that tip or another, and whatever
// workflow files its push changes against the head.
func TestAMergeWhoseWorkflowFilesAreAsTheTipHasThemIsNotRefused(t *testing.T) {
	for _, tip := range []string{refusalMerge.SHA, strings.Repeat("d4", 20)} {
		if r := OwnWorkflowsRefusal(refusalAt, refusalMerge, tip, refusalLogin, nil, fourWorkflows); !reflect.DeepEqual(r, PushRefusal{}) {
			t.Errorf("tip %s: refusal = %+v, want none", tip[:7], r)
		}
	}
}

// A merge of a tip that is not the base's tip on GitHub, with workflow
// files that differ from that tip, is never pushed: the words name the
// tip, the base, three of the files and the login, and name no own
// workflow changes, so the settle never holds it for your y.
func TestAMergeOfAnotherTipWithDifferingWorkflowFilesIsNeverPushed(t *testing.T) {
	tip := strings.Repeat("d4", 20)
	r := OwnWorkflowsRefusal(refusalAt, refusalMerge, tip, refusalLogin, fourWorkflows, fourWorkflows)
	want := PushRefusal{Why: "the merge of master at c0c0c0c has workflow files that are not as the tip d4d4d4d of master on GitHub has them " +
		"(.github/workflows/a.yml, .github/workflows/b.yml, .github/workflows/c.yml), which fixer-app[bot] never pushes"}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("refusal = %+v\nwant %+v", r, want)
	}
}

// A merge of the base's tip on GitHub whose only fault is that it keeps
// the PR's own workflow changes is refused with words that name them (three
// at most) and the login, and names them all and the workflow files its
// push changes against the head, so the settle may hold it for your y.
func TestAMergeThatKeepsThePRsOwnWorkflowChangesNamesThemForYourY(t *testing.T) {
	for _, tc := range []struct {
		name string
		own  []string
		why  string
	}{
		{"one file", []string{".github/workflows/ci.yml"},
			"the merge of master at c0c0c0c keeps the PR's own workflow changes (.github/workflows/ci.yml), which fixer-app[bot] never pushes"},
		{"four files", fourWorkflows, "the merge of master at c0c0c0c keeps the PR's own workflow changes " +
			"(.github/workflows/a.yml, .github/workflows/b.yml, .github/workflows/c.yml), which fixer-app[bot] never pushes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workflows := []string{".github/workflows/ci.yml", ".github/workflows/pronto.yml"}
			r := OwnWorkflowsRefusal(refusalAt, refusalMerge, refusalMerge.SHA, refusalLogin, tc.own, workflows)
			if want := (PushRefusal{Why: tc.why, Own: tc.own, Files: workflows}); !reflect.DeepEqual(r, want) {
				t.Errorf("refusal = %+v\nwant %+v", r, want)
			}
		})
	}
}
