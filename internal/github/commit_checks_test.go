package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

const baseTipOid = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"

// commitAnswer answers a CommitChecks call with object as the commit.
func commitAnswer(t *testing.T, object string) []byte {
	t.Helper()
	return compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4900,"used":100,"resetAt":"2026-10-13T10:00:00Z"},`+
		`"repository":{"object":`+object+`}}}`)
}

// The talkable#11483 case (2026-10-13): the base branch's tip fails
// Dependency Audit as the PR's head does. CommitChecks reads one commit's
// rollup by its oid with the contexts Details reads of a head and
// normalizes them the same way: the latest run of each workflow and name (a
// re-run supersedes the run before it), a run not completed pending,
// SUCCESS and NEUTRAL passing, SKIPPED skipped, any other conclusion
// failing, and a commit status passing on SUCCESS, waiting on PENDING or
// EXPECTED and failing otherwise.
func TestCommitChecksReadsTheCommitsRollup(t *testing.T) {
	var req gqlReq
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, r gqlReq) (execx.Result, error) {
		req = r
		return okResult(commitAnswer(t, `{"oid":"`+baseTipOid+`","statusCheckRollup":{"contexts":{"nodes":[
  {"__typename":"CheckRun","name":"audit","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-10-13T08:00:00Z","completedAt":"2026-10-13T08:02:00Z","checkSuite":{"workflowRun":{"workflow":{"name":"Dependency Audit"}}}},
  {"__typename":"CheckRun","name":"rspec","status":"COMPLETED","conclusion":"TIMED_OUT","startedAt":"2026-10-13T08:00:00Z","completedAt":"2026-10-13T08:30:00Z","checkSuite":{"workflowRun":{"workflow":{"name":"CI"}}}},
  {"__typename":"CheckRun","name":"rspec","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-13T08:40:00Z","completedAt":"2026-10-13T09:10:00Z","checkSuite":{"workflowRun":{"workflow":{"name":"CI"}}}},
  {"__typename":"CheckRun","name":"jest","status":"IN_PROGRESS","conclusion":null,"startedAt":"2026-10-13T08:05:00Z","completedAt":null,"checkSuite":{"workflowRun":{"workflow":{"name":"CI"}}}},
  {"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"NEUTRAL","startedAt":"2026-10-13T08:01:00Z","completedAt":"2026-10-13T08:03:00Z","checkSuite":{"workflowRun":{"workflow":{"name":"CI"}}}},
  {"__typename":"CheckRun","name":"deploy-preview","status":"COMPLETED","conclusion":"SKIPPED","startedAt":"2026-10-13T08:00:00Z","completedAt":"2026-10-13T08:00:00Z","checkSuite":{"workflowRun":{"workflow":{"name":"CI"}}}},
  {"__typename":"StatusContext","context":"coverage","state":"ERROR","createdAt":"2026-10-13T08:04:00Z"},
  {"__typename":"StatusContext","context":"license","state":"EXPECTED","createdAt":"2026-10-13T08:06:00Z"}
]}}}`))
	})}}
	got, err := (&Client{Run: f}).CommitChecks(context.Background(), "talkable", "talkable", baseTipOid)
	if err != nil {
		t.Fatal(err)
	}
	at := func(hm string) time.Time {
		ts, err := time.Parse(time.RFC3339, "2026-10-13T"+hm+":00Z")
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	want := []Check{
		{Name: "audit", State: CheckFailed, Workflow: "Dependency Audit", At: at("08:02")},
		{Name: "rspec", State: CheckPassed, Workflow: "CI", At: at("09:10")}, // the re-run replaces the timeout
		{Name: "jest", State: CheckPending, Workflow: "CI", At: at("08:05")},
		{Name: "lint", State: CheckPassed, Workflow: "CI", At: at("08:03")},
		{Name: "deploy-preview", State: CheckSkipped, Workflow: "CI", At: at("08:00")},
		{Name: "coverage", State: CheckFailed, Workflow: "", Status: true, At: at("08:04")},
		{Name: "license", State: CheckPending, Workflow: "", Status: true, At: at("08:06")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("checks =\n%+v\nwant\n%+v", got, want)
	}
	q := oneLine(req.Query)
	for _, s := range []string{"repository(owner: $owner, name: $name) { object(oid: $oid) { ... on Commit { oid statusCheckRollup { contexts(first: 100) { nodes {",
		"... on CheckRun { name status conclusion startedAt completedAt checkSuite { workflowRun { workflow { name resourcePath } } } }",
		"... on StatusContext { context state createdAt }", "rateLimit { limit cost remaining used resetAt }"} {
		if !strings.Contains(q, s) {
			t.Errorf("query lacks %q:\n%s", s, q)
		}
	}
	if req.Variables["owner"] != "talkable" || req.Variables["name"] != "talkable" || req.Variables["oid"] != baseTipOid {
		t.Errorf("variables = %v", req.Variables)
	}
}

// A commit without checks has none (an empty list); a commit GitHub does
// not have is an error matching ErrNotFound, and a sha that is no full
// commit id is refused before any call.
func TestCommitChecksOfACommitWithoutChecksOrOfNone(t *testing.T) {
	answer := `{"oid":"` + baseTipOid + `","statusCheckRollup":null}`
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(execx.Cmd, gqlReq) (execx.Result, error) {
		return okResult(commitAnswer(t, answer))
	})}}
	c := &Client{Run: f}
	got, err := c.CommitChecks(context.Background(), "talkable", "talkable", baseTipOid)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("checks = %#v, %v; want none", got, err)
	}
	answer = "null"
	if _, err := c.CommitChecks(context.Background(), "talkable", "talkable", baseTipOid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing commit: %v, want ErrNotFound", err)
	}
	calls := len(f.Calls)
	for _, sha := range []string{"", "c0ffee0", "master", strings.ToUpper(baseTipOid), baseTipOid + "0"} {
		if _, err := c.CommitChecks(context.Background(), "talkable", "talkable", sha); err == nil {
			t.Errorf("sha %q: no error", sha)
		}
	}
	if len(f.Calls) != calls {
		t.Errorf("an invalid sha reached GitHub: %d calls", len(f.Calls)-calls)
	}
}

// Two workflow files may declare one name, and a PR can add one: a `CI` of
// its own whose rspec passes after the base's CI failed it. A commit status,
// which anyone who can write to the repository posts, may carry a check
// run's name. The checks are kept by their workflow's file (read from the
// workflow's resourcePath: "/talkable/talkable/actions/workflows/ci.yml" is
// "ci.yml", a nested one keeps its rest) and name, a status apart, so none
// replaces the other; a re-run of one file's job still supersedes its run.
func TestChecksAreKeyedByWorkflowFileWithStatusesApart(t *testing.T) {
	run := func(name, conclusion, at, workflow, resource string) string {
		suite := `{"workflowRun":null}`
		if workflow != "" {
			suite = `{"workflowRun":{"workflow":{"name":"` + workflow + `","resourcePath":"` + resource + `"}}}`
		}
		return `{"__typename":"CheckRun","name":"` + name + `","status":"COMPLETED","conclusion":"` + conclusion +
			`","startedAt":"2026-10-13T08:00:00Z","completedAt":"2026-10-13T` + at + `:00Z","checkSuite":` + suite + `}`
	}
	status := func(context, state, at string) string {
		return `{"__typename":"StatusContext","context":"` + context + `","state":"` + state + `","createdAt":"2026-10-13T` + at + `:00Z"}`
	}
	const ci, own, scan = "/talkable/talkable/actions/workflows/ci.yml", "/talkable/talkable/actions/workflows/ci-pr.yml",
		"/talkable/talkable/actions/workflows/dynamic/github-code-scanning/codeql"
	nodes := strings.Join([]string{
		run("rspec", "FAILURE", "08:30", "CI", ci),
		run("rspec", "SUCCESS", "08:50", "CI", own), // the PR's own CI: kept beside the base's
		run("jest", "FAILURE", "08:10", "CI", ci),
		run("jest", "SUCCESS", "08:40", "CI", ci), // a re-run of the same file's job supersedes it
		run("danger", "FAILURE", "08:20", "", ""),
		status("danger", "SUCCESS", "08:45"), // a status beside another app's run
		run("Completion", "FAILURE", "08:31", "CI", ci),
		status("Completion", "SUCCESS", "08:55"), // a status beside a workflow's run
		run("Analyze", "SUCCESS", "08:15", "CodeQL", scan),
	}, ",")
	var req gqlReq
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, r gqlReq) (execx.Result, error) {
		req = r
		return okResult(commitAnswer(t, `{"oid":"`+baseTipOid+`","statusCheckRollup":{"contexts":{"nodes":[`+nodes+`]}}}`))
	})}}
	got, err := (&Client{Run: f}).CommitChecks(context.Background(), "talkable", "talkable", baseTipOid)
	if err != nil {
		t.Fatal(err)
	}
	at := func(hm string) time.Time {
		ts, err := time.Parse(time.RFC3339, "2026-10-13T"+hm+":00Z")
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	want := []Check{
		{Name: "rspec", State: CheckFailed, Workflow: "CI", WorkflowFile: "ci.yml", At: at("08:30")},
		{Name: "rspec", State: CheckPassed, Workflow: "CI", WorkflowFile: "ci-pr.yml", At: at("08:50")},
		{Name: "jest", State: CheckPassed, Workflow: "CI", WorkflowFile: "ci.yml", At: at("08:40")},
		{Name: "danger", State: CheckFailed, At: at("08:20")},
		{Name: "danger", State: CheckPassed, Status: true, At: at("08:45")},
		{Name: "Completion", State: CheckFailed, Workflow: "CI", WorkflowFile: "ci.yml", At: at("08:31")},
		{Name: "Completion", State: CheckPassed, Status: true, At: at("08:55")},
		{Name: "Analyze", State: CheckPassed, Workflow: "CodeQL", WorkflowFile: "dynamic/github-code-scanning/codeql", At: at("08:15")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("checks =\n%+v\nwant\n%+v", got, want)
	}
	if q := oneLine(req.Query); !strings.Contains(q, "checkSuite { workflowRun { workflow { name resourcePath } } }") {
		t.Errorf("the query does not read the workflow's resourcePath:\n%s", q)
	}
}
