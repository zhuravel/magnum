package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// A "workflow:<glob>" entry names the runs of each workflow whose file
// ("ci.yml") or declared name ("CI") the glob matches; never a commit status
// or another app's check run, which have neither. A workflow file a PR adds
// that declares the same name is one more workflow the entry names: its
// runs count beside the base's, they never replace them (Key).
func TestARequiredWorkflowEntryNamesItsFileOrItsDeclaredName(t *testing.T) {
	base := CheckResult{Name: "rspec", State: CheckFailed, Workflow: "CI", WorkflowFile: "ci.yml", At: t0}
	own := CheckResult{Name: "rspec", State: CheckPassed, Workflow: "CI", WorkflowFile: "ci-pr.yml", At: t0.Add(time.Hour)}
	lint := CheckResult{Name: "lint", State: CheckPassed, Workflow: "Terraform scripts", WorkflowFile: "terraform.yml", At: t0}
	status := CheckResult{Name: "rspec", State: CheckPassed, Status: true, At: t0.Add(time.Hour)}
	app := CheckResult{Name: "danger", State: CheckPassed, At: t0}
	checks := []CheckResult{base, own, lint, status, app}
	for entry, want := range map[string][]CheckResult{
		"workflow:CI":         {base, own},
		"workflow:ci.yml":     {base},
		"workflow:ci*.yml":    {base, own},
		"workflow:terraform*": {lint},
		"workflow:*":          {base, own, lint},
		"workflow:danger":     nil,
	} {
		if got := RequiredRuns(checks, entry); !reflect.DeepEqual(got, want) {
			t.Errorf("RequiredRuns(%q) =\n%+v\nwant\n%+v", entry, got, want)
		}
	}
	if base.Key() == own.Key() {
		t.Errorf("two workflow files share a key: %+v", base.Key())
	}
	if old := (CheckResult{Name: "rspec", Workflow: "CI"}); old.Key() == base.Key() || old.Key().Workflow != "CI" {
		t.Errorf("a run read without its file is keyed %+v", old.Key())
	}
}

// A name glob counts the latest check run of each matching name across
// workflows (a check posted through the API lands in another workflow's
// suite) and, apart, the latest commit status of that name: a status
// anyone who can write to the repository posts never replaces a check run,
// nor another app's run, whichever is newer.
func TestARequiredNameCountsAStatusApartFromTheCheckRun(t *testing.T) {
	failedRun := CheckResult{Name: "rspec", State: CheckFailed, Workflow: "CI", WorkflowFile: "ci.yml", At: t0}
	rerun := CheckResult{Name: "rspec", State: CheckPassed, Workflow: "Dispatch", WorkflowFile: "dispatch.yml", At: t0.Add(30 * time.Minute)}
	older := CheckResult{Name: "rspec", State: CheckFailed, Status: true, At: t0.Add(-time.Hour)}
	newer := CheckResult{Name: "rspec", State: CheckPassed, Status: true, At: t0.Add(time.Hour)}
	appRun := CheckResult{Name: "danger", State: CheckFailed, At: t0}
	appStatus := CheckResult{Name: "danger", State: CheckPassed, Status: true, At: t0.Add(time.Hour)}
	for _, tc := range []struct {
		name, entry  string
		checks, want []CheckResult
	}{
		{"a status after a failed run", "rspec", []CheckResult{failedRun, newer}, []CheckResult{failedRun, newer}},
		{"the latest status and the latest run", "rspec", []CheckResult{older, failedRun, newer, rerun}, []CheckResult{newer, rerun}},
		{"a status after another app's run", "danger", []CheckResult{appRun, appStatus}, []CheckResult{appRun, appStatus}},
	} {
		if got := RequiredRuns(tc.checks, tc.entry); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: RequiredRuns(%q) =\n%+v\nwant\n%+v", tc.name, tc.entry, got, tc.want)
		}
	}
	if appRun.Key() == appStatus.Key() {
		t.Errorf("a status and another app's run share a key: %+v", appRun.Key())
	}
}

// A read of the checks keyed by workflow file replaces an earlier build's
// read of the same checks: the files, the status flags and the mark are
// written though nothing else moved.
func TestChecksKeyedByWorkflowFileReplaceAnEarlierBuildsRead(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	old := ciFixture("h1", "SUCCESS", CheckResult{Name: "rspec", State: CheckPassed, Workflow: "CI", At: t0},
		CheckResult{Name: "license", State: CheckPassed, At: t0})
	in := GitHubPR{RepoID: repo.ID, NodeID: "PR_9", Number: 9, URL: "u9", HeadSHA: "h1", InitialState: PRQueued, Identity: "talkable-app",
		DetailsAt: new(t0), CIState: new("SUCCESS"), CI: old}
	if _, err := st.UpsertPRFromGitHub(ctx, in); err != nil {
		t.Fatal(err)
	}
	keyed := ciFixture("h1", "SUCCESS", CheckResult{Name: "rspec", State: CheckPassed, Workflow: "CI", WorkflowFile: "ci.yml", At: t0},
		CheckResult{Name: "license", State: CheckPassed, Status: true, At: t0})
	keyed.ByWorkflowFile = true
	in.CI = keyed
	res, err := st.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.PR.CI == nil || !reflect.DeepEqual(*res.PR.CI, *keyed) {
		t.Fatalf("stored CI = %+v, want %+v", res.PR.CI, keyed)
	}
}
